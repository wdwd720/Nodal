package quant

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// discounted returns S·e^{−qT} and K·e^{−rT} as the package computes them.
func discounted(t testing.TB, in BSMInput) (sq, kr Dec) {
	t.Helper()
	dfq, err := Exp(in.DividendYield.Mul(in.TimeYears).Neg())
	require.NoError(t, err)
	dfr, err := Exp(in.Rate.Mul(in.TimeYears).Neg())
	require.NoError(t, err)
	return in.Spot.Mul(dfq), in.Strike.Mul(dfr)
}

func TestLimitRegimeTinyTime(t *testing.T) {
	// σ√T = 0.2·√(1e-35) ≈ 6.3e-19 < 1e-16: expiry limit, intrinsic value.
	call := goldenInput(t, Call, "105", "100", "0.00000000000000000000000000000000001", "0.05", "0.01", "0.2")
	put := call
	put.Kind = Put
	cr, err := BlackScholes(call)
	require.NoError(t, err)
	pr, err := BlackScholes(put)
	require.NoError(t, err)
	require.Equal(t, RegimeIntrinsicLimit, cr.Regime)
	require.Equal(t, RegimeIntrinsicLimit, pr.Regime)

	sq, kr := discounted(t, call)
	requireDecEqual(t, sq.Sub(kr), cr.Price, "call = S·e^{-qT} − K·e^{-rT}")
	require.True(t, cr.Price.Sub(DecFromInt64(5)).Abs().Cmp(d(t, "0.0000000000000000000000000000001")) < 0, "≈ intrinsic 5: %s", cr.Price)
	requireDecEqual(t, Dec{}, pr.Price)
	// Derivatives of the intrinsic value.
	dfq, _ := Exp(call.DividendYield.Mul(call.TimeYears).Neg())
	requireDecEqual(t, dfq, cr.Delta)
	requireDecEqual(t, Dec{}, pr.Delta)
	for _, r := range []BSMResult{cr, pr} {
		requireDecEqual(t, Dec{}, r.Gamma)
		requireDecEqual(t, Dec{}, r.Vega)
	}
	requireDecEqual(t, call.DividendYield.Mul(sq).Sub(call.Rate.Mul(kr)), cr.Theta, "theta = q·S·e^{-qT} − r·K·e^{-rT}")
	requireDecEqual(t, call.TimeYears.Mul(kr), cr.Rho)
	requireDecEqual(t, Dec{}, pr.Theta)
	requireDecEqual(t, Dec{}, pr.Rho)
}

func TestLimitRegimeTinyVol(t *testing.T) {
	// σ = 1e-20 over a year: the discounted forward intrinsic value.
	call := goldenInput(t, Call, "100", "100", "1", "0.05", "0", "0.00000000000000000001")
	cr, err := BlackScholes(call)
	require.NoError(t, err)
	require.Equal(t, RegimeIntrinsicLimit, cr.Regime)
	dfr, err := Exp(d(t, "-0.05"))
	require.NoError(t, err)
	requireDecEqual(t, DecFromInt64(100).Sub(DecFromInt64(100).Mul(dfr)), cr.Price)
	requireWithin(t, "100(1 − e^-0.05)", cr.Price, defaultOracle().f().Mul(defaultOracle().int(100),
		defaultOracle().f().Sub(defaultOracle().int(1), defaultOracle().exp(defaultOracle().dec(d(t, "-0.05"))))), ulps(100))

	put := call
	put.Kind = Put
	pr, err := BlackScholes(put)
	require.NoError(t, err)
	requireDecEqual(t, Dec{}, pr.Price)
	requireDecEqual(t, Dec{}, pr.Delta)
	requireDecEqual(t, DecFromInt64(1), cr.Delta)
}

func TestLimitRegimeAtTheForward(t *testing.T) {
	// r = q makes S·e^{-qT} = K·e^{-rT} exactly when S = K: N(d1) = N(d2) = ½.
	call := goldenInput(t, Call, "100", "100", "2", "0.03", "0.03", "0.00000000000000001")
	cr, err := BlackScholes(call)
	require.NoError(t, err)
	require.Equal(t, RegimeIntrinsicLimit, cr.Regime)
	requireDecEqual(t, Dec{}, cr.Price)
	dfq, _ := Exp(d(t, "-0.06"))
	half := d(t, "0.5")
	requireDecEqual(t, dfq.Mul(half), cr.Delta)
	put := call
	put.Kind = Put
	pr, err := BlackScholes(put)
	require.NoError(t, err)
	requireDecEqual(t, Dec{}, pr.Price)
	requireDecEqual(t, dfq.Mul(half).Neg(), pr.Delta)
	requireDecEqual(t, Dec{}, pr.Gamma)
}

