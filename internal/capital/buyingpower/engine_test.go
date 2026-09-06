package buyingpower_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital/buyingpower"
	"github.com/nodal/controlplane/internal/capital/buyingpower/buyingpowertest"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuation/valuationtest"
)

var (
	now    = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	usdcID = assets.NewAssetID()
	solID  = assets.NewAssetID()
	bonkID = assets.NewAssetID()
	usdtID = assets.NewAssetID()
	acctID = accounts.NewAccountID()
)

func usdc() assets.Asset {
	return assets.Asset{ID: usdcID, Symbol: "USDC", Decimals: 6, IsStablecoin: true, PegCurrency: "USD", RiskClass: assets.RiskSettlement, Status: assets.StatusActive}
}

func usdt() assets.Asset {
	return assets.Asset{ID: usdtID, Symbol: "USDT", Decimals: 6, IsStablecoin: true, PegCurrency: "USD", RiskClass: assets.RiskStandard, Status: assets.StatusActive}
}

func sol() assets.Asset {
	return assets.Asset{ID: solID, Symbol: "SOL", Decimals: 9, RiskClass: assets.RiskMajor, Status: assets.StatusActive}
}

func bonk() assets.Asset {
	return assets.Asset{ID: bonkID, Symbol: "BONK", Decimals: 5, RiskClass: assets.RiskSpeculative, Status: assets.StatusActive}
}

func q(t testing.TB, s string) money.Quantity {
	t.Helper()
	v, err := money.ParseQuantity(s)
	require.NoError(t, err)
	return v
}

func price(t testing.TB, decimal string, at time.Time) money.Price {
	t.Helper()
	p, err := money.PriceFromDecimalString(decimal, usdcID.String(), "pyth", at)
	require.NoError(t, err)
	return p
}

func pol(asset assets.AssetID, status assets.Status, factor money.BPS, stable valuation.StablecoinStatus, version string) valuation.AssetPolicy {
	return valuation.AssetPolicy{AssetID: asset, Status: status, CollateralFactor: factor, StablecoinStatus: stable, MaxPriceAge: 30 * time.Second, PolicyVersion: version}
}

type fixture struct {
	engine   *buyingpower.Engine
	policies *valuationtest.Policies
	prices   *valuationtest.Prices
	kills    *buyingpowertest.KillSwitches
	recon    *buyingpowertest.ReconciliationBlocks
	clk      *clock.Fake
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		policies: valuationtest.NewPolicies(),
		prices:   valuationtest.NewPrices(),
		kills:    &buyingpowertest.KillSwitches{},
		recon:    &buyingpowertest.ReconciliationBlocks{},
		clk:      clock.NewFake(now),
	}
	f.policies.
		Set(pol(usdcID, assets.StatusActive, 10000, valuation.StablecoinNormal, "usdc-1")).
		Set(pol(solID, assets.StatusActive, 7500, "", "sol-1")).
		Set(pol(bonkID, assets.StatusActive, 2500, "", "bonk-1")).
		Set(pol(usdtID, assets.StatusActive, 9000, valuation.StablecoinDegraded, "usdt-1"))
	f.prices.
		Set(solID, usdcID, price(t, "150.123456", now.Add(-5*time.Second))).
		Set(bonkID, usdcID, price(t, "0.00002345", now.Add(-5*time.Second))).
		Set(usdtID, usdcID, price(t, "0.970000", now.Add(-5*time.Second)))
	var err error
	f.engine, err = buyingpower.NewEngine(buyingpower.Deps{
		Clock: f.clk, Policies: f.policies, Prices: f.prices, KillSwitches: f.kills, Reconciliation: f.recon, QuoteAssetID: usdcID,
	})
	require.NoError(t, err)
	return f
}

