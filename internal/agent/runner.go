package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/intent"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/prediction"
	"github.com/nodal/controlplane/internal/strategy/ir"
)

// EvalInput is what the deterministic evaluator sees. It contains typed
// observations and platform state, never raw provider bytes, never a
// credential, and never free text presented as an instruction.
type EvalInput struct {
	RunID        RunID
	Authority    Authority
	Now          time.Time
	IR           *ir.IR
	Observations map[string]Observation
	Envelope     EnvelopeSnapshot
}

// Decision is one action the evaluator resolved to. Kinds mirror ir.Action.
type Decision struct {
	ActionName string
	Kind       ir.ActionKind
	// Model is set for CALL_MODEL; the broker performs it.
	Model *ModelCall
	// Prediction is set for COMMIT_PREDICTION.
	Prediction *prediction.Prediction
	// Intent is set for CREATE_TRADE_INTENT.
	Intent *EmitRequest
}

// EvalOutput is the evaluator's verdict.
type EvalOutput struct {
	EvaluatorVersion string
	InputHash        []byte
	TraceHash        []byte
	Signals          json.RawMessage
	ConditionResults json.RawMessage
	Rationale        json.RawMessage
	Decisions        []Decision
	// SkipReason set means the evaluator concluded nothing should happen.
	SkipReason SkipReason
}

// Evaluator is the deterministic strategy evaluator. internal/strategy owns
// the implementation; the runtime depends on this interface so the agent
// runtime never reaches into the compiler and can be tested without it.
//
// It is pure: it performs no I/O, consults no clock and reaches no network.
// Everything it may know is in EvalInput.
type Evaluator interface {
	Evaluate(ctx context.Context, in EvalInput) (EvalOutput, error)
}

// PredictionCommitter is the narrow slice of the prediction ledger the runner
// uses. It can append a prediction and nothing else.
type PredictionCommitter interface {
	Commit(ctx context.Context, tx pgx.Tx, p prediction.Prediction) (prediction.Prediction, error)
}

// RunResult is what one bounded evaluation produced.
type RunResult struct {
	Run          Run
	Observations map[string]Observation
	PredictionID string
	IntentID     string
	SkipReason   SkipReason
}

// RunnerDeps are the runner's collaborators.
type RunnerDeps struct {
	DB          *db.DB
	Clock       clock.Clock
	Store       Store
	Pauses      PauseChecker
	Envelope    EnvelopeReader
	Evaluator   Evaluator
	Predictions PredictionCommitter
	// BrokerFor builds the broker for one run. It is a function so a test can
	// supply a fake without reconstructing the whole dependency graph.
	BrokerFor func(a Authority, runID RunID) (ToolBroker, error)
	// EmitterFor builds the intent emitter for one run.
	EmitterFor func(a Authority) (IntentEmitter, error)
	// IRFor loads the compiled IR of a strategy version.
	IRFor func(ctx context.Context, q db.Querier, strategyVersionID string) (*ir.IR, error)
	// ModelCaller performs a model call through the broker. Nil means the
	// runtime has no model wired, and a required model call then skips the
	// run with MODEL_UNAVAILABLE rather than proceeding without one.
	ModelCaller  func(ctx context.Context, call ModelCall) (ModelResult, error)
	BuildVersion string
}

// Runner performs one bounded evaluation. It is resumable and idempotent per
// run: the prediction is unique per (run, action), the intent's idempotency
// key is derived from the run and action, and the run row's identity, mode
// and linkage are immutable in the database. Re-running after a crash at any
// step therefore converges on exactly one run, one prediction and one intent.
type Runner struct {
	deps RunnerDeps
}

