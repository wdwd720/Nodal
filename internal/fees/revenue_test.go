package fees

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
)

// TestRevenueSplit_BooksEachKindSeparately uses the subscribed strategy
// order of TestQuote_SubscriptionAndStrategy: spread 0.562962, fixed
// 0.500000 + platform 0.168888, strategy 0.450370, venue 0.112592, and a
// SOL network estimate that is booked in SOL and is not part of the USDC
// total.
func TestRevenueSplit_BooksEachKindSeparately(t *testing.T) {
	in := workedInput(SideBuy)
	in.HasSubscription, in.StrategyOriginated = true, true
	quote := mustQuote(t, workedSchedule(), in)
	split := quote.RevenueSplit()

	type row struct {
		category BookingCategory
		asset    assets.AssetID
		amount   string
		revenue  bool
		inTotal  bool
	}
	got := make([]row, 0, len(split.Lines))
	for _, l := range split.Lines {
		got = append(got, row{l.Category, l.Asset, l.Amount.String(), l.PlatformRevenue, l.InQuoteTotal})
	}
	require.Equal(t, []row{
		{BookSpreadRevenue, quoteAsset, "562962", true, true},
		{BookPlatformFeeRevenue, quoteAsset, "668888", true, true},
		{BookStrategyFeeRevenue, quoteAsset, "450370", true, true},
		{BookVenueFeePassThrough, quoteAsset, "112592", false, true},
		{BookNetworkFeePassThrough, solAsset, "5000", false, false},
	}, got)

	require.Equal(t, "1794812", split.QuoteAssetTotal().String())
	require.True(t, split.QuoteAssetTotal().Equal(quote.TotalDebit.Sub(quote.NotionalAtReference)),
		"the booked quote-asset lines are exactly what the customer paid above the notional")
	require.Equal(t, "1682220", split.PlatformRevenue().String())
	require.Equal(t, quote.ScheduleHash, split.ScheduleHash)
	require.Equal(t, SideBuy, split.Side)

	venue, ok := split.Line(BookVenueFeePassThrough)
	require.True(t, ok)
	require.Equal(t, []Component{ComponentVenuePassThrough}, venue.Components)
	_, ok = split.Line("UNKNOWN")
	require.False(t, ok)
}

// TestRevenueSplit_MinimumTopUpAndQuoteAssetNetwork: the minimum top-up is
// platform fee revenue; a network cost in the quote asset is part of the
// total and is still booked as a pass-through, never as revenue.
func TestRevenueSplit_MinimumTopUpAndQuoteAssetNetwork(t *testing.T) {
	s := leanSchedule()
	s.MinFee = q("1000000")
	s.NetworkFee = NetworkFeeEstimate{Asset: quoteAsset, Amount: q("1500")}
	for _, side := range []Side{SideBuy, SideSell} {
		quote := mustQuote(t, s, workedInput(side))
		split := quote.RevenueSplit()
		platform, _ := split.Line(BookPlatformFeeRevenue)
		require.Equal(t, "1000000", platform.Amount.String(), "0.225185 platform fee + 0.774815 minimum top-up")
		network, _ := split.Line(BookNetworkFeePassThrough)
		require.True(t, network.InQuoteTotal)
		require.False(t, network.PlatformRevenue)
		charged := quote.TotalDebit.Sub(quote.NotionalAtReference)
		if side == SideSell {
			charged = quote.NotionalAtReference.Sub(quote.TotalCredit)
		}
		require.Equal(t, "1001500", charged.String())
		require.True(t, split.QuoteAssetTotal().Equal(charged), "%s", side)
	}
}

// TestRevenueSplit_ShapeNeverDependsOnTheNumbers: every category is always
// listed, in order, even when nothing is charged.
func TestRevenueSplit_ShapeNeverDependsOnTheNumbers(t *testing.T) {
	split := mustQuote(t, ZeroSchedule(quoteAsset, 6), workedInput(SideBuy)).RevenueSplit()
	categories := make([]BookingCategory, 0, len(split.Lines))
	for _, l := range split.Lines {
		categories = append(categories, l.Category)
		require.True(t, l.Amount.IsZero())
	}
	require.Equal(t, []BookingCategory{
		BookSpreadRevenue, BookPlatformFeeRevenue, BookStrategyFeeRevenue, BookVenueFeePassThrough, BookNetworkFeePassThrough,
	}, categories)
	require.True(t, split.QuoteAssetTotal().IsZero())
}
