package prediction

import (
	"math/big"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/strategy/ir"
)

// Scoring is exact fixed-point arithmetic on big.Int. There is no float64
// anywhere in this file: a calibration number that differs by platform or by
// compiler is not evidence.

// workScale is the internal precision of the logarithm. It is far above the
// scale-8 output so the final rounding is the only rounding.
const workScale = 30

// ln2Scaled is ln(2) at workScale, i.e. floor(ln(2) * 10^30).
// ln 2 = 0.693147180559945309417232121458176568...
var ln2Scaled, _ = new(big.Int).SetString("693147180559945309417232121458", 10)

// DefaultEpsilon bounds the log loss. A probability of exactly 0 or 1 would
// otherwise make the loss infinite, and one over-confident prediction would
// destroy every aggregate it appears in. 10^-6 is the smallest value the
// scale-6 probability column can express, so the bound costs no resolution.
func DefaultEpsilon() ir.Decimal {
	return ir.Decimal{Mantissa: "1", Scale: ProbabilityScale}
}

// Brier returns the Brier score (p - o)^2 of a probability against a boolean
// outcome, as a scale-8 decimal in [0, 1]. Lower is better. It is exact: the
// square of a scale-6 decimal is a scale-12 decimal, and the only rounding is
// the final widening down to scale 8, half-even.
func Brier(p ir.Decimal, outcome bool) (ir.Decimal, error) {
	pv, one, err := probabilityMantissa(p)
	if err != nil {
		return ir.Decimal{}, err
	}
	diff := new(big.Int).Set(pv)
	if outcome {
		diff.Sub(pv, one)
	}
	sq := new(big.Int).Mul(diff, diff) // scale 2*p.Scale
	sqScale := 2 * int(p.Scale)
	return rescaleInt(sq, sqScale, int(ScoreScale), money.RoundHalfEven)
}

// LogLoss returns -ln(q) where q is the probability the prediction assigned
// to what actually happened, as a scale-8 decimal >= 0. Lower is better.
//
// q is clamped into [eps, 1-eps] before the logarithm, so the result is
// bounded by -ln(eps) and a single certain-and-wrong prediction cannot make
// an aggregate infinite. eps must be a probability strictly between 0 and
// 1/2; DefaultEpsilon supplies the conventional value.
func LogLoss(p ir.Decimal, outcome bool, eps ir.Decimal) (ir.Decimal, error) {
	pv, one, err := probabilityMantissa(p)
	if err != nil {
		return ir.Decimal{}, err
	}
	ev, eOne, err := probabilityMantissa(eps)
	if err != nil {
		return ir.Decimal{}, errs.Wrap(err, errs.CodeValidationFailed, "prediction: invalid epsilon")
	}
	// eps must be in (0, 1/2).
	half := new(big.Int).Div(eOne, big.NewInt(2))
	if ev.Sign() <= 0 || ev.Cmp(half) >= 0 {
		return ir.Decimal{}, errs.New(errs.CodeValidationFailed, "prediction: epsilon must be strictly between 0 and 0.5")
	}

	// q is the probability assigned to the realized outcome.
	q := new(big.Int).Set(pv)
	if !outcome {
		q.Sub(one, pv)
	}
	// Bring q and eps onto the same scale before clamping.
	qScaled, qScale := q, int(p.Scale)
	epsScaled, err := scaleTo(ev, int(eps.Scale), qScale)
	if err != nil {
		return ir.Decimal{}, err
	}
	oneAtScale, err := scaleTo(one, int(p.Scale), qScale)
	if err != nil {
		return ir.Decimal{}, err
	}
	upper := new(big.Int).Sub(oneAtScale, epsScaled)
	if qScaled.Cmp(epsScaled) < 0 {
		qScaled = epsScaled
	}
	if qScaled.Cmp(upper) > 0 {
		qScaled = upper
	}

	lnQ, err := lnFixed(qScaled, qScale)
	if err != nil {
		return ir.Decimal{}, err
	}
	loss := new(big.Int).Neg(lnQ) // ln q <= 0, so -ln q >= 0
	if loss.Sign() < 0 {
		loss.SetInt64(0)
	}
	return rescaleInt(loss, workScale, int(ScoreScale), money.RoundHalfEven)
}

