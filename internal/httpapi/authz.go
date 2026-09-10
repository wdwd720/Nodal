package httpapi

import (
	"context"
	"sort"
	"time"

	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
)

// stepUpMaxAge is how recent a strong authentication must be for the
// operations that demand one. It matches the domain packages that enforce
// their own step-up (gates.StepUpMaxAge, killswitch.StepUpMaxAge,
// withdrawal.StepUpMaxAge) so the boundary never contradicts them.
//
// It is the CEILING, not the answer. CP_AUTH_STEP_UP_MAX_AGE tightens it and
// can never loosen it -- see effectiveStepUpMaxAge.
const stepUpMaxAge = 15 * time.Minute

// effectiveStepUpMaxAge is the window the boundary actually enforces: the
// tighter of this package's constant and the deployment's configured value.
//
// CP_AUTH_STEP_UP_MAX_AGE was loaded, validated as positive, and then read by
// nothing at all. Every step-up window in the process was a hard-coded
// constant, so a deployment that set it to 5 minutes -- as render.yaml does --
// was enforcing 15, and an operator tightening it further changed nothing
// (F-89).
//
// Taking the minimum rather than the configured value outright is deliberate.
// The domain packages set their own windows per action, and a sensitive one
// (break-glass, gate activation) is meant to be shorter than the general rule.
// A deployment may make every window stricter; it may not use this variable to
// widen one the code chose.
func effectiveStepUpMaxAge(configured time.Duration) time.Duration {
	if configured > 0 && configured < stepUpMaxAge {
		return configured
	}
	return stepUpMaxAge
}

// operationPolicy is the explicit authorization requirement of one generated
// operation. There is no implicit default: authorize refuses any operation
// that has no entry in operationPolicies, so a newly generated route fails
// closed until someone writes its policy down.
type operationPolicy struct {
	// Public marks an operation reachable without a session. Only the OIDC
	// entry points, the unauthenticated liveness/version probes and the
	// signature-verified webhook endpoint may set it.
	Public bool
	// AnyOf lists permissions of which the principal must hold at least
	// one. Empty is legal only when Public is set.
	AnyOf []security.Permission
	// StepUp requires a recent strong authentication (MFA/passkey).
	StepUp bool
	// Mutating marks a command: it must carry an Idempotency-Key and is
	// subject to CSRF checks.
	Mutating bool
	// AllowAgent lets an AGENT principal through. Nothing sets it: the REST
	// surface is for humans and operators, and an agent that somehow held a
	// session must not reach signing, withdrawal, capital or risk. Agents
	// propose through internal/intent, never over HTTP.
	AllowAgent bool
}

// proposePermissions and approvePermissions are the propose- and approve-side
// permissions of the admin action table. They are derived from
// internal/admin so a new admin kind cannot silently widen or narrow the route
// floor: the per-kind permission is still enforced by internal/admin itself,
// this is only the boundary's explicit "who may reach this route at all".
var proposePermissions, approvePermissions = func() ([]security.Permission, []security.Permission) {
	var propose, approve []security.Permission
	for _, k := range admin.Kinds() {
		spec, ok := admin.Spec(k)
		if !ok {
			continue
		}
		propose = append(propose, spec.ProposePermission)
		if spec.ApprovePermission != "" {
			approve = append(approve, spec.ApprovePermission)
		}
	}
	return dedupe(propose), dedupe(approve)
}()

