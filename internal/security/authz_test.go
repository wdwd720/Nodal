package security

import (
	"context"
	"errors"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

func at(t time.Time) func() time.Time { return func() time.Time { return t } }

func customer(sub string, accounts ...string) Principal {
	return Principal{SubjectID: sub, ActorType: ActorUser, Roles: []Role{RoleCustomer}, AccountIDs: accounts}
}

func operator(sub string, roles ...Role) Principal {
	return Principal{SubjectID: sub, ActorType: ActorOperator, Roles: roles}
}

func ctxWith(p Principal) context.Context { return WithPrincipal(context.Background(), p) }

func mustIs(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("got error %v, want errors.Is(%v)", err, target)
	}
}

func mustNil(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRequire_Anonymous(t *testing.T) {
	ctx := context.Background()
	mustIs(t, Require(ctx, PermAccountRead), ErrUnauthenticated)
	mustIs(t, RequireAny(ctx, PermAccountRead), ErrUnauthenticated)
	mustIs(t, RequireAccount(ctx, "acct-1"), ErrUnauthenticated)
	mustIs(t, RequireStepUp(ctx, time.Minute, at(t0)), ErrUnauthenticated)
	mustIs(t, RequireDualControl(ctx, PermGateApprove, "someone", at(t0)), ErrUnauthenticated)
	if _, ok := PrincipalFrom(ctx); ok {
		t.Fatal("PrincipalFrom on empty context reported a principal")
	}
}

func TestRequire_InvalidPrincipalFailsClosed(t *testing.T) {
	cases := map[string]Principal{
		"empty subject":              {ActorType: ActorUser, Roles: []Role{RoleCustomer}},
		"unknown actor":              {SubjectID: "s", ActorType: "ROBOT", Roles: []Role{RoleAdmin}},
		"unknown role":               {SubjectID: "s", ActorType: ActorOperator, Roles: []Role{"ROOT"}},
		"empty account id":           {SubjectID: "s", ActorType: ActorUser, Roles: []Role{RoleCustomer}, AccountIDs: []string{""}},
		"break glass without expiry": {SubjectID: "s", ActorType: ActorOperator, Roles: []Role{RoleAdmin, RoleBreakGlass}},
		"agent with roles":           {SubjectID: "agent", ActorType: ActorAgent, Roles: []Role{RoleAdmin}, AccountIDs: []string{"a"}},
		"agent with two accounts":    {SubjectID: "agent", ActorType: ActorAgent, AccountIDs: []string{"a", "b"}},
		"agent with no account":      {SubjectID: "agent", ActorType: ActorAgent},
		"agent with break glass": func() Principal {
			p := AgentPrincipal("agent", "a")
			u := t0.Add(time.Hour)
			p.BreakGlassUntil = &u
			return p
		}(),
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			if p.Validate() == nil {
				t.Fatal("Validate accepted an invalid principal")
			}
			ctx := ctxWith(p)
			for _, perm := range AllPermissions() {
				err := RequireAt(ctx, perm, at(t0))
				mustIs(t, err, ErrForbidden)
				if errors.Is(err, ErrUnauthenticated) {
					t.Fatalf("invalid principal must be FORBIDDEN, not UNAUTHENTICATED")
				}
			}
			if len(p.Permissions(t0)) != 0 {
				t.Fatal("invalid principal must hold no permissions")
			}
			mustIs(t, RequireAccount(ctx, "a"), ErrForbidden)
		})
	}
}

func TestRequireAny(t *testing.T) {
	ctx := ctxWith(operator("ops", RoleOperations))
	mustNil(t, RequireAny(ctx, PermKillRelease, PermKillActivate))
	mustNil(t, RequireAnyAt(ctx, at(t0), PermAccountRead))
	mustIs(t, RequireAny(ctx, PermKillRelease, PermGateApprove), ErrForbidden)
	mustIs(t, RequireAny(ctx), ErrForbidden)
}

