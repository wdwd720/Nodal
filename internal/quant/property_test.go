package quant

import (
	"errors"
	"math/big"
	"testing"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// scaled returns n × 10^-scale exactly.
func scaled(n int64, scale int) Dec {
	c := big.NewInt(n)
	return Dec{c: c.Mul(c, tenPow(scaleDigits-scale))}
}

// genMarket draws a realistic option: spot 1..10_000, moneyness 0.5..2,
// one day to 30 years, volatility 1%..300%, rates −5%..20%, yields −5%..15%.
func genMarket(t *rapid.T) BSMInput {
	spot := scaled(rapid.Int64Range(100, 1_000_000).Draw(t, "spot_cents"), 2)
	k, err := spot.MulRound(scaled(rapid.Int64Range(500, 2000).Draw(t, "moneyness_milli"), 3), money.RoundHalfEven)
	if err != nil {
		t.Fatal(err)
	}
	k, err = k.Round(4, money.RoundHalfEven)
	if err != nil {
		t.Fatal(err)
	}
	return BSMInput{
		Kind:          rapid.SampledFrom([]OptionKind{Call, Put}).Draw(t, "kind"),
		Spot:          spot,
		Strike:        k.Max(scaled(1, 4)),
		TimeYears:     scaled(rapid.Int64Range(3, 30_000).Draw(t, "t_milli"), 3),
		Vol:           scaled(rapid.Int64Range(100, 30_000).Draw(t, "vol_bp"), 4),
		Rate:          scaled(rapid.Int64Range(-500, 2000).Draw(t, "r_bp"), 4),
		DividendYield: scaled(rapid.Int64Range(-500, 1500).Draw(t, "q_bp"), 4),
	}
}

func mustBS(t interface{ Fatalf(string, ...any) }, in BSMInput) BSMResult {
	r, err := BlackScholes(in)
	if err != nil {
		t.Fatalf("BlackScholes(%+v): %v", in, err)
	}
	return r
}

// scaleTol returns base × max(1, S, K).
func scaleTol(in BSMInput, base Dec) Dec {
	return base.Mul(DecFromInt64(1).Max(in.Spot).Max(in.Strike))
}

// condTol returns n ulps × max(1, S, K) × (1 + ⌈1/(σ√T)⌉): the absolute
// error of every output is a few ulps of its inputs amplified by the
// division by the total volatility in d1 (and, for Gamma, in the formula
// itself).
func condTol(in BSMInput, n int64) Dec {
	o := oracle{prec: 128}
	inv, _ := o.f().Quo(o.int(1), o.f().Mul(o.dec(in.Vol), o.sqrt(o.dec(in.TimeYears)))).Int(nil)
	amp := DecFromInt64(inv.Int64() + 2)
	return scaleTol(in, ulps(n)).Mul(amp)
}

// TestProp_PutCallParity: C − P = S·e^{−qT} − K·e^{−rT}. The identity holds
// to a few ulps by construction (N(−d) = 1 − N(d) exactly) and is checked
// both against the package's own discount factors and an oracle forward.
func TestProp_PutCallParity(t *testing.T) {
	o := oracle{prec: 320}
	rapid.Check(t, func(rt *rapid.T) {
		in := genMarket(rt)
		in.Kind = Call
		c := mustBS(rt, in)
		in.Kind = Put
		p := mustBS(rt, in)
		sq, kr := discounted(t, in)
		lhs := c.Price.Sub(p.Price)
		if diff := lhs.Sub(sq.Sub(kr)).Abs(); diff.Cmp(scaleTol(in, ulps(10))) > 0 {
			rt.Fatalf("C − P = %s, S·e^-qT − K·e^-rT = %s, diff %s", lhs, sq.Sub(kr), diff)
		}
		s, k, tt := o.dec(in.Spot), o.dec(in.Strike), o.dec(in.TimeYears)
		fwd := o.f().Sub(o.f().Mul(s, o.exp(o.f().Neg(o.f().Mul(o.dec(in.DividendYield), tt)))),
			o.f().Mul(k, o.exp(o.f().Neg(o.f().Mul(o.dec(in.Rate), tt)))))
		requireWithin(rt, "parity vs oracle forward", lhs, fwd, scaleTol(in, ulps(20)))
		// Delta parity: Δc − Δp = e^{−qT}, exactly up to two roundings.
		dfq := expDec(in.DividendYield.Mul(in.TimeYears).Neg())
		if diff := c.Delta.Sub(p.Delta).Sub(dfq).Abs(); diff.Cmp(ulps(2)) > 0 {
			rt.Fatalf("Δc − Δp = %s, e^-qT = %s", c.Delta.Sub(p.Delta), dfq)
		}
		requireDecEqual(t, c.Gamma, p.Gamma)
		requireDecEqual(t, c.Vega, p.Vega)
	})
}

// TestProp_GreeksSanity checks the no-arbitrage shape of the model: bounded
// deltas, non-negative gamma and vega, prices inside their bounds,
// monotonicity in spot and strike, and convexity in strike.
func TestProp_GreeksSanity(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		in := genMarket(rt)
		res := mustBS(rt, in)
		sq, kr := discounted(t, in)
		dfq := expDec(in.DividendYield.Mul(in.TimeYears).Neg())

		// Exact: these follow from monotone rounding and the projection.
		if in.Kind == Call {
			if res.Delta.Sign() < 0 || res.Delta.Cmp(dfq) > 0 {
				rt.Fatalf("call delta %s outside [0, %s]", res.Delta, dfq)
			}
			if lo := sq.Sub(kr).Max(Dec{}); res.Price.Cmp(lo) < 0 || res.Price.Cmp(sq) > 0 {
				rt.Fatalf("call %s outside [%s, %s]", res.Price, lo, sq)
			}
		} else {
			if res.Delta.Sign() > 0 || res.Delta.Cmp(dfq.Neg()) < 0 {
				rt.Fatalf("put delta %s outside [%s, 0]", res.Delta, dfq.Neg())
			}
			if lo := kr.Sub(sq).Max(Dec{}); res.Price.Cmp(lo) < 0 || res.Price.Cmp(kr) > 0 {
				rt.Fatalf("put %s outside [%s, %s]", res.Price, lo, kr)
			}
		}
		if res.Gamma.Sign() < 0 || res.Vega.Sign() < 0 {
			rt.Fatalf("gamma %s, vega %s", res.Gamma, res.Vega)
		}

		tol := condTol(in, 100)
		// Monotone in spot: calls rise, puts fall.
		bumped := in
		bumped.Spot = in.Spot.Add(in.Spot.Mul(scaled(rapid.Int64Range(1, 1000).Draw(rt, "ds_bp"), 4)))
		up := mustBS(rt, bumped)
		if in.Kind == Call && up.Price.Cmp(res.Price.Sub(tol)) < 0 || in.Kind == Put && up.Price.Cmp(res.Price.Add(tol)) > 0 {
			rt.Fatalf("%s not monotone in spot: %s → %s at S %s → %s", in.Kind, res.Price, up.Price, in.Spot, bumped.Spot)
		}
		// Monotone in strike: calls fall, puts rise.
		h := in.Strike.Mul(scaled(rapid.Int64Range(1, 1000).Draw(rt, "dk_bp"), 4))
		kUp, kDown := in, in
		kUp.Strike = in.Strike.Add(h)
		kDown.Strike = in.Strike.Sub(h)
		cu, cd := mustBS(rt, kUp), mustBS(rt, kDown)
		if in.Kind == Call && cu.Price.Cmp(res.Price.Add(tol)) > 0 || in.Kind == Put && cu.Price.Cmp(res.Price.Sub(tol)) < 0 {
			rt.Fatalf("%s not monotone in strike: %s → %s", in.Kind, res.Price, cu.Price)
		}
		// Convex in strike: V(K−h) + V(K+h) >= 2·V(K).
		if conv := cd.Price.Add(cu.Price).Sub(res.Price.Add(res.Price)); conv.Cmp(tol.Neg()) < 0 {
			rt.Fatalf("%s not convex in strike at K=%s h=%s: %s", in.Kind, in.Strike, h, conv)
		}
	})
}

