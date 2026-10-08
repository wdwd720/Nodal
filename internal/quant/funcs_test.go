package quant

import (
	"fmt"
	"math/big"
	"testing"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/errs"
)

// Published high-precision constants and values computed independently at
// 2000 bits. Each production result must equal the reference rounded to 40
// digits (half-even, which is round-to-nearest here: none of these is a
// midpoint), and the in-test oracle must agree with the reference too.
var referenceValues = []struct {
	name string
	eval func(t *testing.T) Dec
	ref  string
}{
	{"e = exp(1)", func(t *testing.T) Dec { return mustExp(t, "1") }, "2.7182818284590452353602874713526624977572470936999595749669676277240766"},
	{"1/e = exp(-1)", func(t *testing.T) Dec { return mustExp(t, "-1") }, "0.3678794411714423215955237701614608674458111310317678345078368016974615"},
	{"exp(0.5)", func(t *testing.T) Dec { return mustExp(t, "0.5") }, "1.6487212707001281468486507878141635716537761007101480115750793116406610"},
	{"exp(100)", func(t *testing.T) Dec { return mustExp(t, "100") }, "26881171418161354484126255515800135873611118.7737419224151916086152802870349095649141588710972198457108116708791906"},
	{"exp(-50)", func(t *testing.T) Dec { return mustExp(t, "-50") }, "0.0000000000000000000001928749847963917783017342816527012574752832651230"},
	{"exp(-92.5)", func(t *testing.T) Dec { return mustExp(t, "-92.5") }, "0.0000000000000000000000000000000000000000672605514964470814831751474117"},
	{"ln 2", func(t *testing.T) Dec { return mustLn(t, "2") }, "0.6931471805599453094172321214581765680755001343602552541206800094933936"},
	{"ln 10", func(t *testing.T) Dec { return mustLn(t, "10") }, "2.3025850929940456840179914546843642076011014886287729760333279009675726"},
	{"ln 0.5", func(t *testing.T) Dec { return mustLn(t, "0.5") }, "-0.6931471805599453094172321214581765680755001343602552541206800094933936"},
	{"ln 1e-40", func(t *testing.T) Dec { return mustLn(t, "0.0000000000000000000000000000000000000001") }, "-92.1034037197618273607196581873745683040440595451509190413331160387029044"},
	{"ln 123456789.123456789", func(t *testing.T) Dec { return mustLn(t, "123456789.123456789") }, "18.6314017671680180326939333482965375427970151745537353083517566119017413"},
	{"sqrt 2", func(t *testing.T) Dec { return mustSqrt(t, "2") }, "1.41421356237309504880168872420969807856967187537694807317667973799073247846"},
	{"sqrt 3", func(t *testing.T) Dec { return mustSqrt(t, "3") }, "1.7320508075688772935274463415058723669428052538103806280558069794519330"},
	{"sqrt 1e-40", func(t *testing.T) Dec { return mustSqrt(t, "0.0000000000000000000000000000000000000001") }, "0.00000000000000000001"},
	{"phi(0) = 1/sqrt(2 pi)", func(t *testing.T) Dec { return NormPDF(Dec{}) }, "0.39894228040143267793994605993438186847585863116493465766592582967065792589930"},
	{"phi(1)", func(t *testing.T) Dec { return NormPDF(d(t, "1")) }, "0.2419707245191433497978301929355606548286719707374350254875550842811001"},
	{"phi(-2.5)", func(t *testing.T) Dec { return NormPDF(d(t, "-2.5")) }, "0.0175283004935685373621583221667485861485141755177202304201203216959239"},
	{"phi(13.5)", func(t *testing.T) Dec { return NormPDF(d(t, "13.5")) }, "0.0000000000000000000000000000000000000001061268813915216083631546199143"},
	{"Phi(0)", func(t *testing.T) Dec { return NormCDF(Dec{}) }, "0.5"},
	{"Phi(1)", func(t *testing.T) Dec { return NormCDF(d(t, "1")) }, "0.841344746068542948585232545632037922477912966726604390987394"},
	{"Phi(-1.96)", func(t *testing.T) Dec { return NormCDF(d(t, "-1.96")) }, "0.024997895148220434136584269040837190022499779061883391085717"},
	{"Phi(0.5)", func(t *testing.T) Dec { return NormCDF(d(t, "0.5")) }, "0.6914624612740131036377046106083377398836021755545779368207761426791558"},
	{"Phi(2)", func(t *testing.T) Dec { return NormCDF(d(t, "2")) }, "0.9772498680518207927997173628334665625282237762983215660163339998695237"},
	{"Phi(-3)", func(t *testing.T) Dec { return NormCDF(d(t, "-3")) }, "0.0013498980316300945266518147675949773778293681583806493642219853558057"},
	{"Phi(-10)", func(t *testing.T) Dec { return NormCDF(d(t, "-10")) }, "0.0000000000000000000000076198530241605260659733432515993083635040332780"},
	{"Phi(-13.3)", func(t *testing.T) Dec { return NormCDF(d(t, "-13.3")) }, "0.0000000000000000000000000000000000000001157341628369043592451418691987"},
}

