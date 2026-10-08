package feeledger

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

func q(n int64) money.Quantity { return money.QuantityFromInt64(n) }

var quoteAsset = assets.NewAssetID()

// fullSchedule charges every component a schedule can carry.
func fullSchedule() fees.Schedule {
	s := fees.ZeroSchedule(quoteAsset, 6)
	s.Version = "full-v1"
	s.SpreadBPS = 20
	s.FixedFee = q(500_000)
	s.PercentageFeeBPS = 10
	s.VenuePassThroughBPS = 5
	s.NetworkFee = fees.NetworkFeeEstimate{Asset: quoteAsset, Amount: q(100_000)}
	s.SlippageReserveBPS = 30
	s.SubscriptionDiscountBPS = 2_500
	s.StrategyFeeBPS = 15
	s.Tiers = []fees.VolumeTier{{MinTrailingVolume: money.USDFromMinor(1_000_000_00), OverridesPercentageFee: true, PercentageFeeBPS: 5}}
	return s
}

func price(t require.TestingT, s string) money.Price {
	p, err := money.PriceFromDecimalString(s, quoteAsset.String(), "unit", time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	return p
}

// TestQuoteMath_EveryComponentIsItsOwnLine: 2 units at 100.00 under the
// full schedule, strategy-originated, for a subscriber: every component is
// computed on the reference notional and nothing on another charge.
func TestQuoteMath_EveryComponentIsItsOwnLine(t *testing.T) {
	s := fullSchedule()
	in := fees.QuoteInput{Side: fees.SideBuy, BaseQty: q(2_000_000), BaseDecimals: 6, QuoteDecimals: 6, ReferencePrice: price(t, "100.00"),
		HasSubscription: true, StrategyOriginated: true}
	quote, err := s.Quote(in)
	require.NoError(t, err)
	require.NoError(t, fees.VerifyQuote(s, in, quote))
	c := ChargesOf(quote.Disclosure())
	assert.Equal(t, "200000000", quote.NotionalAtReference.String(), "2 x 100.00")
	assert.Equal(t, "400000", c.Spread.String(), "20 bps of 200")
	// fixed 0.50 + 10 bps of 200 with a quarter off (0.15)
	assert.Equal(t, "650000", c.Platform.String())
	assert.Equal(t, "300000", c.Strategy.String(), "15 bps of 200")
	assert.Equal(t, "100000", c.Venue.String(), "5 bps of 200")
	assert.Equal(t, "100000", c.Network.String())
	assert.True(t, c.NetworkInQuote)
	assert.Equal(t, "201550000", quote.TotalDebit.String(), "200 + 0.40 + 0.65 + 0.30 + 0.10 + 0.10")
	assert.Equal(t, quote.TotalDebit.Sub(quote.NotionalAtReference).String(), c.InQuoteAsset().String())
	assert.Equal(t, "10020", quote.ExecutionPrice.Mantissa.String(), "100.20: the reference moved by exactly the spread")
	assert.Equal(t, int32(2), quote.ExecutionPrice.Scale)
	assert.Equal(t, "600000", quote.SlippageReserve.Amount.String(), "held, never charged")
}

// TestRounding_IsInTheCustomersFavourUnlessAllowedAndDisclosed: a notional
// whose basis-point fees are fractional rounds every charge down; a schedule
// that rounds against the customer is refused unless it says so, and then
// every disclosure flags it.
func TestRounding_IsInTheCustomersFavourUnlessAllowedAndDisclosed(t *testing.T) {
	s := fees.ZeroSchedule(quoteAsset, 6)
	s.Version, s.SpreadBPS, s.PercentageFeeBPS = "odd-v1", 13, 7
	in := fees.QuoteInput{Side: fees.SideBuy, BaseQty: q(3_000_000), BaseDecimals: 6, QuoteDecimals: 6, ReferencePrice: price(t, "33.333333")}
	quote, err := s.Quote(in)
	require.NoError(t, err)
	assert.Equal(t, "99999999", quote.NotionalAtReference.String())
	assert.Equal(t, "129999", quote.Spread.Amount.String(), "129999.9987 rounded down")
	assert.Equal(t, "69999", quote.Fees.Total.String(), "69999.9993 rounded down")
	assert.False(t, quote.Disclosure().Rounding.AnyFavoursPlatform)

	up := s
	up.Version = "odd-up-v1"
	up.Rounding.Spread = money.RoundUp
	assert.Error(t, up.Validate(), "rounding against the customer must be explicit")
	up.Rounding.AllowPlatformFavourable = true
	require.NoError(t, up.Validate())
	q2, err := up.Quote(in)
	require.NoError(t, err)
	assert.Equal(t, "130000", q2.Spread.Amount.String())
	assert.True(t, q2.Disclosure().Rounding.AnyFavoursPlatform, "and the disclosure says so")
}

// TestProp_FeeConservation: for any schedule and order, the booking entries
// balance per party and per asset, the payer pays exactly the itemized
// charges (the quote's total less its reference notional, plus a network cost
// in another asset), and the platform's revenue is exactly the spread plus the
// platform and strategy fees -- each on its own account.
func TestProp_FeeConservation(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s, in := genQuote(t)
		quote, err := s.Quote(in)
		if err != nil {
			t.Skip("the schedule refuses this order")
		}
		d := quote.Disclosure()
		c := ChargesOf(d)
		payer := accounts.NewAccountID()
		entries := ChargeEntries(payer, c)
		type key struct {
			owner ledger.OwnerType
			asset assets.AssetID
		}
		net := map[key]money.Quantity{}
		var paid, spreadRevenue, feeRevenue money.Quantity
		for _, e := range entries {
			if !e.Quantity.IsPositive() {
				t.Fatalf("a non-positive entry %s", e.Quantity)
			}
			k := key{e.Account.OwnerType, e.Account.AssetID}
			if e.Side == ledger.Debit {
				net[k] = net[k].Add(e.Quantity)
			} else {
				net[k] = net[k].Sub(e.Quantity)
			}
			switch {
			case e.Account.OwnerType == ledger.OwnerCustomer && e.Side == ledger.Credit:
				paid = paid.Add(e.Quantity)
			case e.Account.Code == ledger.CodeSpreadRevenue:
				spreadRevenue = spreadRevenue.Add(e.Quantity)
			case e.Account.Code == ledger.CodePlatformFeeRevenue:
				feeRevenue = feeRevenue.Add(e.Quantity)
			}
		}
		for k, v := range net {
			if !v.IsZero() {
				t.Fatalf("%s in %s does not balance: %s", k.owner, k.asset, v)
			}
		}
		charged := quote.TotalDebit.Sub(quote.NotionalAtReference)
		if in.Side == fees.SideSell {
			charged = quote.NotionalAtReference.Sub(quote.TotalCredit)
		}
		if !c.InQuoteAsset().Equal(charged) {
			t.Fatalf("itemized %s != charged %s", c.InQuoteAsset(), charged)
		}
		want := charged
		if !c.NetworkInQuote {
			want = want.Add(c.Network)
		}
		if !paid.Equal(want) {
			t.Fatalf("payer paid %s, disclosed %s", paid, want)
		}
		if !spreadRevenue.Equal(c.Spread) || !feeRevenue.Equal(c.Platform.Add(c.Strategy)) {
			t.Fatalf("revenue %s/%s for spread %s and fees %s+%s", spreadRevenue, feeRevenue, c.Spread, c.Platform, c.Strategy)
		}
		if !d.Fees.Total.Equal(c.Platform.Add(c.Strategy).Add(c.Venue)) {
			t.Fatalf("explicit fees %s != %s + %s + %s", d.Fees.Total, c.Platform, c.Strategy, c.Venue)
		}
		// Refunding is the exact mirror.
		for i, e := range refundEntries(payer, c) {
			if e.Side == entries[i].Side || !e.Quantity.Equal(entries[i].Quantity) || e.Account != entries[i].Account {
				t.Fatalf("refund entry %d is not the mirror of its charge", i)
			}
		}
	})
}

