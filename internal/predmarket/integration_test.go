//go:build integration

package predmarket

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
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
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
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
		fmt.Fprintln(os.Stderr, "predmarket integration: migrate up:", err)
		return 1
	}
	var err error
	if testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "predmarket-itest", MaxConns: 40}); err != nil {
		fmt.Fprintln(os.Stderr, "predmarket integration: open:", err)
		return 1
	}
	defer testDB.Close()
	return m.Run()
}

func requireEnv(t testing.TB) {
	t.Helper()
	if testDB == nil {
		t.Skip("set CP_TEST_DATABASE_URL and CP_TEST_MIGRATE_DATABASE_URL")
	}
}

// t0 is where every fixture's clock starts. Markets open at t0 and close an
// hour later.
var t0 = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

type fixture struct {
	t          testing.TB
	ctx        context.Context
	clk        *clock.Fake
	svc        *Service
	ledger     *ledger.Service
	capital    *capital.Service
	recon      *reconciliation.Repository
	oracle     *SandboxOracle
	collateral assets.AssetID
	// Three operators: the creator of every market, a resolver, and a third
	// who disputes or settles. Dual control keeps the first two apart.
	creator, resolver, third econ.Actor
	system                   econ.Actor
}

type fixtureOption func(*Config)

func withKillSwitches(k econ.KillSwitchReader) fixtureOption {
	return func(c *Config) {
		gc, err := gates.NewChecker("LOCAL", func(gates.Capability) bool { return true }, c.Clock)
		if err != nil {
			panic(err)
		}
		c.KillSwitches = k
		c.Authorizer = econ.NewAuthorizer(gc, k)
	}
}

func newFixture(t testing.TB, opts ...fixtureOption) *fixture {
	requireEnv(t)
	clk := clock.NewFake(t0)
	out := event.NewOutbox(clk)
	aw := audit.NewWriterWithBuildVersion("predmarket-itest")
	gc, err := gates.NewChecker("LOCAL", func(gates.Capability) bool { return true }, clk)
	require.NoError(t, err)
	kills := killswitch.NewChecker(killswitch.Policy{})
	router, err := legalrouter.New(legalrouter.ConservativePolicy())
	require.NoError(t, err)
	oracle, err := NewSandboxOracle("sandbox-election-board", sha256Seed("predmarket-itest-oracle"))
	require.NoError(t, err)
	led := ledger.NewService(clk, "predmarket-itest")
	capSvc := capital.NewService(clk, capital.OutboxEmitter{Outbox: out, Clock: clk})
	recon := reconciliation.NewRepository(clk, out, aw)
	cfg := Config{
		Clock: clk, Intents: econ.NewIntentStore(clk, out, aw), Effects: econ.NewEffectStore(clk, out, aw),
		Authorizer: econ.NewAuthorizer(gc, kills), Registry: econ.NewRegistry(), KillSwitches: kills,
		Ledger: led, Capital: capSvc, Risk: risk.NewStore(), RiskPolicy: risk.DefaultAdvancedPolicy(),
		Router: router, Instruments: instruments.NewRepository(), Assets: assets.NewRepository(),
		Reconciliation: recon, Audit: aw, Oracle: oracle,
	}
	for _, o := range opts {
		o(&cfg)
	}
	svc, err := NewService(cfg)
	require.NoError(t, err)
	f := &fixture{
		t: t, ctx: context.Background(), clk: clk, svc: svc, ledger: led, capital: capSvc, recon: recon, oracle: oracle,
		system: econ.SystemActor("predmarket-scheduler"),
	}
	f.collateral = f.sandboxDollar()
	f.creator, f.resolver, f.third = f.operator(), f.operator(), f.operator()
	return f
}

