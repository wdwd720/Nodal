package margin

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/money"
)

// TestEvaluate_FixedAccountMatchesHandArithmetic pins every figure of the
// fixed account to arithmetic done by hand:
//
//	market value  SOL 20 × 100 = 2,000; cash 1,000; total 3,000
//	NAV           3,000 − 2,000 loan = 1,000; SOL share 200% → capped 100%
//	haircut SOL   LOW 10% + concentration 15% (full add-on) = 25%
//	collateral    2,000 × 75% + 1,000 = 2,500
//	equity        2,500 − 2,000 = 500
//	IM / MM       2,000 × 20% = 400 / 2,000 × 10% = 200; warning 250
//	leverage      2,000 / 500 = 4x (40,000 BPS)
//	capacity      500 − 2,000 / 5x = 100; available min(500 − 400, 100) = 100
func TestEvaluate_FixedAccountMatchesHandArithmetic(t *testing.T) {
	p := DefaultPolicy()
	e := mustEval(t, p, fixedAccount(t))

	requireQ(t, usd(3_000), e.MarketValue, "market value")
	requireQ(t, usd(1_000), e.NetAssetValue, "net asset value")
	requireQ(t, usd(2_500), e.EligibleCollateral, "eligible collateral")
	requireQ(t, usd(2_000), e.BorrowLiabilities, "liabilities")
	requireQ(t, money.Quantity{}, e.AccruedInterest, "accrued interest")
	requireQ(t, usd(500), e.Equity, "equity")
	requireQ(t, usd(2_000), e.GrossExposure, "gross")
	requireQ(t, usd(2_000), e.NetExposure, "net")
	requireQ(t, usd(400), e.InitialMargin, "initial")
	requireQ(t, usd(200), e.MaintenanceMargin, "maintenance")
	requireQ(t, usd(250), e.WarningThreshold, "warning threshold")
	requireQ(t, usd(300), e.LiquidationBuffer, "liquidation buffer")
	requireQ(t, usd(100), e.Capacity, "capacity")
	requireQ(t, usd(100), e.MarginAvailable, "margin available")
	assert.Equal(t, money.BPS(40_000), e.EffectiveLeverageBPS)
	assert.False(t, e.LeverageUnbounded)
	assert.True(t, e.Levered)
	assert.Equal(t, StatusNormal, e.Health)
	assert.False(t, e.LiquidationEligible)
	assert.True(t, e.PricesFresh)

	var solV HoldingValuation
	for _, h := range e.Holdings {
		if h.AssetID == solID {
			solV = h
		}
	}
	assert.Equal(t, money.BPS(10_000), solV.ConcentrationShareBPS)
	assert.Equal(t, money.BPS(1_000), solV.Haircut.VolatilityBPS)
	assert.Equal(t, money.BPS(1_500), solV.Haircut.ConcentrationBPS)
	assert.Equal(t, money.BPS(2_500), solV.Haircut.TotalBPS)

	u, err := e.USD()
	require.NoError(t, err)
	assert.Equal(t, "500.00", u.Equity.String())
	assert.Equal(t, "200.00", u.MaintenanceMargin.String())
}

func TestEvaluate_EquityIdentityAndHashAreExact(t *testing.T) {
	p := DefaultPolicy()
	s := fixedAccount(t)
	s.Now = t0.Add(36 * time.Hour) // interest accrues; the SOL mark goes stale
	s.RealizedPnL = money.USDFromMinor(1_234)
	s.AccruedFees = money.USDFromMinor(99)
	s.Derivatives = []DerivativeExposure{{InstrumentID: "perp-1", GrossNotional: money.USDFromMinor(50_000),
		InitialMargin: money.USDFromMinor(5_000), MaintenanceMargin: money.USDFromMinor(2_500), UnrealizedPnL: money.USDFromMinor(-777)}}
	e := mustEval(t, p, s)
	sum := e.EligibleCollateral.Add(e.RealizedPnL).Add(e.UnrealizedPnL).Sub(e.BorrowLiabilities).Sub(e.AccruedFees).Sub(e.AccruedInterest)
	requireQ(t, sum, e.Equity, "canonical identity")
	assert.True(t, e.AccruedInterest.IsPositive(), "36 hours of interest on 2,000 at 8%")
	// 2,000 × 8% × 1.5 / 365 = 0.657534246... → rounded up to the unit.
	requireQ(t, units(t, "0.657535"), e.AccruedInterest, "accrued interest")
	assert.False(t, e.PricesFresh)
	assert.Equal(t, []string{solID}, e.StaleAssets)
	assert.Equal(t, e.ComputeHash(), e.Hash)

	again := mustEval(t, p, s)
	assert.Equal(t, e.Hash, again.Hash, "same state, same evaluation")
}

