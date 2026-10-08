package advancedjobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/errs"
)

// Scheduler bounds.
const (
	// DefaultRunTimeout bounds one run when Options.RunTimeout is zero.
	DefaultRunTimeout = 5 * time.Minute
	// DefaultTick is how often Run looks for due jobs.
	DefaultTick = time.Second
	// DefaultDrainTimeout is how long runs in flight may finish after the
	// worker is told to stop, when Run is given zero.
	DefaultDrainTimeout = 25 * time.Second
	// drainGrace is how long a cancelled run is waited for after the drain
	// budget is spent. Every database call honours cancellation, so a run
	// that has not returned by then is one that ignores its context, and the
	// process exits without it: its transaction rolls back with the
	// connection.
	drainGrace = 5 * time.Second
)

// ErrUnknownJob is returned for a job name the scheduler does not run.
var ErrUnknownJob = errors.New("advancedjobs: unknown job")

// Health is what the scheduler knows about one job.
type Health struct {
	Job      string
	Interval time.Duration
	Batch    int
	// Running reports a run in flight.
	Running bool
	// Runs counts finished runs.
	Runs int64
	// LastStarted and LastFinished are clock times; LastSuccess is the
	// finish of the last run that processed every unit it examined (zero
	// until one has).
	LastStarted  time.Time
	LastFinished time.Time
	LastSuccess  time.Time
	// LastDuration is the last run's monotonic duration.
	LastDuration time.Duration
	// ConsecutiveFailures counts the failed runs since the last success. A
	// run fails when it returns an error, panics, overruns its deadline or
	// reports a unit it could not process.
	ConsecutiveFailures int64
	LastReport          Report
	// LastError is the last run's own failure, redacted (nil when it had
	// none).
	LastError *Failure
}

// Observer receives a job's health after every finished run.
type Observer interface {
	ObserveRun(ctx context.Context, h Health)
}

// Outcome is one finished, or skipped, run.
type Outcome struct {
	Job      string
	Started  time.Time
	Duration time.Duration
	Report   Report
	// Err is the run's own failure, redacted: a returned error, a panic or
	// the deadline.
	Err *Failure
	// Skipped reports a job that was due while its previous run was still
	// going. Nothing ran.
	Skipped bool
}

// Failed reports whether the run failed (see Health.ConsecutiveFailures).
func (o Outcome) Failed() bool { return o.Err != nil || !o.Report.OK() }

// Options configure a Scheduler.
type Options struct {
	Clock  clock.Clock
	Logger *slog.Logger
	// Observer is optional.
	Observer Observer
	// RunTimeout bounds one run; zero takes DefaultRunTimeout.
	RunTimeout time.Duration
}

// Scheduler runs jobs when they are due. It is safe for concurrent use.
type Scheduler struct {
	clk      clock.Clock
	log      *slog.Logger
	observer Observer
	timeout  time.Duration
	slots    []*slot
	byName   map[string]*slot
	inflight sync.WaitGroup
}

type slot struct {
	job  Job
	spec Spec

	mu      sync.Mutex
	running bool
	// due is when the job may next start; zero means at the first look.
	due    time.Time
	health Health
}

// NewScheduler validates the jobs and returns a scheduler that starts each
// of them at its first Step and then every interval.
func NewScheduler(jobs []Job, o Options) (*Scheduler, error) {
	if o.Clock == nil || o.Logger == nil {
		return nil, errs.New(errs.CodeInternal, "advancedjobs: a scheduler needs a clock and a logger")
	}
	timeout := o.RunTimeout
	if timeout <= 0 {
		timeout = DefaultRunTimeout
	}
	s := &Scheduler{clk: o.Clock, log: o.Logger, observer: o.Observer, timeout: timeout, byName: map[string]*slot{}}
	for _, j := range jobs {
		if j == nil {
			return nil, errs.New(errs.CodeInternal, "advancedjobs: a nil job")
		}
		sp := j.Spec()
		switch {
		case strings.TrimSpace(sp.Name) == "":
			return nil, errs.New(errs.CodeInternal, "advancedjobs: a job without a name")
		case sp.Interval <= 0:
			return nil, errs.Newf(errs.CodeInternal, "advancedjobs: job %s has no positive interval", sp.Name)
		case sp.Batch <= 0:
			return nil, errs.Newf(errs.CodeInternal, "advancedjobs: job %s has no positive batch", sp.Name)
		}
		if _, dup := s.byName[sp.Name]; dup {
			return nil, errs.Newf(errs.CodeInternal, "advancedjobs: job %s is registered twice", sp.Name)
		}
		sl := &slot{job: j, spec: sp, health: Health{Job: sp.Name, Interval: sp.Interval, Batch: sp.Batch}}
		s.slots = append(s.slots, sl)
		s.byName[sp.Name] = sl
	}
	return s, nil
}

