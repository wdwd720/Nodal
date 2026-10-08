package quant

import "math/big"

// This file is the integer-only engine behind Exp, Ln, NormPDF and NormCDF.
//
// Values are binary fixed point: an integer y at precision w stands for the
// real number y·2^-w, and one "ulp" at precision w is 2^-w. Every routine
// returns an approximation y together with a RIGOROUS bound e on its error,
// |y·2^-w − f(x)| <= e·2^-w, derived operation by operation in the comments
// below (every multiplication or division that discards bits contributes at
// most one ulp, and propagated errors are scaled by the derivative of what
// follows). zivRound then turns (y, e) into a correctly rounded Dec, or asks
// for more precision when the interval [y−e, y+e] straddles a rounding
// boundary.
//
// TestFixedErrorBoundsHold checks every bound against an independent oracle
// carrying 256 more bits, at precisions from 48 to 260 bits, including low
// ones where a bound that were too small would be exposed.

const (
	// ln2Hex is the fractional part of ln 2, floor(ln2 · 2^1088) in hex.
	ln2Hex = "" +
		"b17217f7d1cf79abc9e3b39803f2f6af40f343267298b62d8a0d175b8baafa2be7b8" +
		"76206debac98559552fb4afa1b10ed2eae35c138214427573b291169b8253e96ca16" +
		"224ae8c51acbda11317c387eb9ea9bc3b136603b256fa0ec7657f74b72ce87b19d65" +
		"48caf5dfa6bd38303248655fa1872f20e3a2da2d97c50f3fd5c607f4ca11fb5bfb90"
	// invSqrt2PiHex is 1/√(2π) = 0.3989…, floor(2^1088/√(2π)) in hex.
	invSqrt2PiHex = "" +
		"662114cf50d942343f2cf1402eae38bfd3829f30512706d8c0471b4802639d2e9df8" +
		"ac55447d3db723f5d3abb374e88deb14277970be1513278e993445f08cff98282e1f" +
		"d947ce4b4ecdbd6e65c23395cacb544de69eea32c276937b1bc567691c1cec4882c7" +
		"7aacefe1f6708329ab3fc8d2ab2435ee45c9150c426bbb49487e480d8928e623122d"
	// constBits is the precision of the constants above. Every routine is
	// only ever called with w <= constBits (TestPrecisionScheduleFitsConstants).
	constBits = 4 * 272

	// zivStartBits is the first working precision. 192 bits leaves about 59
	// bits beyond the 10^-40 (≈ 2^-133) target, so a second attempt is needed
	// only when the exact result lies within ~2^-170 of a rounding boundary.
	zivStartBits = 192
	// zivAttempts bounds the precision doublings: 192, 384, 768 bits.
	zivAttempts = 3

	// expHalvings is the number of argument halvings (and final squarings)
	// in expFx; expGuardBits is extra precision for that stage.
	expHalvings  = 8
	expGuardBits = 16
)

// fxConst returns floor(C·2^w) for the constant C = 0.<hex>, with error
// below one ulp. w must not exceed constBits. Only the leading ceil(w/8)
// bytes are decoded.
func fxConst(hex string, w uint) *big.Int {
	buf := make([]byte, (w+7)/8)
	for i := range buf {
		buf[i] = hexNibble(hex[2*i])<<4 | hexNibble(hex[2*i+1])
	}
	v := new(big.Int).SetBytes(buf)
	return v.Rsh(v, uint(8*len(buf))-w) // #nosec G115 -- len(buf) <= constBits/8
}

// hexNibble decodes one lower-case hexadecimal digit.
func hexNibble(c byte) byte {
	switch {
	case '0' <= c && c <= '9':
		return c - '0'
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10
	}
	// Unreachable: the constants are fixed lower-case hexadecimal strings,
	// pinned to independently computed values by TestConstantsMatchOracle.
	panic("quant: malformed internal constant")
}

// fxOne returns 2^w, the fixed-point representation of 1 at precision w.
func fxOne(w uint) *big.Int { return new(big.Int).Lsh(bigOne(), w) }

