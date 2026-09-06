//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/settlement"
	"github.com/nodal/controlplane/internal/settlement/settlementtest"
)

// runUntilIdle runs the worker loop until every claimable plan has been
// released at least once, or the deadline passes.
func runUntilIdle(t *testing.T, r *Runner, want int, within time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	deadline := time.After(within)
	for {
		s := r.Stats()
		settled := s.Completed + s.Failed + s.Paused + s.Retried + s.Abandoned
		if settled >= int64(want) {
			break
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("worker did not settle %d plans in %s (stats: %+v)", want, within, r.Stats())
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	require.NoError(t, <-done)
}

// assertExactlyOnce is the PART 46 assertion against the real tables: one
// landed transaction, one fill, one journal transaction for that fill, the
// position applied once, and the reservation fully settled.
func assertExactlyOnce(t *testing.T, w *world, planID settlement.PlanID) {
	t.Helper()
	ctx := t.Context()
	assert.Equal(t, 1, w.Chain.Landed(), "exactly one transaction on chain")

	o, err := w.orders.GetByPlan(ctx, w.d, planID.String())
	require.NoError(t, err)
	fills, err := w.orders.ListFills(ctx, w.d, o.ID)
	require.NoError(t, err)
	require.Len(t, fills, 1, "exactly one fill")
	assert.True(t, fills[0].Posted(), "the fill is posted to the ledger")
	assert.True(t, fills[0].PositionApplied(), "the fill moved positions once")

	var journals int
	require.NoError(t, w.d.QueryRow(ctx,
		`SELECT count(*) FROM journal_transactions WHERE reference_type = 'fill' AND reference_id = $1`,
		fills[0].ID.String()).Scan(&journals))
	assert.Equal(t, 1, journals, "exactly one journal transaction for the fill")

	var lots int
	require.NoError(t, w.d.QueryRow(ctx,
		`SELECT count(*) FROM position_lots WHERE account_id = $1 AND asset_id = $2`,
		w.AccountID, w.SOL.ID).Scan(&lots))
	assert.Equal(t, 1, lots, "exactly one position lot opened")

	rid, err := capital.ParseReservationID(o.ReservationID)
	require.NoError(t, err)
	res, err := w.capital.Get(ctx, w.d, rid)
	require.NoError(t, err)
	assert.Equal(t, capital.ReservationConsumed, res.Status, "the reservation is settled, not left hanging")

	assert.Equal(t, 1, w.Adapter.Submissions, "exactly one submission reached the venue")

	report, err := audit.NewVerifier().VerifyStream(ctx, w.d, audit.AccountStream(w.AccountID.String()))
	require.NoError(t, err)
	assert.True(t, report.OK, "the account audit chain verifies: %s", report.Reason)
}

func newWorkerRunner(t *testing.T, w *world, opts RunnerOptions, plans ...settlement.Plan) *Runner {
	t.Helper()
	if opts.Owner == "" {
		opts.Owner = "itest-worker"
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = 5 * time.Millisecond
	}
	if opts.DrainTimeout == 0 {
		opts.DrainTimeout = 20 * time.Second
	}
	q := scoped(NewPGPlanQueue(w.d, func() time.Time { return time.Now().UTC() }), plans...)
	r, err := NewRunner(q, w.executor(t), testLogger(t), clock.System(), opts)
	require.NoError(t, err)
	return r
}

// scopedQueue narrows a real PGPlanQueue to the plans one test owns. The
// isolated database is shared by every test in this package, and the real
// claim query is deliberately global, so without this a runner would pick up
// another test's plan and execute it against the wrong world's chain.
// Everything below the claim is the production path.
type scopedQueue struct {
	inner *PGPlanQueue
	mine  map[settlement.PlanID]bool
}

func scoped(q *PGPlanQueue, plans ...settlement.Plan) *scopedQueue {
	mine := make(map[settlement.PlanID]bool, len(plans))
	for _, p := range plans {
		mine[p.ID] = true
	}
	return &scopedQueue{inner: q, mine: mine}
}

func (s *scopedQueue) Claim(ctx context.Context, owner string, limit int, ttl time.Duration) ([]Lease, error) {
	leases, err := s.inner.Claim(ctx, owner, limit, ttl)
	if err != nil {
		return nil, err
	}
	kept := leases[:0]
	for _, l := range leases {
		if s.mine[l.PlanID] {
			kept = append(kept, l)
			continue
		}
		// Park somebody else's plan far enough out that this test run does
		// not keep re-claiming it.
		if rerr := s.inner.Release(ctx, l, OutcomeRetry, nil, time.Hour); rerr != nil {
			return nil, rerr
		}
	}
	return kept, nil
}

func (s *scopedQueue) Renew(ctx context.Context, l Lease, ttl time.Duration) (bool, error) {
	return s.inner.Renew(ctx, l, ttl)
}

func (s *scopedQueue) Release(ctx context.Context, l Lease, out Outcome, cause error, retryAfter time.Duration) error {
	return s.inner.Release(ctx, l, out, cause, retryAfter)
}

// testLogger sends the worker's structured logs to the test log, so a failing
// run explains itself instead of leaving an empty lease row to guess from.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", bytes.TrimRight(p, "\n"))
	return len(p), nil
}

// The whole point of the binary: claim an approved plan and settle it.
func TestIntegration_Worker_DrivesApprovedPlanToSettled(t *testing.T) {
	d := openTestDB(t)
	w := newWorld(t, d)
	plan := w.approvedPlan(t)

	r := newWorkerRunner(t, w, RunnerOptions{Owner: "itest-settle"}, plan)
	runUntilIdle(t, r, 1, 60*time.Second)

	stored, err := w.plans.Get(t.Context(), d, plan.ID)
	require.NoError(t, err)
	require.Equal(t, settlement.PlanCompleted, stored.Status)
	for _, s := range stored.Steps {
		require.Equal(t, settlement.StepSucceeded, s.State, "step %s: %s", s.Type, s.LastError)
	}
	o, err := w.orders.GetByPlan(t.Context(), d, plan.ID.String())
	require.NoError(t, err)
	assert.Equal(t, execution.OrderSettled, o.Status)
	assertExactlyOnce(t, w, plan.ID)
	assert.Equal(t, int64(1), r.Stats().Completed)

	// The lease records the outcome and the plan is no longer claimable.
	var outcome string
	require.NoError(t, d.QueryRow(t.Context(), `SELECT last_outcome FROM execution_plan_leases WHERE plan_id = $1`, plan.ID).Scan(&outcome))
	assert.Equal(t, string(OutcomeCompleted), outcome)
}

// TRANSPORT TIMEOUT IS NOT EXECUTION FAILURE (PART 48). The submission times
// out while the transaction actually lands. The worker must not call it a
// failure, must not submit again, must keep the reservation, and must resolve
// the truth by investigation.
func TestIntegration_Worker_SubmitTimeoutIsNotFailure(t *testing.T) {
	d := openTestDB(t)
	w := newWorld(t, d)
	w.Adapter.SubmitScript = []settlementtest.SubmitKind{settlementtest.SubmitTimeoutLanded}
	plan := w.approvedPlan(t)

	r := newWorkerRunner(t, w, RunnerOptions{Owner: "itest-timeout"}, plan)
	runUntilIdle(t, r, 1, 60*time.Second)

	// The recoverer found the landed transaction and it was adopted.
	assert.Equal(t, 1, w.Adapter.Calls[execution.MethodSubmit], "the timeout never produced a second submission")
	assert.Equal(t, 1, w.Recoverer.Calls, "the outcome was investigated, not assumed")
	assertExactlyOnce(t, w, plan.ID)

	stored, err := w.plans.Get(t.Context(), d, plan.ID)
	require.NoError(t, err)
	assert.Equal(t, settlement.PlanCompleted, stored.Status)

	// The order passed through SUBMISSION_UNKNOWN on the way, and the
	// transitions are on the record.
	o, err := w.orders.GetByPlan(t.Context(), d, plan.ID.String())
	require.NoError(t, err)
	transitions, err := w.orders.ListTransitions(t.Context(), d, o.ID)
	require.NoError(t, err)
	var sawUnknown bool
	for _, tr := range transitions {
		if tr.To == execution.OrderSubmissionUnknown {
			sawUnknown = true
		}
	}
	assert.True(t, sawUnknown, "a timed-out submission is recorded as SUBMISSION_UNKNOWN")
	assert.Equal(t, execution.OrderSettled, o.Status)
}

// When the investigation cannot resolve the submission, the worker must park
// the plan on the operator path — PAUSED, never FAILED — keep the
// reservation, and schedule a later re-investigation rather than a retry.
func TestIntegration_Worker_UnresolvedSubmissionIsPausedNotFailed(t *testing.T) {
	d := openTestDB(t)
	w := newWorld(t, d)
	w.Adapter.SubmitScript = []settlementtest.SubmitKind{settlementtest.SubmitTimeoutLost}
	// Neither proven present nor proven absent: the blockhash is still valid,
	// so the transaction could still land.
	plan := w.approvedPlan(t)

	r := newWorkerRunner(t, w, RunnerOptions{Owner: "itest-unresolved", PausedBackoff: 10 * time.Minute}, plan)
	runUntilIdle(t, r, 1, 60*time.Second)

	s := r.Stats()
	require.Equal(t, int64(1), s.Paused, "an unresolved submission pauses the plan (stats: %+v)", s)
	assert.Zero(t, s.Failed, "it is never a failure")

	o, err := w.orders.GetByPlan(t.Context(), d, plan.ID.String())
	require.NoError(t, err)
	assert.Equal(t, execution.OrderReconciliationRequired, o.Status)

	// The reservation is KEPT: nothing is given back while an external effect
	// may exist.
	rid, err := capital.ParseReservationID(o.ReservationID)
	require.NoError(t, err)
	res, err := w.capital.Get(t.Context(), d, rid)
	require.NoError(t, err)
	assert.Equal(t, capital.ReservationActive, res.Status, "the reservation is kept while the outcome is unknown")

	var outcome string
	var nextAttempt time.Time
	require.NoError(t, d.QueryRow(t.Context(),
		`SELECT last_outcome, next_attempt_at FROM execution_plan_leases WHERE plan_id = $1`, plan.ID).Scan(&outcome, &nextAttempt))
	assert.Equal(t, string(OutcomePaused), outcome)
	assert.True(t, nextAttempt.After(time.Now().Add(5*time.Minute)), "a paused plan waits for reconciliation, it does not spin")
	assert.Equal(t, 1, w.Adapter.Calls[execution.MethodSubmit], "still exactly one submission attempt, never a retry")
}

// Exactly-once economic effect under at-least-once delivery, end to end: the
// same event is delivered repeatedly, serially and concurrently, and the
// worker runs the plan again for every delivery. One fill, one posting, one
// position change.
func TestIntegration_Worker_DuplicateDeliveryHasExactlyOneEffect(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()

	// tolerable are the errors a losing racer legitimately sees when several
	// processes resume the same plan at once. None of them is a second
	// economic effect; each one is a guard doing its job.
	tolerable := func(t *testing.T, err error) {
		t.Helper()
		switch {
		case err == nil,
			errs.HasCode(err, errs.CodeConflict),               // lost a compare-and-set on a step or plan status
			errs.HasCode(err, errs.CodeInvalidStateTransition), // a peer already finished the plan
			errs.HasCode(err, errs.CodeIdempotencyInProgress),  // the inbox row is held by a peer
			db.IsRetryable(err):                                // Postgres broke a deadlock or a serialization cycle; the transaction rolled back
		default:
			t.Errorf("unexpected error from a concurrent resumption: %v", err)
		}
	}

	t.Run("serial redelivery of the same event", func(t *testing.T) {
		w := newWorld(t, d)
		plan := w.approvedPlan(t)
		ex := w.executor(t)
		inbox := event.NewInbox(clock.System())
		waker, err := NewWaker(d, inbox, nil)
		require.NoError(t, err)
		waker.Source = "execworker-serial-" + plan.ID.String()[:8]

		res, err := ex.Run(ctx, plan.ID)
		require.NoError(t, err)
		require.Equal(t, settlement.PlanCompleted, res.Status)
		o, err := w.orders.GetByPlan(ctx, d, plan.ID.String())
		require.NoError(t, err)
		msg := fillObservedMessage(t, w, plan, o)

		for range 3 {
			require.NoError(t, waker.Handle(ctx, msg))
			_, err := ex.Run(ctx, plan.ID)
			require.True(t, errs.HasCode(err, errs.CodeInvalidStateTransition),
				"a completed plan refuses to run again rather than repeating its effects: %v", err)
		}
		assertExactlyOnce(t, w, plan.ID)

		rec, ok, err := inbox.Get(ctx, d, waker.Source, msg.ID)
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, event.StatusProcessed, rec.Status, "the message was processed once and then deduplicated")
	})

	// The hard case. The plan is stopped with the transaction already on
	// chain and nothing recorded about it yet, then several processes resume
	// it at the same time — exactly what an expired lease plus a redelivered
	// event produces. Every one of them re-enters OBSERVE_FINALITY,
	// POST_LEDGER and UPDATE_POSITION.
	t.Run("concurrent resumption after a redelivered wake", func(t *testing.T) {
		w := newWorld(t, d)
		plan := w.approvedPlan(t)
		inbox := event.NewInbox(clock.System())
		waker, err := NewWaker(d, inbox, nil)
		require.NoError(t, err)
		waker.Source = "execworker-concurrent-" + plan.ID.String()[:8]

		deps := w.deps()
		deps.Hooks = settlement.Hooks{OnStep: func(_ context.Context, s settlement.Step, ph settlement.Phase) error {
			if s.Type == settlement.StepSubmit && ph == settlement.PhaseAfterPersist {
				return errStop
			}
			return nil
		}}
		stopped, err := settlement.NewExecutor(deps, w.Signer)
		require.NoError(t, err)
		_, err = stopped.Run(ctx, plan.ID)
		require.ErrorIs(t, err, errStop)
		require.Equal(t, 1, w.Chain.Landed(), "the transaction is on chain and not yet recorded")

		msg := planTransitionMessage(t, plan)
		const n = 8
		var wg sync.WaitGroup
		start := make(chan struct{})
		for range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ex := w.executor(t)
				<-start
				if err := waker.Handle(context.Background(), msg); err != nil {
					tolerable(t, err)
				}
				_, err := ex.Run(context.Background(), plan.ID)
				tolerable(t, err)
			}()
		}
		close(start)
		wg.Wait()

		// Some racers lose to a rollback (a deadlock, or a compare-and-set
		// another goroutine won). That is exactly what the worker's RETRY
		// outcome is for, so finish the plan the way the worker would.
		stored, err := w.plans.Get(ctx, d, plan.ID)
		require.NoError(t, err)
		if stored.Status != settlement.PlanCompleted {
			_, err = w.executor(t).Run(ctx, plan.ID)
			require.NoError(t, err, "the retry after a lost race completes the plan")
			stored, err = w.plans.Get(ctx, d, plan.ID)
			require.NoError(t, err)
		}
		require.Equal(t, settlement.PlanCompleted, stored.Status)
		assertExactlyOnce(t, w, plan.ID)

		// One inbox row, one processing, whatever the interleaving was.
		rec, ok, err := inbox.Get(ctx, d, waker.Source, msg.ID)
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, event.StatusProcessed, rec.Status)
	})
}

