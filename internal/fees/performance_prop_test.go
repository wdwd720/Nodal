package fees

import (
	"encoding/json"
	"math/big"
	"sort"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/money"
)

// perfCase is one generated period.
type perfCase struct {
	terms         PerformanceTerms
	snapshots     []NAVSnapshot
	closing       money.USD
	contributions []StrategyContribution
}

func cents(t *rapid.T, label string, lo, hi int64) money.USD {
	return money.USDFromMinor(rapid.Int64Range(lo, hi).Draw(t, label))
}

// distinctOffsets draws up to n distinct offsets in [lo, hi], sorted.
func distinctOffsets(t *rapid.T, label string, n int, lo, hi int64) []time.Duration {
	seen := map[int64]bool{}
	var out []time.Duration
	for range n {
		v := rapid.Int64Range(lo, hi).Draw(t, label)
		if !seen[v] {
			seen[v] = true
			out = append(out, time.Duration(v))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// genPerformance draws a valid period: NAVs up to tens of billions of
// dollars, deposits and withdrawals (never more than the NAV before them),
// a hurdle and a rate, a carried-in accrual, intermediate snapshots, and
// strategy contributions that sum to the period's gross PnL.
func genPerformance(t *rapid.T) perfCase {
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	length := rapid.Int64Range(int64(time.Hour), int64(400*24*time.Hour)).Draw(t, "length")
	opening := cents(t, "opening", 0, 1_000_000_000_000)
	hwm, _ := opening.Add(cents(t, "above_mark", 0, 1_000_000_000_000))
	c := perfCase{terms: PerformanceTerms{
		AccountRef:      "acct-prop",
		TermsVersion:    "perf-prop-v1",
		PeriodStart:     start,
		PeriodEnd:       start.Add(time.Duration(length)),
		OpeningNAV:      opening,
		PriorHWM:        hwm,
		HurdleAnnualBPS: money.BPS(rapid.Int64Range(0, 2_000).Draw(t, "hurdle")),
		FeeRateBPS:      money.BPS(rapid.Int64Range(0, 5_000).Draw(t, "rate")),
		Crystallization: CrystallizeAtPeriodEnd,
	}}
	if rapid.Bool().Draw(t, "carried_accrual") {
		c.terms.PriorAccruedFee = cents(t, "prior_accrued", 0, 100_000_000)
	}
	for _, off := range distinctOffsets(t, "flow_at", rapid.IntRange(0, 5).Draw(t, "flows"), 1, length) {
		navBefore := cents(t, "nav_before", 0, 2_000_000_000_000)
		amount := cents(t, "deposit", 1, 1_000_000_000_000)
		if navBefore.IsPositive() && rapid.Bool().Draw(t, "withdraw") {
			amount = money.USDFromMinor(-rapid.Int64Range(1, navBefore.Minor()).Draw(t, "withdrawal"))
		}
		c.terms.Flows = append(c.terms.Flows, CapitalFlow{At: start.Add(off), Amount: amount, NAVBefore: navBefore})
	}
	for _, off := range distinctOffsets(t, "snapshot_at", rapid.IntRange(0, 4).Draw(t, "snapshots"), 1, length-1) {
		c.snapshots = append(c.snapshots, NAVSnapshot{At: start.Add(off), NAV: cents(t, "snapshot_nav", 0, 3_000_000_000_000)})
	}
	c.closing = cents(t, "closing", 0, 3_000_000_000_000)

	if k := rapid.IntRange(0, 5).Draw(t, "strategies"); k > 0 {
		gross := c.closing.Minor() - c.terms.OpeningNAV.Minor()
		for _, f := range c.terms.Flows {
			gross -= f.Amount.Minor()
		}
		names := rapid.Permutation([]string{"s0", "s1", "s2", "s3", "s4"}).Draw(t, "names")[:k]
		rest := gross
		for i, name := range names {
			v := rest
			if i < k-1 {
				v = rapid.Int64Range(-1_000_000_000_000, 1_000_000_000_000).Draw(t, "contribution")
			}
			rest -= v
			c.contributions = append(c.contributions, StrategyContribution{Strategy: name, GrossPnL: money.USDFromMinor(v)})
		}
	}
	return c
}

// run accrues every snapshot, then crystallizes, returning the statement and
// the line history after each accrual.
func (c perfCase) run(t *rapid.T) (PerformanceStatement, [][]PerformanceLine) {
	p, err := OpenPerformancePeriod(c.terms)
	if err != nil {
		t.Fatalf("terms refused: %v %v", err, fieldKeys(err))
	}
	var history [][]PerformanceLine
	for _, s := range c.snapshots {
		if _, err := p.Accrue(s); err != nil {
			t.Fatalf("accrual refused: %v %v", err, fieldKeys(err))
		}
		history = append(history, p.Lines())
	}
	st, err := p.Crystallize(c.closing, c.contributions)
	if err != nil {
		t.Fatalf("crystallization refused: %v %v", err, fieldKeys(err))
	}
	return st, history
}

func bigOf(u money.USD) *big.Int { return big.NewInt(u.Minor()) }

// TestProp_PerformanceFeeBounds: the fee is never negative, never more than
// the rate applied to the period's gross gain, never charged at or below the
// flow-adjusted mark; the net NAV never exceeds the gross NAV; the new mark
// is at least the adjusted mark and the net NAV.
func TestProp_PerformanceFeeBounds(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		c := genPerformance(t)
		st, _ := c.run(t)
		fee := st.FeeAmount
		if fee.IsNegative() {
			t.Fatalf("negative fee %s", fee)
		}
		gain := st.GrossPnL
		if gain.IsNegative() {
			gain = money.USD{}
		}
		lhs := new(big.Int).Mul(bigOf(fee), big.NewInt(int64(money.OneHundredPercent)))
		rhs := new(big.Int).Mul(bigOf(gain), big.NewInt(int64(st.FeeRateBPS)))
		if lhs.Cmp(rhs) > 0 {
			t.Fatalf("fee %s exceeds max(0, gross gain %s) × %d bps", fee, st.GrossPnL, st.FeeRateBPS)
		}
		if st.ClosingNAVNet.Cmp(st.ClosingNAVGross) > 0 {
			t.Fatalf("net NAV %s above gross NAV %s", st.ClosingNAVNet, st.ClosingNAVGross)
		}
		if st.ClosingNAVGross.Cmp(st.HWMAdjusted) <= 0 && !fee.IsZero() {
			t.Fatalf("fee %s charged at or below the adjusted mark %s", fee, st.HWMAdjusted)
		}
		if st.HWMAfter.Cmp(st.HWMAdjusted) < 0 || st.HWMAfter.Cmp(st.ClosingNAVNet) < 0 {
			t.Fatalf("new mark %s below the adjusted mark %s or the net NAV %s", st.HWMAfter, st.HWMAdjusted, st.ClosingNAVNet)
		}
		if st.Hurdle.Threshold.Cmp(st.HWMAdjusted) < 0 {
			t.Fatalf("hurdle threshold %s below the adjusted mark %s", st.Hurdle.Threshold, st.HWMAdjusted)
		}
		if err := VerifyPerformanceStatement(c.terms, st); err != nil {
			t.Fatalf("replay rejects the statement: %v %v", err, fieldKeys(err))
		}
	})
}

// TestProp_PerformanceStatementHashIsDeterministic: the same period always
// produces the same bytes and hash, and the stored JSON is itself enough to
// replay and verify.
func TestProp_PerformanceStatementHashIsDeterministic(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		c := genPerformance(t)
		a, _ := c.run(t)
		b, _ := c.run(t)
		ha, err := a.Hash()
		if err != nil {
			t.Fatal(err)
		}
		hb, err := b.Hash()
		if err != nil {
			t.Fatal(err)
		}
		if ha != hb {
			t.Fatal("the same period produced different statement hashes")
		}
		raw, err := json.Marshal(a)
		if err != nil {
			t.Fatal(err)
		}
		var stored PerformanceStatement
		if err := json.Unmarshal(raw, &stored); err != nil {
			t.Fatal(err)
		}
		hs, err := stored.Hash()
		if err != nil {
			t.Fatal(err)
		}
		if hs != ha {
			t.Fatal("a stored statement re-hashes differently")
		}
		if err := VerifyPerformanceStatement(c.terms, stored); err != nil {
			t.Fatalf("stored statement does not verify: %v %v", err, fieldKeys(err))
		}
	})
}

