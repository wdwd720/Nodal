package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/settlement"
)

// PlanExecutor is the settlement executor the runner hosts.
// *settlement.Executor satisfies it.
type PlanExecutor interface {
	Run(ctx context.Context, planID settlement.PlanID) (settlement.RunResult, error)
}

// Runner defaults.
const (
	DefaultConcurrency   = 4
	DefaultPollInterval  = 500 * time.Millisecond
	DefaultLeaseTTL      = 60 * time.Second
	DefaultRenewInterval = 15 * time.Second
	DefaultRetryBase     = 2 * time.Second
	DefaultRetryMax      = 5 * time.Minute
	// DefaultPausedBackoff is how long a plan on the operator path waits
	// before the worker looks at it again. Re-looking is safe and useful: a
	// SUBMISSION_UNKNOWN plan is re-investigated (never re-submitted), and a
	// transaction that has since landed is adopted.
	DefaultPausedBackoff = 5 * time.Minute
	// DefaultDrainTimeout bounds how long SIGTERM waits for in-flight plans
	// before their contexts are cancelled.
	DefaultDrainTimeout = 25 * time.Second
	// DefaultAbandonGrace is the extra time in-flight runs get after their
	// contexts are cancelled, so an open transaction can roll back and the
	// lease can be released rather than merely lapsing.
	DefaultAbandonGrace = 5 * time.Second
)

// RunnerOptions tunes a Runner. Zero values take the defaults above.
type RunnerOptions struct {
	Owner         string
	Concurrency   int
	PollInterval  time.Duration
	LeaseTTL      time.Duration
	RenewInterval time.Duration
	RetryBase     time.Duration
	RetryMax      time.Duration
	PausedBackoff time.Duration
	DrainTimeout  time.Duration
	AbandonGrace  time.Duration
}

func (o RunnerOptions) withDefaults() RunnerOptions {
	if o.Concurrency <= 0 {
		o.Concurrency = DefaultConcurrency
	}
	if o.PollInterval <= 0 {
		o.PollInterval = DefaultPollInterval
	}
	if o.LeaseTTL <= 0 {
		o.LeaseTTL = DefaultLeaseTTL
	}
	if o.RenewInterval <= 0 {
		o.RenewInterval = DefaultRenewInterval
	}
	if o.RenewInterval >= o.LeaseTTL {
		// Renew well inside the lease or the lease lapses under load.
		o.RenewInterval = o.LeaseTTL / 3
		if o.RenewInterval <= 0 {
			o.RenewInterval = time.Second
		}
	}
	if o.RetryBase <= 0 {
		o.RetryBase = DefaultRetryBase
	}
	if o.RetryMax <= 0 {
		o.RetryMax = DefaultRetryMax
	}
	if o.RetryMax < o.RetryBase {
		o.RetryMax = o.RetryBase
	}
	if o.PausedBackoff <= 0 {
		o.PausedBackoff = DefaultPausedBackoff
	}
	if o.DrainTimeout <= 0 {
		o.DrainTimeout = DefaultDrainTimeout
	}
	if o.AbandonGrace <= 0 {
		o.AbandonGrace = DefaultAbandonGrace
	}
	return o
}

// Stats are the runner's cumulative counters, for tests and /healthz.
type Stats struct {
	Claimed   int64
	Completed int64
	Failed    int64
	Paused    int64
	Retried   int64
	Skipped   int64
	Abandoned int64
	LeaseLost int64
}

// Runner claims leased plans and drives them through the settlement
// executor. It owns claiming, leasing, concurrency, shutdown and
// observability; it contains no execution logic of its own.
//
// # Kill switches
//
// The runner deliberately has no kill-switch gate of its own. The executor
// checks the authoritative switch before RESERVE_CAPITAL and before SUBMIT
// and never after, so a switched-off plan is rejected and compensated
// promptly, while OBSERVE_FINALITY, RECONCILE, POST_LEDGER, UPDATE_POSITION
// and RELEASE_RESERVATION keep running (PART 52). A claim-level gate would
// break exactly that: it would strand already-submitted plans, leaving real
// fills unposted and reservations unreleased for as long as the switch was
// on. Stopping new risk is the executor's job; keeping settlement moving is
// the worker's.
type Runner struct {
	queue PlanQueue
	exec  PlanExecutor
	log   *slog.Logger
	clk   clock.Clock
	opts  RunnerOptions

	mu     sync.Mutex
	stats  Stats
	inTx   map[settlement.PlanID]struct{}
	closed bool
}