// baseSnapshot: 1000 USDC (100 reserved, 50 held), 1.5 SOL, 1M BONK,
// 200 USDC pending, 1000 USDC withdrawal-eligible funding, no withdrawals.
func baseSnapshot(t testing.TB) buyingpower.Snapshot {
	t.Helper()
	return buyingpower.Snapshot{
		AccountID:     acctID,
		AccountStatus: accounts.StatusActive,
		QuoteAsset:    usdc(),
		Assets:        map[assets.AssetID]assets.Asset{usdcID: usdc(), solID: sol(), bonkID: bonk()},
		Balances: map[assets.AssetID]money.Quantity{
			usdcID: q(t, "1000000000"),
			solID:  q(t, "1500000000"),
			bonkID: q(t, "100000000000"),
		},
		Reserved:        map[assets.AssetID]money.Quantity{usdcID: q(t, "100000000")},
		Holds:           map[assets.AssetID]money.Quantity{usdcID: q(t, "50000000")},
		Pending:         map[assets.AssetID]money.Quantity{usdcID: q(t, "200000000")},
		EligibleFunding: map[assets.AssetID]money.Quantity{usdcID: q(t, "1000000000")},
		Withdrawals:     map[assets.AssetID]money.Quantity{},
	}
}

func eval(t *testing.T, f *fixture, snap buyingpower.Snapshot, p buyingpower.Purpose) buyingpower.BuyingPower {
	t.Helper()
	out, err := f.engine.Evaluate(t.Context(), nil, snap, p)
	require.NoError(t, err)
	return out
}

func codes(bp buyingpower.BuyingPower) []buyingpower.RestrictionCode {
	var out []buyingpower.RestrictionCode
	for _, r := range bp.Restrictions {
		out = append(out, r.Code)
	}
	return out
}

func TestEvaluate_Golden_Unrestricted(t *testing.T) {
	f := newFixture(t)
	bp := eval(t, f, baseSnapshot(t), buyingpower.PurposeTrade)

	// SOL 1.5 × 150.123456 = 225.19; BONK 1M × 0.00002345 = 23.45; USDC 1000 at face.
	assert.Equal(t, "1248.64", bp.PortfolioValue.String())
	// USDC net 850 × 100% + SOL 225.19 × 75% = 168.89 + BONK 23.45 × 25% = 5.86.
	assert.Equal(t, "1024.75", bp.BuyingPower.String())
	assert.Equal(t, "850.00", bp.AvailableNow.String())
	assert.Equal(t, "100.00", bp.Reserved.String())
	assert.Equal(t, "200.00", bp.Pending.String())
	assert.Equal(t, "850.00", bp.Withdrawable.String(), "min(available_now, eligible funding)")
	assert.Empty(t, bp.Restrictions)
	assert.False(t, bp.Blocked())
	assert.Equal(t, now, bp.AsOf)
	assert.Equal(t, buyingpower.PurposeTrade, bp.Purpose)
	assert.Contains(t, bp.PolicyVersion, buyingpower.EngineVersion+":")

	require.Len(t, bp.UnderlyingBalances, 3)
	by := map[assets.AssetID]buyingpower.UnderlyingBalance{}
	for _, u := range bp.UnderlyingBalances {
		by[u.AssetID] = u
	}
	assert.Equal(t, "1000.00", by[usdcID].USDValue.String())
	assert.Equal(t, "face:USD", by[usdcID].PriceRef)
	assert.Equal(t, "NORMAL", by[usdcID].Status)
	assert.Equal(t, "225.19", by[solID].USDValue.String())
	assert.Equal(t, "pyth@2026-09-05T11:59:55Z", by[solID].PriceRef)
	assert.Equal(t, "ACTIVE", by[solID].Status)
	assert.Equal(t, uint8(9), by[solID].Decimals)
	assert.Equal(t, "23.45", by[bonkID].USDValue.String())

	require.Len(t, bp.Haircuts, 2, "USDC at 100% has no haircut")
	hc := map[assets.AssetID]buyingpower.Haircut{}
	for _, h := range bp.Haircuts {
		hc[h.AssetID] = h
	}
	assert.Equal(t, money.BPS(7500), hc[solID].FactorBPS)
	assert.Equal(t, valuation.ReasonCollateralFactor, hc[solID].Reason)
	assert.Equal(t, money.BPS(2500), hc[bonkID].FactorBPS)

	// Figures are purpose-independent.
	for _, p := range []buyingpower.Purpose{buyingpower.PurposeDisplay, buyingpower.PurposeAgentDeploy, buyingpower.PurposeWithdrawal} {
		other := eval(t, f, baseSnapshot(t), p)
		assert.Equal(t, bp.BuyingPower, other.BuyingPower)
		assert.Equal(t, bp.Withdrawable, other.Withdrawable)
		assert.Equal(t, bp.PolicyVersion, other.PolicyVersion)
	}
}

