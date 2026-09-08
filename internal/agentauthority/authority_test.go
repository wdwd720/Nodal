package agentauthority

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/valuedomain"
)

// everyCapability is the worst case: every agent-authority gate turned on.
func everyCapability() map[valuedomain.CapabilityKey]bool {
	caps := map[valuedomain.CapabilityKey]bool{}
	for _, l := range AllLevels() {
		if c := l.RequiresCapability(); c != "" {
			caps[c] = true
		}
	}
	return caps
}

func TestLevel_SevenLevelsDeclared(t *testing.T) {
	require.Len(t, AllLevels(), 7, "PART XXVIII names levels 0 through 6")
	for i, l := range AllLevels() {
		require.Equal(t, Level(i), l, "levels must be contiguous from 0")
		require.True(t, l.Valid())
		require.NotEmpty(t, l.Name())
		require.NotEqual(t, "UNKNOWN", l.Name())
	}
	require.False(t, Level(7).Valid())
	require.False(t, Level(-1).Valid())
	require.Equal(t, "UNKNOWN", Level(9).Name())
}

// TestLevel_FourAndAboveAreDisabled is PART XXVIII's default: levels 4 to 6 are
// declared so they can be added later, and disabled so declaring them does not
// make them usable.
func TestLevel_FourAndAboveAreDisabled(t *testing.T) {
	for _, l := range []Level{LevelResearchOnly, LevelRecommendation, LevelPrepareTransaction, LevelUserApprovedRule} {
		require.True(t, l.SupportedInThisBuild(), "%s must be product architecture", l)
		require.Empty(t, l.RequiresCapability(), "%s needs no capability", l)
	}
	for _, l := range []Level{LevelBoundedDiscretion, LevelAutonomousSelection, LevelAutonomousPortfolio} {
		require.False(t, l.SupportedInThisBuild(), "%s must be disabled in this build", l)
		require.NotEmpty(t, l.RequiresCapability(), "%s must name its own capability", l)
	}

	// Each disabled level has its OWN capability: approving "choose among the
	// options I picked" is not approving "choose my investments".
	seen := map[valuedomain.CapabilityKey]bool{}
	for _, l := range []Level{LevelBoundedDiscretion, LevelAutonomousSelection, LevelAutonomousPortfolio} {
		c := l.RequiresCapability()
		require.False(t, seen[c], "levels must not share a capability")
		seen[c] = true
	}
}

func TestLevel_ParseRoundTrips(t *testing.T) {
	for _, l := range AllLevels() {
		for _, form := range []string{l.String(), l.Name(), strings.ToLower(l.Name())} {
			got, err := ParseLevel(form)
			require.NoError(t, err, "parsing %q", form)
			require.Equal(t, l, got)
		}
	}
	for _, bad := range []string{"", "7", "-1", "GOD_MODE", "  "} {
		_, err := ParseLevel(bad)
		require.Error(t, err, "%q must not parse", bad)
	}
}

func TestMatrix_IsComplete(t *testing.T) {
	require.NoError(t, Validate(),
		"every declared action must be either permitted at a level or forbidden always")
}

// TestPermits_ForbiddenActionsAreRefusedAtEveryLevelWithEveryCapability is
// PART XXXI, and acceptance tests AGT-001 and AGT-003.
//
// It is the test that would have to be deleted for an agent to gain withdrawal
// authority, which is exactly why it enumerates rather than sampling.
func TestPermits_ForbiddenActionsAreRefusedAtEveryLevelWithEveryCapability(t *testing.T) {
	forbidden := []Action{
		ActionWithdraw, ActionTransferValue, ActionChangeOwnLimits, ActionChangeOwnAuthority,
		ActionEditPolicy, ActionAccessSecrets, ActionSignArbitrary, ActionBypassRisk,
		ActionCallArbitraryHost,
	}
	for _, a := range forbidden {
		for _, l := range AllLevels() {
			d := Permits(l, a, everyCapability())
			require.False(t, d.Allowed,
				"%s was permitted at %s with every capability active", a, l)
			require.Contains(t, d.Reasons, ReasonForbiddenAlways)
			require.NotEmpty(t, d.Detail, "a refusal must say why")
		}
	}
}

