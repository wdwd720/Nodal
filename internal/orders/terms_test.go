package orders

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/matching"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

func simAssetFor(decimals uint8) assets.Asset {
	return assets.Asset{ID: assets.NewAssetID(), Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated, Decimals: decimals,
		IsStablecoin: true, PegCurrency: "USD"}
}

// unitTerms builds instrument terms and the matching instrument row for a
// spot pair of base and quote, at the given price scale, tick and lot.
func unitTerms(base, quote assets.Asset, scale int32, tick, lot int64) (instruments.Instrument, instruments.Terms) {
	b, q := base.ID, quote.ID
	terms := instruments.Terms{
		Kind: instruments.TypeSpotPair, Symbol: "UNIT", VenueCode: "NODAL-SIM", BaseAssetID: &b, QuoteAssetID: &q,
		SettlementAssetID: q, ContractMultiplier: money.QuantityFromInt64(1), PriceScale: scale,
		TickSize: money.QuantityFromInt64(tick), LotSize: money.QuantityFromInt64(lot), MinQuantity: money.QuantityFromInt64(lot),
		QuantityDecimals: base.Decimals, SettlementStyle: instruments.SettlePhysical, TradingCalendar: "24x7", OracleMaxAgeMS: 30_000,
		MarginModel: instruments.MarginNone, CorporateActionBehavior: instruments.CorporateActionsNone, Transferability: instruments.Transferable,
	}
	inst := instruments.Instrument{ID: instruments.NewInstrumentID(), Type: instruments.TypeSpotPair, BaseAssetID: &b, QuoteAssetID: &q,
		SettlementAssetID: q}
	return inst, terms
}

func unitBook(t testing.TB) BookTerms {
	base, quote := simAssetFor(6), simAssetFor(6)
	inst, terms := unitTerms(base, quote, 2, 1, 100)
	bt, err := DeriveBookTerms(inst, terms, base, quote)
	require.NoError(t, err)
	return bt
}

func priceOf(t testing.TB, bt BookTerms, s string) money.Price {
	p, err := money.PriceFromDecimalString(s, bt.QuoteAsset.String(), "unit", time.Unix(0, 0))
	require.NoError(t, err)
	return p
}

func TestDeriveBookTerms_NotionalPerTickLotIsExact(t *testing.T) {
	bt := unitBook(t)
	assert.Equal(t, "1", bt.NotionalPerTickLot.String(), "0.01 x 0.0001 at 6 decimals is one quote base unit")
	assert.Equal(t, matching.MaxPriceTicks, bt.MaxPriceTicks)
	// 1 unit (10_000 lots) at 100.00 (10_000 ticks) is 100 quote units.
	assert.Equal(t, "100000000", bt.Notional(10_000, 10_000).String())
}

func TestDeriveBookTerms_RefusesWhatCannotTradeExactly(t *testing.T) {
	cases := map[string]func(base, quote *assets.Asset, inst *instruments.Instrument, terms *instruments.Terms){
		"a fractional notional per tick-lot": func(_, _ *assets.Asset, _ *instruments.Instrument, terms *instruments.Terms) {
			terms.LotSize, terms.MinQuantity = money.QuantityFromInt64(1), money.QuantityFromInt64(1)
		},
		"a real-value asset": func(base, _ *assets.Asset, _ *instruments.Instrument, _ *instruments.Terms) {
			base.Kind, base.ValueDomain = assets.KindSPLToken, valuedomain.SelfCustodialCrypto
		},
		"a contract multiplier": func(_, _ *assets.Asset, _ *instruments.Instrument, terms *instruments.Terms) {
			terms.ContractMultiplier = money.QuantityFromInt64(2)
		},
		"a base asset that is not the instrument's": func(_, _ *assets.Asset, inst *instruments.Instrument, _ *instruments.Terms) {
			other := assets.NewAssetID()
			inst.BaseAssetID = &other
		},
		"terms of another kind": func(_, _ *assets.Asset, inst *instruments.Instrument, _ *instruments.Terms) {
			inst.Type = instruments.TypeEquity
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			base, quote := simAssetFor(6), simAssetFor(6)
			inst, terms := unitTerms(base, quote, 2, 1, 100)
			mutate(&base, &quote, &inst, &terms)
			_, err := DeriveBookTerms(inst, terms, base, quote)
			require.Error(t, err)
		})
	}
}