func TestEvaluate_AgentDeployBlocksLikeTrade(t *testing.T) {
	f := newFixture(t)
	snap := baseSnapshot(t)
	snap.AccountStatus = accounts.StatusFrozen
	f.prices.Set(solID, usdcID, price(t, "150.123456", now.Add(-time.Hour)))
	agent := eval(t, f, snap, buyingpower.PurposeAgentDeploy)
	trade := eval(t, f, snap, buyingpower.PurposeTrade)
	require.Len(t, agent.Restrictions, 2)
	for i := range agent.Restrictions {
		assert.Equal(t, trade.Restrictions[i].Blocking, agent.Restrictions[i].Blocking)
	}
	assert.True(t, agent.Blocked())
	assert.Equal(t, buyingpower.PurposeAgentDeploy, agent.Purpose)
}

func TestEvaluate_WithdrawableIsBoundedByFundingAndWithdrawals(t *testing.T) {
	f := newFixture(t)
	snap := baseSnapshot(t)
	snap.EligibleFunding[usdcID] = q(t, "300000000")
	snap.Withdrawals[usdcID] = q(t, "120000000")
	bp := eval(t, f, snap, buyingpower.PurposeWithdrawal)
	assert.Equal(t, "850.00", bp.AvailableNow.String())
	assert.Equal(t, "180.00", bp.Withdrawable.String(), "300 funded − 120 withdrawn")

	snap.Withdrawals[usdcID] = q(t, "400000000")
	bp = eval(t, f, snap, buyingpower.PurposeWithdrawal)
	assert.True(t, bp.Withdrawable.IsZero(), "never negative")

	delete(snap.Withdrawals, usdcID)
	snap.EligibleFunding[usdcID] = q(t, "5000000000")
	bp = eval(t, f, snap, buyingpower.PurposeWithdrawal)
	assert.Equal(t, "850.00", bp.Withdrawable.String(), "capped by available_now")
}

