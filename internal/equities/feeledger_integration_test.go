//go:build integration

package equities

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

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
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
)

// The fee ledger's view of equity fills (feebooking.go, migration 00764).

// bookingsOf returns the fee-ledger bookings registered against a journal,
// by payer.
func (w *world) bookingsOf(journal ledger.TransactionID) map[accounts.AccountID]feeledger.Booking {
	w.t.Helper()
	rows, err := testDB.Query(w.ctx, `SELECT id FROM fee_bookings WHERE journal_transaction_id = $1`, journal)
	require.NoError(w.t, err)
	var ids []feeledger.BookingID
	for rows.Next() {
		var bid feeledger.BookingID
		require.NoError(w.t, rows.Scan(&bid))
		ids = append(ids, bid)
	}
	rows.Close()
	require.NoError(w.t, rows.Err())
	out := map[accounts.AccountID]feeledger.Booking{}
	for _, bid := range ids {
		b, err := w.feeLedger.Booking(w.ctx, testDB, bid)
		require.NoError(w.t, err)
		out[b.AccountID] = b
	}
	return out
}

// feeRecordsOf counts an account's fee-ledger disclosures and bookings.
func (w *world) feeRecordsOf(acct accounts.AccountID) (disclosures, bookings int) {
	w.t.Helper()
	return w.count(`SELECT count(*) FROM fee_quote_disclosures WHERE account_id = $1`, acct),
		w.count(`SELECT count(*) FROM fee_bookings WHERE account_id = $1`, acct)
}

// fillOf returns a filled order's fill.
func (w *world) fillOf(o Order) Fill {
	w.t.Helper()
	require.Equal(w.t, OrderFilled, o.Status)
	f, ok, err := w.svc.FillForOrder(w.ctx, testDB, o.ID)
	require.NoError(w.t, err)
	require.True(w.t, ok)
	return f
}

// scheduleKey is the fee-ledger key of the world's schedule.
func (w *world) scheduleKey() (string, string) {
	w.t.Helper()
	hash, err := w.schedule.Hash()
	require.NoError(w.t, err)
	return FeeScheduleKey(w.cash.ID, hash), hash
}

// withZeroSchedule prices the world's dollar under a schedule that charges
// nothing.
func withZeroSchedule() worldOption {
	return func(c *Config) {
		s := c.Schedules[0]
		z := fees.ZeroSchedule(s.QuoteAsset, s.QuoteDecimals)
		z.Version = "equities-itest-zero-v1"
		c.Schedules = []fees.Schedule{z}
	}
}

// switchboard is a kill-switch register a test flips.
type switchboard struct {
	mu   sync.Mutex
	snap killswitch.Snapshot
}

func (s *switchboard) Snapshot(context.Context, db.Querier) (killswitch.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return killswitch.Snapshot{Switches: append([]killswitch.Switch(nil), s.snap.Switches...)}, nil
}

func (s *switchboard) set(sw ...killswitch.Switch) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap = killswitch.Snapshot{Switches: append([]killswitch.Switch(nil), sw...)}
}

var spreadEngineOff = killswitch.Switch{Kind: killswitch.CapabilityDisable, ScopeID: string(gates.SpreadEngine), Active: true}

