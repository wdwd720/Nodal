package fees

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

func TestSchedule_ZeroAndWorkedSchedulesValidate(t *testing.T) {
	require.NoError(t, ZeroSchedule(quoteAsset, 6).Validate())
	require.NoError(t, workedSchedule().Validate())

	z := ZeroSchedule(quoteAsset, 6)
	require.Equal(t, "fee-schedule-zero-v1", z.Version, "no charge is an explicit, versioned choice")
	require.Equal(t, DefaultRounding(), z.Rounding)
}

func TestSchedule_ValidateRejections(t *testing.T) {
	tier := func(vol string, pct, spread money.BPS, setPct, setSpread bool) VolumeTier {
		return VolumeTier{MinTrailingVolume: usd(t, vol), OverridesPercentageFee: setPct, PercentageFeeBPS: pct, OverridesSpread: setSpread, SpreadBPS: spread}
	}
	cases := []struct {
		name  string
		mut   func(*Schedule)
		field string
	}{
		{"empty version", func(s *Schedule) { s.Version = "" }, "version"},
		{"padded version", func(s *Schedule) { s.Version = " v1" }, "version"},
		{"control character", func(s *Schedule) { s.Version = "v\x001" }, "version"},
		{"overlong version", func(s *Schedule) { s.Version = strings.Repeat("v", MaxLabelLength+1) }, "version"},
		{"no quote asset", func(s *Schedule) { s.QuoteAsset = assets.AssetID{} }, "quote_asset"},
		{"negative spread", func(s *Schedule) { s.SpreadBPS = -1 }, "spread_bps"},
		{"spread of 100%", func(s *Schedule) { s.SpreadBPS = money.OneHundredPercent }, "spread_bps"},
		{"negative fixed fee", func(s *Schedule) { s.FixedFee = q("-1") }, "fixed_fee"},
		{"percentage above 100%", func(s *Schedule) { s.PercentageFeeBPS = 10_001 }, "percentage_fee_bps"},
		{"negative pass-through", func(s *Schedule) { s.VenuePassThroughBPS = -1 }, "venue_pass_through_bps"},
		{"negative network fee", func(s *Schedule) { s.NetworkFee.Amount = q("-1") }, "network_fee.amount"},
		{"network fee without asset", func(s *Schedule) { s.NetworkFee = NetworkFeeEstimate{Amount: q("5")} }, "network_fee.asset"},
		{"reserve above 100%", func(s *Schedule) { s.SlippageReserveBPS = 10_001 }, "slippage_reserve_bps"},
		{"discount above 100%", func(s *Schedule) { s.SubscriptionDiscountBPS = 10_001 }, "subscription_discount_bps"},
		{"negative discount", func(s *Schedule) { s.SubscriptionDiscountBPS = -1 }, "subscription_discount_bps"},
		{"negative strategy fee", func(s *Schedule) { s.StrategyFeeBPS = -5 }, "strategy_fee_bps"},
		{"cap above 100%", func(s *Schedule) { s.MaxTotalFeeBPS = 10_001 }, "max_total_fee_bps"},
		{"negative absolute cap", func(s *Schedule) { s.MaxTotalFee = q("-1") }, "max_total_fee"},
		{"negative minimum", func(s *Schedule) { s.MinFee = q("-1") }, "min_fee"},
		{"minimum above cap", func(s *Schedule) { s.MinFee, s.MaxTotalFee = q("10"), q("5") }, "min_fee"},
		{"zero-volume tier", func(s *Schedule) { s.Tiers = []VolumeTier{tier("0", 5, 0, true, false)} }, "tiers[0].min_trailing_volume"},
		{"descending tiers", func(s *Schedule) {
			s.Tiers = []VolumeTier{tier("100", 5, 0, true, false), tier("50", 4, 0, true, false)}
		}, "tiers[1].min_trailing_volume"},
		{"duplicate tier", func(s *Schedule) {
			s.Tiers = []VolumeTier{tier("100", 5, 0, true, false), tier("100", 4, 0, true, false)}
		}, "tiers[1].min_trailing_volume"},
		{"tier overriding nothing", func(s *Schedule) { s.Tiers = []VolumeTier{tier("100", 0, 0, false, false)} }, "tiers[0]"},
		{"hidden tier percentage", func(s *Schedule) { s.Tiers = []VolumeTier{tier("100", 5, 10, false, true)} }, "tiers[0].percentage_fee_bps"},
		{"hidden tier spread", func(s *Schedule) { s.Tiers = []VolumeTier{tier("100", 5, 10, true, false)} }, "tiers[0].spread_bps"},
		{"tier percentage above 100%", func(s *Schedule) { s.Tiers = []VolumeTier{tier("100", 10_001, 0, true, false)} }, "tiers[0].percentage_fee_bps"},
		{"tier spread of 100%", func(s *Schedule) { s.Tiers = []VolumeTier{tier("100", 0, 10_000, false, true)} }, "tiers[0].spread_bps"},
		{"too many tiers", func(s *Schedule) {
			for i := range MaxTiers + 1 {
				s.Tiers = append(s.Tiers, VolumeTier{MinTrailingVolume: money.USDFromMinor(int64(i + 1)), OverridesSpread: true})
			}
		}, "tiers"},
		{"unset rounding", func(s *Schedule) { s.Rounding = RoundingPolicy{} }, "rounding.buy_notional"},
		{"exact rounding", func(s *Schedule) { s.Rounding.Spread = money.RoundExact }, "rounding.spread"},
		{"spread rounding up", func(s *Schedule) { s.Rounding.Spread = money.RoundUp }, "rounding.spread"},
		{"percentage half-even", func(s *Schedule) { s.Rounding.PercentageFee = money.RoundHalfEven }, "rounding.percentage_fee"},
		{"pass-through ceil", func(s *Schedule) { s.Rounding.VenuePassThrough = money.RoundCeil }, "rounding.venue_pass_through"},
		{"strategy half-up", func(s *Schedule) { s.Rounding.StrategyFee = money.RoundHalfUp }, "rounding.strategy_fee"},
		{"buy notional up", func(s *Schedule) { s.Rounding.BuyNotional = money.RoundUp }, "rounding.buy_notional"},
		{"sell notional down", func(s *Schedule) { s.Rounding.SellNotional = money.RoundDown }, "rounding.sell_notional"},
		{"cap rounding up", func(s *Schedule) { s.Rounding.FeeCap = money.RoundUp }, "rounding.fee_cap"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := workedSchedule()
			c.mut(&s)
			requireFields(t, s.Validate(), errs.CodeValidationFailed, c.field)
		})
	}
}

