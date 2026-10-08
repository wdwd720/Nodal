package nodalsdk

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"unicode/utf8"
)

// InputSchema names the one input document format this SDK reads.
const InputSchema = "nodal.sandbox.input/v1"

// Input bounds. The host enforces the same numbers (it imports them), so a
// document the host builds is always one the SDK accepts.
const (
	MaxInstruments      = 256
	MaxClosesPerSeries  = 4096
	MaxPositions        = 1024
	MaxConfigEntries    = 128
	MaxConfigValueBytes = 1024
	MaxLogFields        = 32
	MaxLogValueBytes    = 256
	MaxNameBytes        = 64
)

// ErrInvalidInput is returned when the input document is not exactly the
// documented format.
var ErrInvalidInput = errors.New("nodalsdk: invalid input document")

// Instrument is one instrument of the market snapshot. Prices are in the
// valuation currency per whole base unit.
type Instrument struct {
	ID            string
	Kind          string
	BaseDecimals  uint8
	QuoteDecimals uint8
	Mark          Decimal
	// BestBid and BestAsk are absent when HasBid / HasAsk is false.
	BestBid Decimal
	HasBid  bool
	BestAsk Decimal
	HasAsk  bool
	// Closes are the recent closes, oldest first.
	Closes []Decimal
}

// Position is a holding of an instrument in base units (negative for a short
// derivative position).
type Position struct {
	InstrumentID string
	Quantity     Decimal
}

// Portfolio is the account's snapshot: cash in the valuation currency and
// positions in base units.
type Portfolio struct {
	Cash      Decimal
	Positions []Position
}

// Snapshot is the decoded input document.
type Snapshot struct {
	AsOf        string
	Instruments []Instrument
	Portfolio   Portfolio
	Config      map[string]string
}

type wireInstrument struct {
	InstrumentID  string   `json:"instrument_id"`
	Kind          string   `json:"kind"`
	BaseDecimals  uint8    `json:"base_decimals"`
	QuoteDecimals uint8    `json:"quote_decimals"`
	Mark          string   `json:"mark"`
	BestBid       string   `json:"best_bid"`
	BestAsk       string   `json:"best_ask"`
	Closes        []string `json:"closes"`
}

type wirePosition struct {
	InstrumentID string `json:"instrument_id"`
	Quantity     string `json:"quantity"`
}

type wirePortfolio struct {
	Cash      string         `json:"cash"`
	Positions []wirePosition `json:"positions"`
}

type wireInput struct {
	Schema      string            `json:"schema"`
	AsOf        string            `json:"as_of"`
	Instruments []wireInstrument  `json:"instruments"`
	Portfolio   wirePortfolio     `json:"portfolio"`
	Config      map[string]string `json:"config"`
}

// DecodeSnapshot parses and validates an input document. Unknown fields,
// trailing data, malformed decimals, unsorted or duplicate instruments and
// out-of-bounds sizes are all refused.
func DecodeSnapshot(data []byte) (Snapshot, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var w wireInput
	if err := dec.Decode(&w); err != nil {
		return Snapshot{}, errors.Join(ErrInvalidInput, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Snapshot{}, errors.Join(ErrInvalidInput, errors.New("trailing data after the input document"))
	}
	if w.Schema != InputSchema {
		return Snapshot{}, errors.Join(ErrInvalidInput, errors.New("unknown schema "+w.Schema))
	}
	if w.AsOf == "" || len(w.AsOf) > MaxNameBytes {
		return Snapshot{}, errors.Join(ErrInvalidInput, errors.New("as_of required"))
	}
	if len(w.Instruments) > MaxInstruments {
		return Snapshot{}, errors.Join(ErrInvalidInput, errors.New("too many instruments"))
	}
	snap := Snapshot{AsOf: w.AsOf, Config: map[string]string{}}
	for i, wi := range w.Instruments {
		if !ValidID(wi.InstrumentID) || !ValidKind(wi.Kind) {
			return Snapshot{}, errors.Join(ErrInvalidInput, errors.New("instrument id or kind malformed"))
		}
		if i > 0 && w.Instruments[i-1].InstrumentID >= wi.InstrumentID {
			return Snapshot{}, errors.Join(ErrInvalidInput, errors.New("instruments must be sorted by id without duplicates"))
		}
		if len(wi.Closes) > MaxClosesPerSeries {
			return Snapshot{}, errors.Join(ErrInvalidInput, errors.New("too many closes"))
		}
		ins := Instrument{ID: wi.InstrumentID, Kind: wi.Kind, BaseDecimals: wi.BaseDecimals, QuoteDecimals: wi.QuoteDecimals}
		var err error
		if ins.Mark, err = positiveDecimal(wi.Mark); err != nil {
			return Snapshot{}, errors.Join(ErrInvalidInput, errors.New("mark: "+err.Error()))
		}
		if wi.BestBid != "" {
			if ins.BestBid, err = positiveDecimal(wi.BestBid); err != nil {
				return Snapshot{}, errors.Join(ErrInvalidInput, errors.New("best_bid: "+err.Error()))
			}
			ins.HasBid = true
		}
		if wi.BestAsk != "" {
			if ins.BestAsk, err = positiveDecimal(wi.BestAsk); err != nil {
				return Snapshot{}, errors.Join(ErrInvalidInput, errors.New("best_ask: "+err.Error()))
			}
			ins.HasAsk = true
		}
		if ins.HasBid && ins.HasAsk && ins.BestBid.Cmp(ins.BestAsk) > 0 {
			return Snapshot{}, errors.Join(ErrInvalidInput, errors.New("best_bid above best_ask"))
		}
		ins.Closes = make([]Decimal, 0, len(wi.Closes))
		for _, c := range wi.Closes {
			d, err := positiveDecimal(c)
			if err != nil {
				return Snapshot{}, errors.Join(ErrInvalidInput, errors.New("close: "+err.Error()))
			}
			ins.Closes = append(ins.Closes, d)
		}
		snap.Instruments = append(snap.Instruments, ins)
	}
	cash, err := ParseDecimal(w.Portfolio.Cash)
	if err != nil || cash.Sign() < 0 {
		return Snapshot{}, errors.Join(ErrInvalidInput, errors.New("portfolio cash must be a non-negative decimal"))
	}
	snap.Portfolio.Cash = cash
	if len(w.Portfolio.Positions) > MaxPositions {
		return Snapshot{}, errors.Join(ErrInvalidInput, errors.New("too many positions"))
	}
	for i, p := range w.Portfolio.Positions {
		if !ValidID(p.InstrumentID) {
			return Snapshot{}, errors.Join(ErrInvalidInput, errors.New("position instrument malformed"))
		}
		if i > 0 && w.Portfolio.Positions[i-1].InstrumentID >= p.InstrumentID {
			return Snapshot{}, errors.Join(ErrInvalidInput, errors.New("positions must be sorted by instrument without duplicates"))
		}
		q, err := ParseUnits(p.Quantity)
		if err != nil {
			return Snapshot{}, errors.Join(ErrInvalidInput, errors.New("position quantity must be an integer"))
		}
		snap.Portfolio.Positions = append(snap.Portfolio.Positions, Position{InstrumentID: p.InstrumentID, Quantity: q})
	}
	if len(w.Config) > MaxConfigEntries {
		return Snapshot{}, errors.Join(ErrInvalidInput, errors.New("too many config entries"))
	}
	for k, v := range w.Config {
		if !ValidName(k) || len(v) > MaxConfigValueBytes || !utf8.ValidString(v) {
			return Snapshot{}, errors.Join(ErrInvalidInput, errors.New("config entry malformed"))
		}
		snap.Config[k] = v
	}
	return snap, nil
}

func positiveDecimal(s string) (Decimal, error) {
	d, err := ParseDecimal(s)
	if err != nil {
		return Decimal{}, err
	}
	if d.Sign() <= 0 {
		return Decimal{}, ErrInvalidDecimal
	}
	return d, nil
}

// ValidID reports whether s is a canonical lowercase UUID (8-4-4-4-12 hex).
func ValidID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
				return false
			}
		}
	}
	return true
}

