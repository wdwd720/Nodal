// Command reconciliation-worker runs the three PART 50 reconciliation modes
// against the control plane's database and the chain observers.
//
// Usage:
//
//	reconciliation-worker run        periodic sweeps until SIGINT/SIGTERM
//	reconciliation-worker sweep      one periodic pass over unknown submissions and pending deposits, then exit
//	reconciliation-worker verify     one internal-consistency pass (PART 21); exit 1 on any drift
//	reconciliation-worker full <id>  one FULL balance reconciliation for an account id
//
// Intervals: CP_RECONCILIATION_PERIODIC_INTERVAL (default 60s),
// CP_RECONCILIATION_FULL_INTERVAL (default 1h),
// CP_RECONCILIATION_VERIFY_INTERVAL (default 5m),
// CP_RECONCILIATION_BATCH (default 100).
//
// The worker deliberately consults no kill switch. Kill switches stop new
// risk; PART 52 requires reconciliation, observation, settlement and ledger
// posting to keep running during any emergency, and this binary is what
// keeps that promise. It also never submits a transaction: the only writes it
// makes are reconciliation records, adopted fills, their ledger postings and
// their position changes.
//
// Chain observers are wired by the composition root once provider credentials
// exist (EB-010/EB-011). Until then `run` and `sweep` reconcile what can be
// decided from persisted state and report the rest as uncertainty, and
// `verify` — which needs no observer at all — is fully functional.
//
// Exit codes: 0 success, 1 failure (verify: drift found), 2 usage error.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/reconciliation"
)

const (
	envPeriodicInterval = "CP_RECONCILIATION_PERIODIC_INTERVAL"
	envFullInterval     = "CP_RECONCILIATION_FULL_INTERVAL"
	envVerifyInterval   = "CP_RECONCILIATION_VERIFY_INTERVAL"
	envBatch            = "CP_RECONCILIATION_BATCH"

	defaultPeriodicInterval = time.Minute
	defaultFullInterval     = time.Hour
	defaultVerifyInterval   = 5 * time.Minute
	defaultBatch            = 100

	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.LookupEnv, os.Stdout, os.Stderr))
}

type deps struct {
	cfg    *config.Config
	db     *db.DB
	engine *reconciliation.Engine
	log    *slog.Logger
	lookup func(string) (string, bool)
	// credits promotes funded Credits out of the reversibility window and asks
	// the provider about purchases still in flight. Nil when this deployment
	// has no Credit purchase provider.
	credits *creditSweeper
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
	case "run", "sweep", "verify", "full":
	default:
		fmt.Fprintf(stderr, "reconciliation-worker: unknown command %q\n", cmd)
		usage(stderr)
		return exitUsage
	}
	if cmd == "full" && len(rest) != 1 {
		fmt.Fprintln(stderr, "reconciliation-worker: full expects exactly one <account-id>")
		usage(stderr)
		return exitUsage
	}
	if cmd != "full" && len(rest) != 0 {
		fmt.Fprintf(stderr, "reconciliation-worker: %s takes no arguments\n", cmd)
		usage(stderr)
		return exitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	d, err := wire(ctx, lookup, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "reconciliation-worker:", err)
		return exitFailure
	}
	defer d.db.Close()

	switch cmd {
	case "run":
		err = cmdRun(ctx, d)
	case "sweep":
		err = cmdSweep(ctx, d, stdout)
	case "verify":
		var clean bool
		clean, err = cmdVerify(ctx, d, stdout)
		if err == nil && !clean {
			return exitFailure
		}
	case "full":
		err = cmdFull(ctx, d, rest[0], stdout)
	}
	if err != nil {
		fmt.Fprintln(stderr, "reconciliation-worker:", err)
		return exitFailure
	}
	return exitOK
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `usage: reconciliation-worker <command> [args]

  run                  periodic sweeps (%s, default %s), full balance passes
                       (%s, default %s) and internal verification (%s, default %s)
  sweep                one periodic pass over unknown submissions and pending deposits
  verify               one internal-consistency pass (PART 21); exit 1 on any drift
  full <account-id>    one FULL balance reconciliation for an account

env: CP_* (see internal/config); %s (records per pass, default %d)
`, envPeriodicInterval, defaultPeriodicInterval, envFullInterval, defaultFullInterval,
		envVerifyInterval, defaultVerifyInterval, envBatch, defaultBatch)
}