// A duplicate message must be recognized as a duplicate, not merely absorbed.
func TestIntegration_Worker_WakerDeduplicates(t *testing.T) {
	d := openTestDB(t)
	w := newWorld(t, d)
	plan := w.approvedPlan(t)
	ctx := t.Context()

	inbox := event.NewInbox(clock.System())
	waker, err := NewWaker(d, inbox, nil)
	require.NoError(t, err)
	waker.Source = "execution-worker-dedup-" + plan.ID.String()[:8]

	// Park the plan far in the future so the wake has something to undo.
	_, err = d.Exec(ctx, `INSERT INTO execution_plan_leases (plan_id, owner, leased_at, lease_expires_at, next_attempt_at)
		VALUES ($1, 'someone', now(), now(), now() + interval '1 hour')`, plan.ID)
	require.NoError(t, err)

	msg := planTransitionMessage(t, plan)
	require.NoError(t, waker.Handle(ctx, msg))
	var next time.Time
	require.NoError(t, d.QueryRow(ctx, `SELECT next_attempt_at FROM execution_plan_leases WHERE plan_id = $1`, plan.ID).Scan(&next))
	assert.False(t, next.After(time.Now().Add(time.Second)), "the wake brought the deadline forward")

	// Push it out again and redeliver: the duplicate must not act.
	_, err = d.Exec(ctx, `UPDATE execution_plan_leases SET next_attempt_at = now() + interval '1 hour' WHERE plan_id = $1`, plan.ID)
	require.NoError(t, err)
	require.NoError(t, waker.Handle(ctx, msg))
	require.NoError(t, d.QueryRow(ctx, `SELECT next_attempt_at FROM execution_plan_leases WHERE plan_id = $1`, plan.ID).Scan(&next))
	assert.True(t, next.After(time.Now().Add(30*time.Minute)), "a duplicate delivery has no effect at all")
}

