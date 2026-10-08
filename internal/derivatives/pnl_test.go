package derivatives

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
)

// dollars returns a price mantissa at scale 2 for a whole-dollar price.
func dollars(d int64) money.Quantity { return q(d * 100) }

type fillStep struct {
	d     int64 // signed contracts
	price money.Quantity
	// want
	realized, cash string
	q, cost        string
}

func runSteps(t *testing.T, s Spec, steps []fillStep) positionState {
	t.Helper()
	st := positionState{}
	for i, step := range steps {
		eff, err := applyFill(s, st, q(step.d), step.price)
		require.NoError(t, err, "step %d", i)
		assert.Equal(t, step.realized, eff.RealizedPnL.String(), "step %d realized", i)
		assert.Equal(t, step.cash, eff.Cash.String(), "step %d cash", i)
		assert.Equal(t, step.q, eff.After.Quantity.String(), "step %d quantity", i)
		assert.Equal(t, step.cost, eff.After.CostValue.String(), "step %d cost", i)
		st = eff.After
	}
	return st
}

// TestFuturesPnL_IsExact walks fixed trade sequences and checks every number
// against hand-computed values. A contract is 0.01 BTC, so $1 of price is
// $0.01 = 10 000 micro-dollars of value per contract.
func TestFuturesPnL_IsExact(t *testing.T) {
	s := perpSpec(t)
	t.Run("long round trip at an average entry", func(t *testing.T) {
		st := runSteps(t, s, []fillStep{
			{d: 3, price: dollars(65_000), realized: "0", cash: "0", q: "3", cost: "1950000000"},
			{d: 1, price: dollars(66_000), realized: "0", cash: "0", q: "4", cost: "2610000000"},
			// Average entry 65 250: a quarter of the cost is 652 500 000.
			{d: -1, price: dollars(67_000), realized: "17500000", cash: "17500000", q: "3", cost: "1957500000"},
			{d: -3, price: dollars(64_000), realized: "-37500000", cash: "-37500000", q: "0", cost: "0"},
		})
		assert.True(t, st.SettledValue.IsZero())
	})
	t.Run("a partial close rounds its share half-even and the remainder telescopes", func(t *testing.T) {
		runSteps(t, s, []fillStep{
			{d: 1, price: q(6_500_000), realized: "0", cash: "0", q: "1", cost: "650000000"},
			{d: 2, price: q(6_500_050), realized: "0", cash: "0", q: "3", cost: "1950010000"},
			// 1 950 010 000 / 3 = 650 003 333.33.. -> 650 003 333
			{d: -1, price: q(6_500_000), realized: "-3333", cash: "-3333", q: "2", cost: "1300006667"},
			// The remainder carries the third: -6 667, total -10 000 exactly.
			{d: -2, price: q(6_500_000), realized: "-6667", cash: "-6667", q: "0", cost: "0"},
		})
	})
	t.Run("a short profits when the price falls", func(t *testing.T) {
		runSteps(t, s, []fillStep{
			{d: -2, price: dollars(65_000), realized: "0", cash: "0", q: "-2", cost: "-1300000000"},
			{d: 2, price: dollars(64_000), realized: "20000000", cash: "20000000", q: "0", cost: "0"},
		})
	})
}

