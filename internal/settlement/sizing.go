package settlement

import (
	"time"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuation"
)

// USDQuoteAsset is the QuoteAsset every PlannerInput price must carry:
// prices are USD-quoted observations of one whole asset unit.
const USDQuoteAsset = "USD"

// sizing is the outcome of sizing an intent (§4).
type sizing struct {
	side        execution.Side
	class       killswitch.ActionClass
	inputAsset  assets.Asset
	outputAsset assets.Asset
	baseAsset   assets.Asset
	quoteAsset  assets.Asset
	// inputQuantity is the reserved / debited quantity in input base units.
	inputQuantity money.Quantity
	// baseQuantity is the base-asset quantity bought or sold (for sells it
	// equals inputQuantity; for buys it is an estimate from the reference
	// price and may be zero when no price is known).
	baseQuantity money.Quantity
	// notionalUSD is the USD value of the trade.
	notionalUSD money.USD
	// notionalQuote is the same in quote-asset base units (parity).
	notionalQuote money.Quantity
	// minOutput is the hard floor for the output leg.
	minOutput money.Quantity
	// referencePrice is the base asset's USD price used for estimates (nil
	// when unknown).
	referencePrice *money.Price
	// settlementBasis records how the settlement asset was sized: FACE or
	// MARKET (with haircut).
	settlementBasis valuation.Basis
}

func (s sizing) isBuy() bool { return s.side == execution.SideBuy }

// size computes the trade size for the intent. It adds NO_VALID_PLAN
// reasons to rs and returns ok=false when sizing is impossible; a
// VALIDATION_FAILED error means the input itself is malformed.
func (p *V1Planner) size(in PlannerInput, rs *reasonSet) (sizing, bool, error) {
	base, quote, settle, err := instrumentAssets(in)
	if err != nil {
		return sizing{}, false, err
	}
	// V1 executes single-venue spot swaps settled in the quote asset. A
	// settlement asset that differs from the quote asset would need a
	// CONVERT step, which is reserved: money on the wrong rail.
	if settle.ID != quote.ID {
		rs.add(ReasonSettlementAssetUnavailable, "settlement asset differs from the instrument quote asset (CONVERT is reserved)")
		return sizing{}, false, nil
	}
	s := sizing{baseAsset: base, quoteAsset: quote}
	refPrice, priceOK := p.usdPrice(in, base.ID)
	if priceOK {
		s.referencePrice = &refPrice
	}
	switch in.Intent.Action {
	case ActionAcquireNotional:
		if in.Intent.NotionalUSD == nil || !in.Intent.NotionalUSD.IsPositive() {
			return sizing{}, false, errs.New(errs.CodeValidationFailed, "settlement: ACQUIRE_NOTIONAL requires a positive notional")
		}
		return p.sizeAcquire(in, s, *in.Intent.NotionalUSD, rs)
	case ActionReduceNotional:
		if in.Intent.Quantity != nil && in.Intent.Quantity.IsPositive() {
			return p.sizeReduceQuantity(in, s, *in.Intent.Quantity, rs)
		}
		if in.Intent.NotionalUSD == nil || !in.Intent.NotionalUSD.IsPositive() {
			return sizing{}, false, errs.New(errs.CodeValidationFailed, "settlement: REDUCE_NOTIONAL requires a positive notional or quantity")
		}
		return p.sizeReduceNotional(in, s, *in.Intent.NotionalUSD, rs)
	case ActionClosePosition:
		open := openHolding(in, base.ID)
		if !open.IsPositive() {
			rs.add(ReasonNotionalBelowMinimum, "no open position to close")
			return sizing{}, false, nil
		}
		return p.sizeReduceQuantity(in, s, open, rs)
	case ActionTargetExposure:
		if in.Intent.TargetExposureUSD == nil || in.Intent.TargetExposureUSD.IsNegative() {
			return sizing{}, false, errs.New(errs.CodeValidationFailed, "settlement: TARGET_EXPOSURE requires a target")
		}
		return p.sizeTarget(in, s, *in.Intent.TargetExposureUSD, rs)
	}
	return sizing{}, false, errs.New(errs.CodeValidationFailed, "settlement: unknown intent action").WithField("action", string(in.Intent.Action))
}

