//go:build integration

package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/intent"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/prediction"
	"github.com/nodal/controlplane/internal/strategy/ir"
)

// scriptedEvaluator is a deterministic stand-in for internal/strategy's
// evaluator. It performs no I/O and reads nothing but its EvalInput, which is
// exactly the contract the real one must satisfy.
type scriptedEvaluator struct {
	decisions  []Decision
	skipReason SkipReason
	err        error
	calls      int
	// seen records the observations the evaluator was handed, so a test can
	// assert it never saw a credential, a URL or raw provider bytes.
	seen map[string]Observation
}

func (e *scriptedEvaluator) Evaluate(_ context.Context, in EvalInput) (EvalOutput, error) {
	e.calls++
	e.seen = in.Observations
	if e.err != nil {
		return EvalOutput{}, e.err
	}
	return EvalOutput{
		EvaluatorVersion: "scripted/v1",
		InputHash:        bytes32("in"),
		TraceHash:        bytes32("trace"),
		Signals:          json.RawMessage(`[{"name":"mid","value":"142.35"}]`),
		ConditionResults: json.RawMessage(`[{"name":"enter","result":true}]`),
		Rationale:        json.RawMessage(`{"summary":"scripted"}`),
		Decisions:        e.decisions,
		SkipReason:       e.skipReason,
	}, nil
}

type runnerFixture struct {
	*brokerFixture
	evaluator *scriptedEvaluator
	runner    *Runner
	ledger    *prediction.PGLedger
}

func newRunnerFixture(t *testing.T, decisions []Decision) *runnerFixture {
	t.Helper()
	b := newBrokerFixture(t, StageShadow)
	doc := b.authorityIR()

	ev := &scriptedEvaluator{decisions: decisions}
	ledger, err := prediction.NewLedger(b.clk)
	require.NoError(t, err)

	intents := intent.NewRepository(b.clk, event.NewOutbox(b.clk), audit.NewWriter())
	runner, err := NewRunner(RunnerDeps{
		DB: testDB, Clock: b.clk, Store: b.store, Pauses: NewPauseChecker(),
		Envelope: NewEnvelopeReader(), Evaluator: ev, Predictions: ledger,
		BrokerFor: func(a Authority, runID RunID) (ToolBroker, error) {
			return NewBroker(BrokerDeps{
				DB: testDB, Clock: b.clk, Registry: NewToolRegistry(), Budgets: NewBudgetReader(),
				Pauses: NewPauseChecker(), Recorder: NewInvocationRecorder(),
				ModelCalls: NewModelCallRecorder(),
				Adapters:   map[string]ToolAdapter{ToolKey(b.toolCode, 1): b.adapter},
			}, a, runID)
		},
		EmitterFor: func(a Authority) (IntentEmitter, error) {
			return NewEmitter(EmitterDeps{
				Clock: b.clk, Intents: intents, Pauses: NewPauseChecker(),
				Budgets: NewBudgetReader(), Envelope: NewEnvelopeReader(),
			}, a)
		},
		IRFor:        func(context.Context, db.Querier, string) (*ir.IR, error) { return doc, nil },
		BuildVersion: "itest",
	})
	require.NoError(t, err)
	return &runnerFixture{brokerFixture: b, evaluator: ev, runner: runner, ledger: ledger}
}

func predictionDecision(t *testing.T, instrumentID instruments.InstrumentID) Decision {
	t.Helper()
	up, err := ir.ParseDecimalString("0.62")
	require.NoError(t, err)
	down, err := ir.ParseDecimalString("0.30")
	require.NoError(t, err)
	conf, err := ir.ParseDecimalString("0.55")
	require.NoError(t, err)
	return Decision{
		ActionName: "forecast", Kind: ir.ActionCommitPrediction,
		Prediction: &prediction.Prediction{
			InstrumentID: instrumentID, Horizon: time.Hour,
			Direction: prediction.DirectionUp, ProbabilityDirection: up,
			ExpectedReturnBPS: money.BPS(120), DownsideProbability: down,
			MaxDownsideBPS: money.BPS(200), Confidence: conf,
		},
	}
}

func intentDecision(instrumentID instruments.InstrumentID, notional money.USD, deadline time.Time) Decision {
	return Decision{
		ActionName: "enter", Kind: ir.ActionCreateTradeIntent,
		Intent: &EmitRequest{
			Action: intent.ActionAcquireNotional, InstrumentID: instrumentID,
			NotionalUSD: &notional, Deadline: deadline,
			Constraints: intent.Constraints{
				MaxSlippageBPS: 50, MaxFeeBPS: 30, MaxPriceImpactBPS: 100,
				QuoteFreshness: 5 * time.Second, ExecutionDeadline: deadline,
			},
		},
	}
}

