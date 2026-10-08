package marketplace

import (
	"math/big"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

func points(equities, turnovers []string) []FollowPoint {
	t0 := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	out := make([]FollowPoint, len(equities))
	for i := range equities {
		out[i] = FollowPoint{AsOf: t0.Add(time.Duration(i) * time.Hour), Equity: equities[i], Turnover: turnovers[i]}
	}
	return out
}

func TestPerformance_IsExactOnAKnownHistory(t *testing.T) {
	p, err := ComputePerformance(points(
		[]string{"1000.00", "1100.00", "990.00", "1210.00"},
		[]string{"0", "150.25", "0", "100.00"}))
	require.NoError(t, err)
	assert.Equal(t, int64(2100), p.ReturnBPS, "(1210-1000)/1000")
	assert.Equal(t, int64(1000), p.MaxDrawdownBPS, "(1100-990)/1100")
	// Per-run returns 1000, -1000 and 2222.222222 BPS: population SD
	// sqrt(1764060.35...) = 1328.18..., rounded down.
	assert.Equal(t, int64(1328), p.VolatilityBPS)
	// 250.25 / mean(1075) = 0.23279..., half-even.
	assert.Equal(t, int64(2328), p.TurnoverBPS)
	assert.Equal(t, 4, p.Points)
	assert.Len(t, p.HistoryDigest, 32)

	single, err := ComputePerformance(points([]string{"500.5"}, []string{"0"}))
	require.NoError(t, err)
	assert.Zero(t, single.ReturnBPS)
	assert.Zero(t, single.MaxDrawdownBPS)
	assert.Zero(t, single.VolatilityBPS)
}

func TestPerformance_RefusesWhatItCannotComputeExactly(t *testing.T) {
	_, err := ComputePerformance(nil)
	assert.Error(t, err)
	_, err = ComputePerformance(points([]string{"0", "10"}, []string{"0", "0"}))
	assert.Error(t, err, "a return from zero equity")
	_, err = ComputePerformance(points([]string{"-1", "10"}, []string{"0", "0"}))
	assert.Error(t, err)
	_, err = ComputePerformance(points([]string{"1.5e3"}, []string{"0"}))
	assert.Error(t, err, "floats are not decimals")
	unordered := points([]string{"1", "2"}, []string{"0", "0"})
	unordered[1].AsOf = unordered[0].AsOf
	_, err = ComputePerformance(unordered)
	assert.Error(t, err)
}

func TestDivHalfEven_RoundsTiesToEven(t *testing.T) {
	for _, c := range []struct{ num, den, want int64 }{
		{5, 2, 2}, {7, 2, 4}, {-5, 2, -2}, {-7, 2, -4}, {1, 3, 0}, {2, 3, 1}, {-2, 3, -1}, {6, 3, 2},
	} {
		got := divHalfEven(big.NewInt(c.num), big.NewInt(c.den))
		assert.Equal(t, c.want, got.Int64(), "%d/%d", c.num, c.den)
	}
}

func genHistory(rt *rapid.T) ([]int64, []int64) {
	n := rapid.IntRange(1, 12).Draw(rt, "n")
	eq := make([]int64, n)
	tv := make([]int64, n)
	for i := range eq {
		eq[i] = rapid.Int64Range(1, 10_000_000).Draw(rt, "equity")
		tv[i] = rapid.Int64Range(0, 1_000_000).Draw(rt, "turnover")
	}
	return eq, tv
}

func render(vals []int64, k int64, scale int) []string {
	out := make([]string, len(vals))
	for i, v := range vals {
		s := strconv.FormatInt(v*k, 10)
		if scale > 0 {
			for len(s) <= scale {
				s = "0" + s
			}
			s = s[:len(s)-scale] + "." + s[len(s)-scale:]
		}
		out[i] = s
	}
	return out
}

// TestProp_PerformanceIsScaleInvariant: performance figures are ratios, so
// multiplying every equity and turnover by the same factor -- or writing
// them at a different decimal scale -- changes nothing.
func TestProp_PerformanceIsScaleInvariant(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		eq, tv := genHistory(rt)
		base, err := ComputePerformance(points(render(eq, 1, 2), render(tv, 1, 2)))
		if err != nil {
			rt.Fatal(err)
		}
		k := rapid.Int64Range(2, 1000).Draw(rt, "k")
		scaled, err := ComputePerformance(points(render(eq, k, 2), render(tv, k, 2)))
		if err != nil {
			rt.Fatal(err)
		}
		rescaled, err := ComputePerformance(points(render(eq, 1000, 5), render(tv, 1000, 5)))
		if err != nil {
			rt.Fatal(err)
		}
		for _, other := range []Performance{scaled, rescaled} {
			if other.ReturnBPS != base.ReturnBPS || other.MaxDrawdownBPS != base.MaxDrawdownBPS ||
				other.VolatilityBPS != base.VolatilityBPS || other.TurnoverBPS != base.TurnoverBPS {
				rt.Fatalf("figures changed under scaling: %+v vs %+v", base, other)
			}
		}
	})
}

// TestProp_PerformanceBounds: drawdown is within [0, 100%], a history that
// never falls has no drawdown, a flat history has no return and no
// volatility, and the same history always gives the same figures.
func TestProp_PerformanceBounds(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		eq, tv := genHistory(rt)
		p, err := ComputePerformance(points(render(eq, 1, 2), render(tv, 1, 2)))
		if err != nil {
			rt.Fatal(err)
		}
		if p.MaxDrawdownBPS < 0 || p.MaxDrawdownBPS > 10_000 || p.VolatilityBPS < 0 || p.TurnoverBPS < 0 {
			rt.Fatalf("out of bounds: %+v", p)
		}
		again, _ := ComputePerformance(points(render(eq, 1, 2), render(tv, 1, 2)))
		if string(again.HistoryDigest) != string(p.HistoryDigest) || again.ReturnBPS != p.ReturnBPS {
			rt.Fatal("not deterministic")
		}
		rising := append([]int64(nil), eq...)
		for i := 1; i < len(rising); i++ {
			if rising[i] < rising[i-1] {
				rising[i] = rising[i-1]
			}
		}
		r, err := ComputePerformance(points(render(rising, 1, 2), render(tv, 1, 2)))
		if err != nil {
			rt.Fatal(err)
		}
		if r.MaxDrawdownBPS != 0 {
			rt.Fatalf("a history that never falls has drawdown %d", r.MaxDrawdownBPS)
		}
		flat := make([]int64, len(eq))
		for i := range flat {
			flat[i] = eq[0]
		}
		f, err := ComputePerformance(points(render(flat, 1, 2), render(tv, 1, 2)))
		if err != nil {
			rt.Fatal(err)
		}
		if f.ReturnBPS != 0 || f.VolatilityBPS != 0 || f.MaxDrawdownBPS != 0 {
			rt.Fatalf("a flat history has figures %+v", f)
		}
	})
}
