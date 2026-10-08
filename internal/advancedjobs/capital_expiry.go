package advancedjobs

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
)

// CapitalExpiry expires capital reservations whose TTL has passed: an
// ACTIVE, unlocked reservation past expires_at moves to EXPIRED and hands its
// remainder back (capital.Service.ExpireDue). Without it a reservation that
// outlived the transaction that should have consumed it would hold buying
// power forever.
type CapitalExpiry struct {
	db   *db.DB
	svc  *capital.Service
	clk  clock.Clock
	spec Spec
}

// NewCapitalExpiry wires the job.
func NewCapitalExpiry(d *db.DB, svc *capital.Service, clk clock.Clock, s Settings) *CapitalExpiry {
	return &CapitalExpiry{db: d, svc: svc, clk: clk, spec: spec(NameCapitalExpiry, s)}
}

// Spec implements Job.
func (j *CapitalExpiry) Spec() Spec { return j.spec }

// Run drains ExpireDueBatch one reservation per transaction until nothing is
// due or the batch is spent. A reservation that cannot be expired is the
// subsystem's to isolate: ExpireDueBatch rolls it back alone, records it with
// a backoff and leaves it out until the backoff has passed, so the drain
// reports it as that reservation's failure and goes on to the next one.
// Only a transaction that fails as a whole -- the due reservations cannot be
// read, the retry budget is spent -- ends the drain, since its reservation
// would be taken first again. ExpireDueBatch skips rows another expirer holds
// (SKIP LOCKED) and never touches a reservation locked to an order.
func (j *CapitalExpiry) Run(ctx context.Context) (Report, error) {
	var rep Report
	for rep.Examined < j.spec.Batch {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		var batch capital.ExpiryBatch
		err := inUnit(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			batch, err = j.svc.ExpireDueBatch(ctx, tx, j.clk.Now(), 1)
			return err
		})
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return rep, ctxErr
			}
			rep.Examined++
			rep.fail("next due reservation", err)
			return rep, nil
		}
		if len(batch.Expired)+len(batch.Failures) == 0 {
			return rep, nil
		}
		rep.Examined += len(batch.Expired) + len(batch.Failures)
		rep.Changed += len(batch.Expired)
		for _, f := range batch.Failures {
			rep.fail("reservation "+f.ReservationID.String(), f.Err)
		}
	}
	return rep, nil
}