// MaxLogLoss is the largest value LogLoss can return for a given epsilon:
// -ln(eps). Reporting it alongside an aggregate makes the bound visible
// instead of implicit.
func MaxLogLoss(eps ir.Decimal) (ir.Decimal, error) {
	ev, eOne, err := probabilityMantissa(eps)
	if err != nil {
		return ir.Decimal{}, err
	}
	half := new(big.Int).Div(eOne, big.NewInt(2))
	if ev.Sign() <= 0 || ev.Cmp(half) >= 0 {
		return ir.Decimal{}, errs.New(errs.CodeValidationFailed, "prediction: epsilon must be strictly between 0 and 0.5")
	}
	lnEps, err := lnFixed(ev, int(eps.Scale))
	if err != nil {
		return ir.Decimal{}, err
	}
	return rescaleInt(new(big.Int).Neg(lnEps), workScale, int(ScoreScale), money.RoundHalfEven)
}

// probabilityMantissa returns (mantissa, 10^scale) for a validated decimal in
// [0, 1].
func probabilityMantissa(d ir.Decimal) (value, one *big.Int, err error) {
	if isUnset(d) {
		return nil, nil, errs.New(errs.CodeValidationFailed, "prediction: probability is required")
	}
	v, ierr := d.Int()
	if ierr != nil {
		return nil, nil, errs.Wrap(ierr, errs.CodeValidationFailed, "prediction: invalid probability")
	}
	unit := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(d.Scale)), nil)
	if v.Sign() < 0 || v.Cmp(unit) > 0 {
		return nil, nil, errs.Newf(errs.CodeValidationFailed, "prediction: probability %s is outside [0, 1]", d.String())
	}
	return v, unit, nil
}

// scaleTo widens an integer from one decimal scale to a larger one. Widening
// is exact; narrowing is refused here because it would round silently.
func scaleTo(v *big.Int, from, to int) (*big.Int, error) {
	switch {
	case from == to:
		return new(big.Int).Set(v), nil
	case from > to:
		return nil, errs.Newf(errs.CodePrecisionLoss, "prediction: cannot narrow scale %d to %d without rounding", from, to)
	default:
		mul := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(to-from)), nil)
		return new(big.Int).Mul(v, mul), nil
	}
}

// rescaleInt converts an integer at scale from into a Decimal at scale to,
// rounding with the named mode when it must narrow.
func rescaleInt(v *big.Int, from, to int, mode money.RoundingMode) (ir.Decimal, error) {
	// Only the destination scale ever lands on a Decimal. The source scale is
	// the internal working precision, which is deliberately far above what a
	// Decimal may carry, so validating it here would refuse every conversion
	// out of the logarithm.
	toScale, err := decimalScale(to)
	if err != nil {
		return ir.Decimal{}, err
	}
	if from == to {
		d := ir.Decimal{Mantissa: v.String(), Scale: toScale}
		return d, d.Validate()
	}
	if from < to {
		out, serr := scaleTo(v, from, to)
		if serr != nil {
			return ir.Decimal{}, serr
		}
		res := ir.Decimal{Mantissa: out.String(), Scale: toScale}
		return res, res.Validate()
	}
	den := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(from-to)), nil)
	q, derr := divRound(v, den, mode)
	if derr != nil {
		return ir.Decimal{}, derr
	}
	res := ir.Decimal{Mantissa: q.String(), Scale: toScale}
	return res, res.Validate()
}

// decimalScale narrows an internal working scale onto the uint8 a Decimal
// carries. A scale outside the IR's declared range is a programming error and
// is refused rather than wrapped around.
func decimalScale(s int) (uint8, error) {
	if s < 0 || s > ir.MaxScale {
		return 0, errs.Newf(errs.CodeInternal, "prediction: scale %d is outside 0..%d", s, ir.MaxScale)
	}
	return uint8(s), nil
}

