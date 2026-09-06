package adminplane

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/security"
)

var now0 = time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

// user returns a canonical users.id string.
func user() string { return id.New[id.Any]().String() }

// operator builds a strongly authenticated OPERATOR principal.
func operator(uid string, roles ...security.Role) security.Principal {
	return security.Principal{
		SubjectID: uid, ActorType: security.ActorOperator, Roles: roles,
		SessionID: "sess", AuthTime: now0.Add(-time.Minute), AMR: []string{"mfa"},
	}
}

// elevated adds a live BREAK_GLASS elevation to p.
func elevated(p security.Principal, until time.Time) security.Principal {
	p.Roles = append(append([]security.Role(nil), p.Roles...), security.RoleBreakGlass)
	u := until
	p.BreakGlassUntil = &u
	return p
}

// requiresDual reads the kind table rather than restating it.
func requiresDual(t *testing.T, k admin.Kind) bool {
	t.Helper()
	spec, ok := admin.Spec(k)
	require.True(t, ok, k)
	return spec.RequiresDual
}

// proposal builds a PROPOSED action of kind proposed by proposedBy.
func proposal(kind admin.Kind, proposedBy string) admin.Action {
	spec, _ := admin.Spec(kind)
	return admin.Action{
		ID: admin.NewActionID(), Kind: kind, TargetType: "target", TargetID: "t-1",
		Reason: "an eight-plus character reason", RequiresDual: spec.RequiresDual,
		Status: admin.StatusProposed, ProposedBy: proposedBy, ProposedAt: now0,
		ExpiresAt: now0.Add(spec.Expiry), UpdatedAt: now0,
	}
}

// approvedBy returns a copy of a moved to APPROVED by uid.
func approvedBy(a admin.Action, uid string) admin.Action {
	t := now0
	a.Status = admin.StatusApproved
	a.ApprovedBy = &uid
	a.ApprovedAt = &t
	return a
}

func TestDecideProposal_FollowsTheKindTable(t *testing.T) {
	t.Parallel()
	uid := user()
	for _, kind := range admin.Kinds() {
		spec, ok := admin.Spec(kind)
		require.True(t, ok)
		for _, role := range security.AllRoles() {
			if role == security.RoleBreakGlass {
				continue // never a standing role; covered separately
			}
			a := NewActor(operator(uid, role))
			d := DecideProposal(a, kind, now0)
			want := security.RoleGrants(role, spec.ProposePermission)
			assert.Equal(t, want, d.Allowed, "%s may propose %s", role, kind)
			assert.Equal(t, spec.ProposePermission, d.Permission, "%s/%s names the permission it needs", role, kind)
			assert.Equal(t, spec.StepUpMaxAge, d.StepUpMaxAge.Duration(), "%s/%s step-up window", role, kind)
			if !want {
				assert.Equal(t, ReasonMissingPermission, d.Reason)
				assert.Equal(t, errs.CodeForbidden, d.Code)
			}
		}
	}
}

func TestDecideProposal_UnknownKindIsRefused(t *testing.T) {
	t.Parallel()
	a := NewActor(operator(user(), security.RoleAdmin))
	d := DecideProposal(a, admin.Kind("BALANCE_EDIT"), now0)
	assert.False(t, d.Allowed)
	assert.Equal(t, ReasonUnknownKind, d.Reason)
	assert.Equal(t, errs.CodeValidationFailed, d.Code)
}

// TestNoStandingRoleCanApprove is the dual-control property expressed as an
// affordance: for every kind whose approve side is a dual-control permission,
// no standing role — ADMIN included — is ever offered the approve button.
func TestNoStandingRoleCanApprove(t *testing.T) {
	t.Parallel()
	proposer, other := user(), user()
	for _, kind := range admin.Kinds() {
		if !ElevationRequired(kind) {
			continue
		}
		action := proposal(kind, proposer)
		for _, role := range security.AllRoles() {
			if role == security.RoleBreakGlass {
				continue
			}
			d := Decide(NewActor(operator(other, role)), action, VerbApprove, now0)
			assert.False(t, d.Allowed, "standing role %s was offered approve on %s", role, kind)
			assert.Equal(t, ReasonMissingPermission, d.Reason, "%s/%s", role, kind)
			assert.Equal(t, errs.CodeForbidden, d.Code)
		}
		// The same principal, elevated, is offered it.
		d := Decide(NewActor(elevated(operator(other, security.RoleAdmin), now0.Add(time.Hour))), action, VerbApprove, now0)
		assert.True(t, d.Allowed, "an elevated distinct principal may approve %s: %+v", kind, d)
	}
}

