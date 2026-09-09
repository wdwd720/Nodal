// Command workflow-worker hosts the Temporal workflows and activities defined
// in internal/workflows: the funding deposit lifecycle and the reconciliation
// escalation ladder (goal PARTS 51, 115, 116, 163).
//
// It is a composition root and nothing else. It loads configuration, opens the
// database, builds the adapters that connect internal/workflows' narrow
// interfaces to internal/funding, internal/reconciliation and
// internal/killswitch, registers everything on one task queue, and runs the
// worker until SIGTERM.
//
// Workflow state is never financial truth (PART 116). Postgres records
// balances, deposits and reconciliation records; a workflow only decides what
// to do next and when. That is why this binary can be restarted, redeployed
// or lost entirely without anyone's money changing.
//
// Usage:
//
//	workflow-worker run     host the workflows until SIGINT/SIGTERM
//	workflow-worker check   validate configuration and connectivity, then exit
//
// Configuration comes from internal/config (CP_* variables), plus:
//
//	CP_WORKFLOW_WORKER_MAX_ACTIVITIES   concurrent activity executions (default 16)
//	CP_WORKFLOW_WORKER_MAX_WORKFLOWS    concurrent workflow task executions (default 16)
//	CP_WORKFLOW_WORKER_DRAIN_TIMEOUT    SIGTERM drain budget (default 25s)
//
// Shutdown is a drain: the worker stops polling for new tasks, lets in-flight
// activities finish within the drain budget, and exits. An activity cut short
// by the deadline is retried by Temporal on another worker, which is safe
// because every activity here is idempotent.
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

	"go.temporal.io/sdk/client"
	temporallog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/worker"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/workflows"
)

// Worker-specific environment variables. Everything else is internal/config.
const (
	envMaxActivities = "CP_WORKFLOW_WORKER_MAX_ACTIVITIES"
	envMaxWorkflows  = "CP_WORKFLOW_WORKER_MAX_WORKFLOWS"
	envDrainTimeout  = "CP_WORKFLOW_WORKER_DRAIN_TIMEOUT"

	serviceName = "workflow-worker"

	defaultMaxActivities = 16
	defaultMaxWorkflows  = 16
	defaultDrainTimeout  = 25 * time.Second

	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

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
	case "run", "check":
		if len(rest) != 0 {
			fmt.Fprintf(stderr, "%s: %s takes no arguments\n", serviceName, cmd)
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
	case "check":
		err = cmdCheck(ctx, d, stdout)
	}
	if err != nil {
		fmt.Fprintln(stderr, serviceName+":", err)
		return exitFailure
	}
	return exitOK
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `usage: workflow-worker <command>

  run      host the funding and reconciliation-escalation workflows until SIGINT/SIGTERM
  check    validate configuration, database and Temporal connectivity, then exit

env: CP_* (see internal/config); %s (default %d); %s (default %d); %s (default %s)
`, envMaxActivities, defaultMaxActivities, envMaxWorkflows, defaultMaxWorkflows, envDrainTimeout, defaultDrainTimeout)
}

// deps is everything the subcommands need.
type deps struct {
	cfg          *config.Config
	db           *db.DB
	log          *slog.Logger
	clk          clock.Clock
	temporal     client.Client
	taskQueue    string
	activities   workflows.Deps
	maxActivity  int
	maxWorkflow  int
	drainTimeout time.Duration
}

func (d *deps) close() {
	if d == nil {
		return
	}
	if d.temporal != nil {
		d.temporal.Close()
	}
	if d.db != nil {
		d.db.Close()
	}
}

// wire loads configuration and builds every dependency.
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
	cfg, err := config.Load(ctx, config.ServiceWorkflowWorker, lookup)
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

	out := &deps{
		cfg: cfg, db: pool, log: log, clk: clock.System(),
		taskQueue: workflows.TaskQueue(cfg.Temporal.TaskQueuePrefix),
	}
	if out.maxActivity, err = intVar(lookup, envMaxActivities, defaultMaxActivities); err != nil {
		pool.Close()
		return nil, err
	}
	if out.maxWorkflow, err = intVar(lookup, envMaxWorkflows, defaultMaxWorkflows); err != nil {
		pool.Close()
		return nil, err
	}
	if out.drainTimeout, err = durationVar(lookup, envDrainTimeout, defaultDrainTimeout); err != nil {
		pool.Close()
		return nil, err
	}

	out.activities, err = buildActivities(out)
	if err != nil {
		pool.Close()
		return nil, err
	}

	out.temporal, err = client.Dial(client.Options{
		HostPort:  cfg.Temporal.HostPort,
		Namespace: cfg.Temporal.Namespace,
		Logger:    temporallog.NewStructuredLogger(log),
	})
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("dial temporal at %s: %w", cfg.Temporal.HostPort, err)
	}
	return out, nil
}

