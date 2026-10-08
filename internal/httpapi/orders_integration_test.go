//go:build integration

package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
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
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
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
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// booksCustomerOf is a signed-in customer owning acct, strongly authenticated
// just now.
func booksCustomerOf(user accounts.UserID, acct accounts.AccountID) security.Principal {
	return security.Principal{
		SubjectID: user.String(), ActorType: security.ActorUser, Roles: []security.Role{security.RoleCustomer},
		AccountIDs: []string{acct.String()}, SessionID: testSessionID, AuthTime: time.Now().UTC(), AMR: []string{"pwd", "mfa"},
	}
}

// booksVerifiedCustomer is a customer whose owner has a verified email address:
// NODAL_IDENTITY, which is what the advanced capabilities ask of simulated
// activity (D-055).
func booksVerifiedCustomer(t *testing.T, d *db.DB) (accounts.UserID, accounts.AccountID) {
	t.Helper()
	repo := accounts.NewRepository()
	email := sha256.Sum256([]byte(uuid.NewString() + "@sandbox.invalid"))
	u, err := repo.CreateUser(t.Context(), d, "httpapi-itest", "sub-"+uuid.NewString(), email[:])
	require.NoError(t, err)
	a, err := repo.CreateAccount(t.Context(), d, u.ID, accounts.KindCustomer)
	require.NoError(t, err)
	return u.ID, a.ID
}

func booksOperatorNow() security.Principal {
	return security.Principal{
		SubjectID: uuid.NewString(), ActorType: security.ActorOperator, Roles: []security.Role{security.RoleOperations},
		SessionID: testSessionID, AuthTime: time.Now().UTC(), AMR: []string{"pwd", "mfa"},
	}
}

// booksFund credits simulated value to an account's WALLET.
func booksFund(t *testing.T, d *db.DB, led *ledger.Service, acct accounts.AccountID, asset assets.AssetID, qty int64) {
	t.Helper()
	q := money.QuantityFromInt64(qty)
	require.NoError(t, d.InTx(t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := led.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindSandboxFunding, IdempotencyKey: "itest-fund:" + ref,
			Reference: ledger.FinancialEventReference{Type: "sandbox_funding", ID: ref}, EffectiveAt: time.Now().UTC(),
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(acct, ledger.CodeWallet, asset), Side: ledger.Debit, Quantity: q},
				{Account: ledger.CustomerAccount(acct, ledger.CodeCapital, asset), Side: ledger.Credit, Quantity: q},
			},
		})
		return err
	}))
}

func booksBalance(t *testing.T, d *db.DB, led *ledger.Service, acct accounts.AccountID, code ledger.Code, asset assets.AssetID) string {
	t.Helper()
	b, err := led.Balance(t.Context(), d, ledger.CustomerAccount(acct, code, asset))
	require.NoError(t, err)
	return b.String()
}

// booksSimAsset creates a simulated sandbox asset with a symbol no other test
// uses; the quote asset is USD-pegged, as a book's must be.
func booksSimAsset(t *testing.T, d *db.DB, prefix string, decimals uint8, usdPegged bool) assets.Asset {
	t.Helper()
	sym := prefix + strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", "")[:8])
	a := assets.Asset{
		Chain: assets.SandboxChain, Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated, Symbol: sym,
		Name: sym + " (sandbox)", Decimals: decimals, RiskClass: assets.RiskSettlement, Status: assets.StatusActive,
	}
	if usdPegged {
		a.IsStablecoin, a.PegCurrency = true, "USD"
	}
	out, err := assets.NewRepository().Create(t.Context(), d, a)
	require.NoError(t, err)
	return out
}

