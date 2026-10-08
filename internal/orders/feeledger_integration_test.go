//go:build integration

package orders

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/feeledger"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
)

// bookingsOf returns the fee bookings registered against a journal, by payer.
func (f *fixture) bookingsOf(journal ledger.TransactionID) map[accounts.AccountID]feeledger.Booking {
	f.t.Helper()
	rows, err := testDB.Query(f.ctx, `SELECT id FROM fee_bookings WHERE journal_transaction_id = $1`, journal)
	require.NoError(f.t, err)
	var ids []feeledger.BookingID
	for rows.Next() {
		var bid feeledger.BookingID
		require.NoError(f.t, rows.Scan(&bid))
		ids = append(ids, bid)
	}
	rows.Close()
	require.NoError(f.t, rows.Err())
	out := map[accounts.AccountID]feeledger.Booking{}
	for _, bid := range ids {
		b, err := f.fees.Booking(f.ctx, testDB, bid)
		require.NoError(f.t, err)
		out[b.AccountID] = b
	}
	return out
}

// feeRecordsOf counts an account's quote disclosures and bookings.
func (f *fixture) feeRecordsOf(acct accounts.AccountID) (disclosures, bookings int) {
	f.t.Helper()
	return f.count(`SELECT count(*) FROM fee_quote_disclosures WHERE account_id = $1`, acct),
		f.count(`SELECT count(*) FROM fee_bookings WHERE account_id = $1`, acct)
}

// strategyLimit is a GTC limit order a strategy originated, whose strategy
// is approved and permitted.
func (f *fixture) strategyLimit(m spotMarket, acct accounts.AccountID, side Side, price string, qty money.Quantity) OrderRequest {
	r := f.limit(m, acct, side, price, qty)
	r.StrategyRef = "strategy-itest"
	r.Risk = RiskContext{Strategy: &risk.StrategySnapshot{Approved: true, ManifestPermits: true}}
	return r
}

