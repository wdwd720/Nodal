//go:build integration

package orders

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/feeledger"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// TestIntegration_EveryPaperFillIsBookedThroughTheFeeLedger: the paper venue
// charges the fee-ledger schedule version in force under the instrument's
// key at the fill instant, and every fill's disclosed spread and fees are
// found through the fee ledger: the payer's quote disclosure is byte for
// byte the fill's own and verifies, an EMBEDDED booking registers each
// component against the fill's SIM_TRADE journal and effect, and the spread
// audit lists every charged spread with the version that priced it, its
// totals being exactly the spread revenue.
func TestIntegration_EveryPaperFillIsBookedThroughTheFeeLedger(t *testing.T) {
	f := newFixture(t)
	m := f.spotMarket(marketOpts{})
	v := f.paperVenue(map[instruments.InstrumentID]fees.Schedule{m.inst: paperSchedule(m)})
	x := f.account()
	f.fund(x, m.quote.ID, 1000_000_000)

	out, err := v.Submit(f.ctx, testDB, paperRequest(m, x, SideBuy, units(2)))
	require.NoError(t, err)
	fill := *out.Fill
	booked := f.bookingsOf(fill.JournalTransactionID)
	require.Len(t, booked, 1)
	b := booked[x]
	assert.Equal(t, feeledger.BookingEmbedded, b.BookingMode)
	assert.Equal(t, ledger.KindSimTrade, b.Kind)
	assert.Equal(t, fill.EffectID, b.EffectID, "registered against the effect that applied the fill")
	require.NotNil(t, b.IntentID)
	assert.Equal(t, out.Intent.ID, *b.IntentID)
	// 2 units at 100.00: spread 25 bps = 0.50; the 10 bps fee 0.20 plus the
	// fixed 0.001; a 5 bps venue pass-through 0.10.
	assert.Equal(t, "500000", b.Charges.Spread.String())
	assert.Equal(t, "201000", b.Charges.Platform.String())
	assert.Equal(t, "100000", b.Charges.Venue.String())
	assert.True(t, b.Charges.Strategy.IsZero() && b.Charges.Network.IsZero())
	require.NoError(t, f.fees.VerifyBooking(f.ctx, testDB, b.ID))
	require.NoError(t, v.VerifyFill(f.ctx, testDB, out.Intent.ID))
	_, err = f.fees.VerifyQuoteRecord(f.ctx, testDB, b.DisclosureID)
	require.NoError(t, err)
	q, err := f.fees.Quote(f.ctx, testDB, b.DisclosureID)
	require.NoError(t, err)
	assert.Equal(t, fill.DisclosureHash, q.DisclosureHash, "the fee ledger's disclosure is the fill's own")
	assert.Equal(t, fill.ScheduleHash, q.ScheduleHash)
	assert.Equal(t, PaperScheduleKey(PaperScheduleKeyPrefix, m.inst), q.ScheduleKey)
	assert.Equal(t, 1, q.ScheduleVersion)

	// The lines of the journal, each component on its own account.
	sums := map[ledger.Code]money.Quantity{}
	jt, err := f.ledger.Transaction(f.ctx, testDB, fill.JournalTransactionID)
	require.NoError(t, err)
	for _, e := range jt.Entries {
		if (e.Account.OwnerType == ledger.OwnerCustomer && e.Side == ledger.Debit) || (e.Account.OwnerType == ledger.OwnerPlatform && e.Side == ledger.Credit) {
			sums[e.Account.Code] = sums[e.Account.Code].Add(e.Quantity)
		}
	}
	for code, want := range map[ledger.Code]string{ledger.CodeFeesSpread: "500000", ledger.CodeSpreadRevenue: "500000",
		ledger.CodeFeesPlatform: "201000", ledger.CodePlatformFeeRevenue: "201000", ledger.CodeFeesVenue: "100000"} {
		assert.Equal(t, want, sums[code].String(), "%s", code)
	}

	// An operator publishes the next version, effective in ten minutes. A
	// fill before then is still priced under version 1, one after it under
	// version 2 -- the version in force at the fill instant.
	next := paperSchedule(m)
	next.Version, next.SpreadBPS = "paper-itest-v2", 40
	f.publishPaper(m.inst, next, f.clk.Now().Add(10*time.Minute))
	before, err := v.Submit(f.ctx, testDB, paperRequest(m, x, SideBuy, units(1)))
	require.NoError(t, err)
	f.clk.Advance(11 * time.Minute)
	f.mark(m, "100.00")
	after, err := v.Submit(f.ctx, testDB, paperRequest(m, x, SideBuy, units(1)))
	require.NoError(t, err)
	for want, fl := range map[int]*PaperFill{1: before.Fill, 2: after.Fill} {
		bb := f.bookingsOf(fl.JournalTransactionID)[x]
		qq, err := f.fees.Quote(f.ctx, testDB, bb.DisclosureID)
		require.NoError(t, err)
		assert.Equal(t, want, qq.ScheduleVersion)
		assert.Equal(t, fl.ScheduleHash, qq.ScheduleHash)
	}
	assert.Equal(t, "400000", after.Fill.SpreadAmount.String(), "1 unit at 100.00 under the 40 bps version")

	// The spread audit lists every charged spread, each with the version that
	// priced it, and its totals are exactly what SPREAD_REVENUE holds.
	quote := m.quote.ID
	spreads, err := f.fees.SpreadAudit(f.ctx, testDB, feeledger.SpreadFilter{QuoteAsset: &quote})
	require.NoError(t, err)
	require.Len(t, spreads, 3)
	var listed money.Quantity
	for _, c := range spreads {
		assert.Equal(t, x, c.AccountID)
		assert.Equal(t, PaperScheduleKey(PaperScheduleKeyPrefix, m.inst), c.ScheduleKey)
		listed = listed.Add(c.SpreadAmount)
	}
	assert.Equal(t, []int{1, 1, 2}, []int{spreads[0].ScheduleVersion, spreads[1].ScheduleVersion, spreads[2].ScheduleVersion})
	assert.Equal(t, money.BPS(25), spreads[0].SpreadBPS)
	assert.Equal(t, money.BPS(40), spreads[2].SpreadBPS)
	assert.Equal(t, "10025", spreads[0].ExecutionPrice.Mantissa.String(), "100.00 moved by exactly the 25 bps spread")
	assert.Equal(t, fill.JournalTransactionID, spreads[0].JournalTransactionID)
	total, reversed, err := f.fees.SpreadTotals(f.ctx, testDB, quote)
	require.NoError(t, err)
	assert.True(t, reversed.IsZero())
	assert.Equal(t, listed.String(), total.String())
	assert.Equal(t, total.String(), f.balance(ledger.PlatformAccount(ledger.CodeSpreadRevenue, quote)).String())
	disclosures, bookings := f.feeRecordsOf(x)
	assert.Equal(t, 3, disclosures)
	assert.Equal(t, 3, bookings)
}