// sizeAcquire converts N USD into settlement-asset base units under the
// stablecoin's current policy: NORMAL at face, DEGRADED at market price with
// the collateral haircut, anything else unavailable.
func (p *V1Planner) sizeAcquire(in PlannerInput, s sizing, notional money.USD, rs *reasonSet) (sizing, bool, error) {
	s.side, s.class = execution.SideBuy, killswitch.NewRisk
	s.inputAsset, s.outputAsset = s.quoteAsset, s.baseAsset
	s.notionalUSD = notional
	policy, ok := in.AssetPolicies[s.quoteAsset.ID]
	if !ok {
		policy = valuation.FailClosedPolicy(s.quoteAsset.ID)
	}
	cls := valuation.Classify(s.quoteAsset, policy)
	if cls.Gate != valuation.GateEligible {
		rs.add(ReasonSettlementAssetUnavailable, "settlement asset is not eligible: "+cls.Reason)
		return s, false, nil
	}
	var units money.Quantity
	switch cls.Basis {
	case valuation.BasisFace:
		u, err := money.USDToQuoteQuantity(notional, s.quoteAsset.Decimals, money.RoundHalfEven)
		if err != nil {
			return s, false, moneyErr(err)
		}
		units = u
		s.settlementBasis = valuation.BasisFace
	case valuation.BasisMarket:
		price, ok := p.usdPrice(in, s.quoteAsset.ID)
		if !ok {
			rs.add(ReasonSettlementAssetUnavailable, "settlement asset is DEGRADED and no fresh market price is known")
			return s, false, nil
		}
		if cls.Factor <= 0 {
			rs.add(ReasonSettlementAssetUnavailable, "settlement asset contributes nothing after its haircut")
			return s, false, nil
		}
		u, err := usdToUnitsAtPrice(notional, s.quoteAsset.Decimals, price, cls.Factor, money.RoundUp)
		if err != nil {
			return s, false, moneyErr(err)
		}
		units = u
		s.settlementBasis = valuation.BasisMarket
	default:
		rs.add(ReasonSettlementAssetUnavailable, "settlement asset has no valuation basis")
		return s, false, nil
	}
	if !units.IsPositive() {
		rs.add(ReasonNotionalBelowMinimum, "notional rounds to zero settlement-asset units")
		return s, false, nil
	}
	s.inputQuantity = units
	s.notionalQuote = units
	if s.referencePrice != nil {
		// Estimated base output at the reference price (informational).
		if q, err := usdToUnitsAtPrice(notional, s.baseAsset.Decimals, *s.referencePrice, money.OneHundredPercent, money.RoundDown); err == nil {
			s.baseQuantity = q
		}
	}
	minOut, err := buyMinOutput(in, s)
	if err != nil {
		return s, false, err
	}
	s.minOutput = minOut
	return s, true, nil
}

// sizeReduceQuantity sells qty base units, capped at the open holding.
func (p *V1Planner) sizeReduceQuantity(in PlannerInput, s sizing, qty money.Quantity, rs *reasonSet) (sizing, bool, error) {
	open := openHolding(in, s.baseAsset.ID)
	if !open.IsPositive() {
		rs.add(ReasonNotionalBelowMinimum, "no open position to reduce")
		return s, false, nil
	}
	if qty.Cmp(open) > 0 {
		qty = open
	}
	if s.referencePrice == nil {
		rs.add(ReasonProviderDegraded, "no fresh price for the base asset; a sale cannot be valued")
		return s, false, nil
	}
	usd, err := unitsToUSDAtPrice(qty, s.baseAsset.Decimals, *s.referencePrice)
	if err != nil {
		return s, false, moneyErr(err)
	}
	return p.finishSell(in, s, qty, usd, rs)
}