// TestRunnerProducesExactlyOneRunPredictionAndIntent, and running the same run
// again after a simulated crash converges on the same three rows.
func TestRunnerProducesExactlyOneRunPredictionAndIntent(t *testing.T) {
	f := newRunnerFixture(t, nil)
	deadline := f.clk.Now().Add(time.Hour)
	f.evaluator.decisions = []Decision{
		predictionDecision(t, mustInstrument(t, f.instrumentID)),
		intentDecision(mustInstrument(t, f.instrumentID), money.USDFromMinor(1000), deadline),
	}

	res, err := f.runner.Run(context.Background(), f.run.ID)
	require.NoError(t, err)
	require.NotEmpty(t, res.PredictionID)
	require.NotEmpty(t, res.IntentID)
	assert.Equal(t, RunIntentCreated, res.Run.Status)

	// The evaluator saw typed observations and nothing else.
	require.Contains(t, f.evaluator.seen, "price")
	obs := f.evaluator.seen["price"]
	assert.Equal(t, `{"mid":"142.35"}`, string(obs.Payload))
	assert.NotEmpty(t, obs.InvocationID.String())

	// Re-running the same run is a no-op: it is already terminal.
	again, err := f.runner.Run(context.Background(), f.run.ID)
	require.NoError(t, err)
	assert.Equal(t, res.PredictionID, again.PredictionID)
	assert.Equal(t, res.IntentID, again.IntentID)

	assertCounts(t, f, 1, 1, 1)
}

// TestRunnerResumesAfterACrashBetweenPredictionAndIntent: the prediction is
// already committed and the intent is not; resuming produces exactly one of
// each rather than a second forecast or a duplicate order.
func TestRunnerResumesAfterACrashBetweenPredictionAndIntent(t *testing.T) {
	f := newRunnerFixture(t, nil)
	deadline := f.clk.Now().Add(time.Hour)
	predict := predictionDecision(t, mustInstrument(t, f.instrumentID))
	emit := intentDecision(mustInstrument(t, f.instrumentID), money.USDFromMinor(1000), deadline)

	// First attempt: the process dies right after the prediction, before the
	// intent. The evaluator therefore returns only the prediction decision.
	f.evaluator.decisions = []Decision{predict}
	first, err := f.runner.Run(context.Background(), f.run.ID)
	require.NoError(t, err)
	require.NotEmpty(t, first.PredictionID)
	assert.Empty(t, first.IntentID)
	assert.Equal(t, RunPredicted, first.Run.Status,
		"a prediction with no trade is a legitimate, recorded outcome")
	assertCounts(t, f, 1, 1, 0)

	// The process restarts and re-evaluates the same run. The prediction is
	// not committed twice, and the intent lands once.
	f.evaluator.decisions = []Decision{predict, emit}
	second, err := f.runner.Run(context.Background(), f.run.ID)
	require.NoError(t, err)
	assert.Equal(t, first.PredictionID, second.PredictionID,
		"resuming reuses the prediction already on record")
	require.NotEmpty(t, second.IntentID)
	assertCounts(t, f, 1, 1, 1)

	// A third attempt changes nothing.
	third, err := f.runner.Run(context.Background(), f.run.ID)
	require.NoError(t, err)
	assert.Equal(t, second.IntentID, third.IntentID)
	assertCounts(t, f, 1, 1, 1)
}

// TestRunnerRefusesAnIntentWithoutAPrediction.
func TestRunnerRefusesAnIntentWithoutAPrediction(t *testing.T) {
	f := newRunnerFixture(t, nil)
	deadline := f.clk.Now().Add(time.Hour)
	f.evaluator.decisions = []Decision{
		intentDecision(mustInstrument(t, f.instrumentID), money.USDFromMinor(1000), deadline),
	}
	_, err := f.runner.Run(context.Background(), f.run.ID)
	require.Error(t, err, "an intent without the prediction that preceded it must be refused")
	assertCounts(t, f, 1, 0, 0)

	run, err := f.store.GetRun(context.Background(), testDB, f.run.ID)
	require.NoError(t, err)
	assert.Equal(t, RunFailed, run.Status)
}

