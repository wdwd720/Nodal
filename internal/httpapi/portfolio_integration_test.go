//go:build integration

package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
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
	"github.com/nodal/controlplane/internal/capital/buyingpower"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/derivatives"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/feeledger"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/margin"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/orders"
	"github.com/nodal/controlplane/internal/predmarket"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// portfolioWorld is every subsystem the portfolio composes, as cmd/api builds
// them: the real services over the integration database, on one fake clock,
// sharing one ledger, one capital service, one price store and one set of
// econ stores, with one verified customer and one operator acting through
// HTTP. No equities service, agent authority service or fee ledger is
// configured, so those sections must say so.
type portfolioWorld struct {
	t        *testing.T
	d        *db.DB
	clk      *clock.Fake
	h        *harness
	led      *ledger.Service
	capital  *capital.Service
	prices   *valuation.PriceStore
	effects  *econ.EffectStore
	records  *reconciliation.Repository
	oracle   *predmarket.SandboxOracle
	usd      assets.Asset
	sol      assets.Asset
	acct     accounts.AccountID
	customer security.Principal
	operator security.Principal
}

func newPortfolioWorld(t *testing.T) *portfolioWorld {
	t.Helper()
	d := openTestDB(t)
	ctx := t.Context()
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Second))
	w := &portfolioWorld{t: t, d: d, clk: clk}

	require.NoError(t, d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		w.usd, err = econ.EnsureSandboxDollar(ctx, tx)
		return err
	}))
	var err error
	w.sol, err = assets.NewRepository().Create(ctx, d, assets.Asset{
		Chain: assets.SandboxChain, Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated,
		Symbol: "PFSOL" + portfolioToken(), Name: "portfolio itest SOL (sandbox)", Decimals: 9,
		RiskClass: assets.RiskMajor, Status: assets.StatusActive,
	})
	require.NoError(t, err)

	outbox := event.NewOutbox(clk)
	aw := audit.NewWriterWithBuildVersion("httpapi-portfolio-itest")
	checker, err := gates.NewChecker("TEST", func(gates.Capability) bool { return false }, clk)
	require.NoError(t, err)
	kills := killswitch.NewChecker(killswitch.Policy{})
	router, err := legalrouter.New(legalrouter.ConservativePolicy())
	require.NoError(t, err)
	w.led = ledger.NewService(clk, "httpapi-portfolio-itest")
	w.capital = capital.NewService(clk, capital.OutboxEmitter{Outbox: outbox, Clock: clk})
	w.prices = valuation.NewPriceStore(clk)
	w.effects = econ.NewEffectStore(clk, outbox, aw)
	w.records = reconciliation.NewRepository(clk, outbox, aw)
	intents := econ.NewIntentStore(clk, outbox, aw)
	authorizer, riskStore := econ.NewAuthorizer(checker, kills), risk.NewStore()
	instrumentRepo, assetRepo := instruments.NewRepository(), assets.NewRepository()
	// The fee ledger margin's liquidation fees and the book's fees are booked
	// through, as cmd/api builds it.
	feeLedger, err := feeledger.NewService(feeledger.Config{
		Clock: clk, Ledger: w.led, Capital: w.capital, Effects: w.effects, Authorizer: authorizer, Assets: assetRepo, Audit: aw,
	})
	require.NoError(t, err)

	pol := margin.DefaultPolicy()
	pol.Version = "portfolio-itest-" + uuid.NewString()
	pol.AssetRules = []margin.AssetRule{{AssetID: w.sol.ID.String(), RiskWeightBPS: 10_000, Liquidity: margin.LiqDeep}}
	require.NoError(t, pol.Validate())
	marginSvc, err := margin.NewService(margin.Config{
		Policy: pol, RiskPolicy: risk.DefaultAdvancedPolicy(),
		LendingPoolName: "portfolio itest pool " + uuid.NewString(), InsuranceFundName: "portfolio itest fund " + uuid.NewString(),
	}, margin.Dependencies{
		Clock: clk, Ledger: w.led, Effects: w.effects, Intents: intents, Authorizer: authorizer, Registry: econ.NewRegistry(),
		Risk: riskStore, Capital: w.capital, Prices: w.prices, Router: router, Identity: compliance.NewAccountVerification(clk),
		KillSwitch: kills, Venue: margin.PaperVenue{}, Audit: aw, FeeLedger: feeLedger,
	})
	require.NoError(t, err)
	derivSvc, err := derivatives.NewService(derivatives.Config{
		Clock: clk, Intents: intents, Effects: w.effects, Authorizer: authorizer, Registry: econ.NewRegistry(), Ledger: w.led,
		Capital: w.capital, RiskStore: riskStore, RiskPolicy: risk.DefaultAdvancedPolicy(), Policy: derivatives.DefaultPolicy(),
		Prices: w.prices, Instruments: instrumentRepo, Router: router, KillSwitches: kills, Reconciliation: w.records,
		USDAssetID: econ.SandboxDollarID(), ClearingHouseName: "portfolio itest clearing house " + uuid.NewString(),
	})
	require.NoError(t, err)
	seed := sha256.Sum256([]byte("httpapi-portfolio-itest-oracle"))
	w.oracle, err = predmarket.NewSandboxOracle("portfolio-itest-board", seed[:])
	require.NoError(t, err)
	predSvc, err := predmarket.NewService(predmarket.Config{
		Clock: clk, Intents: intents, Effects: w.effects, Authorizer: authorizer, Registry: econ.NewRegistry(), KillSwitches: kills,
		Ledger: w.led, Capital: w.capital, Risk: riskStore, RiskPolicy: risk.DefaultAdvancedPolicy(), Router: router,
		Instruments: instrumentRepo, Assets: assetRepo, Reconciliation: w.records, Audit: aw, SandboxOracle: w.oracle,
	})
	require.NoError(t, err)
	booksSvc, err := orders.NewService(orders.Config{
		Clock: clk, Ledger: w.led, Capital: w.capital, Intents: intents, Effects: w.effects, Authorizer: authorizer,
		Registry: econ.NewRegistry(), Risk: riskStore, RiskPolicy: risk.DefaultAdvancedPolicy(), Router: router,
		Instruments: instrumentRepo, Assets: assetRepo, Prices: w.prices, KillSwitches: kills,
		Verification: AdvancedVerificationLevels{Rule: compliance.NewAccountVerification(clk)}, FeeLedger: feeLedger,
	})
	require.NoError(t, err)
	pipe, err := orders.NewPipeline(booksSvc, d)
	require.NoError(t, err)

	ports, err := Wire(WireDeps{
		DB: d, Clock: clk, Env: config.EnvTest, Ledger: w.led, Accounts: accounts.NewRepository(), Assets: assetRepo,
		Instruments: instrumentRepo,
		Advanced: AdvancedDeps{
			Authorizer: authorizer, Intents: intents, Effects: w.effects, Risk: riskStore, Prices: w.prices, MaxPriceAge: time.Minute,
			Margin:            buyingpower.CombinedMarginReader{marginSvc.BuyingPowerReader(), derivSvc.MarginReader()},
			MarginService:     marginSvc,
			Derivatives:       DerivativesDeps{Service: derivSvc, ValuationAssetID: econ.SandboxDollarID()},
			Orders:            OrdersDeps{Service: booksSvc, Pipeline: pipe},
			PredictionMarkets: predSvc, PredictionOracle: w.oracle,
			Portfolio: PortfolioDeps{Capital: w.capital, Reconciliation: w.records},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, ports.Portfolio, "the capital service and the reconciliation records wire the portfolio")
	require.Nil(t, ports.Equities, "this world runs no equities service")

	user, acct := w.verifiedCustomer()
	w.acct = acct
	w.h = newIntegrationHarness(t, d, acct, user)
	served := &w.h.server.opts.Ports
	served.Advanced, served.Margin, served.Derivatives, served.Books = ports.Advanced, ports.Margin, ports.Derivatives, ports.Books
	served.PredictionMarkets, served.Portfolio = ports.PredictionMarkets, ports.Portfolio
	w.customer = *w.h.princip
	w.operator = security.Principal{
		SubjectID: uuid.NewString(), ActorType: security.ActorOperator, Roles: []security.Role{security.RoleAdmin},
		SessionID: testSessionID, AuthTime: time.Now().UTC(), AMR: []string{"pwd", "mfa"},
	}
	return w
}

// portfolioToken is a short random suffix for symbols no other test uses.
func portfolioToken() string {
	return strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", "")[:8])
}

// verifiedCustomer is a customer whose owner has a verified email address and
// a VERIFIED identity profile, which is everything any advanced capability
// asks of simulated activity (D-055).
func (w *portfolioWorld) verifiedCustomer() (accounts.UserID, accounts.AccountID) {
	w.t.Helper()
	ctx := w.t.Context()
	repo := accounts.NewRepository()
	email := sha256.Sum256([]byte(uuid.NewString() + "@sandbox.invalid"))
	u, err := repo.CreateUser(ctx, w.d, "httpapi-portfolio-itest", "sub-"+uuid.NewString(), email[:])
	require.NoError(w.t, err)
	a, err := repo.CreateAccount(ctx, w.d, u.ID, accounts.KindCustomer)
	require.NoError(w.t, err)
	verified, expires := w.clk.Now().Add(-time.Hour), w.clk.Now().Add(365*24*time.Hour)
	require.NoError(w.t, w.d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := compliance.NewRepository(audit.NewWriter()).Upsert(ctx, tx, compliance.Profile{
			UserID: u.ID, IdentityState: compliance.IdentityVerified, SanctionsState: compliance.SanctionsClear,
			VerifiedAt: &verified, ExpiresAt: &expires,
		}, compliance.Change{ActorType: security.ActorSystem, ActorID: "kyc-itest", Reason: "fixture"})
		return err
	}))
	return u.ID, a.ID
}