func mustExp(t testing.TB, s string) Dec {
	t.Helper()
	v, err := Exp(d(t, s))
	require.NoError(t, err)
	return v
}

func mustLn(t testing.TB, s string) Dec {
	t.Helper()
	v, err := Ln(d(t, s))
	require.NoError(t, err)
	return v
}

func mustSqrt(t testing.TB, s string) Dec {
	t.Helper()
	v, err := Sqrt(d(t, s))
	require.NoError(t, err)
	return v
}

func TestReferenceValues(t *testing.T) {
	for _, tc := range referenceValues {
		t.Run(tc.name, func(t *testing.T) {
			requireDecEqual(t, dr(t, tc.ref), tc.eval(t))
		})
	}
	// The textbook quotes in the task statement, to their stated digits.
	require.Equal(t, "0.8413447460685429485852325456320379224779", NormCDF(d(t, "1")).String())
	requireWithin(t, "Phi(-1.96)", NormCDF(d(t, "-1.96")), defaultOracle().cdf(defaultOracle().dec(d(t, "-1.96"))), d(t, "0.0000000000000000000000000000000000000001"))
	require.True(t, NormCDF(d(t, "-1.96")).Sub(d(t, "0.024997895148220435")).Abs().Cmp(d(t, "0.000000000000000001")) <= 0)
}

// TestOracleMatchesReferences validates the oracle itself against the same
// published digits, so that its agreement with production means something.
func TestOracleMatchesReferences(t *testing.T) {
	o := defaultOracle()
	tol := o.f().SetMantExp(o.int(1), -200) // the references carry about 70 digits
	check := func(name string, got *big.Float, ref string) {
		t.Helper()
		want := o.f()
		_, ok := want.SetString(ref)
		require.True(t, ok)
		diff := o.f().Abs(o.f().Sub(got, want))
		require.True(t, diff.Cmp(tol) <= 0, "%s: oracle %s vs reference %s", name, got.Text('f', 60), ref)
	}
	check("e", o.exp(o.int(1)), "2.7182818284590452353602874713526624977572470936999595749669676277240766")
	check("ln 2", o.ln(o.int(2)), "0.6931471805599453094172321214581765680755001343602552541206800094933936")
	check("pi", o.pi(), "3.14159265358979323846264338327950288419716939937510582097494459230781640628620899")
	check("Phi(1)", o.cdf(o.int(1)), "0.841344746068542948585232545632037922477912966726604390987394")
	check("Phi(-10)", o.cdf(o.int(-10)), "0.0000000000000000000000076198530241605260659733432515993083635040332780")
	check("phi(1)", o.pdf(o.int(1)), "0.2419707245191433497978301929355606548286719707374350254875550842811001")
}

