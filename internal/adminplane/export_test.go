package adminplane

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/security"
)

// updateAuthority rewrites the generated document the operator console loads:
//
//	go test ./internal/adminplane -run TestAuthorityGolden -update-authority
var updateAuthority = flag.Bool("update-authority", false,
	"rewrite apps/admin/src/generated/authority.json from the Go policy tables")

// authorityPath is where the console reads the document from. The path is
// relative to this package.
const authorityPath = "../../apps/admin/src/generated/authority.json"

// TestAuthorityGolden keeps the operator console's copy of the authority model
// exactly equal to the Go tables. It is the mechanism that stops the console
// from drifting into claiming an authority the server does not grant: change a
// permission, a step-up window or an action kind in Go and this test fails
// until the console's copy is regenerated.
func TestAuthorityGolden(t *testing.T) {
	got, err := ExportJSON()
	require.NoError(t, err)

	if *updateAuthority {
		require.NoError(t, os.MkdirAll(filepath.Dir(authorityPath), 0o755))
		require.NoError(t, os.WriteFile(authorityPath, got, 0o644))
		t.Logf("wrote %s (%d bytes)", authorityPath, len(got))
		return
	}

	want, err := os.ReadFile(authorityPath)
	if os.IsNotExist(err) {
		t.Fatalf("%s is missing; run: go test ./internal/adminplane -run TestAuthorityGolden -update-authority", authorityPath)
	}
	require.NoError(t, err)
	assert.Equal(t, string(normalizeNewlines(want)), string(got),
		"the operator console's authority document is stale; regenerate it with -update-authority")
}

func normalizeNewlines(b []byte) []byte {
	return []byte(strings.ReplaceAll(string(b), "\r\n", "\n"))
}

// TestExportRestatesNothing checks every exported value against the package it
// came from. A literal that crept into this package instead of being read from
// the source of truth fails here.
func TestExportRestatesNothing(t *testing.T) {
	t.Parallel()
	a := Export()

	assert.Equal(t, security.AllPermissions(), a.Permissions)
	assert.Equal(t, security.DualControlPermissions(), a.DualControl)
	assert.Equal(t, security.AgentPermissions(), a.AgentPermissions)
	assert.Equal(t, admin.MinReasonLength, a.MinReasonLength)
	assert.Equal(t, admin.AllStatuses(), a.ActionStatuses)
	assert.Equal(t, gates.AllCapabilities(), a.Capabilities)
	assert.Equal(t, admin.MaxBreakGlassDuration, a.BreakGlass.MaxDuration.Duration())
	assert.Equal(t, security.PermissionsForRole(security.RoleBreakGlass), a.BreakGlass.Grants)

	require.Len(t, a.Roles, len(security.AllRoles()))
	for _, r := range a.Roles {
		assert.Equal(t, security.PermissionsForRole(r.Role), r.Permissions, r.Role)
		assert.Equal(t, r.Role != security.RoleBreakGlass, r.Standing, r.Role)
	}

	require.Len(t, a.ActionKinds, len(admin.Kinds()))
	for _, k := range a.ActionKinds {
		spec, ok := admin.Spec(k.Kind)
		require.True(t, ok, k.Kind)
		assert.Equal(t, spec.RequiresDual, k.RequiresDual, k.Kind)
		assert.Equal(t, spec.ProposePermission, k.ProposePermission, k.Kind)
		assert.Equal(t, spec.ApprovePermission, k.ApprovePermission, k.Kind)
		assert.Equal(t, spec.StepUpMaxAge, k.StepUpMaxAge.Duration(), k.Kind)
		assert.Equal(t, spec.Expiry, k.Expiry.Duration(), k.Kind)
	}

	require.Len(t, a.KillSwitchKinds, len(killswitch.AllKinds()))
	for _, k := range a.KillSwitchKinds {
		assert.Equal(t, k.Kind.Severity(), k.Severity, k.Kind)
		assert.Equal(t, k.Severity == killswitch.SeveritySevere, k.ReleaseNeedsApproval, k.Kind)
	}

	for _, g := range a.GateActions {
		assert.Equal(t, gates.StepUpMaxAge, g.StepUpMaxAge.Duration(), g.Action)
	}
	assert.Equal(t, allReasons, a.Reasons)
	assert.Equal(t, allVerbs, a.Verbs)
}

// TestStepUpWindowsAgree pins the equality this package would otherwise be
// asserting by coincidence: the HTTP boundary's window is the same as the
// domain windows it sits in front of. If one of them moves, this fails rather
// than the console silently promising the wrong freshness.
func TestStepUpWindowsAgree(t *testing.T) {
	t.Parallel()
	assert.Equal(t, gates.StepUpMaxAge, time.Duration(stepUpHTTP))
	assert.Equal(t, killswitch.StepUpMaxAge, time.Duration(stepUpHTTP))
}

