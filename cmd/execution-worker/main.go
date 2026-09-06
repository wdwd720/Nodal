// Command execution-worker hosts the Settlement Compiler's resumable
// executor: it claims approved execution plans, leases them so no two
// processes drive the same plan, runs them step by step through
// internal/settlement, and advances the orders, attempts and fills that
// internal/execution owns.
//
// It contains no execution logic. Everything that decides what to submit,
// what a fill means, when a submission's fate is unknown and what to do
// about it lives in internal/settlement and internal/execution and is tested
// there, including TestExecutor_ResumeAfterCrash_EveryStepBoundary. This
// binary supplies the process: claiming, leasing, concurrency, wake-ups from
// the event bus, graceful shutdown and observability.
//
// Usage:
//
//	execution-worker run              claim and execute until SIGINT/SIGTERM
//	execution-worker plan <plan-id>   run exactly one plan and exit (operator tool)
//
// Configuration comes from internal/config (CP_* variables), plus:
//
//	CP_EXECUTION_WORKER_CONCURRENCY    plans in flight (default 4)
//	CP_EXECUTION_WORKER_POLL_INTERVAL  claim poll interval (default 500ms)
//	CP_EXECUTION_WORKER_LEASE_TTL      lease duration (default 60s)
//	CP_EXECUTION_WORKER_DRAIN_TIMEOUT  SIGTERM drain budget (default 25s)
//	CP_EXECUTION_WORKER_OWNER          owner id recorded on leases (default host:pid)
//
// Shutdown is a drain, not a kill. On SIGTERM the worker stops claiming
// immediately, lets in-flight plans finish for up to the drain budget, and
// only then cancels them. A cancelled plan rolls its open transaction back
// and stays exactly where its durable per-step state says it is; a cancelled
// submission becomes SUBMISSION_UNKNOWN and is investigated, never retried
// blindly and never called a failure.
//
// Exit codes: 0 success, 1 failure, 2 usage error.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/intent"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/positions"
	"github.com/nodal/controlplane/internal/quote"
	"github.com/nodal/controlplane/internal/settlement"
)

// Worker-specific environment variables. Everything else is internal/config.
const (
	envConcurrency  = "CP_EXECUTION_WORKER_CONCURRENCY"
	envPollInterval = "CP_EXECUTION_WORKER_POLL_INTERVAL"
	envLeaseTTL     = "CP_EXECUTION_WORKER_LEASE_TTL"
	envDrainTimeout = "CP_EXECUTION_WORKER_DRAIN_TIMEOUT"
	envOwner        = "CP_EXECUTION_WORKER_OWNER"

	serviceName = "execution-worker"

	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

var errUsage = errors.New("usage")

func main() { os.Exit(run(os.Args[1:], os.LookupEnv, os.Stdout, os.Stderr)) }

func run(args []string, lookup func(string) (string, bool), stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "help", "-h", "--help":
		usage(stdout)
		return exitOK
	case "run":
		if len(rest) != 0 {
			fmt.Fprintln(stderr, serviceName+": run takes no arguments")
			return exitUsage
		}
	case "plan":
		if len(rest) != 1 {
			fmt.Fprintln(stderr, serviceName+": plan expects exactly one <plan-id>")
			return exitUsage
		}
	default:
		fmt.Fprintf(stderr, "%s: unknown command %q\n", serviceName, cmd)
		usage(stderr)
		return exitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	d, err := wire(ctx, lookup, stderr)
	if err != nil {
		fmt.Fprintln(stderr, serviceName+":", err)
		return exitFailure
	}
	defer d.close()

	switch cmd {
	case "run":
		err = cmdRun(ctx, d)
	case "plan":
		err = cmdPlan(ctx, d, rest[0], stdout)
	}
	if err != nil {
		if errors.Is(err, errUsage) {
			fmt.Fprintln(stderr, serviceName+":", err)
			usage(stderr)
			return exitUsage
		}
		fmt.Fprintln(stderr, serviceName+":", err)
		return exitFailure
	}
	return exitOK
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `usage: execution-worker <command> [args]

  run                  claim leased execution plans and run them until SIGINT/SIGTERM
  plan <plan-id>       run exactly one plan to its next resting state and exit

env: CP_* (see internal/config); %s (default %d); %s (default %s);
     %s (default %s); %s (default %s); %s (default host:pid)
`, envConcurrency, DefaultConcurrency, envPollInterval, DefaultPollInterval,
		envLeaseTTL, DefaultLeaseTTL, envDrainTimeout, DefaultDrainTimeout, envOwner)
}

// deps is everything the subcommands need.
type deps struct {
	cfg   *config.Config
	db    *db.DB
	log   *slog.Logger
	clk   clock.Clock
	queue *PGPlanQueue
	exec  *settlement.Executor
	opts  RunnerOptions
	waker *Waker
}

func (d *deps) close() {
	if d != nil && d.db != nil {
		d.db.Close()
	}
}

