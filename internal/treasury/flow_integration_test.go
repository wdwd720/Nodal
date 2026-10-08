//go:build integration

package treasury

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/orders"
)

// TestIntegration_RealizedAndUnrealizedPnLFromFillsAndMarks walks a house
// book through funding, a paper buy, an internal-book trade with another
// house book and a paper sale, and checks every PnL figure exactly: the
// position register at average cost, realized PnL net of the disclosed
// spread, unrealized PnL at the mark, the persisted mark-to-market, and the
// reconciliation of the register with the ledger.
func TestIntegration_RealizedAndUnrealizedPnLFromFillsAndMarks(t *testing.T) {
	f := newFixture(t)
	m := f.market("100.00")
	mm := f.register(econ.HouseMarketMaking, false)
	p := f.policy(m)
	p.PaperSpreadBPS = 10 // 0.10%
	f.setPolicy(mm, p)
	f.fund(mm, f.usd.ID, units(10_000))

	// Paper buy of 2 at 100.00 with a 10 bps spread: notional 200, spread 0.20.
	buy := f.paper(mm, econ.HouseHedging, m.base.ID, orders.SideBuy, units(2))
	require.NoError(t, buy.Rejection)
	require.NotNil(t, buy.Fill)
	assert.Equal(t, units(200).String(), buy.Fill.NotionalAtReference.String())
	assert.Equal(t, "200000", buy.Fill.SpreadAmount.String())
	assert.Equal(t, "200200000", buy.Fill.TotalQuote.String())
	f.requireOnlyHouseLegs(mm, buy.Fill.JournalTransactionID)
	assert.Equal(t, units(2).String(), f.inventory(mm, m.base.ID).String())
	assert.Equal(t, "9799800000", f.inventory(mm, f.usd.ID).String())
	assert.Equal(t, "200000", f.balance(mm.account(ledger.CodeHouseFees, f.usd.ID)).String())

	// A second house book buys 1 from a resting house ask at 110.00.
	other := f.register(econ.HouseHedging, false)
	f.setPolicy(other, f.policy(m))
	f.fund(other, f.usd.ID, units(10_000))
	ask := f.place(f.limit(mm, m, econ.HouseMarketMaking, orders.SideSell, "110.00", units(1)))
	require.NoError(t, ask.Rejection)
	assert.Equal(t, orders.StatusOpen, ask.Order.Status)
	take := f.place(PlaceOrderRequest{
		HouseAccountID: other.HouseAccountID, InstrumentID: m.inst, Activity: econ.HouseHedging, Side: orders.SideBuy,
		Type: orders.TypeLimit, TimeInForce: orders.IOC, Price: f.price("110.00"), Quantity: units(1),
		IdempotencyKey: "take-" + uuid.NewString(), Actor: houseRobot,
	})
	require.NoError(t, take.Rejection)
	require.Len(t, take.Trades, 1)
	assert.Equal(t, 0, take.Trades[0].CrossingRuleVersion, "house books trade with each other under the default rule")

	// The market maker's register learns of the fill on its next command.
	rep, err := do(f, func(ctx context.Context, tx pgx.Tx) (Report, error) { return f.svc.Report(ctx, tx, mm.HouseAccountID) })
	require.NoError(t, err)
	require.Len(t, rep.Assets, 1)
	a := rep.Assets[0]
	assert.Equal(t, units(1).String(), a.Quantity.String())
	// Cost released at average cost: 200 x 1/2 = 100; proceeds 110; spread 0.20 expensed on the buy.
	assert.Equal(t, units(100).String(), a.Cost.String())
	assert.Equal(t, "9800000", a.RealizedTotal.String(), "+10.00 on the sale less the 0.20 spread paid on the buy")
	assert.Equal(t, a.RealizedTotal.String(), a.RealizedToday.String())
	require.NotNil(t, a.Unrealized)
	assert.Equal(t, "0", a.Unrealized.String(), "1 at the mark 100.00 against a cost of 100.00")

	// The mark moves to 105: unrealized +5, no open loss.
	f.mark(m.base.ID, "105.00")
	snap, err := do(f, func(ctx context.Context, tx pgx.Tx) (Snapshot, error) {
		return f.svc.MarkToMarket(ctx, tx, mm.HouseAccountID, "mtm-"+uuid.NewString(), houseRobot)
	})
	require.NoError(t, err)
	assert.Equal(t, units(5).String(), snap.Report.UnrealizedTotal.String())
	assert.True(t, snap.Report.OpenLosses.IsZero())
	assert.Equal(t, "9800000", snap.Report.LossMeasure.String())
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM treasury_pnl_snapshots WHERE id = $1 AND document_hash = $2`, snap.ID, snap.DocumentHash))

	// Sell the last unit on the paper venue at 105 with the spread: proceeds
	// 105, spread 0.105 -> rounded down to 0.105000.
	sell := f.paper(mm, econ.HouseHedging, m.base.ID, orders.SideSell, units(1))
	require.NoError(t, sell.Rejection)
	assert.Equal(t, "105000", sell.Fill.SpreadAmount.String())
	rep, err = do(f, func(ctx context.Context, tx pgx.Tx) (Report, error) { return f.svc.Report(ctx, tx, mm.HouseAccountID) })
	require.NoError(t, err)
	assert.True(t, rep.Assets[0].Quantity.IsZero())
	assert.True(t, rep.Assets[0].Cost.IsZero(), "a closed position carries no cost")
	// 9.80 + (105 - 100 - 0.105) = 14.695
	assert.Equal(t, "14695000", rep.RealizedTotal.String())

	// The register and the ledger agree, for both books.
	assert.Empty(t, f.reconcile(mm))
	assert.Empty(t, f.reconcile(other))

	// Every fill of the paper venue is recomputable from its record alone.
	for _, fill := range []*PaperFill{buy.Fill, sell.Fill} {
		stored, err := f.svc.Fill(f.ctx, testDB, fill.ID)
		require.NoError(t, err)
		s, err := orders.DecodeSchedule(stored.Schedule)
		require.NoError(t, err)
		require.NoError(t, fees.VerifyDisclosure(s, stored.Disclosure))
		assert.Equal(t, fill.DisclosureHash, stored.DisclosureHash)
	}
}
