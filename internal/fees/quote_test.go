package fees

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// TestQuote_WorkedExampleBuy prices 1.5 SOL at 150.123456 USDC under the
// worked schedule. Every number below was computed by hand:
//
//	notional  = 1.5 × 150.123456                  = 225.185184     (exact)
//	spread    = 225.185184 × 0.25%  = 0.56296296  → 0.562962       (down)
//	exec      = 150.123456 × 1.0025               = 150.49876464   (exact)
//	fixed     =                                     0.500000
//	platform  = 225.185184 × 0.10%  = 0.225185184 → 0.225185       (down)
//	venue     = 225.185184 × 0.05%  = 0.112592592 → 0.112592       (down)
//	reserve   = 225.185184 × 0.50%  = 1.12592592  → 1.125926       (up, held)
//	debit     = 225.185184 + 0.562962 + 0.837777  = 226.585923
//	all-in    = 226.585923 / 1.5                  = 151.057282
func TestQuote_WorkedExampleBuy(t *testing.T) {
	s := workedSchedule()
	quote := mustQuote(t, s, workedInput(SideBuy))

	hash, err := s.Hash()
	require.NoError(t, err)
	require.Equal(t, "fees-sched-v1", quote.ScheduleVersion)
	require.Equal(t, hash, quote.ScheduleHash)
	require.Equal(t, -1, quote.Tier.Index)

	require.Equal(t, "225185184", quote.NotionalAtReference.String())
	require.Equal(t, money.RoundDown, quote.NotionalRounding)
	require.Equal(t, SpreadCharge{BPS: 25, Amount: q("562962"), Rounding: money.RoundDown}, quote.Spread)
	require.Equal(t, "150.49876464 "+quoteAsset.String(), quote.ExecutionPrice.String())
	require.Equal(t, "pyth+spread@fees-sched-v1", quote.ExecutionPrice.Source)
	require.True(t, quote.ExecutionPrice.At.Equal(refTime))
	require.Equal(t, "225748146", quote.NotionalAtExecution.String())

	f := quote.Fees
	require.Equal(t, []string{"500000", "225185", "112592", "0"}, strs(f.Fixed.Charged, f.Percentage.Charged, f.VenuePassThrough.Charged, f.Strategy.Charged))
	require.Equal(t, money.RoundExact, f.Fixed.Rounding, "the fixed fee is never rounded")
	require.Equal(t, money.BPS(10), f.Percentage.RateBPS)
	require.Equal(t, money.BPS(0), f.Percentage.DiscountBPS)
	require.Equal(t, money.BPS(5), f.VenuePassThrough.RateBPS)
	require.Equal(t, money.BPS(0), f.Strategy.RateBPS, "not a strategy order")
	require.Equal(t, []string{"837777", "837777", "0"}, strs(f.ComputedTotal, f.Total, f.MinimumTopUp))
	require.Equal(t, NetworkCost{Asset: solAsset, Amount: q("5000"), InQuoteAsset: false}, quote.Network)
	require.Equal(t, ReserveHold{BPS: 50, Amount: q("1125926"), Rounding: money.RoundUp}, quote.SlippageReserve)

	require.Equal(t, "226585923", quote.TotalDebit.String())
	require.True(t, quote.TotalCredit.IsZero())
	require.Equal(t, "227711849", quote.WorstCaseTotal.String(), "debit plus the held reserve")
	require.Equal(t, "151.057282 "+quoteAsset.String(), quote.EffectivePrice.String())
	require.Equal(t, money.RoundUp, quote.EffectivePriceRounding)
	require.Equal(t, quote.TotalDebit, quote.Total())
}