// TestIntegration_EveryInternalBookFeeIsBookedThroughTheFeeLedger: the book's
// maker and taker schedules are the fee-ledger versions in force from the
// instant it opens, and each customer party of a fill finds its fee in the
// fee ledger: an EMBEDDED booking against the trade's own SIM_TRADE journal
// and effect, each component on its own pair, priced by a quote disclosure of
// its own that verifies against the very schedule the trade names. The book
// charges no spread, and the spread audit says so.
func TestIntegration_EveryInternalBookFeeIsBookedThroughTheFeeLedger(t *testing.T) {
	f := newFixture(t)
	m := f.spotMarket(marketOpts{makerBPS: 10, takerBPS: 20, strategyBPS: 5})
	for role, hash := range map[FeeRole]string{FeeMaker: m.book.MakerScheduleHash, FeeTaker: m.book.TakerScheduleHash} {
		p, err := f.fees.EffectiveSchedule(f.ctx, testDB, BookScheduleKey(m.inst, role), f.clk.Now())
		require.NoError(t, err, "the %s schedule is published when the book opens", role)
		assert.Equal(t, 1, p.Version)
		assert.Equal(t, hash, p.Hash, "the version in force is the schedule the book froze")
	}
	accts := f.fundedAccounts(m, 2)
	maker, taker := accts[0], accts[1]
	rest := f.submit(f.limit(m, maker, SideSell, "100.00", units(2)))
	out := f.submit(f.strategyLimit(m, taker, SideBuy, "100.00", units(2)))
	require.Len(t, out.Trades, 1)
	tr := out.Trades[0]
	// 2 units at 100.00, a notional of 200: the maker pays 10 bps (0.20), the
	// strategy-originated taker 20 bps (0.40) and the 5 bps strategy fee (0.10).
	assert.Equal(t, "200000", tr.MakerFee.String())
	assert.Equal(t, "500000", tr.TakerFee.String())

	booked := f.bookingsOf(tr.JournalTransactionID)
	require.Len(t, booked, 2, "one booking per paying party, against the trade's journal")
	for _, c := range []struct {
		acct               accounts.AccountID
		role               FeeRole
		side               fees.Side
		platform, strategy string
		hash               string
		intent             econ.IntentID
	}{
		{maker, FeeMaker, fees.SideSell, "200000", "0", m.book.MakerScheduleHash, rest.Intent.ID},
		{taker, FeeTaker, fees.SideBuy, "400000", "100000", m.book.TakerScheduleHash, out.Intent.ID},
	} {
		b, ok := booked[c.acct]
		require.True(t, ok, "%s has a booking", c.role)
		assert.Equal(t, feeledger.BookingEmbedded, b.BookingMode)
		assert.Equal(t, feeledger.BookingBooked, b.Status)
		assert.Equal(t, ledger.KindSimTrade, b.Kind)
		assert.Equal(t, gates.ModePaper, b.Mode)
		assert.Equal(t, tr.EffectID, b.EffectID, "registered against the effect that applied the trade")
		require.NotNil(t, b.IntentID)
		assert.Equal(t, c.intent, *b.IntentID)
		assert.Equal(t, c.platform, b.Charges.Platform.String(), "%s platform fee", c.role)
		assert.Equal(t, c.strategy, b.Charges.Strategy.String(), "%s strategy fee", c.role)
		assert.True(t, b.Charges.Spread.IsZero() && b.Charges.Venue.IsZero() && b.Charges.Network.IsZero(), "no spread, no pass-through")
		require.NoError(t, f.fees.VerifyBooking(f.ctx, testDB, b.ID), "the journal carries exactly the disclosed charges")
		d, err := f.fees.VerifyQuoteRecord(f.ctx, testDB, b.DisclosureID)
		require.NoError(t, err, "the disclosure recomputes from its schedule")
		q, err := f.fees.Quote(f.ctx, testDB, b.DisclosureID)
		require.NoError(t, err)
		assert.Equal(t, BookScheduleKey(m.inst, c.role), q.ScheduleKey)
		assert.Equal(t, 1, q.ScheduleVersion)
		assert.Equal(t, c.hash, q.ScheduleHash, "priced under the schedule the trade names")
		assert.Equal(t, econ.SubsystemOrders, q.Subsystem)
		require.NotNil(t, q.AccountID)
		assert.Equal(t, c.acct, *q.AccountID, "disclosed to its payer")
		assert.Equal(t, c.side, d.Input.Side)
		assert.Equal(t, tr.BaseQuantity.String(), d.Input.BaseQty.String())
		assert.Equal(t, tr.Price.Mantissa.String(), d.ReferencePrice.Mantissa.String(), "at the fill price")
		assert.Equal(t, tr.Price.Scale, d.ReferencePrice.Scale)
		assert.Equal(t, c.acct == taker, d.Input.StrategyOriginated)
		assert.Equal(t, tr.QuoteNotional.String(), d.NotionalAtReference.String())
	}

	// The lines: the platform fee and the strategy fee each on its own pair,
	// and every unit of this quote asset's fee revenue is a booked fee.
	jt, err := f.ledger.Transaction(f.ctx, testDB, tr.JournalTransactionID)
	require.NoError(t, err)
	var takerDebits []string
	var revenue money.Quantity
	for _, e := range jt.Entries {
		a := e.Account
		if a.OwnerType == ledger.OwnerCustomer && a.OwnerID == taker.String() && a.Code == ledger.CodeFeesPlatform && e.Side == ledger.Debit {
			takerDebits = append(takerDebits, e.Quantity.String())
		}
		if a.OwnerType == ledger.OwnerPlatform && a.Code == ledger.CodePlatformFeeRevenue && e.Side == ledger.Credit {
			revenue = revenue.Add(e.Quantity)
		}
	}
	assert.ElementsMatch(t, []string{"400000", "100000"}, takerDebits)
	assert.Equal(t, "700000", revenue.String())
	assert.Equal(t, "700000", f.balance(ledger.PlatformAccount(ledger.CodePlatformFeeRevenue, m.quote.ID)).String())

	// Each customer finds its own charge through the fee ledger.
	for _, acct := range []accounts.AccountID{maker, taker} {
		disclosures, bookings := f.feeRecordsOf(acct)
		assert.Equal(t, 1, disclosures)
		assert.Equal(t, 1, bookings)
	}

	// The internal book charges no spread, and the spread audit agrees.
	quote := m.quote.ID
	spreads, err := f.fees.SpreadAudit(f.ctx, testDB, feeledger.SpreadFilter{QuoteAsset: &quote, IncludeReversed: true})
	require.NoError(t, err)
	assert.Empty(t, spreads)
	spread, reversed, err := f.fees.SpreadTotals(f.ctx, testDB, quote)
	require.NoError(t, err)
	assert.True(t, spread.IsZero() && reversed.IsZero())
	assert.True(t, f.balance(ledger.PlatformAccount(ledger.CodeSpreadRevenue, quote)).IsZero())
}

