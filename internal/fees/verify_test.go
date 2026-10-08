package fees

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

func one() money.Quantity { return money.QuantityFromInt64(1) }

// TestVerifyQuote_AcceptsEveryEngineQuote runs the auditor over a matrix of
// schedules, sides and order flags.
func TestVerifyQuote_AcceptsEveryEngineQuote(t *testing.T) {
	capped := workedSchedule()
	capped.MaxTotalFeeBPS, capped.MinFee = 20, q("100000")
	sameAssetNetwork := workedSchedule()
	sameAssetNetwork.NetworkFee = NetworkFeeEstimate{Asset: quoteAsset, Amount: q("1500")}
	for _, s := range []Schedule{ZeroSchedule(quoteAsset, 6), workedSchedule(), ladder(t), leanSchedule(), capped, sameAssetNetwork} {
		for _, side := range []Side{SideBuy, SideSell} {
			for _, flags := range [][2]bool{{false, false}, {true, false}, {false, true}, {true, true}} {
				in := workedInput(side)
				in.HasSubscription, in.StrategyOriginated = flags[0], flags[1]
				in.TrailingVolume = usd(t, "123456.78")
				quote, err := s.Quote(in)
				require.NoError(t, err)
				require.NoError(t, VerifyQuote(s, in, quote), "%s %s %v", s.Version, side, flags)
			}
		}
	}
}

