package advancedjobs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/errs"
)

var t0 = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

// fakeJob is a job whose run is a function.
type fakeJob struct {
	spec Spec
	runs atomic.Int64
	run  func(ctx context.Context) (Report, error)
}

func (f *fakeJob) Spec() Spec { return f.spec }

func (f *fakeJob) Run(ctx context.Context) (Report, error) {
	f.runs.Add(1)
	if f.run == nil {
		return Report{Examined: 1, Changed: 1}, nil
	}
	return f.run(ctx)
}

func newFake(name string, every time.Duration, run func(ctx context.Context) (Report, error)) *fakeJob {
	return &fakeJob{spec: Spec{Name: name, Settings: Settings{Interval: every, Batch: 10}}, run: run}
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newScheduler(t *testing.T, clk clock.Clock, timeout time.Duration, jobs ...Job) *Scheduler {
	t.Helper()
	s, err := NewScheduler(jobs, Options{Clock: clk, Logger: quietLogger(), RunTimeout: timeout})
	require.NoError(t, err)
	return s
}

func byJob(outs []Outcome) map[string]Outcome {
	m := map[string]Outcome{}
	for _, o := range outs {
		m[o.Job] = o
	}
	return m
}

// TestScheduler_RunsEveryJobAtFirstStepThenOnlyWhenDue: a job is due at the
// first look and then exactly every interval of the injected clock.
func TestScheduler_RunsEveryJobAtFirstStepThenOnlyWhenDue(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(t0)
	fast := newFake("test.fast", time.Minute, nil)
	slow := newFake("test.slow", 5*time.Minute, nil)
	s := newScheduler(t, clk, time.Minute, fast, slow)

	outs := byJob(s.Step(context.Background()))
	require.Len(t, outs, 2, "both jobs are due at the first step")
	assert.False(t, outs["test.fast"].Failed())

	clk.Advance(30 * time.Second)
	assert.Empty(t, s.Step(context.Background()), "nothing is due half an interval later")

	clk.Advance(30 * time.Second)
	outs = byJob(s.Step(context.Background()))
	assert.Len(t, outs, 1)
	assert.Contains(t, outs, "test.fast", "the one-minute job is due again")

	clk.Advance(4 * time.Minute)
	outs = byJob(s.Step(context.Background()))
	assert.Len(t, outs, 2, "five minutes in, both are due")
	assert.Equal(t, int64(3), fast.runs.Load())
	assert.Equal(t, int64(2), slow.runs.Load())

	clk.Advance(time.Hour)
	assert.Len(t, s.Step(context.Background()), 2, "a long gap runs each job once, never a burst")
	assert.Equal(t, int64(4), fast.runs.Load())
}

// TestScheduler_NeverOverlapsAJobWithItself: a job that is still running when
// it falls due again is skipped, and the other jobs run.
func TestScheduler_NeverOverlapsAJobWithItself(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(t0)
	started := make(chan struct{})
	release := make(chan struct{})
	var concurrent, maxConcurrent atomic.Int64
	blocking := newFake("test.blocking", time.Minute, func(ctx context.Context) (Report, error) {
		n := concurrent.Add(1)
		defer concurrent.Add(-1)
		for {
			m := maxConcurrent.Load()
			if n <= m || maxConcurrent.CompareAndSwap(m, n) {
				break
			}
		}
		started <- struct{}{}
		<-release
		return Report{Examined: 1, Changed: 1}, nil
	})
	other := newFake("test.other", time.Minute, nil)
	s := newScheduler(t, clk, time.Hour, blocking, other)

	first := make(chan []Outcome, 1)
	go func() { first <- s.Step(context.Background()) }()
	<-started
	require.Eventually(t, func() bool { return healthOf(t, s, "test.other").Runs == 1 },
		5*time.Second, time.Millisecond, "the other job's first run finishes while the blocking one is still going")

	clk.Advance(2 * time.Minute)
	outs := byJob(s.Step(context.Background()))
	require.Contains(t, outs, "test.blocking")
	assert.True(t, outs["test.blocking"].Skipped, "the running job is not started a second time")
	require.Contains(t, outs, "test.other")
	assert.False(t, outs["test.other"].Skipped, "the other job is not held up")
	assert.Equal(t, int64(2), other.runs.Load())

	close(release)
	firstOuts := byJob(<-first)
	assert.False(t, firstOuts["test.blocking"].Failed())
	assert.Equal(t, int64(1), blocking.runs.Load())
	assert.Equal(t, int64(1), maxConcurrent.Load(), "never two runs of one job at once")

	// Once it has returned it runs again when due (release is closed, so the
	// next run only has to announce itself).
	clk.Advance(time.Minute)
	drained := make(chan struct{})
	go func() {
		<-started
		close(drained)
	}()
	outs = byJob(s.Step(context.Background()))
	<-drained
	assert.False(t, outs["test.blocking"].Skipped)
	assert.Equal(t, int64(2), blocking.runs.Load())
}

// TestScheduler_APanicIsAFailureAndTheOtherJobsRun: a panicking job is a
// failed run with a redacted reason, counted, and stops nothing else.
func TestScheduler_APanicIsAFailureAndTheOtherJobsRun(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(t0)
	var logs bytes.Buffer
	panicking := newFake("test.panics", time.Minute, func(context.Context) (Report, error) {
		panic("secret-looking panic text")
	})
	healthy := newFake("test.healthy", time.Minute, nil)
	s, err := NewScheduler([]Job{panicking, healthy}, Options{Clock: clk, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	require.NoError(t, err)

	for i := 0; i < 3; i++ {
		outs := byJob(s.Step(context.Background()))
		p := outs["test.panics"]
		require.NotNil(t, p.Err)
		assert.Equal(t, CodePanic, p.Err.Code)
		assert.NotContains(t, p.Err.Reason, "secret-looking", "the panic value stays out of the outcome")
		assert.False(t, outs["test.healthy"].Failed(), "a panic elsewhere stops nothing")
		clk.Advance(time.Minute)
	}
	h := map[string]Health{}
	for _, x := range s.Health() {
		h[x.Job] = x
	}
	assert.Equal(t, int64(3), h["test.panics"].ConsecutiveFailures)
	assert.True(t, h["test.panics"].LastSuccess.IsZero())
	assert.Equal(t, int64(0), h["test.healthy"].ConsecutiveFailures)
	assert.Equal(t, t0.Add(2*time.Minute), h["test.healthy"].LastSuccess)
	assert.Contains(t, logs.String(), "advanced job panicked", "the detail is in the log")
}

// TestScheduler_ATimeoutIsAFailureAndTheSlotIsHeldUntilTheRunReturns: a run
// that overruns its deadline fails when the deadline passes; a run that
// ignores its context keeps its slot (never two at once) until it returns.
func TestScheduler_ATimeoutIsAFailureAndTheSlotIsHeldUntilTheRunReturns(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(t0)
	honours := newFake("test.honours", time.Minute, func(ctx context.Context) (Report, error) {
		<-ctx.Done()
		return Report{}, ctx.Err()
	})
	release := make(chan struct{})
	ignores := newFake("test.ignores", time.Minute, func(context.Context) (Report, error) {
		<-release
		return Report{Examined: 1}, nil
	})
	s := newScheduler(t, clk, 50*time.Millisecond, honours, ignores)

	outs := byJob(s.Step(context.Background()))
	require.NotNil(t, outs["test.honours"].Err)
	assert.Equal(t, CodeTimeout, outs["test.honours"].Err.Code)
	require.NotNil(t, outs["test.ignores"].Err)
	assert.Equal(t, CodeTimeout, outs["test.ignores"].Err.Code, "an overrun fails when the deadline passes")

	clk.Advance(time.Minute)
	outs = byJob(s.Step(context.Background()))
	assert.True(t, outs["test.ignores"].Skipped, "the overrunning run still holds its slot")
	assert.False(t, outs["test.honours"].Skipped)

	close(release)
	s.Wait()
	clk.Advance(time.Minute)
	outs = byJob(s.Step(context.Background()))
	assert.False(t, outs["test.ignores"].Skipped, "once it has returned it runs again")
	assert.False(t, outs["test.ignores"].Failed(), "and this time it finishes in time")
	assert.Equal(t, int64(0), healthOf(t, s, "test.ignores").ConsecutiveFailures)
	assert.Equal(t, int64(2), healthOf(t, s, "test.ignores").Runs, "the skipped step was not a run")
	assert.Equal(t, int64(3), healthOf(t, s, "test.honours").ConsecutiveFailures, "every overrun counts")
}

// TestScheduler_OneFailingJobDoesNotStopTheOthers: an error and a unit
// failure each fail a run and count; a success resets the count.
func TestScheduler_OneFailingJobDoesNotStopTheOthers(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(t0)
	var mode atomic.Int64
	flaky := newFake("test.flaky", time.Minute, func(context.Context) (Report, error) {
		switch mode.Load() {
		case 0:
			return Report{}, errs.New(errs.CodeStaleMarketData, "no fresh mark")
		case 1:
			var r Report
			r.Examined = 2
			r.Changed = 1
			r.fail("unit-b", errors.New("db exploded with secret detail"))
			return r, nil
		default:
			return Report{Examined: 1, Changed: 1}, nil
		}
	})
	steady := newFake("test.steady", time.Minute, nil)
	s := newScheduler(t, clk, time.Minute, flaky, steady)

	outs := byJob(s.Step(context.Background()))
	require.NotNil(t, outs["test.flaky"].Err)
	assert.Equal(t, string(errs.CodeStaleMarketData), outs["test.flaky"].Err.Code)
	assert.Equal(t, "no fresh mark", outs["test.flaky"].Err.Reason, "a subsystem refusal keeps its detail")
	assert.False(t, outs["test.steady"].Failed())

	mode.Store(1)
	clk.Advance(time.Minute)
	outs = byJob(s.Step(context.Background()))
	f := outs["test.flaky"]
	assert.Nil(t, f.Err)
	assert.True(t, f.Failed(), "a unit that could not be processed fails the run")
	require.Len(t, f.Report.Failures, 1)
	assert.Equal(t, string(errs.CodeInternal), f.Report.Failures[0].Code)
	assert.NotContains(t, f.Report.Failures[0].Reason, "secret detail", "an internal error is redacted")

	h := healthOf(t, s, "test.flaky")
	assert.Equal(t, int64(2), h.ConsecutiveFailures)

	mode.Store(2)
	clk.Advance(time.Minute)
	s.Step(context.Background())
	h = healthOf(t, s, "test.flaky")
	assert.Equal(t, int64(0), h.ConsecutiveFailures, "a success resets the count")
	assert.Equal(t, t0.Add(2*time.Minute), h.LastSuccess)
	assert.Equal(t, int64(3), healthOf(t, s, "test.steady").Runs)
}

func healthOf(t *testing.T, s *Scheduler, name string) Health {
	t.Helper()
	for _, h := range s.Health() {
		if h.Job == name {
			return h
		}
	}
	t.Fatalf("no job %s", name)
	return Health{}
}

// recordingObserver keeps every health it is handed.
type recordingObserver struct {
	mu   sync.Mutex
	seen []Health
}

func (r *recordingObserver) ObserveRun(_ context.Context, h Health) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, h)
}

// TestScheduler_TheObserverSeesEveryRun: the health an observer exports is
// the scheduler's, after every run.
func TestScheduler_TheObserverSeesEveryRun(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(t0)
	obs := &recordingObserver{}
	j := newFake("test.observed", time.Minute, nil)
	s, err := NewScheduler([]Job{j}, Options{Clock: clk, Logger: quietLogger(), Observer: obs})
	require.NoError(t, err)
	s.Step(context.Background())
	clk.Advance(time.Minute)
	s.Step(context.Background())
	obs.mu.Lock()
	defer obs.mu.Unlock()
	require.Len(t, obs.seen, 2)
	assert.Equal(t, int64(2), obs.seen[1].Runs)
	assert.Equal(t, t0.Add(time.Minute), obs.seen[1].LastSuccess)
	assert.False(t, obs.seen[1].Running)
}

// TestScheduler_RunNow runs one job regardless of its schedule and refuses a
// name it does not run.
func TestScheduler_RunNow(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(t0)
	j := newFake("test.once", time.Hour, nil)
	s := newScheduler(t, clk, time.Minute, j)
	s.Step(context.Background())
	o, err := s.RunNow(context.Background(), "test.once")
	require.NoError(t, err)
	assert.False(t, o.Failed())
	assert.Equal(t, int64(2), j.runs.Load(), "not due, run anyway")
	_, err = s.RunNow(context.Background(), "test.nope")
	require.ErrorIs(t, err, ErrUnknownJob)
}

// TestScheduler_RefusesAMalformedJobSet: a job set the scheduler cannot run
// as written is refused at construction, not discovered at runtime.
func TestScheduler_RefusesAMalformedJobSet(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(t0)
	for name, jobs := range map[string][]Job{
		"duplicate name": {newFake("test.a", time.Minute, nil), newFake("test.a", time.Minute, nil)},
		"no interval":    {newFake("test.a", 0, nil)},
		"no name":        {newFake("", time.Minute, nil)},
		"nil job":        {nil},
		"no batch":       {&fakeJob{spec: Spec{Name: "test.a", Settings: Settings{Interval: time.Minute}}}},
	} {
		_, err := NewScheduler(jobs, Options{Clock: clk, Logger: quietLogger()})
		assert.Error(t, err, name)
	}
	_, err := NewScheduler(nil, Options{Logger: quietLogger()})
	assert.Error(t, err, "no clock")
}

// TestScheduler_RunDrainsOnStop: Run starts due jobs on its ticker, and on
// stop lets the run in flight finish on a context the stop did not cancel.
func TestScheduler_RunDrainsOnStop(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(t0)
	started := make(chan struct{}, 1)
	var sawCancel atomic.Bool
	finishing := newFake("test.finishing", time.Hour, func(ctx context.Context) (Report, error) {
		started <- struct{}{}
		time.Sleep(50 * time.Millisecond)
		if ctx.Err() != nil {
			sawCancel.Store(true)
		}
		return Report{Examined: 1, Changed: 1}, nil
	})
	s := newScheduler(t, clk, time.Minute, finishing)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx, 10*time.Millisecond, time.Second)
		close(done)
	}()
	<-started
	stop()
	<-done
	assert.False(t, sawCancel.Load(), "the stop signal does not cancel the run in flight")
	h := healthOf(t, s, "test.finishing")
	assert.Equal(t, int64(1), h.Runs)
	assert.False(t, h.Running)
	assert.Equal(t, int64(0), h.ConsecutiveFailures)
}

