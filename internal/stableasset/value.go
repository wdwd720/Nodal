package stableasset

import (
	"math/big"
	"strings"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// Value is an exact amount of whole units of a stable asset's peg quote
// asset: Coef × 10^Exp. It is never rounded; the reserve invariant is
// decided on Values, and rounding happens only when a Value is rendered as
// USD for a risk input or a report, with a named rounding mode.
type Value struct {
	Coef money.Quantity
	Exp  int32
}

// unitsValue is q base units of an asset with decimals, in whole units.
func unitsValue(q money.Quantity, decimals uint8) Value {
	return Value{Coef: q, Exp: -int32(decimals)}
}

// times multiplies by a price (quote whole units per whole unit).
func (v Value) times(p money.Price) Value {
	return Value{Coef: v.Coef.Mul(p.Mantissa), Exp: v.Exp - p.Scale}
}

// timesBPS multiplies by a basis-point factor (10000 = 1).
func (v Value) timesBPS(b money.BPS) Value {
	return Value{Coef: v.Coef.Mul(money.QuantityFromInt64(int64(b))), Exp: v.Exp - 4}
}

func pow10(n int32) money.Quantity {
	if n <= 0 {
		return money.QuantityFromInt64(1)
	}
	return money.QuantityFromBigInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil))
}

// align returns both coefficients at the smaller exponent.
func align(a, b Value) (money.Quantity, money.Quantity, int32) {
	switch {
	case a.Exp == b.Exp:
		return a.Coef, b.Coef, a.Exp
	case a.Exp > b.Exp:
		return a.Coef.Mul(pow10(a.Exp - b.Exp)), b.Coef, b.Exp
	default:
		return a.Coef, b.Coef.Mul(pow10(b.Exp - a.Exp)), a.Exp
	}
}

// Add returns v + o.
func (v Value) Add(o Value) Value {
	a, b, exp := align(v, o)
	return Value{Coef: a.Add(b), Exp: exp}
}

// Sub returns v - o (which may be negative).
func (v Value) Sub(o Value) Value {
	a, b, exp := align(v, o)
	return Value{Coef: a.Sub(b), Exp: exp}
}

// Cmp compares exactly.
func (v Value) Cmp(o Value) int {
	a, b, _ := align(v, o)
	return a.Cmp(b)
}

// IsZero reports whether the value is zero.
func (v Value) IsZero() bool { return v.Coef.IsZero() }

// String renders the exact decimal, e.g. "1234.500000".
func (v Value) String() string {
	if v.Exp >= 0 {
		return v.Coef.Mul(pow10(v.Exp)).String()
	}
	if -v.Exp > 255 {
		return v.Coef.String() + "e" + money.QuantityFromInt64(int64(v.Exp)).String()
	}
	return v.Coef.ToDecimalString(uint8(-v.Exp))
}

// MarshalText renders String, so a Value is a string in canonical JSON.
func (v Value) MarshalText() ([]byte, error) { return []byte(v.String()), nil }

// ToUSD renders the value as USD cents, treating one whole quote unit as one
// dollar, rounding in mode. It fails with OVERFLOW beyond the int64 range.
func (v Value) ToUSD(mode money.RoundingMode) (money.USD, error) {
	cents := Value{Coef: v.Coef, Exp: v.Exp + 2}
	var minor money.Quantity
	if cents.Exp >= 0 {
		minor = cents.Coef.Mul(pow10(cents.Exp))
	} else {
		q, err := cents.Coef.Div(pow10(-cents.Exp), mode)
		if err != nil {
			return money.USD{}, err
		}
		minor = q
	}
	n, err := minor.Int64()
	if err != nil {
		return money.USD{}, errs.New(errs.CodeOverflow, "stableasset: value exceeds the USD range")
	}
	return money.USDFromMinor(n), nil
}

// ratioBPS returns floor(num / den × 10000), the coverage of den by num. A
// zero denominator is full coverage by definition (nothing to cover).
func ratioBPS(num, den Value) money.BPS {
	if den.Coef.Sign() <= 0 {
		return money.OneHundredPercent
	}
	if num.Coef.Sign() <= 0 {
		return 0
	}
	a, b, _ := align(num, den)
	q, err := a.Mul(money.QuantityFromInt64(10_000)).Div(b, money.RoundFloor)
	if err != nil {
		return 0
	}
	n, err := q.Int64()
	if err != nil {
		return money.BPS(1 << 62)
	}
	return money.BPS(n)
}