// TestSchedule_PlatformFavourableRoundingNeedsExplicitOptIn: a mode that can
// round against the customer is refused unless the schedule adopts it
// explicitly, and the reserve, which is never charged, takes any mode.
func TestSchedule_PlatformFavourableRoundingNeedsExplicitOptIn(t *testing.T) {
	s := workedSchedule()
	s.Rounding.Spread = money.RoundHalfEven
	s.Rounding.SellNotional = money.RoundHalfEven
	requireFields(t, s.Validate(), errs.CodeValidationFailed, "rounding.spread", "rounding.sell_notional")

	s.Rounding.AllowPlatformFavourable = true
	require.NoError(t, s.Validate())

	held := workedSchedule()
	held.Rounding.SlippageReserve = money.RoundDown
	require.NoError(t, held.Validate(), "the reserve is held, not charged; no mode favours the platform")

	floor := workedSchedule()
	floor.Rounding.Spread, floor.Rounding.SellNotional = money.RoundFloor, money.RoundCeil
	require.NoError(t, floor.Validate(), "floor and ceil are the customer-favourable modes for non-negative amounts")
}

func TestSchedule_HashIsStableAndCoversEveryField(t *testing.T) {
	base := workedSchedule()
	h0, err := base.Hash()
	require.NoError(t, err)
	again, err := workedSchedule().Hash()
	require.NoError(t, err)
	require.Equal(t, h0, again, "equal content, equal hash")
	require.Len(t, h0, 64)

	nilTiers, emptyTiers := workedSchedule(), workedSchedule()
	emptyTiers.Tiers = []VolumeTier{}
	hn, _ := nilTiers.Hash()
	he, _ := emptyTiers.Hash()
	require.Equal(t, hn, he, "an absent ladder and an empty ladder are the same schedule")

	tiered := func(s *Schedule) {
		s.Tiers = []VolumeTier{{MinTrailingVolume: money.USDFromMinor(1_000_000), OverridesPercentageFee: true, PercentageFeeBPS: 8}}
	}
	muts := map[string]func(*Schedule){
		"version":          func(s *Schedule) { s.Version = "fees-sched-v2" },
		"quote asset":      func(s *Schedule) { s.QuoteAsset = solAsset },
		"quote decimals":   func(s *Schedule) { s.QuoteDecimals = 9 },
		"spread":           func(s *Schedule) { s.SpreadBPS = 26 },
		"fixed":            func(s *Schedule) { s.FixedFee = q("500001") },
		"percentage":       func(s *Schedule) { s.PercentageFeeBPS = 11 },
		"pass-through":     func(s *Schedule) { s.VenuePassThroughBPS = 6 },
		"network asset":    func(s *Schedule) { s.NetworkFee.Asset = quoteAsset },
		"network amount":   func(s *Schedule) { s.NetworkFee.Amount = q("5001") },
		"reserve":          func(s *Schedule) { s.SlippageReserveBPS = 51 },
		"discount":         func(s *Schedule) { s.SubscriptionDiscountBPS = 2_501 },
		"tier added":       tiered,
		"tier threshold":   func(s *Schedule) { tiered(s); s.Tiers[0].MinTrailingVolume = money.USDFromMinor(1_000_001) },
		"tier percentage":  func(s *Schedule) { tiered(s); s.Tiers[0].PercentageFeeBPS = 7 },
		"tier spread flag": func(s *Schedule) { tiered(s); s.Tiers[0].OverridesSpread = true },
		"strategy":         func(s *Schedule) { s.StrategyFeeBPS = 21 },
		"cap bps":          func(s *Schedule) { s.MaxTotalFeeBPS = 100 },
		"cap absolute":     func(s *Schedule) { s.MaxTotalFee = q("1") },
		"minimum":          func(s *Schedule) { s.MinFee = q("1") },
		"buy notional":     func(s *Schedule) { s.Rounding.BuyNotional = money.RoundFloor },
		"sell notional":    func(s *Schedule) { s.Rounding.SellNotional = money.RoundCeil },
		"spread mode":      func(s *Schedule) { s.Rounding.Spread = money.RoundFloor },
		"percentage mode":  func(s *Schedule) { s.Rounding.PercentageFee = money.RoundFloor },
		"pass-through mode": func(s *Schedule) {
			s.Rounding.VenuePassThrough = money.RoundFloor
		},
		"strategy mode":   func(s *Schedule) { s.Rounding.StrategyFee = money.RoundFloor },
		"cap mode":        func(s *Schedule) { s.Rounding.FeeCap = money.RoundFloor },
		"reserve mode":    func(s *Schedule) { s.Rounding.SlippageReserve = money.RoundCeil },
		"platform opt-in": func(s *Schedule) { s.Rounding.AllowPlatformFavourable = true },
	}
	seen := map[string]string{h0: "base"}
	for name, mut := range muts {
		s := workedSchedule()
		mut(&s)
		h, err := s.Hash()
		require.NoError(t, err)
		prev, dup := seen[h]
		require.False(t, dup, "%s hashes like %s: the field is outside the hash", name, prev)
		seen[h] = name
	}
}