func TestAverageEntryPrice_IsTheExactCostOverQuantityRoundedOnce(t *testing.T) {
	s := perpSpec(t)
	st := runSteps(t, s, []fillStep{
		{d: 3, price: dollars(65_000), realized: "0", cash: "0", q: "3", cost: "1950000000"},
		{d: 1, price: dollars(66_000), realized: "0", cash: "0", q: "4", cost: "2610000000"},
	})
	asPos := func(st positionState) Position { return Position{Quantity: st.Quantity, CostValue: st.CostValue} }
	p, err := AverageEntryPrice(s, asPos(st), money.RoundExact)
	require.NoError(t, err)
	assert.Equal(t, "6525000", p.String(), "(3 × 65 000 + 66 000) / 4 = 65 250.00 exactly")

	st = runSteps(t, s, []fillStep{
		{d: -1, price: q(6_500_000), realized: "0", cash: "0", q: "-1", cost: "-650000000"},
		{d: -2, price: q(6_500_050), realized: "0", cash: "0", q: "-3", cost: "-1950010000"},
	})
	_, err = AverageEntryPrice(s, asPos(st), money.RoundExact)
	require.Error(t, err, "65 000.333.. is not on the price scale; RoundExact says so")
	p, err = AverageEntryPrice(s, asPos(st), money.RoundHalfEven)
	require.NoError(t, err)
	assert.Equal(t, "6500033", p.String(), "a short's average entry, rounded half-even")
	_, err = AverageEntryPrice(s, Position{}, money.RoundHalfEven)
	require.Error(t, err)
}

func TestFuturesPnL_VariationMarginThenCloseSettlesTheSameTotal(t *testing.T) {
	s := futureSpec(t)
	eff, err := applyFill(s, positionState{}, q(2), dollars(65_000))
	require.NoError(t, err)
	st := eff.After
	vm, settled, err := variationCash(s, st, dollars(66_000))
	require.NoError(t, err)
	assert.Equal(t, "20000000", vm.String())
	st.SettledValue = settled
	closeEff, err := applyFill(s, st, q(-2), dollars(65_500))
	require.NoError(t, err)
	assert.Equal(t, "-10000000", closeEff.Cash.String(), "the close pays back what the mark gave above the exit")
	assert.Equal(t, "10000000", closeEff.RealizedPnL.String(), "realized against the entry")
	assert.Equal(t, closeEff.RealizedPnL.String(), vm.Add(closeEff.Cash).String(), "cash over the life equals realized PnL")
}

func TestFuturesPnL_RefusesAFillThroughZero(t *testing.T) {
	s := perpSpec(t)
	eff, err := applyFill(s, positionState{}, q(2), dollars(65_000))
	require.NoError(t, err)
	_, err = applyFill(s, eff.After, q(-3), dollars(65_000))
	require.Error(t, err)
	_, err = applyFill(s, eff.After, q(0), dollars(65_000))
	require.Error(t, err)
	_, err = applyFill(s, eff.After, q(-1), q(6_500_010))
	require.Error(t, err, "off the tick grid")
}

func TestOptionPnL_PremiumIsTheCashAndClosingRecognizesIt(t *testing.T) {
	s := optionSpec(t, instruments.Call, 6_500_000)
	// $500 of premium per BTC is $5 per contract.
	eff, err := applyFill(s, positionState{}, q(2), dollars(500))
	require.NoError(t, err)
	assert.Equal(t, "-10000000", eff.Cash.String(), "the buyer pays $10")
	assert.True(t, eff.After.SettledValue.IsZero(), "an option has no settled baseline")
	closeEff, err := applyFill(s, eff.After, q(-1), dollars(800))
	require.NoError(t, err)
	assert.Equal(t, "8000000", closeEff.Cash.String())
	assert.Equal(t, "5000000", closeEff.AllocatedCost.String())
	assert.Equal(t, "3000000", closeEff.RealizedPnL.String())

	w, err := applyFill(s, positionState{}, q(-3), dollars(500))
	require.NoError(t, err)
	assert.Equal(t, "15000000", w.Cash.String(), "the writer receives $15")
	buyBack, err := applyFill(s, w.After, q(3), dollars(100))
	require.NoError(t, err)
	assert.Equal(t, "-3000000", buyBack.Cash.String())
	assert.Equal(t, "12000000", buyBack.RealizedPnL.String())
}

