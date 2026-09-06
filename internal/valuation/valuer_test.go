package valuation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

var (
	observedAt = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	usdcID     = assets.NewAssetID()
	solID      = assets.NewAssetID()
	bonkID     = assets.NewAssetID()
	usdtID     = assets.NewAssetID()
)

func usdcAsset() assets.Asset {
	return assets.Asset{ID: usdcID, Symbol: "USDC", Decimals: 6, IsStablecoin: true, PegCurrency: "USD", RiskClass: assets.RiskSettlement, Status: assets.StatusActive}
}

func solAsset() assets.Asset {
	return assets.Asset{ID: solID, Symbol: "SOL", Decimals: 9, RiskClass: assets.RiskMajor, Status: assets.StatusActive}
}

func bonkAsset() assets.Asset {
	return assets.Asset{ID: bonkID, Symbol: "BONK", Decimals: 5, RiskClass: assets.RiskSpeculative, Status: assets.StatusActive}
}

func mustPrice(t *testing.T, decimal string) money.Price {
	t.Helper()
	p, err := money.PriceFromDecimalString(decimal, usdcID.String(), "pyth", observedAt)
	require.NoError(t, err)
	return p
}

func mustQ(t *testing.T, s string) money.Quantity {
	t.Helper()
	q, err := money.ParseQuantity(s)
	require.NoError(t, err)
	return q
}

func mustUSD(t *testing.T, s string) money.USD {
	t.Helper()
	u, err := money.ParseUSD(s)
	require.NoError(t, err)
	return u
}

func policy(status assets.Status, factor money.BPS, stable StablecoinStatus) AssetPolicy {
	return AssetPolicy{Status: status, CollateralFactor: factor, StablecoinStatus: stable, MaxPriceAge: 30 * time.Second, PolicyVersion: "p1"}
}

func TestValueQuantity_SolanaNumbers(t *testing.T) {
	v := NewValuer()
	cases := []struct {
		name          string
		qty           string
		assetDecimals uint8
		price         string
		want          string
	}{
		// 1.5 SOL at 150.123456 USDC: 225.185184 USDC → 225.19 (half-even on .184 → down).
		{"1.5 SOL", "1500000000", 9, "150.123456", "225.19"},
		// 1 lamport at 150.123456: 0.000000150123456 USDC → 0 base units → 0.00.
		{"1 lamport", "1", 9, "150.123456", "0.00"},
		// 0.333333333 SOL at 150.123456 → 50.041151.. USDC → 50.04.
		{"third of a SOL", "333333333", 9, "150.123456", "50.04"},
		// 1,000,000 BONK (5 decimals) at 0.00002345 USDC → 23.45 USDC.
		{"1M BONK", "100000000000", 5, "0.00002345", "23.45"},
		// 12,345,678.9 BONK at 0.00002345 → 289.5061... → 289.51.
		{"odd BONK", "1234567890000", 5, "0.00002345", "289.51"},
		// A half-cent tie rounds to even: 0.005 USDC exactly at price 1 for 5000 micro → 5000 base units → 0.005 → 0.00 (even).
		{"tie to even down", "5000", 6, "1", "0.00"},
		{"tie to even up", "15000", 6, "1", "0.02"},
		{"zero", "0", 9, "150.123456", "0.00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := v.ValueQuantity(mustQ(t, tc.qty), tc.assetDecimals, mustPrice(t, tc.price), 6)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.String())
		})
	}
}

func TestValueQuantity_Errors(t *testing.T) {
	v := NewValuer()
	// Invalid price (no source) → VALIDATION_FAILED.
	bad := money.Price{Mantissa: money.QuantityFromInt64(1), Scale: 0, QuoteAsset: "x", At: observedAt}
	_, err := v.ValueQuantity(money.QuantityFromInt64(1), 9, bad, 6)
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	// Overflow: an absurd quantity at an absurd price leaves int64 cents.
	huge := mustQ(t, "1000000000000000000000000000000000000")
	_, err = v.ValueQuantity(huge, 0, mustPrice(t, "1000000000"), 6)
	require.Error(t, err)
	assert.Equal(t, errs.CodeOverflow, errs.CodeOf(err))
}

