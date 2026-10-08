package httpapi

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital/buyingpower"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gen/api"
)

// A holding is valued exactly when buying power marked it, and SIMULATED
// value never is: every status buying power gives a held balance maps to one
// answer, and the three it leaves unvalued keep their reason.
func TestHoldings_ValuationFollowsBuyingPowerAndNeverValuesSimulatedValue(t *testing.T) {
	t.Parallel()
	valued := []string{"NORMAL", "DEGRADED", "ACTIVE", "CLOSE_ONLY", "DELISTING", buyingpower.StatusPortfolioOnly}
	for _, status := range valued {
		v, reason := holdingValuation(false, status)
		assert.Equal(t, HoldingValued, v, status)
		assert.Empty(t, reason, status)
	}
	for status, want := range map[string]HoldingUnvaluedReason{
		buyingpower.StatusStalePrice:    HoldingNoFreshMark,
		buyingpower.StatusPolicyMissing: HoldingNoAssetPolicy,
		buyingpower.StatusHalted:        HoldingAssetExcluded,
	} {
		v, reason := holdingValuation(false, status)
		assert.Equal(t, HoldingUnvalued, v, status)
		assert.Equal(t, want, reason, status)
	}
	// Whatever a policy or a price would say, simulated value has no USD value.
	for _, status := range append(valued, buyingpower.StatusStalePrice, buyingpower.StatusPolicyMissing, buyingpower.StatusHalted) {
		v, reason := holdingValuation(true, status)
		assert.Equal(t, HoldingUnvalued, v, status)
		assert.Equal(t, HoldingSimulatedValue, reason, status)
	}
	// Every reason is one the contract declares.
	for _, r := range []HoldingUnvaluedReason{HoldingSimulatedValue, HoldingNoFreshMark, HoldingNoAssetPolicy, HoldingAssetExcluded} {
		assert.True(t, api.HoldingUnvaluedReason(r).Valid(), r)
	}
}

// The answer carries a figure exactly when its status says it does. A real
// asset without a fresh mark keeps its cost basis and realized result -- they
// are facts about what was paid, not valuations -- and states no mark and no
// unrealized figure.
func TestHoldings_AnUnvaluedHoldingCarriesItsReasonAndNoValuation(t *testing.T) {
	t.Parallel()
	stale := HoldingView{
		AssetID: assets.NewAssetID(), Symbol: "SOL", Decimals: 9, Quantity: qty("2000000000"),
		Valuation: HoldingUnvalued, UnvaluedReason: HoldingNoFreshMark,
		CostBasisUSD: usdRef("300.00"), RealizedUSD: usdRef("-12.50"), CustodyAddress: "SoL1111111111111111111111111111111111111111",
	}
	view := sampleHoldings()
	view.Holdings = append(view.Holdings, stale)
	out, err := toAPIHoldings(view)
	require.NoError(t, err)
	require.Len(t, out.Holdings, 3)

	usdc := out.Holdings[0]
	assert.Equal(t, api.HoldingValued, usdc.ValuationStatus)
	assert.Nil(t, usdc.UnvaluedReason)
	require.NotNil(t, usdc.UsdMark)
	assert.Equal(t, "1000.00", *usdc.UsdMark)

	sbx := out.Holdings[1]
	assert.Equal(t, api.HoldingUnvalued, sbx.ValuationStatus)
	require.NotNil(t, sbx.UnvaluedReason)
	assert.Equal(t, api.HoldingUnvaluedSimulatedValue, *sbx.UnvaluedReason)
	assert.Nil(t, sbx.UsdMark)
	assert.Nil(t, sbx.PriceRef)
	assert.Nil(t, sbx.UnrealizedPnlUsd)
	assert.Nil(t, sbx.CostBasisUsd, "no USD figure at all is stated for simulated value")
	assert.Nil(t, sbx.RealizedPnlUsd)

	sol := out.Holdings[2]
	assert.Equal(t, api.HoldingUnvalued, sol.ValuationStatus)
	require.NotNil(t, sol.UnvaluedReason)
	assert.Equal(t, api.HoldingUnvaluedNoFreshMark, *sol.UnvaluedReason)
	assert.Nil(t, sol.UsdMark)
	assert.Nil(t, sol.PriceRef)
	assert.Nil(t, sol.UnrealizedPnlUsd, "an unrealized result needs a mark")
	require.NotNil(t, sol.CostBasisUsd)
	assert.Equal(t, "300.00", *sol.CostBasisUsd)
	require.NotNil(t, sol.RealizedPnlUsd)
	assert.Equal(t, "-12.50", *sol.RealizedPnlUsd)
}

