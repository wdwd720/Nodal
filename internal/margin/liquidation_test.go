package margin

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/money"
)

func plan(t testing.TB, p Policy, in PlanInput) Plan {
	t.Helper()
	pl, err := PlanLiquidation(p, in)
	require.NoError(t, err)
	return pl
}

func atPrice(t testing.TB, s AccountState, price string) AccountState {
	out := s.Clone()
	out.Holdings[1].Mark = px(t, price, out.Now)
	return out
}

func restoredAfter(t testing.TB, p Policy, s AccountState, o LiquidationOrder) bool {
	t.Helper()
	st := s.Clone()
	_, err := applySale(p, &st, o.AssetID, o.Quantity, *o.ExecutionPrice, o.SlippageBPS)
	require.NoError(t, err)
	return mustEval(t, p, st).Restored(p)
}

func TestPlan_HealthyAccountNeedsNothing(t *testing.T) {
	pl := plan(t, DefaultPolicy(), PlanInput{State: fixedAccount(t), VenueAvailable: true})
	assert.Equal(t, DecisionComplete, pl.Decision)
	assert.Empty(t, pl.Orders)
	assert.Equal(t, pl.ComputeHash(), pl.Hash)
}

// TestPlan_StalePriceRefusal: an eligible account whose mark is stale (or
// missing) is not liquidated; the case waits.
func TestPlan_StalePriceRefusal(t *testing.T) {
	p := DefaultPolicy()
	s := atPrice(t, fixedAccount(t), "75")
	s.Now = s.Now.Add(time.Minute)
	pl := plan(t, p, PlanInput{State: s, VenueAvailable: true})
	assert.True(t, pl.Before.LiquidationEligible)
	assert.Equal(t, DecisionAwaitPrice, pl.Decision)
	assert.Empty(t, pl.Orders)

	s.Holdings[1].Mark = nil
	pl = plan(t, p, PlanInput{State: s, VenueAvailable: true})
	assert.Equal(t, DecisionAwaitPrice, pl.Decision)

	_, _, err := ExecuteFirst(p, s, pl, 10_000)
	require.Error(t, err, "a waiting plan has nothing to execute")
}

func TestPlan_VenueUnavailableWaits(t *testing.T) {
	pl := plan(t, DefaultPolicy(), PlanInput{State: atPrice(t, fixedAccount(t), "75"), VenueAvailable: false})
	assert.Equal(t, DecisionAwaitVenue, pl.Decision)
	assert.Empty(t, pl.Orders)
}

// TestPlan_PartialLiquidationIsTheSmallestThatRestores: at 75 the account is
// below maintenance; the plan sells the least SOL that restores maintenance
// plus the buffer. By hand: equity' = 125 + 17.814375 q and target' = 187.5 −
// 9.375 q, so q ≈ 2.2987 SOL; one base unit less does not restore.
func TestPlan_PartialLiquidationIsTheSmallestThatRestores(t *testing.T) {
	p := DefaultPolicy()
	s := atPrice(t, fixedAccount(t), "75")
	pl := plan(t, p, PlanInput{State: s, VenueAvailable: true})
	require.Equal(t, DecisionSell, pl.Decision)
	require.Len(t, pl.Orders, 1)
	o := pl.Orders[0]
	assert.Equal(t, solID, o.AssetID)
	assert.True(t, o.Quantity.Cmp(money.QuantityFromInt64(2_298_000_000)) > 0 && o.Quantity.Cmp(money.QuantityFromInt64(2_300_000_000)) < 0,
		"about 2.2987 SOL, got %s", o.Quantity)
	assert.True(t, restoredAfter(t, p, s, o))
	less := o
	less.Quantity = o.Quantity.Sub(money.QuantityFromInt64(1))
	assert.False(t, restoredAfter(t, p, s, less), "one base unit less does not restore the target")
	assert.Equal(t, money.BPS(25), o.SlippageBPS, "DEEP liquidity slippage")
	requireQ(t, o.Proceeds, o.Fee.Add(o.Repay).Add(o.Excess), "proceeds split")
	require.NotNil(t, pl.After)
	assert.True(t, pl.After.Restored(p))

	again := plan(t, p, PlanInput{State: s, VenueAvailable: true})
	assert.Equal(t, pl.Hash, again.Hash, "same state, same plan")
}

