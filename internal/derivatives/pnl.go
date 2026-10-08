package derivatives

import (
	"math/big"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// The pure, exact accounting of a position. Every value is in base units of
// the settlement asset and every function here is deterministic integer
// arithmetic.
//
// A position carries three signed sums over its open contracts:
//
//   - cost_value: what the contracts were opened at (Σ q × V(fill price)).
//     Unrealized PnL is Q × V(mark) − cost_value.
//   - settled_value: the baseline cash has already been settled against.
//     For a future or a perpetual it starts at the entry value and moves to
//     Q × V(mark) at every variation-margin settlement; the cash of a
//     settlement is the difference. For an option it is always zero: the
//     whole premium changed hands when the option was traded, so an option's
//     only later cash is what closing it, exercising it or settling it pays.
//   - margin_posted: initial margin held in DERIVATIVES_MARGIN.
//
// A reducing fill allocates the closed share of cost_value and of
// settled_value. The share of a partial reduction is rounded half-even (the
// one rounding in this file), and the remainder stays with the open
// contracts, so the allocations of a position telescope: over the life of a
// position the realized PnL is exactly Σ closing value − Σ opening value and
// the cash is exactly the same number.

// allocationRounding is the rounding of a partial reduction's share of a
// position's cost and settled values.
const allocationRounding = money.RoundHalfEven

// positionState is the economic part of a position.
type positionState struct {
	Quantity     money.Quantity
	CostValue    money.Quantity
	SettledValue money.Quantity
	MarginPosted money.Quantity
}

func (p Position) state() positionState {
	return positionState{Quantity: p.Quantity, CostValue: p.CostValue, SettledValue: p.SettledValue, MarginPosted: p.MarginPosted}
}

// fillEffect is what one fill does to a position.
type fillEffect struct {
	// Increase is true when the fill opens or adds to the position.
	Increase bool
	// Value is d × V(price), signed.
	Value money.Quantity
	// Closed is the signed number of contracts the fill closed (same sign as
	// the position); zero for an increase.
	Closed money.Quantity
	// AllocatedCost and AllocatedSettled are the closed contracts' shares of
	// cost_value and settled_value.
	AllocatedCost    money.Quantity
	AllocatedSettled money.Quantity
	// RealizedPnL is the closing value less the allocated cost.
	RealizedPnL money.Quantity
	// Cash is what the customer receives (> 0) or pays (< 0) the clearing
	// house for this fill: an option's premium, a future's realized
	// difference against its settled baseline.
	Cash money.Quantity
	// MarginRelease is the initial margin a reduction frees.
	MarginRelease money.Quantity
	After         positionState
}

// applyFill computes the effect of a fill of d (signed) contracts at a price
// on the tick grid. A fill that would take the position through zero is
// refused: a reversal is a close and an open.
func applyFill(s Spec, before positionState, d, price money.Quantity) (fillEffect, error) {
	if d.IsZero() {
		return fillEffect{}, errs.New(errs.CodeValidationFailed, "derivatives: a fill has a non-zero quantity")
	}
	value, err := s.Value(d, price)
	if err != nil {
		return fillEffect{}, err
	}
	q := before.Quantity
	if q.IsZero() || q.Sign() == d.Sign() {
		after := positionState{
			Quantity:     q.Add(d),
			CostValue:    before.CostValue.Add(value),
			SettledValue: before.SettledValue,
			MarginPosted: before.MarginPosted,
		}
		eff := fillEffect{Increase: true, Value: value, After: after}
		if s.IsOption() {
			// The premium changes hands now: a buyer pays, a writer receives.
			eff.Cash = value.Neg()
		} else {
			// A future's trade moves no cash; its value becomes the baseline
			// the next settlement is measured from.
			after.SettledValue = before.SettledValue.Add(value)
			eff.After = after
		}
		return eff, nil
	}
	closed := d.Neg()
	if closed.Abs().Cmp(q.Abs()) > 0 {
		return fillEffect{}, errs.New(errs.CodeValidationFailed,
			"derivatives: a fill may not take a position through zero; close it, then open the other side").
			WithField("position_quantity", q.String()).WithField("fill_quantity", d.String())
	}
	allocCost, err := allocate(before.CostValue, closed, q)
	if err != nil {
		return fillEffect{}, err
	}
	allocSettled, err := allocate(before.SettledValue, closed, q)
	if err != nil {
		return fillEffect{}, err
	}
	closingValue := value.Neg() // the closed contracts' value at the fill price: closed × V(price)
	remaining := q.Add(d)
	kept, err := keptMargin(before.MarginPosted, remaining, q)
	if err != nil {
		return fillEffect{}, err
	}
	return fillEffect{
		Value:            value,
		Closed:           closed,
		AllocatedCost:    allocCost,
		AllocatedSettled: allocSettled,
		RealizedPnL:      closingValue.Sub(allocCost),
		Cash:             closingValue.Sub(allocSettled),
		MarginRelease:    before.MarginPosted.Sub(kept),
		After: positionState{
			Quantity:     remaining,
			CostValue:    before.CostValue.Sub(allocCost),
			SettledValue: before.SettledValue.Sub(allocSettled),
			MarginPosted: kept,
		},
	}, nil
}

// allocate returns the share closed/q of total: exactly total when the whole
// position closes, otherwise rounded half-even.
func allocate(total, closed, q money.Quantity) (money.Quantity, error) {
	if closed.Equal(q) {
		return total, nil
	}
	v, err := total.MulDiv(closed, q, allocationRounding)
	if err != nil {
		return money.Quantity{}, errs.Wrap(err, errs.CodeInternal, "derivatives: allocate a position share")
	}
	return v, nil
}

// keptMargin is the margin the remaining contracts keep: the posted margin
// pro rata, rounded up so a partial close never releases more than its share.
func keptMargin(posted, remaining, before money.Quantity) (money.Quantity, error) {
	if remaining.IsZero() || posted.IsZero() {
		return money.Quantity{}, nil
	}
	v, err := posted.MulDiv(remaining.Abs(), before.Abs(), money.RoundCeil)
	if err != nil {
		return money.Quantity{}, errs.Wrap(err, errs.CodeInternal, "derivatives: keep margin")
	}
	return v, nil
}

// AverageEntryPrice is a position's average entry price at the instrument's
// price scale: its exact cost value over its quantity, priced back through
// the contract value (V(p) = p / tick × tick value), rounded once in mode. It
// is exact whenever the average falls on the price scale. It is for display:
// no cash flow is ever computed from it -- a partial close allocates the
// exact cost value itself.
func AverageEntryPrice(s Spec, pos Position, mode money.RoundingMode) (money.Quantity, error) {
	if pos.Quantity.IsZero() {
		return money.Quantity{}, errs.New(errs.CodeValidationFailed, "derivatives: a flat position has no entry price")
	}
	p, err := pos.CostValue.MulDiv(s.Tick, pos.Quantity.Mul(s.TickValue), mode)
	if err != nil {
		return money.Quantity{}, errs.Wrap(err, errs.CodeValidationFailed, "derivatives: average entry price")
	}
	return p, nil
}

// markValue returns Q × V(mark).
func markValue(s Spec, q, mark money.Quantity) (money.Quantity, error) { return s.Value(q, mark) }

// unrealizedPnL is Q × V(mark) − cost_value.
func unrealizedPnL(s Spec, st positionState, mark money.Quantity) (money.Quantity, error) {
	v, err := markValue(s, st.Quantity, mark)
	if err != nil {
		return money.Quantity{}, err
	}
	return v.Sub(st.CostValue), nil
}

// equityAdjustment is what the position adds to the customer's equity beyond
// the ledger balances buying power already counts: Q × V(mark) − settled_value.
// For a future that is the PnL not yet settled; for an option (settled_value
// is zero) it is the option's whole mark value, positive for a holder and
// negative for a writer.
func equityAdjustment(s Spec, st positionState, mark money.Quantity) (money.Quantity, error) {
	v, err := markValue(s, st.Quantity, mark)
	if err != nil {
		return money.Quantity{}, err
	}
	return v.Sub(st.SettledValue), nil
}

// variationCash is the cash a settlement at mark moves for a linear position
// (> 0 received) and the new baseline.
func variationCash(s Spec, st positionState, mark money.Quantity) (cash, newSettled money.Quantity, err error) {
	v, err := markValue(s, st.Quantity, mark)
	if err != nil {
		return money.Quantity{}, money.Quantity{}, err
	}
	return v.Sub(st.SettledValue), v, nil
}

// paperPrice is the price of a paper fill: the mark moved against the
// customer by the disclosed slippage, then rounded onto the tick grid against
// the customer as well (a buy rounds up, a sell down). Both roundings are
// part of the disclosure recorded on the fill.
func paperPrice(s Spec, mark money.Quantity, side FillSide, slippage money.BPS) (money.Quantity, error) {
	if mark.IsNegative() || slippage < 0 || slippage >= money.OneHundredPercent {
		return money.Quantity{}, errs.New(errs.CodeValidationFailed, "derivatives: invalid mark or slippage")
	}
	hundred := money.QuantityFromInt64(int64(money.OneHundredPercent))
	factor := hundred.Add(money.QuantityFromInt64(int64(slippage)))
	mode := money.RoundCeil
	if side == Sell {
		factor = hundred.Sub(money.QuantityFromInt64(int64(slippage)))
		mode = money.RoundFloor
	}
	ticks, err := mark.MulDiv(factor, hundred.Mul(s.Tick), mode)
	if err != nil {
		return money.Quantity{}, errs.Wrap(err, errs.CodeInternal, "derivatives: paper price")
	}
	return ticks.Mul(s.Tick), nil
}

// IndexSample is one (mark, index) observation used by the funding premium
// index.
type IndexSample struct {
	Mark       money.Quantity
	MarkScale  int32
	Index      money.Quantity
	IndexScale int32
}

// fraction is an exact rational num/den with den > 0, reduced.
type fraction struct{ num, den *big.Int }

func newFraction(num, den *big.Int) fraction {
	n, d := new(big.Int).Set(num), new(big.Int).Set(den)
	if d.Sign() < 0 {
		n.Neg(n)
		d.Neg(d)
	}
	g := new(big.Int).GCD(nil, nil, new(big.Int).Abs(n), d)
	if g.Sign() > 0 && g.Cmp(big.NewInt(1)) != 0 {
		n.Quo(n, g)
		d.Quo(d, g)
	}
	return fraction{num: n, den: d}
}

func (f fraction) add(o fraction) fraction {
	n := new(big.Int).Mul(f.num, o.den)
	n.Add(n, new(big.Int).Mul(o.num, f.den))
	return newFraction(n, new(big.Int).Mul(f.den, o.den))
}

func pow10Big(n int32) *big.Int { return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil) }

