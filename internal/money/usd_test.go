package money

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUSD_Bounds(t *testing.T) {
	require.Equal(t, int64(math.MaxInt64), MaxUSD().Minor())
	require.Equal(t, int64(math.MinInt64), MinUSD().Minor())
	require.Equal(t, MaxUSDMinor, MaxUSD().Minor())
	require.Equal(t, MinUSDMinor, MinUSD().Minor())
	require.Equal(t, "92233720368547758.07", MaxUSD().String())
	require.Equal(t, "-92233720368547758.08", MinUSD().String())

	require.Equal(t, MaxUSD(), mustUSD(t, "92233720368547758.07"))
	require.Equal(t, MinUSD(), mustUSD(t, "-92233720368547758.08"))

	_, err := ParseUSD("92233720368547758.08")
	require.ErrorIs(t, err, ErrOverflow)
	_, err = ParseUSD("-92233720368547758.09")
	require.ErrorIs(t, err, ErrOverflow)
	_, err = ParseUSD("99999999999999999999.00")
	require.ErrorIs(t, err, ErrOverflow)

	oneCent := USDFromMinor(1)
	_, err = MaxUSD().Add(oneCent)
	require.ErrorIs(t, err, ErrOverflow)
	_, err = MinUSD().Sub(oneCent)
	require.ErrorIs(t, err, ErrOverflow)

	sum, err := MaxUSD().Add(MinUSD())
	require.NoError(t, err)
	require.Equal(t, USDFromMinor(-1), sum)
}

func TestUSD_MinimumUnit(t *testing.T) {
	require.Equal(t, "0.01", USDFromMinor(1).String())
	require.Equal(t, "-0.01", USDFromMinor(-1).String())
	require.Equal(t, int64(1), mustUSD(t, "0.01").Minor())
	require.Equal(t, int64(-1), mustUSD(t, "-0.01").Minor())

	two, err := USDFromMinor(1).Add(USDFromMinor(1))
	require.NoError(t, err)
	require.Equal(t, "0.02", two.String())

	// Half a cent is below the minimum unit: it must be rejected or rounded
	// only when a mode is named.
	_, err = ParseUSD("0.005")
	require.ErrorIs(t, err, ErrPrecisionLoss)
	for _, tc := range []struct {
		mode RoundingMode
		want int64
	}{
		{RoundDown, 0}, {RoundUp, 1}, {RoundFloor, 0}, {RoundCeil, 1}, {RoundHalfEven, 0}, {RoundHalfUp, 1},
	} {
		got, err := ParseUSDRound("0.005", tc.mode)
		require.NoError(t, err, tc.mode)
		require.Equal(t, tc.want, got.Minor(), tc.mode)
	}
	_, err = ParseUSDRound("0.005", RoundExact)
	require.ErrorIs(t, err, ErrPrecisionLoss)
}

func TestParseUSD_Valid(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		str  string
	}{
		{"0", 0, "0.00"},
		{"0.0", 0, "0.00"},
		{"0.00", 0, "0.00"},
		{"-0", 0, "0.00"},
		{"-0.00", 0, "0.00"},
		{"1", 100, "1.00"},
		{"1.5", 150, "1.50"},
		{"1.05", 105, "1.05"},
		{"1234.56", 123456, "1234.56"},
		{"-1234.56", -123456, "-1234.56"},
		{"-0.01", -1, "-0.01"},
		{"0.01", 1, "0.01"},
		{"0.10", 10, "0.10"},
		{"007.50", 750, "7.50"},
		{"000", 0, "0.00"},
		{"9223372036854775807", math.MaxInt64 / 100 * 100, "9223372036854775807.00"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseUSD(tc.in)
			if tc.in == "9223372036854775807" {
				// 9.2e18 dollars is beyond the range; keep as an overflow case.
				require.ErrorIs(t, err, ErrOverflow)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got.Minor())
			require.Equal(t, tc.str, got.String())
			// Every mode of ParseUSDRound agrees on inputs with <= 2 decimals.
			for _, mode := range allModes {
				r, err := ParseUSDRound(tc.in, mode)
				require.NoError(t, err, mode)
				require.Equal(t, got, r, mode)
			}
		})
	}
}

