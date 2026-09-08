package inspect_test

import (
	"bytes"
	"sort"
	"testing"

	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/signing/inspect"
	"github.com/nodal/controlplane/internal/signing/signingtest"
)

// TestProp_ByteFlipsNeverApprove: flipping any byte of the golden message
// never yields an approval (the simulation hash binds the exact bytes, and
// every field is checked), and the inspector never panics.
func TestProp_ByteFlipsNeverApprove(t *testing.T) {
	t.Parallel()
	s := signingtest.NewSwap()
	golden := s.Golden(inspect.VersionV0)
	exp := s.Expectations(golden)
	rapid.Check(t, func(rt *rapid.T) {
		raw := append([]byte(nil), golden...)
		n := rapid.IntRange(1, 4).Draw(rt, "flips")
		for i := 0; i < n; i++ {
			pos := rapid.IntRange(0, len(raw)-1).Draw(rt, "pos")
			raw[pos] ^= byte(rapid.IntRange(1, 255).Draw(rt, "xor"))
		}
		// Two flips of the same bit cancel, and the result is the golden
		// message unchanged -- which the inspector approves, correctly. The
		// property is about MUTATED transactions and a bitwise-identical one is
		// not mutated.
		//
		// This matters more than it looks. rapid shrinks toward small values,
		// so the shrinker drives every failure toward flips=2, pos=0, xor=1 --
		// the degenerate case. Without this guard a REAL defect would be
		// shrunk into "identical bytes approved" and reported with an
		// explanation that has nothing to do with it. Discarding the case here
		// keeps the shrinker away from it.
		if bytes.Equal(raw, golden) {
			return
		}
		tx, err := inspect.Decode(raw)
		if err != nil {
			return
		}
		res := inspect.Inspect(tx, exp)
		if res.Approved {
			rt.Fatalf("mutated transaction approved")
		}
		if !sort.StringsAreSorted(res.ReasonCodes) || len(res.ReasonCodes) == 0 {
			rt.Fatalf("reason codes %v", res.ReasonCodes)
		}
	})
}

// TestProp_BoundsAreMonotonic: tightening MaxInputDebit below the route's
// in_amount, or raising MinOutput above the guaranteed minimum, always rejects;
// loosening them never changes an approval.
func TestProp_BoundsAreMonotonic(t *testing.T) {
	t.Parallel()
	s := signingtest.NewSwap()
	golden := s.Golden(inspect.VersionLegacy)
	tx, err := inspect.Decode(golden)
	if err != nil {
		t.Fatal(err)
	}
	minOut := money.QuantityFromInt64(int64(s.QuotedOut)).MulBPS(money.OneHundredPercent-money.BPS(s.SlippageBPS), money.RoundDown)
	rapid.Check(t, func(rt *rapid.T) {
		exp := s.Expectations(golden)
		exp.RequireSimulation, exp.Simulation = false, nil
		delta := rapid.Int64Range(-1_000_000, 1_000_000).Draw(rt, "delta")
		exp.MaxInputDebit = money.QuantityFromInt64(int64(s.InAmount) + delta)
		res := inspect.Inspect(tx, exp)
		if delta < 0 && res.Approved {
			rt.Fatalf("approved with max input debit below in_amount")
		}
		if delta >= 0 && !res.Approved {
			rt.Fatalf("rejected with a sufficient bound: %v", res.ReasonCodes)
		}
		exp = s.Expectations(golden)
		exp.RequireSimulation, exp.Simulation = false, nil
		d2 := rapid.Int64Range(-1_000_000_000, 1_000_000_000).Draw(rt, "mindelta")
		exp.MinOutput = minOut.Add(money.QuantityFromInt64(d2))
		if !exp.MinOutput.IsPositive() {
			return
		}
		res = inspect.Inspect(tx, exp)
		if d2 > 0 && res.Approved {
			rt.Fatalf("approved with min output above the guaranteed minimum")
		}
		if d2 <= 0 && !res.Approved {
			rt.Fatalf("rejected with a satisfiable minimum: %v", res.ReasonCodes)
		}
	})
}