// shrTrunc shifts z right by n bits rounding toward zero (big.Int.Rsh
// rounds toward −∞ for negative values), in place, and returns z.
func shrTrunc(z *big.Int, n uint) *big.Int {
	if z.Sign() >= 0 {
		return z.Rsh(z, n)
	}
	z.Neg(z)
	z.Rsh(z, n)
	return z.Neg(z)
}

// fxRatio returns floor(num·2^s / den) for den > 0 and any integer s; the
// error is below one ulp.
func fxRatio(num, den *big.Int, s int64) *big.Int {
	if s >= 0 {
		v := new(big.Int).Lsh(num, uint(s)) // #nosec G115 -- s >= 0 in this branch
		return v.Div(v, den)                // Euclidean division: floor, since den > 0
	}
	d := new(big.Int).Lsh(den, uint(-s)) // #nosec G115 -- s < 0 in this branch
	return new(big.Int).Div(num, d)
}

// cmpPow2 compares a with b·2^s (a, b >= 0).
func cmpPow2(a, b *big.Int, s int64) int {
	if s >= 0 {
		return a.Cmp(new(big.Int).Lsh(b, uint(s))) // #nosec G115 -- s >= 0 in this branch
	}
	return new(big.Int).Lsh(a, uint(-s)).Cmp(b) // #nosec G115 -- s < 0 here
}

// expFx returns (y, e) with |y·2^-w − exp(x·2^-w)| <= e·2^-w.
//
// Precondition: |x·2^-w| <= 200 and w <= constBits.
//
// Method: k = round(x/ln2), r = x − k·ln2 with |r| <= ln2/2; exp(r) by the
// Taylor series of r/2^8 followed by 8 squarings; exp(x) = 2^k·exp(r).
//
// Error (ulps at the stated precision):
//   - ln2 is truncated (< 1 ulp at w), so the computed r differs from the
//     true one by at most |k| ulps at w: a relative error |k|·2^-w in
//     exp(r) ≤ e^{ln2/2} < 1.4143, i.e. at most 1.4143·|k|·2^k ulps of
//     the result, bounded by (3|k|/2 + 1)·2^k for k > 0 and by 1 for k ≤ 0.
//   - Taylor at wt = w + 8 + 16 bits with |r/2^8| <= 0.00136: each term is
//     truncated twice and inherits at most 0.00136 of its predecessor's
//     error, so each term is within 2.01 < 3 ulps; the first term that
//     truncates to zero bounds the tail by 3.01 ulps. Sum: <= 3n + 4 ulps.
//   - Each squaring maps an error ε to at most 2·1.4143·ε + ε²·2^-wt + 1,
//     below 3ε + 1.
//   - Scaling by 2^(k − 24) to precision w multiplies the error by that
//     factor and, when it is a right shift, adds one ulp for the truncation
//     plus one for rounding the scaled error bound up.
func expFx(x *big.Int, w uint) (*big.Int, *big.Int) {
	ln2 := fxConst(ln2Hex, w)
	twoLn2 := new(big.Int).Lsh(ln2, 1)
	kb := new(big.Int).Lsh(x, 1)
	kb.Add(kb, ln2)
	kb.Div(kb, twoLn2) // floor((2x + ln2) / (2·ln2)) = round(x/ln2)
	k := kb.Int64()    // |k| <= 289 under the precondition
	r := new(big.Int).Mul(kb, ln2)
	r.Sub(x, r) // |r·2^-w| <= ln2/2

	const g = expHalvings + expGuardBits
	wt := w + g
	r.Lsh(r, expGuardBits) // r·2^-wt = (r·2^-w) / 2^expHalvings

	sum := fxOne(wt)
	term := fxOne(wt)
	div := new(big.Int)
	var n int64
	for {
		n++
		term.Mul(term, r)
		shrTrunc(term, wt)
		term.Quo(term, div.SetInt64(n)) // truncates toward zero
		if term.Sign() == 0 {
			break
		}
		sum.Add(sum, term)
	}
	eps := 3*n + 4
	for range expHalvings {
		sum.Mul(sum, sum)
		sum.Rsh(sum, wt) // sum > 0
		eps = 3*eps + 1
	}

	e := big.NewInt(eps)
	if shift := k - g; shift >= 0 {
		sum.Lsh(sum, uint(shift)) // #nosec G115 -- shift >= 0 in this branch
		e.Lsh(e, uint(shift))     // #nosec G115 -- shift >= 0 in this branch
	} else {
		sum.Rsh(sum, uint(-shift)) // #nosec G115 -- shift < 0 in this branch
		e.Rsh(e, uint(-shift))     // #nosec G115 -- shift < 0 in this branch
		e.Add(e, big.NewInt(2))
	}
	if k > 0 {
		red := big.NewInt(3*k/2 + 1)
		e.Add(e, red.Lsh(red, uint(k))) // #nosec G115 -- 0 < k <= 289
	} else {
		e.Add(e, bigOne())
	}
	return sum, e
}

