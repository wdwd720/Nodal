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
// holds (SKIP LOCKED) and isolates an intent it cannot complete -- rolled
// back alone, recorded with a backoff and left out until the backoff has
// passed -- so the drain reports it as that intent's failure and goes on.
// Only a transaction that fails as a whole ends the drain.
func (j *OrdersCompleteTerminal) Run(ctx context.Context) (Report, error) {
	var rep Report
	for rep.Examined < j.spec.Batch {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		var batch orders.CompletionBatch
		err := inUnit(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			batch, err = j.svc.CompleteTerminal(ctx, tx, 1)
			return err
		})
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return rep, ctxErr
			}
			rep.Examined++
			rep.fail("next finished order", err)
			return rep, nil
		}
		if len(batch.Completed)+len(batch.Failures) == 0 {
			return rep, nil
		}
		rep.Examined += len(batch.Completed) + len(batch.Failures)
		rep.Changed += len(batch.Completed)
		for _, f := range batch.Failures {
			rep.fail("intent "+f.IntentID.String(), f.Err)
		}
	}
	return rep, nil
}