// TestProposerIsNeverOfferedApproval is the bypass a careless operator would
// otherwise reach: hold the elevation, then approve your own proposal.
func TestProposerIsNeverOfferedApproval(t *testing.T) {
	t.Parallel()
	proposer := user()
	for _, kind := range admin.Kinds() {
		if !requiresDual(t, kind) {
			continue
		}
		action := proposal(kind, proposer)
		// Every elevation the proposer could possibly hold.
		p := elevated(operator(proposer, security.RoleAdmin, security.RoleSecurity), now0.Add(time.Hour))
		d := Decide(NewActor(p), action, VerbApprove, now0)
		assert.False(t, d.Allowed, "the proposer was offered approve on %s", kind)
		assert.Equal(t, ReasonSelfApproval, d.Reason, kind)
		assert.Equal(t, errs.CodeForbidden, d.Code)

		// And they are never offered execute on their own unapproved action.
		e := Decide(NewActor(p), action, VerbExecute, now0)
		assert.False(t, e.Allowed, "the proposer was offered execute on unapproved %s", kind)
		assert.Equal(t, ReasonAwaitingApproval, e.Reason, kind)
	}
}

// TestExecuteRefusesASelfApprovedRecord covers the shape where the stored row
// itself is wrong: APPROVED, but approved_by equals proposed_by. The database
// CHECK makes this unreachable; the affordance refuses it anyway, because a
// console that trusts the row is a console that can be fed a bad row.
func TestExecuteRefusesASelfApprovedRecord(t *testing.T) {
	t.Parallel()
	proposer := user()
	for _, kind := range admin.Kinds() {
		if !requiresDual(t, kind) {
			continue
		}
		action := approvedBy(proposal(kind, proposer), proposer)
		d := Decide(NewActor(operator(user(), security.RoleAdmin)), action, VerbExecute, now0)
		assert.False(t, d.Allowed, kind)
		assert.Equal(t, ReasonApproverNotDistinct, d.Reason, kind)
		assert.Equal(t, errs.CodeForbidden, d.Code)
	}
}

func TestAgentIsRefusedEveryVerbOnEveryKind(t *testing.T) {
	t.Parallel()
	agent := NewActor(security.AgentPrincipal("agent-1", "acct-1"))
	for _, kind := range admin.Kinds() {
		action := proposal(kind, user())
		for _, v := range Verbs() {
			d := Decide(agent, action, v, now0)
			assert.False(t, d.Allowed, "%s/%s", kind, v)
			assert.Equal(t, ReasonAgentPrincipal, d.Reason, "%s/%s", kind, v)
			assert.Equal(t, errs.CodeForbidden, d.Code)
		}
		assert.Empty(t, VisibleSurfaces(agent, now0), "an agent sees an operator surface")
	}
	// A tampered agent carrying roles holds nothing either.
	p := security.AgentPrincipal("agent-1", "acct-1")
	p.Roles = []security.Role{security.RoleAdmin}
	d := Decide(NewActor(p), proposal(admin.KindKillSwitchRelease, user()), VerbApprove, now0)
	assert.Equal(t, ReasonAgentPrincipal, d.Reason)
}

func TestAnonymousIsRefusedEveryVerb(t *testing.T) {
	t.Parallel()
	var anon Actor
	require.True(t, anon.Anonymous())
	for _, v := range Verbs() {
		d := Decide(anon, proposal(admin.KindLedgerCorrection, user()), v, now0)
		assert.False(t, d.Allowed, v)
		assert.Equal(t, ReasonUnauthenticated, d.Reason, v)
		assert.Equal(t, errs.CodeUnauthenticated, d.Code, v)
	}
	assert.Empty(t, VisibleSurfaces(anon, now0))
	assert.Empty(t, AllowedWrites(anon, SurfaceKillSwitches, now0))
}

// TestReasonOrderMatchesEnforcement pins the order in which a principal that
// fails several checks is told why. A console that reports "your session is
// stale" to someone who will still be refused for self-approval sends them to
// re-authenticate for nothing.
func TestReasonOrderMatchesEnforcement(t *testing.T) {
	t.Parallel()
	proposer := user()
	kind := admin.KindKillSwitchRelease
	spec, _ := admin.Spec(kind)
	action := proposal(kind, proposer)

	// Missing permission outranks a stale step-up.
	stale := operator(proposer, security.RoleSupportReadOnly)
	stale.AuthTime = now0.Add(-24 * time.Hour)
	assert.Equal(t, ReasonMissingPermission, Decide(NewActor(stale), action, VerbApprove, now0).Reason)

	// A stale step-up outranks self-approval.
	staleElevated := elevated(operator(proposer, security.RoleAdmin), now0.Add(time.Hour))
	staleElevated.AuthTime = now0.Add(-spec.StepUpMaxAge - time.Minute)
	d := Decide(NewActor(staleElevated), action, VerbApprove, now0)
	assert.Equal(t, ReasonStepUpRequired, d.Reason)
	assert.Equal(t, errs.CodeStepUpRequired, d.Code)

	// Self-approval outranks expiry. The elevation deliberately outlives the
	// action here: an elevation that died first would report the missing
	// permission instead, which is true but is not the point being pinned.
	after := action.ExpiresAt.Add(time.Second)
	freshElevated := elevated(operator(proposer, security.RoleAdmin), after.Add(time.Hour))
	freshElevated.AuthTime = action.ExpiresAt
	assert.Equal(t, ReasonSelfApproval, Decide(NewActor(freshElevated), action, VerbApprove, after).Reason)

	// With a distinct principal, expiry is what remains.
	distinct := elevated(operator(user(), security.RoleAdmin), after.Add(time.Hour))
	distinct.AuthTime = action.ExpiresAt
	assert.Equal(t, ReasonExpired, Decide(NewActor(distinct), action, VerbApprove, after).Reason)
}

