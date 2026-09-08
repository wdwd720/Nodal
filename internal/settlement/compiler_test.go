package settlement

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/agentauthority"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
)

var compileNow = time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

func conservativeRouter(t *testing.T) *legalrouter.Router {
	t.Helper()
	r, err := legalrouter.New(legalrouter.ConservativePolicy())
	require.NoError(t, err)
	return r
}

// productCapability is the gate a permissive test policy attaches to each
// product. The router refuses to validate a policy that permits a
// non-simulation product without one, which is the rule under test elsewhere
// and simply a constraint here.
var productCapability = map[string]valuedomain.CapabilityKey{
	legalrouter.ProductNativeMarketTrade:  valuedomain.CapNativeMarketTrading,
	legalrouter.ProductNativeAssetCreate:  "NATIVE_ASSET_CREATION",
	legalrouter.ProductCreditPurchase:     "CREDIT_PURCHASE",
	legalrouter.ProductInternalCommerce:   "MARKETPLACE",
	legalrouter.ProductPayout:             valuedomain.CapPayoutReserve,
	legalrouter.ProductHostedTrade:        valuedomain.CapHostedTrading,
	legalrouter.ProductSelfCustodialTrade: "LIVE_MANUAL_TRADING",
}

// permissiveRouter is what a deployment that HAS made determinations looks
// like: every product allowed, each behind the capability the compiler would
// require anyway. It exists so that tests of the other checks are not all
// masked by the default denial.
func permissiveRouter(t *testing.T, extra ...legalrouter.Rule) *legalrouter.Router {
	t.Helper()
	rules := append([]legalrouter.Rule(nil), extra...)
	for _, p := range legalrouter.AllProducts() {
		rules = append(rules, legalrouter.Rule{
			Match:              legalrouter.Key{Product: p},
			Outcome:            legalrouter.Allow,
			ReasonCode:         "TEST_POLICY_PERMITS",
			ApprovalReference:  "TEST-001",
			RequiredCapability: productCapability[p],
		})
	}
	rules = append(rules, legalrouter.Rule{
		Outcome: legalrouter.Deny, ReasonCode: "NO_APPROVAL_ON_RECORD",
	})
	r, err := legalrouter.New(legalrouter.Policy{Version: "test-permissive-v1", Rules: rules})
	require.NoError(t, err)
	return r
}

// TestPermissiveRouter_CoversEveryProduct keeps the fixture honest: a new
// product with no capability here would silently make every permissive-policy
// test in this file assert against a policy that denies it.
func TestPermissiveRouter_CoversEveryProduct(t *testing.T) {
	for _, p := range legalrouter.AllProducts() {
		if p == legalrouter.ProductSimulation {
			continue // deliberately ungated
		}
		require.NotEmpty(t, productCapability[p], "product %s has no test capability", p)
	}
}

// allCaps is every capability the routing table or the test policy can
// require, active.
func allCaps() map[valuedomain.CapabilityKey]bool {
	out := map[valuedomain.CapabilityKey]bool{}
	for _, a := range AllActionTypes() {
		for _, c := range profiles[a].capabilities {
			out[c] = true
		}
	}
	for _, c := range productCapability {
		out[c] = true
	}
	return out
}

func qty(n int64) *money.Quantity {
	q := money.QuantityFromInt64(n)
	return &q
}

// intentFor builds a well-formed intent for an action, so a test can change
// the one thing it is about and nothing else.
func intentFor(a ActionType) FinancialIntent {
	domain, _, _, _ := Profile(a)
	subject, _ := RequiredSubject(a)
	side, _ := SideOf(a)
	fi := FinancialIntent{
		IntentID:  "0193b2e0-0000-7000-8000-000000000001",
		AccountID: "0193b2e0-0000-7000-8000-0000000000a1",
		ActorType: security.ActorUser, ActorID: "user-1",
		IdempotencyKey: "key-1", RequestedAt: compileNow,
		CapitalDomain: domain, ActionType: a, Side: side,
		Subject: Subject{
			Type: subject, ID: "0193b2e0-0000-7000-8000-0000000000b1",
			AssetID: "0193b2e0-0000-7000-8000-0000000000c1",
		},
		Jurisdiction: "US-CA",
		Verification: valuedomain.VerificationEnhanced,
		Deadline:     compileNow.Add(time.Hour),
	}
	switch a {
	case ActionBuyNativeAsset, ActionSellNativeAsset, ActionPurchaseInternalService, ActionRequestPayout:
		fi.Quantity = qty(1_000)
	case ActionBuyHostedAsset, ActionSellHostedAsset, ActionBuyOnchainAsset, ActionSellOnchainAsset, ActionSimulatedTrade:
		fi.Quantity = qty(1_000)
	}
	if a == ActionRequestPayout {
		fi.ValueOrigin = valuedomain.OriginCreatorEarning
		fi.PayoutMode = legalrouter.PayoutModePartnerFiat
		fi.Provider = "sandbox"
	}
	return fi
}

