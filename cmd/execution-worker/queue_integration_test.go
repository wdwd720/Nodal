//go:build integration

package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/settlement"
)

// queueAt builds a queue whose clock the test controls.
func queueAt(d *db.DB, now *time.Time) *PGPlanQueue {
	return NewPGPlanQueue(d, func() time.Time { return now.UTC() })
}

func claimedIDs(leases []Lease) map[settlement.PlanID]bool {
	out := map[settlement.PlanID]bool{}
	for _, l := range leases {
		out[l.PlanID] = true
	}
	return out
}

func TestIntegration_Queue_ClaimsApprovedPlansOnce(t *testing.T) {
	d := openTestDB(t)
	w := newWorld(t, d)
	plan := w.approvedPlan(t)
	now := time.Now().UTC()
	q := queueAt(d, &now)

	first, err := q.Claim(t.Context(), "worker-a", 50, time.Minute)
	require.NoError(t, err)
	require.True(t, claimedIDs(first)[plan.ID], "an APPROVED, non-dry-run plan is claimable")

	// While the lease is live nobody else may take it.
	second, err := q.Claim(t.Context(), "worker-b", 50, time.Minute)
	require.NoError(t, err)
	assert.False(t, claimedIDs(second)[plan.ID], "a live lease is exclusive")

	// Renewal by the holder works; renewal by anybody else does not.
	ok, err := q.Renew(t.Context(), Lease{PlanID: plan.ID, Owner: "worker-a"}, time.Minute)
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = q.Renew(t.Context(), Lease{PlanID: plan.ID, Owner: "worker-b"}, time.Minute)
	require.NoError(t, err)
	assert.False(t, ok, "a lease can only be renewed by its owner")

	// After the lease lapses the plan returns to the fleet.
	now = now.Add(2 * time.Minute)
	third, err := q.Claim(t.Context(), "worker-b", 50, time.Minute)
	require.NoError(t, err)
	require.True(t, claimedIDs(third)[plan.ID], "an expired lease is reclaimable")
	for _, l := range third {
		if l.PlanID == plan.ID {
			assert.Equal(t, int32(2), l.Attempts, "attempts count every claim")
		}
	}

	// The previous owner can no longer write to the lease.
	require.NoError(t, q.Release(t.Context(), Lease{PlanID: plan.ID, Owner: "worker-a"}, OutcomeFailed, nil, 0))
	var owner string
	require.NoError(t, d.QueryRow(t.Context(), `SELECT owner FROM execution_plan_leases WHERE plan_id = $1`, plan.ID).Scan(&owner))
	assert.Equal(t, "worker-b", owner, "a stale owner's release is a no-op")
}

func TestIntegration_Queue_ReleaseSchedulesTheNextAttempt(t *testing.T) {
	d := openTestDB(t)
	w := newWorld(t, d)
	plan := w.approvedPlan(t)
	now := time.Now().UTC()
	q := queueAt(d, &now)

	leases, err := q.Claim(t.Context(), "worker-a", 50, time.Minute)
	require.NoError(t, err)
	held := findLease(t, leases, plan.ID)
	require.NoError(t, q.Release(t.Context(), held, OutcomePaused, context.DeadlineExceeded, 10*time.Minute))

	var outcome, lastErr string
	require.NoError(t, d.QueryRow(t.Context(),
		`SELECT last_outcome, coalesce(last_error,'') FROM execution_plan_leases WHERE plan_id = $1`, plan.ID).Scan(&outcome, &lastErr))
	assert.Equal(t, string(OutcomePaused), outcome)
	assert.NotEmpty(t, lastErr)

	// Backed off: not claimable until the deadline passes, then claimable.
	again, err := q.Claim(t.Context(), "worker-b", 50, time.Minute)
	require.NoError(t, err)
	assert.False(t, claimedIDs(again)[plan.ID], "a backed-off plan is not re-claimed early")
	now = now.Add(11 * time.Minute)
	again, err = q.Claim(t.Context(), "worker-b", 50, time.Minute)
	require.NoError(t, err)
	assert.True(t, claimedIDs(again)[plan.ID], "the plan returns once the backoff elapses")
}

func TestIntegration_Queue_SkipsDryRunAndTerminalPlans(t *testing.T) {
	d := openTestDB(t)
	w := newWorld(t, d)
	ctx := t.Context()

	// A dry run must never be claimed by the live worker: previewing is the
	// dry-run executor's job and it holds no signer.
	dry, err := w.Planner.Plan(w.Input())
	require.NoError(t, err)
	dry.DryRun = true
	dry.AccountID = w.AccountID.String()
	dry.Hash, err = settlement.ComputeHash(dry)
	require.NoError(t, err)
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := w.plans.Create(ctx, tx, dry); err != nil {
			return err
		}
		_, err := w.plans.Approve(ctx, tx, dry.ID, "OPERATOR", "op-1", "approved")
		return err
	}))

	// A DRAFT plan is not runnable either.
	draft, err := w.Planner.Plan(w.Input())
	require.NoError(t, err)
	draft.Version = 2
	draft.AccountID = w.AccountID.String()
	draft.Hash, err = settlement.ComputeHash(draft)
	require.NoError(t, err)
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.plans.Create(ctx, tx, draft)
		return err
	}))

	now := time.Now().UTC()
	q := queueAt(d, &now)
	leases, err := q.Claim(ctx, "worker-a", 100, time.Minute)
	require.NoError(t, err)
	got := claimedIDs(leases)
	assert.False(t, got[dry.ID], "dry-run plans are never claimed")
	assert.False(t, got[draft.ID], "DRAFT plans are never claimed")
}

// Two workers racing for the same plans must partition them, never share one.
// This is the property that keeps a single plan from being executed twice.
func TestIntegration_Queue_ConcurrentClaimsAreExclusive(t *testing.T) {
	d := openTestDB(t)
	const workers = 8
	plans := map[settlement.PlanID]bool{}
	for range 4 {
		w := newWorld(t, d)
		plans[w.approvedPlan(t).ID] = true
	}
	now := time.Now().UTC()

	var (
		mu      sync.Mutex
		claims  = map[settlement.PlanID][]string{}
		wg      sync.WaitGroup
		barrier = make(chan struct{})
	)
	for i := range workers {
		owner := "racer-" + string(rune('a'+i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			q := queueAt(d, &now)
			<-barrier
			leases, err := q.Claim(context.Background(), owner, 100, time.Minute)
			assert.NoError(t, err)
			mu.Lock()
			defer mu.Unlock()
			for _, l := range leases {
				claims[l.PlanID] = append(claims[l.PlanID], owner)
			}
		}()
	}
	close(barrier)
	wg.Wait()

	for p := range plans {
		owners := claims[p]
		require.Len(t, owners, 1, "plan %s was claimed by %v; exactly one worker may hold it", p, owners)
	}
}

func findLease(t *testing.T, leases []Lease, p settlement.PlanID) Lease {
	t.Helper()
	for _, l := range leases {
		if l.PlanID == p {
			return l
		}
	}
	t.Fatalf("plan %s was not claimed", p)
	return Lease{}
}
