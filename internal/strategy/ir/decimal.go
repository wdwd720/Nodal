package ir

import (
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/nodal/controlplane/internal/money"
)

// Decimal limits. Scale 18 covers every on-chain token decimals value; forty
// mantissa digits cover 2^128 with room, so no evaluator input can overflow
// into a slow path.
const (
	MaxScale          = 18
	MaxMantissaDigits = 40
	ProbabilityScale  = 4
)

// Decimal errors.
var (
	ErrInvalidDecimal  = errors.New("ir: invalid decimal")
	ErrScaleMismatch   = errors.New("ir: decimal scale mismatch")
	ErrDivisionByZero  = errors.New("ir: division by zero")
	ErrDecimalOverflow = errors.New("ir: decimal exceeds mantissa digit limit")
	ErrInvalidRounding = errors.New("ir: invalid rounding mode")
)

// Decimal is an exact fixed-point number: value = Mantissa × 10^-Scale. The
// mantissa is a decimal integer string (digits only, optional leading '-');
// it is never a float in any language. Two decimals only compare when their
// scales agree (STRATEGY_IR.md §2: no implicit coercion).
type Decimal struct {
	Mantissa string `json:"m"`
	Scale    uint8  `json:"s"`
}

// Probability is a Decimal with Scale == ProbabilityScale and 0 ≤ value ≤ 1.
type Probability = Decimal

// NewDecimal builds a Decimal from an integer mantissa and a scale.
func NewDecimal(mantissa *big.Int, scale uint8) Decimal {
	return Decimal{Mantissa: mantissa.String(), Scale: scale}
}

// DecimalFromInt64 builds a Decimal from an integer at scale 0.
func DecimalFromInt64(v int64) Decimal {
	return Decimal{Mantissa: big.NewInt(v).String(), Scale: 0}
}

// ParseDecimalString parses "123.45", "-0.5", "7" or "0.000" into a Decimal
// whose scale is the number of fractional digits written.
func ParseDecimalString(s string) (Decimal, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Decimal{}, ErrInvalidDecimal
	}
	neg := false
	if s[0] == '-' || s[0] == '+' {
		neg = s[0] == '-'
		s = s[1:]
	}
	// A decimal point requires digits on both sides: ".5" and "1." are as
	// invalid as "1.2.3". One spelling per value keeps the canonical form
	// canonical.
	intPart, fracPart, hasDot := strings.Cut(s, ".")
	if !allDigits(intPart) || (hasDot && !allDigits(fracPart)) {
		return Decimal{}, ErrInvalidDecimal
	}
	scale, err := scaleFromDigits(len(fracPart))
	if err != nil {
		return Decimal{}, err
	}
	digits := strings.TrimLeft(intPart+fracPart, "0")
	if digits == "" {
		digits = "0"
	}
	if len(digits) > MaxMantissaDigits {
		return Decimal{}, ErrDecimalOverflow
	}
	if neg && digits != "0" {
		digits = "-" + digits
	}
	return Decimal{Mantissa: digits, Scale: scale}, nil
}

// scaleFromDigits converts a count of fractional digits into a Scale. The
// guard is what makes the conversion total rather than a truncating cast:
// MaxScale is 18, two orders of magnitude below the uint8 maximum, so any n
// that passes the bound is representable exactly.
func scaleFromDigits(n int) (uint8, error) {
	if n < 0 || n > MaxScale {
		return 0, fmt.Errorf("%w: more than %d fractional digits", ErrInvalidDecimal, MaxScale)
	}
	return uint8(n), nil
}

// Validate checks the mantissa grammar (^-?(0|[1-9][0-9]*)$), the digit
// limit and the scale limit.
func (d Decimal) Validate() error {
	m := d.Mantissa
	if m == "" {
		return fmt.Errorf("%w: empty mantissa", ErrInvalidDecimal)
	}
	if m[0] == '-' {
		m = m[1:]
	}
	if m == "" || !allDigits(m) || (len(m) > 1 && m[0] == '0') {
		return fmt.Errorf("%w: mantissa %q", ErrInvalidDecimal, d.Mantissa)
	}
	if m == "0" && d.Mantissa[0] == '-' {
		return fmt.Errorf("%w: negative zero", ErrInvalidDecimal)
	}
	if len(m) > MaxMantissaDigits {
		return ErrDecimalOverflow
	}
	if d.Scale > MaxScale {
		return fmt.Errorf("%w: scale %d exceeds %d", ErrInvalidDecimal, d.Scale, MaxScale)
	}
	return nil
}

// Int returns the mantissa as a big.Int (a fresh copy).
func (d Decimal) Int() (*big.Int, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	v, ok := new(big.Int).SetString(d.Mantissa, 10)
	if !ok {
		return nil, fmt.Errorf("%w: mantissa %q", ErrInvalidDecimal, d.Mantissa)
	}
	return v, nil
}