// post runs a command as p, with a fresh idempotency key and a strong
// authentication of right now.
func (w *portfolioWorld) post(p security.Principal, path string, body any) *response {
	w.t.Helper()
	p.AuthTime = time.Now().UTC()
	return w.h.as(&p).do(http.MethodPost, path, body, "Idempotency-Key", "portfolio-itest-"+uuid.NewString())
}

func (w *portfolioWorld) get(p security.Principal, path string) *response {
	w.t.Helper()
	p.AuthTime = time.Now().UTC()
	return w.h.as(&p).do(http.MethodGet, path, nil)
}

// fund credits the customer's wallet with sandbox dollars (SANDBOX_FUNDING).
func (w *portfolioWorld) fund(units int64) {
	w.t.Helper()
	q := money.QuantityFromInt64(units)
	require.NoError(w.t, w.d.InTx(w.t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := w.led.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindSandboxFunding, IdempotencyKey: "portfolio-itest-fund:" + ref,
			Reference: ledger.FinancialEventReference{Type: "sandbox_funding", ID: ref}, EffectiveAt: w.clk.Now(),
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(w.acct, ledger.CodeWallet, w.usd.ID), Side: ledger.Debit, Quantity: q},
				{Account: ledger.CustomerAccount(w.acct, ledger.CodeCapital, w.usd.ID), Side: ledger.Credit, Quantity: q},
			},
		})
		return err
	}))
}