func TestEvaluate_StaleMarkIsZeroCollateralAndCallsMargin(t *testing.T) {
	p := DefaultPolicy()
	s := fixedAccount(t)
	s.Now = t0.Add(31 * time.Second) // one second past the 30 s limit
	e := mustEval(t, p, s)
	var solV HoldingValuation
	for _, h := range e.Holdings {
		if h.AssetID == solID {
			solV = h
		}
	}
	assert.True(t, solV.Haircut.ZeroValue)
	assert.Equal(t, money.BPS(10_000), solV.Haircut.TotalBPS)
	requireQ(t, money.Quantity{}, solV.CollateralValue, "stale collateral is worth nothing")
	// 31 s of interest on 2,000 at 8%: 2,000 × 0.08 × 31 / 31,536,000 = 0.000157... → 158 units.
	requireQ(t, money.QuantityFromInt64(158), e.AccruedInterest, "accrued interest")
	requireQ(t, usd(-1_000).Sub(money.QuantityFromInt64(158)), e.Equity, "1,000 cash − 2,000 loan − interest")
	assert.Equal(t, StatusMarginCall, e.Health)
	assert.False(t, e.PricesFresh)

	s.Now = t0.Add(30 * time.Second) // exactly at the limit is fresh
	assert.True(t, mustEval(t, p, s).PricesFresh)
	s.Now = t0.Add(30*time.Second + time.Microsecond)
	assert.False(t, mustEval(t, p, s).PricesFresh, "a fraction of a millisecond past the limit is past it")
}

func TestEvaluate_AnAccountThatOwesNothingIsNeverCalled(t *testing.T) {
	p := DefaultPolicy()
	s := fixedAccount(t)
	s.Loans = nil
	s.Now = t0.Add(time.Hour) // stale: the SOL is worth nothing as collateral
	e := mustEval(t, p, s)
	assert.False(t, e.Levered)
	assert.Equal(t, StatusNormal, e.Health)
	assert.False(t, e.LiquidationEligible)
	requireQ(t, money.Quantity{}, e.InitialMargin, "no requirement without a liability")
	requireQ(t, e.Equity, e.MarginAvailable, "everything is available")
}

func TestEvaluate_RefusesStatesItCannotVouchFor(t *testing.T) {
	p := DefaultPolicy()
	cases := map[string]func(*AccountState){
		"no now":                  func(s *AccountState) { s.Now = time.Time{} },
		"cash account with loan":  func(s *AccountState) { s.Mode = ModeCash },
		"negative holding":        func(s *AccountState) { s.Holdings[1].Quantity = money.QuantityFromInt64(-1) },
		"mark in another quote":   func(s *AccountState) { m := *s.Holdings[1].Mark; m.QuoteAsset = "eur"; s.Holdings[1].Mark = &m },
		"duplicate holding":       func(s *AccountState) { s.Holdings = append(s.Holdings, s.Holdings[1]) },
		"numeraire flag mismatch": func(s *AccountState) { s.Holdings[1].Numeraire = true },
		"isolated with two risky": func(s *AccountState) {
			s.Mode, s.IsolatedAssetID = ModeIsolated, ethID
		},
		"derivative mm above im": func(s *AccountState) {
			s.Derivatives = []DerivativeExposure{{InstrumentID: "x", InitialMargin: money.USDFromMinor(1), MaintenanceMargin: money.USDFromMinor(2)}}
		},
		"numeraire with one decimal": func(s *AccountState) { s.NumeraireDecimals = 1; s.Holdings[0].Decimals = 1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := fixedAccount(t)
			mutate(&s)
			_, err := Evaluate(p, s)
			require.Error(t, err)
		})
	}
}

func TestEvaluate_LeverageIsUnboundedWithoutPositiveEquity(t *testing.T) {
	p := DefaultPolicy()
	s := fixedAccount(t)
	s.Holdings[1].Mark = px(t, "50", t0)
	e := mustEval(t, p, s)
	assert.True(t, e.Equity.IsNegative())
	assert.True(t, e.LeverageUnbounded)
	assert.Equal(t, MaxLeverageBPS, e.EffectiveLeverageBPS)
	requireQ(t, money.Quantity{}, e.MarginAvailable, "nothing is available")
}

func TestRiskSections_RoundAgainstTheAccount(t *testing.T) {
	p := DefaultPolicy()
	s := fixedAccount(t)
	s.Holdings[0].Quantity = units(t, "1000.005") // a half cent of cash
	e := mustEval(t, p, s)
	rs, err := RiskSectionsFor(e)
	require.NoError(t, err)
	assert.Equal(t, "500.00", rs.Margin.EquityAfter.String(), "equity rounds down")
	assert.Equal(t, t0, rs.Oracle.ObservedAt)
	assert.True(t, rs.Oracle.Available)
	assert.Equal(t, "2000.00", rs.Concentration.PositionAfter.String())
}

func TestEffectiveRiskPolicy_TightensToTheMode(t *testing.T) {
	base := DefaultPolicy()
	rp := EffectiveRiskPolicy(riskBase(), base, ModeIsolated)
	assert.Equal(t, money.BPS(30_000), rp.MaxLeverageBPS)
	assert.Contains(t, rp.Version, "ISOLATED")
	rp = EffectiveRiskPolicy(riskBase(), base, ModeCross)
	assert.Equal(t, money.BPS(50_000), rp.MaxLeverageBPS)
}
