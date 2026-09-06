package money

import (
	"fmt"
	"math"
	"math/big"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const uint256Max = "115792089237316195423570985008687907853269984665640564039457584007913129639935"

func TestQuantity_ZeroValueIsUsable(t *testing.T) {
	var z Quantity
	require.True(t, z.IsZero())
	require.Equal(t, 0, z.Sign())
	require.Equal(t, "0", z.String())
	require.Equal(t, "0.000000", z.ToDecimalString(6))
	require.Equal(t, 0, z.Cmp(q64(0)))
	require.True(t, z.Equal(q64(0)))
	require.True(t, z.Add(q64(1)).Equal(q64(1)))
	require.True(t, z.Sub(q64(1)).Equal(q64(-1)))
	require.True(t, z.Mul(q64(9)).IsZero())
	require.True(t, z.Neg().IsZero())
	require.True(t, z.Abs().IsZero())
	require.True(t, z.ScaleUp(9).IsZero())
	n, err := z.Int64()
	require.NoError(t, err)
	require.Equal(t, int64(0), n)
	b := z.BigInt()
	require.NotNil(t, b)
	require.Equal(t, 0, b.Sign())
	d, err := z.Div(q64(3), RoundExact)
	require.NoError(t, err)
	require.True(t, d.IsZero())
	_, err = q64(3).Div(z, RoundDown)
	require.ErrorIs(t, err, ErrDivisionByZero)
	js, err := z.MarshalJSON()
	require.NoError(t, err)
	require.Equal(t, `"0"`, string(js))
}

func TestQuantity_Immutability(t *testing.T) {
	a := q64(5)
	b := a.Add(q64(1))
	require.Equal(t, "5", a.String())
	require.Equal(t, "6", b.String())
	_ = a.Neg()
	_ = a.Abs()
	_ = a.Mul(q64(3))
	_, _ = a.Div(q64(2), RoundDown)
	_ = a.MulBPS(5000, RoundDown)
	_ = a.ScaleUp(3)
	require.Equal(t, "5", a.String())

	src := big.NewInt(42)
	q := QuantityFromBigInt(src)
	src.SetInt64(99)
	require.Equal(t, "42", q.String())

	cp := q.BigInt()
	cp.SetInt64(7)
	require.Equal(t, "42", q.String())

	require.True(t, QuantityFromBigInt(nil).IsZero())

	// Copies share state safely because nothing ever mutates it.
	c := q
	d := c.Add(q64(1))
	require.Equal(t, "42", c.String())
	require.Equal(t, "43", d.String())
}

func TestParseQuantity_Valid(t *testing.T) {
	cases := []struct{ in, want string }{
		{"0", "0"},
		{"-0", "0"},
		{"000", "0"},
		{"123", "123"},
		{"-123", "-123"},
		{"000123", "123"},
		{"9223372036854775807", "9223372036854775807"},
		{"9223372036854775808", "9223372036854775808"},
		{"-9223372036854775809", "-9223372036854775809"},
		{"18446744073709551615", "18446744073709551615"},
		{uint256Max, uint256Max},
		{"-" + uint256Max, "-" + uint256Max},
		{"1" + strings.Repeat("0", 127), "1" + strings.Repeat("0", 127)}, // 10^127, 128 digits
		{strings.Repeat("9", 128), strings.Repeat("9", 128)},             // 10^128 - 1
		{strings.Repeat("0", 500) + "7", "7"},                            // leading zeros are not magnitude
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseQuantity(tc.in)
			require.NoError(t, err)
			require.Equal(t, tc.want, got.String())
			back, err := ParseQuantity(got.String())
			require.NoError(t, err)
			require.True(t, back.Equal(got))
		})
	}
}

func TestParseQuantity_Invalid(t *testing.T) {
	format := []string{
		"", "-", "+1", "+0", " 1", "1 ", "\n1", "1.0", "1.", ".1", "1e3", "1E3", "0x1", "1_0", "1,0", "1 0",
		"NaN", "Inf", "-Inf", "--1", "-+1", "1-", "١٢", "１", "abc", "1a",
		strings.Repeat("1", maxDecimalInputLen+1),
	}
	for _, in := range format {
		t.Run(fmt.Sprintf("%q", in), func(t *testing.T) {
			_, err := ParseQuantity(in)
			require.ErrorIs(t, err, ErrInvalidFormat)
		})
	}
	overflow := []string{
		"1" + strings.Repeat("0", 128),  // 10^128
		"-1" + strings.Repeat("0", 128), // -10^128
		strings.Repeat("9", 129),
	}
	for _, in := range overflow {
		t.Run(in[:8], func(t *testing.T) {
			_, err := ParseQuantity(in)
			require.ErrorIs(t, err, ErrOverflow)
		})
	}
}