// NewRunner builds the runner.
func NewRunner(deps RunnerDeps) (*Runner, error) {
	switch {
	case deps.DB == nil:
		return nil, errs.New(errs.CodeValidationFailed, "agent: runner requires a database")
	case deps.Clock == nil:
		return nil, errs.New(errs.CodeValidationFailed, "agent: runner requires a clock")
	case deps.Pauses == nil:
		return nil, errs.New(errs.CodeValidationFailed, "agent: runner requires a pause checker")
	case deps.Evaluator == nil:
		return nil, errs.New(errs.CodeValidationFailed, "agent: runner requires an evaluator")
	case deps.Predictions == nil:
		return nil, errs.New(errs.CodeValidationFailed, "agent: runner requires a prediction ledger")
	case deps.BrokerFor == nil:
		return nil, errs.New(errs.CodeValidationFailed, "agent: runner requires a broker factory")
	case deps.EmitterFor == nil:
		return nil, errs.New(errs.CodeValidationFailed, "agent: runner requires an emitter factory")
	case deps.IRFor == nil:
		return nil, errs.New(errs.CodeValidationFailed, "agent: runner requires an IR loader")
	}
	return &Runner{deps: deps}, nil
}

// Run executes the run identified by runID. A refusal at any step ends the
// run cleanly with a recorded skip reason; it never leaves a run open and
// never fabricates a result.
func (r *Runner) Run(ctx context.Context, runID RunID) (RunResult, error) {
	run, a, auth, err := r.prepare(ctx, runID)
	if err != nil {
		return RunResult{}, err
	}
	if run.Status.Terminal() {
		return RunResult{Run: run, PredictionID: run.PredictionID, IntentID: run.IntentID, SkipReason: run.SkipReason}, nil
	}

	// A pause is effective immediately, including for a run already in
	// flight: this is re-read at the start and again before every broker
	// call, before the prediction and before the intent.
	if paused, err := r.paused(ctx, auth.AgentID()); err != nil {
		return RunResult{}, err
	} else if paused {
		return r.skip(ctx, run, SkipAgentPaused, "agent is paused")
	}

	// A lifecycle state that cannot open a run cannot finish one either.
	// PGDispatcher.dispatchAgent asks Runnable() before opening anything, and
	// this asks the same predicate for a run already in flight: revoking an
	// agent used to stop new work while every run already open ran through to
	// its intent, which is the opposite of what an operator revoking an agent
	// mid-incident is asking for (F-73). Pause is checked above and keeps its
	// own reason; what reaches here is FAILED, REVOKED and SUPERSEDED.
	if !a.Runnable() {
		return r.skip(ctx, run, SkipAgentNotRunnable, "agent is not runnable: state is "+a.State.String())
	}

	// Run under the agent's own principal: AGENT actor type, no roles, one
	// bound account. Nothing downstream can widen it.
	ctx = WithAgentPrincipal(ctx, auth)

	doc, err := r.deps.IRFor(ctx, r.deps.DB, auth.StrategyVersionID())
	if err != nil {
		return r.fail(ctx, run, err)
	}

	obs, checks, decisionAt, skip, err := r.gather(ctx, run, auth, doc)
	if err != nil {
		return r.fail(ctx, run, err)
	}
	run.Evidence = mustJSON(checks)
	run.ToolCalls = len(checks)
	run.DataCostUSD = totalCost(obs)
	if skip != "" {
		return r.skip(ctx, run, skip, "dependency gathering refused the run")
	}
	if err := r.advance(ctx, run, RunGathering); err != nil {
		return RunResult{}, err
	}

	out, err := r.deps.Evaluator.Evaluate(ctx, EvalInput{
		RunID: run.ID, Authority: auth, Now: decisionAt, IR: doc,
		Observations: obs, Envelope: r.envelopeOrZero(ctx, auth),
	})
	if err != nil {
		if reason := SkipReasonFor(err); reason != "" {
			run.Error = err.Error()
			return r.skip(ctx, run, reason, err.Error())
		}
		return r.fail(ctx, run, err)
	}
	run.EvaluatorVersion = out.EvaluatorVersion
	run.EvalInputHash = out.InputHash
	run.EvalOutputHash = out.TraceHash
	run.Signals = out.Signals
	run.ConditionResults = out.ConditionResults
	run.Rationale = out.Rationale
	if err := r.advance(ctx, run, RunEvaluated); err != nil {
		return RunResult{}, err
	}
	if out.SkipReason != "" {
		return r.skip(ctx, run, out.SkipReason, "evaluator concluded no action")
	}

	result := RunResult{Run: run, Observations: obs}
	for _, d := range out.Decisions {
		switch d.Kind {
		case ir.ActionCallModel:
			if err := r.runModel(ctx, &run, d); err != nil {
				if reason := SkipReasonFor(err); reason != "" {
					return r.skip(ctx, run, reason, err.Error())
				}
				return r.fail(ctx, run, err)
			}
		case ir.ActionCommitPrediction:
			id, err := r.commitPrediction(ctx, &run, auth, d, obs, decisionAt)
			if err != nil {
				if reason := SkipReasonFor(err); reason != "" {
					return r.skip(ctx, run, reason, err.Error())
				}
				return r.fail(ctx, run, err)
			}
			result.PredictionID = id
		case ir.ActionCreateTradeIntent:
			id, err := r.emitIntent(ctx, &run, auth, d)
			if err != nil {
				if reason := SkipReasonFor(err); reason != "" {
					return r.skip(ctx, run, reason, err.Error())
				}
				return r.fail(ctx, run, err)
			}
			result.IntentID = id
		default:
			return r.fail(ctx, run, errs.Newf(errs.CodeUnsupported, "agent: unknown action kind %s", d.Kind))
		}
	}
	if run.IntentID == "" {
		// The evaluator produced no intent: a prediction without a trade is a
		// legitimate, recorded outcome (SHADOW mode does nothing else).
		if run.PredictionID != "" {
			if err := r.finish(ctx, &run, RunPredicted, ""); err != nil {
				return RunResult{}, err
			}
			result.Run = run
			return result, nil
		}
		return r.skip(ctx, run, SkipConditionFalse, "no action was taken")
	}
	if err := r.finish(ctx, &run, RunIntentCreated, ""); err != nil {
		return RunResult{}, err
	}
	result.Run = run
	return result, nil
}