// Specs returns every job's spec in registration order.
func (s *Scheduler) Specs() []Spec {
	out := make([]Spec, len(s.slots))
	for i, sl := range s.slots {
		out[i] = sl.spec
	}
	return out
}

// Health returns every job's health in registration order.
func (s *Scheduler) Health() []Health {
	out := make([]Health, len(s.slots))
	for i, sl := range s.slots {
		sl.mu.Lock()
		out[i] = sl.health
		sl.mu.Unlock()
	}
	return out
}

// Step starts, concurrently, every job that is due at the clock's current
// time and is not already running, and returns when those runs have
// finished -- or, for a run that overruns its deadline, when the deadline
// passed. A job that is due while its previous run is still going is
// reported Skipped and is not started. Outcomes are in registration order.
func (s *Scheduler) Step(ctx context.Context) []Outcome {
	now := s.clk.Now()
	type pending struct {
		i  int
		ch <-chan Outcome
	}
	var waits []pending
	byIndex := map[int]Outcome{}
	for i, sl := range s.slots {
		ch, skipped := s.start(ctx, sl, now, false)
		switch {
		case skipped:
			byIndex[i] = Outcome{Job: sl.spec.Name, Skipped: true}
		case ch != nil:
			waits = append(waits, pending{i: i, ch: ch})
		}
	}
	for _, w := range waits {
		byIndex[w.i] = <-w.ch
	}
	out := make([]Outcome, 0, len(byIndex))
	for i := range s.slots {
		if o, ok := byIndex[i]; ok {
			out = append(out, o)
		}
	}
	return out
}

// RunNow runs one job immediately, whether or not it is due, and returns its
// outcome. A job that is already running is reported Skipped.
func (s *Scheduler) RunNow(ctx context.Context, name string) (Outcome, error) {
	sl, ok := s.byName[name]
	if !ok {
		return Outcome{}, fmt.Errorf("%w %q", ErrUnknownJob, name)
	}
	ch, skipped := s.start(ctx, sl, s.clk.Now(), true)
	if skipped {
		return Outcome{Job: name, Skipped: true}, nil
	}
	return <-ch, nil
}

// Run starts every job whenever it is due, looking every tick, until ctx is
// done. Then it drains: no new run starts, and runs in flight keep a context
// the stop signal does not cancel for up to drain, after which they are
// cancelled. Every unit of work is its own transaction, so a cancelled run
// loses at most the unit it was in, and that unit's effects are keyed to
// happen exactly once when the next run takes it up.
func (s *Scheduler) Run(ctx context.Context, tick, drain time.Duration) {
	if tick <= 0 {
		tick = DefaultTick
	}
	if drain <= 0 {
		drain = DefaultDrainTimeout
	}
	base, cancelRuns := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRuns()
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	s.dispatch(base)
	for {
		select {
		case <-ctx.Done():
			s.drain(base, cancelRuns, drain)
			return
		case <-ticker.C:
			s.dispatch(base)
		}
	}
}

// Wait blocks until every run in flight has returned.
func (s *Scheduler) Wait() { s.inflight.Wait() }

func (s *Scheduler) dispatch(ctx context.Context) {
	now := s.clk.Now()
	for _, sl := range s.slots {
		// The outcome channel is buffered: nobody needs to read it.
		s.start(ctx, sl, now, false)
	}
}

func (s *Scheduler) drain(ctx context.Context, cancelRuns context.CancelFunc, budget time.Duration) {
	done := make(chan struct{})
	go func() {
		s.inflight.Wait()
		close(done)
	}()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case <-done:
		s.log.InfoContext(ctx, "advanced jobs drained")
		return
	case <-timer.C:
	}
	var running []string
	for _, h := range s.Health() {
		if h.Running {
			running = append(running, h.Job)
		}
	}
	s.log.WarnContext(ctx, "advanced jobs still running after the drain budget; cancelling them",
		slog.Any("jobs", running), slog.Duration("drain", budget))
	cancelRuns()
	grace := time.NewTimer(drainGrace)
	defer grace.Stop()
	select {
	case <-done:
	case <-grace.C:
		s.log.ErrorContext(ctx, "advanced jobs ignored cancellation; exiting without them", slog.Any("jobs", running))
	}
}

// start begins a run of sl if it is due (or force) and not already running.
// It returns the channel the outcome will arrive on, or skipped when the job
// was due but still running.
func (s *Scheduler) start(ctx context.Context, sl *slot, now time.Time, force bool) (<-chan Outcome, bool) {
	sl.mu.Lock()
	defer sl.mu.Unlock()
	if !force && !sl.due.IsZero() && now.Before(sl.due) {
		return nil, false
	}
	if sl.running {
		return nil, true
	}
	sl.running = true
	sl.due = now.Add(sl.spec.Interval)
	sl.health.Running = true
	sl.health.LastStarted = now
	out := make(chan Outcome, 1)
	s.inflight.Add(1)
	go s.execute(ctx, sl, now, out)
	return out, false
}

