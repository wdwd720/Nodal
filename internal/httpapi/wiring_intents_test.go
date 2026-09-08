package httpapi

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/intent"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/settlement"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// TestIntentRouting_TheModeDecidesWhetherCapitalIsReal: a simulated mode is
// simulated whatever the instrument is, and a real mode is real whatever the
// UI called it. PART 159 makes the submitter state the mode precisely so this
// is not inferred, and the routing has to honour that.
func TestIntentRouting_TheModeDecidesWhetherCapitalIsReal(t *testing.T) {
	for _, mode := range []intent.Mode{intent.ModeBacktest, intent.ModePaper, intent.ModeShadow} {
		got, err := intentActionType(mode, intent.ActionAcquireNotional, valuedomain.SelfCustodialCrypto)
		require.NoError(t, err, mode)
		require.Equal(t, settlement.ActionSimulatedTrade, got,
			"%s moves simulated capital even on a real instrument", mode)
	}
	for _, mode := range []intent.Mode{intent.ModeCanary, intent.ModeLimited, intent.ModeLive} {
		got, err := intentActionType(mode, intent.ActionAcquireNotional, valuedomain.SelfCustodialCrypto)
		require.NoError(t, err, mode)
		require.Equal(t, settlement.ActionBuyOnchainAsset, got,
			"%s moves real capital and must route to a real rail", mode)
	}
	_, err := intentActionType(intent.Mode("SOMETHING_ELSE"), intent.ActionAcquireNotional, valuedomain.SelfCustodialCrypto)
	require.Error(t, err, "an unknown mode is not quietly treated as simulated")
}

// TestIntentRouting_TheRailComesFromTheInstrument: which real rail an intent
// travels is a property of the asset, read from the registry. Guessing it from
// a symbol or a chain name is how a hosted balance gets settled as though it
// were self-custodial.
func TestIntentRouting_TheRailComesFromTheInstrument(t *testing.T) {
	cases := map[valuedomain.Domain]settlement.ActionType{
		valuedomain.SelfCustodialCrypto: settlement.ActionBuyOnchainAsset,
		valuedomain.HostedCrypto:        settlement.ActionBuyHostedAsset,
		valuedomain.HostedFiat:          settlement.ActionBuyHostedAsset,
	}
	for domain, want := range cases {
		got, err := intentActionType(intent.ModeLive, intent.ActionAcquireNotional, domain)
		require.NoError(t, err, domain)
		require.Equal(t, want, got, domain)
	}

	// A Nodal-native asset has a route, but one whose subject is a market
	// rather than an instrument. Compiling it here would produce a coherent
	// intent about the wrong thing, so it is refused with the reason.
	_, err := intentActionType(intent.ModeLive, intent.ActionAcquireNotional, valuedomain.InternalNativeAsset)
	require.Error(t, err)
	require.Equal(t, errs.CodeUnsupported, errs.CodeOf(err))
	require.Contains(t, err.Error(), "native market endpoints")

	// A domain with no real rail at all is refused rather than defaulted onto
	// whichever rail happens to be first.
	for _, domain := range []valuedomain.Domain{
		valuedomain.InternalCredit, valuedomain.PayoutPending,
		valuedomain.ExternalSettled, valuedomain.Simulated, valuedomain.Domain(""),
	} {
		_, err := intentActionType(intent.ModeLive, intent.ActionAcquireNotional, domain)
		require.Error(t, err, domain)
		require.Equal(t, errs.CodeUnsupported, errs.CodeOf(err), domain)
	}
}

// TestIntentRouting_TheDirectionFollowsTheAction covers every declared action,
// so a new one cannot be added without deciding which way it points.
func TestIntentRouting_TheDirectionFollowsTheAction(t *testing.T) {
	buys := []intent.Action{intent.ActionAcquireNotional, intent.ActionBuyEventOutcome, intent.ActionTargetExposure}
	sells := []intent.Action{intent.ActionReduceNotional, intent.ActionClosePosition}
	classified := map[intent.Action]bool{}
	for _, a := range append(append([]intent.Action{}, buys...), sells...) {
		classified[a] = true
	}
	// Every DECLARED action, not only the enabled ones: BUY_EVENT_OUTCOME
	// reaches this router (PostIntents admits anything declared) and is
	// refused further down as UNSUPPORTED, so it still needs a direction.
	for _, a := range append(intent.Actions(), intent.ActionBuyEventOutcome) {
		require.True(t, classified[a], "%s is declared but this router does not classify it", a)
	}

	for _, a := range buys {
		got, err := intentActionType(intent.ModeLive, a, valuedomain.SelfCustodialCrypto)
		require.NoError(t, err, a)
		require.Equal(t, settlement.ActionBuyOnchainAsset, got, a)
	}
	for _, a := range sells {
		got, err := intentActionType(intent.ModeLive, a, valuedomain.SelfCustodialCrypto)
		require.NoError(t, err, a)
		require.Equal(t, settlement.ActionSellOnchainAsset, got, a)
	}
	_, err := intentActionType(intent.ModeLive, intent.Action("REBALANCE"), valuedomain.SelfCustodialCrypto)
	require.Error(t, err, "an unknown action is not quietly treated as a buy")
}

// TestWire_CarriesTheDeploymentsSettlementPolicy is the test whose absence hid
// a real defect.
//
// The first version of this change added Ports.SettlementPolicy and read it in
// the handler, and never assigned it in Wire. Every deployment silently got the
// zero value -- the conservative policy -- which is fail-closed and still
// wrong: a deployment that HAD configured a legal policy would have been
// refused by a policy it never chose.
//
// Nothing caught it. The refusal test passes either way, because a harness with
// no policy and a compiler that cannot see the policy give the same answer. The
// only thing that distinguishes them is asking whether the wire carries it.
func TestWire_CarriesTheDeploymentsSettlementPolicy(t *testing.T) {
	router, err := legalrouter.New(legalrouter.DevelopmentPolicy())
	require.NoError(t, err)
	deps := NativeEconomyDeps{LegalRouter: router, Clock: clock.NewFake(time.Unix(0, 0).UTC())}

	p, err := Wire(WireDeps{DB: &db.DB{}, NativeEconomy: deps})
	require.NoError(t, err)
	require.Same(t, router, p.SettlementPolicy.LegalRouter,
		"a deployment's own legal policy must reach the compiler, not be replaced by the default")
	require.Equal(t, deps.Clock, p.SettlementPolicy.Clock)

	// And a deployment that configured nothing still gets the conservative
	// policy rather than a nil one, which is what makes the zero value safe.
	empty, err := Wire(WireDeps{DB: &db.DB{}})
	require.NoError(t, err)
	require.Nil(t, empty.SettlementPolicy.LegalRouter)
	require.Equal(t, "legal-router-v1-conservative", empty.SettlementPolicy.routerOf().Policy().Version)
}
