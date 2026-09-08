package settlement

import (
	"sort"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/agentauthority"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// The Settlement Compiler's rail dispatch (gola.md PART XXVI).
//
// # What this adds to the V1 planner
//
// V1Planner compiles ONE rail shape: a self-custodial on-chain spot swap. That
// was the whole system when it was written. It is not the whole system now:
// Domain A executes on Nodal's own ledger, Domain B against simulated markets,
// and a payout leaves the system entirely. Those are four different settlement
// models with four different authoritative balance sources, and a planner that
// knows only one of them cannot be "the only route from a typed intent to
// execution" -- which is the property the compiler exists to have.
//
// Compile is the layer in front: it takes a FinancialIntent and decides,
// deterministically, everything PART XXVI lists -- value domain, legal rail,
// provider, market, eligibility, asset status, policy, reservation
// requirements, quote requirements, risk evaluation, execution authority,
// required confirmation, reconciliation method -- and returns the Route that
// says which executor may run it and under what conditions. The V1 planner
// then does its unchanged job for the one rail it serves.
//
// # Why a Route rather than a Plan for internal rails
//
// A plan with durable per-step state exists because an external settlement can
// be half-done: a transaction can be submitted and its result unknown. An
// internal-ledger settlement cannot be half-done -- it is one database
// transaction that either commits or does not -- so a seventeen-step DAG
// around it would be ceremony that adds failure modes rather than removing
// them. What must NOT be skipped is everything IN FRONT of execution, and that
// is exactly what Route carries.
//
// # What this must never do
//
//   - Infer the value domain from the subject. The intent declares it; a
//     mismatch is a refusal, never a correction.
//   - Select a rail this build cannot execute on, whatever a policy says.
//   - Return a permitting Route when the legal router denied, when the
//     capability gate is off, or when an agent is acting beyond its authority.
//   - Consult a clock, a database or a network. Compile is pure: same inputs,
//     same Route, forever.

// ExecutorKind names which machinery may run a compiled intent. It is part of
// the Route rather than inferred later so that "who executes this" is a
// compiler decision with a reason attached.
type ExecutorKind string

// Executor kinds.
const (
	// ExecutorExternalPlan is the V1 durable plan executor: build, sign,
	// submit, observe finality, reconcile.
	ExecutorExternalPlan ExecutorKind = "EXTERNAL_PLAN"
	// ExecutorInternalAtomic is a single committed database transaction
	// against Nodal's own ledger. There is no partial state to recover.
	ExecutorInternalAtomic ExecutorKind = "INTERNAL_ATOMIC"
	// ExecutorPayout is the payout state machine, which is neither of the
	// above: it reserves internally, calls an external provider, and may end
	// in PAYOUT_STATUS_UNKNOWN.
	ExecutorPayout ExecutorKind = "PAYOUT"
	// ExecutorSimulated is the simulated-market engine. Nothing of value
	// moves and no provider is involved.
	ExecutorSimulated ExecutorKind = "SIMULATED"
	// ExecutorNone is what a refused intent gets. It is a value rather than
	// an empty string so that a Route which permits nothing still says so.
	ExecutorNone ExecutorKind = "NONE"
)

// ConfirmationKind is what a human must do before execution.
type ConfirmationKind string

// Confirmation kinds.
const (
	// ConfirmNone means the request itself is the authorisation.
	ConfirmNone ConfirmationKind = "NONE"
	// ConfirmUserReview means a person must look at this and say yes. It is
	// what the legal router's REQUIRES_USER_CONFIRMATION becomes.
	ConfirmUserReview ConfirmationKind = "USER_CONFIRMATION"
	// ConfirmManualReview means an operator has to look before this proceeds.
	// It is distinct from user confirmation: the person who must act is not
	// the person who asked.
	ConfirmManualReview ConfirmationKind = "MANUAL_REVIEW"
)

// Refusal reason codes. They are strings on Route rather than an enum shared
// with the V1 planner's reason set because these are compiler-level refusals:
// they say the intent may not be executed at all, not that no venue was
// found.
const (
	ReasonUnknownAction            = "UNKNOWN_ACTION_TYPE"
	ReasonDomainRailMismatch       = "DOMAIN_DOES_NOT_LIVE_ON_THIS_RAIL"
	ReasonDeclaredDomainMismatch   = "DECLARED_DOMAIN_DOES_NOT_MATCH_ACTION"
	ReasonRailNotImplemented       = "RAIL_NOT_IMPLEMENTED"
	ReasonLegalRouterDenied        = "LEGAL_ROUTER_DENIED"
	ReasonCapabilityNotActive      = "CAPABILITY_NOT_ACTIVE"
	ReasonVerificationRequired     = "VERIFICATION_REQUIRED"
	ReasonAgentActionForbidden     = "AGENT_ACTION_PERMANENTLY_FORBIDDEN"
	ReasonAgentAuthorityInadequate = "AGENT_AUTHORITY_INSUFFICIENT"
	ReasonDeadlinePassed           = "DEADLINE_PASSED"
	ReasonSubjectAssetMissing      = "SUBJECT_ASSET_NOT_STATED"
	ReasonProviderRequired         = "NO_CONFIGURED_PROVIDER_CAN_DO_THIS"
)

// Route is what the compiler decided. It is the complete answer to "may this
// happen, on what, by whom, and what has to be true first".
type Route struct {
	// Permitted is the single question every caller actually asks. It is
	// false whenever Reasons is non-empty; the two can never disagree.
	Permitted bool

	ValueDomain valuedomain.Domain
	Rail        valuedomain.CapitalRail
	// Product is the legal router's product name for this action.
	Product  string
	Provider string
	Executor ExecutorKind

	// RequiredCapabilities are every gate that must be ACTIVE. It is a list
	// rather than one key because a native-market trade needs both the
	// product's own gate and the conversion's.
	RequiredCapabilities []valuedomain.CapabilityKey
	RequiredVerification valuedomain.VerificationLevel
	RequiredConfirmation ConfirmationKind

	// RequiresQuote, RequiresReservation and RequiresRiskEvaluation are the
	// PART XXVI determinations that shape execution rather than gate it.
	RequiresQuote          bool
	RequiresReservation    bool
	RequiresRiskEvaluation bool

	// AuthoritativeBalanceSource and ReconciliationMethod come from the rail.
	// They are copied onto the Route so that a stored decision records what
	// was believed at the time, rather than what the rail registry says
	// later.
	AuthoritativeBalanceSource string
	ReconciliationMethod       string

	// MinAgentAuthority is the LOWEST authority level at which an agent may
	// take this action at all -- the floor, not a ceiling. Zero is a
	// meaningful value (RESEARCH_ONLY), so it cannot double as "unset";
	// AgentMayAct is what says whether the agent in this request may act.
	//
	// It is absent for actions no agent may ever take: a permanently
	// forbidden action has no minimum level, because there is no level that
	// permits it.
	MinAgentAuthority agentauthority.Level
	AgentMayAct       bool

	// Legal is the router's decision, kept whole so an operator can trace the
	// answer to a rule index in a policy version.
	Legal legalrouter.Decision

	// Reasons are every refusal that applies, sorted, so a caller is told
	// everything wrong at once rather than one thing per attempt.
	Reasons []string
}

// Refused reports whether the route forbids execution outright.
func (r Route) Refused() bool { return !r.Permitted }

// MayExecuteNow reports whether the intent may be executed without anything
// further happening first. A route that is Permitted but requires a
// confirmation is NOT one of these: it is permission to ask a person, and a
// caller that treats it as permission to move value has skipped the step the
// policy exists to insert.
func (r Route) MayExecuteNow() bool {
	return r.Permitted && r.RequiredConfirmation == ConfirmNone
}

// HasReason reports whether code is among the refusal reasons.
func (r Route) HasReason(code string) bool {
	for _, c := range r.Reasons {
		if c == code {
			return true
		}
	}
	return false
}

// actionProfile is the compile-time fact table: everything about an action
// that does not depend on the request. Keeping it as data rather than a switch
// is what makes TestCompile_EveryActionTypeHasAProfile able to prove the table
// is total.
type actionProfile struct {
	domain  valuedomain.Domain
	rail    valuedomain.CapitalRail
	product string
	// capabilities are the gates required in addition to whatever the legal
	// router's matched rule requires.
	capabilities []valuedomain.CapabilityKey
	executor     ExecutorKind
	// agentAction is what an agent doing this is really doing, in the
	// authority ladder's vocabulary. REQUEST_PAYOUT maps to WITHDRAW, which
	// is permanently forbidden -- that is the point of stating it here rather
	// than remembering to check it at the call site.
	agentAction            agentauthority.Action
	minVerification        valuedomain.VerificationLevel
	requiresQuote          bool
	requiresReservation    bool
	requiresRiskEvaluation bool
	payoutMode             string
}

// profiles is the compiler's routing table. Every declared action type must
// appear exactly once; Validate proves it.
var profiles = map[ActionType]actionProfile{
	ActionBuyNativeAsset: {
		domain: valuedomain.InternalNativeAsset, rail: valuedomain.RailNativeInternal,
		product:      legalrouter.ProductNativeMarketTrade,
		capabilities: []valuedomain.CapabilityKey{valuedomain.CapNativeMarketTrading},
		executor:     ExecutorInternalAtomic,
		agentAction:  agentauthority.ActionExecuteApprovedRule,
		// No financial identity: buying a Nodal-native asset with Credits
		// moves nothing across a regulated boundary. Whether it may happen at
		// all is the legal router's question, and it denies by default.
		minVerification:        valuedomain.VerificationNodalIdentity,
		requiresQuote:          true,
		requiresReservation:    false,
		requiresRiskEvaluation: true,
		payoutMode:             legalrouter.PayoutModeNone,
	},
	ActionSellNativeAsset: {
		domain: valuedomain.InternalNativeAsset, rail: valuedomain.RailNativeInternal,
		product:      legalrouter.ProductNativeMarketTrade,
		capabilities: []valuedomain.CapabilityKey{valuedomain.CapNativeMarketTrading},
		executor:     ExecutorInternalAtomic,
		agentAction:  agentauthority.ActionExecuteApprovedRule,
		// Selling is an exit. It carries the same verification requirement as
		// buying and no more: a rule that let people in and not out would
		// trap value, which PART XXXII forbids.
		minVerification:        valuedomain.VerificationNodalIdentity,
		requiresQuote:          true,
		requiresRiskEvaluation: true,
		payoutMode:             legalrouter.PayoutModeNone,
	},
	ActionCreateNativeAsset: {
		domain: valuedomain.InternalNativeAsset, rail: valuedomain.RailNativeInternal,
		product:      legalrouter.ProductNativeAssetCreate,
		capabilities: []valuedomain.CapabilityKey{"NATIVE_ASSET_CREATION"},
		executor:     ExecutorInternalAtomic,
		// Creating an asset is a publication with the creator's name on it.
		// An agent may PREPARE one; the conservative policy then requires a
		// person to confirm it.
		agentAction:            agentauthority.ActionPrepareTransaction,
		minVerification:        valuedomain.VerificationNodalIdentity,
		requiresRiskEvaluation: false,
		payoutMode:             legalrouter.PayoutModeNone,
	},
	ActionPurchaseInternalService: {
		domain: valuedomain.InternalCredit, rail: valuedomain.RailNativeInternal,
		product: legalrouter.ProductInternalCommerce,
		// MARKETPLACE is the gate for users transacting with each other. It
		// was declared long before internal commerce existed and is exactly
		// this: a deployment that has not decided how it feels about a
		// user-to-user marketplace has not enabled one.
		capabilities:           []valuedomain.CapabilityKey{"MARKETPLACE"},
		executor:               ExecutorInternalAtomic,
		agentAction:            agentauthority.ActionExecuteApprovedRule,
		minVerification:        valuedomain.VerificationNodalIdentity,
		requiresRiskEvaluation: false,
		payoutMode:             legalrouter.PayoutModeNone,
	},
	ActionRequestPayout: {
		domain: valuedomain.PayoutPending, rail: valuedomain.RailNativeInternal,
		product: legalrouter.ProductPayout,
		capabilities: []valuedomain.CapabilityKey{
			valuedomain.CapPayoutReserve, valuedomain.CapPayoutSettle,
		},
		executor: ExecutorPayout,
		// An agent may never withdraw. Stating it here means the compiler
		// refuses an agent payout without anyone having to remember to check.
		agentAction:            agentauthority.ActionWithdraw,
		minVerification:        valuedomain.VerificationPayoutKYC,
		requiresReservation:    true,
		requiresRiskEvaluation: true,
		payoutMode:             legalrouter.PayoutModePartnerFiat,
	},
	ActionBuyHostedAsset: {
		domain: valuedomain.HostedCrypto, rail: valuedomain.RailHostedPartner,
		product:      legalrouter.ProductHostedTrade,
		capabilities: []valuedomain.CapabilityKey{valuedomain.CapHostedTrading},
		executor:     ExecutorExternalPlan,
		agentAction:  agentauthority.ActionExecuteApprovedRule,
		// The rail itself requires financial identity; stating it again here
		// makes the requirement visible in the Route rather than only in the
		// rail registry.
		minVerification:        valuedomain.VerificationPayoutKYC,
		requiresQuote:          true,
		requiresReservation:    true,
		requiresRiskEvaluation: true,
		payoutMode:             legalrouter.PayoutModeNone,
	},
	ActionSellHostedAsset: {
		domain: valuedomain.HostedCrypto, rail: valuedomain.RailHostedPartner,
		product:                legalrouter.ProductHostedTrade,
		capabilities:           []valuedomain.CapabilityKey{valuedomain.CapHostedTrading},
		executor:               ExecutorExternalPlan,
		agentAction:            agentauthority.ActionExecuteApprovedRule,
		minVerification:        valuedomain.VerificationPayoutKYC,
		requiresQuote:          true,
		requiresReservation:    true,
		requiresRiskEvaluation: true,
		payoutMode:             legalrouter.PayoutModeNone,
	},
	ActionBuyOnchainAsset: {
		domain: valuedomain.SelfCustodialCrypto, rail: valuedomain.RailSelfCustodialOnchain,
		product:                legalrouter.ProductSelfCustodialTrade,
		capabilities:           []valuedomain.CapabilityKey{"LIVE_MANUAL_TRADING"},
		executor:               ExecutorExternalPlan,
		agentAction:            agentauthority.ActionExecuteApprovedRule,
		minVerification:        valuedomain.VerificationNodalIdentity,
		requiresQuote:          true,
		requiresReservation:    true,
		requiresRiskEvaluation: true,
		payoutMode:             legalrouter.PayoutModeNone,
	},
	ActionSellOnchainAsset: {
		domain: valuedomain.SelfCustodialCrypto, rail: valuedomain.RailSelfCustodialOnchain,
		product:                legalrouter.ProductSelfCustodialTrade,
		capabilities:           []valuedomain.CapabilityKey{"LIVE_MANUAL_TRADING"},
		executor:               ExecutorExternalPlan,
		agentAction:            agentauthority.ActionExecuteApprovedRule,
		minVerification:        valuedomain.VerificationNodalIdentity,
		requiresQuote:          true,
		requiresReservation:    true,
		requiresRiskEvaluation: true,
		payoutMode:             legalrouter.PayoutModeNone,
	},
	ActionSimulatedTrade: {
		domain: valuedomain.Simulated, rail: valuedomain.RailSimulated,
		product: legalrouter.ProductSimulation,
		// No capability. Simulated capital has no economic substance, and
		// gating it would mean a fresh deployment could not even demonstrate
		// itself.
		capabilities:           nil,
		executor:               ExecutorSimulated,
		agentAction:            agentauthority.ActionExecuteApprovedRule,
		minVerification:        valuedomain.VerificationNone,
		requiresQuote:          true,
		requiresReservation:    true,
		requiresRiskEvaluation: true,
		payoutMode:             legalrouter.PayoutModeNone,
	},
}

// Profile returns the compile-time facts for an action type.
// AllRequiredCapabilities returns every capability any action profile can
// require, sorted and de-duplicated.
//
// It is exported so a deployment can CHECK that whatever answers "which
// capabilities are active" answers about all of them. A resolver that is
// silent on a capability reports it inactive, which is fail-closed and also
// unsatisfiable: the gate can be ACTIVE in the database, enabled in
// configuration, and the action still refused, with nothing in the refusal
// saying why. That happened to MARKETPLACE (F-26).
func AllRequiredCapabilities() []valuedomain.CapabilityKey {
	seen := map[valuedomain.CapabilityKey]bool{}
	for _, p := range profiles {
		for _, c := range p.capabilities {
			seen[c] = true
		}
	}
	out := make([]valuedomain.CapabilityKey, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func Profile(a ActionType) (valuedomain.Domain, valuedomain.CapitalRail, string, bool) {
	p, ok := profiles[a]
	return p.domain, p.rail, p.product, ok
}

// CompilerInput is everything Compile needs. Every field is resolved by the
// caller from deployment state; none of it is client input, and none of it is
// read from a clock or a database inside Compile.
type CompilerInput struct {
	Intent FinancialIntent
	// Router evaluates the deployment's legal policy. A nil router is a
	// programming error rather than a permissive default, and Compile returns
	// an error for it.
	Router *legalrouter.Router
	// ActiveCapabilities is the set of gates that are ACTIVE right now. A nil
	// map means none are, which is the correct reading of a fresh deployment.
	ActiveCapabilities map[valuedomain.CapabilityKey]bool
	// Now is the compiler's notion of the present, supplied so that Compile
	// stays pure.
	Now time.Time
}

// Compile decides the route for a financial intent.
//
// It returns an error only when it was asked something malformed -- an invalid
// intent, or no router. A refusal on policy grounds is a Route with
// Permitted=false and every applicable reason, because "you may not do this,
// and here is everything that would have to change" is an answer, not a
// failure.
func Compile(in CompilerInput) (Route, error) {
	if in.Router == nil {
		return Route{}, errs.New(errs.CodeInternal,
			"settlement: Compile requires a legal router; a missing policy is not a permissive one")
	}
	fi := in.Intent
	if err := fi.Validate(); err != nil {
		return Route{}, err
	}

	prof, ok := profiles[fi.ActionType]
	if !ok {
		// Unreachable while Validate covers the same set, and answered
		// anyway: an action with no profile is refused, never defaulted.
		return Route{
			Executor: ExecutorNone,
			Reasons:  []string{ReasonUnknownAction},
		}, nil
	}

	r := Route{
		ValueDomain:                prof.domain,
		Rail:                       prof.rail,
		Product:                    prof.product,
		Provider:                   strings.TrimSpace(fi.Provider),
		Executor:                   prof.executor,
		RequiredCapabilities:       append([]valuedomain.CapabilityKey(nil), prof.capabilities...),
		RequiredVerification:       prof.minVerification,
		RequiredConfirmation:       ConfirmNone,
		RequiresQuote:              prof.requiresQuote,
		RequiresReservation:        prof.requiresReservation,
		RequiresRiskEvaluation:     prof.requiresRiskEvaluation,
		AuthoritativeBalanceSource: prof.rail.AuthoritativeBalanceSource(),
		ReconciliationMethod:       prof.rail.ReconciliationModel(),
	}
	var reasons []string
	add := func(code string) { reasons = append(reasons, code) }

	// --- 1. The declared domain must be the action's domain ---------------
	//
	// The intent states which pot of money this comes out of. If it disagrees
	// with what the action actually touches, that is a refusal: a client that
	// can be wrong about this is a client that can spend the wrong thing.
	if fi.CapitalDomain != prof.domain {
		add(ReasonDeclaredDomainMismatch)
	}
	// And the domain must actually live on the rail. This cannot currently
	// fail given the table, and it is checked because the table is data: an
	// edit that put a domain on the wrong rail would otherwise route real
	// value through the wrong settlement model.
	if prof.domain.Rail() != prof.rail {
		add(ReasonDomainRailMismatch)
	}

	// --- 2. The rail must be one this build can execute on -----------------
	//
	// Before any policy question. PART XXII: do not implement live
	// unsupported products merely because an interface exists -- and do not
	// route to one because a policy row says yes.
	if !prof.rail.Implemented() {
		add(ReasonRailNotImplemented)
	}

	// --- 3. Agent authority ------------------------------------------------
	if fi.ActorType == security.ActorAgent {
		dec := agentauthority.Permits(fi.AgentAuthority, prof.agentAction, in.ActiveCapabilities)
		r.AgentMayAct = dec.Allowed
		if forbidden, _ := agentauthority.ForbiddenAlways(prof.agentAction); forbidden {
			add(ReasonAgentActionForbidden)
		} else if !dec.Allowed {
			add(ReasonAgentAuthorityInadequate)
		}
	} else {
		// A human is not an agent; the ladder does not apply to them.
		r.AgentMayAct = false
	}
	if min, ok := agentauthority.MinimumLevel(prof.agentAction); ok {
		r.MinAgentAuthority = min
	}

	// --- 4. Verification ---------------------------------------------------
	if !verificationSatisfies(fi.Verification, prof.minVerification) {
		add(ReasonVerificationRequired)
	}

	// --- 5. The legal router ------------------------------------------------
	key := legalrouter.Key{
		Jurisdiction:   orUnknown(fi.Jurisdiction),
		Provider:       orNone(fi.Provider),
		Rail:           string(prof.rail),
		Product:        prof.product,
		Asset:          orNone(fi.Subject.AssetID),
		AgentAuthority: authorityName(fi),
		ValueOrigin:    orNone(string(fi.ValueOrigin)),
		PayoutMode:     firstNonEmpty(fi.PayoutMode, prof.payoutMode),
		Compensation:   orNone(fi.Compensation),
		Verification:   string(fi.Verification),
	}
	r.Legal = in.Router.Route(key, in.ActiveCapabilities)
	// Every router outcome is handled, and the default DENIES. An outcome
	// nobody thought about must not fall through to permission -- which is
	// the same rule the router's own catch-all enforces one level down.
	switch r.Legal.Outcome {
	case legalrouter.Allow:
	case legalrouter.RequiresUserConfirmation:
		// Permitted, but only after a person presses the button. Permitted
		// and RequiredConfirmation are separate answers; MayExecuteNow is the
		// conjunction, and callers that execute must use that.
		r.RequiredConfirmation = ConfirmUserReview
	case legalrouter.RequiresManualReview:
		r.RequiredConfirmation = ConfirmManualReview
	case legalrouter.RequiresVerification:
		// The router is saying a higher verification level would permit this.
		// That is the same answer the verification check gives, and it is
		// recorded as such rather than as a flat denial, because it has a
		// next step the user can take.
		add(ReasonVerificationRequired)
	case legalrouter.RequiresProvider:
		add(ReasonProviderRequired)
	default:
		// A DENY the router produced ONLY because a gate is off is not a
		// policy refusal: the policy said yes. Reporting it as
		// LEGAL_ROUTER_DENIED would send an operator to change a policy that
		// is already correct, when the actual next step is activating the
		// capability -- which the gate check below reports with the
		// capability named.
		if r.Legal.ReasonCode != "CAPABILITY_NOT_ACTIVE" {
			add(ReasonLegalRouterDenied)
		}
	}
	// The router's own required capability joins the profile's, deduplicated
	// and sorted so the Route is stable.
	if r.Legal.RequiredCapability != "" {
		r.RequiredCapabilities = append(r.RequiredCapabilities, r.Legal.RequiredCapability)
	}
	r.RequiredCapabilities = sortedUniqueCaps(r.RequiredCapabilities)

	// --- 6. Every required gate must be ACTIVE ------------------------------
	for _, c := range r.RequiredCapabilities {
		if !in.ActiveCapabilities[c] {
			add(ReasonCapabilityNotActive)
			break
		}
	}

	// --- 7. The deadline ----------------------------------------------------
	if !fi.Deadline.IsZero() && !in.Now.IsZero() && !fi.Deadline.After(in.Now) {
		add(ReasonDeadlinePassed)
	}

	// --- 8. A traded subject must name its asset ----------------------------
	//
	// The legal router keys on the asset. Routing a trade without one would
	// mean matching a wildcard rule that was written for a different asset.
	if prof.requiresQuote && strings.TrimSpace(fi.Subject.AssetID) == "" {
		add(ReasonSubjectAssetMissing)
	}

	sort.Strings(reasons)
	r.Reasons = dedupeStrings(reasons)
	r.Permitted = len(r.Reasons) == 0
	if !r.Permitted {
		r.Executor = ExecutorNone
	}
	return r, nil
}

// verificationSatisfies reports whether have meets want. The levels are
// ordered; anything unrecognised is treated as satisfying nothing, because an
// unknown level is not evidence of identity.
func verificationSatisfies(have, want valuedomain.VerificationLevel) bool {
	rank := map[valuedomain.VerificationLevel]int{
		valuedomain.VerificationNone:          0,
		valuedomain.VerificationNodalIdentity: 1,
		valuedomain.VerificationPayoutKYC:     2,
		valuedomain.VerificationEnhanced:      3,
	}
	h, okH := rank[have]
	w, okW := rank[want]
	if !okW {
		return false
	}
	if !okH {
		return w == 0
	}
	return h >= w
}

func authorityName(fi FinancialIntent) string {
	if fi.ActorType != security.ActorAgent {
		return "NONE"
	}
	return fi.AgentAuthority.Name()
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "NONE"
	}
	return s
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "UNKNOWN"
	}
	return s
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	if strings.TrimSpace(b) != "" {
		return b
	}
	return "NONE"
}

func sortedUniqueCaps(in []valuedomain.CapabilityKey) []valuedomain.CapabilityKey {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[valuedomain.CapabilityKey]struct{}, len(in))
	out := make([]valuedomain.CapabilityKey, 0, len(in))
	for _, c := range in {
		if _, dup := seen[c]; dup || c == "" {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := in[:0:0]
	var last string
	for i, s := range in {
		if i > 0 && s == last {
			continue
		}
		out = append(out, s)
		last = s
	}
	return out
}

// ValidateCompiler checks the routing table's totality and internal
// consistency. It is called by a test and by the composition root, so a table
// edit that leaves an action undecided cannot reach production.
func ValidateCompiler() error {
	var problems []string
	for _, a := range AllActionTypes() {
		p, ok := profiles[a]
		if !ok {
			problems = append(problems, "action "+string(a)+" has no routing profile")
			continue
		}
		if !p.domain.Valid() {
			problems = append(problems, "action "+string(a)+" routes to an unknown value domain")
		}
		if !p.rail.Valid() {
			problems = append(problems, "action "+string(a)+" routes to an unknown rail")
		}
		if p.domain.Rail() != p.rail {
			problems = append(problems, "action "+string(a)+" routes to a rail its domain does not live on")
		}
		if p.product == "" {
			problems = append(problems, "action "+string(a)+" has no legal-router product")
		}
		if !p.agentAction.Valid() {
			problems = append(problems, "action "+string(a)+" maps to an unknown agent action")
		}
		if p.executor == "" || p.executor == ExecutorNone {
			problems = append(problems, "action "+string(a)+" has no executor")
		}
	}
	for a := range profiles {
		if !a.Valid() {
			problems = append(problems, "profile for undeclared action "+string(a))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return errs.New(errs.CodeInternal, "settlement: routing table is inconsistent").
			WithField("problems", problems)
	}
	return nil
}
