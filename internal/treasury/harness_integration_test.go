//go:build integration

package treasury

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"sync"
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
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
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
		fmt.Fprintln(os.Stderr, "treasury integration: migrate up:", err)
		return 1
	}
	var err error
	if testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "treasury-itest", MaxConns: 40}); err != nil {
		fmt.Fprintln(os.Stderr, "treasury integration: open:", err)
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

// switchboard is a kill-switch reader a test can flip.
type switchboard struct {
	mu       sync.Mutex
	switches []killswitch.Switch
}

func (s *switchboard) Snapshot(context.Context, db.Querier) (killswitch.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return killswitch.Snapshot{Switches: append([]killswitch.Switch(nil), s.switches...)}, nil
}

func (s *switchboard) set(sw ...killswitch.Switch) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.switches = append([]killswitch.Switch(nil), sw...)
}

var (
	operator    = econ.Actor{Type: security.ActorOperator, ID: "operator-ada"}
	houseRobot  = econ.Actor{Type: security.ActorService, ID: "house-service-1"}
	otherRobot  = econ.Actor{Type: security.ActorService, ID: "house-service-2"}
	systemActor = econ.Actor{Type: security.ActorSystem, ID: "treasury-itest"}
	symbolSeq   atomic.Int64
)

type fixture struct {
	t       *testing.T
	ctx     context.Context
	clk     *clock.Fake
	kills   *switchboard
	ledger  *ledger.Service
	capital *capital.Service
	effects *econ.EffectStore
	auth    *econ.Authorizer
	prices  *valuation.PriceStore
	instr   *instruments.Repository
	assets  *assets.Repository
	orders  *orders.Service
	pipe    *orders.Pipeline
	svc     *Service
	usd     assets.Asset
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	requireEnv(t)
	clk := clock.NewFake(time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	out := event.NewOutbox(clk)
	aw := audit.NewWriterWithBuildVersion("treasury-itest")
	checker, err := gates.NewChecker("TEST", func(gates.Capability) bool { return true }, clk)
	require.NoError(t, err)
	router, err := legalrouter.New(legalrouter.ConservativePolicy())
	require.NoError(t, err)
	f := &fixture{
		t: t, ctx: context.Background(), clk: clk, kills: &switchboard{}, ledger: ledger.NewService(clk, "treasury-itest"),
		capital: capital.NewService(clk, capital.OutboxEmitter{Outbox: out, Clock: clk}),
		effects: econ.NewEffectStore(clk, out, aw), prices: valuation.NewPriceStore(clk),
		instr: instruments.NewRepository(), assets: assets.NewRepository(),
	}
	f.auth = econ.NewAuthorizer(checker, f.kills)
	f.orders, err = orders.NewService(orders.Config{
		Clock: clk, Ledger: f.ledger, Capital: f.capital, Intents: econ.NewIntentStore(clk, out, aw), Effects: f.effects,
		Authorizer: f.auth, Registry: econ.NewRegistry(), Risk: risk.NewStore(), RiskPolicy: risk.DefaultAdvancedPolicy(),
		Router: router, Instruments: f.instr, Assets: f.assets, Prices: f.prices, KillSwitches: f.kills,
	})
	require.NoError(t, err)
	f.pipe, err = orders.NewPipeline(f.orders, testDB)
	require.NoError(t, err)
	f.svc, err = NewService(Config{
		Clock: clk, Ledger: f.ledger, Effects: f.effects, Authorizer: f.auth, Registry: econ.NewRegistry(), Orders: f.orders,
		Prices: f.prices, Assets: f.assets, Instruments: f.instr, KillSwitches: f.kills, Audit: aw,
		RiskPolicy: risk.DefaultAdvancedPolicy(),
	})
	require.NoError(t, err)
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		f.usd, err = econ.EnsureSandboxDollar(ctx, tx)
		return err
	}))
	return f
}

func (f *fixture) inTx(fn func(ctx context.Context, tx pgx.Tx) error) error {
	return testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 5}, fn)
}

// do runs one command in its own transaction and returns its value.
func do[T any](f *fixture, fn func(ctx context.Context, tx pgx.Tx) (T, error)) (T, error) {
	var out T
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		v, err := fn(ctx, tx)
		out = v
		return err
	})
	return out, err
}

func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, testDB.QueryRow(f.ctx, sql, args...).Scan(&n))
	return n
}

func (f *fixture) simAsset(prefix string) assets.Asset {
	f.t.Helper()
	sym := fmt.Sprintf("%s%d%s", prefix, symbolSeq.Add(1), strings.ToUpper(uuid.NewString()[:6]))
	a, err := f.assets.Create(f.ctx, testDB, assets.Asset{
		Chain: assets.SandboxChain, Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated, Symbol: sym,
		Name: sym + " (sandbox)", Decimals: 6, RiskClass: assets.RiskSettlement, Status: assets.StatusActive,
	})
	require.NoError(f.t, err)
	return a
}

// units is a whole number of units of a 6-decimal asset, in base units.
func units(n int64) money.Quantity { return money.QuantityFromInt64(n * 1_000_000) }

