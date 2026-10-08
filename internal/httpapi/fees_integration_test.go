//go:build integration

package httpapi

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/feeledger"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// feeScheduleBody is a schedule of one quote asset in the API's shape: 20 bps
// of spread, a 0.50 fixed fee, 10 bps of platform fee, a 5 bps venue
// pass-through and a 0.10 network estimate, with the customer-favourable
// rounding defaults.
func feeScheduleBody(label, quote string) map[string]any {
	return map[string]any{"label": label, "quote_asset_id": quote, "quote_decimals": 6, "spread_bps": 20, "fixed_fee": "500000",
		"percentage_fee_bps": 10, "venue_pass_through_bps": 5, "network_fee_asset_id": quote, "network_fee_amount": "100000",
		"slippage_reserve_bps": 0, "subscription_discount_bps": 0, "tiers": []any{}, "strategy_fee_bps": 0, "max_total_fee_bps": 0,
		"max_total_fee": "0", "min_fee": "0",
		"rounding": map[string]any{"buy_notional": "down", "sell_notional": "up", "spread": "down", "percentage_fee": "down",
			"venue_pass_through": "down", "strategy_fee": "down", "fee_cap": "down", "slippage_reserve": "up",
			"allow_platform_favourable": false}}
}

// book charges a recorded quote to its payer through the fee ledger, as the
// subsystem a fee accompanies does: the disclosure's charges posted inside a
// SIM_TRADE journal of the charge's own, applied by an effect, and registered
// with RecordEmbedded. There is no HTTP route that books one.
func (w *treasuryWorld) book(did string, payer accounts.AccountID) {
	w.t.Helper()
	disclosure, err := feeledger.ParseDisclosureID(did)
	require.NoError(w.t, err)
	service := econ.Actor{Type: security.ActorService, ID: "orders-service"}
	effects := econ.NewEffectStore(w.clk, event.NewOutbox(w.clk), audit.NewWriterWithBuildVersion("httpapi-fees-itest"))
	require.NoError(w.t, w.d.InTx(w.t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		q, err := w.fees.Quote(ctx, tx, disclosure)
		if err != nil {
			return err
		}
		ref := uuid.NewString()
		posted, err := w.ledger.Post(ctx, tx, ledger.Posting{Kind: ledger.KindSimTrade, IdempotencyKey: "itest-fee-charge:" + ref,
			Reference: ledger.FinancialEventReference{Type: "itest_fee_charge", ID: ref}, EffectiveAt: w.clk.Now(),
			Entries: feeledger.ChargeEntries(payer, q.Charges)})
		if err != nil {
			return err
		}
		eff, _, err := effects.Begin(ctx, tx, econ.BeginRequest{Key: "itest-fee-charge:" + ref, Subsystem: econ.SubsystemOrders,
			Kind: "PAPER_FILL", AccountID: payer.String(), Request: map[string]string{"disclosure_id": did}, PostsLedger: true, Actor: service})
		if err != nil {
			return err
		}
		if _, err := effects.Apply(ctx, tx, eff.ID, econ.ApplyRequest{JournalTransactionID: &posted.TransactionID, Actor: service}); err != nil {
			return err
		}
		_, err = w.fees.RecordEmbedded(ctx, tx, feeledger.EmbeddedRequest{DisclosureID: disclosure, AccountID: payer,
			JournalTransactionID: posted.TransactionID, EffectID: eff.ID, Mode: gates.ModePaper, Actor: service})
		return err
	}))
}