// prepare loads the run, its agent and the frozen authority.
func (r *Runner) prepare(ctx context.Context, runID RunID) (Run, Agent, Authority, error) {
	run, err := r.deps.Store.GetRun(ctx, r.deps.DB, runID)
	if err != nil {
		return Run{}, Agent{}, Authority{}, err
	}
	a, err := r.deps.Store.Get(ctx, r.deps.DB, run.AgentID)
	if err != nil {
		return Run{}, Agent{}, Authority{}, err
	}
	doc, err := r.deps.IRFor(ctx, r.deps.DB, run.StrategyVersionID)
	if err != nil {
		return Run{}, Agent{}, Authority{}, err
	}
	env := EnvelopeSnapshot{}
	if run.EnvelopeID != "" && r.deps.Envelope != nil {
		env, err = r.deps.Envelope.Envelope(ctx, r.deps.DB, run.EnvelopeID)
		if err != nil {
			return Run{}, Agent{}, Authority{}, err
		}
	}
	auth, err := NewAuthority(AuthorityInput{
		AgentID: run.AgentID, AgentVersion: run.AgentVersion, AccountID: run.AccountID,
		StrategyVersionID: run.StrategyVersionID, Stage: a.Stage, Mode: run.Mode,
		EnvelopeID: run.EnvelopeID, RiskPolicyVersion: a.RiskPolicyVersion,
		IR: doc, Envelope: env,
	})
	if err != nil {
		return Run{}, Agent{}, Authority{}, err
	}
	return run, a, auth, nil
}