// genFills draws a sequence of fills on one position that never crosses zero
// and ends flat, at prices on the tick grid.
func genFills(t *rapid.T, s Spec) ([]money.Quantity, []money.Quantity) {
	sign := int64(1)
	if rapid.Bool().Draw(t, "short") {
		sign = -1
	}
	var ds, prices []money.Quantity
	held := int64(0) // contracts held, a magnitude
	steps := rapid.IntRange(1, 12).Draw(t, "steps")
	for i := 0; i < steps; i++ {
		price := q(rapid.Int64Range(1, 400_000).Draw(t, "ticks")).Mul(s.Tick)
		var d int64
		if held == 0 || rapid.Bool().Draw(t, "add") {
			n := rapid.Int64Range(1, 50).Draw(t, "add_qty")
			d, held = sign*n, held+n
		} else {
			n := rapid.Int64Range(1, held).Draw(t, "reduce_qty")
			d, held = -sign*n, held-n
		}
		ds = append(ds, q(d))
		prices = append(prices, price)
	}
	if held != 0 {
		ds = append(ds, q(-sign*held))
		prices = append(prices, q(rapid.Int64Range(1, 400_000).Draw(t, "close_ticks")).Mul(s.Tick))
	}
	return ds, prices
}

func absInt(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// TestProp_RoundTripRealizedPnLIsExact: whatever the sequence of opening,
// adding, partially closing and closing fills (with variation-margin
// settlements in between for a linear contract), once the position is flat
// Σ realized PnL = Σ closing value − Σ opening value exactly, Σ cash equals
// it too, and no cost, baseline or margin remains.
func TestProp_RoundTripRealizedPnLIsExact(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		var s Spec
		if rapid.Bool().Draw(t, "option") {
			s = optionSpec(t, instruments.Call, 6_500_000)
		} else {
			s = futureSpec(t)
		}
		ds, prices := genFills(t, s)
		st := positionState{}
		realized, cash, flow := money.Quantity{}, money.Quantity{}, money.Quantity{}
		for i := range ds {
			if s.IsLinear() && !st.Quantity.IsZero() && rapid.Bool().Draw(t, "vm") {
				mark := q(rapid.Int64Range(1, 400_000).Draw(t, "vm_ticks")).Mul(s.Tick)
				vm, settled, err := variationCash(s, st, mark)
				if err != nil {
					t.Fatal(err)
				}
				cash = cash.Add(vm)
				st.SettledValue = settled
			}
			eff, err := applyFill(s, st, ds[i], prices[i])
			if err != nil {
				t.Fatal(err)
			}
			realized = realized.Add(eff.RealizedPnL)
			cash = cash.Add(eff.Cash)
			flow = flow.Sub(eff.Value)
			st = eff.After
		}
		if !st.Quantity.IsZero() || !st.CostValue.IsZero() || !st.SettledValue.IsZero() {
			t.Fatalf("flat position keeps state: %+v", st)
		}
		if !realized.Equal(flow) {
			t.Fatalf("Σ realized %s != Σ closing − Σ opening %s", realized, flow)
		}
		if !cash.Equal(realized) {
			t.Fatalf("Σ cash %s != Σ realized %s", cash, realized)
		}
	})
}

// TestProp_APartialCloseNeverReleasesMoreThanItsShareOfMargin.
func TestProp_APartialCloseNeverReleasesMoreThanItsShareOfMargin(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := perpSpec(t)
		n := rapid.Int64Range(2, 1_000).Draw(t, "contracts")
		margin := q(rapid.Int64Range(0, 1_000_000_000).Draw(t, "margin"))
		st := positionState{Quantity: q(n), CostValue: q(n).Mul(q(650_000_000)), SettledValue: q(n).Mul(q(650_000_000)), MarginPosted: margin}
		closeN := rapid.Int64Range(1, n).Draw(t, "close")
		eff, err := applyFill(s, st, q(-closeN), dollars(65_000))
		if err != nil {
			t.Fatal(err)
		}
		// kept >= margin × remaining / n, i.e. release <= margin × closed / n.
		lhs := eff.MarginRelease.Mul(q(n))
		rhs := margin.Mul(q(closeN))
		if lhs.Cmp(rhs) > 0 {
			t.Fatalf("released %s of %s for %d of %d contracts", eff.MarginRelease, margin, closeN, n)
		}
		if closeN == n && !eff.After.MarginPosted.IsZero() {
			t.Fatal("a full close keeps margin")
		}
		if !eff.MarginRelease.Add(eff.After.MarginPosted).Equal(margin) {
			t.Fatal("margin is not conserved")
		}
	})
}