// TestNoSurfaceOffersABalanceEdit is the invariant the whole system is built
// around, restated where an operator could most plausibly expect to find one.
func TestNoSurfaceOffersABalanceEdit(t *testing.T) {
	t.Parallel()
	forbidden := []string{"BALANCE", "PATCH", "SQL", "POSITION_EDIT", "ADJUST_BALANCE", "SET_BALANCE"}
	check := func(what, s string) {
		u := strings.ToUpper(s)
		for _, bad := range forbidden {
			assert.NotContains(t, u, bad, "%s: %s", what, s)
		}
	}
	for _, s := range Surfaces() {
		check("surface", string(s.ID))
		for _, w := range s.Writes {
			check("write", w.ID)
		}
		for _, k := range s.ActionKinds {
			check("kind", string(k))
			assert.True(t, k.Valid(), "surface %s names undeclared kind %s", s.ID, k)
		}
	}
	// The only financial repair anywhere in the model is a compensating
	// journal transaction.
	recon, ok := SurfaceByID(SurfaceReconciliation)
	require.True(t, ok)
	assert.Contains(t, recon.ActionKinds, admin.KindLedgerCorrection)
}

// TestEverySurfaceIsReachableBySomeStandingRole: a surface no role can read is
// a page nobody can open, which is a defect, not a security control.
func TestEverySurfaceIsReachableBySomeStandingRole(t *testing.T) {
	t.Parallel()
	for _, s := range Surfaces() {
		require.NotEmpty(t, s.ReadAnyOf, "%s declares no read permission", s.ID)
		reachable := false
		for _, r := range security.AllRoles() {
			if r == security.RoleBreakGlass {
				continue
			}
			for _, p := range s.ReadAnyOf {
				if security.RoleGrants(r, p) {
					reachable = true
				}
			}
		}
		assert.True(t, reachable, "no standing role can read surface %s", s.ID)
		for _, w := range s.Writes {
			require.NotEmpty(t, w.AnyOf, "write %s declares no permission", w.ID)
			assert.True(t, w.NeverForAgent, "write %s does not say it excludes agents", w.ID)
			assert.True(t, w.RequiresReason, "write %s does not demand a reason", w.ID)
		}
	}
}

// TestVisibleSurfacesFollowTheMatrix walks every standing role and asserts the
// surfaces it sees are exactly those whose read permissions it holds.
func TestVisibleSurfacesFollowTheMatrix(t *testing.T) {
	t.Parallel()
	for _, r := range security.AllRoles() {
		if r == security.RoleBreakGlass {
			continue
		}
		a := NewActor(operator(user(), r))
		got := VisibleSurfaces(a, now0)
		var want []SurfaceID
		for _, s := range Surfaces() {
			for _, p := range s.ReadAnyOf {
				if security.RoleGrants(r, p) {
					want = append(want, s.ID)
					break
				}
			}
		}
		assert.Equal(t, want, got, "role %s", r)
	}
	// A customer is not an operator: they see nothing here.
	assert.Empty(t, VisibleSurfaces(NewActor(operator(user(), security.RoleCustomer)), now0))
}

// TestAllowedWritesRespectStepUp: a write with a step-up window is not offered
// to a stale or password-only session.
func TestAllowedWritesRespectStepUp(t *testing.T) {
	t.Parallel()
	uid := user()
	fresh := NewActor(operator(uid, security.RoleOperations))
	assert.Contains(t, AllowedWrites(fresh, SurfaceKillSwitches, now0), "kill.activate",
		"activation is the fast path and demands no step-up")

	stale := operator(uid, security.RoleOperations)
	stale.AuthTime = now0.Add(-24 * time.Hour)
	assert.Contains(t, AllowedWrites(NewActor(stale), SurfaceKillSwitches, now0), "kill.activate",
		"a stale session may still stop new risk")

	weak := operator(uid, security.RoleCompliance)
	weak.AMR = []string{"pwd"}
	assert.NotContains(t, AllowedWrites(NewActor(weak), SurfaceAccounts, now0), "account.status",
		"a password-only session is not offered a step-up write")
	assert.Contains(t, AllowedWrites(NewActor(operator(uid, security.RoleCompliance)), SurfaceAccounts, now0), "account.status")
}

// TestSecondsMarshalsAsWholeSeconds guards the one number-shaped field that
// crosses into JavaScript.
func TestSecondsMarshalsAsWholeSeconds(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(Sec(15 * time.Minute))
	require.NoError(t, err)
	assert.JSONEq(t, "900", string(b))

	var back Seconds
	require.NoError(t, json.Unmarshal([]byte("900"), &back))
	assert.Equal(t, 15*time.Minute, back.Duration())

	b, err = json.Marshal(Sec(1500 * time.Millisecond))
	require.NoError(t, err)
	assert.JSONEq(t, "1", string(b), "sub-second remainders truncate toward zero")
}

// TestExportedDocumentIsDeterministic: two renders are byte-identical, so the
// golden file never churns.
func TestExportedDocumentIsDeterministic(t *testing.T) {
	t.Parallel()
	a, err := ExportJSON()
	require.NoError(t, err)
	b, err := ExportJSON()
	require.NoError(t, err)
	assert.Equal(t, string(a), string(b))
	assert.True(t, strings.HasSuffix(string(a), "}\n"))

	var doc map[string]any
	require.NoError(t, json.Unmarshal(a, &doc), "the document is valid JSON")
	assert.EqualValues(t, AuthorityVersion, doc["version"])
}