// PART 52. A kill switch stops new risk and nothing else: a plan that has
// already submitted must still observe, reconcile, post the ledger, move
// positions and release its reservation.
func TestIntegration_Worker_KillSwitchHaltsTradingNotSettlement(t *testing.T) {
	d := openTestDB(t)

	t.Run("new risk is refused and the reservation is released", func(t *testing.T) {
		w := newWorld(t, d)
		w.KillSwitches.Activate(killswitch.GlobalNewRiskKill, "*")
		plan := w.approvedPlan(t)

		r := newWorkerRunner(t, w, RunnerOptions{Owner: "itest-kill-new"}, plan)
		runUntilIdle(t, r, 1, 60*time.Second)

		require.Equal(t, int64(1), r.Stats().Failed, "a switched-off plan fails promptly (stats: %+v)", r.Stats())
		assert.Zero(t, w.Adapter.Submissions, "nothing was submitted")
		stored, err := w.plans.Get(t.Context(), d, plan.ID)
		require.NoError(t, err)
		assert.Equal(t, settlement.PlanFailed, stored.Status)
		active, err := w.capital.ListActive(t.Context(), d, w.AccountID.String(), w.USDC.ID)
		require.NoError(t, err)
		assert.Empty(t, active, "no capital is left reserved behind a kill switch")
	})

	t.Run("settlement of an already-submitted plan is never halted", func(t *testing.T) {
		w := newWorld(t, d)
		plan := w.approvedPlan(t)

		// Stop the world immediately after SUBMIT persisted, exactly as a
		// crash between submission and settlement would.
		deps := w.deps()
		deps.Hooks = settlement.Hooks{OnStep: func(_ context.Context, s settlement.Step, ph settlement.Phase) error {
			if s.Type == settlement.StepSubmit && ph == settlement.PhaseAfterPersist {
				return errStop
			}
			return nil
		}}
		partial, err := settlement.NewExecutor(deps, w.Signer)
		require.NoError(t, err)
		_, err = partial.Run(t.Context(), plan.ID)
		require.ErrorIs(t, err, errStop)
		require.Equal(t, 1, w.Chain.Landed(), "the transaction is on chain")

		// Now the emergency: every switch on.
		w.KillSwitches.Activate(killswitch.GlobalNewRiskKill, "*")
		w.KillSwitches.Activate(killswitch.AccountFreeze, w.AccountID.String())
		w.KillSwitches.Activate(killswitch.VenueDisable, w.Venue.Code)

		r := newWorkerRunner(t, w, RunnerOptions{Owner: "itest-kill-settle"}, plan)
		runUntilIdle(t, r, 1, 60*time.Second)

		require.Equal(t, int64(1), r.Stats().Completed,
			"settlement of an executed transaction must complete under any kill switch (stats: %+v)", r.Stats())
		assertExactlyOnce(t, w, plan.ID)
		o, err := w.orders.GetByPlan(t.Context(), d, plan.ID.String())
		require.NoError(t, err)
		assert.Equal(t, execution.OrderSettled, o.Status)
	})
}

