package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/settlement"
)

// fakeQueue is an in-memory PlanQueue. It records every release so tests can
// assert the outcome the runner classified.
type fakeQueue struct {
	mu       sync.Mutex
	pending  []Lease
	released []releaseCall
	renewals int
	renewOK  bool
	claimErr error
}

type releaseCall struct {
	lease      Lease
	outcome    Outcome
	err        error
	retryAfter time.Duration
}

func newFakeQueue(leases ...Lease) *fakeQueue {
	return &fakeQueue{pending: leases, renewOK: true}
}

func (q *fakeQueue) Claim(_ context.Context, owner string, limit int, _ time.Duration) ([]Lease, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.claimErr != nil {
		return nil, q.claimErr
	}
	if len(q.pending) == 0 || limit <= 0 {
		return nil, nil
	}
	n := min(limit, len(q.pending))
	out := make([]Lease, n)
	copy(out, q.pending[:n])
	q.pending = q.pending[n:]
	for i := range out {
		out[i].Owner = owner
	}
	return out, nil
}

func (q *fakeQueue) Renew(context.Context, Lease, time.Duration) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.renewals++
	return q.renewOK, nil
}

func (q *fakeQueue) Release(_ context.Context, l Lease, out Outcome, cause error, retryAfter time.Duration) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.released = append(q.released, releaseCall{lease: l, outcome: out, err: cause, retryAfter: retryAfter})
	return nil
}

func (q *fakeQueue) releases() []releaseCall {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]releaseCall(nil), q.released...)
}

// fakeExecutor answers with whatever the test scripted for the plan.
type fakeExecutor struct {
	mu      sync.Mutex
	answers map[settlement.PlanID]func(ctx context.Context) (settlement.RunResult, error)
	calls   atomic.Int64
	running atomic.Int64
	peak    atomic.Int64
}

func newFakeExecutor() *fakeExecutor {
	return &fakeExecutor{answers: map[settlement.PlanID]func(context.Context) (settlement.RunResult, error){}}
}

func (e *fakeExecutor) on(p settlement.PlanID, f func(ctx context.Context) (settlement.RunResult, error)) {
	e.mu.Lock()
	e.answers[p] = f
	e.mu.Unlock()
}

func (e *fakeExecutor) Run(ctx context.Context, p settlement.PlanID) (settlement.RunResult, error) {
	e.calls.Add(1)
	n := e.running.Add(1)
	for {
		peak := e.peak.Load()
		if n <= peak || e.peak.CompareAndSwap(peak, n) {
			break
		}
	}
	defer e.running.Add(-1)
	e.mu.Lock()
	f := e.answers[p]
	e.mu.Unlock()
	if f == nil {
		return settlement.RunResult{PlanID: p, Status: settlement.PlanCompleted}, nil
	}
	return f(ctx)
}

func newTestRunner(t *testing.T, q PlanQueue, ex PlanExecutor, opts RunnerOptions) *Runner {
	t.Helper()
	if opts.Owner == "" {
		opts.Owner = "test-owner"
	}
	r, err := NewRunner(q, ex, nil, clock.System(), opts)
	require.NoError(t, err)
	return r
}

func TestNewRunner_RequiresDependencies(t *testing.T) {
	t.Parallel()
	_, err := NewRunner(nil, newFakeExecutor(), nil, nil, RunnerOptions{Owner: "o"})
	require.Error(t, err)
	_, err = NewRunner(newFakeQueue(), nil, nil, nil, RunnerOptions{Owner: "o"})
	require.Error(t, err)
	_, err = NewRunner(newFakeQueue(), newFakeExecutor(), nil, nil, RunnerOptions{})
	require.Error(t, err, "a lease needs an owner")
}

