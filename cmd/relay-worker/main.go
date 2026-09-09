// Command relay-worker is the process that makes the transactional outbox
// real: it claims committed rows out of outbox_events and publishes them to
// the event bus (goal PART 31, PART 117, PART 199).
//
// Every financial transaction in this system writes its domain events into
// outbox_events in the same Postgres transaction as the state change, so the
// two commit or roll back together and the bus is never a prerequisite of
// durability. That design only pays off if something drains the table.
// Without this binary, order.transitioned, fill.observed and every capital.*
// event are written and never leave: the API's SSE stream has no producer,
// the execution worker's wake-ups never arrive, and every read model built
// from events is silently frozen.
//
// It contains no relay logic. Claiming with SELECT ... FOR UPDATE SKIP
// LOCKED, publishing in (recorded_at, id) order, deferring a partition whose
// older rows are still unpublished, the persisted per-row retry deadline
// (outbox_events.next_attempt_at, migration 00642) and marking rows
// published all live in internal/event and are tested there. This binary
// supplies the process: configuration, the bus binding, the schedule,
// graceful shutdown, and the observability that makes relay lag a
// first-class signal instead of a log line.
//
// Usage:
//
//	relay-worker run                     relay until SIGINT/SIGTERM
//	relay-worker once [-passes N]        run N claim/publish passes and exit (default 1;
//	                                     0 means "until a pass publishes nothing")
//	relay-worker status [-json]          report outbox depth, relay lag, per-topic backlog
//	                                     and blocked partitions; no bus is opened
//
// status flags:
//
//	-json              print the report as JSON instead of text
//	-max-lag DURATION  exit 3 when the oldest unpublished row is older than this
//	-blocked N         list at most N blocked partitions (default 20)
//
// Configuration comes from internal/config (CP_* variables), plus:
//
//	CP_RELAY_WORKER_BATCH_SIZE           rows claimed per pass (default 100)
//	CP_RELAY_WORKER_POLL_INTERVAL        sleep between passes when drained (default 250ms)
//	CP_RELAY_WORKER_MAX_INTERVAL         cap on the failure backoff between passes (default 10s)
//	CP_RELAY_WORKER_RETRY_BACKOFF_BASE   first per-row retry delay after a failed publish (default 1s)
//	CP_RELAY_WORKER_RETRY_BACKOFF_MAX    cap on the per-row retry delay (default 5m)
//	CP_RELAY_WORKER_RUN_TIMEOUT          bound on one claim/publish pass (default 30s)
//	CP_RELAY_WORKER_DRAIN_TIMEOUT        how long an in-flight pass may finish after SIGTERM (default 25s)
//	CP_RELAY_WORKER_SAMPLE_INTERVAL      depth/lag gauge interval (default 15s; 0 disables)
//	CP_RELAY_WORKER_EXCLUSIVE            relay only while holding the advisory lock (default false)
//	CP_RELAY_WORKER_ALLOW_LOOPBACK_BUS   LOCAL/TEST/DEV only: accept the in-process loopback bus
//
// The bus is selected by CP_PROVIDER_EVENT_BUS_MODE and the worker fails
// closed. sandbox and live get the franz-go Redpanda client, which refuses to
// start unless the brokers answer. fake resolves to an in-process loopback
// with no subscriber outside this process, so it is refused unless an
// operator opts in explicitly, and the opt-in is itself refused outside
// LOCAL/TEST/DEV. A relay that appears to run and publishes nowhere is worse
// than one that will not start: the outbox drains and the events are gone.
//
// Shutdown is a drain, not a kill. SIGTERM stops the worker claiming another
// batch; the pass already in flight keeps a context the signal does not
// touch and is allowed to commit for up to the drain budget. If it overruns
// it is abandoned, which is safe because a pass is one transaction: the
// rollback marks nothing published, releases every row lock and leaves the
// backlog exactly as it was. Nothing is ever left claimed-but-unpublished
// and no partition stalls because a process stopped.
//
// Several instances may run at once and coordinate entirely through
// Postgres, with no lease, heartbeat or leader election. SKIP LOCKED gives
// each row to exactly one of them, so no event is published twice; and since
// D-036 internal/event checks every claimed row — not just the oldest of
// each partition — against the rows another instance holds, so a partition
// keeps its order even when its events are split across instances mid-drain.
// Both properties are asserted end to end in this package's integration
// suite, under concurrent claiming and a publish failure mid-partition.
//
// CP_RELAY_WORKER_EXCLUSIVE=true makes the instances elect one active relay
// through a session-level advisory lock instead. It is an operator switch
// for a deliberate single publisher, not a safety control, and it is off by
// default because a singleton costs drain rate and failover latency and now
// protects nothing; see exclusive.go.
//
// Exit codes: 0 success, 1 failure, 2 usage error, 3 status lag threshold
// exceeded.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/observability"
)