// TestProp_PaperPriceIsAdverseOnTheTickGridAndWithinTheDisclosure.
func TestProp_PaperPriceIsAdverseOnTheTickGridAndWithinTheDisclosure(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := perpSpec(t)
		mark := q(rapid.Int64Range(0, 1_000_000).Draw(t, "mark_ticks")).Mul(s.Tick)
		slip := money.BPS(rapid.Int64Range(0, 500).Draw(t, "slip"))
		buy, err := paperPrice(s, mark, Buy, slip)
		if err != nil {
			t.Fatal(err)
		}
		sell, err := paperPrice(s, mark, Sell, slip)
		if err != nil {
			t.Fatal(err)
		}
		if !s.OnTick(buy) || !s.OnTick(sell) {
			t.Fatal("paper price off the tick grid")
		}
		if buy.Cmp(mark) < 0 || sell.Cmp(mark) > 0 {
			t.Fatalf("not adverse: mark %s buy %s sell %s", mark, buy, sell)
		}
		// buy − mark <= mark × slip / 10 000 + one tick (the rounding).
		bound := mark.MulBPS(slip, money.RoundCeil).Add(s.Tick)
		if buy.Sub(mark).Cmp(bound) > 0 || mark.Sub(sell).Cmp(bound) > 0 {
			t.Fatalf("slippage beyond disclosure: mark %s buy %s sell %s", mark, buy, sell)
		}
	})
}

func TestPremiumIndex_IsTheExactAverageRoundedOnce(t *testing.T) {
	cases := []struct {
		name    string
		samples []IndexSample
		want    int64
	}{
		{"1% premium", []IndexSample{{Mark: q(10_100), MarkScale: 2, Index: q(10_000), IndexScale: 2}}, 1_000_000},
		{"premiums cancel", []IndexSample{
			{Mark: q(10_100), MarkScale: 2, Index: q(10_000), IndexScale: 2},
			{Mark: q(9_900), MarkScale: 2, Index: q(10_000), IndexScale: 2},
		}, 0},
		{"different scales", []IndexSample{{Mark: q(10_100), MarkScale: 2, Index: q(100_000_000), IndexScale: 6}}, 1_000_000},
		// (4 − 3) / 3 = 0.3333.. -> 33 333 333.33 -> 33 333 333
		{"a third rounds half-even", []IndexSample{{Mark: q(4), MarkScale: 0, Index: q(3), IndexScale: 0}}, 33_333_333},
		// average of 1/3 and 1/6 = 1/4 exactly: no rounding happens per sample
		{"the average is exact before rounding", []IndexSample{
			{Mark: q(4), MarkScale: 0, Index: q(3), IndexScale: 0},
			{Mark: q(7), MarkScale: 0, Index: q(6), IndexScale: 0},
		}, 25_000_000},
		{"a discount is negative", []IndexSample{{Mark: q(9_950), MarkScale: 2, Index: q(10_000), IndexScale: 2}}, -500_000},
	}
	for _, c := range cases {
		got, err := PremiumIndexE8(c.samples)
		require.NoError(t, err, c.name)
		assert.Equal(t, c.want, got, c.name)
	}
	_, err := PremiumIndexE8(nil)
	require.Error(t, err)
	_, err = PremiumIndexE8([]IndexSample{{Mark: q(1), Index: q(0)}})
	require.Error(t, err, "an index must be positive")
}

func TestClampRate(t *testing.T) {
	r, c := ClampRate(1_000_000, 750_000)
	assert.Equal(t, int64(750_000), r)
	assert.True(t, c)
	r, c = ClampRate(-1_000_000, 750_000)
	assert.Equal(t, int64(-750_000), r)
	assert.True(t, c)
	r, c = ClampRate(500, 750_000)
	assert.Equal(t, int64(500), r)
	assert.False(t, c)
}

