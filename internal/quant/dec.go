package quant

import (
	"math/big"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/money"
)

// Scale is the number of fractional decimal digits every Dec carries: a Dec
// is an integer coefficient c read as c × 10^-Scale. It is the working
// precision of the package; one unit in the last place (ulp) is 10^-40.
const Scale int32 = 40

// scaleDigits is Scale as an int, for slicing and exponent arithmetic.
const scaleDigits = 40

// MaxIntegerDigits bounds the magnitude accepted by ParseDec and
// ParseDecRound: the parsed value must satisfy |value| < 10^MaxIntegerDigits,
// otherwise OVERFLOW is returned. Arithmetic results are not bounded.
const MaxIntegerDigits = 128

// maxParseLen bounds parser input so hostile input cannot force unbounded
// big-integer work.
const maxParseLen = 1024

// Dec is an exact, immutable fixed-point decimal: an arbitrary-precision
// integer coefficient with exactly Scale (40) fractional digits.
//
// Addition, subtraction, negation and comparison are exact. Multiplication
// and division produce more than Scale digits in general and therefore
// round: Mul rounds half-even (error ≤ ½·10^-40), MulRound and Quo take an
// explicit money.RoundingMode, and RoundExact turns any would-be rounding
// into PRECISION_LOSS.
//
// Dec has value semantics: every method returns a new value and never
// modifies its receiver or arguments, so Decs may be copied and shared
// across goroutines freely. The zero value is 0 and is fully usable. Do not
// compare Decs with ==; equal values may hold distinct pointers. Use Equal
// or Cmp.
type Dec struct{ c *big.Int }

// coef returns the coefficient for read-only use. Callers must never mutate
// the result; the zero value is materialised as a fresh 0.
func (d Dec) coef() *big.Int {
	if d.c == nil {
		return new(big.Int)
	}
	return d.c
}

// unit returns a fresh 10^Scale, the coefficient of 1. It is built from its
// big-endian bytes (a test pins them to 10^40) because it sits on every
// multiplication and conversion path.
func unit() *big.Int {
	return new(big.Int).SetBytes([]byte{
		0x1d, 0x63, 0x29, 0xf1, 0xc3, 0x5c, 0xa4, 0xbf, 0xab,
		0xb9, 0xf5, 0x61, 0x00, 0x00, 0x00, 0x00, 0x00,
	})
}