// TestConstantsMatchOracle pins the hexadecimal constants of fixed.go and
// unit() to independently computed values.
func TestConstantsMatchOracle(t *testing.T) {
	o := oracle{prec: constBits + 128}
	ln2 := o.ln(o.int(2))
	inv := o.f().Quo(o.int(1), o.sqrt(o.f().Mul(o.int(2), o.pi())))
	for _, tc := range []struct {
		name string
		hex  string
		want *big.Float
	}{{"ln2", ln2Hex, ln2}, {"1/sqrt(2pi)", invSqrt2PiHex, inv}} {
		require.Len(t, tc.hex, constBits/4, tc.name)
		scaled, _ := o.f().SetMantExp(tc.want, constBits).Int(nil) // floor: positive
		require.Equal(t, scaled.Text(16), tc.hex, tc.name)
		for _, w := range []uint{1, 7, 64, 133, 192, 500, constBits} {
			want, _ := o.f().SetMantExp(tc.want, int(w)).Int(nil)
			require.Equal(t, want.String(), fxConst(tc.hex, w).String(), "%s at %d bits", tc.name, w)
		}
	}
	require.Equal(t, new(big.Int).Exp(big.NewInt(10), big.NewInt(40), nil).String(), unit().String())
	require.Equal(t, 10, len(tenPow(9).String()))
}

// TestPrecisionScheduleFitsConstants checks that the largest precision any
// routine can request stays within the constants' 1088 bits.
func TestPrecisionScheduleFitsConstants(t *testing.T) {
	maxP := uint(zivStartBits) << (zivAttempts - 1)
	require.Equal(t, uint(768), maxP)
	// Exp: w = P + kUp + 16 with kUp <= ceil(1.443·100) + 1.
	require.LessOrEqual(t, maxP+uint((1443*100+999)/1000+1)+16, uint(constBits))
	// NormCDF: ws = P + ceil(0.7214·14²) + 4.
	require.LessOrEqual(t, maxP+uint((7214*14*14+9999)/10000+4), uint(constBits))
}

// accuracyInputs are spread over each function's domain, including the
// boundaries of its clamps.
func accuracyInputs() map[string][]string {
	return map[string][]string{
		"exp": {
			"-92.99", "-92.7959", "-90", "-50.123", "-10", "-1", "-0.5", "-0.0000000000000000000000000000000000000001",
			"0.0000000000000000000000000000000000000001", "0.001", "0.6931471805599453094172321214581765680755",
			"1", "2.5", "10", "33.3333333333333333333333333333333333333333", "50", "99.9999999999999999999999999999999999999999", "100",
		},
		"ln": {
			"0.0000000000000000000000000000000000000001", "0.00000000000000000001", "0.001", "0.5", "0.7071067811865475244008443621048490392848",
			"0.9999999999999999999999999999999999999999", "1.0000000000000000000000000000000000000001", "1.4142135623730950488016887242096980785697",
			"2", "2.7182818284590452353602874713526624977572", "10", "1000000", "123456789012345678901234567890.5",
			"99999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999999",
		},
		"sqrt": {
			"0.0000000000000000000000000000000000000001", "0.0000000000000000000000000000000000000004", "0.25", "0.5", "2", "3", "4",
			"10", "0.0001", "1000000", "123456789.987654321", "99999999999999999999999999999999999999.9999999999999999999999999999999999999999",
		},
		"pdf": {
			"0", "0.0000000000000000000000000000000000000001", "0.5", "-1", "1.96", "-3", "5", "-8.5", "10", "12.75", "13.5", "-13.6", "13.9999999999999999999999999999999999999999",
		},
		"cdf": {
			"0.0000000000000000000000000000000000000001", "-0.0000000000000000000000000000000000000001", "0.001", "-0.25", "0.5", "1", "-1.96",
			"1.6448536269514722", "2.5", "-3.5", "4", "-6", "7.25", "-8.75", "10", "-11.5", "12", "-13.3", "13.4", "-13.9999999999999999999999999999999999999999",
		},
	}
}

