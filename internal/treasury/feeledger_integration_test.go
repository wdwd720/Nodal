//go:build integration

package treasury

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/feeledger"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/orders"
)

// feeMarket is a market like f.market's whose book charges a maker and a
// taker fee.
func (f *fixture) feeMarket(price string, makerBPS, takerBPS money.BPS) market {
	f.t.Helper()
	base := f.simAsset("TRF")
	m := market{base: base}
	schedule := func(pct money.BPS, label string) fees.Schedule {
		s := fees.ZeroSchedule(f.usd.ID, 6)
		s.Version, s.PercentageFeeBPS = label, pct
		return s
	}
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		inst, err := f.instr.CreateSpotPair(ctx, tx, instruments.SpotPairSpec{
			Base: base.ID, Quote: f.usd.ID, Settlement: f.usd.ID, CanonicalName: base.Symbol + "/" + f.usd.Symbol,
			RiskClass: assets.RiskSettlement, Status: assets.StatusActive, ActiveFrom: f.clk.Now().AddDate(0, 0, -1),
		})
		if err != nil {
			return err
		}
		baseID, quoteID := base.ID, f.usd.ID
		terms := instruments.Terms{
			Kind: instruments.TypeSpotPair, Symbol: "TRFEE", VenueCode: "NODAL-SIM", BaseAssetID: &baseID, QuoteAssetID: &quoteID,
			SettlementAssetID: quoteID, ContractMultiplier: money.QuantityFromInt64(1), PriceScale: 2,
			TickSize: money.QuantityFromInt64(1), LotSize: money.QuantityFromInt64(100), MinQuantity: money.QuantityFromInt64(100),
			QuantityDecimals: 6, SettlementStyle: instruments.SettlePhysical, TradingCalendar: "24x7", OracleMaxAgeMS: 30_000,
			MarginModel: instruments.MarginNone, CorporateActionBehavior: instruments.CorporateActionsNone,
			Transferability: instruments.Transferable,
		}
		m.inst = inst.ID
		m.book, err = f.orders.OpenBook(ctx, tx, orders.OpenBookRequest{
			InstrumentID: inst.ID, Mode: gates.ModePaper, SpotTerms: &terms,
			MakerSchedule: schedule(makerBPS, "treasury-itest-maker"), TakerSchedule: schedule(takerBPS, "treasury-itest-taker"),
			SnapshotInterval: 16, Actor: systemActor,
		})
		return err
	}))
	f.mark(base.ID, price)
	return m
}

// TestIntegration_TheHouseIsChargedNothingAndItsCustomerCounterpartyIsBooked:
// the treasury charges no fee and no spread to anyone. Its house order on a
// fee-charging book pays no fee, while the customer it trades with is charged
// the book's fee and finds it through the fee ledger; its paper fills pay the
// external venue's spread and fee into HOUSE_FEES, a cost of the house book,
// which is no customer's charge and nothing the fee ledger books.
func TestIntegration_TheHouseIsChargedNothingAndItsCustomerCounterpartyIsBooked(t *testing.T) {
	f := newFixture(t)
	m := f.feeMarket("100.00", 10, 20)
	cust := f.customer(m.base.ID, units(10))
	b := f.register(econ.HouseMarketMaking, false)
	p := f.policy(m)
	p.PaperSpreadBPS, p.PaperFeeBPS = 25, 5
	f.setPolicy(b, p)
	f.fund(b, f.usd.ID, units(10_000))
	_, err := do(f, func(ctx context.Context, tx pgx.Tx) (CrossingDisclosure, error) {
		return f.svc.SetCrossingRule(ctx, tx, CrossingRuleRequest{InstrumentID: m.inst, Allow: true, Actor: operator,
			Reason:     "sandbox: let the house make markets for customers",
			Disclosure: "Simulated sandbox market: the platform's house book may trade against your simulated orders on this book."})
	})
	require.NoError(t, err)

	ask, err := f.customerOrder(m, cust, orders.SideSell, "100.00", units(1))
	require.NoError(t, err)
	require.Equal(t, orders.StatusOpen, ask.Order.Status)
	crossed := f.place(f.limit(b, m, econ.HouseMarketMaking, orders.SideBuy, "100.00", units(1)))
	require.NoError(t, crossed.Rejection)
	require.Len(t, crossed.Trades, 1)
	tr := crossed.Trades[0]
	assert.True(t, tr.TakerFee.IsZero(), "the house book pays no fee")
	assert.Equal(t, "100000", tr.MakerFee.String(), "the customer maker pays 10 bps of 100.00")

	var ids []feeledger.BookingID
	rows, err := testDB.Query(f.ctx, `SELECT id FROM fee_bookings WHERE journal_transaction_id = $1`, tr.JournalTransactionID)
	require.NoError(t, err)
	for rows.Next() {
		var bid feeledger.BookingID
		require.NoError(t, rows.Scan(&bid))
		ids = append(ids, bid)
	}
	rows.Close()
	require.Len(t, ids, 1, "one booking: the customer's")
	booking, err := f.fees.Booking(f.ctx, testDB, ids[0])
	require.NoError(t, err)
	assert.Equal(t, cust, booking.AccountID)
	assert.Equal(t, feeledger.BookingEmbedded, booking.BookingMode)
	assert.Equal(t, tr.MakerFee.String(), booking.Charges.Platform.String())
	require.NoError(t, f.fees.VerifyBooking(f.ctx, testDB, booking.ID))
	q, err := f.fees.Quote(f.ctx, testDB, booking.DisclosureID)
	require.NoError(t, err)
	assert.Equal(t, orders.BookScheduleKey(m.inst, orders.FeeMaker), q.ScheduleKey)

	// The house paper venue: the spread and the venue fee are the house's
	// cost, on HOUSE_FEES, and the fee ledger records nothing for it.
	disclosuresBefore := f.count(`SELECT count(*) FROM fee_quote_disclosures`)
	bookingsBefore := f.count(`SELECT count(*) FROM fee_bookings`)
	fill := f.paper(b, econ.HouseHedging, m.base.ID, orders.SideBuy, units(1))
	require.NoError(t, fill.Rejection)
	require.NotNil(t, fill.Fill)
	cost := fill.Fill.SpreadAmount.Add(fill.Fill.FeeAmount)
	assert.True(t, cost.IsPositive())
	assert.Equal(t, cost.String(), f.balance(b.account(ledger.CodeHouseFees, f.usd.ID)).String())
	assert.Equal(t, disclosuresBefore, f.count(`SELECT count(*) FROM fee_quote_disclosures`), "no disclosure: no customer is charged")
	assert.Equal(t, bookingsBefore, f.count(`SELECT count(*) FROM fee_bookings`), "no booking")
	assert.Zero(t, f.count(`SELECT count(*) FROM journal_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
		WHERE e.transaction_id = $1 AND la.code IN ('SPREAD_REVENUE','PLATFORM_FEE_REVENUE','FEES_SPREAD','FEES_PLATFORM')`,
		fill.Fill.JournalTransactionID), "no platform revenue and no customer charge in a house paper fill")
}
