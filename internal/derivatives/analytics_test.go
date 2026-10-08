package derivatives

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/quant"
)

func dec(t tb, s string) quant.Dec {
	t.Helper()
	d, err := quant.ParseDec(s)
	require.NoError(t, err)
	return d
}

// TestProp_AnalyticsGreeksAreSane checks the Greeks as Analytics delivers
// them: a call's delta in [0, 1], a put's in [−1, 0], gamma and vega never
// negative, every value labelled as a reference value.
func TestProp_AnalyticsGreeksAreSane(t *testing.T) {
	expiry := time.Date(2026, 12, 25, 8, 0, 0, 0, time.UTC)
	rapid.Check(t, func(t *rapid.T) {
		right := instruments.Call
		if rapid.Bool().Draw(t, "put") {
			right = instruments.Put
		}
		k := rapid.Int64Range(20_000, 200_000).Draw(t, "strike_dollars") * 100
		s := optionSpec(t, right, k)
		in := AnalyticsInput{
			Spot:      q(rapid.Int64Range(20_000, 200_000).Draw(t, "spot_dollars") * 100),
			SpotScale: 2,
			VolBPS:    money.BPS(rapid.Int64Range(500, 20_000).Draw(t, "vol_bps")),
			RateBPS:   money.BPS(rapid.Int64Range(0, 1_000).Draw(t, "rate_bps")),
			Now:       expiry.Add(-time.Duration(rapid.Int64Range(1, 700).Draw(t, "days")) * 24 * time.Hour),
		}
		a, err := ComputeAnalytics(s, in)
		if err != nil {
			t.Fatal(err)
		}
		if a.Label != AnalyticsLabel || a.Model != AnalyticsModel {
			t.Fatal("analytics are not labelled as reference values")
		}
		delta, gamma, vega := dec(t, a.Delta), dec(t, a.Gamma), dec(t, a.Vega)
		one, zero := quant.DecFromInt64(1), quant.Dec{}
		if right == instruments.Call {
			if delta.Cmp(zero) < 0 || delta.Cmp(one) > 0 {
				t.Fatalf("call delta %s outside [0, 1]", a.Delta)
			}
		} else if delta.Cmp(one.Neg()) < 0 || delta.Cmp(zero) > 0 {
			t.Fatalf("put delta %s outside [−1, 0]", a.Delta)
		}
		if gamma.Sign() < 0 || vega.Sign() < 0 {
			t.Fatalf("gamma %s or vega %s negative", a.Gamma, a.Vega)
		}
		if dec(t, a.Price).Sign() < 0 {
			t.Fatalf("negative model price %s", a.Price)
		}
	})
}

func TestAnalytics_ImpliedVolRoundTripsAndTimeValueIsExact(t *testing.T) {
	s := optionSpec(t, instruments.Call, 6_500_000)
	now := time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC) // 90 days before expiry
	in := AnalyticsInput{Spot: dollars(65_000), SpotScale: 2, VolBPS: 5_000, Now: now}
	a, err := ComputeAnalytics(s, in)
	require.NoError(t, err)
	assert.Equal(t, "0.00", a.Intrinsic, "at the money")
	// Quote the model price back as a premium on the tick grid.
	price := dec(t, a.Price)
	premium, err := price.ToQuantity(2, money.RoundHalfEven)
	require.NoError(t, err)
	premium, err = s.AlignToTick(premium, money.RoundHalfEven)
	require.NoError(t, err)
	in.Premium = &premium
	b, err := ComputeAnalytics(s, in)
	require.NoError(t, err)
	require.Empty(t, b.ImpliedVolReason)
	iv := dec(t, b.ImpliedVol)
	diff := iv.Sub(quant.DecFromBPS(5_000)).Abs()
	assert.True(t, diff.Cmp(quant.DecFromBPS(10)) <= 0, "implied vol %s should be 0.50 within the premium's tick rounding", b.ImpliedVol)
	assert.Equal(t, premium.ToDecimalString(2), b.TimeValue, "at the money the whole premium is time value")

	// A premium below intrinsic has no implied volatility; it is reported,
	// never invented.
	itm := optionSpec(t, instruments.Call, 6_000_000)
	low := dollars(100)
	c, err := ComputeAnalytics(itm, AnalyticsInput{Spot: dollars(65_000), SpotScale: 2, VolBPS: 5_000, Premium: &low, Now: now})
	require.NoError(t, err)
	assert.Equal(t, quant.ReasonPriceBelowIntrinsic, c.ImpliedVolReason)
	assert.Empty(t, c.ImpliedVol)
	assert.Equal(t, "5000.00", c.Intrinsic)
	assert.Equal(t, "-4900.00", c.TimeValue)

	_, err = ComputeAnalytics(s, AnalyticsInput{Spot: dollars(65_000), SpotScale: 2, VolBPS: 5_000, Now: *s.Expiry})
	require.Error(t, err, "an expired option has no model value")
	_, err = ComputeAnalytics(perpSpec(t), in)
	require.Error(t, err)
}