// TestProp_NoNegativeFeeAndNoFeeAboveTheConfiguredMax: no component of any
// quote is negative, and the explicit fees never exceed an enabled cap.
func TestProp_NoNegativeFeeAndNoFeeAboveTheConfiguredMax(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s, in := genQuote(t)
		quote, err := s.Quote(in)
		if err != nil {
			t.Skip("the schedule refuses this order")
		}
		c := ChargesOf(quote.Disclosure())
		for name, v := range map[string]money.Quantity{"spread": c.Spread, "platform": c.Platform, "strategy": c.Strategy,
			"venue": c.Venue, "network": c.Network, "reserve": quote.SlippageReserve.Amount} {
			if v.IsNegative() {
				t.Fatalf("%s is negative: %s", name, v)
			}
		}
		if s.MaxTotalFee.IsPositive() && quote.Fees.Total.Cmp(s.MaxTotalFee) > 0 {
			t.Fatalf("fees %s exceed the absolute cap %s", quote.Fees.Total, s.MaxTotalFee)
		}
		if s.MaxTotalFeeBPS > 0 {
			// total x 10_000 <= cap rate x exact notional, with the cap's
			// own rounding allowed one unit.
			lhs := quote.Fees.Total.Mul(q(10_000))
			rhs := quote.NotionalAtReference.Add(q(1)).Mul(q(int64(s.MaxTotalFeeBPS))).Add(q(9_999))
			if lhs.Cmp(rhs) > 0 {
				t.Fatalf("fees %s exceed %d bps of %s", quote.Fees.Total, s.MaxTotalFeeBPS, quote.NotionalAtReference)
			}
		}
	})
}