func TestFunctionAccuracyTables(t *testing.T) {
	o := defaultOracle()
	in := accuracyInputs()
	for _, s := range in["exp"] {
		got, err := Exp(d(t, s))
		require.NoError(t, err)
		requireCorrectlyRounded(t, "exp("+s+")", got, o.exp(o.dec(d(t, s))))
	}
	for _, s := range in["ln"] {
		got, err := Ln(d(t, s))
		require.NoError(t, err)
		requireCorrectlyRounded(t, "ln("+s+")", got, o.ln(o.dec(d(t, s))))
	}
	for _, s := range in["sqrt"] {
		got, err := Sqrt(d(t, s))
		require.NoError(t, err)
		requireCorrectlyRounded(t, "sqrt("+s+")", got, o.sqrt(o.dec(d(t, s))))
	}
	for _, s := range in["pdf"] {
		requireCorrectlyRounded(t, "phi("+s+")", NormPDF(d(t, s)), o.pdf(o.dec(d(t, s))))
	}
	for _, s := range in["cdf"] {
		requireCorrectlyRounded(t, "Phi("+s+")", NormCDF(d(t, s)), o.cdf(o.dec(d(t, s))))
	}
}

// TestNormCDFGrid covers [-15, 15] in steps of 0.05 against the oracle: each
// value is correctly rounded, far beyond the 1e-15 the display requires.
func TestNormCDFGrid(t *testing.T) {
	o := defaultOracle()
	step := d(t, "0.05")
	for x := d(t, "-15"); x.Cmp(d(t, "15")) <= 0; x = x.Add(step) {
		requireCorrectlyRounded(t, "Phi("+x.String()+")", NormCDF(x), o.cdf(o.dec(x)))
	}
}

func TestNormTails(t *testing.T) {
	one := DecFromInt64(1)
	for _, s := range []string{"14", "14.5", "20", "39.9999", "40", "40.0001", "1000", "1e3"} {
		x, err := ParseDec(s)
		if err != nil {
			continue // "1e3" is not a decimal literal
		}
		requireDecEqual(t, one, NormCDF(x), "Phi(%s)", s)
		requireDecEqual(t, Dec{}, NormCDF(x.Neg()), "Phi(-%s)", s)
		requireDecEqual(t, Dec{}, NormPDF(x), "phi(%s)", s)
		requireDecEqual(t, Dec{}, NormPDF(x.Neg()), "phi(-%s)", s)
	}
	// Just inside the cut-off the computed values are already the clamps,
	// so there is no jump at 14.
	in := d(t, "13.9999999999999999999999999999999999999999")
	requireDecEqual(t, one, NormCDF(in))
	requireDecEqual(t, Dec{}, NormCDF(in.Neg()))
	requireDecEqual(t, Dec{}, NormPDF(in))
	// Exp's zero clamp likewise agrees with the computed side.
	requireDecEqual(t, Dec{}, mustExp(t, "-92.9999999999999999999999999999999999999999"))
	requireDecEqual(t, Dec{}, mustExp(t, "-93"))
	requireDecEqual(t, Dec{}, mustExp(t, "-1000000"))
}

func TestNormCDFMonotoneSymmetricBounded(t *testing.T) {
	one := DecFromInt64(1)
	prev := Dec{}
	step := d(t, "0.0625")
	for x := d(t, "-41"); x.Cmp(d(t, "41")) <= 0; x = x.Add(step) {
		p := NormCDF(x)
		require.True(t, p.Sign() >= 0 && p.Cmp(one) <= 0, "Phi(%s) = %s outside [0,1]", x, p)
		require.True(t, p.Cmp(prev) >= 0, "Phi not monotone at %s: %s < %s", x, p, prev)
		requireDecEqual(t, one.Sub(p), NormCDF(x.Neg()), "symmetry at %s", x)
		prev = p
	}
	// Adjacent representable inputs around the steepest point.
	for _, s := range []string{"0", "0.5", "-1.25"} {
		x := d(t, s)
		require.True(t, NormCDF(x).Cmp(NormCDF(x.Add(ulps(1)))) <= 0)
		require.True(t, NormCDF(x.Sub(ulps(1))).Cmp(NormCDF(x)) <= 0)
	}
}

func TestProp_NormCDFMonotoneSymmetric(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		a := genDec(rt, "a", -16, 16, 6)
		b := a.Add(genDec(rt, "gap", 0, 2, 40))
		pa, pb := NormCDF(a), NormCDF(b)
		if pa.Cmp(pb) > 0 {
			rt.Fatalf("Phi(%s) = %s > Phi(%s) = %s", a, pa, b, pb)
		}
		if got := NormCDF(a.Neg()); !got.Equal(DecFromInt64(1).Sub(pa)) {
			rt.Fatalf("Phi(-%s) = %s, 1 - Phi(%s) = %s", a, got, a, DecFromInt64(1).Sub(pa))
		}
		if !NormPDF(a).Equal(NormPDF(a.Neg())) {
			rt.Fatalf("phi not even at %s", a)
		}
	})
}