// TestPlan_FullLiquidationThenCollateralThenBankruptcy follows the 50% gap by
// hand: 20 SOL at 50 × 0.9975 = 997.50 of proceeds, a 9.975 fee, 987.525
// repaid; then the 1,000 of cash repays 1,000; 12.475 remains owed.
func TestPlan_FullLiquidationThenCollateralThenBankruptcy(t *testing.T) {
	p := DefaultPolicy()
	s := atPrice(t, fixedAccount(t), "50")
	pl := plan(t, p, PlanInput{State: s, VenueAvailable: true})
	require.Equal(t, DecisionSellAll, pl.Decision)
	s1, o1, err := ExecuteFirst(p, s, pl, 10_000)
	require.NoError(t, err)
	requireQ(t, sol(20), o1.Quantity, "sells everything")
	requireQ(t, units(t, "997.5"), o1.Proceeds, "proceeds")
	requireQ(t, units(t, "9.975"), o1.Fee, "fee")
	requireQ(t, units(t, "987.525"), o1.Repay, "repaid")
	requireQ(t, money.Quantity{}, o1.Excess, "no excess")

	pl2 := plan(t, p, PlanInput{State: s1, StepsTaken: 1, VenueAvailable: false})
	require.Equal(t, DecisionApplyCollateral, pl2.Decision, "applying cash needs no venue")
	s2, o2, err := ExecuteFirst(p, s1, pl2, 10_000)
	require.NoError(t, err)
	requireQ(t, usd(1_000), o2.Quantity, "all the cash")

	pl3 := plan(t, p, PlanInput{State: s2, StepsTaken: 2, VenueAvailable: true})
	require.Equal(t, DecisionBankrupt, pl3.Decision)
	requireQ(t, units(t, "12.475"), pl3.DeficitUnits, "deficit")
	requireQ(t, units(t, "12.475"), s2.LoanTotal(), "the deficit is what remains owed")
}

func TestPlan_RiskiestHoldingIsSoldFirst(t *testing.T) {
	p := DefaultPolicy()
	s := fixedAccount(t)
	s.Holdings = append(s.Holdings, Holding{AssetID: ethID, Decimals: 9, Quantity: sol(1), Mark: px(t, "100", t0),
		Profile: AssetProfile{Volatility: VolHigh, Liquidity: LiqDeep, Venue: VenueTier1, RiskWeightBPS: 10_000}})
	s.Holdings[1].Mark = px(t, "70", t0)
	pl := plan(t, p, PlanInput{State: s, VenueAvailable: true})
	require.Contains(t, []PlanDecision{DecisionSell, DecisionSellAll}, pl.Decision)
	assert.Equal(t, ethID, pl.Orders[0].AssetID, "the HIGH-volatility holding carries the larger haircut")
	assert.True(t, pl.Orders[0].Quantity.Equal(sol(1)) || len(pl.Orders) == 1)
}

func TestPlan_StepBudgetEndsInManualReview(t *testing.T) {
	p := DefaultPolicy()
	pl := plan(t, p, PlanInput{State: atPrice(t, fixedAccount(t), "75"), StepsTaken: p.MaxLiquidationSteps, VenueAvailable: true})
	assert.Equal(t, DecisionManualReview, pl.Decision)
}

