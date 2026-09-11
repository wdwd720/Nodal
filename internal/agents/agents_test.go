package agents

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/agent"
	"github.com/nodal/controlplane/internal/agentauthority"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

func qty(t *testing.T, s string) money.Quantity {
	t.Helper()
	q, err := money.ParseQuantity(s)
	require.NoError(t, err)
	return q
}

func goodLimits(t *testing.T) Limits {
	t.Helper()
	return Limits{
		BudgetCredits:        qty(t, "100000"),
		PerTradeCapCredits:   qty(t, "10000"),
		DailyLossStopCredits: qty(t, "5000"),
		MaxPositionShareBPS:  2500,
		AllowedAssets:        []assets.AssetID{assets.NewAssetID()},
		Schedule:             Schedule{Kind: ScheduleManual},
	}
}

// TestAuthority_LevelsAboveThreeAreRefusedWithTheirCapability is the §17
// requirement stated as a test: 4, 5 and 6 are declared, disabled, and refused
// with the gate each would need — not "unknown", which would read as a typo.
func TestAuthority_LevelsAboveThreeAreRefusedWithTheirCapability(t *testing.T) {
	t.Parallel()
	for _, level := range []int{4, 5, 6} {
		_, err := ParseAuthorityLevel(level)
		require.Error(t, err, "level %d must be refused", level)
		e, ok := errs.As(err)
		require.True(t, ok)
		assert.Equal(t, errs.CodeCapabilityNotApproved, e.Code,
			"level %d is disabled by policy, which is not a validation mistake", level)
		assert.Contains(t, e.Detail, "disabled by policy")
		assert.NotEmpty(t, e.Fields["required_capability"],
			"a refusal must name the capability that would have to be approved")
	}
}

func TestAuthority_LevelsZeroToThreeAreTheProduct(t *testing.T) {
	t.Parallel()
	for _, level := range []int{0, 1, 2, 3} {
		l, err := ParseAuthorityLevel(level)
		require.NoErrorf(t, err, "level %d is supported in this build", level)
		assert.Equal(t, agentauthority.Level(level), l)
	}
	// And the ceiling this package enforces is the matrix's own, not a second
	// opinion beside it.
	assert.Equal(t, agentauthority.LevelUserApprovedRule, agentauthority.MaxSupportedLevel)
}

func TestAuthority_UndeclaredLevelIsAValidationFailure(t *testing.T) {
	t.Parallel()
	for _, level := range []int{-1, 7, 99} {
		_, err := ParseAuthorityLevel(level)
		require.Error(t, err)
		e, _ := errs.As(err)
		assert.Equal(t, errs.CodeValidationFailed, e.Code, "%d is not a level at all", level)
	}
}

// TestAuthority_EveryLevelHasWordsAndNoneImpliesUnrestrictedAuthority: goal §17
// forbids wording that implies unrestricted autonomous financial authority, and
// the summaries are the only place such wording could enter.
func TestAuthority_EveryLevelHasWordsAndNoneImpliesUnrestrictedAuthority(t *testing.T) {
	t.Parallel()
	levels := AuthorityLevels()
	require.Len(t, levels, len(agentauthority.AllLevels()))
	for _, d := range levels {
		assert.NotEmpty(t, d.Summary, "level %d has no words", d.Level)
		assert.NotContains(t, d.Summary, "unrestricted")
		assert.NotContains(t, d.Summary, "fully autonomous")
		if d.Level <= int(agentauthority.MaxSupportedLevel) {
			assert.True(t, d.Enabled, "level %d is the product and must be enabled", d.Level)
			assert.Empty(t, d.RequiredCapability, "a supported level needs no capability")
			continue
		}
		assert.False(t, d.Enabled, "level %d must be disabled", d.Level)
		assert.NotEmpty(t, d.RequiredCapability, "a disabled level must name its gate")
	}
}

func TestAuthority_OnlyLevelThreeExecutesWithoutConfirmation(t *testing.T) {
	t.Parallel()
	assert.False(t, ExecutesWithoutConfirmation(agentauthority.LevelResearchOnly))
	assert.False(t, ExecutesWithoutConfirmation(agentauthority.LevelRecommendation))
	assert.False(t, ExecutesWithoutConfirmation(agentauthority.LevelPrepareTransaction))
	assert.True(t, ExecutesWithoutConfirmation(agentauthority.LevelUserApprovedRule))
}

