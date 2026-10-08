//go:build integration

package httpapi

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/feeledger"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/orders"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/treasury"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// treasuryWorld is the real treasury and fee ledger, the internal book they
// trade on, their real dependencies and the HTTP surface over them, on the
// package's test database. Its clock is fake, so marks stay fresh and a
// performance period can end.
type treasuryWorld struct {
	t        *testing.T
	d        *db.DB
	clk      *clock.Fake
	h        *harness
	ledger   *ledger.Service
	prices   *valuation.PriceStore
	orders   *orders.Service
	pipe     *orders.Pipeline
	treasury *treasury.Service
	fees     *feeledger.Service
	usd      assets.Asset
	base     assets.Asset
	inst     instruments.InstrumentID
}

func newTreasuryWorld(t *testing.T) *treasuryWorld {
	t.Helper()
	d := openTestDB(t)
	ctx := t.Context()
	w := &treasuryWorld{t: t, d: d, clk: clock.NewFake(time.Now().UTC().Truncate(time.Second))}
	outbox := event.NewOutbox(w.clk)
	aw := audit.NewWriterWithBuildVersion("httpapi-treasury-itest")
	checker, err := gates.NewChecker("TEST", func(gates.Capability) bool { return true }, w.clk)
	require.NoError(t, err)
	kills := killswitch.NewChecker(killswitch.Policy{})
	router, err := legalrouter.New(legalrouter.ConservativePolicy())
	require.NoError(t, err)
	w.ledger = ledger.NewService(w.clk, "httpapi-treasury-itest")
	w.prices = valuation.NewPriceStore(w.clk)
	capitalSvc := capital.NewService(w.clk, capital.OutboxEmitter{Outbox: outbox, Clock: w.clk})
	effects := econ.NewEffectStore(w.clk, outbox, aw)
	authorizer := econ.NewAuthorizer(checker, kills)
	instr, assetRepo := instruments.NewRepository(), assets.NewRepository()
	w.fees, err = feeledger.NewService(feeledger.Config{
		Clock: w.clk, Ledger: w.ledger, Capital: capitalSvc, Effects: effects, Authorizer: authorizer, Assets: assetRepo, Audit: aw,
	})
	require.NoError(t, err)
	w.orders, err = orders.NewService(orders.Config{
		Clock: w.clk, Ledger: w.ledger, Capital: capitalSvc, Intents: econ.NewIntentStore(w.clk, outbox, aw), Effects: effects,
		Authorizer: authorizer, Registry: econ.NewRegistry(), Risk: risk.NewStore(), RiskPolicy: risk.DefaultAdvancedPolicy(),
		Router: router, Instruments: instr, Assets: assetRepo, Prices: w.prices, KillSwitches: kills, FeeLedger: w.fees,
	})
	require.NoError(t, err)
	w.pipe, err = orders.NewPipeline(w.orders, d)
	require.NoError(t, err)
	w.treasury, err = treasury.NewService(treasury.Config{
		Clock: w.clk, Ledger: w.ledger, Effects: effects, Authorizer: authorizer, Registry: econ.NewRegistry(), Orders: w.orders,
		Prices: w.prices, Assets: assetRepo, Instruments: instr, KillSwitches: kills, Audit: aw, RiskPolicy: risk.DefaultAdvancedPolicy(),
	})
	require.NoError(t, err)

	require.NoError(t, d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		w.usd, err = econ.EnsureSandboxDollar(ctx, tx)
		return err
	}))
	w.base = w.simAsset("HSE")
	w.inst = w.spotBook(w.base)
	w.mark(w.base.ID, "100.00")

	ports, err := Wire(WireDeps{DB: d, Clock: w.clk, Env: "TEST", Ledger: w.ledger, Instruments: instr, Assets: assetRepo,
		Advanced: AdvancedDeps{Treasury: TreasuryDeps{Service: w.treasury, Fees: w.fees}}})
	require.NoError(t, err)
	require.NotNil(t, ports.Treasury, "the treasury and the asset registry wire the treasury port")
	require.NotNil(t, ports.Fees, "the fee ledger and the asset registry wire the fee port")

	userID, acct := seedAccount(t, d)
	w.h = newIntegrationHarness(t, d, acct, userID)
	w.h.server.opts.Ports.Treasury = ports.Treasury
	w.h.server.opts.Ports.Fees = ports.Fees
	return w
}