func TestRunnerOptions_Defaults(t *testing.T) {
	t.Parallel()
	o := RunnerOptions{Owner: "o"}.withDefaults()
	assert.Equal(t, DefaultConcurrency, o.Concurrency)
	assert.Equal(t, DefaultLeaseTTL, o.LeaseTTL)
	assert.Less(t, o.RenewInterval, o.LeaseTTL, "the lease must be renewed well before it lapses")

	// A renew interval at or beyond the lease is corrected, not accepted.
	o = RunnerOptions{Owner: "o", LeaseTTL: 9 * time.Second, RenewInterval: time.Minute}.withDefaults()
	assert.Equal(t, 3*time.Second, o.RenewInterval)
}

// TestRunner_Classify is the heart of the worker's judgement: which endings
// mean "done", which mean "try again", which mean "a human or reconciliation
// owns this now", and which mean "we were shut down and nothing is decided".
func TestRunner_Classify(t *testing.T) {
	t.Parallel()
	r := newTestRunner(t, newFakeQueue(), newFakeExecutor(), RunnerOptions{RetryBase: time.Second, RetryMax: time.Minute, PausedBackoff: 5 * time.Minute})
	lease := Lease{PlanID: settlement.NewPlanID(), Attempts: 1}

	cases := []struct {
		name    string
		res     settlement.RunResult
		err     error
		want    Outcome
		wantFor time.Duration
	}{
		{
			name: "completed",
			res:  settlement.RunResult{Status: settlement.PlanCompleted},
			want: OutcomeCompleted,
		},
		{
			// The single most important rule in the system: a submission
			// whose outcome is unknown is neither a success nor a failure.
			name:    "submission state unknown is paused, never failed",
			res:     settlement.RunResult{Status: settlement.PlanExecuting},
			err:     errs.New(errs.CodeSubmissionStateUnknown, "submit timed out"),
			want:    OutcomePaused,
			wantFor: 5 * time.Minute,
		},
		{
			name:    "reconciliation required is paused",
			res:     settlement.RunResult{Status: settlement.PlanExecuting},
			err:     errs.New(errs.CodeReconciliationRequired, "observers disagree"),
			want:    OutcomePaused,
			wantFor: 5 * time.Minute,
		},
		{
			name: "terminal failure",
			res:  settlement.RunResult{Status: settlement.PlanFailed},
			err:  errs.New(errs.CodeKillSwitchActive, "GLOBAL_NEW_RISK_KILL"),
			want: OutcomeFailed,
		},
		{
			name:    "transient error retries with backoff",
			res:     settlement.RunResult{Status: settlement.PlanExecuting},
			err:     errors.New("connection reset"),
			want:    OutcomeRetry,
			wantFor: time.Second,
		},
		{
			name: "shutdown is abandoned, not failed",
			res:  settlement.RunResult{Status: settlement.PlanExecuting},
			err:  context.Canceled,
			want: OutcomeAbandoned,
		},
		{
			name: "deadline exceeded is abandoned",
			res:  settlement.RunResult{Status: settlement.PlanExecuting},
			err:  context.DeadlineExceeded,
			want: OutcomeAbandoned,
		},
		{
			name:    "no error but not completed retries",
			res:     settlement.RunResult{Status: settlement.PlanExecuting},
			want:    OutcomeRetry,
			wantFor: time.Second,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, after := r.classify(lease, tc.res, tc.err)
			assert.Equal(t, tc.want, out)
			assert.Equal(t, tc.wantFor, after)
		})
	}
}

// A paused plan must never be classified from the error text or the plan
// status alone: an errs.Error wrapped deeper down still parks the plan.
func TestRunner_Classify_WrappedPausedCode(t *testing.T) {
	t.Parallel()
	r := newTestRunner(t, newFakeQueue(), newFakeExecutor(), RunnerOptions{})
	wrapped := errs.Wrap(errs.New(errs.CodeSubmissionStateUnknown, "timeout"), errs.CodeSubmissionStateUnknown, "plan paused")
	out, _ := r.classify(Lease{Attempts: 3}, settlement.RunResult{Status: settlement.PlanFailed}, wrapped)
	assert.Equal(t, OutcomePaused, out, "a paused code wins over a terminal plan status")
}

