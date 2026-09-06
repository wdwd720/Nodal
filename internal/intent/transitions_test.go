package intent_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/intent"
)

// legalEdges spells the state machine out independently of the production
// table (SETTLEMENT_COMPILER.md §2) so a change on either side is a visible
// diff here.
var legalEdges = map[string]bool{
	"RECEIVED->ELIGIBILITY_CHECKED": true, "RECEIVED->REJECTED": true, "RECEIVED->EXPIRED": true, "RECEIVED->CANCELLED": true, "RECEIVED->FAILED": true,
	"ELIGIBILITY_CHECKED->RISK_CHECKED": true, "ELIGIBILITY_CHECKED->REJECTED": true, "ELIGIBILITY_CHECKED->EXPIRED": true, "ELIGIBILITY_CHECKED->CANCELLED": true, "ELIGIBILITY_CHECKED->FAILED": true,
	"RISK_CHECKED->RESERVED": true, "RISK_CHECKED->REJECTED": true, "RISK_CHECKED->EXPIRED": true, "RISK_CHECKED->CANCELLED": true, "RISK_CHECKED->FAILED": true,
	"RESERVED->PLANNED": true, "RESERVED->NO_VALID_PLAN": true, "RESERVED->REJECTED": true, "RESERVED->EXPIRED": true, "RESERVED->CANCELLED": true, "RESERVED->FAILED": true,
	"PLANNED->EXECUTING": true, "PLANNED->NO_VALID_PLAN": true, "PLANNED->REJECTED": true, "PLANNED->EXPIRED": true, "PLANNED->CANCELLED": true, "PLANNED->FAILED": true,
	"EXECUTING->COMPLETED": true, "EXECUTING->EXPIRED": true, "EXECUTING->CANCELLED": true, "EXECUTING->FAILED": true,
}

// TestTransitions_Exhaustive checks every from×to pair, including self
// loops and unknown statuses, against legalEdges.
func TestTransitions_Exhaustive(t *testing.T) {
	t.Parallel()
	statuses := intent.Statuses()
	require.Len(t, intent.Transitions, len(statuses), "every status has a row in the table")
	checked := 0
	for _, from := range statuses {
		_, present := intent.Transitions[from]
		require.True(t, present, "status %s missing from Transitions", from)
		for _, to := range statuses {
			key := fmt.Sprintf("%s->%s", from, to)
			assert.Equal(t, legalEdges[key], intent.CanTransition(from, to), key)
			checked++
		}
	}
	assert.Equal(t, len(statuses)*len(statuses), checked)
	assert.Equal(t, len(legalEdges), countEdges(), "the production table has exactly the documented edges")

	for _, s := range intent.TerminalStatuses() {
		for _, to := range statuses {
			assert.False(t, intent.CanTransition(s, to), "%s is terminal", s)
		}
	}
	for _, from := range statuses {
		assert.False(t, intent.CanTransition(from, from), "no self loop for %s", from)
		assert.False(t, intent.CanTransition(from, "BOGUS"))
		assert.False(t, intent.CanTransition("BOGUS", from))
	}
	assert.False(t, intent.CanTransition("", intent.StatusReceived), "creation is not a transition")
}

func countEdges() int {
	n := 0
	for _, tos := range intent.Transitions {
		n += len(tos)
	}
	return n
}

func TestTransitions_HappyPathReachesCompleted(t *testing.T) {
	t.Parallel()
	path := []intent.Status{
		intent.StatusReceived, intent.StatusEligibilityChecked, intent.StatusRiskChecked,
		intent.StatusReserved, intent.StatusPlanned, intent.StatusExecuting, intent.StatusCompleted,
	}
	for i := 1; i < len(path); i++ {
		assert.True(t, intent.CanTransition(path[i-1], path[i]), "%s -> %s", path[i-1], path[i])
	}
	// Every non-terminal status can reach every "policy said no / world moved on" side state.
	for _, from := range intent.Statuses() {
		if from.IsTerminal() {
			continue
		}
		for _, side := range []intent.Status{intent.StatusExpired, intent.StatusCancelled, intent.StatusFailed} {
			assert.True(t, intent.CanTransition(from, side), "%s -> %s", from, side)
		}
	}
	// NO_VALID_PLAN needs capital reserved first; REJECTED is a policy answer and cannot follow execution.
	assert.False(t, intent.CanTransition(intent.StatusReceived, intent.StatusNoValidPlan))
	assert.False(t, intent.CanTransition(intent.StatusRiskChecked, intent.StatusNoValidPlan))
	assert.False(t, intent.CanTransition(intent.StatusExecuting, intent.StatusRejected))
	assert.False(t, intent.CanTransition(intent.StatusExecuting, intent.StatusNoValidPlan))
}

func TestRequiresRejectionCode(t *testing.T) {
	t.Parallel()
	for _, s := range intent.Statuses() {
		want := s == intent.StatusRejected || s == intent.StatusNoValidPlan
		assert.Equal(t, want, intent.RequiresRejectionCode(s), s)
	}
}