// TestEvaluate_Restrictions covers every restriction code with every purpose.
func TestEvaluate_Restrictions(t *testing.T) {
	type expect struct {
		codes        []buyingpower.RestrictionCode
		buying       string
		available    string
		portfolio    string
		blockDisplay bool
		blockTrade   bool
		blockWithdr  bool
	}
	cases := []struct {
		name  string
		setup func(f *fixture, s *buyingpower.Snapshot)
		want  expect
	}{
		{
			"account frozen", func(_ *fixture, s *buyingpower.Snapshot) { s.AccountStatus = accounts.StatusFrozen },
			expect{[]buyingpower.RestrictionCode{buyingpower.RestrictionAccountFrozen}, "0.00", "0.00", "1248.64", false, true, true},
		},
		{
			"account restricted", func(_ *fixture, s *buyingpower.Snapshot) { s.AccountStatus = accounts.StatusRestricted },
			expect{[]buyingpower.RestrictionCode{buyingpower.RestrictionAccountRestricted}, "0.00", "0.00", "1248.64", false, true, true},
		},
		{
			"account closed", func(_ *fixture, s *buyingpower.Snapshot) { s.AccountStatus = accounts.StatusClosed },
			expect{[]buyingpower.RestrictionCode{buyingpower.RestrictionAccountClosed}, "0.00", "0.00", "1248.64", false, true, true},
		},
		{
			"unknown account status fails closed", func(_ *fixture, s *buyingpower.Snapshot) { s.AccountStatus = "LIMBO" },
			expect{[]buyingpower.RestrictionCode{buyingpower.RestrictionAccountFrozen}, "0.00", "0.00", "1248.64", false, true, true},
		},
		{"kill switch blocking new risk", func(_ *fixture, s *buyingpower.Snapshot) {
			s.KillSwitches = []buyingpower.KillSwitchState{{Kind: "GLOBAL_NEW_RISK_KILL", ScopeID: "*", Reason: "incident 42", BlocksNewRisk: true}}
		}, expect{[]buyingpower.RestrictionCode{buyingpower.RestrictionKillSwitch}, "0.00", "0.00", "1248.64", false, true, true}},
		{"kill switch blocking withdrawals only", func(_ *fixture, s *buyingpower.Snapshot) {
			s.KillSwitches = []buyingpower.KillSwitchState{{Kind: "WITHDRAWALS_DISABLE", BlocksWithdrawal: true}}
		}, expect{[]buyingpower.RestrictionCode{buyingpower.RestrictionKillSwitch}, "1024.75", "850.00", "1248.64", false, false, true}},
		{"kill switch with neither flag is ignored", func(_ *fixture, s *buyingpower.Snapshot) {
			s.KillSwitches = []buyingpower.KillSwitchState{{Kind: "MODEL_DISABLE"}}
		}, expect{nil, "1024.75", "850.00", "1248.64", false, false, false}},
		{"reconciliation block", func(_ *fixture, s *buyingpower.Snapshot) {
			s.ReconciliationBlocks = []buyingpower.ReconciliationBlock{{RecordID: "r1", Kind: "WALLET_BALANCE", Detail: "0.5 SOL unexplained"}}
		}, expect{[]buyingpower.RestrictionCode{buyingpower.RestrictionReconciliationRequired}, "0.00", "0.00", "1248.64", false, true, true}},
		{"stale price on a held asset", func(f *fixture, _ *buyingpower.Snapshot) {
			f.prices.Set(solID, usdcID, price(t, "150.123456", now.Add(-31*time.Second)))
		}, expect{[]buyingpower.RestrictionCode{buyingpower.RestrictionStalePrice}, "855.86", "850.00", "1023.45", false, false, true}},
		{"asset halted by policy", func(f *fixture, _ *buyingpower.Snapshot) {
			f.policies.Set(pol(solID, assets.StatusHalted, 7500, "", "sol-halt"))
		}, expect{[]buyingpower.RestrictionCode{buyingpower.RestrictionAssetHalted}, "855.86", "850.00", "1023.45", false, false, true}},
		{"stablecoin halted", func(f *fixture, _ *buyingpower.Snapshot) {
			f.policies.Set(pol(usdcID, assets.StatusActive, 10000, valuation.StablecoinHalted, "usdc-halt"))
		}, expect{[]buyingpower.RestrictionCode{buyingpower.RestrictionAssetHalted}, "174.75", "0.00", "248.64", false, false, true}},
		{"policy missing", func(f *fixture, _ *buyingpower.Snapshot) {
			f.policies = valuationtest.NewPolicies().
				Set(pol(usdcID, assets.StatusActive, 10000, valuation.StablecoinNormal, "usdc-1")).
				Set(pol(bonkID, assets.StatusActive, 2500, "", "bonk-1"))
			var err error
			f.engine, err = buyingpower.NewEngine(buyingpower.Deps{Clock: f.clk, Policies: f.policies, Prices: f.prices, KillSwitches: f.kills, Reconciliation: f.recon, QuoteAssetID: usdcID})
			require.NoError(t, err)
		}, expect{[]buyingpower.RestrictionCode{buyingpower.RestrictionPolicyMissing}, "855.86", "850.00", "1023.45", false, false, true}},
		{"asset restricted contributes to portfolio only", func(f *fixture, _ *buyingpower.Snapshot) {
			f.policies.Set(pol(solID, assets.StatusRestricted, 7500, "", "sol-restricted"))
		}, expect{nil, "855.86", "850.00", "1248.64", false, false, false}},
		{"stablecoin restricted marks to market, no buying power", func(f *fixture, _ *buyingpower.Snapshot) {
			f.policies.Set(pol(usdcID, assets.StatusActive, 10000, valuation.StablecoinRestricted, "usdc-restricted"))
			f.prices.Set(usdcID, usdcID, price(t, "0.980000", now.Add(-time.Second)))
		}, expect{nil, "174.75", "0.00", "1228.64", false, false, false}},
		{"account frozen and stale price: both reported", func(f *fixture, s *buyingpower.Snapshot) {
			s.AccountStatus = accounts.StatusFrozen
			f.prices.Set(solID, usdcID, price(t, "150.123456", now.Add(-time.Hour)))
		}, expect{[]buyingpower.RestrictionCode{buyingpower.RestrictionAccountFrozen, buyingpower.RestrictionStalePrice}, "0.00", "0.00", "1023.45", false, true, true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			snap := baseSnapshot(t)
			tc.setup(f, &snap)
			for _, p := range []buyingpower.Purpose{buyingpower.PurposeDisplay, buyingpower.PurposeTrade, buyingpower.PurposeWithdrawal} {
				bp := eval(t, f, snap, p)
				assert.Equal(t, tc.want.codes, codes(bp), "%s codes", p)
				assert.Equal(t, tc.want.buying, bp.BuyingPower.String(), "%s buying_power", p)
				assert.Equal(t, tc.want.available, bp.AvailableNow.String(), "%s available_now", p)
				assert.Equal(t, tc.want.portfolio, bp.PortfolioValue.String(), "%s portfolio_value", p)
				if len(tc.want.codes) > 0 {
					assert.True(t, bp.Withdrawable.IsZero(), "%s: any restriction zeroes withdrawable", p)
				} else {
					// Without restrictions withdrawable is bounded only by
					// available_now and eligible funding (1000 here).
					assert.LessOrEqual(t, bp.Withdrawable.Cmp(bp.AvailableNow), 0, "%s: withdrawable <= available_now", p)
					assert.Equal(t, bp.AvailableNow.IsZero(), bp.Withdrawable.IsZero(), "%s: withdrawable follows available_now", p)
				}
				wantBlocked := map[buyingpower.Purpose]bool{
					buyingpower.PurposeDisplay: tc.want.blockDisplay, buyingpower.PurposeTrade: tc.want.blockTrade, buyingpower.PurposeWithdrawal: tc.want.blockWithdr,
				}[p]
				assert.Equal(t, wantBlocked, bp.Blocked(), "%s blocked", p)
			}
		})
	}
}

