package equities

import (
	"math/big"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// Exact price arithmetic. A price is a mantissa at a scale; every conversion
// between scales or onto a tick grid is one exact product and at most one
// rounding in a named mode.

// pow10 returns 10^n exactly.
func pow10(n int) money.Quantity {
	if n <= 0 {
		return money.QuantityFromInt64(1)
	}
	return money.QuantityFromBigInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil))
}

var (
	one        = money.QuantityFromInt64(1)
	bpsDivisor = money.QuantityFromInt64(int64(money.OneHundredPercent))
)

// toTicks expresses num/den (a price at priceScale, as an exact rational of
// mantissa units) as a whole number of ticks rounded in mode, and returns the
// resulting mantissa at priceScale.
func toTicks(num, den, tick money.Quantity, mode money.RoundingMode) (money.Quantity, error) {
	ticks, err := num.MulDiv(one, den.Mul(tick), mode)
	if err != nil {
		return money.Quantity{}, err
	}
	return ticks.Mul(tick), nil
}

// adjustedPrice is mark × (10_000 ± bps) / 10_000 at priceScale, rounded onto
// the tick grid in mode. up selects +bps.
func adjustedPrice(m Mark, up bool, bps money.BPS, priceScale int32, tick money.Quantity, mode money.RoundingMode) (money.Quantity, error) {
	factor := money.OneHundredPercent + bps
	if !up {
		factor = money.OneHundredPercent - bps
	}
	if factor <= 0 {
		return money.Quantity{}, errs.New(errs.CodeValidationFailed, "equities: a price adjustment cannot remove the whole price")
	}
	num := m.Mantissa.Mul(money.QuantityFromInt64(int64(factor))).Mul(pow10(int(priceScale)))
	den := bpsDivisor.Mul(pow10(int(m.Scale)))
	return toTicks(num, den, tick, mode)
}

// slippedPrice is the price the paper venue executes a market order at: the
// mark moved by the slippage against the customer and rounded onto the tick
// grid against the customer (up for a buy, down for a sell).
func slippedPrice(m Mark, side Side, bps money.BPS, priceScale int32, tick money.Quantity) (money.Quantity, error) {
	if side == SideBuy {
		return adjustedPrice(m, true, bps, priceScale, tick, money.RoundCeil)
	}
	return adjustedPrice(m, false, bps, priceScale, tick, money.RoundFloor)
}

// compareMark compares the mark with a price mantissa at priceScale exactly.
func compareMark(m Mark, price money.Quantity, priceScale int32) int {
	lhs := m.Mantissa.Mul(pow10(int(priceScale)))
	rhs := price.Mul(pow10(int(m.Scale)))
	return lhs.Cmp(rhs)
}

// rescalePrice expresses a price given at scale as a mantissa at priceScale,
// exactly; a price that needs more decimals than the instrument quotes is
// refused rather than rounded.
func rescalePrice(mantissa money.Quantity, scale, priceScale int32) (money.Quantity, error) {
	switch {
	case scale == priceScale:
		return mantissa, nil
	case scale < priceScale:
		return mantissa.Mul(pow10(int(priceScale - scale))), nil
	}
	return mantissa.Div(pow10(int(scale-priceScale)), money.RoundExact)
}