// divRound divides num by den (den > 0) with the given rounding mode. Only
// the modes the ledger uses are implemented; anything else is refused rather
// than silently approximated.
func divRound(num, den *big.Int, mode money.RoundingMode) (*big.Int, error) {
	if den.Sign() <= 0 {
		return nil, errs.New(errs.CodeInternal, "prediction: non-positive divisor")
	}
	q, r := new(big.Int).QuoRem(num, den, new(big.Int))
	if r.Sign() == 0 {
		return q, nil
	}
	neg := num.Sign() < 0
	absR := new(big.Int).Abs(r)
	twice := new(big.Int).Lsh(absR, 1)
	cmp := twice.Cmp(den)
	step := big.NewInt(1)
	if neg {
		step = big.NewInt(-1)
	}
	switch mode {
	case money.RoundDown:
		return q, nil
	case money.RoundUp:
		return q.Add(q, step), nil
	case money.RoundHalfUp:
		if cmp >= 0 {
			return q.Add(q, step), nil
		}
		return q, nil
	case money.RoundHalfEven:
		switch {
		case cmp > 0:
			return q.Add(q, step), nil
		case cmp < 0:
			return q, nil
		default:
			if q.Bit(0) == 1 {
				return q.Add(q, step), nil
			}
			return q, nil
		}
	case money.RoundFloor:
		if neg {
			return q.Add(q, big.NewInt(-1)), nil
		}
		return q, nil
	case money.RoundCeil:
		if neg {
			return q, nil
		}
		return q.Add(q, big.NewInt(1)), nil
	default:
		return nil, errs.Newf(errs.CodeUnsupported, "prediction: rounding mode %v is not supported here", mode)
	}
}

// lnFixed returns ln(x) at workScale, where x = v / 10^scale and 0 < x <= 1.
//
// It range-reduces x into [1/2, 1] by doubling, so ln(x) = ln(m) - k*ln(2),
// then evaluates the inverse-hyperbolic-tangent series
//
//	ln(m) = 2 * (z + z^3/3 + z^5/5 + ...),  z = (m-1)/(m+1)
//
// which converges geometrically with ratio z^2 <= 1/9 on that interval, so
// about 32 terms reach 10^-30. Every step is integer arithmetic.
func lnFixed(v *big.Int, scale int) (*big.Int, error) {
	if v.Sign() <= 0 {
		return nil, errs.New(errs.CodeValidationFailed, "prediction: logarithm of a non-positive probability")
	}
	x, err := scaleTo(v, scale, workScale)
	if err != nil {
		return nil, err
	}
	one := new(big.Int).Exp(big.NewInt(10), big.NewInt(workScale), nil)
	if x.Cmp(one) > 0 {
		return nil, errs.New(errs.CodeValidationFailed, "prediction: logarithm above 1 is not used here")
	}
	if x.Cmp(one) == 0 {
		return big.NewInt(0), nil
	}
	half := new(big.Int).Div(one, big.NewInt(2))
	k := 0
	m := new(big.Int).Set(x)
	for m.Cmp(half) < 0 {
		m.Lsh(m, 1)
		k++
		if k > 4096 {
			return nil, errs.New(errs.CodeInternal, "prediction: logarithm range reduction did not terminate")
		}
	}
	// z = (m - 1) / (m + 1), at workScale.
	num := new(big.Int).Sub(m, one)
	den := new(big.Int).Add(m, one)
	z := new(big.Int).Mul(num, one)
	z.Quo(z, den)

	// series: sum z^(2i+1) / (2i+1)
	z2 := new(big.Int).Mul(z, z)
	z2.Quo(z2, one)
	term := new(big.Int).Set(z)
	sum := new(big.Int).Set(z)
	for i := 1; i < 64; i++ {
		term.Mul(term, z2)
		term.Quo(term, one)
		if term.Sign() == 0 {
			break
		}
		part := new(big.Int).Quo(term, big.NewInt(int64(2*i+1)))
		if part.Sign() == 0 {
			break
		}
		sum.Add(sum, part)
	}
	lnM := sum.Lsh(sum, 1) // 2 * series

	// ln(x) = ln(m) - k*ln(2)
	kLn2 := new(big.Int).Mul(big.NewInt(int64(k)), ln2Scaled)
	return lnM.Sub(lnM, kLn2), nil
}
