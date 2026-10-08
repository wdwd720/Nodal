package quant

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
)

func ivInput(in BSMInput, price Dec) ImpliedVolInput {
	return ImpliedVolInput{
		Kind: in.Kind, Spot: in.Spot, Strike: in.Strike, TimeYears: in.TimeYears,
		Rate: in.Rate, DividendYield: in.DividendYield, Price: price,
	}
}

func TestImpliedVolRecoversGoldenVols(t *testing.T) {
	maxIt := 0
	for _, g := range bsmGolden {
		for _, kind := range []OptionKind{Call, Put} {
			in := goldenInput(t, kind, g.s, g.k, g.t, g.r, g.q, g.vol)
			price, err := BlackScholesPrice(in)
			require.NoError(t, err)
			res, err := ImpliedVol(ivInput(in, price))
			require.NoError(t, err, "%s %s", g.name, kind)
			bs, err := BlackScholes(in)
			require.NoError(t, err)
			if bs.Vega.Cmp(d(t, "0.000001")) >= 0 {
				require.True(t, res.Vol.Sub(in.Vol).Abs().Cmp(d(t, "0.00000000000000000000000001")) <= 0,
					"%s %s: vol %s, want %s", g.name, kind, res.Vol, in.Vol)
			}
			require.True(t, res.Residual.Abs().Cmp(d(t, "0.0000000000000000000000000001")) <= 0, "residual %s", res.Residual)
			maxIt = max(maxIt, res.Iterations)
		}
	}
	t.Logf("most iterations over the golden cases: %d", maxIt)
	require.Less(t, maxIt, 40)
}

// TestImpliedVolTaskQuote inverts the double-precision call price quoted in
// the task statement; it differs from the exact model price by 1.8e-15, so
// the implied vol is 0.2 to within that over vega (37.5).
func TestImpliedVolTaskQuote(t *testing.T) {
	in := goldenInput(t, Call, "100", "100", "1", "0.05", "0", "0.2")
	res, err := ImpliedVol(ivInput(in, d(t, "10.450583572185565")))
	require.NoError(t, err)
	require.True(t, res.Vol.Sub(d(t, "0.2")).Abs().Cmp(d(t, "0.000000000000001")) < 0, "vol %s", res.Vol)
	in.Kind = Put
	res, err = ImpliedVol(ivInput(in, d(t, "5.573526022256971")))
	require.NoError(t, err)
	require.True(t, res.Vol.Sub(d(t, "0.2")).Abs().Cmp(d(t, "0.000000000000001")) < 0, "vol %s", res.Vol)
}

func TestImpliedVolNoSolution(t *testing.T) {
	itm := goldenInput(t, Call, "110", "100", "1", "0.05", "0", "0.2") // lower bound 110 − 100e^{-0.05} ≈ 14.877
	atm := goldenInput(t, Call, "100", "100", "1", "0", "0", "0.2")    // price(0.0001) ≈ 0.004, price(5) ≈ 98.76
	atmPut := atm
	atmPut.Kind = Put
	cases := []struct {
		name   string
		in     BSMInput
		price  string
		reason string
	}{
		{"negative price", atm, "-0.01", ReasonPriceBelowIntrinsic},
		{"below intrinsic", itm, "14.8", ReasonPriceBelowIntrinsic},
		{"above the spot", itm, "110.0000000000000000000000000000000000000001", ReasonPriceAboveUpperBound},
		{"put above the discounted strike", atmPut, "100.5", ReasonPriceAboveUpperBound},
		{"needs vol below 0.0001", atm, "0.001", ReasonPriceBelowVolMin},
		{"needs vol above 5", atm, "99.5", ReasonPriceAboveVolMax},
		{"put needs vol above 5", atmPut, "99.9", ReasonPriceAboveVolMax},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ImpliedVol(ivInput(tc.in, d(t, tc.price)))
			e := requireCode(t, err, errs.CodeValidationFailed)
			require.Equal(t, tc.reason, e.Fields["reason"])
			require.Equal(t, d(t, tc.price).String(), e.Fields["price"])
		})
	}
}

func TestImpliedVolEndpointsAndBounds(t *testing.T) {
	in := goldenInput(t, Call, "100", "100", "1", "0.01", "0", "0.0001")
	pLo, err := BlackScholesPrice(in)
	require.NoError(t, err)
	res, err := ImpliedVol(ivInput(in, pLo))
	require.NoError(t, err)
	requireDecEqual(t, DecFromBPS(IVVolMinBPS), res.Vol)
	require.Zero(t, res.Iterations)

	in.Vol = d(t, "5")
	pHi, err := BlackScholesPrice(in)
	require.NoError(t, err)
	res, err = ImpliedVol(ivInput(in, pHi))
	require.NoError(t, err)
	requireDecEqual(t, DecFromBPS(IVVolMaxBPS), res.Vol)

	// A price exactly at the intrinsic lower bound of a deep OTM option is
	// matched at VolMin: every small volatility prices it at zero.
	otm := goldenInput(t, Call, "50", "100", "0.1", "0.01", "0", "0.2")
	res, err = ImpliedVol(ivInput(otm, Dec{}))
	require.NoError(t, err)
	requireDecEqual(t, DecFromBPS(IVVolMinBPS), res.Vol)

	// Volatilities near both ends of the interval are recovered near the
	// forward (F = 100·e^{0.005} ≈ 100.501), where vega is not negligible.
	// Far from it a 0.00011 vol is ~500 standard deviations deep and prices
	// exactly at the intrinsic bound, like every smaller vol.
	for _, vol := range []string{"0.00011", "0.001", "4.9", "4.99"} {
		in := goldenInput(t, Put, "100", "100.5", "0.5", "0.02", "0.01", vol)
		price, err := BlackScholesPrice(in)
		require.NoError(t, err)
		res, err := ImpliedVol(ivInput(in, price))
		require.NoError(t, err, vol)
		require.True(t, res.Vol.Sub(in.Vol).Abs().Cmp(d(t, "0.0000000000000000000001")) <= 0, "vol %s got %s", vol, res.Vol)
		require.True(t, res.Vol.Cmp(DecFromBPS(IVVolMinBPS)) >= 0 && res.Vol.Cmp(DecFromBPS(IVVolMaxBPS)) <= 0)
	}
}

func TestImpliedVolValidation(t *testing.T) {
	in := ivInput(goldenInput(t, Call, "100", "100", "1", "0.05", "0", "0.2"), d(t, "10"))
	in.Kind = "STRADDLE"
	_, err := ImpliedVol(in)
	e := requireCode(t, err, errs.CodeValidationFailed)
	require.Equal(t, "kind", e.Fields["field"])
	in.Kind = Call
	in.TimeYears = Dec{}
	_, err = ImpliedVol(in)
	e = requireCode(t, err, errs.CodeValidationFailed)
	require.Equal(t, "time_years", e.Fields["field"])
}

func TestImpliedVolDeterministic(t *testing.T) {
	in := ivInput(goldenInput(t, Put, "97.5", "101.25", "0.75", "0.031", "0.012", "0.37"), d(t, "11.2"))
	first, err := ImpliedVol(in)
	require.NoError(t, err)
	for range 5 {
		again, err := ImpliedVol(in)
		require.NoError(t, err)
		require.Equal(t, first.Vol.String(), again.Vol.String())
		require.Equal(t, first.Residual.String(), again.Residual.String())
		require.Equal(t, first.Iterations, again.Iterations)
	}
}