// Worker-specific environment variables. Everything else is internal/config.
const (
	envBatchSize        = "CP_RELAY_WORKER_BATCH_SIZE"
	envPollInterval     = "CP_RELAY_WORKER_POLL_INTERVAL"
	envMaxInterval      = "CP_RELAY_WORKER_MAX_INTERVAL"
	envRetryBackoffBase = "CP_RELAY_WORKER_RETRY_BACKOFF_BASE"
	envRetryBackoffMax  = "CP_RELAY_WORKER_RETRY_BACKOFF_MAX"
	envRunTimeout       = "CP_RELAY_WORKER_RUN_TIMEOUT"
	envDrainTimeout     = "CP_RELAY_WORKER_DRAIN_TIMEOUT"
	envSampleInterval   = "CP_RELAY_WORKER_SAMPLE_INTERVAL"
	envAllowLoopback    = "CP_RELAY_WORKER_ALLOW_LOOPBACK_BUS"
	envExclusive        = "CP_RELAY_WORKER_EXCLUSIVE"

	serviceName = "relay-worker"

	// DefaultDrainTimeout leaves room for one full pass to commit inside a
	// typical 30 s container stop grace period.
	DefaultDrainTimeout = 25 * time.Second
	// DefaultSampleInterval is how often depth and lag gauges are refreshed.
	DefaultSampleInterval = 15 * time.Second
	// busCloseTimeout bounds the bus shutdown after the loop has stopped.
	busCloseTimeout = 10 * time.Second
	// telemetryFlushTimeout bounds the final metric/trace export.
	telemetryFlushTimeout = 10 * time.Second

	exitOK          = 0
	exitFailure     = 1
	exitUsage       = 2
	exitLagExceeded = 3
)

var (
	errUsage       = errors.New("usage")
	errLagExceeded = errors.New("relay lag exceeds the configured threshold")
)

func main() { os.Exit(run(os.Args[1:], os.LookupEnv, os.Stdout, os.Stderr)) }

