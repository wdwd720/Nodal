package admin

import (
	"sort"
	"time"

	"github.com/nodal/controlplane/internal/security"
)

// Kind names a controlled administrative action (POLICY_AUTHORITY §5). The
// set is closed: an unknown kind is rejected at Propose.
type Kind string

// Kinds. There is deliberately no balance-edit kind (PART 129): the only
// financial repair is LEDGER_CORRECTION, a reason-coded compensating journal
// transaction posted by internal/ledger.
const (
	// KindCapabilityGateApprove approves a production capability gate
	// (internal/gates); the gate's own state machine records the approval id.
	KindCapabilityGateApprove Kind = "CAPABILITY_GATE_APPROVE"
	// KindKillSwitchRelease releases a SEVERE kill switch (GLOBAL, CHAIN,
	// PROVIDER, FUNDING, WITHDRAWALS); internal/killswitch verifies it.
	KindKillSwitchRelease Kind = "KILL_SWITCH_RELEASE"
	// KindLedgerCorrection posts a compensating journal transaction.
	KindLedgerCorrection Kind = "LEDGER_CORRECTION"
	// KindReconciliationResolveMaterial resolves a reconciliation mismatch
	// above the materiality threshold.
	KindReconciliationResolveMaterial Kind = "RECONCILIATION_RESOLVE_MATERIAL"
	// KindEnvelopeAuthorityChange changes the authority fields of a capital
	// envelope (allocation, caps, allowed instruments/venues, status).
	KindEnvelopeAuthorityChange Kind = "ENVELOPE_AUTHORITY_CHANGE"
	// KindAccountUnfreeze lifts an account freeze. ACCOUNT_FREEZE is a
	// STANDARD-severity control (POLICY_AUTHORITY §2), so its release is a
	// single compliance operator with step-up rather than dual control.
	KindAccountUnfreeze Kind = "ACCOUNT_UNFREEZE"
	// KindWithdrawalApprove approves a withdrawal for submission.
	KindWithdrawalApprove Kind = "WITHDRAWAL_APPROVE"
	// KindBreakGlassGrant grants a time-boxed break-glass elevation to one
	// user (PART 93). Its Execute result is a Grant.
	KindBreakGlassGrant Kind = "BREAK_GLASS_GRANT"
	// KindAgentPromote promotes an agent up the SHADOW → CANARY → LIMITED →
	// LIVE ladder (AGENT_RUNTIME.md); the agent package verifies it.
	KindAgentPromote Kind = "AGENT_PROMOTE"

	// --- the Nodal-native economy (gola.md PARTS XIII-XXI, LXVIII) --------
	//
	// These follow one rule, and the asymmetry is deliberate: STOPPING
	// something is a single operator, RESTARTING it is dual control.
	//
	// A control that needs two signatures to stop an incident is a control
	// nobody reaches for at 3am, and PART XXXII is explicit that halting new
	// risk must never be harder than taking it. Restarting is the direction
	// that adds exposure, so that is where the second pair of eyes belongs.

	// KindNativeMarketHalt stops all trading on one internal market.
	KindNativeMarketHalt Kind = "NATIVE_MARKET_HALT"
	// KindNativeMarketCloseOnly lets holders sell and refuses new buyers. It
	// is the softer stop: it does not trap anybody in a position.
	KindNativeMarketCloseOnly Kind = "NATIVE_MARKET_CLOSE_ONLY"
	// KindNativeMarketFreeze stops everything including exits. It is the
	// hardest control in the internal economy and is for a suspected economic
	// incident, where letting holders out would be letting the first movers
	// out at everyone else's expense.
	KindNativeMarketFreeze Kind = "NATIVE_MARKET_FREEZE"
	// KindNativeMarketResume returns a market to a state that accepts new
	// risk. Dual control.
	KindNativeMarketResume Kind = "NATIVE_MARKET_RESUME"
	// KindNativeAssetModerationVerdict records a moderation decision on a
	// user-created asset. A verdict does not by itself start trading:
	// activating a market is a separate, gated act.
	KindNativeAssetModerationVerdict Kind = "NATIVE_ASSET_MODERATION_VERDICT"
	// KindNativeAssetDelist permanently removes an asset from the registry's
	// tradable set.
	KindNativeAssetDelist Kind = "NATIVE_ASSET_DELIST"
	// KindCommerceSellerSuspend stops an account taking new orders.
	KindCommerceSellerSuspend Kind = "COMMERCE_SELLER_SUSPEND"
	// KindCommerceProductWithdraw takes one product down permanently.
	KindCommerceProductWithdraw Kind = "COMMERCE_PRODUCT_WITHDRAW"
	// KindPayoutManualReviewResolve applies an operator's decision to a payout
	// stuck in MANUAL_REVIEW. It can fail, reject or retry one; it can NEVER
	// declare one settled, because the provider is authoritative for that.
	KindPayoutManualReviewResolve Kind = "PAYOUT_MANUAL_REVIEW_RESOLVE"
)