func TestQuantity_Arithmetic(t *testing.T) {
	maxI := q64(math.MaxInt64)
	minI := q64(math.MinInt64)

	require.Equal(t, "9223372036854775808", maxI.Add(q64(1)).String())
	require.Equal(t, "-9223372036854775809", minI.Sub(q64(1)).String())
	require.Equal(t, "9223372036854775808", minI.Neg().String())
	require.Equal(t, "9223372036854775808", minI.Abs().String())
	require.Equal(t, "85070591730234615847396907784232501249", maxI.Mul(maxI).String())
	require.Equal(t, "-85070591730234615847396907784232501249", maxI.Mul(minI.Add(q64(1))).String())

	u256 := mustQ(t, uint256Max)
	require.Equal(t, "115792089237316195423570985008687907853269984665640564039457584007913129639936", u256.Add(q64(1)).String())
	require.True(t, u256.Sub(u256).IsZero())
	require.True(t, u256.Mul(u256).Sub(u256.Mul(u256)).IsZero())

	require.Equal(t, 1, u256.Cmp(maxI))
	require.Equal(t, -1, minI.Cmp(maxI))
	require.Equal(t, 0, maxI.Cmp(q64(math.MaxInt64)))
	require.True(t, maxI.Max(minI).Equal(maxI))
	require.True(t, maxI.Min(minI).Equal(minI))
	require.True(t, q64(3).Max(q64(3)).Equal(q64(3)))
	require.True(t, q64(-3).Abs().Equal(q64(3)))
	require.True(t, q64(0).Neg().IsZero())

	require.True(t, q64(-1).IsNegative())
	require.False(t, q64(-1).IsPositive())
	require.True(t, q64(1).IsPositive())
	require.Equal(t, -1, q64(-1).Sign())
	require.Equal(t, 1, q64(1).Sign())
}

func TestQuantity_Int64(t *testing.T) {
	n, err := q64(math.MaxInt64).Int64()
	require.NoError(t, err)
	require.Equal(t, int64(math.MaxInt64), n)
	n, err = q64(math.MinInt64).Int64()
	require.NoError(t, err)
	require.Equal(t, int64(math.MinInt64), n)
	_, err = q64(math.MaxInt64).Add(q64(1)).Int64()
	require.ErrorIs(t, err, ErrOverflow)
	_, err = q64(math.MinInt64).Sub(q64(1)).Int64()
	require.ErrorIs(t, err, ErrOverflow)
	_, err = mustQ(t, uint256Max).Int64()
	require.ErrorIs(t, err, ErrOverflow)
}

func TestQuantity_Div_Table(t *testing.T) {
	for _, c := range roundCases {
		for _, mode := range inexactModes {
			t.Run(fmt.Sprintf("%d/%d_%s", c.num, c.den, mode), func(t *testing.T) {
				got, err := q64(c.num).Div(q64(c.den), mode)
				require.NoError(t, err)
				require.Equal(t, fmt.Sprint(c.want(mode)), got.String())
			})
		}
		got, err := q64(c.num).Div(q64(c.den), RoundExact)
		if c.exact {
			require.NoError(t, err)
			require.Equal(t, fmt.Sprint(c.down), got.String())
		} else {
			require.ErrorIs(t, err, ErrPrecisionLoss)
		}
	}
	_, err := q64(1).Div(q64(0), RoundDown)
	require.ErrorIs(t, err, ErrDivisionByZero)
	_, err = q64(4).Div(q64(2), roundingUnset)
	require.ErrorIs(t, err, ErrInvalidRoundingMode)
}