func TestBackoff(t *testing.T) {
	t.Parallel()
	assert.Equal(t, time.Second, backoff(time.Second, time.Minute, 0))
	assert.Equal(t, time.Second, backoff(time.Second, time.Minute, 1))
	assert.Equal(t, 2*time.Second, backoff(time.Second, time.Minute, 2))
	assert.Equal(t, 8*time.Second, backoff(time.Second, time.Minute, 4))
	assert.Equal(t, time.Minute, backoff(time.Second, time.Minute, 40), "capped, never overflowing")
}

func TestRunner_RunsClaimedPlansAndReleasesThem(t *testing.T) {
	t.Parallel()
	p1, p2 := settlement.NewPlanID(), settlement.NewPlanID()
	q := newFakeQueue(Lease{PlanID: p1, Attempts: 1}, Lease{PlanID: p2, Attempts: 1})
	ex := newFakeExecutor()
	ex.on(p1, func(context.Context) (settlement.RunResult, error) {
		return settlement.RunResult{PlanID: p1, Status: settlement.PlanCompleted}, nil
	})
	ex.on(p2, func(context.Context) (settlement.RunResult, error) {
		return settlement.RunResult{PlanID: p2, Status: settlement.PlanFailed}, errs.New(errs.CodeKillSwitchActive, "halted")
	})
	r := newTestRunner(t, q, ex, RunnerOptions{PollInterval: time.Millisecond, DrainTimeout: 5 * time.Second})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	require.Eventually(t, func() bool { return len(q.releases()) == 2 }, 5*time.Second, 5*time.Millisecond)
	cancel()
	require.NoError(t, <-done)

	byPlan := map[settlement.PlanID]Outcome{}
	for _, rel := range q.releases() {
		byPlan[rel.lease.PlanID] = rel.outcome
	}
	assert.Equal(t, OutcomeCompleted, byPlan[p1])
	assert.Equal(t, OutcomeFailed, byPlan[p2])
	s := r.Stats()
	assert.Equal(t, int64(2), s.Claimed)
	assert.Equal(t, int64(1), s.Completed)
	assert.Equal(t, int64(1), s.Failed)
}

func TestRunner_RespectsConcurrencyLimit(t *testing.T) {
	t.Parallel()
	const total = 12
	leases := make([]Lease, total)
	ex := newFakeExecutor()
	release := make(chan struct{})
	for i := range leases {
		p := settlement.NewPlanID()
		leases[i] = Lease{PlanID: p, Attempts: 1}
		ex.on(p, func(ctx context.Context) (settlement.RunResult, error) {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return settlement.RunResult{PlanID: p, Status: settlement.PlanCompleted}, nil
		})
	}
	q := newFakeQueue(leases...)
	r := newTestRunner(t, q, ex, RunnerOptions{Concurrency: 3, PollInterval: time.Millisecond, DrainTimeout: 5 * time.Second})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	require.Eventually(t, func() bool { return r.InFlight() == 3 }, 5*time.Second, time.Millisecond)
	// Hold the three in flight long enough for further poll cycles to run.
	time.Sleep(50 * time.Millisecond)
	assert.LessOrEqual(t, int(ex.peak.Load()), 3, "never more than Concurrency plans in flight")
	close(release)
	require.Eventually(t, func() bool { return len(q.releases()) == total }, 10*time.Second, 5*time.Millisecond)
	assert.LessOrEqual(t, int(ex.peak.Load()), 3)
	cancel()
	require.NoError(t, <-done)
}