// wire loads configuration and builds every dependency.
//
// The executor is assembled here and nowhere else. The two dependencies that
// this worker cannot yet supply — a venue adapter and a signing client — are
// selected by config.ProviderMode. Fake modes are refused outside
// LOCAL/TEST/DEV by config.Validate (RuleNoFakeProviders), so no environment
// variable can turn this binary into a live trader on its own; the live
// capability gates in the database decide that, and they default DISABLED.
func wire(ctx context.Context, lookup func(string) (string, bool), stderr io.Writer) (*deps, error) {
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
	cfg, err := config.Load(ctx, lookup)
	if err != nil {
		return nil, err
	}
	log := observability.NewLogger(cfg.Env, stderr).
		With("service", serviceName, "build_version", cfg.BuildVersion, "env", cfg.Env)
	resolver := config.NewResolver(cfg.Env, lookup)

	dbURL, err := resolver.Resolve(ctx, cfg.Database.AppURL)
	if err != nil {
		return nil, fmt.Errorf("resolve database url: %w", err)
	}
	pool, err := db.Open(ctx, db.Config{
		URL: dbURL, AppName: serviceName, RequireTLS: cfg.Database.RequireTLS,
		MaxConns: cfg.Database.MaxConns, MinConns: cfg.Database.MinConns,
		StatementTimeout: cfg.Database.StatementTimeout, LockTimeout: cfg.Database.LockTimeout,
	})
	if err != nil {
		return nil, err
	}
	out := &deps{cfg: cfg, db: pool, log: log, clk: clock.System()}
	out.opts, err = runnerOptions(lookup)
	if err != nil {
		pool.Close()
		return nil, err
	}
	out.queue = NewPGPlanQueue(pool, func() time.Time { return out.clk.Now().UTC() })
	out.waker, err = NewWaker(pool, event.NewInbox(out.clk), log)
	if err != nil {
		pool.Close()
		return nil, err
	}

	// The venue adapter, the chain observer, the transaction inspector, the
	// signing client and the unknown-submission recoverer are provider-mode
	// selected and are not wired in this repository yet (EB-005, EB-010,
	// EB-011 and the reconciliation engine). Deps.validate refuses a nil
	// dependency at construction, so this is a loud, early failure rather
	// than a plan that dies halfway through.
	sd, err := settlementDeps(out)
	if err != nil {
		pool.Close()
		return nil, err
	}
	ex, err := settlement.NewExecutor(sd.deps, sd.signer)
	if err != nil {
		pool.Close()
		return nil, err
	}
	out.exec = ex
	return out, nil
}

// executorDeps is the assembled dependency set plus the signer, which is
// separate because only the live executor may hold one.
type executorDeps struct {
	deps   settlement.Deps
	signer settlement.Signer
}

// ProviderBinding supplies the provider-mode-selected collaborators the
// executor needs. It is a seam, not indirection for its own sake: the venue
// adapter, chain observer, inspector, signer and recoverer are all external
// integrations whose live implementations are blocked on provider approvals,
// and tests substitute the settlementtest fakes here.
type ProviderBinding interface {
	Adapter() execution.ExecutionAdapter
	Observer() settlement.ChainObserver
	Inspector() settlement.TransactionInspector
	Recoverer() settlement.Recoverer
	Signer() settlement.Signer
	Risk() settlement.FinalRiskChecker
	Archive() execution.ArchiveWriter
}

// bindProviders is replaced in tests. In production it fails closed: no
// binding exists yet for any provider mode, and a worker that cannot name
// its venue adapter must refuse to start rather than invent one.
var bindProviders = func(cfg *config.Config) (ProviderBinding, error) {
	return nil, fmt.Errorf(
		"no execution provider binding for mode %q: the venue adapter, chain observer, inspector, signing client and recoverer are not wired in this build",
		cfg.Providers.Execution.Mode)
}

func settlementDeps(d *deps) (executorDeps, error) {
	binding, err := bindProviders(d.cfg)
	if err != nil {
		return executorDeps{}, err
	}
	outbox := event.NewOutbox(d.clk)
	aud := audit.NewWriter()
	sd := settlement.Deps{
		DB:           d.db,
		Plans:        settlement.NewPlanRepository(d.clk, aud),
		Orders:       execution.NewRepository(d.clk, outbox, aud),
		Attempts:     execution.NewAttemptRepository(d.clk, outbox, aud),
		Quotes:       quoteStore{repo: quote.NewRepository()},
		Intents:      intentReader{repo: intent.NewRepository(d.clk, outbox, aud)},
		Adapter:      binding.Adapter(),
		Observer:     binding.Observer(),
		Capital:      capital.NewService(d.clk, capitalEmitter{outbox: outbox, clk: d.clk}),
		Ledger:       ledger.NewService(d.clk, serviceName),
		Positions:    positions.NewEngine(),
		KillSwitches: killswitch.NewChecker(killswitch.Policy{}),
		Risk:         binding.Risk(),
		Inspector:    binding.Inspector(),
		Recoverer:    binding.Recoverer(),
		Audit:        aud,
		Archive:      binding.Archive(),
		Clock:        d.clk,
	}
	return executorDeps{deps: sd, signer: binding.Signer()}, nil
}

