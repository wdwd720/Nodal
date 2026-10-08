package fees

import (
	"strings"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// VerifyQuote is what an auditor runs to prove that a quote charges nothing
// that its schedule and input do not account for. It returns nil, or a
// VALIDATION_FAILED *errs.Error whose Fields name every check that failed.
//
// It does not trust the quote's own arithmetic. It re-derives, from the
// schedule and the input alone:
//
//   - identity: the schedule version and content hash, the quote asset, the
//     input and the reference price are the ones being verified;
//   - conservation: every amount is non-negative; each fee line's charged
//     amount is its computed amount less its cap reduction; the fee total is
//     the sum of the charged lines and the minimum top-up; a cap reduces the
//     lines only in the documented order; the execution notional is the
//     reference notional plus or minus the spread; the total debit (BUY) or
//     credit (SELL) is the reference notional plus or minus exactly the
//     spread, the explicit fees and a quote-asset network cost; and the
//     revenue split's quote-asset lines sum to those charges;
//   - rates: the tier the trailing volume selects, and every rate, discount,
//     fixed amount, bound and rounding mode, are the schedule's;
//   - rounding: every rounded amount equals its exact rational value rounded
//     once in the disclosed mode, lies within one base unit of it, and under
//     a customer-favourable mode never charges more (or credits less) than
//     the exact amount;
//   - bounds: the fee total is min(max(computed, minimum), caps) and exceeds
//     no enabled cap;
//   - prices: the execution price is the reference price moved by exactly
//     the spread, and the effective price is the total divided by the
//     quantity at the documented scale and rounding;
//
// and finally that the quote's disclosure is byte-for-byte the disclosure of
// the quote the engine computes for the same schedule and input.
func VerifyQuote(s Schedule, in QuoteInput, q Quote) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if err := s.checkInput(in); err != nil {
		return err
	}
	hash, err := s.Hash()
	if err != nil {
		return err
	}
	f := fieldErrors{}
	verifyIdentity(f, s, hash, in, q)
	verifyConservation(f, q)
	verifyRates(f, s, in, q)
	verifyRounding(f, s, in, q)
	verifyBounds(f, s, q)
	verifyPrices(f, in, q)
	expected, err := s.Quote(in)
	if err != nil {
		f.add("quote", "the schedule refuses this input, so no quote for it can be valid ("+string(errs.CodeOf(err))+")")
	} else {
		diff, err := diffDisclosures(expected.Disclosure(), q.Disclosure())
		if err != nil {
			return err
		}
		for _, k := range diff {
			f.add("disclosure."+k, "differs from the quote recomputed from the schedule and input")
		}
	}
	return f.err("fees: quote does not verify against its schedule and input")
}

// VerifyDisclosure verifies an audit record on its own: it rebuilds the
// input the disclosure records, recomputes the quote from the schedule,
// proves that quote with VerifyQuote, and requires the disclosure to be
// byte-for-byte that quote's disclosure. A disclosure that passes accounts
// for every unit of spread and fee it states.
func VerifyDisclosure(s Schedule, d Disclosure) error {
	in := d.Input.QuoteInput()
	q, err := s.Quote(in)
	if err != nil {
		return err
	}
	if err := VerifyQuote(s, in, q); err != nil {
		return err
	}
	diff, err := diffDisclosures(q.Disclosure(), d)
	if err != nil {
		return err
	}
	f := fieldErrors{}
	for _, k := range diff {
		f.add(k, "differs from the quote recomputed from the schedule and the recorded input")
	}
	return f.err("fees: disclosure does not verify against its schedule")
}

func verifyIdentity(f fieldErrors, s Schedule, hash string, in QuoteInput, q Quote) {
	if q.ScheduleVersion != s.Version {
		f.add("schedule_version", "does not name the schedule")
	}
	if q.ScheduleHash != hash {
		f.add("schedule_hash", "does not match the schedule's content")
	}
	if q.QuoteAsset != s.QuoteAsset {
		f.add("quote_asset", "is not the schedule's quote asset")
	}
	if !sameInput(in, q.Input) {
		f.add("input", "is not the input being verified")
	}
	if !samePrice(in.ReferencePrice, q.ReferencePrice) {
		f.add("reference_price", "is not the input's reference price")
	}
}