// TestIntegration_EveryEquityFillIsBookedThroughTheFeeLedger: a buy, a sell
// and a strategy-originated buy each book their spread and fees through the
// fee ledger in the transaction that applies the fill. The world's schedule
// is the version in force under its equities key; the payer's disclosure is
// byte for byte the fill's own and re-verifies; one EMBEDDED booking per fill
// registers every component against the fill's SIM_TRADE journal and the
// effect that applied it, exactly the amounts the fill records; the journal
// carries every component on its own pair (the strategy fee apart from the
// platform fee); and a seller's charges come out of the proceeds, so its
// WALLET does not move.
func TestIntegration_EveryEquityFillIsBookedThroughTheFeeLedger(t *testing.T) {
	w := newWorld(t, withStrategies(fakeStrategies{snap: risk.StrategySnapshot{Approved: true, ManifestPermits: true}}))
	acct := w.account()
	l := w.list(1, 1)
	w.fund(acct, w.cash.ID, 10_000_000_000)
	w.fund(acct, l.ShareAsset.ID, 10_000_000)
	w.mark(l, 15_000)
	key, hash := w.scheduleKey()

	buy := w.trade(acct, l, market(SideBuy, 2_500_000))
	walletBeforeSell := w.cust(acct, ledger.CodeWallet, w.cash.ID)
	sell := w.trade(acct, l, market(SideSell, 4_000_000))
	assert.Equal(t, walletBeforeSell.String(), w.cust(acct, ledger.CodeWallet, w.cash.ID).String(),
		"a seller's charges come out of the proceeds: its WALLET does not move")
	req := w.request(acct, l, market(SideBuy, 1_000_000))
	req.StrategyRef = "strategy-" + uuid.NewString()
	strategy := w.place(w.accept(w.submit(req).ID))

	for _, tc := range []struct {
		name                            string
		o                               Order
		spread, platform, strategy, ven string
	}{
		// 2.5 at 150.15: spread 20 bps 0.750750, fee 10 bps 0.375375, venue 3 bps 0.112612.
		{"buy", buy, "750750", "375375", "0", "112612"},
		// 4 at 149.85: spread 1.198800, fee 0.599400, venue 0.179820.
		{"sell", sell, "1198800", "599400", "0", "179820"},
		// 1 at 150.15 for a strategy: spread 0.300300, fee 0.150150, strategy 5 bps 0.075075, venue 0.045045.
		{"strategy buy", strategy, "300300", "150150", "75075", "45045"},
	} {
		f := w.fillOf(tc.o)
		booked := w.bookingsOf(f.JournalTransactionID)
		require.Len(t, booked, 1, tc.name)
		b, ok := booked[acct]
		require.True(t, ok, "%s: the payer is the fill's account", tc.name)
		assert.Equal(t, feeledger.BookingEmbedded, b.BookingMode, tc.name)
		assert.Equal(t, ledger.KindSimTrade, b.Kind, tc.name)
		assert.Equal(t, gates.ModePaper, b.Mode, tc.name)
		assert.Equal(t, f.EffectID, b.EffectID, "%s: registered against the effect that applied the fill", tc.name)
		require.NotNil(t, b.IntentID, tc.name)
		assert.Equal(t, tc.o.IntentID, *b.IntentID, tc.name)
		assert.Equal(t, tc.spread, b.Charges.Spread.String(), tc.name)
		assert.Equal(t, tc.platform, b.Charges.Platform.String(), tc.name)
		assert.Equal(t, tc.strategy, b.Charges.Strategy.String(), tc.name)
		assert.Equal(t, tc.ven, b.Charges.Venue.String(), tc.name)
		assert.True(t, b.Charges.Network.IsZero(), tc.name)
		assert.Equal(t, f.Charges.Spread.String(), b.Charges.Spread.String(), "%s: the booking is the fill's own charges", tc.name)
		assert.Equal(t, f.Charges.PlatformFees.String(), b.Charges.Platform.Add(b.Charges.Strategy).String(), tc.name)
		assert.Equal(t, f.Charges.VenueFees.String(), b.Charges.Venue.String(), tc.name)
		require.NoError(t, w.feeLedger.VerifyBooking(w.ctx, testDB, b.ID), tc.name)

		q, err := w.feeLedger.Quote(w.ctx, testDB, b.DisclosureID)
		require.NoError(t, err)
		assert.True(t, bytes.Equal(f.FeeDisclosure, q.DisclosureBytes), "%s: one disclosure, byte for byte in both records", tc.name)
		assert.Equal(t, f.FeeDisclosureHash, q.DisclosureHash, tc.name)
		assert.Equal(t, f.FeeScheduleHash, q.ScheduleHash, tc.name)
		assert.Equal(t, hash, q.ScheduleHash, tc.name)
		assert.Equal(t, key, q.ScheduleKey, tc.name)
		assert.Equal(t, 1, q.ScheduleVersion, tc.name)
		assert.Equal(t, econ.SubsystemEquities, q.Subsystem, tc.name)
		assert.Equal(t, tradeKey(tc.o.ID), q.IdempotencyKey, tc.name)
		require.NotNil(t, q.AccountID, tc.name)
		assert.Equal(t, acct, *q.AccountID, "%s: the disclosure is bound to its payer", tc.name)
		_, err = w.feeLedger.VerifyQuoteRecord(w.ctx, testDB, b.DisclosureID)
		require.NoError(t, err, tc.name)
		_, err = w.svc.VerifyFill(w.ctx, testDB, tc.o.ID)
		require.NoError(t, err, tc.name)

		// Every component on its own pair of the fill's journal.
		jt, err := w.ledger.Transaction(w.ctx, testDB, f.JournalTransactionID)
		require.NoError(t, err)
		revenueLegs := 0
		for _, e := range jt.Entries {
			if e.Account.Code == ledger.CodePlatformFeeRevenue {
				revenueLegs++
			}
		}
		assert.Equal(t, 1+btoi(b.Charges.Strategy.IsPositive()), revenueLegs, "%s: the strategy fee is its own pair", tc.name)
	}
	sf := w.fillOf(sell)
	assert.Equal(t, sf.Charges.Total.String(), w.cust(acct, ledger.CodeUnsettledReceivable, w.cash.ID).String(),
		"the net proceeds wait for settlement")

	history, err := w.feeLedger.ScheduleHistory(w.ctx, testDB, key)
	require.NoError(t, err)
	require.Len(t, history, 1, "the schedule is published once and found after")
	assert.Equal(t, w.schedule.Version, history[0].Label)
	assert.Equal(t, hash, history[0].Hash)
	disclosures, bookings := w.feeRecordsOf(acct)
	assert.Equal(t, 3, disclosures)
	assert.Equal(t, 3, bookings)

	// A fill that charges nothing books nothing.
	free := newWorld(t, withZeroSchedule())
	payer := free.account()
	fl := free.list(1, 1)
	free.fund(payer, free.cash.ID, 1_000_000_000)
	free.mark(fl, 15_000)
	o := free.trade(payer, fl, market(SideBuy, 1_000_000))
	f := free.fillOf(o)
	assert.True(t, f.Charges.Spread.IsZero() && f.Charges.PlatformFees.IsZero() && f.Charges.VenueFees.IsZero())
	assert.Empty(t, free.bookingsOf(f.JournalTransactionID))
	disclosures, bookings = free.feeRecordsOf(payer)
	assert.Zero(t, disclosures)
	assert.Zero(t, bookings)
}