func TestDeriveBookTerms_OutcomeSharesAreCappedAtOneUnitOfCollateral(t *testing.T) {
	share, collateral := simAssetFor(6), simAssetFor(6)
	s, c := share.ID, collateral.ID
	expiry := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	terms := instruments.Terms{
		Kind: instruments.TypeEventOutcome, Symbol: "WILL-IT", VenueCode: "NODAL-SIM", BaseAssetID: &s, QuoteAssetID: &c,
		SettlementAssetID: c, ContractMultiplier: money.QuantityFromInt64(1), PriceScale: 2, TickSize: money.QuantityFromInt64(1),
		LotSize: money.QuantityFromInt64(100), MinQuantity: money.QuantityFromInt64(100), QuantityDecimals: 6,
		Expiry: &expiry, SettlementStyle: instruments.SettleCash, TradingCalendar: "24x7", OracleMaxAgeMS: 30_000,
		MarginModel: instruments.MarginBinaryOutcome, CorporateActionBehavior: instruments.CorporateActionsNone,
		Transferability: instruments.Transferable, Outcomes: []string{"YES", "NO"},
	}
	inst := instruments.Instrument{ID: instruments.NewInstrumentID(), Type: instruments.TypeEventOutcome, BaseAssetID: &s,
		QuoteAssetID: &c, SettlementAssetID: c}
	bt, err := DeriveBookTerms(inst, terms, share, collateral)
	require.NoError(t, err)
	assert.EqualValues(t, 100, bt.MaxPriceTicks, "a share never trades above 1.00 collateral")
	_, err = bt.PriceToTicks(priceOf(t, bt, "1.01"))
	assert.Equal(t, ReasonPriceOutOfRange, ReasonOf(err))
	ticks, err := bt.PriceToTicks(priceOf(t, bt, "0.37"))
	require.NoError(t, err)
	assert.EqualValues(t, 37, ticks)
}

func TestBookTerms_ConversionsAreExactOrRefusedWithAStableReason(t *testing.T) {
	bt := unitBook(t)
	ticks, err := bt.PriceToTicks(priceOf(t, bt, "100.00"))
	require.NoError(t, err)
	assert.EqualValues(t, 10_000, ticks)
	ticks, err = bt.PriceToTicks(priceOf(t, bt, "100.0000"))
	require.NoError(t, err, "trailing zeros below the scale are exact")
	assert.EqualValues(t, 10_000, ticks)

	wrongQuote := priceOf(t, bt, "100.00")
	wrongQuote.QuoteAsset = assets.NewAssetID().String()
	zero := priceOf(t, bt, "0.00")
	priceCases := map[string]struct {
		p      money.Price
		reason string
	}{
		"too precise":      {priceOf(t, bt, "100.001"), ReasonPricePrecision},
		"wrong quote":      {wrongQuote, ReasonPriceQuoteAsset},
		"zero":             {zero, ReasonPriceNotPositive},
		"negative":         {money.Price{Mantissa: money.QuantityFromInt64(-1), Scale: 2, QuoteAsset: bt.QuoteAsset.String()}, ReasonPriceNotPositive},
		"scale too large":  {money.Price{Mantissa: money.QuantityFromInt64(1), Scale: 99, QuoteAsset: bt.QuoteAsset.String()}, ReasonPriceNotPositive},
		"beyond the range": {priceOf(t, bt, "10000000000000.01"), ReasonPriceOutOfRange},
	}
	for name, c := range priceCases {
		_, err := bt.PriceToTicks(c.p)
		require.Error(t, err, name)
		assert.Equal(t, c.reason, ReasonOf(err), name)
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), name)
	}

	tick5 := bt
	tick5.TickSize = money.QuantityFromInt64(5)
	_, err = tick5.PriceToTicks(priceOf(t, bt, "100.03"))
	assert.Equal(t, ReasonPriceOffTick, ReasonOf(err))

	lots, err := bt.QuantityToLots(money.QuantityFromInt64(1_000_000))
	require.NoError(t, err)
	assert.EqualValues(t, 10_000, lots)
	qtyCases := map[string]struct {
		q      money.Quantity
		reason string
	}{
		"zero":        {money.QuantityFromInt64(0), ReasonQtyNotPositive},
		"negative":    {money.QuantityFromInt64(-100), ReasonQtyNotPositive},
		"off lot":     {money.QuantityFromInt64(150), ReasonQtyOffLot},
		"below min":   {money.QuantityFromInt64(99), ReasonQtyBelowMinimum},
		"too many":    {money.QuantityFromInt64(matching.MaxQtyLots).Mul(money.QuantityFromInt64(100)).Add(money.QuantityFromInt64(100)), ReasonQtyOutOfRange},
		"astronomic":  {pow10(60), ReasonQtyOutOfRange},
		"min is lots": {money.QuantityFromInt64(100), ""},
	}
	for name, c := range qtyCases {
		_, err := bt.QuantityToLots(c.q)
		assert.Equal(t, c.reason, ReasonOf(err), name)
	}
	assert.Equal(t, ReasonNotionalTooLarge, ReasonOf(bt.CheckNotional(matching.MaxPriceTicks, 1_001)))
	assert.NoError(t, bt.CheckNotional(1_000, 1_000))
}

