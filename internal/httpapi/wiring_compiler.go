package httpapi

import (
	"context"
	"strings"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/settlement"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Routing Domain A through the Settlement Compiler (gola.md PARTS XXV-XXVI).
//
// # What changed and why it matters
//
// Before this, a native-market trade, an internal purchase and a payout each
// reached their domain service directly from an HTTP handler. Each had its own
// checks -- the ledger's domain isolation for the trade, the marketplace gate
// for the purchase, the eligibility engine for the payout -- and each was
// sound on its own. What none of them had was the thing PART XXVI actually
// asks for: ONE place that decides value domain, legal rail, provider,
// eligibility, policy, reservation and quote requirements, execution
// authority, required confirmation and reconciliation method, for every action
// in the system, from one table.
//
// compileRoute is that place for the internal economy. Every Domain A command
// now passes through it before its domain service is touched, and a refusal
// carries the policy version, the rule index that produced it and every reason
// at once.
//
// # This does not replace the deeper checks
//
// It sits in front of them. The ledger still refuses a cross-domain posting
// with no capability; internal/commerce still refuses a purchase with the
// marketplace gate off; the payout engine still decides eligibility per unit
// of provenance. A gate that exists only at the edge is one a worker walks
// around, so the edge check is an addition and never a substitution.

// JurisdictionResolver reports which jurisdiction's rules apply to an account.
//
// A nil resolver means UNKNOWN, and the conservative policy denies an unknown
// jurisdiction -- which is the correct reading of "nobody has determined where
// this user is". It is never inferred from an IP address here: that is a
// product decision with legal consequences and belongs to whatever implements
// this interface, with its evidence.
type JurisdictionResolver interface {
	Jurisdiction(ctx context.Context, accountID accounts.AccountID) (string, error)
}

// compileContext is what a caller supplies about the request itself. Everything
// else the compiler needs is deployment state, resolved here.
type compileContext struct {
	Action    settlement.ActionType
	Subject   settlement.Subject
	AccountID accounts.AccountID
	// Amount is the exact base-unit quantity the action moves, as a string so
	// that callers holding a money.Quantity and callers holding a decimal
	// string agree. Empty means the action moves nothing.
	Amount string
	// NotionalUSD is the alternative denomination the external and simulated
	// rails accept. An INTERNAL action never carries one -- a USD figure there
	// would mean somebody converted a price into money outside the ledger --
	// and the compiler refuses an intent that states both or neither.
	NotionalUSD    *money.USD
	IdempotencyKey string
	CorrelationID  string
	ValueOrigin    valuedomain.CreditOrigin
	Provider       string
}

// compileRoute compiles one Domain A command and returns the route, or the
// typed refusal routeRefusal builds from it.
func (d NativeEconomyDeps) compileRoute(ctx context.Context, cc compileContext) (settlement.Route, error) {
	router := d.routerOf()
	caps, err := d.capsOf(ctx)
	if err != nil {
		return settlement.Route{}, err
	}
	level, err := d.verificationOf(ctx, cc.AccountID)
	if err != nil {
		return settlement.Route{}, err
	}
	jurisdiction, err := d.jurisdictionOf(ctx, cc.AccountID)
	if err != nil {
		return settlement.Route{}, err
	}

	domain, _, _, ok := settlement.Profile(cc.Action)
	if !ok {
		return settlement.Route{}, errs.Newf(errs.CodeInternal,
			"settlement: no routing profile for action %s", cc.Action)
	}
	side, _ := settlement.SideOf(cc.Action)

	fi := settlement.FinancialIntent{
		IntentID:  newRouteIntentID(cc),
		AccountID: cc.AccountID.String(),
		// The principal is the actor. An agent never reaches this surface --
		// TestAgentPrincipalsAreRefusedEverywhere holds that line at the
		// router -- and the compiler is told the truth anyway rather than a
		// convenient default.
		ActorType:      actorTypeOf(ctx),
		ActorID:        actorIDOf(ctx),
		IdempotencyKey: cc.IdempotencyKey,
		CorrelationID:  cc.CorrelationID,
		RequestedAt:    d.now(),
		CapitalDomain:  domain,
		ActionType:     cc.Action,
		Subject:        cc.Subject,
		Side:           side,
		Jurisdiction:   jurisdiction,
		Verification:   level,
		ValueOrigin:    cc.ValueOrigin,
		Provider:       cc.Provider,
	}
	if q, perr := parseOptionalQuantity(cc.Amount); perr == nil && q != nil {
		fi.Quantity = q
	}
	fi.NotionalUSD = cc.NotionalUSD

	route, err := settlement.Compile(settlement.CompilerInput{
		Intent: fi, Router: router, ActiveCapabilities: caps, Now: d.now(),
	})
	if err != nil {
		return settlement.Route{}, err
	}
	if route.MayExecuteNow() {
		return route, nil
	}
	return route, routeRefusal(route)
}

// routeRefusal turns a refused route into the error a caller sees.
//
// A refused route usually carries SEVERAL reasons, and the order below decides
// which one the caller is told about first. It is not arbitrary: it is the
// order in which the obstacles would actually have to be removed.
//
//  1. The rail is not implemented. Nothing else matters; no policy or gate
//     change makes machinery exist.
//  2. The policy denies. The policy is the standing position. Telling an
//     operator to activate a gate while the policy still says no would send
//     them to do something that changes nothing.
//  3. The gate is off. Now activation is the actual next step, so the
//     capability is named.
//  4. Verification, then provider — the two obstacles a USER can act on.
//
// Every reason is in the `reasons` field regardless, so nothing is hidden by
// the ordering; what the ordering decides is which one leads.
func routeRefusal(route settlement.Route) error {
	code := errs.CodeForbidden
	detail := "this deployment does not permit that action"
	switch {
	case route.HasReason(settlement.ReasonRailNotImplemented):
		code, detail = errs.CodeUnsupported,
			"the rail this action would settle on is declared and not implemented in this build"
	case route.HasReason(settlement.ReasonAgentActionForbidden):
		code, detail = errs.CodeForbidden,
			"an agent may never take this action, at any authority level"
	case route.HasReason(settlement.ReasonLegalRouterDenied):
		code, detail = errs.CodeForbidden,
			"no approval on record permits this action in this deployment"
	case route.HasReason(settlement.ReasonCapabilityNotActive):
		code, detail = errs.CodeCapabilityNotApproved,
			"a capability this action needs is not active in this deployment"
	case route.HasReason(settlement.ReasonVerificationRequired):
		code, detail = errs.CodeVerificationRequired,
			"this action requires a higher level of identity verification"
	case route.HasReason(settlement.ReasonProviderRequired):
		code, detail = errs.CodeProviderUnavailable,
			"no configured provider can perform this action"
	case route.Permitted && route.RequiredConfirmation != settlement.ConfirmNone:
		code, detail = errs.CodeForbidden,
			"this action is permitted but requires a confirmation that has not happened"
	}

	e := errs.New(code, detail).
		WithField("reasons", route.Reasons).
		WithField("value_domain", string(route.ValueDomain)).
		WithField("rail", string(route.Rail)).
		WithField("product", route.Product).
		WithField("policy_version", route.Legal.PolicyVersion)
	if route.Legal.RuleIndex >= 0 {
		e = e.WithField("policy_rule_index", route.Legal.RuleIndex)
	}
	if route.Legal.ReasonCode != "" {
		e = e.WithField("policy_reason", route.Legal.ReasonCode)
	}
	if len(route.RequiredCapabilities) > 0 {
		names := make([]string, 0, len(route.RequiredCapabilities))
		for _, c := range route.RequiredCapabilities {
			names = append(names, string(c))
		}
		e = e.WithField("required_capabilities", names)
	}
	if route.RequiredConfirmation != settlement.ConfirmNone {
		e = e.WithField("required_confirmation", string(route.RequiredConfirmation))
	}
	if route.RequiredVerification != "" {
		e = e.WithField("required_verification", string(route.RequiredVerification))
	}
	return e
}

// routerOf returns the deployment's legal policy, defaulting to the
// conservative one. A deployment that has configured no policy has made no
// determination, and the absence of a determination is a refusal.
func (d NativeEconomyDeps) routerOf() *legalrouter.Router {
	if d.LegalRouter != nil {
		return d.LegalRouter
	}
	return conservativeRouterOnce()
}

func (d NativeEconomyDeps) jurisdictionOf(ctx context.Context, accountID accounts.AccountID) (string, error) {
	if d.Jurisdiction == nil {
		return "UNKNOWN", nil
	}
	j, err := d.Jurisdiction.Jurisdiction(ctx, accountID)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(j) == "" {
		return "UNKNOWN", nil
	}
	return j, nil
}

func actorTypeOf(ctx context.Context) security.ActorType {
	if p, ok := security.PrincipalFrom(ctx); ok {
		return p.ActorType
	}
	return security.ActorSystem
}

func actorIDOf(ctx context.Context) string {
	if p, ok := security.PrincipalFrom(ctx); ok {
		return p.SubjectID
	}
	return "system"
}