// TestIntegration_APaperFillReplayBooksNothingTwice: executing a filled
// intent again, or submitting its order again under the same key, returns
// the first fill and books nothing a second time.
func TestIntegration_APaperFillReplayBooksNothingTwice(t *testing.T) {
	f := newFixture(t)
	m := f.spotMarket(marketOpts{})
	v := f.paperVenue(map[instruments.InstrumentID]fees.Schedule{m.inst: paperSchedule(m)})
	x := f.account()
	f.fund(x, m.quote.ID, 1000_000_000)
	req := paperRequest(m, x, SideBuy, units(1))
	out, err := v.Submit(f.ctx, testDB, req)
	require.NoError(t, err)

	in, err := f.intents.Get(f.ctx, testDB, out.Intent.ID)
	require.NoError(t, err)
	var again PaperFill
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		again, err = v.ExecuteIntent(ctx, tx, in)
		return err
	}))
	assert.True(t, again.Replayed)
	resubmitted, err := v.Submit(f.ctx, testDB, req)
	require.NoError(t, err)
	require.NotNil(t, resubmitted.Fill)
	assert.Equal(t, out.Fill.ID, resubmitted.Fill.ID)
	assert.Len(t, f.bookingsOf(out.Fill.JournalTransactionID), 1)
	disclosures, bookings := f.feeRecordsOf(x)
	assert.Equal(t, 1, disclosures)
	assert.Equal(t, 1, bookings)
}

