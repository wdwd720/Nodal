package nodalsdk

import (
	"errors"
	"math/big"
	"strings"
)

// MaxScale is the largest number of fractional digits a Decimal may carry.
const MaxScale int32 = 38

// maxDecimalLen bounds the textual form a parser accepts.
const maxDecimalLen = 100

// Errors returned by the decimal functions.
var (
	ErrInvalidDecimal = errors.New("nodalsdk: invalid decimal")
	ErrScale          = errors.New("nodalsdk: scale out of range")
	ErrDivisionByZero = errors.New("nodalsdk: division by zero")
	ErrRounding       = errors.New("nodalsdk: unknown rounding mode")
	ErrOverflow       = errors.New("nodalsdk: integer overflow")
)

// Rounding names how an inexact result is rounded. Every operation that can
// lose precision takes one explicitly; there is no default.
type Rounding int

// Rounding modes.
const (
	// RoundDown rounds toward zero.
	RoundDown Rounding = iota + 1
	// RoundUp rounds away from zero.
	RoundUp
	// RoundHalfEven rounds to the nearest, ties to the even neighbour.
	RoundHalfEven
	// RoundHalfUp rounds to the nearest, ties away from zero.
	RoundHalfUp
	// RoundFloor rounds toward negative infinity.
	RoundFloor
	// RoundCeil rounds toward positive infinity.
	RoundCeil
)

// Valid reports whether r is a declared rounding mode.
func (r Rounding) Valid() bool { return r >= RoundDown && r <= RoundCeil }

// Decimal is an exact decimal number, Unscaled x 10^-Scale. The zero value is
// zero at scale 0. A Decimal is immutable: every operation returns a new one.
type Decimal struct {
	unscaled *big.Int
	scale    int32
}

func (d Decimal) int() *big.Int {
	if d.unscaled == nil {
		return new(big.Int)
	}
	return d.unscaled
}

// ParseDecimal parses a plain decimal: an optional '-', an integer part with
// no leading zeros (a single "0" is allowed), and an optional '.' followed by
// at least one digit. No '+', no exponent, no whitespace, no negative zero.
// The number of fractional digits is the scale and is preserved exactly:
// "150.10" has scale 2.
func ParseDecimal(s string) (Decimal, error) {
	if len(s) == 0 || len(s) > maxDecimalLen {
		return Decimal{}, ErrInvalidDecimal
	}
	neg := false
	body := s
	if body[0] == '-' {
		neg = true
		body = body[1:]
	}
	intPart, fracPart, hasDot := strings.Cut(body, ".")
	if intPart == "" || (hasDot && fracPart == "") {
		return Decimal{}, ErrInvalidDecimal
	}
	if len(intPart) > 1 && intPart[0] == '0' {
		return Decimal{}, ErrInvalidDecimal
	}
	if !allDigits(intPart) || !allDigits(fracPart) {
		return Decimal{}, ErrInvalidDecimal
	}
	if int64(len(fracPart)) > int64(MaxScale) {
		return Decimal{}, ErrScale
	}
	u, ok := new(big.Int).SetString(intPart+fracPart, 10)
	if !ok {
		return Decimal{}, ErrInvalidDecimal
	}
	if neg {
		if u.Sign() == 0 {
			return Decimal{}, ErrInvalidDecimal
		}
		u.Neg(u)
	}
	return Decimal{unscaled: u, scale: int32(len(fracPart))}, nil // #nosec G115 -- len(fracPart) <= MaxScale (38)
}

