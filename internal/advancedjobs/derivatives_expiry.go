package advancedjobs

import (
	"context"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/derivatives"
	"github.com/nodal/controlplane/internal/instruments"
)

// DerivativesExpiry settles expired dated futures and options
// (derivatives.Service.SettleExpiry) that still have open positions.
//
// It settles ONLY at the price the subsystem derives itself -- a future at the
// last fresh mark at or before expiry, an option at that mark's index -- and
// never passes a settlement price override: the request it builds carries the
// instrument and a correlation id and nothing else. A contract without a fresh
// mark at expiry is refused by the subsystem (STALE_MARKET_DATA) and stays a
// failure every run until an operator settles it. derivative_settlements is
// UNIQUE per instrument, so a re-run is a replay that posts nothing.
type DerivativesExpiry struct {
	db    *db.DB
	svc   *derivatives.Service
	instr *instruments.Repository
	clk   clock.Clock
	spec  Spec
}

// NewDerivativesExpiry wires the job.
func NewDerivativesExpiry(d *db.DB, svc *derivatives.Service, instr *instruments.Repository, clk clock.Clock, s Settings) *DerivativesExpiry {
	return &DerivativesExpiry{db: d, svc: svc, instr: instr, clk: clk, spec: spec(NameDerivativesExpiry, s)}
}

// Spec implements Job.
func (j *DerivativesExpiry) Spec() Spec { return j.spec }

// settledInstrumentsSQL lists the instruments already finally settled.
// Read-only; derivatives exports no lister.
const settledInstrumentsSQL = `SELECT instrument_id FROM derivative_settlements`

// Run settles each expired contract in its own transaction, earliest expiry
// first.
func (j *DerivativesExpiry) Run(ctx context.Context) (Report, error) {
	now := j.clk.Now().UTC()
	listed, err := listInstruments(ctx, j.db, j.instr, instruments.TypeFuture, instruments.TypeOption)
	if err != nil {
		return Report{}, err
	}
	var open, settled map[instruments.InstrumentID]bool
	if err := readOnly(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if open, err = instrumentSet(ctx, tx, instrumentsWithOpenPositionsSQL); err != nil {
			return err
		}
		settled, err = instrumentSet(ctx, tx, settledInstrumentsSQL)
		return err
	}); err != nil {
		return Report{}, err
	}
	var due []instruments.Listed
	for _, l := range listed {
		expiry := l.Current.Terms.Expiry
		if expiry == nil || now.Before(*expiry) || settled[l.Instrument.ID] || !open[l.Instrument.ID] {
			continue
		}
		due = append(due, l)
	}
	sort.SliceStable(due, func(a, b int) bool { return due[a].Current.Terms.Expiry.Before(*due[b].Current.Terms.Expiry) })
	if len(due) > j.spec.Batch {
		due = due[:j.spec.Batch]
	}
	var rep Report
	for _, l := range due {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		rep.Examined++
		id := l.Instrument.ID
		var res derivatives.SettlementResult
		err := inUnit(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			// No Price, no PriceSource: the subsystem's own mark-derived price.
			res, err = j.svc.SettleExpiry(ctx, tx, derivatives.SettleRequest{
				InstrumentID: id, CorrelationID: "advanced-worker/" + NameDerivativesExpiry + "/" + id.String(),
			})
			return err
		})
		if err != nil {
			rep.fail(id.String(), err)
			continue
		}
		if !res.Replay {
			rep.Changed++
		}
	}
	return rep, nil
}