func (f *fixture) price(s string) *money.Price {
	f.t.Helper()
	p, err := money.PriceFromDecimalString(s, f.usd.ID.String(), "treasury-itest", f.clk.Now())
	require.NoError(f.t, err)
	return &p
}

// mark records a fresh reference price of an asset in the sandbox dollar.
// The clock moves one second first: an observation is identified by its
// time, and a new price is a new observation.
func (f *fixture) mark(a assets.AssetID, price string) {
	f.t.Helper()
	f.clk.Advance(time.Second)
	p := f.price(price)
	_, err := f.prices.RecordPrice(f.ctx, testDB, valuation.PriceObservation{
		AssetID: a, QuoteAssetID: f.usd.ID, Mantissa: p.Mantissa, Scale: p.Scale, Source: "treasury-itest", ObservedAt: f.clk.Now(),
	})
	require.NoError(f.t, err)
}

// market is a sandbox spot book quoted in the sandbox dollar: a tick is one
// cent and a lot 0.0001 of the base, so one tick-lot is one quote base unit.
type market struct {
	inst instruments.InstrumentID
	base assets.Asset
	book orders.Book
}

func (f *fixture) market(price string) market {
	f.t.Helper()
	base := f.simAsset("TRB")
	m := market{base: base}
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		inst, err := f.instr.CreateSpotPair(ctx, tx, instruments.SpotPairSpec{
			Base: base.ID, Quote: f.usd.ID, Settlement: f.usd.ID, CanonicalName: base.Symbol + "/" + f.usd.Symbol,
			RiskClass: assets.RiskSettlement, Status: assets.StatusActive, ActiveFrom: f.clk.Now().Add(-time.Hour),
		})
		if err != nil {
			return err
		}
		baseID, quoteID := base.ID, f.usd.ID
		terms := instruments.Terms{
			Kind: instruments.TypeSpotPair, Symbol: "TRSPOT", VenueCode: "NODAL-SIM", BaseAssetID: &baseID, QuoteAssetID: &quoteID,
			SettlementAssetID: quoteID, ContractMultiplier: money.QuantityFromInt64(1), PriceScale: 2,
			TickSize: money.QuantityFromInt64(1), LotSize: money.QuantityFromInt64(100), MinQuantity: money.QuantityFromInt64(100),
			QuantityDecimals: 6, SettlementStyle: instruments.SettlePhysical, TradingCalendar: "24x7", OracleMaxAgeMS: 30_000,
			MarginModel: instruments.MarginNone, CorporateActionBehavior: instruments.CorporateActionsNone,
			Transferability: instruments.Transferable,
		}
		zero := fees.ZeroSchedule(f.usd.ID, 6)
		m.inst = inst.ID
		m.book, err = f.orders.OpenBook(ctx, tx, orders.OpenBookRequest{
			InstrumentID: inst.ID, Mode: gates.ModePaper, SpotTerms: &terms, MakerSchedule: zero, TakerSchedule: zero,
			SnapshotInterval: 16, Actor: systemActor,
		})
		return err
	}))
	f.mark(base.ID, price)
	return m
}

// register creates a treasury book whose house service is houseRobot.
func (f *fixture) register(strategy econ.HouseStrategy, operatorsAllowed bool) Book {
	f.t.Helper()
	b, err := do(f, func(ctx context.Context, tx pgx.Tx) (Book, error) {
		return f.svc.Register(ctx, tx, RegisterRequest{
			Name: "itest " + string(strategy) + " " + uuid.NewString(), Strategy: strategy, ValuationAsset: f.usd.ID,
			Signing:        SigningPolicy{ServiceActorID: houseRobot.ID, OperatorsAllowed: operatorsAllowed},
			IdempotencyKey: "register-" + uuid.NewString(), Reason: "integration test book", Actor: operator,
		})
	})
	require.NoError(f.t, err)
	return b
}

// policy is a permissive policy for one market: daily loss limit 1,000,
// inventory cap 100 units of the base, exposure cap 1,000,000 on the
// instrument, mark age 30s, no paper spread or fee.
func (f *fixture) policy(m market) Policy {
	return Policy{
		Label: "itest-" + uuid.NewString()[:8], ValuationAsset: f.usd.ID, DailyLossLimit: units(1_000),
		MaxMarkAge: 30 * time.Second, MaxInventory: []AssetLimit{{Asset: m.base.ID, MaxQuantity: units(100)}},
		MaxInstrumentExposure: []InstrumentLimit{{Instrument: m.inst, MaxExposure: units(1_000_000)}},
	}
}

func (f *fixture) setPolicy(b Book, p Policy) PolicyVersion {
	f.t.Helper()
	v, err := do(f, func(ctx context.Context, tx pgx.Tx) (PolicyVersion, error) {
		return f.svc.SetPolicy(ctx, tx, b.HouseAccountID, p, "policy-"+uuid.NewString(), "integration test limits", operator)
	})
	require.NoError(f.t, err)
	return v
}

