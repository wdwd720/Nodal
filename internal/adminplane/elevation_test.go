package adminplane

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/security"
)

func TestElevationOf_States(t *testing.T) {
	t.Parallel()
	uid := user()
	past, future := now0.Add(-time.Minute), now0.Add(time.Hour)

	none := ElevationOf(operator(uid, security.RoleAdmin), now0)
	assert.Equal(t, ElevationNone, none.State)
	assert.Empty(t, none.Grants)
	assert.Equal(t, admin.MaxBreakGlassDuration, none.MaxDuration.Duration())

	active := ElevationOf(elevated(operator(uid, security.RoleAdmin), future), now0)
	assert.Equal(t, ElevationActive, active.State)
	assert.Equal(t, future, active.Until)
	assert.Equal(t, time.Hour, active.Remaining.Duration())
	assert.ElementsMatch(t, security.DualControlPermissions(), active.Grants,
		"a live elevation confers exactly the approve-side permissions")

	expired := ElevationOf(elevated(operator(uid, security.RoleAdmin), past), now0)
	assert.Equal(t, ElevationExpired, expired.State)
	assert.Empty(t, expired.Grants, "an expired elevation confers nothing")
	assert.Zero(t, expired.Remaining)

	// A deadline with no role, and a role with no deadline, are both incoherent.
	deadlineOnly := operator(uid, security.RoleAdmin)
	deadlineOnly.BreakGlassUntil = &future
	assert.Equal(t, ElevationIncoherent, ElevationOf(deadlineOnly, now0).State)

	roleOnly := operator(uid, security.RoleAdmin, security.RoleBreakGlass)
	assert.Equal(t, ElevationIncoherent, ElevationOf(roleOnly, now0).State)
	require.Error(t, roleOnly.Validate(), "security refuses this shape outright")
}

// TestExpiredElevationIsNeverHonored is the bypass: a session minted while the
// grant was live keeps carrying the BREAK_GLASS role after the deadline, and
// only the clock takes it away. Nothing must honor it after that instant.
func TestExpiredElevationIsNeverHonored(t *testing.T) {
	t.Parallel()
	proposer, approver := user(), user()
	until := now0.Add(time.Hour)
	p := elevated(operator(approver, security.RoleAdmin), until)

	for _, kind := range admin.Kinds() {
		if !ElevationRequired(kind) {
			continue
		}
		action := proposal(kind, proposer)
		spec, _ := admin.Spec(kind)
		// One instant before the deadline: allowed.
		before := until.Add(-time.Second)
		p.AuthTime = before.Add(-time.Minute)
		if before.Before(action.ExpiresAt) && spec.StepUpMaxAge >= time.Minute {
			assert.True(t, Decide(NewActor(p), action, VerbApprove, before).Allowed, "%s just before expiry", kind)
		}
		// At the deadline and after it: the approve permission is simply gone.
		for _, at := range []time.Time{until, until.Add(time.Second)} {
			p.AuthTime = at.Add(-time.Minute)
			d := Decide(NewActor(p), action, VerbApprove, at)
			assert.False(t, d.Allowed, "%s at %s", kind, at)
			assert.Equal(t, ReasonMissingPermission, d.Reason, kind)
			assert.Equal(t, ElevationExpired, ElevationOf(p, at).State)
		}
	}
}

// TestAGrantForSomeoneElseNeverElevatesYou: an operator who obtains the
// execution result of another operator's BREAK_GLASS_GRANT gains nothing.
func TestAGrantForSomeoneElseNeverElevatesYou(t *testing.T) {
	t.Parallel()
	mine, theirs := user(), user()
	grant := admin.Grant{ActionID: admin.NewActionID().String(), UserID: theirs, Scope: "incident-1", ExpiresAt: now0.Add(time.Hour)}

	me := operator(mine, security.RoleAdmin)
	got := admin.PrincipalWithBreakGlass(me, grant)
	assert.False(t, got.HasRole(security.RoleBreakGlass))
	assert.Equal(t, ElevationNone, ElevationOf(got, now0).State)

	action := proposal(admin.KindKillSwitchRelease, user())
	d := Decide(NewActor(got), action, VerbApprove, now0)
	assert.False(t, d.Allowed)
	assert.Equal(t, ReasonMissingPermission, d.Reason)

	// The grantee does get it.
	them := operator(theirs, security.RoleAdmin)
	theirsElevated := admin.PrincipalWithBreakGlass(them, grant)
	assert.Equal(t, ElevationActive, ElevationOf(theirsElevated, now0).State)
	assert.True(t, Decide(NewActor(theirsElevated), action, VerbApprove, now0).Allowed)
}

// TestAnAgentIsNeverElevated: PrincipalWithBreakGlass refuses to elevate an
// AGENT even when the grant names its subject.
func TestAnAgentIsNeverElevated(t *testing.T) {
	t.Parallel()
	uid := user()
	agent := security.AgentPrincipal(uid, "acct-1")
	grant := admin.Grant{ActionID: admin.NewActionID().String(), UserID: uid, Scope: "incident-1", ExpiresAt: now0.Add(time.Hour)}
	got := admin.PrincipalWithBreakGlass(agent, grant)
	assert.False(t, got.HasRole(security.RoleBreakGlass))
	assert.Nil(t, got.BreakGlassUntil)
	assert.Equal(t, ElevationNone, ElevationOf(got, now0).State)
	assert.Equal(t, ReasonAgentPrincipal,
		Decide(NewActor(got), proposal(admin.KindKillSwitchRelease, user()), VerbApprove, now0).Reason)
}

// TestElevationRequiredMatchesTheMatrix: a kind needs an elevation to approve
// exactly when no standing role holds its approve permission.
func TestElevationRequiredMatchesTheMatrix(t *testing.T) {
	t.Parallel()
	for _, kind := range admin.Kinds() {
		spec, ok := admin.Spec(kind)
		require.True(t, ok)
		standingHolder := false
		for _, r := range security.AllRoles() {
			if r != security.RoleBreakGlass && spec.ApprovePermission != "" && security.RoleGrants(r, spec.ApprovePermission) {
				standingHolder = true
			}
		}
		want := spec.RequiresDual && spec.ApprovePermission != "" && !standingHolder
		assert.Equal(t, want, ElevationRequired(kind), kind)
	}
	assert.False(t, ElevationRequired(admin.Kind("NOPE")))
}

// TestGrantDurationIsBounded: the console must never offer an elevation longer
// than the domain will mint.
func TestGrantDurationIsBounded(t *testing.T) {
	t.Parallel()
	e := ElevationOf(operator(user(), security.RoleAdmin), now0)
	assert.Equal(t, admin.MaxBreakGlassDuration, e.MaxDuration.Duration())
	assert.Positive(t, e.MaxDuration.Int())
}
