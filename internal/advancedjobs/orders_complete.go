package advancedjobs

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/orders"
)

// OrdersCompleteTerminal completes the economic intents of orders that are
// already filled or cancelled (orders.Service.CompleteTerminal): the sweep
// that finishes a submission a crash interrupted between Execute and
// Complete. It moves only intent statuses -- nothing is posted.
type OrdersCompleteTerminal struct {
	db   *db.DB
	svc  *orders.Service
	spec Spec
}

// NewOrdersCompleteTerminal wires the job.
func NewOrdersCompleteTerminal(d *db.DB, svc *orders.Service, s Settings) *OrdersCompleteTerminal {
	return &OrdersCompleteTerminal{db: d, svc: svc, spec: spec(NameOrdersCompleteTerminal, s)}
}

// Spec implements Job.
func (j *OrdersCompleteTerminal) Spec() Spec { return j.spec }

// Run drains CompleteTerminal one intent per transaction until nothing is
// left or the batch is spent. CompleteTerminal skips intents another sweeper
// holds (SKIP LOCKED); a failure ends the drain, since the failing intent
// would be taken first again.
func (j *OrdersCompleteTerminal) Run(ctx context.Context) (Report, error) {
	var rep Report
	for rep.Examined < j.spec.Batch {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		var n int
		err := inUnit(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			n, err = j.svc.CompleteTerminal(ctx, tx, 1)
			return err
		})
		if err != nil {
			rep.Examined++
			rep.fail("next finished order", err)
			return rep, nil
		}
		if n == 0 {
			return rep, nil
		}
		rep.Examined += n
		rep.Changed += n
	}
	return rep, nil
}
