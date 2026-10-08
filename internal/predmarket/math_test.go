package predmarket

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

func q(n int64) money.Quantity { return money.QuantityFromInt64(n) }

func TestValidateSetQuantity_AcceptsOnlyWholeSets(t *testing.T) {
	for _, ok := range []int64{1_000_000, 2_000_000, 7_000_000_000} {
		require.NoError(t, ValidateSetQuantity(q(ok)), ok)
	}
	for _, bad := range []int64{0, -1_000_000, 1, 999_999, 1_500_000} {
		err := ValidateSetQuantity(q(bad))
		require.Error(t, err, bad)
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	}
}

func TestRefundRatePerShare_IsTheFloorOfOneNth(t *testing.T) {
	for n, want := range map[int]int64{2: 500_000, 3: 333_333, 4: 250_000, 6: 166_666, 7: 142_857, 64: 15_625} {
		got, err := RefundRatePerShare(ShareUnit(), n)
		require.NoError(t, err)
		assert.Equal(t, q(want).String(), got.String(), "N=%d", n)
	}
	_, err := RefundRatePerShare(ShareUnit(), 1)
	require.Error(t, err)
	_, err = RefundRatePerShare(ShareUnit(), 65)
	require.Error(t, err)
}

func TestPayoutFor_AWinningSharePaysOneUnitAndALosingSharePaysNothing(t *testing.T) {
	shares := []money.Quantity{q(3_000_000), q(5_000_000)}
	won, err := PayoutFor(shares, 0, ShareUnit())
	require.NoError(t, err)
	assert.Equal(t, "3000000", won.String(), "three winning shares pay three units")
	lost, err := PayoutFor([]money.Quantity{q(0), q(5_000_000)}, 0, ShareUnit())
	require.NoError(t, err)
	assert.True(t, lost.IsZero(), "losing shares pay nothing")
	one, err := PayoutFor([]money.Quantity{q(1), q(0)}, 0, ShareUnit())
	require.NoError(t, err)
	assert.Equal(t, "1", one.String(), "one share base unit pays one collateral base unit, exactly")
	_, err = PayoutFor(shares, 2, ShareUnit())
	require.Error(t, err)
}

func TestRefundFor_RoundsDownOncePerHolder(t *testing.T) {
	rate, err := RefundRatePerShare(ShareUnit(), 3)
	require.NoError(t, err)
	// One whole share of each of three outcomes refunds 3 × 333,333.
	got, err := RefundFor([]money.Quantity{q(1_000_000), q(1_000_000), q(1_000_000)}, rate)
	require.NoError(t, err)
	assert.Equal(t, "999999", got.String())
	// 1.5 shares: 1,500,000 × 333,333 / 1,000,000 = 499,999.5, rounded down once.
	got, err = RefundFor([]money.Quantity{q(1_500_000), q(0), q(0)}, rate)
	require.NoError(t, err)
	assert.Equal(t, "499999", got.String())
	// A sliver of a share refunds nothing rather than something the escrow does not owe.
	got, err = RefundFor([]money.Quantity{q(1), q(1), q(0)}, rate)
	require.NoError(t, err)
	assert.True(t, got.IsZero())
}

func holding(acct accounts.AccountID, shares ...int64) Holding {
	h := Holding{AccountID: acct}
	for _, s := range shares {
		h.Shares = append(h.Shares, q(s))
	}
	return h
}

func TestPlanPayout_RefusesBooksThatDoNotReconcile(t *testing.T) {
	a, b := accounts.NewAccountID(), accounts.NewAccountID()
	outstanding := []money.Quantity{q(2_000_000), q(2_000_000)}
	holdings := []Holding{holding(a, 2_000_000, 0), holding(b, 0, 2_000_000)}

	p, err := PlanPayout(q(2_000_000), outstanding, holdings, 1, ShareUnit())
	require.NoError(t, err)
	assert.Equal(t, "2000000", p.TotalPaid.String())
	assert.True(t, p.Residual.IsZero())

	_, err = PlanPayout(q(2_000_001), outstanding, holdings, 1, ShareUnit())
	require.Error(t, err, "escrow that does not back exactly the outstanding sets")
	assert.Equal(t, errs.CodeReconciliationRequired, errs.CodeOf(err))

	_, err = PlanPayout(q(2_000_000), outstanding, holdings[:1], 1, ShareUnit())
	require.Error(t, err, "shares outstanding that no customer holds")
	assert.Equal(t, errs.CodeReconciliationRequired, errs.CodeOf(err))

	_, err = PlanPayout(q(2_000_000), outstanding, []Holding{holdings[0], holdings[0]}, 1, ShareUnit())
	require.Error(t, err, "a holder named twice")
}