func TestFaceValue(t *testing.T) {
	v := NewValuer()
	got, err := v.FaceValue(mustQ(t, "1234567890"), 6) // 1234.567890 USDC
	require.NoError(t, err)
	assert.Equal(t, "1234.57", got.String())
	got, err = v.FaceValue(mustQ(t, "5000"), 6) // 0.005 → tie → 0.00 (even)
	require.NoError(t, err)
	assert.Equal(t, "0.00", got.String())
}

func TestApplyCollateral_RoundsTowardLessBuyingPower(t *testing.T) {
	v := NewValuer()
	cases := []struct {
		value  string
		factor money.BPS
		want   string
	}{
		{"225.19", 9500, "213.93"}, // 213.9305 → down
		{"225.19", 10000, "225.19"},
		{"225.19", 0, "0.00"},
		{"0.01", 9999, "0.00"},      // 0.009999 → down, never up
		{"100.00", 12000, "100.00"}, // clamped to 100%
		{"100.00", -5, "0.00"},      // clamped to 0
	}
	for _, tc := range cases {
		got, err := v.ApplyCollateral(mustUSD(t, tc.value), tc.factor)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got.String(), "%s × %d", tc.value, tc.factor)
	}
	_, err := v.ApplyCollateral(mustUSD(t, "-1.00"), 5000)
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

func TestClassify_AssetStatus(t *testing.T) {
	cases := []struct {
		status assets.Status
		gate   Gate
		reason string
	}{
		{assets.StatusActive, GateEligible, ReasonCollateralFactor},
		{assets.StatusCloseOnly, GateEligible, ReasonCollateralFactor},
		{assets.StatusDelisting, GateEligible, ReasonCollateralFactor},
		{assets.StatusRestricted, GatePortfolioOnly, ReasonAssetRestricted},
		{assets.StatusDelisted, GatePortfolioOnly, ReasonAssetDelisted},
		{assets.StatusHalted, GateExcluded, ReasonAssetHalted},
		{assets.Status("BOGUS"), GateExcluded, ReasonAssetStatusUnknown},
	}
	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			c := Classify(solAsset(), policy(tc.status, 8000, ""))
			assert.Equal(t, tc.gate, c.Gate)
			assert.Equal(t, BasisMarket, c.Basis)
			assert.Equal(t, tc.reason, c.Reason)
			if tc.gate == GateEligible {
				assert.Equal(t, money.BPS(8000), c.Factor)
			} else {
				assert.Equal(t, money.BPS(0), c.Factor, "closed gates carry no factor")
			}
		})
	}
	// Full collateral and eligible: no reason at all.
	c := Classify(solAsset(), policy(assets.StatusActive, 10000, ""))
	assert.Equal(t, "", c.Reason)
	assert.Equal(t, money.BPS(10000), c.Factor)

	// Missing policy: portfolio only, market basis, zero factor.
	c = Classify(solAsset(), FailClosedPolicy(solID))
	assert.Equal(t, Classification{Gate: GatePortfolioOnly, Basis: BasisMarket, Reason: ReasonPolicyMissing}, c)
}

