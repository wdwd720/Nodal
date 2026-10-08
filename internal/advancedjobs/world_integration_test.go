//go:build integration

package advancedjobs

import (
	"context"
	"crypto/rand"
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
	"github.com/nodal/controlplane/internal/bridge"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/derivatives"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/equities"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/feeledger"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/httpapi"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/margin"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/notification"
	"github.com/nodal/controlplane/internal/orders"
	"github.com/nodal/controlplane/internal/predmarket"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/sandboxchain"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/stableasset"
	"github.com/nodal/controlplane/internal/transfers"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

var (
	testAppURL     = os.Getenv("CP_TEST_DATABASE_URL")
	testMigrateURL = os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	testDB         *db.DB
)

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	if testAppURL == "" || testMigrateURL == "" {
		return m.Run()
	}
	ctx := context.Background()
	var err error
	if testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "advancedjobs-itest", MaxConns: 40}); err != nil {
		fmt.Fprintln(os.Stderr, "advancedjobs integration: open:", err)
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

// wednesday10NY is 10:00 New York time on Wednesday 2026-10-07: inside the
// regular equity session, and the start of every world's clock.
var wednesday10NY = time.Date(2026, 10, 7, 14, 0, 0, 0, time.UTC)

// world is one test's deployment of the advanced services: the same
// constructors and defaults cmd/advanced-worker uses, over the shared test
// database, with a clock the test moves. The environment is LOCAL, where
// the simulated modes run without a gate row and LIVE is refused as
// everywhere.
type world struct {
	t   *testing.T
	ctx context.Context
	clk *clock.Fake

	outbox   *event.Outbox
	audit    audit.Writer
	ledger   *ledger.Service
	capital  *capital.Service
	intents  *econ.IntentStore
	effects  *econ.EffectStore
	registry *econ.Registry
	authz    *econ.Authorizer
	kills    *killswitch.Checker
	router   *legalrouter.Router
	risk     *risk.Store
	prices   *valuation.PriceStore
	assets   *assets.Repository
	instr    *instruments.Repository
	recon    *reconciliation.Repository

	margin      *margin.Service
	derivatives *derivatives.Service
	orders      *orders.Service
	pipeline    *orders.Pipeline
	predictions *predmarket.Service
	oracle      *predmarket.SandboxOracle
	equities    *equities.Service
	transfers   *transfers.Service

	usd    assets.Asset
	system econ.Actor
}

func newWorld(t *testing.T) *world {
	requireEnv(t)
	ctx := context.Background()
	clk := clock.NewFake(wednesday10NY)
	out := event.NewOutbox(clk)
	aw := audit.NewWriterWithBuildVersion("advancedjobs-itest")
	checker, err := gates.NewChecker(string(config.EnvLocal), func(gates.Capability) bool { return false }, clk)
	require.NoError(t, err)
	kills := killswitch.NewChecker(killswitch.Policy{})
	router, err := legalrouter.New(legalrouter.ConservativePolicy())
	require.NoError(t, err)
	w := &world{
		t: t, ctx: ctx, clk: clk, outbox: out, audit: aw,
		ledger:  ledger.NewService(clk, "advancedjobs-itest"),
		capital: capital.NewService(clk, capital.OutboxEmitter{Outbox: out, Clock: clk}),
		intents: econ.NewIntentStore(clk, out, aw), effects: econ.NewEffectStore(clk, out, aw),
		registry: econ.NewRegistry(), authz: econ.NewAuthorizer(checker, kills), kills: kills, router: router,
		risk: risk.NewStore(), prices: valuation.NewPriceStore(clk), assets: assets.NewRepository(),
		instr: instruments.NewRepository(), recon: reconciliation.NewRepository(clk, out, aw),
		system: econ.SystemActor("advancedjobs-itest"),
	}
	require.NoError(t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		w.usd, err = econ.EnsureSandboxDollar(ctx, tx)
		return err
	}))

	// The fee ledger every charging service books through, as cmd/api builds it.
	feeLedger, err := feeledger.NewService(feeledger.Config{
		Clock: clk, Ledger: w.ledger, Capital: w.capital, Effects: w.effects, Authorizer: w.authz, Assets: w.assets, Audit: aw,
	})
	require.NoError(t, err)

	// The margin service exactly as cmd/api builds it.
	w.margin, err = margin.NewService(margin.Config{
		Policy: margin.DefaultPolicy(), RiskPolicy: risk.DefaultAdvancedPolicy(),
		LendingPoolName: "Margin lending pool (sandbox)", InsuranceFundName: "Margin insurance fund (sandbox)",
	}, margin.Dependencies{
		Clock: clk, Ledger: w.ledger, Effects: w.effects, Intents: w.intents, Authorizer: w.authz,
		Registry: w.registry, Risk: w.risk, Capital: w.capital, Prices: w.prices, Router: router,
		Identity: compliance.NewAccountVerification(clk), KillSwitch: kills, Venue: margin.PaperVenue{}, Audit: aw,
		FeeLedger: feeLedger,
	})
	require.NoError(t, err)

	// Derivatives as cmd/api builds them, except for a clearing house of the
	// test's own, so one test's flows never meet another's.
	w.derivatives, err = derivatives.NewService(derivatives.Config{
		Clock: clk, Intents: w.intents, Effects: w.effects, Authorizer: w.authz, Registry: w.registry, Ledger: w.ledger,
		Capital: w.capital, RiskStore: w.risk, RiskPolicy: risk.DefaultAdvancedPolicy(), Policy: derivatives.DefaultPolicy(),
		Prices: w.prices, Instruments: w.instr, Router: router, KillSwitches: kills, Reconciliation: w.recon,
		USDAssetID: econ.SandboxDollarID(), ClearingHouseName: "advancedjobs itest clearing " + uuid.NewString()[:8],
	})
	require.NoError(t, err)

	w.orders, err = orders.NewService(orders.Config{
		Clock: clk, Ledger: w.ledger, Capital: w.capital, Intents: w.intents, Effects: w.effects, Authorizer: w.authz,
		Registry: w.registry, Risk: w.risk, RiskPolicy: risk.DefaultAdvancedPolicy(), Router: router,
		Instruments: w.instr, Assets: w.assets, Prices: w.prices, KillSwitches: kills, FeeLedger: feeLedger,
	})
	require.NoError(t, err)
	w.pipeline, err = orders.NewPipeline(w.orders, testDB)
	require.NoError(t, err)

	seed := sha256.Sum256([]byte("advancedjobs-itest-oracle"))
	w.oracle, err = predmarket.NewSandboxOracle("advancedjobs-itest-oracle", seed[:])
	require.NoError(t, err)
	w.predictions, err = predmarket.NewService(predmarket.Config{
		Clock: clk, Intents: w.intents, Effects: w.effects, Authorizer: w.authz, Registry: w.registry,
		KillSwitches: kills, Ledger: w.ledger, Capital: w.capital, Risk: w.risk, RiskPolicy: risk.DefaultAdvancedPolicy(),
		Router: router, Instruments: w.instr, Assets: w.assets, Reconciliation: w.recon, Audit: aw,
		SandboxOracle: w.oracle, Books: w.orders,
	})
	require.NoError(t, err)

	w.equities, err = newEquities(clk, w, aw)
	require.NoError(t, err)

	screen, err := transfers.NewListScreen("advancedjobs-itest-screen-v1", nil)
	require.NoError(t, err)
	w.transfers, err = transfers.NewService(transfers.Deps{
		Clock: clk, Ledger: w.ledger, Capital: w.capital, Intents: w.intents, Effects: w.effects,
		Authorizer: w.authz, KillSwitches: kills, Router: router, Risk: w.risk,
		RiskPolicy: risk.DefaultAdvancedPolicy(), Policy: transfers.DefaultPolicy(),
		Valuer: transfers.RegistryValuer{Prices: w.prices, Assets: w.assets, QuoteAssetID: econ.SandboxDollarID(), MaxMarkAge: 5 * time.Minute},
		Screen: screen, Notifier: transfers.NewNotificationNotifier(notification.NewRepository()), Audit: aw,
	})
	require.NoError(t, err)
	return w
}

