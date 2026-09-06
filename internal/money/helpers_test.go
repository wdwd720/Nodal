package money

import (
	"math/big"
	"testing"

	"github.com/stretchr/testify/require"
)

// allModes lists every valid rounding mode, RoundExact last.
var allModes = []RoundingMode{RoundDown, RoundUp, RoundHalfEven, RoundHalfUp, RoundFloor, RoundCeil, RoundExact}

// inexactModes lists the modes that always produce a result.
var inexactModes = allModes[:6]

func q64(n int64) Quantity { return QuantityFromInt64(n) }

func mustQ(t testing.TB, s string) Quantity {
	t.Helper()
	q, err := ParseQuantity(s)
	require.NoError(t, err, "ParseQuantity(%q)", s)
	return q
}

func mustUSD(t testing.TB, s string) USD {
	t.Helper()
	u, err := ParseUSD(s)
	require.NoError(t, err, "ParseUSD(%q)", s)
	return u
}

func bigFromString(t testing.TB, s string) *big.Int {
	t.Helper()
	v, ok := new(big.Int).SetString(s, 10)
	require.True(t, ok, "big.Int.SetString(%q)", s)
	return v
}

// roundCase is one exact rational num/den with the expected integer under
// every non-exact mode. exact is true when den divides num.
type roundCase struct {
	num, den                                int64
	down, up, floor, ceil, halfEven, halfUp int64
	exact                                   bool
}

func (c roundCase) want(mode RoundingMode) int64 {
	switch mode {
	case RoundDown:
		return c.down
	case RoundUp:
		return c.up
	case RoundFloor:
		return c.floor
	case RoundCeil:
		return c.ceil
	case RoundHalfEven:
		return c.halfEven
	case RoundHalfUp:
		return c.halfUp
	}
	panic("no expectation for mode " + mode.String())
}

// roundCases were computed by hand from the exact quotient; the comment on
// each row is that quotient.
var roundCases = []roundCase{
	//   num,   den,  down,   up, floor, ceil, halfEven, halfUp, exact
	{7, 2, 3, 4, 3, 4, 4, 4, false},          // 3.5
	{-7, 2, -3, -4, -4, -3, -4, -4, false},   // -3.5
	{7, -2, -3, -4, -4, -3, -4, -4, false},   // -3.5 (negative divisor)
	{-7, -2, 3, 4, 3, 4, 4, 4, false},        // 3.5 (both negative)
	{5, 2, 2, 3, 2, 3, 2, 3, false},          // 2.5 tie: even is 2
	{-5, 2, -2, -3, -3, -2, -2, -3, false},   // -2.5 tie: even is -2
	{3, 2, 1, 2, 1, 2, 2, 2, false},          // 1.5 tie: even is 2
	{-3, 2, -1, -2, -2, -1, -2, -2, false},   // -1.5 tie: even is -2
	{1, 2, 0, 1, 0, 1, 0, 1, false},          // 0.5 tie: even is 0
	{-1, 2, 0, -1, -1, 0, 0, -1, false},      // -0.5 tie: even is 0
	{1, 3, 0, 1, 0, 1, 0, 0, false},          // 0.333...
	{-1, 3, 0, -1, -1, 0, 0, 0, false},       // -0.333...
	{2, 3, 0, 1, 0, 1, 1, 1, false},          // 0.666...
	{-2, 3, 0, -1, -1, 0, -1, -1, false},     // -0.666...
	{25, 10, 2, 3, 2, 3, 2, 3, false},        // 2.5
	{35, 10, 3, 4, 3, 4, 4, 4, false},        // 3.5
	{-25, 10, -2, -3, -3, -2, -2, -3, false}, // -2.5
	{-35, 10, -3, -4, -4, -3, -4, -4, false}, // -3.5
	{15, 10, 1, 2, 1, 2, 2, 2, false},        // 1.5
	{-15, 10, -1, -2, -2, -1, -2, -2, false}, // -1.5
	{9999, 10000, 0, 1, 0, 1, 1, 1, false},   // 0.9999
	{-9999, 10000, 0, -1, -1, 0, -1, -1, false},
	{10001, 10000, 1, 2, 1, 2, 1, 1, false}, // 1.0001
	{-10001, 10000, -1, -2, -2, -1, -1, -1, false},
	{6, 2, 3, 3, 3, 3, 3, 3, true},
	{-6, 3, -2, -2, -2, -2, -2, -2, true},
	{0, 5, 0, 0, 0, 0, 0, 0, true},
	{0, -5, 0, 0, 0, 0, 0, 0, true},
	{-9223372036854775808, 2, -4611686018427387904, -4611686018427387904, -4611686018427387904, -4611686018427387904, -4611686018427387904, -4611686018427387904, true},
	// MaxInt64/2 = 4611686018427387903.5; the truncated quotient is odd so half-even goes up.
	{9223372036854775807, 2, 4611686018427387903, 4611686018427387904, 4611686018427387903, 4611686018427387904, 4611686018427387904, 4611686018427387904, false},
	// MinInt64/3 = -3074457345618258602.666...
	{-9223372036854775808, 3, -3074457345618258602, -3074457345618258603, -3074457345618258603, -3074457345618258602, -3074457345618258603, -3074457345618258603, false},
}
