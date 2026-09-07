package security

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"
)

// goldenPermissions is the closed list of permissions. It uses raw strings
// on purpose: renaming a constant must not silently move a cell.
var goldenPermissions = []string{
	"account:freeze", "account:read", "account:read_any",
	"admin:audit_read",
	"credit:adjust", "credit:purchase", "credit:read",
	"agent:pause", "agent:promote", "agent:promote_approve", "agent:run",
	"break_glass:approve", "break_glass:request",
	"envelope:approve", "envelope:authority_write",
	"funding:create", "funding:read",
	"gate:approve", "gate:propose", "gate:read",
	"instrument:status_write",
	"intent:create_agent",
	"kill:activate", "kill:release",
	"ledger:approve_correction", "ledger:post_correction", "ledger:read",
	"native_asset:create", "native_asset:moderate", "native_asset:read",
	"native_market:halt", "native_market:surveil", "native_market:trade",
	"payout:approve", "payout:create", "payout:read", "payout:review",
	"prediction:commit",
	"provider:disable", "provider:enable",
	"reconciliation:approve", "reconciliation:read", "reconciliation:resolve",
	"risk:policy_write", "risk:read",
	"session:list_own", "session:revoke_any", "session:revoke_own",
	"strategy:read", "strategy:write",
	"trade:create", "trade:read",
	"withdrawal:approve", "withdrawal:create", "withdrawal:review",
}

// goldenMatrix is the reviewed role→permission matrix. Any change to
// RolePermissions that is not mirrored here — in particular any privilege
// expansion — fails TestGoldenMatrix_Equal. Edit deliberately, with review.
var goldenMatrix = map[string][]string{
	// A customer may hold Credits, create an internal asset, trade an internal
	// market and ask for a payout. Holding the permission means only that this
	// is the role that may ASK; whether any of it is permitted today is a
	// capability gate and legal-router question, checked independently.
	"CUSTOMER": {
		"account:read", "agent:pause",
		"credit:purchase", "credit:read",
		"funding:create", "funding:read",
		"native_asset:create", "native_asset:read", "native_market:trade",
		"payout:create", "payout:read",
		"session:list_own", "session:revoke_own",
		"strategy:read", "strategy:write", "trade:create", "trade:read", "withdrawal:create",
	},
	"SUPPORT_READ_ONLY": {
		"account:read", "account:read_any", "credit:read", "funding:read", "gate:read", "ledger:read",
		"native_asset:read", "payout:read",
		"reconciliation:read", "risk:read", "session:list_own", "session:revoke_own",
		"strategy:read", "trade:read",
	},
	"OPERATIONS": {
		"account:read", "account:read_any", "agent:pause", "agent:promote",
		"credit:read", "funding:read", "gate:read",
		"instrument:status_write", "kill:activate", "ledger:read",
		"native_asset:read", "native_market:halt", "native_market:surveil",
		"payout:read", "payout:review", "provider:disable",
		"reconciliation:read", "reconciliation:resolve", "risk:read",
		"session:list_own", "session:revoke_own", "strategy:read", "trade:read",
	},
	"RISK": {
		"account:read", "account:read_any", "agent:promote", "credit:read", "envelope:authority_write",
		"funding:read", "gate:propose", "gate:read",
		"instrument:status_write", "kill:activate", "ledger:read",
		"native_asset:read", "payout:read", "reconciliation:read",
		"risk:policy_write", "risk:read", "session:list_own", "session:revoke_own",
		"strategy:read", "trade:read",
	},
	// Compliance is the role that judges user-generated content and market
	// conduct, so it holds the moderation and surveillance permissions and the
	// halt that follows from them.
	"COMPLIANCE": {
		"account:freeze", "account:read", "account:read_any", "credit:read", "funding:read", "gate:propose",
		"gate:read", "ledger:read",
		"native_asset:moderate", "native_asset:read", "native_market:halt", "native_market:surveil",
		"payout:read", "payout:review",
		"reconciliation:read", "risk:read",
		"session:list_own", "session:revoke_own", "strategy:read", "trade:read", "withdrawal:review",
	},
	"FINANCE": {
		"account:read", "account:read_any", "credit:read", "funding:read", "gate:read", "ledger:post_correction",
		"ledger:read", "native_asset:read", "payout:read",
		"reconciliation:read", "reconciliation:resolve", "risk:read",
		"session:list_own", "session:revoke_own", "strategy:read", "trade:read", "withdrawal:review",
	},
	"SECURITY": {
		"account:read", "account:read_any", "break_glass:approve", "credit:read", "funding:read",
		"gate:read", "kill:activate",
		"ledger:read", "native_asset:read", "payout:read", "provider:disable",
		"reconciliation:read", "risk:read",
		"session:list_own", "session:revoke_any", "session:revoke_own", "strategy:read", "trade:read",
	},
	// ADMIN is the union of the operator roles plus the customer surface, and
	// deliberately does NOT hold credit:adjust: adjusting a Credit balance is
	// dual-controlled, exactly like approving a ledger correction.
	"ADMIN": {
		"account:freeze", "account:read", "account:read_any", "admin:audit_read", "agent:pause", "agent:promote",
		"break_glass:approve", "break_glass:request",
		"credit:purchase", "credit:read",
		"envelope:authority_write", "funding:create", "funding:read", "gate:propose", "gate:read",
		"instrument:status_write", "kill:activate", "ledger:post_correction", "ledger:read",
		"native_asset:create", "native_asset:moderate", "native_asset:read",
		"native_market:halt", "native_market:surveil", "native_market:trade",
		"payout:create", "payout:read", "payout:review",
		"provider:disable", "provider:enable", "reconciliation:read", "reconciliation:resolve",
		"risk:policy_write", "risk:read", "session:list_own", "session:revoke_any", "session:revoke_own",
		"strategy:read", "strategy:write", "trade:create", "trade:read", "withdrawal:create", "withdrawal:review",
	},
	"BREAK_GLASS": {
		"agent:promote_approve", "credit:adjust", "envelope:approve", "gate:approve", "kill:release",
		"ledger:approve_correction", "payout:approve",
		"reconciliation:approve", "withdrawal:approve",
	},
}

