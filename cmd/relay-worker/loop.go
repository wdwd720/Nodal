package main

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/event"
)

// snapshotTimeout bounds one depth/lag sample. A sampler that hangs must
// never hold shutdown open.
const snapshotTimeout = 10 * time.Second

// Runner hosts one *event.Relay in a process.
//
// It owns the schedule and the shutdown, and nothing else. Claiming,
// per-partition ordering, the persisted retry deadline and blocked-partition
// handling all live in internal/event and are not restated here.
//
// The loop calls RunOnce rather than event.Relay.Run for one reason:
// shutdown. Relay.Run stops only when its context is cancelled, and
// canceling it mid-pass tears down an open transaction — the publishes
// already acknowledged by the broker in that pass are not marked published,
// so they are all delivered a second time on the next start. Driving the
// passes here lets the signal stop the *next* claim while the current pass
// runs to its commit on a context the signal does not touch. The schedule
// below is the one Relay.Run documents, derived from Relay.Options() so the
// two cannot drift.
type Runner struct {
	relay   *event.Relay
	metrics *relayMetrics
	db      *db.DB
	clk     clock.Clock
	log     *slog.Logger

	// drain bounds how long a pass may keep running after the shutdown
	// signal before it is abandoned.
	drain time.Duration
	// sample is the depth/lag gauge interval; zero disables the sampler.
	sample time.Duration
	// exclusive makes this instance relay only while it holds the advisory
	// lock of Lease. Off by default: concurrent relaying is already safe,
	// and a singleton costs drain rate and failover latency. See
	// exclusive.go.
	exclusive bool

	lease      *Lease
	standingBy bool
}

// Run relays until ctx is done, then returns nil.
//
// Shutdown is a drain, not a kill. The signal stops the loop from claiming
// another batch; the pass already in flight keeps its own context and is
// allowed to finish and commit for up to the drain budget. Only if it
// overruns is it abandoned, and abandoning is safe by construction: the pass
// is a single transaction, so a rollback marks nothing published, releases
// every FOR UPDATE lock, and leaves each row exactly as claimable as before.
// No row is ever left claimed-but-unpublished, and no partition stalls
// because of a shutdown.
func (r *Runner) Run(ctx context.Context) error {
	opts := r.relay.Options()
	stopSampler := r.startSampler(ctx)
	defer stopSampler()
	defer r.ReleaseLease(ctx)

	r.log.Info("relay-worker: relaying",
		"batch_size", opts.BatchSize, "poll_interval", opts.PollInterval, "max_interval", opts.MaxInterval,
		"retry_backoff_base", opts.RetryBackoffBase, "retry_backoff_max", opts.RetryBackoffMax,
		"run_timeout", opts.RunTimeout, "drain_timeout", r.drain, "sample_interval", r.sample,
		"exclusive", r.exclusive)

	failures := 0
	for {
		if ctx.Err() != nil {
			return nil
		}
		active, err := r.ensureLease(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			failures++
			r.log.ErrorContext(ctx, "relay-worker: could not evaluate the exclusive relay lease", "error", err, "consecutive_failures", failures)
			if !sleepWithContext(ctx, jitter(backoffDelay(opts.PollInterval, opts.MaxInterval, failures), opts.PollInterval, opts.JitterPercent)) {
				return nil
			}
			continue
		}
		if !active {
			// A standby still samples depth and lag, so an operator sees the
			// backlog from every instance, not only the active one.
			if !sleepWithContext(ctx, jitter(opts.PollInterval, opts.PollInterval, opts.JitterPercent)) {
				return nil
			}
			continue
		}
		published, failed, err := r.pass(ctx, opts.RunTimeout)

		var delay time.Duration
		switch {
		case err != nil:
			if ctx.Err() != nil {
				// The pass was abandoned by the drain deadline, or lost its
				// database connection while shutting down. Nothing was
				// marked published; the rows are still there.
				return nil
			}
			failures++
			r.log.ErrorContext(ctx, "relay-worker: relay pass failed", "error", err, "consecutive_failures", failures)
			delay = backoffDelay(opts.PollInterval, opts.MaxInterval, failures)
		case failed > 0 && published == 0:
			// The bus is rejecting everything. Every failed row already has
			// its own persisted next_attempt_at, so this backoff only stops
			// the process from spinning on an empty claim.
			failures++
			delay = backoffDelay(opts.PollInterval, opts.MaxInterval, failures)
		case published >= opts.BatchSize:
			failures = 0
			delay = 0
		default:
			failures = 0
			delay = opts.PollInterval
		}
		if !sleepWithContext(ctx, jitter(delay, opts.PollInterval, opts.JitterPercent)) {
			return nil
		}
	}
}

// ensureLease reports whether this instance may relay right now. With
// exclusive mode off it always may.
//
// The liveness check matters: if the lease connection died, Postgres has
// already released the lock and a standby may be relaying, so continuing
// would put two instances on the outbox at once, which is the situation the
// lease exists to prevent.
func (r *Runner) ensureLease(ctx context.Context) (bool, error) {
	if !r.exclusive {
		return true, nil
	}
	if r.lease != nil {
		if err := r.lease.Alive(ctx); err == nil {
			return true, nil
		} else if ctx.Err() != nil {
			return false, nil
		}
		r.log.WarnContext(ctx, "relay-worker: the exclusive relay lease connection is gone; standing down until it can be retaken")
		r.lease.Release(ctx)
		r.lease = nil
	}
	lease, err := TryAcquireLease(ctx, r.db, relayAdvisoryLockKey)
	if err != nil {
		return false, err
	}
	if lease == nil {
		if !r.standingBy {
			r.standingBy = true
			r.log.InfoContext(ctx, "relay-worker: another instance holds the exclusive relay lease; standing by as a hot spare",
				"lock_key", relayAdvisoryLockKey, "opt_out", envExclusive)
		}
		return false, nil
	}
	r.lease = lease
	r.standingBy = false
	r.log.InfoContext(ctx, "relay-worker: holding the exclusive relay lease", "lock_key", relayAdvisoryLockKey)
	return true, nil
}

