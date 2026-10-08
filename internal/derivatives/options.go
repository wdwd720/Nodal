package derivatives

import (
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/quant"
)

// IntrinsicPrice returns an option's exact intrinsic value per unit of the
// underlying at an underlying price on the option's tick grid, as a price
// mantissa at the option's price scale: max(S − K, 0) for a call and
// max(K − S, 0) for a put (quant.IntrinsicCall / quant.IntrinsicPut). It is
// on the tick grid because S and K are.
func IntrinsicPrice(s Spec, underlying money.Quantity) (money.Quantity, error) {
	if !s.IsOption() {
		return money.Quantity{}, errs.New(errs.CodeValidationFailed, "derivatives: intrinsic value is an option's")
	}
	if !s.OnTick(underlying) {
		return money.Quantity{}, errs.New(errs.CodeValidationFailed, "derivatives: the settlement price must lie on the option's tick grid").
			WithField("price_mantissa", underlying.String())
	}
	if s.Right == instruments.Call {
		return quant.IntrinsicCall(underlying, s.Strike), nil
	}
	return quant.IntrinsicPut(underlying, s.Strike), nil
}

// IntrinsicPerContract is the exact settlement value of one contract at an
// underlying price on the tick grid: V(intrinsic price).
func IntrinsicPerContract(s Spec, underlying money.Quantity) (money.Quantity, error) {
	ip, err := IntrinsicPrice(s, underlying)
	if err != nil {
		return money.Quantity{}, err
	}
	return s.Value(money.QuantityFromInt64(1), ip)
}

// commonScale expresses a mantissa at scale from at a scale to >= from,
// exactly.
func commonScale(m money.Quantity, from, to int32) money.Quantity {
	return m.ScaleUp(uint8(to - from)) // #nosec G115 -- callers pass 0 <= from <= to <= 38
}

// intrinsicAt is an option's exact intrinsic value per unit of the
// underlying at a spot of any scale. It works at the common scale
// c = max(price scale, spot scale) and returns the spot and strike at that
// scale too.
func intrinsicAt(s Spec, spot money.Quantity, spotScale int32) (intrinsic, x, k money.Quantity, c int32) {
	c = s.PriceScale
	if spotScale > c {
		c = spotScale
	}
	x = commonScale(spot, spotScale, c)
	k = commonScale(s.Strike, s.PriceScale, c)
	if s.Right == instruments.Put {
		return quant.IntrinsicPut(x, k), x, k, c
	}
	return quant.IntrinsicCall(x, k), x, k, c
}

// OptionDivergenceBPS measures how far an option's mark lies outside the
// model-free bounds its index allows -- intrinsic(S) <= premium <= S for a
// call, intrinsic(S) <= premium <= K for a put -- relative to the index, in
// basis points rounded up. A mark inside the bounds diverges by zero. It is
// the option's form of mark/index divergence: a premium below intrinsic value
// or above the underlying is a mark that disagrees with its index.
func OptionDivergenceBPS(s Spec, mark money.Quantity, index money.Quantity, indexScale int32) (money.BPS, error) {
	if !s.IsOption() || !index.IsPositive() || mark.IsNegative() || indexScale < 0 || indexScale > money.MaxPriceScale {
		return 0, errs.New(errs.CodeValidationFailed, "derivatives: option divergence needs an option, a positive index and a mark")
	}
	lower, x, k, c := intrinsicAt(s, index, indexScale)
	m := commonScale(mark, s.PriceScale, c)
	upper := x
	if s.Right == instruments.Put {
		upper = k
	}
	dist := lower.Sub(m).Max(money.Quantity{}).Add(m.Sub(upper).Max(money.Quantity{}))
	if dist.IsZero() {
		return 0, nil
	}
	v, err := dist.MulDiv(money.QuantityFromInt64(int64(money.OneHundredPercent)), x, money.RoundUp)
	if err != nil {
		return 0, errs.Wrap(err, errs.CodeInternal, "derivatives: option divergence")
	}
	if v.Cmp(money.QuantityFromInt64(int64(maxDivergenceBPS))) > 0 {
		return maxDivergenceBPS, nil
	}
	b, _ := v.Int64()
	return money.BPS(b), nil
}

// Allocation assigns Quantity exercised contracts of exercise Exercise to
// writer Writer, or to the clearing house when Writer is -1.
type Allocation struct {
	Exercise int
	Writer   int
	Quantity money.Quantity
}

// HouseWriter is the Writer index of an allocation the clearing house takes:
// the writer of record when exercised contracts outnumber written ones.
const HouseWriter = -1

// AssignFIFO allocates exercised contracts to writers deterministically:
// exercises in the order given, each consuming the writers' open contracts in
// the order given (the caller orders writers by position open time, then
// id). Contracts no writer is left to take are assigned to the clearing
// house. Every input quantity must be positive; the result conserves both
// sides exactly: per exercise the allocations sum to the exercised quantity,
// and no writer is allocated more than it wrote.
func AssignFIFO(exercised []money.Quantity, writers []money.Quantity) ([]Allocation, error) {
	left := make([]money.Quantity, len(writers))
	for i, w := range writers {
		if !w.IsPositive() {
			return nil, errs.New(errs.CodeValidationFailed, "derivatives: a writer's open contracts must be positive")
		}
		left[i] = w
	}
	var out []Allocation
	next := 0
	for ei, e := range exercised {
		if !e.IsPositive() {
			return nil, errs.New(errs.CodeValidationFailed, "derivatives: an exercised quantity must be positive")
		}
		need := e
		for need.IsPositive() && next < len(left) {
			take := need.Min(left[next])
			out = append(out, Allocation{Exercise: ei, Writer: next, Quantity: take})
			need = need.Sub(take)
			left[next] = left[next].Sub(take)
			if left[next].IsZero() {
				next++
			}
		}
		if need.IsPositive() {
			out = append(out, Allocation{Exercise: ei, Writer: HouseWriter, Quantity: need})
		}
	}
	return out, nil
}
