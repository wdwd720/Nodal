package equities

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/money"
)

// TestSplit_Examples pins the arithmetic on cases a person can check.
func TestSplit_Examples(t *testing.T) {
	price := &PricePoint{Mantissa: q(30_000), Scale: 2} // 300.00 per new share
	// 2-for-1 on 1.5 shares: exactly 3 shares, nothing in lieu.
	o, err := ComputeSplit(q(1_500_000), Ratio{2, 1}, ActionSplit, 6, 6, nil, RoundingSharesDownCashDown)
	require.NoError(t, err)
	assert.Equal(t, "3000000", o.NewQuantity.String())
	assert.True(t, o.CashInLieu.IsZero())
	assert.Equal(t, "1500000", o.SharesDelta().String())

	// 1-for-3 on 10 shares: 3.333333 new shares, and 1/3 of a millionth of a
	// new share given up, worth 1/3 × 300.00 = 100 micro-dollars exactly.
	o, err = ComputeSplit(q(10_000_000), Ratio{1, 3}, ActionReverseSplit, 6, 6, price, RoundingSharesDownCashDown)
	require.NoError(t, err)
	assert.Equal(t, "3333333", o.NewQuantity.String())
	assert.Equal(t, "1", o.Remainder.String())
	assert.Equal(t, "3", o.Denominator.String())
	assert.Equal(t, "100", o.CashInLieu.String())
	assert.Equal(t, "-6666667", o.SharesDelta().String())

	// 1-for-7 on 1 share at 123.45: 0.142857 new shares and 1/7 of a
	// millionth given up, worth 123.45/7 = 17.6357... micro-dollars: 17 down,
	// 18 to the nearest.
	p2 := &PricePoint{Mantissa: q(12_345), Scale: 2}
	o, err = ComputeSplit(q(1_000_000), Ratio{1, 7}, ActionReverseSplit, 6, 6, p2, RoundingSharesDownCashDown)
	require.NoError(t, err)
	assert.Equal(t, "142857", o.NewQuantity.String())
	assert.Equal(t, "1", o.Remainder.String())
	assert.Equal(t, "17", o.CashInLieu.String())
	o, err = ComputeSplit(q(1_000_000), Ratio{1, 7}, ActionReverseSplit, 6, 6, p2, RoundingSharesDownCashHalfEven)
	require.NoError(t, err)
	assert.Equal(t, "18", o.CashInLieu.String())

	// A fraction without a price is refused rather than dropped.
	_, err = ComputeSplit(q(10_000_000), Ratio{1, 3}, ActionReverseSplit, 6, 6, nil, RoundingSharesDownCashDown)
	require.Error(t, err)
	// The direction must match the type.
	_, err = ComputeSplit(q(1), Ratio{1, 2}, ActionSplit, 6, 6, price, RoundingSharesDownCashDown)
	require.Error(t, err)
	_, err = ComputeSplit(q(1), Ratio{3, 2}, ActionReverseSplit, 6, 6, price, RoundingSharesDownCashDown)
	require.Error(t, err)
	_, err = ComputeSplit(q(1), Ratio{3, 2}, ActionSplit, 6, 6, price, "SHARES_UP")
	require.Error(t, err)
}