func sha256Seed(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// sandboxDollar creates this test's collateral: a USD-pegged six-decimal
// SIMULATED asset under a symbol of its own (the platform's one SBX-USD
// belongs to econ).
func (f *fixture) sandboxDollar() assets.AssetID {
	f.t.Helper()
	a, err := assets.NewRepository().Create(f.ctx, testDB, assets.Asset{
		Chain: assets.SandboxChain, Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated,
		Symbol: "PMUSD-" + strings.ToUpper(uuid.NewString()[:8]), Name: "prediction market test dollar (simulated)",
		Decimals: 6, IsStablecoin: true, PegCurrency: "USD", RiskClass: assets.RiskSettlement, Status: assets.StatusActive,
	})
	require.NoError(f.t, err)
	return a.ID
}

func (f *fixture) user(verified bool) accounts.UserID {
	f.t.Helper()
	var emailHash []byte
	if verified {
		sum := sha256.Sum256([]byte(uuid.NewString()))
		emailHash = sum[:]
	}
	u, err := accounts.NewRepository().CreateUser(f.ctx, testDB, "predmarket-itest", uuid.NewString(), emailHash)
	require.NoError(f.t, err)
	return u.ID
}

func (f *fixture) operator() econ.Actor {
	return econ.Actor{Type: security.ActorOperator, ID: f.user(true).String()}
}

// customer creates a verified customer account funded with units collateral
// units.
func (f *fixture) customer(units int64) accounts.AccountID {
	return f.customerWith(true, units)
}

func (f *fixture) customerWith(verified bool, units int64) accounts.AccountID {
	f.t.Helper()
	a, err := accounts.NewRepository().CreateAccount(f.ctx, testDB, f.user(verified), accounts.KindCustomer)
	require.NoError(f.t, err)
	if units > 0 {
		f.fund(a.ID, f.collateral, units*shareUnitBaseUnits)
	}
	return a.ID
}

func (f *fixture) fund(acct accounts.AccountID, asset assets.AssetID, qty int64) {
	f.t.Helper()
	amount := money.QuantityFromInt64(qty)
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := f.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindSandboxFunding, IdempotencyKey: "itest-fund:" + ref,
			Reference: ledger.FinancialEventReference{Type: "sandbox_funding", ID: ref}, EffectiveAt: f.clk.Now(),
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(acct, ledger.CodeWallet, asset), Side: ledger.Debit, Quantity: amount},
				{Account: ledger.CustomerAccount(acct, ledger.CodeCapital, asset), Side: ledger.Credit, Quantity: amount},
			},
		})
		return err
	}))
}

// inTx runs fn in its own READ COMMITTED transaction, rolled back on error.
func (f *fixture) inTx(fn func(ctx context.Context, tx pgx.Tx) error) error {
	return testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, fn)
}

// inTxKeepingRefusal commits whatever fn left in the transaction even when fn
// refuses -- the REJECTED intent and the risk decision a refusal records --
// and returns fn's error.
func (f *fixture) inTxKeepingRefusal(fn func(ctx context.Context, tx pgx.Tx) error) error {
	var opErr error
	err := testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		opErr = fn(ctx, tx)
		if opErr != nil && db.IsRetryable(opErr) {
			return opErr
		}
		return nil
	})
	if err != nil {
		return err
	}
	return opErr
}

func randomSymbol(prefix string) string {
	return prefix + strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", "")[:12])
}

func (f *fixture) spec(outcomes ...string) MarketSpec {
	return MarketSpec{
		Symbol: randomSymbol("PM"), Question: "Which outcome will the sandbox election board certify?",
		Outcomes: outcomes, CollateralAssetID: f.collateral, Mode: gates.ModeSandbox,
		OpenAt: f.clk.Now(), CloseAt: f.clk.Now().Add(time.Hour),
		OracleSource: f.oracle.Source(), OraclePublicKey: f.oracle.PublicKey(),
	}
}

// openMarket creates and opens a market with the given outcomes.
func (f *fixture) openMarket(outcomes ...string) Market {
	f.t.Helper()
	return f.openMarketFrom(f.spec(outcomes...))
}

