package advancedjobs

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/predmarket"
	"github.com/nodal/controlplane/internal/reconciliation"
)

// PredictionEscrow runs reconciliation kind PREDICTION_ESCROW
// (predmarket.Service.ReconcileEscrow) for every market that holds escrow:
// escrow = collateral = shares outstanding + disclosed residual, and shares
// issued = outstanding = held by customers. A mismatch opens or refreshes a
// material MISMATCH record; a match is recorded as MATCHED (the subsystem
// opens one per matched pass, which is why the default interval is an hour).
type PredictionEscrow struct {
	db    *db.DB
	svc   *predmarket.Service
	spec  Spec
	pages pager
}

// NewPredictionEscrow wires the job.
func NewPredictionEscrow(d *db.DB, svc *predmarket.Service, s Settings) *PredictionEscrow {
	return &PredictionEscrow{db: d, svc: svc, spec: spec(NamePredictionEscrow, s)}
}

// Spec implements Job.
func (j *PredictionEscrow) Spec() Spec { return j.spec }

type marketUnit struct{ id predmarket.MarketID }

func (u marketUnit) cursorKey() string { return u.id.String() }

// escrowMarketsSQL lists the markets that can hold escrow -- OPEN through
// RESOLVED or DISPUTED; a DRAFT market has none yet and a SETTLED or VOIDED
// one has paid it out -- after a paging key, in id order. Read-only;
// predmarket exports no lister.
const escrowMarketsSQL = `SELECT id FROM prediction_markets
	WHERE status IN ('OPEN','CLOSED','RESOLVING','RESOLVED','DISPUTED') AND id > $1::uuid
	ORDER BY id LIMIT $2`

// Run reconciles each market in its own transaction.
func (j *PredictionEscrow) Run(ctx context.Context) (Report, error) {
	units, err := readPage(ctx, &j.pages, j.db, j.spec.Batch, func(ctx context.Context, q db.Querier, after string, limit int) ([]marketUnit, error) {
		rows, err := q.Query(ctx, escrowMarketsSQL, after, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []marketUnit
		for rows.Next() {
			var u marketUnit
			if err := rows.Scan(&u.id); err != nil {
				return nil, err
			}
			out = append(out, u)
		}
		return out, rows.Err()
	})
	if err != nil {
		return Report{}, err
	}
	var rep Report
	for _, u := range units {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		rep.Examined++
		var rec reconciliation.Record
		err := inUnit(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			rec, err = j.svc.ReconcileEscrow(ctx, tx, predmarket.ReconcileRequest{
				MarketID: u.id, Mode: reconciliation.ModePeriodic, Actor: workerReconActor(),
				Reason: "scheduled prediction market escrow reconciliation",
			})
			return err
		})
		if err != nil {
			rep.fail(u.id.String(), err)
			continue
		}
		if rec.Status != reconciliation.StatusMatched {
			rep.Changed++
		}
	}
	return rep, nil
}
