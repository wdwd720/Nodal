package advancedjobs

import (
	"context"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/derivatives"
	"github.com/nodal/controlplane/internal/instruments"
)

// DerivativesVariationMargin runs variation margin
// (derivatives.Service.RunVariationMargin) for the latest completed interval
// of every unexpired future and perpetual with open positions.
//
// The interval is the run's identity -- its start on the policy's grid is the
// deterministic key -- and the subsystem claims it with a UNIQUE row, so a
// re-run is a replay that posts nothing. Variation margin moves each
// position's baseline to the mark, so the latest interval settles everything
// since the last run: an interval the worker missed is subsumed, never owed,
// and the subsystem refuses to run an earlier interval after a later one.
type DerivativesVariationMargin struct {
	db    *db.DB
	svc   *derivatives.Service
	instr *instruments.Repository
	clk   clock.Clock
	spec  Spec
	turn  pager
}

// NewDerivativesVariationMargin wires the job.
func NewDerivativesVariationMargin(d *db.DB, svc *derivatives.Service, instr *instruments.Repository, clk clock.Clock, s Settings) *DerivativesVariationMargin {
	return &DerivativesVariationMargin{db: d, svc: svc, instr: instr, clk: clk, spec: spec(NameDerivativesVariation, s)}
}

// Spec implements Job.
func (j *DerivativesVariationMargin) Spec() Spec { return j.spec }

// Run settles each instrument's latest completed interval in its own
// transaction.
func (j *DerivativesVariationMargin) Run(ctx context.Context) (Report, error) {
	now := j.clk.Now().UTC()
	listed, err := listInstruments(ctx, j.db, j.instr, instruments.TypeFuture, instruments.TypePerpetual)
	if err != nil {
		return Report{}, err
	}
	var open map[instruments.InstrumentID]bool
	if err := readOnly(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		open, err = instrumentSet(ctx, tx, instrumentsWithOpenPositionsSQL)
		return err
	}); err != nil {
		return Report{}, err
	}
	var due []keyedInstrument
	for _, l := range listed {
		expiry := l.Current.Terms.Expiry
		if !open[l.Instrument.ID] || (expiry != nil && !now.Before(*expiry)) {
			// No position to settle, or expired: an expired future is settled
			// by the final settlement, never by variation margin.
			continue
		}
		due = append(due, keyedInstrument{l})
	}
	j.turn.mu.Lock()
	due = rotate(due, j.turn.after)
	j.turn.mu.Unlock()
	if len(due) > j.spec.Batch {
		due = due[:j.spec.Batch]
	}
	start := latestCompleted(now, j.svc.Policy().VariationMarginIntervalMS)
	corr := "advanced-worker/" + NameDerivativesVariation + "/" + strconv.FormatInt(start.UnixMilli(), 10)
	var rep Report
	for _, l := range due {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		rep.Examined++
		id := l.Instrument.ID
		var res derivatives.RunResult
		err := inUnit(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			res, err = j.svc.RunVariationMargin(ctx, tx, derivatives.RunRequest{InstrumentID: id, IntervalStart: start, CorrelationID: corr})
			return err
		})
		j.turn.mu.Lock()
		j.turn.after = id.String()
		j.turn.mu.Unlock()
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