func wire(ctx context.Context, lookup func(string) (string, bool), stderr io.Writer) (*deps, error) {
	if v, ok := lookup(config.EnvVarEnvironment); !ok || strings.TrimSpace(v) == "" {
		fmt.Fprintf(stderr, "reconciliation-worker: WARNING %s is not set; assuming LOCAL\n", config.EnvVarEnvironment)
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
		With("service", "reconciliation-worker", "build_version", cfg.BuildVersion, "env", cfg.Env)
	resolver := config.NewResolver(cfg.Env, lookup)
	dbURL, err := resolver.Resolve(ctx, cfg.Database.AppURL)
	if err != nil {
		return nil, fmt.Errorf("resolve database url: %w", err)
	}
	d, err := db.Open(ctx, db.Config{
		URL: dbURL, AppName: "reconciliation-worker", RequireTLS: cfg.Database.RequireTLS,
		MaxConns: cfg.Database.MaxConns, MinConns: cfg.Database.MinConns,
		StatementTimeout: cfg.Database.StatementTimeout, LockTimeout: cfg.Database.LockTimeout,
	})
	if err != nil {
		return nil, err
	}
	clk := clock.System()
	outbox := event.NewOutbox(clk)
	writer := audit.NewWriter()
	engine, err := reconciliation.NewEngine(reconciliation.Config{
		DB:      d,
		Clock:   clk,
		Records: reconciliation.NewRepository(clk, outbox, writer),
		Policy:  reconciliation.DefaultPolicy(),
		// Chain observers, execution adapters and the wallet/asset registries
		// are attached by the composition root once provider credentials exist.
		// Without them the engine still runs every check that needs only
		// persisted state, and reports the rest as uncertainty rather than
		// guessing.
		Orders:   execution.NewRepository(clk, outbox, writer),
		Attempts: execution.NewAttemptRepository(clk, outbox, writer),
		Metrics:  reconciliation.NoopMetrics(),
		Logger:   log,
	})
	if err != nil {
		d.Close()
		return nil, err
	}
	return &deps{
		cfg: cfg, db: d, engine: engine, log: log, lookup: lookup,
		credits: newCreditSweeper(ctx, cfg, d, resolver, clk, log),
	}, nil
}

func cmdRun(ctx context.Context, d *deps) error {
	periodic := durationVar(d.lookup, envPeriodicInterval, defaultPeriodicInterval)
	full := durationVar(d.lookup, envFullInterval, defaultFullInterval)
	verify := durationVar(d.lookup, envVerifyInterval, defaultVerifyInterval)
	batch := intVar(d.lookup, envBatch, defaultBatch)

	d.log.InfoContext(ctx, "reconciliation worker started",
		slog.Duration("periodic_interval", periodic), slog.Duration("full_interval", full),
		slog.Duration("verify_interval", verify), slog.Int("batch", batch))

	if d.credits != nil {
		d.credits.window = durationVar(d.lookup, envSettlementWindow, defaultSettlementWindow)
	}
	creditEvery := durationVar(d.lookup, envCreditInterval, defaultCreditInterval)

	periodicTick := time.NewTicker(periodic)
	defer periodicTick.Stop()
	creditTick := time.NewTicker(creditEvery)
	defer creditTick.Stop()
	verifyTick := time.NewTicker(verify)
	defer verifyTick.Stop()
	fullTick := time.NewTicker(full)
	defer fullTick.Stop()

	for {
		select {
		case <-ctx.Done():
			d.log.InfoContext(context.WithoutCancel(ctx), "reconciliation worker stopped")
			return nil
		case <-periodicTick.C:
			d.sweep(ctx, batch)
		case <-verifyTick.C:
			if _, err := d.engine.VerifyInternal(ctx); err != nil {
				d.log.ErrorContext(ctx, "internal verification failed", slog.String("error", err.Error()))
			}
			if _, err := d.engine.SweepEscalations(ctx, batch); err != nil {
				d.log.ErrorContext(ctx, "escalation sweep failed", slog.String("error", err.Error()))
			}
		case <-creditTick.C:
			d.credits.settle(ctx, batch)
			d.credits.reconcile(ctx, batch)
		case <-fullTick.C:
			d.log.InfoContext(ctx, "full balance reconciliation is scheduled per account by the composition root")
		}
	}
}

func (d *deps) sweep(ctx context.Context, batch int) {
	recs, err := d.engine.RunPeriodic(ctx, batch)
	if err != nil {
		d.log.ErrorContext(ctx, "execution sweep failed", slog.String("error", err.Error()))
	} else if len(recs) > 0 {
		d.log.InfoContext(ctx, "execution sweep complete", slog.Int("records", len(recs)))
	}
	deposits, err := d.engine.RunPeriodicFunding(ctx, batch)
	if err != nil {
		d.log.ErrorContext(ctx, "funding sweep failed", slog.String("error", err.Error()))
	} else if len(deposits) > 0 {
		d.log.InfoContext(ctx, "funding sweep complete", slog.Int("records", len(deposits)))
	}
}

func cmdSweep(ctx context.Context, d *deps, stdout io.Writer) error {
	batch := intVar(d.lookup, envBatch, defaultBatch)
	recs, err := d.engine.RunPeriodic(ctx, batch)
	if err != nil {
		return err
	}
	deposits, err := d.engine.RunPeriodicFunding(ctx, batch)
	if err != nil {
		return err
	}
	return writeJSON(stdout, map[string]any{
		"execution_records": summarize(recs),
		"funding_records":   summarize(deposits),
	})
}

func cmdVerify(ctx context.Context, d *deps, stdout io.Writer) (bool, error) {
	recs, err := d.engine.VerifyInternal(ctx)
	if err != nil {
		return false, err
	}
	if err := writeJSON(stdout, map[string]any{
		"drift_records": summarize(recs), "clean": len(recs) == 0,
	}); err != nil {
		return false, err
	}
	return len(recs) == 0, nil
}

func cmdFull(ctx context.Context, d *deps, accountID string, stdout io.Writer) error {
	id, err := accounts.ParseAccountID(accountID)
	if err != nil {
		return fmt.Errorf("account id: %w", err)
	}
	recs, err := d.engine.RunFull(ctx, id)
	if err != nil {
		return err
	}
	return writeJSON(stdout, map[string]any{"records": summarize(recs)})
}

func summarize(recs []reconciliation.Record) []map[string]any {
	out := make([]map[string]any, 0, len(recs))
	for _, r := range recs {
		out = append(out, map[string]any{
			"id": r.ID.String(), "kind": string(r.Kind), "mode": string(r.Mode),
			"scope_type": r.ScopeType, "scope_id": r.ScopeID, "status": string(r.Status),
			"material": r.Material, "blocks_new_risk": r.BlocksNewRisk, "opened_at": r.OpenedAt,
		})
	}
	return out
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func durationVar(lookup func(string) (string, bool), name string, def time.Duration) time.Duration {
	v, ok := lookup(name)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil || d <= 0 {
		return def
	}
	return d
}

func intVar(lookup func(string) (string, bool), name string, def int) int {
	v, ok := lookup(name)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return def
	}
	return n
}
