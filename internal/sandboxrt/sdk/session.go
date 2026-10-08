package nodalsdk

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
)

// maxInputBytes bounds what Run reads from stdin. The host's own limit is
// never larger.
const maxInputBytes = 16 << 20

// Exit statuses of Run.
const (
	exitOK            = 0
	exitStrategyError = 1
	exitInputInvalid  = 2
	exitOutputFailed  = 3
)

// Session is what a strategy sees: the decoded snapshot, read-only, and the
// two things it may do, EmitIntent and Log.
type Session struct {
	snap    Snapshot
	byID    map[string]int
	out     *bufio.Writer
	seq     int
	failure error
}

func newSession(snap Snapshot, w io.Writer) *Session {
	s := &Session{snap: snap, byID: make(map[string]int, len(snap.Instruments)), out: bufio.NewWriter(w)}
	for i, ins := range snap.Instruments {
		s.byID[ins.ID] = i
	}
	return s
}

// AsOf returns the snapshot's as-of time (RFC 3339, UTC). It is the only
// notion of "now" a strategy has.
func (s *Session) AsOf() string { return s.snap.AsOf }

// Instruments returns every instrument of the snapshot, sorted by id.
func (s *Session) Instruments() []Instrument {
	out := make([]Instrument, len(s.snap.Instruments))
	for i, ins := range s.snap.Instruments {
		out[i] = copyInstrument(ins)
	}
	return out
}

// Instrument returns one instrument by id.
func (s *Session) Instrument(id string) (Instrument, bool) {
	i, ok := s.byID[id]
	if !ok {
		return Instrument{}, false
	}
	return copyInstrument(s.snap.Instruments[i]), true
}

func copyInstrument(ins Instrument) Instrument {
	ins.Closes = append([]Decimal(nil), ins.Closes...)
	return ins
}

// Portfolio returns the account snapshot.
func (s *Session) Portfolio() Portfolio {
	return Portfolio{Cash: s.snap.Portfolio.Cash, Positions: append([]Position(nil), s.snap.Portfolio.Positions...)}
}

// Position returns the position in one instrument, zero when there is none.
func (s *Session) Position(instrumentID string) Decimal {
	for _, p := range s.snap.Portfolio.Positions {
		if p.InstrumentID == instrumentID {
			return p.Quantity
		}
	}
	return Decimal{}
}

// Config returns an approved configuration value.
func (s *Session) Config(key string) (string, bool) {
	v, ok := s.snap.Config[key]
	return v, ok
}

type intentRecord struct {
	Type         string `json:"type"`
	Seq          int    `json:"seq"`
	Kind         string `json:"kind"`
	InstrumentID string `json:"instrument_id"`
	Side         string `json:"side"`
	Quantity     string `json:"quantity"`
	LimitPrice   string `json:"limit_price,omitempty"`
}

type logRecord struct {
	Type   string            `json:"type"`
	Seq    int               `json:"seq"`
	Event  string            `json:"event"`
	Fields map[string]string `json:"fields,omitempty"`
}

// EmitIntent proposes an economic intent. It is validated here and again by
// the host; the instrument must be in the snapshot. limitPrice may be empty.
// A proposal is a request, not an order: the host drops anything outside the
// strategy's manifest and puts the rest through the risk kernel.
func (s *Session) EmitIntent(kind, instrument, side, quantity, limitPrice string) error {
	if err := CheckProposal(kind, instrument, side, quantity, limitPrice); err != nil {
		return err
	}
	if _, ok := s.byID[instrument]; !ok {
		return errors.New("nodalsdk: instrument " + instrument + " is not in the snapshot")
	}
	return s.write(intentRecord{Type: "intent", Seq: s.seq + 1, Kind: kind, InstrumentID: instrument, Side: side, Quantity: quantity, LimitPrice: limitPrice})
}

// Log records a structured event.
func (s *Session) Log(event string, fields map[string]string) error {
	if err := CheckLog(event, fields); err != nil {
		return err
	}
	return s.write(logRecord{Type: "log", Seq: s.seq + 1, Event: event, Fields: fields})
}

func (s *Session) write(rec any) error {
	if s.failure != nil {
		return s.failure
	}
	b, err := json.Marshal(rec)
	if err != nil {
		s.failure = err
		return err
	}
	b = append(b, '\n')
	if _, err := s.out.Write(b); err != nil {
		s.failure = err
		return err
	}
	s.seq++
	return nil
}

// Run is a strategy's entry point: it reads the input document from stdin,
// calls strategy with a Session, writes the records and exits. A strategy
// error, an invalid input document or a failed write ends the process with a
// non-zero status, and the host then discards every proposal of the run.
func Run(strategy func(*Session) error) {
	os.Exit(run(os.Stdin, os.Stdout, os.Stderr, strategy))
}

func run(in io.Reader, out, diag io.Writer, strategy func(*Session) error) int {
	data, err := io.ReadAll(io.LimitReader(in, maxInputBytes+1))
	if err != nil || len(data) > maxInputBytes {
		report(diag, "input unreadable")
		return exitInputInvalid
	}
	snap, err := DecodeSnapshot(data)
	if err != nil {
		report(diag, err.Error())
		return exitInputInvalid
	}
	s := newSession(snap, out)
	if err := strategy(s); err != nil {
		report(diag, "strategy error: "+err.Error())
		return exitStrategyError
	}
	if s.failure != nil {
		report(diag, "output failed: "+s.failure.Error())
		return exitOutputFailed
	}
	if err := s.out.Flush(); err != nil {
		report(diag, "output failed: "+err.Error())
		return exitOutputFailed
	}
	return exitOK
}

func report(diag io.Writer, msg string) {
	if len(msg) > 512 {
		msg = msg[:512]
	}
	_, _ = io.WriteString(diag, "nodalsdk: "+strconv.Quote(msg)+"\n")
}