// mark records the oracle's price of an asset in the sandbox dollar, now.
func (w *portfolioWorld) mark(asset assets.AssetID, price string) {
	w.t.Helper()
	p, err := money.PriceFromDecimalString(price, w.usd.ID.String(), "portfolio-itest-oracle", w.clk.Now())
	require.NoError(w.t, err)
	_, err = w.prices.RecordPrice(w.t.Context(), w.d, valuation.PriceObservation{
		AssetID: asset, QuoteAssetID: w.usd.ID, Mantissa: p.Mantissa, Scale: p.Scale, Source: "portfolio-itest-oracle",
		ObservedAt: w.clk.Now(),
	})
	require.NoError(w.t, err)
}

// simulatedAsset creates a sandbox asset with a symbol no other test uses.
func (w *portfolioWorld) simulatedAsset(prefix string, decimals uint8, risk assets.RiskClass) assets.Asset {
	w.t.Helper()
	sym := prefix + portfolioToken()
	a, err := assets.NewRepository().Create(w.t.Context(), w.d, assets.Asset{
		Chain: assets.SandboxChain, Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated,
		Symbol: sym, Name: sym + " (sandbox)", Decimals: decimals, RiskClass: risk, Status: assets.StatusActive,
	})
	require.NoError(w.t, err)
	return a
}

// spotPair lists base against the sandbox dollar.
func (w *portfolioWorld) spotPair(base assets.Asset) instruments.InstrumentID {
	w.t.Helper()
	var id instruments.InstrumentID
	require.NoError(w.t, w.d.InTx(w.t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		i, err := instruments.NewRepository().CreateSpotPair(ctx, tx, instruments.SpotPairSpec{
			Base: base.ID, Quote: w.usd.ID, Settlement: w.usd.ID, CanonicalName: base.Symbol + "/SBX-USD " + uuid.NewString(),
			RiskClass: assets.RiskMajor, Status: assets.StatusActive, ActiveFrom: w.clk.Now().Add(-time.Hour),
		})
		id = i.ID
		return err
	}))
	return id
}

// listPerpetual lists a perpetual on a sandbox underlying (the terms of the
// derivatives lane's own integration test): one contract is 0.01 of the
// underlying, prices have 2 decimals on a $0.50 tick.
func (w *portfolioWorld) listPerpetual() instruments.InstrumentID {
	w.t.Helper()
	under := w.spotPair(w.simulatedAsset("PFBTC", 8, assets.RiskMajor))
	var perp instruments.InstrumentID
	require.NoError(w.t, w.d.InTx(w.t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		terms := instruments.Terms{
			Kind: instruments.TypePerpetual, Symbol: "PFPERP" + portfolioToken(), VenueCode: "NODAL-SANDBOX",
			SettlementAssetID: w.usd.ID, UnderlyingInstrumentID: &under, ContractMultiplier: money.QuantityFromInt64(1_000_000),
			PriceScale: 2, TickSize: money.QuantityFromInt64(50), LotSize: money.QuantityFromInt64(1),
			MinQuantity: money.QuantityFromInt64(1), SettlementStyle: instruments.SettleCash, TradingCalendar: derivatives.Calendar24x7,
			OracleMaxAgeMS: 60_000, OracleSources: []string{"itest-operator"}, MarginModel: instruments.MarginLinear,
			CorporateActionBehavior: instruments.CorporateActionsNone, Transferability: instruments.NonTransferable,
			FundingIntervalMS: 8 * 3600 * 1000,
		}
		ins, _, err := instruments.NewRepository().CreateAdvanced(ctx, tx, instruments.AdvancedSpec{
			Terms: terms, CanonicalName: terms.Symbol, RiskClass: assets.RiskSpeculative, Status: assets.StatusActive,
			ActiveFrom: w.clk.Now().Add(-time.Hour), ActorType: "OPERATOR", ActorID: "itest-operator",
		})
		perp = ins.ID
		return err
	}))
	return perp
}