// relativeDifference returns (mark − index) / index exactly.
func relativeDifference(smp IndexSample) (fraction, error) {
	if !smp.Index.IsPositive() || smp.Mark.IsNegative() {
		return fraction{}, errs.New(errs.CodeValidationFailed, "derivatives: an index must be positive and a mark non-negative")
	}
	if smp.MarkScale < 0 || smp.IndexScale < 0 || smp.MarkScale > money.MaxPriceScale || smp.IndexScale > money.MaxPriceScale {
		return fraction{}, errs.New(errs.CodeValidationFailed, "derivatives: price scale out of range")
	}
	// mark/10^ms − index/10^xs over index/10^xs = (mark·10^xs − index·10^ms) / (index·10^ms)
	a := new(big.Int).Mul(smp.Mark.BigInt(), pow10Big(smp.IndexScale))
	b := new(big.Int).Mul(smp.Index.BigInt(), pow10Big(smp.MarkScale))
	return newFraction(new(big.Int).Sub(a, b), b), nil
}

// PremiumIndexE8 is the funding premium index of an interval: the exact
// average of (mark − index) / index over the samples, rounded half-even once
// to units of 10^-8.
func PremiumIndexE8(samples []IndexSample) (int64, error) {
	if len(samples) == 0 {
		return 0, errs.New(errs.CodeStaleMarketData, "derivatives: no mark with an index in the funding interval")
	}
	sum := newFraction(big.NewInt(0), big.NewInt(1))
	for _, smp := range samples {
		f, err := relativeDifference(smp)
		if err != nil {
			return 0, err
		}
		sum = sum.add(f)
	}
	num := new(big.Int).Mul(sum.num, big.NewInt(FundingRateScale))
	den := new(big.Int).Mul(sum.den, big.NewInt(int64(len(samples))))
	r, err := money.QuantityFromBigInt(num).Div(money.QuantityFromBigInt(den), money.RoundHalfEven)
	if err != nil {
		return 0, errs.Wrap(err, errs.CodeInternal, "derivatives: premium index")
	}
	return clampToInt64(r), nil
}