func TestProp_CorrectlyRoundedVsOracle(t *testing.T) {
	o := defaultOracle()
	rapid.Check(t, func(rt *rapid.T) {
		x := genDec(rt, "x", -90, 100, 40)
		got, err := Exp(x)
		if err != nil {
			rt.Fatalf("Exp(%s): %v", x, err)
		}
		requireCorrectlyRounded(rt, "exp("+x.String()+")", got, o.exp(o.dec(x)))

		p := genDec(rt, "p", 0, 1000000, 40)
		if p.Sign() > 0 {
			got, err = Ln(p)
			if err != nil {
				rt.Fatalf("Ln(%s): %v", p, err)
			}
			requireCorrectlyRounded(rt, "ln("+p.String()+")", got, o.ln(o.dec(p)))
			got, err = Sqrt(p)
			if err != nil {
				rt.Fatalf("Sqrt(%s): %v", p, err)
			}
			requireCorrectlyRounded(rt, "sqrt("+p.String()+")", got, o.sqrt(o.dec(p)))
		}

		z := genDec(rt, "z", -14, 14, 40)
		requireCorrectlyRounded(rt, "Phi("+z.String()+")", NormCDF(z), o.cdf(o.dec(z)))
		requireCorrectlyRounded(rt, "phi("+z.String()+")", NormPDF(z), o.pdf(o.dec(z)))
	})
}

// TestInverseRoundTrips checks the documented consequences of correct
// rounding: with |Exp(x) − e^x| <= ½ulp, Ln(Exp(x)) is within
// ½ulp + ½ulp/e^x·(1 + ε) of x; Exp(Ln(y)) within (1 + y)·½ulp·(1 + ε) + ½ulp
// of y; Sqrt(y)² within (√y + 1)·ulp of y.
func TestInverseRoundTrips(t *testing.T) {
	ulp := ulps(1)
	for x := d(t, "-40"); x.Cmp(d(t, "40")) <= 0; x = x.Add(d(t, "0.37")) {
		y, err := Exp(x)
		require.NoError(t, err)
		back, err := Ln(y)
		require.NoError(t, err)
		bound := ulp.Add(ulp.quo(y)) // ulp·(1 + 1/e^x), with headroom over ½ + ½/e^x
		require.True(t, back.Sub(x).Abs().Cmp(bound) <= 0, "Ln(Exp(%s)) = %s, |diff| %s > %s", x, back, back.Sub(x).Abs(), bound)
	}
	for _, s := range []string{"0.0000001", "0.5", "1", "2", "17.25", "1000", "1234567.891"} {
		y := d(t, s)
		l, err := Ln(y)
		require.NoError(t, err)
		back, err := Exp(l)
		require.NoError(t, err)
		bound := ulp.Mul(DecFromInt64(2).Add(y)) // (1 + y)·ulp + ulp, with headroom
		require.True(t, back.Sub(y).Abs().Cmp(bound) <= 0, "Exp(Ln(%s)) = %s", s, back)

		r := mustSqrt(t, s)
		bound = ulp.Mul(sqrtDec(y).Add(DecFromInt64(1)))
		require.True(t, r.Mul(r).Sub(y).Abs().Cmp(bound) <= 0, "Sqrt(%s)^2 = %s", s, r.Mul(r))
	}
	// Exact squares are exact.
	for _, s := range []string{"4", "0.25", "1.44", "0.0000000000000000000000000000000000000004", "152415787532388367504942236884722755800955129"} {
		r := mustSqrt(t, s)
		requireDecEqual(t, d(t, s), r.Mul(r), "Sqrt(%s)", s)
	}
}