// TestIntegration_FeesOverHTTP drives the fee ledger through HTTP over the
// real adapter, fee ledger and database: an operator publishes a schedule and
// a later version of it; any customer reads both, with the version in force
// marked, and prices an order exactly under it; quotes recorded for two
// customers are charged to each; each customer sees only their own quotes and
// bookings, every component of every charge and its spread line; an operator
// reverses one booking with a reason code and audits the spread.
func TestIntegration_FeesOverHTTP(t *testing.T) {
	w := newTreasuryWorld(t)
	op := w.operator()
	quote := w.simAsset("FQT")
	alice, alicePrincipal := w.customer(quote.ID, money.QuantityFromInt64(100_000_000))
	bob, bobPrincipal := w.customer(quote.ID, money.QuantityFromInt64(100_000_000))
	key := "orders:paper:itest-" + strings.ToLower(uuid.NewString()[:8])

	// A customer may not publish.
	res := w.post(alicePrincipal, "/v1/advanced/fees/schedules", map[string]any{"key": key,
		"schedule": feeScheduleBody("fees-v1", quote.ID.String()), "reason": "mine"}, "cust-publish-001")
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())

	res = w.post(op, "/v1/advanced/fees/schedules", map[string]any{"key": key, "schedule": feeScheduleBody("fees-v1", quote.ID.String()),
		"reason": "the first schedule"}, "publish-v1-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var v1 api.FeeScheduleVersion
	res.json(&v1)
	assert.Equal(t, 1, v1.Version)
	assert.True(t, v1.InForce)
	later := w.clk.Now().Add(time.Hour).UTC()
	res = w.post(op, "/v1/advanced/fees/schedules", map[string]any{"key": key, "schedule": feeScheduleBody("fees-v2", quote.ID.String()),
		"effective_from": later.Format(time.RFC3339), "reason": "the next schedule"}, "publish-v2-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var v2 api.FeeScheduleVersion
	res.json(&v2)
	assert.Equal(t, 2, v2.Version)
	assert.False(t, v2.InForce, "it takes effect in an hour")

	// A version never takes effect in the past.
	res = w.post(op, "/v1/advanced/fees/schedules", map[string]any{"key": key, "schedule": feeScheduleBody("fees-v0", quote.ID.String()),
		"effective_from": w.clk.Now().Add(-time.Hour).UTC().Format(time.RFC3339), "reason": "backdated"}, "publish-past-001")
	require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, feeledger.ReasonBackdated, res.problem().Fields["reason"])

	// Published schedules are any customer's to read, with their history.
	res = w.get(bobPrincipal, "/v1/advanced/fees/schedules/history?key="+url.QueryEscape(key))
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var history api.FeeScheduleHistory
	res.json(&history)
	require.Len(t, history.Versions, 2)
	require.NotNil(t, history.EffectiveVersion)
	assert.Equal(t, 1, *history.EffectiveVersion)
	assert.Equal(t, "fees-v1", history.Versions[0].Schedule.Label)
	assert.Equal(t, "500000", history.Versions[0].Schedule.FixedFee)
	assert.False(t, history.Versions[1].InForce)
	res = w.get(bobPrincipal, "/v1/advanced/fees/schedules?limit=200")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var keys api.FeeScheduleSummaryPage
	res.json(&keys)
	listed := false
	for _, k := range keys.Items {
		if k.Key == key {
			listed = true
			assert.Equal(t, 2, k.LatestVersion)
			require.NotNil(t, k.Effective)
			assert.Equal(t, 1, k.Effective.Version)
		}
	}
	assert.True(t, listed, "the key is listed")

	// The calculator: 10 units at 100.00 under version 1, exactly.
	q := url.Values{}
	q.Set("schedule_key", key)
	q.Set("side", "BUY")
	q.Set("base_quantity", "10000000")
	q.Set("base_decimals", "6")
	q.Set("reference_price", "100.00")
	res = w.get(alicePrincipal, "/v1/advanced/fees/quote-preview?"+q.Encode())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var preview api.FeeQuotePreview
	res.json(&preview)
	assert.False(t, preview.Recorded)
	assert.Equal(t, "fees-v1", preview.Schedule.Label)
	assert.Equal(t, "1000000000", preview.Disclosure.NotionalAtReference)
	assert.Equal(t, "2000000", preview.Charges.Spread)
	assert.Equal(t, "1500000", preview.Charges.Platform, "a 0.50 fixed fee plus 10 bps of 1,000.00")
	assert.Equal(t, "500000", preview.Charges.Venue)
	assert.Equal(t, "100000", preview.Charges.Network)
	assert.Equal(t, "1004100000", preview.Disclosure.TotalDebit)
	q.Set("schedule_key", "orders:paper:no-such-key")
	res = w.get(alicePrincipal, "/v1/advanced/fees/quote-preview?"+q.Encode())
	require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, feeledger.ReasonScheduleNotFound, res.problem().Fields["reason"])

	// An operator records a quote for each customer; each is charged it.
	record := func(payer accounts.AccountID, idem string) api.FeeQuoteRecord {
		res := w.post(op, "/v1/advanced/fees/quotes", map[string]any{"schedule_key": key, "side": "BUY", "base_quantity": "10000000",
			"base_decimals": 6, "reference_price": "100.00", "subsystem": "ORDERS", "mode": "PAPER", "account_id": payer.String()}, idem)
		require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
		var out api.FeeQuoteRecord
		res.json(&out)
		return out
	}
	aliceQuote, bobQuote := record(alice, "quote-alice-0001"), record(bob, "quote-bob-000001")
	assert.Equal(t, "1004100000", aliceQuote.Disclosure.TotalDebit, "the recorded quote is the calculator's")
	w.book(aliceQuote.Id.String(), alice)
	w.book(bobQuote.Id.String(), bob)

	// Each customer sees their own, and only their own.
	res = w.get(alicePrincipal, "/v1/advanced/fees/bookings?account_id="+alice.String())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var bookings api.FeeBookingPage
	res.json(&bookings)
	require.Len(t, bookings.Items, 1)
	b := bookings.Items[0]
	assert.Equal(t, aliceQuote.Id, b.DisclosureId)
	assert.Equal(t, "2000000", b.Charges.Spread)
	assert.Equal(t, "2000000", b.SpreadLine.SpreadAmount)
	assert.Equal(t, 20, b.SpreadLine.SpreadBps)
	assert.Equal(t, "10000", b.SpreadLine.ReferencePrice.Mantissa)
	assert.Equal(t, "10020", b.SpreadLine.ExecutionPrice.Mantissa, "the reference price moved by exactly the spread")
	assert.Equal(t, api.FeeBookingBooked, b.Status)
	res = w.get(alicePrincipal, "/v1/advanced/fees/bookings?account_id="+bob.String())
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	res = w.get(alicePrincipal, "/v1/advanced/fees/quotes?account_id="+alice.String())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var quotes api.FeeQuotePage
	res.json(&quotes)
	require.Len(t, quotes.Items, 1)
	assert.Equal(t, aliceQuote.Id, quotes.Items[0].Id)

	res = w.get(bobPrincipal, "/v1/advanced/fees/bookings?account_id="+bob.String())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var bobs api.FeeBookingPage
	res.json(&bobs)
	require.Len(t, bobs.Items, 1)
	bobBooking := bobs.Items[0].Id
	res = w.get(alicePrincipal, "/v1/advanced/fees/bookings/"+bobBooking.String())
	require.Equal(t, http.StatusNotFound, res.Code, "another customer's booking reads as absent; body=%s", res.Body.String())
	res = w.get(alicePrincipal, "/v1/advanced/fees/quotes/"+bobQuote.Id.String())
	require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())

	// A booking re-proves from the record.
	res = w.get(alicePrincipal, "/v1/advanced/fees/bookings/"+b.Id.String())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var detail api.FeeBookingDetail
	res.json(&detail)
	assert.True(t, detail.Verification.Verified, "%+v", detail.Verification)
	assert.Equal(t, "1004100000", detail.Disclosure.TotalDebit)

	// An operator reverses Alice's booking with a reason code.
	res = w.post(op, "/v1/advanced/fees/bookings/"+b.Id.String()+"/reverse", map[string]any{"reason_code": "GOODWILL_REFUND",
		"reason": "integration test reversal"}, "reverse-booking-1")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var reversed api.FeeBooking
	res.json(&reversed)
	assert.Equal(t, api.FeeBookingReversed, reversed.Status)
	require.NotNil(t, reversed.ReversalReasonCode)
	assert.Equal(t, "GOODWILL_REFUND", *reversed.ReversalReasonCode)

	// The spread audit and totals in the quote asset: both charged, one
	// reversed.
	res = w.get(op, "/v1/advanced/fees/spread-totals?quote_asset_id="+quote.ID.String())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var totals api.FeeSpreadTotals
	res.json(&totals)
	assert.Equal(t, "4000000", totals.Booked)
	assert.Equal(t, "2000000", totals.Reversed)
	res = w.get(op, "/v1/advanced/fees/spread-audit?include_reversed=true&quote_asset_id="+quote.ID.String())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var audit api.FeeSpreadAudit
	res.json(&audit)
	require.Len(t, audit.Items, 2)
	res = w.get(alicePrincipal, "/v1/advanced/fees/spread-totals?quote_asset_id="+quote.ID.String())
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
}

