package bridge

import (
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital/buyingpower"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

func pricedRoute(t require.TestingT, mantissa int64, scale int32, decimals uint8) Route {
	r := testRoute()
	p, err := money.NewPrice(money.QuantityFromInt64(mantissa), scale, "USD", "ROUTE_UNIT_PRICE", time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	r.UnitPrice, r.Decimals = p, decimals
	return r
}

func legIn(r Route, st CrossChainCapitalState, q int64) Leg {
	l := Leg{ID: id.New[legKind](), RouteID: r.ID, Quantity: money.QuantityFromInt64(q), Status: st}
	tx := func() *ledger.TransactionID { v := ledger.NewTransactionID(); return &v }
	switch st {
	case StateSourceFinalized, StateMessageInFlight, StateDestinationObserved, StateManualReview:
		l.SourceJournal = tx()
	case StateDestinationFinalized, StateReconciled:
		l.SourceJournal, l.DestinationJournal = tx(), tx()
	}
	return l
}

func usdOf(t *testing.T, s string) money.USD {
	t.Helper()
	u, err := money.ParseUSD(s)
	require.NoError(t, err)
	return u
}

func v2(t *testing.T, holdings []buyingpower.V2Holding) buyingpower.V2 {
	t.Helper()
	out, err := buyingpower.EvaluateV2(buyingpower.V2Input{OwnerType: "CUSTOMER", OwnerID: "a", Holdings: holdings,
		Policy: buyingpower.DefaultV2Policy(), AsOf: time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)})
	require.NoError(t, err)
	return out
}

// TestHaircut_InFlightValueCountsOnlyAtThePolicyFactor: 250 units in flight at
// 1.00 are marked 250.00; by default none of it is eligible; a policy factor
// counts exactly that share; a leg in manual review never counts.
func TestHaircut_InFlightValueCountsOnlyAtThePolicyFactor(t *testing.T) {
	r := pricedRoute(t, 1, 0, 6)
	in := []ValuedLeg{{Leg: legIn(r, StateMessageInFlight, 250_000_000), Route: r, Symbol: "SRC"}}

	out, perAsset, err := EvaluateCrossChain(in, DefaultHaircutPolicy())
	require.NoError(t, err)
	require.Len(t, out.Holdings, 1)
	h := out.Holdings[0]
	assert.Equal(t, buyingpower.ComponentBridgeInFlight, h.Component)
	assert.Equal(t, usdOf(t, "250.00"), h.MarkedUSD)
	assert.Equal(t, money.BPS(0), h.FactorBPS)
	assert.Equal(t, "250000000", perAsset[r.SourceAssetID].String())
	got := v2(t, out.Holdings)
	assert.True(t, got.EligibleCollateral.IsZero(), "in-flight value is not final")
	assert.Equal(t, usdOf(t, "250.00"), got.PendingBridgeHaircut)
	assert.Equal(t, usdOf(t, "250.00"), got.GrossEquity, "it is still the customer's value")
	assert.True(t, got.TradeBuyingPower.IsZero() && got.Withdrawable.IsZero())

	policy := HaircutPolicy{Version: "test", InFlightBPS: 5_000, ObservedBPS: 8_000}
	for st, want := range map[CrossChainCapitalState]string{
		StateSourceFinalized: "125.00", StateMessageInFlight: "125.00", StateDestinationObserved: "200.00", StateManualReview: "0.00",
	} {
		out, _, err := EvaluateCrossChain([]ValuedLeg{{Leg: legIn(r, st, 250_000_000), Route: r, Symbol: "SRC"}}, policy)
		require.NoError(t, err)
		got := v2(t, out.Holdings)
		assert.Equal(t, usdOf(t, want), got.EligibleCollateral, "%s", st)
		haircut, err := usdOf(t, "250.00").Sub(usdOf(t, want))
		require.NoError(t, err)
		assert.Equal(t, haircut, got.PendingBridgeHaircut, "%s", st)
	}

	// Rounding is never in the account's favour: a lone base unit in flight
	// marks to 0.00, a lone base unit being locked reserves 0.01.
	out, _, err = EvaluateCrossChain([]ValuedLeg{
		{Leg: legIn(r, StateMessageInFlight, 1), Route: r, Symbol: "SRC"},
		{Leg: legIn(r, StateBridgeRequested, 1), Route: r, Symbol: "SRC"},
	}, DefaultHaircutPolicy())
	require.NoError(t, err)
	assert.True(t, out.Holdings[0].MarkedUSD.IsZero())
	assert.Equal(t, usdOf(t, "0.01"), out.PendingLocks)

	for _, bad := range []HaircutPolicy{
		{Version: "", InFlightBPS: 0, ObservedBPS: 0},
		{Version: "x", InFlightBPS: 6_000, ObservedBPS: 5_000},
		{Version: "x", InFlightBPS: 0, ObservedBPS: 10_001},
		{Version: "x", InFlightBPS: -1, ObservedBPS: 0},
	} {
		_, _, err := EvaluateCrossChain(in, bad)
		require.Error(t, err, "%+v", bad)
	}
}

func int64Of(t require.TestingT, q money.Quantity) int64 {
	n, err := q.Int64()
	require.NoError(t, err)
	return n
}

// centsOf is q base units at mantissa x 10^-scale USD per unit, in cents,
// rounded up or down: computed independently of money.Notional.
func centsOf(q, mantissa int64, scale int32, decimals uint8, up bool) int64 {
	num := new(big.Int).Mul(big.NewInt(q), big.NewInt(mantissa))
	num.Mul(num, big.NewInt(100))
	den := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)+int64(scale)), nil)
	quo, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	if up && rem.Sign() > 0 {
		quo.Add(quo, big.NewInt(1))
	}
	return quo.Int64()
}

