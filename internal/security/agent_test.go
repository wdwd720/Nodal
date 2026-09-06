package security

import (
	"errors"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"
)

func TestAgentPrincipal_Shape(t *testing.T) {
	p := AgentPrincipal("agent-1", "acct-1")
	if p.ActorType != ActorAgent || p.SubjectID != "agent-1" {
		t.Fatalf("unexpected principal %+v", p)
	}
	if len(p.Roles) != 0 || len(p.AccountIDs) != 1 || p.AccountIDs[0] != "acct-1" || p.BreakGlassUntil != nil {
		t.Fatalf("agent principal must have no roles and one account: %+v", p)
	}
	if !p.IsAgent() {
		t.Fatal("IsAgent false")
	}
	mustNil(t, p.Validate())
	for _, r := range AllRoles() {
		if p.HasRole(r) {
			t.Fatalf("agent reports role %s", r)
		}
	}
}

func TestAgent_ExactPermissionSet(t *testing.T) {
	ctx := ctxWith(AgentPrincipal("agent-1", "acct-1"))
	allowed := map[Permission]bool{}
	for _, p := range AgentPermissions() {
		allowed[p] = true
	}
	for _, perm := range AllPermissions() {
		err := RequireAt(ctx, perm, at(t0))
		if allowed[perm] {
			mustNil(t, err)
		} else {
			mustIs(t, err, ErrForbidden)
		}
	}
	for _, perm := range []Permission{
		PermKillActivate, PermKillRelease, PermWithdrawalCreate, PermWithdrawalApprove,
		PermGatePropose, PermGateApprove, PermLedgerPostCorrection, PermLedgerApproveCorrection,
		PermRiskPolicyWrite, PermProviderEnable, PermProviderDisable, PermAccountFreeze,
		PermAccountReadAny, PermSessionRevokeAny, PermBreakGlassRequest, PermTradeCreate,
		PermFundingCreate, PermStrategyWrite, PermAgentPause,
	} {
		mustIs(t, RequireAt(ctx, perm, at(t0)), ErrForbidden)
	}
}

func TestAgent_RolesNeverCount(t *testing.T) {
	p := AgentPrincipal("agent-1", "acct-1")
	p.Roles = []Role{RoleAdmin}
	ctx := ctxWith(p)
	// A tampered agent principal fails Validate and therefore holds nothing.
	for _, perm := range AllPermissions() {
		mustIs(t, RequireAt(ctx, perm, at(t0)), ErrForbidden)
	}
	// Even the raw evaluator ignores roles for agents.
	if p.has(PermKillActivate, true) || p.has(PermGateApprove, true) {
		t.Fatal("roles were consulted for an agent")
	}
}

func TestAgent_RequireAccountExactMatch(t *testing.T) {
	ctx := ctxWith(AgentPrincipal("agent-1", "acct-1"))
	mustNil(t, RequireAccount(ctx, "acct-1"))
	mustIs(t, RequireAccount(ctx, "acct-2"), ErrCrossTenant)
	mustIs(t, RequireAccount(ctx, "ACCT-1"), ErrCrossTenant)
	mustIs(t, RequireAccount(ctx, "acct-1 "), ErrCrossTenant)
	mustIs(t, RequireAccount(ctx, ""), ErrCrossTenant)
}

var forbiddenAgentPrefixes = []string{"kill:", "withdrawal:", "gate:", "ledger:", "risk:", "provider:", "break_glass:", "instrument:", "admin:"}

// genPermission mixes real permissions, real prefixes with random actions,
// and arbitrary strings.
func genPermission() *rapid.Generator[Permission] {
	real := make([]Permission, 0, 36)
	real = append(real, AllPermissions()...)
	prefixes := []string{
		"account", "trade", "funding", "withdrawal", "strategy", "agent", "prediction", "intent",
		"ledger", "reconciliation", "risk", "gate", "kill", "provider", "instrument", "admin", "session", "break_glass",
	}
	return rapid.OneOf(
		rapid.SampledFrom(real),
		rapid.Custom(func(t *rapid.T) Permission {
			return Permission(rapid.SampledFrom(prefixes).Draw(t, "prefix") + ":" + rapid.StringMatching(`[a-z_]{1,20}`).Draw(t, "action"))
		}),
		rapid.Custom(func(t *rapid.T) Permission { return Permission(rapid.String().Draw(t, "any")) }),
	)
}