// sizeReduceNotional sells the base quantity worth N USD at the reference
// price, capped at the open holding.
func (p *V1Planner) sizeReduceNotional(in PlannerInput, s sizing, notional money.USD, rs *reasonSet) (sizing, bool, error) {
	open := openHolding(in, s.baseAsset.ID)
	if !open.IsPositive() {
		rs.add(ReasonNotionalBelowMinimum, "no open position to reduce")
		return s, false, nil
	}
	if s.referencePrice == nil {
		rs.add(ReasonProviderDegraded, "no fresh price for the base asset; a notional reduction cannot be sized")
		return s, false, nil
	}
	qty, err := usdToUnitsAtPrice(notional, s.baseAsset.Decimals, *s.referencePrice, money.OneHundredPercent, money.RoundDown)
	if err != nil {
		return s, false, moneyErr(err)
	}
	if qty.Cmp(open) > 0 {
		qty = open
	}
	if !qty.IsPositive() {
		rs.add(ReasonNotionalBelowMinimum, "notional rounds to zero base units")
		return s, false, nil
	}
	usd, err := unitsToUSDAtPrice(qty, s.baseAsset.Decimals, *s.referencePrice)
	if err != nil {
		return s, false, moneyErr(err)
	}
	return p.finishSell(in, s, qty, usd, rs)
}

func (p *V1Planner) finishSell(in PlannerInput, s sizing, qty money.Quantity, usd money.USD, rs *reasonSet) (sizing, bool, error) {
	s.side, s.class = execution.SideSell, killswitch.ReduceRisk
	s.inputAsset, s.outputAsset = s.baseAsset, s.quoteAsset
	s.inputQuantity, s.baseQuantity = qty, qty
	s.notionalUSD = usd
	nq, err := money.USDToQuoteQuantity(usd, s.quoteAsset.Decimals, money.RoundHalfEven)
	if err != nil {
		return s, false, moneyErr(err)
	}
	s.notionalQuote = nq
	if !usd.IsPositive() {
		rs.add(ReasonNotionalBelowMinimum, "sale is worth less than one cent")
		return s, false, nil
	}
	minOut, err := sellMinOutput(in, s)
	if err != nil {
		return s, false, err
	}
	s.minOutput = minOut
	return s, true, nil
}

// sizeTarget computes the delta between the target and the current marked
// exposure and becomes an acquire or a reduce.
func (p *V1Planner) sizeTarget(in PlannerInput, s sizing, target money.USD, rs *reasonSet) (sizing, bool, error) {
	open := openHolding(in, s.baseAsset.ID)
	current := money.USD{}
	if open.IsPositive() {
		if s.referencePrice == nil {
			rs.add(ReasonProviderDegraded, "no fresh price for the base asset; current exposure cannot be marked")
			return s, false, nil
		}
		usd, err := unitsToUSDAtPrice(open, s.baseAsset.Decimals, *s.referencePrice)
		if err != nil {
			return s, false, moneyErr(err)
		}
		current = usd
	}
	delta, err := target.Sub(current)
	if err != nil {
		return s, false, moneyErr(err)
	}
	abs, err := delta.Abs()
	if err != nil {
		return s, false, moneyErr(err)
	}
	if abs.Cmp(p.opts.MinDeltaUSD) < 0 {
		rs.add(ReasonDeltaBelowMinimum, "exposure delta "+abs.String()+" is below the minimum "+p.opts.MinDeltaUSD.String())
		return s, false, nil
	}
	if delta.IsPositive() {
		return p.sizeAcquire(in, s, delta, rs)
	}
	return p.sizeReduceNotional(in, s, abs, rs)
}

