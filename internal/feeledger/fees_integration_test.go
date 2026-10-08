//go:build integration

package feeledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// TestIntegration_SchedulesAreVersionedImmutableAndHashed: versions of a
// key are consecutive, each takes effect strictly after the last and never in
// the past, the version in force at an instant is the latest one effective by
// then, a label means one content forever, and an agent never publishes.
func TestIntegration_SchedulesAreVersionedImmutableAndHashed(t *testing.T) {
	f := newFixture(t)
	quote := f.simAsset("FLQ")
	key := "itest:" + uuid.NewString()
	v1 := f.publish(key, f.schedule(quote.ID, "v1"), time.Time{})
	later := f.clk.Now().Add(time.Hour)
	s2 := f.schedule(quote.ID, "v2")
	s2.SpreadBPS = 30
	v2 := f.publish(key, s2, later)
	assert.Equal(t, 1, v1.Version)
	assert.Equal(t, 2, v2.Version)

	at, err := f.svc.EffectiveSchedule(f.ctx, testDB, key, f.clk.Now())
	require.NoError(t, err)
	assert.Equal(t, v1.ID, at.ID)
	at, err = f.svc.EffectiveSchedule(f.ctx, testDB, key, later)
	require.NoError(t, err)
	assert.Equal(t, v2.ID, at.ID)
	_, err = f.svc.EffectiveSchedule(f.ctx, testDB, key, f.clk.Now().Add(-time.Minute))
	assert.Equal(t, ReasonScheduleNotFound, ReasonOf(err))

	again := f.publish(key, s2, later)
	assert.Equal(t, v2.ID, again.ID, "publishing the same label and content again is a replay")
	changed := s2
	changed.PercentageFeeBPS = 99
	_, err = do(f, func(ctx context.Context, tx pgx.Tx) (PublishedSchedule, error) {
		return f.svc.PublishSchedule(ctx, tx, PublishRequest{Key: key, Schedule: changed, EffectiveFrom: later.Add(time.Hour), Reason: "x", Actor: operator})
	})
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err), "a label means one content forever")
	s3 := f.schedule(quote.ID, "v3")
	for name, from := range map[string]time.Time{"in the past": f.clk.Now().Add(-time.Second), "not after v2": later} {
		_, err = do(f, func(ctx context.Context, tx pgx.Tx) (PublishedSchedule, error) {
			return f.svc.PublishSchedule(ctx, tx, PublishRequest{Key: key, Schedule: s3, EffectiveFrom: from, Reason: "x", Actor: operator})
		})
		assert.Equal(t, ReasonBackdated, ReasonOf(err), name)
	}
	_, err = do(f, func(ctx context.Context, tx pgx.Tx) (PublishedSchedule, error) {
		return f.svc.PublishSchedule(ctx, tx, PublishRequest{Key: key, Schedule: s3, Reason: "x",
			Actor: econ.Actor{Type: security.ActorAgent, ID: "agent-1"}})
	})
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))

	history, err := f.svc.ScheduleHistory(f.ctx, testDB, key)
	require.NoError(t, err)
	require.Len(t, history, 2)
	sum := sha256.Sum256(history[1].Canonical)
	assert.Equal(t, hex.EncodeToString(sum[:]), history[1].Hash)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM audit_events WHERE action = 'fees.schedule.published' AND resource_id = $1`,
		v2.ID.String()))
}

// TestIntegration_QuoteDisclosuresAreStoredAndVerifiable: a recorded quote
// stores the reference, execution and effective prices, the spread, every
// explicit fee and the network estimate; it re-verifies from the record
// alone; a replay returns it and another request under its key is refused.
func TestIntegration_QuoteDisclosuresAreStoredAndVerifiable(t *testing.T) {
	f := newFixture(t)
	quote := f.simAsset("FLQ")
	key := "itest:" + uuid.NewString()
	f.publish(key, f.schedule(quote.ID, "v1"), time.Time{})
	req := QuoteRequest{
		ScheduleKey: key, Subsystem: econ.SubsystemOrders, IdempotencyKey: "q-" + uuid.NewString(), Mode: gates.ModePaper, Actor: service,
		Input: fees.QuoteInput{Side: fees.SideBuy, BaseQty: units(2), BaseDecimals: 6, QuoteDecimals: 6,
			ReferencePrice: f.price(quote.ID, "100.00"), StrategyOriginated: true},
	}
	stored, err := do(f, func(ctx context.Context, tx pgx.Tx) (StoredQuote, error) { return f.svc.RecordQuote(ctx, tx, req) })
	require.NoError(t, err)
	assert.Equal(t, "10000", stored.ReferencePrice().Mantissa.String())
	assert.Equal(t, "10020", stored.ExecutionPrice().Mantissa.String(), "100.20: the reference plus the 20 bps spread")
	c := stored.Charges
	assert.Equal(t, "400000", c.Spread.String())
	assert.Equal(t, "700000", c.Platform.String(), "0.50 fixed + 0.20 percentage")
	assert.Equal(t, "300000", c.Strategy.String())
	assert.Equal(t, "100000", c.Venue.String())
	assert.Equal(t, "100000", c.Network.String())
	assert.Equal(t, "201600000", stored.Disclosure.TotalDebit.String(), "200 + 0.40 + 1.10 fees + 0.10 network")
	var spread, total, network string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT spread_amount::text, total_debit::text, network_fee::text
		FROM fee_quote_disclosures WHERE id = $1`, stored.ID).Scan(&spread, &total, &network))
	assert.Equal(t, []string{"400000", "201600000", "100000"}, []string{spread, total, network})

	d, err := f.svc.VerifyQuoteRecord(f.ctx, testDB, stored.ID)
	require.NoError(t, err)
	verified, err := d.Hash()
	require.NoError(t, err)
	assert.Equal(t, stored.DisclosureHash, verified, "the record verifies as the disclosure that was stored")
	again, err := do(f, func(ctx context.Context, tx pgx.Tx) (StoredQuote, error) { return f.svc.RecordQuote(ctx, tx, req) })
	require.NoError(t, err)
	assert.Equal(t, stored.ID, again.ID)
	other := req
	other.Input.BaseQty = units(3)
	_, err = do(f, func(ctx context.Context, tx pgx.Tx) (StoredQuote, error) { return f.svc.RecordQuote(ctx, tx, other) })
	assert.Equal(t, errs.CodeInvalidIdempotencyReuse, errs.CodeOf(err))
}

