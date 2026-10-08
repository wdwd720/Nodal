package predmarket

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/security"
)

// TestLifecycle_IsExactlyTheSpecifiedTable pins the lifecycle edge by edge:
// DRAFT -> OPEN -> CLOSED -> RESOLVING -> RESOLVED -> SETTLED, any state but
// SETTLED and VOIDED -> VOIDED, RESOLVED -> DISPUTED -> RESOLVED | VOIDED.
// Nothing else.
func TestLifecycle_IsExactlyTheSpecifiedTable(t *testing.T) {
	want := map[[2]MarketStatus]bool{
		{StatusDraft, StatusOpen}: true, {StatusDraft, StatusVoided}: true,
		{StatusOpen, StatusClosed}: true, {StatusOpen, StatusVoided}: true,
		{StatusClosed, StatusResolving}: true, {StatusClosed, StatusVoided}: true,
		{StatusResolving, StatusResolved}: true, {StatusResolving, StatusVoided}: true,
		{StatusResolved, StatusSettled}: true, {StatusResolved, StatusDisputed}: true, {StatusResolved, StatusVoided}: true,
		{StatusDisputed, StatusResolved}: true, {StatusDisputed, StatusVoided}: true,
	}
	for _, from := range AllMarketStatuses() {
		for _, to := range AllMarketStatuses() {
			assert.Equal(t, want[[2]MarketStatus{from, to}], CanTransition(from, to), "%s -> %s", from, to)
		}
	}
	for _, s := range AllMarketStatuses() {
		if s.Terminal() {
			assert.Contains(t, []MarketStatus{StatusSettled, StatusVoided}, s)
			continue
		}
		assert.True(t, CanTransition(s, StatusVoided), "every unsettled state can be voided: %s", s)
	}
	assert.False(t, CanTransition(StatusDisputed, StatusSettled), "a disputed market never settles directly")
	assert.False(t, CanTransition(StatusClosed, StatusSettled), "no settlement before resolution")
	assert.False(t, CanTransition(StatusOpen, StatusResolved), "no resolution before close")
	assert.False(t, CanTransition(statusNone, StatusDraft), "a birth is an insert, never a transition")
}

func TestEnumLists_AreDistinctAndValid(t *testing.T) {
	seen := map[MarketStatus]bool{}
	for _, s := range AllMarketStatuses() {
		assert.True(t, s.Valid())
		assert.False(t, seen[s])
		seen[s] = true
	}
	assert.Len(t, seen, 8)
	assert.False(t, MarketStatus("LIVE").Valid())
	assert.False(t, statusNone.Valid(), "NONE names a birth, not a state")

	assert.ElementsMatch(t, []gates.Mode{gates.ModePaper, gates.ModeSandbox}, SupportedModes())
	for _, m := range SupportedModes() {
		assert.True(t, m.Simulated())
		spec, ok := gates.LookupAdvanced(gates.PredictionMarkets)
		require.True(t, ok)
		assert.Contains(t, spec.SimulatedModes, m, "a supported mode is one the gate spec implements")
	}
	assert.False(t, supportedMode(gates.ModeLive))
	assert.False(t, supportedMode(gates.ModeDevnet))

	assert.ElementsMatch(t, []security.ActorType{security.ActorOperator, security.ActorSystem}, LifecycleActorTypes())
	assert.NotContains(t, LifecycleActorTypes(), security.ActorAgent)
	assert.Len(t, AllResolutionKinds(), 3)
	assert.Len(t, AllSettlementKinds(), 2)
	assert.Len(t, AllRoundingPolicies(), 2)
}