func TestRequireAccount_TenantIsolation(t *testing.T) {
	cases := []struct {
		name    string
		p       Principal
		account string
		want    error // nil means allowed
	}{
		{"customer owns account", customer("A", "acct-A1", "acct-A2"), "acct-A2", nil},
		{"customer A cannot read customer B", customer("A", "acct-A1"), "acct-B1", ErrCrossTenant},
		{"customer with no accounts", customer("A"), "acct-A1", ErrCrossTenant},
		{"empty account id refused", customer("A", "acct-A1"), "", ErrCrossTenant},
		{"user without roles cannot cross", Principal{SubjectID: "u", ActorType: ActorUser}, "acct-B1", ErrCrossTenant},
		{"support read-only sees any", operator("sup", RoleSupportReadOnly), "acct-B1", nil},
		{"operations sees any", operator("ops", RoleOperations), "acct-B1", nil},
		{"risk sees any", operator("risk", RoleRisk), "acct-B1", nil},
		{"compliance sees any", operator("comp", RoleCompliance), "acct-B1", nil},
		{"finance sees any", operator("fin", RoleFinance), "acct-B1", nil},
		{"security sees any", operator("sec", RoleSecurity), "acct-B1", nil},
		{"admin sees any", operator("adm", RoleAdmin), "acct-B1", nil},
		{"operator with no roles cannot cross", operator("nobody"), "acct-B1", ErrCrossTenant},
		{"service with no roles cannot cross", Principal{SubjectID: "svc", ActorType: ActorService}, "acct-B1", ErrCrossTenant},
		{"agent bound account", AgentPrincipal("agent-1", "acct-A1"), "acct-A1", nil},
		{"agent other account", AgentPrincipal("agent-1", "acct-A1"), "acct-B1", ErrCrossTenant},
		{"agent empty account", AgentPrincipal("agent-1", "acct-A1"), "", ErrCrossTenant},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := RequireAccount(ctxWith(tc.p), tc.account)
			if tc.want == nil {
				mustNil(t, err)
				return
			}
			mustIs(t, err, tc.want)
			mustIs(t, err, ErrForbidden) // cross-tenant is a kind of forbidden
		})
	}
}

func TestRequireAccount_BreakGlassNeverGrantsVisibility(t *testing.T) {
	until := t0.Add(time.Hour)
	p := Principal{SubjectID: "bg", ActorType: ActorOperator, Roles: []Role{RoleBreakGlass}, BreakGlassUntil: &until}
	mustIs(t, RequireAccount(ctxWith(p), "acct-B1"), ErrCrossTenant)
	mustIs(t, RequireAt(ctxWith(p), PermAccountRead, at(t0)), ErrForbidden)
}

func TestRequireStepUp(t *testing.T) {
	base := customer("A", "acct-A1")
	withAuth := func(age time.Duration, amr ...string) Principal {
		p := base
		p.AuthTime = t0.Add(-age)
		p.AMR = amr
		return p
	}
	cases := []struct {
		name   string
		p      Principal
		maxAge time.Duration
		ok     bool
	}{
		{"fresh mfa", withAuth(time.Minute, "pwd", "mfa"), 5 * time.Minute, true},
		{"fresh password only", withAuth(time.Minute, "pwd"), 5 * time.Minute, false},
		{"fresh sms only", withAuth(time.Minute, "pwd", "sms"), 5 * time.Minute, false},
		{"fresh but no amr", withAuth(time.Minute), 5 * time.Minute, false},
		{"stale mfa", withAuth(6*time.Minute, "mfa"), 5 * time.Minute, false},
		{"exactly max age", withAuth(5*time.Minute, "mfa"), 5 * time.Minute, true},
		{"one nanosecond over", withAuth(5*time.Minute+time.Nanosecond, "mfa"), 5 * time.Minute, false},
		{"future auth time treated as now", withAuth(-time.Minute, "mfa"), 5 * time.Minute, true},
		{"zero auth time", func() Principal { p := base; p.AMR = []string{"mfa"}; return p }(), 5 * time.Minute, false},
		{"zero max age fails closed", withAuth(0, "mfa"), 0, false},
		{"negative max age fails closed", withAuth(0, "mfa"), -time.Second, false},
		{"case-insensitive amr", withAuth(time.Minute, "MFA"), 5 * time.Minute, true},
		{"agent can never step up", func() Principal { p := AgentPrincipal("ag", "acct"); p.AuthTime = t0; return p }(), time.Hour, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := RequireStepUp(ctxWith(tc.p), tc.maxAge, at(t0))
			if tc.ok {
				mustNil(t, err)
			} else {
				mustIs(t, err, ErrStepUpRequired)
			}
		})
	}
	for _, strong := range StrongAMR {
		t.Run("strong "+strong, func(t *testing.T) {
			mustNil(t, RequireStepUp(ctxWith(withAuth(time.Second, strong)), time.Minute, at(t0)))
		})
	}
}

func TestHasStrongAMR(t *testing.T) {
	if HasStrongAMR(nil) || HasStrongAMR([]string{}) || HasStrongAMR([]string{"pwd", "sms", "kba"}) {
		t.Fatal("weak methods reported strong")
	}
	if !HasStrongAMR([]string{"pwd", "otp"}) || !HasStrongAMR([]string{"WebAuthn"}) {
		t.Fatal("strong methods not recognized")
	}
}

