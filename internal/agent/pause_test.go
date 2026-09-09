package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/security"
)

// TestPauseRequiresAnExplicitOpenOrdersPolicy: PART 71 forbids an implicit
// decision about money already in flight.
func TestPauseRequiresAnExplicitOpenOrdersPolicy(t *testing.T) {
	t.Parallel()
	req := PauseRequest{ReasonCode: PauseOwnerRequest, Reason: "owner asked to stop"}
	err := req.Validate()
	require.Error(t, err, "there is no default open-orders policy")

	req.OpenOrdersPolicy = LeaveOpenOrders
	require.NoError(t, req.Validate())

	req.OpenOrdersPolicy = CancelCancelableOrders
	require.NoError(t, req.Validate())

	req.OpenOrdersPolicy = "CANCEL_EVERYTHING"
	require.Error(t, req.Validate(), "only the two declared policies exist")
}

func TestLeavePolicyNamesNoCancellationWorkflow(t *testing.T) {
	t.Parallel()
	req := PauseRequest{
		ReasonCode: PauseOperator, Reason: "incident triage",
		OpenOrdersPolicy: LeaveOpenOrders, CancelWorkflowID: "wf-1",
	}
	require.Error(t, req.Validate(),
		"LEAVE cancels nothing, so naming a cancellation workflow is a contradiction")
}

func TestKillSwitchPauseCarriesItsSwitch(t *testing.T) {
	t.Parallel()
	req := PauseRequest{
		ReasonCode: PauseKillSwitch, Reason: "global kill engaged",
		OpenOrdersPolicy: LeaveOpenOrders,
	}
	require.Error(t, req.Validate(), "a KILL_SWITCH pause must name the switch")
	req.KillSwitchID = NewAgentID().String()
	require.NoError(t, req.Validate())
}

func TestPauseReasonMustBeSubstantive(t *testing.T) {
	t.Parallel()
	req := PauseRequest{ReasonCode: PauseOperator, Reason: "x", OpenOrdersPolicy: LeaveOpenOrders}
	require.Error(t, req.Validate(), "a one-character reason is not auditable")
}

// TestOnlyPeopleResume is PART 71's resume rule: never SYSTEM, never an agent.
func TestOnlyPeopleResume(t *testing.T) {
	t.Parallel()
	assert.True(t, CanResume(security.ActorUser))
	assert.True(t, CanResume(security.ActorOperator))
	assert.False(t, CanResume(security.ActorSystem), "SYSTEM must never resume: the condition must be judged")
	assert.False(t, CanResume(security.ActorAgent), "an agent must never resume itself")
	assert.False(t, CanResume(security.ActorService))
	assert.False(t, CanResume(""))
}

// TestSystemCannotRaiseAPersonalPause: an automatic pause is for machine
// conditions; "the owner asked" is not one of them.
func TestSystemCannotRaiseAPersonalPause(t *testing.T) {
	t.Parallel()
	assert.False(t, PauseOwnerRequest.SystemMayRaise())
	assert.False(t, PauseOperator.SystemMayRaise())
	for _, r := range []PauseReason{
		PauseKillSwitch, PauseBudgetExhausted, PauseModelUnavailable,
		PauseDataGap, PauseRiskViolation, PauseReconciliationMismatch, PauseSecurity,
	} {
		assert.Truef(t, r.SystemMayRaise(), "SYSTEM must be able to raise %s", r)
	}
}

// These two were called ...MirrorTheDatabaseCheck and never read the database:
// they asserted a hardcoded length and then that every member of a list is a
// member of that list, which holds however far the CHECK constraint has drifted
// from the Go declaration. The comparison their old names claimed is
// TestIntegration_TheReasonListsMatchTheDatabase, which reads
// pg_get_constraintdef and compares the literals. What is left here is what a
// test with no database can actually establish: the count is deliberate, and an
// undeclared value is refused.

func TestPauseReasonsAreDeclaredAndClosed(t *testing.T) {
	t.Parallel()
	require.Len(t, PauseReasons(), 9)
	for _, r := range PauseReasons() {
		assert.True(t, r.Valid())
	}
	assert.False(t, PauseReason("BECAUSE").Valid())
}

func TestSkipReasonsAreDeclaredAndClosed(t *testing.T) {
	t.Parallel()
	require.Len(t, SkipReasons(), 11)
	for _, r := range SkipReasons() {
		assert.True(t, r.Valid())
	}
	assert.False(t, SkipReason("DUNNO").Valid())
}

func TestPauseOpenReporting(t *testing.T) {
	t.Parallel()
	p := Pause{ID: NewPauseID(), AgentID: NewAgentID()}
	assert.True(t, p.Open())
	at := nowForTest()
	p.ResumedAt = &at
	assert.False(t, p.Open())
}