// TestLimits_MoneyIsExactAndBoundsAreEnforced covers the budget validation the
// brief asks for: strings, bounds, and the two relationships that make a cap a
// cap.
func TestLimits_MoneyIsExactAndBoundsAreEnforced(t *testing.T) {
	t.Parallel()
	base := goodLimits(t)
	require.NoError(t, base.Validate())

	cases := []struct {
		name  string
		mut   func(l *Limits)
		field string
	}{
		{"zero budget", func(l *Limits) { l.BudgetCredits = qty(t, "0") }, "budget_credits"},
		{"negative budget", func(l *Limits) { l.BudgetCredits = qty(t, "-1") }, "budget_credits"},
		{"zero per-trade cap", func(l *Limits) { l.PerTradeCapCredits = qty(t, "0") }, "per_trade_cap_credits"},
		{"cap above budget", func(l *Limits) { l.PerTradeCapCredits = qty(t, "100001") }, "per_trade_cap_credits"},
		{"loss stop above budget", func(l *Limits) { l.DailyLossStopCredits = qty(t, "100001") }, "daily_loss_stop_credits"},
		{"share zero", func(l *Limits) { l.MaxPositionShareBPS = 0 }, "max_position_share_bps"},
		{"share above 100%", func(l *Limits) { l.MaxPositionShareBPS = 10001 }, "max_position_share_bps"},
		{"no universe", func(l *Limits) { l.AllowedAssets = nil }, "allowed_assets"},
		{"duplicate asset", func(l *Limits) {
			a := assets.NewAssetID()
			l.AllowedAssets = []assets.AssetID{a, a}
		}, "allowed_assets"},
		{"zero asset id", func(l *Limits) { l.AllowedAssets = []assets.AssetID{{}} }, "allowed_assets"},
		{"interval without minutes", func(l *Limits) { l.Schedule = Schedule{Kind: ScheduleInterval} }, "schedule.interval_minutes"},
		{"interval too short", func(l *Limits) {
			l.Schedule = Schedule{Kind: ScheduleInterval, IntervalMinutes: 1}
		}, "schedule.interval_minutes"},
		{"manual with minutes", func(l *Limits) {
			l.Schedule = Schedule{Kind: ScheduleManual, IntervalMinutes: 30}
		}, "schedule.interval_minutes"},
		{"unknown schedule", func(l *Limits) { l.Schedule = Schedule{Kind: "HOURLY"} }, "schedule.kind"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := goodLimits(t)
			tc.mut(&l)
			err := l.Validate()
			require.Error(t, err)
			e, ok := errs.As(err)
			require.True(t, ok)
			assert.Equal(t, errs.CodeValidationFailed, e.Code)
			assert.Contains(t, e.Fields, tc.field, "the refusal must name the field: %v", e.Fields)
		})
	}
}

func TestLimits_TooManyAssetsIsRefused(t *testing.T) {
	t.Parallel()
	l := goodLimits(t)
	l.AllowedAssets = nil
	for i := 0; i <= MaxAllowedAssets; i++ {
		l.AllowedAssets = append(l.AllowedAssets, assets.NewAssetID())
	}
	err := l.Validate()
	require.Error(t, err)
	e, _ := errs.As(err)
	assert.Contains(t, e.Fields, "allowed_assets")
}

func TestLimits_AssetsAreRenderedSortedSoTwoIdenticalUniversesStoreIdentically(t *testing.T) {
	t.Parallel()
	a, b := assets.NewAssetID(), assets.NewAssetID()
	one := Limits{AllowedAssets: []assets.AssetID{a, b}}
	two := Limits{AllowedAssets: []assets.AssetID{b, a}}
	assert.Equal(t, one.AllowedAssetStrings(), two.AllowedAssetStrings())
}

// TestRuntime_NothingDeployedIsNeverReportedAsIdle is the honesty requirement.
// IDLE means something is there with nothing to do; a deployment that runs no
// evaluator must never say it.
func TestRuntime_NothingDeployedIsNeverReportedAsIdle(t *testing.T) {
	t.Parallel()
	st := Runtime(Deployment{}, RuntimeEvidence{})
	assert.Equal(t, ComponentNotDeployed, st.Evaluator)
	assert.Equal(t, ComponentNotDeployed, st.Executor)
	assert.Nil(t, st.LastHeartbeat)
	assert.Contains(t, st.Detail, "not being")
	assert.NotEmpty(t, st.Detail, "a status with no explanation reads as a transient glitch")
}