// clampToInt64 saturates an integer to the int64 range. Rates beyond it are
// clamped by the policy long before they matter.
func clampToInt64(q money.Quantity) int64 {
	if v, err := q.Int64(); err == nil {
		return v
	}
	if q.IsNegative() {
		return -1 << 63
	}
	return 1<<63 - 1
}

// ClampRate clamps a rate to ±maxE8 and reports whether it clamped.
func ClampRate(rateE8, maxE8 int64) (int64, bool) {
	switch {
	case rateE8 > maxE8:
		return maxE8, true
	case rateE8 < -maxE8:
		return -maxE8, true
	}
	return rateE8, false
}

// fundingPerContract is what one contract pays or receives for an interval
// at a funding rate: V(mark) × |rate| / 10^8, rounded down once. Every
// contract of the interval uses this same integer, which is what makes the
// funding of a balanced book conserve exactly: Σ paid = amount × Σ long
// contracts = amount × Σ short contracts = Σ received.
func fundingPerContract(s Spec, mark money.Quantity, rateE8 int64) (money.Quantity, error) {
	v, err := s.Value(money.QuantityFromInt64(1), mark)
	if err != nil {
		return money.Quantity{}, err
	}
	r := rateE8
	if r < 0 {
		r = -r
	}
	amt, err := v.MulDiv(money.QuantityFromInt64(r), money.QuantityFromInt64(FundingRateScale), money.RoundDown)
	if err != nil {
		return money.Quantity{}, errs.Wrap(err, errs.CodeInternal, "derivatives: funding per contract")
	}
	return amt, nil
}

