package advancedjobs

import (
	"context"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/margin"
)

// MarginLiquidation works the liquidation queue
// (margin.Service.ProcessLiquidationQueue): one deterministic step for each of
// up to a batch of accounts in MARGIN_CALL or with an open case, most
// under-margined first. The queue is a driver that owns its transactions --
// the ranking is read in one read-only transaction and every account's step
// commits in its own -- so this job hands it the database and reports what it
// did.
type MarginLiquidation struct {
	db   *db.DB
	svc  *margin.Service
	spec Spec
}

// NewMarginLiquidation wires the job.
func NewMarginLiquidation(d *db.DB, svc *margin.Service, s Settings) *MarginLiquidation {
	return &MarginLiquidation{db: d, svc: svc, spec: spec(NameMarginLiquidation, s)}
}

// Spec implements Job.
func (j *MarginLiquidation) Spec() Spec { return j.spec }

// Run processes one page of the queue. A result changed something when it
// executed a step or reached a decision that moves the case: completion,
// bankruptcy or manual review; a case left waiting for a price or a venue
// changed nothing.
func (j *MarginLiquidation) Run(ctx context.Context) (Report, error) {
	res, err := j.svc.ProcessLiquidationQueue(ctx, j.db, j.spec.Batch)
	if err != nil {
		return Report{}, err
	}
	rep := Report{Examined: len(res.Results) + len(res.Failures)}
	for _, r := range res.Results {
		switch {
		case r.Replayed:
		case r.Step != nil:
			rep.Changed++
		case r.Eligible && (r.Decision == margin.DecisionComplete || r.Decision == margin.DecisionBankrupt ||
			r.Decision == margin.DecisionManualReview):
			rep.Changed++
		case r.Decision == margin.DecisionAwaitPrice || r.Decision == margin.DecisionAwaitVenue:
			rep.Deferred++
		}
	}
	for _, f := range res.Failures {
		rep.fail(f.AccountID.String(), f.Err)
	}
	return rep, nil
}
