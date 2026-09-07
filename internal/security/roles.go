package security

import "sort"

// Role is a standing operator or customer role (PART 91). BREAK_GLASS is a
// time-boxed elevation, never a standing assignment.
type Role string

// Roles.
const (
	RoleCustomer        Role = "CUSTOMER"
	RoleSupportReadOnly Role = "SUPPORT_READ_ONLY"
	RoleOperations      Role = "OPERATIONS"
	RoleRisk            Role = "RISK"
	RoleCompliance      Role = "COMPLIANCE"
	RoleFinance         Role = "FINANCE"
	RoleSecurity        Role = "SECURITY"
	RoleAdmin           Role = "ADMIN"
	// RoleBreakGlass grants the dual-control approve permissions only while
	// Principal.BreakGlassUntil is in the future (PART 93).
	RoleBreakGlass Role = "BREAK_GLASS"
)

// ActorType classifies who is acting. It is orthogonal to roles except for
// AGENT, which never carries roles (PART 9).
type ActorType string

// Actor types.
const (
	ActorUser     ActorType = "USER"     // a customer, human
	ActorOperator ActorType = "OPERATOR" // staff, human, individually identified (no shared accounts)
	ActorService  ActorType = "SERVICE"  // machine-to-machine caller with short-lived credentials
	ActorAgent    ActorType = "AGENT"    // an untrusted proposal generator bound to one account
	ActorSystem   ActorType = "SYSTEM"   // internal background work
)

// Permission is a fine-grained capability of the form "<resource>:<action>".
type Permission string

// Permissions. The list is closed: AllPermissions returns exactly these.
const (
	PermAccountRead    Permission = "account:read"
	PermAccountReadAny Permission = "account:read_any"
	PermAccountFreeze  Permission = "account:freeze"

	PermTradeCreate Permission = "trade:create"
	PermTradeRead   Permission = "trade:read"

	PermFundingCreate Permission = "funding:create"
	PermFundingRead   Permission = "funding:read"

	PermWithdrawalCreate  Permission = "withdrawal:create"
	PermWithdrawalReview  Permission = "withdrawal:review"
	PermWithdrawalApprove Permission = "withdrawal:approve"

	PermStrategyWrite Permission = "strategy:write"
	PermStrategyRead  Permission = "strategy:read"

	PermAgentPause          Permission = "agent:pause"
	PermAgentRun            Permission = "agent:run"
	PermAgentPromote        Permission = "agent:promote"
	PermAgentPromoteApprove Permission = "agent:promote_approve"

	PermEnvelopeAuthorityWrite Permission = "envelope:authority_write"
	PermEnvelopeApprove        Permission = "envelope:approve"

	PermPredictionCommit  Permission = "prediction:commit"
	PermIntentCreateAgent Permission = "intent:create_agent"

	PermLedgerRead              Permission = "ledger:read"
	PermLedgerPostCorrection    Permission = "ledger:post_correction"
	PermLedgerApproveCorrection Permission = "ledger:approve_correction"

	PermReconciliationRead    Permission = "reconciliation:read"
	PermReconciliationResolve Permission = "reconciliation:resolve"
	PermReconciliationApprove Permission = "reconciliation:approve"

	PermRiskRead        Permission = "risk:read"
	PermRiskPolicyWrite Permission = "risk:policy_write"

	PermGateRead    Permission = "gate:read"
	PermGatePropose Permission = "gate:propose"
	PermGateApprove Permission = "gate:approve"

	PermKillActivate Permission = "kill:activate"
	PermKillRelease  Permission = "kill:release"

	PermProviderDisable Permission = "provider:disable"
	PermProviderEnable  Permission = "provider:enable"

	PermInstrumentStatusWrite Permission = "instrument:status_write"

	PermAdminAuditRead Permission = "admin:audit_read"

	PermSessionListOwn   Permission = "session:list_own"
	PermSessionRevokeOwn Permission = "session:revoke_own"
	PermSessionRevokeAny Permission = "session:revoke_any"

	PermBreakGlassRequest Permission = "break_glass:request"
	PermBreakGlassApprove Permission = "break_glass:approve"

	// --- Nodal-native economy (gola.md PARTS XII-XXI) ---------------------
	//
	// These are separate permissions rather than reuses of trade:* and
	// withdrawal:* because the internal economy is a different legal animal
	// from external trading, and a deployment must be able to grant one
	// without the other. A role that may trade real assets is not thereby
	// permitted to launch speculative internal ones.

	// PermCreditRead reads a Credit balance and its provenance breakdown.
	PermCreditRead Permission = "credit:read"
	// PermCreditPurchase starts a Credit purchase.
	PermCreditPurchase Permission = "credit:purchase"
	// PermCreditAdjust is the privileged, audited administrative adjustment
	// of a Credit balance (PART XLIX). No standing role holds it.
	PermCreditAdjust Permission = "credit:adjust"

	// PermNativeAssetCreate creates a Nodal-native asset.
	PermNativeAssetCreate Permission = "native_asset:create"
	// PermNativeAssetRead reads the native asset registry.
	PermNativeAssetRead Permission = "native_asset:read"
	// PermNativeAssetModerate records a moderation verdict.
	PermNativeAssetModerate Permission = "native_asset:moderate"
	// PermNativeMarketTrade buys and sells on an internal market.
	PermNativeMarketTrade Permission = "native_market:trade"
	// PermNativeMarketHalt halts, freezes or closes a market.
	PermNativeMarketHalt Permission = "native_market:halt"
	// PermNativeMarketSurveil reads surveillance alerts.
	PermNativeMarketSurveil Permission = "native_market:surveil"

	// PermPayoutCreate requests a payout of eligible value.
	PermPayoutCreate Permission = "payout:create"
	// PermPayoutRead reads payout requests.
	PermPayoutRead Permission = "payout:read"
	// PermPayoutReview is the operator side of a manual payout review.
	PermPayoutReview Permission = "payout:review"
	// PermPayoutApprove is the approve half of a dual-controlled payout
	// release. No standing role holds it.
	PermPayoutApprove Permission = "payout:approve"
)