func TestEvaluate_RestrictionDetailsAndScopes(t *testing.T) {
	f := newFixture(t)
	snap := baseSnapshot(t)
	snap.AccountStatus = accounts.StatusFrozen
	snap.KillSwitches = []buyingpower.KillSwitchState{{Kind: "ACCOUNT_FREEZE", ScopeID: acctID.String(), Reason: "fraud review", BlocksNewRisk: true}}
	f.prices.Set(bonkID, usdcID, price(t, "0.00002345", now.Add(-time.Hour)))
	bp := eval(t, f, snap, buyingpower.PurposeTrade)
	require.Len(t, bp.Restrictions, 3)
	assert.Equal(t, buyingpower.ScopeAccount, bp.Restrictions[0].Scope)
	assert.Equal(t, "account is FROZEN", bp.Restrictions[0].Detail)
	assert.True(t, bp.Restrictions[0].Blocking)
	assert.Equal(t, buyingpower.RestrictionKillSwitch, bp.Restrictions[1].Code)
	assert.Equal(t, "ACCOUNT_FREEZE scope="+acctID.String()+": fraud review", bp.Restrictions[1].Detail)
	assert.Equal(t, buyingpower.RestrictionStalePrice, bp.Restrictions[2].Code)
	assert.Equal(t, bonkID, bp.Restrictions[2].AssetID)
	assert.Equal(t, buyingpower.ScopeAsset, bp.Restrictions[2].Scope)
	assert.False(t, bp.Restrictions[2].Blocking, "asset-scoped does not block TRADE")
	assert.Equal(t, "no price within 30s", bp.Restrictions[2].Detail)

	// The unvalued asset still appears in the underlying balances and haircuts.
	var found bool
	for _, u := range bp.UnderlyingBalances {
		if u.AssetID == bonkID {
			found = true
			assert.Equal(t, buyingpower.StatusStalePrice, u.Status)
			assert.True(t, u.USDValue.IsZero())
			assert.Equal(t, "", u.PriceRef)
			assert.Equal(t, "100000000000", u.Quantity.String())
		}
	}
	assert.True(t, found)
	var hc *buyingpower.Haircut
	for i := range bp.Haircuts {
		if bp.Haircuts[i].AssetID == bonkID {
			hc = &bp.Haircuts[i]
		}
	}
	require.NotNil(t, hc)
	assert.Equal(t, money.BPS(0), hc.FactorBPS)
	assert.Equal(t, buyingpower.StatusStalePrice, hc.Reason)
}

