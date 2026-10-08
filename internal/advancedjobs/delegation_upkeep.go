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
// taken first, oldest deadline first (delegation.Service.DueSwaps /
// DueWithdrawals); the rest of the batch is a paged sweep over every pending
// one (PendingSwaps / PendingWithdrawals), which finds those whose capability,
// account, agent, destination, gate or kill switch no longer stands. A pending
// subject whose network was never recorded is a candidate too: no service can
// sign it, so it is ended and its funds released. Which subjects are
// candidates is the package's selection and whether one is stale its own
// standing check: the job only pages and drives, and one that still stands is
// left exactly as it is. Neither selection can be held by subjects that
// stand: one past its deadline is ended when taken, and the sweep moves past
// every other.
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

type swapUnit struct{ id delegation.SwapID }

func (u swapUnit) cursorKey() string { return u.id.String() }

type withdrawalUnit struct{ id delegation.WithdrawalID }

func (u withdrawalUnit) cursorKey() string { return u.id.String() }

// pageAfter is the pager's position as a subject id: the start of the walk
// (nilUUID) is the zero id, which the selection reads as "from the first".
func pageAfter[K any](after string, parse func(string) (K, error)) (K, error) {
	var zero K
	if after == nilUUID {
		return zero, nil
	}
	return parse(after)
}

// Run expires what is stale, then observes what is in flight.
func (j *DelegationUpkeep) Run(ctx context.Context) (Report, error) {
	var rep Report
	now := j.clk.Now()
	swaps, err := j.swapCandidates(ctx, now)
	if err != nil {
		return rep, err
	}
	withdrawals, err := j.withdrawalCandidates(ctx, now)
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
func (j *DelegationUpkeep) swapCandidates(ctx context.Context, now time.Time) ([]delegation.SwapID, error) {
	var due []swapUnit
	if err := readOnly(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
		ids, err := j.svc.DueSwaps(ctx, tx, now, j.spec.Batch)
		due = swapUnits(ids)
		return err
	}); err != nil {
		return nil, err
	}
	units := due
	if left := j.spec.Batch - len(due); left > 0 {
		swept, err := readPage(ctx, &j.swaps, j.db, left, func(ctx context.Context, q db.Querier, after string, limit int) ([]swapUnit, error) {
			from, err := pageAfter(after, delegation.ParseSwapID)
			if err != nil {
				return nil, err
			}
			ids, err := j.svc.PendingSwaps(ctx, q, from, now, limit)
			return swapUnits(ids), err
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
func (j *DelegationUpkeep) withdrawalCandidates(ctx context.Context, now time.Time) ([]delegation.WithdrawalID, error) {
	var due []withdrawalUnit
	if err := readOnly(ctx, j.db, func(ctx context.Context, tx pgx.Tx) error {
		ids, err := j.svc.DueWithdrawals(ctx, tx, now, j.spec.Batch)
		due = withdrawalUnits(ids)
		return err
	}); err != nil {
		return nil, err
	}
	units := due
	if left := j.spec.Batch - len(due); left > 0 {
		swept, err := readPage(ctx, &j.withdrawals, j.db, left, func(ctx context.Context, q db.Querier, after string, limit int) ([]withdrawalUnit, error) {
			from, err := pageAfter(after, delegation.ParseWithdrawalID)
			if err != nil {
				return nil, err
			}
			ids, err := j.svc.PendingWithdrawals(ctx, q, from, now, limit)
			return withdrawalUnits(ids), err
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

// swapUnits wraps selected swap ids for the pager.
func swapUnits(ids []delegation.SwapID) []swapUnit {
	out := make([]swapUnit, 0, len(ids))
	for _, sid := range ids {
		out = append(out, swapUnit{id: sid})
	}
	return out
}

// withdrawalUnits wraps selected withdrawal ids for the pager.
func withdrawalUnits(ids []delegation.WithdrawalID) []withdrawalUnit {
	out := make([]withdrawalUnit, 0, len(ids))
	for _, wid := range ids {
		out = append(out, withdrawalUnit{id: wid})
	}
	return out
}