// A view whose status and figures disagree is a fault, answered as one: the
// client is never handed a zero, or a status, it cannot trust.
func TestHoldings_AViewWhoseStatusAndFiguresDisagreeIsAFault(t *testing.T) {
	t.Parallel()
	valued := sampleHoldings().Holdings[0]
	simulated := sampleHoldings().Holdings[1]
	cases := map[string]func() HoldingView{
		"valued without a mark":              func() HoldingView { h := valued; h.USDMark = nil; return h },
		"valued without a price reference":   func() HoldingView { h := valued; h.PriceRef = ""; return h },
		"valued without an unrealized value": func() HoldingView { h := valued; h.UnrealizedUSD = nil; return h },
		"valued with a reason":               func() HoldingView { h := valued; h.UnvaluedReason = HoldingNoFreshMark; return h },
		"unvalued with a zero mark":          func() HoldingView { h := simulated; h.USDMark = usdRef("0.00"); return h },
		"unvalued with a price reference":    func() HoldingView { h := simulated; h.PriceRef = "face:USD"; return h },
		"unvalued with an unrealized value":  func() HoldingView { h := simulated; h.UnrealizedUSD = usdRef("0.00"); return h },
		"unvalued without a reason":          func() HoldingView { h := simulated; h.UnvaluedReason = ""; return h },
		"unvalued with an unknown reason":    func() HoldingView { h := simulated; h.UnvaluedReason = "BECAUSE"; return h },
		"simulated with a cost basis":        func() HoldingView { h := simulated; h.CostBasisUSD = usdRef("0.00"); return h },
		"simulated with a realized result":   func() HoldingView { h := simulated; h.RealizedUSD = usdRef("0.00"); return h },
		"no status at all":                   func() HoldingView { h := valued; h.Valuation = ""; return h },
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := toAPIHoldings(HoldingsView{AsOf: testNow, Holdings: []HoldingView{build()}})
			require.Error(t, err)
			assert.Equal(t, errs.CodeInternal, errs.CodeOf(err))

			h := newHarness(t)
			h.ports.holdings.value = HoldingsView{AsOf: testNow, Holdings: []HoldingView{build()}}
			res := h.do(http.MethodGet, "/v1/accounts/"+testAccountID.String()+"/holdings", nil)
			require.Equal(t, http.StatusInternalServerError, res.Code, "body=%s", res.Body.String())
			assert.NotContains(t, res.Body.String(), "usd_mark")
		})
	}
}

// The wire shape: the valued holding's figures as exact strings, and the
// unvalued one with its reason and none of the keys a figure would use.
func TestHoldings_TheWireShapeOmitsEveryFigureOfAnUnvaluedHolding(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	res := h.do(http.MethodGet, "/v1/accounts/"+testAccountID.String()+"/holdings", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var out api.HoldingsResponse
	res.json(&out)
	require.Len(t, out.Holdings, 2)
	assert.Equal(t, api.HoldingValued, out.Holdings[0].ValuationStatus)
	assert.Equal(t, api.HoldingUnvalued, out.Holdings[1].ValuationStatus)

	sim := res.raw()["holdings"].([]any)[1].(map[string]any)
	assert.Equal(t, "SIMULATED_VALUE", sim["unvalued_reason"])
	for _, figure := range []string{"usd_mark", "price_ref", "cost_basis_usd", "unrealized_pnl_usd", "realized_pnl_usd"} {
		_, present := sim[figure]
		assert.False(t, present, "%s must be absent, not zero", figure)
	}
	// Everything that is stated is still a string.
	valued := res.raw()["holdings"].([]any)[0].(map[string]any)
	for _, figure := range []string{"usd_mark", "cost_basis_usd", "unrealized_pnl_usd", "quantity"} {
		assert.IsType(t, "", valued[figure], figure)
	}
}