// TestProp_PriceAndQuantityConversionsRoundTripExactly: every tick count is a
// price that converts back to the same tick count, in any equivalent decimal
// representation, and every lot count is a quantity that converts back to it.
func TestProp_PriceAndQuantityConversionsRoundTripExactly(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		base, quote := simAssetFor(uint8(rapid.IntRange(0, 12).Draw(rt, "bd"))), simAssetFor(uint8(rapid.IntRange(0, 12).Draw(rt, "qd")))
		scale := int32(rapid.IntRange(0, 8).Draw(rt, "scale"))
		tick := rapid.Int64Range(1, 1000).Draw(rt, "tick")
		lot := rapid.Int64Range(1, 1_000_000).Draw(rt, "lot")
		inst, terms := unitTerms(base, quote, scale, tick, lot)
		bt, err := DeriveBookTerms(inst, terms, base, quote)
		if err != nil {
			if ReasonOf(err) != "ORDER_TERMS_NOTIONAL_INEXACT" {
				rt.Fatalf("unexpected refusal: %v", err)
			}
			return
		}
		ticks := rapid.Int64Range(1, 1_000_000_000).Draw(rt, "ticks")
		p := bt.TicksToPrice(ticks, "prop", time.Unix(0, 0))
		got, err := bt.PriceToTicks(p)
		if err != nil || got != ticks {
			rt.Fatalf("ticks %d -> %s -> %d (%v)", ticks, p, got, err)
		}
		k := rapid.IntRange(0, 6).Draw(rt, "extra zeros")
		p.Mantissa, p.Scale = p.Mantissa.Mul(pow10(k)), p.Scale+int32(k)
		if got, err := bt.PriceToTicks(p); err != nil || got != ticks {
			rt.Fatalf("an equivalent representation converted differently: %d (%v)", got, err)
		}
		lots := rapid.Int64Range(1, 1_000_000_000).Draw(rt, "lots")
		if got, err := bt.QuantityToLots(bt.LotsToQuantity(lots)); err != nil || got != lots {
			rt.Fatalf("lots %d -> %d (%v)", lots, got, err)
		}
		// The book's notional is exactly what money.Notional computes, with
		// nothing to round.
		exact, err := money.Notional(bt.LotsToQuantity(lots), bt.BaseDecimals, bt.TicksToPrice(ticks, "prop", time.Unix(0, 0)),
			bt.QuoteDecimals, money.RoundExact)
		if err != nil || !exact.Equal(bt.Notional(ticks, lots)) {
			rt.Fatalf("notional %s differs from the exact %s (%v)", bt.Notional(ticks, lots), exact, err)
		}
	})
}