func TestQuantity_MulBPS(t *testing.T) {
	cases := []struct {
		q    string
		bps  BPS
		mode RoundingMode
		want string
	}{
		{"1000000", 25, RoundDown, "2500"}, // exact
		{"1000000", 25, RoundHalfEven, "2500"},
		{"1", 5000, RoundHalfEven, "0"}, // 0.5 tie -> 0
		{"1", 5000, RoundHalfUp, "1"},
		{"1", 5000, RoundUp, "1"},
		{"1", 5000, RoundDown, "0"},
		{"1", 5000, RoundCeil, "1"},
		{"1", 5000, RoundFloor, "0"},
		{"-1", 5000, RoundHalfEven, "0"},
		{"-1", 5000, RoundHalfUp, "-1"},
		{"-1", 5000, RoundFloor, "-1"},
		{"-1", 5000, RoundCeil, "0"},
		{"-1", 5000, RoundDown, "0"},
		{"-1", 5000, RoundUp, "-1"},
		{"3", 3333, RoundHalfEven, "1"}, // 0.9999
		{"3", 3333, RoundDown, "0"},
		{"3", 5000, RoundHalfEven, "2"}, // 1.5 -> 2
		{"5", 5000, RoundHalfEven, "2"}, // 2.5 -> 2
		{"5", 5000, RoundHalfUp, "3"},
		{"1000000", -50, RoundDown, "-5000"},
		{"1000000", 10000, RoundDown, "1000000"},
		{"1000000", 20000, RoundDown, "2000000"},
		{uint256Max, 10000, RoundDown, uint256Max},
		{uint256Max, 5000, RoundHalfEven, "57896044618658097711785492504343953926634992332820282019728792003956564819968"}, // ...967.5 -> even 968
		{uint256Max, 5000, RoundDown, "57896044618658097711785492504343953926634992332820282019728792003956564819967"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s_x_%d_%s", tc.q, tc.bps, tc.mode), func(t *testing.T) {
			q := mustQ(t, tc.q)
			got := q.MulBPS(tc.bps, tc.mode)
			require.Equal(t, tc.want, got.String())
			checked, err := q.MulBPSChecked(tc.bps, tc.mode)
			require.NoError(t, err)
			require.True(t, checked.Equal(got))
		})
	}

	require.Panics(t, func() { q64(1).MulBPS(5000, roundingUnset) })
	require.Panics(t, func() { q64(1).MulBPS(5000, RoundingMode(12)) })
	require.Panics(t, func() { q64(1).MulBPS(5000, RoundExact) })
	require.Panics(t, func() { q64(2).MulBPS(5000, RoundExact) }) // even when exact: RoundExact is refused up front
	require.NotPanics(t, func() { q64(1).MulBPS(5000, RoundHalfEven) })

	_, err := q64(1).MulBPSChecked(5000, RoundExact)
	require.ErrorIs(t, err, ErrPrecisionLoss)
	got, err := q64(2).MulBPSChecked(5000, RoundExact)
	require.NoError(t, err)
	require.Equal(t, "1", got.String())
	_, err = q64(1).MulBPSChecked(5000, roundingUnset)
	require.ErrorIs(t, err, ErrInvalidRoundingMode)
}

func TestQuantity_MulDiv(t *testing.T) {
	// 7 × 3 / 2 = 10.5
	got, err := q64(7).MulDiv(q64(3), q64(2), RoundHalfEven)
	require.NoError(t, err)
	require.Equal(t, "10", got.String())
	got, err = q64(7).MulDiv(q64(3), q64(2), RoundHalfUp)
	require.NoError(t, err)
	require.Equal(t, "11", got.String())
	got, err = q64(-7).MulDiv(q64(3), q64(2), RoundFloor)
	require.NoError(t, err)
	require.Equal(t, "-11", got.String())
	// A single rounding step: (1 × 3) / 3 is exact even though 1/3 is not.
	got, err = q64(1).MulDiv(q64(3), q64(3), RoundExact)
	require.NoError(t, err)
	require.Equal(t, "1", got.String())
	_, err = q64(1).MulDiv(q64(1), q64(0), RoundDown)
	require.ErrorIs(t, err, ErrDivisionByZero)
	_, err = q64(1).MulDiv(q64(1), q64(3), RoundExact)
	require.ErrorIs(t, err, ErrPrecisionLoss)
}