// TestQuote_WorkedExampleSell is the same order sold:
//
//	exec   = 150.123456 × 0.9975                      = 149.74814736
//	credit = 225.185184 − 0.562962 − 0.837777         = 223.784445
//	worst  = 223.784445 − 1.125926                    = 222.658519
//	all-in = 223.784445 / 1.5                         = 149.189630
func TestQuote_WorkedExampleSell(t *testing.T) {
	quote := mustQuote(t, workedSchedule(), workedInput(SideSell))
	require.Equal(t, money.RoundUp, quote.NotionalRounding, "a SELL credits the notional, so it rounds up by default")
	require.Equal(t, "225185184", quote.NotionalAtReference.String())
	require.Equal(t, "149.74814736 "+quoteAsset.String(), quote.ExecutionPrice.String())
	require.Equal(t, "224622222", quote.NotionalAtExecution.String())
	require.Equal(t, "837777", quote.Fees.Total.String())
	require.Equal(t, "223784445", quote.TotalCredit.String())
	require.True(t, quote.TotalDebit.IsZero())
	require.Equal(t, "222658519", quote.WorstCaseTotal.String())
	require.Equal(t, "149.189630 "+quoteAsset.String(), quote.EffectivePrice.String())
	require.Equal(t, money.RoundDown, quote.EffectivePriceRounding)
}

// TestQuote_ZeroScheduleIsAPassThrough: with no spread and no fees the
// execution price IS the reference price and the total IS the notional.
func TestQuote_ZeroScheduleIsAPassThrough(t *testing.T) {
	s := ZeroSchedule(quoteAsset, 6)
	for _, side := range []Side{SideBuy, SideSell} {
		for _, in := range []QuoteInput{
			workedInput(side),
			{Side: side, BaseQty: q("1"), BaseDecimals: 0, QuoteDecimals: 6, ReferencePrice: refPrice("1", 0)},
			{Side: side, BaseQty: q("123456789"), BaseDecimals: 9, QuoteDecimals: 6, ReferencePrice: refPrice("15010", 2)},
		} {
			quote := mustQuote(t, s, in)
			require.True(t, quote.ExecutionPrice.Mantissa.Equal(in.ReferencePrice.Mantissa), "%s: execution mantissa", side)
			require.Equal(t, in.ReferencePrice.Scale, quote.ExecutionPrice.Scale, "%s: execution scale", side)
			require.True(t, quote.Spread.Amount.IsZero())
			require.True(t, quote.Fees.Total.IsZero())
			require.True(t, quote.NotionalAtExecution.Equal(quote.NotionalAtReference))
			require.True(t, quote.Total().Equal(quote.NotionalAtReference), "%s: total == notional", side)
			require.True(t, quote.WorstCaseTotal.Equal(quote.Total()))
		}
	}
	exact := mustQuote(t, s, workedInput(SideBuy))
	require.True(t, exact.EffectivePrice.Mantissa.Equal(exact.ReferencePrice.Mantissa))
	require.Equal(t, exact.ReferencePrice.Scale, exact.EffectivePrice.Scale, "an exact notional with no charges has the reference price as its all-in price")
}

// TestQuote_SubscriptionAndStrategy: the discount reduces the percentage fee
// only; the strategy fee applies only to strategy-originated orders.
//
//	platform = 225.185184 × 0.10% × 75% = 0.168888888 → 0.168888
//	strategy = 225.185184 × 0.20%       = 0.450370368 → 0.450370
func TestQuote_SubscriptionAndStrategy(t *testing.T) {
	s := workedSchedule()
	plain := mustQuote(t, s, workedInput(SideBuy))
	in := workedInput(SideBuy)
	in.HasSubscription, in.StrategyOriginated = true, true
	quote := mustQuote(t, s, in)

	require.Equal(t, money.BPS(10), quote.Fees.Percentage.RateBPS)
	require.Equal(t, money.BPS(2_500), quote.Fees.Percentage.DiscountBPS)
	require.Equal(t, "168888", quote.Fees.Percentage.Charged.String())
	require.Equal(t, money.BPS(20), quote.Fees.Strategy.RateBPS)
	require.Equal(t, "450370", quote.Fees.Strategy.Charged.String())
	require.Equal(t, "1231850", quote.Fees.Total.String())
	require.Equal(t, "226979996", quote.TotalDebit.String())
	require.Equal(t, plain.Spread, quote.Spread, "the discount never touches the spread")
	require.Equal(t, plain.Fees.VenuePassThrough, quote.Fees.VenuePassThrough, "nor the pass-through")
	require.Equal(t, plain.Fees.Fixed, quote.Fees.Fixed, "nor the fixed fee")

	waived := workedSchedule()
	waived.SubscriptionDiscountBPS = money.OneHundredPercent
	free := mustQuote(t, waived, in)
	require.True(t, free.Fees.Percentage.Charged.IsZero(), "a 100% discount waives the fee and never makes it negative")
	require.False(t, free.Fees.Percentage.Charged.IsNegative())
}