// TestSchedule_CanonicalForm pins the hashed format: changing it changes
// every recorded schedule hash, so it must be a deliberate, versioned act.
func TestSchedule_CanonicalForm(t *testing.T) {
	b, err := ZeroSchedule(quoteAsset, 6).Canonical()
	require.NoError(t, err)
	require.JSONEq(t, `{
		"kind":"fees.schedule.v1","version":"fee-schedule-zero-v1",
		"quote_asset":"01920000-0000-7000-8000-0000000000a1","quote_decimals":6,
		"spread_bps":0,"fixed_fee":"0","percentage_fee_bps":0,"venue_pass_through_bps":0,
		"network_fee_asset":"00000000-0000-0000-0000-000000000000","network_fee_amount":"0",
		"slippage_reserve_bps":0,"subscription_discount_bps":0,"tiers":[],"strategy_fee_bps":0,
		"max_total_fee_bps":0,"max_total_fee":"0","min_fee":"0",
		"rounding":{"buy_notional":"down","sell_notional":"up","spread":"down","percentage_fee":"down",
			"venue_pass_through":"down","strategy_fee":"down","fee_cap":"down","slippage_reserve":"up",
			"allow_platform_favourable":false}}`, string(b))
	require.NotContains(t, string(b), " ", "canonical form has no insignificant whitespace")
}