// decisionInstant is the T of the point-in-time rule "a decision at time T may
// use an observation iff decision_available_at <= T" (POINT_IN_TIME.md).
//
// In a replay mode (BACKTEST, PAPER) it is the run's recorded decision_time.
// Simulated time is the whole point: a replay that used the wall clock, or
// that advanced T to cover whatever it happened to read, would leak the future
// into the past, and the leakage test exists to catch exactly that.
//
// In a live mode it is the instant the evaluation actually happens: at or
// after the trigger, and at or after every observation the run consumed became
// usable. A live decision cannot precede the data it is made from, so pinning
// T to the trigger instant would make a run refuse its own fresh data. What
// still binds in live mode is the freshness rule — an observation older than
// the dependency's max_age_ms is stale and ends the run — and that is the
// guard live mode actually needs.
//
// decision_time stays as recorded: it is when the trigger fired, and the
// database forbids changing it.
func (r *Runner) decisionInstant(run Run, obs map[string]Observation) time.Time {
	if run.Mode == ModeBacktest || run.Mode == ModePaper {
		return run.DecisionTime
	}
	at := run.DecisionTime
	if now := r.deps.Clock.Now(); now.After(at) {
		at = now
	}
	for _, o := range obs {
		if o.DecisionAvailableAt.After(at) {
			at = o.DecisionAvailableAt
		}
	}
	return at
}

func (r *Runner) envelopeOrZero(ctx context.Context, auth Authority) EnvelopeSnapshot {
	if auth.EnvelopeID() == "" || r.deps.Envelope == nil {
		return EnvelopeSnapshot{}
	}
	env, err := r.deps.Envelope.Envelope(ctx, r.deps.DB, auth.EnvelopeID())
	if err != nil {
		return EnvelopeSnapshot{}
	}
	return env
}

func (r *Runner) paused(ctx context.Context, agentID AgentID) (bool, error) {
	_, paused, err := r.deps.Pauses.OpenPause(ctx, r.deps.DB, agentID)
	return paused, err
}

// gather invokes every declared dependency through the broker, then judges
// what came back against the run's decision instant. A refused call is
// recorded, never skipped silently; a required dependency that is missing,
// stale, or (in a replay mode) not yet knowable ends the run.
//
// Fetching and judging are separate passes because the decision instant of a
// live run depends on what was fetched: see decisionInstant.
func (r *Runner) gather(ctx context.Context, run Run, auth Authority, doc *ir.IR) (map[string]Observation, []FreshnessCheck, time.Time, SkipReason, error) {
	broker, err := r.deps.BrokerFor(auth, run.ID)
	if err != nil {
		return nil, nil, time.Time{}, "", err
	}
	type fetched struct {
		dep ir.Dependency
		obs Observation
		err error
	}
	deadline := run.DecisionTime.Add(time.Minute)
	if now := r.deps.Clock.Now(); now.After(run.DecisionTime) {
		deadline = now.Add(time.Minute)
	}

	results := make([]fetched, 0, len(doc.Dependencies))
	obs := make(map[string]Observation, len(doc.Dependencies))
	for _, dep := range doc.Dependencies {
		if ir.EffectOfDependency(dep.Kind) == "" || dep.Kind == ir.DepModel {
			continue // computed features and model calls are not fetched here
		}
		// A pause is effective immediately, including between two calls of a
		// run that is already in flight.
		if paused, perr := r.paused(ctx, auth.AgentID()); perr != nil {
			return nil, nil, time.Time{}, "", perr
		} else if paused {
			return nil, nil, time.Time{}, SkipAgentPaused, nil
		}
		o, ierr := broker.Invoke(ctx, ToolCall{
			RunID: run.ID, Dependency: dep, Params: dep.Params,
			Effect: ir.EffectOfDependency(dep.Kind), Deadline: deadline,
		})
		results = append(results, fetched{dep: dep, obs: o, err: ierr})
		if ierr == nil {
			obs[dep.Name.String()] = o
		}
		if ierr != nil && dep.Required {
			break // a required dependency that was refused ends the gathering
		}
	}

	decisionAt := r.decisionInstant(run, obs)
	checks := make([]FreshnessCheck, 0, len(results))
	var skip SkipReason
	for _, res := range results {
		dep := res.dep
		check := FreshnessCheck{
			Dependency: dep.Name.String(), ToolCode: dep.ToolCode, ToolVersion: dep.ToolVersion,
			MaxAgeMS: dep.MaxAgeMS, Required: dep.Required,
		}
		if res.err != nil {
			check.Refusal = string(errs.CodeOf(res.err))
			checks = append(checks, check)
			if dep.Required && skip == "" {
				reason := SkipReasonFor(res.err)
				if reason == "" {
					reason = SkipMissingDependency
				}
				skip = reason
			}
			continue
		}
		o := res.obs
		check.Present = true
		check.InvocationID = o.InvocationID.String()
		check.DecisionAvailableAt = o.DecisionAvailableAt
		check.AgeMS = o.Age(decisionAt).Milliseconds()
		check.Fresh = o.Fresh(decisionAt, time.Duration(dep.MaxAgeMS)*time.Millisecond)
		check.PointInTimeValid = o.UsableAt(decisionAt)
		check.OutputHash = encodeHex(o.OutputHash)
		check.Untrusted = len(o.Untrusted)
		for _, sig := range o.Signals() {
			check.InjectionSignals = append(check.InjectionSignals, string(sig))
		}
		checks = append(checks, check)
		if dep.Required && skip == "" && (!check.PointInTimeValid || !check.Fresh) {
			skip = SkipStaleData
			delete(obs, dep.Name.String())
		}
	}
	return obs, checks, decisionAt, skip, nil
}