// leanSchedule charges only a 10 bps percentage fee (0.225185 on the worked
// order) so that bounds are easy to read.
func leanSchedule() Schedule {
	s := ZeroSchedule(quoteAsset, 6)
	s.Version = "lean-v1"
	s.PercentageFeeBPS = 10
	return s
}

func TestQuote_MinimumFee(t *testing.T) {
	s := leanSchedule()
	s.MinFee = q("1000000")
	quote := mustQuote(t, s, workedInput(SideBuy))
	require.Equal(t, "225185", quote.Fees.Percentage.Charged.String())
	require.Equal(t, "774815", quote.Fees.MinimumTopUp.String(), "the minimum is a separate, disclosed top-up")
	require.Equal(t, "1000000", quote.Fees.Total.String())
	require.True(t, quote.Fees.Bounds.MinimumApplied)
	require.False(t, quote.Fees.Bounds.MaxBPSApplied || quote.Fees.Bounds.MaxAbsoluteApplied)

	// A cap always wins over the minimum: 30 bps caps the fee at 0.675555.
	s.MaxTotalFeeBPS = 30
	capped := mustQuote(t, s, workedInput(SideBuy))
	require.Equal(t, "675555", capped.Fees.Bounds.MaxBPSAmount.String())
	require.Equal(t, "675555", capped.Fees.Total.String())
	require.Equal(t, "450370", capped.Fees.MinimumTopUp.String())
	require.True(t, capped.Fees.Bounds.MinimumApplied, "the minimum raised the fee")
	require.True(t, capped.Fees.Bounds.MaxBPSApplied, "and the cap limited the raise")

	// A cap below the computed fee cuts the fee itself and the minimum
	// plays no part: 1 bps caps it at 0.022518.
	s.MaxTotalFeeBPS = 1
	below := mustQuote(t, s, workedInput(SideBuy))
	require.Equal(t, "22518", below.Fees.Total.String())
	require.Equal(t, "202667", below.Fees.Percentage.CapReduction.String())
	require.True(t, below.Fees.MinimumTopUp.IsZero())
	require.False(t, below.Fees.Bounds.MinimumApplied)
	require.True(t, below.Fees.Bounds.MaxBPSApplied)
}