// TestScheduler_RunCancelsRunsThatOutlastTheDrain: a run still going when
// the drain budget is spent is cancelled, and is a failed run.
func TestScheduler_RunCancelsRunsThatOutlastTheDrain(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(t0)
	started := make(chan struct{}, 1)
	slow := newFake("test.slow", time.Hour, func(ctx context.Context) (Report, error) {
		started <- struct{}{}
		<-ctx.Done()
		return Report{}, ctx.Err()
	})
	s := newScheduler(t, clk, time.Hour, slow)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx, 10*time.Millisecond, 20*time.Millisecond)
		close(done)
	}()
	<-started
	stop()
	<-done
	h := healthOf(t, s, "test.slow")
	assert.Equal(t, int64(1), h.ConsecutiveFailures)
	require.NotNil(t, h.LastError)
	assert.Equal(t, CodeCancelled, h.LastError.Code)
}

// TestProp_SchedulerRunsAJobExactlyWhenDue is the due computation as a
// property: whatever the intervals and however the clock moves, a job runs
// at a step if and only if at least its interval has passed since it last
// started, and the first step runs every job.
func TestProp_SchedulerRunsAJobExactlyWhenDue(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		clk := clock.NewFake(t0)
		n := rapid.IntRange(1, 4).Draw(rt, "jobs")
		jobs := make([]Job, n)
		fakes := make([]*fakeJob, n)
		intervals := make([]time.Duration, n)
		for i := range jobs {
			intervals[i] = time.Duration(rapid.IntRange(1, 600).Draw(rt, "interval_s")) * time.Second
			fakes[i] = newFake("test.p"+string(rune('a'+i)), intervals[i], nil)
			jobs[i] = fakes[i]
		}
		s, err := NewScheduler(jobs, Options{Clock: clk, Logger: quietLogger(), RunTimeout: time.Minute})
		if err != nil {
			rt.Fatal(err)
		}
		last := make([]*time.Time, n)
		expected := make([]int64, n)
		steps := rapid.IntRange(1, 30).Draw(rt, "steps")
		for step := 0; step < steps; step++ {
			now := clk.Now()
			for i := range jobs {
				if last[i] == nil || !now.Before(last[i].Add(intervals[i])) {
					expected[i]++
					at := now
					last[i] = &at
				}
			}
			s.Step(context.Background())
			for i := range jobs {
				if got := fakes[i].runs.Load(); got != expected[i] {
					rt.Fatalf("job %d: ran %d times, want %d", i, got, expected[i])
				}
			}
			clk.Advance(time.Duration(rapid.IntRange(0, 900).Draw(rt, "advance_s")) * time.Second)
		}
	})
}