// TestIntegration_AReplayedEquityFillBooksItsChargesOnce: the venue's answer
// applied by many workers at once, applied again afterwards, and the order
// submitted again under its key all leave one fill, one disclosure, one
// booking and one charge.
func TestIntegration_AReplayedEquityFillBooksItsChargesOnce(t *testing.T) {
	w := newWorld(t)
	acct := w.account()
	l := w.list(1, 1)
	w.fund(acct, w.cash.ID, 10_000_000_000)
	w.mark(l, 15_000)
	spec := market(SideBuy, 1_000_000)
	spec.key = "replay-" + uuid.NewString()
	o := w.accept(w.submit(w.request(acct, l, spec)).ID)
	require.Equal(t, OrderAccepted, o.Status)
	report, err := w.svc.PlaceAtVenue(w.ctx, testDB, o.ID)
	require.NoError(t, err)
	require.Equal(t, ReportFilled, report.Status)

	const n = 8
	var wg sync.WaitGroup
	applied := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			applied <- inTxRetrying(w.ctx, func(ctx context.Context, tx pgx.Tx) error {
				_, err := w.svc.ApplyOrderReport(ctx, tx, o.ID, report)
				return err
			})
		}()
	}
	wg.Wait()
	close(applied)
	for err := range applied {
		require.NoError(t, err, "a replayed answer is not an error")
	}
	f := w.fillOf(w.order(o.ID))
	paid := w.cust(acct, ledger.CodeWallet, w.cash.ID)
	again, err := w.apply(o.ID, report)
	require.NoError(t, err)
	assert.Equal(t, OrderFilled, again.Status)
	in := w.submit(w.request(acct, l, spec))
	assert.Equal(t, o.IntentID, in.ID, "the same request under its key is the same intent")
	assert.Equal(t, o.ID, w.accept(in.ID).ID, "and the same order")

	require.Len(t, w.bookingsOf(f.JournalTransactionID), 1)
	disclosures, bookings := w.feeRecordsOf(acct)
	assert.Equal(t, 1, disclosures, "one disclosure")
	assert.Equal(t, 1, bookings, "one booking")
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM fee_quote_disclosures WHERE subsystem = 'EQUITIES' AND idempotency_key = $1`,
		tradeKey(o.ID)))
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM equity_fills WHERE order_id = $1`, o.ID))
	assert.Equal(t, paid.String(), w.cust(acct, ledger.CodeWallet, w.cash.ID).String(), "charged once")
	assert.Equal(t, f.Charges.Spread.String(), w.platform(ledger.CodeSpreadRevenue).String())
}