// TestClassify_StablecoinMatrix is one case per PART 26 state, plus the
// fail-closed combinations.
func TestClassify_StablecoinMatrix(t *testing.T) {
	cases := []struct {
		name   string
		asset  assets.Asset
		policy AssetPolicy
		want   Classification
	}{
		{
			"NORMAL usd peg → face", usdcAsset(), policy(assets.StatusActive, 9500, StablecoinNormal),
			Classification{Gate: GateEligible, Basis: BasisFace, Factor: 9500, Reason: ReasonCollateralFactor},
		},
		{
			"NORMAL full collateral → face, no reason", usdcAsset(), policy(assets.StatusActive, 10000, StablecoinNormal),
			Classification{Gate: GateEligible, Basis: BasisFace, Factor: 10000},
		},
		{
			"DEGRADED → market with haircut", usdcAsset(), policy(assets.StatusActive, 9000, StablecoinDegraded),
			Classification{Gate: GateEligible, Basis: BasisMarket, Factor: 9000, Reason: ReasonStablecoinDegraded},
		},
		{
			"RESTRICTED → portfolio only at market", usdcAsset(), policy(assets.StatusActive, 9500, StablecoinRestricted),
			Classification{Gate: GatePortfolioOnly, Basis: BasisMarket, Reason: ReasonStablecoinRestricted},
		},
		{
			"HALTED → excluded", usdcAsset(), policy(assets.StatusActive, 9500, StablecoinHalted),
			Classification{Gate: GateExcluded, Basis: BasisMarket, Reason: ReasonStablecoinHalted},
		},
		{
			"stablecoin without status → fail closed", usdcAsset(), policy(assets.StatusActive, 9500, ""),
			Classification{Gate: GatePortfolioOnly, Basis: BasisMarket, Reason: ReasonStablecoinStatusMissing},
		},
		{
			"NORMAL but non-USD peg → market",
			assets.Asset{ID: usdtID, Decimals: 6, IsStablecoin: true, PegCurrency: "EUR", Status: assets.StatusActive},
			policy(assets.StatusActive, 9500, StablecoinNormal),
			Classification{Gate: GateEligible, Basis: BasisMarket, Factor: 9500, Reason: ReasonCollateralFactor},
		},
		{
			"NORMAL peg but asset RESTRICTED → face, portfolio only", usdcAsset(), policy(assets.StatusRestricted, 9500, StablecoinNormal),
			Classification{Gate: GatePortfolioOnly, Basis: BasisFace, Reason: ReasonAssetRestricted},
		},
		{
			"asset HALTED beats stablecoin NORMAL", usdcAsset(), policy(assets.StatusHalted, 9500, StablecoinNormal),
			Classification{Gate: GateExcluded, Basis: BasisFace, Reason: ReasonAssetHalted},
		},
		{
			"stablecoin HALTED beats asset RESTRICTED", usdcAsset(), policy(assets.StatusRestricted, 9500, StablecoinHalted),
			Classification{Gate: GateExcluded, Basis: BasisMarket, Reason: ReasonStablecoinHalted},
		},
		{
			"non-stablecoin with a stablecoin status honors it", solAsset(), policy(assets.StatusActive, 9500, StablecoinRestricted),
			Classification{Gate: GatePortfolioOnly, Basis: BasisMarket, Reason: ReasonStablecoinRestricted},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Classify(tc.asset, tc.policy))
		})
	}
}