// TestPlan_TerminalAccountingIsDecidedBeforeTheStepBudget (F1): a case that
// has spent its trading budget but has nothing left to sell still applies its
// numeraire collateral and still reaches BANKRUPT. It never lands in
// MANUAL_REVIEW for a budget that bounds trading, not the accounting that
// ends a case; with something left to sell, the spent budget still stops it.
func TestPlan_TerminalAccountingIsDecidedBeforeTheStepBudget(t *testing.T) {
	p := DefaultPolicy()
	s := atPrice(t, fixedAccount(t), "50")
	pl := plan(t, p, PlanInput{State: s, VenueAvailable: true})
	require.Equal(t, DecisionSellAll, pl.Decision)
	s1, _, err := ExecuteFirst(p, s, pl, 10_000)
	require.NoError(t, err)

	spent := p.MaxLiquidationSteps
	pl2 := plan(t, p, PlanInput{State: s1, StepsTaken: spent, VenueAvailable: true})
	require.Equal(t, DecisionApplyCollateral, pl2.Decision, "applying collateral is not a trading step")
	s2, _, err := ExecuteFirst(p, s1, pl2, 10_000)
	require.NoError(t, err)
	pl3 := plan(t, p, PlanInput{State: s2, StepsTaken: spent + 7, VenueAvailable: true})
	require.Equal(t, DecisionBankrupt, pl3.Decision, "bankruptcy is decided whatever the budget")
	requireQ(t, units(t, "12.475"), pl3.DeficitUnits, "deficit")

	stuck := plan(t, p, PlanInput{State: atPrice(t, fixedAccount(t), "75"), StepsTaken: spent, VenueAvailable: true})
	assert.Equal(t, DecisionManualReview, stuck.Decision, "with something left to sell, the spent budget stops the case")
	_, err = PlanLiquidation(p, PlanInput{State: s2, StepsTaken: -1})
	require.Error(t, err)
}

// TestPlan_FreeWalletCashRepaysTheLoansBeforeBankruptcy (F2): with nothing left
// in the account, loans remaining and free numeraire cash in the customer's
// wallet, the plan sweeps min(free cash, loans) into the loans -- a recorded
// step, at any budget -- and only once nothing is left anywhere is the account
// bankrupt. Enough free cash restores the account outright.
func TestPlan_FreeWalletCashRepaysTheLoansBeforeBankruptcy(t *testing.T) {
	p := DefaultPolicy()
	s := atPrice(t, fixedAccount(t), "50")
	pl := plan(t, p, PlanInput{State: s, VenueAvailable: true})
	s1, _, err := ExecuteFirst(p, s, pl, 10_000)
	require.NoError(t, err)
	pl2 := plan(t, p, PlanInput{State: s1, VenueAvailable: true, FreeNumeraire: usd(5)})
	require.Equal(t, DecisionApplyCollateral, pl2.Decision, "collateral in the account is applied first")
	s2, _, err := ExecuteFirst(p, s1, pl2, 10_000)
	require.NoError(t, err)

	partial := plan(t, p, PlanInput{State: s2, StepsTaken: p.MaxLiquidationSteps, VenueAvailable: false, FreeNumeraire: usd(5)})
	require.Equal(t, DecisionSweepWallet, partial.Decision, "a sweep needs neither a venue nor budget")
	require.Len(t, partial.Orders, 1)
	o := partial.Orders[0]
	assert.Equal(t, ActionSweepWallet, o.Action)
	requireQ(t, usd(5), o.Quantity, "all the free cash, less than the loans")
	requireQ(t, usd(5), o.Repay, "it all repays")
	requireQ(t, money.Quantity{}, o.Fee, "no fee on a sweep")
	s3, done, err := ExecuteFirst(p, s2, partial, 1)
	require.NoError(t, err)
	requireQ(t, usd(5), done.Quantity, "a sweep is bookkeeping, never a partial fill")
	requireQ(t, units(t, "7.475"), s3.LoanTotal(), "12.475 owed less 5 swept")
	requireQ(t, sol(0), s3.Holdings[1].Quantity, "the account's holdings are untouched by a sweep")

	broke := plan(t, p, PlanInput{State: s3, VenueAvailable: true})
	require.Equal(t, DecisionBankrupt, broke.Decision, "nothing left anywhere")
	requireQ(t, units(t, "7.475"), broke.DeficitUnits, "the deficit is what the sweep could not repay")

	rich := plan(t, p, PlanInput{State: s2, VenueAvailable: true, FreeNumeraire: usd(1_000)})
	require.Equal(t, DecisionSweepWallet, rich.Decision)
	requireQ(t, units(t, "12.475"), rich.Orders[0].Quantity, "never more than is owed")
	require.NotNil(t, rich.After)
	assert.True(t, rich.After.Restored(p), "the loans are repaid in full")

	_, err = PlanLiquidation(p, PlanInput{State: s2, FreeNumeraire: money.QuantityFromInt64(-1)})
	require.Error(t, err)
}