// Crash safety: killing the worker mid-plan leaves recoverable state, and the
// restart resumes without duplicating an external effect.
func TestIntegration_Worker_CrashResumesWithoutDuplicatingEffects(t *testing.T) {
	d := openTestDB(t)
	for _, phase := range []settlement.Phase{settlement.PhaseBeforeEffect, settlement.PhaseAfterEffect, settlement.PhaseAfterPersist} {
		t.Run(string(phase), func(t *testing.T) {
			w := newWorld(t, d)
			plan := w.approvedPlan(t)

			deps := w.deps()
			deps.Hooks = settlement.Hooks{OnStep: func(_ context.Context, s settlement.Step, ph settlement.Phase) error {
				if s.Type == settlement.StepSubmit && ph == phase {
					return errStop
				}
				return nil
			}}
			crashing, err := settlement.NewExecutor(deps, w.Signer)
			require.NoError(t, err)
			_, err = crashing.Run(t.Context(), plan.ID)
			require.ErrorIs(t, err, errStop, "the crash must fire")

			if w.Chain.Landed() == 0 {
				// The signed transaction never left the process; by the time
				// the worker restarts the blockhash window has passed, so
				// recovery proves absence instead of guessing.
				w.Chain.SetHeight(w.Chain.Height() + 1000)
			}

			// Restart: a brand new worker over the same durable state.
			r := newWorkerRunner(t, w, RunnerOptions{Owner: "itest-restart-" + string(phase)}, plan)
			runUntilIdle(t, r, 1, 60*time.Second)

			stored, err := w.plans.Get(t.Context(), d, plan.ID)
			require.NoError(t, err)
			require.Equal(t, settlement.PlanCompleted, stored.Status, "the restart resumed to completion")
			assertExactlyOnce(t, w, plan.ID)
			assert.LessOrEqual(t, w.Adapter.Calls[execution.MethodSubmit], 1, "never more than one Submit call across the crash")
		})
	}
}

