// Command agent-worker is the composition root of the agent runtime: it
// dispatches triggers into runs, executes runs through the ToolBroker, and
// resolves and calibrates the prediction ledger.
//
// Usage:
//
//	agent-worker run                            dispatch + execute + resolve until SIGINT/SIGTERM
//	agent-worker tick                           one interval dispatch pass, then exit
//	agent-worker execute                        execute every open run once, then exit
//	agent-worker resolve                        resolve every prediction whose horizon has elapsed
//	agent-worker calibrate <version-id> <mode>  compute and persist calibration for a strategy version
//	agent-worker gate                           report the LIVE_AGENT_TRADING capability gate
//
// Intervals: CP_AGENT_TICK_INTERVAL (default 60s), CP_AGENT_RESOLVE_INTERVAL
// (default 5m), CP_AGENT_BATCH (default 100).
//
// Live money is never enabled by an environment variable. LIVE mode requires
// the LIVE_AGENT_TRADING capability gate to be ACTIVE in this environment, and
// a gate only becomes ACTIVE through the dual-controlled workflow in
// internal/gates under an operator principal. This binary reads the gate and
// refuses to execute LIVE runs when it is anything other than ACTIVE; it has
// no code path that can change it.
//
// The worker holds no key and signs nothing. Its only money-adjacent write is
// proposing a trade intent, which the rest of the platform then validates,
// risk-checks, reserves capital for and executes independently.
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/agent"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/prediction"
	"github.com/nodal/controlplane/internal/strategy/ir"
)

const (
	envTickInterval    = "CP_AGENT_TICK_INTERVAL"
	envResolveInterval = "CP_AGENT_RESOLVE_INTERVAL"
	envBatch           = "CP_AGENT_BATCH"

	defaultTickInterval    = time.Minute
	defaultResolveInterval = 5 * time.Minute
	defaultBatch           = 100

	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.LookupEnv, os.Stdout, os.Stderr))
}

type deps struct {
	cfg        *config.Config
	db         *db.DB
	log        *slog.Logger
	clk        clock.Clock
	store      agent.Store
	pauses     agent.PauseChecker
	dispatcher agent.Dispatcher
	runner     *agent.Runner
	ledger     *prediction.PGLedger
	resolver   *prediction.Resolver
	calibrator *prediction.PGCalibrator
	batch      int
	tick       time.Duration
	resolve    time.Duration
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
	case "run", "tick", "execute", "resolve", "gate":
		if len(rest) != 0 {
			fmt.Fprintf(stderr, "agent-worker: %s takes no arguments\n", cmd)
			usage(stderr)
			return exitUsage
		}
	case "calibrate":
		if len(rest) != 2 {
			fmt.Fprintln(stderr, "agent-worker: calibrate expects <strategy-version-id> <mode>")
			usage(stderr)
			return exitUsage
		}
	default:
		fmt.Fprintf(stderr, "agent-worker: unknown command %q\n", cmd)
		usage(stderr)
		return exitUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	d, err := wire(ctx, lookup, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "agent-worker:", err)
		return exitFailure
	}
	defer d.db.Close()

	switch cmd {
	case "run":
		err = cmdRun(ctx, d)
	case "tick":
		err = cmdTick(ctx, d, stdout)
	case "execute":
		err = cmdExecute(ctx, d, stdout)
	case "resolve":
		err = cmdResolve(ctx, d, stdout)
	case "calibrate":
		err = cmdCalibrate(ctx, d, rest[0], rest[1], stdout)
	case "gate":
		err = cmdGate(ctx, d, stdout)
	}
	if err != nil {
		fmt.Fprintln(stderr, "agent-worker:", err)
		return exitFailure
	}
	return exitOK
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `usage: agent-worker <command> [args]

  run                            dispatch (%s, default %s), execute open runs and
                                 resolve predictions (%s, default %s) until interrupted
  tick                           one interval dispatch pass
  execute                        execute every open run once
  resolve                        resolve every prediction whose horizon has elapsed
  calibrate <version-id> <mode>  compute and persist calibration for a strategy version
  gate                           report the LIVE_AGENT_TRADING capability gate

env: CP_* (see internal/config); %s (records per pass, default %d)

LIVE mode requires the LIVE_AGENT_TRADING capability gate to be ACTIVE. No
environment variable can enable it.
`, envTickInterval, defaultTickInterval, envResolveInterval, defaultResolveInterval, envBatch, defaultBatch)
}