func sameInput(a, b QuoteInput) bool {
	return a.Side == b.Side && a.BaseQty.Equal(b.BaseQty) &&
		a.BaseDecimals == b.BaseDecimals && a.QuoteDecimals == b.QuoteDecimals &&
		samePrice(a.ReferencePrice, b.ReferencePrice) && a.TrailingVolume == b.TrailingVolume &&
		a.HasSubscription == b.HasSubscription && a.StrategyOriginated == b.StrategyOriginated
}

func samePrice(a, b money.Price) bool {
	return a.Mantissa.Equal(b.Mantissa) && a.Scale == b.Scale && a.QuoteAsset == b.QuoteAsset &&
		a.Source == b.Source && a.At.Equal(b.At)
}

// lineKey is the field key prefix of a fee line.
func lineKey(c Component) string { return "fees." + strings.ToLower(string(c)) }

type namedAmount struct {
	key string
	v   money.Quantity
}

func verifyConservation(f fieldErrors, q Quote) {
	amounts := []namedAmount{
		{"notional_at_reference", q.NotionalAtReference},
		{"notional_at_execution", q.NotionalAtExecution},
		{"spread.amount", q.Spread.Amount},
		{"fees.minimum_fee_top_up", q.Fees.MinimumTopUp},
		{"fees.computed_total", q.Fees.ComputedTotal},
		{"fees.total", q.Fees.Total},
		{"fees.bounds.max_bps_amount", q.Fees.Bounds.MaxBPSAmount},
		{"network.amount", q.Network.Amount},
		{"slippage_reserve.amount", q.SlippageReserve.Amount},
		{"total_debit", q.TotalDebit},
		{"total_credit", q.TotalCredit},
		{"worst_case_total", q.WorstCaseTotal},
	}
	var computed, charged money.Quantity
	reduced := false
	for _, l := range q.Fees.Lines() {
		k := lineKey(l.Component)
		amounts = append(amounts, namedAmount{k + ".computed", l.Computed}, namedAmount{k + ".cap_reduction", l.CapReduction}, namedAmount{k + ".charged", l.Charged})
		if !l.Charged.Equal(l.Computed.Sub(l.CapReduction)) {
			f.add(k+".charged", "must equal computed minus cap_reduction")
		}
		computed = computed.Add(l.Computed)
		charged = charged.Add(l.Charged)
		reduced = reduced || l.CapReduction.IsPositive()
	}
	for _, a := range amounts {
		if a.v.IsNegative() {
			f.add(a.key, "must not be negative")
		}
	}
	if !q.Fees.ComputedTotal.Equal(computed) {
		f.add("fees.computed_total", "must equal the sum of the computed fee lines")
	}
	if !q.Fees.Total.Equal(charged.Add(q.Fees.MinimumTopUp)) {
		f.add("fees.total", "must equal the sum of the charged fee lines and the minimum-fee top-up")
	}
	if reduced && q.Fees.MinimumTopUp.IsPositive() {
		f.add("fees.minimum_fee_top_up", "a minimum top-up and a cap reduction cannot both apply")
	}
	fees := q.Fees
	blocked := false
	for _, l := range fees.capReductionOrder() {
		if blocked && l.CapReduction.IsPositive() {
			f.add(lineKey(l.Component)+".cap_reduction", "a cap reduces this line only after every line before it in the reduction order is removed")
		}
		blocked = blocked || l.Charged.IsPositive()
	}
	if q.Network.InQuoteAsset != (q.Network.Asset == q.QuoteAsset) {
		f.add("network.in_quote_asset", "must say exactly whether the network cost is in the quote asset")
	}

	charges := q.QuoteAssetCharges()
	n := q.NotionalAtReference
	switch q.Input.Side {
	case SideBuy:
		if !q.NotionalAtExecution.Equal(n.Add(q.Spread.Amount)) {
			f.add("notional_at_execution", "must equal the reference notional plus the spread")
		}
		if !q.TotalDebit.Equal(n.Add(charges)) {
			f.add("total_debit", "must equal the reference notional plus the spread, the explicit fees and a quote-asset network cost")
		}
		if !q.TotalCredit.IsZero() {
			f.add("total_credit", "must be zero on a BUY")
		}
		if !q.WorstCaseTotal.Equal(q.TotalDebit.Add(q.SlippageReserve.Amount)) {
			f.add("worst_case_total", "must equal the total debit plus the slippage reserve")
		}
	case SideSell:
		if !q.NotionalAtExecution.Equal(n.Sub(q.Spread.Amount)) {
			f.add("notional_at_execution", "must equal the reference notional minus the spread")
		}
		if !q.TotalCredit.Equal(n.Sub(charges)) {
			f.add("total_credit", "must equal the reference notional minus the spread, the explicit fees and a quote-asset network cost")
		}
		if !q.TotalDebit.IsZero() {
			f.add("total_debit", "must be zero on a SELL")
		}
		if !q.WorstCaseTotal.Equal(q.TotalCredit.Sub(q.SlippageReserve.Amount).Max(money.Quantity{})) {
			f.add("worst_case_total", "must equal the total credit less the slippage reserve, floored at zero")
		}
	default:
		f.add("input.side", "must be BUY or SELL")
	}
	split := q.RevenueSplit()
	if !split.QuoteAssetTotal().Equal(charges) {
		f.add("revenue_split", "the quote-asset lines must sum to the quote's charges")
	}
}