// lnFx returns (y, e) with |y·2^-w − ln(num/den)| <= e·2^-w.
//
// Precondition: num, den > 0 and w <= constBits.
//
// Method: write num/den = 2^m·f with f in [1/√2, √2); then
// ln(num/den) = m·ln2 + 2·atanh(z) with z = (f−1)/(f+1), |z| <= 0.1716, and
// atanh(z) = Σ z^(2j+1)/(2j+1), which gains more than 5 bits per term.
//
// Error (ulps at w):
//   - F = floor(f·2^w) is within 1 ulp; dz/df <= 0.686, and the division
//     forming z truncates once more, so |Z − z| <= 1.686. Through
//     2·atanh' <= 2.061 that is at most 3.48.
//   - Each power z^(2j+1) is within 1.21 ulps of the exact power of the
//     computed z (truncations of 1 ulp, and the squared z is within 1 ulp,
//     scaled by |z| <= 0.1716); dividing by 2j+1 adds one: <= 1.41 per term.
//     The tail after the first zero power is below 0.42. Doubled: 2.82·t + 0.84.
//   - m·ln2 with ln2 truncated: |m| ulps.
//
// Total <= 4·t + |m| + 12 for t non-zero series terms.
func lnFx(num, den *big.Int, w uint) (*big.Int, *big.Int) {
	m := int64(num.BitLen() - den.BitLen())
	if cmpPow2(num, den, m) < 0 {
		m-- // now 2^m <= num/den < 2^(m+1)
	}
	n2 := new(big.Int).Mul(num, num)
	d2 := new(big.Int).Mul(den, den)
	if cmpPow2(n2, d2, 2*m+1) >= 0 {
		m++ // num/den >= √2·2^m: centre f in [1/√2, 1)
	}
	f := fxRatio(num, den, int64(w)-m) // #nosec G115 -- w <= constBits
	one := fxOne(w)
	z := new(big.Int).Sub(f, one)
	z.Lsh(z, w)
	z.Quo(z, f.Add(f, one)) // truncates toward zero
	z2 := new(big.Int).Mul(z, z)
	z2.Rsh(z2, w)

	sum := new(big.Int).Set(z)
	term := new(big.Int).Set(z)
	t := new(big.Int)
	div := new(big.Int)
	var terms int64
	for j := int64(1); ; j++ {
		term.Mul(term, z2)
		shrTrunc(term, w)
		if term.Sign() == 0 {
			break
		}
		terms++
		sum.Add(sum, t.Quo(term, div.SetInt64(2*j+1)))
	}
	y := sum.Lsh(sum, 1)
	l2 := fxConst(ln2Hex, w)
	y.Add(y, l2.Mul(l2, big.NewInt(m)))
	if m < 0 {
		m = -m
	}
	return y, big.NewInt(4*terms + m + 12)
}