func wire(ctx context.Context, lookup func(string) (string, bool), stderr io.Writer) (*deps, error) {
	if v, ok := lookup(config.EnvVarEnvironment); !ok || strings.TrimSpace(v) == "" {
		fmt.Fprintf(stderr, "agent-worker: WARNING %s is not set; assuming LOCAL\n", config.EnvVarEnvironment)
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
		With("service", "agent-worker", "build_version", cfg.BuildVersion, "env", cfg.Env)
	resolverCfg := config.NewResolver(cfg.Env, lookup)
	dbURL, err := resolverCfg.Resolve(ctx, cfg.Database.AppURL)
	if err != nil {
		return nil, fmt.Errorf("resolve database url: %w", err)
	}
	database, err := db.Open(ctx, db.Config{
		URL: dbURL, AppName: "agent-worker", RequireTLS: cfg.Database.RequireTLS,
		MaxConns: cfg.Database.MaxConns, MinConns: cfg.Database.MinConns,
		StatementTimeout: cfg.Database.StatementTimeout, LockTimeout: cfg.Database.LockTimeout,
	})
	if err != nil {
		return nil, err
	}
	clk := clock.System()
	store := agent.NewStore()
	pauses := agent.NewPauseChecker()

	dispatcher, err := agent.NewDispatcher(agent.DispatcherDeps{
		DB: database, Clock: clk, Store: store, Pauses: pauses,
		IRFor: loadIR, MaxAgents: intFromEnv(lookup, envBatch, defaultBatch),
		BuildVersion: cfg.BuildVersion,
	})
	if err != nil {
		database.Close()
		return nil, err
	}
	ledger, err := prediction.NewLedger(clk)
	if err != nil {
		database.Close()
		return nil, err
	}
	// A price older than this against its own cut-off does not measure the
	// window it is being used to score, so the prediction stays unresolved and
	// is retried rather than being written off as FLAT (F-59). Fifteen minutes
	// is the coarsest cadence any instrument this worker scores is fed at; a
	// feed quieter than that cannot support a prediction anyway.
	const maxPredictionPriceAge = 15 * time.Minute
	outcomeResolver, err := prediction.NewResolver(clk, prediction.NewPriceReader(), maxPredictionPriceAge)
	if err != nil {
		database.Close()
		return nil, err
	}
	calibrator, err := prediction.NewCalibrator(clk)
	if err != nil {
		database.Close()
		return nil, err
	}

	// The evaluator, the tool adapters and the model provider are attached by
	// the composition root once internal/strategy exposes its deterministic
	// evaluator and provider credentials exist. Until then the runner is
	// constructed with an evaluator that refuses rather than one that guesses:
	// a run that cannot be evaluated is SKIPPED with a recorded reason, never
	// completed with an invented decision.
	runner, err := agent.NewRunner(agent.RunnerDeps{
		DB: database, Clock: clk, Store: store, Pauses: pauses,
		Envelope: agent.NewEnvelopeReader(), Evaluator: unavailableEvaluator{},
		Predictions: ledger,
		BrokerFor: func(a agent.Authority, runID agent.RunID) (agent.ToolBroker, error) {
			return agent.NewBroker(agent.BrokerDeps{
				DB: database, Clock: clk, Registry: agent.NewToolRegistry(),
				Budgets: agent.NewBudgetReader(), Pauses: pauses,
				Recorder: agent.NewInvocationRecorder(), ModelCalls: agent.NewModelCallRecorder(),
				Environment: string(cfg.Env),
				// Adapters are wired per tool once provider credentials exist.
				// An unwired tool is refused with PROVIDER_UNAVAILABLE and the
				// refusal is recorded; it is never treated as an empty answer.
				Adapters: map[string]agent.ToolAdapter{},
			}, a, runID)
		},
		EmitterFor: func(a agent.Authority) (agent.IntentEmitter, error) {
			return nil, errs.New(errs.CodeUnsupported,
				"agent-worker: the intent emitter is wired by the API composition root; this worker proposes no intents yet")
		},
		IRFor: loadIR, BuildVersion: cfg.BuildVersion,
	})
	if err != nil {
		database.Close()
		return nil, err
	}

	return &deps{
		cfg: cfg, db: database, log: log, clk: clk, store: store, pauses: pauses,
		dispatcher: dispatcher, runner: runner, ledger: ledger,
		resolver: outcomeResolver, calibrator: calibrator,
		batch:   intFromEnv(lookup, envBatch, defaultBatch),
		tick:    durationFromEnv(lookup, envTickInterval, defaultTickInterval),
		resolve: durationFromEnv(lookup, envResolveInterval, defaultResolveInterval),
	}, nil
}

// unavailableEvaluator refuses every evaluation with MODEL_UNAVAILABLE so the
// run is SKIPPED with a recorded reason. It exists so the binary is honest
// about what is not yet wired instead of silently doing nothing.
type unavailableEvaluator struct{}

func (unavailableEvaluator) Evaluate(context.Context, agent.EvalInput) (agent.EvalOutput, error) {
	return agent.EvalOutput{}, errs.New(errs.CodeModelUnavailable,
		"agent-worker: no deterministic evaluator is wired in this build")
}

// loadIR reads the compiled IR of a strategy version. It parses through
// ir.ParseIR so a document that does not satisfy the schema is rejected here
// rather than half-interpreted by the runtime.
func loadIR(ctx context.Context, q db.Querier, strategyVersionID string) (*ir.IR, error) {
	var raw []byte
	err := q.QueryRow(ctx, `SELECT ir FROM strategy_versions WHERE id = $1`, strategyVersionID).Scan(&raw)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeNotFound, "agent-worker: load strategy version ir")
	}
	doc, err := ir.ParseIR(raw)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeValidationFailed, "agent-worker: parse strategy version ir")
	}
	return doc, nil
}

