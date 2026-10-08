package fees

import (
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/money"
)

// BookingCategory names a destination a ledger posts one kind of collected
// amount to. Spread revenue, explicit fee revenue and amounts collected on
// behalf of a venue or the network are different things and are never
// booked to one account.
type BookingCategory string

// Booking categories, in the order RevenueSplit lists them.
const (
	// BookSpreadRevenue is the platform's revenue from the disclosed spread.
	BookSpreadRevenue BookingCategory = "SPREAD_REVENUE"
	// BookPlatformFeeRevenue is the platform's explicit fee revenue: the
	// fixed fee, the percentage fee and any minimum-fee top-up.
	BookPlatformFeeRevenue BookingCategory = "PLATFORM_FEE_REVENUE"
	// BookStrategyFeeRevenue is the explicit strategy fee, kept apart so
	// that it can be accounted for per strategy.
	BookStrategyFeeRevenue BookingCategory = "STRATEGY_FEE_REVENUE"
	// BookVenueFeePassThrough is collected to pay the venue. Not revenue.
	BookVenueFeePassThrough BookingCategory = "VENUE_FEE_PASS_THROUGH"
	// BookNetworkFeePassThrough is the network cost, in its own asset. Not
	// revenue.
	BookNetworkFeePassThrough BookingCategory = "NETWORK_FEE_PASS_THROUGH"
)

// RevenueLine is one amount to book. PlatformRevenue is true for the
// platform's own revenue and false for amounts it passes through.
// InQuoteTotal is true when the amount is part of the quote's TotalDebit or
// TotalCredit; a network cost in another asset is not, and is booked in that
// asset. Components lists the quote components the line is made of.
type RevenueLine struct {
	Category        BookingCategory
	Asset           assets.AssetID
	Amount          money.Quantity
	PlatformRevenue bool
	InQuoteTotal    bool
	Components      []Component
}

// RevenueSplit says how a quote's charges are booked. It always lists every
// category, in a fixed order, even when an amount is zero, so its shape never
// depends on the numbers.
//
// The quote-asset lines sum exactly to the quote's charges:
// QuoteAssetTotal() == TotalDebit − NotionalAtReference for a BUY and
// NotionalAtReference − TotalCredit for a SELL.
type RevenueSplit struct {
	ScheduleVersion string
	ScheduleHash    string
	Side            Side
	QuoteAsset      assets.AssetID
	Lines           []RevenueLine
}

// RevenueSplit returns the booking split of q.
func (q Quote) RevenueSplit() RevenueSplit {
	platform := q.Fees.Fixed.Charged.Add(q.Fees.Percentage.Charged).Add(q.Fees.MinimumTopUp)
	return RevenueSplit{
		ScheduleVersion: q.ScheduleVersion,
		ScheduleHash:    q.ScheduleHash,
		Side:            q.Input.Side,
		QuoteAsset:      q.QuoteAsset,
		Lines: []RevenueLine{
			{
				Category: BookSpreadRevenue, Asset: q.QuoteAsset, Amount: q.Spread.Amount,
				PlatformRevenue: true, InQuoteTotal: true, Components: []Component{ComponentSpread},
			},
			{
				Category: BookPlatformFeeRevenue, Asset: q.QuoteAsset, Amount: platform,
				PlatformRevenue: true, InQuoteTotal: true,
				Components: []Component{ComponentFixedFee, ComponentPercentageFee, ComponentMinimumFee},
			},
			{
				Category: BookStrategyFeeRevenue, Asset: q.QuoteAsset, Amount: q.Fees.Strategy.Charged,
				PlatformRevenue: true, InQuoteTotal: true, Components: []Component{ComponentStrategyFee},
			},
			{
				Category: BookVenueFeePassThrough, Asset: q.QuoteAsset, Amount: q.Fees.VenuePassThrough.Charged,
				PlatformRevenue: false, InQuoteTotal: true, Components: []Component{ComponentVenuePassThrough},
			},
			{
				Category: BookNetworkFeePassThrough, Asset: q.Network.Asset, Amount: q.Network.Amount,
				PlatformRevenue: false, InQuoteTotal: q.Network.InQuoteAsset, Components: []Component{ComponentNetworkFee},
			},
		},
	}
}

// Line returns the line of category c.
func (r RevenueSplit) Line(c BookingCategory) (RevenueLine, bool) {
	for _, l := range r.Lines {
		if l.Category == c {
			return l, true
		}
	}
	return RevenueLine{}, false
}

// QuoteAssetTotal sums the lines that are part of the quote's total.
func (r RevenueSplit) QuoteAssetTotal() money.Quantity {
	var sum money.Quantity
	for _, l := range r.Lines {
		if l.InQuoteTotal {
			sum = sum.Add(l.Amount)
		}
	}
	return sum
}

// PlatformRevenue sums the platform's own revenue (always in the quote
// asset): spread, explicit platform fees and the strategy fee.
func (r RevenueSplit) PlatformRevenue() money.Quantity {
	var sum money.Quantity
	for _, l := range r.Lines {
		if l.PlatformRevenue {
			sum = sum.Add(l.Amount)
		}
	}
	return sum
}
