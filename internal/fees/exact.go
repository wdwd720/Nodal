package fees

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// bpsScale is the number of decimal places one basis point adds: a rate of
// b basis points is exactly b × 10^-4.
const bpsScale = 4

// MaxLabelLength bounds every free-text identifier this package records
// (schedule versions, terms versions, account and strategy references).
const MaxLabelLength = 128

// rational is an exact rational number num/den with den > 0. Every amount in
// this package is first formed as a rational from integer inputs and then
// rounded exactly once, in a named money.RoundingMode, so that no value is
// ever the product of two roundings and the exact amount it approximates can
// always be recomputed by an auditor.
type rational struct {
	num money.Quantity
	den money.Quantity
}

// bpsRatio returns b / 10_000 as an exact rational.
func bpsRatio(b money.BPS) rational {
	return rational{num: money.QuantityFromInt64(int64(b)), den: money.QuantityFromInt64(int64(money.OneHundredPercent))}
}

// mulBPS returns r × b / 10_000 exactly.
func (r rational) mulBPS(b money.BPS) rational {
	f := bpsRatio(b)
	return rational{num: r.num.Mul(f.num), den: r.den.Mul(f.den)}
}

// round returns r rounded to an integer in mode.
func (r rational) round(mode money.RoundingMode) (money.Quantity, error) {
	return r.num.Div(r.den, mode)
}

// lessThan reports whether r < q.
func (r rational) lessThan(q money.Quantity) bool {
	return r.num.Cmp(q.Mul(r.den)) < 0
}

// greaterThan reports whether r > q.
func (r rational) greaterThan(q money.Quantity) bool {
	return r.num.Cmp(q.Mul(r.den)) > 0
}

// withinOneUnit reports whether |q − r| < 1, the bound every single rounding
// step satisfies whatever its mode: q − 1 < r < q + 1.
func (r rational) withinOneUnit(q money.Quantity) bool {
	one := money.QuantityFromInt64(1)
	return r.greaterThan(q.Sub(one)) && r.lessThan(q.Add(one))
}

// pow10 returns 10^n. n is always derived from uint8 decimals and int32
// price scales, so it is small and non-negative.
func pow10(n int) money.Quantity {
	return money.QuantityFromBigInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil))
}

// trimTrailingZeros removes trailing decimal zeros from mantissa × 10^-scale
// while the scale stays at or above minScale. The value is unchanged.
func trimTrailingZeros(mantissa money.Quantity, scale, minScale int32) (money.Quantity, int32) {
	v := mantissa.BigInt()
	ten := big.NewInt(10)
	for scale > minScale {
		q, r := new(big.Int).QuoRem(v, ten, new(big.Int))
		if r.Sign() != 0 {
			break
		}
		v = q
		scale--
	}
	return money.QuantityFromBigInt(v), scale
}

// usdMulDiv returns u × num / den rounded in mode, with the product formed
// exactly. It fails with OVERFLOW when the result leaves the USD range.
func usdMulDiv(u money.USD, num, den money.Quantity, mode money.RoundingMode) (money.USD, error) {
	r, err := money.QuantityFromInt64(u.Minor()).MulDiv(num, den, mode)
	if err != nil {
		return money.USD{}, arithmeticError(err)
	}
	minor, err := r.Int64()
	if err != nil {
		return money.USD{}, arithmeticError(err)
	}
	return money.USDFromMinor(minor), nil
}

// arithmeticError maps a money sentinel onto the stable errs code an API
// boundary reports for it, preserving the cause.
func arithmeticError(err error) error {
	if _, ok := errs.As(err); ok {
		return err
	}
	switch {
	case errors.Is(err, money.ErrOverflow):
		return errs.Wrap(err, errs.CodeOverflow, "fees: amount outside the representable range")
	case errors.Is(err, money.ErrPrecisionLoss):
		return errs.Wrap(err, errs.CodePrecisionLoss, "fees: amount cannot be represented exactly")
	default:
		return errs.Wrap(err, errs.CodeValidationFailed, "fees: arithmetic on invalid input")
	}
}

// fieldErrors collects per-field problems in the shape every validator in
// this package returns: a VALIDATION_FAILED *errs.Error whose Fields name
// each offending field once (the first message wins).
type fieldErrors map[string]any

func (f fieldErrors) add(key, msg string) {
	if _, dup := f[key]; !dup {
		f[key] = msg
	}
}

func (f fieldErrors) addf(key, format string, a ...any) {
	f.add(key, fmt.Sprintf(format, a...))
}

func (f fieldErrors) err(detail string) error {
	if len(f) == 0 {
		return nil
	}
	return errs.New(errs.CodeValidationFailed, detail).WithFields(f)
}

// checkLabel validates a required recorded identifier: at most
// MaxLabelLength bytes of printable UTF-8 without surrounding whitespace.
func checkLabel(f fieldErrors, key, v string) {
	switch {
	case v == "":
		f.add(key, "required")
	case len(v) > MaxLabelLength:
		f.addf(key, "must be at most %d bytes", MaxLabelLength)
	case strings.TrimSpace(v) != v:
		f.add(key, "must not have leading or trailing whitespace")
	case !utf8.ValidString(v) || strings.IndexFunc(v, unicode.IsControl) >= 0:
		f.add(key, "must be printable UTF-8")
	}
}

// checkBPS reports a rate outside [lo, hi].
func checkBPS(f fieldErrors, key string, b, lo, hi money.BPS) {
	if b < lo || b > hi {
		f.addf(key, "must be within %d..%d basis points", lo, hi)
	}
}
