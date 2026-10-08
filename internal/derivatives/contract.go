package derivatives

import (
	"time"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Spec is everything the arithmetic of this package needs to know about one
// derivative instrument: its current terms resolved against the decimals of
// the settlement and underlying assets.
//
// Units. A price is a mantissa at PriceScale decimals, in whole settlement
// units per whole unit of the underlying (the convention of
// instruments.Terms). One contract is Multiplier base units of the
// underlying. The value of q contracts at price p, in base units of the
// settlement asset, is
//
//	q × p × Multiplier × 10^(settlementDecimals − underlyingDecimals − PriceScale)
//
// This package admits only instruments whose tick value -- that expression
// for one contract and one tick -- is a whole, positive number of settlement
// base units. Every trade price, mark, strike and settlement price is on the
// tick grid, so every value, profit and loss this package posts is an exact
// integer and nothing is ever rounded on its way to the ledger except where a
// function says so and names its rounding mode.
type Spec struct {
	InstrumentID        instruments.InstrumentID
	Kind                instruments.InstrumentType
	Symbol              string
	InstrumentStatus    assets.Status
	SettlementAssetID   assets.AssetID
	SettlementDecimals  uint8
	UnderlyingAssetID   assets.AssetID
	UnderlyingDecimals  uint8
	UnderlyingRiskClass assets.RiskClass
	PriceScale          int32
	Tick                money.Quantity
	LotSize             money.Quantity
	MinQuantity         money.Quantity
	Multiplier          money.Quantity
	// TickValue is the value of one contract moving one tick, in settlement
	// base units. Always a positive integer.
	TickValue       money.Quantity
	Expiry          *time.Time
	FundingInterval time.Duration
	Strike          money.Quantity
	Right           instruments.OptionRight
	Style           instruments.ExerciseStyle
	OracleMaxAge    time.Duration
	TradingCalendar string
}

// Calendar24x7 is the only trading calendar this lane trades on.
const Calendar24x7 = "24x7"

// NewSpec resolves an instrument's terms into a Spec. It fails closed: an
// instrument this package cannot settle exactly, or that could settle in
// anything but SIMULATED value, is refused rather than approximated.
func NewSpec(ins instruments.Instrument, t instruments.Terms, settlement, underlying assets.Asset) (Spec, error) {
	var problems []string
	switch t.Kind {
	case instruments.TypeFuture, instruments.TypePerpetual, instruments.TypeOption:
	default:
		return Spec{}, errs.Newf(errs.CodeUnsupported, "derivatives: %s is not a future, a perpetual or an option", t.Kind).
			WithField("instrument_id", ins.ID.String())
	}
	if ins.Type != t.Kind {
		problems = append(problems, "instrument type and terms kind disagree")
	}
	if t.SettlementAssetID != settlement.ID || ins.SettlementAssetID != settlement.ID {
		problems = append(problems, "settlement asset does not match the terms")
	}
	if settlement.Kind != assets.KindSimulated || settlement.ValueDomain != valuedomain.Simulated {
		return Spec{}, errs.New(errs.CodeForbidden, "derivatives settle only in SIMULATED value in this build").
			WithField("settlement_asset_id", settlement.ID.String())
	}
	if t.SettlementStyle != instruments.SettleCash {
		return Spec{}, errs.New(errs.CodeUnsupported, "derivatives: only cash-settled contracts are supported").
			WithField("instrument_id", ins.ID.String())
	}
	if underlying.ID.IsZero() {
		problems = append(problems, "the underlying asset is unknown")
	}
	if !t.TickSize.IsPositive() || !t.ContractMultiplier.IsPositive() || !t.LotSize.IsPositive() {
		problems = append(problems, "tick size, lot size and multiplier must be positive")
	}
	if t.PriceScale < 0 || t.PriceScale > 18 {
		problems = append(problems, "price scale must be within 0..18")
	}
	if t.OracleMaxAgeMS <= 0 {
		problems = append(problems, "oracle max age must be positive")
	}
	if len(problems) > 0 {
		return Spec{}, errs.New(errs.CodeValidationFailed, "derivatives: instrument cannot be traded").
			WithField("instrument_id", ins.ID.String()).WithField("problems", problems)
	}
	s := Spec{
		InstrumentID: ins.ID, Kind: t.Kind, Symbol: t.Symbol, InstrumentStatus: ins.Status,
		SettlementAssetID: settlement.ID, SettlementDecimals: settlement.Decimals,
		UnderlyingAssetID: underlying.ID, UnderlyingDecimals: underlying.Decimals, UnderlyingRiskClass: underlying.RiskClass,
		PriceScale: t.PriceScale, Tick: t.TickSize, LotSize: t.LotSize, MinQuantity: t.MinQuantity,
		Multiplier:      t.ContractMultiplier,
		OracleMaxAge:    time.Duration(t.OracleMaxAgeMS) * time.Millisecond,
		TradingCalendar: t.TradingCalendar,
	}
	tv, err := scaledValue(s.Tick.Mul(s.Multiplier), s.exponent(s.PriceScale), money.RoundExact)
	if err != nil || !tv.IsPositive() {
		return Spec{}, errs.New(errs.CodeValidationFailed,
			"derivatives: one tick of one contract is not a whole number of settlement base units; the contract cannot settle exactly").
			WithField("instrument_id", ins.ID.String()).
			WithField("tick_size", s.Tick.String()).WithField("multiplier", s.Multiplier.String())
	}
	s.TickValue = tv
	if t.Expiry != nil {
		e := t.Expiry.UTC()
		s.Expiry = &e
	}
	switch t.Kind {
	case instruments.TypeFuture:
		if s.Expiry == nil {
			return Spec{}, errs.New(errs.CodeValidationFailed, "derivatives: a dated future needs an expiry")
		}
	case instruments.TypePerpetual:
		if t.FundingIntervalMS <= 0 {
			return Spec{}, errs.New(errs.CodeValidationFailed, "derivatives: a perpetual needs a funding interval")
		}
		s.FundingInterval = time.Duration(t.FundingIntervalMS) * time.Millisecond
	case instruments.TypeOption:
		if s.Expiry == nil || t.StrikeMantissa == nil || !t.StrikeMantissa.IsPositive() {
			return Spec{}, errs.New(errs.CodeValidationFailed, "derivatives: an option needs an expiry and a positive strike")
		}
		if !s.OnTick(*t.StrikeMantissa) {
			return Spec{}, errs.New(errs.CodeValidationFailed, "derivatives: an option's strike must lie on its tick grid").
				WithField("strike_mantissa", t.StrikeMantissa.String())
		}
		if t.OptionRight != instruments.Call && t.OptionRight != instruments.Put {
			return Spec{}, errs.New(errs.CodeValidationFailed, "derivatives: an option is a CALL or a PUT")
		}
		if t.ExerciseStyle != instruments.European && t.ExerciseStyle != instruments.American {
			return Spec{}, errs.New(errs.CodeValidationFailed, "derivatives: an option is EUROPEAN or AMERICAN")
		}
		s.Strike, s.Right, s.Style = *t.StrikeMantissa, t.OptionRight, t.ExerciseStyle
	}
	return s, nil
}

// IsOption reports whether the instrument is an option.
func (s Spec) IsOption() bool { return s.Kind == instruments.TypeOption }

// IsLinear reports whether the instrument is a future or a perpetual.
func (s Spec) IsLinear() bool {
	return s.Kind == instruments.TypeFuture || s.Kind == instruments.TypePerpetual
}

// exponent is settlementDecimals − underlyingDecimals − priceScale.
func (s Spec) exponent(priceScale int32) int {
	return int(s.SettlementDecimals) - int(s.UnderlyingDecimals) - int(priceScale)
}

// scaledValue returns v × 10^exp, rounded once in mode when exp < 0.
func scaledValue(v money.Quantity, exp int, mode money.RoundingMode) (money.Quantity, error) {
	if exp >= 0 {
		for exp > 0 {
			step := exp
			if step > 18 {
				step = 18
			}
			v = v.ScaleUp(uint8(step)) // #nosec G115 -- step is in [1, 18]
			exp -= step
		}
		return v, nil
	}
	den := money.QuantityFromInt64(1)
	for n := -exp; n > 0; {
		step := n
		if step > 18 {
			step = 18
		}
		den = den.ScaleUp(uint8(step)) // #nosec G115 -- step is in [1, 18]
		n -= step
	}
	return v.Div(den, mode)
}

// OnTick reports whether a price mantissa lies on the tick grid.
func (s Spec) OnTick(price money.Quantity) bool {
	if price.IsNegative() {
		return false
	}
	_, err := price.Div(s.Tick, money.RoundExact)
	return err == nil
}

// Value returns the value of q (signed) contracts at a price on the tick
// grid, exactly: q × (price / tick) × tick value.
func (s Spec) Value(q, price money.Quantity) (money.Quantity, error) {
	ticks, err := price.Div(s.Tick, money.RoundExact)
	if err != nil || price.IsNegative() {
		return money.Quantity{}, errs.New(errs.CodeValidationFailed, "derivatives: price is not on the instrument's tick grid").
			WithField("price_mantissa", price.String()).WithField("tick_size", s.Tick.String())
	}
	return q.Mul(ticks).Mul(s.TickValue), nil
}

// ValueAnyPrice returns the value of q contracts at any non-negative price
// mantissa at priceScale, rounded once in mode. It is used for reference
// prices that need not be on the tick grid (an index, a shocked scenario).
func (s Spec) ValueAnyPrice(q, price money.Quantity, priceScale int32, mode money.RoundingMode) (money.Quantity, error) {
	if price.IsNegative() {
		return money.Quantity{}, errs.New(errs.CodeValidationFailed, "derivatives: a price cannot be negative")
	}
	v, err := scaledValue(q.Mul(price).Mul(s.Multiplier), s.exponent(priceScale), mode)
	if err != nil {
		return money.Quantity{}, errs.Wrap(err, errs.CodeValidationFailed, "derivatives: value a reference price")
	}
	return v, nil
}

// AlignToTick rounds a price mantissa at the instrument's price scale onto
// the tick grid in mode. RoundExact refuses a price that is off the grid.
func (s Spec) AlignToTick(price money.Quantity, mode money.RoundingMode) (money.Quantity, error) {
	ticks, err := price.Div(s.Tick, mode)
	if err != nil {
		return money.Quantity{}, errs.Wrap(err, errs.CodeValidationFailed, "derivatives: align a price to the tick grid")
	}
	return ticks.Mul(s.Tick), nil
}

// Rescale expresses a price mantissa at fromScale as a mantissa at toScale,
// rounding once in mode when precision would be lost.
func Rescale(mantissa money.Quantity, fromScale, toScale int32, mode money.RoundingMode) (money.Quantity, error) {
	if fromScale < 0 || toScale < 0 || fromScale > money.MaxPriceScale || toScale > money.MaxPriceScale {
		return money.Quantity{}, errs.New(errs.CodeValidationFailed, "derivatives: price scale out of range")
	}
	if toScale >= fromScale {
		return mantissa.ScaleUp(uint8(toScale - fromScale)), nil // #nosec G115 -- both scales are in [0, 38]
	}
	return mantissa.ScaleDown(uint8(fromScale-toScale), mode) // #nosec G115 -- both scales are in [0, 38]
}

// CheckQuantity validates a contract quantity against the lot size and the
// minimum.
func (s Spec) CheckQuantity(q money.Quantity) error {
	if !q.IsPositive() {
		return errs.New(errs.CodeValidationFailed, "derivatives: a quantity of contracts must be positive")
	}
	if _, err := q.Div(s.LotSize, money.RoundExact); err != nil {
		return errs.New(errs.CodeValidationFailed, "derivatives: quantity must be a whole number of lots").
			WithField("lot_size", s.LotSize.String())
	}
	if q.Cmp(s.MinQuantity) < 0 {
		return errs.New(errs.CodeValidationFailed, "derivatives: quantity is below the instrument's minimum").
			WithField("min_quantity", s.MinQuantity.String())
	}
	return nil
}

// ExpiredAt reports whether the instrument has reached its expiry at t.
func (s Spec) ExpiredAt(t time.Time) bool { return s.Expiry != nil && !t.Before(*s.Expiry) }
