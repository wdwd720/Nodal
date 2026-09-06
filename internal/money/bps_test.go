package money

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBPS_OneHundredPercentIsIdentity(t *testing.T) {
	require.Equal(t, BPS(10_000), OneHundredPercent)
	for _, minor := range []int64{0, 1, -1, 123456, math.MaxInt64, math.MinInt64} {
		for _, mode := range allModes {
			got, err := OneHundredPercent.ApplyUSD(USDFromMinor(minor), mode)
			require.NoError(t, err)
			require.Equal(t, minor, got.Minor())
		}
	}
	for _, q := range []Quantity{q64(0), q64(-1), q64(7), mustQ(t, "115792089237316195423570985008687907853269984665640564039457584007913129639935")} {
		for _, mode := range allModes {
			got, err := OneHundredPercent.ApplyQuantity(q, mode)
			require.NoError(t, err)
			require.True(t, got.Equal(q))
		}
	}
}

func TestBPS_ApplyQuantity(t *testing.T) {
	got, err := BPS(25).ApplyQuantity(q64(1_000_000), RoundExact)
	require.NoError(t, err)
	require.Equal(t, "2500", got.String())

	got, err = BPS(5000).ApplyQuantity(q64(1), RoundHalfEven)
	require.NoError(t, err)
	require.True(t, got.IsZero())
	got, err = BPS(5000).ApplyQuantity(q64(1), RoundHalfUp)
	require.NoError(t, err)
	require.Equal(t, "1", got.String())

	_, err = BPS(5000).ApplyQuantity(q64(1), RoundExact)
	require.ErrorIs(t, err, ErrPrecisionLoss)
	_, err = BPS(5000).ApplyQuantity(q64(1), roundingUnset)
	require.ErrorIs(t, err, ErrInvalidRoundingMode)
}

func TestBPS_String(t *testing.T) {
	cases := map[BPS]string{
		0: "0.00%", 1: "0.01%", 150: "1.50%", -25: "-0.25%", 10_000: "100.00%", 12_345: "123.45%",
		math.MinInt64: "-92233720368547758.08%",
	}
	for bps, want := range cases {
		require.Equal(t, want, bps.String())
	}
}
