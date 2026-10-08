// Command advanced-worker runs the time-driven work of the advanced financial
// capabilities on a schedule: reservation expiry, margin interest, mark to
// market and the liquidation queue, derivatives variation margin, funding,
// expiry settlement and clearing reconciliation, prediction market closing
// and escrow reconciliation, order book completion and journal proofs, equity
// settlement, corporate actions and the paper venue, scheduled marketplace
// follows, stable asset monitoring, supply reconciliation, unknown
// submissions and redemptions, bridge legs and routes, delegated authority
// upkeep and the transfers register -- each job one operation of its
// subsystem, every unit of work in its own transaction (internal/advancedjobs)
// -- and records the advanced metrics from the database
// (internal/advancedmetrics).
//
// Usage:
//
//	advanced-worker run          every job on its schedule until SIGINT/SIGTERM, then drain
//	advanced-worker once <job>   one run of one job; exit 1 if any unit failed
//	advanced-worker list         every job, its interval and batch, and whether it is wired
//
// Schedules: CP_ADVANCED_WORKER_INTERVALS and CP_ADVANCED_WORKER_BATCHES
// override a job's documented default (`list` prints them), as
// comma-separated <job>=<value> pairs; a name the worker does not run is a
// failed start. CP_ADVANCED_WORKER_RUN_TIMEOUT bounds one run (default 5m),
// CP_ADVANCED_WORKER_DRAIN_TIMEOUT bounds the drain at shutdown (default
// 25s), CP_ADVANCED_WORKER_METRICS_INTERVAL is the collector's interval
// (default 1m). No setting turns a job off.
//
// Kill switches: the worker consults none of its own and bypasses none. Every
// job calls a subsystem operation that applies the subsystem's own rule:
// settlement, liquidation, expiry and reconciliation keep running while a
// switch stops new risk (PART 52), and the only job that adds risk -- the
// scheduled marketplace follow -- is refused by the marketplace under a switch
// or a closed gate. Nothing here enables LIVE or a capability gate: the gate
// checker is built from the same CP_API_ENABLED_CAPABILITIES the API reads,
// so both decide alike.
//
// The jobs on the persistent simulated chains (stable assets, bridge,
// delegated authority) are wired only when CP_SANDBOX_DEVNET_NETWORK and
// CP_SANDBOX_DEVNET_SEED_REF name the sandbox chain, and the equity jobs only
// where the paper venue may run (LOCAL, TEST and DEV), exactly as the API
// wires those services; `list` says which are not wired and why.
//
// Exit codes: 0 success, 1 failure, 2 usage error.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/nodal/controlplane/internal/advancedjobs"
	"github.com/nodal/controlplane/internal/advancedmetrics"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/observability"
)

const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2

	serviceName = "advanced-worker"
	meterName   = "github.com/nodal/controlplane/cmd/advanced-worker"
)