// ReleaseLease gives the exclusive lease back. It is safe to call when none
// is held.
func (r *Runner) ReleaseLease(ctx context.Context) {
	if r.lease == nil {
		return
	}
	r.lease.Release(ctx)
	r.lease = nil
	r.log.Info("relay-worker: released the exclusive relay lease")
}

// pass runs exactly one claim-publish-mark transaction and reports what it
// published and how many publishes the bus rejected.
//
// The pass context is derived with context.WithoutCancel so the shutdown
// signal cannot cancel a transaction that is mid-commit; it is bounded by
// the relay's own RunTimeout, and by the drain budget once shutdown starts.
func (r *Runner) pass(stop context.Context, timeout time.Duration) (published, failed int, err error) {
	passCtx, cancel := context.WithTimeout(context.WithoutCancel(stop), timeout)
	defer cancel()
	release := r.watchDrain(stop, cancel)
	defer release()

	before := r.metrics.counters().failed
	published, err = r.relay.RunOnce(passCtx)
	failed = int(r.metrics.counters().failed - before)
	return published, failed, err
}

// watchDrain abandons the in-flight pass if it is still running drain after
// the shutdown signal. The returned function stops the watcher and must be
// called exactly once.
func (r *Runner) watchDrain(stop context.Context, abandon context.CancelFunc) func() {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-done:
			return
		case <-stop.Done():
		}
		r.log.Info("relay-worker: shutdown requested; no further batches will be claimed, letting the in-flight pass commit",
			"drain_timeout", r.drain)
		t := time.NewTimer(r.drain)
		defer t.Stop()
		select {
		case <-done:
		case <-t.C:
			r.log.Warn("relay-worker: the in-flight relay pass outran the drain budget; abandoning it. "+
				"Its transaction rolls back, so nothing is marked published and every row stays claimable.",
				"drain_timeout", r.drain)
			abandon()
		}
	}()
	return func() {
		close(done)
		wg.Wait()
	}
}

// startSampler publishes the depth and lag gauges on a timer. The returned
// function stops it and waits.
func (r *Runner) startSampler(ctx context.Context) func() {
	if r.sample <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(r.sample)
		defer t.Stop()
		r.Sample(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-t.C:
				r.Sample(ctx)
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(done) })
		wg.Wait()
	}
}

// Sample reads the outbox depth once and records the gauges. A failure here
// is never fatal: the relay's job is publishing, not measuring.
func (r *Runner) Sample(ctx context.Context) Snapshot {
	qctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
	defer cancel()
	snap, err := TakeSnapshot(qctx, r.db, r.clk.Now())
	if err != nil {
		if ctx.Err() != nil {
			return Snapshot{}
		}
		r.metrics.recordSnapshotError(ctx)
		r.log.WarnContext(ctx, "relay-worker: could not read the outbox depth", "error", err)
		return Snapshot{}
	}
	r.metrics.recordSnapshot(ctx, snap)
	attrs := []any{
		"depth", snap.Unpublished, "eligible", snap.Eligible, "lag", snap.OldestAge().Round(time.Second),
		"partitions", snap.Partitions, "blocked_partitions", snap.BlockedPartitions,
		"failing_rows", snap.Failing, "max_attempts", snap.MaxAttempts,
	}
	if snap.Unpublished == 0 {
		r.log.DebugContext(ctx, "relay-worker: outbox drained", attrs...)
		return snap
	}
	r.log.InfoContext(ctx, "relay-worker: outbox backlog", attrs...)
	return snap
}

// backoffDelay is base doubled per consecutive failure, capped at maximum.
// It mirrors event.Relay.Run's own failure schedule.
func backoffDelay(base, maximum time.Duration, failures int) time.Duration {
	if base <= 0 {
		return 0
	}
	d := base
	for i := 1; i < failures && d < maximum; i++ {
		d *= 2
	}
	if d > maximum {
		d = maximum
	}
	return d
}

// jitter spreads d by +/- percent so several relay instances do not poll in
// lockstep. A zero d yields a small random pause (up to percent of poll) so
// back-to-back full batches still yield between passes.
func jitter(d, poll time.Duration, percent int) time.Duration {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	if d <= 0 {
		span := poll * time.Duration(percent) / 100
		if span <= 0 {
			return 0
		}
		return time.Duration(rand.Int64N(int64(span) + 1)) // #nosec G404 -- poll jitter only, to desynchronise relay instances; this value has no security role
	}
	span := d * time.Duration(percent) / 100
	if span <= 0 {
		return d
	}
	return d - span + time.Duration(rand.Int64N(int64(2*span)+1)) // #nosec G404 -- poll jitter only, to desynchronise relay instances; this value has no security role
}

// sleepWithContext reports whether the sleep completed (false means ctx is
// done and the caller must stop).
func sleepWithContext(ctx context.Context, d time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