func TestEvaluate_StablecoinDegradedMarksToMarket(t *testing.T) {
	f := newFixture(t)
	snap := baseSnapshot(t)
	snap.Assets[usdtID] = usdt()
	snap.Balances[usdtID] = q(t, "500000000") // 500 USDT at 0.97 with 90% factor
	bp := eval(t, f, snap, buyingpower.PurposeTrade)
	assert.Equal(t, "1733.64", bp.PortfolioValue.String()) // 1248.64 + 485.00
	assert.Equal(t, "1461.25", bp.BuyingPower.String())    // 1024.75 + 436.50
	assert.Equal(t, "850.00", bp.AvailableNow.String(), "USDT is not a settlement asset")
	var u buyingpower.UnderlyingBalance
	for _, x := range bp.UnderlyingBalances {
		if x.AssetID == usdtID {
			u = x
		}
	}
	assert.Equal(t, "485.00", u.USDValue.String())
	assert.Equal(t, "DEGRADED", u.Status)
	var h buyingpower.Haircut
	for _, x := range bp.Haircuts {
		if x.AssetID == usdtID {
			h = x
		}
	}
	assert.Equal(t, money.BPS(9000), h.FactorBPS)
	assert.Equal(t, valuation.ReasonStablecoinDegraded, h.Reason)
}

func TestEvaluate_ReservedAndHoldsNeverGoNegative(t *testing.T) {
	f := newFixture(t)
	snap := baseSnapshot(t)
	snap.Reserved[usdcID] = q(t, "900000000")
	snap.Holds[usdcID] = q(t, "900000000") // over-subscribed on paper
	bp := eval(t, f, snap, buyingpower.PurposeTrade)
	assert.Equal(t, "174.75", bp.BuyingPower.String(), "USDC net floored at 0; SOL + BONK remain")
	assert.Equal(t, "0.00", bp.AvailableNow.String())
	assert.Equal(t, "900.00", bp.Reserved.String())
	assert.Equal(t, "1248.64", bp.PortfolioValue.String(), "portfolio value is gross")
}