// TestProp_EachLegIsCountedOnceWhereItsValueIs: for any mix of legs on two
// routes in any state, a leg not yet final on the source is counted once, as
// a pending lock; a leg in flight once, in its asset's in-flight holding; a
// released, failed or refunded leg not at all (its value is on a wallet
// line). Marks round down, reservations up, and with the default policy no
// in-flight value is eligible collateral.
func TestProp_EachLegIsCountedOnceWhereItsValueIs(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		routes := []Route{pricedRoute(rt, rapid.Int64Range(1, 5_000).Draw(rt, "p1"), 2, 6),
			pricedRoute(rt, rapid.Int64Range(1, 900).Draw(rt, "p2"), 0, 9)}
		n := rapid.IntRange(0, 20).Draw(rt, "n")
		var in []ValuedLeg
		wantPending := int64(0)
		wantInFlight := map[assets.AssetID]int64{}
		counted := map[LegID]bool{}
		for i := 0; i < n; i++ {
			r := routes[rapid.IntRange(0, 1).Draw(rt, "route")]
			st := rapid.SampledFrom(AllStates()).Draw(rt, "state")
			q := rapid.Int64Range(1, 1_000_000_000_000).Draw(rt, "q")
			l := legIn(r, st, q)
			if st == StateFailed && rapid.Bool().Draw(rt, "refunded") {
				tx1, tx2 := ledger.NewTransactionID(), ledger.NewTransactionID()
				l.SourceJournal, l.RefundJournal = &tx1, &tx2
			}
			in = append(in, ValuedLeg{Leg: l, Route: r, Symbol: "S"})
			switch l.Holding() {
			case HoldingSourceReserved:
				wantPending += centsOf(q, int64Of(rt, r.UnitPrice.Mantissa), r.UnitPrice.Scale, r.Decimals, true)
				counted[l.ID] = true
			case HoldingInFlight:
				wantInFlight[r.SourceAssetID] += q
				counted[l.ID] = true
			}
		}
		policy := DefaultHaircutPolicy()
		out, perAsset, err := EvaluateCrossChain(in, policy)
		require.NoError(rt, err)
		require.Equal(rt, money.USDFromMinor(wantPending), out.PendingLocks)
		require.Len(rt, out.Legs, len(counted))
		for _, line := range out.Legs {
			require.True(rt, counted[line.LegID], "leg %s counted but its value is on a wallet line", line.LegID)
			delete(counted, line.LegID)
		}
		got := map[assets.AssetID]int64{}
		for _, h := range out.Holdings {
			var asset assets.AssetID
			require.NoError(rt, asset.UnmarshalText([]byte(h.AssetID)))
			got[asset] += int64Of(rt, h.Quantity)
		}
		require.Equal(rt, len(wantInFlight), len(perAsset))
		for asset, q := range wantInFlight {
			require.Equal(rt, q, got[asset], "in-flight holdings of %s", asset)
			require.Equal(rt, q, int64Of(rt, perAsset[asset]))
			for _, r := range routes {
				if r.SourceAssetID != asset {
					continue
				}
				// Each line marks down on its own, so the lines never add up
				// to more than the asset's whole value marked down.
				marked := money.USD{}
				for _, h := range out.Holdings {
					if h.AssetID != asset.String() {
						continue
					}
					price := int64Of(rt, r.UnitPrice.Mantissa)
					require.Equal(rt, money.USDFromMinor(centsOf(int64Of(rt, h.Quantity), price, r.UnitPrice.Scale, r.Decimals, false)), h.MarkedUSD)
					marked, err = marked.Add(h.MarkedUSD)
					require.NoError(rt, err)
				}
				whole := money.USDFromMinor(centsOf(q, int64Of(rt, r.UnitPrice.Mantissa), r.UnitPrice.Scale, r.Decimals, false))
				require.LessOrEqual(rt, marked.Cmp(whole), 0)
			}
		}
		res, err := buyingpower.EvaluateV2(buyingpower.V2Input{OwnerType: "CUSTOMER", OwnerID: "a", Holdings: out.Holdings,
			Policy: buyingpower.DefaultV2Policy(), AsOf: time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)})
		require.NoError(rt, err)
		require.True(rt, res.EligibleCollateral.IsZero(), "in-flight value is never eligible under the default policy")
		require.True(rt, res.TradeBuyingPower.IsZero() && res.Withdrawable.IsZero())
	})
}
