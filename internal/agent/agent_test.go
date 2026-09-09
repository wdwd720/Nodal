package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLadderIsExactlyPart68 pins the ladder: adding, removing or reordering a
// rung is a product decision, not a refactor.
func TestLadderIsExactlyPart68(t *testing.T) {
	t.Parallel()
	require.Equal(t, []Stage{
		StageDraft, StageCompiled, StageValidated, StageBacktestEligible,
		StageShadow, StageCanary, StageLimited, StageLive,
	}, Stages())
	require.Len(t, States(), 12, "eight ladder positions plus four side states")
}

// TestCanTransitionAllowsOnlyAdjacentPromotions is the core lifecycle table:
// every non-adjacent ladder move is refused, in both directions.
func TestCanTransitionAllowsOnlyAdjacentPromotions(t *testing.T) {
	t.Parallel()
	stages := Stages()
	for i, from := range stages {
		for j, to := range stages {
			want := j == i+1
			got := CanTransition(from.State(), to.State())
			assert.Equalf(t, want, got, "CanTransition(%s, %s)", from, to)
		}
	}
}

func TestCanTransitionSkippedRungsAreRefused(t *testing.T) {
	t.Parallel()
	skips := []struct{ from, to State }{
		{StateDraft, StateValidated},
		{StateDraft, StateLive},
		{StateCompiled, StateBacktestEligible},
		{StateValidated, StateShadow},
		{StateBacktestEligible, StateCanary},
		{StateShadow, StateLimited},
		{StateShadow, StateLive},
		{StateCanary, StateLive},
	}
	for _, s := range skips {
		assert.Falsef(t, CanTransition(s.from, s.to), "%s -> %s must be refused: stages are never skipped", s.from, s.to)
	}
}

func TestCanTransitionDemotionIsRefused(t *testing.T) {
	t.Parallel()
	// Reducing authority is PAUSE or REVOKE, never a downward ladder step:
	// a demotion would leave no pause record and no revocation reason.
	for _, s := range []struct{ from, to State }{
		{StateLive, StateLimited},
		{StateLimited, StateCanary},
		{StateCanary, StateShadow},
		{StateShadow, StateBacktestEligible},
	} {
		assert.Falsef(t, CanTransition(s.from, s.to), "%s -> %s", s.from, s.to)
		assert.Truef(t, CanTransition(s.from, StatePaused), "%s must always be pausable", s.from)
		assert.Truef(t, CanTransition(s.from, StateRevoked), "%s must always be revocable", s.from)
	}
}

func TestCanTransitionSideStates(t *testing.T) {
	t.Parallel()
	for _, stage := range Stages() {
		from := stage.State()
		assert.Truef(t, CanTransition(from, StatePaused), "%s -> PAUSED", from)
		assert.Truef(t, CanTransition(from, StateFailed), "%s -> FAILED", from)
		assert.Truef(t, CanTransition(from, StateRevoked), "%s -> REVOKED", from)
		assert.Truef(t, CanTransition(from, StateSuperseded), "%s -> SUPERSEDED", from)
	}
	// PAUSED returns to a stage.
	for _, stage := range Stages() {
		assert.Truef(t, CanTransition(StatePaused, stage.State()), "PAUSED -> %s", stage)
	}
	// FAILED has only the two terminal exits.
	assert.True(t, CanTransition(StateFailed, StateRevoked))
	assert.True(t, CanTransition(StateFailed, StateSuperseded))
	for _, stage := range Stages() {
		assert.Falsef(t, CanTransition(StateFailed, stage.State()), "FAILED -> %s must be refused", stage)
	}
	assert.False(t, CanTransition(StateFailed, StatePaused))
	// Terminal states never move.
	for _, terminal := range []State{StateRevoked, StateSuperseded} {
		for _, to := range States() {
			assert.Falsef(t, CanTransition(terminal, to), "%s -> %s must be refused", terminal, to)
		}
	}
}

func TestCanTransitionRejectsUnknownAndSelf(t *testing.T) {
	t.Parallel()
	assert.False(t, CanTransition(StateShadow, StateShadow))
	assert.False(t, CanTransition("NOPE", StateShadow))
	assert.False(t, CanTransition(StateShadow, "NOPE"))
	assert.False(t, CanTransition("", ""))
}

// TestModeMappingMirrorsTheDatabaseCheck: the Go mapping and the agents CHECK
// must agree, or a legal-looking promotion fails at COMMIT.
// TestModeMappingMirrorsTheDatabaseCheck compares ModesForStage against a table
// typed out below. That is worth having -- it states the intended mapping in one
// readable place -- but the name overclaims: the CHECK it names is
// agents_check3, and nothing here opens a database. The comparison against the
// constraint itself is test/integration/enums'
// TestIntegration_TheStageModeMappingMatchesTheDatabase, added with F-74.
func TestModeMappingMirrorsTheDatabaseCheck(t *testing.T) {
	t.Parallel()
	cases := []struct {
		stage Stage
		modes []Mode
	}{
		{StageDraft, nil},
		{StageCompiled, nil},
		{StageValidated, nil},
		{StageBacktestEligible, []Mode{ModeBacktest, ModePaper}},
		{StageShadow, []Mode{ModeShadow}},
		{StageCanary, []Mode{ModeCanary}},
		{StageLimited, []Mode{ModeLimited}},
		{StageLive, []Mode{ModeLive}},
	}
	for _, tc := range cases {
		t.Run(tc.stage.String(), func(t *testing.T) {
			require.Equal(t, tc.modes, ModesForStage(tc.stage))
			for _, m := range Modes() {
				want := false
				for _, allowed := range tc.modes {
					if allowed == m {
						want = true
					}
				}
				assert.Equalf(t, want, ModeAllowed(tc.stage, m), "%s at %s", m, tc.stage)
			}
			if len(tc.modes) == 0 {
				assert.True(t, ModeAllowed(tc.stage, ""), "a stage with no mode accepts the empty mode")
			} else {
				assert.False(t, ModeAllowed(tc.stage, ""), "a running stage must carry a mode")
			}
		})
	}
}