// KindSpec is the policy attached to a kind.
//
// ApprovePermission is empty for kinds that do not require dual control.
// For dual-control kinds it is either a dual-control permission from
// internal/security (held only by a live BREAK_GLASS elevation) or, where
// the permission matrix declares no approve-side permission for the domain,
// the propose permission itself: two distinct principals of the same role
// must then agree, which is still two-person control because Approve
// refuses the proposer.
type KindSpec struct {
	RequiresDual      bool
	ProposePermission security.Permission
	ApprovePermission security.Permission
	// StepUpMaxAge is how recent the principal's strong authentication must
	// be to propose, approve or execute.
	StepUpMaxAge time.Duration
	// Expiry is how long after proposal the action can still be approved
	// and executed.
	Expiry time.Duration
	// ApproverIsNotTarget marks a kind whose target_id names a person who
	// *gains* something from the action, so that person must not be the one
	// who approves it. Approver ≠ proposer alone is not enough there: an
	// operator who talks a colleague into proposing an elevation for them
	// and then approves it themselves has granted themselves the elevation,
	// with the second signature supplied by the beneficiary. It is set only
	// for BREAK_GLASS_GRANT, the one kind whose target is a principal.
	ApproverIsNotTarget bool
}

// Step-up freshness and expiry defaults, by sensitivity.
const (
	stepUpStandard  = 15 * time.Minute
	stepUpSensitive = 5 * time.Minute
)