// simAsset is a fresh SIMULATED 6-decimal asset.
func (w *treasuryWorld) simAsset(prefix string) assets.Asset {
	w.t.Helper()
	sym := prefix + strings.ToUpper(uuid.NewString()[:8])
	a, err := assets.NewRepository().Create(w.t.Context(), w.d, assets.Asset{
		Chain: assets.SandboxChain, Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated, Symbol: sym,
		Name: sym + " (sandbox)", Decimals: 6, RiskClass: assets.RiskSettlement, Status: assets.StatusActive,
	})
	require.NoError(w.t, err)
	return a
}

// spotBook opens a PAPER spot book of base against the sandbox dollar: a tick
// is one cent and a lot 0.0001 of the base, with no explicit fees.
func (w *treasuryWorld) spotBook(base assets.Asset) instruments.InstrumentID {
	w.t.Helper()
	var out instruments.InstrumentID
	require.NoError(w.t, w.d.InTx(w.t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		inst, err := instruments.NewRepository().CreateSpotPair(ctx, tx, instruments.SpotPairSpec{
			Base: base.ID, Quote: w.usd.ID, Settlement: w.usd.ID, CanonicalName: base.Symbol + "/" + w.usd.Symbol,
			RiskClass: assets.RiskSettlement, Status: assets.StatusActive, ActiveFrom: w.clk.Now().Add(-time.Hour),
		})
		if err != nil {
			return err
		}
		baseID, quoteID := base.ID, w.usd.ID
		terms := instruments.Terms{
			Kind: instruments.TypeSpotPair, Symbol: "HSPOT", VenueCode: "NODAL-SIM", BaseAssetID: &baseID, QuoteAssetID: &quoteID,
			SettlementAssetID: quoteID, ContractMultiplier: money.QuantityFromInt64(1), PriceScale: 2,
			TickSize: money.QuantityFromInt64(1), LotSize: money.QuantityFromInt64(100), MinQuantity: money.QuantityFromInt64(100),
			QuantityDecimals: 6, SettlementStyle: instruments.SettlePhysical, TradingCalendar: "24x7", OracleMaxAgeMS: 60_000,
			MarginModel: instruments.MarginNone, CorporateActionBehavior: instruments.CorporateActionsNone,
			Transferability: instruments.Transferable,
		}
		zero := fees.ZeroSchedule(w.usd.ID, w.usd.Decimals)
		out = inst.ID
		_, err = w.orders.OpenBook(ctx, tx, orders.OpenBookRequest{
			InstrumentID: inst.ID, Mode: gates.ModePaper, SpotTerms: &terms, MakerSchedule: zero, TakerSchedule: zero,
			SnapshotInterval: 16, Actor: econ.Actor{Type: security.ActorSystem, ID: "treasury-httpapi-itest"},
		})
		return err
	}))
	return out
}

// mark records a fresh reference price of an asset in the sandbox dollar,
// one second on (an observation is identified by its time).
func (w *treasuryWorld) mark(a assets.AssetID, price string) {
	w.t.Helper()
	w.clk.Advance(time.Second)
	p, err := money.PriceFromDecimalString(price, w.usd.ID.String(), "httpapi-itest-oracle", w.clk.Now())
	require.NoError(w.t, err)
	_, err = w.prices.RecordPrice(w.t.Context(), w.d, valuation.PriceObservation{AssetID: a, QuoteAssetID: w.usd.ID, Mantissa: p.Mantissa,
		Scale: p.Scale, Source: "httpapi-itest-oracle", ObservedAt: w.clk.Now()})
	require.NoError(w.t, err)
}

