package advancedjobs

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/equities"
)

// equitiesSettleLimit is the most settlements of one account one SettleDue
// call takes (the subsystem's own bound).
const equitiesSettleLimit = 500

// EquitiesSettlement settles equity fills on their settlement date
// (equities.Service.SettleDue): EQUITY_SETTLEMENT moves what a fill bought or
// raised out of UNSETTLED_RECEIVABLE into the WALLET at T+1 on the New York
// trading calendar -- not before -- and completes the order's intent. Each
// settlement is an exactly-once effect keyed on its fill.
type EquitiesSettlement struct {
	db   *db.DB
	svc  *equities.Service
	spec Spec
}

// NewEquitiesSettlement wires the job.
func NewEquitiesSettlement(d *db.DB, svc *equities.Service, s Settings) *EquitiesSettlement {
	return &EquitiesSettlement{db: d, svc: svc, spec: spec(NameEquitiesSettlement, s)}
}

// Spec implements Job.
func (j *EquitiesSettlement) Spec() Spec { return j.spec }

// Run calls SettleDueBatch -- one account's due settlements per transaction,
// the way its documentation asks a worker to -- until it takes on nothing or
// the batch of accounts is spent. SettleDueBatch isolates a settlement it
// cannot settle -- rolled back alone, recorded with a backoff and left out
// until the backoff has passed -- so the run reports it as that settlement's
// failure and goes on to the next account. Only a transaction that fails as
// a whole ends the run.
func (j *EquitiesSettlement) Run(ctx context.Context) (Report, error) {
	var rep Report
	for accountsDone := 0; accountsDone < j.spec.Batch; accountsDone++ {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		var batch equities.SettlementBatch
		err := inUnit(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			batch, err = j.svc.SettleDueBatch(ctx, tx, equitiesSettleLimit)
			return err
		})
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return rep, ctxErr
			}
			rep.Examined++
			rep.fail("next due account", err)
			return rep, nil
		}
		if batch.Examined() == 0 {
			return rep, nil
		}
		rep.Examined += batch.Examined()
		rep.Changed += len(batch.Settled)
		for _, f := range batch.Failures {
			rep.fail("settlement "+f.SettlementID.String(), f.Err)
		}
	}
	return rep, nil
}