// buildActivities connects internal/workflows' narrow interfaces to the
// packages that own the state.
//
// The funding side is wired here; the reconciliation side is not, because
// internal/reconciliation's repository is being written in parallel and this
// binary must not guess at its shape. Until it lands, an escalation activity
// group is built over a store that fails closed, so a workflow started
// against this build reports a clear UNSUPPORTED rather than silently
// pretending a record was escalated.
func buildActivities(d *deps) (workflows.Deps, error) {
	var out workflows.Deps
	alerter := newLogAlerter(d.log)

	driver, err := bindFundingDriver(d)
	if err != nil {
		return workflows.Deps{}, err
	}
	if driver != nil {
		fa, err := workflows.NewFundingActivities(driver, alerter, d.log)
		if err != nil {
			return workflows.Deps{}, err
		}
		out.Funding = fa
	}

	records, containment := bindReconciliation(d)
	ea, err := workflows.NewEscalationActivities(records, alerter, containment, d.log)
	if err != nil {
		return workflows.Deps{}, err
	}
	out.Escalation = ea
	if out.Funding == nil && out.Escalation == nil {
		return workflows.Deps{}, errors.New("no workflow activities could be wired")
	}
	return out, nil
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
	dur, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil || dur <= 0 {
		return 0, fmt.Errorf("%s: expected a positive Go duration, got %q", name, v)
	}
	return dur, nil
}

// cmdRun hosts the worker until the signal context is done.
func cmdRun(ctx context.Context, d *deps) error {
	w := worker.New(d.temporal, d.taskQueue, worker.Options{
		MaxConcurrentActivityExecutionSize:     d.maxActivity,
		MaxConcurrentWorkflowTaskExecutionSize: d.maxWorkflow,
		WorkerStopTimeout:                      d.drainTimeout,
		OnFatalError:                           func(err error) { d.log.Error("workflow-worker: fatal worker error", "error", err) },
	})
	if err := workflows.Register(w, d.activities); err != nil {
		return err
	}
	d.log.Info("workflow-worker: starting",
		"task_queue", d.taskQueue, "namespace", d.cfg.Temporal.Namespace, "host_port", d.cfg.Temporal.HostPort,
		"workflows", workflows.WorkflowNames(), "activities", workflows.ActivityNames(),
		"max_activities", d.maxActivity, "max_workflow_tasks", d.maxWorkflow,
		"drain_timeout", d.drainTimeout, "config_hash", d.cfg.Hash())

	if err := w.Start(); err != nil {
		return fmt.Errorf("start worker: %w", err)
	}
	<-ctx.Done()

	// Stop drains: it stops polling, waits for in-flight tasks up to
	// WorkerStopTimeout, and returns. Anything cut short is retried by
	// Temporal on another worker, which is safe because every activity is
	// idempotent.
	d.log.Info("workflow-worker: draining", "drain_timeout", d.drainTimeout)
	w.Stop()
	d.log.Info("workflow-worker: stopped")
	return nil
}

// cmdCheck validates that the worker could run: configuration parses, the
// database answers, Temporal answers, and every workflow and activity
// registers without a name collision.
func cmdCheck(ctx context.Context, d *deps, stdout io.Writer) error {
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := d.db.Ping(pingCtx); err != nil {
		return fmt.Errorf("database ping: %w", err)
	}
	if _, err := d.temporal.CheckHealth(pingCtx, &client.CheckHealthRequest{}); err != nil {
		return fmt.Errorf("temporal health: %w", err)
	}
	w := worker.New(d.temporal, d.taskQueue, worker.Options{DisableRegistrationAliasing: true})
	if err := workflows.Register(w, d.activities); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "ok: env=%s namespace=%s task_queue=%s workflows=%v activities=%d\n",
		d.cfg.Env, d.cfg.Temporal.Namespace, d.taskQueue, workflows.WorkflowNames(), len(workflows.ActivityNames()))
	return nil
}