func TestFunctionDomains(t *testing.T) {
	_, err := Exp(d(t, "100.0000000000000000000000000000000000000001"))
	e := requireCode(t, err, errs.CodeOverflow)
	require.Contains(t, e.Fields, "x")
	_, err = Exp(DecFromInt64(1000))
	requireCode(t, err, errs.CodeOverflow)
	_, err = Exp(d(t, "100"))
	require.NoError(t, err)
	requireDecEqual(t, DecFromInt64(1), mustExp(t, "0"))

	for _, s := range []string{"0", "-0.0000000000000000000000000000000000000001", "-5"} {
		_, err = Ln(d(t, s))
		requireCode(t, err, errs.CodeValidationFailed)
	}
	requireDecEqual(t, Dec{}, mustLn(t, "1"))

	_, err = Sqrt(d(t, "-0.0000000000000000000000000000000000000001"))
	requireCode(t, err, errs.CodeValidationFailed)
	requireDecEqual(t, Dec{}, mustSqrt(t, "0"))
}

// TestFixedErrorBoundsHold checks the rigorous error bounds of the
// fixed-point routines against the oracle at several precisions, including
// low ones where an underestimated bound would be caught.
func TestFixedErrorBoundsHold(t *testing.T) {
	precisions := []uint{48, 64, 100, 136, 192, 260}
	check := func(rt *rapid.T, label string, y, e *big.Int, w uint, want *big.Float) {
		o := oracle{prec: want.Prec()}
		diff := o.f().Abs(o.f().Sub(o.fixed(y, w), want))
		bound := o.fixed(e, w)
		if diff.Cmp(bound) > 0 {
			rt.Fatalf("%s at %d bits: |error| %s exceeds bound %s ulps (%s)", label, w, diff.Text('g', 6), e, bound.Text('g', 6))
		}
	}
	rapid.Check(t, func(rt *rapid.T) {
		w := rapid.SampledFrom(precisions).Draw(rt, "w")
		o := oracle{prec: w + 256}

		// expFx on an exact binary input in [-98, 100].
		x := genDec(rt, "x", -98, 100, 40)
		xf := fxRatio(x.coef(), unit(), int64(w))
		y, e := expFx(xf, w)
		check(rt, "expFx("+x.String()+")", y, e, w, o.exp(o.fixed(xf, w)))

		// lnFx on a ratio spanning many orders of magnitude.
		num := genDec(rt, "num", 0, 1000000, 40).Add(ulps(1))
		den := genDec(rt, "den", 0, 1000, 40).Add(ulps(1))
		y, e = lnFx(num.coef(), den.coef(), w)
		check(rt, "lnFx("+num.String()+"/"+den.String()+")", y, e, w, o.ln(o.f().Quo(o.dec(num), o.dec(den))))

		// pdfFx and cdfFx for |z| < 14.
		z := genDec(rt, "z", -13, 13, 40)
		y, e = pdfFx(z.coef(), w)
		check(rt, "pdfFx("+z.String()+")", y, e, w, o.pdf(o.dec(z)))
		y, e, ws := cdfFx(z.coef(), w)
		check(rt, "cdfFx("+z.String()+")", y, e, ws, oracle{prec: ws + 256}.cdf(o.dec(z)))
	})
}