func verifyRates(f fieldErrors, s Schedule, in QuoteInput, q Quote) {
	tier := s.resolveTier(in.TrailingVolume)
	if q.Tier != tier {
		f.add("tier", "is not the tier the trailing volume selects")
	}
	if q.Spread.BPS != tier.SpreadBPS {
		f.add("spread.bps", "is not the spread of the selected tier")
	}
	discount, strategyRate := appliedDiscount(s, in), appliedStrategyRate(s, in)
	lines := []struct {
		got                    FeeLine
		component              Component
		rate, discount         money.BPS
		fixed                  money.Quantity
		mode                   money.RoundingMode
		computedMustEqualFixed bool
	}{
		{q.Fees.Fixed, ComponentFixedFee, 0, 0, s.FixedFee, money.RoundExact, true},
		{q.Fees.Percentage, ComponentPercentageFee, tier.PercentageFeeBPS, discount, money.Quantity{}, s.Rounding.PercentageFee, false},
		{q.Fees.VenuePassThrough, ComponentVenuePassThrough, s.VenuePassThroughBPS, 0, money.Quantity{}, s.Rounding.VenuePassThrough, false},
		{q.Fees.Strategy, ComponentStrategyFee, strategyRate, 0, money.Quantity{}, s.Rounding.StrategyFee, false},
	}
	for _, l := range lines {
		k := lineKey(l.component)
		switch {
		case l.got.Component != l.component:
			f.add(k+".component", "is not the component of this line")
		case l.got.RateBPS != l.rate:
			f.add(k+".rate_bps", "is not the rate the schedule applies to this order")
		case l.got.DiscountBPS != l.discount:
			f.add(k+".discount_bps", "is not the discount the schedule applies to this order")
		case !l.got.FixedAmount.Equal(l.fixed):
			f.add(k+".fixed_amount", "is not the schedule's fixed amount")
		case l.got.Rounding != l.mode:
			f.add(k+".rounding", "is not the schedule's rounding mode for this line")
		case l.computedMustEqualFixed && !l.got.Computed.Equal(l.fixed):
			f.add(k+".computed", "must equal the schedule's fixed fee")
		}
	}
	if q.SlippageReserve.BPS != s.SlippageReserveBPS || q.SlippageReserve.Rounding != s.Rounding.SlippageReserve {
		f.add("slippage_reserve", "is not the schedule's reserve rate and rounding")
	}
	if q.Network.Asset != s.NetworkFee.Asset || !q.Network.Amount.Equal(s.NetworkFee.Amount) {
		f.add("network", "is not the schedule's network estimate")
	}
	b := q.Fees.Bounds
	if !b.MinFee.Equal(s.MinFee) || b.MaxTotalFeeBPS != s.MaxTotalFeeBPS || !b.MaxTotalFee.Equal(s.MaxTotalFee) || b.MaxBPSRounding != s.Rounding.FeeCap {
		f.add("fees.bounds", "are not the schedule's minimum and caps")
	}
	if q.Rounding != s.Rounding {
		f.add("rounding", "is not the schedule's rounding policy")
	}
	if mode, _ := s.Rounding.notionalMode(in.Side); q.NotionalRounding != mode {
		f.add("notional_rounding", "is not the schedule's notional rounding for this side")
	}
	if q.Spread.Rounding != s.Rounding.Spread {
		f.add("spread.rounding", "is not the schedule's spread rounding")
	}
	if q.EffectivePriceRounding != effectivePriceRounding(in.Side) {
		f.add("effective_price_rounding", "must round up on a BUY and down on a SELL")
	}
}