// ParseUnits parses an integer quantity of base units: ParseDecimal with
// scale 0.
func ParseUnits(s string) (Decimal, error) {
	d, err := ParseDecimal(s)
	if err != nil {
		return Decimal{}, err
	}
	if d.scale != 0 {
		return Decimal{}, ErrInvalidDecimal
	}
	return d, nil
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// NewDecimal returns unscaled x 10^-scale.
func NewDecimal(unscaled int64, scale int32) (Decimal, error) {
	if scale < 0 || scale > MaxScale {
		return Decimal{}, ErrScale
	}
	return Decimal{unscaled: big.NewInt(unscaled), scale: scale}, nil
}

// IntDecimal returns n at scale 0.
func IntDecimal(n int64) Decimal { return Decimal{unscaled: big.NewInt(n)} }

// Scale returns the number of fractional digits.
func (d Decimal) Scale() int32 { return d.scale }

// Unscaled returns a copy of the unscaled integer.
func (d Decimal) Unscaled() *big.Int { return new(big.Int).Set(d.int()) }

// Sign returns -1, 0 or +1.
func (d Decimal) Sign() int { return d.int().Sign() }

// IsZero reports whether d is zero.
func (d Decimal) IsZero() bool { return d.Sign() == 0 }

// IsInteger reports whether d has no fractional part.
func (d Decimal) IsInteger() bool {
	if d.scale == 0 {
		return true
	}
	_, r := new(big.Int).QuoRem(d.int(), pow10(d.scale), new(big.Int))
	return r.Sign() == 0
}

// String renders d with exactly Scale fractional digits.
func (d Decimal) String() string {
	u := d.int()
	neg := u.Sign() < 0
	digits := new(big.Int).Abs(u).String()
	if d.scale > 0 {
		sc := int(d.scale)
		if len(digits) <= sc {
			digits = strings.Repeat("0", sc-len(digits)+1) + digits
		}
		digits = digits[:len(digits)-sc] + "." + digits[len(digits)-sc:]
	}
	if neg {
		return "-" + digits
	}
	return digits
}

// align returns both unscaled values at the larger of the two scales.
func align(a, b Decimal) (*big.Int, *big.Int, int32) {
	switch {
	case a.scale == b.scale:
		return a.int(), b.int(), a.scale
	case a.scale > b.scale:
		return a.int(), new(big.Int).Mul(b.int(), pow10(a.scale-b.scale)), a.scale
	default:
		return new(big.Int).Mul(a.int(), pow10(b.scale-a.scale)), b.int(), b.scale
	}
}

// Cmp compares numerically: "1.0" equals "1.00".
func (d Decimal) Cmp(o Decimal) int {
	x, y, _ := align(d, o)
	return x.Cmp(y)
}

// Add returns d + o exactly, at the larger scale.
func (d Decimal) Add(o Decimal) Decimal {
	x, y, s := align(d, o)
	return Decimal{unscaled: new(big.Int).Add(x, y), scale: s}
}

// Sub returns d - o exactly, at the larger scale.
func (d Decimal) Sub(o Decimal) Decimal {
	x, y, s := align(d, o)
	return Decimal{unscaled: new(big.Int).Sub(x, y), scale: s}
}

// Neg returns -d.
func (d Decimal) Neg() Decimal { return Decimal{unscaled: new(big.Int).Neg(d.int()), scale: d.scale} }

// Abs returns |d|.
func (d Decimal) Abs() Decimal { return Decimal{unscaled: new(big.Int).Abs(d.int()), scale: d.scale} }

// Mul returns d x o exactly. The scale is the sum of the scales and must not
// exceed MaxScale; Rescale an operand first if it would.
func (d Decimal) Mul(o Decimal) (Decimal, error) {
	s := int64(d.scale) + int64(o.scale)
	if s > int64(MaxScale) {
		return Decimal{}, ErrScale
	}
	return Decimal{unscaled: new(big.Int).Mul(d.int(), o.int()), scale: int32(s)}, nil // #nosec G115 -- s <= MaxScale
}

// MulInt returns d x n exactly, at d's scale.
func (d Decimal) MulInt(n int64) Decimal {
	return Decimal{unscaled: new(big.Int).Mul(d.int(), big.NewInt(n)), scale: d.scale}
}

// Quo returns d / o rounded to scale with mode.
func (d Decimal) Quo(o Decimal, scale int32, mode Rounding) (Decimal, error) {
	if scale < 0 || scale > MaxScale {
		return Decimal{}, ErrScale
	}
	if !mode.Valid() {
		return Decimal{}, ErrRounding
	}
	if o.Sign() == 0 {
		return Decimal{}, ErrDivisionByZero
	}
	// d/o at scale s: (du / 10^ds) / (ou / 10^os) x 10^s
	//               = du x 10^(s + os - ds) / ou
	num := d.int()
	den := o.int()
	shift := int64(scale) + int64(o.scale) - int64(d.scale)
	if shift >= 0 {
		num = new(big.Int).Mul(num, pow10(int32(shift))) // #nosec G115 -- |shift| <= 2*MaxScale
	} else {
		den = new(big.Int).Mul(den, pow10(int32(-shift))) // #nosec G115 -- |shift| <= 2*MaxScale
	}
	return Decimal{unscaled: divRound(num, den, mode), scale: scale}, nil
}

// QuoInt returns d / n rounded to scale with mode.
func (d Decimal) QuoInt(n int64, scale int32, mode Rounding) (Decimal, error) {
	return d.Quo(IntDecimal(n), scale, mode)
}

// MovePointLeft returns d x 10^-n exactly (the unscaled integer is kept and
// the scale grows by n): 1500000 base units with 6 decimals is
// IntDecimal(1500000).MovePointLeft(6) = "1.500000".
func (d Decimal) MovePointLeft(n int32) (Decimal, error) {
	s := int64(d.scale) + int64(n)
	if n < 0 || s > int64(MaxScale) {
		return Decimal{}, ErrScale
	}
	return Decimal{unscaled: new(big.Int).Set(d.int()), scale: int32(s)}, nil // #nosec G115 -- s <= MaxScale
}

// Rescale returns d at scale, rounding with mode when digits are dropped.
func (d Decimal) Rescale(scale int32, mode Rounding) (Decimal, error) {
	if scale < 0 || scale > MaxScale {
		return Decimal{}, ErrScale
	}
	if !mode.Valid() {
		return Decimal{}, ErrRounding
	}
	switch {
	case scale == d.scale:
		return Decimal{unscaled: new(big.Int).Set(d.int()), scale: scale}, nil
	case scale > d.scale:
		return Decimal{unscaled: new(big.Int).Mul(d.int(), pow10(scale-d.scale)), scale: scale}, nil
	default:
		return Decimal{unscaled: divRound(d.int(), pow10(d.scale-scale), mode), scale: scale}, nil
	}
}

// pow10 returns 10^n for 0 <= n.
func pow10(n int32) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// divRound returns num/den rounded with mode. den is never zero here.
func divRound(num, den *big.Int, mode Rounding) *big.Int {
	q, r := new(big.Int).QuoRem(num, den, new(big.Int))
	if r.Sign() == 0 {
		return q
	}
	// The exact quotient lies strictly between q and q + sign, where sign is
	// the sign of the exact result. QuoRem truncates toward zero.
	sign := num.Sign() * den.Sign()
	awayFromZero := false
	switch mode {
	case RoundDown:
		awayFromZero = false
	case RoundUp:
		awayFromZero = true
	case RoundFloor:
		awayFromZero = sign < 0
	case RoundCeil:
		awayFromZero = sign > 0
	case RoundHalfUp, RoundHalfEven:
		twice := new(big.Int).Abs(r)
		twice.Lsh(twice, 1)
		c := twice.Cmp(new(big.Int).Abs(den))
		switch {
		case c > 0:
			awayFromZero = true
		case c < 0:
			awayFromZero = false
		default:
			awayFromZero = mode == RoundHalfUp || q.Bit(0) == 1
		}
	}
	if awayFromZero {
		if sign < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	return q
}

// AddInt64 returns a + b, or ErrOverflow.
func AddInt64(a, b int64) (int64, error) {
	c := a + b
	if (c > a) != (b > 0) {
		return 0, ErrOverflow
	}
	return c, nil
}

// SubInt64 returns a - b, or ErrOverflow.
func SubInt64(a, b int64) (int64, error) {
	c := a - b
	if (c < a) != (b > 0) {
		return 0, ErrOverflow
	}
	return c, nil
}

// MulInt64 returns a x b, or ErrOverflow.
func MulInt64(a, b int64) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	c := a * b
	if c/b != a || (a == -1 && b == minInt64) || (b == -1 && a == minInt64) {
		return 0, ErrOverflow
	}
	return c, nil
}

const minInt64 = -1 << 63
