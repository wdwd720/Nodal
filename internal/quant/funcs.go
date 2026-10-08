package quant

import "math/big"

// Domain limits of the transcendental functions.
const (
	// ExpMaxArg is the largest argument Exp accepts; e^100 ≈ 2.7·10^43.
	// Larger arguments fail with OVERFLOW.
	ExpMaxArg = 100
	// ExpZeroArg: for x <= ExpZeroArg, e^x < 4.1·10^-41 is below half an
	// ulp, so the correctly rounded result is exactly 0 and Exp returns 0
	// without computing.
	ExpZeroArg = -93
	// NormTailCutoff: for |x| >= NormTailCutoff, Φ(−|x|) < 7.8·10^-45 and
	// φ(x) < 1.1·10^-43, both below half an ulp, so NormCDF returns exactly
	// 0 or 1 and NormPDF returns exactly 0. These clamps ARE the correctly
	// rounded values, not approximations.
	NormTailCutoff = 14
)

// Exp returns e^x correctly rounded to Scale digits (absolute error at most
// ½·10^-40) for x <= ExpMaxArg, and OVERFLOW above it. Exp(0) is exactly 1
// and x <= ExpZeroArg yields exactly 0.
//
// The result is certified: it is computed in binary fixed point with a
// rigorous error bound at 192, 384 or 768 bits plus the bits of e^x, and
// accepted only when the whole error interval rounds to the same decimal.
// See zivRound for the (astronomically unlikely) fallback.
func Exp(x Dec) (Dec, error) {
	if x.Cmp(DecFromInt64(ExpMaxArg)) > 0 {
		return Dec{}, errOverflow("exp argument above 100").WithField("x", x.String())
	}
	return expDec(x), nil
}

// expDec is Exp for arguments the caller has already bounded by ExpMaxArg.
func expDec(x Dec) Dec {
	c := x.coef()
	switch {
	case c.Sign() == 0:
		return DecFromInt64(1)
	case x.Cmp(DecFromInt64(ExpZeroArg)) <= 0:
		return Dec{}
	}
	return Dec{c: expCoef(c, zivStartBits)}
}

// expCoef returns the correctly rounded coefficient of e^x for x = c·10^-40
// in (ExpZeroArg, ExpMaxArg], starting the precision schedule at start bits.
func expCoef(c *big.Int, start uint) *big.Int {
	// kUp >= x/ln2 + 1 bounds the binary exponent of e^x for x > 0; the
	// working precision grows by kUp so the ABSOLUTE error stays at 2^-start.
	var kUp int64
	if c.Sign() > 0 {
		u := unit()
		xi := new(big.Int).Add(c, u)
		xi.Sub(xi, bigOne())
		xc := xi.Quo(xi, u).Int64() // ceil(x) <= 100
		kUp = (1443*xc+999)/1000 + 1
	}
	return zivRound(func(p uint) (*big.Int, *big.Int, uint) {
		w := p + uint(kUp) + 16 // #nosec G115 -- 0 <= kUp <= 146
		x := fxRatio(c, unit(), int64(w))
		y, e := expFx(x, w)
		// x was floored (< 1 ulp low), which lowers e^x by less than
		// e^x·2^-w <= 2^(kUp−1) ulps.
		return y, e.Add(e, new(big.Int).Lsh(bigOne(), uint(kUp)+1)), w // #nosec G115 -- 0 <= kUp <= 146
	}, start)
}

// Ln returns the natural logarithm of x correctly rounded to Scale digits
// (absolute error at most ½·10^-40). x must be positive; anything else fails
// with VALIDATION_FAILED. There is no upper limit; Ln(1) is exactly 0.
func Ln(x Dec) (Dec, error) {
	if x.Sign() <= 0 {
		return Dec{}, errField("x", "logarithm of a non-positive number").WithField("x", x.String())
	}
	return Dec{c: lnCoef(x.coef(), unit(), zivStartBits)}, nil
}