func cmdRun(ctx context.Context, d *deps) error {
	d.log.Info("agent worker started",
		"tick_interval", d.tick.String(), "resolve_interval", d.resolve.String(), "batch", d.batch)
	tick := time.NewTicker(d.tick)
	defer tick.Stop()
	resolveTick := time.NewTicker(d.resolve)
	defer resolveTick.Stop()
	for {
		select {
		case <-ctx.Done():
			d.log.Info("agent worker stopping")
			return nil
		case <-tick.C:
			if err := cmdTick(ctx, d, io.Discard); err != nil {
				d.log.Error("dispatch pass failed", "error", err)
			}
			if err := cmdExecute(ctx, d, io.Discard); err != nil {
				d.log.Error("execution pass failed", "error", err)
			}
		case <-resolveTick.C:
			if err := cmdResolve(ctx, d, io.Discard); err != nil {
				d.log.Error("resolution pass failed", "error", err)
			}
		}
	}
}

func cmdTick(ctx context.Context, d *deps, out io.Writer) error {
	runs, err := d.dispatcher.OnTick(ctx, d.clk.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "dispatched %d run(s)\n", len(runs))
	for _, r := range runs {
		d.log.Info("run opened", "run_id", r.ID.String(), "agent_id", r.AgentID.String(),
			"mode", r.Mode.String(), "trigger", r.TriggerName)
	}
	return nil
}

func cmdExecute(ctx context.Context, d *deps, out io.Writer) error {
	live, state, err := d.store.LiveTradingEnabled(ctx, d.db, string(d.cfg.Env))
	if err != nil {
		return err
	}
	open, err := d.store.ListOpenRuns(ctx, d.db, d.batch)
	if err != nil {
		return err
	}
	executed, refused := 0, 0
	for _, r := range open {
		if r.Mode == agent.ModeLive && !live {
			// Live money is gated, and no environment variable opens the gate.
			d.log.Warn("refusing a LIVE run: capability gate is not ACTIVE",
				"run_id", r.ID.String(), "gate", agent.LiveTradingCapability, "state", state)
			refused++
			continue
		}
		if _, err := d.runner.Run(ctx, r.ID); err != nil {
			d.log.Warn("run ended with an error", "run_id", r.ID.String(), "error", err)
		}
		executed++
	}
	fmt.Fprintf(out, "executed %d run(s), refused %d LIVE run(s) (gate %s)\n", executed, refused, state)
	return nil
}

func cmdResolve(ctx context.Context, d *deps, out io.Writer) error {
	now := d.clk.Now()
	due, err := d.ledger.DueForResolution(ctx, d.db, now, d.batch)
	if err != nil {
		return err
	}
	resolved, skipped := 0, 0
	for _, p := range due {
		outcome, rerr := d.resolver.Resolve(ctx, d.db, p)
		if rerr != nil {
			// A missing price is not an excuse to invent one: the prediction
			// stays unresolved and is retried on the next pass.
			d.log.Warn("prediction not resolvable", "prediction_id", p.ID.String(), "error", rerr)
			skipped++
			continue
		}
		if err := d.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 2},
			func(ctx context.Context, tx pgx.Tx) error {
				return d.ledger.RecordOutcome(ctx, tx, outcome)
			}); err != nil {
			d.log.Warn("outcome not recorded", "prediction_id", p.ID.String(), "error", err)
			skipped++
			continue
		}
		resolved++
	}
	fmt.Fprintf(out, "resolved %d prediction(s), %d left unresolved\n", resolved, skipped)
	return nil
}