func TestQuantity_Scale(t *testing.T) {
	require.Equal(t, "1500000000", q64(15).ScaleUp(8).String())
	require.Equal(t, "-1500000000", q64(-15).ScaleUp(8).String())
	require.Equal(t, "7", q64(7).ScaleUp(0).String())
	require.Equal(t, "1"+strings.Repeat("0", 255), q64(1).ScaleUp(255).String())

	got, err := q64(1500000000).ScaleDown(8, RoundExact)
	require.NoError(t, err)
	require.Equal(t, "15", got.String())
	got, err = q64(1500000000).ScaleDown(9, RoundHalfEven)
	require.NoError(t, err)
	require.Equal(t, "2", got.String())
	got, err = q64(1500000000).ScaleDown(9, RoundDown)
	require.NoError(t, err)
	require.Equal(t, "1", got.String())
	got, err = q64(-1500000000).ScaleDown(9, RoundFloor)
	require.NoError(t, err)
	require.Equal(t, "-2", got.String())
	got, err = q64(7).ScaleDown(0, RoundExact)
	require.NoError(t, err)
	require.Equal(t, "7", got.String())
	_, err = q64(1500000000).ScaleDown(9, RoundExact)
	require.ErrorIs(t, err, ErrPrecisionLoss)
	_, err = q64(1).ScaleDown(1, roundingUnset)
	require.ErrorIs(t, err, ErrInvalidRoundingMode)
}

func TestQuantity_ToDecimalString(t *testing.T) {
	cases := []struct {
		q        string
		decimals uint8
		want     string
	}{
		{"0", 0, "0"},
		{"0", 2, "0.00"},
		{"0", 6, "0.000000"},
		{"1", 6, "0.000001"},
		{"-1", 6, "-0.000001"},
		{"1500000000", 9, "1.500000000"},
		{"-1500000000", 9, "-1.500000000"},
		{"123456789", 6, "123.456789"},
		{"1000000000000000000", 18, "1.000000000000000000"},
		{"1", 18, "0.000000000000000001"},
		{"-123", 2, "-1.23"},
		{"5", 1, "0.5"},
		{"1234", 0, "1234"},
		{"-1234", 0, "-1234"},
		{"100", 2, "1.00"},
		{"225185184", 6, "225.185184"},
		{uint256Max, 18, "115792089237316195423570985008687907853269984665640564039457.584007913129639935"},
		{"1", 255, "0." + strings.Repeat("0", 254) + "1"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s@%d", tc.q, tc.decimals), func(t *testing.T) {
			q := mustQ(t, tc.q)
			require.Equal(t, tc.want, q.ToDecimalString(tc.decimals))
			back, err := QuantityFromDecimalString(tc.want, tc.decimals, RoundExact)
			require.NoError(t, err)
			require.True(t, back.Equal(q), "round trip of %q", tc.want)
		})
	}
}