// TestIntegration_AReplayedFillBooksItsFeesOnce: submitting the same order
// again, resuming it, and replaying the fee ledger's own calls for the fill
// return what was recorded -- the same disclosures and the same bookings --
// and book nothing a second time.
func TestIntegration_AReplayedFillBooksItsFeesOnce(t *testing.T) {
	f := newFixture(t)
	m := f.spotMarket(marketOpts{makerBPS: 10, takerBPS: 20})
	accts := f.fundedAccounts(m, 2)
	f.submit(f.limit(m, accts[0], SideSell, "100.00", units(1)))
	req := f.limit(m, accts[1], SideBuy, "100.00", units(1))
	first := f.submit(req)
	require.Len(t, first.Trades, 1)
	tr := first.Trades[0]
	booked := f.bookingsOf(tr.JournalTransactionID)
	require.Len(t, booked, 2)

	again := f.submit(req)
	assert.Equal(t, first.Order.ID, again.Order.ID, "the same key is the same order")
	assert.Empty(t, again.Trades)
	_, err := f.pipe.Resume(f.ctx, first.Order.ID)
	require.NoError(t, err)

	for acct, b := range booked {
		q, err := f.fees.Quote(f.ctx, testDB, b.DisclosureID)
		require.NoError(t, err)
		var replayQ feeledger.StoredQuote
		var replayB feeledger.Booking
		require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			base := m.base.ID
			var err error
			if replayQ, err = f.fees.RecordQuote(ctx, tx, feeledger.QuoteRequest{
				ScheduleKey: q.ScheduleKey, Input: q.Disclosure.Input.QuoteInput(), Subsystem: econ.SubsystemOrders,
				IdempotencyKey: q.IdempotencyKey, AccountID: q.AccountID, BaseAsset: &base, Mode: gates.ModePaper, Actor: systemActor,
			}); err != nil {
				return err
			}
			replayB, err = f.fees.RecordEmbedded(ctx, tx, feeledger.EmbeddedRequest{
				DisclosureID: b.DisclosureID, AccountID: acct, JournalTransactionID: b.JournalTransactionID, EffectID: b.EffectID,
				IntentID: b.IntentID, Mode: gates.ModePaper, Actor: systemActor,
			})
			return err
		}))
		assert.Equal(t, b.DisclosureID, replayQ.ID, "a replayed quote is the stored disclosure")
		assert.Equal(t, b.ID, replayB.ID, "a replayed registration is the stored booking")
	}
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM book_trades WHERE book_id = $1`, m.book.ID))
	assert.Equal(t, 2, f.count(`SELECT count(*) FROM fee_bookings WHERE journal_transaction_id = $1`, tr.JournalTransactionID))
	assert.Equal(t, 2, f.count(`SELECT count(*) FROM fee_quote_disclosures WHERE subsystem = 'ORDERS' AND idempotency_key LIKE $1`,
		tradeKey(m.book.ID, tr.TradeSeq)+":%"))
	for _, acct := range accts {
		disclosures, bookings := f.feeRecordsOf(acct)
		assert.Equal(t, 1, disclosures)
		assert.Equal(t, 1, bookings)
	}
}

// TestIntegration_ConcurrentExecutionsOfAnOrderBookItsFeesOnce: one reserved
// order executed by several transactions at once trades once and books each
// party's fee once: the book's lock serializes them, and every execution
// after the first finds the order already on the book.
func TestIntegration_ConcurrentExecutionsOfAnOrderBookItsFeesOnce(t *testing.T) {
	f := newFixture(t)
	m := f.spotMarket(marketOpts{makerBPS: 10, takerBPS: 20})
	accts := f.fundedAccounts(m, 2)
	f.submit(f.limit(m, accts[0], SideSell, "100.00", units(1)))
	var staged StageOutcome
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		staged, err = f.svc.Accept(ctx, tx, f.limit(m, accts[1], SideBuy, "100.00", units(1)))
		return err
	}))
	for _, stage := range []func(context.Context, pgx.Tx, OrderID) (StageOutcome, error){f.svc.Approve, f.svc.Reserve} {
		require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error { _, err := stage(ctx, tx, staged.Order.ID); return err }))
	}
	const n = 6
	var wg sync.WaitGroup
	failures := make([]error, n)
	trades := make([]int, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			failures[i] = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
				out, err := f.svc.Execute(ctx, tx, staged.Order.ID)
				trades[i] = len(out.Trades)
				return err
			})
		}(i)
	}
	close(start)
	wg.Wait()
	total := 0
	for i := range failures {
		require.NoError(t, failures[i])
		total += trades[i]
	}
	assert.Equal(t, 1, total, "the order traded once")
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM book_trades WHERE taker_order_id = $1`, staged.Order.ID))
	assert.Equal(t, 2, f.count(`SELECT count(*) FROM fee_bookings b JOIN book_trades t ON t.journal_transaction_id = b.journal_transaction_id
		WHERE t.taker_order_id = $1`, staged.Order.ID))
	for _, acct := range accts {
		disclosures, bookings := f.feeRecordsOf(acct)
		assert.Equal(t, 1, disclosures)
		assert.Equal(t, 1, bookings)
	}
}

