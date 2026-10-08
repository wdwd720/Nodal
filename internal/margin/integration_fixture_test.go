//go:build integration

package margin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
	"github.com/nodal/controlplane/internal/gates"
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
		fmt.Fprintln(os.Stderr, "margin integration: migrate up:", err)
		return 1
	}
	var err error
	if testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "margin-itest", MaxConns: 40}); err != nil {
		fmt.Fprintln(os.Stderr, "margin integration: open:", err)
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

// capitalEmitter forwards capital events to the outbox for the topics the
// registry knows, exactly as the composition root's adapter does.
type capitalEmitter struct {
	outbox *event.Outbox
	clk    clock.Clock
}

func (e capitalEmitter) Emit(ctx context.Context, tx pgx.Tx, topic string, payload any) error {
	if _, ok := event.Lookup(event.Topic(topic)); !ok {
		return nil
	}
	ev, ok := payload.(capital.ReservationEvent)
	if !ok {
		return nil
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return e.outbox.Enqueue(ctx, tx, topic, event.Envelope{
		ID: event.NewEventID().String(), Type: topic, SchemaVersion: event.Topic(topic).Version(), Source: "capital",
		AggregateType: event.AggregateReservation, AggregateID: ev.ReservationID, OccurredAt: e.clk.Now(), Payload: body,
	})
}

type identity struct{ verified bool }

func (i identity) IdentityVerified(context.Context, db.Querier, accounts.AccountID) (bool, error) {
	return i.verified, nil
}

// switchVenue is a paper venue an operator can take down.
type switchVenue struct{ down atomic.Bool }

func (v *switchVenue) Available(context.Context, assets.AssetID) (bool, error) {
	return !v.down.Load(), nil
}

// killReader returns a fixed kill-switch snapshot.
type killReader struct{ snap killswitch.Snapshot }

func (k killReader) Snapshot(context.Context, db.Querier) (killswitch.Snapshot, error) {
	return k.snap, nil
}

type fixture struct {
	t      *testing.T
	ctx    context.Context
	clk    *clock.Fake
	svc    *Service
	deps   Dependencies
	cfg    Config
	ledger *ledger.Service
	prices *valuation.PriceStore
	venue  *switchVenue
	usd    assets.AssetID
	sol    assets.AssetID
	actor  econ.Actor
}

var userRequester = Requester{Type: security.ActorUser, ID: "margin-itest-user"}

func newFixture(t *testing.T) *fixture {
	requireEnv(t)
	f := &fixture{t: t, ctx: context.Background(), clk: clock.NewFake(time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)),
		venue: &switchVenue{}, actor: econ.Actor{Type: security.ActorSystem, ID: "margin-itest"}}
	f.usd = f.dollar()
	f.sol = f.simAsset("MSOL", 9, assets.RiskMajor)

	pol := DefaultPolicy()
	pol.Version = "margin-itest-" + uuid.NewString()
	// SOL trades deep: the fixed account of the unit tests, on real books.
	pol.AssetRules = []AssetRule{{AssetID: f.sol.String(), RiskWeightBPS: 10_000, Liquidity: LiqDeep}}
	require.NoError(t, pol.Validate())
	f.cfg = Config{Policy: pol, RiskPolicy: risk.DefaultAdvancedPolicy(),
		LendingPoolName: "itest pool " + uuid.NewString(), InsuranceFundName: "itest fund " + uuid.NewString()}
	f.deps = f.dependencies(killswitch.NewChecker(killswitch.Policy{}))
	svc, err := NewService(f.cfg, f.deps)
	require.NoError(t, err)
	f.svc, f.ledger = svc, f.deps.Ledger
	f.prices = f.deps.Prices.(*valuation.PriceStore)
	return f
}

func (f *fixture) dependencies(kills econ.KillSwitchReader) Dependencies {
	f.t.Helper()
	out := event.NewOutbox(f.clk)
	aw := audit.NewWriterWithBuildVersion("margin-itest")
	checker, err := gates.NewChecker("LOCAL", func(gates.Capability) bool { return true }, f.clk)
	require.NoError(f.t, err)
	router, err := legalrouter.New(legalrouter.ConservativePolicy())
	require.NoError(f.t, err)
	d := Dependencies{
		Clock: f.clk, Ledger: ledger.NewService(f.clk, "margin-itest"),
		Effects: econ.NewEffectStore(f.clk, out, aw), Intents: econ.NewIntentStore(f.clk, out, aw),
		Authorizer: econ.NewAuthorizer(checker, kills), Registry: econ.NewRegistry(), Risk: risk.NewStore(),
		Capital: capital.NewService(f.clk, capitalEmitter{outbox: out, clk: f.clk}), Prices: valuation.NewPriceStore(f.clk),
		Router: router, Identity: identity{verified: true}, KillSwitch: kills, Venue: f.venue, Audit: aw,
	}
	d.FeeLedger, err = feeledger.NewService(feeledger.Config{
		Clock: f.clk, Ledger: d.Ledger, Capital: d.Capital, Effects: d.Effects, Authorizer: d.Authorizer,
		Assets: assets.NewRepository(), Audit: aw,
	})
	require.NoError(f.t, err)
	return d
}

func (f *fixture) inTx(fn func(ctx context.Context, tx pgx.Tx) error) error {
	return testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, fn)
}

