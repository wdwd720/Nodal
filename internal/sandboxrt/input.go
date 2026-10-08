package sandboxrt

import (
	"crypto/sha256"
	"sort"
	"time"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/errs"
	nodalsdk "github.com/nodal/controlplane/internal/sandboxrt/sdk"
)

// InputSchema is the input document format (nodalsdk.InputSchema).
const InputSchema = nodalsdk.InputSchema

// Input is the one document a strategy reads: a market snapshot, a portfolio
// snapshot and the approved configuration. Every number is an exact decimal
// string whose fractional digit count is its scale; prices are in simulated
// US dollars per whole base unit; quantities are integers of base units.
//
// The host builds it, renders it canonically (sorted keys, no whitespace)
// and validates it with the SDK's own decoder before a run, so a document the
// host sends is always one the strategy accepts.
type Input struct {
	Schema      string            `json:"schema"`
	AsOf        time.Time         `json:"as_of"`
	Instruments []InputInstrument `json:"instruments"`
	Portfolio   InputPortfolio    `json:"portfolio"`
	Config      map[string]string `json:"config"`
}

// InputInstrument is one instrument of the market snapshot.
type InputInstrument struct {
	InstrumentID  string   `json:"instrument_id"`
	Kind          string   `json:"kind"`
	BaseDecimals  uint8    `json:"base_decimals"`
	QuoteDecimals uint8    `json:"quote_decimals"`
	Mark          string   `json:"mark"`
	BestBid       string   `json:"best_bid"`
	BestAsk       string   `json:"best_ask"`
	Closes        []string `json:"closes"`
}

// InputPortfolio is the account snapshot.
type InputPortfolio struct {
	// Cash is the account's cash in simulated dollars.
	Cash      string          `json:"cash"`
	Positions []InputPosition `json:"positions"`
}

// InputPosition is a position in base units.
type InputPosition struct {
	InstrumentID string `json:"instrument_id"`
	Quantity     string `json:"quantity"`
}

// normalized returns a copy in canonical shape: schema set, as-of in UTC,
// instruments and positions sorted, and every nil collection empty so the
// canonical form never contains null.
func (in Input) normalized() Input {
	out := in
	if out.Schema == "" {
		out.Schema = InputSchema
	}
	out.AsOf = in.AsOf.UTC()
	out.Instruments = make([]InputInstrument, len(in.Instruments))
	for i, ins := range in.Instruments {
		ins.Closes = append([]string{}, ins.Closes...)
		out.Instruments[i] = ins
	}
	sort.SliceStable(out.Instruments, func(i, j int) bool {
		return out.Instruments[i].InstrumentID < out.Instruments[j].InstrumentID
	})
	out.Portfolio.Positions = append([]InputPosition{}, in.Portfolio.Positions...)
	sort.SliceStable(out.Portfolio.Positions, func(i, j int) bool {
		return out.Portfolio.Positions[i].InstrumentID < out.Portfolio.Positions[j].InstrumentID
	})
	out.Config = make(map[string]string, len(in.Config))
	for k, v := range in.Config {
		out.Config[k] = v
	}
	return out
}

// Canonical renders the input canonically and validates it with the SDK's
// decoder. It returns the exact bytes a strategy will read on stdin.
func (in Input) Canonical() ([]byte, error) {
	n := in.normalized()
	if n.AsOf.IsZero() {
		return nil, errs.New(errs.CodeValidationFailed, "sandboxrt: input as_of is required")
	}
	b, err := audit.CanonicalJSON(n)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeValidationFailed, "sandboxrt: input is not canonically encodable")
	}
	if _, err := nodalsdk.DecodeSnapshot(b); err != nil {
		return nil, errs.Wrap(err, errs.CodeValidationFailed, "sandboxrt: input is not a valid strategy input document")
	}
	return b, nil
}

// Snapshot decodes the canonical input exactly as the strategy will.
func (in Input) Snapshot() (nodalsdk.Snapshot, error) {
	b, err := in.Canonical()
	if err != nil {
		return nodalsdk.Snapshot{}, err
	}
	return nodalsdk.DecodeSnapshot(b)
}

// Digest returns the sha256 of the canonical input.
func (in Input) Digest() ([]byte, error) {
	b, err := in.Canonical()
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	return sum[:], nil
}

// Instrument returns one instrument of the snapshot.
func (in Input) Instrument(instrumentID string) (InputInstrument, bool) {
	for _, ins := range in.Instruments {
		if ins.InstrumentID == instrumentID {
			return ins, true
		}
	}
	return InputInstrument{}, false
}

// Equity is the portfolio's exact value in simulated dollars: cash plus every
// position x its instrument's mark. A position whose instrument has no mark
// in the snapshot cannot be valued, and Equity refuses rather than guess.
func (in Input) Equity() (nodalsdk.Decimal, error) {
	snap, err := in.Snapshot()
	if err != nil {
		return nodalsdk.Decimal{}, err
	}
	total := snap.Portfolio.Cash
	for _, p := range snap.Portfolio.Positions {
		var ins *nodalsdk.Instrument
		for i := range snap.Instruments {
			if snap.Instruments[i].ID == p.InstrumentID {
				ins = &snap.Instruments[i]
			}
		}
		if ins == nil {
			return nodalsdk.Decimal{}, errs.New(errs.CodeStaleMarketData, "sandboxrt: a position has no mark in the snapshot").
				WithField("instrument_id", p.InstrumentID)
		}
		value, err := positionValue(p.Quantity, ins.BaseDecimals, ins.Mark)
		if err != nil {
			return nodalsdk.Decimal{}, err
		}
		total = total.Add(value)
	}
	return total, nil
}

// positionValue is quantity (base units) x 10^-baseDecimals x price, exactly.
// A product whose scale would exceed nodalsdk.MaxScale is refused rather than
// rounded.
func positionValue(quantity nodalsdk.Decimal, baseDecimals uint8, price nodalsdk.Decimal) (nodalsdk.Decimal, error) {
	whole, err := quantity.MovePointLeft(int32(baseDecimals))
	if err != nil {
		return nodalsdk.Decimal{}, errs.Wrap(err, errs.CodeOverflow, "sandboxrt: position scale out of range")
	}
	v, err := whole.Mul(price)
	if err != nil {
		return nodalsdk.Decimal{}, errs.Wrap(err, errs.CodeOverflow, "sandboxrt: position value scale out of range")
	}
	return v, nil
}