func run(args []string, lookup func(string) (string, bool), stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]

	var (
		passes       int
		asJSON       bool
		maxLag       time.Duration
		blockedLimit int
	)
	switch cmd {
	case "help", "-h", "--help":
		usage(stdout)
		return exitOK
	case "run":
		if len(rest) != 0 {
			fmt.Fprintln(stderr, serviceName+": run takes no arguments")
			return exitUsage
		}
	case "once":
		fs := flag.NewFlagSet("once", flag.ContinueOnError)
		fs.SetOutput(stderr)
		fs.IntVar(&passes, "passes", 1, "claim/publish passes to run; 0 means until a pass publishes nothing")
		if err := fs.Parse(rest); err != nil {
			return exitUsage
		}
		if fs.NArg() != 0 || passes < 0 {
			fmt.Fprintln(stderr, serviceName+": once takes no positional arguments and -passes must be >= 0")
			return exitUsage
		}
	case "status":
		fs := flag.NewFlagSet("status", flag.ContinueOnError)
		fs.SetOutput(stderr)
		fs.BoolVar(&asJSON, "json", false, "print the report as JSON")
		fs.DurationVar(&maxLag, "max-lag", 0, "exit 3 when the oldest unpublished row is older than this")
		fs.IntVar(&blockedLimit, "blocked", DefaultBlockedLimit, "list at most this many blocked partitions")
		if err := fs.Parse(rest); err != nil {
			return exitUsage
		}
		if fs.NArg() != 0 || maxLag < 0 || blockedLimit < 0 {
			fmt.Fprintln(stderr, serviceName+": status takes no positional arguments and -max-lag/-blocked must be >= 0")
			return exitUsage
		}
	default:
		fmt.Fprintf(stderr, "%s: unknown command %q\n", serviceName, cmd)
		usage(stderr)
		return exitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	d, err := wire(ctx, lookup, stderr, cmd != "status")
	if err != nil {
		fmt.Fprintln(stderr, serviceName+":", err)
		return exitFailure
	}
	defer d.close(ctx)

	switch cmd {
	case "run":
		err = cmdRun(ctx, d)
	case "once":
		err = cmdOnce(ctx, d, passes, stdout)
	case "status":
		err = cmdStatus(ctx, d, statusRequest{json: asJSON, maxLag: maxLag, blocked: blockedLimit}, stdout)
	}
	switch {
	case errors.Is(err, errLagExceeded):
		fmt.Fprintln(stderr, serviceName+":", err)
		return exitLagExceeded
	case errors.Is(err, errUsage):
		fmt.Fprintln(stderr, serviceName+":", err)
		usage(stderr)
		return exitUsage
	case err != nil:
		fmt.Fprintln(stderr, serviceName+":", err)
		return exitFailure
	}
	return exitOK
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `usage: relay-worker <command> [flags]

  run                       claim committed outbox rows and publish them until SIGINT/SIGTERM
  once [-passes N]          run N claim/publish passes and exit (default 1; 0 = until a pass publishes nothing)
  status [-json]            report outbox depth, relay lag, per-topic backlog and blocked partitions
         [-max-lag D]       exit %d when the oldest unpublished row is older than D
         [-blocked N]       list at most N blocked partitions (default %d)

env: CP_* (see internal/config); %s (default %d); %s (default %s);
     %s (default %s); %s (default %s); %s (default %s);
     %s (default %s); %s (default %s); %s (default %s);
     %s (default false); %s (LOCAL/TEST/DEV only)

The bus is chosen by %s and the worker fails closed: fake mode is an
in-process loopback with no subscriber outside this process, so it is refused unless
%s=true in LOCAL, TEST or DEV.

Any number of instances may relay at once: SKIP LOCKED gives each row to one of them
and internal/event keeps each partition in order across them. Set %s=true only when
you want a deliberate single publisher; it costs drain rate and failover latency.
`,
		exitLagExceeded, DefaultBlockedLimit,
		envBatchSize, event.DefaultBatchSize, envPollInterval, event.DefaultPollInterval,
		envMaxInterval, event.DefaultMaxInterval, envRetryBackoffBase, event.DefaultRetryBackoffBase,
		envRetryBackoffMax, event.DefaultRetryBackoffMax, envRunTimeout, event.DefaultRunTimeout,
		envDrainTimeout, DefaultDrainTimeout, envSampleInterval, DefaultSampleInterval,
		envExclusive, envAllowLoopback, envVarEventBusMode, envAllowLoopback, envExclusive)
}

// workerConfig is everything this binary reads beyond internal/config.
type workerConfig struct {
	Relay         event.RelayOptions
	Drain         time.Duration
	Sample        time.Duration
	AllowLoopback bool
	Exclusive     bool
}