func TestPlanRefund_BinaryIsExactAndMultiOutcomeDisclosesTheResidualPerSet(t *testing.T) {
	a, b, c := accounts.NewAccountID(), accounts.NewAccountID(), accounts.NewAccountID()

	// Binary: 5 sets split between two holders in whole shares.
	p, err := PlanRefund(q(5_000_000), []money.Quantity{q(5_000_000), q(5_000_000)},
		[]Holding{holding(a, 5_000_000, 2_000_000), holding(b, 0, 3_000_000)}, ShareUnit())
	require.NoError(t, err)
	assert.Equal(t, "5000000", p.TotalPaid.String(), "binary refunds are exact")
	assert.True(t, p.Residual.IsZero())
	assert.Equal(t, "500000", p.RefundRate.String())

	// Three outcomes: 10 sets, one outcome per holder. Each set leaves
	// 1,000,000 mod 3 = 1 base unit behind.
	p, err = PlanRefund(q(10_000_000), []money.Quantity{q(10_000_000), q(10_000_000), q(10_000_000)},
		[]Holding{holding(a, 10_000_000, 0, 0), holding(b, 0, 10_000_000, 0), holding(c, 0, 0, 10_000_000)}, ShareUnit())
	require.NoError(t, err)
	assert.Equal(t, "333333", p.RefundRate.String())
	assert.Equal(t, "9999990", p.TotalPaid.String())
	assert.Equal(t, "10", p.Residual.String(), "the residual is exactly 1 base unit per complete set")
	assert.Equal(t, p.EscrowBefore.String(), p.TotalPaid.Add(p.Residual).String())
}

func TestEscrowBooks_ViolationsNameEveryBrokenLeg(t *testing.T) {
	ok := EscrowBooks{
		Escrow: q(3), Collateral: q(3),
		SharesIssued: []money.Quantity{q(3), q(3)}, SharesOutstanding: []money.Quantity{q(3), q(3)},
		HeldByCustomers: []money.Quantity{q(3), q(3)},
	}
	assert.Empty(t, ok.Violations(money.Quantity{}))
	assert.NotEmpty(t, ok.Violations(q(1)), "a residual the books do not hold")

	broken := ok
	broken.Escrow = q(4)
	assert.NotEmpty(t, broken.Violations(money.Quantity{}))
	broken = ok
	broken.HeldByCustomers = []money.Quantity{q(3), q(2)}
	assert.NotEmpty(t, broken.Violations(money.Quantity{}))
	broken = ok
	broken.SharesIssued = []money.Quantity{q(2), q(3)}
	assert.NotEmpty(t, broken.Violations(money.Quantity{}))

	refunded := EscrowBooks{
		Escrow: q(2), Collateral: q(2), SharesIssued: []money.Quantity{q(0), q(0), q(0)},
		SharesOutstanding: []money.Quantity{q(0), q(0), q(0)}, HeldByCustomers: []money.Quantity{q(0), q(0), q(0)},
	}
	assert.Empty(t, refunded.Violations(q(2)), "a voided market holds exactly its disclosed residual")
}

// --- the market model the properties drive ------------------------------------

// book is an in-memory market: holdings per account per outcome, outstanding
// shares per outcome, and the escrow, moved only by the operations the
// service performs (issue, redeem) and by secondary trades between holders.
type book struct {
	n           int
	accts       []accounts.AccountID
	holds       map[accounts.AccountID][]money.Quantity
	outstanding []money.Quantity
	escrow      money.Quantity
}

func newBook(n, holders int) *book {
	b := &book{n: n, holds: map[accounts.AccountID][]money.Quantity{}, outstanding: make([]money.Quantity, n)}
	for i := 0; i < holders; i++ {
		a := accounts.NewAccountID()
		b.accts = append(b.accts, a)
		b.holds[a] = make([]money.Quantity, n)
	}
	return b
}

func (b *book) issue(a accounts.AccountID, sets int64) {
	qty := q(sets * shareUnitBaseUnits)
	b.escrow = b.escrow.Add(qty)
	for i := 0; i < b.n; i++ {
		b.holds[a][i] = b.holds[a][i].Add(qty)
		b.outstanding[i] = b.outstanding[i].Add(qty)
	}
}

// redeem returns as many whole sets (up to want) as the account holds.
func (b *book) redeem(a accounts.AccountID, want int64) {
	minHeld := b.holds[a][0]
	for _, h := range b.holds[a] {
		minHeld = minHeld.Min(h)
	}
	whole, _ := minHeld.Div(ShareUnit(), money.RoundDown)
	sets := q(want).Min(whole)
	if !sets.IsPositive() {
		return
	}
	qty := sets.Mul(ShareUnit())
	b.escrow = b.escrow.Sub(qty)
	for i := 0; i < b.n; i++ {
		b.holds[a][i] = b.holds[a][i].Sub(qty)
		b.outstanding[i] = b.outstanding[i].Sub(qty)
	}
}

// trade moves shares of one outcome between holders (the cash leg of a
// secondary trade does not touch the market's books).
func (b *book) trade(from, to accounts.AccountID, outcome int, amount money.Quantity) {
	amount = amount.Min(b.holds[from][outcome])
	if !amount.IsPositive() || from == to {
		return
	}
	b.holds[from][outcome] = b.holds[from][outcome].Sub(amount)
	b.holds[to][outcome] = b.holds[to][outcome].Add(amount)
}