func main() {
	os.Exit(run(os.Args[1:], os.LookupEnv, os.Stdout, os.Stderr))
}

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
	case "run", "list":
		if len(rest) != 0 {
			fmt.Fprintf(stderr, "%s: %s takes no arguments\n", serviceName, cmd)
			usage(stderr)
			return exitUsage
		}
	case "once":
		if len(rest) != 1 {
			fmt.Fprintf(stderr, "%s: once expects exactly one <job>\n", serviceName)
			usage(stderr)
			return exitUsage
		}
		if !knownJob(rest[0]) {
			fmt.Fprintf(stderr, "%s: unknown job %q; `%s list` prints every job\n", serviceName, rest[0], serviceName)
			return exitUsage
		}
	default:
		fmt.Fprintf(stderr, "%s: unknown command %q\n", serviceName, cmd)
		usage(stderr)
		return exitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := loadConfig(ctx, lookup, stderr)
	if err != nil {
		fmt.Fprintln(stderr, serviceName+":", err)
		return exitFailure
	}
	p := newPlan(cfg)
	if cmd == "list" {
		if err := p.write(stdout); err != nil {
			fmt.Fprintln(stderr, serviceName+":", err)
			return exitFailure
		}
		return exitOK
	}
	if cmd == "once" {
		if reason, unwired := p.unwired[rest[0]]; unwired {
			fmt.Fprintf(stderr, "%s: %s is not wired in this deployment: %s\n", serviceName, rest[0], reason)
			return exitFailure
		}
	}

	log := observability.NewLogger(cfg.Env, stderr).
		With("service", serviceName, "build_version", cfg.BuildVersion, "env", cfg.Env)
	if cmd == "run" {
		shutdownTelemetry, err := observability.Setup(ctx, cfg.Telemetry, cfg.ServiceName, config.BuildVersion, cfg.Env)
		if err != nil {
			fmt.Fprintln(stderr, serviceName+": telemetry:", err)
			return exitFailure
		}
		defer func() {
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = shutdownTelemetry(sctx)
		}()
	}

	resolver := config.NewResolver(cfg.Env, lookup)
	dbURL, err := resolver.Resolve(ctx, cfg.Database.AppURL)
	if err != nil {
		fmt.Fprintln(stderr, serviceName+": resolve database url:", err)
		return exitFailure
	}
	database, err := db.Open(ctx, db.Config{
		URL: dbURL, AppName: serviceName, RequireTLS: cfg.Database.RequireTLS,
		MaxConns: cfg.Database.MaxConns, MinConns: cfg.Database.MinConns,
		StatementTimeout: cfg.Database.StatementTimeout, LockTimeout: cfg.Database.LockTimeout,
	})
	if err != nil {
		fmt.Fprintln(stderr, serviceName+":", err)
		return exitFailure
	}
	defer database.Close()

	clk := clock.System()
	w, err := build(ctx, buildInput{cfg: cfg, plan: p, resolver: resolver, database: database, clock: clk, logger: log,
		meter: otel.GetMeterProvider().Meter(meterName)})
	if err != nil {
		fmt.Fprintln(stderr, serviceName+":", err)
		return exitFailure
	}
	defer w.close(context.WithoutCancel(ctx))

	sched, err := advancedjobs.NewScheduler(w.jobs, advancedjobs.Options{
		Clock: clk, Logger: log, Observer: w.health, RunTimeout: cfg.AdvancedWorker.RunTimeout,
	})
	if err != nil {
		fmt.Fprintln(stderr, serviceName+":", err)
		return exitFailure
	}

	if cmd == "once" {
		out, err := sched.RunNow(ctx, rest[0])
		if err != nil {
			fmt.Fprintln(stderr, serviceName+":", err)
			return exitFailure
		}
		if err := writeJSON(stdout, outcomeView(out)); err != nil {
			fmt.Fprintln(stderr, serviceName+":", err)
			return exitFailure
		}
		if out.Failed() {
			return exitFailure
		}
		return exitOK
	}

	names := make([]string, 0, len(w.jobs))
	for _, s := range sched.Specs() {
		names = append(names, s.Name)
	}
	log.InfoContext(ctx, "advanced worker started", slog.Any("jobs", names), slog.Int("unwired", len(p.unwired)))
	for name, reason := range p.unwired {
		log.WarnContext(ctx, "job not wired", slog.String("job", name), slog.String("reason", reason))
	}
	drain := cfg.AdvancedWorker.DrainTimeout
	if drain <= 0 {
		drain = advancedjobs.DefaultDrainTimeout
	}
	sched.Run(ctx, advancedjobs.DefaultTick, drain)
	log.InfoContext(context.WithoutCancel(ctx), "advanced worker stopped")
	return exitOK
}

// loadConfig loads the worker's configuration, assuming LOCAL -- loudly --
// when CP_ENV is unset, as every worker does, and refuses an override that
// names a job the worker does not run.
func loadConfig(ctx context.Context, lookup func(string) (string, bool), stderr io.Writer) (*config.Config, error) {
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
	cfg, err := config.Load(ctx, config.ServiceAdvancedWorker, lookup)
	if err != nil {
		return nil, err
	}
	if _, ok := cfg.AdvancedWorker.Intervals[advancedmetrics.Name]; ok {
		return nil, fmt.Errorf("the collector's interval is CP_ADVANCED_WORKER_METRICS_INTERVAL, not an entry of CP_ADVANCED_WORKER_INTERVALS")
	}
	if err := advancedjobs.ValidateOverrides(cfg.AdvancedWorker.Intervals, cfg.AdvancedWorker.Batches); err != nil {
		return nil, err
	}
	return cfg, nil
}