// fundingDirection returns whether a position of signed quantity q pays or
// receives at rate: longs pay a positive rate, shorts a negative one.
func fundingDirection(q money.Quantity, rateE8 int64) FundingDirection {
	if (rateE8 > 0) == q.IsPositive() {
		return FundingPay
	}
	return FundingReceive
}

// maxDivergenceBPS caps a reported divergence so it always fits money.BPS.
const maxDivergenceBPS money.BPS = 1_000_000_000

// DivergenceBPS is |mark − index| / index in basis points, rounded up (a
// divergence just over a limit is never reported as just under it).
func DivergenceBPS(smp IndexSample) (money.BPS, error) {
	f, err := relativeDifference(smp)
	if err != nil {
		return 0, err
	}
	n := new(big.Int).Abs(f.num)
	n.Mul(n, big.NewInt(int64(money.OneHundredPercent)))
	v, err := money.QuantityFromBigInt(n).Div(money.QuantityFromBigInt(f.den), money.RoundUp)
	if err != nil {
		return 0, errs.Wrap(err, errs.CodeInternal, "derivatives: divergence")
	}
	if v.Cmp(money.QuantityFromInt64(int64(maxDivergenceBPS))) > 0 {
		return maxDivergenceBPS, nil
	}
	b, _ := v.Int64()
	return money.BPS(b), nil
}
