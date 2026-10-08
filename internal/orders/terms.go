package orders

import (
	"errors"
	"strconv"
	"time"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/matching"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// BookTerms is everything a book needs to convert between an instrument's
// exact decimal terms and the engine's integer units, fixed when the book is
// opened and recorded on book_states.
//
//   - A price is quote per whole base unit at PriceScale decimals (at scale 2,
//     sixty-five thousand and fifty cents is 6500050 price units); one tick is
//     TickSize price units.
//   - A quantity is base units of the base asset; one lot is LotSize base
//     units.
//   - NotionalPerTickLot is the quote-asset base units of one tick times one
//     lot. It is a positive integer by construction (a book whose terms would
//     make it fractional is refused), so every trade notional is exact:
//     price_ticks x qty_lots x NotionalPerTickLot, with nothing to round.
//
// Every conversion is exact or refused with a stable reason; nothing rounds.
type BookTerms struct {
	InstrumentID       instruments.InstrumentID
	Kind               instruments.InstrumentType
	BaseAsset          assets.AssetID
	QuoteAsset         assets.AssetID
	BaseDecimals       uint8
	QuoteDecimals      uint8
	PriceScale         int32
	TickSize           money.Quantity
	LotSize            money.Quantity
	MinQuantity        money.Quantity
	NotionalPerTickLot money.Quantity
	MaxPriceTicks      int64
	OracleMaxAge       time.Duration
	TradingCalendar    string
	Expiry             *time.Time
}

// rejection builds the VALIDATION_FAILED error of a refused order with its
// stable reason.
func rejection(reason, detail string) *errs.Error {
	return errs.New(errs.CodeValidationFailed, "orders: "+detail).WithField("reason", reason)
}

// ReasonOf returns the stable reason an error from this package carries, or
// "" when it carries none.
func ReasonOf(err error) string {
	var e *errs.Error
	if !errors.As(err, &e) || e.Fields == nil {
		return ""
	}
	r, _ := e.Fields["reason"].(string)
	return r
}

var ten = money.QuantityFromInt64(10)

// pow10 returns 10^n for n >= 0.
func pow10(n int) money.Quantity {
	out := money.QuantityFromInt64(1)
	for i := 0; i < n; i++ {
		out = out.Mul(ten)
	}
	return out
}

// DeriveBookTerms derives a book's terms from an instrument, its terms and its
// two assets. It refuses anything this book cannot trade exactly: a kind it
// does not support, a multiplier other than one, an asset that is not
// SIMULATED, assets that disagree with the instrument, and terms whose
// notional per tick-lot is not a positive integer.
func DeriveBookTerms(inst instruments.Instrument, t instruments.Terms, base, quote assets.Asset) (BookTerms, error) {
	if err := t.Validate(); err != nil {
		return BookTerms{}, err
	}
	supported := false
	for _, k := range AllBookInstrumentKinds() {
		if k == t.Kind {
			supported = true
		}
	}
	if !supported || inst.Type != t.Kind {
		return BookTerms{}, errs.Newf(errs.CodeValidationFailed,
			"orders: an internal book trades SPOT_PAIR, EQUITY and EVENT_OUTCOME instruments, not %s", t.Kind)
	}
	if t.BaseAssetID == nil {
		return BookTerms{}, errs.New(errs.CodeValidationFailed, "orders: a book's instrument names its base asset")
	}
	quoteID := t.SettlementAssetID
	if t.QuoteAssetID != nil {
		quoteID = *t.QuoteAssetID
	}
	if base.ID != *t.BaseAssetID || quote.ID != quoteID || base.ID == quote.ID {
		return BookTerms{}, errs.New(errs.CodeValidationFailed, "orders: the base and quote assets are not the instrument's")
	}
	if inst.BaseAssetID != nil && *inst.BaseAssetID != base.ID {
		return BookTerms{}, errs.New(errs.CodeValidationFailed, "orders: terms and instrument disagree about the base asset")
	}
	if inst.QuoteAssetID != nil && *inst.QuoteAssetID != quote.ID {
		return BookTerms{}, errs.New(errs.CodeValidationFailed, "orders: terms and instrument disagree about the quote asset")
	}
	if inst.SettlementAssetID != t.SettlementAssetID {
		return BookTerms{}, errs.New(errs.CodeValidationFailed, "orders: terms and instrument disagree about the settlement asset")
	}
	for _, a := range []assets.Asset{base, quote} {
		if a.Kind != assets.KindSimulated || a.ValueDomain != valuedomain.Simulated {
			return BookTerms{}, errs.Newf(errs.CodeForbidden,
				"orders: asset %s is not SIMULATED; the internal book moves only simulated value", a.ID)
		}
	}
	if !t.ContractMultiplier.Equal(money.QuantityFromInt64(1)) {
		return BookTerms{}, errs.New(errs.CodeValidationFailed, "orders: a book trades the base asset itself; the contract multiplier must be 1")
	}
	if t.PriceScale < 0 || t.PriceScale > 18 {
		return BookTerms{}, errs.New(errs.CodeValidationFailed, "orders: price scale must be within 0..18")
	}
	bt := BookTerms{
		InstrumentID:    inst.ID,
		Kind:            t.Kind,
		BaseAsset:       base.ID,
		QuoteAsset:      quote.ID,
		BaseDecimals:    base.Decimals,
		QuoteDecimals:   quote.Decimals,
		PriceScale:      t.PriceScale,
		TickSize:        t.TickSize,
		LotSize:         t.LotSize,
		MinQuantity:     t.MinQuantity,
		OracleMaxAge:    time.Duration(t.OracleMaxAgeMS) * time.Millisecond,
		TradingCalendar: t.TradingCalendar,
		Expiry:          t.Expiry,
	}
	nptl, err := notionalPerTickLot(t.TickSize, t.LotSize, int(quote.Decimals)-int(base.Decimals)-int(t.PriceScale))
	if err != nil {
		return BookTerms{}, err
	}
	bt.NotionalPerTickLot = nptl
	bt.MaxPriceTicks = matching.MaxPriceTicks
	if t.Kind == instruments.TypeEventOutcome {
		// An outcome share pays at most one unit of collateral per unit of
		// share, so no price above that is meaningful: the book's ceiling is
		// 10^(base decimals - quote decimals + price scale) price units.
		exp := int(base.Decimals) - int(quote.Decimals) + int(t.PriceScale)
		if exp < 0 {
			return BookTerms{}, errs.New(errs.CodeValidationFailed, "orders: these decimals cannot express a price of one unit of collateral")
		}
		maxTicks, err := pow10(exp).Div(t.TickSize, money.RoundDown)
		if err != nil {
			return BookTerms{}, errs.Wrap(err, errs.CodeValidationFailed, "orders: outcome price ceiling")
		}
		m, err := maxTicks.Int64()
		if err != nil || m < 1 {
			return BookTerms{}, errs.New(errs.CodeValidationFailed, "orders: the tick size leaves no price at or below one unit of collateral")
		}
		bt.MaxPriceTicks = min(m, matching.MaxPriceTicks)
	}
	return bt, nil
}

// notionalPerTickLot is tick x lot x 10^exp, which must be a positive integer.
func notionalPerTickLot(tick, lot money.Quantity, exp int) (money.Quantity, error) {
	prod := tick.Mul(lot)
	var out money.Quantity
	if exp >= 0 {
		out = prod.Mul(pow10(exp))
	} else {
		q, err := prod.Div(pow10(-exp), money.RoundExact)
		if err != nil {
			return money.Quantity{}, errs.New(errs.CodeValidationFailed,
				"orders: one tick times one lot is not a whole number of quote base units; a trade notional would have to be rounded").
				WithField("reason", "ORDER_TERMS_NOTIONAL_INEXACT")
		}
		out = q
	}
	if !out.IsPositive() {
		return money.Quantity{}, errs.New(errs.CodeValidationFailed, "orders: the notional of one tick-lot must be positive")
	}
	return out, nil
}

// PriceToTicks converts a price (quote per whole base unit) into ticks. The
// price must be quoted in the book's quote asset, carry no non-zero digit
// below the book's price scale, sit exactly on a tick and lie within
// [1, MaxPriceTicks].
func (t BookTerms) PriceToTicks(p money.Price) (int64, error) {
	if p.Scale < 0 || p.Scale > money.MaxPriceScale || p.Mantissa.Sign() < 0 {
		return 0, rejection(ReasonPriceNotPositive, "price is not a valid price")
	}
	if !p.Mantissa.IsPositive() {
		return 0, rejection(ReasonPriceNotPositive, "price must be positive")
	}
	if p.QuoteAsset != t.QuoteAsset.String() {
		return 0, rejection(ReasonPriceQuoteAsset, "price must be quoted in the book's quote asset")
	}
	var units money.Quantity
	if p.Scale <= t.PriceScale {
		units = p.Mantissa.Mul(pow10(int(t.PriceScale - p.Scale)))
	} else {
		u, err := p.Mantissa.Div(pow10(int(p.Scale-t.PriceScale)), money.RoundExact)
		if err != nil {
			return 0, rejection(ReasonPricePrecision, "price has digits below the book's price scale of "+strconv.Itoa(int(t.PriceScale)))
		}
		units = u
	}
	ticks, err := units.Div(t.TickSize, money.RoundExact)
	if err != nil {
		return 0, rejection(ReasonPriceOffTick, "price is not a whole number of ticks")
	}
	n, err := ticks.Int64()
	if err != nil || n < 1 || n > t.MaxPriceTicks {
		return 0, rejection(ReasonPriceOutOfRange, "price is outside the book's range")
	}
	return n, nil
}

// QuantityToLots converts a base-unit quantity into lots. It must be positive,
// at least the minimum quantity, a whole number of lots and within the
// engine's bound.
func (t BookTerms) QuantityToLots(q money.Quantity) (int64, error) {
	if !q.IsPositive() {
		return 0, rejection(ReasonQtyNotPositive, "quantity must be positive")
	}
	if q.Cmp(t.MinQuantity) < 0 {
		return 0, rejection(ReasonQtyBelowMinimum, "quantity is below the instrument's minimum")
	}
	lots, err := q.Div(t.LotSize, money.RoundExact)
	if err != nil {
		return 0, rejection(ReasonQtyOffLot, "quantity is not a whole number of lots")
	}
	n, err := lots.Int64()
	if err != nil || n < 1 || n > matching.MaxQtyLots {
		return 0, rejection(ReasonQtyOutOfRange, "quantity is outside the engine's range")
	}
	return n, nil
}

// CheckNotional refuses an order whose price times quantity in tick-lots
// exceeds the engine's bound.
func (t BookTerms) CheckNotional(ticks, lots int64) error {
	if ticks < 1 || lots < 1 || lots > matching.MaxNotionalTickLots/ticks {
		return rejection(ReasonNotionalTooLarge, "price times quantity exceeds the engine's bound")
	}
	return nil
}

// TicksToPrice is the exact price of a number of ticks.
func (t BookTerms) TicksToPrice(ticks int64, source string, at time.Time) money.Price {
	return money.Price{
		Mantissa:   money.QuantityFromInt64(ticks).Mul(t.TickSize),
		Scale:      t.PriceScale,
		QuoteAsset: t.QuoteAsset.String(),
		Source:     source,
		At:         at.UTC(),
	}
}

// PriceString renders a tick count as a decimal price, for display and terms.
func (t BookTerms) PriceString(ticks int64) string {
	return money.QuantityFromInt64(ticks).Mul(t.TickSize).ToDecimalString(uint8(t.PriceScale)) // #nosec G115 -- PriceScale is within 0..18
}

// LotsToQuantity is the base-unit quantity of a number of lots.
func (t BookTerms) LotsToQuantity(lots int64) money.Quantity {
	return money.QuantityFromInt64(lots).Mul(t.LotSize)
}

// Notional is the exact quote-asset notional of qty lots at price ticks.
func (t BookTerms) Notional(ticks, lots int64) money.Quantity {
	return money.QuantityFromInt64(ticks).Mul(money.QuantityFromInt64(lots)).Mul(t.NotionalPerTickLot)
}
