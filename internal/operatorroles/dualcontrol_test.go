package operatorroles_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/operatorroles"
	"github.com/nodal/controlplane/internal/security"
)

// Dual control is a rule about PEOPLE, not about roles, and the bootstrap must
// not be able to weaken it however many roles it grants.
//
// These tests are in this package rather than in internal/security or
// internal/gates because the question they answer belongs to the mechanism that
// grants roles: "what could the worst declaration this variable accepts
// actually do?" The answer must be "no more than one person can do", and it has
// to keep being the answer as the permission matrix changes.

// everyRolePrincipal is the most privileged principal the directory could ever
// produce: every role it may name, at once, with a live break-glass elevation on
// top -- which the bootstrap itself refuses to grant, and which is added here
// anyway so the test measures the ceiling rather than the mechanism's own limit.
func everyRolePrincipal(subject string, now time.Time) security.Principal {
	until := now.Add(time.Hour)
	roles := append(operatorroles.Directory(), security.RoleBreakGlass)
	return security.Principal{
		SubjectID: subject, ActorType: security.ActorOperator, Roles: roles,
		SessionID: "0193b2e0-0000-7000-8000-0000000000ff",
		AuthTime:  now.Add(-time.Minute), AMR: []string{"pwd", "mfa"},
		BreakGlassUntil: &until,
	}
}

func TestBootstrap_APrincipalWithEveryRoleStillCannotApproveItsOwnProposal(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	p := everyRolePrincipal("subject-one", now)
	require.NoError(t, p.Validate())
	ctx := security.WithPrincipal(context.Background(), p)
	clk := func() time.Time { return now }

	for _, perm := range []security.Permission{
		security.PermGateApprove, security.PermPayoutApprove, security.PermCreditAdjust,
		security.PermKillRelease, security.PermWithdrawalApprove, security.PermLedgerApproveCorrection,
		security.PermReconciliationApprove, security.PermEnvelopeApprove, security.PermAgentPromoteApprove,
		security.PermNativeMarketResume,
	} {
		// It holds the permission -- that is what makes the test meaningful.
		require.NoErrorf(t, security.RequireAt(ctx, perm, clk), "%s", perm)
		// And it still cannot be the second person.
		err := security.RequireDualControl(ctx, perm, p.SubjectID, clk)
		require.Errorf(t, err, "%s: a principal approved its own proposal", perm)
		assert.ErrorIsf(t, err, security.ErrSelfApproval, "%s", perm)
		// A different proposer is the only thing that changes the answer.
		assert.NoErrorf(t, security.RequireDualControl(ctx, perm, "subject-two", clk), "%s", perm)
	}
}

// The gate ceremony is stricter still: two distinct approvers, neither of whom
// proposed. One principal holding every role satisfies none of it.
func TestBootstrap_APrincipalWithEveryRoleCannotActivateAGateAlone(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	effective := now.Add(-time.Hour)

	full := func(approvers []gates.Approver, proposedBy string) *gates.Gate {
		return &gates.Gate{
			Capability: gates.LiveFunding, Environment: "PROD", State: gates.StateActive,
			ApprovalVersion: 1,
			LegalReviewRef:  "legal-1", ProviderContractRef: "provider-1",
			RiskApprovalRef: "risk-1", SecurityApprovalRef: "security-1",
			EvidenceHashes: []string{"e1"},
			ProposedBy:     proposedBy, Approvers: approvers,
			EffectiveAt: &effective,
		}
	}

	alone := full([]gates.Approver{
		{UserID: "subject-one", Step: gates.StepPropose, At: effective},
		{UserID: "subject-one", Step: gates.StepApprove, At: effective},
		{UserID: "subject-one", Step: gates.StepActivate, At: effective},
	}, "subject-one")
	v := gates.Evaluate(alone, true, now)
	assert.False(t, v.Active, "one principal activated a gate by itself")
	assert.Equal(t, gates.ReasonProposerApproved, v.Reason)

	// Two people is still not enough when one of them proposed.
	twoOfWhichOneProposed := full([]gates.Approver{
		{UserID: "subject-one", Step: gates.StepPropose, At: effective},
		{UserID: "subject-one", Step: gates.StepApprove, At: effective},
		{UserID: "subject-two", Step: gates.StepActivate, At: effective},
	}, "subject-one")
	v = gates.Evaluate(twoOfWhichOneProposed, true, now)
	assert.False(t, v.Active)
	assert.Equal(t, gates.ReasonProposerApproved, v.Reason)

	// Two distinct approvers who did not propose is the shape that passes, and
	// it is here as the positive control: without it the assertions above would
	// hold on a gate that could never be active for some other reason.
	proper := full([]gates.Approver{
		{UserID: "subject-one", Step: gates.StepPropose, At: effective},
		{UserID: "subject-two", Step: gates.StepApprove, At: effective},
		{UserID: "subject-three", Step: gates.StepActivate, At: effective},
	}, "subject-one")
	v = gates.Evaluate(proper, true, now)
	assert.True(t, v.Active, "three distinct principals did not satisfy the ceremony: %s", v.Reason)
}

// No role the directory may name carries a dual-control permission on its own.
// ADMIN is the interesting case: it is "everything except", and the exception is
// exactly this set.
func TestBootstrap_NoDirectoryRoleHoldsADualControlPermission(t *testing.T) {
	t.Parallel()
	for _, role := range operatorroles.Directory() {
		for _, perm := range security.RolePermissions[role] {
			assert.Falsef(t, security.IsDualControl(perm),
				"role %s holds the dual-control permission %s; the bootstrap could then grant one principal both sides", role, perm)
		}
	}
	// The negative control: BREAK_GLASS, which the directory may NOT name, is
	// where those permissions live.
	var dual int
	for _, perm := range security.RolePermissions[security.RoleBreakGlass] {
		if security.IsDualControl(perm) {
			dual++
		}
	}
	require.Positive(t, dual, "no role holds a dual-control permission at all; the check above proves nothing")
}