// TestRunnerSkipsAPausedAgentBeforeAnyToolCall.
func TestRunnerSkipsAPausedAgentBeforeAnyToolCall(t *testing.T) {
	f := newRunnerFixture(t, nil)
	f.evaluator.decisions = []Decision{predictionDecision(t, mustInstrument(t, f.instrumentID))}
	require.NoError(t, inTx(ctxAs(f.operator()), t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.lifecycle.Pause(ctx, tx, f.agent.ID, PauseRequest{
			ReasonCode: PauseOperator, Reason: "paused before the run started",
			OpenOrdersPolicy: LeaveOpenOrders,
		})
		return err
	}))

	res, err := f.runner.Run(context.Background(), f.run.ID)
	require.NoError(t, err)
	assert.Equal(t, SkipAgentPaused, res.SkipReason)
	assert.Equal(t, RunSkipped, res.Run.Status)
	assert.EqualValues(t, 0, f.adapter.dials.Load(), "a paused agent makes no provider call")
	assert.Zero(t, f.evaluator.calls, "a paused agent is not evaluated")
	assertCounts(t, f, 1, 0, 0)
}

// TestRunnerSkipsWhenARequiredDependencyIsRefused: a refused required tool
// ends the run cleanly with a recorded reason, and no prediction or intent is
// produced from partial data.
func TestRunnerSkipsWhenARequiredDependencyIsRefused(t *testing.T) {
	f := newRunnerFixture(t, nil)
	f.evaluator.decisions = []Decision{predictionDecision(t, mustInstrument(t, f.instrumentID))}
	_, err := testDB.Exec(context.Background(), `UPDATE tools SET status = 'DISABLED' WHERE id = $1`, f.toolID)
	require.NoError(t, err)

	res, err := f.runner.Run(context.Background(), f.run.ID)
	require.NoError(t, err)
	assert.Equal(t, RunSkipped, res.Run.Status)
	assert.Equal(t, SkipMissingDependency, res.SkipReason)
	assert.Zero(t, f.evaluator.calls, "a run with a missing required dependency is never evaluated")
	assertCounts(t, f, 1, 0, 0)

	// The refusal is on the record with its freshness check.
	run, err := f.store.GetRun(context.Background(), testDB, f.run.ID)
	require.NoError(t, err)
	var checks []FreshnessCheck
	require.NoError(t, json.Unmarshal(run.Evidence, &checks))
	require.Len(t, checks, 1)
	assert.False(t, checks[0].Present)
	assert.Equal(t, string(errs.CodeProviderUnavailable), checks[0].Refusal)
}

// TestRunnerSkipsWhenTheEvaluatorFindsNoAction.
func TestRunnerSkipsWhenTheEvaluatorFindsNoAction(t *testing.T) {
	f := newRunnerFixture(t, nil)
	f.evaluator.skipReason = SkipConditionFalse

	res, err := f.runner.Run(context.Background(), f.run.ID)
	require.NoError(t, err)
	assert.Equal(t, RunSkipped, res.Run.Status)
	assert.Equal(t, SkipConditionFalse, res.SkipReason)
	assertCounts(t, f, 1, 0, 0)
}

// TestRunnerSkipsWhenAModelIsRequiredButUnavailable is PART 177: there is no
// heuristic fallback and no synthetic answer.
func TestRunnerSkipsWhenAModelIsRequiredButUnavailable(t *testing.T) {
	f := newRunnerFixture(t, nil)
	f.evaluator.decisions = []Decision{
		{
			ActionName: "sentiment", Kind: ir.ActionCallModel,
			Model: &ModelCall{
				RunID: f.run.ID, Spec: ir.ModelCall{TemplateVersion: "t/1", MaxOutputTokens: 128, Required: true},
			},
		},
		predictionDecision(t, mustInstrument(t, f.instrumentID)),
	}
	// RunnerDeps.ModelCaller is nil: no model provider is wired.
	res, err := f.runner.Run(context.Background(), f.run.ID)
	require.NoError(t, err)
	assert.Equal(t, RunSkipped, res.Run.Status)
	assert.Equal(t, SkipModelUnavailable, res.SkipReason)
	assertCounts(t, f, 1, 0, 0, "no prediction and no intent follow an unavailable required model")
}

// TestRunnerContinuesWhenAnOptionalModelIsUnavailable: the model output is
// simply absent, never invented.
func TestRunnerContinuesWhenAnOptionalModelIsUnavailable(t *testing.T) {
	f := newRunnerFixture(t, nil)
	f.evaluator.decisions = []Decision{
		{
			ActionName: "sentiment", Kind: ir.ActionCallModel,
			Model: &ModelCall{
				RunID: f.run.ID, Spec: ir.ModelCall{TemplateVersion: "t/1", MaxOutputTokens: 128, Required: false},
			},
		},
		predictionDecision(t, mustInstrument(t, f.instrumentID)),
	}
	res, err := f.runner.Run(context.Background(), f.run.ID)
	require.NoError(t, err)
	assert.Equal(t, RunPredicted, res.Run.Status)
	require.NotEmpty(t, res.PredictionID)
	assertCounts(t, f, 1, 1, 0)

	var modelCalls int
	require.NoError(t, testDB.QueryRow(context.Background(),
		`SELECT count(*) FROM model_calls WHERE agent_run_id = $1`, f.run.ID).Scan(&modelCalls))
	assert.Zero(t, modelCalls, "an unwired model produces no fabricated call record")
}