var allPermissions = []Permission{
	PermAccountRead, PermAccountReadAny, PermAccountFreeze,
	PermTradeCreate, PermTradeRead,
	PermFundingCreate, PermFundingRead,
	PermWithdrawalCreate, PermWithdrawalReview, PermWithdrawalApprove,
	PermStrategyWrite, PermStrategyRead,
	PermAgentPause, PermAgentRun, PermAgentPromote, PermAgentPromoteApprove,
	PermEnvelopeAuthorityWrite, PermEnvelopeApprove,
	PermPredictionCommit, PermIntentCreateAgent,
	PermLedgerRead, PermLedgerPostCorrection, PermLedgerApproveCorrection,
	PermReconciliationRead, PermReconciliationResolve, PermReconciliationApprove,
	PermRiskRead, PermRiskPolicyWrite,
	PermGateRead, PermGatePropose, PermGateApprove,
	PermKillActivate, PermKillRelease,
	PermProviderDisable, PermProviderEnable,
	PermInstrumentStatusWrite,
	PermAdminAuditRead,
	PermSessionListOwn, PermSessionRevokeOwn, PermSessionRevokeAny,
	PermBreakGlassRequest, PermBreakGlassApprove,
	PermCreditRead, PermCreditPurchase, PermCreditAdjust,
	PermNativeAssetCreate, PermNativeAssetRead, PermNativeAssetModerate,
	PermNativeMarketTrade, PermNativeMarketHalt, PermNativeMarketSurveil,
	PermPayoutCreate, PermPayoutRead, PermPayoutReview, PermPayoutApprove,
}

var allRoles = []Role{
	RoleCustomer, RoleSupportReadOnly, RoleOperations, RoleRisk,
	RoleCompliance, RoleFinance, RoleSecurity, RoleAdmin, RoleBreakGlass,
}

var allActorTypes = []ActorType{ActorUser, ActorOperator, ActorService, ActorAgent, ActorSystem}

