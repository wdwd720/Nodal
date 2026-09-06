package money

import (
	"fmt"
	"math/big"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRoundingMode_ZeroValueIsInvalid(t *testing.T) {
	var m RoundingMode
	require.False(t, m.Valid())
	require.ErrorIs(t, m.validate(), ErrInvalidRoundingMode)
	require.Equal(t, "invalid(0)", m.String())
	require.False(t, RoundingMode(99).Valid())
	require.False(t, RoundingMode(-1).Valid())
	require.False(t, RoundExact+1 == roundingUnset || (RoundExact+1).Valid())
	for _, mode := range allModes {
		require.True(t, mode.Valid(), mode)
		require.NoError(t, mode.validate())
	}
}

func TestRoundingMode_StringAndParse(t *testing.T) {
	want := map[RoundingMode]string{
		RoundDown: "down", RoundUp: "up", RoundHalfEven: "half_even", RoundHalfUp: "half_up",
		RoundFloor: "floor", RoundCeil: "ceil", RoundExact: "exact",
	}
	for mode, name := range want {
		require.Equal(t, name, mode.String())
		parsed, err := ParseRoundingMode(name)
		require.NoError(t, err)
		require.Equal(t, mode, parsed)
	}
	parsed, err := ParseRoundingMode(" Half-Even ")
	require.NoError(t, err)
	require.Equal(t, RoundHalfEven, parsed)
	for _, bad := range []string{"", "nearest", "invalid(0)", "0", "round_down"} {
		_, err := ParseRoundingMode(bad)
		require.ErrorIs(t, err, ErrInvalidRoundingMode, bad)
	}
}

func TestDivRound_Table(t *testing.T) {
	for _, c := range roundCases {
		for _, mode := range inexactModes {
			t.Run(fmt.Sprintf("%d/%d_%s", c.num, c.den, mode), func(t *testing.T) {
				got, err := divRound(big.NewInt(c.num), big.NewInt(c.den), mode)
				require.NoError(t, err)
				require.True(t, got.IsInt64())
				require.Equal(t, c.want(mode), got.Int64())
			})
		}
		t.Run(fmt.Sprintf("%d/%d_exact", c.num, c.den), func(t *testing.T) {
			got, err := divRound(big.NewInt(c.num), big.NewInt(c.den), RoundExact)
			if c.exact {
				require.NoError(t, err)
				require.Equal(t, c.down, got.Int64())
			} else {
				require.ErrorIs(t, err, ErrPrecisionLoss)
			}
		})
	}
}

func TestDivRound_DoesNotMutateInputs(t *testing.T) {
	num, den := big.NewInt(-7), big.NewInt(2)
	for _, mode := range inexactModes {
		_, err := divRound(num, den, mode)
		require.NoError(t, err)
		require.Equal(t, int64(-7), num.Int64())
		require.Equal(t, int64(2), den.Int64())
	}
}

func TestDivRound_Errors(t *testing.T) {
	_, err := divRound(big.NewInt(1), big.NewInt(0), RoundDown)
	require.ErrorIs(t, err, ErrDivisionByZero)

	// An invalid mode is rejected even when no rounding would be needed.
	_, err = divRound(big.NewInt(4), big.NewInt(2), roundingUnset)
	require.ErrorIs(t, err, ErrInvalidRoundingMode)
	_, err = divRound(big.NewInt(4), big.NewInt(2), RoundingMode(42))
	require.ErrorIs(t, err, ErrInvalidRoundingMode)

	// Mode validation precedes the zero-divisor check.
	_, err = divRound(big.NewInt(1), big.NewInt(0), roundingUnset)
	require.ErrorIs(t, err, ErrInvalidRoundingMode)
}

func TestDivRound_LargeValues(t *testing.T) {
	// (10^40 + 5) / 10 = 10^39 + 0.5: a tie whose truncated quotient is even.
	num := bigFromString(t, "10000000000000000000000000000000000000005")
	den := big.NewInt(10)
	even := bigFromString(t, "1000000000000000000000000000000000000000")
	odd := new(big.Int).Add(even, big.NewInt(1))

	got, err := divRound(num, den, RoundHalfEven)
	require.NoError(t, err)
	require.Equal(t, 0, got.Cmp(even))

	got, err = divRound(num, den, RoundHalfUp)
	require.NoError(t, err)
	require.Equal(t, 0, got.Cmp(odd))

	got, err = divRound(new(big.Int).Neg(num), den, RoundFloor)
	require.NoError(t, err)
	require.Equal(t, 0, got.Cmp(new(big.Int).Neg(odd)))
}

func TestPow10(t *testing.T) {
	require.Equal(t, "1", pow10(0).String())
	require.Equal(t, "10", pow10(1).String())
	require.Equal(t, "1000000000000000000", pow10(18).String())
	require.Len(t, pow10(38).String(), 39)
	require.Panics(t, func() { pow10(-1) })
}