type runResult struct {
	report   Report
	err      error
	panicked bool
}

// execute runs one pass of sl's job under the per-run deadline. A panic is
// recovered into a failed run. A run that overruns the deadline is recorded
// as failed when the deadline passes, and the job keeps its slot until it
// actually returns, so it is never started twice at once.
func (s *Scheduler) execute(parent context.Context, sl *slot, started time.Time, out chan<- Outcome) {
	ctx, cancel := context.WithTimeout(parent, s.timeout)
	defer cancel()
	stop := clock.Start()
	done := make(chan runResult, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				s.log.LogAttrs(context.WithoutCancel(ctx), slog.LevelError, "advanced job panicked",
					slog.String("job", sl.spec.Name), slog.String("panic_type", fmt.Sprintf("%T", p)),
					slog.String("panic", fmt.Sprint(p)), slog.String("stack", string(debug.Stack())))
				done <- runResult{panicked: true}
			}
		}()
		rep, err := sl.job.Run(ctx)
		done <- runResult{report: rep, err: err}
	}()

	var res runResult
	overran := false
	select {
	case res = <-done:
	case <-ctx.Done():
		res = runResult{err: ctx.Err()}
		overran = true
	}
	// A run that returned frees its slot in the same step that records it,
	// so whoever sees the outcome may start the job again at once. An
	// overrun keeps the slot until its goroutine actually returns.
	o := s.finish(ctx, sl, started, stop(), res, !overran)
	out <- o
	if overran {
		<-done
		s.release(sl)
	}
	s.inflight.Done()
}

func (s *Scheduler) release(sl *slot) {
	sl.mu.Lock()
	sl.running = false
	sl.health.Running = false
	sl.mu.Unlock()
}

// finish records a run's outcome in the job's health -- freeing the slot when
// free is set -- logs it and hands the health to the observer.
func (s *Scheduler) finish(ctx context.Context, sl *slot, started time.Time, elapsed time.Duration, res runResult, free bool) Outcome {
	o := Outcome{Job: sl.spec.Name, Started: started, Duration: elapsed, Report: res.report}
	switch {
	case res.panicked:
		o.Err = &Failure{Unit: sl.spec.Name, Code: CodePanic, Reason: "the job panicked; the worker log has the detail"}
	case res.err != nil:
		code, reason := Redact(res.err)
		o.Err = &Failure{Unit: sl.spec.Name, Code: code, Reason: reason, err: res.err}
	}
	finished := s.clk.Now()
	sl.mu.Lock()
	h := &sl.health
	h.Runs++
	h.LastFinished = finished
	h.LastDuration = elapsed
	h.LastReport = res.report
	h.LastError = o.Err
	if o.Failed() {
		h.ConsecutiveFailures++
	} else {
		h.ConsecutiveFailures = 0
		h.LastSuccess = finished
	}
	if free {
		sl.running = false
		h.Running = false
	}
	snapshot := *h
	sl.mu.Unlock()

	logCtx := context.WithoutCancel(ctx)
	s.logOutcome(logCtx, o, snapshot)
	if s.observer != nil {
		s.observer.ObserveRun(logCtx, snapshot)
	}
	return o
}

func (s *Scheduler) logOutcome(ctx context.Context, o Outcome, h Health) {
	attrs := []slog.Attr{
		slog.String("job", o.Job), slog.Int("examined", o.Report.Examined), slog.Int("changed", o.Report.Changed),
		slog.Int("deferred", o.Report.Deferred), slog.Int("failures", len(o.Report.Failures)),
		slog.Duration("duration", o.Duration), slog.Int64("consecutive_failures", h.ConsecutiveFailures),
	}
	for _, f := range o.Report.Failures {
		fa := []slog.Attr{slog.String("job", o.Job), slog.String("unit", f.Unit), slog.String("code", f.Code),
			slog.String("reason", f.Reason)}
		if f.err != nil {
			fa = append(fa, slog.String("error", f.err.Error()))
		}
		s.log.LogAttrs(ctx, slog.LevelWarn, "advanced job could not process a unit", fa...)
	}
	switch {
	case o.Err != nil:
		attrs = append(attrs, slog.String("code", o.Err.Code), slog.String("reason", o.Err.Reason))
		if o.Err.err != nil {
			attrs = append(attrs, slog.String("error", o.Err.err.Error()))
		}
		s.log.LogAttrs(ctx, slog.LevelError, "advanced job run failed", attrs...)
	case !o.Report.OK():
		s.log.LogAttrs(ctx, slog.LevelWarn, "advanced job run finished with failures", attrs...)
	case o.Report.Changed > 0 || o.Report.Deferred > 0:
		s.log.LogAttrs(ctx, slog.LevelInfo, "advanced job run finished", attrs...)
	default:
		s.log.LogAttrs(ctx, slog.LevelDebug, "advanced job run finished", attrs...)
	}
}