// newEquities builds the equities service the way the worker (and cmd/api)
// does: the paper venue NewExecutionAdapter gives LOCAL, on the XNYS calendar
// with the recorded closures, the sandbox schedule in the sandbox dollar, and
// the advanced capabilities' one identity rule.
func newEquities(clk clock.Clock, w *world, aw audit.Writer) (*equities.Service, error) {
	holidays, err := equities.Holidays(w.ctx, testDB, equities.CalendarXNYS)
	if err != nil {
		return nil, err
	}
	cal, err := equities.NewCalendar(equities.CalendarXNYS, holidays)
	if err != nil {
		return nil, err
	}
	venue, err := equities.NewExecutionAdapter(config.EnvLocal, config.ProviderModeFake, equities.PaperConfig{
		Name: "nodal-paper-equities", Calendar: cal, SlippageBPS: 10,
	})
	if err != nil {
		return nil, err
	}
	return newEquitiesOver(clk, w, aw, venue)
}

// newEquitiesOver is newEquities on a venue of the test's choosing.
func newEquitiesOver(clk clock.Clock, w *world, aw audit.Writer, venue equities.EquitiesExecutionAdapter) (*equities.Service, error) {
	return equities.NewService(equities.Config{
		Clock: clk, Intents: w.intents, Effects: w.effects, Authorizer: w.authz, KillSwitches: w.kills, Router: w.router,
		RiskPolicy: risk.DefaultAdvancedPolicy(), Capital: w.capital, Ledger: w.ledger, Prices: w.prices, Audit: aw,
		Adapter: venue, Schedules: []fees.Schedule{{
			Version: "equities-sandbox-v1", QuoteAsset: econ.SandboxDollarID(), QuoteDecimals: econ.SandboxDollarDecimals,
			SpreadBPS: 10, PercentageFeeBPS: 10, StrategyFeeBPS: 5, SlippageReserveBPS: 50, Rounding: fees.DefaultRounding(),
		}},
		MarketCollarBPS: 100, MinOrderNotional: money.USDFromMinor(1_00),
		Verification: httpapi.AdvancedVerificationLevels{Rule: compliance.NewAccountVerification(clk)},
	})
}