func compile(t *testing.T, fi FinancialIntent, router *legalrouter.Router, caps map[valuedomain.CapabilityKey]bool) Route {
	t.Helper()
	r, err := Compile(CompilerInput{Intent: fi, Router: router, ActiveCapabilities: caps, Now: compileNow})
	require.NoError(t, err)
	return r
}

// ---------------------------------------------------------------------------
// The table itself
// ---------------------------------------------------------------------------

// TestValidateCompiler_TheRoutingTableIsTotal. An action with no profile would
// reach Compile and be refused with UNKNOWN_ACTION_TYPE, which is the right
// refusal and the wrong place to discover it: the table is the compiler.
func TestValidateCompiler_TheRoutingTableIsTotal(t *testing.T) {
	require.NoError(t, ValidateCompiler())
	require.Len(t, profiles, len(AllActionTypes()))
}

// TestCompile_EveryActionTypeRoutesToItsDeclaredDomainAndRail.
func TestCompile_EveryActionTypeRoutesToItsDeclaredDomainAndRail(t *testing.T) {
	router := permissiveRouter(t)
	for _, a := range AllActionTypes() {
		t.Run(string(a), func(t *testing.T) {
			r := compile(t, intentFor(a), router, allCaps())
			domain, rail, product, ok := Profile(a)
			require.True(t, ok)
			require.Equal(t, domain, r.ValueDomain)
			require.Equal(t, rail, r.Rail)
			require.Equal(t, product, r.Product)
			require.Equal(t, rail.AuthoritativeBalanceSource(), r.AuthoritativeBalanceSource)
			require.Equal(t, rail.ReconciliationModel(), r.ReconciliationMethod)
			require.Equal(t, domain.Rail(), r.Rail,
				"a domain must live on the rail it is routed to")
		})
	}
}

// TestCompile_NoActionRoutesToAnUnimplementedRail is the guard PART XXII asks
// for: whatever a policy says, an action must not be routed to machinery that
// does not exist. Hosted trading is the live example -- the rail is declared,
// no adapter exists, and the compiler refuses it with every capability active
// and a policy that says yes.
func TestCompile_NoActionRoutesToAnUnimplementedRail(t *testing.T) {
	router := permissiveRouter(t)
	for _, a := range AllActionTypes() {
		_, rail, _, _ := Profile(a)
		r := compile(t, intentFor(a), router, allCaps())
		if rail.Implemented() {
			require.False(t, r.HasReason(ReasonRailNotImplemented), "action %s", a)
			continue
		}
		require.True(t, r.HasReason(ReasonRailNotImplemented),
			"action %s routes to unimplemented rail %s and must be refused", a, rail)
		require.False(t, r.Permitted)
		require.Equal(t, ExecutorNone, r.Executor)
	}
}

func TestCompile_HostedActionsAreRefusedBecauseTheRailHasNoAdapter(t *testing.T) {
	router := permissiveRouter(t)
	for _, a := range []ActionType{ActionBuyHostedAsset, ActionSellHostedAsset} {
		r := compile(t, intentFor(a), router, allCaps())
		require.False(t, r.Permitted, "%s must not be executable", a)
		require.True(t, r.HasReason(ReasonRailNotImplemented))
	}
}

// ---------------------------------------------------------------------------
// A fresh deployment
// ---------------------------------------------------------------------------

// TestCompile_AFreshDeploymentPermitsOnlySimulation. This is PART LXIII stated
// as an executable fact: with the conservative policy and no capability
// active, the only thing anybody can do is simulate.
func TestCompile_AFreshDeploymentPermitsOnlySimulation(t *testing.T) {
	router := conservativeRouter(t)
	for _, a := range AllActionTypes() {
		t.Run(string(a), func(t *testing.T) {
			r := compile(t, intentFor(a), router, nil)
			if a == ActionSimulatedTrade {
				require.True(t, r.Permitted,
					"simulation must work out of the box; reasons=%v", r.Reasons)
				require.Equal(t, ExecutorSimulated, r.Executor)
				return
			}
			require.False(t, r.Permitted, "%s must be refused on a fresh deployment", a)
			require.NotEmpty(t, r.Reasons)
			require.Equal(t, ExecutorNone, r.Executor)
		})
	}
}