// lnRatio returns ln(a/b) correctly rounded for a, b > 0, computed from the
// exact ratio of the coefficients: unlike Ln(a.Quo(b)) it loses nothing to
// the quotient when a/b is very small or very large.
func lnRatio(a, b Dec) Dec { return Dec{c: lnCoef(a.coef(), b.coef(), zivStartBits)} }

func lnCoef(num, den *big.Int, start uint) *big.Int {
	return zivRound(func(w uint) (*big.Int, *big.Int, uint) {
		y, e := lnFx(num, den, w)
		return y, e, w
	}, start)
}

// Sqrt returns √x correctly rounded to Scale digits (error at most
// ½·10^-40, exact when the root has at most Scale digits). It takes the
// integer square root of the coefficient scaled by 10^40 by integer Newton
// iteration and rounds to nearest using the exact remainder; ties are
// impossible. x must not be negative (VALIDATION_FAILED).
func Sqrt(x Dec) (Dec, error) {
	if x.Sign() < 0 {
		return Dec{}, errField("x", "square root of a negative number").WithField("x", x.String())
	}
	return sqrtDec(x), nil
}

// sqrtDec is Sqrt for x >= 0.
func sqrtDec(x Dec) Dec {
	c := x.coef()
	if c.Sign() == 0 {
		return Dec{}
	}
	n := new(big.Int).Mul(c, unit())
	s := isqrt(n)
	rem := new(big.Int).Mul(s, s)
	rem.Sub(n, rem)
	// √n >= s + 1/2  <=>  n >= s² + s + 1/4  <=>  n − s² > s (integers).
	if rem.Cmp(s) > 0 {
		s.Add(s, bigOne())
	}
	return Dec{c: s}
}

// NormPDF returns the standard normal density φ(x) = e^{−x²/2}/√(2π)
// correctly rounded to Scale digits (absolute error at most ½·10^-40). It
// is defined for every x; |x| >= NormTailCutoff returns exactly 0, which is
// the correctly rounded value there.
func NormPDF(x Dec) Dec { return Dec{c: pdfCoef(x, zivStartBits)} }

func pdfCoef(x Dec, start uint) *big.Int {
	if x.Abs().Cmp(DecFromInt64(NormTailCutoff)) >= 0 {
		return new(big.Int)
	}
	c := x.coef()
	return zivRound(func(w uint) (*big.Int, *big.Int, uint) {
		y, e := pdfFx(c, w)
		return y, e, w
	}, start)
}

// NormCDF returns the standard normal distribution function Φ(x) correctly
// rounded to Scale digits: absolute error at most ½·10^-40 for every x,
// far inside the 10^-15 a risk display needs. Consequences of correct
// rounding that callers may rely on:
//
//   - 0 <= NormCDF(x) <= 1, and NormCDF(0) is exactly 0.5;
//   - monotone: x <= y implies NormCDF(x) <= NormCDF(y);
//   - symmetric: NormCDF(−x) = 1 − NormCDF(x) exactly;
//   - |x| >= NormTailCutoff (14) returns exactly 0 or 1, the correctly
//     rounded values (so [-40, 40] and beyond are covered without loss).
//
// These hold for every certified result; the uncertified fallback of
// zivRound (exact value within ~2^-700 of a rounding boundary) could break
// them by at most one ulp.
func NormCDF(x Dec) Dec { return Dec{c: cdfCoef(x, zivStartBits)} }

func cdfCoef(x Dec, start uint) *big.Int {
	c := x.coef()
	switch {
	case c.Sign() == 0:
		return new(big.Int).Rsh(unit(), 1)
	case x.Cmp(DecFromInt64(NormTailCutoff)) >= 0:
		return unit()
	case x.Cmp(DecFromInt64(-NormTailCutoff)) <= 0:
		return new(big.Int)
	}
	return zivRound(func(w uint) (*big.Int, *big.Int, uint) {
		return cdfFx(c, w)
	}, start)
}