func TestParseUSD_Invalid(t *testing.T) {
	format := []string{
		"", " ", " 1", "1 ", "\t1", "1\n", "+5", "+0", "1.", ".5", "-.5", "-", "--1", "1-",
		"1e5", "1E5", "1e-2", "0x10", "1_000", "1,000", "1 000", "NaN", "nan", "Inf", "-Inf", "inf", "Infinity",
		"1.2.3", "1..2", "1.-5", "abc", "$1", "1$", "１", "١", "1.5\x00", "0b1", "0o7",
		strings.Repeat("1", maxUSDInputLen+1),
	}
	for _, in := range format {
		t.Run(fmt.Sprintf("format_%q", in), func(t *testing.T) {
			_, err := ParseUSD(in)
			require.ErrorIs(t, err, ErrInvalidFormat)
			_, err = ParseUSDRound(in, RoundHalfEven)
			require.ErrorIs(t, err, ErrInvalidFormat)
		})
	}
	precision := []string{"1.234", "1.230", "0.001", "0.000", "-0.001", "1234.5678"}
	for _, in := range precision {
		t.Run(fmt.Sprintf("precision_%q", in), func(t *testing.T) {
			_, err := ParseUSD(in)
			require.ErrorIs(t, err, ErrPrecisionLoss)
		})
	}
}

func TestParseUSDRound(t *testing.T) {
	cases := []struct {
		in   string
		mode RoundingMode
		want int64
	}{
		{"0.015", RoundHalfEven, 2}, // 1.5 -> 2 (even)
		{"0.015", RoundHalfUp, 2},
		{"0.025", RoundHalfEven, 2}, // 2.5 -> 2 (even)
		{"0.025", RoundHalfUp, 3},
		{"0.025", RoundDown, 2},
		{"0.025", RoundUp, 3},
		{"0.025", RoundFloor, 2},
		{"0.025", RoundCeil, 3},
		{"-0.025", RoundHalfEven, -2},
		{"-0.025", RoundHalfUp, -3},
		{"-0.025", RoundDown, -2},
		{"-0.025", RoundUp, -3},
		{"-0.025", RoundFloor, -3},
		{"-0.025", RoundCeil, -2},
		{"1.2300", RoundExact, 123},
		{"1.230000000000000000000000000000", RoundExact, 123},
		{"1.999", RoundDown, 199},
		{"1.999", RoundUp, 200},
		{"1.999", RoundHalfEven, 200},
		{"-1.999", RoundFloor, -200},
		{"-1.999", RoundCeil, -199},
		{"1.5", RoundDown, 150},
		{"0.001", RoundHalfEven, 0},
		{"0.009", RoundHalfEven, 1},
		{"92233720368547758.074", RoundHalfEven, math.MaxInt64},
		{"92233720368547758.075", RoundDown, math.MaxInt64},
		{"-92233720368547758.085", RoundHalfEven, math.MinInt64}, // tie, quotient even
		{"-92233720368547758.085", RoundCeil, math.MinInt64},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s_%s", tc.in, tc.mode), func(t *testing.T) {
			got, err := ParseUSDRound(tc.in, tc.mode)
			require.NoError(t, err)
			require.Equal(t, tc.want, got.Minor())
		})
	}

	_, err := ParseUSDRound("1.234", RoundExact)
	require.ErrorIs(t, err, ErrPrecisionLoss)
	_, err = ParseUSDRound("92233720368547758.075", RoundHalfUp)
	require.ErrorIs(t, err, ErrOverflow)
	_, err = ParseUSDRound("-92233720368547758.085", RoundHalfUp)
	require.ErrorIs(t, err, ErrOverflow)
	_, err = ParseUSDRound("1.00", roundingUnset)
	require.ErrorIs(t, err, ErrInvalidRoundingMode)
	_, err = ParseUSDRound("1.00", RoundingMode(77))
	require.ErrorIs(t, err, ErrInvalidRoundingMode)
}

func TestUSD_String(t *testing.T) {
	cases := map[int64]string{
		0: "0.00", 1: "0.01", -1: "-0.01", 5: "0.05", 50: "0.50", 99: "0.99", -99: "-0.99",
		100: "1.00", -100: "-1.00", 101: "1.01", 123456: "1234.56", -123456: "-1234.56",
		1000000000: "10000000.00", math.MaxInt64: "92233720368547758.07", math.MinInt64: "-92233720368547758.08",
	}
	for minor, want := range cases {
		require.Equal(t, want, USDFromMinor(minor).String(), minor)
	}
}