// TestProp_ImpliedVolRoundTrip: price(IV(price(σ))) = price(σ) to the
// solver tolerance, and IV = σ wherever vega is not tiny.
func TestProp_ImpliedVolRoundTrip(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		in := genMarket(rt)
		in.Vol = scaled(rapid.Int64Range(100, 30_000).Draw(rt, "vol_bp"), 4)
		res := mustBS(rt, in)
		iv, err := ImpliedVol(ivInput(in, res.Price))
		if err != nil {
			// Only a price indistinguishable from the VolMin price may fail,
			// and only as below-VolMin.
			e, ok := errs.As(err)
			lowIn := in
			lowIn.Vol = DecFromBPS(IVVolMinBPS)
			pLo, perr := BlackScholesPrice(lowIn)
			if !ok || e.Fields["reason"] != ReasonPriceBelowVolMin || perr != nil || pLo.Sub(res.Price).Abs().Cmp(ulps(10)) > 0 {
				rt.Fatalf("ImpliedVol(%+v at price %s): %v", in, res.Price, err)
			}
			return
		}
		back := in
		back.Vol = iv.Vol
		again, err := BlackScholesPrice(back)
		if err != nil {
			rt.Fatal(err)
		}
		if diff := again.Sub(res.Price).Abs(); diff.Cmp(scaleTol(in, d(t, "0.000000000000000000000001"))) > 0 {
			rt.Fatalf("round trip price %s → vol %s → %s (diff %s)", res.Price, iv.Vol, again, diff)
		}
		if res.Vega.Cmp(scaleTol(in, d(t, "0.000001"))) >= 0 {
			if diff := iv.Vol.Sub(in.Vol).Abs(); diff.Cmp(d(t, "0.00000000000000000001")) > 0 {
				rt.Fatalf("vol %s recovered as %s (vega %s)", in.Vol, iv.Vol, res.Vega)
			}
		}
		if iv.Iterations > IVMaxIterations {
			rt.Fatalf("iterations %d", iv.Iterations)
		}
	})
}

