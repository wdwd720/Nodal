//go:build integration

package feeledger

import (
	"context"
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
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
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
		fmt.Fprintln(os.Stderr, "feeledger integration: migrate up:", err)
		return 1
	}
	var err error
	if testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "feeledger-itest", MaxConns: 40}); err != nil {
		fmt.Fprintln(os.Stderr, "feeledger integration: open:", err)
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
	operator  = econ.Actor{Type: security.ActorOperator, ID: "operator-ada"}
	service   = econ.Actor{Type: security.ActorService, ID: "orders-service"}
	symbolSeq atomic.Int64
)

type fixture struct {
	t       *testing.T
	ctx     context.Context
	clk     *clock.Fake
	kills   *switchboard
	ledger  *ledger.Service
	capital *capital.Service
	effects *econ.EffectStore
	assets  *assets.Repository
	svc     *Service
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	requireEnv(t)
	clk := clock.NewFake(time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	out := event.NewOutbox(clk)
	aw := audit.NewWriterWithBuildVersion("feeledger-itest")
	checker, err := gates.NewChecker("TEST", func(gates.Capability) bool { return true }, clk)
	require.NoError(t, err)
	f := &fixture{
		t: t, ctx: context.Background(), clk: clk, kills: &switchboard{}, ledger: ledger.NewService(clk, "feeledger-itest"),
		capital: capital.NewService(clk, capital.OutboxEmitter{Outbox: out, Clock: clk}),
		effects: econ.NewEffectStore(clk, out, aw), assets: assets.NewRepository(),
	}
	f.svc, err = NewService(Config{
		Clock: clk, Ledger: f.ledger, Capital: f.capital, Effects: f.effects, Authorizer: econ.NewAuthorizer(checker, f.kills),
		Assets: f.assets, Audit: aw,
	})
	require.NoError(t, err)
	return f
}

func (f *fixture) inTx(fn func(ctx context.Context, tx pgx.Tx) error) error {
	return testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 5}, fn)
}

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

// simAsset is a fresh SIMULATED 6-decimal asset, so a test's balances are
// its own.
func (f *fixture) simAsset(prefix string) assets.Asset {
	f.t.Helper()
	sym := fmt.Sprintf("%s%d%s", prefix, symbolSeq.Add(1), strings.ToUpper(uuid.NewString()[:6]))
	a, err := f.assets.Create(f.ctx, testDB, assets.Asset{
		Chain: assets.SandboxChain, Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated, Symbol: sym,
		Name: sym + " (sandbox)", Decimals: 6, IsStablecoin: true, PegCurrency: "USD", RiskClass: assets.RiskSettlement,
		Status: assets.StatusActive,
	})
	require.NoError(f.t, err)
	return a
}

func units(n int64) money.Quantity { return money.QuantityFromInt64(n * 1_000_000) }

func (f *fixture) customer(a assets.AssetID, qty money.Quantity) accounts.AccountID {
	f.t.Helper()
	repo := accounts.NewRepository()
	u, err := repo.CreateUser(f.ctx, testDB, "feeledger-itest", uuid.NewString(), nil)
	require.NoError(f.t, err)
	acct, err := repo.CreateAccount(f.ctx, testDB, u.ID, accounts.KindCustomer)
	require.NoError(f.t, err)
	if qty.IsPositive() {
		f.fund(acct.ID, a, qty)
	}
	return acct.ID
}

func (f *fixture) fund(acct accounts.AccountID, a assets.AssetID, qty money.Quantity) {
	f.t.Helper()
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := f.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindSandboxFunding, IdempotencyKey: "sbx-fund:" + ref,
			Reference: ledger.FinancialEventReference{Type: "sandbox_funding", ID: ref}, EffectiveAt: f.clk.Now(),
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(acct, ledger.CodeWallet, a), Side: ledger.Debit, Quantity: qty},
				{Account: ledger.CustomerAccount(acct, ledger.CodeCapital, a), Side: ledger.Credit, Quantity: qty},
			},
		})
		return err
	}))
}

func (f *fixture) balance(ref ledger.AccountRef) money.Quantity {
	f.t.Helper()
	b, err := f.ledger.Balance(f.ctx, testDB, ref)
	require.NoError(f.t, err)
	return b
}

func (f *fixture) wallet(acct accounts.AccountID, a assets.AssetID) money.Quantity {
	return f.balance(ledger.CustomerAccount(acct, ledger.CodeWallet, a))
}

func (f *fixture) price(a assets.AssetID, s string) money.Price {
	f.t.Helper()
	p, err := money.PriceFromDecimalString(s, a.String(), "feeledger-itest", f.clk.Now())
	require.NoError(f.t, err)
	return p
}

// schedule is the full schedule of a quote asset: every component a
// schedule can carry, so every booking line is exercised.
func (f *fixture) schedule(quote assets.AssetID, version string) fees.Schedule {
	s := fees.ZeroSchedule(quote, 6)
	s.Version = version
	s.SpreadBPS = 20
	s.FixedFee = money.QuantityFromInt64(500_000)
	s.PercentageFeeBPS = 10
	s.VenuePassThroughBPS = 5
	s.NetworkFee = fees.NetworkFeeEstimate{Asset: quote, Amount: money.QuantityFromInt64(100_000)}
	s.StrategyFeeBPS = 15
	return s
}

func (f *fixture) publish(key string, s fees.Schedule, from time.Time) PublishedSchedule {
	f.t.Helper()
	p, err := do(f, func(ctx context.Context, tx pgx.Tx) (PublishedSchedule, error) {
		return f.svc.PublishSchedule(ctx, tx, PublishRequest{Key: key, Schedule: s, EffectiveFrom: from, Reason: "itest", Actor: operator})
	})
	require.NoError(f.t, err)
	return p
}

func (f *fixture) quote(key string, quote assets.AssetID, payer *accounts.AccountID, side fees.Side, qty money.Quantity, price string) StoredQuote {
	f.t.Helper()
	q, err := do(f, func(ctx context.Context, tx pgx.Tx) (StoredQuote, error) {
		return f.svc.RecordQuote(ctx, tx, QuoteRequest{
			ScheduleKey: key, Subsystem: econ.SubsystemOrders, IdempotencyKey: "quote-" + uuid.NewString(), AccountID: payer,
			Mode: gates.ModePaper, Actor: service,
			Input: fees.QuoteInput{Side: side, BaseQty: qty, BaseDecimals: 6, QuoteDecimals: 6, ReferencePrice: f.price(quote, price),
				StrategyOriginated: true},
		})
	})
	require.NoError(f.t, err)
	return q
}

func (f *fixture) book(q StoredQuote, payer accounts.AccountID) Booking {
	f.t.Helper()
	b, err := do(f, func(ctx context.Context, tx pgx.Tx) (Booking, error) {
		return f.svc.book(ctx, tx, bookRequest{DisclosureID: q.ID, AccountID: payer, Kind: ledger.KindSimTrade, Mode: gates.ModePaper,
			Actor: service})
	})
	require.NoError(f.t, err)
	return b
}

func usd(s string) money.USD {
	u, err := money.ParseUSD(s)
	if err != nil {
		panic(err)
	}
	return u
}
