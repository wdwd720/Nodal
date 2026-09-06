package reconciliation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
)

func TestStatus_TransitionTableMatchesPart51(t *testing.T) {
	t.Parallel()
	// PART 51 / RECONCILIATION.md §2, written out literally.
	want := map[Status][]Status{
		StatusOpen:              {StatusMatched, StatusMismatch},
		StatusMatched:           {},
		StatusMismatch:          {StatusInvestigating, StatusResolvedAutomatic, StatusEscalated},
		StatusInvestigating:     {StatusResolvedAutomatic, StatusResolvedManual, StatusEscalated},
		StatusEscalated:         {StatusInvestigating},
		StatusResolvedAutomatic: {},
		StatusResolvedManual:    {},
	}
	assert.Equal(t, want, Transitions)

	for _, s := range AllStatuses() {
		assert.True(t, s.Valid(), "%s must be a declared status", s)
	}
	assert.False(t, StatusNone.Valid(), "NONE is not a stored status")

	// RESOLVED_MANUAL is reachable only from INVESTIGATING: a human must have
	// looked at the evidence before resolving.
	for from, tos := range Transitions {
		for _, to := range tos {
			if to == StatusResolvedManual {
				assert.Equal(t, StatusInvestigating, from,
					"RESOLVED_MANUAL must only be reachable from INVESTIGATING, not %s", from)
			}
		}
	}
}

func TestStatus_TerminalAndUnresolved(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		status     Status
		terminal   bool
		unresolved bool
	}{
		{StatusOpen, false, true},
		{StatusMatched, true, false},
		{StatusMismatch, false, true},
		{StatusInvestigating, false, true},
		{StatusEscalated, false, true},
		{StatusResolvedAutomatic, true, false},
		{StatusResolvedManual, true, false},
	} {
		assert.Equal(t, tc.terminal, tc.status.Terminal(), "%s terminal", tc.status)
		assert.Equal(t, tc.unresolved, tc.status.Unresolved(), "%s unresolved", tc.status)
	}
}

func TestCanTransition_RejectsEveryUndeclaredEdge(t *testing.T) {
	t.Parallel()
	for _, from := range AllStatuses() {
		for _, to := range AllStatuses() {
			declared := false
			for _, x := range Transitions[from] {
				if x == to {
					declared = true
				}
			}
			assert.Equal(t, declared, CanTransition(from, to), "%s -> %s", from, to)
		}
	}
	assert.False(t, CanTransition(StatusMatched, StatusMismatch))
	assert.False(t, CanTransition(StatusResolvedManual, StatusInvestigating))
	assert.False(t, CanTransition(StatusMismatch, StatusResolvedManual))
	assert.False(t, CanTransition(Status("NOPE"), StatusOpen))
}

func TestKind_AlwaysMaterial(t *testing.T) {
	t.Parallel()
	assert.True(t, KindLedgerInternal.AlwaysMaterial())
	assert.True(t, KindPositionLedger.AlwaysMaterial())
	assert.True(t, KindSubmissionUnknown.AlwaysMaterial())
	assert.False(t, KindExecution.AlwaysMaterial())
	assert.False(t, KindWalletBalance.AlwaysMaterial())
	assert.False(t, KindFunding.AlwaysMaterial())
	for _, k := range AllKinds() {
		assert.True(t, k.Valid())
	}
	assert.False(t, Kind("BALANCE_EDIT").Valid(), "there is no balance-edit kind in this system")
}

func TestActor_AgentIsRefusedEverywhere(t *testing.T) {
	t.Parallel()
	err := Actor{Type: security.ActorAgent, ID: "agent-1"}.validate()
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))

	for _, at := range []security.ActorType{security.ActorOperator, security.ActorUser, security.ActorSystem, security.ActorService} {
		assert.NoError(t, Actor{Type: at, ID: "x"}.validate(), "%s must be allowed", at)
	}
	assert.Error(t, Actor{Type: "WIZARD", ID: "x"}.validate())
	assert.Error(t, Actor{Type: security.ActorSystem}.validate(), "actor id is required")

	sys := SystemActor()
	assert.Equal(t, security.ActorSystem, sys.Type)
	assert.Equal(t, ActorName, sys.ID)
}

func TestManualResolution_Validate(t *testing.T) {
	t.Parallel()
	base := ManualResolution{
		Operator:    Actor{Type: security.ActorOperator, ID: "op-1"},
		Reason:      "chain credited 0.01 USDC less than the provider reported",
		EvidenceRef: "s3://evidence/incident-42",
	}
	require.NoError(t, base.Validate())

	agent := base
	agent.Operator = Actor{Type: security.ActorAgent, ID: "agent-1"}
	require.Error(t, agent.Validate())
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(agent.Validate()))

	system := base
	system.Operator = Actor{Type: security.ActorSystem, ID: "reconciliation"}
	err := system.Validate()
	require.Error(t, err, "SYSTEM may not resolve manually")
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	short := base
	short.Reason = "ok"
	assert.Error(t, short.Validate())

	noEvidence := base
	noEvidence.EvidenceRef = ""
	assert.Error(t, noEvidence.Validate())
}

func TestAutoCause_ClosedSet(t *testing.T) {
	t.Parallel()
	assert.Len(t, AllAutoCauses(), 4)
	for _, c := range AllAutoCauses() {
		assert.True(t, c.Valid())
	}
	assert.False(t, AutoCause("BECAUSE_I_SAID_SO").Valid())
}

func TestOpenRequest_Validate(t *testing.T) {
	t.Parallel()
	ok := OpenRequest{
		Kind: KindExecution, Mode: ModeEventDriven, ScopeType: ScopeAttempt, ScopeID: "a",
		Status: StatusMismatch, Actor: SystemActor(),
	}
	require.NoError(t, ok.validate())

	matchedBlocking := ok
	matchedBlocking.Status = StatusMatched
	matchedBlocking.BlocksNewRisk = true
	assert.Error(t, matchedBlocking.validate(), "a matched record never blocks new risk")

	resolved := ok
	resolved.Status = StatusResolvedManual
	assert.Error(t, resolved.validate(), "a record is never created already resolved")

	badKind := ok
	badKind.Kind = "SOMETHING"
	assert.Error(t, badKind.validate())
}