var kindSpecs = map[Kind]KindSpec{
	KindCapabilityGateApprove: {
		RequiresDual: true, ProposePermission: security.PermGatePropose, ApprovePermission: security.PermGateApprove,
		StepUpMaxAge: stepUpStandard, Expiry: 24 * time.Hour,
	},
	KindKillSwitchRelease: {
		RequiresDual: true, ProposePermission: security.PermKillActivate, ApprovePermission: security.PermKillRelease,
		StepUpMaxAge: stepUpStandard, Expiry: time.Hour,
	},
	KindLedgerCorrection: {
		RequiresDual: true, ProposePermission: security.PermLedgerPostCorrection, ApprovePermission: security.PermLedgerApproveCorrection,
		StepUpMaxAge: stepUpSensitive, Expiry: 4 * time.Hour,
	},
	KindReconciliationResolveMaterial: {
		RequiresDual: true, ProposePermission: security.PermReconciliationResolve, ApprovePermission: security.PermReconciliationApprove,
		StepUpMaxAge: stepUpStandard, Expiry: 4 * time.Hour,
	},
	KindEnvelopeAuthorityChange: {
		RequiresDual: true, ProposePermission: security.PermEnvelopeAuthorityWrite, ApprovePermission: security.PermEnvelopeApprove,
		StepUpMaxAge: stepUpStandard, Expiry: 24 * time.Hour,
	},
	KindAccountUnfreeze: {
		RequiresDual: false, ProposePermission: security.PermAccountFreeze,
		StepUpMaxAge: stepUpStandard, Expiry: time.Hour,
	},
	KindWithdrawalApprove: {
		// Compliance/finance reviews the payout; a break-glass approver confirms.
		RequiresDual: true, ProposePermission: security.PermWithdrawalReview, ApprovePermission: security.PermWithdrawalApprove,
		StepUpMaxAge: stepUpSensitive, Expiry: time.Hour,
	},
	KindBreakGlassGrant: {
		// An ADMIN requests; a distinct SECURITY or ADMIN principal approves
		// (break_glass:approve is a standing permission so the first elevation
		// remains possible; Approve still refuses the proposer, and refuses
		// the grantee named by target_id).
		RequiresDual: true, ProposePermission: security.PermBreakGlassRequest, ApprovePermission: security.PermBreakGlassApprove,
		StepUpMaxAge: stepUpSensitive, Expiry: 30 * time.Minute, ApproverIsNotTarget: true,
	},
	KindAgentPromote: {
		RequiresDual: true, ProposePermission: security.PermAgentPromote, ApprovePermission: security.PermAgentPromoteApprove,
		StepUpMaxAge: stepUpStandard, Expiry: 24 * time.Hour,
	},

	// --- the Nodal-native economy ----------------------------------------

	// Stopping: one operator, short expiry. A halt proposed yesterday is not
	// a halt anybody still wants executed today.
	KindNativeMarketHalt: {
		RequiresDual: false, ProposePermission: security.PermNativeMarketHalt,
		StepUpMaxAge: stepUpStandard, Expiry: time.Hour,
	},
	KindNativeMarketCloseOnly: {
		RequiresDual: false, ProposePermission: security.PermNativeMarketHalt,
		StepUpMaxAge: stepUpStandard, Expiry: time.Hour,
	},
	KindNativeMarketFreeze: {
		// The hardest stop still needs only one operator. Freezing traps
		// holders, which is exactly why it must be reachable instantly when
		// the alternative is letting the first movers out.
		RequiresDual: false, ProposePermission: security.PermNativeMarketHalt,
		StepUpMaxAge: stepUpSensitive, Expiry: time.Hour,
	},
	// Restarting: dual control, with a dedicated approve-side permission that
	// no standing role holds. Halting is one operator's call at 3am;
	// restarting a market after an economic incident is a decision two people
	// should be awake for.
	KindNativeMarketResume: {
		RequiresDual: true, ProposePermission: security.PermNativeMarketHalt,
		ApprovePermission: security.PermNativeMarketResume,
		StepUpMaxAge:      stepUpSensitive, Expiry: 4 * time.Hour,
	},
	KindNativeAssetModerationVerdict: {
		RequiresDual: false, ProposePermission: security.PermNativeAssetModerate,
		StepUpMaxAge: stepUpStandard, Expiry: 24 * time.Hour,
	},
	KindNativeAssetDelist: {
		RequiresDual: false, ProposePermission: security.PermNativeAssetModerate,
		StepUpMaxAge: stepUpStandard, Expiry: time.Hour,
	},
	KindCommerceSellerSuspend: {
		RequiresDual: false, ProposePermission: security.PermCommerceModerate,
		StepUpMaxAge: stepUpStandard, Expiry: time.Hour,
	},
	KindCommerceProductWithdraw: {
		RequiresDual: false, ProposePermission: security.PermCommerceModerate,
		StepUpMaxAge: stepUpStandard, Expiry: time.Hour,
	},
	// Resolving a payout by hand decides what happens to money somebody is
	// waiting for, so it is the one Domain A kind that is dual-controlled in
	// both directions.
	KindPayoutManualReviewResolve: {
		RequiresDual: true, ProposePermission: security.PermPayoutReview,
		ApprovePermission: security.PermPayoutApprove,
		StepUpMaxAge:      stepUpSensitive, Expiry: 4 * time.Hour,
	},
}

// Spec returns the policy for k.
func Spec(k Kind) (KindSpec, bool) {
	s, ok := kindSpecs[k]
	return s, ok
}

// Valid reports whether k is a declared kind.
func (k Kind) Valid() bool {
	_, ok := kindSpecs[k]
	return ok
}

// Kinds returns every declared kind, sorted.
func Kinds() []Kind {
	out := make([]Kind, 0, len(kindSpecs))
	for k := range kindSpecs {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// readPermissions is the set of permissions any one of which lets a
// principal read actions (Get, ListPending): the audit-read permission and
// every propose/approve permission of the table.
var readPermissions = func() []security.Permission {
	set := map[security.Permission]struct{}{security.PermAdminAuditRead: {}}
	for _, s := range kindSpecs {
		set[s.ProposePermission] = struct{}{}
		if s.ApprovePermission != "" {
			set[s.ApprovePermission] = struct{}{}
		}
	}
	out := make([]security.Permission, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}()

// ReadPermissions returns a copy of the permissions that grant read access
// to administrative actions.
func ReadPermissions() []security.Permission {
	return append([]security.Permission(nil), readPermissions...)
}