// appliedDiscount is the subscription discount that applies to in.
func appliedDiscount(s Schedule, in QuoteInput) money.BPS {
	if in.HasSubscription {
		return s.SubscriptionDiscountBPS
	}
	return 0
}

// appliedStrategyRate is the strategy fee rate that applies to in.
func appliedStrategyRate(s Schedule, in QuoteInput) money.BPS {
	if in.StrategyOriginated {
		return s.StrategyFeeBPS
	}
	return 0
}

// roundingCheck is one rounded amount and the exact value it approximates.
type roundingCheck struct {
	key   string
	got   money.Quantity
	exact rational
	mode  money.RoundingMode
	role  roundingRole
}

func verifyRounding(f fieldErrors, s Schedule, in QuoteInput, q Quote) {
	exact := referenceNotional(in)
	tier := s.resolveTier(in.TrailingVolume)
	notionalMode, notionalRole := s.Rounding.notionalMode(in.Side)
	checks := []roundingCheck{
		{"notional_at_reference", q.NotionalAtReference, exact, notionalMode, notionalRole},
		{"spread.amount", q.Spread.Amount, exact.mulBPS(tier.SpreadBPS), s.Rounding.Spread, roleCharge},
		{lineKey(ComponentPercentageFee) + ".computed", q.Fees.Percentage.Computed,
			exact.mulBPS(tier.PercentageFeeBPS).mulBPS(money.OneHundredPercent - appliedDiscount(s, in)), s.Rounding.PercentageFee, roleCharge},
		{lineKey(ComponentVenuePassThrough) + ".computed", q.Fees.VenuePassThrough.Computed,
			exact.mulBPS(s.VenuePassThroughBPS), s.Rounding.VenuePassThrough, roleCharge},
		{lineKey(ComponentStrategyFee) + ".computed", q.Fees.Strategy.Computed,
			exact.mulBPS(appliedStrategyRate(s, in)), s.Rounding.StrategyFee, roleCharge},
		{"slippage_reserve.amount", q.SlippageReserve.Amount, exact.mulBPS(s.SlippageReserveBPS), s.Rounding.SlippageReserve, roleHold},
	}
	if s.MaxTotalFeeBPS > 0 {
		checks = append(checks, roundingCheck{"fees.bounds.max_bps_amount", q.Fees.Bounds.MaxBPSAmount, exact.mulBPS(s.MaxTotalFeeBPS), s.Rounding.FeeCap, roleCharge})
	} else if !q.Fees.Bounds.MaxBPSAmount.IsZero() {
		f.add("fees.bounds.max_bps_amount", "must be zero when the basis-point cap is disabled")
	}
	for _, c := range checks {
		want, err := c.exact.round(c.mode)
		if err != nil {
			f.add(c.key, "cannot be recomputed ("+string(errs.CodeOf(arithmeticError(err)))+")")
			continue
		}
		if !c.got.Equal(want) {
			f.addf(c.key, "is %s, but the exact amount rounded %s is %s", c.got, c.mode, want)
			continue
		}
		if !c.exact.withinOneUnit(c.got) {
			f.add(c.key, "is not within one base unit of the exact amount")
		}
		if !favoursPlatform(c.role, c.mode) {
			if c.role == roleCharge && c.exact.lessThan(c.got) {
				f.add(c.key, "charges more than the exact amount under a customer-favourable mode")
			}
			if c.role == roleCredit && c.exact.greaterThan(c.got) {
				f.add(c.key, "credits less than the exact amount under a customer-favourable mode")
			}
		}
	}
}

