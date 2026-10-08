package margin

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/money"
)

func baseFactors() HaircutFactors {
	return HaircutFactors{Volatility: VolMedium, Liquidity: LiqThin, Venue: VenueTier2}
}

func TestHaircut_ComponentsAddAndCap(t *testing.T) {
	p := DefaultPolicy()
	b, err := ComputeHaircut(p, baseFactors())
	require.NoError(t, err)
	assert.Equal(t, money.BPS(2_000), b.VolatilityBPS)
	assert.Equal(t, money.BPS(500), b.LiquidityBPS)
	assert.Equal(t, money.BPS(250), b.VenueBPS)
	assert.Equal(t, money.BPS(2_750), b.TotalBPS)
	assert.False(t, b.ZeroValue)

	f := baseFactors()
	f.OracleAgeMS = 20_000 // in the aging band
	f.ConcentrationShareBPS = 7_500
	f.SettlementDelayMS = 2*msPerHour + 1 // three started hours
	f.InFlight, f.InFlightMS = true, msPerHour
	b, err = ComputeHaircut(p, f)
	require.NoError(t, err)
	assert.Equal(t, money.BPS(100), b.OracleBPS)
	assert.Equal(t, money.BPS(750), b.ConcentrationBPS, "1500 × (7500 − 5000) / 5000")
	assert.Equal(t, money.BPS(150), b.SettlementBPS)
	assert.Equal(t, money.BPS(2_600), b.CrossChainBPS)
	assert.Equal(t, money.BPS(2_750+100+750+150+2_600), b.TotalBPS)

	f.Volatility = VolExtreme
	f.Liquidity = LiqIlliquid
	b, err = ComputeHaircut(p, f)
	require.NoError(t, err)
	assert.Equal(t, money.BPS(10_000), b.TotalBPS, "capped at 100%")
	assert.False(t, b.ZeroValue, "capped, not zeroed by a single factor")
}

func TestHaircut_StaleMissingFutureOrStuckIsWorthless(t *testing.T) {
	p := DefaultPolicy()
	for name, mutate := range map[string]func(*HaircutFactors){
		"stale":        func(f *HaircutFactors) { f.OracleAgeMS = p.OracleMaxAgeMS + 1 },
		"missing":      func(f *HaircutFactors) { f.MarkMissing = true },
		"future":       func(f *HaircutFactors) { f.MarkFromFuture = true },
		"bridge stuck": func(f *HaircutFactors) { f.InFlight, f.BridgeDelayed = true, true },
	} {
		f := HaircutFactors{Volatility: VolStable, Liquidity: LiqDeep, Venue: VenueTier1}
		mutate(&f)
		b, err := ComputeHaircut(p, f)
		require.NoError(t, err, name)
		assert.True(t, b.ZeroValue, name)
		assert.Equal(t, money.BPS(10_000), b.TotalBPS, name)
	}
	f := HaircutFactors{Volatility: VolStable, Liquidity: LiqDeep, Venue: VenueTier1, BridgeDelayed: true}
	b, err := ComputeHaircut(p, f)
	require.NoError(t, err)
	assert.False(t, b.ZeroValue, "a stuck bridge only matters for collateral in flight")
}

func TestHaircut_RefusesUnknownOrNegativeFactors(t *testing.T) {
	p := DefaultPolicy()
	_, err := ComputeHaircut(p, HaircutFactors{Volatility: "WILD", Liquidity: LiqDeep, Venue: VenueTier1})
	require.Error(t, err)
	f := baseFactors()
	f.SettlementDelayMS = -1
	_, err = ComputeHaircut(p, f)
	require.Error(t, err)
}