// TestZivRetryPath forces the precision schedule to start too low, so the
// first (and sometimes second) attempt is ambiguous, and checks that the
// certified results equal the default ones. Starting at 34 bits also
// drives the uncertified fallback, whose result must be the nearest
// coefficient to the last approximation.
func TestZivRetryPath(t *testing.T) {
	o := defaultOracle()
	type probe struct {
		name    string
		eval    fxEval
		def     Dec
		oracleV *big.Float
	}
	var probes []probe
	for i, s := range []string{"-50.5", "-1", "0.3", "1", "7.77", "42.42", "99.5"} {
		x := d(t, s)
		def, err := Exp(x)
		require.NoError(t, err)
		c := x.coef()
		probes = append(probes, probe{"exp(" + s + ")", func(w uint) (*big.Int, *big.Int, uint) {
			wx := w + 16 + 146
			y, e := expFx(fxRatio(c, unit(), int64(wx)), wx)
			return y, e.Add(e, new(big.Int).Lsh(bigOne(), 147)), wx
		}, def, o.exp(o.dec(x))})

		z := d(t, fmt.Sprintf("%d.%d", i-3, 3+i))
		zc := z.coef()
		probes = append(probes, probe{"Phi(" + z.String() + ")", func(w uint) (*big.Int, *big.Int, uint) {
			return cdfFx(zc, w)
		}, NormCDF(z), o.cdf(o.dec(z))})
		probes = append(probes, probe{"phi(" + z.String() + ")", func(w uint) (*big.Int, *big.Int, uint) {
			y, e := pdfFx(zc, w)
			return y, e, w
		}, NormPDF(z), o.pdf(o.dec(z))})

		y := x.Abs().Add(ulps(7))
		ln, err := Ln(y)
		require.NoError(t, err)
		yc := y.coef()
		probes = append(probes, probe{"ln(" + y.String() + ")", func(w uint) (*big.Int, *big.Int, uint) {
			v, e := lnFx(yc, unit(), w)
			return v, e, w
		}, ln, o.ln(o.dec(y))})
	}

	var retries, fallbacks int
	for _, p := range probes {
		// From 136 bits the first attempt is usually ambiguous; whatever
		// the number of attempts, the certified result is the default one.
		calls := 0
		counted := func(w uint) (*big.Int, *big.Int, uint) { calls++; return p.eval(w) }
		requireDecEqual(t, p.def, Dec{c: zivRound(counted, 136)}, "%s from 136 bits", p.name)
		if calls > 1 {
			retries++
		}
		// From 34 bits (attempts 34, 68, 136) the last attempt is often
		// still ambiguous, so the uncertified fallback runs. Its contract is
		// the nearest coefficient to the last approximation: within ½ ulp
		// plus that attempt's certified error, which at the production
		// schedule's 768 bits is far below one ulp.
		var lastY, lastE *big.Int
		var lastW uint
		recording := func(w uint) (*big.Int, *big.Int, uint) {
			lastY, lastE, lastW = p.eval(w)
			return lastY, lastE, lastW
		}
		low := Dec{c: zivRound(recording, 34)}
		lo := nearestScaled(new(big.Int).Sub(lastY, lastE), lastW)
		hi := nearestScaled(new(big.Int).Add(lastY, lastE), lastW)
		if lo.Cmp(hi) != 0 {
			fallbacks++
			requireDecEqual(t, Dec{c: nearestScaled(lastY, lastW)}, low, "%s fallback", p.name)
		}
		bound := o.f().Add(o.dec(ulps(1)).SetMantExp(o.dec(ulps(1)), -1), o.fixed(lastE, lastW))
		if diff := absDiff(low, p.oracleV); diff.Cmp(bound) > 0 {
			t.Fatalf("%s from 34 bits: %s is %s from the exact value, bound %s", p.name, low, diff.Text('g', 6), bound.Text('g', 6))
		}
	}
	t.Logf("probes %d, retried from 136 bits %d, uncertified fallbacks from 34 bits %d", len(probes), retries, fallbacks)
	require.Positive(t, retries, "the retry path was never exercised")
	require.Positive(t, fallbacks, "the fallback path was never exercised")
}

// genDec draws a decimal in [lo, hi] with up to frac fractional digits.
func genDec(t *rapid.T, label string, lo, hi int64, frac int) Dec {
	ip := rapid.Int64Range(lo, hi).Draw(t, label+"_int")
	digits := rapid.StringMatching(fmt.Sprintf("[0-9]{%d}", frac)).Draw(t, label+"_frac")
	v := DecFromInt64(ip)
	if frac == 0 {
		return v
	}
	fd, err := ParseDec("0." + digits)
	if err != nil {
		t.Fatalf("ParseDec: %v", err)
	}
	if ip < 0 || (ip == 0 && rapid.Bool().Draw(t, label+"_neg")) {
		v = v.Sub(fd)
	} else {
		v = v.Add(fd)
	}
	if v.Cmp(DecFromInt64(lo)) < 0 {
		return DecFromInt64(lo)
	}
	if v.Cmp(DecFromInt64(hi)) > 0 {
		return DecFromInt64(hi)
	}
	return v
}
