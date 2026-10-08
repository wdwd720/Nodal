//go:build integration

package orders

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/feeledger"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

var (
	testAppURL     = os.Getenv("CP_TEST_DATABASE_URL")
	testMigrateURL = os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	testDB         *db.DB
)

func TestMain(m *testing.M) { os.Exit(testMain(m)) }

func testMain(m *testing.M) int {
	if testAppURL == "" || testMigrateURL == "" {
		return m.Run()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := migrate.Up(ctx, testMigrateURL); err != nil {
		fmt.Fprintln(os.Stderr, "orders integration: migrate up:", err)
		return 1
	}
	var err error
	if testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "orders-itest", MaxConns: 40}); err != nil {
		fmt.Fprintln(os.Stderr, "orders integration: open:", err)
		return 1
	}
	defer testDB.Close()
	return m.Run()
}

func requireEnv(t *testing.T) {
	t.Helper()
	if testDB == nil {
		t.Skip("set CP_TEST_DATABASE_URL and CP_TEST_MIGRATE_DATABASE_URL")
	}
}

var systemActor = econ.Actor{Type: security.ActorSystem, ID: "orders-itest"}

var symbolSeq atomic.Int64

// uniqueSymbol returns a symbol no other test uses. It is never "SBX-USD":
// the one sandbox dollar belongs to internal/econ.
func uniqueSymbol(prefix string) string {
	return fmt.Sprintf("%s%d%s", prefix, symbolSeq.Add(1), strings.ToUpper(uuid.NewString()[:6]))
}

type fixture struct {
	t        *testing.T
	ctx      context.Context
	clk      *clock.Fake
	ledger   *ledger.Service
	capital  *capital.Service
	intents  *econ.IntentStore
	effects  *econ.EffectStore
	auth     *econ.Authorizer
	registry *econ.Registry
	risk     *risk.Store
	prices   *valuation.PriceStore
	instr    *instruments.Repository
	assets   *assets.Repository
	kills    *killswitch.Checker
	router   *legalrouter.Router
	fees     *feeledger.Service
	svc      *Service
	pipe     *Pipeline
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	requireEnv(t)
	clk := clock.NewFake(time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	out := event.NewOutbox(clk)
	aw := audit.NewWriterWithBuildVersion("orders-itest")
	checker, err := gates.NewChecker("TEST", func(gates.Capability) bool { return true }, clk)
	require.NoError(t, err)
	router, err := legalrouter.New(legalrouter.ConservativePolicy())
	require.NoError(t, err)
	f := &fixture{
		t: t, ctx: context.Background(), clk: clk, ledger: ledger.NewService(clk, "orders-itest"),
		capital: capital.NewService(clk, capital.OutboxEmitter{Outbox: out, Clock: clk}),
		intents: econ.NewIntentStore(clk, out, aw), effects: econ.NewEffectStore(clk, out, aw),
		registry: econ.NewRegistry(), risk: risk.NewStore(), prices: valuation.NewPriceStore(clk),
		instr: instruments.NewRepository(), assets: assets.NewRepository(), kills: killswitch.NewChecker(killswitch.Policy{}),
		router: router,
	}
	f.auth = econ.NewAuthorizer(checker, f.kills)
	f.fees, err = feeledger.NewService(feeledger.Config{
		Clock: clk, Ledger: f.ledger, Capital: f.capital, Effects: f.effects, Authorizer: f.auth, Assets: f.assets, Audit: aw,
	})
	require.NoError(t, err)
	f.svc = f.newService()
	f.pipe, err = NewPipeline(f.svc, testDB)
	require.NoError(t, err)
	return f
}

// newService returns a second, independent service over the same database:
// what a second process sees.
func (f *fixture) newService() *Service {
	f.t.Helper()
	svc, err := NewService(Config{
		Clock: f.clk, Ledger: f.ledger, Capital: f.capital, Intents: f.intents, Effects: f.effects, Authorizer: f.auth,
		Registry: f.registry, Risk: f.risk, RiskPolicy: risk.DefaultAdvancedPolicy(), Router: f.router,
		Instruments: f.instr, Assets: f.assets, Prices: f.prices, KillSwitches: f.kills, FeeLedger: f.fees,
	})
	require.NoError(f.t, err)
	return svc
}

func (f *fixture) inTx(fn func(ctx context.Context, tx pgx.Tx) error) error {
	return testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 5}, fn)
}