// TestCompile_ThePolicySayingYesIsNotEnough: policy and gate must agree, and
// the compiler must say which one refused.
func TestCompile_ThePolicySayingYesIsNotEnough(t *testing.T) {
	router := permissiveRouter(t)
	fi := intentFor(ActionBuyNativeAsset)

	withGate := compile(t, fi, router, allCaps())
	require.True(t, withGate.Permitted, "reasons=%v", withGate.Reasons)

	withoutGate := compile(t, fi, router, nil)
	require.False(t, withoutGate.Permitted)
	require.True(t, withoutGate.HasReason(ReasonCapabilityNotActive))
	require.Contains(t, withoutGate.RequiredCapabilities, valuedomain.CapNativeMarketTrading)
}

// TestCompile_AGateBeingOffIsNotAPolicyRefusal. The router turns an ALLOW
// whose gate is inactive into a DENY -- correctly, because policy and gate
// must agree. But the two refusals have different next steps: one means
// "change the policy", the other means "activate the capability". A compiler
// that reported both as a policy denial would send an operator to edit a
// policy that already says yes.
func TestCompile_AGateBeingOffIsNotAPolicyRefusal(t *testing.T) {
	router := permissiveRouter(t)
	fi := intentFor(ActionPurchaseInternalService)

	off := compile(t, fi, router, nil)
	require.False(t, off.Permitted)
	require.True(t, off.HasReason(ReasonCapabilityNotActive))
	require.False(t, off.HasReason(ReasonLegalRouterDenied),
		"the policy permits this; only the gate refused, and the reasons must say so: %v", off.Reasons)

	// A policy that genuinely denies still reports a policy refusal, even
	// when the gate is also off.
	denied := compile(t, intentFor(ActionPurchaseInternalService), conservativeRouter(t), nil)
	require.True(t, denied.HasReason(ReasonLegalRouterDenied), "reasons=%v", denied.Reasons)
	require.True(t, denied.HasReason(ReasonCapabilityNotActive), "reasons=%v", denied.Reasons)
}

// TestCompile_EveryPermittedRouteNamesTheGatesThatPermittedIt. A route that
// permits without naming a capability would be a route nobody can audit.
func TestCompile_EveryPermittedRouteNamesTheGatesThatPermittedIt(t *testing.T) {
	router := permissiveRouter(t)
	for _, a := range AllActionTypes() {
		if a == ActionSimulatedTrade {
			continue // deliberately ungated
		}
		_, rail, _, _ := Profile(a)
		if !rail.Implemented() {
			continue
		}
		r := compile(t, intentFor(a), router, allCaps())
		require.True(t, r.Permitted, "action %s reasons=%v", a, r.Reasons)
		require.NotEmpty(t, r.RequiredCapabilities,
			"action %s permits value to move and names no capability", a)
	}
}

// ---------------------------------------------------------------------------
// Agents
// ---------------------------------------------------------------------------

// TestCompile_AnAgentCanNeverRequestAPayout is the compiler-level statement of
// PART XXXI's forbidden list. It holds at every authority level, with every
// capability active, under a policy that permits payouts.
func TestCompile_AnAgentCanNeverRequestAPayout(t *testing.T) {
	router := permissiveRouter(t)
	caps := allCaps()
	for _, l := range agentauthority.AllLevels() {
		caps[l.RequiresCapability()] = true
	}
	for _, l := range agentauthority.AllLevels() {
		fi := intentFor(ActionRequestPayout)
		fi.ActorType = security.ActorAgent
		fi.ActorID = "agent-1"
		fi.AgentID = "agent-1"
		fi.AgentAuthority = l

		r := compile(t, fi, router, caps)
		require.False(t, r.Permitted, "level %s must not reach a payout", l)
		require.True(t, r.HasReason(ReasonAgentActionForbidden),
			"level %s: reasons=%v", l, r.Reasons)
		require.Equal(t, ExecutorNone, r.Executor)
	}
}

