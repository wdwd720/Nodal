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
// standing still between requests. A run has two passes, both over the
// service's own devnet network: every swap and withdrawal records the network
// it was admitted on (migration 00760), and a subject of another network is
// never touched -- this worker's host would read its transaction as never
// seen.
//
// Expiry. A pending swap (ADMITTED, AWAITING_APPROVAL or APPROVED) or
// withdrawal (AWAITING_APPROVAL or APPROVED) that can no longer be signed is
// ended by delegation.Service.ExpireStale (ExpireStaleSwap /
// ExpireStaleWithdrawal, each subject in its own transaction): the
// reservation is released and the intent REJECTED. The ones past their
// deadline -- a swap's venue quote, a withdrawal's approval window -- are
// taken first, oldest deadline first; the rest of the batch is a paged sweep
// over every pending one, which finds those whose capability, account, agent,
// destination, gate or kill switch no longer stands. A pending subject whose
// network was never recorded is a candidate too: no service can sign it, so
// it is ended and its funds released. The job only finds candidates; whether
// one is stale is the package's own standing check, and one that still stands
// is left exactly as it is.
//
// Observation. delegation.Service.ObserveInFlight then observes the SIGNED,
// SUBMITTED and UNKNOWN swaps and withdrawals by transaction id through the
// devnet host -- settling what the chain finalized, failing what can no
// longer land, leaving the rest -- and never resubmits anything. It is called
// with produce set: in SANDBOX mode the platform is the chain's block
// producer, so the reconciler produces the blocks a submitted transaction's
// finality needs (and, for one merely SIGNED, only once it is stale, so it
// never ages out a transaction a caller is about to submit); a DEVNET-mode
// subject is only observed, its blocks coming from the chain's own producer.
//
// A subject either pass cannot handle is the package's to isolate: it is
// recorded in the upkeep failure ledger with a backoff, reported here as that
// subject's failure, and every other subject is handled. Until its backoff
// has passed it is left out of both passes, so a subject that keeps failing
// never fills a batch.
//
// Kill switches: expiry follows the package's own rule -- a pending subject
// that a kill switch or a closed gate now stops is ended and its funds
// released, which reduces risk -- and observing and settling a signed or
// submitted one consults no switch: what the chain finalized is booked. The
// job adds no switch and bypasses none, and it never signs, submits or
// approves anything.
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
// sets are exactly the ones ExpireStaleSwap / ExpireStaleWithdrawal act on;
// a subject of another network is never a candidate, and one whose expiry
// failed is left out while the package's backoff runs (upkeep_failures,
// operation $4).
const (
	// dueSwapsSQL: pending swaps whose quote has expired, oldest deadline
	// first. $1 now, $2 limit, $3 network, $4 operation.
	dueSwapsSQL = `SELECT id FROM delegated_swaps
		WHERE status IN ('ADMITTED','AWAITING_APPROVAL','APPROVED') AND quote_expires_at <= $1
		  AND (network = $3 OR network IS NULL)
		  AND NOT EXISTS (SELECT 1 FROM upkeep_failures uf WHERE uf.operation = $4 AND uf.item_id = delegated_swaps.id
		                    AND uf.cleared_at IS NULL AND uf.retry_after > $1 AND uf.last_failed_at <= $1)
		ORDER BY quote_expires_at, id LIMIT $2`
	// pendingSwapsSQL: every pending swap after a paging key, in id order.
	// $1 paging key, $2 limit, $3 network, $4 operation, $5 now.
	pendingSwapsSQL = `SELECT id FROM delegated_swaps
		WHERE status IN ('ADMITTED','AWAITING_APPROVAL','APPROVED') AND id > $1::uuid
		  AND (network = $3 OR network IS NULL)
		  AND NOT EXISTS (SELECT 1 FROM upkeep_failures uf WHERE uf.operation = $4 AND uf.item_id = delegated_swaps.id
		                    AND uf.cleared_at IS NULL AND uf.retry_after > $5 AND uf.last_failed_at <= $5)
		ORDER BY id LIMIT $2`
	// dueWithdrawalsSQL: withdrawals still waiting for a person after their
	// approval deadline, oldest deadline first. $1 now, $2 limit, $3 network,
	// $4 operation.
	dueWithdrawalsSQL = `SELECT id FROM delegated_withdrawals
		WHERE status = 'AWAITING_APPROVAL' AND approval_expires_at <= $1
		  AND (network = $3 OR network IS NULL)
		  AND NOT EXISTS (SELECT 1 FROM upkeep_failures uf WHERE uf.operation = $4 AND uf.item_id = delegated_withdrawals.id
		                    AND uf.cleared_at IS NULL AND uf.retry_after > $1 AND uf.last_failed_at <= $1)
		ORDER BY approval_expires_at, id LIMIT $2`
	// pendingWithdrawalsSQL: every pending withdrawal after a paging key, in
	// id order. $1 paging key, $2 limit, $3 network, $4 operation, $5 now.
	pendingWithdrawalsSQL = `SELECT id FROM delegated_withdrawals
		WHERE status IN ('AWAITING_APPROVAL','APPROVED') AND id > $1::uuid
		  AND (network = $3 OR network IS NULL)
		  AND NOT EXISTS (SELECT 1 FROM upkeep_failures uf WHERE uf.operation = $4 AND uf.item_id = delegated_withdrawals.id
		                    AND uf.cleared_at IS NULL AND uf.retry_after > $5 AND uf.last_failed_at <= $5)
		ORDER BY id LIMIT $2`
)