// account creates a customer account whose owner has a verified email
// address, which is what NODAL_IDENTITY verification is.
func (f *fixture) account() accounts.AccountID {
	f.t.Helper()
	repo := accounts.NewRepository()
	hash := sha256.Sum256([]byte(uuid.NewString()))
	u, err := repo.CreateUser(f.ctx, testDB, "orders-itest", uuid.NewString(), hash[:])
	require.NoError(f.t, err)
	a, err := repo.CreateAccount(f.ctx, testDB, u.ID, accounts.KindCustomer)
	require.NoError(f.t, err)
	return a.ID
}

// unverifiedAccount creates a customer account that has proven nothing.
func (f *fixture) unverifiedAccount() accounts.AccountID {
	f.t.Helper()
	repo := accounts.NewRepository()
	u, err := repo.CreateUser(f.ctx, testDB, "orders-itest", uuid.NewString(), nil)
	require.NoError(f.t, err)
	a, err := repo.CreateAccount(f.ctx, testDB, u.ID, accounts.KindCustomer)
	require.NoError(f.t, err)
	return a.ID
}

func (f *fixture) simAsset(prefix string, decimals uint8, usdPegged bool) assets.Asset {
	f.t.Helper()
	sym := uniqueSymbol(prefix)
	a := assets.Asset{
		Chain: assets.SandboxChain, Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated,
		Symbol: sym, Name: sym + " (sandbox)", Decimals: decimals, RiskClass: assets.RiskSettlement, Status: assets.StatusActive,
	}
	if usdPegged {
		a.IsStablecoin, a.PegCurrency = true, "USD"
	}
	created, err := f.assets.Create(f.ctx, testDB, a)
	require.NoError(f.t, err)
	return created
}

func (f *fixture) fund(acct accounts.AccountID, asset assets.AssetID, qty int64) {
	f.t.Helper()
	q := money.QuantityFromInt64(qty)
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := f.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindSandboxFunding, IdempotencyKey: "sbx-fund:" + ref,
			Reference: ledger.FinancialEventReference{Type: "sandbox_funding", ID: ref}, EffectiveAt: f.clk.Now(),
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(acct, ledger.CodeWallet, asset), Side: ledger.Debit, Quantity: q},
				{Account: ledger.CustomerAccount(acct, ledger.CodeCapital, asset), Side: ledger.Credit, Quantity: q},
			},
		})
		return err
	}))
}

func (f *fixture) wallet(acct accounts.AccountID, asset assets.AssetID) money.Quantity {
	f.t.Helper()
	b, err := f.ledger.Balance(f.ctx, testDB, ledger.CustomerAccount(acct, ledger.CodeWallet, asset))
	require.NoError(f.t, err)
	return b
}

func (f *fixture) balance(ref ledger.AccountRef) money.Quantity {
	f.t.Helper()
	b, err := f.ledger.Balance(f.ctx, testDB, ref)
	require.NoError(f.t, err)
	return b
}

// spotMarket is a sandbox spot book: base and quote both have 6 decimals, the
// price scale is 2, a tick is 0.01 and a lot 0.0001, so one tick-lot is one
// quote base unit and 1 base unit at 100.00 is 100_000_000 quote base units.
type spotMarket struct {
	inst  instruments.InstrumentID
	base  assets.Asset
	quote assets.Asset
	book  Book
}