func (w *world) inTx(fn func(ctx context.Context, tx pgx.Tx) error) error {
	return testDB.InTx(w.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, fn)
}

// customer is a user with one ACTIVE customer account, a verified email
// address and a VERIFIED compliance profile, and the principal it acts as.
type customer struct {
	user    accounts.UserID
	account accounts.AccountID
	p       security.Principal
}

func (w *world) customer(label string) customer {
	w.t.Helper()
	repo := accounts.NewRepository()
	email := sha256.Sum256([]byte("advancedjobs-" + label + "-" + uuid.NewString() + "@sandbox.invalid"))
	u, err := repo.CreateUser(w.ctx, testDB, "advancedjobs-itest", "advancedjobs-"+label+"-"+uuid.NewString(), email[:])
	require.NoError(w.t, err)
	a, err := repo.CreateAccount(w.ctx, testDB, u.ID, accounts.KindCustomer)
	require.NoError(w.t, err)
	_, err = testDB.Pool().Exec(w.ctx, `INSERT INTO compliance_profiles (user_id, identity_state, verified_at) VALUES ($1, 'VERIFIED', now())`, u.ID)
	require.NoError(w.t, err)
	return customer{user: u.ID, account: a.ID, p: security.Principal{
		SubjectID: u.ID.String(), ActorType: security.ActorUser, Roles: []security.Role{security.RoleCustomer},
		AccountIDs: []string{a.ID.String()}, AuthTime: w.clk.Now(), AMR: []string{"pwd", "mfa"},
	}}
}

// operator is a person with an operator role, identified by a user id.
func (w *world) operator(label string) econ.Actor {
	w.t.Helper()
	email := sha256.Sum256([]byte("advancedjobs-operator-" + label + "@sandbox.invalid"))
	u, err := accounts.NewRepository().CreateUser(w.ctx, testDB, "advancedjobs-itest", "advancedjobs-operator-"+label+"-"+uuid.NewString(), email[:])
	require.NoError(w.t, err)
	return econ.Actor{Type: security.ActorOperator, ID: u.ID.String()}
}

// fund credits sandbox value to a customer's WALLET (SANDBOX_FUNDING).
func (w *world) fund(acct accounts.AccountID, asset assets.AssetID, qty money.Quantity) {
	w.t.Helper()
	require.NoError(w.t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := w.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindSandboxFunding, IdempotencyKey: "advancedjobs-fund:" + ref,
			Reference: ledger.FinancialEventReference{Type: "sandbox_funding", ID: ref}, EffectiveAt: w.clk.Now(),
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(acct, ledger.CodeWallet, asset), Side: ledger.Debit, Quantity: qty},
				{Account: ledger.CustomerAccount(acct, ledger.CodeCapital, asset), Side: ledger.Credit, Quantity: qty},
			},
		})
		return err
	}))
}

// simAsset registers a SIMULATED asset on the sandbox chain with a unique
// symbol.
func (w *world) simAsset(prefix string, decimals uint8, rc assets.RiskClass) assets.Asset {
	w.t.Helper()
	sym := prefix + strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", "")[:6])
	a, err := w.assets.Create(w.ctx, testDB, assets.Asset{
		Chain: assets.SandboxChain, Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated,
		Symbol: sym, Name: sym + " (simulated)", Decimals: decimals, RiskClass: rc, Status: assets.StatusActive,
	})
	require.NoError(w.t, err)
	return a
}