func TestEvaluate_PendingOnlyAssetIsValuedButNotHeld(t *testing.T) {
	f := newFixture(t)
	snap := buyingpower.Snapshot{
		AccountID: acctID, AccountStatus: accounts.StatusActive, QuoteAsset: usdc(),
		Assets:   map[assets.AssetID]assets.Asset{usdcID: usdc()},
		Balances: map[assets.AssetID]money.Quantity{},
		Pending:  map[assets.AssetID]money.Quantity{usdcID: q(t, "250000000")},
	}
	bp := eval(t, f, snap, buyingpower.PurposeDisplay)
	assert.Equal(t, "250.00", bp.Pending.String())
	assert.True(t, bp.BuyingPower.IsZero())
	assert.True(t, bp.PortfolioValue.IsZero())
	assert.Empty(t, bp.UnderlyingBalances, "a pending deposit is not a balance")
	assert.Empty(t, bp.Restrictions)
}

func TestEvaluate_PolicyVersionIsDeterministicAndSensitive(t *testing.T) {
	f := newFixture(t)
	a := eval(t, f, baseSnapshot(t), buyingpower.PurposeTrade)
	b := eval(t, f, baseSnapshot(t), buyingpower.PurposeTrade)
	assert.Equal(t, a.PolicyVersion, b.PolicyVersion)

	f.policies.Set(pol(solID, assets.StatusActive, 7500, "", "sol-2"))
	c := eval(t, f, baseSnapshot(t), buyingpower.PurposeTrade)
	assert.NotEqual(t, a.PolicyVersion, c.PolicyVersion, "a new policy version changes the hash even with identical figures")
	assert.Equal(t, a.BuyingPower, c.BuyingPower)

	// Assets that are merely referenced (pending) are part of the hash too.
	snap := baseSnapshot(t)
	snap.Assets[usdtID] = usdt()
	snap.Pending[usdtID] = q(t, "1")
	d := eval(t, f, snap, buyingpower.PurposeTrade)
	assert.NotEqual(t, c.PolicyVersion, d.PolicyVersion)
}