// TestCompile_AnAgentBelowItsActionsLevelIsRefused.
func TestCompile_AnAgentBelowItsActionsLevelIsRefused(t *testing.T) {
	router := permissiveRouter(t)
	caps := allCaps()

	low := intentFor(ActionBuyNativeAsset)
	low.ActorType = security.ActorAgent
	low.ActorID, low.AgentID = "agent-1", "agent-1"
	low.AgentAuthority = agentauthority.LevelResearchOnly
	r := compile(t, low, router, caps)
	require.False(t, r.Permitted)
	require.True(t, r.HasReason(ReasonAgentAuthorityInadequate), "reasons=%v", r.Reasons)

	high := low
	high.AgentAuthority = agentauthority.LevelUserApprovedRule
	r = compile(t, high, router, caps)
	require.True(t, r.Permitted, "reasons=%v", r.Reasons)
	require.True(t, r.AgentMayAct)
}

// TestCompile_AHumanIsNotSubjectToTheAgentLadder: the same intent a
// RESEARCH_ONLY agent may not submit is fine from the person themselves.
func TestCompile_AHumanIsNotSubjectToTheAgentLadder(t *testing.T) {
	router := permissiveRouter(t)
	fi := intentFor(ActionBuyNativeAsset)
	r := compile(t, fi, router, allCaps())
	require.True(t, r.Permitted, "reasons=%v", r.Reasons)
	require.False(t, r.AgentMayAct, "a human is not an agent, and the field must not claim otherwise")
}

// ---------------------------------------------------------------------------
// Declared domain
// ---------------------------------------------------------------------------

// TestCompile_TheDeclaredDomainMustMatchTheAction. A client that can be wrong
// about which pot of money an action comes out of is a client that can spend
// the wrong one.
func TestCompile_TheDeclaredDomainMustMatchTheAction(t *testing.T) {
	router := permissiveRouter(t)
	fi := intentFor(ActionBuyNativeAsset)
	fi.CapitalDomain = valuedomain.SelfCustodialCrypto

	r := compile(t, fi, router, allCaps())
	require.False(t, r.Permitted)
	require.True(t, r.HasReason(ReasonDeclaredDomainMismatch), "reasons=%v", r.Reasons)
	require.Equal(t, valuedomain.InternalNativeAsset, r.ValueDomain,
		"the route still reports the action's real domain; the intent's claim is what was wrong")
}

// TestCompile_EveryDomainMismatchIsRefused walks the whole action × domain
// space rather than one example.
func TestCompile_EveryDomainMismatchIsRefused(t *testing.T) {
	router := permissiveRouter(t)
	for _, a := range AllActionTypes() {
		correct, _, _, _ := Profile(a)
		for _, d := range valuedomain.AllDomains() {
			fi := intentFor(a)
			fi.CapitalDomain = d
			r := compile(t, fi, router, allCaps())
			if d == correct {
				require.False(t, r.HasReason(ReasonDeclaredDomainMismatch), "%s/%s", a, d)
				continue
			}
			require.True(t, r.HasReason(ReasonDeclaredDomainMismatch), "%s claiming %s", a, d)
			require.False(t, r.Permitted)
		}
	}
}

// ---------------------------------------------------------------------------
// Verification
// ---------------------------------------------------------------------------

func TestCompile_VerificationIsOrdered(t *testing.T) {
	router := permissiveRouter(t)
	for _, tc := range []struct {
		level  valuedomain.VerificationLevel
		payout bool
	}{
		{valuedomain.VerificationNone, false},
		{valuedomain.VerificationNodalIdentity, false},
		{valuedomain.VerificationPayoutKYC, true},
		{valuedomain.VerificationEnhanced, true},
	} {
		t.Run(string(tc.level), func(t *testing.T) {
			fi := intentFor(ActionRequestPayout)
			fi.Verification = tc.level
			r := compile(t, fi, router, allCaps())
			require.Equal(t, !tc.payout, r.HasReason(ReasonVerificationRequired),
				"level %s: reasons=%v", tc.level, r.Reasons)
		})
	}
}

// TestCompile_AnUnknownVerificationLevelSatisfiesNothing.
func TestCompile_AnUnknownVerificationLevelSatisfiesNothing(t *testing.T) {
	router := permissiveRouter(t)
	fi := intentFor(ActionBuyNativeAsset)
	fi.Verification = "SUPER_VERIFIED"
	r := compile(t, fi, router, allCaps())
	require.False(t, r.Permitted)
	require.True(t, r.HasReason(ReasonVerificationRequired))
}

// ---------------------------------------------------------------------------
// Confirmation
// ---------------------------------------------------------------------------

