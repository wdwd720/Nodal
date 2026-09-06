package money

import (
	"fmt"
	"math/big"
	"strings"
)

// RoundingMode selects how an inexact quotient is mapped onto an integer.
// Every API in this package that can lose precision takes one explicitly.
//
// The zero value is intentionally not a valid mode: an unset RoundingMode
// yields ErrInvalidRoundingMode instead of silently truncating.
//
// In the examples below "a/b" is the exact rational quotient and the arrow
// shows the integer produced.
//
// RoundDown rounds toward zero (truncation):
//
//	7/2 →  3     -7/2 → -3
//	5/2 →  2     -5/2 → -2
//	1/3 →  0     -1/3 →  0
//
// RoundUp rounds away from zero:
//
//	7/2 →  4     -7/2 → -4
//	1/3 →  1     -1/3 → -1
//	6/2 →  3     (exact results are never changed)
//
// RoundFloor rounds toward negative infinity:
//
//	7/2 →  3     -7/2 → -4
//	1/3 →  0     -1/3 → -1
//
// RoundCeil rounds toward positive infinity:
//
//	7/2 →  4     -7/2 → -3
//	1/3 →  1     -1/3 →  0
//
// RoundHalfEven rounds to the nearest integer; an exact tie goes to the even
// neighbor (banker's rounding):
//
//	5/2 →  2     -5/2 → -2
//	7/2 →  4     -7/2 → -4
//	3/2 →  2      2/3 →  1
//
// RoundHalfUp rounds to the nearest integer; an exact tie goes away from zero:
//
//	5/2 →  3     -5/2 → -3
//	7/2 →  4     -7/2 → -4
//	1/3 →  0      2/3 →  1
//
// RoundExact never rounds: if the quotient is not an integer the operation
// fails with ErrPrecisionLoss. Use it wherever a result must be exact by
// construction, for example when converting a decimal string that is
// required to already be at the target precision.
type RoundingMode int

const (
	roundingUnset RoundingMode = iota // zero value; always invalid
	// RoundDown rounds toward zero.
	RoundDown
	// RoundUp rounds away from zero.
	RoundUp
	// RoundHalfEven rounds to nearest, ties to even.
	RoundHalfEven
	// RoundHalfUp rounds to nearest, ties away from zero.
	RoundHalfUp
	// RoundFloor rounds toward negative infinity.
	RoundFloor
	// RoundCeil rounds toward positive infinity.
	RoundCeil
	// RoundExact fails with ErrPrecisionLoss if any rounding would occur.
	RoundExact
)

// roundingModeNames maps each valid mode to its canonical lower-case name.
// It is never mutated after initialisation.
var roundingModeNames = map[RoundingMode]string{
	RoundDown:     "down",
	RoundUp:       "up",
	RoundHalfEven: "half_even",
	RoundHalfUp:   "half_up",
	RoundFloor:    "floor",
	RoundCeil:     "ceil",
	RoundExact:    "exact",
}

// Valid reports whether m is one of the declared rounding modes.
func (m RoundingMode) Valid() bool {
	_, ok := roundingModeNames[m]
	return ok
}

// String returns the canonical name ("down", "up", "half_even", "half_up",
// "floor", "ceil", "exact"); invalid values render as "invalid(<n>)".
func (m RoundingMode) String() string {
	if name, ok := roundingModeNames[m]; ok {
		return name
	}
	return fmt.Sprintf("invalid(%d)", int(m))
}

// ParseRoundingMode parses a canonical name as produced by String.
// Matching is case-insensitive; "-" is accepted in place of "_".
func ParseRoundingMode(s string) (RoundingMode, error) {
	norm := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "-", "_")
	for m, name := range roundingModeNames {
		if name == norm {
			return m, nil
		}
	}
	return roundingUnset, fmt.Errorf("%w: %q", ErrInvalidRoundingMode, s)
}

// validate returns ErrInvalidRoundingMode for anything that is not a
// declared mode. Every rounding API calls this first, even when the specific
// inputs would not need rounding, so that a bad mode is always detected.
func (m RoundingMode) validate() error {
	if !m.Valid() {
		return fmt.Errorf("%w: %s", ErrInvalidRoundingMode, m)
	}
	return nil
}

// divRound returns num/den rounded per mode. It is the single rounding
// primitive of the package; everything else reduces to it so that negative
// values, ties and exactness behave identically everywhere.
//
// The result is a freshly allocated big.Int; num and den are not modified.
func divRound(num, den *big.Int, mode RoundingMode) (*big.Int, error) {
	if err := mode.validate(); err != nil {
		return nil, err
	}
	if den.Sign() == 0 {
		return nil, ErrDivisionByZero
	}
	quo, rem := new(big.Int).QuoRem(num, den, new(big.Int)) // truncated toward zero
	if rem.Sign() == 0 {
		return quo, nil
	}
	if mode == RoundExact {
		return nil, fmt.Errorf("%w: %s/%s is not an integer", ErrPrecisionLoss, num, den)
	}

	negative := (num.Sign() < 0) != (den.Sign() < 0)
	one := big.NewInt(1)
	awayFromZero := func() *big.Int {
		if negative {
			return quo.Sub(quo, one)
		}
		return quo.Add(quo, one)
	}

	switch mode {
	case RoundDown:
		return quo, nil
	case RoundUp:
		return awayFromZero(), nil
	case RoundFloor:
		if negative {
			return quo.Sub(quo, one), nil
		}
		return quo, nil
	case RoundCeil:
		if negative {
			return quo, nil
		}
		return quo.Add(quo, one), nil
	case RoundHalfEven, RoundHalfUp:
		// Compare 2|rem| with |den| to classify the fractional part as
		// below, exactly at, or above one half.
		twiceRem := new(big.Int).Abs(rem)
		twiceRem.Lsh(twiceRem, 1)
		switch twiceRem.Cmp(new(big.Int).Abs(den)) {
		case -1:
			return quo, nil
		case 1:
			return awayFromZero(), nil
		default: // exact tie
			if mode == RoundHalfUp {
				return awayFromZero(), nil
			}
			if quo.Bit(0) == 0 { // Bit(0) is the parity of |quo|
				return quo, nil
			}
			return awayFromZero(), nil
		}
	}
	// Unreachable: validate rejected every other value.
	return nil, fmt.Errorf("%w: %s", ErrInvalidRoundingMode, mode)
}

// pow10 returns 10^n as a new big.Int. n must be non-negative; every caller
// derives n from validated uint8/int32 inputs so a negative n is a bug.
func pow10(n int) *big.Int {
	if n < 0 {
		panic("money: pow10 called with negative exponent")
	}
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}