// TestProp_SplitPreservesEconomicValueUnderRoundingPolicy is the conservation
// claim of a split, exactly, in rationals with a common denominator:
//
//	old × P_before = new × P_after + cash-in-lieu (exact)
//
// where P_after is the cash-in-lieu price per new share and P_before =
// P_after × N / D. The shares never round up, the remainder is a proper
// fraction of one base unit, and the cash paid is within one cash base unit
// of the exact amount, in the direction the policy names.
func TestProp_SplitPreservesEconomicValueUnderRoundingPolicy(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		typ := rapid.SampledFrom([]CorporateActionType{ActionSplit, ActionReverseSplit}).Draw(rt, "type")
		a := rapid.Int64Range(1, 50).Draw(rt, "a")
		b := rapid.Int64Range(a+1, a+60).Draw(rt, "b")
		ratio := Ratio{Numerator: b, Denominator: a}
		if typ == ActionReverseSplit {
			ratio = Ratio{Numerator: a, Denominator: b}
		}
		old := q(rapid.Int64Range(0, 1_000_000_000_000).Draw(rt, "old"))
		sd := uint8(rapid.IntRange(0, 9).Draw(rt, "shareDecimals"))
		cd := uint8(rapid.IntRange(0, 9).Draw(rt, "cashDecimals"))
		price := &PricePoint{Mantissa: q(rapid.Int64Range(1, 100_000_000).Draw(rt, "price")), Scale: int32(rapid.IntRange(0, 6).Draw(rt, "scale"))}
		policy := rapid.SampledFrom(AllCorporateRoundings()).Draw(rt, "policy")
		o, err := ComputeSplit(old, ratio, typ, sd, cd, price, policy)
		if err != nil {
			rt.Fatalf("split: %v", err)
		}
		n, d := q(ratio.Numerator), q(ratio.Denominator)
		// Q × N = new × D + r, 0 <= r < D.
		if !old.Mul(n).Equal(o.NewQuantity.Mul(d).Add(o.Remainder)) || o.Remainder.IsNegative() || o.Remainder.Cmp(d) >= 0 {
			rt.Fatalf("Q×N = new×D + r broken: %+v", o)
		}
		// Value, in cash base units × D × 10^(scale+sd):
		//   before = old × m × N × 10^cd            (P_before = m/10^s × N/D)
		//   after  = new × D × m × 10^cd + exact cash numerator × (D×10^(s+sd)) / den
		common := d.Mul(pow10(int(price.Scale) + int(sd)))
		before := old.Mul(price.Mantissa).Mul(n).Mul(pow10(int(cd)))
		afterShares := o.NewQuantity.Mul(d).Mul(price.Mantissa).Mul(pow10(int(cd)))
		// exact cash = CashExactNum / CashExactDen; scale it to the common denominator.
		cashScaled, err := o.CashExactNum.MulDiv(common, o.CashExactDen, money.RoundExact)
		if err != nil {
			rt.Fatalf("the exact cash-in-lieu is not on the common denominator: %v", err)
		}
		if !before.Equal(afterShares.Add(cashScaled)) {
			rt.Fatalf("value not preserved: before %s, after %s + %s (common %s)", before, afterShares, cashScaled, common)
		}
		// The cash paid is the exact cash rounded once, within one unit.
		paid := o.CashInLieu.Mul(o.CashExactDen)
		diff := paid.Sub(o.CashExactNum)
		if diff.Abs().Cmp(o.CashExactDen) >= 0 {
			rt.Fatalf("cash-in-lieu %s is a unit or more from exact %s/%s", o.CashInLieu, o.CashExactNum, o.CashExactDen)
		}
		if policy == RoundingSharesDownCashDown && diff.IsPositive() {
			rt.Fatalf("SHARES_DOWN_CASH_DOWN paid more than exact")
		}
		if o.Remainder.IsZero() && !o.CashInLieu.IsZero() {
			rt.Fatalf("cash without a fraction")
		}
		// Shares never round up.
		if o.NewQuantity.Mul(d).Cmp(old.Mul(n)) > 0 {
			rt.Fatalf("shares rounded up")
		}
	})
}

// TestProp_DividendIsExactAndNeverOverpays: every holder's dividend is the
// exact amount rounded once in the policy's cash mode, within one cash base
// unit, and under SHARES_DOWN_CASH_DOWN the sum over holders never exceeds
// the dividend on their total holding.
func TestProp_DividendIsExactAndNeverOverpays(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		per := PricePoint{Mantissa: q(rapid.Int64Range(1, 10_000_000).Draw(rt, "per")), Scale: int32(rapid.IntRange(0, 8).Draw(rt, "scale"))}
		sd := uint8(rapid.IntRange(0, 9).Draw(rt, "sd"))
		cd := uint8(rapid.IntRange(0, 9).Draw(rt, "cd"))
		policy := rapid.SampledFrom(AllCorporateRoundings()).Draw(rt, "policy")
		holders := rapid.SliceOfN(rapid.Int64Range(1, 1_000_000_000), 1, 20).Draw(rt, "holders")
		var total, sum money.Quantity
		for _, h := range holders {
			d, err := ComputeDividend(q(h), sd, cd, per, policy)
			if err != nil {
				rt.Fatal(err)
			}
			exactNum := q(h).Mul(per.Mantissa).Mul(pow10(int(cd)))
			exactDen := pow10(int(per.Scale) + int(sd))
			if !d.ExactNum.Equal(exactNum) || !d.ExactDen.Equal(exactDen) {
				rt.Fatalf("exact amount misstated")
			}
			diff := d.Amount.Mul(exactDen).Sub(exactNum)
			if diff.Abs().Cmp(exactDen) >= 0 {
				rt.Fatalf("dividend %s is a unit or more from exact", d.Amount)
			}
			if policy == RoundingSharesDownCashDown && diff.IsPositive() {
				rt.Fatalf("dividend overpaid")
			}
			total = total.Add(q(h))
			sum = sum.Add(d.Amount)
		}
		if policy == RoundingSharesDownCashDown {
			whole, err := ComputeDividend(total, sd, cd, per, policy)
			if err != nil {
				rt.Fatal(err)
			}
			if sum.Cmp(whole.Amount) > 0 {
				rt.Fatalf("holders were paid %s, more than the %s due on their %s shares", sum, whole.Amount, total)
			}
		}
	})
}

func TestDividend_Example(t *testing.T) {
	// 0.24 per share on 12.345678 shares, cash at 6 decimals: 2.96296272 →
	// 2.962962 dollars, i.e. 2962962 micro-dollars.
	d, err := ComputeDividend(q(12_345_678), 6, 6, PricePoint{Mantissa: q(24), Scale: 2}, RoundingSharesDownCashDown)
	require.NoError(t, err)
	assert.Equal(t, "2962962", d.Amount.String())
	_, err = ComputeDividend(q(1), 6, 6, PricePoint{Mantissa: q(0), Scale: 2}, RoundingSharesDownCashDown)
	require.Error(t, err)
}
