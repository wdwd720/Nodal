package prediction

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/strategy/ir"
)

// prob builds a scale-6 probability from its decimal string.
func prob(t *testing.T, s string) ir.Decimal {
	t.Helper()
	d, err := ir.ParseDecimalString(s)
	require.NoError(t, err, "parse %q", s)
	n, err := NormalizeProbability(d)
	require.NoError(t, err, "normalize %q", s)
	return n
}

func TestBrier(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		p       string
		outcome bool
		want    string
	}{
		{"certain and right", "1.0", true, "0.00000000"},
		{"certain and wrong", "0.0", true, "1.00000000"},
		{"coin flip", "0.5", true, "0.25000000"},
		{"coin flip other way", "0.5", false, "0.25000000"},
		{"confident and right", "0.9", true, "0.01000000"},
		{"confident and wrong", "0.9", false, "0.81000000"},
		{"zero on a false outcome", "0.0", false, "0.00000000"},
		{"six decimals kept", "0.123456", true, "0.76832938"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Brier(prob(t, tc.p), tc.outcome)
			require.NoError(t, err)
			require.Equal(t, tc.want, got.String())
			require.Equal(t, ScoreScale, got.Scale)
		})
	}
}

func TestLogLoss(t *testing.T) {
	t.Parallel()
	eps := DefaultEpsilon()
	cases := []struct {
		name    string
		p       string
		outcome bool
		want    string // -ln(q), scale 8
	}{
		{"coin flip is ln 2", "0.5", true, "0.69314718"},
		{"coin flip other way is ln 2", "0.5", false, "0.69314718"},
		{"certain and right is the epsilon floor", "1.0", true, "0.00000100"},
		{"certain and wrong is the epsilon ceiling", "0.0", true, "13.81551056"},
		{"three quarters right", "0.75", true, "0.28768207"},
		{"quarter wrong is three quarters right", "0.25", false, "0.28768207"},
		{"nine tenths right", "0.9", true, "0.10536052"},
		{"one tenth right", "0.1", true, "2.30258509"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := LogLoss(prob(t, tc.p), tc.outcome, eps)
			require.NoError(t, err)
			require.Equal(t, tc.want, got.String())
			require.Equal(t, ScoreScale, got.Scale)
		})
	}
}

func TestMaxLogLoss(t *testing.T) {
	t.Parallel()
	got, err := MaxLogLoss(DefaultEpsilon())
	require.NoError(t, err)
	require.Equal(t, "13.81551056", got.String())
}

func TestLogLossRejectsBadEpsilon(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"0.0", "0.5", "0.9", "1.0"} {
		_, err := LogLoss(prob(t, "0.5"), true, prob(t, bad))
		require.Error(t, err, "epsilon %s must be refused", bad)
	}
}