func TestBreakGlass_Expiry(t *testing.T) {
	until := t0.Add(10 * time.Minute)
	p := operator("adm", RoleAdmin, RoleBreakGlass)
	p.BreakGlassUntil = &until
	ctx := ctxWith(p)

	for _, perm := range DualControlPermissions() {
		mustNil(t, RequireAt(ctx, perm, at(t0)))
		mustNil(t, RequireAt(ctx, perm, at(until.Add(-time.Nanosecond))))
		mustIs(t, RequireAt(ctx, perm, at(until)), ErrForbidden)
		mustIs(t, RequireAt(ctx, perm, at(until.Add(time.Hour))), ErrForbidden)
	}
	// Standing ADMIN permissions are unaffected by expiry.
	mustNil(t, RequireAt(ctx, PermKillActivate, at(until.Add(time.Hour))))
	mustNil(t, RequireAt(ctx, PermGatePropose, at(until.Add(time.Hour))))

	if !p.BreakGlassActive(t0) || p.BreakGlassActive(until) {
		t.Fatal("BreakGlassActive boundary wrong")
	}
	live := p.Permissions(t0)
	expired := p.Permissions(until)
	if len(live) != len(expired)+len(DualControlPermissions()) {
		t.Fatalf("live %d permissions, expired %d, dual-control %d", len(live), len(expired), len(DualControlPermissions()))
	}
}

func TestBreakGlass_WallClockWrapper(t *testing.T) {
	past := time.Now().UTC().Add(-time.Minute)
	future := time.Now().UTC().Add(time.Hour)
	expired := operator("adm", RoleAdmin, RoleBreakGlass)
	expired.BreakGlassUntil = &past
	live := operator("adm", RoleAdmin, RoleBreakGlass)
	live.BreakGlassUntil = &future
	mustIs(t, Require(ctxWith(expired), PermGateApprove), ErrForbidden)
	mustNil(t, Require(ctxWith(live), PermGateApprove))
	mustNil(t, RequireAny(ctxWith(live), PermGateApprove))
	mustIs(t, RequireAny(ctxWith(expired), PermGateApprove), ErrForbidden)
}

func TestRequireDualControl(t *testing.T) {
	until := t0.Add(10 * time.Minute)
	approver := operator("approver", RoleAdmin, RoleBreakGlass)
	approver.BreakGlassUntil = &until
	ctx := ctxWith(approver)

	mustNil(t, RequireDualControl(ctx, PermWithdrawalApprove, "proposer", at(t0)))
	mustIs(t, RequireDualControl(ctx, PermWithdrawalApprove, "approver", at(t0)), ErrSelfApproval)
	mustIs(t, RequireDualControl(ctx, PermWithdrawalApprove, "approver", at(t0)), ErrForbidden)
	mustIs(t, RequireDualControl(ctx, PermWithdrawalApprove, "", at(t0)), ErrSelfApproval)
	mustIs(t, RequireDualControl(ctx, PermWithdrawalApprove, "proposer", at(until)), ErrForbidden)
	mustIs(t, RequireDualControl(ctx, PermKillActivate, "proposer", at(t0)), ErrForbidden)

	plainAdmin := ctxWith(operator("adm", RoleAdmin))
	mustIs(t, RequireDualControl(plainAdmin, PermGateApprove, "proposer", at(t0)), ErrForbidden)
}

func TestContext_PrincipalIsolation(t *testing.T) {
	p := customer("A", "acct-A1")
	p.AMR = []string{"mfa"}
	ctx := WithPrincipal(context.Background(), p)
	p.Roles[0] = RoleAdmin
	p.AccountIDs[0] = "acct-B1"
	p.AMR[0] = "pwd"

	got, ok := PrincipalFrom(ctx)
	if !ok {
		t.Fatal("principal missing")
	}
	if got.Roles[0] != RoleCustomer || got.AccountIDs[0] != "acct-A1" || got.AMR[0] != "mfa" {
		t.Fatalf("context principal aliased caller slices: %+v", got)
	}
	got.Roles[0] = RoleAdmin
	again, _ := PrincipalFrom(ctx)
	if again.Roles[0] != RoleCustomer {
		t.Fatal("PrincipalFrom returned aliased slices")
	}
}

func TestErrors_Chain(t *testing.T) {
	mustIs(t, ErrCrossTenant, ErrForbidden)
	mustIs(t, ErrSelfApproval, ErrForbidden)
	if errors.Is(ErrStepUpRequired, ErrForbidden) || errors.Is(ErrUnauthenticated, ErrForbidden) {
		t.Fatal("step-up and unauthenticated must map to their own codes")
	}
}
