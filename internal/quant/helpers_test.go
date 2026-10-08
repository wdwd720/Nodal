package quant

import (
	"math/big"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// allModes lists every valid rounding mode, RoundExact last.
func allModes() []money.RoundingMode {
	return []money.RoundingMode{
		money.RoundDown, money.RoundUp, money.RoundHalfEven, money.RoundHalfUp,
		money.RoundFloor, money.RoundCeil, money.RoundExact,
	}
}

// inexactModes lists the modes that always produce a result.
func inexactModes() []money.RoundingMode { return allModes()[:6] }

// d parses a decimal literal or fails the test.
func d(t testing.TB, s string) Dec {
	t.Helper()
	v, err := ParseDec(s)
	require.NoError(t, err, "ParseDec(%q)", s)
	return v
}

// dr parses a literal with any number of fractional digits, rounding to
// Scale half-even (for long reference constants).
func dr(t testing.TB, s string) Dec {
	t.Helper()
	v, err := ParseDecRound(s, money.RoundHalfEven)
	require.NoError(t, err, "ParseDecRound(%q)", s)
	return v
}

// ulps returns n units in the last place, n × 10^-40.
func ulps(n int64) Dec { return Dec{c: big.NewInt(n)} }

// requireCode asserts that err is an *errs.Error carrying code.
func requireCode(t testing.TB, err error, code errs.Code) *errs.Error {
	t.Helper()
	require.Error(t, err)
	e, ok := errs.As(err)
	require.True(t, ok, "error %v is not an *errs.Error", err)
	require.Equal(t, code, e.Code, "error %v", err)
	return e
}

// requireDecEqual compares two Decs by value with readable output.
func requireDecEqual(t testing.TB, want, got Dec, msgAndArgs ...any) {
	t.Helper()
	require.Equal(t, want.String(), got.String(), msgAndArgs...)
}