// goldenDualControl are the approve-side permissions no standing role holds.
var goldenDualControl = []string{
	"agent:promote_approve", "credit:adjust", "envelope:approve", "gate:approve", "kill:release",
	"ledger:approve_correction", "payout:approve",
	"reconciliation:approve", "withdrawal:approve",
}

// goldenAgent is everything an AGENT can ever satisfy.
var goldenAgent = []string{
	"account:read", "agent:run", "intent:create_agent",
	"prediction:commit", "strategy:read", "trade:read",
}

func permStrings(ps []Permission) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, string(p))
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestGoldenMatrix_PermissionListClosed(t *testing.T) {
	got := permStrings(AllPermissions())
	want := append([]string(nil), goldenPermissions...)
	sort.Strings(want)
	if !equalStrings(got, want) {
		t.Fatalf("AllPermissions drifted from golden list\n got=%v\nwant=%v", got, want)
	}
	// 55 after the Nodal-native economy: credit:{read,purchase,adjust},
	// native_asset:{create,read,moderate}, native_market:{trade,halt,surveil},
	// payout:{create,read,review,approve} (13 added). They are separate from
	// trade:* and withdrawal:* because the internal economy is a different
	// legal animal, and a deployment must be able to grant one without the
	// other.
	if len(got) != 55 {
		t.Fatalf("expected 55 permissions, got %d", len(got))
	}
	for _, p := range goldenPermissions {
		if !Permission(p).Valid() {
			t.Errorf("golden permission %q not Valid()", p)
		}
	}
	if Permission("account:delete").Valid() {
		t.Error("unknown permission reported Valid()")
	}
}

func TestGoldenMatrix_Equal(t *testing.T) {
	if len(RolePermissions) != len(goldenMatrix) {
		t.Fatalf("RolePermissions has %d roles, golden has %d", len(RolePermissions), len(goldenMatrix))
	}
	for role, want := range goldenMatrix {
		got, ok := RolePermissions[Role(role)]
		if !ok {
			t.Errorf("role %s missing from RolePermissions", role)
			continue
		}
		w := append([]string(nil), want...)
		sort.Strings(w)
		if g := permStrings(got); !equalStrings(g, w) {
			t.Errorf("role %s permissions drifted from golden\n got=%v\nwant=%v", role, g, w)
		}
	}
	for role := range RolePermissions {
		if _, ok := goldenMatrix[string(role)]; !ok {
			t.Errorf("RolePermissions has role %s not in golden", role)
		}
	}
	for _, r := range AllRoles() {
		if !r.Valid() {
			t.Errorf("role %s not Valid()", r)
		}
	}
	if Role("ROOT").Valid() {
		t.Error("unknown role reported Valid()")
	}
}

func TestGoldenMatrix_SortedUniqueAndKnown(t *testing.T) {
	for role, ps := range RolePermissions {
		if !sort.SliceIsSorted(ps, func(i, j int) bool { return ps[i] < ps[j] }) {
			t.Errorf("role %s permissions not sorted", role)
		}
		seen := map[Permission]bool{}
		for _, p := range ps {
			if seen[p] {
				t.Errorf("role %s lists %s twice", role, p)
			}
			seen[p] = true
			if !p.Valid() {
				t.Errorf("role %s grants unknown permission %q", role, p)
			}
		}
		if got := permStrings(PermissionsForRole(role)); !equalStrings(got, permStrings(ps)) {
			t.Errorf("PermissionsForRole(%s) differs from RolePermissions", role)
		}
	}
	if PermissionsForRole(Role("ROOT")) != nil {
		t.Error("PermissionsForRole(unknown) should be nil")
	}
}

