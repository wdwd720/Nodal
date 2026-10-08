package derivatives

import (
	"testing"

	"github.com/nodal/controlplane/internal/money"
)

// FuzzApplyFill feeds arbitrary positions and fills to the accounting core.
// It must never panic on data, and whenever it accepts a fill the result
// keeps its invariants: no position goes through zero, margin is conserved,
// and a fill closes no more than the position held.
func FuzzApplyFill(f *testing.F) {
	f.Add(int64(3), int64(1_950_000_000), int64(0), int64(-1), int64(130_000), false)
	f.Add(int64(-2), int64(-1_300_000_000), int64(500), int64(2), int64(128_000), true)
	f.Add(int64(0), int64(0), int64(0), int64(5), int64(1), false)
	f.Fuzz(func(t *testing.T, qty, cost, margin, d, priceTicks int64, option bool) {
		var s Spec
		if option {
			s = optionSpec(t, "CALL", 6_500_000)
		} else {
			s = perpSpec(t)
		}
		if margin < 0 {
			margin = -margin
		}
		st := positionState{Quantity: q(qty), CostValue: q(cost), MarginPosted: q(margin)}
		if !option {
			st.SettledValue = q(cost)
		}
		eff, err := applyFill(s, st, q(d), q(priceTicks).Mul(s.Tick))
		if err != nil {
			return
		}
		if eff.After.Quantity.Sign() != 0 && st.Quantity.Sign() != 0 && eff.After.Quantity.Sign() != st.Quantity.Sign() {
			t.Fatalf("position went through zero: %s -> %s", st.Quantity, eff.After.Quantity)
		}
		if !eff.MarginRelease.Add(eff.After.MarginPosted).Equal(st.MarginPosted) {
			t.Fatal("margin not conserved")
		}
		if !eff.Increase && eff.Closed.Abs().Cmp(st.Quantity.Abs()) > 0 {
			t.Fatal("closed more than held")
		}
		if eff.After.Quantity.IsZero() && (!eff.After.CostValue.IsZero() || !eff.After.SettledValue.IsZero()) {
			t.Fatal("a flat position keeps a cost")
		}
	})
}

// FuzzDivergence: any (mark, index) pair either is refused or yields a
// non-negative divergence that never exceeds the cap.
func FuzzDivergence(f *testing.F) {
	f.Add(int64(10_500), int32(2), int64(10_000), int32(2))
	f.Add(int64(0), int32(0), int64(1), int32(38))
	f.Fuzz(func(t *testing.T, mark int64, ms int32, index int64, xs int32) {
		d, err := DivergenceBPS(IndexSample{Mark: q(mark), MarkScale: ms, Index: q(index), IndexScale: xs})
		if err != nil {
			return
		}
		if d < 0 || d > maxDivergenceBPS {
			t.Fatalf("divergence %d out of range", d)
		}
		if _, err := PremiumIndexE8([]IndexSample{{Mark: q(mark), MarkScale: ms, Index: q(index), IndexScale: xs}}); err != nil {
			t.Fatalf("a sample divergence accepts is refused by the premium index: %v", err)
		}
		_ = money.BPS(d)
	})
}