func genQuote(t *rapid.T) (fees.Schedule, fees.QuoteInput) {
	s := fees.ZeroSchedule(quoteAsset, 6)
	s.Version = "gen-v1"
	s.SpreadBPS = money.BPS(rapid.Int64Range(0, 500).Draw(t, "spread"))
	s.FixedFee = q(rapid.Int64Range(0, 2_000_000).Draw(t, "fixed"))
	s.PercentageFeeBPS = money.BPS(rapid.Int64Range(0, 300).Draw(t, "pct"))
	s.VenuePassThroughBPS = money.BPS(rapid.Int64Range(0, 100).Draw(t, "venue"))
	s.StrategyFeeBPS = money.BPS(rapid.Int64Range(0, 100).Draw(t, "strategy"))
	s.SubscriptionDiscountBPS = money.BPS(rapid.Int64Range(0, 10_000).Draw(t, "discount"))
	if rapid.Bool().Draw(t, "network") {
		asset := quoteAsset
		if rapid.Bool().Draw(t, "foreignNetwork") {
			asset = assets.NewAssetID()
		}
		s.NetworkFee = fees.NetworkFeeEstimate{Asset: asset, Amount: q(rapid.Int64Range(1, 1_000_000).Draw(t, "networkFee"))}
	}
	if rapid.Bool().Draw(t, "capBPS") {
		s.MaxTotalFeeBPS = money.BPS(rapid.Int64Range(1, 200).Draw(t, "maxBPS"))
	}
	if rapid.Bool().Draw(t, "capAbs") {
		s.MaxTotalFee = q(rapid.Int64Range(1, 5_000_000).Draw(t, "maxAbs"))
	}
	if rapid.Bool().Draw(t, "minFee") {
		s.MinFee = q(rapid.Int64Range(0, 1_000_000).Draw(t, "min"))
		if s.MaxTotalFee.IsPositive() && s.MinFee.Cmp(s.MaxTotalFee) > 0 {
			s.MinFee = s.MaxTotalFee
		}
	}
	side := fees.SideBuy
	if rapid.Bool().Draw(t, "sell") {
		side = fees.SideSell
	}
	ref, err := money.NewPrice(q(rapid.Int64Range(1, 10_000_000).Draw(t, "price")), int32(rapid.IntRange(0, 6).Draw(t, "scale")),
		quoteAsset.String(), "gen", time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return s, fees.QuoteInput{
		Side: side, BaseQty: q(rapid.Int64Range(1, 50_000_000).Draw(t, "qty")), BaseDecimals: 6, QuoteDecimals: 6,
		ReferencePrice: ref, HasSubscription: rapid.Bool().Draw(t, "subscriber"), StrategyOriginated: rapid.Bool().Draw(t, "strategyOrder"),
	}
}

func TestDecodeSchedule_RoundTripsAndRefusesAnythingElse(t *testing.T) {
	s := fullSchedule()
	b, err := s.Canonical()
	require.NoError(t, err)
	back, err := DecodeSchedule(b)
	require.NoError(t, err)
	again, err := back.Canonical()
	require.NoError(t, err)
	assert.Equal(t, b, again)
	h1, _ := s.Hash()
	h2, _ := back.Hash()
	assert.Equal(t, h1, h2)
	for _, bad := range [][]byte{nil, []byte(`{}`), append([]byte(" "), b...), []byte(`{"kind":"fees.schedule.v1","extra":1}`)} {
		_, err := DecodeSchedule(bad)
		assert.Error(t, err)
	}
}

func TestStatusMachineAndEnumLists(t *testing.T) {
	assert.True(t, CanTransitionPeriod(PeriodOpen, PeriodCrystallized))
	assert.True(t, CanTransitionPeriod(PeriodOpen, PeriodCancelled))
	assert.True(t, CanTransitionPeriod(PeriodCrystallized, PeriodReversed))
	for _, from := range AllPeriodStatuses() {
		for _, to := range AllPeriodStatuses() {
			legal := (from == PeriodOpen && (to == PeriodCrystallized || to == PeriodCancelled)) ||
				(from == PeriodCrystallized && to == PeriodReversed)
			assert.Equal(t, legal, CanTransitionPeriod(from, to), "%s -> %s", from, to)
		}
	}
	for _, k := range AllBookingKinds() {
		assert.True(t, k.IsSimulatedOnly(), "%s books simulated value only", k)
	}
	assert.Len(t, AllLineKinds(), 4)
}