func (f *fixture) fund(b Book, a assets.AssetID, qty money.Quantity) Funding {
	f.t.Helper()
	fd, err := do(f, func(ctx context.Context, tx pgx.Tx) (Funding, error) {
		return f.svc.Fund(ctx, tx, FundRequest{HouseAccountID: b.HouseAccountID, Asset: a, Quantity: qty,
			IdempotencyKey: "fund-" + uuid.NewString(), Reason: "seed the house book", Actor: operator})
	})
	require.NoError(f.t, err)
	return fd
}

func (f *fixture) place(r PlaceOrderRequest) OrderOutcome {
	f.t.Helper()
	out, err := do(f, func(ctx context.Context, tx pgx.Tx) (OrderOutcome, error) { return f.svc.PlaceOrder(ctx, tx, r) })
	require.NoError(f.t, err)
	return out
}

func (f *fixture) limit(b Book, m market, activity econ.HouseStrategy, side orders.Side, price string, qty money.Quantity) PlaceOrderRequest {
	return PlaceOrderRequest{
		HouseAccountID: b.HouseAccountID, InstrumentID: m.inst, Activity: activity, Side: side, Type: orders.TypeLimit,
		TimeInForce: orders.GTC, Price: f.price(price), Quantity: qty, IdempotencyKey: "order-" + uuid.NewString(), Actor: houseRobot,
	}
}

func (f *fixture) paper(b Book, activity econ.HouseStrategy, a assets.AssetID, side orders.Side, qty money.Quantity) PaperOutcome {
	f.t.Helper()
	out, err := do(f, func(ctx context.Context, tx pgx.Tx) (PaperOutcome, error) {
		return f.svc.ExecutePaper(ctx, tx, PaperRequest{HouseAccountID: b.HouseAccountID, Activity: activity, Asset: a, Side: side,
			Quantity: qty, IdempotencyKey: "paper-" + uuid.NewString(), Actor: houseRobot})
	})
	require.NoError(f.t, err)
	return out
}

func (f *fixture) balance(ref ledger.AccountRef) money.Quantity {
	f.t.Helper()
	b, err := f.ledger.Balance(f.ctx, testDB, ref)
	require.NoError(f.t, err)
	return b
}

func (f *fixture) inventory(b Book, a assets.AssetID) money.Quantity {
	return f.balance(b.account(ledger.CodeHouseInventory, a))
}

// customer creates a customer account whose owner has a verified email
// address, funded with qty of an asset.
func (f *fixture) customer(a assets.AssetID, qty money.Quantity) accounts.AccountID {
	f.t.Helper()
	repo := accounts.NewRepository()
	hash := sha256.Sum256([]byte(uuid.NewString()))
	u, err := repo.CreateUser(f.ctx, testDB, "treasury-itest", uuid.NewString(), hash[:])
	require.NoError(f.t, err)
	acct, err := repo.CreateAccount(f.ctx, testDB, u.ID, accounts.KindCustomer)
	require.NoError(f.t, err)
	if qty.IsPositive() {
		require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			ref := uuid.NewString()
			_, err := f.ledger.Post(ctx, tx, ledger.Posting{
				Kind: ledger.KindSandboxFunding, IdempotencyKey: "sbx-fund:" + ref,
				Reference: ledger.FinancialEventReference{Type: "sandbox_funding", ID: ref}, EffectiveAt: f.clk.Now(),
				Entries: []ledger.Entry{
					{Account: ledger.CustomerAccount(acct.ID, ledger.CodeWallet, a), Side: ledger.Debit, Quantity: qty},
					{Account: ledger.CustomerAccount(acct.ID, ledger.CodeCapital, a), Side: ledger.Credit, Quantity: qty},
				},
			})
			return err
		}))
	}
	return acct.ID
}

func (f *fixture) customerOrder(m market, acct accounts.AccountID, side orders.Side, price string, qty money.Quantity) (orders.StageOutcome, error) {
	return f.pipe.Submit(f.ctx, orders.OrderRequest{
		InstrumentID: m.inst, AccountID: acct, Side: side, Type: orders.TypeLimit, TimeInForce: orders.GTC, Price: f.price(price),
		Quantity: qty, IdempotencyKey: "cust-" + uuid.NewString(), ActorType: security.ActorUser, ActorID: acct.String(),
	})
}

func (f *fixture) reconcile(b Book) []Discrepancy {
	f.t.Helper()
	d, err := do(f, func(ctx context.Context, tx pgx.Tx) ([]Discrepancy, error) {
		return f.svc.Reconcile(ctx, tx, b.HouseAccountID)
	})
	require.NoError(f.t, err)
	return d
}

// requireOnlyHouseLegs checks that a journal touches nothing but this house
// book's own accounts.
func (f *fixture) requireOnlyHouseLegs(b Book, journal ledger.TransactionID) {
	f.t.Helper()
	jt, err := f.ledger.Transaction(f.ctx, testDB, journal)
	require.NoError(f.t, err)
	require.NotEmpty(f.t, jt.Entries)
	for _, e := range jt.Entries {
		require.Equal(f.t, ledger.OwnerHouse, e.Account.OwnerType, "entry %d is %s", e.Seq, e.Account.String())
		require.Equal(f.t, b.HouseAccountID.String(), e.Account.OwnerID)
	}
}