// openPredictionMarket has the operator define and open a YES/NO market on
// the sandbox oracle, and returns its id.
func (w *portfolioWorld) openPredictionMarket() string {
	w.t.Helper()
	now := w.clk.Now()
	res := w.post(w.operator, "/v1/advanced/prediction-markets", map[string]any{
		"symbol": "PFPM-" + portfolioToken(), "question": "Which outcome will the sandbox election board certify?",
		"outcomes": []string{"YES", "NO"}, "mode": "SANDBOX", "open_at": now, "close_at": now.Add(time.Hour),
		"dispute_window_ms": 0, "oracle_source": w.oracle.Source(), "oracle_public_key": hex.EncodeToString(w.oracle.PublicKey()),
	})
	require.Equal(w.t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	var market api.PredictionMarket
	res.json(&market)
	id := market.Id.String()
	res = w.post(w.operator, "/v1/advanced/prediction-markets/"+id+"/open", nil)
	require.Equal(w.t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	return id
}

// openBook has the operator open a PAPER internal book of base against the
// sandbox dollar.
func (w *portfolioWorld) openBook(base assets.Asset, inst instruments.InstrumentID) {
	w.t.Helper()
	fee := func(version string, pct int) map[string]any {
		return map[string]any{"version": version, "quote_asset_id": w.usd.ID.String(), "quote_decimals": 6, "percentage_fee_bps": pct}
	}
	res := w.post(w.operator, "/v1/advanced/books", map[string]any{
		"instrument_id": inst.String(), "mode": "PAPER", "snapshot_interval": 2,
		"maker_fee": fee("portfolio-maker-10bps", 10), "taker_fee": fee("portfolio-taker-20bps", 20),
		"spot_terms": map[string]any{
			"kind": "SPOT_PAIR", "symbol": base.Symbol + "/SBX-USD", "venue_code": "NODAL-SIM", "base_asset_id": base.ID.String(),
			"quote_asset_id": w.usd.ID.String(), "settlement_asset_id": w.usd.ID.String(), "contract_multiplier": "1",
			"price_scale": 2, "tick_size": "1", "lot_size": "10000", "min_quantity": "10000", "quantity_decimals": 8,
			"settlement_style": "PHYSICAL", "trading_calendar": "24x7", "jurisdiction_tags": []string{},
			"oracle_max_age_ms": 600000, "oracle_sources": []string{"portfolio-itest-oracle"}, "margin_model": "NONE",
			"corporate_action_behavior": "NONE", "transferability": "TRANSFERABLE",
		},
	})
	require.Equal(w.t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
}

// blockNewRisk records what stands in the way of new risk on the account: an
// economic effect whose outcome is UNKNOWN, and the reconciliation record
// opened on it.
func (w *portfolioWorld) blockNewRisk() (econ.Effect, reconciliation.Record) {
	w.t.Helper()
	var effect econ.Effect
	var record reconciliation.Record
	actor := econ.SystemActor("portfolio-itest")
	require.NoError(w.t, w.d.InTx(w.t.Context(), db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		begun, _, err := w.effects.Begin(ctx, tx, econ.BeginRequest{
			Key: "portfolio-itest:" + uuid.NewString(), Subsystem: econ.SubsystemBridge, Kind: "BRIDGE_SOURCE_FINALIZED",
			AccountID: w.acct.String(), Request: map[string]string{"quantity": "1"}, PostsLedger: true, Actor: actor,
		})
		if err != nil {
			return err
		}
		if effect, err = w.effects.MarkUnknown(ctx, tx, begun.ID, "devnet-tx-portfolio", actor, "the submission timed out"); err != nil {
			return err
		}
		record, err = w.records.Open(ctx, tx, reconciliation.OpenRequest{
			Kind: reconciliation.KindEconomicEffect, Mode: reconciliation.ModeEventDriven, ScopeType: "ECONOMIC_EFFECT",
			ScopeID: effect.ID.String(), AccountID: w.acct, AssetID: w.usd.ID, Status: reconciliation.StatusMismatch,
			Material: true, BlocksNewRisk: true, Actor: reconciliation.SystemActor(), Reason: "an effect of unknown outcome",
		})
		return err
	}))
	return effect, record
}

// TestIntegration_PortfolioIsTheServicesOwnReads drives every subsystem the
// portfolio composes through HTTP over the real services and database -- a
// funded, verified customer opens a derivative position, buys prediction
// shares, rests an order on an internal book and opens a margin position;
// an effect of unknown outcome and the reconciliation record holding it are
// recorded -- and then reads the portfolio. Every figure is checked against
// the services' own reads to the base unit: each balance against the
// ledger's balances, the capital service's availability and reservations and
// the price store's mark; each section against the subsystem route's own
// answer, byte for byte. The equities section says it is not configured, a
// stranger reads nothing, and an operator reads exactly what the owner reads.
func TestIntegration_PortfolioIsTheServicesOwnReads(t *testing.T) {
	w := newPortfolioWorld(t)
	ctx := t.Context()
	acct, cust, op := w.acct, w.customer, w.operator
	w.fund(100_000_000_000) // 100,000 sandbox dollars

	// --- a derivative position: a capitalized clearing house, a mark, LONG 10.
	perp := w.listPerpetual()
	res := w.post(op, "/v1/advanced/derivatives/clearing-house/funding", map[string]any{"quantity": "1000000000000"})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	res = w.post(op, "/v1/advanced/derivatives/contracts/"+perp.String()+"/marks", map[string]any{
		"mark": "65000.00", "index": "65000.00", "source": "itest-operator", "observed_at": w.clk.Now().Format(time.RFC3339Nano),
	})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	res = w.post(cust, "/v1/advanced/derivatives/positions", map[string]any{
		"account_id": acct.String(), "instrument_id": perp.String(), "side": "LONG", "quantity": "10", "mode": "PAPER",
	})
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())

	// --- prediction shares: three complete sets of a market the operator opened.
	market := w.openPredictionMarket()
	res = w.post(cust, "/v1/advanced/prediction-markets/"+market+"/issuances", map[string]any{"account_id": acct.String(), "quantity": "3000000"})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	// --- a resting order: a BUY of one unit below the mark, which nothing crosses.
	base := w.simulatedAsset("PFOBB", 8, assets.RiskMajor)
	inst := w.spotPair(base)
	w.openBook(base, inst)
	w.mark(base.ID, "2500.00")
	res = w.post(cust, "/v1/advanced/books/"+inst.String()+"/orders", map[string]any{
		"account_id": acct.String(), "side": "BUY", "order_type": "LIMIT", "time_in_force": "GTC",
		"price": "2490.00", "quantity": "100000000", "client_order_id": "portfolio-itest-1",
	})
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	var rested api.AdvancedBookCommandResult
	res.json(&rested)
	require.Equal(t, api.BookOrderOpen, rested.Order.Status, "the order rests")

	// --- a margin position: seeded facilities, SOL at 100, 1,000 posted, 5 SOL bought on margin.
	res = w.post(op, "/v1/advanced/margin/ops/facilities/seed", map[string]any{"pool_amount": "1000000000000", "fund_amount": "100000000000"})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	w.mark(w.sol.ID, "100")
	res = w.post(cust, "/v1/advanced/margin/account", map[string]any{"account_id": acct.String(), "margin_mode": "CROSS", "mode": "PAPER"})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	res = w.post(cust, "/v1/advanced/margin/collateral/post", map[string]any{
		"account_id": acct.String(), "asset_id": w.usd.ID.String(), "quantity": "1000000000",
	})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	res = w.post(cust, "/v1/advanced/margin/leveraged-buys", map[string]any{
		"account_id": acct.String(), "asset_id": w.sol.ID.String(), "quantity": "5000000000",
	})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	// --- what stands in the way of new risk.
	unknown, record := w.blockNewRisk()

	// ======================================================================
	// The portfolio.
	// ======================================================================
	path := "/v1/advanced/portfolio?account_id=" + acct.String()
	res = w.get(cust, path)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	owned := res.Body.String()
	var pf api.AdvancedPortfolio
	res.json(&pf)
	doc := res.raw()
	now := w.clk.Now()

	assert.Equal(t, api.SIMULATED, pf.ValueDomain)
	assert.Equal(t, acct.String(), pf.AccountId.String())
	assert.True(t, pf.QuoteProvisioned)
	assert.Equal(t, w.usd.ID.String(), pf.QuoteAssetId.String())
	assert.Equal(t, econ.SandboxDollarSymbol, pf.QuoteSymbol)
	assert.Equal(t, int(econ.SandboxDollarDecimals), pf.QuoteDecimals)
	assert.Equal(t, time.Minute.Milliseconds(), pf.MaxMarkAgeMs)
	assert.True(t, pf.AsOf.Equal(now), "the snapshot is taken at the clock's instant")

	// --- balances: exactly the SIMULATED assets the ledger holds for the
	// account, each figure the ledger's, the capital service's and the price
	// store's own.
	ledgerBalances, err := w.led.BalancesForOwner(ctx, w.d, ledger.OwnerCustomer, acct.String())
	require.NoError(t, err)
	held := map[string]bool{}
	for _, b := range ledgerBalances {
		if !portfolioReadsCode(b.Account.Code) || b.Balance.IsZero() {
			continue
		}
		a, err := assets.NewRepository().Get(ctx, w.d, b.Account.AssetID)
		require.NoError(t, err)
		if a.Kind == assets.KindSimulated {
			held[a.ID.String()] = true
		}
	}
	listed := map[string]api.AdvancedPortfolioBalance{}
	for _, b := range pf.Balances {
		listed[b.AssetId.String()] = b
		assert.Equal(t, api.SIMULATED, b.ValueDomain)
	}
	assert.Equal(t, portfolioKeys(held), portfolioKeysOf(listed), "every SIMULATED asset the ledger holds, and nothing else")
	require.Len(t, listed, 4, "the sandbox dollar, the SOL bought on margin and the two outcome shares")
	require.Contains(t, listed, w.sol.ID.String())

	for id, b := range listed {
		assetID, err := assets.ParseAssetID(id)
		require.NoError(t, err)
		asset, err := assets.NewRepository().Get(ctx, w.d, assetID)
		require.NoError(t, err)
		ledgerOf := func(code ledger.Code) money.Quantity {
			t.Helper()
			q, err := w.led.Balance(ctx, w.d, ledger.CustomerAccount(acct, code, asset.ID))
			require.NoError(t, err)
			return q
		}
		assert.Equal(t, asset.Symbol, b.Symbol, id)
		assert.Equal(t, asset.Chain, b.Chain, id)
		assert.Equal(t, int(asset.Decimals), b.Decimals, id)
		assert.Equal(t, ledgerOf(ledger.CodeWallet).String(), b.Wallet, "%s wallet", asset.Symbol)
		total := ledgerOf(ledger.CodeWallet)
		elsewhere := map[string]string{}
		for _, e := range b.HeldElsewhere {
			elsewhere[string(e.Code)] = e.Quantity
		}
		for _, code := range portfolioElsewhereCodes {
			q := ledgerOf(code)
			total = total.Add(q)
			if q.IsZero() {
				assert.NotContains(t, elsewhere, string(code), "%s %s", asset.Symbol, code)
				continue
			}
			assert.Equal(t, q.String(), elsewhere[string(code)], "%s %s", asset.Symbol, code)
		}
		owed := map[string]string{}
		owedTotal := money.Quantity{}
		for _, o := range b.Owed {
			owed[string(o.Code)] = o.Quantity
		}
		for _, code := range portfolioOwedCodes {
			q := ledgerOf(code)
			owedTotal = owedTotal.Add(q)
			if q.IsZero() {
				assert.NotContains(t, owed, string(code), "%s %s", asset.Symbol, code)
				continue
			}
			assert.Equal(t, q.String(), owed[string(code)], "%s %s", asset.Symbol, code)
		}
		assert.Equal(t, total.String(), b.Total, "%s total", asset.Symbol)

		avail, err := w.capital.Availability(ctx, w.d, acct.String(), asset.ID, now)
		require.NoError(t, err)
		assert.Equal(t, avail.Reserved.String(), b.Reserved, "%s reserved", asset.Symbol)
		assert.Equal(t, avail.Held.String(), b.WithdrawalHolds, "%s holds", asset.Symbol)
		assert.Equal(t, avail.Available.String(), b.Available, "%s available", asset.Symbol)
		active, err := w.capital.ListActive(ctx, w.d, acct.String(), asset.ID)
		require.NoError(t, err)
		reservedSum, purposeSum, count := money.Quantity{}, money.Quantity{}, 0
		for _, r := range active {
			reservedSum = reservedSum.Add(r.Remaining())
		}
		for _, r := range b.Reservations {
			q, err := money.ParseQuantity(r.Quantity)
			require.NoError(t, err)
			purposeSum = purposeSum.Add(q)
			count += r.Reservations
		}
		assert.Equal(t, reservedSum.String(), b.Reserved, "%s: the reservations are the capital service's", asset.Symbol)
		assert.Equal(t, b.Reserved, purposeSum.String(), "%s: the purposes add up to what is reserved", asset.Symbol)
		assert.Equal(t, len(active), count, asset.Symbol)

		switch {
		case asset.ID == w.usd.ID:
			assert.Equal(t, api.PortfolioValuedAtPar, b.Valuation.Status)
			require.NotNil(t, b.Valuation.Value)
			assert.Equal(t, b.Total, *b.Valuation.Value, "the sandbox dollar is valued at face")
			require.NotNil(t, b.Valuation.OwedValue)
			assert.Equal(t, owedTotal.String(), *b.Valuation.OwedValue)
		case asset.ID == w.sol.ID:
			assert.Equal(t, api.PortfolioValuedAtMark, b.Valuation.Status)
			mark, err := w.prices.Latest(ctx, w.d, w.sol.ID, w.usd.ID, time.Minute, now)
			require.NoError(t, err)
			want, err := money.Notional(total, w.sol.Decimals, mark, w.usd.Decimals, money.RoundDown)
			require.NoError(t, err)
			require.NotNil(t, b.Valuation.Value)
			assert.Equal(t, want.String(), *b.Valuation.Value, "the price store's mark, rounded down")
			require.NotNil(t, b.Valuation.Mark)
			assert.Equal(t, mark.Mantissa.String(), b.Valuation.Mark.Mantissa)
			assert.Equal(t, "100", b.Valuation.Mark.Price)
			assert.Equal(t, int64(0), b.Valuation.Mark.AgeMs)
		default:
			// The outcome shares: no mark values them, so no value is given.
			assert.Equal(t, api.PortfolioUnvalued, b.Valuation.Status, asset.Symbol)
			assert.Nil(t, b.Valuation.Value, asset.Symbol)
			require.NotNil(t, b.Valuation.Problem, asset.Symbol)
			assert.Equal(t, string(errs.CodeStaleMarketData), b.Valuation.Problem.Code, asset.Symbol)
		}
	}

	// The sandbox dollar holds the margin collateral, the derivatives margin
	// and the loan, and the resting order is its one reservation.
	usdBalance, ok := listed[w.usd.ID.String()]
	require.True(t, ok)
	assert.Equal(t, "1000000000", portfolioHeldAs(usdBalance, api.PortfolioCodeMarginCollateral), "the 1,000 posted")
	assert.NotEmpty(t, portfolioHeldAs(usdBalance, api.PortfolioCodeDerivativesMargin), "the derivative's margin")
	require.Len(t, usdBalance.Reservations, 1)
	assert.Equal(t, api.PortfolioReservedOpenOrders, usdBalance.Reservations[0].Purpose)
	require.NotNil(t, usdBalance.Reservations[0].Capability)
	assert.Equal(t, string(gates.InternalMatching), *usdBalance.Reservations[0].Capability)
	assert.NotEqual(t, "0", usdBalance.Reserved, "the resting BUY holds sandbox dollars back")
	require.Len(t, usdBalance.Owed, 1)
	assert.Equal(t, api.PortfolioCodeMarginLoan, usdBalance.Owed[0].Code)

	// --- positions: each the subsystem route's own answer, byte for byte.
	same := func(route string, at ...any) {
		t.Helper()
		own := w.get(cust, route)
		require.Equal(t, http.StatusOK, own.Code, "%s body=%s", route, own.Body.String())
		assert.JSONEq(t, own.Body.String(), portfolioJSON(t, portfolioAt(t, doc, at...)), "%v is %s's own answer", at, route)
	}
	require.Equal(t, api.PortfolioSectionPresent, pf.BuyingPower.State)
	same("/v1/advanced/buying-power?account_id="+acct.String(), "buying_power", "buying_power")
	require.Equal(t, api.PortfolioSectionPresent, pf.Positions.Margin.State)
	same("/v1/advanced/margin/account?account_id="+acct.String(), "positions", "margin", "account")
	require.Equal(t, api.PortfolioSectionPresent, pf.Positions.Derivatives.State)
	same("/v1/advanced/derivatives/positions?status=OPEN&limit=50&account_id="+acct.String(), "positions", "derivatives", "positions")
	require.NotNil(t, pf.Positions.Derivatives.Positions)
	require.Len(t, pf.Positions.Derivatives.Positions.Items, 1)
	assert.Equal(t, "10", pf.Positions.Derivatives.Positions.Items[0].Quantity)
	assert.Equal(t, portfolioHeldAs(usdBalance, api.PortfolioCodeDerivativesMargin), pf.Positions.Derivatives.Positions.Items[0].MarginPosted,
		"the derivatives margin balance is the position's posted margin")
	require.Equal(t, api.PortfolioSectionPresent, pf.Positions.PredictionMarkets.State)
	require.Len(t, pf.Positions.PredictionMarkets.Markets, 1)
	assert.False(t, pf.Positions.PredictionMarkets.Truncated)
	assert.Equal(t, market, pf.Positions.PredictionMarkets.Markets[0].Market.Id.String())
	same("/v1/advanced/prediction-markets/"+market+"/positions?account_id="+acct.String(),
		"positions", "prediction_markets", "markets", 0, "positions")
	shares := map[string]string{}
	for _, p := range pf.Positions.PredictionMarkets.Markets[0].Positions.Items {
		shares[p.OutcomeSymbol] = p.Quantity
		assert.Equal(t, p.Quantity, listed[p.ShareAssetId.String()].Wallet, "the shares are the share asset's wallet balance")
	}
	assert.Equal(t, map[string]string{"YES": "3000000", "NO": "3000000"}, shares)
	assert.Equal(t, api.PortfolioSectionNotConfigured, pf.Positions.Equities.State, "no equities service runs here")
	assert.Empty(t, pf.Positions.Equities.Holdings)
	assert.Nil(t, pf.Positions.Equities.PendingSettlements)
	assert.Equal(t, api.PortfolioSectionNotConfigured, pf.Delegation.State, "no agent authority service runs here")
	assert.Nil(t, pf.Delegation.Capabilities)
	assert.Equal(t, api.PortfolioSectionNotConfigured, pf.PerformanceFees.State, "no fee ledger runs here")
	assert.Nil(t, pf.PerformanceFees.Periods)
	require.Equal(t, api.PortfolioSectionPresent, pf.Positions.OrderBooks.State)
	same("/v1/advanced/books/orders?scope=WORKING&limit=50&account_id="+acct.String(), "positions", "order_books", "orders")
	require.NotNil(t, pf.Positions.OrderBooks.Orders)
	require.Len(t, pf.Positions.OrderBooks.Orders.Items, 1)
	assert.Equal(t, rested.Order.Id, pf.Positions.OrderBooks.Orders.Items[0].Id)

	// --- risk: the newest decisions as their route answers them, and what
	// stands in the way of new risk, from the stores that hold it.
	require.Equal(t, api.PortfolioSectionPresent, pf.Risk.Decisions.State)
	decisions := w.get(cust, "/v1/advanced/risk/decisions?limit=20&account_id="+acct.String())
	require.Equal(t, http.StatusOK, decisions.Code, "body=%s", decisions.Body.String())
	assert.JSONEq(t, portfolioJSON(t, decisions.raw()["items"]), portfolioJSON(t, portfolioAt(t, doc, "risk", "decisions", "items")))
	assert.NotEmpty(t, pf.Risk.Decisions.Items, "every command above was decided by Risk Kernel V2")
	assert.Equal(t, "ACTIVE", pf.Risk.AccountStatus)
	blocked, _, err := reconciliation.NewBlocks(w.records, reconciliation.DefaultPolicy()).BlocksNewRisk(ctx, w.d, acct)
	require.NoError(t, err)
	require.True(t, blocked)
	assert.True(t, pf.Risk.BlocksNewRisk, "reconciliation's own answer")
	require.Len(t, pf.Risk.Reconciliation, 1)
	rec := pf.Risk.Reconciliation[0]
	assert.Equal(t, record.ID.String(), rec.Id.String())
	assert.Equal(t, string(reconciliation.KindEconomicEffect), rec.Kind)
	assert.Equal(t, string(reconciliation.StatusMismatch), rec.Status)
	assert.True(t, rec.BlocksNewRisk)
	assert.True(t, rec.Material)
	require.Len(t, pf.Risk.UnknownEffects, 1)
	assert.Equal(t, unknown.ID.String(), pf.Risk.UnknownEffects[0].Id.String())
	assert.Equal(t, api.EconomicEffectStatus("UNKNOWN"), pf.Risk.UnknownEffects[0].Status)

	// --- scoping: a stranger reads nothing; an operator reads exactly what the
	// owner reads.
	_, strangerAcct := w.verifiedCustomer()
	stranger := security.Principal{
		SubjectID: uuid.NewString(), ActorType: security.ActorUser, Roles: []security.Role{security.RoleCustomer},
		AccountIDs: []string{strangerAcct.String()}, SessionID: testSessionID, AMR: []string{"pwd", "mfa"},
	}
	res = w.get(stranger, path)
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeForbidden, res.problem().Code)
	assert.NotContains(t, res.Body.String(), w.usd.ID.String())

	res = w.get(op, path)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.JSONEq(t, owned, res.Body.String(), "an operator reading the account reads the owner's portfolio")
}

