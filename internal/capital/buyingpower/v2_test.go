package buyingpower

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/money"
)

func usdM(minor int64) money.USD { return money.USDFromMinor(minor) }

var v2Now = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func cashHolding(minor int64) V2Holding {
	return V2Holding{Component: ComponentWallet, AssetID: "a-usd", Symbol: "SBX-USD", Quantity: money.QuantityFromInt64(minor),
		MarkedUSD: usdM(minor), FactorBPS: money.OneHundredPercent, SpendableCash: true}
}

func TestV2_CashOnlyAccount(t *testing.T) {
	bp, err := EvaluateV2(V2Input{OwnerType: "CUSTOMER", OwnerID: "acct", Holdings: []V2Holding{cashHolding(1_000_00)},
		Policy: DefaultV2Policy(), AsOf: v2Now})
	require.NoError(t, err)
	assert.Equal(t, usdM(1_000_00), bp.GrossEquity)
	assert.Equal(t, usdM(1_000_00), bp.TradeBuyingPower)
	assert.Equal(t, usdM(1_000_00), bp.RiskIncreasingBuyingPower, "a cash account has no leverage")
	assert.Equal(t, usdM(1_000_00), bp.Withdrawable)
	assert.Empty(t, bp.Reasons)
}

func TestV2_BridgeInFlightIsNotFinal(t *testing.T) {
	inFlight := V2Holding{Component: ComponentBridgeInFlight, AssetID: "a-usd-sol", Symbol: "SBX-USD@sol",
		Quantity: money.QuantityFromInt64(500_00), MarkedUSD: usdM(500_00), FactorBPS: 0, Reason: "BRIDGE_IN_FLIGHT"}
	bp, err := EvaluateV2(V2Input{OwnerType: "CUSTOMER", OwnerID: "acct", Holdings: []V2Holding{cashHolding(100_00), inFlight},
		Policy: DefaultV2Policy(), AsOf: v2Now})
	require.NoError(t, err)
	assert.Equal(t, usdM(600_00), bp.GrossEquity, "in-flight value is still the customer's")
	assert.Equal(t, usdM(100_00), bp.EligibleCollateral, "but it is not eligible until final")
	assert.Equal(t, usdM(500_00), bp.PendingBridgeHaircut)
	assert.Equal(t, usdM(100_00), bp.TradeBuyingPower)
	assert.Equal(t, usdM(100_00), bp.Withdrawable)

	// A policy that allows a haircut counts the remainder.
	inFlight.FactorBPS = 5_000
	bp, err = EvaluateV2(V2Input{OwnerType: "CUSTOMER", OwnerID: "acct", Holdings: []V2Holding{cashHolding(100_00), inFlight},
		Policy: DefaultV2Policy(), AsOf: v2Now})
	require.NoError(t, err)
	assert.Equal(t, usdM(350_00), bp.EligibleCollateral)
	assert.Equal(t, usdM(250_00), bp.PendingBridgeHaircut)
}

func TestV2_MaintenanceLimitsWithdrawalAndLeverageScalesMargin(t *testing.T) {
	in := V2Input{OwnerType: "CUSTOMER", OwnerID: "acct", Holdings: []V2Holding{
		cashHolding(1_000_00),
		{Component: ComponentMarginCollateral, AssetID: "a-btc", Symbol: "SBX-BTC", Quantity: money.QuantityFromInt64(1),
			MarkedUSD: usdM(2_000_00), FactorBPS: 8_000, Reason: "MAJOR risk class haircut"},
	}, Margin: V2MarginState{Mode: "CROSS", BorrowLiabilities: usdM(1_500_00), InitialMarginUsed: usdM(500_00),
		MaintenanceMargin: usdM(1_200_00), InitialMarginRateBPS: 2_000}, Policy: DefaultV2Policy(), AsOf: v2Now}
	bp, err := EvaluateV2(in)
	require.NoError(t, err)
	// eligible = 1000 + 1600 = 2600; eligible equity = 2600 − 1500 = 1100.
	assert.Equal(t, usdM(2_600_00), bp.EligibleCollateral)
	assert.Equal(t, usdM(400_00), bp.Haircuts)
	assert.Equal(t, usdM(1_100_00), bp.EligibleEquity)
	// margin available = 1100 − 500 = 600; × (10000/2000) = 3000.
	assert.Equal(t, usdM(600_00), bp.MarginAvailable)
	assert.Equal(t, usdM(3_000_00), bp.RiskIncreasingBuyingPower)
	// Equity 1100 is below maintenance 1200: nothing is withdrawable.
	assert.True(t, bp.Withdrawable.IsZero())
	assert.Contains(t, reasonCodes(bp), V2ReasonMaintenanceBinding)
}