// deviationBPS returns ceil(|price - peg| / peg × 10000): the peg deviation,
// rounded against the asset so a deviation is never understated.
func deviationBPS(price, peg money.Price) money.BPS {
	p := Value{Coef: price.Mantissa, Exp: -price.Scale}
	g := Value{Coef: peg.Mantissa, Exp: -peg.Scale}
	diff := p.Sub(g)
	if diff.Coef.IsNegative() {
		diff.Coef = diff.Coef.Neg()
	}
	if g.Coef.Sign() <= 0 {
		return money.OneHundredPercent
	}
	a, b, _ := align(diff, g)
	q, err := a.Mul(money.QuantityFromInt64(10_000)).Div(b, money.RoundCeil)
	if err != nil {
		return money.OneHundredPercent
	}
	n, err := q.Int64()
	if err != nil {
		return money.BPS(1 << 62)
	}
	return money.BPS(n)
}

// parPrice is one quote unit per unit.
func parPrice(like money.Price) money.Price {
	return money.Price{Mantissa: money.QuantityFromInt64(1), Scale: 0, QuoteAsset: like.QuoteAsset, Source: like.Source, At: like.At}
}

// capAtPar values a cash equivalent at the lower of its price and par: a
// reserve dollar is never worth more than a dollar to the coverage test.
func capAtPar(p money.Price) money.Price {
	if p.Mantissa.Cmp(pow10(p.Scale)) > 0 {
		return parPrice(p)
	}
	return p
}

// requiredReserve is the smallest quantity r of a reserve asset (base units,
// reserveDecimals) whose value at price and factor covers q base units of the
// stable asset (stableDecimals) at the target peg:
//
//	r × 10^-reserveDecimals × price × factor/10000 >= q × 10^-stableDecimals × peg
//
// rounded up, so an issuance never under-backs by a rounding remainder.
func requiredReserve(q money.Quantity, stableDecimals uint8, peg money.Price, reserveDecimals uint8, price money.Price, factor money.BPS) (money.Quantity, error) {
	if factor <= 0 || factor > money.OneHundredPercent {
		return money.Quantity{}, errs.New(errs.CodeValidationFailed, "stableasset: an eligibility factor must be in (0, 10000]")
	}
	if price.Mantissa.Sign() <= 0 {
		return money.Quantity{}, errs.New(errs.CodeStaleMarketData, "stableasset: a reserve price of zero cannot back anything")
	}
	target := unitsValue(q, stableDecimals).times(peg)
	unit := unitsValue(money.QuantityFromInt64(1), reserveDecimals).times(price).timesBPS(factor)
	num, den := target.Coef, unit.Coef
	if d := target.Exp - unit.Exp; d >= 0 {
		num = num.Mul(pow10(d))
	} else {
		den = den.Mul(pow10(-d))
	}
	r, err := num.Div(den, money.RoundCeil)
	if err != nil {
		return money.Quantity{}, err
	}
	if unitsValue(r, reserveDecimals).times(price).timesBPS(factor).Cmp(target) < 0 {
		return money.Quantity{}, errs.New(errs.CodeInternal, "stableasset: required reserve does not cover its target")
	}
	return r, nil
}

// parPayout is what redeeming q base units pays in a cash-equivalent reserve
// asset at par: floor(q × 10^-stableDecimals × peg × 10^reserveDecimals). It
// rounds down, so a redemption never pays out more than the peg value of
// what it retires (the database checks the same inequality, SV005).
func parPayout(q money.Quantity, stableDecimals uint8, peg money.Price, reserveDecimals uint8) (money.Quantity, error) {
	v := unitsValue(q, stableDecimals).times(peg)
	exp := v.Exp + int32(reserveDecimals)
	if exp >= 0 {
		return v.Coef.Mul(pow10(exp)), nil
	}
	return v.Coef.Div(pow10(-exp), money.RoundFloor)
}

// parseDecimalValue parses the exact decimal String renders (used when a
// stored snapshot is verified).
func parseDecimalValue(s string) (Value, error) {
	neg := strings.HasPrefix(s, "-")
	body := strings.TrimPrefix(s, "-")
	intPart, frac, hasPoint := strings.Cut(body, ".")
	if intPart == "" || (hasPoint && frac == "") {
		return Value{}, errs.New(errs.CodeValidationFailed, "stableasset: not a decimal")
	}
	digits := intPart + frac
	if neg {
		digits = "-" + digits
	}
	q, err := money.ParseQuantity(digits)
	if err != nil {
		return Value{}, errs.Wrap(err, errs.CodeValidationFailed, "stableasset: not a decimal")
	}
	return Value{Coef: q, Exp: -int32(len(frac))}, nil
}