// Sign returns -1, 0 or +1. An invalid decimal reports 0.
func (d Decimal) Sign() int {
	v, err := d.Int()
	if err != nil {
		return 0
	}
	return v.Sign()
}

// IsZero reports whether the value is zero.
func (d Decimal) IsZero() bool { return d.Sign() == 0 }

// String renders the value as a plain decimal literal ("-12.345", "0.5000").
func (d Decimal) String() string {
	v, err := d.Int()
	if err != nil {
		return "<invalid>"
	}
	neg := v.Sign() < 0
	digits := new(big.Int).Abs(v).String()
	if d.Scale == 0 {
		if neg {
			return "-" + digits
		}
		return digits
	}
	s := int(d.Scale)
	if len(digits) <= s {
		digits = strings.Repeat("0", s-len(digits)+1) + digits
	}
	out := digits[:len(digits)-s] + "." + digits[len(digits)-s:]
	if neg {
		return "-" + out
	}
	return out
}

// Cmp compares two decimals of the same scale. Differing scales are an
// error, never a coercion.
func (d Decimal) Cmp(o Decimal) (int, error) {
	if d.Scale != o.Scale {
		return 0, fmt.Errorf("%w: %d vs %d", ErrScaleMismatch, d.Scale, o.Scale)
	}
	a, err := d.Int()
	if err != nil {
		return 0, err
	}
	b, err := o.Int()
	if err != nil {
		return 0, err
	}
	return a.Cmp(b), nil
}

// Rescale converts d to the target scale. Scaling up is exact; scaling
// down applies mode (RoundExact fails on any lost precision).
func (d Decimal) Rescale(scale uint8, mode money.RoundingMode) (Decimal, error) {
	v, err := d.Int()
	if err != nil {
		return Decimal{}, err
	}
	if scale > MaxScale {
		return Decimal{}, fmt.Errorf("%w: scale %d exceeds %d", ErrInvalidDecimal, scale, MaxScale)
	}
	if scale >= d.Scale {
		v.Mul(v, pow10(int(scale-d.Scale)))
		return finish(v, scale)
	}
	q, err := roundDiv(v, pow10(int(d.Scale-scale)), mode)
	if err != nil {
		return Decimal{}, err
	}
	return finish(q, scale)
}

// ParseRounding maps an IR rounding name ("half_even", "down", ...) to the
// money rounding mode.
func ParseRounding(name string) (money.RoundingMode, error) {
	m, err := money.ParseRoundingMode(name)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrInvalidRounding, name)
	}
	return m, nil
}

// Add returns a + b at the target scale under mode.
func Add(a, b Decimal, scale uint8, mode money.RoundingMode) (Decimal, error) {
	return addSub(a, b, scale, mode, false)
}

// Sub returns a − b at the target scale under mode.
func Sub(a, b Decimal, scale uint8, mode money.RoundingMode) (Decimal, error) {
	return addSub(a, b, scale, mode, true)
}

func addSub(a, b Decimal, scale uint8, mode money.RoundingMode, sub bool) (Decimal, error) {
	common := max(a.Scale, b.Scale)
	x, err := a.Rescale(common, money.RoundExact)
	if err != nil {
		return Decimal{}, err
	}
	y, err := b.Rescale(common, money.RoundExact)
	if err != nil {
		return Decimal{}, err
	}
	xi, _ := x.Int()
	yi, _ := y.Int()
	if sub {
		xi.Sub(xi, yi)
	} else {
		xi.Add(xi, yi)
	}
	r, err := finish(xi, common)
	if err != nil {
		return Decimal{}, err
	}
	return r.Rescale(scale, mode)
}

// Mul returns a × b at the target scale under mode.
func Mul(a, b Decimal, scale uint8, mode money.RoundingMode) (Decimal, error) {
	x, err := a.Int()
	if err != nil {
		return Decimal{}, err
	}
	y, err := b.Int()
	if err != nil {
		return Decimal{}, err
	}
	prod := new(big.Int).Mul(x, y)
	prodScale := int(a.Scale) + int(b.Scale)
	if prodScale > int(scale) {
		q, err := roundDiv(prod, pow10(prodScale-int(scale)), mode)
		if err != nil {
			return Decimal{}, err
		}
		return finish(q, scale)
	}
	prod.Mul(prod, pow10(int(scale)-prodScale))
	return finish(prod, scale)
}