func TestUSD_StringParseRoundTrip(t *testing.T) {
	for _, minor := range []int64{0, 1, -1, 9, 10, 99, 100, 101, -101, 123456, -123456, math.MaxInt64, math.MinInt64, math.MaxInt64 - 1, math.MinInt64 + 1} {
		u := USDFromMinor(minor)
		back, err := ParseUSD(u.String())
		require.NoError(t, err)
		require.Equal(t, u, back)
	}
	for _, s := range []string{"0.00", "1234.56", "-0.01", "92233720368547758.07", "-92233720368547758.08"} {
		require.Equal(t, s, mustUSD(t, s).String())
	}
}

func TestUSD_AddSub(t *testing.T) {
	add := []struct {
		a, b   int64
		want   int64
		overfl bool
	}{
		{1, 2, 3, false},
		{-1, 1, 0, false},
		{math.MaxInt64, 0, math.MaxInt64, false},
		{math.MaxInt64, -1, math.MaxInt64 - 1, false},
		{math.MaxInt64, 1, 0, true},
		{math.MinInt64, -1, 0, true},
		{math.MinInt64, 1, math.MinInt64 + 1, false},
		{math.MaxInt64, math.MaxInt64, 0, true},
		{math.MinInt64, math.MinInt64, 0, true},
		{math.MaxInt64, math.MinInt64, -1, false},
		{math.MaxInt64 / 2, math.MaxInt64/2 + 1, math.MaxInt64, false},
		{math.MaxInt64 / 2, math.MaxInt64/2 + 2, 0, true},
	}
	for _, tc := range add {
		got, err := USDFromMinor(tc.a).Add(USDFromMinor(tc.b))
		if tc.overfl {
			require.ErrorIs(t, err, ErrOverflow, "%d + %d", tc.a, tc.b)
			continue
		}
		require.NoError(t, err, "%d + %d", tc.a, tc.b)
		require.Equal(t, tc.want, got.Minor(), "%d + %d", tc.a, tc.b)
	}

	sub := []struct {
		a, b   int64
		want   int64
		overfl bool
	}{
		{3, 2, 1, false},
		{0, 0, 0, false},
		{math.MaxInt64, -1, 0, true},
		{math.MinInt64, 1, 0, true},
		{0, math.MinInt64, 0, true},
		{-1, math.MinInt64, math.MaxInt64, false},
		{math.MinInt64, math.MinInt64, 0, false},
		{math.MaxInt64, math.MaxInt64, 0, false},
		{0, math.MaxInt64, -math.MaxInt64, false},
		{-2, math.MaxInt64, 0, true},
		{math.MinInt64, -1, math.MinInt64 + 1, false},
	}
	for _, tc := range sub {
		got, err := USDFromMinor(tc.a).Sub(USDFromMinor(tc.b))
		if tc.overfl {
			require.ErrorIs(t, err, ErrOverflow, "%d - %d", tc.a, tc.b)
			continue
		}
		require.NoError(t, err, "%d - %d", tc.a, tc.b)
		require.Equal(t, tc.want, got.Minor(), "%d - %d", tc.a, tc.b)
	}
}

func TestUSD_NegAbs(t *testing.T) {
	require.Equal(t, USDFromMinor(-5), USDFromMinor(5).Neg())
	require.Equal(t, USDFromMinor(5), USDFromMinor(-5).Neg())
	require.Equal(t, USDFromMinor(0), USDFromMinor(0).Neg())
	require.Equal(t, USDFromMinor(-math.MaxInt64), MaxUSD().Neg())

	require.Panics(t, func() { MinUSD().Neg() })
	_, err := MinUSD().NegChecked()
	require.ErrorIs(t, err, ErrOverflow)
	n, err := USDFromMinor(-7).NegChecked()
	require.NoError(t, err)
	require.Equal(t, USDFromMinor(7), n)

	a, err := USDFromMinor(-5).Abs()
	require.NoError(t, err)
	require.Equal(t, USDFromMinor(5), a)
	a, err = USDFromMinor(5).Abs()
	require.NoError(t, err)
	require.Equal(t, USDFromMinor(5), a)
	a, err = USDFromMinor(0).Abs()
	require.NoError(t, err)
	require.True(t, a.IsZero())
	_, err = MinUSD().Abs()
	require.ErrorIs(t, err, ErrOverflow)
	a, err = USDFromMinor(math.MinInt64 + 1).Abs()
	require.NoError(t, err)
	require.Equal(t, MaxUSD(), a)
}