func verifyBounds(f fieldErrors, s Schedule, q Quote) {
	fees := q.Fees
	if s.MaxTotalFeeBPS > 0 && fees.Total.Cmp(fees.Bounds.MaxBPSAmount) > 0 {
		f.add("fees.total", "exceeds the basis-point cap")
	}
	if s.MaxTotalFee.IsPositive() && fees.Total.Cmp(s.MaxTotalFee) > 0 {
		f.add("fees.total", "exceeds the absolute cap")
	}
	preCap := fees.ComputedTotal.Max(s.MinFee)
	want := preCap
	if s.MaxTotalFeeBPS > 0 {
		want = want.Min(fees.Bounds.MaxBPSAmount)
	}
	if s.MaxTotalFee.IsPositive() {
		want = want.Min(s.MaxTotalFee)
	}
	if !fees.Total.Equal(want) {
		f.add("fees.total", "is not min(max(computed total, minimum fee), enabled caps)")
	}
	b := fees.Bounds
	if b.MinimumApplied != (want.Cmp(fees.ComputedTotal) > 0) {
		f.add("fees.bounds.minimum_applied", "does not say whether the minimum raised the fees")
	}
	if b.MaxBPSApplied != (s.MaxTotalFeeBPS > 0 && b.MaxBPSAmount.Cmp(preCap) < 0 && want.Equal(b.MaxBPSAmount)) {
		f.add("fees.bounds.max_bps_applied", "does not say whether the basis-point cap limited the fees")
	}
	if b.MaxAbsoluteApplied != (s.MaxTotalFee.IsPositive() && s.MaxTotalFee.Cmp(preCap) < 0 && want.Equal(s.MaxTotalFee)) {
		f.add("fees.bounds.max_absolute_applied", "does not say whether the absolute cap limited the fees")
	}
}

func verifyPrices(f fieldErrors, in QuoteInput, q Quote) {
	ref := in.ReferencePrice
	factor := money.OneHundredPercent + q.Spread.BPS
	if in.Side == SideSell {
		factor = money.OneHundredPercent - q.Spread.BPS
	}
	ep := q.ExecutionPrice
	switch {
	case ep.Validate() != nil:
		f.add("execution_price", "is not a valid price")
	case ep.QuoteAsset != ref.QuoteAsset || !ep.At.Equal(ref.At):
		f.add("execution_price", "must be quoted in the reference price's asset at its observation time")
	case ep.Scale < ref.Scale:
		f.add("execution_price", "must carry at least the reference price's scale")
	case !ep.Mantissa.Mul(pow10(int(ref.Scale) + bpsScale)).Equal(ref.Mantissa.Mul(money.QuantityFromInt64(int64(factor))).Mul(pow10(int(ep.Scale)))):
		f.add("execution_price", "is not the reference price moved by exactly the spread")
	}

	scale := effectivePriceScale(ref)
	gp := q.EffectivePrice
	want, err := effectivePriceRational(q.Total(), in, scale).round(effectivePriceRounding(in.Side))
	switch {
	case err != nil:
		f.add("effective_price", "cannot be recomputed")
	case gp.Validate() != nil:
		f.add("effective_price", "is not a valid price")
	case gp.QuoteAsset != ref.QuoteAsset || !gp.At.Equal(ref.At):
		f.add("effective_price", "must be quoted in the reference price's asset at its observation time")
	case gp.Scale > scale:
		f.add("effective_price", "carries more decimal places than the documented scale")
	case !gp.Mantissa.Mul(pow10(int(scale - gp.Scale))).Equal(want):
		f.add("effective_price", "is not the total divided by the quantity at the documented scale and rounding")
	}
}