// buyMinOutput derives the output floor of a buy from MinReceive and the
// limit price.
func buyMinOutput(in PlannerInput, s sizing) (money.Quantity, error) {
	minOut := money.Quantity{}
	if mr := in.Intent.Constraints.MinReceive; mr != nil && mr.IsPositive() {
		minOut = *mr
	}
	if lim := in.Intent.Constraints.MaxPrice; lim != nil {
		if err := lim.Validate(); err != nil {
			return money.Quantity{}, errs.Wrap(err, errs.CodeValidationFailed, "settlement: invalid max price")
		}
		// base units = input quote units × 10^(baseDec + scale) / (mantissa × 10^quoteDec)
		num := s.inputQuantity.ScaleUp(s.baseAsset.Decimals).ScaleUp(scaleDigits(lim.Scale))
		den := lim.Mantissa.ScaleUp(s.quoteAsset.Decimals)
		if !den.IsPositive() {
			return money.Quantity{}, errs.New(errs.CodeValidationFailed, "settlement: max price must be positive")
		}
		q, err := num.Div(den, money.RoundDown)
		if err != nil {
			return money.Quantity{}, moneyErr(err)
		}
		minOut = minOut.Max(q)
	}
	return minOut, nil
}

// sellMinOutput derives the output floor of a sell from MinReceive and the
// limit price (the least received per whole base unit).
func sellMinOutput(in PlannerInput, s sizing) (money.Quantity, error) {
	minOut := money.Quantity{}
	if mr := in.Intent.Constraints.MinReceive; mr != nil && mr.IsPositive() {
		minOut = *mr
	}
	if lim := in.Intent.Constraints.MaxPrice; lim != nil {
		if err := lim.Validate(); err != nil {
			return money.Quantity{}, errs.Wrap(err, errs.CodeValidationFailed, "settlement: invalid max price")
		}
		// quote units = base units × mantissa × 10^quoteDec / 10^(baseDec + scale)
		num := s.inputQuantity.Mul(lim.Mantissa).ScaleUp(s.quoteAsset.Decimals)
		den := money.QuantityFromInt64(1).ScaleUp(s.baseAsset.Decimals).ScaleUp(scaleDigits(lim.Scale))
		q, err := num.Div(den, money.RoundDown)
		if err != nil {
			return money.Quantity{}, moneyErr(err)
		}
		minOut = minOut.Max(q)
	}
	return minOut, nil
}

// instrumentAssets resolves the instrument's base, quote and settlement
// assets from the input registry snapshot.
func instrumentAssets(in PlannerInput) (base, quote, settle assets.Asset, err error) {
	inst := in.Instrument
	if inst.BaseAssetID == nil || inst.QuoteAssetID == nil {
		return base, quote, settle, errs.New(errs.CodeValidationFailed, "settlement: instrument needs base and quote assets")
	}
	var ok bool
	if base, ok = in.Assets[*inst.BaseAssetID]; !ok {
		return base, quote, settle, errs.New(errs.CodeValidationFailed, "settlement: base asset missing from input").WithField("asset_id", inst.BaseAssetID.String())
	}
	if quote, ok = in.Assets[*inst.QuoteAssetID]; !ok {
		return base, quote, settle, errs.New(errs.CodeValidationFailed, "settlement: quote asset missing from input").WithField("asset_id", inst.QuoteAssetID.String())
	}
	if settle, ok = in.Assets[inst.SettlementAssetID]; !ok {
		return base, quote, settle, errs.New(errs.CodeValidationFailed, "settlement: settlement asset missing from input").WithField("asset_id", inst.SettlementAssetID.String())
	}
	if base.ID == quote.ID {
		return base, quote, settle, errs.New(errs.CodeValidationFailed, "settlement: base and quote assets must differ")
	}
	return base, quote, settle, nil
}

// openHolding returns the account's open quantity in an asset.
func openHolding(in PlannerInput, asset assets.AssetID) money.Quantity {
	total := money.Quantity{}
	for _, h := range in.Holdings {
		if h.AssetID == asset {
			total = total.Add(h.Quantity)
		}
	}
	return total
}