// ValidKind reports whether s is an upper-case token such as SPOT_PAIR or
// ACQUIRE_EXPOSURE: [A-Z][A-Z0-9_]{0,63}.
func ValidKind(s string) bool {
	if len(s) == 0 || len(s) > MaxNameBytes || s[0] < 'A' || s[0] > 'Z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') && c != '_' {
			return false
		}
	}
	return true
}

// ValidName reports whether s is a lower-case name such as a configuration
// key, a log event or a log field: [a-z][a-z0-9_.-]{0,63}.
func ValidName(s string) bool {
	if len(s) == 0 || len(s) > MaxNameBytes || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '_' && c != '.' && c != '-' {
			return false
		}
	}
	return true
}

// ProposableKinds are the only intent kinds a strategy may propose: taking
// and reducing exposure. Moving value between accounts, withdrawing,
// bridging, borrowing, minting and every other kind are absent on purpose.
func ProposableKinds() []string {
	return []string{"ACQUIRE_EXPOSURE", "CLOSE_POSITION", "OPEN_POSITION", "REDUCE_EXPOSURE"}
}

// Sides are the sides a proposal may carry.
func Sides() []string { return []string{"BUY", "SELL"} }

func contains(list []string, s string) bool {
	i := sort.SearchStrings(list, s)
	return i < len(list) && list[i] == s
}

// CheckProposal validates a proposal's fields exactly as the host does. A
// kind that fixes the side (ACQUIRE_EXPOSURE buys, REDUCE_EXPOSURE sells)
// must carry that side.
func CheckProposal(kind, instrument, side, quantity, limitPrice string) error {
	if !contains(ProposableKinds(), kind) {
		return errors.New("nodalsdk: kind " + kind + " cannot be proposed")
	}
	if !ValidID(instrument) {
		return errors.New("nodalsdk: instrument must be a canonical lowercase uuid")
	}
	if !contains(Sides(), side) {
		return errors.New("nodalsdk: side must be BUY or SELL")
	}
	if (kind == "ACQUIRE_EXPOSURE" && side != "BUY") || (kind == "REDUCE_EXPOSURE" && side != "SELL") {
		return errors.New("nodalsdk: " + kind + " cannot be " + side)
	}
	q, err := ParseUnits(quantity)
	if err != nil || q.Sign() <= 0 {
		return errors.New("nodalsdk: quantity must be a positive integer of base units")
	}
	if limitPrice != "" {
		if _, err := positiveDecimal(limitPrice); err != nil {
			return errors.New("nodalsdk: limit price must be a positive decimal")
		}
	}
	return nil
}

// CheckLog validates a log record exactly as the host does.
func CheckLog(event string, fields map[string]string) error {
	if !ValidName(event) {
		return errors.New("nodalsdk: log event must match [a-z][a-z0-9_.-]{0,63}")
	}
	if len(fields) > MaxLogFields {
		return errors.New("nodalsdk: too many log fields")
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := fields[k]
		if !ValidName(k) || len(v) > MaxLogValueBytes || !utf8.ValidString(v) || strings.ContainsRune(v, 0) {
			if len(k) > MaxNameBytes || !utf8.ValidString(k) {
				return errors.New("nodalsdk: a log field name is malformed")
			}
			return errors.New("nodalsdk: log field " + k + " malformed")
		}
	}
	return nil
}
