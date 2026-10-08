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

// Run calls SettleDue -- one account's due settlements per transaction, the
// way its documentation asks a worker to -- until it returns nothing or the
// batch of accounts is spent. A failure ends the run: SettleDue starts from
// the oldest due settlement no other worker holds, so the failing account
// would be taken first again.
func (j *EquitiesSettlement) Run(ctx context.Context) (Report, error) {
	var rep Report
	for accountsDone := 0; accountsDone < j.spec.Batch; accountsDone++ {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		var settled []equities.Settlement
		err := inUnit(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			settled, err = j.svc.SettleDue(ctx, tx, equitiesSettleLimit)
			return err
		})
		if err != nil {
			rep.Examined++
			rep.fail("next due account", err)
			return rep, nil
		}
		if len(settled) == 0 {
			return rep, nil
		}
		rep.Examined += len(settled)
		rep.Changed += len(settled)
	}
	return rep, nil
}
