package admin

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/security"
)

var declaredKinds = []Kind{
	KindCapabilityGateApprove, KindKillSwitchRelease, KindLedgerCorrection, KindReconciliationResolveMaterial,
	KindEnvelopeAuthorityChange, KindAccountUnfreeze, KindWithdrawalApprove, KindBreakGlassGrant, KindAgentPromote,
	KindNativeMarketHalt, KindNativeMarketCloseOnly, KindNativeMarketFreeze, KindNativeMarketResume,
	KindNativeAssetModerationVerdict, KindNativeAssetDelist,
	KindCommerceSellerSuspend, KindCommerceProductWithdraw, KindPayoutManualReviewResolve,
}

func TestKindTable_Complete(t *testing.T) {
	t.Parallel()
	kinds := Kinds()
	require.ElementsMatch(t, declaredKinds, kinds, "every declared constant has a spec and vice versa")
	for i := 1; i < len(kinds); i++ {
		assert.Less(t, kinds[i-1], kinds[i], "Kinds is sorted")
	}
	for _, k := range kinds {
		spec, ok := Spec(k)
		require.True(t, ok, k)
		assert.True(t, k.Valid())
		assert.True(t, spec.ProposePermission.Valid(), "%s propose permission %q", k, spec.ProposePermission)
		assert.Positive(t, spec.StepUpMaxAge, k)
		assert.LessOrEqual(t, spec.StepUpMaxAge, 15*time.Minute, k)
		assert.Positive(t, spec.Expiry, k)
		assert.LessOrEqual(t, spec.Expiry, 24*time.Hour, k)
		if spec.RequiresDual {
			require.True(t, spec.ApprovePermission.Valid(), "%s approve permission %q", k, spec.ApprovePermission)
			assert.NotEqual(t, spec.ProposePermission, spec.ApprovePermission, "%s: approve and propose permissions must differ", k)
			// break_glass:approve is deliberately a standing permission (the first
			// elevation must be approvable without an existing elevation); every
			// other dual-control kind's approve side is held by no standing role.
			if k != KindBreakGlassGrant {
				assert.True(t, security.IsDualControl(spec.ApprovePermission), "%s: approve permission must be a dual-control permission", k)
			}
		} else {
			assert.Empty(t, spec.ApprovePermission, "%s: non-dual kinds have no approve permission", k)
		}
	}
	assert.False(t, Kind("PATCH_BALANCE").Valid())
	_, ok := Spec("")
	assert.False(t, ok)
}

