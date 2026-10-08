package fees

import (
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// Side is the direction of an order relative to its base asset. BUY pays the
// quote asset and receives the base asset; SELL is the reverse.
type Side string

// Sides.
const (
	SideBuy  Side = "BUY"
	SideSell Side = "SELL"
)

// Valid reports whether s is declared.
func (s Side) Valid() bool { return s == SideBuy || s == SideSell }

// EffectivePriceExtraScale is how many more decimal places the all-in
// effective price carries than the reference price (at most
// money.MaxPriceScale in total). The execution price needs four more to be
// exact; the remaining eight resolve the explicit fees per unit.
const EffectivePriceExtraScale int32 = 12

// QuoteInput is everything about one order that a schedule prices.
// BaseQty is in base units of an asset with BaseDecimals decimals; the
// reference price must be quoted in the schedule's quote asset (its
// QuoteAsset is that asset's id string, as internal/quote writes it) and
// QuoteDecimals must equal the schedule's. TrailingVolume is the customer's
// trailing 30-day notional volume, computed by the caller.
type QuoteInput struct {
	Side               Side
	BaseQty            money.Quantity
	BaseDecimals       uint8
	QuoteDecimals      uint8
	ReferencePrice     money.Price
	TrailingVolume     money.USD
	HasSubscription    bool
	StrategyOriginated bool
}

// SpreadCharge is the disclosed spread: the rate applied to the reference
// price and the amount it adds to a BUY or removes from a SELL, in quote
// base units.
type SpreadCharge struct {
	BPS      money.BPS
	Amount   money.Quantity
	Rounding money.RoundingMode
}

// FeeLine is one explicit fee with its inputs.
//
// Computed is the amount the schedule's rate (or fixed amount) produces;
// CapReduction is what a cap removed from it; Charged = Computed −
// CapReduction is what the customer pays. RateBPS is the rate actually
// applied to this order (zero for the fixed fee, and for the strategy fee on
// an order that is not strategy-originated); DiscountBPS is the subscription
// discount applied to the percentage fee. Rounding is the mode used for the
// basis-point product; the fixed fee is exact by construction and says so.
type FeeLine struct {
	Component    Component
	RateBPS      money.BPS
	DiscountBPS  money.BPS
	FixedAmount  money.Quantity
	Rounding     money.RoundingMode
	Computed     money.Quantity
	CapReduction money.Quantity
	Charged      money.Quantity
}

// FeeBounds records the minimum and caps of a schedule and which of them
// changed the explicit fees. MaxBPSAmount is the basis-point cap expressed in
// quote base units for this order (zero when that cap is disabled).
//
// MinimumApplied means the fees were raised above their computed total;
// MaxBPSApplied and MaxAbsoluteApplied mean that cap is what the total was
// limited to. A cap always wins over the minimum, so a small order can show
// both: the minimum raised the fee and a cap limited the raise.
type FeeBounds struct {
	MinFee             money.Quantity
	MaxTotalFeeBPS     money.BPS
	MaxBPSAmount       money.Quantity
	MaxBPSRounding     money.RoundingMode
	MaxTotalFee        money.Quantity
	MinimumApplied     bool
	MaxBPSApplied      bool
	MaxAbsoluteApplied bool
}

// ExplicitFees itemizes every explicit fee of a quote.
//
// Total = Σ Charged + MinimumTopUp, and ComputedTotal = Σ Computed. At most
// one of MinimumTopUp and the cap reductions is non-zero. A cap removes
// amounts in a fixed order: the percentage fee, then the strategy fee, then
// the fixed fee, and only then the venue pass-through, so the platform's own
// fees absorb a cap before the recovery of a cost it actually pays.
type ExplicitFees struct {
	Fixed            FeeLine
	Percentage       FeeLine
	VenuePassThrough FeeLine
	Strategy         FeeLine
	MinimumTopUp     money.Quantity
	ComputedTotal    money.Quantity
	Total            money.Quantity
	Bounds           FeeBounds
}

// Lines returns the fee lines in disclosure order.
func (f ExplicitFees) Lines() []FeeLine {
	return []FeeLine{f.Fixed, f.Percentage, f.VenuePassThrough, f.Strategy}
}

// capReductionOrder returns pointers to the lines in the order a cap reduces
// them.
func (f *ExplicitFees) capReductionOrder() []*FeeLine {
	return []*FeeLine{&f.Percentage, &f.Strategy, &f.Fixed, &f.VenuePassThrough}
}

// NetworkCost is the disclosed network estimate. It is part of a quote's
// total only when it is denominated in the quote asset (InQuoteAsset);
// otherwise it is a separate cost in its own asset and is never converted.
type NetworkCost struct {
	Asset        assets.AssetID
	Amount       money.Quantity
	InQuoteAsset bool
}

// inTotal returns the part of the network cost that a quote-asset total
// includes.
func (n NetworkCost) inTotal() money.Quantity {
	if n.InQuoteAsset {
		return n.Amount
	}
	return money.Quantity{}
}

// ReserveHold is the slippage reserve: an amount HELD against adverse
// execution and released if unused. It is never charged and never part of
// TotalDebit or TotalCredit.
type ReserveHold struct {
	BPS      money.BPS
	Amount   money.Quantity
	Rounding money.RoundingMode
}

// Quote is the fully itemized price of one order under one schedule. Every
// amount is in base units of QuoteAsset unless it says otherwise.
//
// The conservation identities hold exactly, by construction, and VerifyQuote
// re-proves them:
//
//	BUY:  TotalDebit  = NotionalAtReference + Spread + Fees.Total + network (when in the quote asset)
//	SELL: TotalCredit = NotionalAtReference − Spread − Fees.Total − network (when in the quote asset)
//	NotionalAtExecution = NotionalAtReference ± Spread
//
// Exactly one of TotalDebit (BUY) and TotalCredit (SELL) is meaningful; the
// other is zero. NotionalAtExecution is defined as the reference notional
// plus or minus the disclosed spread, so the spread line is exactly the
// difference the customer pays; it is within two base units of quantity ×
// ExecutionPrice, and never against the customer under the default rounding.
//
// WorstCaseTotal includes the slippage reserve: for a BUY it is the most
// that can be debited (the amount to hold), for a SELL the least that can be
// credited, floored at zero.
type Quote struct {
	ScheduleVersion string
	ScheduleHash    string
	Input           QuoteInput
	QuoteAsset      assets.AssetID
	Tier            AppliedTier

	ReferencePrice money.Price
	// ExecutionPrice is ref × (1 ± spread) exactly: its scale is the
	// reference scale plus four, less any trailing zeros.
	ExecutionPrice      money.Price
	NotionalAtReference money.Quantity
	NotionalRounding    money.RoundingMode
	NotionalAtExecution money.Quantity

	Spread          SpreadCharge
	Fees            ExplicitFees
	Network         NetworkCost
	SlippageReserve ReserveHold

	TotalDebit     money.Quantity
	TotalCredit    money.Quantity
	WorstCaseTotal money.Quantity

	// EffectivePrice is the all-in price per whole base unit: the total
	// divided by the quantity. It is for disclosure only and is rounded
	// against flattering the platform: up for a BUY, down for a SELL, at
	// the reference scale plus EffectivePriceExtraScale.
	EffectivePrice         money.Price
	EffectivePriceRounding money.RoundingMode

	// Rounding is the schedule's rounding policy, copied onto the quote.
	Rounding RoundingPolicy
}

// Total returns TotalDebit for a BUY and TotalCredit for a SELL.
func (q Quote) Total() money.Quantity {
	if q.Input.Side == SideSell {
		return q.TotalCredit
	}
	return q.TotalDebit
}

// QuoteAssetCharges returns everything the quote charges in the quote asset
// on top of (BUY) or out of (SELL) the reference notional: the spread, the
// explicit fees and a network cost denominated in the quote asset.
func (q Quote) QuoteAssetCharges() money.Quantity {
	return q.Spread.Amount.Add(q.Fees.Total).Add(q.Network.inTotal())
}

// Quote prices one order. It validates the schedule and the input, computes
// every component exactly and rounds each one once in the mode the schedule
// names, and assembles the totals from the itemized components only, so
// nothing can be charged that is not disclosed.
func (s Schedule) Quote(in QuoteInput) (Quote, error) {
	if err := s.Validate(); err != nil {
		return Quote{}, err
	}
	if err := s.checkInput(in); err != nil {
		return Quote{}, err
	}
	hash, err := s.Hash()
	if err != nil {
		return Quote{}, err
	}
	tier := s.resolveTier(in.TrailingVolume)
	exact := referenceNotional(in)
	q := Quote{
		ScheduleVersion: s.Version,
		ScheduleHash:    hash,
		Input:           in,
		QuoteAsset:      s.QuoteAsset,
		Tier:            tier,
		ReferencePrice:  in.ReferencePrice,
		Rounding:        s.Rounding,
	}

	q.NotionalRounding, _ = s.Rounding.notionalMode(in.Side)
	if q.NotionalAtReference, err = money.Notional(in.BaseQty, in.BaseDecimals, in.ReferencePrice, in.QuoteDecimals, q.NotionalRounding); err != nil {
		return Quote{}, arithmeticError(err)
	}
	spread, err := exact.mulBPS(tier.SpreadBPS).round(s.Rounding.Spread)
	if err != nil {
		return Quote{}, arithmeticError(err)
	}
	q.Spread = SpreadCharge{BPS: tier.SpreadBPS, Amount: spread, Rounding: s.Rounding.Spread}
	if q.ExecutionPrice, err = executionPrice(in.ReferencePrice, in.Side, tier.SpreadBPS, s.Version); err != nil {
		return Quote{}, err
	}
	if q.Fees, err = s.explicitFees(in, tier, exact); err != nil {
		return Quote{}, err
	}
	q.Network = NetworkCost{Asset: s.NetworkFee.Asset, Amount: s.NetworkFee.Amount, InQuoteAsset: s.NetworkFee.Asset == s.QuoteAsset}
	reserve, err := exact.mulBPS(s.SlippageReserveBPS).round(s.Rounding.SlippageReserve)
	if err != nil {
		return Quote{}, arithmeticError(err)
	}
	q.SlippageReserve = ReserveHold{BPS: s.SlippageReserveBPS, Amount: reserve, Rounding: s.Rounding.SlippageReserve}

	switch in.Side {
	case SideBuy:
		q.NotionalAtExecution = q.NotionalAtReference.Add(q.Spread.Amount)
		q.TotalDebit = q.NotionalAtReference.Add(q.QuoteAssetCharges())
		q.WorstCaseTotal = q.TotalDebit.Add(reserve)
	case SideSell:
		q.NotionalAtExecution = q.NotionalAtReference.Sub(q.Spread.Amount)
		q.TotalCredit = q.NotionalAtReference.Sub(q.QuoteAssetCharges())
		if q.NotionalAtExecution.IsNegative() || q.TotalCredit.IsNegative() {
			return Quote{}, errs.New(errs.CodeValidationFailed, "fees: order too small to cover its charges").
				WithField("base_qty", "the proceeds of this sell do not cover its spread and fees")
		}
		q.WorstCaseTotal = q.TotalCredit.Sub(reserve).Max(money.Quantity{})
	}
	if q.EffectivePrice, q.EffectivePriceRounding, err = effectivePrice(q.Total(), in, s.Version); err != nil {
		return Quote{}, err
	}
	return q, nil
}

// checkInput validates an order against the schedule it is priced under.
func (s Schedule) checkInput(in QuoteInput) error {
	f := fieldErrors{}
	if !in.Side.Valid() {
		f.add("side", "must be BUY or SELL")
	}
	if !in.BaseQty.IsPositive() {
		f.add("base_qty", "must be positive")
	}
	switch err := in.ReferencePrice.Validate(); {
	case err != nil:
		f.add("reference_price", err.Error())
	case !in.ReferencePrice.Mantissa.IsPositive():
		f.add("reference_price", "must be positive")
	case in.ReferencePrice.QuoteAsset != s.QuoteAsset.String():
		f.add("reference_price.quote_asset", "must be the schedule's quote asset id")
	}
	if in.QuoteDecimals != s.QuoteDecimals {
		f.addf("quote_decimals", "must equal the schedule's quote decimals (%d)", s.QuoteDecimals)
	}
	if in.TrailingVolume.IsNegative() {
		f.add("trailing_volume", "must not be negative")
	}
	return f.err("fees: invalid quote input")
}

// referenceNotional returns quantity × reference price in quote base units,
// exactly: qty × mantissa × 10^quoteDecimals / 10^(baseDecimals + scale).
func referenceNotional(in QuoteInput) rational {
	return rational{
		num: in.BaseQty.Mul(in.ReferencePrice.Mantissa).Mul(pow10(int(in.QuoteDecimals))),
		den: pow10(int(in.BaseDecimals) + int(in.ReferencePrice.Scale)),
	}
}

// explicitFees computes, bounds and allocates the explicit fees.
func (s Schedule) explicitFees(in QuoteInput, tier AppliedTier, exact rational) (ExplicitFees, error) {
	var f ExplicitFees
	f.Fixed = FeeLine{Component: ComponentFixedFee, FixedAmount: s.FixedFee, Rounding: money.RoundExact, Computed: s.FixedFee}

	discount := money.BPS(0)
	if in.HasSubscription {
		discount = s.SubscriptionDiscountBPS
	}
	pct, err := exact.mulBPS(tier.PercentageFeeBPS).mulBPS(money.OneHundredPercent - discount).round(s.Rounding.PercentageFee)
	if err != nil {
		return ExplicitFees{}, arithmeticError(err)
	}
	f.Percentage = FeeLine{Component: ComponentPercentageFee, RateBPS: tier.PercentageFeeBPS, DiscountBPS: discount, Rounding: s.Rounding.PercentageFee, Computed: pct}

	venue, err := exact.mulBPS(s.VenuePassThroughBPS).round(s.Rounding.VenuePassThrough)
	if err != nil {
		return ExplicitFees{}, arithmeticError(err)
	}
	f.VenuePassThrough = FeeLine{Component: ComponentVenuePassThrough, RateBPS: s.VenuePassThroughBPS, Rounding: s.Rounding.VenuePassThrough, Computed: venue}

	strategyRate := money.BPS(0)
	if in.StrategyOriginated {
		strategyRate = s.StrategyFeeBPS
	}
	strategy, err := exact.mulBPS(strategyRate).round(s.Rounding.StrategyFee)
	if err != nil {
		return ExplicitFees{}, arithmeticError(err)
	}
	f.Strategy = FeeLine{Component: ComponentStrategyFee, RateBPS: strategyRate, Rounding: s.Rounding.StrategyFee, Computed: strategy}

	for _, l := range f.capReductionOrder() {
		l.Charged = l.Computed
		f.ComputedTotal = f.ComputedTotal.Add(l.Computed)
	}

	b := FeeBounds{MinFee: s.MinFee, MaxTotalFeeBPS: s.MaxTotalFeeBPS, MaxBPSRounding: s.Rounding.FeeCap, MaxTotalFee: s.MaxTotalFee}
	preCap := f.ComputedTotal.Max(s.MinFee)
	bounded := preCap
	if s.MaxTotalFeeBPS > 0 {
		if b.MaxBPSAmount, err = exact.mulBPS(s.MaxTotalFeeBPS).round(s.Rounding.FeeCap); err != nil {
			return ExplicitFees{}, arithmeticError(err)
		}
		bounded = bounded.Min(b.MaxBPSAmount)
	}
	if s.MaxTotalFee.IsPositive() {
		bounded = bounded.Min(s.MaxTotalFee)
	}
	b.MinimumApplied = bounded.Cmp(f.ComputedTotal) > 0
	b.MaxBPSApplied = s.MaxTotalFeeBPS > 0 && b.MaxBPSAmount.Cmp(preCap) < 0 && bounded.Equal(b.MaxBPSAmount)
	b.MaxAbsoluteApplied = s.MaxTotalFee.IsPositive() && s.MaxTotalFee.Cmp(preCap) < 0 && bounded.Equal(s.MaxTotalFee)
	f.Bounds = b

	if bounded.Cmp(f.ComputedTotal) >= 0 {
		f.MinimumTopUp = bounded.Sub(f.ComputedTotal)
	} else {
		excess := f.ComputedTotal.Sub(bounded)
		for _, l := range f.capReductionOrder() {
			cut := excess.Min(l.Computed)
			l.CapReduction = cut
			l.Charged = l.Computed.Sub(cut)
			excess = excess.Sub(cut)
		}
	}
	f.Total = bounded
	return f, nil
}

// executionPrice returns ref × (10_000 ± spread) / 10_000 as an exact price.
func executionPrice(ref money.Price, side Side, spread money.BPS, version string) (money.Price, error) {
	factor := money.OneHundredPercent + spread
	if side == SideSell {
		factor = money.OneHundredPercent - spread
	}
	mantissa, scale := trimTrailingZeros(ref.Mantissa.Mul(money.QuantityFromInt64(int64(factor))), ref.Scale+bpsScale, ref.Scale)
	if scale > money.MaxPriceScale {
		return money.Price{}, errs.Newf(errs.CodePrecisionLoss,
			"fees: the execution price needs %d decimal places, more than a price can carry (%d)", scale, money.MaxPriceScale)
	}
	p, err := money.NewPrice(mantissa, scale, ref.QuoteAsset, derivedSource(ref.Source, "spread", version), ref.At)
	if err != nil {
		return money.Price{}, arithmeticError(err)
	}
	return p, nil
}

// effectivePriceScale returns the scale the effective price is computed at.
func effectivePriceScale(ref money.Price) int32 {
	return min(money.MaxPriceScale, ref.Scale+EffectivePriceExtraScale)
}

// effectivePriceRounding returns the display rounding of the all-in price:
// never understate a buyer's cost, never overstate a seller's proceeds.
func effectivePriceRounding(side Side) money.RoundingMode {
	if side == SideSell {
		return money.RoundDown
	}
	return money.RoundUp
}

// effectivePrice returns total / quantity as a price per whole base unit.
func effectivePrice(total money.Quantity, in QuoteInput, version string) (money.Price, money.RoundingMode, error) {
	mode := effectivePriceRounding(in.Side)
	scale := effectivePriceScale(in.ReferencePrice)
	mantissa, err := effectivePriceRational(total, in, scale).round(mode)
	if err != nil {
		return money.Price{}, 0, arithmeticError(err)
	}
	mantissa, scale = trimTrailingZeros(mantissa, scale, min(scale, in.ReferencePrice.Scale))
	p, err := money.NewPrice(mantissa, scale, in.ReferencePrice.QuoteAsset, derivedSource(in.ReferencePrice.Source, "all-in", version), in.ReferencePrice.At)
	if err != nil {
		return money.Price{}, 0, arithmeticError(err)
	}
	return p, mode, nil
}

// effectivePriceRational is the exact mantissa of total / quantity at scale:
// total × 10^(baseDecimals + scale) / (qty × 10^quoteDecimals).
func effectivePriceRational(total money.Quantity, in QuoteInput, scale int32) rational {
	return rational{
		num: total.Mul(pow10(int(in.BaseDecimals) + int(scale))),
		den: in.BaseQty.Mul(pow10(int(in.QuoteDecimals))),
	}
}

// derivedSource names a price this package derived from an observed one:
// the observation's source, what was applied, and the schedule version.
func derivedSource(refSource, what, version string) string {
	return refSource + "+" + what + "@" + version
}