// TestProp_BlackScholesVsOracle compares every output with the independent
// big.Float implementation.
func TestProp_BlackScholesVsOracle(t *testing.T) {
	o := oracle{prec: 320}
	worst := new(big.Float)
	rapid.Check(t, func(rt *rapid.T) {
		in := genMarket(rt)
		res := mustBS(rt, in)
		call, put := o.bsm(o.dec(in.Spot), o.dec(in.Strike), o.dec(in.TimeYears), o.dec(in.Rate), o.dec(in.DividendYield), o.dec(in.Vol))
		want := call
		if in.Kind == Put {
			want = put
		}
		got := resultFields(res)
		for i := range got {
			mag := o.f().Abs(want[i])
			tol := condTol(in, 1000)
			if mag.Cmp(o.int(1)) > 0 { // large Greeks (short-dated gamma, theta) get a relative bound
				f, _ := mag.Int(nil)
				tol = tol.Mul(Dec{c: f.Mul(f, unit())})
			}
			if diff := absDiff(got[i], want[i]); diff.Cmp(worst) > 0 {
				worst.Set(diff)
			}
			requireWithin(rt, string(in.Kind)+" "+greekNames[i], got[i], want[i], tol)
		}
	})
	t.Logf("largest absolute deviation from the oracle: %s", worst.Text('g', 4))
}

// TestProp_DivRoundMatchesMoney cross-checks the package's rounding
// primitive against the money package's on random operands and every mode.
func TestProp_DivRoundMatchesMoney(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		num := genBig(rt, "num", 60)
		den := genBig(rt, "den", 30)
		mode := rapid.SampledFrom(allModes()).Draw(rt, "mode")
		got, err := divRound(num, den, mode)
		want, werr := moneyDiv(num, den, mode)
		if (err == nil) != (werr == nil) {
			rt.Fatalf("%s / %s (%s): quant %v, money %v", num, den, mode, err, werr)
		}
		if err != nil {
			if den.Sign() == 0 {
				requireCodeR(rt, err, errs.CodeValidationFailed)
			} else {
				requireCodeR(rt, err, errs.CodePrecisionLoss)
			}
			return
		}
		if got.Cmp(want) != 0 {
			rt.Fatalf("%s / %s (%s): quant %s, money %s", num, den, mode, got, want)
		}
		if mode == money.RoundHalfEven { // the internal fast path must agree
			if den.Sign() != 0 && quoHalfEven(num, den).Cmp(want) != 0 {
				rt.Fatalf("quoHalfEven(%s, %s) = %s, want %s", num, den, quoHalfEven(num, den), want)
			}
		}
	})
}