// usdPrice returns the fresh USD price of an asset, or false when none is
// known, the price is malformed, is not USD-quoted, or is older than the
// asset policy's maximum age (the planner's default when the policy has
// none).
func (p *V1Planner) usdPrice(in PlannerInput, asset assets.AssetID) (money.Price, bool) {
	price, ok := in.Prices[asset]
	if !ok {
		return money.Price{}, false
	}
	if err := price.Validate(); err != nil || price.QuoteAsset != USDQuoteAsset || !price.Mantissa.IsPositive() {
		return money.Price{}, false
	}
	maxAge := p.opts.DefaultMaxPriceAge
	if pol, ok := in.AssetPolicies[asset]; ok && pol.MaxPriceAge > 0 {
		maxAge = pol.MaxPriceAge
	}
	if price.At.After(in.Now) || in.Now.Sub(price.At) > maxAge {
		return money.Price{}, false
	}
	return price, true
}

func scaleDigits(scale int32) uint8 {
	if scale < 0 || scale > money.MaxPriceScale {
		return 0
	}
	return uint8(scale) // #nosec G115 -- bounded to [0, MaxPriceScale] above
}

// usdToUnitsAtPrice converts a USD amount into base units of an asset with
// the given decimals at a USD price, applying a haircut factor in basis
// points (10_000 = none):
//
//	units = usdMinor × 10^(decimals + scale) × 10_000 / (100 × mantissa × factor)
func usdToUnitsAtPrice(usd money.USD, decimals uint8, price money.Price, factor money.BPS, mode money.RoundingMode) (money.Quantity, error) {
	if err := price.Validate(); err != nil {
		return money.Quantity{}, err
	}
	if factor <= 0 || !price.Mantissa.IsPositive() {
		return money.Quantity{}, money.ErrDivisionByZero
	}
	num := money.QuantityFromInt64(usd.Minor()).ScaleUp(decimals).ScaleUp(scaleDigits(price.Scale)).Mul(money.QuantityFromInt64(int64(money.OneHundredPercent)))
	den := money.QuantityFromInt64(100).Mul(price.Mantissa).Mul(money.QuantityFromInt64(int64(factor)))
	return num.Div(den, mode)
}

// unitsToUSDAtPrice values base units of an asset at a USD price:
//
//	cents = units × mantissa × 100 / 10^(decimals + scale)   (RoundHalfEven)
func unitsToUSDAtPrice(units money.Quantity, decimals uint8, price money.Price) (money.USD, error) {
	if err := price.Validate(); err != nil {
		return money.USD{}, err
	}
	num := units.Mul(price.Mantissa).Mul(money.QuantityFromInt64(100))
	den := money.QuantityFromInt64(1).ScaleUp(decimals).ScaleUp(scaleDigits(price.Scale))
	cents, err := num.Div(den, money.RoundHalfEven)
	if err != nil {
		return money.USD{}, err
	}
	minor, err := cents.Int64()
	if err != nil {
		return money.USD{}, err
	}
	return money.USDFromMinor(minor), nil
}

// quoteUnitsToUSD converts quote-asset base units to USD at parity.
func quoteUnitsToUSD(units money.Quantity, decimals uint8) (money.USD, error) {
	return money.QuoteQuantityToUSD(units, decimals, money.RoundHalfEven)
}

func moneyErr(err error) error {
	switch {
	case errs.HasCode(err, errs.CodeValidationFailed):
		return err
	default:
		return errs.Wrap(err, errs.CodeValidationFailed, "settlement: "+err.Error())
	}
}

// deadlineFor returns the effective deadline: the earliest of the intent
// deadline and the execution deadline constraint, or Now + the default.
func (p *V1Planner) deadlineFor(in PlannerInput) time.Time {
	deadline := time.Time{}
	for _, t := range []time.Time{in.Intent.Deadline, in.Intent.Constraints.ExecutionDeadline} {
		if t.IsZero() {
			continue
		}
		if deadline.IsZero() || t.Before(deadline) {
			deadline = t
		}
	}
	if deadline.IsZero() {
		deadline = in.Now.Add(p.opts.DefaultDeadline)
	}
	return deadline.UTC()
}