// NewRunner wires a Runner.
func NewRunner(q PlanQueue, ex PlanExecutor, log *slog.Logger, clk clock.Clock, opts RunnerOptions) (*Runner, error) {
	if q == nil || ex == nil {
		return nil, errs.New(errs.CodeInternal, "execution-worker: runner needs a queue and an executor")
	}
	if opts.Owner == "" {
		return nil, errs.New(errs.CodeInternal, "execution-worker: runner needs an owner id")
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if clk == nil {
		clk = clock.System()
	}
	return &Runner{queue: q, exec: ex, log: log, clk: clk, opts: opts.withDefaults(), inTx: map[settlement.PlanID]struct{}{}}, nil
}

// Options returns the effective options after defaults.
func (r *Runner) Options() RunnerOptions { return r.opts }

// Stats returns a snapshot of the counters.
func (r *Runner) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stats
}

// InFlight reports how many plans are currently running.
func (r *Runner) InFlight() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.inTx)
}

// Run claims and drives plans until ctx is done, then shuts down gracefully:
// it stops claiming immediately, lets in-flight plans finish for at most
// DrainTimeout, cancels whatever is still running after that, waits
// AbandonGrace for those to unwind (a cancelled transaction rolls back; a
// cancelled submission becomes SUBMISSION_UNKNOWN, never a failure), and
// returns. It always returns nil: a shutdown is not an error.
//
// In-flight runs do not inherit ctx's cancellation. That is what makes a
// SIGTERM a drain rather than a kill: a plan that is three steps into a
// transaction is not torn down the instant the signal arrives.
func (r *Runner) Run(ctx context.Context) error {
	runCtx, cancelRuns := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRuns()

	var wg sync.WaitGroup
	poll := time.NewTimer(0)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			r.drain(&wg, cancelRuns)
			return nil
		case <-poll.C:
		}

		free := r.opts.Concurrency - r.InFlight()
		claimed := 0
		if free > 0 {
			leases, err := r.queue.Claim(ctx, r.opts.Owner, free, r.opts.LeaseTTL)
			switch {
			case err != nil && ctx.Err() != nil:
				r.drain(&wg, cancelRuns)
				return nil
			case err != nil:
				r.log.ErrorContext(ctx, "execution-worker: claim failed", "error", err)
			default:
				claimed = len(leases)
				// track before spawning, so InFlight is an accurate upper
				// bound on concurrency in the next poll and the loop never
				// blocks waiting for a slot.
				for _, l := range leases {
					r.track(l.PlanID)
					wg.Add(1)
					go func(l Lease) {
						defer wg.Done()
						defer r.untrack(l.PlanID)
						r.runOne(runCtx, l)
					}(l)
				}
			}
		}
		r.mu.Lock()
		r.stats.Claimed += int64(claimed)
		r.mu.Unlock()

		delay := r.opts.PollInterval
		if claimed > 0 && claimed == free {
			delay = 0 // there may be more work waiting
		}
		poll.Reset(delay)
	}
}

// drain implements the bounded shutdown described on Run.
func (r *Runner) drain(wg *sync.WaitGroup, cancelRuns context.CancelFunc) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	n := len(r.inTx)
	r.mu.Unlock()

	r.log.Info("execution-worker: draining", "in_flight", n, "drain_timeout", r.opts.DrainTimeout)
	if waitFor(wg, r.opts.DrainTimeout) {
		r.log.Info("execution-worker: drained cleanly")
		return
	}
	r.log.Warn("execution-worker: drain deadline reached; canceling in-flight plans", "in_flight", r.InFlight())
	cancelRuns()
	if !waitFor(wg, r.opts.AbandonGrace) {
		r.log.Error("execution-worker: in-flight plans did not unwind within the abandon grace; leases will lapse",
			"in_flight", r.InFlight())
	}
}