// A SIGTERM must let an in-flight plan finish rather than tearing it down.
func TestRunner_GracefulShutdown_FinishesInFlightWork(t *testing.T) {
	t.Parallel()
	p := settlement.NewPlanID()
	started := make(chan struct{})
	var sawCancel atomic.Bool
	ex := newFakeExecutor()
	ex.on(p, func(ctx context.Context) (settlement.RunResult, error) {
		close(started)
		select {
		case <-ctx.Done():
			sawCancel.Store(true)
		case <-time.After(150 * time.Millisecond):
		}
		return settlement.RunResult{PlanID: p, Status: settlement.PlanCompleted}, nil
	})
	q := newFakeQueue(Lease{PlanID: p, Attempts: 1})
	r := newTestRunner(t, q, ex, RunnerOptions{PollInterval: time.Millisecond, DrainTimeout: 5 * time.Second})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	<-started
	cancel() // SIGTERM arrives mid-plan

	require.NoError(t, <-done)
	assert.False(t, sawCancel.Load(), "the in-flight plan's context is not cancelled by the signal")
	rel := q.releases()
	require.Len(t, rel, 1)
	assert.Equal(t, OutcomeCompleted, rel[0].outcome, "the plan finished, so it is COMPLETED and not ABANDONED")
}

// Past the drain budget the worker must still exit, and it must hand the
// plan back cleanly rather than letting the lease merely lapse.
func TestRunner_ShutdownIsBounded_AbandonsAndReleases(t *testing.T) {
	t.Parallel()
	p := settlement.NewPlanID()
	started := make(chan struct{})
	ex := newFakeExecutor()
	ex.on(p, func(ctx context.Context) (settlement.RunResult, error) {
		close(started)
		<-ctx.Done() // only the drain deadline stops this plan
		return settlement.RunResult{PlanID: p, Status: settlement.PlanExecuting}, ctx.Err()
	})
	q := newFakeQueue(Lease{PlanID: p, Attempts: 1})
	r := newTestRunner(t, q, ex, RunnerOptions{
		PollInterval: time.Millisecond, DrainTimeout: 50 * time.Millisecond, AbandonGrace: 2 * time.Second,
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- r.Run(ctx) }()
	<-started
	cancel()
	require.NoError(t, <-done)
	assert.Less(t, time.Since(start), 5*time.Second, "shutdown is bounded")

	rel := q.releases()
	require.Len(t, rel, 1, "the lease is handed back, not left to lapse")
	assert.Equal(t, OutcomeAbandoned, rel[0].outcome)
	assert.Zero(t, rel[0].retryAfter, "an abandoned plan is immediately claimable again")
	assert.Equal(t, int64(1), r.Stats().Abandoned)
}

// Losing a lease mid-run means another worker owns the plan. Writing to it
// would clobber the new holder, so the runner must stay silent.
func TestRunner_LostLease_DoesNotRelease(t *testing.T) {
	t.Parallel()
	p := settlement.NewPlanID()
	q := newFakeQueue(Lease{PlanID: p, Attempts: 1})
	q.renewOK = false
	ex := newFakeExecutor()
	ex.on(p, func(ctx context.Context) (settlement.RunResult, error) {
		<-ctx.Done()
		return settlement.RunResult{PlanID: p}, ctx.Err()
	})
	r := newTestRunner(t, q, ex, RunnerOptions{
		PollInterval: time.Millisecond, LeaseTTL: 30 * time.Millisecond, RenewInterval: 5 * time.Millisecond,
		DrainTimeout: 20 * time.Millisecond, AbandonGrace: time.Second,
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	require.Eventually(t, func() bool { return r.Stats().LeaseLost == 1 || r.InFlight() > 0 }, 5*time.Second, time.Millisecond)
	cancel()
	require.NoError(t, <-done)
	assert.Equal(t, int64(1), r.Stats().LeaseLost)
	assert.Empty(t, q.releases(), "a lost lease is never released by its old owner")
}

// A claim failure must not kill the worker: the database may be briefly
// unavailable, and the plans are durable.
func TestRunner_ClaimErrorIsSurvivable(t *testing.T) {
	t.Parallel()
	q := newFakeQueue()
	q.claimErr = errors.New("connection refused")
	r := newTestRunner(t, q, newFakeExecutor(), RunnerOptions{PollInterval: time.Millisecond, DrainTimeout: time.Second})
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Millisecond)
	defer cancel()
	require.NoError(t, r.Run(ctx))
	assert.Zero(t, r.Stats().Claimed)
}
