package money

import (
	"fmt"
	"math/big"
	"strings"
)

// MaxQuantityDigits bounds the magnitude of a Quantity accepted by the
// parsers (ParseQuantity, QuantityFromDecimalString, ScanQuantity and the
// JSON/text decoders): |value| must be below 10^MaxQuantityDigits, otherwise
// ErrOverflow is returned. 128 digits is well above any on-chain integer
// (a 256-bit unsigned maximum has 78 digits) and above the NUMERIC(38,0)
// columns used for persistence, which remain the effective storage bound.
// Arithmetic results are not bounded.
const MaxQuantityDigits = 128

// Quantity is an exact, arbitrary-precision integer number of asset base
// units (lamports, wei, micro-USDC, shares, ...). It carries no asset
// identity and no decimal point; the owner of the value knows both.
//
// Quantity has immutable value semantics: every method returns a new value
// and never modifies its receiver or arguments, so Quantities may be copied
// and shared freely. The zero value is 0 and is fully usable.
//
// Do not compare Quantities with ==; two equal values may hold distinct
// pointers. Use Equal or Cmp.
type Quantity struct{ v *big.Int }

// QuantityFromInt64 constructs a Quantity from an int64.
func QuantityFromInt64(n int64) Quantity { return Quantity{v: big.NewInt(n)} }

// QuantityFromBigInt constructs a Quantity from a copy of b. A nil b is 0.
func QuantityFromBigInt(b *big.Int) Quantity {
	if b == nil {
		return Quantity{}
	}
	return Quantity{v: new(big.Int).Set(b)}
}

// ParseQuantity parses a decimal integer: ASCII digits with an optional
// leading "-". Leading zeros are accepted. Anything else ("+1", "1.0",
// "1e3", whitespace, separators, empty) fails with ErrInvalidFormat;
// magnitudes of MaxQuantityDigits digits or more fail with ErrOverflow.
func ParseQuantity(s string) (Quantity, error) {
	if s == "" {
		return Quantity{}, fmt.Errorf("%w: empty string", ErrInvalidFormat)
	}
	if len(s) > maxDecimalInputLen {
		return Quantity{}, fmt.Errorf("%w: input longer than %d bytes", ErrInvalidFormat, maxDecimalInputLen)
	}
	digits := s
	negative := false
	if s[0] == '-' {
		negative = true
		digits = s[1:]
	}
	if digits == "" {
		return Quantity{}, fmt.Errorf("%w: expected a digit", ErrInvalidFormat)
	}
	for i := 0; i < len(digits); i++ {
		if !isASCIIDigit(digits[i]) {
			return Quantity{}, fmt.Errorf("%w: unexpected character at byte %d", ErrInvalidFormat, len(s)-len(digits)+i)
		}
	}
	v, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		// Unreachable: every byte was checked above.
		return Quantity{}, fmt.Errorf("%w: not a decimal integer", ErrInvalidFormat)
	}
	if negative {
		v.Neg(v)
	}
	if err := checkQuantityMagnitude(v); err != nil {
		return Quantity{}, err
	}
	return Quantity{v: v}, nil
}

// checkQuantityMagnitude enforces MaxQuantityDigits on parsed values.
func checkQuantityMagnitude(v *big.Int) error {
	if v.CmpAbs(pow10(MaxQuantityDigits)) >= 0 {
		return fmt.Errorf("%w: magnitude has %d or more digits", ErrOverflow, MaxQuantityDigits)
	}
	return nil
}

// ref returns the underlying integer for read-only use. Callers must never
// mutate the result; the zero value is materialized as a fresh 0.
func (q Quantity) ref() *big.Int {
	if q.v == nil {
		return new(big.Int)
	}
	return q.v
}

// Add returns q + o.
func (q Quantity) Add(o Quantity) Quantity {
	return Quantity{v: new(big.Int).Add(q.ref(), o.ref())}
}

// Sub returns q - o.
func (q Quantity) Sub(o Quantity) Quantity {
	return Quantity{v: new(big.Int).Sub(q.ref(), o.ref())}
}