func TestFundingPerContractIsRoundedDownOnce(t *testing.T) {
	s := perpSpec(t)
	per, err := fundingPerContract(s, dollars(65_000), 1_000_000) // 1%
	require.NoError(t, err)
	assert.Equal(t, "6500000", per.String(), "1% of $650")
	per, err = fundingPerContract(s, dollars(65_000), -1)
	require.NoError(t, err)
	assert.Equal(t, "6", per.String(), "650 000 000 × 10^-8 = 6.5 -> 6")
	assert.Equal(t, FundingPay, fundingDirection(q(3), 10))
	assert.Equal(t, FundingReceive, fundingDirection(q(-3), 10))
	assert.Equal(t, FundingReceive, fundingDirection(q(3), -10))
	assert.Equal(t, FundingPay, fundingDirection(q(-3), -10))
}

// TestProp_FundingConservesOnABalancedBook: for any book whose long contracts
// equal its short contracts, at any mark and rate, Σ paid = Σ received
// exactly; for a one-sided book the difference is exactly the per-contract
// amount times the net open interest -- what the clearing house absorbs.
func TestProp_FundingConservesOnABalancedBook(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := perpSpec(t)
		mark := q(rapid.Int64Range(1, 400_000).Draw(t, "mark_ticks")).Mul(s.Tick)
		rate := rapid.Int64Range(-750_000, 750_000).Draw(t, "rate")
		per, err := fundingPerContract(s, mark, rate)
		if err != nil {
			t.Fatal(err)
		}
		longs := rapid.SliceOfN(rapid.Int64Range(1, 1_000), 1, 8).Draw(t, "longs")
		var total int64
		for _, l := range longs {
			total += l
		}
		// Split the same total across a different number of shorts.
		var shorts []int64
		left := total
		for left > 0 {
			n := rapid.Int64Range(1, left).Draw(t, "short")
			shorts = append(shorts, -n)
			left -= n
		}
		extra := rapid.Int64Range(0, 100).Draw(t, "one_sided_extra")
		book := append(append([]int64{}, longs...), shorts...)
		if extra > 0 {
			book = append(book, extra)
		}
		paid, received := money.Quantity{}, money.Quantity{}
		for _, p := range book {
			amt := q(absInt(p)).Mul(per)
			if fundingDirection(q(p), rate) == FundingPay {
				paid = paid.Add(amt)
			} else {
				received = received.Add(amt)
			}
		}
		net := q(extra).Mul(per)
		if rate < 0 {
			net = net.Neg()
		}
		if rate == 0 {
			net = money.Quantity{}
		}
		if !paid.Sub(received).Equal(net) {
			t.Fatalf("paid %s received %s, want difference %s", paid, received, net)
		}
	})
}

func TestDivergenceBPS(t *testing.T) {
	d, err := DivergenceBPS(IndexSample{Mark: q(10_500), MarkScale: 2, Index: q(10_000), IndexScale: 2})
	require.NoError(t, err)
	assert.Equal(t, money.BPS(500), d)
	d, err = DivergenceBPS(IndexSample{Mark: q(9_500), MarkScale: 2, Index: q(10_000), IndexScale: 2})
	require.NoError(t, err)
	assert.Equal(t, money.BPS(500), d, "a discount diverges as much as a premium")
	d, err = DivergenceBPS(IndexSample{Mark: q(1_000_050), MarkScale: 4, Index: q(10_000), IndexScale: 2})
	require.NoError(t, err)
	assert.Equal(t, money.BPS(1), d, "half a basis point rounds up")
	d, err = DivergenceBPS(IndexSample{Mark: q(10_000), MarkScale: 2, Index: q(10_000), IndexScale: 2})
	require.NoError(t, err)
	assert.Equal(t, money.BPS(0), d)
}