// TestProp_AttributionSumsExactlyToTheFee: shares sum to the fee, only
// positive contributors bear it, each share is its exact pro-rata amount
// rounded down plus at most the one remainder cent, and exactly the leftover
// cents are handed out.
func TestProp_AttributionSumsExactlyToTheFee(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		c := genPerformance(t)
		if len(c.contributions) == 0 {
			c.contributions = []StrategyContribution{{Strategy: "only", GrossPnL: grossOf(c)}}
		}
		st, _ := c.run(t)
		positive := new(big.Int)
		for _, s := range c.contributions {
			if s.GrossPnL.IsPositive() {
				positive.Add(positive, bigOf(s.GrossPnL))
			}
		}
		sum := new(big.Int)
		floors := new(big.Int)
		remainderCents := int64(0)
		for i, a := range st.Attribution {
			if i > 0 && st.Attribution[i-1].Strategy >= a.Strategy {
				t.Fatal("attribution is not sorted by strategy")
			}
			sum.Add(sum, bigOf(a.FeeShare))
			if !a.Eligible {
				if !a.FeeShare.IsZero() || a.GrossPnL.IsPositive() {
					t.Fatalf("ineligible %s carries %s", a.Strategy, a.FeeShare)
				}
				continue
			}
			floor := new(big.Int).Quo(new(big.Int).Mul(bigOf(st.FeeAmount), bigOf(a.GrossPnL)), positive)
			floors.Add(floors, floor)
			extra := new(big.Int).Sub(bigOf(a.FeeShare), floor).Int64()
			if extra != a.RemainderAllocated.Minor() || extra < 0 || extra > 1 {
				t.Fatalf("%s share %s is not floor %s plus its recorded remainder %s", a.Strategy, a.FeeShare, floor, a.RemainderAllocated)
			}
			remainderCents += extra
		}
		if sum.Cmp(bigOf(st.FeeAmount)) != 0 {
			t.Fatalf("attribution sums to %s, fee is %s", sum, st.FeeAmount)
		}
		if new(big.Int).Sub(bigOf(st.FeeAmount), floors).Int64() != remainderCents {
			t.Fatal("the remainder cents handed out differ from the leftover")
		}
	})
}