// loadWorkerConfig reads the worker tunables. Every value is validated here
// so a typo is a startup failure and never a silently different schedule.
func loadWorkerConfig(lookup func(string) (string, bool), env config.Environment) (workerConfig, error) {
	var (
		wc  workerConfig
		err error
	)
	if wc.Relay.BatchSize, err = intVar(lookup, envBatchSize, event.DefaultBatchSize); err != nil {
		return workerConfig{}, err
	}
	for _, f := range []struct {
		name string
		def  time.Duration
		dst  *time.Duration
	}{
		{envPollInterval, event.DefaultPollInterval, &wc.Relay.PollInterval},
		{envMaxInterval, event.DefaultMaxInterval, &wc.Relay.MaxInterval},
		{envRetryBackoffBase, event.DefaultRetryBackoffBase, &wc.Relay.RetryBackoffBase},
		{envRetryBackoffMax, event.DefaultRetryBackoffMax, &wc.Relay.RetryBackoffMax},
		{envRunTimeout, event.DefaultRunTimeout, &wc.Relay.RunTimeout},
		{envDrainTimeout, DefaultDrainTimeout, &wc.Drain},
	} {
		if *f.dst, err = durationVar(lookup, f.name, f.def); err != nil {
			return workerConfig{}, err
		}
	}
	// A zero sample interval disables the sampler, so it is the one
	// duration allowed to be zero.
	if wc.Sample, err = optionalDurationVar(lookup, envSampleInterval, DefaultSampleInterval); err != nil {
		return workerConfig{}, err
	}
	if wc.AllowLoopback, err = boolVar(lookup, envAllowLoopback); err != nil {
		return workerConfig{}, err
	}
	if wc.AllowLoopback && env.IsProductionLike() {
		return workerConfig{}, fmt.Errorf("%s is set but the in-process loopback bus is never permitted in %s", envAllowLoopback, env)
	}
	// Exclusive relaying defaults OFF: concurrent instances are safe on
	// their own, and a singleton costs drain rate and failover latency for
	// no correctness gain. See exclusive.go.
	if wc.Exclusive, err = boolVar(lookup, envExclusive); err != nil {
		return workerConfig{}, err
	}
	return wc, nil
}

// deps is everything the subcommands need.
type deps struct {
	cfg           *config.Config
	db            *db.DB
	log           *slog.Logger
	clk           clock.Clock
	resolver      config.Resolver
	metrics       *relayMetrics
	relayOpts     event.RelayOptions
	drain         time.Duration
	sample        time.Duration
	allowLoopback bool
	exclusive     bool
	shutdownOTel  func(context.Context) error
}

// close releases the pool and flushes telemetry on a context the shutdown
// signal has already cancelled, so the flush is never skipped.
func (d *deps) close(ctx context.Context) {
	if d == nil {
		return
	}
	if d.shutdownOTel != nil {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), telemetryFlushTimeout)
		defer cancel()
		if err := d.shutdownOTel(sctx); err != nil {
			d.log.Error("relay-worker: telemetry shutdown failed", "error", err)
		}
	}
	if d.db != nil {
		d.db.Close()
	}
}

