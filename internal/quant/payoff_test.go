package quant

import (
	"math"
	"math/big"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

func qty(t testing.TB, s string) money.Quantity {
	t.Helper()
	q, err := money.ParseQuantity(s)
	require.NoError(t, err)
	return q
}

// TestPayoffGrid checks the exercise values over a grid of spots and
// strikes, including zero, equal and 30-digit values, against an
// independent big.Int computation, and the expiry put-call parity
// C − P = S − K, exactly.
func TestPayoffGrid(t *testing.T) {
	big30 := "1" + strings.Repeat("0", 30)
	values := []string{"0", "1", "2", "99", "100", "101", "150000000", "999999999999", big30, big30 + "1", "9223372036854775807", "9223372036854775808"}
	for _, ss := range values {
		for _, ks := range values {
			s, k := qty(t, ss), qty(t, ks)
			diff := new(big.Int).Sub(s.BigInt(), k.BigInt())
			wantCall, wantPut := new(big.Int), new(big.Int)
			if diff.Sign() > 0 {
				wantCall.Set(diff)
			} else {
				wantPut.Neg(diff)
			}
			c, p := IntrinsicCall(s, k), IntrinsicPut(s, k)
			require.Equal(t, wantCall.String(), c.String(), "call S=%s K=%s", ss, ks)
			require.Equal(t, wantPut.String(), p.String(), "put S=%s K=%s", ss, ks)
			require.False(t, c.IsNegative() || p.IsNegative())
			require.True(t, c.Sub(p).Equal(s.Sub(k)), "parity S=%s K=%s", ss, ks)
			require.True(t, c.IsZero() || p.IsZero(), "at most one leg pays")
		}
	}
	// Inputs are never modified.
	s, k := qty(t, "150"), qty(t, "100")
	_ = IntrinsicCall(s, k)
	_ = IntrinsicPut(s, k)
	require.Equal(t, "150", s.String())
	require.Equal(t, "100", k.String())
}

func TestProp_PayoffMonotoneAndConvex(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		k := money.QuantityFromInt64(rapid.Int64Range(0, math.MaxInt32).Draw(rt, "k"))
		s := money.QuantityFromInt64(rapid.Int64Range(0, math.MaxInt32).Draw(rt, "s"))
		h := money.QuantityFromInt64(rapid.Int64Range(1, 1_000_000).Draw(rt, "h"))
		// Monotone in spot: calls rise, puts fall.
		if IntrinsicCall(s.Add(h), k).Cmp(IntrinsicCall(s, k)) < 0 || IntrinsicPut(s.Add(h), k).Cmp(IntrinsicPut(s, k)) > 0 {
			rt.Fatalf("payoff not monotone at S=%s K=%s", s, k)
		}
		// Convex in spot.
		two := money.QuantityFromInt64(2)
		for _, f := range []func(a, b money.Quantity) money.Quantity{IntrinsicCall, IntrinsicPut} {
			lhs := f(s.Add(h), k).Add(f(s.Sub(h).Max(money.Quantity{}), k))
			if s.Cmp(h) >= 0 && lhs.Cmp(f(s, k).Mul(two)) < 0 {
				rt.Fatalf("payoff not convex at S=%s K=%s h=%s", s, k, h)
			}
		}
	})
}

func TestScenarioGrid(t *testing.T) {
	base := qty(t, "1000003")
	shocks := []money.BPS{-10_000, -2_500, -1, 0, 1, 2_500, 10_000, 33_333}
	// exact: base × (10_000 + s) / 10_000
	exact := func(s money.BPS) *big.Rat {
		return new(big.Rat).SetFrac(new(big.Int).Mul(base.BigInt(), big.NewInt(10_000+int64(s))), big.NewInt(10_000))
	}
	for _, mode := range inexactModes() {
		got := ScenarioGrid(base, shocks, mode)
		require.Len(t, got, len(shocks))
		for i, s := range shocks {
			want := exact(s)
			g := new(big.Rat).SetInt(got[i].BigInt())
			diff := new(big.Rat).Sub(g, want)
			require.True(t, new(big.Rat).Abs(diff).Cmp(big.NewRat(1, 1)) < 0, "%s shock %d: %s vs %s", mode, s, got[i], want.FloatString(4))
			switch mode {
			case money.RoundFloor, money.RoundDown:
				require.True(t, diff.Sign() <= 0)
			case money.RoundCeil, money.RoundUp:
				require.True(t, diff.Sign() >= 0)
			}
		}
		require.True(t, got[0].IsZero(), "−100% is a zero price")
		require.Equal(t, base.String(), got[3].String(), "zero shock is the base")
		require.Equal(t, "2000006", got[6].String(), "+100% doubles")
	}
	// Exact values in a convenient case.
	require.Equal(t, []string{"0", "75", "99", "100", "101", "125", "200"},
		quantityStrings(ScenarioGrid(qty(t, "100"), []money.BPS{-10_000, -2_500, -100, 0, 100, 2_500, 10_000}, money.RoundHalfEven)))
	// Shocks below −100% give negative values (rejected by the checked form).
	require.Equal(t, "-50", ScenarioGrid(qty(t, "100"), []money.BPS{-15_000}, money.RoundHalfEven)[0].String())
	// Huge shocks cannot overflow: the factor is formed in a big integer.
	huge := ScenarioGrid(qty(t, "10000"), []money.BPS{money.BPS(math.MaxInt64)}, money.RoundDown)[0]
	want := new(big.Int).Add(big.NewInt(math.MaxInt64), big.NewInt(10_000))
	require.Equal(t, want.String(), huge.String())
	require.Empty(t, ScenarioGrid(base, nil, money.RoundDown))

	require.Panics(t, func() { ScenarioGrid(base, shocks, money.RoundExact) })
	require.Panics(t, func() { ScenarioGrid(base, shocks, 0) })
}

