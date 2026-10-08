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

// Run drains ExpireDue one reservation per transaction, so one reservation's
// failure never rolls back another's expiry, until nothing is due or the batch
// is spent. A failure ends the drain: ExpireDue takes the due reservations in
// a fixed order, so the failing one would be taken first again. ExpireDue
// skips rows another expirer holds (SKIP LOCKED) and never touches a
// reservation locked to an order.
func (j *CapitalExpiry) Run(ctx context.Context) (Report, error) {
	var rep Report
	for rep.Examined < j.spec.Batch {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		var expired []capital.ReservationID
		err := inUnit(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			expired, err = j.svc.ExpireDue(ctx, tx, j.clk.Now(), 1)
			return err
		})
		if err != nil {
			rep.Examined++
			rep.fail("next due reservation", err)
			return rep, nil
		}
		if len(expired) == 0 {
			return rep, nil
		}
		rep.Examined += len(expired)
		rep.Changed += len(expired)
	}
	return rep, nil
}