func TestQuantityFromDecimalString(t *testing.T) {
	cases := []struct {
		in       string
		decimals uint8
		mode     RoundingMode
		want     string
	}{
		{"1.5", 9, RoundExact, "1500000000"},
		{"1.500000000", 9, RoundExact, "1500000000"},
		{"1.5000000000000", 9, RoundExact, "1500000000"}, // trailing zeros are exact
		{"1.5000000001", 9, RoundHalfEven, "1500000000"},
		{"1.5000000005", 9, RoundHalfEven, "1500000000"}, // tie -> even (…000)
		{"1.5000000005", 9, RoundHalfUp, "1500000001"},
		{"1.5000000015", 9, RoundHalfEven, "1500000002"}, // tie -> even (…002)
		{"1.5000000005", 9, RoundUp, "1500000001"},
		{"1.5000000005", 9, RoundDown, "1500000000"},
		{"1.5000000005", 9, RoundCeil, "1500000001"},
		{"1.5000000005", 9, RoundFloor, "1500000000"},
		{"-1.5000000005", 9, RoundFloor, "-1500000001"},
		{"-1.5000000005", 9, RoundCeil, "-1500000000"},
		{"-1.5000000005", 9, RoundDown, "-1500000000"},
		{"-1.5000000005", 9, RoundUp, "-1500000001"},
		{"-1.5000000005", 9, RoundHalfEven, "-1500000000"},
		{"-1.5000000005", 9, RoundHalfUp, "-1500000001"},
		{"0.000000001", 9, RoundExact, "1"},
		{"0.0000000000", 9, RoundExact, "0"},
		{"-0.0000000000", 9, RoundExact, "0"},
		{"0.0000000001", 9, RoundDown, "0"},
		{"0.0000000001", 9, RoundUp, "1"},
		{"123", 0, RoundExact, "123"},
		{"123.000", 0, RoundExact, "123"},
		{"123.4", 0, RoundHalfEven, "123"},
		{"123.5", 0, RoundHalfEven, "124"},
		{"124.5", 0, RoundHalfEven, "124"},
		{"-123.5", 0, RoundHalfUp, "-124"},
		{"-123.5", 0, RoundHalfEven, "-124"},
		{"-124.5", 0, RoundHalfEven, "-124"},
		{"1.5", 18, RoundExact, "1500000000000000000"},
		{"0.000000000000000001", 18, RoundExact, "1"},
		{"1", 6, RoundExact, "1000000"},
		{"007.5", 1, RoundExact, "75"},
		{"150.123456", 6, RoundExact, "150123456"},
		{"-0", 6, RoundExact, "0"},
		{"1", 0, RoundDown, "1"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s@%d_%s", tc.in, tc.decimals, tc.mode), func(t *testing.T) {
			got, err := QuantityFromDecimalString(tc.in, tc.decimals, tc.mode)
			require.NoError(t, err)
			require.Equal(t, tc.want, got.String())
		})
	}

	_, err := QuantityFromDecimalString("1.5000000001", 9, RoundExact)
	require.ErrorIs(t, err, ErrPrecisionLoss)
	_, err = QuantityFromDecimalString("0.0000000001", 9, RoundExact)
	require.ErrorIs(t, err, ErrPrecisionLoss)
	_, err = QuantityFromDecimalString("123.5", 0, RoundExact)
	require.ErrorIs(t, err, ErrPrecisionLoss)
	_, err = QuantityFromDecimalString("1", 0, roundingUnset) // no rounding needed, mode still checked
	require.ErrorIs(t, err, ErrInvalidRoundingMode)
	_, err = QuantityFromDecimalString("1", 0, RoundingMode(99))
	require.ErrorIs(t, err, ErrInvalidRoundingMode)
	_, err = QuantityFromDecimalString("1", 255, RoundExact) // 10^255 exceeds the parse bound
	require.ErrorIs(t, err, ErrOverflow)
	_, err = QuantityFromDecimalString("1"+strings.Repeat("0", 120), 9, RoundExact)
	require.ErrorIs(t, err, ErrOverflow)
	for _, bad := range []string{"", "+1.5", "1.", ".5", "1e9", "1,5", "1 .5", "NaN", "0x1", "1_000.0", "١.٥"} {
		_, err := QuantityFromDecimalString(bad, 9, RoundHalfEven)
		require.ErrorIs(t, err, ErrInvalidFormat, bad)
	}
}

func TestQuantity_DecimalRoundTrip(t *testing.T) {
	values := []string{"0", "1", "-1", "7", "1500000000", "-1500000000", "123456789012345678901234567890", uint256Max, "-" + uint256Max}
	for _, v := range values {
		q := mustQ(t, v)
		for _, decimals := range []uint8{0, 6, 9, 18} {
			s := q.ToDecimalString(decimals)
			for _, mode := range allModes {
				back, err := QuantityFromDecimalString(s, decimals, mode)
				require.NoError(t, err, "%s @%d %s", s, decimals, mode)
				require.True(t, back.Equal(q), "%s @%d %s", s, decimals, mode)
			}
			require.Equal(t, s, mustQ(t, back(t, s, decimals)).ToDecimalString(decimals))
		}
	}
}

// back re-renders a decimal string via the parser to check canonical form.
func back(t testing.TB, s string, decimals uint8) string {
	t.Helper()
	q, err := QuantityFromDecimalString(s, decimals, RoundExact)
	require.NoError(t, err)
	return q.String()
}

func TestQuantity_PrecisionBoundaries(t *testing.T) {
	// Largest parseable magnitude is 10^128 - 1 (128 nines).
	limit := mustQ(t, strings.Repeat("9", 128))
	require.Len(t, limit.String(), 128)
	// Arithmetic is not bounded by the parse limit.
	beyond := limit.Add(q64(1))
	require.Equal(t, "1"+strings.Repeat("0", 128), beyond.String())
	_, err := ParseQuantity(beyond.String())
	require.ErrorIs(t, err, ErrOverflow)

	// Smallest unit survives every scaling boundary.
	one := q64(1)
	require.Equal(t, "0.000000000000000001", one.ToDecimalString(18))
	d18, err := QuantityFromDecimalString("0.000000000000000001", 18, RoundExact)
	require.NoError(t, err)
	require.True(t, d18.Equal(one))
	_, err = QuantityFromDecimalString("0.0000000000000000001", 18, RoundExact)
	require.ErrorIs(t, err, ErrPrecisionLoss)
}