// TestLimitRegimeContinuity compares both sides of the σ√T = 10^-16
// threshold at the forward, where the jump is largest: the time value
// dropped by the limit is S·e^{-qT}·σ√T·φ(0) ≈ 4·10^-15 for S = 100.
func TestLimitRegimeContinuity(t *testing.T) {
	above := goldenInput(t, Call, "100", "100", "1", "0.02", "0.02", "0.0000000000000001")
	below := above
	below.Vol = d(t, "0.0000000000000000999999")
	ra, err := BlackScholes(above)
	require.NoError(t, err)
	rb, err := BlackScholes(below)
	require.NoError(t, err)
	require.Equal(t, RegimeAnalytic, ra.Regime)
	require.Equal(t, RegimeIntrinsicLimit, rb.Regime)
	jump := ra.Price.Sub(rb.Price)
	require.True(t, jump.Sign() >= 0 && jump.Cmp(d(t, "0.000000000000005")) < 0, "jump %s", jump)
	require.True(t, ra.Delta.Sub(rb.Delta).Abs().Cmp(d(t, "0.0000000000000001")) < 0)

	// Away from the forward the two regimes agree to the last digit.
	otm := goldenInput(t, Call, "100", "120", "1", "0.02", "0.02", "0.0000000000000001")
	r1, err := BlackScholes(otm)
	require.NoError(t, err)
	otm.Vol = d(t, "0.00000000000000009")
	r2, err := BlackScholes(otm)
	require.NoError(t, err)
	requireDecEqual(t, r1.Price, r2.Price)
	requireDecEqual(t, Dec{}, r1.Price)
}

// TestDeepMoneynessStability drives the formula far into the tails and to
// the edges of the input domain: no errors, Greeks within their bounds, and
// prices landing exactly on the no-arbitrage bounds when N saturates.
func TestDeepMoneynessStability(t *testing.T) {
	cases := [][3]string{ // spot, strike, time
		{"1000000", "1", "1"},
		{"1", "1000000", "1"},
		{"1000000000000000000", "0.000000000000000001", "100"},
		{"0.000000000000000001", "1000000000000000000", "100"},
		{"0.000000000000000001", "0.000000000000000001", "0.0000000000000000000000000000000000000001"},
		{"1000000000000000000", "999999999999999999.9999", "0.5"},
	}
	for _, c := range cases {
		for _, kind := range []OptionKind{Call, Put} {
			for _, vol := range []string{"0.0001", "0.2", "10"} {
				in := goldenInput(t, kind, c[0], c[1], c[2], "0.05", "0.01", vol)
				res, err := BlackScholes(in)
				require.NoError(t, err, "%+v", in)
				sq, kr := discounted(t, in)
				lower := sq.Sub(kr).Max(Dec{})
				upper := sq
				if kind == Put {
					lower, upper = kr.Sub(sq).Max(Dec{}), kr
				}
				require.True(t, res.Price.Cmp(lower) >= 0 && res.Price.Cmp(upper) <= 0, "%s %v price %s outside [%s, %s]", kind, c, res.Price, lower, upper)
				require.True(t, res.Gamma.Sign() >= 0 && res.Vega.Sign() >= 0)
				dfq, _ := Exp(in.DividendYield.Mul(in.TimeYears).Neg())
				if kind == Call {
					require.True(t, res.Delta.Sign() >= 0 && res.Delta.Cmp(dfq) <= 0)
				} else {
					require.True(t, res.Delta.Sign() <= 0 && res.Delta.Cmp(dfq.Neg()) >= 0)
				}
			}
		}
	}
	// Saturated tails: exactly the bound, exactly the full delta.
	in := goldenInput(t, Call, "1000000", "1", "1", "0.05", "0.01", "0.2")
	res, err := BlackScholes(in)
	require.NoError(t, err)
	sq, kr := discounted(t, in)
	requireDecEqual(t, sq.Sub(kr), res.Price)
	dfq, _ := Exp(d(t, "-0.01"))
	requireDecEqual(t, dfq, res.Delta)
	requireDecEqual(t, Dec{}, res.Gamma)
	in.Kind = Put
	res, err = BlackScholes(in)
	require.NoError(t, err)
	requireDecEqual(t, Dec{}, res.Price)
	requireDecEqual(t, Dec{}, res.Delta)
}