// tenPow returns a fresh 10^n for n >= 0.
func tenPow(n int) *big.Int {
	if n == scaleDigits {
		return unit()
	}
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// decPow10 returns 10^n as a Dec, for -Scale <= n.
func decPow10(n int) Dec { return Dec{c: tenPow(scaleDigits + n)} }

// DecFromInt64 returns n as a Dec. It is exact.
func DecFromInt64(n int64) Dec {
	c := big.NewInt(n)
	return Dec{c: c.Mul(c, unit())}
}

// DecFromBPS returns b basis points as a fraction (10_000 bps = 1). It is
// exact: 1 bp = 0.0001.
func DecFromBPS(b money.BPS) Dec {
	c := big.NewInt(int64(b))
	return Dec{c: c.Mul(c, tenPow(scaleDigits-4))}
}

// DecFromQuantity returns q base units of an asset with the given number of
// decimals as a decimal amount, q × 10^-decimals. The conversion is exact
// when decimals <= Scale; beyond that the extra digits are rounded in mode
// (RoundExact fails with PRECISION_LOSS if any of them is non-zero). The
// mode is validated even when no rounding is needed.
func DecFromQuantity(q money.Quantity, decimals uint8, mode money.RoundingMode) (Dec, error) {
	if err := checkMode(mode); err != nil {
		return Dec{}, err
	}
	v := q.BigInt()
	d := int(decimals)
	if d <= scaleDigits {
		return Dec{c: v.Mul(v, tenPow(scaleDigits-d))}, nil
	}
	c, err := divRound(v, tenPow(d-scaleDigits), mode)
	if err != nil {
		return Dec{}, err
	}
	return Dec{c: c}, nil
}

// DecFromPrice returns the value of p (Mantissa × 10^-Scale quote per base
// unit) as a Dec. The price is validated first; its scale is at most
// money.MaxPriceScale (38) so the conversion is always exact. The quote
// asset, source and timestamp are not carried: the caller keeps the
// provenance of every number it feeds into a model.
func DecFromPrice(p money.Price) (Dec, error) {
	if err := p.Validate(); err != nil {
		return Dec{}, mapMoneyError(err, "price")
	}
	v := p.Mantissa.BigInt()
	return Dec{c: v.Mul(v, tenPow(scaleDigits-int(p.Scale)))}, nil
}

// ToQuantity returns d expressed in base units of an asset with the given
// number of decimals, d × 10^decimals, rounded in mode when d carries more
// fractional digits than the asset (RoundExact fails with PRECISION_LOSS).
func (d Dec) ToQuantity(decimals uint8, mode money.RoundingMode) (money.Quantity, error) {
	if err := checkMode(mode); err != nil {
		return money.Quantity{}, err
	}
	n := int(decimals)
	if n >= scaleDigits {
		v := new(big.Int).Mul(d.coef(), tenPow(n-scaleDigits))
		return money.QuantityFromBigInt(v), nil
	}
	v, err := divRound(d.coef(), tenPow(scaleDigits-n), mode)
	if err != nil {
		return money.Quantity{}, err
	}
	return money.QuantityFromBigInt(v), nil
}

// ToInt64 returns d rounded to an integer in mode (RoundExact fails with
// PRECISION_LOSS for a fractional d), or OVERFLOW when the result does not
// fit an int64.
func (d Dec) ToInt64(mode money.RoundingMode) (int64, error) {
	q, err := d.ToQuantity(0, mode)
	if err != nil {
		return 0, err
	}
	n, err := q.Int64()
	if err != nil {
		return 0, mapMoneyError(err, "int64")
	}
	return n, nil
}

// ToPrice returns d as a money.Price with the given scale (0..38), rounding
// the mantissa in mode, and validates it: a negative value, a blank quote
// asset or source, or a zero timestamp fail with VALIDATION_FAILED. The
// result is still a model output; labelling it with a source is the
// caller's responsibility and must never present it as a market price.
func (d Dec) ToPrice(scale int32, quoteAsset, source string, at time.Time, mode money.RoundingMode) (money.Price, error) {
	if scale < 0 || scale > money.MaxPriceScale {
		return money.Price{}, errField("scale", "price scale must be within [0, 38]")
	}
	mantissa, err := d.ToQuantity(uint8(scale), mode) // #nosec G115 -- scale is in [0, 38] by the check above
	if err != nil {
		return money.Price{}, err
	}
	p, err := money.NewPrice(mantissa, scale, quoteAsset, source, at)
	if err != nil {
		return money.Price{}, mapMoneyError(err, "price")
	}
	return p, nil
}

// ParseDec parses a plain decimal literal
//
//	literal := ["-"] digit+ ["." digit+]
//
// and nothing else: no leading "+", whitespace, exponent, separators, "NaN"
// or "Inf", no bare "1." or ".5". At most Scale fractional digits may be
// non-zero (more fail with PRECISION_LOSS; use ParseDecRound to round), and
// the magnitude must stay below 10^MaxIntegerDigits (OVERFLOW). Other
// malformed input fails with VALIDATION_FAILED. Inputs longer than 1024
// bytes are rejected.
func ParseDec(s string) (Dec, error) { return parseDec(s, money.RoundExact) }

// ParseDecRound is ParseDec with fractional digits beyond Scale rounded in
// mode instead of rejected.
func ParseDecRound(s string, mode money.RoundingMode) (Dec, error) { return parseDec(s, mode) }

func parseDec(s string, mode money.RoundingMode) (Dec, error) {
	if err := checkMode(mode); err != nil {
		return Dec{}, err
	}
	if s == "" {
		return Dec{}, errInvalid("empty decimal")
	}
	if len(s) > maxParseLen {
		return Dec{}, errInvalid("decimal longer than 1024 bytes")
	}
	i := 0
	negative := false
	if s[0] == '-' {
		negative = true
		i = 1
	}
	start := i
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	if i == start {
		return Dec{}, errInvalid("expected a digit").WithField("offset", i)
	}
	intDigits := s[start:i]
	fracDigits := ""
	if i < len(s) {
		if s[i] != '.' {
			return Dec{}, errInvalid("unexpected character").WithField("offset", i)
		}
		i++
		start = i
		for i < len(s) && isDigit(s[i]) {
			i++
		}
		if i == start {
			return Dec{}, errInvalid("expected a digit after the decimal point").WithField("offset", i)
		}
		if i != len(s) {
			return Dec{}, errInvalid("unexpected character").WithField("offset", i)
		}
		fracDigits = s[start:i]
	}
	whole, ok := new(big.Int).SetString(intDigits+fracDigits, 10)
	if !ok {
		// Unreachable: every byte was checked to be an ASCII digit.
		return Dec{}, errInvalid("not a decimal literal")
	}
	if negative {
		whole.Neg(whole)
	}
	c := whole
	if f := len(fracDigits); f <= scaleDigits {
		c.Mul(c, tenPow(scaleDigits-f))
	} else {
		var err error
		if c, err = divRound(whole, tenPow(f-scaleDigits), mode); err != nil {
			return Dec{}, err
		}
	}
	if new(big.Int).Abs(c).Cmp(tenPow(MaxIntegerDigits+scaleDigits)) >= 0 {
		return Dec{}, errOverflow("decimal magnitude must be below 10^128")
	}
	return Dec{c: c}, nil
}

func isDigit(b byte) bool { return '0' <= b && b <= '9' }

// String renders d in canonical form: an optional "-" (never for zero), the
// integer digits without leading zeros, and a fractional part without
// trailing zeros, omitted entirely when d is an integer. Equal values render
// identically and the output round-trips through ParseDec.
//
//	"0"  "12"  "-0.5"  "3.1415926535897932384626433832795028841972"
func (d Dec) String() string {
	c := d.coef()
	if c.Sign() == 0 {
		return "0"
	}
	digits := new(big.Int).Abs(c).String()
	if len(digits) <= scaleDigits {
		digits = strings.Repeat("0", scaleDigits+1-len(digits)) + digits
	}
	cut := len(digits) - scaleDigits
	frac := strings.TrimRight(digits[cut:], "0")
	var b strings.Builder
	b.Grow(len(digits) + 2)
	if c.Sign() < 0 {
		b.WriteByte('-')
	}
	b.WriteString(digits[:cut])
	if frac != "" {
		b.WriteByte('.')
		b.WriteString(frac)
	}
	return b.String()
}

// StringFixed rounds d to scale fractional digits (0..Scale) in mode and
// renders exactly that many, for display: StringFixed(2, RoundHalfEven) of
// 1.5 is "1.50". RoundExact fails with PRECISION_LOSS if digits would be
// dropped.
func (d Dec) StringFixed(scale int32, mode money.RoundingMode) (string, error) {
	if scale < 0 || scale > Scale {
		return "", errField("scale", "display scale must be within [0, 40]")
	}
	r, err := d.Round(scale, mode)
	if err != nil {
		return "", err
	}
	shown := new(big.Int).Quo(r.coef(), tenPow(scaleDigits-int(scale)))
	return money.QuantityFromBigInt(shown).ToDecimalString(uint8(scale)), nil // #nosec G115 -- scale is in [0, 40]
}

// Round returns d rounded to scale fractional digits in mode. A scale of
// Scale or more returns d unchanged (after validating mode); a negative
// scale fails with VALIDATION_FAILED; RoundExact fails with PRECISION_LOSS
// when d has non-zero digits beyond scale.
func (d Dec) Round(scale int32, mode money.RoundingMode) (Dec, error) {
	if err := checkMode(mode); err != nil {
		return Dec{}, err
	}
	if scale < 0 {
		return Dec{}, errField("scale", "rounding scale must not be negative")
	}
	if scale >= Scale {
		return d, nil
	}
	f := tenPow(scaleDigits - int(scale))
	q, err := divRound(d.coef(), f, mode)
	if err != nil {
		return Dec{}, err
	}
	return Dec{c: q.Mul(q, f)}, nil
}

// Add returns d + o exactly.
func (d Dec) Add(o Dec) Dec { return Dec{c: new(big.Int).Add(d.coef(), o.coef())} }

// Sub returns d − o exactly.
func (d Dec) Sub(o Dec) Dec { return Dec{c: new(big.Int).Sub(d.coef(), o.coef())} }

// Neg returns −d exactly.
func (d Dec) Neg() Dec { return Dec{c: new(big.Int).Neg(d.coef())} }

// Abs returns |d| exactly.
func (d Dec) Abs() Dec { return Dec{c: new(big.Int).Abs(d.coef())} }

// Mul returns d × o rounded half-even to Scale digits; the error is at most
// ½·10^-40. Use MulRound to choose another mode.
func (d Dec) Mul(o Dec) Dec {
	p := new(big.Int).Mul(d.coef(), o.coef())
	return Dec{c: quoHalfEven(p, unit())}
}

// MulRound returns d × o rounded to Scale digits in mode (RoundExact fails
// with PRECISION_LOSS when the product has more than Scale digits).
func (d Dec) MulRound(o Dec, mode money.RoundingMode) (Dec, error) {
	p := new(big.Int).Mul(d.coef(), o.coef())
	c, err := divRound(p, unit(), mode)
	if err != nil {
		return Dec{}, err
	}
	return Dec{c: c}, nil
}

// Quo returns d / o rounded to Scale digits in mode. Division by zero fails
// with VALIDATION_FAILED; RoundExact fails with PRECISION_LOSS when the
// quotient has more than Scale digits.
func (d Dec) Quo(o Dec, mode money.RoundingMode) (Dec, error) {
	num := new(big.Int).Mul(d.coef(), unit())
	c, err := divRound(num, o.coef(), mode)
	if err != nil {
		return Dec{}, err
	}
	return Dec{c: c}, nil
}

// quo is Quo rounded half-even for internal callers that guarantee o != 0.
func (d Dec) quo(o Dec) Dec {
	return Dec{c: quoHalfEven(new(big.Int).Mul(d.coef(), unit()), o.coef())}
}

// Cmp compares d and o and returns -1, 0 or +1.
func (d Dec) Cmp(o Dec) int { return d.coef().Cmp(o.coef()) }

// Equal reports whether d and o have the same value.
func (d Dec) Equal(o Dec) bool { return d.Cmp(o) == 0 }

// Sign returns -1, 0 or +1.
func (d Dec) Sign() int { return d.coef().Sign() }

// IsZero reports whether d is exactly zero.
func (d Dec) IsZero() bool { return d.Sign() == 0 }

// IsNegative reports whether d is below zero.
func (d Dec) IsNegative() bool { return d.Sign() < 0 }

// IsPositive reports whether d is above zero.
func (d Dec) IsPositive() bool { return d.Sign() > 0 }

// Min returns the smaller of d and o.
func (d Dec) Min(o Dec) Dec {
	if d.Cmp(o) <= 0 {
		return d
	}
	return o
}

// Max returns the larger of d and o.
func (d Dec) Max(o Dec) Dec {
	if d.Cmp(o) >= 0 {
		return d
	}
	return o
}