func TestPlan_SlippageIsCapped(t *testing.T) {
	p := DefaultPolicy()
	p.LiquidationSlippageCapBPS = 300
	s := atPrice(t, fixedAccount(t), "60")
	s.Holdings[1].Profile.Liquidity = LiqIlliquid // 400 BPS of slippage, above the cap
	pl := plan(t, p, PlanInput{State: s, VenueAvailable: true})
	require.NotEmpty(t, pl.Orders)
	assert.Equal(t, money.BPS(300), pl.Orders[0].SlippageBPS)
	want, err := scalePrice(*pl.Orders[0].Mark, 10_000-300)
	require.NoError(t, err)
	assert.True(t, want.Mantissa.Equal(pl.Orders[0].ExecutionPrice.Mantissa) && want.Scale == pl.Orders[0].ExecutionPrice.Scale)

	pl = plan(t, DefaultPolicy(), PlanInput{State: atPrice(t, fixedAccount(t), "60"), VenueAvailable: true, SlippageAtCap: true})
	assert.Equal(t, DefaultPolicy().LiquidationSlippageCapBPS, pl.Orders[0].SlippageBPS, "extreme spread trades at the cap")
}

func TestExecuteFirst_PartialFillSellsLessAndAtLeastOneUnit(t *testing.T) {
	p := DefaultPolicy()
	s := atPrice(t, fixedAccount(t), "75")
	pl := plan(t, p, PlanInput{State: s, VenueAvailable: true})
	_, half, err := ExecuteFirst(p, s, pl, 5_000)
	require.NoError(t, err)
	want, err := pl.Orders[0].Quantity.Div(money.QuantityFromInt64(2), money.RoundFloor)
	require.NoError(t, err)
	requireQ(t, want, half.Quantity, "half fill")
	assert.True(t, FillQuantity(money.QuantityFromInt64(1), 1).Equal(money.QuantityFromInt64(1)), "a fill always makes progress")
}

func TestPlan_DerivativeShortfallIsForAPerson(t *testing.T) {
	p := DefaultPolicy()
	s := AccountState{AccountID: "acct-d", Mode: ModeCross, NumeraireAssetID: usdID, NumeraireDecimals: 6, Now: t0,
		Holdings: []Holding{{AssetID: usdID, Decimals: 6, Quantity: usd(10), Numeraire: true}},
		Derivatives: []DerivativeExposure{{InstrumentID: "perp", GrossNotional: money.USDFromMinor(100_000),
			InitialMargin: money.USDFromMinor(10_000), MaintenanceMargin: money.USDFromMinor(5_000), RiskIncreasingDirection: true}}}
	pl := plan(t, p, PlanInput{State: s, VenueAvailable: true})
	assert.Equal(t, DecisionManualReview, pl.Decision, "spot liquidation cannot close a derivative")
}

func TestApplySale_IsReduceOnly(t *testing.T) {
	p := DefaultPolicy()
	s := fixedAccount(t)
	_, err := applySale(p, &s, solID, sol(21), *px(t, "100", t0), 0)
	require.Error(t, err, "cannot sell more than is held")
	_, err = applySale(p, &s, usdID, usd(1), *px(t, "1", t0), 0)
	require.Error(t, err, "the numeraire is not sold")
}
