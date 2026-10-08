package orders

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/matching"
	"github.com/nodal/controlplane/internal/money"
)

// FuzzOrderConversion: malformed prices, quantities and instrument terms never
// panic. Terms either derive a book or are refused; a price and a quantity
// either convert exactly -- the ticks and lots are in range and map back to
// exactly the price and quantity given -- or are refused with a stable reason
// and VALIDATION_FAILED. Nothing in between: nothing is rounded.
func FuzzOrderConversion(f *testing.F) {
	f.Add(uint8(6), uint8(6), int32(2), int64(1), int64(100), "10000", int32(2), "1000000", false)
	f.Add(uint8(8), uint8(6), int32(2), int64(1), int64(10_000), "6500050", int32(2), "100000000", false)
	f.Add(uint8(6), uint8(6), int32(2), int64(5), int64(100), "10003", int32(2), "150", true)
	f.Add(uint8(0), uint8(18), int32(18), int64(-1), int64(0), "-1", int32(40), "-5", false)
	f.Add(uint8(20), uint8(6), int32(2), int64(1), int64(100), "99999999999999999999999999999999", int32(0), "1e3", false)
	f.Fuzz(func(t *testing.T, bd, qd uint8, scale int32, tick, lot int64, mantissa string, pscale int32, qty string, wrongQuote bool) {
		base, quote := simAssetFor(bd), simAssetFor(qd)
		b, q := base.ID, quote.ID
		terms := instruments.Terms{
			Kind: instruments.TypeSpotPair, Symbol: "FUZZ", VenueCode: "NODAL-SIM", BaseAssetID: &b, QuoteAssetID: &q,
			SettlementAssetID: q, ContractMultiplier: money.QuantityFromInt64(1), PriceScale: scale,
			TickSize: money.QuantityFromInt64(tick), LotSize: money.QuantityFromInt64(lot), MinQuantity: money.QuantityFromInt64(lot),
			QuantityDecimals: bd, SettlementStyle: instruments.SettlePhysical, TradingCalendar: "24x7", OracleMaxAgeMS: 1,
			MarginModel: instruments.MarginNone, CorporateActionBehavior: instruments.CorporateActionsNone,
			Transferability: instruments.Transferable,
		}
		inst := instruments.Instrument{ID: instruments.NewInstrumentID(), Type: instruments.TypeSpotPair, BaseAssetID: &b,
			QuoteAssetID: &q, SettlementAssetID: q}
		bt, err := DeriveBookTerms(inst, terms, base, quote)
		if err != nil {
			return
		}
		m, err := money.ParseQuantity(mantissa)
		if err != nil {
			return
		}
		p := money.Price{Mantissa: m, Scale: pscale, QuoteAsset: bt.QuoteAsset.String(), Source: "fuzz", At: time.Unix(0, 0)}
		if wrongQuote {
			p.QuoteAsset = assets.NewAssetID().String()
		}
		ticks, err := bt.PriceToTicks(p)
		if err != nil {
			require.NotEmpty(t, ReasonOf(err))
			require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		} else {
			require.True(t, ticks >= 1 && ticks <= bt.MaxPriceTicks)
			back := bt.TicksToPrice(ticks, "fuzz", time.Unix(0, 0))
			require.Zero(t, priceCmp(back, p), "the ticks are exactly the price")
		}
		qv, err := money.ParseQuantity(qty)
		if err != nil {
			return
		}
		lots, err := bt.QuantityToLots(qv)
		if err != nil {
			require.NotEmpty(t, ReasonOf(err))
			return
		}
		require.True(t, lots >= 1 && lots <= matching.MaxQtyLots)
		require.True(t, bt.LotsToQuantity(lots).Equal(qv), "the lots are exactly the quantity")
	})
}
