package margin

import (
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// The engine's unit of account is the numeraire's base unit (for a paper
// dollar with 6 decimals, one millionth of a dollar), held as an exact
// money.Quantity. Every sum in the canonical equity is formed in that unit,
// so the identity holds exactly; USD (cents) appears only at the edges, each
// conversion naming its rounding.

// usdToUnits converts cents to numeraire base units. It is exact because the
// numeraire has at least two decimals.
func usdToUnits(u money.USD, decimals uint8) (money.Quantity, error) {
	if decimals < 2 {
		return money.Quantity{}, errs.New(errs.CodeValidationFailed, "margin: the numeraire needs at least two decimals")
	}
	q, err := money.USDToQuoteQuantity(u, decimals, money.RoundExact)
	if err != nil {
		return money.Quantity{}, errs.Wrap(err, errs.CodeInternal, "margin: convert cents to numeraire units")
	}
	return q, nil
}

// unitsToUSD converts numeraire base units to cents in the given mode.
func unitsToUSD(q money.Quantity, decimals uint8, mode money.RoundingMode) (money.USD, error) {
	u, err := money.QuoteQuantityToUSD(q, decimals, mode)
	if err != nil {
		return money.USD{}, errs.Wrap(err, errs.CodeOverflow, "margin: value does not fit in cents")
	}
	return u, nil
}

const (
	// bpsDenominator is 100% in basis points.
	bpsDenominator int64 = 10_000
	// bpsSquared divides a rate in BPS times a weight in BPS.
	bpsSquared int64 = bpsDenominator * bpsDenominator
)

// mulBPS returns q × bps / 10_000 rounded in mode.
func mulBPS(q money.Quantity, bps money.BPS, mode money.RoundingMode) (money.Quantity, error) {
	r, err := q.MulDiv(money.QuantityFromInt64(int64(bps)), money.QuantityFromInt64(bpsDenominator), mode)
	if err != nil {
		return money.Quantity{}, errs.Wrap(err, errs.CodeInternal, "margin: bps arithmetic")
	}
	return r, nil
}

// mulRateWeight returns q × rate × weight / 10^8 rounded up: a requirement
// of rate (BPS) on q with a risk weight (BPS).
func mulRateWeight(q money.Quantity, rate, weight money.BPS) (money.Quantity, error) {
	num := money.QuantityFromInt64(int64(rate) * int64(weight))
	r, err := q.MulDiv(num, money.QuantityFromInt64(bpsSquared), money.RoundCeil)
	if err != nil {
		return money.Quantity{}, errs.Wrap(err, errs.CodeInternal, "margin: requirement arithmetic")
	}
	return r, nil
}

// ratioBPSCeil returns ceil(num × 10_000 / den) for den > 0.
func ratioBPSCeil(num, den money.Quantity) (money.Quantity, error) {
	r, err := num.MulDiv(money.QuantityFromInt64(bpsDenominator), den, money.RoundCeil)
	if err != nil {
		return money.Quantity{}, errs.Wrap(err, errs.CodeInternal, "margin: ratio arithmetic")
	}
	return r, nil
}

// shareBPS returns min(10_000, floor(part × 10_000 / whole)) for whole > 0.
func shareBPS(part, whole money.Quantity) (money.BPS, error) {
	r, err := part.MulDiv(money.QuantityFromInt64(bpsDenominator), whole, money.RoundFloor)
	if err != nil {
		return 0, errs.Wrap(err, errs.CodeInternal, "margin: share arithmetic")
	}
	if r.Cmp(money.QuantityFromInt64(10_000)) >= 0 {
		return 10_000, nil
	}
	if r.Sign() <= 0 {
		return 0, nil
	}
	v, err := r.Int64()
	if err != nil {
		return 0, errs.Wrap(err, errs.CodeInternal, "margin: share arithmetic")
	}
	return money.BPS(v), nil
}

// scalePrice returns the price multiplied by num / 10_000. The product is
// exact whenever the scale can grow by four digits (the mantissa is
// multiplied by num and the scale raised by 4); otherwise it is floored at
// the original scale. num <= 0 yields a zero price.
func scalePrice(p money.Price, num int64) (money.Price, error) {
	out := p
	if num <= 0 {
		out.Mantissa = money.Quantity{}
		return out, nil
	}
	if p.Scale+4 <= money.MaxPriceScale {
		out.Mantissa = p.Mantissa.Mul(money.QuantityFromInt64(num))
		out.Scale = p.Scale + 4
		return out, nil
	}
	m, err := p.Mantissa.MulDiv(money.QuantityFromInt64(num), money.QuantityFromInt64(bpsDenominator), money.RoundFloor)
	if err != nil {
		return money.Price{}, errs.Wrap(err, errs.CodeInternal, "margin: price arithmetic")
	}
	out.Mantissa = m
	return out, nil
}

// notional values qty base units of an asset at a price, in numeraire base
// units, rounded in mode.
func notional(qty money.Quantity, decimals uint8, price money.Price, numeraireDecimals uint8, mode money.RoundingMode) (money.Quantity, error) {
	v, err := money.Notional(qty, decimals, price, numeraireDecimals, mode)
	if err != nil {
		return money.Quantity{}, errs.Wrap(err, errs.CodeValidationFailed, "margin: value a holding")
	}
	return v, nil
}

func maxQ(a, b money.Quantity) money.Quantity { return a.Max(b) }

func minQ(a, b money.Quantity) money.Quantity { return a.Min(b) }