// TestIntegration_ARefusedOrRolledBackEquityFillBooksNothing: a fill whose
// transaction rolls back after its charges were booked inside it, a fill the
// venue reports through the order's limit, and a buy's fill applied while a
// SPREAD_ENGINE kill switch refuses its charge leave no disclosure, no
// booking, no fill and nothing paid -- and once nothing refuses it, the same
// answer fills and books exactly once. A fill whose schedule key another
// version governs is refused, never repriced, and books nothing either.
func TestIntegration_ARefusedOrRolledBackEquityFillBooksNothing(t *testing.T) {
	sb := &switchboard{}
	w := newWorld(t, withKillSwitches(sb))
	acct := w.account()
	l := w.list(1, 1)
	w.fund(acct, w.cash.ID, 10_000_000_000)
	w.mark(l, 15_000)
	wallet := w.cust(acct, ledger.CodeWallet, w.cash.ID)
	nothingBooked := func(w *world, acct accounts.AccountID, wallet money.Quantity, what string) {
		t.Helper()
		disclosures, bookings := w.feeRecordsOf(acct)
		assert.Zero(t, disclosures, "%s: no disclosure", what)
		assert.Zero(t, bookings, "%s: no booking", what)
		assert.Zero(t, w.count(`SELECT count(*) FROM equity_fills WHERE account_id = $1`, acct), "%s: no fill", what)
		assert.Equal(t, wallet.String(), w.cust(acct, ledger.CodeWallet, w.cash.ID).String(), "%s: nothing paid", what)
	}

	// Rolled back: inside its transaction the charge is booked, and none of it
	// survives.
	o := w.accept(w.submit(w.request(acct, l, market(SideBuy, 1_000_000))).ID)
	report, err := w.svc.PlaceAtVenue(w.ctx, testDB, o.ID)
	require.NoError(t, err)
	require.Equal(t, ReportFilled, report.Status)
	errRollBack := errors.New("roll the fill back")
	err = w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := w.svc.ApplyOrderReport(ctx, tx, o.ID, report); err != nil {
			return err
		}
		var inside int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM fee_bookings WHERE account_id = $1`, acct).Scan(&inside); err != nil {
			return err
		}
		require.Equal(t, 1, inside, "booked in the fill's own transaction")
		return errRollBack
	})
	require.ErrorIs(t, err, errRollBack)
	nothingBooked(w, acct, wallet, "a rolled-back fill")
	assert.Equal(t, OrderAccepted, w.order(o.ID).Status)

	// A fill through the order's limit is refused before anything is written.
	lim := w.trade(acct, l, limit(SideBuy, 1_000_000, 14_000))
	require.Equal(t, OrderOpen, lim.Status)
	forged, err := w.svc.PollVenue(w.ctx, testDB, lim.ID)
	require.NoError(t, err)
	require.Equal(t, ReportOpen, forged.Status)
	forged.Status = ReportFilled
	forged.Fill = &BrokerFill{
		VenueFillID: "forged-" + uuid.NewString(), Quantity: q(1_000_000), Price: q(14_500), PriceScale: 2,
		Mark: *mark(14_500, 2, w.clk.Now()), ExecutedAt: w.clk.Now(),
	}
	_, err = w.apply(lim.ID, forged)
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	nothingBooked(w, acct, wallet, "a fill through the limit")

	// A SPREAD_ENGINE kill switch: the buy's charge is new activity, refused.
	sb.set(spreadEngineOff)
	_, err = w.apply(o.ID, report)
	require.Error(t, err)
	assert.Equal(t, errs.CodeKillSwitchActive, errs.CodeOf(err), "%v", err)
	nothingBooked(w, acct, wallet, "a buy under a SPREAD_ENGINE kill switch")
	assert.Equal(t, OrderAccepted, w.order(o.ID).Status)

	// Released, the same answer fills and books once.
	sb.set()
	filled, err := w.apply(o.ID, report)
	require.NoError(t, err)
	require.Len(t, w.bookingsOf(w.fillOf(filled).JournalTransactionID), 1)
	disclosures, bookings := w.feeRecordsOf(acct)
	assert.Equal(t, 1, disclosures)
	assert.Equal(t, 1, bookings)

	// Another version governs the schedule's key: the fill is refused, never
	// repriced, and the order waits.
	v := newWorld(t)
	payer := v.account()
	vl := v.list(1, 1)
	v.fund(payer, v.cash.ID, 10_000_000_000)
	v.mark(vl, 15_000)
	funded := v.cust(payer, ledger.CodeWallet, v.cash.ID)
	waiting := v.accept(v.submit(v.request(payer, vl, market(SideBuy, 1_000_000))).ID)
	answer, err := v.svc.PlaceAtVenue(v.ctx, testDB, waiting.ID)
	require.NoError(t, err)
	key, _ := v.scheduleKey()
	other := v.schedule
	other.Version, other.SpreadBPS = "equities-itest-operator-v2", 30
	require.NoError(t, v.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := v.feeLedger.PublishSchedule(ctx, tx, feeledger.PublishRequest{
			Key: key, Schedule: other, Reason: "itest",
			Actor: v.sys,
		})
		return err
	}))
	_, err = v.apply(waiting.ID, answer)
	require.Error(t, err)
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err))
	assert.Equal(t, feeledger.ReasonScheduleNotInForce, feeledger.ReasonOf(err), "%v", err)
	nothingBooked(v, payer, funded, "a fill whose schedule is not the version in force")
	assert.Equal(t, OrderAccepted, v.order(waiting.ID).Status, "the order waits; nothing was repriced")
}

// TestIntegration_TheDatabaseRefusesAnUnbookedEquityFee writes an equity fill
// around Go -- its journal posted and applied by an effect, its order moved
// to FILLED -- and watches the database refuse it at COMMIT (FW001) when no
// booking registers its charges, when the booking's disclosure is not
// byte for byte the fill's own, and when the fill claims a spread the booking
// did not charge. The same row with its own booking commits.
func TestIntegration_TheDatabaseRefusesAnUnbookedEquityFee(t *testing.T) {
	w := newWorld(t)
	acct := w.account()
	l := w.list(1, 1)
	w.fund(acct, w.cash.ID, 10_000_000_000)
	w.mark(l, 15_000)
	// A real fill supplies a schedule and a disclosure the fee ledger has seen;
	// an open order of the same quantity is what the forged fill fills.
	paid := w.fillOf(w.trade(acct, l, market(SideBuy, 1_000_000)))
	open := w.trade(acct, l, limit(SideBuy, 1_000_000, 14_000))
	require.Equal(t, OrderOpen, open.Status)
	d, err := DecodeDisclosure(paid.FeeDisclosure)
	require.NoError(t, err)
	charged := feeledger.ChargesOf(d)
	require.True(t, charged.Spread.IsPositive())
	key, _ := w.scheduleKey()
	system := econ.SystemActor("equities-itest")

	forge := func(book bool, disclosure string, extraSpread int64) error {
		return w.inTx(func(ctx context.Context, tx pgx.Tx) error {
			ref := uuid.NewString()
			entries, err := TradeEntries(acct, SideBuy, l.ShareAsset.ID, w.cash.ID, paid.Quantity, paid.Charges, charged)
			if err != nil {
				return err
			}
			posted, err := w.ledger.Post(ctx, tx, ledger.Posting{
				Kind: ledger.KindSimTrade, IdempotencyKey: "forged:" + ref, EffectiveAt: w.clk.Now(), Entries: entries,
				Reference: ledger.FinancialEventReference{Type: "equity_fill", ID: ref},
			})
			if err != nil {
				return err
			}
			eff, _, err := w.effects.Begin(ctx, tx, econ.BeginRequest{
				Key: "forged:" + ref, Subsystem: econ.SubsystemEquities, Kind: "EQUITY_TRADE", AccountID: acct.String(),
				Request: map[string]string{"ref": ref}, PostsLedger: true, Actor: system,
			})
			if err != nil {
				return err
			}
			if _, err := w.effects.Apply(ctx, tx, eff.ID, econ.ApplyRequest{JournalTransactionID: &posted.TransactionID, Actor: system}); err != nil {
				return err
			}
			if book {
				fq, err := w.feeLedger.RecordQuote(ctx, tx, feeledger.QuoteRequest{
					ScheduleKey: key, Input: d.Input.QuoteInput(), Subsystem: econ.SubsystemEquities, IdempotencyKey: "forged:" + ref,
					AccountID: &acct, Mode: gates.ModePaper, Actor: system,
				})
				if err != nil {
					return err
				}
				if _, err := w.feeLedger.RecordEmbedded(ctx, tx, feeledger.EmbeddedRequest{
					DisclosureID: fq.ID, AccountID: acct, JournalTransactionID: posted.TransactionID, EffectID: eff.ID,
					Mode: gates.ModePaper, Actor: system,
				}); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, `INSERT INTO equity_fills (id, order_id, account_id, instrument_id, side, quantity, price,
				price_scale, mark_mantissa, mark_scale, mark_source, mark_observed_at, slippage_bps, venue, venue_fill_id, executed_at,
				trade_date, settlement_date, notional, spread_amount, platform_fees, venue_fees, total, fee_schedule_version,
				fee_schedule_hash, fee_schedule, fee_disclosure, fee_disclosure_hash, journal_transaction_id, effect_id)
				SELECT $1, $2, account_id, instrument_id, side, quantity, $3, price_scale, mark_mantissa, mark_scale, mark_source,
				       mark_observed_at, slippage_bps, venue, $4, $5, trade_date, settlement_date, notional, spread_amount + $6,
				       platform_fees, venue_fees, total + $6, fee_schedule_version, fee_schedule_hash, fee_schedule, $7,
				       fee_disclosure_hash, $8, $9
				  FROM equity_fills WHERE id = $10`,
				uuid.New(), open.ID, open.LimitPrice.String(), "forged-"+ref, open.CreatedAt, extraSpread, disclosure,
				posted.TransactionID, eff.ID, paid.ID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO equity_order_transitions (id, order_id, from_status, to_status, actor_type,
				actor_id, reason, occurred_at) VALUES ($1,$2,'OPEN','FILLED','SYSTEM','forged','forged',now())`, uuid.New(), open.ID); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE equity_orders SET status = 'FILLED' WHERE id = $1`, open.ID)
			return err
		})
	}
	own := string(paid.FeeDisclosure)

	err = forge(false, own, 0)
	require.Error(t, err)
	assert.Equal(t, "FW001", db.SQLState(err), "an equity fee the fee ledger did not book: %v", err)
	err = forge(true, `{"kind":"not the disclosure the fee ledger booked"}`, 0)
	require.Error(t, err)
	assert.Equal(t, "FW001", db.SQLState(err), "a disclosure that is not the booking's, byte for byte: %v", err)
	err = forge(true, own, 1)
	require.Error(t, err)
	assert.Equal(t, "FW001", db.SQLState(err), "a spread one unit beyond the booking's: %v", err)
	assert.Equal(t, OrderOpen, w.order(open.ID).Status, "nothing of the refused fills survived")
	require.NoError(t, forge(true, own, 0), "the same fill with its own booking commits")
	assert.Equal(t, OrderFilled, w.order(open.ID).Status)
}

// TestIntegration_TheSpreadAuditFindsEveryEquitySpread runs the spread audit's
// mechanism (TestIntegration_EveryChargedSpreadIsDiscoverableFromTheAuditRecord)
// over equity fills: the fee ledger's audit record lists every spread an
// equity fill charged -- with its schedule key and version, its disclosure
// and the fill's own journal -- every listed charge re-verifies from its
// record, the listed spread is exactly what SPREAD_REVENUE holds in the
// settlement asset, before and after a reversal, and every journal that
// credited that SPREAD_REVENUE is a listed booking's.
func TestIntegration_TheSpreadAuditFindsEveryEquitySpread(t *testing.T) {
	w := newWorld(t)
	acct := w.account()
	other := w.account()
	l := w.list(1, 1)
	for _, a := range []accounts.AccountID{acct, other} {
		w.fund(a, w.cash.ID, 10_000_000_000)
		w.fund(a, l.ShareAsset.ID, 10_000_000)
	}
	w.mark(l, 15_000)
	key, hash := w.scheduleKey()
	filled := []Order{
		w.trade(acct, l, market(SideBuy, 2_500_000)),
		w.trade(acct, l, market(SideSell, 4_000_000)),
		w.trade(other, l, limit(SideBuy, 1_234_567, 15_100)),
		w.trade(other, l, market(SideSell, 3_000_000)),
	}
	byJournal := map[ledger.TransactionID]Fill{}
	var fillSpread money.Quantity
	for _, o := range filled {
		f := w.fillOf(o)
		byJournal[f.JournalTransactionID] = f
		fillSpread = fillSpread.Add(f.Charges.Spread)
	}

	listed, err := w.feeLedger.SpreadAudit(w.ctx, testDB, feeledger.SpreadFilter{QuoteAsset: &w.cash.ID})
	require.NoError(t, err)
	require.Len(t, listed, len(filled), "every equity fill's spread is in the audit record")
	var listedSpread money.Quantity
	for _, c := range listed {
		f, ok := byJournal[c.JournalTransactionID]
		require.True(t, ok, "a listed charge names an equity fill's journal")
		assert.Equal(t, f.AccountID, c.AccountID)
		assert.Equal(t, f.Charges.Spread.String(), c.SpreadAmount.String())
		assert.Equal(t, f.FeeDisclosureHash, c.DisclosureHash)
		assert.Equal(t, key, c.ScheduleKey)
		assert.Equal(t, 1, c.ScheduleVersion)
		assert.Equal(t, hash, c.ScheduleHash)
		assert.Equal(t, w.schedule.SpreadBPS, c.SpreadBPS)
		assert.Equal(t, feeledger.BookingEmbedded, c.BookingMode)
		assert.Equal(t, feeledger.BookingBooked, c.Status)
		_, err := w.feeLedger.VerifyQuoteRecord(w.ctx, testDB, c.DisclosureID)
		require.NoError(t, err)
		require.NoError(t, w.feeLedger.VerifyBooking(w.ctx, testDB, c.BookingID))
		listedSpread = listedSpread.Add(c.SpreadAmount)
	}
	assert.Equal(t, fillSpread.String(), listedSpread.String())
	booked, reversed, err := w.feeLedger.SpreadTotals(w.ctx, testDB, w.cash.ID)
	require.NoError(t, err)
	assert.True(t, reversed.IsZero())
	assert.Equal(t, listedSpread.String(), booked.String())
	assert.Equal(t, listedSpread.String(), w.platform(ledger.CodeSpreadRevenue).String(),
		"every unit of spread revenue is a listed equity charge")

	// A reversed charge leaves the booked list; SPREAD_REVENUE follows it.
	r, err := func() (feeledger.Booking, error) {
		var out feeledger.Booking
		err := w.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			out, err = w.feeLedger.Reverse(ctx, tx, feeledger.ReverseRequest{
				BookingID:  listed[1].BookingID,
				ReasonCode: "CUSTOMER_GOODWILL", Reason: "sandbox correction", Actor: w.sys,
			})
			return err
		})
		return out, err
	}()
	require.NoError(t, err)
	assert.Equal(t, feeledger.BookingReversed, r.Status)
	still, err := w.feeLedger.SpreadAudit(w.ctx, testDB, feeledger.SpreadFilter{QuoteAsset: &w.cash.ID})
	require.NoError(t, err)
	assert.Len(t, still, len(filled)-1)
	all, err := w.feeLedger.SpreadAudit(w.ctx, testDB, feeledger.SpreadFilter{QuoteAsset: &w.cash.ID, IncludeReversed: true})
	require.NoError(t, err)
	assert.Len(t, all, len(filled))
	booked, reversed, err = w.feeLedger.SpreadTotals(w.ctx, testDB, w.cash.ID)
	require.NoError(t, err)
	assert.Equal(t, listed[1].SpreadAmount.String(), reversed.String())
	assert.Equal(t, booked.Sub(reversed).String(), w.platform(ledger.CodeSpreadRevenue).String())
	// Every journal that credited this asset's SPREAD_REVENUE is a booking's.
	assert.Zero(t, w.count(`SELECT count(*) FROM journal_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
		WHERE la.owner_type = 'PLATFORM' AND la.code = 'SPREAD_REVENUE' AND la.asset_id = $1 AND e.side = 'CREDIT'
		  AND e.transaction_id NOT IN (SELECT journal_transaction_id FROM fee_bookings WHERE quote_asset_id = $1)`, w.cash.ID))
	assert.Equal(t, len(filled), w.count(`SELECT count(DISTINCT e.transaction_id) FROM journal_entries e
		JOIN ledger_accounts la ON la.id = e.ledger_account_id
		WHERE la.owner_type = 'PLATFORM' AND la.code = 'SPREAD_REVENUE' AND la.asset_id = $1 AND e.side = 'CREDIT'`, w.cash.ID),
		"one credit per equity fill: the check above is not vacuous")
}

// TestIntegration_ASpreadEngineKillSwitchStopsEquityBuysNotSells: the fee
// ledger books a charge only under SPREAD_ENGINE. While it is kill-switched,
// a new order whose fill would be charged is refused at acceptance, naming
// SPREAD_ENGINE; a resting buy does not execute even when the mark crosses
// it, because its charge is new activity; a resting sell does, and its charge
// is booked as risk reduction. Released, the buy fills and books. A schedule
// that charges nothing never needs SPREAD_ENGINE.
func TestIntegration_ASpreadEngineKillSwitchStopsEquityBuysNotSells(t *testing.T) {
	sb := &switchboard{}
	w := newWorld(t, withKillSwitches(sb))
	acct := w.account()
	l := w.list(1, 1)
	w.fund(acct, w.cash.ID, 10_000_000_000)
	w.fund(acct, l.ShareAsset.ID, 5_000_000)
	w.mark(l, 15_000)
	buy := w.trade(acct, l, limit(SideBuy, 1_000_000, 14_900))
	sell := w.trade(acct, l, limit(SideSell, 1_000_000, 15_100))
	require.Equal(t, OrderOpen, buy.Status)
	require.Equal(t, OrderOpen, sell.Status)

	sb.set(spreadEngineOff)
	for _, s := range []orderSpec{market(SideBuy, 1_000_000), market(SideSell, 1_000_000)} {
		o := w.accept(w.submit(w.request(acct, l, s)).ID)
		require.Equal(t, OrderRejected, o.Status, "%s", s.side)
		assert.Equal(t, RejectKillSwitch, o.RejectionCode)
		assert.Equal(t, []string{string(gates.SpreadEngine) + ":" + gates.ReasonCapabilityKillSwitched}, o.RejectionReasons)
		assert.Nil(t, o.ReservationID)
	}
	w.at(2026, 10, 7, 11, 0)
	w.mark(l, 14_800)
	assert.Equal(t, OrderOpen, w.poll(buy).Status, "a buy's charge is new activity: it does not execute")
	w.at(2026, 10, 7, 11, 1)
	w.mark(l, 15_200)
	sold := w.poll(sell)
	require.Equal(t, OrderFilled, sold.Status, "a sell reduces risk and executes")
	sf := w.fillOf(sold)
	b, ok := w.bookingsOf(sf.JournalTransactionID)[acct]
	require.True(t, ok, "and its charge is booked")
	assert.Equal(t, sf.Charges.Spread.String(), b.Charges.Spread.String())
	require.NoError(t, w.feeLedger.VerifyBooking(w.ctx, testDB, b.ID))

	sb.set()
	w.at(2026, 10, 7, 11, 2)
	w.mark(l, 14_800)
	bought := w.poll(buy)
	require.Equal(t, OrderFilled, bought.Status, "released, the buy executes")
	require.Len(t, w.bookingsOf(w.fillOf(bought).JournalTransactionID), 1)

	free := newWorld(t, withZeroSchedule(), withKillSwitches(&switchboard{snap: killswitch.Snapshot{
		Switches: []killswitch.Switch{spreadEngineOff},
	}}))
	payer := free.account()
	fl := free.list(1, 1)
	free.fund(payer, free.cash.ID, 1_000_000_000)
	free.mark(fl, 15_000)
	o := free.trade(payer, fl, market(SideBuy, 1_000_000))
	assert.Equal(t, OrderFilled, o.Status, "nothing is charged, so SPREAD_ENGINE is not needed")
	disclosures, bookings := free.feeRecordsOf(payer)
	assert.Zero(t, disclosures)
	assert.Zero(t, bookings)
}