// runModel performs one model call through the broker and records its cost.
func (r *Runner) runModel(ctx context.Context, run *Run, d Decision) error {
	if d.Model == nil {
		return errs.New(errs.CodeValidationFailed, "agent: CALL_MODEL decision carries no call")
	}
	if r.deps.ModelCaller == nil {
		if d.Model.Spec.Required {
			return errs.New(errs.CodeModelUnavailable, "agent: no model provider is wired and the strategy requires one")
		}
		return nil
	}
	res, err := r.deps.ModelCaller(ctx, *d.Model)
	if err != nil {
		if d.Model.Spec.Required {
			return err
		}
		// PART 177: an optional model that failed leaves its output absent.
		// It is never replaced by a heuristic or a synthetic answer.
		return nil
	}
	run.ModelCalls++
	total, aerr := run.ModelCostUSD.Add(res.CostUSD)
	if aerr != nil {
		return errs.Wrap(aerr, errs.CodeOverflow, "agent: model cost overflow")
	}
	run.ModelCostUSD = total
	return nil
}

// commitPrediction appends the prediction in its own transaction. The unique
// index on (agent_run_id, action_name) makes a retry return the original, so
// a crash between the commit and the run update cannot produce two forecasts.
func (r *Runner) commitPrediction(ctx context.Context, run *Run, auth Authority, d Decision, obs map[string]Observation, decisionAt time.Time) (string, error) {
	if d.Prediction == nil {
		return "", errs.New(errs.CodeValidationFailed, "agent: COMMIT_PREDICTION decision carries no prediction")
	}
	if run.PredictionID != "" {
		return run.PredictionID, nil // already committed by an earlier attempt
	}
	p := *d.Prediction
	p.AgentID = auth.AgentID().String()
	p.AgentVersion = auth.AgentVersion()
	p.RunID = run.ID.String()
	p.ActionName = d.ActionName
	p.StrategyVersionID = auth.StrategyVersionID()
	p.AccountID = auth.AccountID()
	p.Mode = prediction.Mode(auth.Mode())
	items := informationSet(obs)
	p.InformationSetHash = prediction.InformationSetHash(items)
	p.DecisionAvailableAt = prediction.LatestAvailable(items)
	if p.DecisionAvailableAt.IsZero() {
		p.DecisionAvailableAt = decisionAt
	}
	p.CorrelationID = run.CorrelationID

	var committed prediction.Prediction
	err := r.deps.DB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 2},
		func(ctx context.Context, tx pgx.Tx) error {
			if _, paused, perr := r.deps.Pauses.OpenPause(ctx, tx, auth.AgentID()); perr != nil {
				return perr
			} else if paused {
				return errs.New(errs.CodeKillSwitchActive, "agent: paused; no prediction is committed")
			}
			out, cerr := r.deps.Predictions.Commit(ctx, tx, p)
			committed = out
			return cerr
		})
	if err != nil {
		return "", err
	}
	run.PredictionID = committed.ID.String()
	if err := r.advance(ctx, *run, RunPredicted); err != nil {
		return "", err
	}
	return run.PredictionID, nil
}