// TestCompile_ARouteRequiringConfirmationIsNotPermissionToExecute. This
// distinction is the whole value of REQUIRES_USER_CONFIRMATION: a caller that
// reads Permitted and moves value has skipped the step the policy inserted.
func TestCompile_ARouteRequiringConfirmationIsNotPermissionToExecute(t *testing.T) {
	router, err := legalrouter.New(legalrouter.Policy{
		Version: "test-confirm-v1",
		Rules: []legalrouter.Rule{
			{
				Match:              legalrouter.Key{Product: legalrouter.ProductNativeAssetCreate},
				Outcome:            legalrouter.RequiresUserConfirmation,
				ReasonCode:         "A_PERSON_MUST_CONFIRM",
				ApprovalReference:  "TEST-002",
				RequiredCapability: "NATIVE_ASSET_CREATION",
			},
			{Outcome: legalrouter.Deny, ReasonCode: "NO_APPROVAL_ON_RECORD"},
		},
	})
	require.NoError(t, err)

	r := compile(t, intentFor(ActionCreateNativeAsset), router,
		map[valuedomain.CapabilityKey]bool{"NATIVE_ASSET_CREATION": true})
	require.True(t, r.Permitted)
	require.Equal(t, ConfirmUserReview, r.RequiredConfirmation)
	require.False(t, r.MayExecuteNow(),
		"a route awaiting a person's confirmation must not read as executable")
}

// TestCompile_ManualReviewIsNotUserConfirmation: the person who must act is
// not the person who asked, and the Route says which.
func TestCompile_ManualReviewIsNotUserConfirmation(t *testing.T) {
	router, err := legalrouter.New(legalrouter.Policy{
		Version: "test-review-v1",
		Rules: []legalrouter.Rule{
			{
				Match:              legalrouter.Key{Product: legalrouter.ProductInternalCommerce},
				Outcome:            legalrouter.RequiresManualReview,
				ReasonCode:         "AN_OPERATOR_MUST_LOOK",
				ApprovalReference:  "TEST-003",
				RequiredCapability: "MARKETPLACE",
			},
			{Outcome: legalrouter.Deny, ReasonCode: "NO_APPROVAL_ON_RECORD"},
		},
	})
	require.NoError(t, err)

	r := compile(t, intentFor(ActionPurchaseInternalService), router,
		map[valuedomain.CapabilityKey]bool{"MARKETPLACE": true})
	require.Equal(t, ConfirmManualReview, r.RequiredConfirmation)
	require.False(t, r.MayExecuteNow())
}