func TestV2_UnsettledFundsTradeButNeverWithdraw(t *testing.T) {
	p := DefaultV2Policy()
	p.UnsettledTradeFactorBPS = 10_000
	bp, err := EvaluateV2(V2Input{OwnerType: "CUSTOMER", OwnerID: "acct", Holdings: []V2Holding{
		cashHolding(100_00),
		{Component: ComponentUnsettledReceivable, AssetID: "a-usd", Symbol: "SBX-USD", Quantity: money.QuantityFromInt64(900_00),
			MarkedUSD: usdM(900_00), FactorBPS: 10_000},
	}, Policy: p, AsOf: v2Now})
	require.NoError(t, err)
	assert.Equal(t, usdM(1_000_00), bp.TradeBuyingPower)
	assert.Equal(t, usdM(100_00), bp.Withdrawable)
	assert.Contains(t, reasonCodes(bp), V2ReasonUnsettled)
}

func TestV2_BlockedAccountHasNoBuyingPower(t *testing.T) {
	bp, err := EvaluateV2(V2Input{OwnerType: "CUSTOMER", OwnerID: "acct", Holdings: []V2Holding{cashHolding(1_000_00)},
		Policy: DefaultV2Policy(), Blocked: true, BlockReasons: []string{"ACCOUNT_FROZEN"}, AsOf: v2Now})
	require.NoError(t, err)
	assert.True(t, bp.TradeBuyingPower.IsZero())
	assert.True(t, bp.Withdrawable.IsZero())
	assert.True(t, bp.RiskIncreasingBuyingPower.IsZero())
	assert.Equal(t, usdM(1_000_00), bp.GrossEquity, "blocking hides nothing: equity is still reported")
}

func TestV2_RefusesMalformedInput(t *testing.T) {
	_, err := EvaluateV2(V2Input{OwnerType: "PLATFORM", Policy: DefaultV2Policy()})
	require.Error(t, err)
	_, err = EvaluateV2(V2Input{OwnerType: "CUSTOMER"})
	require.Error(t, err)
	_, err = EvaluateV2(V2Input{OwnerType: "CUSTOMER", Policy: DefaultV2Policy(), Holdings: []V2Holding{
		{Component: ComponentWallet, MarkedUSD: usdM(-1), FactorBPS: 10_000}}})
	require.Error(t, err)
}

func genV2(rt *rapid.T) V2Input {
	n := rapid.IntRange(0, 6).Draw(rt, "holdings")
	comps := []V2Component{ComponentWallet, ComponentMarginCollateral, ComponentDerivativesMargin, ComponentUnsettledReceivable, ComponentBridgeInFlight}
	var hs []V2Holding
	for i := 0; i < n; i++ {
		c := rapid.SampledFrom(comps).Draw(rt, "component")
		m := rapid.Int64Range(0, 1e11).Draw(rt, "marked")
		hs = append(hs, V2Holding{Component: c, AssetID: "a" + string(rune('a'+i)), Quantity: money.QuantityFromInt64(m),
			MarkedUSD: usdM(m), FactorBPS: money.BPS(rapid.Int64Range(0, 10_000).Draw(rt, "factor")),
			SpendableCash: c == ComponentWallet && rapid.Bool().Draw(rt, "cash")})
	}
	mode := rapid.SampledFrom([]string{"CASH", "CROSS", "ISOLATED"}).Draw(rt, "mode")
	maint := rapid.Int64Range(0, 1e11).Draw(rt, "maint")
	return V2Input{OwnerType: "CUSTOMER", OwnerID: "acct", Holdings: hs,
		Margin: V2MarginState{Mode: mode, BorrowLiabilities: usdM(rapid.Int64Range(0, 1e11).Draw(rt, "borrow")),
			AccruedInterest:   usdM(rapid.Int64Range(0, 1e9).Draw(rt, "interest")),
			UnrealizedPnL:     usdM(rapid.Int64Range(-1e10, 1e10).Draw(rt, "upnl")),
			InitialMarginUsed: usdM(maint + rapid.Int64Range(0, 1e10).Draw(rt, "im_extra")), MaintenanceMargin: usdM(maint),
			InitialMarginRateBPS: money.BPS(rapid.Int64Range(500, 10_000).Draw(rt, "im_rate"))},
		Reservations: V2Reservations{OpenOrders: usdM(rapid.Int64Range(0, 1e10).Draw(rt, "orders")),
			PendingTransfers:   usdM(rapid.Int64Range(0, 1e9).Draw(rt, "transfers")),
			PendingWithdrawals: usdM(rapid.Int64Range(0, 1e9).Draw(rt, "withdrawals"))},
		Policy: DefaultV2Policy(), AsOf: v2Now}
}

