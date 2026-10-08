package quant

import (
	"math/big"
	"testing"
)

// The oracle is an independent, slower, high-precision reference built on
// big.Float. It is allowed here, in a _test.go file, and nowhere else. It
// deliberately shares no algorithm with the production engine:
//
//   - exp: split x = n + f with n = round(x), e^f by Taylor, e^n by binary
//     powering of e = Σ 1/k! (production reduces by ln 2 and squares);
//   - ln: Halley iteration on that exp (production uses an atanh series);
//   - Φ: (1 + erf(x/√2))/2 with erf's alternating Maclaurin series, carried
//     at extra precision to absorb its cancellation (production uses
//     Marsaglia's positive series);
//   - π: Machin's formula; √: big.Float.Sqrt.
type oracle struct{ prec uint }

// defaultOracle carries 640 bits, about 190 decimal digits: far beyond the
// 40 digits under test, so its own error never decides a comparison.
func defaultOracle() oracle { return oracle{prec: 640} }

func (o oracle) f() *big.Float { return new(big.Float).SetPrec(o.prec) }

func (o oracle) int(n int64) *big.Float { return o.f().SetInt64(n) }

// tiny returns 2^-(prec+16), the series cut-off.
func (o oracle) tiny() *big.Float {
	return o.f().SetMantExp(o.int(1), -int(o.prec)-16)
}

// dec converts a Dec exactly (up to the oracle's precision).
func (o oracle) dec(d Dec) *big.Float {
	return o.f().Quo(o.f().SetInt(d.coef()), o.f().SetInt(unit()))
}

// fixed converts a fixed-point value y·2^-w exactly.
func (o oracle) fixed(y *big.Int, w uint) *big.Float {
	v := new(big.Float).SetPrec(uint(y.BitLen()) + 64).SetInt(y)
	return o.f().SetMantExp(v, -int(w))
}

func (o oracle) expTaylor(x *big.Float) *big.Float {
	sum, term := o.int(1), o.int(1)
	for k := int64(1); ; k++ {
		term = o.f().Quo(o.f().Mul(term, x), o.int(k))
		if o.f().Abs(term).Cmp(o.tiny()) < 0 {
			return sum
		}
		sum = o.f().Add(sum, term)
	}
}

func (o oracle) exp(x *big.Float) *big.Float {
	w := oracle{prec: o.prec + 64}
	xs := w.f().Set(x)
	half := w.f().SetMantExp(w.int(1), -1)
	r := w.f().Add(xs, half)
	if xs.Sign() < 0 {
		r = w.f().Sub(xs, half)
	}
	nb, _ := r.Int(nil) // truncation: round half away from zero overall
	n := nb.Int64()
	frac := w.f().Sub(xs, w.f().SetInt64(n))
	e := w.expTaylor(w.int(1))
	en, base := w.int(1), e
	for m := abs64(n); m > 0; m >>= 1 {
		if m&1 == 1 {
			en = w.f().Mul(en, base)
		}
		base = w.f().Mul(base, base)
	}
	if n < 0 {
		en = w.f().Quo(w.int(1), en)
	}
	return o.f().Mul(en, w.expTaylor(frac))
}

func (o oracle) ln(x *big.Float) *big.Float {
	w := oracle{prec: o.prec + 64}
	xs := w.f().Set(x)
	mant := w.f()
	e2 := xs.MantExp(mant)
	ln2, _ := w.f().SetString("0.6931471805599453094172321214581765680755")
	y := w.f().Mul(w.int(int64(e2)), ln2)
	for range 64 {
		ey := w.exp(y)
		d := w.f().Quo(w.f().Sub(xs, ey), w.f().Add(xs, ey))
		d = w.f().Mul(d, w.int(2))
		y = w.f().Add(y, d)
		if d.Sign() == 0 || w.f().Abs(d).Cmp(w.tiny()) < 0 {
			break
		}
	}
	return o.f().Set(y)
}