// TestIntegration_ARefusedOrRolledBackFillBooksNothing: an order refused at
// its capability check (a SPREAD_ENGINE kill switch on a book that charges
// fees), for want of funds, or because the book's schedule is no longer the
// version in force, and a fill whose transaction rolls back, leave no
// disclosure and no booking. A book that charges nothing does not need the
// spread engine at all.
func TestIntegration_ARefusedOrRolledBackFillBooksNothing(t *testing.T) {
	f := newFixture(t)
	m := f.spotMarket(marketOpts{makerBPS: 10, takerBPS: 20})
	accts := f.fundedAccounts(m, 2)
	maker, taker := accts[0], accts[1]
	f.submit(f.limit(m, maker, SideSell, "100.00", units(1)))
	trades := func() int { return f.count(`SELECT count(*) FROM book_trades WHERE book_id = $1`, m.book.ID) }
	requireNothingBooked := func(what string, acct accounts.AccountID) {
		t.Helper()
		disclosures, bookings := f.feeRecordsOf(acct)
		assert.Zero(t, disclosures, "%s: no disclosure", what)
		assert.Zero(t, bookings, "%s: no booking", what)
	}

	// The spread engine switched off: a fee-charging book refuses the order
	// at its capability check, and nothing reaches the book.
	kills := fixedKills{snap: killswitch.Snapshot{Switches: []killswitch.Switch{
		{Kind: killswitch.CapabilityDisable, ScopeID: string(gates.SpreadEngine), Active: true}}}}
	checker, err := gates.NewChecker("TEST", func(gates.Capability) bool { return true }, f.clk)
	require.NoError(t, err)
	switched, err := NewService(Config{
		Clock: f.clk, Ledger: f.ledger, Capital: f.capital, Intents: f.intents, Effects: f.effects,
		Authorizer: econ.NewAuthorizer(checker, kills), Registry: f.registry, Risk: f.risk, RiskPolicy: risk.DefaultAdvancedPolicy(),
		Router: f.router, Instruments: f.instr, Assets: f.assets, Prices: f.prices, KillSwitches: kills, FeeLedger: f.fees,
	})
	require.NoError(t, err)
	killed, err := NewPipeline(switched, testDB)
	require.NoError(t, err)
	out, err := killed.Submit(f.ctx, f.limit(m, taker, SideBuy, "100.00", units(1)))
	assert.Equal(t, errs.CodeKillSwitchActive, errs.CodeOf(err))
	assert.Equal(t, StatusRejected, out.Order.Status)
	assert.Equal(t, ReasonCapability, out.Order.RejectReason)
	assert.Zero(t, trades())
	requireNothingBooked("refused by the switch", taker)
	// A book that charges nothing trades on: it needs no spread engine.
	free := f.spotMarket(marketOpts{})
	freeAccts := f.fundedAccounts(free, 2)
	_, err = killed.Submit(f.ctx, f.limit(free, freeAccts[0], SideSell, "100.00", units(1)))
	require.NoError(t, err)
	freeFill, err := killed.Submit(f.ctx, f.limit(free, freeAccts[1], SideBuy, "100.00", units(1)))
	require.NoError(t, err)
	require.Len(t, freeFill.Trades, 1)
	assert.Empty(t, f.bookingsOf(freeFill.Trades[0].JournalTransactionID), "a fill that charges nothing books nothing")
	for _, acct := range freeAccts {
		requireNothingBooked("a free book", acct)
	}

	// No funds: refused by Risk Kernel V2's balance check before any fill.
	poor := f.account()
	out, err = f.pipe.Submit(f.ctx, f.limit(m, poor, SideBuy, "100.00", units(1)))
	require.Error(t, err)
	assert.Equal(t, StatusRejected, out.Order.Status)
	assert.Contains(t, out.Order.RejectReason, risk.ReasonInsufficientBalance)
	requireNothingBooked("refused for want of funds", poor)

	// A fill in a transaction that rolls back: inside it the fees are
	// booked, and none of it survives.
	errRollBack := errors.New("roll the fill back")
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		out, err := f.svc.submitAtomic(ctx, tx, f.limit(m, taker, SideBuy, "100.00", units(1)))
		if err != nil {
			return err
		}
		require.Len(t, out.Trades, 1)
		var inside int
		require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM fee_bookings WHERE journal_transaction_id = $1`,
			out.Trades[0].JournalTransactionID).Scan(&inside))
		require.Equal(t, 2, inside, "booked in the fill's own transaction")
		return errRollBack
	})
	require.ErrorIs(t, err, errRollBack)
	assert.Zero(t, trades())
	requireNothingBooked("rolled back", taker)
	requireNothingBooked("rolled back", maker)

	// An operator publishes another taker schedule under the book's key: the
	// book's own is no longer in force, so its fills could not be booked and
	// an order is refused before it reaches the book.
	other := bookSchedule(m.quote.ID, 30)
	other.Version = "operator-taker-v2"
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.fees.PublishSchedule(ctx, tx, feeledger.PublishRequest{Key: BookScheduleKey(m.inst, FeeTaker), Schedule: other,
			EffectiveFrom: f.clk.Now().Add(time.Second), Reason: "an operator re-prices the book's key", Actor: paperOperator})
		return err
	}))
	f.clk.Advance(2 * time.Second)
	f.mark(m, "100.00")
	out, err = f.pipe.Submit(f.ctx, f.limit(m, taker, SideBuy, "100.00", units(1)))
	require.Error(t, err)
	assert.Equal(t, ReasonFeeScheduleNotInForce, ReasonOf(err))
	assert.Equal(t, StatusRejected, out.Order.Status)
	assert.Equal(t, ReasonFeeScheduleNotInForce, out.Order.RejectReason)
	assert.Zero(t, trades())
	requireNothingBooked("the schedule is not in force", taker)
	assert.Equal(t, StatusOpen, f.orderView(f.restingOrderOf(m, maker)).Status, "the maker still rests")
}

// restingOrderOf is an account's one working order on a book.
func (f *fixture) restingOrderOf(m spotMarket, acct accounts.AccountID) OrderID {
	f.t.Helper()
	var oid OrderID
	require.NoError(f.t, testDB.QueryRow(f.ctx, `SELECT id FROM book_orders WHERE book_id = $1 AND owner_id = $2
		AND status IN ('OPEN','PARTIALLY_FILLED')`, m.book.ID, acct).Scan(&oid))
	return oid
}

// TestIntegration_TheDatabaseRefusesAnUnbookedFee: a trade row or a paper
// fill that charges a fee the fee ledger did not book -- written around the
// services, with a journal and an effect of its own -- is refused at COMMIT
// (FW001), whoever writes it.
func TestIntegration_TheDatabaseRefusesAnUnbookedFee(t *testing.T) {
	f := newFixture(t)
	tm := f.tradedMarket()
	var maker, taker accounts.AccountID
	var makerFee, takerFee money.Quantity
	var mf, tf string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT maker_owner_id, taker_owner_id, maker_fee::text, taker_fee::text FROM book_trades
		WHERE book_id = $1 ORDER BY trade_seq LIMIT 1`, tm.book.ID).Scan(&maker, &taker, &mf, &tf))
	var err error
	makerFee, err = money.ScanQuantity(mf)
	require.NoError(t, err)
	takerFee, err = money.ScanQuantity(tf)
	require.NoError(t, err)
	require.True(t, makerFee.IsPositive() && takerFee.IsPositive())

	// journalAndEffect posts the fee legs of a fill and applies them by an
	// effect, as a fill would -- without registering anything in the fee
	// ledger.
	journalAndEffect := func(ctx context.Context, tx pgx.Tx, legs map[accounts.AccountID]money.Quantity) (ledger.TransactionID, econ.EffectID, error) {
		var entries []ledger.Entry
		for acct, fee := range legs {
			entries = append(entries, feeledger.ChargeEntries(acct, feeledger.Charges{QuoteAsset: tm.m.quote.ID, Platform: fee})...)
		}
		ref := uuid.NewString()
		posted, err := f.ledger.Post(ctx, tx, ledger.Posting{Kind: ledger.KindSimTrade, IdempotencyKey: "bypass:" + ref,
			Reference: ledger.FinancialEventReference{Type: "bypass", ID: ref}, EffectiveAt: f.clk.Now(), Entries: entries})
		if err != nil {
			return ledger.TransactionID{}, econ.EffectID{}, err
		}
		eff, _, err := f.effects.Begin(ctx, tx, econ.BeginRequest{Key: "bypass:" + ref, Subsystem: econ.SubsystemOrders, Kind: "SIM_TRADE",
			Request: map[string]string{"ref": ref}, PostsLedger: true, Actor: systemActor})
		if err != nil {
			return ledger.TransactionID{}, econ.EffectID{}, err
		}
		if _, err := f.effects.Apply(ctx, tx, eff.ID, econ.ApplyRequest{JournalTransactionID: &posted.TransactionID, Actor: systemActor}); err != nil {
			return ledger.TransactionID{}, econ.EffectID{}, err
		}
		return posted.TransactionID, eff.ID, nil
	}

	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		journal, effect, err := journalAndEffect(ctx, tx, map[accounts.AccountID]money.Quantity{maker: makerFee, taker: takerFee})
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO book_trades (id, book_id, instrument_id, trade_seq, command_seq, event_seq, price_ticks,
			qty_lots, maker_order_id, taker_order_id, maker_owner_type, maker_owner_id, taker_owner_type, taker_owner_id, aggressor_side,
			base_asset_id, quote_asset_id, base_quantity, quote_notional, maker_fee, taker_fee, maker_schedule_hash, taker_schedule_hash,
			crossing_rule_version, journal_transaction_id, effect_id, evidence_ref, evidence_sha256)
			SELECT $1, book_id, instrument_id, trade_seq + 1000, command_seq, event_seq, price_ticks, qty_lots, maker_order_id,
			taker_order_id, maker_owner_type, maker_owner_id, taker_owner_type, taker_owner_id, aggressor_side, base_asset_id,
			quote_asset_id, base_quantity, quote_notional, maker_fee, taker_fee, maker_schedule_hash, taker_schedule_hash,
			crossing_rule_version, $2, $3, evidence_ref, evidence_sha256
			FROM book_trades WHERE book_id = $4 ORDER BY trade_seq LIMIT 1`, uuid.New(), journal, effect, tm.book.ID)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "FW001", db.SQLState(err), "a trade fee the fee ledger did not book: %v", err)

	// A paper fill that charges a spread and fees nobody booked.
	v := f.paperVenue(map[instruments.InstrumentID]fees.Schedule{tm.m.inst: paperSchedule(tm.m)})
	x := f.account()
	f.fund(x, tm.m.quote.ID, 1000_000_000)
	paper, err := v.Submit(f.ctx, testDB, paperRequest(tm.m, x, SideBuy, units(1)))
	require.NoError(t, err)
	require.NotNil(t, paper.Fill)
	require.Len(t, f.bookingsOf(paper.Fill.JournalTransactionID), 1, "the venue's own fill is booked")
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		qty := units(1)
		base := tm.m.base.ID
		in, _, err := f.intents.Submit(ctx, tx, econ.Intent{AccountID: x, Kind: econ.KindAcquireExposure, Mode: gates.ModePaper,
			Capability: gates.SpreadEngine, ActorType: paper.Intent.ActorType, ActorID: paper.Intent.ActorID,
			InstrumentID: tm.m.inst.String(), AssetID: &base, Quantity: &qty, Terms: paper.Intent.Terms,
			IdempotencyKey: "bypass-" + uuid.NewString()})
		if err != nil {
			return err
		}
		journal, effect, err := journalAndEffect(ctx, tx, map[accounts.AccountID]money.Quantity{x: paper.Fill.PlatformFeeAmount})
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO paper_fills (id, intent_id, account_id, instrument_id, side, base_asset_id, quote_asset_id,
			base_quantity, notional_at_reference, spread_amount, platform_fee_amount, venue_fee_amount, network_fee_amount, total_quote,
			schedule_version, schedule_hash, schedule, disclosure, disclosure_hash, reservation_id, journal_transaction_id, effect_id,
			evidence_ref, evidence_sha256)
			SELECT $1, $2, account_id, instrument_id, side, base_asset_id, quote_asset_id, base_quantity, notional_at_reference,
			spread_amount, platform_fee_amount, venue_fee_amount, network_fee_amount, total_quote, schedule_version, schedule_hash,
			schedule, disclosure, disclosure_hash, reservation_id, $3, $4, evidence_ref, evidence_sha256
			FROM paper_fills WHERE id = $5`, uuid.New(), in.ID, journal, effect, paper.Fill.ID)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "FW001", db.SQLState(err), "a paper charge the fee ledger did not book: %v", err)
}
