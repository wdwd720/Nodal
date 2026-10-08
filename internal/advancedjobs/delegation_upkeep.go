package advancedjobs

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/delegation"
	"github.com/nodal/controlplane/internal/errs"
)

// DelegationUpkeep keeps delegated agent authority (internal/delegation) from
// standing still between requests. A run has two passes.
//
// Expiry. A pending swap (ADMITTED, AWAITING_APPROVAL or APPROVED) or
// withdrawal (AWAITING_APPROVAL or APPROVED) that can no longer be signed is
// ended by delegation.Service.ExpireStaleSwap / ExpireStaleWithdrawal, each
// in its own transaction: the reservation is released and the intent
// REJECTED. The ones past their deadline -- a swap's venue quote, a
// withdrawal's approval window -- are taken first, oldest deadline first;
// the rest of the batch is a paged sweep over every pending one, which finds
// those whose capability, account, agent, destination, gate or kill switch no
// longer stands. The job only finds candidates; whether one is stale is the
// package's own standing check, and one that still stands is left exactly as
// it is.
//
// Observation. delegation.Service.ReconcilePending then observes every
// SIGNED, SUBMITTED or UNKNOWN swap and withdrawal by transaction id through
// the devnet host -- settling what the chain finalized, failing what can no
// longer land, leaving the rest -- and never resubmits anything. It is called
// with produce set: in SANDBOX mode the platform is the chain's block
// producer, so the reconciler produces the blocks a submitted transaction's
// finality needs (and, for one merely SIGNED, only once it is stale, so it
// never ages out a transaction a caller is about to submit); a DEVNET-mode
// subject is only observed, its blocks coming from the chain's own producer.
// ReconcilePending owns its transactions and stops at the first subject it
// cannot handle; that is reported as the run's failure, and the subjects
// behind it wait for the next run.
//
// Kill switches: expiry follows the package's own rule -- a pending subject
// that a kill switch or a closed gate now stops is ended and its funds
// released, which reduces risk -- and observing and settling a signed or
// submitted one consults no switch: what the chain finalized is booked. The
// job adds no switch and bypasses none, and it never signs, submits or
// approves anything.
//
// delegation records no network on a swap or withdrawal: the package keeps
// one sandbox network per database, so this worker must name the same
// CP_SANDBOX_DEVNET_NETWORK as every binary that signs delegated
// transactions, or it would observe their transactions on a chain that has
// never seen them.
type DelegationUpkeep struct {
	db          *db.DB
	svc         *delegation.Service
	clk         clock.Clock
	spec        Spec
	swaps       pager
	withdrawals pager
}

// NewDelegationUpkeep wires the job.
func NewDelegationUpkeep(d *db.DB, svc *delegation.Service, clk clock.Clock, s Settings) (*DelegationUpkeep, error) {
	if d == nil || svc == nil || clk == nil {
		return nil, errs.New(errs.CodeInternal, "advancedjobs: the delegation upkeep needs the database, the service and a clock")
	}
	return &DelegationUpkeep{db: d, svc: svc, clk: clk, spec: spec(NameDelegationUpkeep, s)}, nil
}

// Spec implements Job.
func (j *DelegationUpkeep) Spec() Spec { return j.spec }

