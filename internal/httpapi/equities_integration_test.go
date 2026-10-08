//go:build integration

package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/equities"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/feeledger"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuation"
)

// eqQuantityOf parses a rendered base-unit integer.
func eqQuantityOf(t *testing.T, s string) money.Quantity {
	t.Helper()
	q, err := money.ParseQuantity(s)
	require.NoError(t, err, "a rendered quantity %q", s)
	return q
}

// equityWednesday10 is 10:00 New York time on Wednesday 2026-10-07; a trade
// then settles on Thursday 2026-10-08.
var equityWednesday10 = time.Date(2026, 10, 7, 14, 0, 0, 0, time.UTC)

// TestIntegration_EquitiesBuySettlesTPlusOneThroughHTTP drives the equities
// routes over the real adapter, service, paper venue and database, wired the
// way cmd/api wires them: an operator lists an equity, a customer's market
// buy is accepted, placed and filled in one request, the shares wait in
// UNSETTLED_RECEIVABLE -- not spendable, so selling them is refused -- until
// the operator's settlement run on the settlement date settles them into the
// WALLET; a resting limit order fills on the operator's venue run when the
// mark crosses; a stranger reads none of it.
func TestIntegration_EquitiesBuySettlesTPlusOneThroughHTTP(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	clk := clock.NewFake(equityWednesday10)

	// --- wiring, as cmd/api does it ---------------------------------------
	outbox := event.NewOutbox(clk)
	aw := audit.NewWriterWithBuildVersion("httpapi-itest")
	led := ledger.NewService(clk, "httpapi-itest")
	checker, err := gates.NewChecker("TEST", func(gates.Capability) bool { return false }, clk)
	require.NoError(t, err)
	kills := killswitch.NewChecker(killswitch.Policy{})
	router, err := legalrouter.New(legalrouter.ConservativePolicy())
	require.NoError(t, err)
	prices := valuation.NewPriceStore(clk)
	require.NoError(t, d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := econ.EnsureSandboxDollar(ctx, tx)
		return err
	}))
	holidays, err := equities.Holidays(ctx, d, equities.CalendarXNYS)
	require.NoError(t, err)
	cal, err := equities.NewCalendar(equities.CalendarXNYS, holidays)
	require.NoError(t, err)
	venue, err := equities.NewExecutionAdapter(config.EnvTest, config.ProviderModeFake, equities.PaperConfig{
		Name: "nodal-paper-equities", Calendar: cal, SlippageBPS: 10,
	})
	require.NoError(t, err)
	capitalSvc := capital.NewService(clk, capital.OutboxEmitter{Outbox: outbox, Clock: clk})
	// The fee ledger every fill's spread and fees are booked through, as
	// cmd/api builds it.
	feeLedger, err := feeledger.NewService(feeledger.Config{
		Clock: clk, Ledger: led, Capital: capitalSvc, Effects: econ.NewEffectStore(clk, outbox, aw),
		Authorizer: econ.NewAuthorizer(checker, kills), Assets: assets.NewRepository(), Audit: aw,
	})
	require.NoError(t, err)
	svc, err := equities.NewService(equities.Config{
		Clock: clk, Intents: econ.NewIntentStore(clk, outbox, aw), Effects: econ.NewEffectStore(clk, outbox, aw),
		Authorizer: econ.NewAuthorizer(checker, kills), KillSwitches: kills, Router: router,
		RiskPolicy: risk.DefaultAdvancedPolicy(), Capital: capitalSvc,
		Ledger: led, Prices: prices, Audit: aw, Adapter: venue,
		Schedules: []fees.Schedule{{
			Version: "equities-sandbox-v1", QuoteAsset: econ.SandboxDollarID(), QuoteDecimals: econ.SandboxDollarDecimals,
			SpreadBPS: 10, PercentageFeeBPS: 10, StrategyFeeBPS: 5, SlippageReserveBPS: 50, Rounding: fees.DefaultRounding(),
		}},
		FeeLedger:       feeLedger,
		MarketCollarBPS: 100, MinOrderNotional: money.USDFromMinor(1_00),
		Verification: AdvancedVerificationLevels{Rule: compliance.NewAccountVerification(clk)},
	})
	require.NoError(t, err)
	ports, err := Wire(WireDeps{DB: d, Clock: clk, Env: config.EnvTest, Advanced: AdvancedDeps{
		Equities: EquitiesDeps{Service: svc, Prices: prices},
	}})
	require.NoError(t, err)
	require.NotNil(t, ports.Equities, "the service wires the port")

	user, cust := booksVerifiedCustomer(t, d)
	strangerUser, strangerAcct := booksVerifiedCustomer(t, d)
	h := newIntegrationHarness(t, d, cust, user)
	h.server.opts.Ports.Equities = ports.Equities
	asCust, asStranger := booksCustomerOf(user, cust), booksCustomerOf(strangerUser, strangerAcct)
	operator := booksOperatorNow()
	key := func(name string) string { return "itest-" + name + "-" + uuid.NewString() }
	post := func(p security.Principal, path string, body any, k string) *response {
		t.Helper()
		p.AuthTime = time.Now().UTC()
		return h.as(&p).do(http.MethodPost, path, body, "Idempotency-Key", k)
	}
	get := func(p security.Principal, path string) *response {
		t.Helper()
		p.AuthTime = time.Now().UTC()
		return h.as(&p).do(http.MethodGet, path, nil)
	}
	usd := econ.SandboxDollarID()
	balance := func(code ledger.Code, asset assets.AssetID) string {
		t.Helper()
		b, err := led.Balance(ctx, d, ledger.CustomerAccount(cust, code, asset))
		require.NoError(t, err)
		return b.String()
	}
	const base = "/v1/advanced/equities"

	// --- an operator lists an equity in the sandbox dollar -----------------------
	symbol := "EQ" + strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", "")[:8])
	listing := map[string]any{
		"kind": "EQUITY", "symbol": symbol, "name": "Example Corp", "exchange_mic": "XNAS", "share_decimals": 6,
		"price_scale": 2, "tick_size": "1", "lot_size": "1", "min_quantity": "1", "oracle_max_age_ms": 60000,
		"oracle_sources": []string{"httpapi-itest"}, "risk_class": "STANDARD",
	}
	res := post(asCust, base+"/listings", listing, key("list-cust"))
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	res = post(operator, base+"/listings", listing, key("list"))
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	var listed api.AdvancedEquityListing
	res.json(&listed)
	assert.Equal(t, api.SIMULATED, listed.ValueDomain)
	assert.Equal(t, usd.String(), listed.SettlementAssetId.String(), "the sandbox dollar by default")
	assert.Equal(t, "0.01", listed.TickPrice)
	assert.Equal(t, "nodal-paper-equities", listed.Venue)
	shareAsset, err := assets.ParseAssetID(listed.ShareAssetId.String())
	require.NoError(t, err)
	inst := listed.InstrumentId.String()

	// A mark of one share, observed a second ago; the clock moves on a
	// second first, so each mark is its own observation.
	mark := func(cents int64) {
		t.Helper()
		clk.Advance(time.Second)
		_, err := prices.RecordPrice(ctx, d, valuation.PriceObservation{
			AssetID: shareAsset, QuoteAssetID: usd,
			Mantissa: money.QuantityFromInt64(cents), Scale: 2, Source: "httpapi-itest", ObservedAt: clk.Now().Add(-time.Second),
		})
		require.NoError(t, err)
	}
	mark(15_000)

	res = get(asCust, base+"/listings/"+inst)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var one api.AdvancedEquityListing
	res.json(&one)
	require.NotNil(t, one.Mark)
	assert.Equal(t, "150.00", one.Mark.Price)
	res = get(asCust, base+"/listings")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var list api.AdvancedEquityListingList
	res.json(&list)
	assert.Equal(t, api.EquitySessionRegular, list.Session.Session)
	assert.Equal(t, "2026-10-07", list.Session.TradeDate)
	require.NotNil(t, list.Session.SettlementDate)
	assert.Equal(t, "2026-10-08", *list.Session.SettlementDate)

	// --- the customer's market buy fills in one request ---------------------------
	booksFund(t, d, led, cust, usd, 1_000_000_000) // 1,000 sandbox dollars
	buy := map[string]any{
		"account_id": cust.String(), "instrument_id": inst, "side": "BUY", "order_type": "MARKET",
		"quantity": "2000000", "mode": "PAPER",
	}
	buyKey := key("buy")
	first := post(asCust, base+"/orders", buy, buyKey)
	require.Equal(t, http.StatusCreated, first.Code, "body=%s", first.Body.String())
	var submitted api.AdvancedEquityOrderSubmission
	first.json(&submitted)
	assert.Equal(t, api.SIMULATED, submitted.ValueDomain)
	assert.Equal(t, api.EquityPlacementPlaced, submitted.Placement.Status)
	require.NotNil(t, submitted.Placement.VenueStatus)
	assert.Equal(t, api.EquityVenueFilled, *submitted.Placement.VenueStatus)
	bought := submitted.Order
	assert.Equal(t, api.EquityOrderFilled, bought.Status)
	require.NotNil(t, bought.Fill)
	f := *bought.Fill
	assert.Equal(t, "150.15", f.Price, "the mark plus the disclosed 10 bps of slippage, on the tick grid")
	assert.Equal(t, "300300000", f.Notional, "2 shares at 150.15 is 300.30 sandbox dollars, exactly")
	notional, spread := eqQuantityOf(t, f.Notional), eqQuantityOf(t, f.Spread)
	platform, venueFees := eqQuantityOf(t, f.PlatformFees), eqQuantityOf(t, f.VenueFees)
	total := eqQuantityOf(t, f.Total)
	assert.Equal(t, notional.Add(spread).Add(platform).Add(venueFees).String(), total.String(),
		"a buyer pays the notional and every disclosed charge, and nothing else")
	assert.True(t, spread.IsPositive() && platform.IsPositive(), "the schedule's spread and fee are charged and disclosed")
	assert.Equal(t, "0", f.VenueFees, "the paper venue charges nothing to pass through")
	assert.Equal(t, "2026-10-08", f.SettlementDate)
	require.NotNil(t, f.Settlement)
	assert.Equal(t, api.EquitySettlementPending, f.Settlement.State)
	assert.Equal(t, "2000000", f.Settlement.Quantity)
	assert.Equal(t, shareAsset.String(), f.Settlement.AssetId.String())

	// The same key and body is the same answer and the same single order.
	again := post(asCust, base+"/orders", buy, buyKey)
	require.Equal(t, http.StatusOK, again.Code, "body=%s", again.Body.String())
	assert.JSONEq(t, first.Body.String(), again.Body.String())

	// Paid on the trade date; the shares wait in UNSETTLED_RECEIVABLE.
	paid := money.QuantityFromInt64(1_000_000_000).Sub(total)
	assert.Equal(t, paid.String(), balance(ledger.CodeWallet, usd))
	assert.Equal(t, "0", balance(ledger.CodeWallet, shareAsset), "nothing is spendable before settlement")
	assert.Equal(t, "2000000", balance(ledger.CodeUnsettledReceivable, shareAsset))

	// --- unsettled shares cannot be sold -----------------------------------------
	sell := map[string]any{
		"account_id": cust.String(), "instrument_id": inst, "side": "SELL", "order_type": "MARKET",
		"quantity": "2000000", "mode": "PAPER",
	}
	res = post(asCust, base+"/orders", sell, key("early-sell"))
	require.Equal(t, http.StatusUnprocessableEntity, res.Code, "body=%s", res.Body.String())
	early := res.problem()
	assert.Equal(t, errs.CodeInsufficientBuyingPower, early.Code)
	assert.Equal(t, string(equities.RejectInsufficientBuyingPower), early.Fields["rejection_code"])
	earlyOrder, _ := early.Fields["order_id"].(string)
	require.NotEmpty(t, earlyOrder, "the refusal names the REJECTED order it recorded")
	res = get(asCust, base+"/orders/"+earlyOrder)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, "REJECTED", res.raw()["status"])

	res = get(asCust, base+"/settlements?state=PENDING&account_id="+cust.String())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var pending api.AdvancedEquitySettlementPage
	res.json(&pending)
	require.Len(t, pending.Items, 1)
	assert.Equal(t, "2026-10-08", pending.Items[0].DueDate)

	// --- the operator's settlement run settles nothing before the due date ------
	res = post(asCust, base+"/settlement-runs", map[string]any{}, key("settle-cust"))
	require.Equal(t, http.StatusForbidden, res.Code, "a customer runs no worker; body=%s", res.Body.String())
	res = post(operator, base+"/settlement-runs", map[string]any{}, key("settle-early"))
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var run api.AdvancedEquitySettlementRun
	res.json(&run)
	assert.Empty(t, run.Settled, "nothing is due on the trade date")
	assert.Equal(t, "0", balance(ledger.CodeWallet, shareAsset))

	// --- on the settlement date it settles, exactly once --------------------------
	clk.Set(equityWednesday10.Add(24 * time.Hour))
	res = post(operator, base+"/settlement-runs", map[string]any{}, key("settle"))
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	res.json(&run)
	assert.Equal(t, "2026-10-08", run.TradeDate)
	require.Len(t, run.Settled, 1)
	assert.Equal(t, api.EquitySettlementSettled, run.Settled[0].State)
	assert.NotNil(t, run.Settled[0].JournalTransactionId)
	assert.Equal(t, "2000000", balance(ledger.CodeWallet, shareAsset), "settled: the shares are the customer's to spend")
	assert.Equal(t, "0", balance(ledger.CodeUnsettledReceivable, shareAsset))
	res = post(operator, base+"/settlement-runs", map[string]any{}, key("settle-again"))
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	res.json(&run)
	assert.Empty(t, run.Settled, "a settlement settles once")

	res = get(asCust, base+"/orders/"+bought.Id.String())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var settledOrder api.AdvancedEquityOrder
	res.json(&settledOrder)
	require.NotNil(t, settledOrder.Fill)
	require.NotNil(t, settledOrder.Fill.Settlement)
	assert.Equal(t, api.EquitySettlementSettled, settledOrder.Fill.Settlement.State)

	// --- now the shares sell; the proceeds wait for their own T+1 ---------------------
	mark(15_100)
	res = post(asCust, base+"/orders", sell, key("sell"))
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	var sold api.AdvancedEquityOrderSubmission
	res.json(&sold)
	require.Equal(t, api.EquityOrderFilled, sold.Order.Status)
	require.NotNil(t, sold.Order.Fill)
	require.NotNil(t, sold.Order.Fill.Settlement)
	proceeds := sold.Order.Fill.Settlement
	assert.Equal(t, usd.String(), proceeds.AssetId.String(), "a seller is owed sandbox dollars")
	assert.Equal(t, "2026-10-09", proceeds.DueDate)
	assert.Equal(t, sold.Order.Fill.Total, proceeds.Quantity, "the proceeds net of the disclosed charges")
	assert.Equal(t, "0", balance(ledger.CodeWallet, shareAsset))
	assert.Equal(t, paid.String(), balance(ledger.CodeWallet, usd), "proceeds are not spendable before settlement")
	assert.Equal(t, proceeds.Quantity, balance(ledger.CodeUnsettledReceivable, usd))

	// --- a resting limit order fills on the operator's venue run ----------------------
	limit := map[string]any{
		"account_id": cust.String(), "instrument_id": inst, "side": "BUY", "order_type": "LIMIT",
		"quantity": "1000000", "limit_price": "140.00", "mode": "PAPER",
	}
	res = post(asCust, base+"/orders", limit, key("limit"))
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	var rested api.AdvancedEquityOrderSubmission
	res.json(&rested)
	assert.Equal(t, api.EquityOrderOpen, rested.Order.Status)
	require.NotNil(t, rested.Placement.VenueStatus)
	assert.Equal(t, api.EquityVenueOpen, *rested.Placement.VenueStatus, "the mark is above the limit: it rests")
	require.NotNil(t, rested.Order.LimitPrice)
	assert.Equal(t, "140.00", *rested.Order.LimitPrice)

	mark(13_900)
	res = post(operator, base+"/venue-runs", map[string]any{}, key("venue"))
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var vr api.AdvancedEquityVenueRun
	res.json(&vr)
	var polled *api.AdvancedEquityVenueRunItem
	for i := range vr.Items {
		if vr.Items[i].OrderId == rested.Order.Id {
			polled = &vr.Items[i]
		}
	}
	require.NotNil(t, polled, "the venue run worked the resting order")
	assert.Equal(t, api.EquityVenueActionPoll, polled.Action)
	require.NotNil(t, polled.StatusAfter)
	assert.Equal(t, api.EquityOrderFilled, *polled.StatusAfter, "the mark crossed the limit")
	res = get(asCust, base+"/orders/"+rested.Order.Id.String())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var filled api.AdvancedEquityOrder
	res.json(&filled)
	require.NotNil(t, filled.Fill)
	assert.Equal(t, "139.14", filled.Fill.Price, "the slipped mark, never above the limit")

	// --- a resting order is cancelled at the venue, its reservation released ------------
	low := map[string]any{
		"account_id": cust.String(), "instrument_id": inst, "side": "BUY", "order_type": "LIMIT",
		"quantity": "1000000", "limit_price": "100.00", "mode": "PAPER",
	}
	res = post(asCust, base+"/orders", low, key("low"))
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	var lowOrder api.AdvancedEquityOrderSubmission
	res.json(&lowOrder)
	require.Equal(t, api.EquityOrderOpen, lowOrder.Order.Status)
	cancelPath := base + "/orders/" + lowOrder.Order.Id.String() + "/cancel"
	res = post(asStranger, cancelPath, map[string]any{"account_id": strangerAcct.String()}, key("steal"))
	require.Equal(t, http.StatusNotFound, res.Code, "a stranger's cancel: body=%s", res.Body.String())
	res = post(asCust, cancelPath, map[string]any{"account_id": cust.String()}, key("cancel"))
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, "CANCELLED", res.raw()["status"])
	var reservationStatus string
	require.NoError(t, d.Pool().QueryRow(ctx, `SELECT status FROM asset_reservations WHERE id = $1`,
		lowOrder.Order.ReservationId).Scan(&reservationStatus))
	assert.Equal(t, "RELEASED", reservationStatus)

	// --- the account's reads, and a stranger's ---------------------------------------
	res = get(asCust, base+"/orders?account_id="+cust.String())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var orderPage api.AdvancedEquityOrderPage
	res.json(&orderPage)
	assert.Len(t, orderPage.Items, 5, "the buy, the refused sell, the sell, the filled limit and the cancelled one")
	res = get(asCust, base+"/fills?account_id="+cust.String())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var fills api.AdvancedEquityFillPage
	res.json(&fills)
	assert.Len(t, fills.Items, 3)
	res = get(asCust, base+"/settlements?state=SETTLED&account_id="+cust.String())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var settledPage api.AdvancedEquitySettlementPage
	res.json(&settledPage)
	require.Len(t, settledPage.Items, 1)

	for _, path := range []string{"/orders", "/fills", "/settlements", "/corporate-action-applications"} {
		res = get(asStranger, base+path+"?account_id="+cust.String())
		require.Equal(t, http.StatusForbidden, res.Code, "%s: body=%s", path, res.Body.String())
		assert.Equal(t, errs.CodeForbidden, res.problem().Code)
	}
	res = get(asStranger, base+"/orders/"+bought.Id.String())
	require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeNotFound, res.problem().Code)
	assert.NotContains(t, res.Body.String(), cust.String())

	// --- a corporate action is announced, listed and withdrawn ---------------------------
	dividend := map[string]any{
		"instrument_id": inst, "type": "CASH_DIVIDEND", "dividend_per_share": "0.25", "ex_date": "2026-10-09",
		"record_date": "2026-10-09", "pay_date": "2026-10-13", "reason": "itest quarterly dividend",
	}
	res = post(operator, base+"/corporate-actions", dividend, key("announce"))
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	var announced api.AdvancedEquityCorporateAction
	res.json(&announced)
	assert.Equal(t, api.EquityActionAnnounced, announced.Status)
	assert.Equal(t, "2026-10-09", announced.EffectiveDate, "a dividend takes effect on its ex-date")
	require.NotNil(t, announced.DividendPerShare)
	assert.Equal(t, "0.25", *announced.DividendPerShare)
	res = get(asCust, base+"/corporate-actions?instrument_id="+inst)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var actions api.AdvancedEquityCorporateActionPage
	res.json(&actions)
	require.Len(t, actions.Items, 1)
	res = post(operator, base+"/corporate-actions/"+announced.Id.String()+"/cancel", map[string]any{"reason": "mis-announced"}, key("withdraw"))
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, "CANCELLED", res.raw()["status"])

	// --- Friday: the sale's proceeds and the limit buy's share settle -------------------
	clk.Set(equityWednesday10.Add(48 * time.Hour))
	res = post(operator, base+"/settlement-runs", map[string]any{}, key("settle-friday"))
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	res.json(&run)
	assert.Equal(t, "2026-10-09", run.TradeDate)
	assert.Len(t, run.Settled, 2, "the sale's proceeds and the limit buy's share")
	spendable := paid.Sub(eqQuantityOf(t, filled.Fill.Total)).Add(eqQuantityOf(t, proceeds.Quantity))
	assert.Equal(t, spendable.String(), balance(ledger.CodeWallet, usd), "settled proceeds are spendable")
	assert.Equal(t, "0", balance(ledger.CodeUnsettledReceivable, usd))
	assert.Equal(t, "1000000", balance(ledger.CodeWallet, shareAsset))

	// --- an operator's holiday and the calendar a customer reads ------------------------
	holiday := map[string]any{"date": "2031-03-05", "name": "Itest closure"}
	res = post(asCust, base+"/calendar/holidays", holiday, key("holiday-cust"))
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	res = post(operator, base+"/calendar/holidays", holiday, key("holiday"))
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	res = get(asCust, base+"/calendar")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var calendar api.AdvancedEquityCalendar
	res.json(&calendar)
	assert.Equal(t, 1, calendar.SettlementCycleDays)
	assert.Equal(t, "2026-10-09", calendar.State.TradeDate)
	assert.Equal(t, "2026-10-12", calendar.State.NextTradingDay)
	var closure bool
	for _, h := range calendar.Holidays {
		closure = closure || (h.Date == "2031-03-05" && h.Name == "Itest closure")
	}
	assert.True(t, closure, "the recorded holiday is in the calendar")

	// --- a two-for-one split applies to the settled holding, once ---------------------
	split := map[string]any{
		"instrument_id": inst, "type": "SPLIT", "ratio": map[string]any{"numerator": 2, "denominator": 1},
		"effective_date": "2026-10-09", "reason": "itest two-for-one split",
	}
	res = post(operator, base+"/corporate-actions", split, key("split"))
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	var splitAction api.AdvancedEquityCorporateAction
	res.json(&splitAction)
	applyPath := base + "/corporate-actions/" + splitAction.Id.String() + "/apply"
	res = post(asCust, applyPath, nil, key("apply-cust"))
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	res = post(operator, applyPath, nil, key("apply"))
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var applied api.AdvancedEquityCorporateActionResult
	res.json(&applied)
	assert.Equal(t, api.EquityActionApplied, applied.Action.Status)
	assert.False(t, applied.Replayed)
	require.Len(t, applied.Applications, 1)
	app := applied.Applications[0]
	assert.Equal(t, cust.String(), app.AccountId.String())
	assert.Equal(t, "1000000", app.EntitledQuantity)
	assert.Equal(t, "1000000", app.SharesDelta, "one share becomes two")
	assert.Equal(t, "0", app.SettlementAmount, "a whole ratio leaves nothing in lieu")
	assert.Equal(t, "2000000", balance(ledger.CodeWallet, shareAsset))
	res = post(operator, applyPath, nil, key("apply-again"))
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	res.json(&applied)
	assert.True(t, applied.Replayed, "an applied action posts nothing again")
	require.Len(t, applied.Applications, 1)
	assert.Equal(t, app.Id, applied.Applications[0].Id)
	assert.Equal(t, "2000000", balance(ledger.CodeWallet, shareAsset))

	res = get(asCust, base+"/corporate-action-applications?account_id="+cust.String())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var apps api.AdvancedEquityApplicationPage
	res.json(&apps)
	require.Len(t, apps.Items, 1)
	assert.Equal(t, app.Id, apps.Items[0].Id)
	assert.Equal(t, splitAction.Id, apps.Items[0].ActionId)
	res = get(asStranger, base+"/corporate-action-applications?account_id="+strangerAcct.String())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	res.json(&apps)
	assert.Empty(t, apps.Items, "a stranger's own list holds none of the customer's")
}