// Mul returns q × o exactly.
func (q Quantity) Mul(o Quantity) Quantity {
	return Quantity{v: new(big.Int).Mul(q.ref(), o.ref())}
}

// Neg returns -q. Unlike USD this can never overflow.
func (q Quantity) Neg() Quantity {
	return Quantity{v: new(big.Int).Neg(q.ref())}
}

// Abs returns |q|.
func (q Quantity) Abs() Quantity {
	return Quantity{v: new(big.Int).Abs(q.ref())}
}

// Div returns q / o rounded in the given mode. A zero o fails with
// ErrDivisionByZero; RoundExact fails with ErrPrecisionLoss when o does not
// divide q.
func (q Quantity) Div(o Quantity, mode RoundingMode) (Quantity, error) {
	r, err := divRound(q.ref(), o.ref(), mode)
	if err != nil {
		return Quantity{}, err
	}
	return Quantity{v: r}, nil
}

// MulDiv returns q × num / den with the product formed exactly and a single
// rounding step in the given mode.
func (q Quantity) MulDiv(num, den Quantity, mode RoundingMode) (Quantity, error) {
	prod := new(big.Int).Mul(q.ref(), num.ref())
	r, err := divRound(prod, den.ref(), mode)
	if err != nil {
		return Quantity{}, err
	}
	return Quantity{v: r}, nil
}

// MulBPSChecked returns q × bps / 10_000 rounded in the given mode.
// It is the error-returning form of MulBPS and the only one that supports
// RoundExact.
func (q Quantity) MulBPSChecked(bps BPS, mode RoundingMode) (Quantity, error) {
	return q.MulDiv(QuantityFromInt64(int64(bps)), QuantityFromInt64(int64(OneHundredPercent)), mode)
}

// MulBPS returns q × bps / 10_000 rounded in the given mode.
//
// Because the contract signature has no error return, MulBPS panics on the
// two programmer errors it cannot report: an invalid mode, and RoundExact
// (whose outcome would depend on data). Both are independent of the values
// involved, so MulBPS never panics on data; use MulBPSChecked when RoundExact
// is wanted.
func (q Quantity) MulBPS(bps BPS, mode RoundingMode) Quantity {
	if err := mode.validate(); err != nil {
		panic("money: Quantity.MulBPS: " + err.Error())
	}
	if mode == RoundExact {
		panic("money: Quantity.MulBPS does not support RoundExact; use MulBPSChecked")
	}
	r, err := q.MulBPSChecked(bps, mode)
	if err != nil {
		// Unreachable: the mode is valid and non-exact and 10_000 is non-zero.
		panic("money: Quantity.MulBPS: " + err.Error())
	}
	return r
}

// ScaleUp returns q × 10^decimals exactly.
func (q Quantity) ScaleUp(decimals uint8) Quantity {
	if decimals == 0 {
		return q
	}
	return Quantity{v: new(big.Int).Mul(q.ref(), pow10(int(decimals)))}
}

// ScaleDown returns q / 10^decimals rounded in the given mode.
func (q Quantity) ScaleDown(decimals uint8, mode RoundingMode) (Quantity, error) {
	r, err := divRound(q.ref(), pow10(int(decimals)), mode)
	if err != nil {
		return Quantity{}, err
	}
	return Quantity{v: r}, nil
}

// Cmp compares q and o and returns -1, 0 or +1.
func (q Quantity) Cmp(o Quantity) int { return q.ref().Cmp(o.ref()) }

// Equal reports whether q and o have the same value.
func (q Quantity) Equal(o Quantity) bool { return q.Cmp(o) == 0 }

// Sign returns -1, 0 or +1.
func (q Quantity) Sign() int { return q.ref().Sign() }

// IsZero reports whether q is exactly zero.
func (q Quantity) IsZero() bool { return q.Sign() == 0 }

// IsNegative reports whether q is below zero.
func (q Quantity) IsNegative() bool { return q.Sign() < 0 }

// IsPositive reports whether q is above zero.
func (q Quantity) IsPositive() bool { return q.Sign() > 0 }