// customer is a customer whose owner has a verified email address, its
// session, and its wallet funded with qty of an asset (SANDBOX_FUNDING).
func (w *treasuryWorld) customer(a assets.AssetID, qty money.Quantity) (accounts.AccountID, security.Principal) {
	w.t.Helper()
	repo := accounts.NewRepository()
	hash := sha256.Sum256([]byte(uuid.NewString()))
	u, err := repo.CreateUser(w.t.Context(), w.d, "httpapi-treasury-itest", uuid.NewString(), hash[:])
	require.NoError(w.t, err)
	acct, err := repo.CreateAccount(w.t.Context(), w.d, u.ID, accounts.KindCustomer)
	require.NoError(w.t, err)
	if qty.IsPositive() {
		w.fund(acct.ID, a, qty)
	}
	return acct.ID, security.Principal{
		SubjectID: u.ID.String(), ActorType: security.ActorUser, Roles: []security.Role{security.RoleCustomer},
		AccountIDs: []string{acct.ID.String()}, SessionID: testSessionID, AuthTime: time.Now().UTC(), AMR: []string{"pwd", "mfa"},
	}
}

func (w *treasuryWorld) fund(acct accounts.AccountID, a assets.AssetID, qty money.Quantity) {
	w.t.Helper()
	require.NoError(w.t, w.d.InTx(w.t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := w.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindSandboxFunding, IdempotencyKey: "itest-treasury-fund:" + ref,
			Reference: ledger.FinancialEventReference{Type: "sandbox_funding", ID: ref}, EffectiveAt: w.clk.Now(),
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(acct, ledger.CodeWallet, a), Side: ledger.Debit, Quantity: qty},
				{Account: ledger.CustomerAccount(acct, ledger.CodeCapital, a), Side: ledger.Credit, Quantity: qty},
			},
		})
		return err
	}))
}

func (w *treasuryWorld) operator() security.Principal {
	return security.Principal{
		SubjectID: uuid.NewString(), ActorType: security.ActorOperator, Roles: []security.Role{security.RoleRisk},
		SessionID: testSessionID, AuthTime: time.Now().UTC(), AMR: []string{"pwd", "mfa"},
	}
}

func (w *treasuryWorld) post(p security.Principal, path string, body any, key string) *response {
	w.t.Helper()
	w.h.as(&p)
	return w.h.do(http.MethodPost, path, body, "Idempotency-Key", key)
}

func (w *treasuryWorld) get(p security.Principal, path string) *response {
	w.t.Helper()
	w.h.as(&p)
	return w.h.do(http.MethodGet, path, nil)
}

// units is a whole number of units of a 6-decimal asset, in base units.
func trUnits(n int64) string { return fmt.Sprintf("%d", n*1_000_000) }