// The candidate reads. Read-only; delegation exports no lister. The status
// sets are exactly the ones ExpireStaleSwap / ExpireStaleWithdrawal act on.
const (
	// dueSwapsSQL: pending swaps whose quote has expired, oldest deadline
	// first.
	dueSwapsSQL = `SELECT id FROM delegated_swaps
		WHERE status IN ('ADMITTED','AWAITING_APPROVAL','APPROVED') AND quote_expires_at <= $1
		ORDER BY quote_expires_at, id LIMIT $2`
	// pendingSwapsSQL: every pending swap after a paging key, in id order.
	pendingSwapsSQL = `SELECT id FROM delegated_swaps
		WHERE status IN ('ADMITTED','AWAITING_APPROVAL','APPROVED') AND id > $1::uuid
		ORDER BY id LIMIT $2`
	// dueWithdrawalsSQL: withdrawals still waiting for a person after their
	// approval deadline, oldest deadline first.
	dueWithdrawalsSQL = `SELECT id FROM delegated_withdrawals
		WHERE status = 'AWAITING_APPROVAL' AND approval_expires_at <= $1
		ORDER BY approval_expires_at, id LIMIT $2`
	// pendingWithdrawalsSQL: every pending withdrawal after a paging key, in
	// id order.
	pendingWithdrawalsSQL = `SELECT id FROM delegated_withdrawals
		WHERE status IN ('AWAITING_APPROVAL','APPROVED') AND id > $1::uuid
		ORDER BY id LIMIT $2`
	// inFlightSwapsSQL and inFlightWithdrawalsSQL are what ReconcilePending
	// observes -- SIGNED, SUBMITTED and UNKNOWN, oldest first, as many as it
	// takes -- read before it runs so the run can say what moved.
	inFlightSwapsSQL = `SELECT id::text, status FROM delegated_swaps
		WHERE status IN ('SIGNED','SUBMITTED','UNKNOWN') ORDER BY created_at, id LIMIT $1`
	inFlightWithdrawalsSQL = `SELECT id::text, status FROM delegated_withdrawals
		WHERE status IN ('SIGNED','SUBMITTED','UNKNOWN') ORDER BY created_at, id LIMIT $1`
	swapStatusesSQL       = `SELECT id::text, status FROM delegated_swaps WHERE id = ANY($1::uuid[])`
	withdrawalStatusesSQL = `SELECT id::text, status FROM delegated_withdrawals WHERE id = ANY($1::uuid[])`
)

type swapUnit struct{ id delegation.SwapID }

func (u swapUnit) cursorKey() string { return u.id.String() }

type withdrawalUnit struct{ id delegation.WithdrawalID }

func (u withdrawalUnit) cursorKey() string { return u.id.String() }

// Run expires what is stale, then observes what is in flight.
func (j *DelegationUpkeep) Run(ctx context.Context) (Report, error) {
	var rep Report
	now := j.clk.Now()
	if err := j.expireSwaps(ctx, now, &rep); err != nil {
		return rep, err
	}
	if err := j.expireWithdrawals(ctx, now, &rep); err != nil {
		return rep, err
	}
	if err := j.observe(ctx, &rep); err != nil {
		return rep, err
	}
	return rep, nil
}

func (j *DelegationUpkeep) expireSwaps(ctx context.Context, now time.Time, rep *Report) error {
	var due []swapUnit
	if err := readOnly(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		due, err = scanSubjects[swapUnit](ctx, tx, dueSwapsSQL, now, j.spec.Batch)
		return err
	}); err != nil {
		return err
	}
	units := due
	if left := j.spec.Batch - len(due); left > 0 {
		swept, err := readPage(ctx, &j.swaps, j.db, left, func(ctx context.Context, q db.Querier, after string, limit int) ([]swapUnit, error) {
			return scanSubjects[swapUnit](ctx, q, pendingSwapsSQL, after, limit)
		})
		if err != nil {
			return err
		}
		units = append(units, swept...)
	}
	seen := make(map[delegation.SwapID]bool, len(units))
	for _, u := range units {
		if seen[u.id] {
			continue
		}
		seen[u.id] = true
		if err := ctx.Err(); err != nil {
			return err
		}
		rep.Examined++
		var out delegation.Swap
		err := inUnit(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			out, err = j.svc.ExpireStaleSwap(ctx, tx, u.id)
			return err
		})
		if err != nil {
			rep.fail("swap "+u.id.String(), err)
			continue
		}
		if out.Status == delegation.SwapRejected {
			rep.Changed++
		}
	}
	return nil
}