func (b *book) holdings() []Holding {
	out := make([]Holding, 0, len(b.accts))
	for _, a := range b.accts {
		out = append(out, Holding{AccountID: a, Shares: append([]money.Quantity(nil), b.holds[a]...)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AccountID.String() > out[j].AccountID.String() }) // order must not matter
	return out
}

// drive applies a random sequence of issuances, redemptions and secondary
// trades. wholeShares restricts trades to whole shares, as the book's lot
// size does.
func drive(rt *rapid.T, wholeShares bool) *book {
	n := rapid.IntRange(MinOutcomes, 8).Draw(rt, "outcomes")
	b := newBook(n, rapid.IntRange(1, 6).Draw(rt, "holders"))
	steps := rapid.IntRange(0, 40).Draw(rt, "steps")
	for i := 0; i < steps; i++ {
		a := b.accts[rapid.IntRange(0, len(b.accts)-1).Draw(rt, "account")]
		switch rapid.IntRange(0, 2).Draw(rt, "op") {
		case 0:
			b.issue(a, rapid.Int64Range(1, 50).Draw(rt, "sets"))
		case 1:
			b.redeem(a, rapid.Int64Range(1, 50).Draw(rt, "redeem"))
		default:
			to := b.accts[rapid.IntRange(0, len(b.accts)-1).Draw(rt, "to")]
			var amt money.Quantity
			if wholeShares {
				amt = q(rapid.Int64Range(1, 20).Draw(rt, "whole") * shareUnitBaseUnits)
			} else {
				amt = q(rapid.Int64Range(1, 30_000_000).Draw(rt, "base_units"))
			}
			b.trade(a, to, rapid.IntRange(0, n-1).Draw(rt, "outcome"), amt)
		}
	}
	return b
}

// TestProp_PayoutConservation: after any sequence of issuances, redemptions
// and redistributions, settlement pays exactly the escrow -- every winning
// share one unit, every losing share nothing -- and retires every
// outstanding share.
func TestProp_PayoutConservation(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		b := drive(rt, rapid.Bool().Draw(rt, "whole_shares"))
		winning := rapid.IntRange(0, b.n-1).Draw(rt, "winning")
		p, err := PlanPayout(b.escrow, b.outstanding, b.holdings(), winning, ShareUnit())
		if err != nil {
			rt.Fatalf("plan: %v", err)
		}
		if p.TotalPaid.Cmp(b.escrow) != 0 || !p.Residual.IsZero() {
			rt.Fatalf("paid %s of escrow %s (residual %s)", p.TotalPaid, b.escrow, p.Residual)
		}
		retired := make([]money.Quantity, b.n)
		for _, l := range p.Lines {
			if l.Amount.Cmp(l.Shares[winning]) != 0 {
				rt.Fatalf("holder of %s winning shares paid %s", l.Shares[winning], l.Amount)
			}
			for i, s := range l.Shares {
				retired[i] = retired[i].Add(s)
			}
		}
		for i := range retired {
			if retired[i].Cmp(b.outstanding[i]) != 0 {
				rt.Fatalf("outcome %d: retired %s of %s outstanding", i, retired[i], b.outstanding[i])
			}
		}
		for i := 1; i < len(p.Lines); i++ {
			if p.Lines[i-1].AccountID.String() >= p.Lines[i].AccountID.String() {
				rt.Fatalf("lines are not in account order")
			}
		}
	})
}

// TestProp_RefundConservesAndBoundsTheResidual: a void pays Σ refunds +
// residual = escrow exactly, never more than the escrow; with whole-share
// holdings the residual is exactly (unit mod N) per complete set (zero for a
// binary market), and with any holdings it is below that plus one base unit
// per holder.
func TestProp_RefundConservesAndBoundsTheResidual(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		whole := rapid.Bool().Draw(rt, "whole_shares")
		b := drive(rt, whole)
		p, err := PlanRefund(b.escrow, b.outstanding, b.holdings(), ShareUnit())
		if err != nil {
			rt.Fatalf("plan: %v", err)
		}
		if p.TotalPaid.Add(p.Residual).Cmp(b.escrow) != 0 || p.Residual.IsNegative() {
			rt.Fatalf("refunds %s + residual %s != escrow %s", p.TotalPaid, p.Residual, b.escrow)
		}
		sets, err := b.escrow.Div(ShareUnit(), money.RoundExact)
		if err != nil {
			rt.Fatalf("escrow %s is not whole sets", b.escrow)
		}
		perSet := ShareUnit().Sub(p.RefundRate.Mul(q(int64(b.n))))
		if whole {
			if p.Residual.Cmp(sets.Mul(perSet)) != 0 {
				rt.Fatalf("whole shares: residual %s != %s sets × %s", p.Residual, sets, perSet)
			}
			if b.n == 2 && !p.Residual.IsZero() {
				rt.Fatalf("a binary void must be exact")
			}
		} else {
			bound := sets.Mul(perSet).Add(q(int64(len(p.Lines))))
			if p.Residual.Cmp(bound) > 0 {
				rt.Fatalf("residual %s exceeds %s", p.Residual, bound)
			}
		}
	})
}
