package advancedjobs

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/margin"
)

// MarginInterest accrues margin interest for every margin account holding an
// open loan lot (margin.Service.AccrueInterest). AccrueInterest charges
// complete UTC days only, each lot and period exactly once (the period is in
// the effect key and the database refuses a second accrual of it), so running
// it hourly -- or three times in a row -- charges a day once.
type MarginInterest struct {
	db    *db.DB
	svc   *margin.Service
	spec  Spec
	pages pager
}

// NewMarginInterest wires the job.
func NewMarginInterest(d *db.DB, svc *margin.Service, s Settings) *MarginInterest {
	return &MarginInterest{db: d, svc: svc, spec: spec(NameMarginInterest, s)}
}

// Spec implements Job.
func (j *MarginInterest) Spec() Spec { return j.spec }

// marginUnit is one margin account: its id (the paging key) and the customer
// account the margin operations take.
type marginUnit struct {
	id      string
	account accounts.AccountID
}

func (u marginUnit) cursorKey() string { return u.id }

// openLoanAccountsSQL lists the margin accounts of the service's policy that
// hold an open loan lot -- the accounts AccrueInterest has anything to charge
// -- after a paging key, in id order. Read-only; margin exports no lister.
// BANKRUPT and CLOSED accounts accrue nothing and are left out;
// AccrueInterest would answer them with nothing anyway.
const openLoanAccountsSQL = `SELECT m.id::text, m.account_id FROM margin_accounts m
	WHERE m.policy_version = $1 AND m.status NOT IN ('BANKRUPT','CLOSED') AND m.id > $2::uuid
	  AND EXISTS (SELECT 1 FROM margin_borrow_lots l WHERE l.margin_account_id = m.id AND l.closed_at IS NULL)
	ORDER BY m.id LIMIT $3`

// Run accrues each account in its own transaction.
func (j *MarginInterest) Run(ctx context.Context) (Report, error) {
	units, err := readPage(ctx, &j.pages, j.db, j.spec.Batch, func(ctx context.Context, q db.Querier, after string, limit int) ([]marginUnit, error) {
		return scanMarginUnits(ctx, q, openLoanAccountsSQL, j.svc.Policy().Version, after, limit)
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
		var accruals []margin.Accrual
		err := inUnit(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			accruals, err = j.svc.AccrueInterest(ctx, tx, u.account)
			return err
		})
		if err != nil {
			rep.fail(u.account.String(), err)
			continue
		}
		if len(accruals) > 0 {
			rep.Changed++
		}
	}
	return rep, nil
}

func scanMarginUnits(ctx context.Context, q db.Querier, sql, policy, after string, limit int) ([]marginUnit, error) {
	rows, err := q.Query(ctx, sql, policy, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []marginUnit
	for rows.Next() {
		var u marginUnit
		if err := rows.Scan(&u.id, &u.account); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