func (f *fixture) openMarketFrom(spec MarketSpec) Market {
	f.t.Helper()
	var m Market
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		m, _, err = f.svc.CreateMarket(ctx, tx, spec, f.creator)
		return err
	}))
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		m, err = f.svc.Open(ctx, tx, m.ID, f.creator)
		return err
	}))
	require.Equal(f.t, StatusOpen, m.Status)
	return m
}

func (f *fixture) positionRequest(m Market, acct accounts.AccountID, sets int64) PositionRequest {
	return PositionRequest{
		MarketID: m.ID, AccountID: acct, Quantity: money.QuantityFromInt64(sets * shareUnitBaseUnits),
		IdempotencyKey: "itest-" + uuid.NewString(), ActorType: security.ActorUser, ActorID: "user-" + acct.String(),
	}
}

func (f *fixture) tryIssue(req PositionRequest) (ActionResult, error) {
	var res ActionResult
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = f.svc.Issue(ctx, tx, req)
		return err
	})
	return res, err
}

func (f *fixture) issue(m Market, acct accounts.AccountID, sets int64) ActionResult {
	f.t.Helper()
	res, err := f.tryIssue(f.positionRequest(m, acct, sets))
	require.NoError(f.t, err)
	require.Equal(f.t, econ.StatusCompleted, res.Intent.Status)
	return res
}

func (f *fixture) redeem(m Market, acct accounts.AccountID, sets int64) (ActionResult, error) {
	var res ActionResult
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = f.svc.Redeem(ctx, tx, f.positionRequest(m, acct, sets))
		return err
	})
	return res, err
}

// trade is a TEST-ONLY stand-in for the internal order book, which another
// lane builds: it posts the SIM_TRADE that a matched trade of shares of one
// outcome against price collateral posts -- buyer and seller each balanced
// per party. Settlement must be right whoever ends up holding the shares.
func (f *fixture) trade(seller, buyer accounts.AccountID, share assets.AssetID, shares, price int64) {
	f.t.Helper()
	require.NoError(f.t, f.tryTrade(seller, buyer, share, shares, price))
}

func (f *fixture) tryTrade(seller, buyer accounts.AccountID, share assets.AssetID, shares, price int64) error {
	s, p := money.QuantityFromInt64(shares), money.QuantityFromInt64(price)
	return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		entries := []ledger.Entry{
			{Account: ledger.CustomerAccount(seller, ledger.CodeWallet, share), Side: ledger.Credit, Quantity: s},
			{Account: ledger.CustomerAccount(seller, ledger.CodeTradingOutflow, share), Side: ledger.Debit, Quantity: s},
			{Account: ledger.CustomerAccount(buyer, ledger.CodeWallet, share), Side: ledger.Debit, Quantity: s},
			{Account: ledger.CustomerAccount(buyer, ledger.CodeTradingInflow, share), Side: ledger.Credit, Quantity: s},
		}
		if price > 0 {
			entries = append(entries,
				ledger.Entry{Account: ledger.CustomerAccount(buyer, ledger.CodeWallet, f.collateral), Side: ledger.Credit, Quantity: p},
				ledger.Entry{Account: ledger.CustomerAccount(buyer, ledger.CodeTradingOutflow, f.collateral), Side: ledger.Debit, Quantity: p},
				ledger.Entry{Account: ledger.CustomerAccount(seller, ledger.CodeWallet, f.collateral), Side: ledger.Debit, Quantity: p},
				ledger.Entry{Account: ledger.CustomerAccount(seller, ledger.CodeTradingInflow, f.collateral), Side: ledger.Credit, Quantity: p},
			)
		}
		_, err := f.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindSimTrade, IdempotencyKey: "itest-trade:" + ref,
			Reference: ledger.FinancialEventReference{Type: "itest_sim_trade", ID: ref}, EffectiveAt: f.clk.Now(),
			Entries: entries,
		})
		return err
	})
}

func (f *fixture) wallet(acct accounts.AccountID, asset assets.AssetID) money.Quantity {
	f.t.Helper()
	b, err := f.ledger.Balance(f.ctx, testDB, ledger.CustomerAccount(acct, ledger.CodeWallet, asset))
	require.NoError(f.t, err)
	return b
}