// Max returns the larger of q and o.
func (q Quantity) Max(o Quantity) Quantity {
	if q.Cmp(o) >= 0 {
		return q
	}
	return o
}

// Min returns the smaller of q and o.
func (q Quantity) Min(o Quantity) Quantity {
	if q.Cmp(o) <= 0 {
		return q
	}
	return o
}

// String renders q as a plain decimal integer with an optional leading "-".
// The output round-trips through ParseQuantity (within MaxQuantityDigits).
func (q Quantity) String() string { return q.ref().String() }

// Int64 returns q as an int64, or ErrOverflow if it does not fit.
func (q Quantity) Int64() (int64, error) {
	v := q.ref()
	if !v.IsInt64() {
		return 0, fmt.Errorf("%w: %s does not fit int64", ErrOverflow, v)
	}
	return v.Int64(), nil
}

// BigInt returns a copy of the underlying integer. Mutating the copy does
// not affect q.
func (q Quantity) BigInt() *big.Int { return new(big.Int).Set(q.ref()) }

// ToDecimalString renders q as a human-readable decimal with exactly
// decimals fractional digits, interpreting q as base units of an asset
// with that many decimals:
//
//	QuantityFromInt64(1500000000).ToDecimalString(9)  // "1.500000000"
//	QuantityFromInt64(-1).ToDecimalString(6)          // "-0.000001"
//	QuantityFromInt64(0).ToDecimalString(2)           // "0.00"
//
// The result is for display and interchange only and must never be used for
// arithmetic. It round-trips through QuantityFromDecimalString with the
// same decimals and RoundExact.
func (q Quantity) ToDecimalString(decimals uint8) string {
	if decimals == 0 {
		return q.String()
	}
	v := q.ref()
	digits := new(big.Int).Abs(v).String()
	d := int(decimals)
	if len(digits) <= d {
		digits = strings.Repeat("0", d-len(digits)+1) + digits
	}
	var b strings.Builder
	b.Grow(len(digits) + 2)
	if v.Sign() < 0 {
		b.WriteByte('-')
	}
	b.WriteString(digits[:len(digits)-d])
	b.WriteByte('.')
	b.WriteString(digits[len(digits)-d:])
	return b.String()
}

// QuantityFromDecimalString parses a plain decimal such as "1.5" or
// "-0.000001" as base units of an asset with the given number of decimals.
// Fractional digits beyond decimals are rounded in the given mode; with
// RoundExact they must all be zero, otherwise ErrPrecisionLoss is returned.
//
//	QuantityFromDecimalString("1.5", 9, RoundExact)           // 1500000000
//	QuantityFromDecimalString("1.5000000001", 9, RoundExact)  // ErrPrecisionLoss
//	QuantityFromDecimalString("1.5000000001", 9, RoundDown)   // 1500000000
//	QuantityFromDecimalString("123.000", 0, RoundExact)       // 123
//
// The grammar is the same as ParseUSD: ["-"] digits ["." digits].
func QuantityFromDecimalString(s string, decimals uint8, mode RoundingMode) (Quantity, error) {
	if err := mode.validate(); err != nil {
		return Quantity{}, err
	}
	p, err := parseDecimalLiteral(s, maxDecimalInputLen)
	if err != nil {
		return Quantity{}, err
	}
	whole, ok := new(big.Int).SetString(p.intDigits+p.fracDigits, 10)
	if !ok {
		// Unreachable: parseDecimalLiteral guarantees ASCII digits.
		return Quantity{}, fmt.Errorf("%w: not a decimal literal", ErrInvalidFormat)
	}
	if p.negative {
		whole.Neg(whole)
	}
	d := int(decimals)
	var v *big.Int
	switch {
	case len(p.fracDigits) == d:
		v = whole
	case len(p.fracDigits) < d:
		v = whole.Mul(whole, pow10(d-len(p.fracDigits)))
	default:
		v, err = divRound(whole, pow10(len(p.fracDigits)-d), mode)
		if err != nil {
			return Quantity{}, err
		}
	}
	if err := checkQuantityMagnitude(v); err != nil {
		return Quantity{}, err
	}
	return Quantity{v: v}, nil
}