func (j *DelegationUpkeep) expireWithdrawals(ctx context.Context, now time.Time, rep *Report) error {
	var due []withdrawalUnit
	if err := readOnly(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		due, err = scanSubjects[withdrawalUnit](ctx, tx, dueWithdrawalsSQL, now, j.spec.Batch)
		return err
	}); err != nil {
		return err
	}
	units := due
	if left := j.spec.Batch - len(due); left > 0 {
		swept, err := readPage(ctx, &j.withdrawals, j.db, left, func(ctx context.Context, q db.Querier, after string, limit int) ([]withdrawalUnit, error) {
			return scanSubjects[withdrawalUnit](ctx, q, pendingWithdrawalsSQL, after, limit)
		})
		if err != nil {
			return err
		}
		units = append(units, swept...)
	}
	seen := make(map[delegation.WithdrawalID]bool, len(units))
	for _, u := range units {
		if seen[u.id] {
			continue
		}
		seen[u.id] = true
		if err := ctx.Err(); err != nil {
			return err
		}
		rep.Examined++
		var out delegation.Withdrawal
		err := inUnit(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			out, err = j.svc.ExpireStaleWithdrawal(ctx, tx, u.id)
			return err
		})
		if err != nil {
			rep.fail("withdrawal "+u.id.String(), err)
			continue
		}
		if out.Status == delegation.WithdrawalRejected {
			rep.Changed++
		}
	}
	return nil
}

// observe runs ReconcilePending over the subjects in flight and reports, of
// the ones it was going to take, how many moved and how many still wait on
// the chain.
func (j *DelegationUpkeep) observe(ctx context.Context, rep *Report) error {
	var swaps, withdrawals map[string]string
	if err := readOnly(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if swaps, err = statuses(ctx, tx, inFlightSwapsSQL, j.spec.Batch); err != nil {
			return err
		}
		withdrawals, err = statuses(ctx, tx, inFlightWithdrawalsSQL, j.spec.Batch)
		return err
	}); err != nil {
		return err
	}
	if len(swaps) == 0 && len(withdrawals) == 0 {
		// Nothing in flight: the pending pass ReconcilePending would also
		// make was this run's expiry pass.
		return nil
	}
	if _, err := j.svc.ReconcilePending(ctx, j.spec.Batch, true); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		rep.fail("reconcile_pending", err)
	}
	var swapsAfter, withdrawalsAfter map[string]string
	if err := readOnly(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if swapsAfter, err = statuses(ctx, tx, swapStatusesSQL, keys(swaps)); err != nil {
			return err
		}
		withdrawalsAfter, err = statuses(ctx, tx, withdrawalStatusesSQL, keys(withdrawals))
		return err
	}); err != nil {
		return err
	}
	tally := func(before, after map[string]string) {
		for id, was := range before {
			rep.Examined++
			now := after[id]
			switch {
			case now != was:
				rep.Changed++
			case now == "SIGNED" || now == "SUBMITTED" || now == "UNKNOWN":
				rep.Deferred++
			}
		}
	}
	tally(swaps, swapsAfter)
	tally(withdrawals, withdrawalsAfter)
	return nil
}

// scanSubjects reads one column of subject ids.
func scanSubjects[T swapUnit | withdrawalUnit](ctx context.Context, q db.Querier, sql string, args ...any) ([]T, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		var u T
		switch p := any(&u).(type) {
		case *swapUnit:
			err = rows.Scan(&p.id)
		case *withdrawalUnit:
			err = rows.Scan(&p.id)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// statuses reads (id, status) rows into a map.
func statuses(ctx context.Context, q db.Querier, sql string, arg any) (map[string]string, error) {
	rows, err := q.Query(ctx, sql, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, status string
		if err := rows.Scan(&id, &status); err != nil {
			return nil, err
		}
		out[id] = status
	}
	return out, rows.Err()
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