func (f *fixture) books(m Market) EscrowBooks {
	f.t.Helper()
	b, err := f.svc.Books(f.ctx, testDB, m)
	require.NoError(f.t, err)
	return b
}

func (f *fixture) get(m Market) Market {
	f.t.Helper()
	got, err := f.svc.Get(f.ctx, testDB, m.ID)
	require.NoError(f.t, err)
	return got
}

// closeMarket moves the clock past close and closes the market through the
// scheduler path.
func (f *fixture) closeMarket(m Market) Market {
	f.t.Helper()
	if f.clk.Now().Before(m.CloseAt) {
		f.clk.Set(m.CloseAt.Add(time.Minute))
	}
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.CloseDue(ctx, tx, f.system, 1000)
		return err
	}))
	got := f.get(m)
	require.Equal(f.t, StatusClosed, got.Status)
	return got
}

// report has the sandbox oracle observe an outcome after close and returns
// its signed report.
func (f *fixture) report(m Market, outcome string) OracleReport {
	f.t.Helper()
	at := m.CloseAt.Add(30 * time.Second)
	if f.clk.Now().Before(at) {
		f.clk.Set(at.Add(time.Second))
	}
	require.NoError(f.t, f.oracle.Observe(m.ID, outcome, at, []byte(`{"certified":"`+outcome+`","board":"sandbox"}`)))
	r, err := f.svc.FetchReport(f.ctx, m.ID)
	require.NoError(f.t, err)
	return r
}

func (f *fixture) requestResolution(m Market) {
	f.t.Helper()
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.RequestResolution(ctx, tx, m.ID, f.system)
		return err
	}))
}

func (f *fixture) tryResolve(m Market, r OracleReport, by econ.Actor) (ResolveResult, error) {
	var out ResolveResult
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = f.svc.Resolve(ctx, tx, ResolveRequest{MarketID: m.ID, Report: r, Actor: by})
		return err
	})
	return out, err
}

// resolve closes the market if needed and resolves it to outcome.
func (f *fixture) resolve(m Market, outcome string) ResolveResult {
	f.t.Helper()
	if f.get(m).Status == StatusOpen {
		f.closeMarket(m)
	}
	f.requestResolution(m)
	res, err := f.tryResolve(m, f.report(m, outcome), f.resolver)
	require.NoError(f.t, err)
	require.Equal(f.t, StatusResolved, res.Market.Status)
	return res
}

func (f *fixture) trySettle(m Market, by econ.Actor) (SettleResult, error) {
	var out SettleResult
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = f.svc.Settle(ctx, tx, SettleRequest{MarketID: m.ID, Actor: by})
		return err
	})
	return out, err
}

func (f *fixture) tryVoid(m Market, by econ.Actor) (SettleResult, error) {
	var out SettleResult
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = f.svc.Void(ctx, tx, VoidRequest{MarketID: m.ID, Actor: by, Reason: "the event was cancelled by its organiser",
			Evidence: map[string]string{"notice": "cancelled"}})
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

func (f *fixture) instrumentStatus(iid instruments.InstrumentID) assets.Status {
	f.t.Helper()
	inst, err := instruments.NewRepository().Get(f.ctx, testDB, iid)
	require.NoError(f.t, err)
	return inst.Status
}

// requireConserved asserts the escrow identity from the books, with the
// disclosed residual of a refunded market.
func (f *fixture) requireConserved(m Market, residual money.Quantity) EscrowBooks {
	f.t.Helper()
	b := f.books(m)
	require.Empty(f.t, b.Violations(residual), "the escrow identity must hold")
	return b
}

func units(n int64) money.Quantity { return money.QuantityFromInt64(n * shareUnitBaseUnits) }

func requireCode(t testing.TB, err error, code errs.Code, msgAndArgs ...any) {
	t.Helper()
	require.Error(t, err, msgAndArgs...)
	require.Equal(t, code, errs.CodeOf(err), "%v", err)
}