// pdfFx returns (y, e) with |y·2^-w − φ(x)| <= e·2^-w for x = c·10^-40 and
// the standard normal density φ(x) = exp(−x²/2)/√(2π).
//
// Precondition: |x| < 14 and w <= constBits.
//
// Error (ulps at w): u = x²/2 is floored (u − U < 1 ulp, so exp(−U) exceeds
// exp(−u) by at most 1.0000001 ulps); the exponential adds its own bound
// e0; multiplying by the truncated 1/√(2π) (< 0.399, error < 1 ulp) and
// truncating gives at most 0.399·(e0 + 2) + 1 + 1 <= (e0 + 2)/2 + 3.
func pdfFx(c *big.Int, w uint) (*big.Int, *big.Int) {
	u := new(big.Int).Mul(c, c)
	u.Lsh(u, w)
	den := new(big.Int).Mul(unit(), unit())
	u.Quo(u, den.Lsh(den, 1)) // floor(x²/2 · 2^w)
	y, e := expFx(u.Neg(u), w)
	e.Add(e, big.NewInt(2))
	y.Mul(y, fxConst(invSqrt2PiHex, w))
	y.Rsh(y, w)
	e.Rsh(e, 1)
	return y, e.Add(e, big.NewInt(3))
}

// cdfFx returns (y, e, ws) with |y·2^-ws − Φ(x)| <= e·2^-ws for
// x = c·10^-40 and the standard normal distribution function Φ.
//
// Precondition: |x| < 14 and w <= constBits − 146.
//
// Method (Marsaglia's series): Φ(x) = 1/2 + φ(x)·S(x) with
// S(x) = Σ_{n>=0} x^(2n+1)/(1·3·5···(2n+1)), all terms positive for x > 0,
// and Φ(−x) = 1 − Φ(x). Because φ(x)·S(x) <= 1/2, S(x) < 1.26·e^{x²/2},
// i.e. log2 S < 0.7214·x² + 1: the work precision ws = w + extra carries
// that many additional bits so that φ's absolute error, multiplied by S,
// stays at the scale of 2^-w.
//
// Error of the series (ulps at ws): every computed term is an underestimate
// (all operations floor non-negative values, and x² is floored). Writing t_n
// for the exact terms, N for the number of iterations and S for the sum,
// the truncation errors contribute at most 2·Σ_{j<=n} t_n/t_j to term n,
// the error of x² at most n·t_n/x², and stopping at the first term that
// floors to zero once the ratio x²/(2n+3) is below 1/2 leaves a tail of at
// most twice that term's error. Summed this is below (N+1)(N + 3S) + 6N + 4
// for x >= 1 and below N² + 7N + 5 for x < 1; both are below
// E_S = 4(N+2)(N + floor(S) + 2).
//
// Product: with φ̃ within E_φ of φ, |φ̃S̃ − φS| <= E_φ·(floor(S)+1) +
// (φ̃ + E_φ)·E_S·2^-ws, plus 2 for the truncating multiply and the rounded-up
// bound.
func cdfFx(c *big.Int, w uint) (*big.Int, *big.Int, uint) {
	a := new(big.Int).Abs(c)
	u := unit()
	xi := new(big.Int).Add(a, u)
	xi.Sub(xi, bigOne())
	xc := xi.Quo(xi, u).Int64()                // ceil(|x|) <= 14
	extra := uint((7214*xc*xc+9999)/10000 + 4) // #nosec G115 -- 1 <= xc <= 14, so 5 <= extra <= 146
	ws := w + extra

	x := new(big.Int).Lsh(a, ws)
	x.Quo(x, u) // floor(|x|·2^ws)
	x2 := new(big.Int).Mul(a, a)
	x2.Lsh(x2, ws)
	x2.Quo(x2, new(big.Int).Mul(u, u)) // floor(x²·2^ws)

	sum := new(big.Int).Set(x)
	term := new(big.Int).Set(x)
	div := new(big.Int)
	n := int64(1)
	for ; ; n++ {
		term.Mul(term, x2)
		term.Rsh(term, ws)
		term.Quo(term, div.SetInt64(2*n+1))
		if term.Sign() == 0 && 2*n+3 > 2*xc*xc {
			break
		}
		sum.Add(sum, term)
	}
	sInt := new(big.Int).Rsh(sum, ws)
	eS := big.NewInt(n + 2)
	eS.Add(eS, sInt)
	eS.Mul(eS, big.NewInt(4*(n+2)))

	yp, ep := pdfFx(a, ws)
	p := new(big.Int).Mul(yp, sum)
	p.Rsh(p, ws)

	e := new(big.Int).Add(sInt, bigOne())
	e.Mul(e, ep)
	t := new(big.Int).Lsh(ep, 1)
	t.Add(t, yp)
	t.Mul(t, eS)
	t.Rsh(t, ws)
	e.Add(e, t)
	e.Add(e, big.NewInt(2))

	y := new(big.Int).Lsh(bigOne(), ws-1) // 1/2
	if c.Sign() >= 0 {
		y.Add(y, p)
	} else {
		y.Sub(y, p)
	}
	return y, e, ws
}