// runnerOptions reads the worker-specific tunables.
func runnerOptions(lookup func(string) (string, bool)) (RunnerOptions, error) {
	o := RunnerOptions{Owner: defaultOwner()}
	if v, ok := lookup(envOwner); ok && strings.TrimSpace(v) != "" {
		o.Owner = strings.TrimSpace(v)
	}
	n, err := intVar(lookup, envConcurrency, DefaultConcurrency)
	if err != nil {
		return RunnerOptions{}, err
	}
	o.Concurrency = n
	for _, f := range []struct {
		name string
		def  time.Duration
		dst  *time.Duration
	}{
		{envPollInterval, DefaultPollInterval, &o.PollInterval},
		{envLeaseTTL, DefaultLeaseTTL, &o.LeaseTTL},
		{envDrainTimeout, DefaultDrainTimeout, &o.DrainTimeout},
	} {
		d, err := durationVar(lookup, f.name, f.def)
		if err != nil {
			return RunnerOptions{}, err
		}
		*f.dst = d
	}
	return o, nil
}

func defaultOwner() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	return host + ":" + strconv.Itoa(os.Getpid())
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

// bindBus supplies the event bus the waker consumes. It is a seam: the
// Redpanda client lives in another package that this binary does not yet
// depend on, and LOCAL/TEST may substitute the in-memory bus.
//
// A nil bus is not an error. The wake-up is an optimisation on top of
// polling: without it a plan is picked up at the next claim instead of the
// instant its event arrives. Nothing about correctness depends on it, which
// is exactly why it may be absent.
var bindBus = func(cfg *config.Config) (event.Bus, error) { return nil, nil }

// cmdRun claims and executes plans until the signal context is done.
func cmdRun(ctx context.Context, d *deps) error {
	r, err := NewRunner(d.queue, d.exec, d.log, d.clk, d.opts)
	if err != nil {
		return err
	}
	bus, err := bindBus(d.cfg)
	if err != nil {
		return err
	}
	if bus == nil {
		d.log.Warn("execution-worker: no event bus is wired; plans are picked up by polling only",
			"poll_interval", r.Options().PollInterval)
	} else {
		if err := d.waker.Subscribe(ctx, bus, d.opts.Owner); err != nil {
			return err
		}
		defer func() {
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if cerr := bus.Close(cctx); cerr != nil {
				d.log.Error("execution-worker: closing the event bus failed", "error", cerr)
			}
		}()
		d.log.Info("execution-worker: subscribed for wake-ups", "topics", WakeTopics, "group", d.opts.Owner)
	}
	d.log.Info("execution-worker: starting",
		"owner", r.Options().Owner, "concurrency", r.Options().Concurrency,
		"lease_ttl", r.Options().LeaseTTL, "drain_timeout", r.Options().DrainTimeout,
		"config_hash", d.cfg.Hash())
	if err := r.Run(ctx); err != nil {
		return err
	}
	s := r.Stats()
	d.log.Info("execution-worker: stopped",
		"claimed", s.Claimed, "completed", s.Completed, "failed", s.Failed,
		"paused", s.Paused, "retried", s.Retried, "skipped", s.Skipped, "abandoned", s.Abandoned, "lease_lost", s.LeaseLost)
	return nil
}

// cmdPlan runs one named plan and prints the result. It takes the same lease
// as the loop would, so it can never race a running worker.
func cmdPlan(ctx context.Context, d *deps, planIDText string, stdout io.Writer) error {
	planID, err := settlement.ParsePlanID(strings.TrimSpace(planIDText))
	if err != nil {
		return fmt.Errorf("%w: %q is not a plan id", errUsage, planIDText)
	}
	leases, err := d.queue.Claim(ctx, d.opts.Owner, 1, d.opts.LeaseTTL)
	if err != nil {
		return err
	}
	var held *Lease
	for i := range leases {
		if leases[i].PlanID == planID {
			held = &leases[i]
			continue
		}
		// Give back anything else this claim happened to take.
		if rerr := d.queue.Release(ctx, leases[i], OutcomeAbandoned, nil, 0); rerr != nil {
			d.log.WarnContext(ctx, "execution-worker: release unrelated lease failed", "error", rerr)
		}
	}
	if held == nil {
		return fmt.Errorf("plan %s is not runnable now: it is leased, backed off, not approved, terminal, or a dry run", planID)
	}
	res, runErr := d.exec.Run(ctx, planID)
	out := OutcomeCompleted
	if runErr != nil || res.Status != settlement.PlanCompleted {
		out = OutcomeRetry
	}
	if rerr := d.queue.Release(ctx, *held, out, runErr, 0); rerr != nil {
		d.log.WarnContext(ctx, "execution-worker: release lease failed", "error", rerr)
	}
	fmt.Fprintf(stdout, "plan=%s status=%s order=%s stopped_at=%s reason=%s\n",
		res.PlanID, res.Status, res.OrderID, res.StoppedAt, res.Reason)
	return runErr
}