func TestGoldenMatrix_DualControlHeldByNoStandingRole(t *testing.T) {
	if got := permStrings(DualControlPermissions()); !equalStrings(got, goldenDualControl) {
		t.Fatalf("dual-control set drifted: got %v want %v", got, goldenDualControl)
	}
	for _, p := range goldenDualControl {
		if !IsDualControl(Permission(p)) {
			t.Errorf("IsDualControl(%s) = false", p)
		}
		for _, r := range AllRoles() {
			if r == RoleBreakGlass {
				if !RoleGrants(r, Permission(p)) {
					t.Errorf("BREAK_GLASS must carry %s", p)
				}
				continue
			}
			if RoleGrants(r, Permission(p)) {
				t.Errorf("standing role %s must not hold dual-control permission %s", r, p)
			}
		}
	}
	for _, p := range goldenPermissions {
		if IsDualControl(Permission(p)) {
			continue
		}
		if RoleGrants(RoleBreakGlass, Permission(p)) {
			t.Errorf("BREAK_GLASS must not hold non-approve permission %s", p)
		}
	}
	if IsDualControl(PermKillActivate) {
		t.Error("kill:activate must be fast and single-person (PART 93), not dual-control")
	}
}

func TestGoldenMatrix_AgentSet(t *testing.T) {
	if got := permStrings(AgentPermissions()); !equalStrings(got, goldenAgent) {
		t.Fatalf("agent set drifted: got %v want %v", got, goldenAgent)
	}
	for _, p := range []Permission{PermAgentRun, PermPredictionCommit, PermIntentCreateAgent} {
		if !IsAgentOnly(p) {
			t.Errorf("IsAgentOnly(%s) = false", p)
		}
		for _, r := range AllRoles() {
			if RoleGrants(r, p) {
				t.Errorf("role %s must not hold agent-only permission %s", r, p)
			}
		}
	}
	if IsAgentOnly(PermAccountRead) {
		t.Error("account:read is shared, not agent-only")
	}
}

func TestGoldenMatrix_EveryPermissionReachable(t *testing.T) {
	for _, p := range goldenPermissions {
		perm := Permission(p)
		reachable := false
		for _, r := range AllRoles() {
			if RoleGrants(r, perm) {
				reachable = true
			}
		}
		for _, a := range AgentPermissions() {
			if a == perm {
				reachable = true
			}
		}
		if !reachable {
			t.Errorf("permission %s is granted to nobody", p)
		}
	}
}

// TestRoleMatrix_EveryCell drives RequireAt through every role × permission
// cell and compares with the golden matrix, so the runtime decision and the
// declared matrix cannot diverge.
func TestRoleMatrix_EveryCell(t *testing.T) {
	t0 := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	clk := func() time.Time { return t0 }
	for _, role := range AllRoles() {
		want := map[string]bool{}
		for _, p := range goldenMatrix[string(role)] {
			want[p] = true
		}
		p := Principal{SubjectID: "sub-" + string(role), ActorType: ActorOperator, Roles: []Role{role}}
		if role == RoleCustomer {
			p.ActorType = ActorUser
			p.AccountIDs = []string{"acct-1"}
		}
		if role == RoleBreakGlass {
			until := t0.Add(15 * time.Minute)
			p.BreakGlassUntil = &until
		}
		ctx := WithPrincipal(context.Background(), p)
		for _, perm := range goldenPermissions {
			t.Run(string(role)+"/"+perm, func(t *testing.T) {
				err := RequireAt(ctx, Permission(perm), clk)
				switch {
				case want[perm] && err != nil:
					t.Fatalf("golden grants %s to %s but RequireAt: %v", perm, role, err)
				case !want[perm] && err == nil:
					t.Fatalf("golden denies %s to %s but RequireAt allowed it", perm, role)
				case !want[perm] && !errors.Is(err, ErrForbidden):
					t.Fatalf("denial must be ErrForbidden, got %v", err)
				}
				if has := p.Has(Permission(perm), t0); has != want[perm] {
					t.Fatalf("Principal.Has(%s) = %v, golden %v", perm, has, want[perm])
				}
			})
		}
		if got := permStrings(p.Permissions(t0)); !equalStrings(got, sortedCopy(goldenMatrix[string(role)])) {
			t.Errorf("Principal.Permissions for %s = %v, golden %v", role, got, goldenMatrix[string(role)])
		}
	}
}

func sortedCopy(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}