// fxEval evaluates a function at working precision w and returns an
// approximation y at precision wy (>= w) with error bound e in ulps of wy.
type fxEval func(w uint) (y, e *big.Int, wy uint)

// zivRound returns the Dec coefficient nearest to the exact value that eval
// approximates. It evaluates at precision start, 2·start, 4·start and stops
// as soon as both ends of the certified interval [y−e, y+e] round to the
// same coefficient, which is then the correctly rounded result (ties cannot
// occur: the exact values computed here are never midpoints, and exact
// special cases are handled by the callers). If every attempt is ambiguous
// — the exact value lies within about 2^-700 of a midpoint — the nearest
// coefficient to the last approximation is returned, which is still within
// one ulp. The sequence of precisions depends only on the input, so the
// result is deterministic either way.
func zivRound(eval fxEval, start uint) *big.Int {
	w := start
	var y *big.Int
	var wy uint
	for range zivAttempts {
		var e *big.Int
		y, e, wy = eval(w)
		if c, ok := certify(y, e, wy); ok {
			return c
		}
		w <<= 1
	}
	return nearestScaled(y, wy)
}

// certify returns the Dec coefficient nearest to every point of
// [(y−e)·2^-wy, (y+e)·2^-wy] and true, or false when the two ends of the
// interval round differently. It is nearestScaled(y−e) == nearestScaled(y+e)
// with the product by 10^40 formed once.
func certify(y, e *big.Int, wy uint) (*big.Int, bool) {
	u := unit()
	t := new(big.Int).Mul(y, u)
	t.Add(t, new(big.Int).Lsh(bigOne(), wy-1))
	de := new(big.Int).Mul(e, u)
	lo := new(big.Int).Sub(t, de)
	lo.Rsh(lo, wy)
	hi := t.Add(t, de)
	hi.Rsh(hi, wy)
	return lo, lo.Cmp(hi) == 0
}

// nearestScaled returns the integer nearest to v·2^-wy·10^40 (ties toward
// +∞), i.e. the Dec coefficient nearest to the fixed-point value v.
func nearestScaled(v *big.Int, wy uint) *big.Int {
	t := new(big.Int).Mul(v, unit())
	t.Add(t, new(big.Int).Lsh(bigOne(), wy-1))
	return t.Rsh(t, wy)
}

// isqrt returns floor(√n) for n >= 0 by integer Newton iteration: starting
// from 2^ceil(bits/2) >= √n, x ← floor((x + floor(n/x))/2) strictly
// decreases while x > floor(√n) and stops as soon as it would not.
func isqrt(n *big.Int) *big.Int {
	if n.Sign() == 0 {
		return new(big.Int)
	}
	x := new(big.Int).Lsh(bigOne(), uint(n.BitLen()+1)/2) // #nosec G115 -- BitLen is non-negative
	y := new(big.Int)
	for {
		y.Quo(n, x)
		y.Add(y, x)
		y.Rsh(y, 1)
		if y.Cmp(x) >= 0 {
			return x
		}
		x, y = y, x
	}
}