// knownJob reports whether name is a job this binary can run.
func knownJob(name string) bool {
	if name == advancedmetrics.Name {
		return true
	}
	_, ok := advancedjobs.Describe(name)
	return ok
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `usage: %[1]s <command> [args]

  run          every job on its schedule until SIGINT/SIGTERM, then drain
  once <job>   one run of one job; prints its report, exit 1 if any unit failed
  list         every job with its interval and batch, and whether it is wired

env: CP_* (see internal/config), and:
  CP_ADVANCED_WORKER_INTERVALS        <job>=<duration>,... overrides a job's interval
  CP_ADVANCED_WORKER_BATCHES          <job>=<n>,... overrides a job's batch (clamped to its subsystem's bound)
  CP_ADVANCED_WORKER_RUN_TIMEOUT      bound on one run (default %[2]s)
  CP_ADVANCED_WORKER_DRAIN_TIMEOUT    bound on the drain at shutdown (default %[3]s)
  CP_ADVANCED_WORKER_METRICS_INTERVAL the collector's interval (default %[4]s)

No setting turns a job off, and the worker consults no kill switch of its own:
settlement, liquidation, expiry and reconciliation keep running while new risk
is stopped. The chain jobs need CP_SANDBOX_DEVNET_NETWORK and
CP_SANDBOX_DEVNET_SEED_REF.

exit codes: 0 success, 1 failure, 2 usage error
`, serviceName, advancedjobs.DefaultRunTimeout, advancedjobs.DefaultDrainTimeout, advancedmetrics.DefaultInterval)
}

// --- the plan: what this configuration runs ---------------------------------------

// planned is one job as this configuration schedules it.
type planned struct {
	Name     string
	Settings advancedjobs.Settings
	Calls    string
}

// plan is decided from the configuration alone: which jobs are wired, with
// which settings, and why the others are not. `list` prints it without
// opening the database; build wires exactly the jobs it names.
type plan struct {
	jobs    []planned
	unwired map[string]string
}

func newPlan(cfg *config.Config) plan {
	p := plan{unwired: map[string]string{}}
	equitiesReason := equitiesUnavailable(cfg)
	for _, d := range advancedjobs.Catalog() {
		s := d.Resolve(advancedjobs.Settings{Interval: cfg.AdvancedWorker.Intervals[d.Name], Batch: cfg.AdvancedWorker.Batches[d.Name]})
		p.jobs = append(p.jobs, planned{Name: d.Name, Settings: s, Calls: d.Calls})
		switch {
		case d.Needs == advancedjobs.NeedsDevnet && cfg.Sandbox.DevnetNetwork == "":
			p.unwired[d.Name] = "needs " + advancedjobs.NeedsDevnet + " (the sandbox chain)"
		case isEquitiesJob(d.Name) && equitiesReason != "":
			p.unwired[d.Name] = equitiesReason
		}
	}
	interval := cfg.AdvancedWorker.MetricsInterval
	if interval <= 0 {
		interval = advancedmetrics.DefaultInterval
	}
	p.jobs = append(p.jobs, planned{Name: advancedmetrics.Name, Settings: advancedjobs.Settings{Interval: interval, Batch: 1},
		Calls: "records the advanced metrics from the database (read-only)"})
	return p
}

func isEquitiesJob(name string) bool {
	switch name {
	case advancedjobs.NameEquitiesSettlement, advancedjobs.NameEquitiesCorporate, advancedjobs.NameEquitiesVenue:
		return true
	}
	return false
}

func (p plan) wired(name string) bool {
	_, unwired := p.unwired[name]
	return !unwired
}

func (p plan) write(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "JOB\tINTERVAL\tBATCH\tSTATUS")
	for _, j := range p.jobs {
		status := "wired"
		if reason, ok := p.unwired[j.Name]; ok {
			status = "not wired: " + reason
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", j.Name, j.Settings.Interval, j.Settings.Batch, status)
	}
	return tw.Flush()
}

// --- output -------------------------------------------------------------------------

type failureView struct {
	Unit   string `json:"unit"`
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

func outcomeView(o advancedjobs.Outcome) map[string]any {
	failures := make([]failureView, 0, len(o.Report.Failures))
	for _, f := range o.Report.Failures {
		failures = append(failures, failureView{Unit: f.Unit, Code: f.Code, Reason: f.Reason})
	}
	out := map[string]any{
		"job": o.Job, "started": o.Started.UTC(), "duration_ms": o.Duration.Milliseconds(),
		"examined": o.Report.Examined, "changed": o.Report.Changed, "deferred": o.Report.Deferred,
		"failures": failures, "failed": o.Failed(),
	}
	if o.Err != nil {
		out["error"] = failureView{Unit: o.Err.Unit, Code: o.Err.Code, Reason: o.Err.Reason}
	}
	return out
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