func TestKindTable_Golden(t *testing.T) {
	t.Parallel()
	want := map[Kind]KindSpec{
		KindCapabilityGateApprove: {
			RequiresDual: true, ProposePermission: security.PermGatePropose, ApprovePermission: security.PermGateApprove,
			StepUpMaxAge: 15 * time.Minute, Expiry: 24 * time.Hour,
		},
		KindKillSwitchRelease: {
			RequiresDual: true, ProposePermission: security.PermKillActivate, ApprovePermission: security.PermKillRelease,
			StepUpMaxAge: 15 * time.Minute, Expiry: time.Hour,
		},
		KindLedgerCorrection: {
			RequiresDual: true, ProposePermission: security.PermLedgerPostCorrection, ApprovePermission: security.PermLedgerApproveCorrection,
			StepUpMaxAge: 5 * time.Minute, Expiry: 4 * time.Hour,
		},
		KindReconciliationResolveMaterial: {
			RequiresDual: true, ProposePermission: security.PermReconciliationResolve, ApprovePermission: security.PermReconciliationApprove,
			StepUpMaxAge: 15 * time.Minute, Expiry: 4 * time.Hour,
		},
		KindEnvelopeAuthorityChange: {
			RequiresDual: true, ProposePermission: security.PermEnvelopeAuthorityWrite, ApprovePermission: security.PermEnvelopeApprove,
			StepUpMaxAge: 15 * time.Minute, Expiry: 24 * time.Hour,
		},
		KindAccountUnfreeze: {
			RequiresDual: false, ProposePermission: security.PermAccountFreeze,
			StepUpMaxAge: 15 * time.Minute, Expiry: time.Hour,
		},
		KindWithdrawalApprove: {
			RequiresDual: true, ProposePermission: security.PermWithdrawalReview, ApprovePermission: security.PermWithdrawalApprove,
			StepUpMaxAge: 5 * time.Minute, Expiry: time.Hour,
		},
		KindBreakGlassGrant: {
			RequiresDual: true, ProposePermission: security.PermBreakGlassRequest, ApprovePermission: security.PermBreakGlassApprove,
			StepUpMaxAge: 5 * time.Minute, Expiry: 30 * time.Minute, ApproverIsNotTarget: true,
		},
		KindAgentPromote: {
			RequiresDual: true, ProposePermission: security.PermAgentPromote, ApprovePermission: security.PermAgentPromoteApprove,
			StepUpMaxAge: 15 * time.Minute, Expiry: 24 * time.Hour,
		},

		// The Nodal-native economy. Stopping is one operator; restarting and
		// resolving money by hand are two.
		KindNativeMarketHalt: {
			RequiresDual: false, ProposePermission: security.PermNativeMarketHalt,
			StepUpMaxAge: 15 * time.Minute, Expiry: time.Hour,
		},
		KindNativeMarketCloseOnly: {
			RequiresDual: false, ProposePermission: security.PermNativeMarketHalt,
			StepUpMaxAge: 15 * time.Minute, Expiry: time.Hour,
		},
		KindNativeMarketFreeze: {
			RequiresDual: false, ProposePermission: security.PermNativeMarketHalt,
			StepUpMaxAge: 5 * time.Minute, Expiry: time.Hour,
		},
		KindNativeMarketResume: {
			RequiresDual: true, ProposePermission: security.PermNativeMarketHalt,
			ApprovePermission: security.PermNativeMarketResume,
			StepUpMaxAge:      5 * time.Minute, Expiry: 4 * time.Hour,
		},
		KindNativeAssetModerationVerdict: {
			RequiresDual: false, ProposePermission: security.PermNativeAssetModerate,
			StepUpMaxAge: 15 * time.Minute, Expiry: 24 * time.Hour,
		},
		KindNativeAssetDelist: {
			RequiresDual: false, ProposePermission: security.PermNativeAssetModerate,
			StepUpMaxAge: 15 * time.Minute, Expiry: time.Hour,
		},
		KindCommerceSellerSuspend: {
			RequiresDual: false, ProposePermission: security.PermCommerceModerate,
			StepUpMaxAge: 15 * time.Minute, Expiry: time.Hour,
		},
		KindCommerceProductWithdraw: {
			RequiresDual: false, ProposePermission: security.PermCommerceModerate,
			StepUpMaxAge: 15 * time.Minute, Expiry: time.Hour,
		},
		KindPayoutManualReviewResolve: {
			RequiresDual: true, ProposePermission: security.PermPayoutReview,
			ApprovePermission: security.PermPayoutApprove,
			StepUpMaxAge:      5 * time.Minute, Expiry: 4 * time.Hour,
		},
	}
	assert.Equal(t, want, kindSpecs, "the kind policy table changed; review dual-control consequences before updating this golden")

	// The asymmetry that makes the internal economy safe to operate: every
	// kind that STOPS something is reachable by one operator, and every kind
	// that restarts it or decides where money goes needs two. A control that
	// takes two signatures to stop an incident is one nobody reaches for.
	stops := []Kind{
		KindNativeMarketHalt, KindNativeMarketCloseOnly, KindNativeMarketFreeze,
		KindNativeAssetDelist, KindCommerceSellerSuspend, KindCommerceProductWithdraw,
	}
	for _, k := range stops {
		spec, ok := Spec(k)
		require.True(t, ok, "%s", k)
		assert.False(t, spec.RequiresDual, "%s stops risk and must not need two signatures", k)
	}
	for _, k := range []Kind{KindNativeMarketResume, KindPayoutManualReviewResolve} {
		spec, ok := Spec(k)
		require.True(t, ok, "%s", k)
		assert.True(t, spec.RequiresDual, "%s adds risk or moves money and must need two", k)
		assert.NotEmpty(t, spec.ApprovePermission, "%s declares dual control with no approve side", k)
	}

	// ApproverIsNotTarget belongs to exactly the kind whose target_id names a
	// person. Setting it on a kind whose target is a gate, a switch or an
	// account would compare a user id against something that is not one, which
	// is a silent no-op rather than a control.
	for _, k := range Kinds() {
		spec, _ := Spec(k)
		assert.Equal(t, k == KindBreakGlassGrant, spec.ApproverIsNotTarget,
			"%s: only the kind that elevates a principal refuses the target as approver", k)
	}
}