// TestQuote_CapsReduceInTheDocumentedOrder: a cap takes the percentage fee,
// then the strategy fee, then the fixed fee, and only then the venue
// pass-through. The strategy order computes 0.500000 fixed + 0.225185
// platform + 0.112592 venue + 0.450370 strategy = 1.288147.
func TestQuote_CapsReduceInTheDocumentedOrder(t *testing.T) {
	in := workedInput(SideBuy)
	in.StrategyOriginated = true
	cases := []struct {
		name                                string
		capBPS                              money.BPS
		capAbs                              string
		fixed, pct, venue, strategy, total  string
		bpsApplied, absApplied, minApplied  bool
		reductionFixed, reductionVenue      string
		reductionPct, reductionStrategyWant string
	}{
		{"bps cap cuts into the fixed fee", 20, "0", "337778", "0", "112592", "0", "450370", true, false, false, "162222", "0", "225185", "450370"},
		{"absolute cap reaches the pass-through last", 0, "100000", "0", "0", "100000", "0", "100000", false, true, false, "500000", "12592", "225185", "450370"},
		{"the tighter cap binds", 20, "300000", "187408", "0", "112592", "0", "300000", false, true, false, "312592", "0", "225185", "450370"},
		{"equal caps both bind", 20, "450370", "337778", "0", "112592", "0", "450370", true, true, false, "162222", "0", "225185", "450370"},
		{"a loose cap changes nothing", 100, "5000000", "500000", "225185", "112592", "450370", "1288147", false, false, false, "0", "0", "0", "0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := workedSchedule()
			s.MaxTotalFeeBPS, s.MaxTotalFee = c.capBPS, q(c.capAbs)
			f := mustQuote(t, s, in).Fees
			require.Equal(t, "1288147", f.ComputedTotal.String())
			require.Equal(t, []string{c.fixed, c.pct, c.venue, c.strategy, c.total},
				strs(f.Fixed.Charged, f.Percentage.Charged, f.VenuePassThrough.Charged, f.Strategy.Charged, f.Total))
			require.Equal(t, []string{c.reductionFixed, c.reductionVenue, c.reductionPct, c.reductionStrategyWant},
				strs(f.Fixed.CapReduction, f.VenuePassThrough.CapReduction, f.Percentage.CapReduction, f.Strategy.CapReduction))
			require.Equal(t, c.bpsApplied, f.Bounds.MaxBPSApplied)
			require.Equal(t, c.absApplied, f.Bounds.MaxAbsoluteApplied)
			require.Equal(t, c.minApplied, f.Bounds.MinimumApplied)
			require.True(t, f.MinimumTopUp.IsZero())
			if c.capBPS > 0 {
				require.LessOrEqual(t, f.Total.Cmp(f.Bounds.MaxBPSAmount), 0, "never above the bps cap")
			}
			if cap := q(c.capAbs); cap.IsPositive() {
				require.LessOrEqual(t, f.Total.Cmp(cap), 0, "never above the absolute cap")
			}
		})
	}
}

// TestQuote_NetworkCost: a network cost in the quote asset is part of the
// total on both sides; in another asset it is disclosed and never converted.
func TestQuote_NetworkCost(t *testing.T) {
	other := mustQuote(t, workedSchedule(), workedInput(SideBuy))
	require.False(t, other.Network.InQuoteAsset)
	require.Equal(t, "226585923", other.TotalDebit.String(), "a SOL estimate is not added to a USDC total")

	s := workedSchedule()
	s.NetworkFee = NetworkFeeEstimate{Asset: quoteAsset, Amount: q("1500")}
	buy := mustQuote(t, s, workedInput(SideBuy))
	require.True(t, buy.Network.InQuoteAsset)
	require.Equal(t, "226587423", buy.TotalDebit.String())
	sell := mustQuote(t, s, workedInput(SideSell))
	require.Equal(t, "223782945", sell.TotalCredit.String())
}

// TestQuote_SlippageReserveIsHeldNotCharged: the reserve never changes what
// is charged; it only widens the worst case.
func TestQuote_SlippageReserveIsHeldNotCharged(t *testing.T) {
	without := workedSchedule()
	without.SlippageReserveBPS = 0
	for _, side := range []Side{SideBuy, SideSell} {
		held := mustQuote(t, workedSchedule(), workedInput(side))
		none := mustQuote(t, without, workedInput(side))
		require.True(t, held.Total().Equal(none.Total()), "%s: the reserve is not charged", side)
		require.True(t, held.SlippageReserve.Amount.IsPositive())
		require.True(t, none.WorstCaseTotal.Equal(none.Total()))
	}

	// A sell whose reserve exceeds its proceeds has a worst case of zero,
	// never a negative credit.
	s := ZeroSchedule(quoteAsset, 6)
	s.SlippageReserveBPS = money.OneHundredPercent
	tiny := mustQuote(t, s, QuoteInput{Side: SideSell, BaseQty: q("1000"), BaseDecimals: 9, QuoteDecimals: 6, ReferencePrice: refPrice("150123456", 6)})
	require.Equal(t, "151", tiny.TotalCredit.String())
	require.Equal(t, "151", tiny.SlippageReserve.Amount.String())
	require.True(t, tiny.WorstCaseTotal.IsZero())
}