func grossOf(c perfCase) money.USD {
	g := c.closing.Minor() - c.terms.OpeningNAV.Minor()
	for _, f := range c.terms.Flows {
		g -= f.Amount.Minor()
	}
	return money.USDFromMinor(g)
}

// TestProp_AccrualLinesReconcileAndAreNeverEdited: every Accrue appends one
// line and leaves earlier lines untouched; the carried-in accrual plus every
// accrual and reversal equals the crystallized fee.
func TestProp_AccrualLinesReconcileAndAreNeverEdited(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		c := genPerformance(t)
		st, history := c.run(t)
		for i, h := range history {
			if len(h) != i+1 {
				t.Fatalf("accrual %d left %d lines", i+1, len(h))
			}
			for j := range h {
				if h[j] != st.Lines[j] {
					t.Fatalf("line %d changed after it was written", j+1)
				}
			}
		}
		if len(st.Lines) != len(c.snapshots)+2 {
			t.Fatalf("%d lines for %d accruals", len(st.Lines), len(c.snapshots))
		}
		accrued := st.PriorAccruedFee.Minor()
		for _, l := range st.Lines[:len(st.Lines)-1] {
			switch {
			case l.Kind == LineAccrual && !l.Amount.IsPositive(),
				l.Kind == LineReversal && !l.Amount.IsNegative(),
				l.Kind == LineNoChange && !l.Amount.IsZero():
				t.Fatalf("line %d kind %s with amount %s", l.Seq, l.Kind, l.Amount)
			}
			accrued += l.Amount.Minor()
			if accrued != l.AccrualAfter.Minor() || l.AccrualAfter.IsNegative() {
				t.Fatalf("line %d: running accrual %d, recorded %s", l.Seq, accrued, l.AccrualAfter)
			}
		}
		last := st.Lines[len(st.Lines)-1]
		if accrued != st.FeeAmount.Minor() || last.Kind != LineCrystallization || last.Amount != st.FeeAmount {
			t.Fatalf("accruals %d do not reconcile to the crystallized fee %s", accrued, st.FeeAmount)
		}
	})
}