// wire loads configuration and builds every dependency. needsBus is false
// for `status`, which opens no transport and sets up no telemetry: it must
// answer during an incident without depending on either.
//
// CP_ENV defaults to LOCAL with a warning, mirroring the other workers, so a
// developer can inspect a local outbox without a full environment; every
// deployed environment sets it explicitly and config.Validate then applies
// the production rules, including RuleNoFakeProviders.
func wire(ctx context.Context, lookup func(string) (string, bool), stderr io.Writer, needsBus bool) (*deps, error) {
	if v, ok := lookup(config.EnvVarEnvironment); !ok || strings.TrimSpace(v) == "" {
		fmt.Fprintf(stderr, "%s: WARNING %s is not set; assuming LOCAL\n", serviceName, config.EnvVarEnvironment)
		inner := lookup
		lookup = func(k string) (string, bool) {
			if k == config.EnvVarEnvironment {
				return string(config.EnvLocal), true
			}
			return inner(k)
		}
	}
	cfg, err := config.Load(ctx, config.ServiceRelayWorker, lookup)
	if err != nil {
		return nil, err
	}
	log := observability.NewLogger(cfg.Env, stderr).
		With("service", serviceName, "build_version", cfg.BuildVersion, "env", cfg.Env)
	wc, err := loadWorkerConfig(lookup, cfg.Env)
	if err != nil {
		return nil, err
	}

	d := &deps{
		cfg: cfg, log: log, clk: clock.System(), resolver: config.NewResolver(cfg.Env, lookup),
		relayOpts: wc.Relay, drain: wc.Drain, sample: wc.Sample,
		allowLoopback: wc.AllowLoopback, exclusive: wc.Exclusive,
	}
	// Fail closed first. A worker that cannot name a real bus must refuse
	// before it holds a database connection and looks like it is working.
	if needsBus {
		if _, err = resolveBusBinding(cfg.Env, cfg.Providers.EventBus.Mode, wc.AllowLoopback); err != nil {
			return nil, err
		}
		if d.shutdownOTel, err = observability.Setup(ctx, cfg.Telemetry, serviceName, cfg.BuildVersion, cfg.Env); err != nil {
			return nil, err
		}
	}
	if d.metrics, err = newRelayMetrics(otel.GetMeterProvider().Meter("github.com/nodal/controlplane/cmd/relay-worker")); err != nil {
		d.close(ctx)
		return nil, err
	}
	d.relayOpts.Clock = d.clk
	d.relayOpts.Observer = d.metrics

	dbURL, err := d.resolver.Resolve(ctx, cfg.Database.AppURL)
	if err != nil {
		d.close(ctx)
		return nil, fmt.Errorf("resolve database url: %w", err)
	}
	pool, err := db.Open(ctx, db.Config{
		URL: dbURL, AppName: serviceName, RequireTLS: cfg.Database.RequireTLS,
		MaxConns: cfg.Database.MaxConns, MinConns: cfg.Database.MinConns,
		StatementTimeout: cfg.Database.StatementTimeout, LockTimeout: cfg.Database.LockTimeout,
	})
	if err != nil {
		d.close(ctx)
		return nil, err
	}
	d.db = pool
	return d, nil
}

// newRunner assembles the relay and its host loop over an already-open bus.
func (d *deps) newRunner(bus event.Bus) *Runner {
	return &Runner{
		relay:     event.NewRelay(d.db, bus, d.log, d.relayOpts),
		metrics:   d.metrics,
		db:        d.db,
		clk:       d.clk,
		log:       d.log,
		drain:     d.drain,
		sample:    d.sample,
		exclusive: d.exclusive,
	}
}

// closeBus shuts the transport down on a context the signal has already
// cancelled, so a drained relay still leaves its consumer groups cleanly.
func (d *deps) closeBus(ctx context.Context, bus event.Bus) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), busCloseTimeout)
	defer cancel()
	if err := bus.Close(cctx); err != nil {
		d.log.Error("relay-worker: closing the event bus failed", "error", err)
	}
}

func cmdRun(ctx context.Context, d *deps) error {
	bus, err := d.openBus(ctx)
	if err != nil {
		return err
	}
	defer d.closeBus(ctx, bus)

	d.log.Info("relay-worker: starting", "config_hash", d.cfg.Hash(), "topics_registered", len(event.Topics()))
	r := d.newRunner(bus)
	if err := r.Run(ctx); err != nil {
		return err
	}

	c := d.metrics.counters()
	attrs := []any{
		"published", c.published, "publish_failures", c.failed,
		"retry_attempts", c.retried, "unregistered_topic_events", c.unregistered,
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotTimeout)
	defer cancel()
	if snap, serr := TakeSnapshot(sctx, d.db, d.clk.Now()); serr == nil {
		attrs = append(attrs, "outbox_depth", snap.Unpublished, "lag", snap.OldestAge().Round(time.Second),
			"blocked_partitions", snap.BlockedPartitions)
	}
	d.log.Info("relay-worker: stopped", attrs...)
	return nil
}

// cmdOnce runs a bounded number of passes and reports what moved. It is the
// incident-response and test entry point: one pass, or "keep going until a
// pass publishes nothing" with -passes 0.
func cmdOnce(ctx context.Context, d *deps, passes int, stdout io.Writer) error {
	bus, err := d.openBus(ctx)
	if err != nil {
		return err
	}
	defer d.closeBus(ctx, bus)
	return relayOnce(ctx, d, bus, passes, stdout)
}

