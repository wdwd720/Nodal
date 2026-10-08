package orders

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/money"
)

func unitSchedule(bt BookTerms, pct money.BPS) fees.Schedule {
	s := fees.ZeroSchedule(bt.QuoteAsset, bt.QuoteDecimals)
	s.Version = "unit-book-fees"
	s.PercentageFeeBPS = pct
	return s
}

// TestValidateBookSchedule_TheInternalBookChargesNoSpreadAndNothingPerOrder:
// the only charges a book schedule may carry are proportional, per fill, and
// disclosed; a spread is refused outright, because on the internal book the
// price is the counterparty's.
func TestValidateBookSchedule_TheInternalBookChargesNoSpreadAndNothingPerOrder(t *testing.T) {
	bt := unitBook(t)
	require.NoError(t, ValidateBookSchedule(unitSchedule(bt, 25), bt))
	ok := unitSchedule(bt, 25)
	ok.StrategyFeeBPS, ok.MaxTotalFeeBPS, ok.SubscriptionDiscountBPS = 10, 30, 5_000
	require.NoError(t, ValidateBookSchedule(ok, bt))

	cases := map[string]func(*fees.Schedule){
		"a spread":             func(s *fees.Schedule) { s.SpreadBPS = 1 },
		"a fixed fee":          func(s *fees.Schedule) { s.FixedFee = money.QuantityFromInt64(1) },
		"a minimum fee":        func(s *fees.Schedule) { s.MinFee = money.QuantityFromInt64(1) },
		"an absolute cap":      func(s *fees.Schedule) { s.MaxTotalFee = money.QuantityFromInt64(10) },
		"a venue pass-through": func(s *fees.Schedule) { s.VenuePassThroughBPS = 1 },
		"a network cost": func(s *fees.Schedule) {
			s.NetworkFee = fees.NetworkFeeEstimate{Asset: bt.QuoteAsset, Amount: money.QuantityFromInt64(1)}
		},
		"a slippage reserve":           func(s *fees.Schedule) { s.SlippageReserveBPS = 1 },
		"another quote asset":          func(s *fees.Schedule) { s.QuoteAsset = assets.NewAssetID() },
		"platform-favourable rounding": func(s *fees.Schedule) { s.Rounding.AllowPlatformFavourable = true },
		"a volume tier": func(s *fees.Schedule) {
			s.Tiers = []fees.VolumeTier{{MinTrailingVolume: money.USDFromMinor(100), OverridesPercentageFee: true, PercentageFeeBPS: 1}}
		},
		"a spread hidden in a tier only": func(s *fees.Schedule) {
			s.Tiers = []fees.VolumeTier{{MinTrailingVolume: money.USDFromMinor(100), OverridesSpread: true, SpreadBPS: 5}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := unitSchedule(bt, 25)
			mutate(&s)
			err := ValidateBookSchedule(s, bt)
			require.Error(t, err)
		})
	}
}

// TestProp_FeeBoundCoversEveryFillSplit: however a buy order of N lots at limit
// price P is filled -- any number of fills, at any prices at or below P, each
// as maker or as taker under its own schedule -- the fees it pays never exceed
// FeeBound(maker, taker, N x P), which is what its reservation holds beyond
// the notional. The notionals of the fills never exceed N x P either, so the
// reservation always covers every fill.
func TestProp_FeeBoundCoversEveryFillSplit(t *testing.T) {
	bt := unitBook(t)
	rapid.Check(t, func(rt *rapid.T) {
		maker := unitSchedule(bt, money.BPS(rapid.Int64Range(0, 500).Draw(rt, "maker bps")))
		taker := unitSchedule(bt, money.BPS(rapid.Int64Range(0, 500).Draw(rt, "taker bps")))
		maker.StrategyFeeBPS = money.BPS(rapid.Int64Range(0, 100).Draw(rt, "maker strategy"))
		taker.StrategyFeeBPS = money.BPS(rapid.Int64Range(0, 100).Draw(rt, "taker strategy"))
		if rapid.Bool().Draw(rt, "capped") {
			taker.MaxTotalFeeBPS = money.BPS(rapid.Int64Range(1, 1000).Draw(rt, "cap"))
		}
		strategy := rapid.Bool().Draw(rt, "strategy")
		limit := rapid.Int64Range(1, 1_000_000).Draw(rt, "limit ticks")
		lots := rapid.Int64Range(1, 100_000).Draw(rt, "lots")
		bound, err := FeeBound(maker, taker, bt.Notional(limit, lots), strategy)
		require.NoError(rt, err)
		var paid, notional money.Quantity
		left := lots
		for left > 0 {
			q := rapid.Int64Range(1, left).Draw(rt, "fill lots")
			price := rapid.Int64Range(1, limit).Draw(rt, "fill ticks")
			s := maker
			if rapid.Bool().Draw(rt, "as taker") {
				s = taker
			}
			fee, err := fillFee(s, SideBuy, bt, q, price, strategy, bt.TicksToPrice(price, "prop", time.Unix(0, 0)))
			require.NoError(rt, err)
			paid = paid.Add(fee)
			notional = notional.Add(bt.Notional(price, q))
			left -= q
		}
		if paid.Cmp(bound) > 0 {
			rt.Fatalf("fills paid %s in fees, more than the bound %s", paid, bound)
		}
		if notional.Cmp(bt.Notional(limit, lots)) > 0 {
			rt.Fatalf("fills cost %s, more than quantity x limit", notional)
		}
	})
}

func TestFillFee_IsTheScheduleQuoteOfTheFill(t *testing.T) {
	bt := unitBook(t)
	// 1 unit at 100.00 is 100 quote units; 25 bps of it is 0.25.
	fee, err := fillFee(unitSchedule(bt, 25), SideSell, bt, 10_000, 10_000, false, bt.TicksToPrice(10_000, "unit", time.Unix(0, 0)))
	require.NoError(t, err)
	assert.Equal(t, "250000", fee.String())
	// A spread can never be charged on a fill, even if a schedule carried one.
	spread := unitSchedule(bt, 25)
	spread.SpreadBPS = 10
	_, err = fillFee(spread, SideBuy, bt, 10_000, 10_000, false, bt.TicksToPrice(10_000, "unit", time.Unix(0, 0)))
	require.Error(t, err)
}

func TestDecodeSchedule_RoundTripsAndRefusesAnythingNotCanonical(t *testing.T) {
	bt := unitBook(t)
	s := unitSchedule(bt, 25)
	s.StrategyFeeBPS, s.MaxTotalFeeBPS = 7, 40
	s.Tiers = []fees.VolumeTier{{MinTrailingVolume: money.USDFromMinor(100_00), OverridesPercentageFee: true, PercentageFeeBPS: 3}}
	b, err := s.Canonical()
	require.NoError(t, err)
	got, err := DecodeSchedule(b)
	require.NoError(t, err)
	h1, err := s.Hash()
	require.NoError(t, err)
	h2, err := got.Hash()
	require.NoError(t, err)
	assert.Equal(t, h1, h2, "the decoded schedule is the one the hash names")

	for name, mutated := range map[string]string{
		"whitespace":    " " + string(b),
		"unknown field": string(b[:len(b)-1]) + `,"x":1}`,
		"wrong kind":    strings.ReplaceAll(string(b), `"fees.schedule.v1"`, `"fees.schedule.v0"`),
		"bad rounding":  strings.ReplaceAll(string(b), `"buy_notional":"down"`, `"buy_notional":"sideways"`),
	} {
		_, err := DecodeSchedule([]byte(mutated))
		require.Error(t, err, name)
	}
}