// TestCompile_EveryRouterOutcomeIsHandled. An outcome the compiler does not
// know must be refused, never permitted by falling through.
func TestCompile_EveryRouterOutcomeIsHandled(t *testing.T) {
	for _, o := range legalrouter.AllOutcomes() {
		t.Run(string(o), func(t *testing.T) {
			router, err := legalrouter.New(legalrouter.Policy{
				Version: "test-outcome-" + string(o),
				Rules: []legalrouter.Rule{
					{
						Match:              legalrouter.Key{Product: legalrouter.ProductNativeMarketTrade},
						Outcome:            o,
						ReasonCode:         "TEST_OUTCOME",
						ApprovalReference:  "TEST-004",
						RequiredCapability: valuedomain.CapNativeMarketTrading,
					},
					{Outcome: legalrouter.Deny, ReasonCode: "NO_APPROVAL_ON_RECORD"},
				},
			})
			require.NoError(t, err)
			r := compile(t, intentFor(ActionBuyNativeAsset), router, allCaps())

			switch o {
			case legalrouter.Allow:
				require.True(t, r.MayExecuteNow(), "reasons=%v", r.Reasons)
			case legalrouter.RequiresUserConfirmation, legalrouter.RequiresManualReview:
				require.True(t, r.Permitted)
				require.False(t, r.MayExecuteNow())
			default:
				require.False(t, r.Permitted, "outcome %s must not permit execution", o)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Determinism and shape
// ---------------------------------------------------------------------------

// TestCompile_IsDeterministic. Same inputs, same Route, including the order of
// the capability list and the reasons.
func TestCompile_IsDeterministic(t *testing.T) {
	router := permissiveRouter(t)
	fi := intentFor(ActionRequestPayout)
	fi.Verification = valuedomain.VerificationNone // produce several reasons at once

	first := compile(t, fi, router, nil)
	for i := 0; i < 50; i++ {
		require.Equal(t, first, compile(t, fi, router, nil), "iteration %d", i)
	}
	require.GreaterOrEqual(t, len(first.Reasons), 2, "this case is meant to produce several reasons")
	for i := 1; i < len(first.Reasons); i++ {
		require.Less(t, first.Reasons[i-1], first.Reasons[i], "reasons must be sorted and unique")
	}
}

// TestCompile_ReportsEveryProblemAtOnce. Telling a caller one thing per
// attempt turns a refusal into a guessing game.
func TestCompile_ReportsEveryProblemAtOnce(t *testing.T) {
	router := conservativeRouter(t)
	fi := intentFor(ActionRequestPayout)
	fi.Verification = valuedomain.VerificationNone
	fi.CapitalDomain = valuedomain.Simulated
	fi.Deadline = compileNow.Add(-time.Minute)

	r := compile(t, fi, router, nil)
	require.False(t, r.Permitted)
	for _, want := range []string{
		ReasonCapabilityNotActive, ReasonDeadlinePassed,
		ReasonDeclaredDomainMismatch, ReasonLegalRouterDenied, ReasonVerificationRequired,
	} {
		require.True(t, r.HasReason(want), "expected %s in %v", want, r.Reasons)
	}
}

// TestCompile_PermittedAndReasonsCanNeverDisagree.
func TestCompile_PermittedAndReasonsCanNeverDisagree(t *testing.T) {
	for _, router := range []*legalrouter.Router{conservativeRouter(t), permissiveRouter(t)} {
		for _, caps := range []map[valuedomain.CapabilityKey]bool{nil, allCaps()} {
			for _, a := range AllActionTypes() {
				for _, level := range []valuedomain.VerificationLevel{
					valuedomain.VerificationNone, valuedomain.VerificationNodalIdentity,
					valuedomain.VerificationPayoutKYC, valuedomain.VerificationEnhanced,
				} {
					fi := intentFor(a)
					fi.Verification = level
					r := compile(t, fi, router, caps)
					require.Equal(t, len(r.Reasons) == 0, r.Permitted,
						"%s/%s: permitted=%v reasons=%v", a, level, r.Permitted, r.Reasons)
					if !r.Permitted {
						require.Equal(t, ExecutorNone, r.Executor)
						require.False(t, r.MayExecuteNow())
					}
				}
			}
		}
	}
}

// TestCompile_ADeadlineInThePastIsRefused.
func TestCompile_ADeadlineInThePastIsRefused(t *testing.T) {
	router := permissiveRouter(t)
	fi := intentFor(ActionBuyNativeAsset)
	fi.Deadline = compileNow.Add(-time.Second)
	require.True(t, compile(t, fi, router, allCaps()).HasReason(ReasonDeadlinePassed))

	fi.Deadline = compileNow.Add(time.Second)
	require.False(t, compile(t, fi, router, allCaps()).HasReason(ReasonDeadlinePassed))
}

// TestCompile_ATradeMustNameItsAsset: the legal router keys on the asset, and
// routing a trade without one matches a rule written for something else.
func TestCompile_ATradeMustNameItsAsset(t *testing.T) {
	router := permissiveRouter(t)
	fi := intentFor(ActionBuyNativeAsset)
	fi.Subject.AssetID = ""
	r := compile(t, fi, router, allCaps())
	require.False(t, r.Permitted)
	require.True(t, r.HasReason(ReasonSubjectAssetMissing))
}

// ---------------------------------------------------------------------------
// Malformed input
// ---------------------------------------------------------------------------

func TestCompile_RefusesAMissingRouter(t *testing.T) {
	_, err := Compile(CompilerInput{Intent: intentFor(ActionSimulatedTrade), Now: compileNow})
	require.Error(t, err, "a missing policy is not a permissive one")
}

func TestCompile_RefusesAnInvalidIntent(t *testing.T) {
	router := permissiveRouter(t)
	for _, tc := range []struct {
		what   string
		mutate func(*FinancialIntent)
	}{
		{"no intent id", func(fi *FinancialIntent) { fi.IntentID = "" }},
		{"no account", func(fi *FinancialIntent) { fi.AccountID = "" }},
		{"no idempotency key", func(fi *FinancialIntent) { fi.IdempotencyKey = "" }},
		{"an unknown action", func(fi *FinancialIntent) { fi.ActionType = "BUY_SOMETHING" }},
		{"the wrong subject type", func(fi *FinancialIntent) { fi.Subject.Type = SubjectInstrument }},
		{"no subject id", func(fi *FinancialIntent) { fi.Subject.ID = "" }},
		{"the wrong side", func(fi *FinancialIntent) { fi.Side = SideSell }},
		{"a USD notional on an internal action", func(fi *FinancialIntent) {
			usd := money.USDFromMinor(1000)
			fi.NotionalUSD = &usd
		}},
		{"no quantity", func(fi *FinancialIntent) { fi.Quantity = nil }},
		{"an agent with no id", func(fi *FinancialIntent) { fi.ActorType = security.ActorAgent }},
	} {
		t.Run(tc.what, func(t *testing.T) {
			fi := intentFor(ActionBuyNativeAsset)
			tc.mutate(&fi)
			_, err := Compile(CompilerInput{Intent: fi, Router: router, Now: compileNow})
			require.Error(t, err, "an intent with %s must not compile", tc.what)
		})
	}
}

// TestFinancialIntent_EveryActionDeclaresItsShape. A new action type that
// forgot its subject or side would be undecided, and validateActionShape would
// have nothing to check it against.
func TestFinancialIntent_EveryActionDeclaresItsShape(t *testing.T) {
	for _, a := range AllActionTypes() {
		subject, ok := RequiredSubject(a)
		require.True(t, ok, "action %s declares no required subject", a)
		require.True(t, subject.Valid(), "action %s requires unknown subject %s", a, subject)
		side, ok := SideOf(a)
		require.True(t, ok, "action %s declares no side", a)
		require.True(t, side.Valid())
	}
}

// TestFinancialIntent_AWellFormedIntentValidates for every action, so the
// fixture above cannot silently become invalid.
func TestFinancialIntent_AWellFormedIntentValidates(t *testing.T) {
	for _, a := range AllActionTypes() {
		require.NoError(t, intentFor(a).Validate(), "action %s", a)
	}
}

// ---------------------------------------------------------------------------
// PART LXXII adversarial items
// ---------------------------------------------------------------------------

// TestCompile_AnAgentIsRefusedAProhibitedAsset (PART LXXII item 25). The legal
// router keys on the asset, so a policy can forbid one asset to agents while
// permitting it to people. The compiler must honour that rather than deciding
// on the product alone.
func TestCompile_AnAgentIsRefusedAProhibitedAsset(t *testing.T) {
	const prohibited = "0193b2e0-0000-7000-8000-0000000000ff"
	router, err := legalrouter.New(legalrouter.Policy{
		Version: "test-prohibited-asset-v1",
		Rules: []legalrouter.Rule{
			{
				Match: legalrouter.Key{
					Product:        legalrouter.ProductNativeMarketTrade,
					Asset:          prohibited,
					AgentAuthority: agentauthority.LevelUserApprovedRule.Name(),
				},
				Outcome:    legalrouter.Deny,
				ReasonCode: "ASSET_NOT_PERMITTED_TO_AGENTS",
			},
			{
				Match:              legalrouter.Key{Product: legalrouter.ProductNativeMarketTrade},
				Outcome:            legalrouter.Allow,
				ReasonCode:         "TEST_POLICY_PERMITS",
				ApprovalReference:  "TEST-025",
				RequiredCapability: valuedomain.CapNativeMarketTrading,
			},
			{Outcome: legalrouter.Deny, ReasonCode: "NO_APPROVAL_ON_RECORD"},
		},
	})
	require.NoError(t, err)

	caps := allCaps()
	for _, l := range agentauthority.AllLevels() {
		caps[l.RequiresCapability()] = true
	}

	agent := intentFor(ActionBuyNativeAsset)
	agent.ActorType = security.ActorAgent
	agent.ActorID, agent.AgentID = "agent-1", "agent-1"
	agent.AgentAuthority = agentauthority.LevelUserApprovedRule
	agent.Subject.AssetID = prohibited

	r := compile(t, agent, router, caps)
	require.False(t, r.Permitted, "an agent must not reach a prohibited asset")
	require.True(t, r.HasReason(ReasonLegalRouterDenied), "reasons=%v", r.Reasons)
	require.Equal(t, "ASSET_NOT_PERMITTED_TO_AGENTS", r.Legal.ReasonCode)

	// The same asset, the same agent authority, but a HUMAN acting: permitted.
	// Without this the test would pass against a compiler that refused
	// everything.
	human := intentFor(ActionBuyNativeAsset)
	human.Subject.AssetID = prohibited
	require.True(t, compile(t, human, router, caps).Permitted)

	// And the same agent on a different asset: permitted. So the refusal was
	// about the asset, not about being an agent.
	other := agent
	other.Subject.AssetID = "0193b2e0-0000-7000-8000-0000000000aa"
	require.True(t, compile(t, other, router, caps).Permitted)
}

// TestCompile_MaliciousQuantitiesAreRefused (PART LXXII items 19 and 20). A
// negative or zero quantity must never reach a domain service, and neither
// must an amount stated in the wrong denomination.
func TestCompile_MaliciousQuantitiesAreRefused(t *testing.T) {
	router := permissiveRouter(t)
	neg := money.QuantityFromInt64(-1)
	zero := money.QuantityFromInt64(0)

	for _, tc := range []struct {
		what string
		q    *money.Quantity
	}{
		{"negative", &neg},
		{"zero", &zero},
		{"absent", nil},
	} {
		t.Run(tc.what, func(t *testing.T) {
			fi := intentFor(ActionBuyNativeAsset)
			fi.Quantity = tc.q
			_, err := Compile(CompilerInput{Intent: fi, Router: router, Now: compileNow})
			require.Error(t, err, "a %s quantity must not compile", tc.what)
			require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}

	// A huge quantity is NOT refused here: whether an account can afford it is
	// the ledger's question, and refusing it in the compiler would be a limit
	// nobody set. This asserts that on purpose, so a future edit that adds a
	// silent cap here fails.
	huge, err := money.ParseQuantity("99999999999999999999999999999999999999")
	require.NoError(t, err)
	fi := intentFor(ActionBuyNativeAsset)
	fi.Quantity = &huge
	r := compile(t, fi, router, allCaps())
	require.True(t, r.Permitted,
		"the compiler decides legality, not affordability; the ledger refuses what cannot be paid")
}

// TestCompile_MinAgentAuthorityIsAFloorNotACeiling. The field is the LOWEST
// level at which an agent may take the action; a permanently forbidden action
// has none, because there is no level that permits it.
func TestCompile_MinAgentAuthorityIsAFloorNotACeiling(t *testing.T) {
	router := permissiveRouter(t)
	caps := allCaps()

	trade := compile(t, intentFor(ActionBuyNativeAsset), router, caps)
	require.Equal(t, agentauthority.LevelUserApprovedRule, trade.MinAgentAuthority,
		"executing an approved rule becomes available at USER_APPROVED_RULE")

	// An agent at exactly that level may act; one below may not. That is what
	// makes it a floor.
	at := intentFor(ActionBuyNativeAsset)
	at.ActorType, at.ActorID, at.AgentID = security.ActorAgent, "a", "a"
	at.AgentAuthority = trade.MinAgentAuthority
	require.True(t, compile(t, at, router, caps).AgentMayAct)

	below := at
	below.AgentAuthority = trade.MinAgentAuthority - 1
	require.False(t, compile(t, below, router, caps).AgentMayAct)

	// A payout maps to WITHDRAW, which no level permits, so there is no floor.
	payout := compile(t, intentFor(ActionRequestPayout), router, caps)
	require.Equal(t, agentauthority.Level(0), payout.MinAgentAuthority,
		"a permanently forbidden action has no minimum level")
	require.False(t, payout.AgentMayAct)
}

// TestProfiles_BuyAndSellAgreeOnEveryExternalRail underwrites a decision made
// in internal/httpapi rather than here.
//
// A TARGET_EXPOSURE trade intent says where the account wants to end up, not
// which way it is about to move, and the HTTP layer does not know the current
// position. It routes such an intent as a BUY, visibly and on purpose. That is
// harmless only while the two sides of a rail are routed identically — same
// capabilities, same verification, same executor, same confirmation. The
// moment they diverge, routing a sell as a buy becomes a real misclassification
// and intentActionType has to learn the position instead.
//
// So this test fails when that day comes, rather than the mistake being
// discovered by whoever the misrouted refusal happens to.
func TestProfiles_BuyAndSellAgreeOnEveryExternalRail(t *testing.T) {
	for _, pair := range [][2]ActionType{
		{ActionBuyOnchainAsset, ActionSellOnchainAsset},
		{ActionBuyHostedAsset, ActionSellHostedAsset},
	} {
		buy, ok := profiles[pair[0]]
		require.True(t, ok, pair[0])
		sell, ok := profiles[pair[1]]
		require.True(t, ok, pair[1])
		require.Equal(t, buy, sell,
			"%s and %s must route identically, or internal/httpapi cannot route TARGET_EXPOSURE as a buy",
			pair[0], pair[1])
	}
}