// TestProp_V2NoWithdrawalBreachesMaintenance: whatever the state, withdrawing
// the withdrawable amount leaves eligible equity at or above maintenance.
func TestProp_V2NoWithdrawalBreachesMaintenance(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		bp, err := EvaluateV2(genV2(rt))
		if err != nil {
			rt.Fatal(err)
		}
		after, err := bp.EligibleEquity.Sub(bp.Withdrawable)
		if err != nil {
			rt.Fatal(err)
		}
		if bp.Withdrawable.IsPositive() && after.Cmp(bp.MaintenanceMargin) < 0 {
			rt.Fatalf("withdrawable %s leaves equity %s below maintenance %s", bp.Withdrawable, after, bp.MaintenanceMargin)
		}
		for _, v := range []money.USD{bp.Withdrawable, bp.TradeBuyingPower, bp.RiskIncreasingBuyingPower, bp.MarginAvailable} {
			if v.IsNegative() {
				rt.Fatalf("negative buying power figure %s", v)
			}
		}
	})
}

// TestProp_V2ReservationsNeverIncreaseBuyingPower and the same for borrowing.
func TestProp_V2ReservationsAndDebtNeverIncreaseBuyingPower(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		in := genV2(rt)
		base, err := EvaluateV2(in)
		if err != nil {
			rt.Fatal(err)
		}
		more := in
		more.Reservations.OpenOrders = usdM(in.Reservations.OpenOrders.Minor() + rapid.Int64Range(1, 1e9).Draw(rt, "extra"))
		r, err := EvaluateV2(more)
		if err != nil {
			rt.Fatal(err)
		}
		if r.TradeBuyingPower.Cmp(base.TradeBuyingPower) > 0 || r.Withdrawable.Cmp(base.Withdrawable) > 0 ||
			r.RiskIncreasingBuyingPower.Cmp(base.RiskIncreasingBuyingPower) > 0 {
			rt.Fatalf("a larger reservation increased buying power")
		}
		debt := in
		debt.Margin.BorrowLiabilities = usdM(in.Margin.BorrowLiabilities.Minor() + rapid.Int64Range(1, 1e9).Draw(rt, "debt"))
		d, err := EvaluateV2(debt)
		if err != nil {
			rt.Fatal(err)
		}
		if d.Withdrawable.Cmp(base.Withdrawable) > 0 || d.RiskIncreasingBuyingPower.Cmp(base.RiskIncreasingBuyingPower) > 0 {
			rt.Fatalf("more debt increased withdrawable or risk-increasing buying power")
		}
	})
}

// TestProp_V2InFlightBridgeNeverCountsAtDefaultPolicy: with the default
// policy (factor 0 for in-flight legs), adding in-flight value changes equity
// and nothing a customer can spend or withdraw.
func TestProp_V2InFlightBridgeNeverCountsAtDefaultPolicy(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		in := genV2(rt)
		base, err := EvaluateV2(in)
		if err != nil {
			rt.Fatal(err)
		}
		m := rapid.Int64Range(1, 1e10).Draw(rt, "in_flight")
		in.Holdings = append(in.Holdings, V2Holding{Component: ComponentBridgeInFlight, AssetID: "zz-bridge",
			Quantity: money.QuantityFromInt64(m), MarkedUSD: usdM(m), FactorBPS: 0})
		with, err := EvaluateV2(in)
		if err != nil {
			rt.Fatal(err)
		}
		if with.TradeBuyingPower != base.TradeBuyingPower || with.Withdrawable != base.Withdrawable ||
			with.RiskIncreasingBuyingPower != base.RiskIncreasingBuyingPower || with.EligibleCollateral != base.EligibleCollateral {
			rt.Fatalf("in-flight bridge value changed spendable figures (phantom capital)")
		}
		if with.PendingBridgeHaircut.Minor() != base.PendingBridgeHaircut.Minor()+m {
			rt.Fatalf("pending bridge haircut not reported")
		}
	})
}

// TestProp_V2LinesExplainEligibleCollateral: the explanation lines for
// eligible_collateral sum exactly to the reported figure.
func TestProp_V2LinesExplainEligibleCollateral(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		bp, err := EvaluateV2(genV2(rt))
		if err != nil {
			rt.Fatal(err)
		}
		var sum money.USD
		for _, l := range bp.Lines {
			if l.Field == "eligible_collateral" {
				if sum, err = sum.Add(l.AmountUSD); err != nil {
					rt.Fatal(err)
				}
			}
		}
		if sum != bp.EligibleCollateral {
			rt.Fatalf("lines sum %s, reported %s", sum, bp.EligibleCollateral)
		}
	})
}

func reasonCodes(bp V2) []string {
	out := make([]string, 0, len(bp.Reasons))
	for _, r := range bp.Reasons {
		out = append(out, r.Code)
	}
	return out
}