// TestIntegration_BooksTwoCustomersTradeThroughHTTP drives the order book
// routes over the real adapter, service, pipeline and database, wired the way
// cmd/api wires them: an operator opens a PAPER book, one customer's sell
// rests, another's buy crosses it, and the fills, the balances and the
// trade's audit proof agree with each other and with the journal's replay. A
// reused client order id is refused and recorded, a stranger can read
// neither account's orders, and a cancel is exactly-once.
func TestIntegration_BooksTwoCustomersTradeThroughHTTP(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	clk := clock.System()

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
	svc, err := orders.NewService(orders.Config{
		Clock: clk, Ledger: led, Capital: capital.NewService(clk, capital.OutboxEmitter{Outbox: outbox, Clock: clk}),
		Intents: econ.NewIntentStore(clk, outbox, aw), Effects: econ.NewEffectStore(clk, outbox, aw),
		Authorizer: econ.NewAuthorizer(checker, kills), Registry: econ.NewRegistry(), Risk: risk.NewStore(),
		RiskPolicy: risk.DefaultAdvancedPolicy(), Router: router, Instruments: instruments.NewRepository(),
		Assets: assets.NewRepository(), Prices: prices, KillSwitches: kills,
		Verification: AdvancedVerificationLevels{Rule: compliance.NewAccountVerification(clk)},
	})
	require.NoError(t, err)
	pipe, err := orders.NewPipeline(svc, d)
	require.NoError(t, err)
	ports, err := Wire(WireDeps{DB: d, Clock: clk, Env: config.EnvTest, Advanced: AdvancedDeps{
		Orders: OrdersDeps{Service: svc, Pipeline: pipe},
	}})
	require.NoError(t, err)
	require.NotNil(t, ports.Books, "the service and its pipeline wire the port")

	aliceUser, alice := booksVerifiedCustomer(t, d)
	bobUser, bob := booksVerifiedCustomer(t, d)
	carolUser, carol := booksVerifiedCustomer(t, d)
	h := newIntegrationHarness(t, d, alice, aliceUser)
	h.server.opts.Ports.Books = ports.Books
	asAlice, asBob, asCarol := booksCustomerOf(aliceUser, alice), booksCustomerOf(bobUser, bob), booksCustomerOf(carolUser, carol)
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

	// --- the market: SBX base (8 decimals) against a USD-pegged quote ------
	base := booksSimAsset(t, d, "OBB", 8, false)
	quote := booksSimAsset(t, d, "OBQ", 6, true)
	var inst instruments.InstrumentID
	require.NoError(t, d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		i, err := instruments.NewRepository().CreateSpotPair(ctx, tx, instruments.SpotPairSpec{
			Base: base.ID, Quote: quote.ID, Settlement: quote.ID, CanonicalName: base.Symbol + "/" + quote.Symbol,
			RiskClass: assets.RiskMajor, Status: assets.StatusActive, ActiveFrom: clk.Now().Add(-time.Hour),
		})
		inst = i.ID
		return err
	}))
	// A fresh mark of the base in the quote: the risk kernel refuses new risk
	// (a buy) on a stale or missing one.
	mark := func() {
		t.Helper()
		_, err := prices.RecordPrice(ctx, d, valuation.PriceObservation{
			AssetID: base.ID, QuoteAssetID: quote.ID, Mantissa: money.QuantityFromInt64(250_000), Scale: 2,
			Source: "httpapi-itest", ObservedAt: clk.Now(),
		})
		require.NoError(t, err)
	}
	booksFund(t, d, led, alice, base.ID, 300_000_000)    // 3 base units
	booksFund(t, d, led, bob, quote.ID, 10_000_000_000)  // 10,000 quote units
	booksFund(t, d, led, carol, quote.ID, 1_000_000_000) // 1,000 quote units

	// --- an operator opens the book; a customer cannot ---------------------
	fee := func(version string, pct int) map[string]any {
		return map[string]any{"version": version, "quote_asset_id": quote.ID.String(), "quote_decimals": 6, "percentage_fee_bps": pct}
	}
	openBody := map[string]any{
		"instrument_id": inst.String(), "mode": "PAPER", "snapshot_interval": 2,
		"maker_fee": fee("itest-maker-10bps", 10), "taker_fee": fee("itest-taker-20bps", 20),
		"spot_terms": map[string]any{
			"kind": "SPOT_PAIR", "symbol": "SBXITEST/USD", "venue_code": "NODAL-SIM", "base_asset_id": base.ID.String(),
			"quote_asset_id": quote.ID.String(), "settlement_asset_id": quote.ID.String(), "contract_multiplier": "1",
			"price_scale": 2, "tick_size": "1", "lot_size": "10000", "min_quantity": "10000", "quantity_decimals": 8,
			"settlement_style": "PHYSICAL", "trading_calendar": "24x7", "jurisdiction_tags": []string{},
			"oracle_max_age_ms": 600000, "oracle_sources": []string{"httpapi-itest"}, "margin_model": "NONE",
			"corporate_action_behavior": "NONE", "transferability": "TRANSFERABLE",
		},
	}
	res := post(asAlice, "/v1/advanced/books", openBody, key("open-cust"))
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	res = post(operator, "/v1/advanced/books", openBody, key("open"))
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	var book api.AdvancedBook
	res.json(&book)
	assert.Equal(t, api.SIMULATED, book.ValueDomain)
	assert.Equal(t, api.BookStatusOpen, book.Status)
	assert.Equal(t, "0.01", book.TickPrice)
	assert.Equal(t, 10, book.MakerFee.PercentageFeeBps)
	assert.True(t, book.CrossingRule.IsDefault, "no crossing rule: a house book never trades with a customer")
	bookPath := "/v1/advanced/books/" + inst.String()

	// --- Alice's sell rests -------------------------------------------------
	sell := map[string]any{
		"account_id": alice.String(), "side": "SELL", "order_type": "LIMIT", "time_in_force": "GTC",
		"price": "2500.00", "quantity": "200000000", "client_order_id": "alice-1",
	}
	res = post(asAlice, bookPath+"/orders", sell, key("sell"))
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	var rested api.AdvancedBookCommandResult
	res.json(&rested)
	assert.Equal(t, api.BookOrderOpen, rested.Order.Status)
	assert.Empty(t, rested.Fills)
	require.NotNil(t, rested.IntentStatus)
	assert.Equal(t, "EXECUTING", *rested.IntentStatus, "a resting order's intent is executing")
	aliceOrder := rested.Order.Id.String()

	res = get(asCarol, bookPath+"/depth")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var depth api.AdvancedBookDepth
	res.json(&depth)
	require.Len(t, depth.Asks, 1)
	assert.Equal(t, "2500.00", depth.Asks[0].Price)
	assert.Equal(t, "200000000", depth.Asks[0].Quantity)
	assert.Empty(t, depth.Bids)

	// --- Bob's buy crosses it -------------------------------------------------
	buyKey := key("buy")
	buy := map[string]any{
		"account_id": bob.String(), "side": "BUY", "order_type": "LIMIT", "time_in_force": "GTC",
		"price": "2500.00", "quantity": "200000000", "client_order_id": "bob-1",
	}
	mark()
	first := post(asBob, bookPath+"/orders", buy, buyKey)
	require.Equal(t, http.StatusCreated, first.Code, "body=%s", first.Body.String())
	var bought api.AdvancedBookCommandResult
	first.json(&bought)
	assert.Equal(t, api.BookOrderFilled, bought.Order.Status)
	require.NotNil(t, bought.IntentStatus)
	assert.Equal(t, "COMPLETED", *bought.IntentStatus)
	require.Len(t, bought.Fills, 1)
	fill := bought.Fills[0]
	assert.Equal(t, api.BookLiquidityTaker, fill.Liquidity)
	assert.Equal(t, api.BookSideBuy, fill.Side)
	assert.Equal(t, "2500.00", fill.Price)
	assert.Equal(t, "200000000", fill.Quantity)
	assert.Equal(t, "5000000000", fill.QuoteNotional, "2 units at 2,500.00 is 5,000 quote units, exactly")
	assert.Equal(t, "10000000", fill.Fee, "the taker's 20 bps of 5,000")
	assert.Equal(t, api.BookOwnerCustomer, fill.CounterpartyOwnerType)
	assert.NotContains(t, first.Body.String(), alice.String(), "the counterparty's account is never shown")

	// The same key and body is the same answer and the same single order.
	again := post(asBob, bookPath+"/orders", buy, buyKey)
	require.Equal(t, http.StatusOK, again.Code, "body=%s", again.Body.String())
	assert.JSONEq(t, first.Body.String(), again.Body.String())

	// Balances moved exactly by the trade and its disclosed fees.
	assert.Equal(t, "100000000", booksBalance(t, d, led, alice, ledger.CodeWallet, base.ID))
	assert.Equal(t, "4995000000", booksBalance(t, d, led, alice, ledger.CodeWallet, quote.ID), "5,000 less the maker's 10 bps")
	assert.Equal(t, "200000000", booksBalance(t, d, led, bob, ledger.CodeWallet, base.ID))
	assert.Equal(t, "4990000000", booksBalance(t, d, led, bob, ledger.CodeWallet, quote.ID), "10,000 less 5,000 and the taker's 20 bps")

	// Alice's order was finished by Bob's command.
	res = get(asAlice, "/v1/advanced/books/orders/"+aliceOrder)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, "FILLED", res.raw()["status"])

	// Each side reads its own side of the fill.
	res = get(asAlice, "/v1/advanced/books/trades?account_id="+alice.String())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var aliceFills api.AdvancedBookFillPage
	res.json(&aliceFills)
	require.Len(t, aliceFills.Items, 1)
	assert.Equal(t, api.BookLiquidityMaker, aliceFills.Items[0].Liquidity)
	assert.Equal(t, api.BookSideSell, aliceFills.Items[0].Side)
	assert.Equal(t, "5000000", aliceFills.Items[0].Fee)
	assert.Equal(t, fill.TradeSeq, aliceFills.Items[0].TradeSeq)
	assert.NotContains(t, res.Body.String(), bob.String())

	// --- the trade's audit proof verifies ---------------------------------------
	proofPath := bookPath + "/trades/" + itoaBooks(fill.TradeSeq) + "/proof"
	res = get(asBob, proofPath)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var proof api.AdvancedBookTradeProof
	res.json(&proof)
	assert.Equal(t, api.SIMULATED, proof.ValueDomain)
	assert.Equal(t, fill.JournalTransactionId, proof.JournalTransactionId)
	assert.Equal(t, "5000000", proof.MakerFee)
	assert.Equal(t, "10000000", proof.TakerFee)
	assert.Equal(t, book.MakerFee.Hash, proof.MakerScheduleHash, "the schedule that priced it is the book's")
	// The SIM_TRADE journal exists, and the APPLIED effect names it and the
	// evidence, whose archived digest is the proof's.
	var kind string
	require.NoError(t, d.Pool().QueryRow(ctx, `SELECT kind FROM journal_transactions WHERE id = $1`, proof.JournalTransactionId).Scan(&kind))
	assert.Equal(t, string(ledger.KindSimTrade), kind)
	var effStatus, effEvidence string
	var effJournal uuid.UUID
	require.NoError(t, d.Pool().QueryRow(ctx, `SELECT status, journal_transaction_id, evidence_ref FROM economic_effects WHERE id = $1`,
		proof.EffectId).Scan(&effStatus, &effJournal, &effEvidence))
	assert.Equal(t, "APPLIED", effStatus)
	assert.Equal(t, uuid.UUID(proof.JournalTransactionId), effJournal)
	assert.Equal(t, proof.EvidenceRef, effEvidence)
	var stored []byte
	require.NoError(t, d.Pool().QueryRow(ctx, `SELECT sha256 FROM provider_evidence WHERE key = $1`,
		strings.TrimPrefix(proof.EvidenceRef, "pg://provider_evidence/")).Scan(&stored))
	assert.Equal(t, proof.EvidenceSha256, hex.EncodeToString(stored), "the archived document is the one the proof names")
	// The journal replays, from the database alone, to the cursor the proof
	// reports.
	res = get(operator, bookPath+"/journal-verification")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var verified api.AdvancedBookJournalVerification
	res.json(&verified)
	assert.Equal(t, verified.StateHash, verified.FullReplayHash)
	assert.Equal(t, verified.StateHash, verified.SnapshotReplayHash)
	assert.Equal(t, proof.BookStateHash, verified.StateHash)
	assert.Equal(t, proof.BookLastCommandSeq, verified.Commands)
	assert.EqualValues(t, 1, verified.Trades)
	// A customer cannot run the verification, and a stranger cannot see the proof.
	require.Equal(t, http.StatusForbidden, get(asBob, bookPath+"/journal-verification").Code)
	res = get(asCarol, proofPath)
	require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeNotFound, res.problem().Code)

	// --- a reused client order id is refused, and the refusal recorded ----------
	dup := map[string]any{
		"account_id": bob.String(), "side": "BUY", "order_type": "LIMIT", "time_in_force": "GTC",
		"price": "2400.00", "quantity": "10000", "client_order_id": "bob-1",
	}
	mark()
	res = post(asBob, bookPath+"/orders", dup, key("dup"))
	require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	refusal := res.problem()
	assert.Equal(t, errs.CodeValidationFailed, refusal.Code)
	assert.Equal(t, orders.ReasonDuplicateClientOrderID, refusal.Fields["reason"])
	dupOrder, _ := refusal.Fields["order_id"].(string)
	require.NotEmpty(t, dupOrder, "the refusal names the REJECTED order it recorded")
	res = get(asBob, "/v1/advanced/books/orders/"+dupOrder)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, "REJECTED", res.raw()["status"])
	assert.Equal(t, orders.ReasonDuplicateClientOrderID, res.raw()["reject_reason"])
	assert.Equal(t, "200000000", booksBalance(t, d, led, bob, ledger.CodeWallet, base.ID), "a refusal moves nothing")

	// --- a stranger reads neither account's orders -------------------------------
	res = get(asCarol, "/v1/advanced/books/orders?account_id="+alice.String())
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeForbidden, res.problem().Code)
	res = get(asCarol, "/v1/advanced/books/trades?account_id="+bob.String())
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	res = get(asCarol, "/v1/advanced/books/orders/"+aliceOrder)
	require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeNotFound, res.problem().Code)
	assert.NotContains(t, res.Body.String(), alice.String())
	// ... nor cancel one, even naming an account it owns.
	res = post(asCarol, "/v1/advanced/books/orders/"+aliceOrder+"/cancel", map[string]any{"account_id": carol.String()}, key("steal"))
	require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())

	// --- a resting order is cancelled exactly once ----------------------------
	rest := map[string]any{
		"account_id": carol.String(), "side": "BUY", "order_type": "LIMIT", "time_in_force": "GTC",
		"price": "2000.00", "quantity": "10000000",
	}
	mark()
	res = post(asCarol, bookPath+"/orders", rest, key("rest"))
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	var carolOrder api.AdvancedBookCommandResult
	res.json(&carolOrder)
	require.Equal(t, api.BookOrderOpen, carolOrder.Order.Status)
	res = get(asCarol, "/v1/advanced/books/orders?account_id="+carol.String())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var working api.AdvancedBookOrderPage
	res.json(&working)
	require.Len(t, working.Items, 1, "the working order, and nothing else")
	cancelKey := key("cancel")
	cancelPath := "/v1/advanced/books/orders/" + carolOrder.Order.Id.String() + "/cancel"
	cancelled := post(asCarol, cancelPath, map[string]any{"account_id": carol.String()}, cancelKey)
	require.Equal(t, http.StatusOK, cancelled.Code, "body=%s", cancelled.Body.String())
	assert.Equal(t, "CANCELLED", cancelled.raw()["order"].(map[string]any)["status"])
	replayed := post(asCarol, cancelPath, map[string]any{"account_id": carol.String()}, cancelKey)
	require.Equal(t, http.StatusOK, replayed.Code, "body=%s", replayed.Body.String())
	assert.JSONEq(t, cancelled.Body.String(), replayed.Body.String())
	assert.Equal(t, "1000000000", booksBalance(t, d, led, carol, ledger.CodeWallet, quote.ID), "the cancel released everything")
	res = get(asCarol, "/v1/advanced/books/orders?account_id="+carol.String())
	require.Equal(t, http.StatusOK, res.Code)
	res.json(&working)
	assert.Empty(t, working.Items, "nothing works any more")
	res = get(asCarol, "/v1/advanced/books/orders?scope=ALL&account_id="+carol.String())
	require.Equal(t, http.StatusOK, res.Code)
	res.json(&working)
	require.Len(t, working.Items, 1)

	// --- the operator halts the book; a new order is refused ----------------------
	res = post(operator, bookPath+"/status", map[string]any{"status": "HALTED", "reason": "itest drill"}, key("halt"))
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, "HALTED", res.raw()["status"])
	res = post(asCarol, bookPath+"/orders", rest, key("halted"))
	require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, orders.ReasonBookNotOpen, res.problem().Fields["reason"])
}

func itoaBooks(n int64) string { return strconv.FormatInt(n, 10) }