func bookSchedule(quote assets.AssetID, pct money.BPS) fees.Schedule {
	return bookScheduleWithStrategy(quote, pct, 0)
}

// bookScheduleWithStrategy also charges strategy-originated orders a
// strategy fee.
func bookScheduleWithStrategy(quote assets.AssetID, pct, strategy money.BPS) fees.Schedule {
	s := fees.ZeroSchedule(quote, 6)
	s.Version = fmt.Sprintf("orders-itest-%d-bps", pct)
	if strategy > 0 {
		s.Version = fmt.Sprintf("orders-itest-%d-bps-strategy-%d-bps", pct, strategy)
	}
	s.PercentageFeeBPS, s.StrategyFeeBPS = pct, strategy
	return s
}

func spotTerms(base, quote assets.AssetID) instruments.Terms {
	return instruments.Terms{
		Kind: instruments.TypeSpotPair, Symbol: "SBXSPOT", VenueCode: "NODAL-SIM", BaseAssetID: &base, QuoteAssetID: &quote,
		SettlementAssetID: quote, ContractMultiplier: money.QuantityFromInt64(1), PriceScale: 2,
		TickSize: money.QuantityFromInt64(1), LotSize: money.QuantityFromInt64(100), MinQuantity: money.QuantityFromInt64(100),
		QuantityDecimals: 6, SettlementStyle: instruments.SettlePhysical, TradingCalendar: "24x7", OracleMaxAgeMS: 30_000,
		MarginModel: instruments.MarginNone, CorporateActionBehavior: instruments.CorporateActionsNone,
		Transferability: instruments.Transferable,
	}
}

type marketOpts struct {
	makerBPS, takerBPS money.BPS
	// strategyBPS is both schedules' strategy fee.
	strategyBPS      money.BPS
	snapshotInterval int
	mode             gates.Mode
}

func (f *fixture) spotMarket(o marketOpts) spotMarket {
	f.t.Helper()
	if o.snapshotInterval == 0 {
		o.snapshotInterval = 8
	}
	if o.mode == "" {
		o.mode = gates.ModePaper
	}
	base := f.simAsset("OBB", 6, false)
	quote := f.simAsset("OBQ", 6, true)
	m := spotMarket{base: base, quote: quote}
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		inst, err := f.instr.CreateSpotPair(ctx, tx, instruments.SpotPairSpec{
			Base: base.ID, Quote: quote.ID, Settlement: quote.ID, CanonicalName: base.Symbol + "/" + quote.Symbol,
			RiskClass: assets.RiskSettlement, Status: assets.StatusActive, ActiveFrom: f.clk.Now().Add(-time.Hour),
		})
		if err != nil {
			return err
		}
		terms := spotTerms(base.ID, quote.ID)
		m.inst = inst.ID
		m.book, err = f.svc.OpenBook(ctx, tx, OpenBookRequest{
			InstrumentID: inst.ID, Mode: o.mode, SpotTerms: &terms,
			MakerSchedule:    bookScheduleWithStrategy(quote.ID, o.makerBPS, o.strategyBPS),
			TakerSchedule:    bookScheduleWithStrategy(quote.ID, o.takerBPS, o.strategyBPS),
			SnapshotInterval: o.snapshotInterval, Actor: systemActor,
		})
		return err
	}))
	f.mark(m, "100.00")
	return m
}

// mark records a reference price of the market's base in its quote, observed
// now.
func (f *fixture) mark(m spotMarket, price string) {
	f.t.Helper()
	p, err := money.PriceFromDecimalString(price, m.quote.ID.String(), "orders-itest", f.clk.Now())
	require.NoError(f.t, err)
	_, err = f.prices.RecordPrice(f.ctx, testDB, valuation.PriceObservation{
		AssetID: m.base.ID, QuoteAssetID: m.quote.ID, Mantissa: p.Mantissa, Scale: p.Scale, Source: "orders-itest",
		ObservedAt: f.clk.Now(),
	})
	require.NoError(f.t, err)
}