// simAsset creates a simulated test asset under a unique symbol. Tests never
// create the platform's sandbox dollar (econ owns the one SBX-USD); the
// margin service takes its numeraire as a parameter.
func (f *fixture) simAsset(symbol string, decimals uint8, rc assets.RiskClass) assets.AssetID {
	f.t.Helper()
	return f.createAsset(assets.Asset{Symbol: symbol + "-" + uuid.NewString()[:8], Decimals: decimals, RiskClass: rc})
}

// dollar creates a USD-pegged simulated test numeraire under a unique symbol.
func (f *fixture) dollar() assets.AssetID {
	f.t.Helper()
	return f.createAsset(assets.Asset{Symbol: "MUSD-" + uuid.NewString()[:8], Decimals: 6, RiskClass: assets.RiskSettlement,
		IsStablecoin: true, PegCurrency: "USD"})
}

func (f *fixture) createAsset(a assets.Asset) assets.AssetID {
	f.t.Helper()
	a.Chain, a.Kind, a.ValueDomain, a.Status = assets.SandboxChain, assets.KindSimulated, valuedomain.Simulated, assets.StatusActive
	a.Name = a.Symbol + " (margin test)"
	created, err := assets.NewRepository().Create(f.ctx, testDB, a)
	require.NoError(f.t, err)
	return created.ID
}

func (f *fixture) account() accounts.AccountID {
	f.t.Helper()
	repo := accounts.NewRepository()
	u, err := repo.CreateUser(f.ctx, testDB, "margin-itest", uuid.NewString(), nil)
	require.NoError(f.t, err)
	a, err := repo.CreateAccount(f.ctx, testDB, u.ID, accounts.KindCustomer)
	require.NoError(f.t, err)
	return a.ID
}

// fund credits a customer wallet with simulated value (SANDBOX_FUNDING).
func (f *fixture) fund(acct accounts.AccountID, asset assets.AssetID, qty money.Quantity) {
	f.t.Helper()
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := f.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindSandboxFunding, IdempotencyKey: "sbx-fund:" + ref,
			Reference: ledger.FinancialEventReference{Type: "sandbox_funding", ID: ref}, EffectiveAt: f.clk.Now(),
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(acct, ledger.CodeWallet, asset), Side: ledger.Debit, Quantity: qty},
				{Account: ledger.CustomerAccount(acct, ledger.CodeCapital, asset), Side: ledger.Credit, Quantity: qty},
			},
		})
		return err
	}))
}

// seed funds the fixture's lending pool and insurance fund.
func (f *fixture) seed(pool, fund money.Quantity) Facilities {
	f.t.Helper()
	var out Facilities
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = f.svc.SeedFacilities(ctx, tx, SeedRequest{Asset: f.usd, PoolAmount: pool, FundAmount: fund, Key: "seed-" + uuid.NewString(), Actor: f.actor})
		return err
	}))
	return out
}