// TestIntegration_PerformanceFeesOverHTTP: a performance-fee period accrues
// and crystallizes only above its high-water mark. Valued below it, nothing
// accrues; above it, 20% of the gain does; crystallization at the end charges
// the true fee and the next period chains from the net NAV, where a period
// that ends below its high-water mark charges nothing. Customers read their
// own periods; a period another one continues cannot be reversed.
func TestIntegration_PerformanceFeesOverHTTP(t *testing.T) {
	w := newTreasuryWorld(t)
	op := w.operator()
	acct, owner := w.customer(w.usd.ID, money.QuantityFromInt64(1_000_000_000))
	_, stranger := w.customer(w.usd.ID, money.Quantity{})
	start := w.clk.Now().UTC()
	end := start.Add(10 * 24 * time.Hour)
	at := func(d time.Duration) string { return start.Add(d).Format(time.RFC3339) }

	res := w.post(op, "/v1/advanced/fees/periods", map[string]any{"account_id": acct.String(), "strategy_ref": "strat-itest",
		"terms_version": "perf-v1", "period_start": start.Format(time.RFC3339), "period_end": end.Format(time.RFC3339),
		"opening_nav": "10000.00", "hurdle_annual_bps": 0, "fee_rate_bps": 2000}, "open-period-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var first api.FeePeriod
	res.json(&first)
	assert.Equal(t, api.FeePeriodOpen, first.Status)
	assert.Equal(t, "10000.00", first.Terms.PriorHwm)
	period := "/v1/advanced/fees/periods/" + first.Id.String()

	accrue := func(path, when, nav, idem string) api.FeePerformanceLine {
		t.Helper()
		res := w.post(op, path+"/accruals", map[string]any{"at": when, "nav": nav}, idem)
		require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
		var out api.FeeLineResult
		res.json(&out)
		return out.Line
	}
	below := accrue(period, at(2*24*time.Hour), "9500.00", "accrue-below-001")
	assert.Equal(t, "0.00", below.Amount, "below the high-water mark nothing accrues")
	assert.Equal(t, "0.00", below.FeeBase)
	above := accrue(period, at(4*24*time.Hour), "11000.00", "accrue-above-001")
	assert.Equal(t, api.FeeLineAccrual, above.Kind)
	assert.Equal(t, "200.00", above.Amount, "20% of the 1,000.00 above the high-water mark")
	assert.Equal(t, "1000.00", above.FeeBase)

	// Not before the period's end.
	res = w.post(op, period+"/crystallize", map[string]any{"closing_nav": "10800.00"}, "crystallize-early1")
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, feeledger.ReasonPeriodNotEnded, res.problem().Fields["reason"])

	w.clk.Advance(11 * 24 * time.Hour)
	res = w.post(op, period+"/crystallize", map[string]any{"closing_nav": "10800.00"}, "crystallize-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var crystallized api.FeePeriod
	res.json(&crystallized)
	assert.Equal(t, api.FeePeriodCrystallized, crystallized.Status)
	require.NotNil(t, crystallized.Statement)
	assert.Equal(t, "160.00", crystallized.Statement.FeeAmount, "20% of the 800.00 above the high-water mark at the end")
	assert.Equal(t, "10640.00", crystallized.Statement.HwmAfter, "the next period opens at the net NAV")

	// The owner reads it, re-proved; a stranger is told it does not exist.
	res = w.get(owner, period)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var read api.FeePeriod
	res.json(&read)
	require.NotNil(t, read.Verification)
	assert.True(t, read.Verification.Verified, "%+v", read.Verification)
	res = w.get(stranger, period)
	require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
	res = w.get(owner, "/v1/advanced/fees/periods?account_id="+acct.String()+"&strategy_ref=strat-itest")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var periods api.FeePeriodPage
	res.json(&periods)
	require.Len(t, periods.Items, 1)

	// The next period chains from the statement and charges nothing below it.
	nextEnd := end.Add(10 * 24 * time.Hour)
	res = w.post(op, "/v1/advanced/fees/periods", map[string]any{"account_id": acct.String(), "strategy_ref": "strat-itest",
		"terms_version": "perf-v1", "period_end": nextEnd.Format(time.RFC3339), "hurdle_annual_bps": 0, "fee_rate_bps": 2000},
		"open-period-0002")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var second api.FeePeriod
	res.json(&second)
	assert.Equal(t, "10640.00", second.Terms.PriorHwm)
	assert.Equal(t, "10640.00", second.Terms.OpeningNav)
	require.NotNil(t, second.PreviousPeriodId)
	assert.Equal(t, first.Id, *second.PreviousPeriodId)
	next := "/v1/advanced/fees/periods/" + second.Id.String()
	line := accrue(next, end.Add(2*24*time.Hour).Format(time.RFC3339), "10600.00", "accrue-next-0001")
	assert.Equal(t, "0.00", line.Amount, "below the carried high-water mark nothing accrues")
	w.clk.Advance(11 * 24 * time.Hour)
	res = w.post(op, next+"/crystallize", map[string]any{"closing_nav": "10600.00"}, "crystallize-0002")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var flat api.FeePeriod
	res.json(&flat)
	require.NotNil(t, flat.Statement)
	assert.Equal(t, "0.00", flat.Statement.FeeAmount, "no fee below the high-water mark")
	assert.Equal(t, "10640.00", flat.Statement.HwmAfter, "the high-water mark never falls")

	// A crystallization another period continues from stays.
	res = w.post(op, period+"/reverse", map[string]any{"reason_code": "NAV_RESTATED", "reason": "restated"}, "reverse-first-001")
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	p := res.problem()
	assert.Equal(t, errs.CodeConflict, p.Code)
	assert.Equal(t, feeledger.ReasonPeriodContinued, p.Fields["reason"])
}