// TestStepUpIsNotDemandedWhereTheDomainDoesNotDemandIt: refusing a proposal and
// withdrawing your own must never wait on a second factor.
func TestStepUpIsNotDemandedWhereTheDomainDoesNotDemandIt(t *testing.T) {
	t.Parallel()
	proposer := user()
	action := proposal(admin.KindLedgerCorrection, proposer)
	weak := operator(proposer, security.RoleFinance)
	weak.AuthTime = now0.Add(-30 * 24 * time.Hour)
	weak.AMR = []string{"pwd"}

	reject := Decide(NewActor(weak), action, VerbReject, now0)
	assert.True(t, reject.Allowed, "%+v", reject)
	assert.Zero(t, reject.StepUpMaxAge)

	cancel := Decide(NewActor(weak), action, VerbCancel, now0)
	assert.True(t, cancel.Allowed, "%+v", cancel)

	// But executing does demand one.
	exec := Decide(NewActor(weak), approvedBy(action, user()), VerbExecute, now0)
	assert.False(t, exec.Allowed)
	assert.Equal(t, ReasonStepUpRequired, exec.Reason)
}

func TestCancelBelongsToTheProposerAlone(t *testing.T) {
	t.Parallel()
	proposer, other := user(), user()
	action := proposal(admin.KindAccountUnfreeze, proposer)
	assert.True(t, Decide(NewActor(operator(proposer, security.RoleCompliance)), action, VerbCancel, now0).Allowed)
	d := Decide(NewActor(elevated(operator(other, security.RoleAdmin), now0.Add(time.Hour))), action, VerbCancel, now0)
	assert.False(t, d.Allowed)
	assert.Equal(t, ReasonNotProposer, d.Reason)
	assert.Equal(t, errs.CodeForbidden, d.Code)
}

func TestSingleControlKindTakesNoApproval(t *testing.T) {
	t.Parallel()
	var single []admin.Kind
	for _, k := range admin.Kinds() {
		if !requiresDual(t, k) {
			single = append(single, k)
		}
	}
	require.NotEmpty(t, single, "the table has at least one single-control kind")
	for _, k := range single {
		action := proposal(k, user())
		d := Decide(NewActor(elevated(operator(user(), security.RoleAdmin), now0.Add(time.Hour))), action, VerbApprove, now0)
		assert.False(t, d.Allowed, k)
		assert.Equal(t, ReasonKindTakesNoApproval, d.Reason, k)
		assert.Equal(t, errs.CodeInvalidStateTransition, d.Code, k)
		// It executes straight from PROPOSED for a holder of its permission.
		spec, _ := admin.Spec(k)
		var holder security.Role
		for _, r := range security.AllRoles() {
			if r != security.RoleBreakGlass && security.RoleGrants(r, spec.ProposePermission) {
				holder = r
				break
			}
		}
		require.NotEmpty(t, holder, "some standing role can act on %s", k)
		assert.True(t, Decide(NewActor(operator(user(), holder)), action, VerbExecute, now0).Allowed, k)
	}
}

func TestTerminalStatusesOfferNothing(t *testing.T) {
	t.Parallel()
	proposer := user()
	actor := NewActor(elevated(operator(proposer, security.RoleAdmin), now0.Add(time.Hour)))
	for _, st := range admin.AllStatuses() {
		if !st.Terminal() {
			continue
		}
		action := proposal(admin.KindLedgerCorrection, proposer)
		action.Status = st
		for _, d := range DecideAll(actor, action, now0) {
			assert.False(t, d.Allowed, "%s offered %s", st, d.Verb)
		}
	}
}

func TestSubjectMustBeAUserID(t *testing.T) {
	t.Parallel()
	p := operator("not-a-uuid", security.RoleAdmin)
	a := NewActor(p)
	assert.Empty(t, a.UserID)
	d := DecideProposal(a, admin.KindAccountUnfreeze, now0)
	assert.False(t, d.Allowed)
	assert.Equal(t, ReasonSubjectNotUser, d.Reason)
	assert.Equal(t, errs.CodeForbidden, d.Code)
}

func TestDecideAllCoversEveryVerbButPropose(t *testing.T) {
	t.Parallel()
	got := DecideAll(NewActor(operator(user(), security.RoleAdmin)), proposal(admin.KindAgentPromote, user()), now0)
	require.Len(t, got, len(Verbs())-1)
	for _, d := range got {
		assert.NotEqual(t, VerbPropose, d.Verb)
		assert.NotEmpty(t, d.Reason, "every decision explains itself")
	}
}
