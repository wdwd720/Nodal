package adminplane

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/security"
)

// The beneficiary of an elevation is never offered a live Approve button
// (F-71).
//
// decideApprove has the rule: for a kind whose spec sets ApproverIsNotTarget,
// the person named by target_id may not approve it, because the second
// signature would be supplied by the beneficiary. The server enforces it and
// that is proven end to end.
//
// The affordance layer's copy of it was never evaluated true. Every fixture in
// this package used TargetID "t-1", which is not a user id, so canonicalUserID
// returned "" and the a.UserID == "" case above it always caught first. The
// three guards that should have noticed each missed for a different structural
// reason: TestVectorsAreNotVacuous asserts every Reason occurs, and
// ReasonSelfApproval did occur -- always from the proposer arm, never this one;
// the agreement test drives the same fixtures; and TestNoStandingRoleCanApprove
// skips kinds whose approve permission is standing, which skips
// BREAK_GLASS_GRANT, the one kind carrying the flag.
//
// So authority.json could not carry the flag, the console's port had no target
// check at all, and the grantee was shown an enabled Approve button the server
// refuses -- which doc.go names as the one thing this package must never do:
// "Be more permissive than the enforcing layer ... that is the bug that
// produces a dead button."
func TestDecideApprove_TheTargetOfAnElevationCannotApproveIt(t *testing.T) {
	const (
		proposer = "01a0754e-1111-7000-8000-000000000001"
		grantee  = "01a0754e-2222-7000-8000-000000000002"
		third    = "01a0754e-3333-7000-8000-000000000003"
	)
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

	spec, ok := admin.Spec(admin.KindBreakGlassGrant)
	require.True(t, ok)
	require.True(t, spec.ApproverIsNotTarget,
		"this test is about that flag; if BREAK_GLASS_GRANT no longer carries it, it guards nothing")

	action := admin.Action{
		Kind: admin.KindBreakGlassGrant, Status: admin.StatusProposed, RequiresDual: true,
		TargetType: "user", TargetID: grantee,
		ProposedBy: proposer, ProposedAt: now.Add(-time.Minute),
		ExpiresAt: now.Add(spec.Expiry),
	}
	// Fully entitled in every other respect: holds the approve permission as a
	// standing role, freshly and strongly authenticated, not the proposer.
	entitled := func(uid string) Actor {
		return NewActor(security.Principal{
			SubjectID: uid, ActorType: security.ActorOperator,
			Roles: []security.Role{security.RoleAdmin}, AMR: []string{"mfa"},
			AuthTime: now.Add(-time.Minute), SessionID: "s-" + uid,
		})
	}

	d := Decide(entitled(grantee), action, VerbApprove, now)
	assert.False(t, d.Allowed, "the console offered the grantee a live Approve button")
	assert.Equal(t, ReasonSelfApproval, d.Reason)

	// The positive control, and it is the point: a third principal with the
	// same entitlements must still be offered the button, or the affordance has
	// been broken rather than corrected.
	allowed := Decide(entitled(third), action, VerbApprove, now)
	assert.True(t, allowed.Allowed, "a distinct third principal must still approve: %+v", allowed)

	// A target that is not a user id changes nothing. That is the case every
	// fixture used to have, and the reason this went unseen for so long.
	notAUser := action
	notAUser.TargetID = "t-1"
	assert.True(t, Decide(entitled(third), notAUser, VerbApprove, now).Allowed)

	// And the rule is scoped to kinds that carry the flag: a target_id that
	// happens to be a user id must not block approval of a kind whose target is
	// not a beneficiary, or every action against a user account becomes
	// unapprovable by that user's own admin.
	other := action
	other.Kind = admin.KindCapabilityGateApprove
	s, known := admin.Spec(other.Kind)
	require.True(t, known)
	require.False(t, s.ApproverIsNotTarget)
	other.ExpiresAt = now.Add(s.Expiry)
	// gate:approve is dual-control, so this one needs a live elevation to get
	// past the permission check at all -- without it the case would pass on
	// MISSING_PERMISSION and prove nothing about the target rule.
	until := now.Add(time.Hour)
	elevated := NewActor(security.Principal{
		SubjectID: grantee, ActorType: security.ActorOperator,
		Roles:    []security.Role{security.RoleAdmin, security.RoleBreakGlass},
		AMR:      []string{"mfa"},
		AuthTime: now.Add(-time.Minute), SessionID: "s-elevated", BreakGlassUntil: &until,
	})
	leak := Decide(elevated, other, VerbApprove, now)
	assert.True(t, leak.Allowed, "the target rule leaked to a kind whose spec does not set it: %+v", leak)
}

// TestTheVectorsReachTheTargetBranch: the generated corpus must contain at
// least one action naming a user as its target on a kind that carries the flag.
// Without one the console port could lose its target check tomorrow and every
// vector would still agree.
func TestTheVectorsReachTheTargetBranch(t *testing.T) {
	const (
		proposer = "01a0754e-1111-7000-8000-000000000001"
		grantee  = "01a0754e-2222-7000-8000-000000000002"
	)
	reached := 0
	for name, v := range vectorActions(proposer, grantee) {
		spec, known := admin.Spec(v.Kind)
		if !known || !spec.ApproverIsNotTarget || v.TargetID == "" {
			continue
		}
		require.Equal(t, grantee, v.TargetID, "%s: the target must name a principal the corpus has", name)
		reached++
	}
	assert.Positive(t, reached,
		"no generated action names a user as its target, so the approver-is-not-target rule is "+
			"unreachable in every vector -- which is how the console came to have no target check at all")
}