func TestEvaluate_Errors(t *testing.T) {
	f := newFixture(t)
	_, err := f.engine.Evaluate(t.Context(), nil, baseSnapshot(t), "GAMBLE")
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	snap := baseSnapshot(t)
	snap.QuoteAsset = sol()
	_, err = f.engine.Evaluate(t.Context(), nil, snap, buyingpower.PurposeTrade)
	require.Error(t, err)
	assert.Equal(t, errs.CodeInternal, errs.CodeOf(err), "non-USD quote asset is a wiring error")

	snap = baseSnapshot(t)
	delete(snap.Assets, solID)
	_, err = f.engine.Evaluate(t.Context(), nil, snap, buyingpower.PurposeTrade)
	require.Error(t, err)
	assert.Equal(t, errs.CodeInternal, errs.CodeOf(err))

	f.prices.Err = errs.New(errs.CodeProviderUnavailable, "feed down")
	_, err = f.engine.Evaluate(t.Context(), nil, baseSnapshot(t), buyingpower.PurposeTrade)
	require.Error(t, err)
	assert.True(t, errs.HasCode(err, errs.CodeProviderUnavailable), "non-stale price errors propagate: %v", err)

	f.prices.Err = nil
	f.policies.Err = errs.New(errs.CodeInternal, "boom")
	_, err = f.engine.Evaluate(t.Context(), nil, baseSnapshot(t), buyingpower.PurposeTrade)
	require.Error(t, err)

	_, err = f.engine.Compute(t.Context(), nil, "not-a-uuid", buyingpower.PurposeTrade)
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

func TestNewEngine_RequiresDependencies(t *testing.T) {
	_, err := buyingpower.NewEngine(buyingpower.Deps{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Clock")
	assert.Contains(t, err.Error(), "QuoteAssetID")
}

// TestProp_BuyingPowerBounds: for random snapshots under random policies,
// buying_power <= portfolio_value, available_now <= buying_power,
// withdrawable <= available_now, nothing is negative, and haircuts never
// increase buying power (a snapshot with every factor at 100% and no
// haircut reasons yields at least as much buying power).
func TestProp_BuyingPowerBounds(t *testing.T) {
	statuses := []assets.Status{assets.StatusActive, assets.StatusCloseOnly, assets.StatusRestricted, assets.StatusHalted, assets.StatusDelisting, assets.StatusDelisted}
	stables := []valuation.StablecoinStatus{valuation.StablecoinNormal, valuation.StablecoinDegraded, valuation.StablecoinRestricted, valuation.StablecoinHalted, ""}
	rapid.Check(t, func(rt *rapid.T) {
		f := newFixture(t)
		snap := baseSnapshot(t)
		snap.Assets[usdtID] = usdt()
		genQty := func(label string) money.Quantity {
			return money.QuantityFromInt64(rapid.Int64Range(0, 5_000_000_000_000).Draw(rt, label))
		}
		for _, aid := range []assets.AssetID{usdcID, solID, bonkID, usdtID} {
			snap.Balances[aid] = genQty("bal")
			snap.Reserved[aid] = genQty("res")
			snap.Holds[aid] = genQty("hold")
			snap.Pending[aid] = genQty("pend")
			snap.EligibleFunding[aid] = genQty("fund")
			snap.Withdrawals[aid] = genQty("wd")
			status := rapid.SampledFrom(statuses).Draw(rt, "status")
			factor := money.BPS(rapid.Int64Range(0, 10000).Draw(rt, "factor"))
			stable := valuation.StablecoinStatus("")
			if snap.Assets[aid].IsStablecoin {
				stable = rapid.SampledFrom(stables).Draw(rt, "stable")
			}
			f.policies.Set(pol(aid, status, factor, stable, "v"))
			age := time.Duration(rapid.Int64Range(0, 60).Draw(rt, "age")) * time.Second
			f.prices.Set(aid, usdcID, price(t, "1.234567", now.Add(-age)))
		}
		if rapid.Bool().Draw(rt, "frozen") {
			snap.AccountStatus = accounts.StatusFrozen
		}
		bp, err := f.engine.Evaluate(t.Context(), nil, snap, buyingpower.PurposeTrade)
		require.NoError(rt, err)
		require.False(rt, bp.PortfolioValue.IsNegative() || bp.BuyingPower.IsNegative() || bp.AvailableNow.IsNegative() ||
			bp.Reserved.IsNegative() || bp.Pending.IsNegative() || bp.Withdrawable.IsNegative(), "negative figure: %+v", bp)
		require.LessOrEqual(rt, bp.BuyingPower.Cmp(bp.PortfolioValue), 0, "buying_power %s > portfolio_value %s", bp.BuyingPower, bp.PortfolioValue)
		require.LessOrEqual(rt, bp.AvailableNow.Cmp(bp.BuyingPower), 0)
		require.LessOrEqual(rt, bp.Withdrawable.Cmp(bp.AvailableNow), 0)
		if len(bp.Restrictions) > 0 {
			require.True(rt, bp.Withdrawable.IsZero())
		}
		for _, h := range bp.Haircuts {
			require.True(rt, h.FactorBPS >= 0 && h.FactorBPS <= money.OneHundredPercent)
		}

		// Haircuts never increase buying power: relax every factor to 100%.
		for _, aid := range []assets.AssetID{usdcID, solID, bonkID, usdtID} {
			p, _ := f.policies.Current(t.Context(), nil, aid, now)
			p.CollateralFactor = 10000
			f.policies.Set(p)
		}
		relaxed, err := f.engine.Evaluate(t.Context(), nil, snap, buyingpower.PurposeTrade)
		require.NoError(rt, err)
		require.LessOrEqual(rt, bp.BuyingPower.Cmp(relaxed.BuyingPower), 0, "haircut increased buying power: %s > %s", bp.BuyingPower, relaxed.BuyingPower)
	})
}