// dualControlPermissions are the approve side of a two-person action. No
// standing role holds them: an ADMIN may propose, and a *different* principal
// holding a live BREAK_GLASS elevation must approve (RequireDualControl).
var dualControlPermissions = []Permission{
	PermPayoutApprove,
	PermCreditAdjust,
	PermLedgerApproveCorrection,
	PermGateApprove,
	PermKillRelease,
	PermWithdrawalApprove,
	PermReconciliationApprove,
	PermEnvelopeApprove,
	PermAgentPromoteApprove,
}

// agentOnlyPermissions are intrinsic to ActorType AGENT and are never
// granted to any role, ADMIN included: a human must not be able to act
// through the agent path.
var agentOnlyPermissions = []Permission{
	PermAgentRun,
	PermPredictionCommit,
	PermIntentCreateAgent,
}

// agentPermissions is the complete set an AGENT principal can ever satisfy
// (PART 9: permitted read tools, prediction commit, typed intent creation)
// and only on the single account it is bound to.
var agentPermissions = []Permission{
	PermAgentRun,
	PermPredictionCommit,
	PermIntentCreateAgent,
	PermAccountRead,
	PermTradeRead,
	PermStrategyRead,
}

// reads are the "*:read" permissions shared by every operator role.
var reads = []Permission{
	PermAccountRead, PermTradeRead, PermFundingRead, PermStrategyRead,
	PermLedgerRead, PermReconciliationRead, PermRiskRead, PermGateRead,
	PermCreditRead, PermNativeAssetRead, PermPayoutRead,
}

// ownSession lets every human manage the sessions of their own subject
// (PART 192: session listing/revocation).
var ownSession = []Permission{PermSessionListOwn, PermSessionRevokeOwn}

// operatorBase is what every staff role starts from: all reads, visibility
// across accounts, and their own sessions.
var operatorBase = union(reads, []Permission{PermAccountReadAny}, ownSession)

// RolePermissions is the static permission matrix. It is a package-level
// value so callers can render it (e.g. an admin UI); mutate it and the golden
// test in matrix_golden_test.go fails. Use PermissionsForRole for a copy.
var RolePermissions = map[Role][]Permission{
	RoleCustomer: union(
		[]Permission{
			PermAccountRead, PermTradeCreate, PermTradeRead, PermFundingCreate, PermFundingRead,
			PermWithdrawalCreate, PermStrategyWrite, PermStrategyRead, PermAgentPause,
			// The internal economy. A customer may hold Credits, create an
			// asset, trade an internal market and ask for a payout. Whether
			// any of that is permitted TODAY is a capability and legal-router
			// question; holding the permission only means the role is the
			// right one to ask.
			PermCreditRead, PermCreditPurchase,
			PermNativeAssetCreate, PermNativeAssetRead,
			PermNativeMarketTrade,
			PermPayoutCreate, PermPayoutRead,
		},
		ownSession,
	),
	RoleSupportReadOnly: operatorBase,
	RoleOperations: union(operatorBase, []Permission{
		PermAgentPause, PermAgentPromote, PermProviderDisable, PermKillActivate, PermInstrumentStatusWrite, PermReconciliationResolve,
		PermNativeMarketHalt, PermNativeMarketSurveil, PermPayoutReview,
	}),
	RoleRisk: union(operatorBase, []Permission{
		PermRiskPolicyWrite, PermKillActivate, PermInstrumentStatusWrite, PermGatePropose, PermEnvelopeAuthorityWrite, PermAgentPromote,
	}),
	RoleCompliance: union(operatorBase, []Permission{
		PermNativeAssetModerate, PermNativeMarketSurveil, PermNativeMarketHalt, PermPayoutReview,
		PermAccountFreeze, PermGatePropose, PermWithdrawalReview,
	}),
	RoleFinance: union(operatorBase, []Permission{
		PermLedgerPostCorrection, PermReconciliationResolve, PermWithdrawalReview,
	}),
	// break_glass:approve is a standing SECURITY/ADMIN permission rather than a
	// dual-control one: the first elevation must be approvable by a second
	// human who does not yet hold an elevation (approver ≠ proposer is still enforced).
	RoleSecurity: union(operatorBase, []Permission{
		PermSessionRevokeAny, PermKillActivate, PermProviderDisable, PermBreakGlassApprove,
	}),
	RoleAdmin:      except(allPermissions, union(dualControlPermissions, agentOnlyPermissions)),
	RoleBreakGlass: sorted(dualControlPermissions),
}