func TestUSD_Predicates(t *testing.T) {
	zero, pos, neg := USDFromMinor(0), USDFromMinor(3), USDFromMinor(-3)
	require.True(t, zero.IsZero())
	require.False(t, zero.IsPositive())
	require.False(t, zero.IsNegative())
	require.Equal(t, 0, zero.Sign())
	require.True(t, pos.IsPositive())
	require.False(t, pos.IsNegative())
	require.Equal(t, 1, pos.Sign())
	require.True(t, neg.IsNegative())
	require.Equal(t, -1, neg.Sign())

	require.Equal(t, -1, neg.Cmp(pos))
	require.Equal(t, 1, pos.Cmp(neg))
	require.Equal(t, 0, pos.Cmp(USDFromMinor(3)))
	require.True(t, pos.Equal(USDFromMinor(3)))
	require.False(t, pos.Equal(neg))
	require.Equal(t, -1, MinUSD().Cmp(MaxUSD()))

	var zeroValue USD
	require.True(t, zeroValue.IsZero())
	require.Equal(t, "0.00", zeroValue.String())
}

func TestUSD_MulBPS(t *testing.T) {
	cases := []struct {
		minor int64
		bps   BPS
		mode  RoundingMode
		want  int64
	}{
		{10_000, 150, RoundHalfEven, 150}, // 100.00 × 1.50% = 1.50 exact
		{10_000, 150, RoundExact, 150},
		{10_000, OneHundredPercent, RoundExact, 10_000},
		{10_000, 0, RoundExact, 0},
		{10_000, -50, RoundExact, -50}, // rebate
		{100, 1, RoundHalfEven, 0},     // 1.00 × 0.01% = 0.0001 -> 0.00
		{100, 1, RoundHalfUp, 0},
		{100, 1, RoundUp, 1},
		{100, 1, RoundCeil, 1},
		{100, 1, RoundDown, 0},
		{3333, 3333, RoundDown, 1110}, // 33.33 × 33.33% = 11.108889
		{3333, 3333, RoundHalfEven, 1111},
		{3333, 3333, RoundUp, 1111},
		{-3333, 3333, RoundDown, -1110},
		{-3333, 3333, RoundFloor, -1111},
		{-3333, 3333, RoundCeil, -1110},
		{-3333, 3333, RoundHalfEven, -1111},
		{5, 5000, RoundHalfEven, 2}, // 0.05 × 50% = 0.025 -> tie -> even
		{5, 5000, RoundHalfUp, 3},
		{15, 5000, RoundHalfEven, 8}, // 0.075 -> tie -> even (8)
		{15, 5000, RoundHalfUp, 8},
		{-5, 5000, RoundHalfEven, -2},
		{-5, 5000, RoundHalfUp, -3},
		{-5, 5000, RoundFloor, -3},
		{-5, 5000, RoundCeil, -2},
		{math.MaxInt64, OneHundredPercent, RoundExact, math.MaxInt64},
		{math.MinInt64, OneHundredPercent, RoundExact, math.MinInt64},
		{math.MaxInt64, 5000, RoundHalfEven, 4611686018427387904}, // MaxInt64/2 = ...903.5 -> even is ...904
		{math.MaxInt64, 5000, RoundDown, 4611686018427387903},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d_x_%d_%s", tc.minor, tc.bps, tc.mode), func(t *testing.T) {
			got, err := USDFromMinor(tc.minor).MulBPS(tc.bps, tc.mode)
			require.NoError(t, err)
			require.Equal(t, tc.want, got.Minor())
			// BPS.ApplyUSD is the same operation.
			viaBPS, err := tc.bps.ApplyUSD(USDFromMinor(tc.minor), tc.mode)
			require.NoError(t, err)
			require.Equal(t, got, viaBPS)
		})
	}

	_, err := USDFromMinor(100).MulBPS(1, RoundExact)
	require.ErrorIs(t, err, ErrPrecisionLoss)
	_, err = MaxUSD().MulBPS(10_001, RoundDown)
	require.ErrorIs(t, err, ErrOverflow)
	_, err = MaxUSD().MulBPS(20_000, RoundDown)
	require.ErrorIs(t, err, ErrOverflow)
	_, err = MinUSD().MulBPS(-OneHundredPercent, RoundDown) // = MaxInt64 + 1
	require.ErrorIs(t, err, ErrOverflow)
	_, err = USDFromMinor(100).MulBPS(1, roundingUnset)
	require.ErrorIs(t, err, ErrInvalidRoundingMode)
	_, err = USDFromMinor(100).MulBPS(OneHundredPercent, RoundingMode(9)) // exact, but mode still checked
	require.ErrorIs(t, err, ErrInvalidRoundingMode)
}