// TestIntegration_PortfolioOfAnAccountWithNothing reads the portfolio of a
// fresh account over the same wiring: no balances, a margin section that says
// the account has no margin account, empty sections that are PRESENT, and an
// absent account that is NOT_FOUND to an operator only.
func TestIntegration_PortfolioOfAnAccountWithNothing(t *testing.T) {
	w := newPortfolioWorld(t)
	res := w.get(w.customer, "/v1/advanced/portfolio?account_id="+w.acct.String())
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var pf api.AdvancedPortfolio
	res.json(&pf)
	assert.Empty(t, pf.Balances)
	assert.Equal(t, api.PortfolioSectionNone, pf.Positions.Margin.State, "no margin account is NONE, not a refusal")
	assert.Nil(t, pf.Positions.Margin.Problem)
	assert.Equal(t, api.PortfolioSectionPresent, pf.Positions.Derivatives.State)
	assert.Equal(t, api.PortfolioSectionPresent, pf.Positions.PredictionMarkets.State)
	assert.Empty(t, pf.Positions.PredictionMarkets.Markets)
	assert.Equal(t, api.PortfolioSectionPresent, pf.Positions.OrderBooks.State)
	assert.Equal(t, api.PortfolioSectionNotConfigured, pf.Positions.Equities.State)
	assert.Equal(t, api.PortfolioSectionPresent, pf.BuyingPower.State)
	require.NotNil(t, pf.BuyingPower.BuyingPower)
	assert.Equal(t, "0.00", pf.BuyingPower.BuyingPower.GrossEquity)
	assert.False(t, pf.Risk.BlocksNewRisk)
	assert.Empty(t, pf.Risk.Reconciliation)
	assert.Empty(t, pf.Risk.UnknownEffects)

	absent := accounts.NewAccountID()
	res = w.get(w.customer, "/v1/advanced/portfolio?account_id="+absent.String())
	require.Equal(t, http.StatusForbidden, res.Code, "a customer cannot tell an absent account from a foreign one; body=%s", res.Body.String())
	res = w.get(w.operator, "/v1/advanced/portfolio?account_id="+absent.String())
	require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeNotFound, res.problem().Code)
}

func portfolioKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func portfolioKeysOf(m map[string]api.AdvancedPortfolioBalance) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// portfolioHeldAs is a balance's quantity held under one ledger code, "" when
// none is listed.
func portfolioHeldAs(b api.AdvancedPortfolioBalance, code api.AdvancedPortfolioLedgerCode) string {
	for _, e := range b.HeldElsewhere {
		if e.Code == code {
			return e.Quantity
		}
	}
	return ""
}