// waitFor waits for wg with a timeout, reporting whether it finished.
func waitFor(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

func (r *Runner) track(p settlement.PlanID) {
	r.mu.Lock()
	r.inTx[p] = struct{}{}
	r.mu.Unlock()
}

func (r *Runner) untrack(p settlement.PlanID) {
	r.mu.Lock()
	delete(r.inTx, p)
	r.mu.Unlock()
}

// runOne executes one leased plan, heart-beating the lease while it runs and
// releasing it with the classified outcome afterwards.
func (r *Runner) runOne(ctx context.Context, l Lease) {
	ctx = observability.WithCorrelationID(ctx, l.PlanID.String())
	log := r.log.With("plan_id", l.PlanID.String(), "attempts", l.Attempts, "owner", l.Owner)
	ctx = observability.WithLogger(ctx, log)

	hbCtx, stopHB := context.WithCancel(ctx)
	lost := make(chan struct{})
	var hbDone sync.WaitGroup
	hbDone.Add(1)
	go func() {
		defer hbDone.Done()
		r.heartbeat(hbCtx, l, lost)
	}()

	start := r.clk.Now()
	res, err := r.exec.Run(ctx, l.PlanID)
	stopHB()
	hbDone.Wait()

	out, retryAfter := r.classify(l, res, err)
	select {
	case <-lost:
		// Another worker owns the plan now. Releasing would either be a no-op
		// (the owner guard rejects it) or would clobber a live lease, so stop.
		r.bump(func(s *Stats) { s.LeaseLost++ })
		log.WarnContext(ctx, "execution-worker: lease lost during run; not releasing",
			"outcome", string(out), "elapsed_ms", clock.Elapsed(start).Milliseconds(), "error", err)
		return
	default:
	}

	// The release must survive the cancellation that produced ABANDONED,
	// otherwise a drained plan keeps its lease until it lapses.
	relCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if rerr := r.queue.Release(relCtx, l, out, err, retryAfter); rerr != nil {
		log.ErrorContext(ctx, "execution-worker: release lease failed", "error", rerr, "outcome", string(out))
	}

	attrs := []any{
		"outcome", string(out), "plan_status", string(res.Status), "order_id", res.OrderID,
		"stopped_at", string(res.StoppedAt), "elapsed_ms", clock.Elapsed(start).Milliseconds(),
	}
	switch out {
	case OutcomeCompleted:
		r.bump(func(s *Stats) { s.Completed++ })
		log.InfoContext(ctx, "execution-worker: plan completed", attrs...)
	case OutcomeFailed:
		r.bump(func(s *Stats) { s.Failed++ })
		log.WarnContext(ctx, "execution-worker: plan failed", append(attrs, "error", err)...)
	case OutcomePaused:
		r.bump(func(s *Stats) { s.Paused++ })
		log.WarnContext(ctx, "execution-worker: plan parked on the operator path", append(attrs, "error", err, "retry_after", retryAfter)...)
	case OutcomeNotRunnable:
		r.bump(func(s *Stats) { s.Skipped++ })
		log.InfoContext(ctx, "execution-worker: plan is not runnable", append(attrs, "error", err)...)
	case OutcomeAbandoned:
		r.bump(func(s *Stats) { s.Abandoned++ })
		log.InfoContext(ctx, "execution-worker: plan abandoned at shutdown; state is durable and resumable", attrs...)
	default:
		r.bump(func(s *Stats) { s.Retried++ })
		log.WarnContext(ctx, "execution-worker: plan will be retried", append(attrs, "error", err, "retry_after", retryAfter)...)
	}
}

func (r *Runner) bump(f func(*Stats)) {
	r.mu.Lock()
	f(&r.stats)
	r.mu.Unlock()
}

// heartbeat renews the lease until the run finishes. It closes lost when the
// lease is gone, so the caller knows not to write to it any more.
func (r *Runner) heartbeat(ctx context.Context, l Lease, lost chan<- struct{}) {
	t := time.NewTicker(r.opts.RenewInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		ok, err := r.queue.Renew(rctx, l, r.opts.LeaseTTL)
		cancel()
		switch {
		case err != nil:
			r.log.WarnContext(ctx, "execution-worker: lease renewal failed", "plan_id", l.PlanID.String(), "error", err)
		case !ok:
			r.log.ErrorContext(ctx, "execution-worker: lease lost", "plan_id", l.PlanID.String())
			close(lost)
			return
		}
	}
}

// classify decides the outcome and the backoff for one finished run.
//
// The three codes that must never be treated as ordinary failures are
// RECONCILIATION_REQUIRED and SUBMISSION_STATE_UNKNOWN, which park the plan
// on the operator path, and a cancelled context, which means shutdown and
// not a verdict about the money at all.
func (r *Runner) classify(l Lease, res settlement.RunResult, err error) (Outcome, time.Duration) {
	switch {
	case err == nil:
		if res.Status == settlement.PlanCompleted {
			return OutcomeCompleted, 0
		}
		// A dry-run preview or a plan left EXECUTING: look again soon.
		return OutcomeRetry, backoff(r.opts.RetryBase, r.opts.RetryMax, l.Attempts)
	case errs.HasCode(err, errs.CodeReconciliationRequired), errs.HasCode(err, errs.CodeSubmissionStateUnknown):
		return OutcomePaused, r.opts.PausedBackoff
	case errs.HasCode(err, errs.CodeInvalidStateTransition):
		// The plan is terminal already (a peer finished it between the claim
		// and the run) or a step is FAILED and cannot resume. Retrying would
		// spin forever on the same answer.
		return OutcomeNotRunnable, r.opts.PausedBackoff
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// Shutdown. The plan keeps its durable per-step state and is claimable
		// again immediately, here or on another worker.
		return OutcomeAbandoned, 0
	case res.Status.Terminal():
		return OutcomeFailed, 0
	default:
		return OutcomeRetry, backoff(r.opts.RetryBase, r.opts.RetryMax, l.Attempts)
	}
}

// backoff doubles base per attempt, capped at max, without overflowing.
func backoff(base, max time.Duration, attempts int32) time.Duration {
	d := base
	for i := int32(1); i < attempts && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	return d
}