// relayOnce is cmdOnce over an already-open bus. It takes the same exclusive
// lease the loop does: a manual drain that races a running worker is exactly
// the concurrency the lease exists to prevent.
func relayOnce(ctx context.Context, d *deps, bus event.Bus, passes int, stdout io.Writer) error {
	r := d.newRunner(bus)
	defer r.ReleaseLease(ctx)
	active, err := r.ensureLease(ctx)
	if err != nil {
		return err
	}
	if !active {
		return fmt.Errorf("another relay-worker instance holds the exclusive relay lease; stop it first, "+
			"or set %s=false to accept concurrent relaying", envExclusive)
	}
	var total, ran int
	for passes == 0 || ran < passes {
		if ctx.Err() != nil {
			break
		}
		published, _, err := r.pass(ctx, d.relayOpts.RunTimeout)
		ran++
		total += published
		if err != nil {
			return err
		}
		// A pass that publishes nothing has drained everything currently
		// eligible: whatever is left is waiting out a persisted retry
		// deadline, and another pass now would only re-read an empty claim.
		if published == 0 {
			break
		}
	}
	snap := r.Sample(context.WithoutCancel(ctx))
	c := d.metrics.counters()
	fmt.Fprintf(stdout, "passes=%d published=%d failures=%d unregistered=%d remaining=%d lag=%s blocked_partitions=%d\n",
		ran, total, c.failed, c.unregistered, snap.Unpublished, snap.OldestAge().Round(time.Second), snap.BlockedPartitions)
	return nil
}

// statusRequest is the parsed `status` flag set.
type statusRequest struct {
	json    bool
	maxLag  time.Duration
	blocked int
}

// cmdStatus answers "is the relay behind, and by how much" without any SQL
// and without a bus: it must work while the broker is down, which is exactly
// when it is asked.
func cmdStatus(ctx context.Context, d *deps, req statusRequest, stdout io.Writer) error {
	qctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
	defer cancel()
	st, err := TakeStatus(qctx, d.db, d.clk.Now(), req.blocked)
	if err != nil {
		return err
	}
	if req.json {
		err = st.WriteJSON(stdout)
	} else {
		err = st.WriteText(stdout)
	}
	if err != nil {
		return err
	}
	if age := st.Snapshot.OldestAge(); req.maxLag > 0 && age > req.maxLag {
		return fmt.Errorf("%w: oldest unpublished event is %s old, threshold %s",
			errLagExceeded, age.Round(time.Second), req.maxLag)
	}
	return nil
}

func intVar(lookup func(string) (string, bool), name string, def int) (int, error) {
	v, ok := lookup(name)
	if !ok || strings.TrimSpace(v) == "" {
		return def, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s: expected a positive integer, got %q", name, v)
	}
	return n, nil
}

func durationVar(lookup func(string) (string, bool), name string, def time.Duration) (time.Duration, error) {
	v, ok := lookup(name)
	if !ok || strings.TrimSpace(v) == "" {
		return def, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s: expected a positive Go duration, got %q", name, v)
	}
	return d, nil
}

// optionalDurationVar accepts zero, which disables the feature it configures.
func optionalDurationVar(lookup func(string) (string, bool), name string, def time.Duration) (time.Duration, error) {
	v, ok := lookup(name)
	if !ok || strings.TrimSpace(v) == "" {
		return def, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%s: expected a non-negative Go duration, got %q", name, v)
	}
	return d, nil
}

// boolVar parses a strict boolean defaulting to false: anything
// unrecognized is a startup failure, never a silent false.
func boolVar(lookup func(string) (string, bool), name string) (bool, error) {
	v, ok := lookup(name)
	v = strings.TrimSpace(v)
	if !ok || v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: expected a boolean, got %q", name, v)
	}
	return b, nil
}