func dedupe(in []security.Permission) []security.Permission {
	seen := make(map[security.Permission]struct{}, len(in))
	out := make([]security.Permission, 0, len(in))
	for _, p := range in {
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func perms(p ...security.Permission) []security.Permission { return p }

// operationPolicies is the authorization contract of the HTTP surface, keyed
// by the operation id the generated strict server passes to its middleware
// (which is the StrictServerInterface method name). Deny-by-default: an
// operation missing from this map is refused.
//
// Tenant scoping (does this principal own *this* account?) is a separate,
// per-request check made by the handler with security.RequireAccount once the
// account id is known; it cannot be expressed here.
var operationPolicies = map[string]operationPolicy{
	// --- auth -------------------------------------------------------------
	"GetAuthLogin":            {Public: true},
	"GetAuthCallback":         {Public: true},
	"PostAuthLogout":          {AnyOf: perms(security.PermSessionRevokeOwn), Mutating: false},
	"GetMe":                   {AnyOf: perms(security.PermAccountRead, security.PermAccountReadAny)},
	"GetSessions":             {AnyOf: perms(security.PermSessionListOwn)},
	"DeleteSessionsSessionId": {AnyOf: perms(security.PermSessionRevokeOwn)},

	// --- accounts ---------------------------------------------------------
	"GetAccounts":                     {AnyOf: perms(security.PermAccountRead, security.PermAccountReadAny)},
	"GetAccountsAccountId":            {AnyOf: perms(security.PermAccountRead, security.PermAccountReadAny)},
	"GetAccountsAccountIdBuyingPower": {AnyOf: perms(security.PermAccountRead, security.PermAccountReadAny)},
	"GetAccountsAccountIdHoldings":    {AnyOf: perms(security.PermAccountRead, security.PermAccountReadAny)},
	// A customer reads their own journal under account:read plus tenant
	// scoping (RequireAccount); an operator reads any account under
	// ledger:read. The customer role deliberately does not hold ledger:read.
	"GetAccountsAccountIdLedgerTransactions": {AnyOf: perms(security.PermAccountRead, security.PermLedgerRead)},
	"GetAccountsAccountIdActivity":           {AnyOf: perms(security.PermAccountRead, security.PermAccountReadAny)},
	"GetAccountsAccountIdExport":             {AnyOf: perms(security.PermAccountRead, security.PermLedgerRead)},

	// --- market reference data -------------------------------------------
	"GetAssets":                  {AnyOf: perms(security.PermAccountRead, security.PermAccountReadAny)},
	"GetInstruments":             {AnyOf: perms(security.PermAccountRead, security.PermAccountReadAny)},
	"GetInstrumentsInstrumentId": {AnyOf: perms(security.PermAccountRead, security.PermAccountReadAny)},

	// --- Nodal-native economy (gola.md PARTS XII-XXI) ---------------------
	//
	// These are separate permissions from trade:* on purpose: a deployment must
	// be able to grant real-asset trading without granting the launch of
	// speculative internal ones, and the reverse. Tenant scoping is a separate
	// per-request check, as everywhere else.
	"GetCreditsBalance": {AnyOf: perms(security.PermCreditRead)},
	// Reading the rate is not reading anybody's money, but it is still not
	// public: an unauthenticated caller has no business enumerating this
	// deployment's pricing, and credit:read is the narrowest permission that
	// already exists for it.
	"GetCreditsPricing":    {AnyOf: perms(security.PermCreditRead)},
	"GetPaymentsPaymentId": {AnyOf: perms(security.PermCreditRead)},
	"PostPayments":         {AnyOf: perms(security.PermCreditPurchase), Mutating: true},
	"PostNativeAssets":     {AnyOf: perms(security.PermNativeAssetCreate), Mutating: true},
	// Submitting your own draft for review is the same authority as creating
	// it: the creator is asking for a decision, not making one. Who may submit
	// WHICH asset is an ownership question and is answered per request, not by
	// a permission.
	"PostNativeAssetsAssetIdSubmit": {AnyOf: perms(security.PermNativeAssetCreate), Mutating: true},
	// Cancelling your own payout is the same authority as creating one. WHICH
	// payout you may cancel is an ownership question, answered per request.
	"PostPayoutsPayoutIdCancel": {AnyOf: perms(security.PermPayoutCreate), Mutating: true},
	"GetNativeAssets":           {AnyOf: perms(security.PermNativeAssetRead)},
	"GetNativeAssetsAssetId":    {AnyOf: perms(security.PermNativeAssetRead)},
	"GetNativeMarketsMarketId":  {AnyOf: perms(security.PermNativeAssetRead)},
	// A native-market quote is persisted -- it is the record of what the user
	// was shown, with the state version it was priced against -- so it is a
	// command with an idempotency key, not a read that happens to write.
	"PostNativeMarketsMarketIdQuotes": {AnyOf: perms(security.PermNativeMarketTrade), Mutating: true},
	"PostNativeMarketsMarketIdOrders": {AnyOf: perms(security.PermNativeMarketTrade), Mutating: true},
	"PostPayouts":                     {AnyOf: perms(security.PermPayoutCreate), Mutating: true},
	"GetPayouts":                      {AnyOf: perms(security.PermPayoutRead)},
	"GetPayoutsPayoutId":              {AnyOf: perms(security.PermPayoutRead)},

	// --- markets, charts, portfolio and activity (product goal SS12-16, 35) -
	//
	// Discovery, the chart and the tape are the same authority as reading a
	// native asset: they are public market data about assets anyone with
	// native_asset:read may already list, and none of them names an account.
	// The tape deliberately carries no account id, so it cannot become a way
	// to watch a particular trader.
	"GetNativeMarkets":                {AnyOf: perms(security.PermNativeAssetRead)},
	"GetNativeMarketsMarketIdSummary": {AnyOf: perms(security.PermNativeAssetRead)},
	"GetNativeMarketsMarketIdCandles": {AnyOf: perms(security.PermNativeAssetRead)},
	"GetNativeMarketsMarketIdTrades":  {AnyOf: perms(security.PermNativeAssetRead)},
	// The portfolio returns a Credit balance, so it needs the permission that
	// reads one. Which account is a per-request tenant check (accountScope),
	// as everywhere else.
	"GetMePortfolio": {AnyOf: perms(security.PermCreditRead)},
	// The timeline is the account's own history, so it is the same authority
	// as GET /accounts/{id}/activity.
	"GetMeActivity": {AnyOf: perms(security.PermAccountRead, security.PermAccountReadAny)},

	// --- internal commerce (gola.md PART XVII) ----------------------------
	//
	// Buying and selling are separate permissions because they are different
	// exposures. A buyer spends Credits; a seller MINTS the earning provenance
	// that a payout policy may one day permit to be withdrawn. A deployment
	// that has not decided how it feels about creator payouts can let people
	// buy from each other while granting nobody the ability to sell.
	"PostInternalSellers":                 {AnyOf: perms(security.PermCommerceSell), Mutating: true},
	"PostInternalProducts":                {AnyOf: perms(security.PermCommerceSell), Mutating: true},
	"PostInternalProductsProductIdStatus": {AnyOf: perms(security.PermCommerceSell), Mutating: true},
	"GetInternalProducts":                 {AnyOf: perms(security.PermCommerceRead)},
	"GetInternalProductsProductId":        {AnyOf: perms(security.PermCommerceRead)},
	"PostInternalProductsProductIdOrders": {AnyOf: perms(security.PermCommerceBuy), Mutating: true},
	"GetInternalOrders":                   {AnyOf: perms(security.PermCommerceRead)},

	// --- trading ----------------------------------------------------------
	"PostQuotesPreview":         {AnyOf: perms(security.PermTradeRead)},
	"PostIntents":               {AnyOf: perms(security.PermTradeCreate), Mutating: true},
	"GetIntents":                {AnyOf: perms(security.PermTradeRead)},
	"GetIntentsIntentId":        {AnyOf: perms(security.PermTradeRead)},
	"PostIntentsIntentIdCancel": {AnyOf: perms(security.PermTradeCreate), Mutating: true},
	"GetOrders":                 {AnyOf: perms(security.PermTradeRead)},
	"GetOrdersOrderId":          {AnyOf: perms(security.PermTradeRead)},

	// --- funding ----------------------------------------------------------
	"PostFundingDeposits":         {AnyOf: perms(security.PermFundingCreate), Mutating: true},
	"GetFundingDeposits":          {AnyOf: perms(security.PermFundingRead)},
	"GetFundingDepositsDepositId": {AnyOf: perms(security.PermFundingRead)},

	// --- withdrawals ------------------------------------------------------
	// A withdrawal always needs a human actor and a recent step-up; the
	// WITHDRAWALS capability gate refuses it beyond that (PART 94).
	"PostWithdrawals": {AnyOf: perms(security.PermWithdrawalCreate), StepUp: true, Mutating: true},

	// --- realtime ---------------------------------------------------------
	"GetEventsStream": {AnyOf: perms(security.PermAccountRead, security.PermAccountReadAny)},

	// --- admin ------------------------------------------------------------
	"GetAdminAccounts":                 {AnyOf: perms(security.PermAccountReadAny)},
	"PostAdminAccountsAccountIdStatus": {AnyOf: perms(security.PermAccountFreeze), StepUp: true, Mutating: true},
	"GetAdminGates":                    {AnyOf: perms(security.PermGateRead)},
	"PostAdminGatesCapabilityAction": {
		AnyOf: perms(security.PermGatePropose, security.PermGateApprove), StepUp: true, Mutating: true,
	},
	"GetAdminKillSwitches": {AnyOf: perms(security.PermRiskRead, security.PermGateRead)},
	// Activation must be fast: one operator with kill:activate and no
	// step-up (POLICY_AUTHORITY §2). Release additionally requires step-up
	// and, for SEVERE switches, an approved admin action; internal/killswitch
	// enforces both.
	"PostAdminKillSwitches": {AnyOf: perms(security.PermKillActivate, security.PermKillRelease), Mutating: true},
	"GetAdminActions":       {AnyOf: admin.ReadPermissions()},
	"PostAdminActions":      {AnyOf: proposePermissions, StepUp: true, Mutating: true},
	"PostAdminActionsActionIdDecision": {
		AnyOf:  append(append([]security.Permission{}, approvePermissions...), proposePermissions...),
		StepUp: true, Mutating: true,
	},
	"PostAdminInstrumentsInstrumentIdStatus": {
		AnyOf: perms(security.PermInstrumentStatusWrite), StepUp: true, Mutating: true,
	},
	"GetAdminProviders":             {AnyOf: perms(security.PermAdminAuditRead)},
	"GetAdminReconciliationRecords": {AnyOf: perms(security.PermReconciliationRead)},
	"PostAdminReconciliationRecordsRecordIdResolve": {
		AnyOf: perms(security.PermReconciliationResolve), StepUp: true, Mutating: true,
	},

	// --- system -----------------------------------------------------------
	"GetHealthz": {Public: true},
	"GetReadyz":  {Public: true},
	"GetVersion": {Public: true},
	// The webhook endpoint carries no session. Its authority is the
	// provider signature, verified over the raw bytes by internal/webhook
	// before anything is persisted beyond a security event.
	"PostWebhooksProvider": {Public: true},
}

// policyFor returns the policy for operationID, or false when there is none.
func policyFor(operationID string) (operationPolicy, bool) {
	p, ok := operationPolicies[operationID]
	return p, ok
}

// authorize enforces the operation's policy against the principal in ctx. It
// is the single deny-by-default gate: an unknown operation id is FORBIDDEN.
func authorize(ctx context.Context, operationID string, now func() time.Time, maxStepUpAge time.Duration) error {
	pol, ok := policyFor(operationID)
	if !ok {
		return errs.Newf(errs.CodeForbidden, "operation %q has no authorization policy", operationID)
	}
	if pol.Public {
		return nil
	}
	if len(pol.AnyOf) == 0 {
		// A non-public operation with no permission requirement is a
		// programming error; fail closed rather than let it through.
		return errs.Newf(errs.CodeForbidden, "operation %q declares no permission requirement", operationID)
	}
	p, ok := security.PrincipalFrom(ctx)
	if !ok {
		return errs.New(errs.CodeUnauthenticated, "authentication is required")
	}
	if p.IsAgent() && !pol.AllowAgent {
		return errs.New(errs.CodeForbidden, "agents may not call the HTTP API")
	}
	if err := security.RequireAnyAt(ctx, now, pol.AnyOf...); err != nil {
		return err
	}
	if pol.StepUp {
		if err := security.RequireStepUp(ctx, maxStepUpAge, now); err != nil {
			return err
		}
	}
	return nil
}