func TestNoBalanceEditKindExists(t *testing.T) {
	t.Parallel()
	for _, k := range Kinds() {
		u := strings.ToUpper(string(k))
		assert.NotContains(t, u, "BALANCE", k)
		assert.NotContains(t, u, "PATCH", k)
		assert.NotContains(t, u, "SQL", k)
		assert.NotContains(t, u, "POSITION_EDIT", k)
	}
	// Financial repair exists only as a compensating journal transaction.
	assert.True(t, KindLedgerCorrection.Valid())
}

func TestAgentsHoldNoAdminPermission(t *testing.T) {
	t.Parallel()
	agent := security.AgentPrincipal("agent-1", "acct-1")
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	agentPerms := map[security.Permission]bool{}
	for _, p := range security.AgentPermissions() {
		agentPerms[p] = true
	}
	for _, k := range Kinds() {
		spec, _ := Spec(k)
		assert.False(t, agentPerms[spec.ProposePermission], "%s propose permission is agent-holdable", k)
		assert.False(t, agent.Has(spec.ProposePermission, now), k)
		if spec.ApprovePermission != "" {
			assert.False(t, agentPerms[spec.ApprovePermission], "%s approve permission is agent-holdable", k)
			assert.False(t, agent.Has(spec.ApprovePermission, now), k)
		}
	}
	for _, p := range ReadPermissions() {
		assert.False(t, agentPerms[p], "read permission %s is agent-holdable", p)
	}
}

func TestDualControlKindsNeedTwoPeople(t *testing.T) {
	t.Parallel()
	// A single standing role must not hold both a dual-control approve
	// permission and its propose permission: for dual-control permissions
	// the matrix guarantees no standing role holds the approve side at all.
	for _, k := range Kinds() {
		spec, _ := Spec(k)
		if !spec.RequiresDual || !security.IsDualControl(spec.ApprovePermission) {
			continue
		}
		for _, r := range security.AllRoles() {
			if r == security.RoleBreakGlass {
				continue
			}
			assert.False(t, security.RoleGrants(r, spec.ApprovePermission), "standing role %s holds %s", r, spec.ApprovePermission)
		}
	}
}

func TestReadPermissions(t *testing.T) {
	t.Parallel()
	perms := ReadPermissions()
	assert.Contains(t, perms, security.PermAdminAuditRead)
	for _, k := range Kinds() {
		spec, _ := Spec(k)
		assert.Contains(t, perms, spec.ProposePermission)
		if spec.ApprovePermission != "" {
			assert.Contains(t, perms, spec.ApprovePermission)
		}
	}
	for i := 1; i < len(perms); i++ {
		assert.Less(t, perms[i-1], perms[i])
	}
	perms[0] = "mutated"
	assert.NotEqual(t, security.Permission("mutated"), ReadPermissions()[0], "copy")
}