// registerBook registers a market-making house book whose signing policy lets
// operators trade, sets its limits (inventory 10 units of the base, exposure
// 2,000 on the instrument, daily loss 1,000, 20 bps paper spread and a 5 bps
// paper fee) and funds it with 100,000 sandbox dollars, all over HTTP.
func (w *treasuryWorld) registerBook(op security.Principal) string {
	w.t.Helper()
	res := w.post(op, "/v1/advanced/treasury/books", map[string]any{"name": "itest book " + uuid.NewString(), "strategy": "MARKET_MAKING",
		"service_actor_id": "house-service-itest", "operators_allowed": true, "reason": "integration test house book"}, "reg-"+uuid.NewString())
	require.Equal(w.t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var book api.TreasuryBookDetail
	res.json(&book)
	require.Equal(w.t, w.usd.ID.String(), book.ValuationAsset.AssetId.String(), "the sandbox dollar values a book by default")
	return "/v1/advanced/treasury/books/" + book.HouseAccountId.String()
}

func (w *treasuryWorld) setLimits(op security.Principal, path string) {
	w.t.Helper()
	res := w.post(op, path+"/limits", map[string]any{"label": "itest-limits", "daily_loss_limit": trUnits(1_000), "max_mark_age_ms": 600_000,
		"max_inventory":           []any{map[string]any{"asset_id": w.base.ID.String(), "max_quantity": trUnits(10)}},
		"max_instrument_exposure": []any{map[string]any{"instrument_id": w.inst.String(), "max_exposure": trUnits(2_000)}},
		"paper_spread_bps":        20, "paper_fee_bps": 5, "reason": "integration test limits"}, "limits-"+uuid.NewString())
	require.Equal(w.t, http.StatusOK, res.Code, "body=%s", res.Body.String())
}

func (w *treasuryWorld) fundBook(op security.Principal, path string, units int64) {
	w.t.Helper()
	res := w.post(op, path+"/fundings", map[string]any{"asset_id": w.usd.ID.String(), "quantity": trUnits(units),
		"reason": "seed the house book"}, "fund-"+uuid.NewString())
	require.Equal(w.t, http.StatusOK, res.Code, "body=%s", res.Body.String())
}

func (w *treasuryWorld) order(side, price string, units int64) map[string]any {
	return map[string]any{"instrument_id": w.inst.String(), "activity": "MARKET_MAKING", "side": side, "order_type": "LIMIT",
		"time_in_force": "GTC", "price": price, "quantity": trUnits(units)}
}

func trFindLimit(t *testing.T, limits []api.TreasuryLimitUse, kind api.TreasuryLimitKind) api.TreasuryLimitUse {
	t.Helper()
	for _, l := range limits {
		if l.Kind == kind {
			return l
		}
	}
	require.FailNow(t, "no limit of kind "+string(kind))
	return api.TreasuryLimitUse{}
}

// TestIntegration_TreasuryOverHTTP drives the house book end to end through
// HTTP over the real adapter, treasury, order book and database: an operator
// registers a book, sets its limits and funds it; a house order within the
// limits rests and is cancelled; one over the inventory cap is refused with
// the treasury's reason codes and recorded; a hedge fills on the paper venue
// with its spread and fee disclosed; the report states positions, PnL and
// every limit in use exactly; a disclosed crossing rule lets the house trade
// with a customer, who then sees that trade and its disclosure, and nobody
// else does; the register reconciles with the ledger.
func TestIntegration_TreasuryOverHTTP(t *testing.T) {
	w := newTreasuryWorld(t)
	op := w.operator()
	seller, alice := w.customer(w.base.ID, money.QuantityFromInt64(5_000_000))
	_, bob := w.customer(w.usd.ID, money.Quantity{})

	// The treasury is the operator's.
	res := w.get(alice, "/v1/advanced/treasury/books")
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())

	book := w.registerBook(op)

	// Without limits nothing trades: the refusal is recorded and answered.
	res = w.post(op, book+"/hedges", map[string]any{"asset_id": w.base.ID.String(), "side": "BUY", "quantity": trUnits(1)}, "early-hedge-1")
	require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	p := res.problem()
	assert.Equal(t, treasury.ReasonPolicyMissing, p.Fields["reason"])
	assert.NotEmpty(t, p.Fields["action_id"])

	w.setLimits(op, book)
	w.fundBook(op, book, 100_000)

	// A house order within the limits rests, and is cancelled.
	res = w.post(op, book+"/orders", w.order("BUY", "95.00", 1), "rest-order-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var placed api.TreasuryOrderOutcome
	res.json(&placed)
	assert.Equal(t, api.TreasuryOutcomeApplied, placed.Action.Outcome)
	require.NotNil(t, placed.Order)
	assert.Equal(t, api.TreasuryOrderOpen, placed.Order.Status)
	assert.Equal(t, "95.00", placed.Order.Price)
	require.NotNil(t, placed.Decision)
	assert.Equal(t, api.TreasuryVerdictAllow, placed.Decision.Verdict)

	res = w.post(op, book+"/orders/"+placed.Order.Id.String()+"/cancel", nil, "cancel-order-001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var cancelled api.TreasuryOrderOutcome
	res.json(&cancelled)
	assert.Equal(t, api.TreasuryOrderCancelled, cancelled.Order.Status)

	// The same key replays the first answer.
	again := w.post(op, book+"/orders", w.order("BUY", "95.00", 1), "rest-order-0001")
	require.Equal(t, http.StatusOK, again.Code, "body=%s", again.Body.String())
	var replay api.TreasuryOrderOutcome
	again.json(&replay)
	assert.Equal(t, placed.Action.Id, replay.Action.Id)

	// Twenty units breach the ten-unit inventory cap: refused before the book,
	// with the treasury's codes, and recorded.
	res = w.post(op, book+"/orders", w.order("BUY", "95.00", 20), "breach-order-001")
	require.Equal(t, http.StatusUnprocessableEntity, res.Code, "body=%s", res.Body.String())
	p = res.problem()
	assert.Equal(t, errs.CodeRiskRejected, p.Code)
	assert.Contains(t, p.Fields["reason_codes"], treasury.ReasonInventoryCap)
	refusedAction := p.Fields["action_id"]
	require.NotEmpty(t, refusedAction)

	// A hedge fills on the paper venue at the mark with its spread and fee
	// disclosed: 2 units at 100.00, 20 bps of spread (0.40) and a 5 bps fee
	// (0.10).
	res = w.post(op, book+"/hedges", map[string]any{"asset_id": w.base.ID.String(), "side": "BUY", "quantity": trUnits(2)}, "hedge-0000001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var hedge api.TreasuryPaperOutcome
	res.json(&hedge)
	require.NotNil(t, hedge.Fill)
	assert.Equal(t, "SIMULATED_PAPER_VENUE", hedge.Fill.VenueLabel)
	assert.Equal(t, "200000000", hedge.Fill.NotionalAtReference)
	assert.Equal(t, "400000", hedge.Fill.SpreadAmount)
	assert.Equal(t, "100000", hedge.Fill.FeeAmount)
	assert.Equal(t, "200500000", hedge.Fill.TotalQuote)
	assert.Equal(t, hedge.Fill.TotalQuote, hedge.Fill.Disclosure.TotalDebit, "the disclosure is the fill's own")

	// The report: the position at cost, the paper costs as realized loss, and
	// every limit with its use.
	res = w.get(op, book+"/report")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var rep api.TreasuryReport
	res.json(&rep)
	require.Len(t, rep.Assets, 1)
	assert.Equal(t, "2000000", rep.Assets[0].Quantity)
	assert.Equal(t, "200000000", rep.Assets[0].Cost)
	require.NotNil(t, rep.Assets[0].MarketValue)
	assert.Equal(t, "200000000", *rep.Assets[0].MarketValue)
	assert.Equal(t, "-500000", rep.RealizedToday, "the spread and fee are expensed when paid")
	assert.Equal(t, "-500000", rep.LossMeasure)
	require.NotNil(t, rep.Limits)
	inv := trFindLimit(t, *rep.Limits, api.TreasuryLimitInventory)
	require.NotNil(t, inv.Used)
	assert.Equal(t, trUnits(2), *inv.Used)
	assert.Equal(t, trUnits(10), *inv.Cap)
	assert.Empty(t, inv.Codes, "eight units of room are left")
	exp := trFindLimit(t, *rep.Limits, api.TreasuryLimitInstrumentExposure)
	require.NotNil(t, exp.Used)
	assert.Equal(t, "200000000", *exp.Used, "two units at a mark of 100.00, nothing working")
	loss := trFindLimit(t, *rep.Limits, api.TreasuryLimitDailyLoss)
	assert.Equal(t, "-500000", *loss.Used)

	// A disclosed crossing rule lets the house trade with a customer.
	disclosure := "House books may trade against customers on this book; this is simulated sandbox activity."
	res = w.post(op, "/v1/advanced/treasury/crossing-rules", map[string]any{"instrument_id": w.inst.String(), "allow": true,
		"disclosure": disclosure, "reason": "integration test market making"}, "crossing-0000001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var rule api.TreasuryCrossingDisclosure
	res.json(&rule)
	assert.Equal(t, 1, rule.RuleVersion)

	sell, err := w.pipe.Submit(t.Context(), orders.OrderRequest{
		InstrumentID: w.inst, AccountID: seller, Side: orders.SideSell, Type: orders.TypeLimit, TimeInForce: orders.GTC,
		Price: trPrice(t, w, "100.00"), Quantity: money.QuantityFromInt64(1_000_000), IdempotencyKey: "cust-sell-" + uuid.NewString(),
		ActorType: security.ActorUser, ActorID: seller.String(),
	})
	require.NoError(t, err)
	assert.Equal(t, orders.StatusOpen, sell.Order.Status)

	res = w.post(op, book+"/orders", w.order("BUY", "100.00", 1), "cross-order-001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var crossed api.TreasuryOrderOutcome
	res.json(&crossed)
	require.Len(t, crossed.Trades, 1)
	assert.Equal(t, 1, crossed.Trades[0].CrossingRuleVersion)
	assert.Equal(t, api.TreasuryOwnerCustomer, crossed.Trades[0].MakerOwnerType)

	// The customer sees the house trade against them and what they were told.
	res = w.get(alice, "/v1/advanced/treasury/house-trades?account_id="+seller.String())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var trades api.TreasuryHouseTradePage
	res.json(&trades)
	require.Len(t, trades.Items, 1)
	tr := trades.Items[0]
	assert.Equal(t, api.TreasurySideSell, tr.AccountSide)
	assert.Equal(t, api.TreasurySideBuy, tr.HouseSide)
	assert.Equal(t, "1000000", tr.BaseQuantity)
	assert.Equal(t, "100000000", tr.QuoteNotional)
	assert.Equal(t, "10000", tr.Price.Mantissa)
	assert.Equal(t, 2, tr.Price.Scale)
	require.NotNil(t, tr.Disclosure)
	assert.Equal(t, disclosure, *tr.Disclosure)
	res = w.get(bob, "/v1/advanced/treasury/house-trades?account_id="+seller.String())
	require.Equal(t, http.StatusForbidden, res.Code, "another customer reads none of it; body=%s", res.Body.String())
	res = w.get(alice, "/v1/advanced/treasury/crossing-disclosures?instrument_id="+w.inst.String())
	require.Equal(t, http.StatusOK, res.Code, "a customer reads what they are told; body=%s", res.Body.String())
	var told api.TreasuryCrossingDisclosureList
	res.json(&told)
	require.Len(t, told.Items, 1)
	assert.Equal(t, disclosure, told.Items[0].Disclosure)

	// The register reads back and agrees with the ledger.
	res = w.get(op, book+"/reconciliation")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var rec api.TreasuryReconciliation
	res.json(&rec)
	assert.True(t, rec.Agrees, "discrepancies: %+v", rec.Discrepancies)
	res = w.get(op, book+"/balances")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var bal api.TreasuryBalanceList
	res.json(&bal)
	held := map[string]api.TreasuryBalance{}
	for _, b := range bal.Items {
		held[b.Asset.AssetId.String()] = b
	}
	assert.Equal(t, trUnits(3), held[w.base.ID.String()].Inventory, "two units hedged, one bought from the customer")
	res = w.get(op, book+"/events")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var events api.TreasuryPositionEventPage
	res.json(&events)
	require.Len(t, events.Items, 2)
	assert.Equal(t, api.TreasuryEventBookFill, events.Items[0].Kind, "newest first")
	res = w.get(op, book+"/paper-fills")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var fills api.TreasuryPaperFillPage
	res.json(&fills)
	require.Len(t, fills.Items, 1)
	res = w.get(op, book+"/risk-decisions")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var decisions api.TreasuryRiskDecisionPage
	res.json(&decisions)
	rejected := 0
	for _, d := range decisions.Items {
		if d.Verdict == api.TreasuryVerdictReject {
			rejected++
			assert.Contains(t, d.TreasuryCodes, treasury.ReasonInventoryCap)
		}
	}
	assert.Equal(t, 1, rejected)
	res = w.get(op, book+"/actions")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var actions api.TreasuryActionList
	res.json(&actions)
	found := false
	for _, a := range actions.Items {
		if a.Id.String() == refusedAction {
			found = true
			assert.Equal(t, api.TreasuryOutcomeRejected, a.Outcome)
		}
	}
	assert.True(t, found, "the refusal is on the register")

	res = w.post(op, book+"/sync", nil, "sync-00000001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	res = w.post(op, book+"/mark-to-market", nil, "mark-00000001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var snap api.TreasurySnapshot
	res.json(&snap)
	assert.Equal(t, "300000000", snap.Report.Assets[0].Cost, "three units at cost 100.00 each")
}

func trPrice(t *testing.T, w *treasuryWorld, s string) *money.Price {
	t.Helper()
	p, err := money.PriceFromDecimalString(s, w.usd.ID.String(), "httpapi-itest", w.clk.Now())
	require.NoError(t, err)
	return &p
}

// TestIntegration_HouseOrdersExistOnlyThroughTheTreasury: a HOUSE order on
// the book exists only as the result of a treasury action (00759, TY007).
// The treasury's own orders commit; a house order written around it -- a raw
// INSERT, or the order book's own SubmitHouseOrder called alone, accepted or
// refused -- is refused at COMMIT and leaves nothing behind.
func TestIntegration_HouseOrdersExistOnlyThroughTheTreasury(t *testing.T) {
	w := newTreasuryWorld(t)
	op := w.operator()
	book := w.registerBook(op)
	w.setLimits(op, book)
	w.fundBook(op, book, 10_000)

	res := w.post(op, book+"/orders", w.order("BUY", "90.00", 1), "bound-order-001")
	require.Equal(t, http.StatusOK, res.Code, "the treasury's own order commits; body=%s", res.Body.String())
	var placed api.TreasuryOrderOutcome
	res.json(&placed)
	require.NotNil(t, placed.Order)
	houseID, err := econ.ParseHouseAccountID(placed.Order.HouseAccountId.String())
	require.NoError(t, err)

	raw := func(sql string, args ...any) error {
		return w.d.InTx(t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql, args...)
			return err
		})
	}
	copyOrder := `INSERT INTO book_orders (id, book_id, instrument_id, owner_type, owner_id, house_account_id, idempotency_key,
		request_hash, side, order_type, time_in_force, stp_mode, liquidity_intent, post_only, reduce_only, mode, price_ticks, qty_lots,
		remaining_lots, status, reject_reason)
		SELECT $1, book_id, instrument_id, owner_type, owner_id, house_account_id, $2, request_hash, side, order_type, time_in_force,
		stp_mode, liquidity_intent, post_only, reduce_only, mode, price_ticks, qty_lots, %s, %s FROM book_orders WHERE id = $3`

	// A raw house order the book would rest: refused at COMMIT.
	key := "raw-bypass-" + uuid.NewString()
	err = raw(fmt.Sprintf(copyOrder, "qty_lots", "'PENDING', NULL"), uuid.New(), key, placed.Order.Id)
	require.Error(t, err)
	assert.Equal(t, "TY007", db.SQLState(err), "%v", err)
	// A raw REJECTED house order is no record of anything the treasury did
	// either.
	err = raw(fmt.Sprintf(copyOrder, "0", "'REJECTED', 'RAW'"), uuid.New(), key+"-rejected", placed.Order.Id)
	require.Error(t, err)
	assert.Equal(t, "TY007", db.SQLState(err), "%v", err)

	// The order book's own house path, called around the treasury, accepted
	// and refused alike.
	accepted := "service-bypass-" + uuid.NewString()
	_, err = w.pipe.SubmitHouse(t.Context(), orders.HouseOrderRequest{InstrumentID: w.inst, HouseAccountID: houseID, Side: orders.SideBuy,
		Type: orders.TypeLimit, TimeInForce: orders.GTC, Price: trPrice(t, w, "90.00"), Quantity: money.QuantityFromInt64(1_000_000),
		IdempotencyKey: accepted, Actor: econ.Actor{Type: security.ActorService, ID: "house-service-itest"}})
	require.Error(t, err)
	assert.Equal(t, "TY007", db.SQLState(err), "%v", err)
	refused := "service-bypass-sell-" + uuid.NewString()
	_, err = w.pipe.SubmitHouse(t.Context(), orders.HouseOrderRequest{InstrumentID: w.inst, HouseAccountID: houseID, Side: orders.SideSell,
		Type: orders.TypeLimit, TimeInForce: orders.GTC, Price: trPrice(t, w, "110.00"), Quantity: money.QuantityFromInt64(1_000_000),
		IdempotencyKey: refused, Actor: econ.Actor{Type: security.ActorService, ID: "house-service-itest"}})
	require.Error(t, err)
	assert.Equal(t, "TY007", db.SQLState(err), "the book's own refusal is not a treasury action either: %v", err)

	var left int
	require.NoError(t, w.d.QueryRow(t.Context(), `SELECT count(*) FROM book_orders WHERE owner_id = $1 AND idempotency_key = ANY($2)`,
		houseID, []string{key, key + "-rejected", accepted, refused}).Scan(&left))
	assert.Zero(t, left, "nothing written around the treasury remains")
	var bound int
	require.NoError(t, w.d.QueryRow(t.Context(), `SELECT count(*) FROM book_orders o JOIN treasury_actions a
		ON a.order_id = o.id AND a.kind = 'PLACE_ORDER' WHERE o.house_account_id = $1`, houseID).Scan(&bound))
	assert.Equal(t, 1, bound, "the one house order there is names the treasury action that placed it")
}