func (o oracle) atanInv(n int64) *big.Float {
	x := o.f().Quo(o.int(1), o.int(n))
	x2 := o.f().Mul(x, x)
	sum, term := o.f().Set(x), o.f().Set(x)
	for k := int64(1); ; k++ {
		term = o.f().Mul(term, x2)
		t := o.f().Quo(term, o.int(2*k+1))
		if t.Cmp(o.tiny()) < 0 {
			return sum
		}
		if k%2 == 1 {
			sum = o.f().Sub(sum, t)
		} else {
			sum = o.f().Add(sum, t)
		}
	}
}

func (o oracle) pi() *big.Float {
	w := oracle{prec: o.prec + 32}
	return o.f().Sub(w.f().Mul(w.int(16), w.atanInv(5)), w.f().Mul(w.int(4), w.atanInv(239)))
}

func (o oracle) sqrt(x *big.Float) *big.Float { return o.f().Sqrt(x) }

// erf by its alternating series, at enough extra precision to absorb the
// e^{z²} cancellation.
func (o oracle) erf(z *big.Float) *big.Float {
	z2f, _ := new(big.Float).Mul(z, z).Int64()
	w := oracle{prec: o.prec + uint(2*z2f) + 96}
	zs := w.f().Set(z)
	z2 := w.f().Mul(zs, zs)
	p, sum := w.f().Set(zs), w.f().Set(zs) // p = z^(2n+1)/n!
	for n := int64(1); ; n++ {
		p = w.f().Quo(w.f().Mul(p, z2), w.int(n))
		t := w.f().Quo(p, w.int(2*n+1))
		if n%2 == 1 {
			sum = w.f().Sub(sum, t)
		} else {
			sum = w.f().Add(sum, t)
		}
		if n > 2*z2f+8 && w.f().Abs(t).Cmp(w.tiny()) < 0 {
			break
		}
	}
	return o.f().Quo(w.f().Mul(sum, w.int(2)), w.sqrt(w.pi()))
}

// cdf returns Φ(x). Beyond |x| = 15 it returns 0 or 1: Φ(−15) < 3.7·10^-51,
// negligible for every comparison made here (production's own clamp at 14
// is checked against the unclamped series by the grid and table tests).
func (o oracle) cdf(x *big.Float) *big.Float {
	if x.Cmp(o.int(15)) > 0 {
		return o.int(1)
	}
	if x.Cmp(o.int(-15)) < 0 {
		return o.f()
	}
	z := o.f().Quo(x, o.sqrt(o.int(2)))
	return o.f().Quo(o.f().Add(o.int(1), o.erf(z)), o.int(2))
}

func (o oracle) pdf(x *big.Float) *big.Float {
	u := o.f().Quo(o.f().Mul(x, x), o.int(-2))
	return o.f().Quo(o.exp(u), o.sqrt(o.f().Mul(o.int(2), o.pi())))
}