func TestUSD_MulRatio(t *testing.T) {
	cases := []struct {
		minor    int64
		num, den int64
		mode     RoundingMode
		want     int64
	}{
		{1000, 1, 3, RoundHalfEven, 333}, // 10.00 / 3 = 3.3333
		{1000, 1, 3, RoundDown, 333},
		{1000, 1, 3, RoundUp, 334},
		{1000, 2, 3, RoundHalfEven, 667}, // 6.6667
		{1000, 2, 3, RoundDown, 666},
		{1000, -1, 3, RoundDown, -333},
		{1000, -1, 3, RoundFloor, -334},
		{1000, -1, 3, RoundUp, -334},
		{1000, -1, 3, RoundHalfEven, -333},
		{1000, 1, -3, RoundFloor, -334},
		{1000, -1, -3, RoundFloor, 333},
		{1000, 3, 3, RoundExact, 1000},
		{math.MaxInt64, 2, 2, RoundExact, math.MaxInt64},                         // exact intermediate product
		{math.MaxInt64, math.MaxInt64, math.MaxInt64, RoundExact, math.MaxInt64}, // 2^126 intermediate
		{math.MinInt64, -1, -1, RoundExact, math.MinInt64},
		{math.MinInt64, 1, 2, RoundExact, math.MinInt64 / 2},
		{0, math.MaxInt64, 1, RoundExact, 0},
		{7, 0, 5, RoundExact, 0},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d_x_%d/%d_%s", tc.minor, tc.num, tc.den, tc.mode), func(t *testing.T) {
			got, err := USDFromMinor(tc.minor).MulRatio(tc.num, tc.den, tc.mode)
			require.NoError(t, err)
			require.Equal(t, tc.want, got.Minor())
		})
	}

	_, err := USDFromMinor(1000).MulRatio(1, 0, RoundHalfEven)
	require.ErrorIs(t, err, ErrDivisionByZero)
	_, err = USDFromMinor(1000).MulRatio(1, 3, RoundExact)
	require.ErrorIs(t, err, ErrPrecisionLoss)
	_, err = MaxUSD().MulRatio(2, 1, RoundDown)
	require.ErrorIs(t, err, ErrOverflow)
	_, err = MinUSD().MulRatio(1, -1, RoundDown)
	require.ErrorIs(t, err, ErrOverflow)
	_, err = MinUSD().MulRatio(-1, 1, RoundDown)
	require.ErrorIs(t, err, ErrOverflow)
	_, err = USDFromMinor(1000).MulRatio(1, 1, roundingUnset)
	require.ErrorIs(t, err, ErrInvalidRoundingMode)
	_, err = USDFromMinor(1000).MulRatio(1, 0, roundingUnset) // mode checked first
	require.ErrorIs(t, err, ErrInvalidRoundingMode)
}

func TestUSD_MulInt64(t *testing.T) {
	got, err := USDFromMinor(150).MulInt64(3)
	require.NoError(t, err)
	require.Equal(t, int64(450), got.Minor())
	got, err = USDFromMinor(-150).MulInt64(3)
	require.NoError(t, err)
	require.Equal(t, int64(-450), got.Minor())
	got, err = MinUSD().MulInt64(1)
	require.NoError(t, err)
	require.Equal(t, MinUSD(), got)
	got, err = MaxUSD().MulInt64(0)
	require.NoError(t, err)
	require.True(t, got.IsZero())
	_, err = MaxUSD().MulInt64(2)
	require.ErrorIs(t, err, ErrOverflow)
	_, err = MinUSD().MulInt64(-1)
	require.ErrorIs(t, err, ErrOverflow)
	_, err = USDFromMinor(math.MaxInt64/2 + 1).MulInt64(2)
	require.ErrorIs(t, err, ErrOverflow)
}