func (m spotMarket) price(t *testing.T, s string) *money.Price {
	t.Helper()
	p, err := money.PriceFromDecimalString(s, m.quote.ID.String(), "order", time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	return &p
}

// units is a whole number of base (or quote) units in base units (6 decimals).
func units(n int64) money.Quantity { return money.QuantityFromInt64(n * 1_000_000) }

func (f *fixture) order(m spotMarket, acct accounts.AccountID, side Side, typ OrderType, tif TimeInForce, price string, qty money.Quantity) OrderRequest {
	return OrderRequest{
		InstrumentID: m.inst, AccountID: acct, Side: side, Type: typ, TimeInForce: tif, Price: m.price(f.t, price),
		Quantity: qty, IdempotencyKey: "itest-" + uuid.NewString(), ActorType: security.ActorUser, ActorID: acct.String(),
	}
}

func (f *fixture) limit(m spotMarket, acct accounts.AccountID, side Side, price string, qty money.Quantity) OrderRequest {
	return f.order(m, acct, side, TypeLimit, GTC, price, qty)
}

func (f *fixture) submit(r OrderRequest) StageOutcome {
	f.t.Helper()
	out, err := f.pipe.Submit(f.ctx, r)
	require.NoError(f.t, err)
	return out
}

func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, testDB.QueryRow(f.ctx, sql, args...).Scan(&n))
	return n
}

func (f *fixture) houseBook(name string) econ.HouseAccount {
	f.t.Helper()
	var h econ.HouseAccount
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		h, err = f.registry.CreateHouseAccount(ctx, tx, name+" "+uuid.NewString(), econ.HouseMarketMaking, systemActor)
		return err
	}))
	return h
}

func (f *fixture) fundHouse(h econ.HouseAccount, asset assets.AssetID, qty money.Quantity) {
	f.t.Helper()
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := f.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindHouseFunding, IdempotencyKey: "house-fund:" + ref,
			Reference: ledger.FinancialEventReference{Type: "house_funding", ID: ref}, EffectiveAt: f.clk.Now(),
			Entries: []ledger.Entry{
				{Account: h.Account(ledger.CodeHouseInventory, asset), Side: ledger.Debit, Quantity: qty},
				{Account: h.Account(ledger.CodeHouseCapital, asset), Side: ledger.Credit, Quantity: qty},
			},
		})
		return err
	}))
}

// requireArchived checks that an evidence URI names a document in the
// write-once archive with exactly this digest.
func (f *fixture) requireArchived(uri string, digest []byte) {
	f.t.Helper()
	key := strings.TrimPrefix(uri, "pg://provider_evidence/")
	require.NotEqual(f.t, uri, key, "evidence lives in the database archive")
	var stored []byte
	require.NoError(f.t, testDB.QueryRow(f.ctx, `SELECT sha256 FROM provider_evidence WHERE key = $1`, key).Scan(&stored))
	require.Equal(f.t, digest, stored)
}

// requireNoCrossedBook checks the persisted resting book: the best bid is
// strictly below the best ask.
func (f *fixture) requireNoCrossedBook(m spotMarket) {
	f.t.Helper()
	var bid, ask *int64
	require.NoError(f.t, testDB.QueryRow(f.ctx, `SELECT
		(SELECT max(price_ticks) FROM book_orders WHERE instrument_id = $1 AND side = 'BUY' AND status IN ('OPEN','PARTIALLY_FILLED')),
		(SELECT min(price_ticks) FROM book_orders WHERE instrument_id = $1 AND side = 'SELL' AND status IN ('OPEN','PARTIALLY_FILLED'))`,
		m.inst).Scan(&bid, &ask))
	if bid != nil && ask != nil {
		require.Less(f.t, *bid, *ask, "the resting book is crossed")
	}
}