func quantityStrings(qs []money.Quantity) []string {
	out := make([]string, len(qs))
	for i, q := range qs {
		out[i] = q.String()
	}
	return out
}

func TestScenarioGridChecked(t *testing.T) {
	base := qty(t, "1000003")
	shocks := []money.BPS{-10_000, -2_500, 0, 1, 2_500}
	for _, mode := range inexactModes() {
		got, err := ScenarioGridChecked(base, shocks, mode)
		require.NoError(t, err)
		require.Equal(t, quantityStrings(ScenarioGrid(base, shocks, mode)), quantityStrings(got))
	}
	got, err := ScenarioGridChecked(qty(t, "100"), []money.BPS{-2_500, 0, 5_000}, money.RoundExact)
	require.NoError(t, err)
	require.Equal(t, []string{"75", "100", "150"}, quantityStrings(got))

	_, err = ScenarioGridChecked(base, []money.BPS{1}, money.RoundExact)
	requireCode(t, err, errs.CodePrecisionLoss)
	_, err = ScenarioGridChecked(base, []money.BPS{0, -10_001}, money.RoundDown)
	e := requireCode(t, err, errs.CodeValidationFailed)
	require.Equal(t, "shocks_bps", e.Fields["field"])
	require.Equal(t, 1, e.Fields["index"])
	_, err = ScenarioGridChecked(qty(t, "-1"), shocks, money.RoundDown)
	requireCode(t, err, errs.CodeValidationFailed)
	_, err = ScenarioGridChecked(base, shocks, 0)
	requireCode(t, err, errs.CodeValidationFailed)
}

// TestScenarioWorstCaseLoss shows the intended composition: the exact
// worst-case payoff of a short call over a shock grid is attained at the
// largest shocked spot and is an exact integer.
func TestScenarioWorstCaseLoss(t *testing.T) {
	spot, strike := qty(t, "65000000000"), qty(t, "70000000000") // micro-USDC
	grid := ScenarioGrid(spot, []money.BPS{-3000, -1500, 0, 1500, 3000}, money.RoundCeil)
	worst := money.Quantity{}
	for _, s := range grid {
		worst = worst.Max(IntrinsicCall(s, strike))
	}
	require.Equal(t, "14500000000", worst.String()) // 65e9 × 1.3 − 70e9
}

func TestProp_ScenarioGrid(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		base := money.QuantityFromInt64(rapid.Int64Range(0, math.MaxInt64).Draw(rt, "base"))
		n := rapid.IntRange(0, 8).Draw(rt, "n")
		shocks := make([]money.BPS, n)
		for i := range shocks {
			shocks[i] = money.BPS(rapid.Int64Range(-10_000, 100_000).Draw(rt, "shock"))
		}
		mode := rapid.SampledFrom(inexactModes()).Draw(rt, "mode")
		got := ScenarioGrid(base, shocks, mode)
		checked, err := ScenarioGridChecked(base, shocks, mode)
		if err != nil {
			rt.Fatal(err)
		}
		for i, s := range shocks {
			if !got[i].Equal(checked[i]) || got[i].IsNegative() {
				rt.Fatalf("shock %d: %s vs %s", s, got[i], checked[i])
			}
			want, err := base.MulBPSChecked(money.OneHundredPercent+s, mode)
			if err != nil || !want.Equal(got[i]) {
				rt.Fatalf("shock %d: %s vs MulBPS %s (%v)", s, got[i], want, err)
			}
		}
	})
}
