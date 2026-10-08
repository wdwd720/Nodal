package margin

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/money"
)

func results(t testing.TB, r StressReport) map[StressScenario]StressResult {
	t.Helper()
	out := map[StressScenario]StressResult{}
	for _, x := range r.Results {
		out[x.Scenario] = x
	}
	require.Len(t, out, len(AllStressScenarios()))
	return out
}

// TestStress_FixedAccountGapsProduceTheExpectedNumbers pins the gap
// scenarios of the fixed account (1,000 cash + 20 SOL at 100 against a 2,000
// loan) to hand arithmetic. Every gap keeps SOL above 100% of net asset
// value, so its haircut stays 25%:
//
//	gap  SOL  collateral        equity   MM    outcome
//	 5%   95  1,000 + 1,425       425   190    none
//	10%   90  1,000 + 1,350       350   180    none
//	25%   75  1,000 + 1,125       125   150    partial sale restores, no deficit
//	50%   50  1,000 +   750      −250   100    sell all, apply cash, 12.475 deficit
//
// and a gap up only helps a long spot book: at +50% equity is 1,000 + 2,250
// − 2,000 = 1,250.
func TestStress_FixedAccountGapsProduceTheExpectedNumbers(t *testing.T) {
	p := DefaultPolicy()
	rep, err := RunStress(p, fixedAccount(t))
	require.NoError(t, err)
	r := results(t, rep)

	type row struct {
		sc       StressScenario
		equity   string
		mm       string
		liq      bool
		outcome  PlanDecision
		steps    int
		deficit  string
		deficitC string
	}
	for _, w := range []row{
		{StressGapDown5, "425", "190", false, DecisionComplete, 0, "0", "0.00"},
		{StressGapDown10, "350", "180", false, DecisionComplete, 0, "0", "0.00"},
		{StressGapDown25, "125", "150", true, DecisionComplete, 1, "0", "0.00"},
		{StressGapDown50, "-250", "100", true, DecisionBankrupt, 2, "12.475", "12.48"},
		{StressGapUp5, "575", "210", false, DecisionComplete, 0, "0", "0.00"},
		{StressGapUp50, "1250", "300", false, DecisionComplete, 0, "0", "0.00"},
	} {
		got := r[w.sc]
		requireQ(t, units(t, w.equity), got.EquityAfter, string(w.sc)+" equity")
		requireQ(t, units(t, w.mm), got.MaintenanceAfter, string(w.sc)+" maintenance")
		assert.Equal(t, w.liq, got.WouldLiquidate, w.sc)
		assert.Equal(t, w.outcome, got.Outcome, w.sc)
		assert.Equal(t, w.steps, got.StepsSimulated, w.sc)
		requireQ(t, units(t, w.deficit), got.Deficit, string(w.sc)+" deficit")
		assert.Equal(t, w.deficitC, got.DeficitUSD.String(), w.sc)
		requireQ(t, got.Deficit, got.InsuranceFundDraw, string(w.sc)+" the fund covers the deficit")
		assert.True(t, got.Executable, w.sc)
	}

	// Oracle unavailable: SOL is worth nothing as collateral and the case
	// would wait; the conservative shortfall is 2,000 − 1,000.
	o := r[StressOracleUnavailable]
	assert.True(t, o.WouldLiquidate)
	assert.False(t, o.Executable)
	assert.Equal(t, DecisionAwaitPrice, o.Outcome)
	requireQ(t, usd(1_000), o.Deficit, "oracle unavailable shortfall")

	// Venue unavailable after the 25% execution gap: eligible, cannot sell,
	// equity still positive so nothing is owed yet.
	v := r[StressVenueUnavailable]
	assert.True(t, v.WouldLiquidate)
	assert.False(t, v.Executable)
	assert.Equal(t, DecisionAwaitVenue, v.Outcome)
	requireQ(t, money.Quantity{}, v.Deficit, "venue unavailable")

	// Partial fills halve every step: the case uses its whole budget and goes
	// to a person.
	pf := r[StressPartialFill]
	assert.Equal(t, DecisionManualReview, pf.Outcome)
	assert.Equal(t, p.MaxLiquidationSteps, pf.StepsSimulated)

	// Extreme spread: selling at the 5% cap still restores the account.
	es := r[StressExtremeSpread]
	assert.Equal(t, DecisionComplete, es.Outcome)
	assert.Equal(t, 1, es.StepsSimulated)

	// Nothing in flight and nothing stable: these two change nothing.
	requireQ(t, usd(500), r[StressBridgeDelayed].EquityAfter, "bridge delayed")
	requireQ(t, usd(500), r[StressCollateralDepeg].EquityAfter, "depeg")

	again, err := RunStress(p, fixedAccount(t))
	require.NoError(t, err)
	assert.Equal(t, rep.Hash, again.Hash, "stress is deterministic")
	assert.Equal(t, rep.ComputeHash(), rep.Hash)
}

func TestStress_BridgeDelayAndDepegHitTheirCollateral(t *testing.T) {
	p := DefaultPolicy()
	s := fixedAccount(t)
	s.Holdings = append(s.Holdings,
		Holding{AssetID: stbID, Decimals: 6, Quantity: usd(500), Mark: px(t, "1", t0),
			Profile: AssetProfile{Volatility: VolStable, Liquidity: LiqDeep, Venue: VenueTier1, RiskWeightBPS: 10_000}},
		Holding{AssetID: ethID, Decimals: 9, Quantity: sol(1), Mark: px(t, "100", t0), InFlight: true, InFlightMS: 1,
			Profile: AssetProfile{Volatility: VolLow, Liquidity: LiqDeep, Venue: VenueTier1, RiskWeightBPS: 10_000}})
	base := mustEval(t, p, s)
	rep, err := RunStress(p, s)
	require.NoError(t, err)
	r := results(t, rep)
	assert.True(t, r[StressBridgeDelayed].EquityAfter.Cmp(base.Equity) < 0, "in-flight collateral is worth nothing when the bridge is stuck")
	assert.True(t, r[StressCollateralDepeg].EquityAfter.Cmp(base.Equity) < 0, "a depeg marks stable collateral down")
	assert.True(t, r[StressGapDown10].EquityAfter.Cmp(base.Equity) < 0)
}

func TestStress_DerivativesLoseInBothDirections(t *testing.T) {
	p := DefaultPolicy()
	s := AccountState{AccountID: "acct-d", Mode: ModeCross, NumeraireAssetID: usdID, NumeraireDecimals: 6, Now: t0,
		Holdings: []Holding{{AssetID: usdID, Decimals: 6, Quantity: usd(1_000), Numeraire: true}},
		Derivatives: []DerivativeExposure{{InstrumentID: "perp", GrossNotional: money.USDFromMinor(200_000),
			InitialMargin: money.USDFromMinor(20_000), MaintenanceMargin: money.USDFromMinor(10_000), RiskIncreasingDirection: true}}}
	rep, err := RunStress(p, s)
	require.NoError(t, err)
	r := results(t, rep)
	// 10% of 2,000 notional is 200 lost either way.
	requireQ(t, usd(800), r[StressGapDown10].EquityAfter, "gap down")
	requireQ(t, usd(800), r[StressGapUp10].EquityAfter, "gap up")
}