// TestProp_AgentNeverGainsForbiddenPermissions: whatever permission string
// is asked for and however the principal is tampered with, an AGENT never
// satisfies anything outside AgentPermissions, and never anything under the
// kill/withdrawal/gate/ledger/risk/provider/admin/break_glass families.
func TestProp_AgentNeverGainsForbiddenPermissions(t *testing.T) {
	agentSet := map[Permission]bool{}
	for _, p := range AgentPermissions() {
		agentSet[p] = true
	}
	rapid.Check(t, func(rt *rapid.T) {
		p := AgentPrincipal(rapid.StringMatching(`agent-[a-z0-9]{1,8}`).Draw(rt, "agent"), rapid.StringMatching(`acct-[a-z0-9]{1,8}`).Draw(rt, "acct"))
		// Optional tampering.
		if rapid.Bool().Draw(rt, "tamperRoles") {
			p.Roles = rapid.SliceOfN(rapid.SampledFrom(AllRoles()), 1, 4).Draw(rt, "roles")
		}
		if rapid.Bool().Draw(rt, "tamperAccounts") {
			p.AccountIDs = rapid.SliceOfN(rapid.StringMatching(`acct-[a-z0-9]{1,8}`), 0, 3).Draw(rt, "accounts")
		}
		if rapid.Bool().Draw(rt, "tamperBreakGlass") {
			u := t0.Add(time.Hour)
			p.BreakGlassUntil = &u
		}
		p.AMR = rapid.SliceOfN(rapid.SampledFrom(append([]string{"pwd"}, StrongAMR...)), 0, 3).Draw(rt, "amr")
		perm := genPermission().Draw(rt, "perm")
		now := t0.Add(time.Duration(rapid.IntRange(-3600, 3600).Draw(rt, "skew")) * time.Second)

		granted := RequireAt(ctxWith(p), perm, at(now)) == nil
		if granted && !agentSet[perm] {
			rt.Fatalf("agent granted %q outside AgentPermissions", perm)
		}
		if granted && p.Validate() != nil {
			rt.Fatalf("tampered agent principal granted %q", perm)
		}
		for _, prefix := range forbiddenAgentPrefixes {
			if granted && strings.HasPrefix(string(perm), prefix) {
				rt.Fatalf("agent granted forbidden family permission %q", perm)
			}
		}
		if p.Has(perm, now) != granted {
			rt.Fatalf("Has and RequireAt disagree for %q", perm)
		}
	})
}

// TestProp_AgentNeverCrossesAccount: RequireAccount for an agent passes only
// for the single bound account.
func TestProp_AgentNeverCrossesAccount(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		bound := rapid.StringMatching(`[a-z0-9-]{1,16}`).Draw(rt, "bound")
		p := AgentPrincipal("agent", bound)
		target := rapid.OneOf(rapid.Just(bound), rapid.StringMatching(`[a-z0-9-]{0,16}`)).Draw(rt, "target")
		err := RequireAccount(ctxWith(p), target)
		if target == bound {
			if err != nil {
				rt.Fatalf("bound account refused: %v", err)
			}
			return
		}
		if !errors.Is(err, ErrCrossTenant) {
			rt.Fatalf("agent reached account %q (bound %q): %v", target, bound, err)
		}
	})
}

// TestProp_NoRoleGrantsDualControlOrAgentOnly: for any role combination and
// any time, a non-agent principal never holds agent-only permissions and
// never holds a dual-control permission without a live break-glass grant.
func TestProp_NoRoleGrantsDualControlOrAgentOnly(t *testing.T) {
	standing := []Role{RoleCustomer, RoleSupportReadOnly, RoleOperations, RoleRisk, RoleCompliance, RoleFinance, RoleSecurity, RoleAdmin}
	rapid.Check(t, func(rt *rapid.T) {
		p := Principal{
			SubjectID: "s",
			ActorType: rapid.SampledFrom([]ActorType{ActorUser, ActorOperator, ActorService, ActorSystem}).Draw(rt, "actor"),
			Roles:     rapid.SliceOfN(rapid.SampledFrom(standing), 0, 4).Draw(rt, "roles"),
		}
		withBreakGlass := rapid.Bool().Draw(rt, "bg")
		offset := time.Duration(rapid.IntRange(-3600, 3600).Draw(rt, "offset")) * time.Second
		if withBreakGlass {
			p.Roles = append(p.Roles, RoleBreakGlass)
			until := t0.Add(offset)
			p.BreakGlassUntil = &until
		}
		live := withBreakGlass && offset > 0
		ctx := ctxWith(p)
		for _, perm := range AllPermissions() {
			granted := RequireAt(ctx, perm, at(t0)) == nil
			if IsAgentOnly(perm) && granted {
				rt.Fatalf("roles %v granted agent-only %s", p.Roles, perm)
			}
			if IsDualControl(perm) && granted && !live {
				rt.Fatalf("roles %v granted dual-control %s without live break-glass", p.Roles, perm)
			}
			if IsDualControl(perm) && live && !granted {
				rt.Fatalf("live break-glass did not grant %s", perm)
			}
		}
	})
}