// TestPredictionRecordsTheInformationSetItActuallyUsed.
func TestPredictionRecordsTheInformationSetItActuallyUsed(t *testing.T) {
	f := newRunnerFixture(t, nil)
	f.evaluator.decisions = []Decision{predictionDecision(t, mustInstrument(t, f.instrumentID))}

	res, err := f.runner.Run(context.Background(), f.run.ID)
	require.NoError(t, err)
	require.NotEmpty(t, res.PredictionID)

	id, err := prediction.ParsePredictionID(res.PredictionID)
	require.NoError(t, err)
	p, err := f.ledger.Get(context.Background(), testDB, id)
	require.NoError(t, err)

	obs := f.evaluator.seen["price"]
	want := prediction.InformationSetHash([]prediction.InformationItem{{
		Dependency: "price", InvocationID: obs.InvocationID.String(),
		OutputHash: obs.OutputHash, DecisionAvailableAt: obs.DecisionAvailableAt,
	}})
	assert.Equal(t, want, p.InformationSetHash,
		"the prediction pins exactly the observations the run consumed")
	assert.Equal(t, obs.DecisionAvailableAt.UTC(), p.DecisionAvailableAt.UTC(),
		"decision_available_at is the latest instant in the information set")
	assert.False(t, p.DecisionAvailableAt.After(p.CommittedAt))
}

// TestAgentIntentCarriesItsLinkageAndMode.
func TestAgentIntentCarriesItsLinkageAndMode(t *testing.T) {
	f := newRunnerFixture(t, nil)
	deadline := f.clk.Now().Add(time.Hour)
	f.evaluator.decisions = []Decision{
		predictionDecision(t, mustInstrument(t, f.instrumentID)),
		intentDecision(mustInstrument(t, f.instrumentID), money.USDFromMinor(1000), deadline),
	}
	res, err := f.runner.Run(context.Background(), f.run.ID)
	require.NoError(t, err)
	require.NotEmpty(t, res.IntentID)

	var (
		actorType, actorID, mode, key string
		agentID, versionID, predID    string
	)
	require.NoError(t, testDB.QueryRow(context.Background(),
		`SELECT actor_type, actor_id, mode, idempotency_key, agent_id::text, strategy_version_id::text,
		        prediction_id::text
		   FROM trade_intents WHERE id = $1`, res.IntentID).
		Scan(&actorType, &actorID, &mode, &key, &agentID, &versionID, &predID))

	assert.Equal(t, "AGENT", actorType)
	assert.Equal(t, f.agent.ID.String(), actorID, "the agent files the intent as itself, never as a person")
	assert.Equal(t, "SHADOW", mode, "the run's mode is copied, never inferred")
	assert.Equal(t, IdempotencyKeyFor(f.run.ID, "enter"), key,
		"the idempotency key is derived from the run and action, so a replay cannot duplicate the order")
	assert.Equal(t, f.agent.ID.String(), agentID)
	assert.Equal(t, f.agent.StrategyVersionID, versionID)
	assert.Equal(t, res.PredictionID, predID)
}

// assertCounts checks the number of runs, predictions and intents belonging to
// the fixture's run.
func assertCounts(t *testing.T, f *runnerFixture, runs, predictions, intents int, msgAndArgs ...any) {
	t.Helper()
	ctx := context.Background()
	var gotRuns, gotPredictions, gotIntents int
	require.NoError(t, testDB.QueryRow(ctx, `SELECT count(*) FROM agent_runs WHERE id = $1`, f.run.ID).Scan(&gotRuns))
	require.NoError(t, testDB.QueryRow(ctx, `SELECT count(*) FROM predictions WHERE agent_run_id = $1`, f.run.ID).Scan(&gotPredictions))
	require.NoError(t, testDB.QueryRow(ctx,
		`SELECT count(*) FROM trade_intents WHERE idempotency_key LIKE $1`, "run:"+f.run.ID.String()+":%").Scan(&gotIntents))
	assert.Equal(t, runs, gotRuns, msgAndArgs...)
	assert.Equal(t, predictions, gotPredictions, msgAndArgs...)
	assert.Equal(t, intents, gotIntents, msgAndArgs...)
}

func mustInstrument(t *testing.T, s string) instruments.InstrumentID {
	t.Helper()
	out, err := instruments.ParseInstrumentID(s)
	require.NoError(t, err)
	return out
}