// TestProp_FlowsNeverCreatePerformance: when the NAV moves only by deposits
// and withdrawals, no fee is charged and the adjusted mark stays at or above
// the NAV, whatever the order and size of the flows.
func TestProp_FlowsNeverCreatePerformance(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
		opening := cents(t, "opening", 0, 1_000_000_000_000)
		hwm, _ := opening.Add(cents(t, "above_mark", 0, 1_000_000_000_000))
		terms := PerformanceTerms{
			AccountRef: "acct-flows", TermsVersion: "v1", PeriodStart: start, PeriodEnd: start.Add(31 * 24 * time.Hour),
			OpeningNAV: opening, PriorHWM: hwm, FeeRateBPS: money.BPS(rapid.Int64Range(1, 10_000).Draw(t, "rate")),
			Crystallization: CrystallizeAtPeriodEnd,
		}
		nav := opening
		for i := range rapid.IntRange(1, 8).Draw(t, "flows") {
			amount := cents(t, "deposit", 1, 1_000_000_000_000)
			if nav.IsPositive() && rapid.Bool().Draw(t, "withdraw") {
				amount = money.USDFromMinor(-rapid.Int64Range(1, nav.Minor()).Draw(t, "withdrawal"))
			}
			terms.Flows = append(terms.Flows, CapitalFlow{At: start.Add(time.Duration(i+1) * time.Hour), Amount: amount, NAVBefore: nav})
			nav, _ = nav.Add(amount)
		}
		st, err := ComputePerformanceFee(terms, nav, nil)
		if err != nil {
			t.Fatalf("%v %v", err, fieldKeys(err))
		}
		if !st.FeeAmount.IsZero() || !st.GrossPnL.IsZero() {
			t.Fatalf("flows alone produced gross %s and fee %s", st.GrossPnL, st.FeeAmount)
		}
		if st.HWMAdjusted.Cmp(nav) < 0 {
			t.Fatalf("adjusted mark %s fell below a NAV %s that never performed", st.HWMAdjusted, nav)
		}
		for _, f := range st.Flows {
			// A withdrawal keeps the remaining capital at least as far below
			// the mark as before, in relative terms, and by less than one cent
			// more: HWMafter/NAVafter ≥ HWMbefore/NAVbefore.
			if f.Amount.IsNegative() {
				l := new(big.Int).Mul(bigOf(f.HWMAfter), bigOf(f.NAVBefore))
				r := new(big.Int).Mul(bigOf(f.HWMBefore), bigOf(f.NAVAfter))
				if l.Cmp(r) < 0 || new(big.Int).Sub(l, r).Cmp(bigOf(f.NAVBefore)) >= 0 {
					t.Fatalf("withdrawal moved the mark from %s to %s for NAV %s → %s", f.HWMBefore, f.HWMAfter, f.NAVBefore, f.NAVAfter)
				}
			}
		}
	})
}