// mark records a price for asset in the sandbox dollar at the world's time.
func (w *world) mark(asset assets.AssetID, price string) {
	w.t.Helper()
	p, err := money.PriceFromDecimalString(price, w.usd.ID.String(), "advancedjobs-itest", w.clk.Now())
	require.NoError(w.t, err)
	_, err = w.prices.RecordPrice(w.ctx, testDB, valuation.PriceObservation{
		AssetID: asset, QuoteAssetID: w.usd.ID, Mantissa: p.Mantissa, Scale: p.Scale,
		Source: "advancedjobs-itest", ObservedAt: w.clk.Now(),
	})
	require.NoError(w.t, err)
}

// units is whole units of an asset with the given decimals, in base units.
func units(whole int64, decimals uint8) money.Quantity {
	q := money.QuantityFromInt64(whole)
	for i := uint8(0); i < decimals; i++ {
		q = q.Mul(money.QuantityFromInt64(10))
	}
	return q
}

func (w *world) usdUnits(n int64) money.Quantity { return units(n, w.usd.Decimals) }

func (w *world) balance(ref ledger.AccountRef) money.Quantity {
	w.t.Helper()
	b, err := w.ledger.Balance(w.ctx, testDB, ref)
	require.NoError(w.t, err)
	return b
}

func (w *world) count(sql string, args ...any) int {
	w.t.Helper()
	var n int
	require.NoError(w.t, testDB.QueryRow(w.ctx, sql, args...).Scan(&n))
	return n
}

// chain opens a devnet network of the test's own through sandboxchain, the
// way the worker does, with a fresh random seed.
func (w *world) chain() sandboxchain.Chain {
	w.t.Helper()
	c, _ := w.seededChain()
	return c
}

// seededChain is chain with the seed it was opened with, from which the
// delegated signer and the venue co-signers derive too.
func (w *world) seededChain() (sandboxchain.Chain, sandboxchain.Seed) {
	w.t.Helper()
	var seed sandboxchain.Seed
	_, err := rand.Read(seed[:])
	require.NoError(w.t, err)
	network := "ajobs-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	c, err := sandboxchain.Open(w.ctx, testDB, w.clk, config.EnvLocal, network, seed)
	require.NoError(w.t, err)
	return c, seed
}

// stable builds the stable asset controller over a chain, as the worker does.
func (w *world) stable(c sandboxchain.Chain) *stableasset.Service {
	w.t.Helper()
	svc, err := stableasset.New(stableasset.Deps{
		DB: testDB, Clock: w.clk, Host: c.Host, Intents: w.intents, Effects: w.effects, Authorizer: w.authz,
		Facilities: w.registry, Ledger: w.ledger, Capital: w.capital, Risk: w.risk, RiskPolicy: risk.DefaultAdvancedPolicy(),
		Router: w.router, Prices: w.prices, Kills: w.kills, Reconciliation: w.recon, Audit: w.audit,
		Accounts: accounts.NewRepository(), Assets: w.assets, Issuer: c.Keys.Issuer, Custody: c.Keys.Custody,
		Snapshots: c.Keys.Snapshots,
	})
	require.NoError(w.t, err)
	return svc
}

// bridge builds the bridge over a chain, as the worker does.
func (w *world) bridge(c sandboxchain.Chain) *bridge.Service {
	w.t.Helper()
	svc, err := bridge.New(bridge.Deps{
		DB: testDB, Clock: w.clk, Host: c.Host, Intents: w.intents, Effects: w.effects, Authorizer: w.authz,
		Facilities: w.registry, Ledger: w.ledger, Capital: w.capital, Risk: w.risk, RiskPolicy: risk.DefaultAdvancedPolicy(),
		Router: w.router, Kills: w.kills, Reconciliation: w.recon, Audit: w.audit, Accounts: accounts.NewRepository(),
		Assets: w.assets, Holder: c.Keys.Custody, Attester: c.Keys.Attester,
	})
	require.NoError(w.t, err)
	return svc
}

// run runs a job once and fails the test on a run error.
func run(t *testing.T, ctx context.Context, j Job) Report {
	t.Helper()
	rep, err := j.Run(ctx)
	require.NoError(t, err, "%s", j.Spec().Name)
	return rep
}