// bsm evaluates the Black–Scholes–Merton price and Greeks (call and put)
// with the oracle's functions, in the order Price, Delta, Gamma, Vega,
// Theta, Rho.
func (o oracle) bsm(s, k, t, r, q, vol *big.Float) (call, put [6]*big.Float) {
	sqrtT := o.sqrt(t)
	sd := o.f().Mul(vol, sqrtT)
	dfq := o.exp(o.f().Neg(o.f().Mul(q, t)))
	dfr := o.exp(o.f().Neg(o.f().Mul(r, t)))
	m := o.f().Add(o.ln(o.f().Quo(s, k)), o.f().Mul(o.f().Sub(r, q), t))
	d1 := o.f().Add(o.f().Quo(m, sd), o.f().Quo(sd, o.int(2)))
	d2 := o.f().Sub(d1, sd)
	n1, n2 := o.cdf(d1), o.cdf(d2)
	nm1, nm2 := o.cdf(o.f().Neg(d1)), o.cdf(o.f().Neg(d2))
	sq, kr := o.f().Mul(s, dfq), o.f().Mul(k, dfr)
	ph := o.pdf(d1)
	gamma := o.f().Quo(o.f().Mul(dfq, ph), o.f().Mul(s, sd))
	vega := o.f().Mul(o.f().Mul(sq, ph), sqrtT)
	diffusion := o.f().Quo(o.f().Mul(o.f().Mul(sq, ph), vol), o.f().Mul(o.int(2), sqrtT))
	call = [6]*big.Float{
		o.f().Sub(o.f().Mul(sq, n1), o.f().Mul(kr, n2)),
		o.f().Mul(dfq, n1),
		gamma,
		vega,
		o.f().Add(o.f().Neg(diffusion), o.f().Sub(o.f().Mul(q, o.f().Mul(sq, n1)), o.f().Mul(r, o.f().Mul(kr, n2)))),
		o.f().Mul(t, o.f().Mul(kr, n2)),
	}
	put = [6]*big.Float{
		o.f().Sub(o.f().Mul(kr, nm2), o.f().Mul(sq, nm1)),
		o.f().Neg(o.f().Mul(dfq, nm1)),
		gamma,
		vega,
		o.f().Add(o.f().Neg(diffusion), o.f().Sub(o.f().Mul(r, o.f().Mul(kr, nm2)), o.f().Mul(q, o.f().Mul(sq, nm1)))),
		o.f().Neg(o.f().Mul(t, o.f().Mul(kr, nm2))),
	}
	return call, put
}

func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// nearestCoef returns the Dec coefficient nearest to v (v·10^40 rounded to
// the nearest integer) and whether the oracle can decide it, i.e. v·10^40
// is not within 2^-(prec/2) of a midpoint.
func nearestCoef(v *big.Float) (*big.Int, bool) {
	p := v.Prec() + 160
	s := new(big.Float).SetPrec(p).Mul(v, new(big.Float).SetPrec(p).SetInt(unit()))
	neg := s.Sign() < 0
	s.Abs(s)
	ip, _ := s.Int(nil)
	frac := new(big.Float).SetPrec(p).Sub(s, new(big.Float).SetPrec(p).SetInt(ip))
	one := new(big.Float).SetPrec(p).SetInt64(1)
	half := new(big.Float).SetPrec(p).SetMantExp(one, -1)
	dist := new(big.Float).SetPrec(p).Sub(frac, half)
	margin := new(big.Float).SetPrec(p).SetMantExp(one, -int(v.Prec()/2))
	clear := new(big.Float).Abs(dist).Cmp(margin) > 0
	if frac.Cmp(half) >= 0 {
		ip.Add(ip, big.NewInt(1))
	}
	if neg {
		ip.Neg(ip)
	}
	return ip, clear
}

// tb is the part of testing.TB that *rapid.T also implements.
type tb interface {
	Helper()
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
}

var _ tb = (testing.TB)(nil)

// requireCorrectlyRounded fails unless got is the Dec nearest to want.
func requireCorrectlyRounded(t tb, label string, got Dec, want *big.Float) {
	t.Helper()
	wc, clear := nearestCoef(want)
	if !clear {
		t.Logf("%s: oracle value within 2^-%d of a rounding midpoint; skipped", label, want.Prec()/2)
		return
	}
	if got.coef().Cmp(wc) != 0 {
		t.Fatalf("%s: got %s, want %s (oracle %s)", label, got, Dec{c: wc}, want.Text('g', 60))
	}
}

// absDiff returns |got − want| as a big.Float.
func absDiff(got Dec, want *big.Float) *big.Float {
	o := oracle{prec: want.Prec()}
	return o.f().Abs(o.f().Sub(o.dec(got), want))
}

// requireWithin fails unless |got − want| <= tol.
func requireWithin(t tb, label string, got Dec, want *big.Float, tol Dec) {
	t.Helper()
	o := oracle{prec: want.Prec()}
	if d := absDiff(got, want); d.Cmp(o.dec(tol)) > 0 {
		t.Fatalf("%s: got %s, want %s, |diff| = %s > %s", label, got, want.Text('f', 45), d.Text('g', 6), tol)
	}
}