// TestVerifyQuote_DetectsTampering: every way of charging something the
// schedule does not account for -- including tampering that keeps the
// totals arithmetically consistent -- is caught and named.
func TestVerifyQuote_DetectsTampering(t *testing.T) {
	strategyIn := workedInput(SideBuy)
	strategyIn.StrategyOriginated = true
	capped := workedSchedule()
	capped.MaxTotalFeeBPS = 20 // cuts the fixed fee by 0.162222 (see TestQuote_CapsReduceInTheDocumentedOrder)

	cases := []struct {
		name   string
		s      Schedule
		in     QuoteInput
		tamper func(*Quote)
		field  string
	}{
		{"a hidden unit in the total", workedSchedule(), workedInput(SideBuy), func(x *Quote) {
			x.TotalDebit = x.TotalDebit.Add(one())
		}, "total_debit"},
		{"a wider spread with the total kept consistent", workedSchedule(), workedInput(SideBuy), func(x *Quote) {
			x.Spread.Amount = x.Spread.Amount.Add(one())
			x.NotionalAtExecution = x.NotionalAtExecution.Add(one())
			x.TotalDebit = x.TotalDebit.Add(one())
			x.WorstCaseTotal = x.WorstCaseTotal.Add(one())
		}, "spread.amount"},
		{"a fee rounded up with every sum kept consistent", workedSchedule(), workedInput(SideBuy), func(x *Quote) {
			x.Fees.Percentage.Computed = x.Fees.Percentage.Computed.Add(one())
			x.Fees.Percentage.Charged = x.Fees.Percentage.Charged.Add(one())
			x.Fees.ComputedTotal = x.Fees.ComputedTotal.Add(one())
			x.Fees.Total = x.Fees.Total.Add(one())
			x.TotalDebit = x.TotalDebit.Add(one())
			x.WorstCaseTotal = x.WorstCaseTotal.Add(one())
		}, "fees.percentage_fee.computed"},
		{"a buy notional rounded against the customer", workedSchedule(), func() QuoteInput {
			in := workedInput(SideBuy)
			in.BaseQty = q("1500000001")
			return in
		}(), func(x *Quote) {
			x.NotionalAtReference = x.NotionalAtReference.Add(one())
			x.NotionalAtExecution = x.NotionalAtExecution.Add(one())
			x.TotalDebit = x.TotalDebit.Add(one())
			x.WorstCaseTotal = x.WorstCaseTotal.Add(one())
		}, "notional_at_reference"},
		{"a sell credit short by one unit", workedSchedule(), workedInput(SideSell), func(x *Quote) {
			x.TotalCredit = x.TotalCredit.Sub(one())
		}, "total_credit"},
		{"a cap taken from the pass-through first", capped, strategyIn, func(x *Quote) {
			moved := money.QuantityFromInt64(1000)
			x.Fees.Fixed.CapReduction = x.Fees.Fixed.CapReduction.Sub(moved)
			x.Fees.Fixed.Charged = x.Fees.Fixed.Charged.Add(moved)
			x.Fees.VenuePassThrough.CapReduction = moved
			x.Fees.VenuePassThrough.Charged = x.Fees.VenuePassThrough.Charged.Sub(moved)
		}, "fees.venue_pass_through.cap_reduction"},
		{"an invented minimum top-up", workedSchedule(), workedInput(SideBuy), func(x *Quote) {
			x.Fees.MinimumTopUp = one()
			x.Fees.Total = x.Fees.Total.Add(one())
			x.TotalDebit = x.TotalDebit.Add(one())
			x.WorstCaseTotal = x.WorstCaseTotal.Add(one())
		}, "fees.total"},
		{"the reserve folded into the debit", workedSchedule(), workedInput(SideBuy), func(x *Quote) {
			x.TotalDebit = x.TotalDebit.Add(x.SlippageReserve.Amount)
			x.WorstCaseTotal = x.WorstCaseTotal.Add(x.SlippageReserve.Amount)
		}, "total_debit"},
		{"a misstated rate", workedSchedule(), workedInput(SideBuy), func(x *Quote) {
			x.Fees.Percentage.RateBPS = 9
		}, "fees.percentage_fee.rate_bps"},
		{"a discount the customer did not get", workedSchedule(), workedInput(SideBuy), func(x *Quote) {
			x.Fees.Percentage.DiscountBPS = 2_500
		}, "fees.percentage_fee.discount_bps"},
		{"a different schedule", workedSchedule(), workedInput(SideBuy), func(x *Quote) {
			x.ScheduleHash = "00" + x.ScheduleHash[2:]
		}, "schedule_hash"},
		{"a different input", workedSchedule(), workedInput(SideBuy), func(x *Quote) {
			x.Input.HasSubscription = true
		}, "input"},
		{"a foreign network cost added to the total", workedSchedule(), workedInput(SideBuy), func(x *Quote) {
			x.Network.InQuoteAsset = true
		}, "network.in_quote_asset"},
		{"an inflated execution price", workedSchedule(), workedInput(SideBuy), func(x *Quote) {
			x.ExecutionPrice.Mantissa = x.ExecutionPrice.Mantissa.Add(one())
		}, "execution_price"},
		{"a flattering all-in price", workedSchedule(), workedInput(SideBuy), func(x *Quote) {
			x.EffectivePrice.Mantissa = x.EffectivePrice.Mantissa.Sub(one())
		}, "effective_price"},
		{"a misreported tier", ladder(t), workedInput(SideBuy), func(x *Quote) {
			x.Tier.Index = 0
		}, "tier"},
		{"a misreported rounding mode", workedSchedule(), workedInput(SideBuy), func(x *Quote) {
			x.Spread.Rounding = money.RoundUp
		}, "spread.rounding"},
		{"a negative fee line", workedSchedule(), workedInput(SideBuy), func(x *Quote) {
			x.Fees.Strategy.Computed = q("-1")
			x.Fees.Strategy.Charged = q("-1")
		}, "fees.strategy_fee.computed"},
		{"a misstated worst case", workedSchedule(), workedInput(SideBuy), func(x *Quote) {
			x.WorstCaseTotal = x.TotalDebit
		}, "worst_case_total"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			quote := mustQuote(t, c.s, c.in)
			c.tamper(&quote)
			requireFields(t, VerifyQuote(c.s, c.in, quote), errs.CodeValidationFailed, c.field)
		})
	}
}

// TestVerifyQuote_BindsTheQuoteToItsScheduleAndInput.
func TestVerifyQuote_BindsTheQuoteToItsScheduleAndInput(t *testing.T) {
	quote := mustQuote(t, workedSchedule(), workedInput(SideBuy))

	other := workedSchedule()
	other.SpreadBPS = 30
	requireFields(t, VerifyQuote(other, workedInput(SideBuy), quote), errs.CodeValidationFailed, "schedule_hash", "spread.amount")

	bigger := workedInput(SideBuy)
	bigger.BaseQty = q("3000000000")
	requireFields(t, VerifyQuote(workedSchedule(), bigger, quote), errs.CodeValidationFailed, "input", "notional_at_reference")

	refused := workedInput(SideBuy)
	refused.QuoteDecimals = 9
	requireFields(t, VerifyQuote(workedSchedule(), refused, quote), errs.CodeValidationFailed, "quote_decimals")

	invalid := workedSchedule()
	invalid.Rounding.Spread = money.RoundUp
	requireFields(t, VerifyQuote(invalid, workedInput(SideBuy), quote), errs.CodeValidationFailed, "rounding.spread")
}
