package advancedjobs

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/margin"
)

// MarginMarkToMarket marks margin accounts to market: each NORMAL, WARNING or
// MARGIN_CALL account is evaluated (margin.Service.Evaluate) and moves between
// those statuses, a margin call issued when equity falls below maintenance --
// which is what puts it on the liquidation queue.
//
// It drives Evaluate per account rather than margin.Service.MarkToMarket,
// whose unit Evaluate is, for two reasons. MarkToMarket evaluates the first
// accounts by id in one transaction, holding every account's row lock until
// the whole batch commits; and it starts at the lowest id every time, so a
// population larger than its batch would leave the same accounts unmarked
// forever. Here every account is its own transaction and the batch pages
// through the population (the same selection MarkToMarket makes, read with
// the documented SELECT below).
type MarginMarkToMarket struct {
	db    *db.DB
	svc   *margin.Service
	spec  Spec
	pages pager
}

// NewMarginMarkToMarket wires the job.
func NewMarginMarkToMarket(d *db.DB, svc *margin.Service, s Settings) *MarginMarkToMarket {
	return &MarginMarkToMarket{db: d, svc: svc, spec: spec(NameMarginMarkToMarket, s)}
}

// Spec implements Job.
func (j *MarginMarkToMarket) Spec() Spec { return j.spec }

// markableAccountsSQL is MarkToMarket's own selection -- the accounts of the
// service's policy in NORMAL, WARNING or MARGIN_CALL -- after a paging key,
// in id order. Read-only; margin exports no lister. An account whose case is
// open is LIQUIDATING and is the liquidation queue's, not this job's.
const markableAccountsSQL = `SELECT m.id::text, m.account_id FROM margin_accounts m
	WHERE m.policy_version = $1 AND m.status IN ('NORMAL','WARNING','MARGIN_CALL') AND m.id > $2::uuid
	ORDER BY m.id LIMIT $3`

// Run evaluates each account in its own transaction.
func (j *MarginMarkToMarket) Run(ctx context.Context) (Report, error) {
	units, err := readPage(ctx, &j.pages, j.db, j.spec.Batch, func(ctx context.Context, q db.Querier, after string, limit int) ([]marginUnit, error) {
		return scanMarginUnits(ctx, q, markableAccountsSQL, j.svc.Policy().Version, after, limit)
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
		var res margin.EvaluateResult
		err := inUnit(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			res, err = j.svc.Evaluate(ctx, tx, u.account)
			return err
		})
		if err != nil {
			rep.fail(u.account.String(), err)
			continue
		}
		if res.Status != res.Previous {
			rep.Changed++
		}
	}
	return rep, nil
}