func TestPermits_TheLadderIsMonotonic(t *testing.T) {
	// Anything permitted at a level is permitted at every higher SUPPORTED
	// level. Monotonicity is what makes "level" a meaningful word.
	for _, a := range AllActions() {
		if forbidden, _ := ForbiddenAlways(a); forbidden {
			continue
		}
		firstAllowed := -1
		for _, l := range AllLevels() {
			if Permits(l, a, everyCapability()).Allowed {
				firstAllowed = int(l)
				break
			}
		}
		if firstAllowed < 0 {
			continue
		}
		for l := Level(firstAllowed); l <= MaxSupportedLevel; l++ {
			require.True(t, Permits(l, a, everyCapability()).Allowed,
				"%s is permitted at %d but not at the higher level %s", a, firstAllowed, l)
		}
	}
}

func TestPermits_LevelZeroCanOnlyThink(t *testing.T) {
	caps := everyCapability()
	for _, a := range []Action{ActionReadData, ActionRequestInference, ActionCommitPrediction} {
		require.True(t, Permits(LevelResearchOnly, a, caps).Allowed, "%s", a)
	}
	d := Permits(LevelResearchOnly, ActionProposeIntent, caps)
	require.False(t, d.Allowed, "research-only must not even propose an action")
	require.Contains(t, d.Reasons, ReasonLevelTooLow)
	require.Equal(t, LevelRecommendation, d.RequiredLevel,
		"the refusal must say which level would permit it")
}

func TestPermits_LevelThreeExecutesAnApprovedRuleAndNothingBeyond(t *testing.T) {
	caps := everyCapability()
	require.True(t, Permits(LevelUserApprovedRule, ActionExecuteApprovedRule, caps).Allowed)

	d := Permits(LevelUserApprovedRule, ActionSelectAmongApproved, caps)
	require.False(t, d.Allowed,
		"the user approved a specific rule, not the agent's judgement about options")
	require.Contains(t, d.Reasons, ReasonLevelTooLow)
	require.Equal(t, LevelBoundedDiscretion, d.RequiredLevel)
}

// TestPermits_DisabledLevelsRefuseEvenWithTheirCapabilityOn is the belt to the
// capability's braces: level 4 needs both a capability AND a build that
// supports it, and this build supports neither.
func TestPermits_DisabledLevelsRefuseEvenWithTheirCapabilityOn(t *testing.T) {
	for _, tc := range []struct {
		level  Level
		action Action
	}{
		{LevelBoundedDiscretion, ActionSelectAmongApproved},
		{LevelAutonomousSelection, ActionSelectInstrument},
		{LevelAutonomousPortfolio, ActionAllocatePortfolio},
	} {
		d := Permits(tc.level, tc.action, everyCapability())
		require.False(t, d.Allowed,
			"%s must be refused in this build even with %s active", tc.level, tc.level.RequiresCapability())
		require.Contains(t, d.Reasons, ReasonLevelNotSupported)
	}
}

func TestPermits_ADisabledLevelWithNoCapabilityGivesBothReasons(t *testing.T) {
	d := Permits(LevelBoundedDiscretion, ActionSelectAmongApproved, nil)
	require.False(t, d.Allowed)
	require.Contains(t, d.Reasons, ReasonLevelNotSupported)
	require.Contains(t, d.Reasons, ReasonCapabilityOff,
		"an operator needs to know about both obstacles, not just the first")
	require.Equal(t, valuedomain.CapabilityKey("AGENT_BOUNDED_DISCRETION"), d.RequiredCapability)
}

func TestPermits_UnknownInputsFailClosed(t *testing.T) {
	d := Permits(LevelUserApprovedRule, "MINT_MONEY", everyCapability())
	require.False(t, d.Allowed)
	require.Contains(t, d.Reasons, ReasonUnknownAction)

	d = Permits(Level(99), ActionReadData, everyCapability())
	require.False(t, d.Allowed)
	require.Contains(t, d.Reasons, ReasonUnknownLevel)
}

// TestPermits_ForbiddenIsCheckedBeforeLevel matters because it means no future
// edit to the level matrix can open a permanently forbidden action.
func TestPermits_ForbiddenIsCheckedBeforeLevel(t *testing.T) {
	d := Permits(Level(99), ActionWithdraw, everyCapability())
	require.False(t, d.Allowed)
	require.Contains(t, d.Reasons, ReasonForbiddenAlways,
		"withdrawal is refused before the level is even consulted")
	require.NotContains(t, d.Reasons, ReasonUnknownLevel)
}

func TestDescribe_CoversEveryLevelAndEveryForbiddenAction(t *testing.T) {
	desc := Describe()
	for _, l := range AllLevels() {
		require.Contains(t, desc, l.String())
	}
	for _, a := range AllActions() {
		require.Contains(t, desc, string(a))
	}
	require.Contains(t, desc, "DISABLED IN THIS BUILD")
	require.Contains(t, desc, "Never permitted at any level")
}