type swapUnit struct{ id delegation.SwapID }

func (u swapUnit) cursorKey() string { return u.id.String() }

type withdrawalUnit struct{ id delegation.WithdrawalID }

func (u withdrawalUnit) cursorKey() string { return u.id.String() }

// Run expires what is stale, then observes what is in flight.
func (j *DelegationUpkeep) Run(ctx context.Context) (Report, error) {
	var rep Report
	now := j.clk.Now()
	network := j.svc.Network()
	swaps, err := j.swapCandidates(ctx, now, network)
	if err != nil {
		return rep, err
	}
	withdrawals, err := j.withdrawalCandidates(ctx, now, network)
	if err != nil {
		return rep, err
	}
	expired, err := j.svc.ExpireStale(ctx, swaps, withdrawals)
	rep.Examined += expired.Pending
	rep.Changed += expired.Ended
	failPending(&rep, expired.Failures)
	if err != nil {
		return rep, err
	}
	observed, err := j.svc.ObserveInFlight(ctx, j.spec.Batch, true)
	rep.Examined += observed.InFlight
	rep.Changed += observed.Moved
	rep.Deferred += observed.Waiting
	failPending(&rep, observed.Failures)
	return rep, err
}

// failPending reports each subject a pass could not handle under the unit
// name the expiry pass has always used ("swap <id>", "withdrawal <id>").
func failPending(rep *Report, failures []delegation.PendingFailure) {
	for _, f := range failures {
		unit := "swap "
		if f.Kind == delegation.SubjectWithdrawal {
			unit = "withdrawal "
		}
		rep.fail(unit+f.SubjectID, f.Err)
	}
}

// swapCandidates is the swaps past their deadline, then a page of the rest,
// each once.
func (j *DelegationUpkeep) swapCandidates(ctx context.Context, now time.Time, network string) ([]delegation.SwapID, error) {
	var due []swapUnit
	if err := readOnly(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		due, err = scanSubjects[swapUnit](ctx, tx, dueSwapsSQL, now, j.spec.Batch, network, delegation.UpkeepExpireSwap)
		return err
	}); err != nil {
		return nil, err
	}
	units := due
	if left := j.spec.Batch - len(due); left > 0 {
		swept, err := readPage(ctx, &j.swaps, j.db, left, func(ctx context.Context, q db.Querier, after string, limit int) ([]swapUnit, error) {
			return scanSubjects[swapUnit](ctx, q, pendingSwapsSQL, after, limit, network, delegation.UpkeepExpireSwap, now)
		})
		if err != nil {
			return nil, err
		}
		units = append(units, swept...)
	}
	seen := make(map[delegation.SwapID]bool, len(units))
	out := make([]delegation.SwapID, 0, len(units))
	for _, u := range units {
		if !seen[u.id] {
			seen[u.id] = true
			out = append(out, u.id)
		}
	}
	return out, nil
}

// withdrawalCandidates is the withdrawals past their approval deadline, then
// a page of the rest, each once.
func (j *DelegationUpkeep) withdrawalCandidates(ctx context.Context, now time.Time, network string) ([]delegation.WithdrawalID, error) {
	var due []withdrawalUnit
	if err := readOnly(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		due, err = scanSubjects[withdrawalUnit](ctx, tx, dueWithdrawalsSQL, now, j.spec.Batch, network, delegation.UpkeepExpireWithdrawal)
		return err
	}); err != nil {
		return nil, err
	}
	units := due
	if left := j.spec.Batch - len(due); left > 0 {
		swept, err := readPage(ctx, &j.withdrawals, j.db, left, func(ctx context.Context, q db.Querier, after string, limit int) ([]withdrawalUnit, error) {
			return scanSubjects[withdrawalUnit](ctx, q, pendingWithdrawalsSQL, after, limit, network, delegation.UpkeepExpireWithdrawal, now)
		})
		if err != nil {
			return nil, err
		}
		units = append(units, swept...)
	}
	seen := make(map[delegation.WithdrawalID]bool, len(units))
	out := make([]delegation.WithdrawalID, 0, len(units))
	for _, u := range units {
		if !seen[u.id] {
			seen[u.id] = true
			out = append(out, u.id)
		}
	}
	return out, nil
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