// SIGTERM during a plan must abandon cleanly: no torn transaction, the lease
// handed back, and the next worker resumes to the same single effect.
func TestIntegration_Worker_GracefulShutdownThenResume(t *testing.T) {
	d := openTestDB(t)
	w := newWorld(t, d)
	plan := w.approvedPlan(t)

	// A worker whose drain budget expires while a step is blocked.
	blocked := make(chan struct{})
	released := make(chan struct{})
	var once sync.Once
	deps := w.deps()
	deps.Hooks = settlement.Hooks{OnStep: func(ctx context.Context, s settlement.Step, ph settlement.Phase) error {
		if s.Type == settlement.StepAcquireQuote && ph == settlement.PhaseBeforeEffect {
			once.Do(func() { close(blocked) })
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-released:
			}
		}
		return nil
	}}
	slow, err := settlement.NewExecutor(deps, w.Signer)
	require.NoError(t, err)

	q := scoped(NewPGPlanQueue(d, func() time.Time { return time.Now().UTC() }), plan)
	r, err := NewRunner(q, slow, testLogger(t), clock.System(), RunnerOptions{
		Owner: "itest-drain", PollInterval: 5 * time.Millisecond,
		DrainTimeout: 100 * time.Millisecond, AbandonGrace: 10 * time.Second,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	select {
	case <-blocked:
	case <-time.After(30 * time.Second):
		cancel()
		t.Fatal("the plan never reached ACQUIRE_QUOTE")
	}
	start := time.Now()
	cancel()
	require.NoError(t, <-done)
	assert.Less(t, time.Since(start), 20*time.Second, "shutdown is bounded")
	close(released)
	require.Equal(t, int64(1), r.Stats().Abandoned)

	// The plan is durable, immediately claimable, and settles on the next
	// worker with exactly one economic effect.
	var outcome string
	require.NoError(t, d.QueryRow(t.Context(), `SELECT last_outcome FROM execution_plan_leases WHERE plan_id = $1`, plan.ID).Scan(&outcome))
	assert.Equal(t, string(OutcomeAbandoned), outcome)

	r2 := newWorkerRunner(t, w, RunnerOptions{Owner: "itest-drain-restart"}, plan)
	runUntilIdle(t, r2, 1, 60*time.Second)
	stored, err := w.plans.Get(t.Context(), d, plan.ID)
	require.NoError(t, err)
	require.Equal(t, settlement.PlanCompleted, stored.Status)
	assertExactlyOnce(t, w, plan.ID)
}

// Two workers racing on the same plans must produce one effect each, not two.
func TestIntegration_Worker_TwoWorkersNeverDoubleExecute(t *testing.T) {
	d := openTestDB(t)
	w := newWorld(t, d)
	plan := w.approvedPlan(t)

	a := newWorkerRunner(t, w, RunnerOptions{Owner: "itest-race-a"}, plan)
	b := newWorkerRunner(t, w, RunnerOptions{Owner: "itest-race-b"}, plan)
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	for _, r := range []*Runner{a, b} {
		wg.Add(1)
		go func() { defer wg.Done(); assert.NoError(t, r.Run(ctx)) }()
	}
	require.Eventually(t, func() bool {
		p, err := w.plans.Get(ctx, d, plan.ID)
		return err == nil && p.Status == settlement.PlanCompleted
	}, 60*time.Second, 20*time.Millisecond)
	cancel()
	wg.Wait()

	assertExactlyOnce(t, w, plan.ID)
	assert.Equal(t, int64(1), a.Stats().Completed+b.Stats().Completed, "one worker completed it, not both")
}

var errStop = errors.New("itest: simulated process stop")

// fillObservedMessage builds the bus message the relay would publish for the
// order's fill, so the waker sees exactly what production sees.
func fillObservedMessage(t *testing.T, w *world, plan settlement.Plan, o execution.Order) event.Message {
	t.Helper()
	fills, err := w.orders.ListFills(t.Context(), w.d, o.ID)
	require.NoError(t, err)
	require.NotEmpty(t, fills)
	payload, err := json.Marshal(execution.FillObservedEvent{
		FillID: fills[0].ID.String(), OrderID: o.ID.String(), AccountID: w.AccountID.String(),
		Venue: fills[0].Venue, ExternalFillID: fills[0].ExternalFillID,
		InputAssetID: fills[0].InputAssetID.String(), InputQuantity: fills[0].InputQuantity,
		OutputAssetID: fills[0].OutputAssetID.String(), OutputQuantity: fills[0].OutputQuantity,
		Source: fills[0].Source, Finality: fills[0].Finality, OrderStatus: o.Status,
		ObservedAt: fills[0].ObservedAt, OccurredAt: fills[0].ObservedAt,
	})
	require.NoError(t, err)
	return busMessage(t, event.TopicFillObserved, event.AggregateOrder, o.ID.String(), payload, plan.ID.String())
}

// planTransitionMessage builds an order.transitioned message naming the plan.
func planTransitionMessage(t *testing.T, plan settlement.Plan) event.Message {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"order_id": "", "plan_id": plan.ID.String(), "from": "SUBMITTING", "to": "SUBMITTED",
	})
	require.NoError(t, err)
	return busMessage(t, event.TopicOrderTransitioned, event.AggregateOrder, plan.ID.String(), payload, plan.ID.String())
}

func busMessage(t *testing.T, topic event.Topic, aggType, aggID string, payload []byte, correlation string) event.Message {
	t.Helper()
	env := event.Envelope{
		ID: event.NewEventID().String(), Type: string(topic), SchemaVersion: topic.Version(), Source: "execution",
		AggregateType: aggType, AggregateID: aggID, CorrelationID: correlation,
		OccurredAt: time.Now().UTC(), RecordedAt: time.Now().UTC(), Payload: payload,
	}
	value, err := env.CanonicalBytes()
	require.NoError(t, err)
	return event.Message{ID: env.ID, Topic: string(topic), Key: aggID, Value: value}
}