// moneyDiv divides with money's own rounding primitive.
func moneyDiv(num, den *big.Int, mode money.RoundingMode) (*big.Int, error) {
	q, err := money.QuantityFromBigInt(num).Div(money.QuantityFromBigInt(den), mode)
	if err != nil {
		return nil, err
	}
	return q.BigInt(), nil
}

func requireCodeR(t *rapid.T, err error, code errs.Code) {
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("error %v, want code %s", err, code)
	}
}

func genBig(t *rapid.T, label string, maxDigits int) *big.Int {
	s := rapid.StringMatching(`-?[0-9]{1,`+big.NewInt(int64(maxDigits)).String()+`}`).Draw(t, label)
	v, _ := new(big.Int).SetString(s, 10)
	return v
}

func TestProp_DecArithmetic(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		a := Dec{c: genBig(rt, "a", 60)}
		b := Dec{c: genBig(rt, "b", 60)}
		if !a.Add(b).Sub(b).Equal(a) || !a.Sub(b).Add(b).Equal(a) || !a.Add(b).Equal(b.Add(a)) {
			rt.Fatalf("add/sub laws fail for %s, %s", a, b)
		}
		if !a.Neg().Neg().Equal(a) || !a.Add(a.Neg()).IsZero() || !a.Abs().Equal(a.Neg().Abs()) {
			rt.Fatalf("neg/abs laws fail for %s", a)
		}
		// Mul: commutative, within ½ ulp of the exact product.
		p := a.Mul(b)
		if !p.Equal(b.Mul(a)) {
			rt.Fatalf("Mul not commutative for %s, %s", a, b)
		}
		exact := new(big.Int).Mul(a.coef(), b.coef())
		twice := new(big.Int).Mul(p.coef(), unit())
		twice.Sub(twice, exact).Abs(twice).Lsh(twice, 1)
		if twice.Cmp(unit()) > 0 {
			rt.Fatalf("%s × %s = %s is more than ½ ulp off", a, b, p)
		}
		// Quo in every inexact mode: within one ulp of the exact quotient,
		// and the nearest modes within ½ ulp.
		if b.IsZero() {
			return
		}
		for _, mode := range inexactModes() {
			q, err := a.Quo(b, mode)
			if err != nil {
				rt.Fatalf("%s / %s (%s): %v", a, b, mode, err)
			}
			// |q·b − a·10^40|... in coefficient terms: |q·b − a·u| < |b|
			lhs := new(big.Int).Mul(q.coef(), b.coef())
			rhs := new(big.Int).Mul(a.coef(), unit())
			diff := lhs.Sub(lhs, rhs).Abs(lhs)
			if diff.Cmp(new(big.Int).Abs(b.coef())) >= 0 {
				rt.Fatalf("%s / %s (%s) = %s not within one ulp", a, b, mode, q)
			}
			if mode == money.RoundHalfEven || mode == money.RoundHalfUp {
				if new(big.Int).Lsh(diff, 1).Cmp(new(big.Int).Abs(b.coef())) > 0 {
					rt.Fatalf("%s / %s (%s) = %s not nearest", a, b, mode, q)
				}
			}
		}
		requireDecEqual(t, a, mustParse(rt, a.String()))
	})
}

func mustParse(t *rapid.T, s string) Dec {
	v, err := ParseDec(s)
	if err != nil {
		t.Fatalf("ParseDec(%q): %v", s, err)
	}
	return v
}

func TestProp_StringParseRoundTrip(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		v := Dec{c: genBig(rt, "coef", 160)}
		s := v.String()
		back := mustParse(rt, s)
		if !back.Equal(v) || back.String() != s {
			rt.Fatalf("round trip %s → %q → %s", v, s, back)
		}
		scale := int32(rapid.IntRange(0, 40).Draw(rt, "scale"))
		fixed, err := v.StringFixed(scale, money.RoundHalfEven)
		if err != nil {
			rt.Fatal(err)
		}
		r, err := v.Round(scale, money.RoundHalfEven)
		if err != nil {
			rt.Fatal(err)
		}
		if !mustParse(rt, fixed).Equal(r) {
			rt.Fatalf("StringFixed(%d) = %q, Round = %s", scale, fixed, r)
		}
	})
}

func TestPropertyHelpersAgree(t *testing.T) {
	require.Equal(t, "0.0003", scaled(3, 4).String())
	require.Equal(t, "-12.5", scaled(-125, 1).String())
}