// TestIntegration_ChargesAreBookedComponentByComponentExactlyOnce: a booking
// moves exactly the disclosed charges, each component on its own accounts,
// as one effect, however many times and however concurrently it is asked
// for; another payer, an expired quote and a zero charge are refused.
func TestIntegration_ChargesAreBookedComponentByComponentExactlyOnce(t *testing.T) {
	f := newFixture(t)
	quote := f.simAsset("FLQ")
	key := "itest:" + uuid.NewString()
	f.publish(key, f.schedule(quote.ID, "v1"), time.Time{})
	payer := f.customer(quote.ID, units(1_000))
	stored := f.quote(key, quote.ID, &payer, fees.SideBuy, units(2), "100.00")

	const n = 8
	var wg sync.WaitGroup
	ids := make(chan BookingID, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, err := do(f, func(ctx context.Context, tx pgx.Tx) (Booking, error) {
				return f.svc.Book(ctx, tx, BookRequest{DisclosureID: stored.ID, AccountID: payer, Kind: ledger.KindSimTrade,
					Mode: gates.ModePaper, Actor: service})
			})
			if assert.NoError(t, err) {
				ids <- b.ID
			}
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[BookingID]bool{}
	for bid := range ids {
		seen[bid] = true
	}
	require.Len(t, seen, 1, "one booking, whoever asked")
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM fee_bookings WHERE disclosure_id = $1`, stored.ID))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM economic_effects WHERE effect_key = $1`, "fees:charge:"+stored.ID.String()))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM journal_transactions WHERE reference_type = 'fee_charge' AND reference_id = $1`,
		stored.ID.String()))

	c := stored.Charges
	assert.Equal(t, units(1_000).Sub(c.InQuoteAsset()).String(), f.wallet(payer, quote.ID).String(), "charged once, exactly")
	for code, want := range map[ledger.Code]money.Quantity{
		ledger.CodeFeesSpread: c.Spread, ledger.CodeFeesPlatform: c.Platform.Add(c.Strategy), ledger.CodeFeesVenue: c.Venue,
		ledger.CodeFeesNetwork: c.Network,
	} {
		assert.Equal(t, want.String(), f.balance(ledger.CustomerAccount(payer, code, quote.ID)).String(), code)
	}
	assert.Equal(t, c.Spread.String(), f.balance(ledger.PlatformAccount(ledger.CodeSpreadRevenue, quote.ID)).String())
	assert.Equal(t, c.Platform.Add(c.Strategy).String(), f.balance(ledger.PlatformAccount(ledger.CodePlatformFeeRevenue, quote.ID)).String())
	// The platform and strategy fees are separate pairs of entries.
	b := f.book(stored, payer)
	jt, err := f.ledger.Transaction(f.ctx, testDB, b.JournalTransactionID)
	require.NoError(t, err)
	revenueEntries := 0
	for _, e := range jt.Entries {
		if e.Account.Code == ledger.CodePlatformFeeRevenue {
			revenueEntries++
		}
	}
	assert.Equal(t, 2, revenueEntries)
	require.NoError(t, f.svc.VerifyBooking(f.ctx, testDB, b.ID))
	assert.Zero(t, f.count(`SELECT count(*) FROM asset_reservations WHERE account_id = $1 AND status = 'ACTIVE'`, payer),
		"the reservation behind the charge is consumed")

	// Refusals.
	stranger := f.customer(quote.ID, units(1_000))
	other := f.quote(key, quote.ID, &payer, fees.SideBuy, units(1), "100.00")
	_, err = do(f, func(ctx context.Context, tx pgx.Tx) (Booking, error) {
		return f.svc.Book(ctx, tx, BookRequest{DisclosureID: other.ID, AccountID: stranger, Kind: ledger.KindSimTrade, Mode: gates.ModePaper, Actor: service})
	})
	assert.Equal(t, ReasonPayerMismatch, ReasonOf(err))
	f.clk.Advance(3 * time.Minute)
	_, err = do(f, func(ctx context.Context, tx pgx.Tx) (Booking, error) {
		return f.svc.Book(ctx, tx, BookRequest{DisclosureID: other.ID, AccountID: payer, Kind: ledger.KindSimTrade, Mode: gates.ModePaper, Actor: service})
	})
	assert.Equal(t, ReasonQuoteExpired, ReasonOf(err))
	zeroKey := "itest:" + uuid.NewString()
	f.publish(zeroKey, fees.ZeroSchedule(quote.ID, 6), time.Time{})
	free := f.quote(zeroKey, quote.ID, &payer, fees.SideBuy, units(1), "100.00")
	_, err = do(f, func(ctx context.Context, tx pgx.Tx) (Booking, error) {
		return f.svc.Book(ctx, tx, BookRequest{DisclosureID: free.ID, AccountID: payer, Kind: ledger.KindSimTrade, Mode: gates.ModePaper, Actor: service})
	})
	assert.Equal(t, ReasonNothingToBook, ReasonOf(err))
	poor := f.customer(quote.ID, units(1))
	broke := f.quote(key, quote.ID, &poor, fees.SideBuy, units(2), "100.00")
	_, err = do(f, func(ctx context.Context, tx pgx.Tx) (Booking, error) {
		return f.svc.Book(ctx, tx, BookRequest{DisclosureID: broke.ID, AccountID: poor, Kind: ledger.KindSimTrade, Mode: gates.ModePaper, Actor: service})
	})
	assert.Equal(t, errs.CodeInsufficientBuyingPower, errs.CodeOf(err), "a fee never spends what the payer does not have free")
}

// TestIntegration_EveryChargedSpreadIsDiscoverableFromTheAuditRecord: with
// charges booked by the fee ledger, charges embedded in a caller's trade
// journal, and a reversal, the spread the audit record lists is exactly what
// SPREAD_REVENUE holds, every listed charge re-verifies from its record, and
// every journal that credited SPREAD_REVENUE is a listed booking's.
func TestIntegration_EveryChargedSpreadIsDiscoverableFromTheAuditRecord(t *testing.T) {
	f := newFixture(t)
	quote := f.simAsset("FLQ")
	base := f.simAsset("FLB")
	key := "itest:" + uuid.NewString()
	f.publish(key, f.schedule(quote.ID, "v1"), time.Time{})
	var payers []accounts.AccountID
	var bookings []Booking
	for i := 0; i < 3; i++ {
		payer := f.customer(quote.ID, units(10_000))
		payers = append(payers, payer)
		bookings = append(bookings, f.book(f.quote(key, quote.ID, &payer, fees.SideBuy, units(int64(i+1)), "101.37"), payer))
	}

	// An embedded charge: the caller posts a trade and its fees in one
	// SIM_TRADE journal and registers the fees.
	trader := f.customer(quote.ID, units(10_000))
	embeddedQuote := f.quote(key, quote.ID, &trader, fees.SideBuy, units(1), "99.99")
	embedded, err := do(f, func(ctx context.Context, tx pgx.Tx) (Booking, error) {
		notional := embeddedQuote.Disclosure.NotionalAtReference
		qty := units(1)
		entries := []ledger.Entry{
			{Account: ledger.CustomerAccount(trader, ledger.CodeWallet, quote.ID), Side: ledger.Credit, Quantity: notional},
			{Account: ledger.CustomerAccount(trader, ledger.CodeTradingOutflow, quote.ID), Side: ledger.Debit, Quantity: notional},
			{Account: ledger.CustomerAccount(trader, ledger.CodeWallet, base.ID), Side: ledger.Debit, Quantity: qty},
			{Account: ledger.CustomerAccount(trader, ledger.CodeTradingInflow, base.ID), Side: ledger.Credit, Quantity: qty},
		}
		entries = append(entries, ChargeEntries(trader, embeddedQuote.Charges)...)
		ref := uuid.NewString()
		posted, err := f.ledger.Post(ctx, tx, ledger.Posting{Kind: ledger.KindSimTrade, IdempotencyKey: "trade:" + ref,
			Reference: ledger.FinancialEventReference{Type: "paper_fill", ID: ref}, EffectiveAt: f.clk.Now(), Entries: entries})
		if err != nil {
			return Booking{}, err
		}
		eff, _, err := f.effects.Begin(ctx, tx, econ.BeginRequest{Key: "trade:" + ref, Subsystem: econ.SubsystemOrders, Kind: "PAPER_FILL",
			AccountID: trader.String(), Request: map[string]string{"ref": ref}, PostsLedger: true, Actor: service})
		if err != nil {
			return Booking{}, err
		}
		if _, err := f.effects.Apply(ctx, tx, eff.ID, econ.ApplyRequest{JournalTransactionID: &posted.TransactionID, Actor: service}); err != nil {
			return Booking{}, err
		}
		return f.svc.RecordEmbedded(ctx, tx, EmbeddedRequest{DisclosureID: embeddedQuote.ID, AccountID: trader,
			JournalTransactionID: posted.TransactionID, EffectID: eff.ID, Mode: gates.ModePaper, Actor: service})
	})
	require.NoError(t, err)
	assert.Equal(t, BookingEmbedded, embedded.BookingMode)
	bookings = append(bookings, embedded)

	// One posted and one embedded charge are reversed.
	for _, b := range []Booking{bookings[1], embedded} {
		r, err := do(f, func(ctx context.Context, tx pgx.Tx) (Booking, error) {
			return f.svc.Reverse(ctx, tx, ReverseRequest{BookingID: b.ID, ReasonCode: "CUSTOMER_GOODWILL", Reason: "sandbox correction", Actor: operator})
		})
		require.NoError(t, err)
		assert.Equal(t, BookingReversed, r.Status)
	}
	assert.Equal(t, units(10_000).String(), f.wallet(trader, quote.ID).Add(embeddedQuote.Disclosure.NotionalAtReference).String(),
		"the reversed trader paid the notional and nothing else")

	listed, err := f.svc.SpreadAudit(f.ctx, testDB, SpreadFilter{QuoteAsset: &quote.ID})
	require.NoError(t, err)
	var listedSpread money.Quantity
	for _, c := range listed {
		assert.Equal(t, BookingBooked, c.Status)
		listedSpread = listedSpread.Add(c.SpreadAmount)
		_, err := f.svc.VerifyQuoteRecord(f.ctx, testDB, c.DisclosureID)
		require.NoError(t, err)
		require.NoError(t, f.svc.VerifyBooking(f.ctx, testDB, c.BookingID))
		assert.Equal(t, int32(20), int32(c.SpreadBPS))
	}
	assert.Len(t, listed, 2, "four charged, two reversed: two still booked")
	booked, reversed, err := f.svc.SpreadTotals(f.ctx, testDB, quote.ID)
	require.NoError(t, err)
	assert.Equal(t, listedSpread.String(), booked.Sub(reversed).String())
	assert.Equal(t, listedSpread.String(), f.balance(ledger.PlatformAccount(ledger.CodeSpreadRevenue, quote.ID)).String(),
		"every unit of spread revenue is a listed charge")
	all, err := f.svc.SpreadAudit(f.ctx, testDB, SpreadFilter{QuoteAsset: &quote.ID, IncludeReversed: true})
	require.NoError(t, err)
	assert.Len(t, all, 4)
	// Every journal that credited this asset's SPREAD_REVENUE is a booking's.
	assert.Zero(t, f.count(`SELECT count(*) FROM journal_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
		WHERE la.owner_type = 'PLATFORM' AND la.code = 'SPREAD_REVENUE' AND la.asset_id = $1 AND e.side = 'CREDIT'
		  AND e.transaction_id NOT IN (SELECT journal_transaction_id FROM fee_bookings WHERE quote_asset_id = $1)`, quote.ID))
	_ = payers
}

// TestIntegration_NoNegativeFeeAndNoFeeAboveTheConfiguredMax: the engine
// never prices either, and the database refuses a disclosure that says so
// even when its columns and document agree.
func TestIntegration_NoNegativeFeeAndNoFeeAboveTheConfiguredMax(t *testing.T) {
	f := newFixture(t)
	quote := f.simAsset("FLQ")
	key := "itest:" + uuid.NewString()
	capped := f.schedule(quote.ID, "capped-v1")
	capped.MaxTotalFee = money.QuantityFromInt64(1_000_000)
	capped.MaxTotalFeeBPS = 50
	f.publish(key, capped, time.Time{})
	big := f.quote(key, quote.ID, nil, fees.SideBuy, units(1_000), "100.00")
	assert.Equal(t, "1000000", big.Disclosure.Fees.Total.String(), "the absolute cap binds")
	assert.True(t, big.Disclosure.Fees.MaxAbsoluteApplied)
	negative := capped
	negative.FixedFee = money.QuantityFromInt64(-1)
	assert.Error(t, negative.Validate(), "a negative fee is not a schedule")

	// Forge: a disclosure whose document and columns agree on a fee above the
	// cap, then one with a negative spread. The database refuses both.
	notional, network := big.Disclosure.NotionalAtReference, big.Charges.Network
	forge := func(spread, platform money.Quantity) (DisclosureID, error) {
		forged := id.New[disclosureKind]()
		return forged, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var doc map[string]any
			if err := json.Unmarshal(big.DisclosureBytes, &doc); err != nil {
				return err
			}
			debit := notional.Add(spread).Add(platform).Add(network)
			doc["spread"].(map[string]any)["amount"] = spread.String()
			doc["notional_at_execution"] = notional.Add(spread).String()
			doc["total_debit"] = debit.String()
			fs := doc["fees"].(map[string]any)
			for _, l := range fs["lines"].([]any) {
				line := l.(map[string]any)
				line["charged"], line["computed"], line["cap_reduction"] = "0", "0", "0"
				if line["component"] == "FIXED_FEE" {
					line["charged"], line["computed"] = platform.String(), platform.String()
				}
			}
			fs["total"], fs["computed_total"], fs["minimum_fee_top_up"] = platform.String(), platform.String(), "0"
			body, err := json.Marshal(doc)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(body)
			_, err = tx.Exec(ctx, `INSERT INTO fee_quote_disclosures (id, schedule_id, schedule_hash, subsystem, idempotency_key, request_hash,
				quote_asset_id, side, base_quantity, reference_mantissa, reference_scale, execution_mantissa, execution_scale,
				effective_mantissa, effective_scale, notional_at_reference, notional_at_execution, spread_bps, spread_amount, platform_fee,
				strategy_fee, venue_fee, explicit_fees_total, network_fee_asset_id, network_fee, network_in_quote, slippage_reserve,
				total_debit, total_credit, max_total_fee_bps, max_bps_amount, max_total_fee, disclosure, disclosure_hash, quoted_at, valid_until)
				SELECT $1, schedule_id, schedule_hash, 'ORDERS', $2, request_hash, quote_asset_id, side, base_quantity, reference_mantissa,
				       reference_scale, execution_mantissa, execution_scale, effective_mantissa, effective_scale, notional_at_reference,
				       notional_at_reference + $3::numeric, spread_bps, $3::numeric, $4::numeric, 0, 0, $4::numeric, network_fee_asset_id,
				       network_fee, network_in_quote, slippage_reserve, $5::numeric, 0, max_total_fee_bps, max_bps_amount, max_total_fee,
				       $6, $7, quoted_at, valid_until
				  FROM fee_quote_disclosures WHERE id = $8`,
				forged, "forged-"+uuid.NewString(), spread.String(), platform.String(), debit.String(), body,
				hex.EncodeToString(sum[:]), big.ID)
			return err
		})
	}
	_, err := forge(big.Charges.Spread, money.QuantityFromInt64(2_000_000))
	require.Error(t, err)
	assert.Equal(t, "23514", db.SQLState(err), "a fee above the configured maximum: %v", err)
	assert.Equal(t, "fee_quote_disclosures_absolute_cap", db.ConstraintName(err))
	_, err = forge(money.QuantityFromInt64(-1), money.QuantityFromInt64(500_000))
	require.Error(t, err)
	assert.Equal(t, "23514", db.SQLState(err), "a negative spread: %v", err)
	// Within the cap and conserved, the forgery is a row the database cannot
	// tell from a real one -- and the auditor's verification can: it is not
	// what the schedule computes for its input.
	forged, err := forge(big.Charges.Spread, money.QuantityFromInt64(1_000_000))
	require.NoError(t, err)
	_, err = f.svc.VerifyQuoteRecord(f.ctx, testDB, forged)
	assert.Equal(t, ReasonRecordDoesNotVerify, ReasonOf(err))
}

// TestIntegration_TheDatabaseRefusesInvalidFeeStates writes around Go and
// watches the database refuse every invalid schedule, disclosure and booking.
func TestIntegration_TheDatabaseRefusesInvalidFeeStates(t *testing.T) {
	f := newFixture(t)
	quote := f.simAsset("FLQ")
	key := "itest:" + uuid.NewString()
	pub := f.publish(key, f.schedule(quote.ID, "v1"), time.Time{})
	payer := f.customer(quote.ID, units(1_000))
	stored := f.quote(key, quote.ID, &payer, fees.SideBuy, units(2), "100.00")
	b := f.book(stored, payer)

	insertSchedule := func(version, spread int, from, published time.Time) error {
		return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO fee_schedules (id, schedule_key, version, label, quote_asset_id, quote_decimals, spread_bps,
				percentage_fee_bps, max_total_fee_bps, max_total_fee, schedule, schedule_hash, effective_from, published_at,
				created_by_actor_type, created_by_actor_id, reason)
				VALUES ($1,$2,$3,$4,$5,6,$6,10,0,0,$7,$8,$9,$10,'OPERATOR','op','raw')`,
				uuid.New(), key, version, pub.Label, quote.ID, spread, pub.Canonical, pub.Hash, from, published)
			return err
		})
	}
	day := pub.EffectiveFrom.Add(24 * time.Hour)
	assert.Equal(t, "FE001", db.SQLState(insertSchedule(3, 20, day, day)), "a version gap")
	assert.Equal(t, "FE002", db.SQLState(insertSchedule(2, 99, day, day)), "columns that lie about the document")
	err := insertSchedule(2, 20, pub.EffectiveFrom.Add(time.Hour), pub.EffectiveFrom.Add(2*time.Hour))
	assert.Equal(t, "23514", db.SQLState(err), "effective before it was published: %v", err)
	assert.Equal(t, "fee_schedules_not_backdated", db.ConstraintName(err))
	owner, err := pgx.Connect(f.ctx, testMigrateURL)
	require.NoError(t, err)
	defer owner.Close(f.ctx)
	_, err = owner.Exec(f.ctx, `UPDATE fee_schedules SET spread_bps = 0 WHERE id = $1`, pub.ID)
	assert.Equal(t, "P0001", db.SQLState(err), "a published schedule never changes")

	// A disclosure whose columns lie about its document (FE003).
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO fee_quote_disclosures (id, schedule_id, schedule_hash, subsystem, idempotency_key, request_hash,
			quote_asset_id, side, base_quantity, reference_mantissa, reference_scale, execution_mantissa, execution_scale,
			effective_mantissa, effective_scale, notional_at_reference, notional_at_execution, spread_bps, spread_amount, platform_fee,
			strategy_fee, venue_fee, explicit_fees_total, network_fee_asset_id, network_fee, network_in_quote, slippage_reserve,
			total_debit, total_credit, max_total_fee_bps, max_bps_amount, max_total_fee, disclosure, disclosure_hash, quoted_at, valid_until)
			SELECT $1, schedule_id, schedule_hash, 'ORDERS', $2, request_hash, quote_asset_id, side, base_quantity, reference_mantissa,
			       reference_scale, execution_mantissa, execution_scale, effective_mantissa, effective_scale, notional_at_reference,
			       notional_at_reference, spread_bps, 0, platform_fee, strategy_fee, venue_fee, explicit_fees_total, network_fee_asset_id,
			       network_fee, network_in_quote, slippage_reserve, total_debit - spread_amount, 0, max_total_fee_bps, max_bps_amount,
			       max_total_fee, disclosure, disclosure_hash, quoted_at, valid_until
			  FROM fee_quote_disclosures WHERE id = $3`, uuid.New(), "lie-"+uuid.NewString(), stored.ID)
		return err
	})
	assert.Equal(t, "FE003", db.SQLState(err), "a disclosure that hides its spread from its columns: %v", err)

	// A second booking of a disclosure, and a booking whose amounts or
	// journal are not the disclosure's (FE004).
	other := f.quote(key, quote.ID, &payer, fees.SideBuy, units(1), "100.00")
	book := func(spread money.Quantity) error {
		return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO fee_bookings (id, disclosure_id, account_id, mode, booking_mode, journal_kind,
				journal_transaction_id, effect_id, quote_asset_id, network_fee_asset_id, spread_amount, platform_fee, strategy_fee,
				venue_fee, network_fee, status, evidence_ref, evidence_sha256, booked_at)
				SELECT $1, $2, $3, 'PAPER', 'POSTED', 'SIM_TRADE', $4, $5, quote_asset_id, network_fee_asset_id, $6::numeric, platform_fee,
				       strategy_fee, venue_fee, network_fee, 'BOOKED', 'e', $7, quoted_at FROM fee_quote_disclosures WHERE id = $2`,
				uuid.New(), other.ID, payer, b.JournalTransactionID, b.EffectID, spread.String(), make([]byte, 32))
			return err
		})
	}
	assert.Equal(t, "FE004", db.SQLState(book(money.QuantityFromInt64(1))), "amounts that are not the disclosure's")
	assert.Equal(t, "FE004", db.SQLState(book(other.Charges.Spread)), "another booking's journal, which charged other amounts")

	// A booking never changes but by its reversal, and a reversal must be a
	// COMPENSATION of exactly its charges (FE005).
	_, err = owner.Exec(f.ctx, `UPDATE fee_bookings SET spread_amount = 0 WHERE id = $1`, b.ID)
	assert.Equal(t, "FE005", db.SQLState(err))
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO fee_booking_transitions (id, booking_id, from_status, to_status, actor_type, actor_id, reason)
			VALUES ($1,$2,'BOOKED','REVERSED','OPERATOR','op','forged')`, uuid.New(), b.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE fee_bookings SET status = 'REVERSED', reversal_journal_transaction_id = $2, reversal_effect_id = $3,
			reversal_reason_code = 'FORGED', reversed_by_actor_type = 'OPERATOR', reversed_by_actor_id = 'op', reversed_at = now()
			WHERE id = $1`, b.ID, b.JournalTransactionID, b.EffectID)
		return err
	})
	assert.Equal(t, "FE005", db.SQLState(err), "a reversal that names the charge itself: %v", err)
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE fee_bookings SET spread_amount = 0 WHERE id = $1`, b.ID)
		return err
	})
	assert.Equal(t, "42501", db.SQLState(err), "the application cannot even name an amount column")
	require.NoError(t, f.svc.VerifyBooking(f.ctx, testDB, b.ID), "and the booking is as it was")
}
