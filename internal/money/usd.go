package money

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// usdDecimals is the number of fractional decimal digits a USD carries.
const usdDecimals = 2

// Bounds of the USD type expressed in minor units (cents).
const (
	// MaxUSDMinor is the largest representable amount in cents:
	// 92_233_720_368_547_758.07 USD.
	MaxUSDMinor int64 = math.MaxInt64
	// MinUSDMinor is the smallest (most negative) representable amount in
	// cents: -92_233_720_368_547_758.08 USD. Note that its negation does not
	// fit; see Neg and NegChecked.
	MinUSDMinor int64 = math.MinInt64
)

// USD is an exact amount of United States dollars stored as int64 minor
// units (cents). The zero value is 0.00 USD.
//
// USD is a value type: it is safe to copy and compare with ==. Every
// arithmetic method is overflow-checked and returns ErrOverflow rather than
// wrapping. USD never becomes a JSON number; see MarshalJSON.
type USD struct{ minor int64 }

// MaxUSD returns the largest representable amount, 92233720368547758.07.
func MaxUSD() USD { return USD{minor: MaxUSDMinor} }

// MinUSD returns the smallest representable amount, -92233720368547758.08.
func MinUSD() USD { return USD{minor: MinUSDMinor} }

// USDFromMinor constructs a USD from a count of cents.
func USDFromMinor(minor int64) USD { return USD{minor: minor} }

// ParseUSD parses a plain decimal dollar amount such as "1234.56", "-0.01"
// or "7". At most two fractional digits are accepted; more fractional digits
// (even zeros) fail with ErrPrecisionLoss so that a caller who wants
// rounding must say so via ParseUSDRound. Any other deviation from the
// grammar ["-"] digits ["." digits] (a leading "+", whitespace, exponents,
// hex, separators, NaN, Inf, empty input) fails with ErrInvalidFormat.
// Amounts outside [MinUSD, MaxUSD] fail with ErrOverflow.
func ParseUSD(s string) (USD, error) {
	p, err := parseDecimalLiteral(s, maxUSDInputLen)
	if err != nil {
		return USD{}, err
	}
	if len(p.fracDigits) > usdDecimals {
		return USD{}, fmt.Errorf("%w: USD carries at most %d decimal places (use ParseUSDRound to round explicitly)", ErrPrecisionLoss, usdDecimals)
	}
	return usdFromExactParts(p)
}

// ParseUSDRound parses like ParseUSD but accepts any number of fractional
// digits and rounds to cents in the given mode. With RoundExact the extra
// digits must all be zero, otherwise ErrPrecisionLoss is returned.
func ParseUSDRound(s string, mode RoundingMode) (USD, error) {
	if err := mode.validate(); err != nil {
		return USD{}, err
	}
	p, err := parseDecimalLiteral(s, maxUSDInputLen)
	if err != nil {
		return USD{}, err
	}
	if len(p.fracDigits) <= usdDecimals {
		return usdFromExactParts(p)
	}
	whole, ok := new(big.Int).SetString(p.intDigits+p.fracDigits, 10)
	if !ok {
		// Unreachable: parseDecimalLiteral guarantees ASCII digits.
		return USD{}, fmt.Errorf("%w: not a decimal literal", ErrInvalidFormat)
	}
	if p.negative {
		whole.Neg(whole)
	}
	minor, err := divRound(whole, pow10(len(p.fracDigits)-usdDecimals), mode)
	if err != nil {
		return USD{}, err
	}
	return usdFromBig(minor)
}

// usdFromExactParts builds a USD from parts with at most two fractional
// digits using int64 parsing; the only possible failure is range.
func usdFromExactParts(p decimalParts) (USD, error) {
	var b strings.Builder
	b.Grow(len(p.intDigits) + usdDecimals + 1)
	if p.negative {
		b.WriteByte('-')
	}
	b.WriteString(p.intDigits)
	b.WriteString(p.fracDigits)
	for i := len(p.fracDigits); i < usdDecimals; i++ {
		b.WriteByte('0')
	}
	minor, err := strconv.ParseInt(b.String(), 10, 64)
	if err != nil {
		return USD{}, fmt.Errorf("%w: amount outside the USD range", ErrOverflow)
	}
	return USD{minor: minor}, nil
}

// usdFromBig converts a big integer count of cents, failing with
// ErrOverflow if it does not fit int64.
func usdFromBig(minor *big.Int) (USD, error) {
	if !minor.IsInt64() {
		return USD{}, fmt.Errorf("%w: amount outside the USD range", ErrOverflow)
	}
	return USD{minor: minor.Int64()}, nil
}