// mark advances the clock one second and records an observation of the
// SOL price then (an observation's identity includes its time).
func (f *fixture) mark(price string) {
	f.t.Helper()
	f.clk.Advance(time.Second)
	p, err := money.PriceFromDecimalString(price, f.usd.String(), "itest", f.clk.Now())
	require.NoError(f.t, err)
	_, err = f.prices.RecordPrice(f.ctx, testDB, valuation.PriceObservation{AssetID: f.sol, QuoteAssetID: f.usd,
		Mantissa: p.Mantissa, Scale: p.Scale, Source: "itest", ObservedAt: f.clk.Now()})
	require.NoError(f.t, err)
}

func (f *fixture) open(acct accounts.AccountID, mode MarginMode) Account {
	f.t.Helper()
	var a Account
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		req := OpenRequest{AccountID: acct, MarginMode: mode, Mode: gates.ModePaper, NumeraireAssetID: f.usd,
			Actor: econ.Actor{Type: security.ActorUser, ID: "margin-itest-user"}}
		if mode == ModeIsolated {
			sol := f.sol
			req.IsolatedAssetID = &sol
		}
		a, err = f.svc.OpenAccount(ctx, tx, req)
		return err
	}))
	return a
}

func (f *fixture) post(acct accounts.AccountID, asset assets.AssetID, qty money.Quantity) CollateralResult {
	f.t.Helper()
	var r CollateralResult
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r, err = f.svc.PostCollateral(ctx, tx, CollateralRequest{AccountID: acct, AssetID: asset, Quantity: qty,
			IdempotencyKey: "post-" + uuid.NewString(), Requester: userRequester})
		return err
	}))
	return r
}

func (f *fixture) borrow(acct accounts.AccountID, amount money.Quantity, key string) (LoanResult, error) {
	var r LoanResult
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r, err = f.svc.Borrow(ctx, tx, LoanRequest{AccountID: acct, Amount: amount, IdempotencyKey: key, Requester: userRequester})
		return err
	})
	return r, err
}

func (f *fixture) evaluate(acct accounts.AccountID) EvaluateResult {
	f.t.Helper()
	var r EvaluateResult
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r, err = f.svc.Evaluate(ctx, tx, acct)
		return err
	}))
	return r
}

func (f *fixture) liquidate(acct accounts.AccountID, step int) (LiquidationResult, error) {
	var r LiquidationResult
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r, err = f.svc.Liquidate(ctx, tx, LiquidateRequest{AccountID: acct, ExpectedStep: step})
		return err
	})
	return r, err
}

func (f *fixture) balance(ref ledger.AccountRef) money.Quantity {
	f.t.Helper()
	b, err := f.ledger.Balance(f.ctx, testDB, ref)
	require.NoError(f.t, err)
	return b
}

func (f *fixture) customer(acct accounts.AccountID, code ledger.Code, asset assets.AssetID) money.Quantity {
	return f.balance(ledger.CustomerAccount(acct, code, asset))
}

// fixedAccountOnBooks builds, through the service, the fixed account of the
// unit tests: 1,000 paper dollars and 20 SOL at 100 in collateral against a
// 2,000 dollar loan.
func (f *fixture) fixedAccountOnBooks() (accounts.AccountID, Account) {
	f.t.Helper()
	acct := f.account()
	f.fund(acct, f.usd, usd(1_000))
	f.fund(acct, f.sol, sol(20))
	a := f.open(acct, ModeCross)
	f.mark("100")
	f.post(acct, f.usd, usd(1_000))
	f.post(acct, f.sol, sol(20))
	_, err := f.borrow(acct, usd(2_000), "fixed-borrow-"+uuid.NewString())
	require.NoError(f.t, err)
	return acct, a
}

func (f *fixture) countJournals(acct accounts.AccountID, kind ledger.Kind) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, testDB.QueryRow(f.ctx, `SELECT count(DISTINCT t.id) FROM journal_transactions t
		JOIN journal_entries e ON e.transaction_id = t.id JOIN ledger_accounts la ON la.id = e.ledger_account_id
		WHERE t.kind = $1 AND la.owner_type = 'CUSTOMER' AND la.owner_id = $2`, kind, acct).Scan(&n))
	return n
}