func TestMark_StablecoinContributionRule(t *testing.T) {
	v := NewValuer()
	qty := mustQ(t, "1000000000")     // 1000 USDC
	depeg := mustPrice(t, "0.970000") // USDC trading at 0.97 quote units

	t.Run("NORMAL: face × factor, no price needed", func(t *testing.T) {
		m, err := v.Mark(usdcAsset(), policy(assets.StatusActive, 9500, StablecoinNormal), qty, nil, 6)
		require.NoError(t, err)
		assert.Equal(t, "1000.00", m.Value.String())
		assert.Equal(t, "950.00", m.Contribution.String())
		assert.Equal(t, "face:USD", m.PriceRef)
		assert.Equal(t, BasisFace, m.Basis)
	})
	t.Run("DEGRADED: market × factor", func(t *testing.T) {
		m, err := v.Mark(usdcAsset(), policy(assets.StatusActive, 9000, StablecoinDegraded), qty, &depeg, 6)
		require.NoError(t, err)
		assert.Equal(t, "970.00", m.Value.String())
		assert.Equal(t, "873.00", m.Contribution.String())
		assert.Equal(t, "pyth@2026-09-05T12:00:00Z", m.PriceRef)
		assert.Equal(t, ReasonStablecoinDegraded, m.Reason)
	})
	t.Run("DEGRADED without price is an error, never face", func(t *testing.T) {
		_, err := v.Mark(usdcAsset(), policy(assets.StatusActive, 9000, StablecoinDegraded), qty, nil, 6)
		require.Error(t, err)
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	})
	t.Run("RESTRICTED: portfolio value only", func(t *testing.T) {
		m, err := v.Mark(usdcAsset(), policy(assets.StatusActive, 9500, StablecoinRestricted), qty, &depeg, 6)
		require.NoError(t, err)
		assert.Equal(t, "970.00", m.Value.String())
		assert.True(t, m.Contribution.IsZero())
		assert.Equal(t, money.BPS(0), m.Factor)
		assert.Equal(t, GatePortfolioOnly, m.Gate)
	})
	t.Run("HALTED: excluded entirely", func(t *testing.T) {
		m, err := v.Mark(usdcAsset(), policy(assets.StatusActive, 9500, StablecoinHalted), qty, &depeg, 6)
		require.NoError(t, err)
		assert.True(t, m.Value.IsZero())
		assert.True(t, m.Contribution.IsZero())
		assert.Equal(t, GateExcluded, m.Gate)
		assert.Equal(t, "", m.PriceRef)
	})
	t.Run("negative quantity rejected", func(t *testing.T) {
		_, err := v.Mark(usdcAsset(), policy(assets.StatusActive, 9500, StablecoinNormal), mustQ(t, "-1"), nil, 6)
		require.Error(t, err)
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	})
}

func TestMark_VolatileAssetWithCollateralFactor(t *testing.T) {
	v := NewValuer()
	sol := mustPrice(t, "150.123456")
	m, err := v.Mark(solAsset(), policy(assets.StatusActive, 7500, ""), mustQ(t, "1500000000"), &sol, 6)
	require.NoError(t, err)
	assert.Equal(t, "225.19", m.Value.String())
	assert.Equal(t, "168.89", m.Contribution.String()) // 168.8925 → down
	assert.Equal(t, money.BPS(7500), m.Factor)
	assert.Equal(t, ReasonCollateralFactor, m.Reason)

	bonk := mustPrice(t, "0.00002345")
	m, err = v.Mark(bonkAsset(), policy(assets.StatusCloseOnly, 2500, ""), mustQ(t, "100000000000"), &bonk, 6)
	require.NoError(t, err)
	assert.Equal(t, "23.45", m.Value.String())
	assert.Equal(t, "5.86", m.Contribution.String()) // 5.8625 → down
}

func TestPriceStore_Latest_NonPositiveMaxAgeIsStale(t *testing.T) {
	s := NewPriceStore(nil)
	_, err := s.Latest(t.Context(), nil, solID, usdcID, 0, observedAt)
	require.Error(t, err)
	assert.Equal(t, errs.CodeStaleMarketData, errs.CodeOf(err))
	_, err = s.Latest(t.Context(), nil, solID, usdcID, -time.Second, observedAt)
	assert.Equal(t, errs.CodeStaleMarketData, errs.CodeOf(err))
}

func TestPriceObservation_Validate(t *testing.T) {
	good := PriceObservation{AssetID: solID, QuoteAssetID: usdcID, Mantissa: money.QuantityFromInt64(150123456), Scale: 6, Source: "pyth", ObservedAt: observedAt}
	require.NoError(t, good.Validate())
	bad := good
	bad.Scale = 39
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(bad.Validate()))
	bad = good
	bad.Source = " "
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(bad.Validate()))
	bad = good
	bad.Mantissa = money.QuantityFromInt64(-1)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(bad.Validate()))
}