// Minor returns the amount in cents.
func (u USD) Minor() int64 { return u.minor }

// String renders the amount as "[-]dollars.cc", always with exactly two
// fractional digits: "1234.56", "-0.01", "0.00". The output round-trips
// through ParseUSD.
func (u USD) String() string { return formatCents(u.minor) }

// IsZero reports whether the amount is exactly zero.
func (u USD) IsZero() bool { return u.minor == 0 }

// IsNegative reports whether the amount is below zero.
func (u USD) IsNegative() bool { return u.minor < 0 }

// IsPositive reports whether the amount is above zero.
func (u USD) IsPositive() bool { return u.minor > 0 }

// Sign returns -1, 0 or +1.
func (u USD) Sign() int {
	switch {
	case u.minor < 0:
		return -1
	case u.minor > 0:
		return 1
	}
	return 0
}

// Cmp compares two amounts and returns -1, 0 or +1.
func (u USD) Cmp(o USD) int {
	switch {
	case u.minor < o.minor:
		return -1
	case u.minor > o.minor:
		return 1
	}
	return 0
}

// Equal reports whether the two amounts are identical.
func (u USD) Equal(o USD) bool { return u.minor == o.minor }

// Neg returns -u.
//
// MinUSD has no representable negation. Neg panics for that single value
// because the contract signature carries no error; code that negates
// externally supplied amounts must use NegChecked.
func (u USD) Neg() USD {
	n, err := u.NegChecked()
	if err != nil {
		panic("money: USD.Neg of MinUSD overflows; use NegChecked")
	}
	return n
}

// NegChecked returns -u, or ErrOverflow for MinUSD.
func (u USD) NegChecked() (USD, error) {
	if u.minor == MinUSDMinor {
		return USD{}, fmt.Errorf("%w: negating MinUSD", ErrOverflow)
	}
	return USD{minor: -u.minor}, nil
}

// Abs returns |u|, or ErrOverflow for MinUSD.
func (u USD) Abs() (USD, error) {
	if u.minor >= 0 {
		return u, nil
	}
	return u.NegChecked()
}

// Add returns u + o, or ErrOverflow if the sum leaves the int64 range.
func (u USD) Add(o USD) (USD, error) {
	sum := u.minor + o.minor
	if (o.minor > 0 && sum < u.minor) || (o.minor < 0 && sum > u.minor) {
		return USD{}, fmt.Errorf("%w: %s + %s", ErrOverflow, u, o)
	}
	return USD{minor: sum}, nil
}

// Sub returns u - o, or ErrOverflow if the difference leaves the int64 range.
func (u USD) Sub(o USD) (USD, error) {
	diff := u.minor - o.minor
	if (o.minor > 0 && diff > u.minor) || (o.minor < 0 && diff < u.minor) {
		return USD{}, fmt.Errorf("%w: %s - %s", ErrOverflow, u, o)
	}
	return USD{minor: diff}, nil
}

// MulInt64 returns u × n exactly, or ErrOverflow.
func (u USD) MulInt64(n int64) (USD, error) {
	prod := new(big.Int).Mul(big.NewInt(u.minor), big.NewInt(n))
	return usdFromBig(prod)
}

// MulBPS returns u × bps / 10_000 rounded to cents in the given mode.
// The product is computed exactly before the single rounding step.
//
//	USDFromMinor(10_000).MulBPS(150, RoundHalfEven)  // 100.00 × 1.50% = 1.50
//	USDFromMinor(100).MulBPS(1, RoundHalfEven)        // 1.00 × 0.01% = 0.0001 → 0.00
//	USDFromMinor(100).MulBPS(1, RoundUp)              // → 0.01
func (u USD) MulBPS(bps BPS, mode RoundingMode) (USD, error) {
	return u.MulRatio(int64(bps), int64(OneHundredPercent), mode)
}

// MulRatio returns u × num / den rounded to cents in the given mode.
// The product u × num is formed exactly in a big integer, so intermediate
// overflow is impossible; only the final result is range-checked.
// A zero den fails with ErrDivisionByZero.
func (u USD) MulRatio(num, den int64, mode RoundingMode) (USD, error) {
	if err := mode.validate(); err != nil {
		return USD{}, err
	}
	if den == 0 {
		return USD{}, ErrDivisionByZero
	}
	prod := new(big.Int).Mul(big.NewInt(u.minor), big.NewInt(num))
	q, err := divRound(prod, big.NewInt(den), mode)
	if err != nil {
		return USD{}, err
	}
	return usdFromBig(q)
}