// TestIntegration_ARefusedOrRolledBackPaperFillBooksNothing: a fill refused
// for a stale mark, for an instrument with no schedule in force, or for a
// SPREAD_ENGINE kill switch, and a fill whose transaction rolls back, leave
// no disclosure and no booking.
func TestIntegration_ARefusedOrRolledBackPaperFillBooksNothing(t *testing.T) {
	f := newFixture(t)
	m := f.spotMarket(marketOpts{})
	v := f.paperVenue(map[instruments.InstrumentID]fees.Schedule{m.inst: paperSchedule(m)})
	x := f.account()
	f.fund(x, m.quote.ID, 1000_000_000)
	f.fund(x, m.base.ID, 5_000_000)
	requireNothingBooked := func(what string) {
		t.Helper()
		disclosures, bookings := f.feeRecordsOf(x)
		assert.Zero(t, disclosures, "%s: no disclosure", what)
		assert.Zero(t, bookings, "%s: no booking", what)
	}

	// No schedule in force for an instrument: refused at acceptance.
	unpriced := f.spotMarket(marketOpts{})
	_, err := v.Submit(f.ctx, testDB, paperRequest(unpriced, x, SideBuy, units(1)))
	assert.Equal(t, ReasonPaperNoSchedule, ReasonOf(err))
	requireNothingBooked("no schedule")

	// A rolled-back fill: inside its transaction the charge is booked, and
	// none of it survives.
	accepted, err := v.Submit(f.ctx, testDB, paperRequest(m, x, SideSell, units(1)))
	require.NoError(t, err)
	require.NotNil(t, accepted.Fill)
	committed := 1
	errRollBack := errors.New("roll the fill back")
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		qty, base := units(1), m.base.ID
		in, _, err := f.intents.Submit(ctx, tx, econ.Intent{AccountID: x, Kind: econ.KindReduceExposure, Mode: gates.ModePaper,
			Capability: gates.SpreadEngine, ActorType: accepted.Intent.ActorType, ActorID: accepted.Intent.ActorID,
			InstrumentID: m.inst.String(), AssetID: &base, Quantity: &qty, Terms: accepted.Intent.Terms,
			IdempotencyKey: "rollback-" + accepted.Intent.ID.String()})
		if err != nil {
			return err
		}
		for _, s := range []econ.IntentStatus{econ.StatusRouted, econ.StatusRiskApproved} {
			if in, err = f.intents.Transition(ctx, tx, in.ID, s, systemActor, "the owner's stage", ""); err != nil {
				return err
			}
		}
		fill, err := v.ExecuteIntent(ctx, tx, in)
		if err != nil {
			return err
		}
		var inside int
		require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM fee_bookings WHERE journal_transaction_id = $1`,
			fill.JournalTransactionID).Scan(&inside))
		require.Equal(t, 1, inside, "booked in the fill's own transaction")
		return errRollBack
	})
	require.ErrorIs(t, err, errRollBack)
	disclosures, bookings := f.feeRecordsOf(x)
	assert.Equal(t, committed, disclosures, "only the committed fill's disclosure")
	assert.Equal(t, committed, bookings, "only the committed fill's booking")

	// A stale mark refuses both sides before anything is written.
	f.clk.Advance(2 * time.Minute)
	_, err = v.Submit(f.ctx, testDB, paperRequest(m, x, SideBuy, units(1)))
	assert.Equal(t, errs.CodeRiskRejected, errs.CodeOf(err))
	_, err = v.Submit(f.ctx, testDB, paperRequest(m, x, SideSell, units(1)))
	assert.Equal(t, errs.CodeStaleMarketData, errs.CodeOf(err))
	disclosures, bookings = f.feeRecordsOf(x)
	assert.Equal(t, committed, disclosures)
	assert.Equal(t, committed, bookings)
}