func TestRuntime_EvidenceCanOnlyWeakenTheDeploymentsClaim(t *testing.T) {
	t.Parallel()
	last := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	// Runs exist, but no evaluator is deployed: still NOT_DEPLOYED, and no
	// heartbeat is invented from a historical run.
	st := Runtime(Deployment{}, RuntimeEvidence{TotalRuns: 9, OpenRuns: 3, LastRunAt: &last})
	assert.Equal(t, ComponentNotDeployed, st.Evaluator)
	assert.Nil(t, st.LastHeartbeat)

	// Deployed with an open run: RUNNING.
	st = Runtime(Deployment{EvaluatorDeployed: true}, RuntimeEvidence{TotalRuns: 9, OpenRuns: 1, LastRunAt: &last})
	assert.Equal(t, ComponentRunning, st.Evaluator)
	require.NotNil(t, st.LastHeartbeat)

	// Deployed, nothing in flight: IDLE, and the detail names the last status.
	st = Runtime(Deployment{EvaluatorDeployed: true},
		RuntimeEvidence{TotalRuns: 9, LastRunAt: &last, LastRunStatus: "EVALUATED"})
	assert.Equal(t, ComponentIdle, st.Evaluator)
	assert.Contains(t, st.Detail, "EVALUATED")

	// Deployed and never used.
	st = Runtime(Deployment{EvaluatorDeployed: true}, RuntimeEvidence{})
	assert.Equal(t, ComponentIdle, st.Evaluator)
	assert.Contains(t, st.Detail, "not been evaluated")
}

func TestRuntime_EnabledIsNotRunning(t *testing.T) {
	t.Parallel()
	// The lifecycle says the agent may open runs...
	assert.True(t, Runnable(agent.StateBacktestEligible))
	// ...and the runtime says nothing will.
	assert.Equal(t, ComponentNotDeployed, Runtime(Deployment{}, RuntimeEvidence{}).Evaluator)
	assert.False(t, Runnable(agent.StateValidated))
	assert.False(t, Runnable(agent.StatePaused))
	assert.False(t, Runnable(agent.StateRevoked))
}

func TestActions_TheSetIsClosedAndNamed(t *testing.T) {
	t.Parallel()
	assert.ElementsMatch(t,
		[]Action{ActionEnable, ActionPause, ActionResume, ActionDisable, ActionArchive}, Actions())
	for _, a := range Actions() {
		assert.True(t, a.Valid(), a)
		assert.NotEmpty(t, defaultReason(a))
	}
	assert.False(t, Action("delete").Valid())
	assert.False(t, Action("").Valid())
}

func TestEvents_TheKindsAreClosedAndCoverEveryAction(t *testing.T) {
	t.Parallel()
	kinds := EventKinds()
	assert.ElementsMatch(t, []EventKind{
		EventAgentCreated, EventAgentEnabled, EventAgentPaused,
		EventAgentResumed, EventAgentDisabled, EventAgentArchived,
	}, kinds)
	for _, k := range kinds {
		assert.True(t, k.Valid(), k)
	}
	assert.False(t, EventKind("AGENT_TRADED").Valid())
}

func TestScheduleKinds_MatchTheDeclaredSet(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []ScheduleKind{ScheduleManual, ScheduleInterval}, ScheduleKinds())
	assert.True(t, ScheduleManual.Valid())
	assert.False(t, ScheduleKind("DAILY").Valid())
}

func TestGrant_RequiresTheThingsAPersonSupplied(t *testing.T) {
	t.Parallel()
	full := Grant{
		ID: NewGrantID(), AgentID: agent.NewAgentID(), AccountID: "acct",
		StrategyVersionID: "ver", Level: agentauthority.LevelRecommendation,
		Limits: goodLimits(t), GrantedByUserID: "user", GrantedAt: time.Now(),
	}
	require.NoError(t, full.Validate())

	missingVersion := full
	missingVersion.StrategyVersionID = ""
	err := missingVersion.Validate()
	require.Error(t, err)
	e, _ := errs.As(err)
	assert.Contains(t, e.Fields, "strategy_version_id")

	missingGrantor := full
	missingGrantor.GrantedByUserID = ""
	err = missingGrantor.Validate()
	require.Error(t, err)
	e, _ = errs.As(err)
	assert.Contains(t, e.Fields, "granted_by_user_id")

	overLevel := full
	overLevel.Level = agentauthority.LevelAutonomousPortfolio
	err = overLevel.Validate()
	require.Error(t, err)
	e, _ = errs.As(err)
	assert.Contains(t, e.Fields, "authority_level")
}

// TestExecutionCapability_IsTheGateThatAlreadyExists: a second name for the
// same authority is how a deployment ends up with one of them switched on.
func TestExecutionCapability_IsTheGateThatAlreadyExists(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "LIVE_AGENT_TRADING", ExecutionCapability)
}

func TestCompilerUnavailable_SaysWhatDidNotHappen(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "COMPILER_UNAVAILABLE", CompilerUnavailable)
	assert.Contains(t, compilerUnavailableDetail, "no strategy compiler backend configured")
	assert.Contains(t, compilerUnavailableDetail, "The attempt is recorded")
	assert.NotContains(t, compilerUnavailableDetail, "try again later",
		"a missing configuration is not a transient outage and must not be described as one")
}