// emitIntent proposes the trade. The idempotency key is derived from the run
// and the action, so a replay returns the original intent.
func (r *Runner) emitIntent(ctx context.Context, run *Run, auth Authority, d Decision) (string, error) {
	if d.Intent == nil {
		return "", errs.New(errs.CodeValidationFailed, "agent: CREATE_TRADE_INTENT decision carries no intent")
	}
	if run.IntentID != "" {
		return run.IntentID, nil
	}
	if run.PredictionID == "" {
		return "", errs.New(errs.CodeValidationFailed,
			"agent: an intent needs the prediction that preceded it").WithField("prediction_id", "required")
	}
	emitter, err := r.deps.EmitterFor(auth)
	if err != nil {
		return "", err
	}
	req := *d.Intent
	req.RunID = run.ID
	req.ActionName = d.ActionName
	req.PredictionID = run.PredictionID
	req.CorrelationID = run.CorrelationID

	var created intent.TradeIntent
	err = r.deps.DB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 2},
		func(ctx context.Context, tx pgx.Tx) error {
			out, eerr := emitter.Emit(ctx, tx, req)
			created = out
			return eerr
		})
	if err != nil {
		return "", err
	}
	run.IntentID = created.ID.String()
	return run.IntentID, nil
}

// informationSet turns the gathered observations into the prediction's
// information set: what was read, from where, and from when it was usable.
func informationSet(obs map[string]Observation) []prediction.InformationItem {
	out := make([]prediction.InformationItem, 0, len(obs))
	for name, o := range obs {
		out = append(out, prediction.InformationItem{
			Dependency:          name,
			InvocationID:        o.InvocationID.String(),
			OutputHash:          o.OutputHash,
			DecisionAvailableAt: o.DecisionAvailableAt,
		})
	}
	return out
}

func totalCost(obs map[string]Observation) money.USD {
	var total money.USD
	for _, o := range obs {
		sum, err := total.Add(o.CostUSD)
		if err != nil {
			return total
		}
		total = sum
	}
	return total
}

// advance moves the run forward one status.
func (r *Runner) advance(ctx context.Context, run Run, to RunStatus) error {
	if !CanAdvance(run.Status, to) {
		return nil
	}
	run.Status = to
	return r.deps.DB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 2},
		func(ctx context.Context, tx pgx.Tx) error {
			return r.deps.Store.UpdateRun(ctx, tx, run)
		})
}

// finish writes the terminal status onto the caller's run, so what the caller
// returns is what was persisted rather than a stale copy.
func (r *Runner) finish(ctx context.Context, run *Run, to RunStatus, reason SkipReason) error {
	now := r.deps.Clock.Now()
	run.Status = to
	run.SkipReason = reason
	run.FinishedAt = &now
	if run.BuildVersion == "" {
		run.BuildVersion = r.deps.BuildVersion
	}
	return r.deps.DB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 2},
		func(ctx context.Context, tx pgx.Tx) error {
			return r.deps.Store.UpdateRun(ctx, tx, *run)
		})
}

// skip ends the run cleanly with a recorded reason. Every early exit goes
// through here, so "the run stopped" is never an unexplained absence.
func (r *Runner) skip(ctx context.Context, run Run, reason SkipReason, detail string) (RunResult, error) {
	if !reason.Valid() {
		reason = SkipMissingDependency
	}
	run.Error = detail
	if err := r.finish(ctx, &run, RunSkipped, reason); err != nil {
		return RunResult{}, err
	}
	return RunResult{Run: run, SkipReason: reason}, nil
}

// fail records an unrecoverable run error and returns it.
func (r *Runner) fail(ctx context.Context, run Run, cause error) (RunResult, error) {
	run.Error = cause.Error()
	if err := r.finish(ctx, &run, RunFailed, ""); err != nil {
		return RunResult{}, err
	}
	return RunResult{Run: run}, cause
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`[]`)
	}
	return b
}