// TestQuote_RoundingTable prices 1.500000001 SOL, whose every component is
// fractional:
//
//	notional  225.185184150123456   spread   0.56296296037530864
//	platform    0.225185184150123   venue    0.112592592075061728
//	strategy    0.450370368300247   reserve  1.12592592075061728
//
// Under the default policy every charge rounds down and a SELL's notional
// rounds up; under an explicitly adopted platform-favourable policy every
// charge rounds up and a SELL's notional down, and the disclosure says so.
func TestQuote_RoundingTable(t *testing.T) {
	in := func(side Side) QuoteInput {
		x := workedInput(side)
		x.BaseQty = q("1500000001")
		x.StrategyOriginated = true
		return x
	}
	favourable := workedSchedule()
	favourable.Rounding = RoundingPolicy{
		BuyNotional: money.RoundUp, SellNotional: money.RoundDown, Spread: money.RoundUp,
		PercentageFee: money.RoundUp, VenuePassThrough: money.RoundUp, StrategyFee: money.RoundUp,
		FeeCap: money.RoundUp, SlippageReserve: money.RoundDown, AllowPlatformFavourable: true,
	}
	cases := []struct {
		name                                            string
		s                                               Schedule
		side                                            Side
		notional, spread, pct, venue, strategy, reserve string
		total                                           string
		anyFavoursPlatform                              bool
	}{
		{"default buy", workedSchedule(), SideBuy, "225185184", "562962", "225185", "112592", "450370", "1125926", "227036293", false},
		{"default sell", workedSchedule(), SideSell, "225185185", "562962", "225185", "112592", "450370", "1125926", "223334076", false},
		{"platform-favourable buy", favourable, SideBuy, "225185185", "562963", "225186", "112593", "450371", "1125925", "227036298", true},
		{"platform-favourable sell", favourable, SideSell, "225185184", "562963", "225186", "112593", "450371", "1125925", "223334071", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			quote := mustQuote(t, c.s, in(c.side))
			require.Equal(t,
				[]string{c.notional, c.spread, c.pct, c.venue, c.strategy, c.reserve, c.total},
				strs(quote.NotionalAtReference, quote.Spread.Amount, quote.Fees.Percentage.Charged,
					quote.Fees.VenuePassThrough.Charged, quote.Fees.Strategy.Charged, quote.SlippageReserve.Amount, quote.Total()))
			d := quote.Disclosure().Rounding
			require.Equal(t, c.anyFavoursPlatform, d.AnyFavoursPlatform)
			require.Equal(t, c.s.Rounding.AllowPlatformFavourable, d.PlatformFavourableAllowed)
			for _, m := range d.Components {
				if m.FavoursPlatform {
					require.True(t, d.PlatformFavourableAllowed, "%s favours the platform without the schedule's explicit opt-in", m.Component)
				}
			}
		})
	}
}