// rolePermSets is the lookup form of RolePermissions, built once from it.
var rolePermSets = func() map[Role]map[Permission]struct{} {
	m := make(map[Role]map[Permission]struct{}, len(RolePermissions))
	for r, ps := range RolePermissions {
		m[r] = toSet(ps)
	}
	return m
}()

var (
	agentPermSet       = toSet(agentPermissions)
	agentOnlyPermSet   = toSet(agentOnlyPermissions)
	dualControlPermSet = toSet(dualControlPermissions)
	allPermSet         = toSet(allPermissions)
)

// AllRoles returns every role, in declaration order.
func AllRoles() []Role { return append([]Role(nil), allRoles...) }

// AllActorTypes returns every actor type, in declaration order.
func AllActorTypes() []ActorType { return append([]ActorType(nil), allActorTypes...) }

// AllPermissions returns every known permission, sorted.
func AllPermissions() []Permission { return sorted(allPermissions) }

// Valid reports whether r is a known role.
func (r Role) Valid() bool {
	_, ok := rolePermSets[r]
	return ok
}

// Valid reports whether a is a known actor type.
func (a ActorType) Valid() bool {
	for _, t := range allActorTypes {
		if t == a {
			return true
		}
	}
	return false
}

// Valid reports whether p is a known permission.
func (p Permission) Valid() bool {
	_, ok := allPermSet[p]
	return ok
}

// PermissionsForRole returns a sorted copy of the role's permissions
// (nil for an unknown role).
func PermissionsForRole(r Role) []Permission {
	ps, ok := RolePermissions[r]
	if !ok {
		return nil
	}
	return sorted(ps)
}

// RoleGrants reports whether the standing matrix grants p to r. It ignores
// break-glass timing; use Principal.Has or RequireAt for a live decision.
func RoleGrants(r Role, p Permission) bool {
	_, ok := rolePermSets[r][p]
	return ok
}

// IsDualControl reports whether p is an approve-side permission that only a
// live BREAK_GLASS elevation held by a principal other than the proposer can
// satisfy.
func IsDualControl(p Permission) bool {
	_, ok := dualControlPermSet[p]
	return ok
}

// DualControlPermissions returns the sorted approve-side permissions.
func DualControlPermissions() []Permission { return sorted(dualControlPermissions) }

// AgentPermissions returns the sorted, complete set an AGENT can satisfy.
func AgentPermissions() []Permission { return sorted(agentPermissions) }

// IsAgentOnly reports whether p is intrinsic to the AGENT actor type and
// therefore never granted to a role.
func IsAgentOnly(p Permission) bool {
	_, ok := agentOnlyPermSet[p]
	return ok
}

func toSet(ps []Permission) map[Permission]struct{} {
	s := make(map[Permission]struct{}, len(ps))
	for _, p := range ps {
		s[p] = struct{}{}
	}
	return s
}

func sorted(ps []Permission) []Permission {
	out := append([]Permission(nil), ps...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// union returns the sorted, de-duplicated union of the given lists.
func union(lists ...[]Permission) []Permission {
	set := map[Permission]struct{}{}
	for _, l := range lists {
		for _, p := range l {
			set[p] = struct{}{}
		}
	}
	out := make([]Permission, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	return sorted(out)
}

// except returns the sorted elements of all that are not in minus.
func except(all, minus []Permission) []Permission {
	skip := toSet(minus)
	out := make([]Permission, 0, len(all))
	for _, p := range all {
		if _, ok := skip[p]; !ok {
			out = append(out, p)
		}
	}
	return sorted(out)
}