func TestStagesBelowBacktestEligibleNeverRun(t *testing.T) {
	t.Parallel()
	for _, s := range []State{StateDraft, StateCompiled, StateValidated} {
		assert.Falsef(t, s.Runs(), "%s must never open a run", s)
	}
	for _, s := range []State{StateBacktestEligible, StateShadow, StateCanary, StateLimited, StateLive} {
		assert.Truef(t, s.Runs(), "%s runs", s)
	}
	for _, s := range []State{StatePaused, StateFailed, StateRevoked, StateSuperseded} {
		assert.Falsef(t, s.Runs(), "%s must never open a run", s)
	}
}

func TestRealCapitalStagesRequireAnEnvelope(t *testing.T) {
	t.Parallel()
	for _, s := range []Stage{StageCanary, StageLimited, StageLive} {
		require.True(t, s.RealCapital(), "%s deploys capital", s)
		a := validAgent(s)
		a.EnvelopeID = ""
		err := a.Validate()
		require.Error(t, err, "%s without an envelope must be refused", s)
		assert.Contains(t, err.Error(), "invalid")
	}
	for _, s := range []Stage{StageDraft, StageCompiled, StageValidated, StageBacktestEligible, StageShadow} {
		assert.Falsef(t, s.RealCapital(), "%s must not deploy real capital", s)
	}
}

func TestAgentValidateRejectsAgentAuthoredAgents(t *testing.T) {
	t.Parallel()
	a := validAgent(StageShadow)
	a.CreatedByActorType = "AGENT"
	err := a.Validate()
	require.Error(t, err, "an agent can never create an agent")
}

func TestAgentValidateRejectsStateStageDisagreement(t *testing.T) {
	t.Parallel()
	a := validAgent(StageShadow)
	a.State = StateLive // not a side state, and not the stage
	require.Error(t, a.Validate())

	a = validAgent(StageShadow)
	a.State = StatePaused // a side state is allowed to differ
	require.NoError(t, a.Validate())
}

func TestRunStatusAdvanceIsForwardOnly(t *testing.T) {
	t.Parallel()
	assert.True(t, CanAdvance(RunStarted, RunGathering))
	assert.True(t, CanAdvance(RunGathering, RunEvaluated))
	assert.True(t, CanAdvance(RunEvaluated, RunPredicted))
	assert.True(t, CanAdvance(RunPredicted, RunIntentCreated))
	assert.True(t, CanAdvance(RunStarted, RunIntentCreated), "a run may jump forward")
	assert.False(t, CanAdvance(RunEvaluated, RunGathering), "runs never move backwards")
	assert.False(t, CanAdvance(RunIntentCreated, RunFailed), "a terminal status is final")
	assert.False(t, CanAdvance(RunSkipped, RunGathering))
	for _, from := range []RunStatus{RunStarted, RunGathering, RunEvaluated, RunPredicted} {
		assert.Truef(t, CanAdvance(from, RunSkipped), "%s -> SKIPPED", from)
		assert.Truef(t, CanAdvance(from, RunFailed), "%s -> FAILED", from)
	}
}

func TestRunValidateRequiresASkipReason(t *testing.T) {
	t.Parallel()
	r := validRun()
	r.Status = RunSkipped
	require.Error(t, r.Validate(), "a skipped run must name a declared reason")
	r.SkipReason = SkipConditionFalse
	require.NoError(t, r.Validate())
}

func TestRunValidateRequiresPredictionForIntent(t *testing.T) {
	t.Parallel()
	r := validRun()
	r.Status = RunIntentCreated
	r.IntentID = NewRunID().String()
	require.Error(t, r.Validate(), "INTENT_CREATED without a prediction must be refused")
	r.PredictionID = NewRunID().String()
	require.NoError(t, r.Validate())
}

func validAgent(stage Stage) Agent {
	a := Agent{
		ID: NewAgentID(), AccountID: NewAgentID().String(), StrategyID: NewAgentID().String(),
		StrategyVersionID: NewAgentID().String(), Name: "test agent", Stage: stage,
		State: stage.State(), Mode: DefaultModeForStage(stage), Version: 1,
		CreatedByActorType: "OPERATOR", CreatedByActorID: "op-1",
	}
	if stage.RealCapital() {
		a.EnvelopeID = NewAgentID().String()
	}
	if stage == StageDraft {
		a.StrategyVersionID = ""
	}
	return a
}

func validRun() Run {
	return Run{
		ID: NewRunID(), AgentID: NewAgentID(), AgentVersion: 1,
		StrategyVersionID: NewAgentID().String(), AccountID: NewAgentID().String(),
		Mode: ModeShadow, TriggerName: "tick", TriggerKind: TriggerOnInterval,
		TriggerDedupKey: []byte("k"), DecisionTime: nowForTest(), Status: RunStarted,
		CorrelationID: "corr-1",
	}
}