// Div returns a ÷ b at the target scale under mode. Division by zero is an
// error, never infinity or NaN.
func Div(a, b Decimal, scale uint8, mode money.RoundingMode) (Decimal, error) {
	x, err := a.Int()
	if err != nil {
		return Decimal{}, err
	}
	y, err := b.Int()
	if err != nil {
		return Decimal{}, err
	}
	if y.Sign() == 0 {
		return Decimal{}, ErrDivisionByZero
	}
	// (x / 10^sa) / (y / 10^sb) = x × 10^(sb + scale - sa) / y at the target scale.
	num := new(big.Int).Set(x)
	den := new(big.Int).Set(y)
	shift := int(b.Scale) + int(scale) - int(a.Scale)
	if shift >= 0 {
		num.Mul(num, pow10(shift))
	} else {
		den.Mul(den, pow10(-shift))
	}
	q, err := roundDiv(num, den, mode)
	if err != nil {
		return Decimal{}, err
	}
	return finish(q, scale)
}

// Min returns the smaller of a and b (same scale required) rescaled to scale.
func Min(a, b Decimal, scale uint8, mode money.RoundingMode) (Decimal, error) {
	c, err := a.Cmp(b)
	if err != nil {
		return Decimal{}, err
	}
	if c <= 0 {
		return a.Rescale(scale, mode)
	}
	return b.Rescale(scale, mode)
}

// Max returns the larger of a and b (same scale required) rescaled to scale.
func Max(a, b Decimal, scale uint8, mode money.RoundingMode) (Decimal, error) {
	c, err := a.Cmp(b)
	if err != nil {
		return Decimal{}, err
	}
	if c >= 0 {
		return a.Rescale(scale, mode)
	}
	return b.Rescale(scale, mode)
}

// IsProbability reports whether d has the probability scale and lies in [0, 1].
func (d Decimal) IsProbability() bool {
	if d.Scale != ProbabilityScale {
		return false
	}
	v, err := d.Int()
	if err != nil {
		return false
	}
	return v.Sign() >= 0 && v.Cmp(pow10(ProbabilityScale)) <= 0
}

// ToBPS converts a scale-0 decimal to basis points, rejecting anything else.
func (d Decimal) ToBPS() (money.BPS, error) {
	if d.Scale != 0 {
		return 0, fmt.Errorf("%w: bps require scale 0, got %d", ErrScaleMismatch, d.Scale)
	}
	v, err := d.Int()
	if err != nil {
		return 0, err
	}
	if !v.IsInt64() {
		return 0, ErrDecimalOverflow
	}
	return money.BPS(v.Int64()), nil
}

// ToUSD converts a decimal with at most two fractional digits to USD.
func (d Decimal) ToUSD() (money.USD, error) {
	r, err := d.Rescale(2, money.RoundExact)
	if err != nil {
		return money.USD{}, err
	}
	v, _ := r.Int()
	if !v.IsInt64() {
		return money.USD{}, ErrDecimalOverflow
	}
	return money.USDFromMinor(v.Int64()), nil
}

// DecimalFromUSD renders a USD amount as a scale-2 decimal.
func DecimalFromUSD(u money.USD) Decimal {
	return Decimal{Mantissa: big.NewInt(u.Minor()).String(), Scale: 2}
}

func finish(v *big.Int, scale uint8) (Decimal, error) {
	d := Decimal{Mantissa: v.String(), Scale: scale}
	if err := d.Validate(); err != nil {
		return Decimal{}, err
	}
	return d, nil
}

// roundDiv divides num by den under mode with the exact semantics of
// internal/money (truncation toward zero, away from zero, floor, ceil, half
// even, half up; exact fails on any remainder).
func roundDiv(num, den *big.Int, mode money.RoundingMode) (*big.Int, error) {
	if !mode.Valid() {
		return nil, ErrInvalidRounding
	}
	if den.Sign() == 0 {
		return nil, ErrDivisionByZero
	}
	quo, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	if rem.Sign() == 0 {
		return quo, nil
	}
	if mode == money.RoundExact {
		return nil, fmt.Errorf("ir: %s/%s is not exact: %w", num, den, money.ErrPrecisionLoss)
	}
	negative := (num.Sign() < 0) != (den.Sign() < 0)
	one := big.NewInt(1)
	away := func() *big.Int {
		if negative {
			return quo.Sub(quo, one)
		}
		return quo.Add(quo, one)
	}
	switch mode {
	case money.RoundDown:
		return quo, nil
	case money.RoundUp:
		return away(), nil
	case money.RoundFloor:
		if negative {
			return quo.Sub(quo, one), nil
		}
		return quo, nil
	case money.RoundCeil:
		if negative {
			return quo, nil
		}
		return quo.Add(quo, one), nil
	case money.RoundHalfEven, money.RoundHalfUp:
		twice := new(big.Int).Abs(rem)
		twice.Lsh(twice, 1)
		switch twice.Cmp(new(big.Int).Abs(den)) {
		case -1:
			return quo, nil
		case 1:
			return away(), nil
		default:
			if mode == money.RoundHalfUp || quo.Bit(0) == 1 {
				return away(), nil
			}
			return quo, nil
		}
	}
	return nil, ErrInvalidRounding
}

func pow10(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