func ladder(t *testing.T) Schedule {
	s := workedSchedule()
	s.Tiers = []VolumeTier{
		{MinTrailingVolume: usd(t, "10000.00"), OverridesPercentageFee: true, PercentageFeeBPS: 8, OverridesSpread: true, SpreadBPS: 20},
		{MinTrailingVolume: usd(t, "100000.00"), OverridesPercentageFee: true, PercentageFeeBPS: 5},
		{MinTrailingVolume: usd(t, "1000000.00"), OverridesSpread: true, SpreadBPS: 10},
	}
	return s
}

// TestTierFor_Boundaries: a tier applies from its threshold inclusive; one
// cent below it the previous tier (or the base schedule) applies.
func TestTierFor_Boundaries(t *testing.T) {
	s := ladder(t)
	cases := []struct {
		volume      string
		index       int
		pct, spread money.BPS
	}{
		{"0.00", -1, 10, 25},
		{"9999.99", -1, 10, 25},
		{"10000.00", 0, 8, 20},
		{"10000.01", 0, 8, 20},
		{"99999.99", 0, 8, 20},
		{"100000.00", 1, 5, 25},
		{"999999.99", 1, 5, 25},
		{"1000000.00", 2, 10, 10},
		{"92233720368547758.07", 2, 10, 10},
	}
	for _, c := range cases {
		got, err := s.TierFor(usd(t, c.volume))
		require.NoError(t, err)
		require.Equal(t, c.index, got.Index, "volume %s", c.volume)
		require.Equal(t, c.pct, got.PercentageFeeBPS, "volume %s", c.volume)
		require.Equal(t, c.spread, got.SpreadBPS, "volume %s", c.volume)
		if c.index >= 0 {
			require.Equal(t, s.Tiers[c.index].MinTrailingVolume, got.MinTrailingVolume)
		} else {
			require.True(t, got.MinTrailingVolume.IsZero())
		}

		quote := mustQuote(t, s, QuoteInput{
			Side: SideBuy, BaseQty: q("1500000000"), BaseDecimals: 9, QuoteDecimals: 6,
			ReferencePrice: refPrice("150123456", 6), TrailingVolume: usd(t, c.volume),
		})
		require.Equal(t, got, quote.Tier, "the quote records the tier that priced it")
		require.Equal(t, c.spread, quote.Spread.BPS)
		require.Equal(t, c.pct, quote.Fees.Percentage.RateBPS)
	}
}

// TestTierFor_OverridesAreRelativeToTheBaseSchedule: a tier that overrides
// only the percentage fee keeps the BASE spread, not a lower tier's.
func TestTierFor_OverridesAreRelativeToTheBaseSchedule(t *testing.T) {
	s := ladder(t)
	got, err := s.TierFor(usd(t, "150000.00"))
	require.NoError(t, err)
	require.Equal(t, 1, got.Index)
	require.Equal(t, s.SpreadBPS, got.SpreadBPS, "tier 1 does not override the spread, so the base spread applies, not tier 0's")
}

func TestTierFor_Rejections(t *testing.T) {
	_, err := ladder(t).TierFor(money.USDFromMinor(-1))
	requireFields(t, err, errs.CodeValidationFailed, "trailing_volume")

	bad := ladder(t)
	bad.Tiers[1].MinTrailingVolume = bad.Tiers[0].MinTrailingVolume
	_, err = bad.TierFor(money.USDFromMinor(0))
	requireFields(t, err, errs.CodeValidationFailed, "tiers[1].min_trailing_volume")
}