func TestQuote_InputRejections(t *testing.T) {
	s := workedSchedule()
	cases := []struct {
		name  string
		mut   func(*QuoteInput)
		field string
	}{
		{"unknown side", func(in *QuoteInput) { in.Side = "HOLD" }, "side"},
		{"zero quantity", func(in *QuoteInput) { in.BaseQty = q("0") }, "base_qty"},
		{"negative quantity", func(in *QuoteInput) { in.BaseQty = q("-1") }, "base_qty"},
		{"unsourced price", func(in *QuoteInput) { in.ReferencePrice.Source = "" }, "reference_price"},
		{"zero price", func(in *QuoteInput) { in.ReferencePrice.Mantissa = q("0") }, "reference_price"},
		{"price in another asset", func(in *QuoteInput) { in.ReferencePrice.QuoteAsset = solAsset.String() }, "reference_price.quote_asset"},
		{"wrong quote decimals", func(in *QuoteInput) { in.QuoteDecimals = 9 }, "quote_decimals"},
		{"negative volume", func(in *QuoteInput) { in.TrailingVolume = money.USDFromMinor(-1) }, "trailing_volume"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := workedInput(SideBuy)
			c.mut(&in)
			_, err := s.Quote(in)
			requireFields(t, err, errs.CodeValidationFailed, c.field)
		})
	}

	bad := workedSchedule()
	bad.Version = ""
	_, err := bad.Quote(workedInput(SideBuy))
	requireFields(t, err, errs.CodeValidationFailed, "version")
}

// TestQuote_SellTooSmallIsRefused: a sell whose proceeds do not cover its
// charges is refused rather than producing a negative credit.
func TestQuote_SellTooSmallIsRefused(t *testing.T) {
	in := workedInput(SideSell)
	in.BaseQty = q("1000") // 0.000001 SOL ≈ 0.000151 USDC, far below the 0.50 fixed fee
	_, err := workedSchedule().Quote(in)
	requireFields(t, err, errs.CodeValidationFailed, "base_qty")

	in.Side = SideBuy
	buy := mustQuote(t, workedSchedule(), in)
	require.Equal(t, "500150", buy.TotalDebit.String(), "a small buy pays its fixed fee on top")
}

// TestQuote_ExecutionPriceMustBeExact: a spread on a price already at the
// maximum scale cannot be represented exactly and is refused, never rounded.
func TestQuote_ExecutionPriceMustBeExact(t *testing.T) {
	s := ZeroSchedule(quoteAsset, 6)
	s.SpreadBPS = 25
	in := QuoteInput{Side: SideBuy, BaseQty: q("1"), BaseDecimals: 0, QuoteDecimals: 6, ReferencePrice: refPrice("1", 36)}
	_, err := s.Quote(in)
	require.Equal(t, errs.CodePrecisionLoss, errs.CodeOf(err), "error: %v", err)

	// With trailing zeros to spare, the same spread is exact: 100e-36 ×
	// 1.0025 = 10025e-38.
	in.ReferencePrice = refPrice("100", 36)
	quote := mustQuote(t, s, in)
	require.Equal(t, int32(38), quote.ExecutionPrice.Scale)
	require.Equal(t, "10025", quote.ExecutionPrice.Mantissa.String())

	// A zero spread never needs extra scale.
	s.SpreadBPS = 0
	in.ReferencePrice = refPrice("1", 38)
	zero := mustQuote(t, s, in)
	require.Equal(t, int32(38), zero.ExecutionPrice.Scale)
	require.Equal(t, "1", zero.ExecutionPrice.Mantissa.String())
}

// TestQuote_IsDeterministic: the same schedule and input always produce the
// same record, whatever time zone the reference price's timestamp carries.
func TestQuote_IsDeterministic(t *testing.T) {
	s := ladder(t)
	in := workedInput(SideSell)
	in.TrailingVolume = usd(t, "250000.00")
	in.HasSubscription, in.StrategyOriginated = true, true
	a, b := mustQuote(t, s, in), mustQuote(t, s, in)
	ha, err := a.Disclosure().Hash()
	require.NoError(t, err)
	hb, err := b.Disclosure().Hash()
	require.NoError(t, err)
	require.Equal(t, ha, hb)

	elsewhere := in
	elsewhere.ReferencePrice.At = refTime.In(time.FixedZone("UTC+5", 5*3600))
	hc, err := mustQuote(t, s, elsewhere).Disclosure().Hash()
	require.NoError(t, err)
	require.Equal(t, ha, hc, "the record is in UTC; the zone of the input does not change it")
}