func cmdCalibrate(ctx context.Context, d *deps, versionID, mode string, out io.Writer) error {
	m := prediction.Mode(strings.ToUpper(mode))
	if !m.Valid() {
		return fmt.Errorf("unknown mode %q", mode)
	}
	now := d.clk.Now()
	scope := prediction.CalibrationScope{
		StrategyVersionID: versionID,
		Mode:              m,
		WindowStart:       now.AddDate(0, 0, -90),
		WindowEnd:         now,
	}
	rows, err := d.calibrator.Compute(ctx, d.db, scope, now)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Fprintln(out, "no resolved predictions in the window; nothing to calibrate")
		return nil
	}
	if err := d.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 2},
		func(ctx context.Context, tx pgx.Tx) error {
			return d.calibrator.Persist(ctx, tx, rows)
		}); err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	for _, r := range rows {
		if err := enc.Encode(struct {
			Bucket            string `json:"bucket"`
			N                 int    `json:"n"`
			MeanPredicted     string `json:"mean_predicted"`
			RealizedFrequency string `json:"realized_frequency"`
			BrierMean         string `json:"brier_mean"`
			LogLossMean       string `json:"log_loss_mean"`
			ExpectedReturnBPS int64  `json:"expected_return_bps_mean"`
			RealizedReturnBPS int64  `json:"realized_return_bps_mean"`
		}{
			Bucket:            r.BucketLower.String() + ".." + r.BucketUpper.String(),
			N:                 r.NPredictions,
			MeanPredicted:     r.MeanPredicted.String(),
			RealizedFrequency: r.RealizedFrequency.String(),
			BrierMean:         r.BrierMean.String(),
			LogLossMean:       r.LogLossMean.String(),
			ExpectedReturnBPS: int64(r.ExpectedReturnBPSMean),
			RealizedReturnBPS: int64(r.RealizedReturnBPSMean),
		}); err != nil {
			return err
		}
	}
	return nil
}

func cmdGate(ctx context.Context, d *deps, out io.Writer) error {
	live, state, err := d.store.LiveTradingEnabled(ctx, d.db, string(d.cfg.Env))
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s in %s: state=%s live_agent_trading_enabled=%t\n",
		agent.LiveTradingCapability, d.cfg.Env, state, live)
	return nil
}

// auditAdapter bridges agent.AuditEvent onto the real audit writer. It is
// declared here, in the composition root, so internal/agent needs no import of
// internal/audit to be testable.
type auditAdapter struct{ w audit.Writer }

func (a auditAdapter) Append(ctx context.Context, tx pgx.Tx, e agent.AuditEvent) error {
	_, err := a.w.Append(ctx, tx, audit.Event{
		Stream: e.Stream, ActorType: e.ActorType, ActorID: e.ActorID, Action: e.Action,
		ResourceType: e.ResourceType, ResourceID: e.ResourceID, Reason: e.Reason,
		EvidenceRef: e.EvidenceRef, PolicyVersion: e.PolicyVersion, CorrelationID: e.CorrelationID,
		Payload: e.Payload, OccurredAt: e.OccurredAt,
	})
	return err
}

// NewAuditAppender exposes the adapter to the API composition root, which
// drives the lifecycle (create, promote, pause, resume) over HTTP.
func NewAuditAppender(w audit.Writer) agent.AuditAppender { return auditAdapter{w: w} }

// approvalAdapter reads admin_actions for promotion approvals. It is a SELECT
// only: internal/agent must not import internal/admin, and nothing here can
// create or approve an admin action.
type approvalAdapter struct{}

const selectApprovalSQL = `
SELECT id::text, kind, target_id, proposed_by_user_id::text,
       coalesce(approved_by_user_id::text, ''), expires_at
  FROM admin_actions
 WHERE id = $1 AND status IN ('APPROVED','EXECUTED')`

func (approvalAdapter) VerifyApproved(ctx context.Context, q db.Querier, approvalID, kind, targetID string) (agent.Approval, error) {
	var a agent.Approval
	err := q.QueryRow(ctx, selectApprovalSQL, approvalID).
		Scan(&a.ID, &a.Kind, &a.TargetID, &a.ProposedBy, &a.ApprovedBy, &a.ExpiresAt)
	if err != nil {
		return agent.Approval{}, errs.Wrap(err, errs.CodeForbidden,
			"agent-worker: no approved admin action with that id")
	}
	if a.Kind != kind || a.TargetID != targetID {
		return agent.Approval{}, errs.New(errs.CodeForbidden,
			"agent-worker: the approval does not match the requested promotion").
			WithField("approval_id", approvalID)
	}
	return a, nil
}

// NewApprovalVerifier exposes the adapter to the API composition root.
func NewApprovalVerifier() agent.ApprovalVerifier { return approvalAdapter{} }

func intFromEnv(lookup func(string) (string, bool), key string, def int) int {
	v, ok := lookup(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func durationFromEnv(lookup func(string) (string, bool), key string, def time.Duration) time.Duration {
	v, ok := lookup(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil || d <= 0 {
		return def
	}
	return d
}
