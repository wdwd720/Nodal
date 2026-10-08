package fees

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/money"
)

// TestRational_Comparisons pins the orientation of the exact-rational
// predicates every rounding proof in this package relies on.
func TestRational_Comparisons(t *testing.T) {
	half := rational{num: q64(7), den: q64(2)}  // 3.5
	whole := rational{num: q64(6), den: q64(2)} // 3
	cases := []struct {
		r                    rational
		q                    int64
		less, greater, close bool
	}{
		{half, 3, false, true, true},
		{half, 4, true, false, true},
		{half, 2, false, true, false},
		{half, 5, true, false, false},
		{whole, 3, false, false, true},
		{whole, 2, false, true, false}, // |2 − 3| = 1 is not within one unit
		{whole, 4, true, false, false},
		{rational{num: q64(-1), den: q64(3)}, 0, true, false, true},
	}
	for _, c := range cases {
		q := money.QuantityFromInt64(c.q)
		require.Equal(t, c.less, c.r.lessThan(q), "%s/%s < %d", c.r.num, c.r.den, c.q)
		require.Equal(t, c.greater, c.r.greaterThan(q), "%s/%s > %d", c.r.num, c.r.den, c.q)
		require.Equal(t, c.close, c.r.withinOneUnit(q), "|%d − %s/%s| < 1", c.q, c.r.num, c.r.den)
	}

	got, err := half.mulBPS(2_500).round(money.RoundDown) // 3.5 × 25% = 0.875
	require.NoError(t, err)
	require.Equal(t, "0", got.String())
	got, err = half.mulBPS(2_500).round(money.RoundUp)
	require.NoError(t, err)
	require.Equal(t, "1", got.String())
}

func TestTrimTrailingZeros(t *testing.T) {
	for _, c := range []struct {
		mantissa         string
		scale, min, want int32
		wantMantissa     string
	}{
		{"1504987646400", 10, 6, 8, "15049876464"},
		{"1000000", 10, 6, 6, "100"},
		{"0", 12, 6, 6, "0"},
		{"10025", 40, 36, 40, "10025"},
		{"1230", 2, 2, 2, "1230"},
	} {
		m, s := trimTrailingZeros(q(c.mantissa), c.scale, c.min)
		require.Equal(t, c.wantMantissa, m.String())
		require.Equal(t, c.want, s)
	}
}
