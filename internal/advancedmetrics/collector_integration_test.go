//go:build integration

package advancedmetrics

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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/feeledger"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/margin"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/orders"
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
	var err error
	if testDB, err = db.Open(context.Background(), db.Config{URL: testAppURL, AppName: "advancedmetrics-itest", MaxConns: 20}); err != nil {
		fmt.Fprintln(os.Stderr, "advancedmetrics integration: open:", err)
		return 1
	}
	defer testDB.Close()
	return m.Run()
}

// fixture is the services the collector reads through, over the shared test
// database, with the instruments on a manual reader.
type fixture struct {
	t        *testing.T
	ctx      context.Context
	clk      *clock.Fake
	ledger   *ledger.Service
	capital  *capital.Service
	effects  *econ.EffectStore
	registry *econ.Registry
	prices   *valuation.PriceStore
	assets   *assets.Repository
	instr    *instruments.Repository
	recon    *reconciliation.Repository
	margin   *margin.Service
	orders   *orders.Service
	pipeline *orders.Pipeline
	chain    sandboxchain.Chain
	stable   *stableasset.Service
	usd      assets.Asset
	system   econ.Actor
	reader   *sdkmetric.ManualReader
	metrics  *observability.AdvancedMetrics
	extra    *Instruments
	// The collaborators the services above were built with, for the tests
	// that build more of them.
	outbox  *event.Outbox
	audit   *audit.PGWriter
	intents *econ.IntentStore
	authz   *econ.Authorizer
	kills   *killswitch.Checker
	router  *legalrouter.Router
	fees    *feeledger.Service
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if testDB == nil {
		t.Skip("set CP_TEST_DATABASE_URL and CP_TEST_MIGRATE_DATABASE_URL")
	}
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 7, 14, 0, 0, 0, time.UTC))
	out := event.NewOutbox(clk)
	aw := audit.NewWriterWithBuildVersion("advancedmetrics-itest")
	checker, err := gates.NewChecker(string(config.EnvLocal), func(gates.Capability) bool { return false }, clk)
	require.NoError(t, err)
	kills := killswitch.NewChecker(killswitch.Policy{})
	router, err := legalrouter.New(legalrouter.ConservativePolicy())
	require.NoError(t, err)
	f := &fixture{
		t: t, ctx: ctx, clk: clk, ledger: ledger.NewService(clk, "advancedmetrics-itest"),
		capital: capital.NewService(clk, capital.OutboxEmitter{Outbox: out, Clock: clk}), effects: econ.NewEffectStore(clk, out, aw),
		registry: econ.NewRegistry(), prices: valuation.NewPriceStore(clk), assets: assets.NewRepository(),
		instr: instruments.NewRepository(), recon: reconciliation.NewRepository(clk, out, aw), system: econ.SystemActor("advancedmetrics-itest"),
	}
	intents := econ.NewIntentStore(clk, out, aw)
	authz := econ.NewAuthorizer(checker, kills)
	f.outbox, f.audit, f.intents, f.authz, f.kills, f.router = out, aw, intents, authz, kills, router
	require.NoError(t, testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		f.usd, err = econ.EnsureSandboxDollar(ctx, tx)
		return err
	}))
	feeLedger, err := feeledger.NewService(feeledger.Config{
		Clock: clk, Ledger: f.ledger, Capital: f.capital, Effects: f.effects, Authorizer: authz, Assets: f.assets, Audit: aw,
	})
	require.NoError(t, err)
	f.fees = feeLedger
	f.margin, err = margin.NewService(margin.Config{
		Policy: margin.DefaultPolicy(), RiskPolicy: risk.DefaultAdvancedPolicy(),
		LendingPoolName: "Margin lending pool (sandbox)", InsuranceFundName: "Margin insurance fund (sandbox)",
	}, margin.Dependencies{
		Clock: clk, Ledger: f.ledger, Effects: f.effects, Intents: intents, Authorizer: authz, Registry: f.registry,
		Risk: risk.NewStore(), Capital: f.capital, Prices: f.prices, Router: router, Identity: compliance.NewAccountVerification(clk),
		KillSwitch: kills, Venue: margin.PaperVenue{}, Audit: aw, FeeLedger: feeLedger,
	})
	require.NoError(t, err)
	f.orders, err = orders.NewService(orders.Config{
		Clock: clk, Ledger: f.ledger, Capital: f.capital, Intents: intents, Effects: f.effects, Authorizer: authz,
		Registry: f.registry, Risk: risk.NewStore(), RiskPolicy: risk.DefaultAdvancedPolicy(), Router: router,
		Instruments: f.instr, Assets: f.assets, Prices: f.prices, KillSwitches: kills, FeeLedger: feeLedger,
	})
	require.NoError(t, err)
	f.pipeline, err = orders.NewPipeline(f.orders, testDB)
	require.NoError(t, err)
	var seed sandboxchain.Seed
	_, err = rand.Read(seed[:])
	require.NoError(t, err)
	f.chain, err = sandboxchain.Open(ctx, testDB, clk, config.EnvLocal, "amet-"+strings.ReplaceAll(uuid.NewString(), "-", "")[:12], seed)
	require.NoError(t, err)
	f.stable, err = stableasset.New(stableasset.Deps{
		DB: testDB, Clock: clk, Host: f.chain.Host, Intents: intents, Effects: f.effects, Authorizer: authz,
		Facilities: f.registry, Ledger: f.ledger, Capital: f.capital, Risk: risk.NewStore(), RiskPolicy: risk.DefaultAdvancedPolicy(),
		Router: router, Prices: f.prices, Kills: kills, Reconciliation: f.recon, Audit: aw, Accounts: accounts.NewRepository(),
		Assets: f.assets, Issuer: f.chain.Keys.Issuer, Custody: f.chain.Keys.Custody, Snapshots: f.chain.Keys.Snapshots,
	})
	require.NoError(t, err)
	f.reader = sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(f.reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	meter := mp.Meter("advancedmetrics-itest")
	f.metrics, err = observability.NewAdvancedMetrics(meter)
	require.NoError(t, err)
	f.extra, err = NewInstruments(meter)
	require.NoError(t, err)
	return f
}

func (f *fixture) collector() *Collector {
	f.t.Helper()
	c, err := NewCollector(Config{
		DB: testDB, Clock: f.clk, Metrics: f.metrics, Extra: f.extra, Assets: f.assets,
		Valuer: transfers.RegistryValuer{Prices: f.prices, Assets: f.assets, QuoteAssetID: econ.SandboxDollarID(), MaxMarkAge: DefaultMaxMarkAge},
		Margin: f.margin, Stable: f.stable, Network: f.chain.Network, Lag: 0, Horizon: time.Nanosecond,
	})
	require.NoError(f.t, err)
	return c
}

func (f *fixture) collect(c *Collector) Snapshot {
	f.t.Helper()
	s, rep, err := c.Collect(f.ctx)
	require.NoError(f.t, err)
	require.True(f.t, rep.OK(), "%+v", rep.Failures)
	return s
}

func (f *fixture) inTx(fn func(ctx context.Context, tx pgx.Tx) error) {
	f.t.Helper()
	require.NoError(f.t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, fn))
}

type customer struct {
	user    accounts.UserID
	account accounts.AccountID
	p       security.Principal
}

func (f *fixture) customer() customer {
	f.t.Helper()
	repo := accounts.NewRepository()
	email := sha256.Sum256([]byte(uuid.NewString() + "@sandbox.invalid"))
	u, err := repo.CreateUser(f.ctx, testDB, "advancedmetrics-itest", "advancedmetrics-"+uuid.NewString(), email[:])
	require.NoError(f.t, err)
	a, err := repo.CreateAccount(f.ctx, testDB, u.ID, accounts.KindCustomer)
	require.NoError(f.t, err)
	_, err = testDB.Pool().Exec(f.ctx, `INSERT INTO compliance_profiles (user_id, identity_state, verified_at) VALUES ($1, 'VERIFIED', now())`, u.ID)
	require.NoError(f.t, err)
	return customer{user: u.ID, account: a.ID, p: security.Principal{SubjectID: u.ID.String(), ActorType: security.ActorUser,
		Roles: []security.Role{security.RoleCustomer}, AccountIDs: []string{a.ID.String()}, AuthTime: f.clk.Now(), AMR: []string{"pwd", "mfa"}}}
}

func (f *fixture) post(kind ledger.Kind, entries ...ledger.Entry) {
	f.t.Helper()
	f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := f.ledger.Post(ctx, tx, ledger.Posting{Kind: kind, IdempotencyKey: "advancedmetrics:" + ref,
			Reference: ledger.FinancialEventReference{Type: "advancedmetrics_itest", ID: ref}, EffectiveAt: f.clk.Now(), Entries: entries})
		return err
	})
}

func (f *fixture) fund(acct accounts.AccountID, asset assets.AssetID, qty money.Quantity) {
	f.t.Helper()
	f.post(ledger.KindSandboxFunding,
		ledger.Entry{Account: ledger.CustomerAccount(acct, ledger.CodeWallet, asset), Side: ledger.Debit, Quantity: qty},
		ledger.Entry{Account: ledger.CustomerAccount(acct, ledger.CodeCapital, asset), Side: ledger.Credit, Quantity: qty})
}

func (f *fixture) simAsset(prefix string, decimals uint8) assets.Asset {
	f.t.Helper()
	sym := prefix + strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", "")[:6])
	a, err := f.assets.Create(f.ctx, testDB, assets.Asset{Chain: assets.SandboxChain, Kind: assets.KindSimulated,
		ValueDomain: valuedomain.Simulated, Symbol: sym, Name: sym + " (simulated)", Decimals: decimals,
		RiskClass: assets.RiskMajor, Status: assets.StatusActive})
	require.NoError(f.t, err)
	return a
}

func (f *fixture) mark(asset assets.AssetID, price string) {
	f.t.Helper()
	p, err := money.PriceFromDecimalString(price, f.usd.ID.String(), "advancedmetrics-itest", f.clk.Now())
	require.NoError(f.t, err)
	_, err = f.prices.RecordPrice(f.ctx, testDB, valuation.PriceObservation{AssetID: asset, QuoteAssetID: f.usd.ID,
		Mantissa: p.Mantissa, Scale: p.Scale, Source: "advancedmetrics-itest", ObservedAt: f.clk.Now()})
	require.NoError(f.t, err)
}

func units(whole int64, decimals uint8) money.Quantity {
	q := money.QuantityFromInt64(whole)
	for i := uint8(0); i < decimals; i++ {
		q = q.Mul(money.QuantityFromInt64(10))
	}
	return q
}

// leveraged opens a CROSS paper margin account with 1,000 sandbox dollars of
// collateral and 20 units of an asset marked at 100 bought on margin.
func (f *fixture) leveraged() (customer, assets.Asset) {
	f.t.Helper()
	sol := f.simAsset("AMSOL", 9)
	f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.margin.SeedFacilities(ctx, tx, margin.SeedRequest{Asset: f.usd.ID, PoolAmount: units(1_000_000, 6),
			FundAmount: units(100_000, 6), Key: "advancedmetrics-seed-" + uuid.NewString(), Actor: f.system})
		return err
	})
	c := f.customer()
	f.fund(c.account, f.usd.ID, units(1_000, 6))
	req := margin.Requester{Type: security.ActorUser, ID: c.user.String()}
	f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.margin.OpenAccount(ctx, tx, margin.OpenRequest{AccountID: c.account, MarginMode: margin.ModeCross,
			Mode: gates.ModePaper, NumeraireAssetID: f.usd.ID, Actor: econ.Actor{Type: security.ActorUser, ID: c.user.String()}})
		return err
	})
	f.mark(sol.ID, "100")
	f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := f.margin.PostCollateral(ctx, tx, margin.CollateralRequest{AccountID: c.account, AssetID: f.usd.ID,
			Quantity: units(1_000, 6), IdempotencyKey: "advancedmetrics-post-" + uuid.NewString(), Requester: req}); err != nil {
			return err
		}
		_, err := f.margin.OpenLeveragedSpot(ctx, tx, margin.SpotRequest{AccountID: c.account, AssetID: sol.ID,
			Quantity: units(20, sol.Decimals), IdempotencyKey: "advancedmetrics-buy-" + uuid.NewString(), Requester: req})
		return err
	})
	return c, sol
}

// gauge reads a gauge's last value for one attribute value ("" for none).
func (f *fixture) gauge(name, key, value string) (int64, bool) {
	f.t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(f.t, f.reader.Collect(f.ctx, &rm))
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[int64])
			require.True(f.t, ok, "%s is not an int64 gauge", name)
			for _, dp := range g.DataPoints {
				v, _ := dp.Attributes.Value(attribute.Key(key))
				if key == "" || v.AsString() == value {
					return dp.Value, true
				}
			}
		}
	}
	return 0, false
}

// counter sums a counter's data points for one attribute value ("" for all).
func (f *fixture) counter(name, key, value string) int64 {
	f.t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(f.t, f.reader.Collect(f.ctx, &rm))
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			s, ok := m.Data.(metricdata.Sum[int64])
			require.True(f.t, ok, "%s is not an int64 sum", name)
			for _, dp := range s.DataPoints {
				v, _ := dp.Attributes.Value(attribute.Key(key))
				if key == "" || v.AsString() == value {
					total += dp.Value
				}
			}
		}
	}
	return total
}

// TestIntegration_GaugesEqualTheRowDerivedTruth: the capital and
// reconciliation gauges move by exactly what the rows added between two
// passes say -- par for the sandbox dollar, the mark for a marked asset,
// nothing (and one unvalued asset) for an unmarked one, the house book apart
// from the customers' -- the margin gauges are the margin engine's own
// aggregate, the reserve coverage is the stable asset's reserve ledger, and
// what was recorded is what was read.
func TestIntegration_GaugesEqualTheRowDerivedTruth(t *testing.T) {
	f := newFixture(t)
	f.leveraged()
	sa, err := f.stable.Register(f.ctx, stableasset.Definition{
		Name: "amet-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8], Symbol: "AMPUSD" + strings.ToUpper(uuid.NewString()[:4]),
		PegCurrency: "USD", Decimals: 6, IssuanceCap: units(1_000_000, 6), RedemptionMode: stableasset.RedemptionInstant,
		PegQuoteAssetID: f.usd.ID, PegMantissa: money.QuantityFromInt64(1_000_000), PegScale: 6, OracleSource: "advancedmetrics-itest",
		MaxPriceAge: time.Hour, PegDeviationThreshold: 50, Attestable: true,
		Reserves: []stableasset.ReserveSpec{{AssetID: f.usd.ID, Kind: stableasset.ReserveCashEquivalent, FactorBPS: 10_000, AcceptedForMint: true}},
	}, econ.Actor{Type: security.ActorOperator, ID: uuid.NewString()})
	require.NoError(t, err)
	holder := f.customer()
	f.fund(holder.account, f.usd.ID, units(500, 6))
	minted, err := f.stable.Mint(security.WithPrincipal(f.ctx, holder.p), stableasset.MintRequest{StableAssetID: sa.ID,
		AccountID: holder.account, Quantity: units(200, 6), ReserveAssetID: f.usd.ID, IdempotencyKey: "advancedmetrics-mint-" + uuid.NewString(),
		Mode: gates.ModeSandbox, Actor: econ.Actor{Type: security.ActorUser, ID: holder.user.String()},
		Facts: stableasset.AccountFacts{Verification: valuedomain.VerificationNodalIdentity, IdentityVerified: true}})
	require.NoError(t, err)
	require.Equal(t, stableasset.MintIssued, minted.Request.Status)

	c := f.collector()
	s0 := f.collect(c)

	// What the rows add: 100 sandbox dollars (par) and 2 units marked at
	// 50.00 for two customers, 5 units of an unmarked asset for a third, 30
	// sandbox dollars in a house book, two open mismatches and one effect
	// whose outcome is unknown.
	a, b, u := f.customer(), f.customer(), f.customer()
	f.fund(a.account, f.usd.ID, units(100, 6))
	marked := f.simAsset("AMX", 6)
	f.mark(marked.ID, "50.00")
	f.fund(b.account, marked.ID, units(2, 6))
	unmarked := f.simAsset("AMY", 6)
	f.fund(u.account, unmarked.ID, units(5, 6))
	f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		h, err := f.registry.CreateHouseAccount(ctx, tx, "advancedmetrics house "+uuid.NewString()[:8], econ.HouseMarketMaking, f.system)
		if err != nil {
			return err
		}
		ref := uuid.NewString()
		_, err = f.ledger.Post(ctx, tx, ledger.Posting{Kind: ledger.KindHouseFunding, IdempotencyKey: "advancedmetrics-house:" + ref,
			Reference: ledger.FinancialEventReference{Type: "advancedmetrics_itest", ID: ref}, EffectiveAt: f.clk.Now(),
			Entries: []ledger.Entry{
				{Account: h.Account(ledger.CodeHouseInventory, f.usd.ID), Side: ledger.Debit, Quantity: units(30, 6)},
				{Account: h.Account(ledger.CodeHouseCapital, f.usd.ID), Side: ledger.Credit, Quantity: units(30, 6)},
			}})
		return err
	})
	for _, kind := range []reconciliation.Kind{reconciliation.KindStableReserve, reconciliation.KindBridge} {
		f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.recon.Open(ctx, tx, reconciliation.OpenRequest{Kind: kind, Mode: reconciliation.ModePeriodic,
				ScopeType: "advancedmetrics_itest", ScopeID: uuid.NewString(), Status: reconciliation.StatusMismatch, Material: true,
				Reason: "advancedmetrics integration test"})
			return err
		})
	}
	f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		e, _, err := f.effects.Begin(ctx, tx, econ.BeginRequest{Key: "advancedmetrics:" + uuid.NewString(), Subsystem: econ.SubsystemTransfers,
			Kind: "P2P_TRANSFER", AccountID: a.account.String(), Request: map[string]string{"test": "unknown"}, PostsLedger: true, Actor: f.system})
		if err != nil {
			return err
		}
		_, err = f.effects.MarkUnknown(ctx, tx, e.ID, "advancedmetrics-itest", f.system, "the outcome is unknown")
		return err
	})

	s1 := f.collect(c)
	assert.Equal(t, int64(200_00), s1.CustomerLiabilities-s0.CustomerLiabilities, "100 at par + 2 x 50.00 at the mark; the unmarked asset is not valued at zero")
	assert.Equal(t, int64(30_00), s1.HouseAssets-s0.HouseAssets)
	assert.Equal(t, int64(230_00), s1.GrossAssets-s0.GrossAssets, "every owner's holdings")
	assert.Equal(t, int64(1), s1.UnvaluedAssets-s0.UnvaluedAssets)
	assert.Equal(t, int64(1), s1.OpenRecords[string(reconciliation.KindStableReserve)]-s0.OpenRecords[string(reconciliation.KindStableReserve)])
	assert.Equal(t, int64(1), s1.OpenRecords[string(reconciliation.KindBridge)]-s0.OpenRecords[string(reconciliation.KindBridge)])
	assert.Equal(t, int64(1), s1.ChainMismatches-s0.ChainMismatches)
	assert.Equal(t, int64(1), s1.ReserveMismatches-s0.ReserveMismatches)
	assert.Equal(t, int64(1), s1.UnmatchedEffects[string(econ.SubsystemTransfers)]-s0.UnmatchedEffects[string(econ.SubsystemTransfers)])

	agg, err := f.margin.Aggregate(f.ctx, testDB, DefaultMarginScan)
	require.NoError(t, err)
	require.NotNil(t, s1.Margin)
	assert.Equal(t, agg.GrossExposure.Minor(), s1.Margin.GrossExposure, "the margin engine's own aggregate")
	assert.Equal(t, agg.LiquidationBuffer.Minor(), s1.Margin.LiquidationBuffer)
	assert.Positive(t, s1.Margin.GrossExposure)
	if bps, ok := agg.LeverageBPS(); ok {
		require.NotNil(t, s1.Margin.LeverageBPS)
		assert.Equal(t, int64(bps), *s1.Margin.LeverageBPS)
	}
	rl, err := f.stable.ReserveLedger(f.ctx, testDB, sa.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(rl.CoverageBPS), s1.ReserveCoverage[sa.Name])
	assert.Equal(t, int64(10_000), s1.ReserveCoverage[sa.Name], "fully reserved")

	for name, want := range map[string]int64{
		"advanced_gross_assets": s1.GrossAssets, "advanced_customer_liabilities": s1.CustomerLiabilities,
		"advanced_house_assets": s1.HouseAssets, "advanced_margin_exposure": s1.Margin.GrossExposure,
		"advanced_liquidation_buffer": s1.Margin.LiquidationBuffer, "advanced_chain_mismatches": s1.ChainMismatches,
		"advanced_reserve_mismatches": s1.ReserveMismatches, "advanced_unvalued_holdings": s1.UnvaluedAssets,
	} {
		got, ok := f.gauge(name, "", "")
		require.True(t, ok, "%s recorded", name)
		assert.Equal(t, want, got, "%s records what was read", name)
	}
	got, ok := f.gauge("advanced_reserve_coverage", "stable_asset", sa.Name)
	require.True(t, ok)
	assert.Equal(t, int64(10_000), got)
	got, ok = f.gauge("advanced_open_reconciliation_records", "kind", string(reconciliation.KindBridge))
	require.True(t, ok)
	assert.Equal(t, s1.OpenRecords[string(reconciliation.KindBridge)], got)
	got, ok = f.gauge("advanced_unmatched_economic_effects", "subsystem", string(econ.SubsystemTransfers))
	require.True(t, ok)
	assert.Equal(t, s1.UnmatchedEffects[string(econ.SubsystemTransfers)], got)
}

// seedSandboxRun writes a sandbox run row with the given termination, with
// an artifact of its own, its digests computed by the database the way the
// table's constraints check them, and charged -- as every sandbox run is
// (00757, SX002) -- to an account of its own: its wall time and the
// wall-time limit it was granted (none for a run the kill switch stopped),
// within a budget it states.
func (f *fixture) seedSandboxRun(termination string) {
	f.t.Helper()
	wasm := []byte("\x00asm\x01\x00\x00\x00" + uuid.NewString())
	runID := uuid.New()
	payer := f.customer().account
	f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO strategy_artifacts (id, artifact_digest, source_digest, wasm, wasm_size,
				toolchain_version, sdk_version, build_log_digest, scan_report, scan_codes, created_by_actor_type, created_by_actor_id)
			VALUES (gen_random_uuid(), sha256($1::bytea), sha256('source'::bytea), $1::bytea, length($1::bytea), 'go1.27.1',
				'nodalsdk/v1@sha256:' || repeat('a', 64), sha256('log'::bytea), '{}'::jsonb, '{}'::text[], 'SYSTEM', 'advancedmetrics-itest')`,
			wasm); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO sandbox_runs (id, artifact_digest, input_digest, limits_version, limits_hash, limits,
				termination_reason, termination_detail, exit_code, artifact_verified, wall_ms, output, output_bytes, stderr_bytes,
				stderr_digest, peak_memory_bytes, output_digest, run_digest, proposals_count, accepted_count, violations_count,
				manifest_hash, strategy_ref, actor_type, actor_id, started_at)
			VALUES ($3, sha256($1::bytea), sha256('input'::bytea), 'v1', repeat('b', 64), '{"wall_time_ms": 1000}'::jsonb,
				$2, '', 0, true, 1, ''::bytea, 0, 0, sha256(''::bytea), 0, sha256(''::bytea),
				sha256(sha256($1::bytea) || sha256('input'::bytea) || ''::bytea), 0, 0, 0,
				sha256('manifest'::bytea), 'advancedmetrics-itest', 'SYSTEM', 'advancedmetrics-itest', now())`,
			wasm, termination, runID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO sandbox_usage (id, run_id, account_id, purpose, cpu_ms, wall_ms, charged_at, budget_version,
				window_ms, max_runs, max_cpu_ms, max_wall_ms)
			SELECT gen_random_uuid(), r.id, $2, 'SIMULATION',
				CASE WHEN r.termination_reason = 'KILLED_BY_SWITCH' THEN 0 ELSE (r.limits->>'wall_time_ms')::bigint END,
				r.wall_ms, now(), 'advancedmetrics-itest', 3600000, 100, 3600000, 3600000
			FROM sandbox_runs r WHERE r.id = $1`, runID, payer)
		return err
	})
}

func (f *fixture) seedDecision(verdict string, codes ...string) {
	f.t.Helper()
	if codes == nil {
		codes = []string{}
	}
	f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO advanced_risk_decisions (id, intent_kind, stage, verdict, reason_codes, input_hash,
				policy_version, policy_hash, evaluator_version, decision_hash, input, decision, evaluated_at)
			VALUES (gen_random_uuid(), 'OPEN_POSITION', 'PRE_TRADE', $1, $2, 'x', 'v', 'h', 'e', 'd', '{}', '{}', now())`, verdict, codes)
		return err
	})
}

// TestIntegration_CountersCountEachNewRowOncePerProcess: the first pass
// counts no history; the next counts exactly the rows created since -- one
// increment per reason of a REJECT, the stale-oracle refusals, the margin
// call and liquidation steps of a falling mark, the sandbox runs and their
// terminations, an internal trade's quote notional at par -- and a third
// pass counts nothing again. A new collector (a restarted process) starts
// from a new baseline instead of re-counting.
func TestIntegration_CountersCountEachNewRowOncePerProcess(t *testing.T) {
	f := newFixture(t)
	borrower, sol := f.leveraged()
	c := f.collector()
	s0 := f.collect(c)
	assert.Nil(t, s0.Counted, "the first pass only sets the watermark")

	f.seedDecision("REJECT", risk.ReasonOracleStale, "RISK_VELOCITY")
	f.seedDecision("REJECT", "RISK_VELOCITY")
	f.seedDecision("ALLOW")
	f.seedSandboxRun("OK")
	f.seedSandboxRun("TIMEOUT")
	f.seedSandboxRun("MEMORY_LIMIT")
	f.clk.Advance(time.Minute)
	f.mark(sol.ID, "40")
	f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := f.margin.Evaluate(ctx, tx, borrower.account); err != nil {
			return err
		}
		_, err := f.margin.Liquidate(ctx, tx, margin.LiquidateRequest{AccountID: borrower.account})
		return err
	})
	steps := 0
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT count(*) FROM margin_liquidation_steps s JOIN margin_liquidation_cases c
		ON c.id = s.case_id JOIN margin_accounts m ON m.id = c.margin_account_id WHERE m.account_id = $1`, borrower.account).Scan(&steps))
	require.Positive(t, steps)
	notional := f.trade()
	// An intent proposed and refused at its first stage, stamped by a
	// service on the system clock (the fixture's services stamp theirs at
	// the fake clock's date, before every window this test counts).
	live := clock.System()
	liveIntents := econ.NewIntentStore(live, event.NewOutbox(live), audit.NewWriterWithBuildVersion("advancedmetrics-itest"))
	f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		in, _, err := liveIntents.Submit(ctx, tx, econ.Intent{AccountID: borrower.account, Kind: econ.KindTransferValue,
			Mode: gates.ModePaper, Capability: gates.P2PTransfers, ActorType: security.ActorUser, ActorID: borrower.user.String(),
			IdempotencyKey: "advancedmetrics-" + uuid.NewString()})
		if err != nil {
			return err
		}
		_, err = liveIntents.Transition(ctx, tx, in.ID, econ.StatusRejected, f.system, "advancedmetrics integration test", "ADVANCEDMETRICS_TEST")
		return err
	})

	s1 := f.collect(c)
	require.NotNil(t, s1.Counted)
	w := s1.Counted
	assert.Equal(t, map[string]int64{risk.ReasonOracleStale: 1, "RISK_VELOCITY": 2}, w.RiskDenies, "one increment per reason, REJECTs only")
	assert.Equal(t, int64(1), w.StaleOracleEvents)
	assert.Equal(t, int64(1), w.MarginCalls)
	assert.Equal(t, int64(steps), w.LiquidationEvents)
	assert.Equal(t, int64(3), w.SandboxRuns)
	assert.Equal(t, map[string]int64{"TIMEOUT": 1, "MEMORY_LIMIT": 1}, w.SandboxKilled)
	assert.Equal(t, int64(1), w.SandboxCPULimit)
	assert.Equal(t, int64(1), w.SandboxMemoryLimit)
	assert.Equal(t, notional, w.InternalMatchVolume, "the trade's quote notional at par")
	assert.Zero(t, w.UnvaluedMatchTrades)
	assert.Equal(t, map[string]int64{"USER": 1}, w.IntentProposals, "by actor type")
	assert.Equal(t, map[string]int64{"RECEIVED": 1}, w.DeniedIntents, "by the stage it was refused at")
	assert.Empty(t, w.SigningRequests, "no delegated signing in this window")
	assert.Empty(t, w.WithdrawalProposals)

	s2 := f.collect(c)
	require.NotNil(t, s2.Counted)
	assert.Empty(t, s2.Counted.RiskDenies, "nothing new: nothing counted again")
	assert.Zero(t, s2.Counted.SandboxRuns)
	assert.Zero(t, s2.Counted.InternalMatchVolume)

	assert.Equal(t, int64(2), f.counter("advanced_risk_denies", "reason", "RISK_VELOCITY"))
	assert.Equal(t, int64(1), f.counter("advanced_stale_oracle_events", "", ""))
	assert.Equal(t, int64(3), f.counter("advanced_sandbox_runs", "", ""))
	assert.Equal(t, int64(1), f.counter("advanced_sandbox_killed_runs", "reason", "TIMEOUT"))
	assert.Equal(t, notional, f.counter("advanced_internal_match_volume", "", ""))
	assert.Equal(t, int64(1), f.counter("advanced_margin_calls", "", ""))
	assert.Equal(t, int64(1), f.counter("advanced_intent_proposals", "actor_type", "USER"))
	assert.Equal(t, int64(1), f.counter("advanced_denied_intents", "stage", "RECEIVED"))
	assert.Empty(t, s2.Counted.IntentProposals, "counted once")

	restarted := f.collector()
	r0 := f.collect(restarted)
	assert.Nil(t, r0.Counted, "a restarted collector does not count history again")
}

// trade crosses two limit orders on a fresh internal book and returns the
// trade's quote notional in USD minor units (the quote asset is the sandbox
// dollar, valued at par).
func (f *fixture) trade() int64 {
	f.t.Helper()
	inst, eth := f.spotBook()
	maker, taker := f.customer(), f.customer()
	f.fund(maker.account, eth.ID, units(1, eth.Decimals))
	f.fund(taker.account, f.usd.ID, units(5_000, 6))
	_, err := f.pipeline.Submit(f.ctx, f.limit(maker, inst, eth, orders.SideSell, "2500.00", 1))
	require.NoError(f.t, err)
	out, err := f.pipeline.Submit(f.ctx, f.limit(taker, inst, eth, orders.SideBuy, "2500.00", 1))
	require.NoError(f.t, err)
	require.Len(f.t, out.Trades, 1)
	return 2500_00
}

// limit is a GTC limit order of whole units of base at a decimal price in
// the sandbox dollar.
func (f *fixture) limit(c customer, inst instruments.InstrumentID, base assets.Asset, side orders.Side, price string, whole int64) orders.OrderRequest {
	f.t.Helper()
	p, err := money.PriceFromDecimalString(price, f.usd.ID.String(), "order", f.clk.Now())
	require.NoError(f.t, err)
	return orders.OrderRequest{InstrumentID: inst, AccountID: c.account, Side: side, Type: orders.TypeLimit, TimeInForce: orders.GTC,
		Price: &p, Quantity: units(whole, base.Decimals), IdempotencyKey: "advancedmetrics-" + uuid.NewString(),
		ActorType: security.ActorUser, ActorID: c.user.String()}
}

// spotBook opens a fee-free PAPER book for a fresh simulated asset against
// the sandbox dollar, ticks of 0.01 and lots of 0.0001, the asset marked at
// 2500.00, and returns the instrument and the asset.
func (f *fixture) spotBook() (instruments.InstrumentID, assets.Asset) {
	f.t.Helper()
	eth := f.simAsset("AMETH", 8)
	var inst instruments.InstrumentID
	f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		ins, err := f.instr.CreateSpotPair(ctx, tx, instruments.SpotPairSpec{Base: eth.ID, Quote: f.usd.ID, Settlement: f.usd.ID,
			CanonicalName: eth.Symbol + "/SBX-USD", RiskClass: assets.RiskMajor, Status: assets.StatusActive, ActiveFrom: f.clk.Now().Add(-time.Hour)})
		if err != nil {
			return err
		}
		inst = ins.ID
		base, quote := eth.ID, f.usd.ID
		terms := instruments.Terms{Kind: instruments.TypeSpotPair, Symbol: eth.Symbol + "/SBX-USD", VenueCode: "NODAL-SIM",
			BaseAssetID: &base, QuoteAssetID: &quote, SettlementAssetID: quote, ContractMultiplier: money.QuantityFromInt64(1),
			PriceScale: 2, TickSize: money.QuantityFromInt64(1), LotSize: money.QuantityFromInt64(10_000), MinQuantity: money.QuantityFromInt64(10_000),
			QuantityDecimals: eth.Decimals, SettlementStyle: instruments.SettlePhysical, TradingCalendar: "24x7", OracleMaxAgeMS: 30_000,
			MarginModel: instruments.MarginNone, CorporateActionBehavior: instruments.CorporateActionsNone, Transferability: instruments.Transferable}
		zero := fees.ZeroSchedule(f.usd.ID, 6)
		zero.Version = "advancedmetrics-zero"
		_, err = f.orders.OpenBook(ctx, tx, orders.OpenBookRequest{InstrumentID: ins.ID, Mode: gates.ModePaper, SpotTerms: &terms,
			MakerSchedule: zero, TakerSchedule: zero, SnapshotInterval: 2, Actor: f.system})
		return err
	})
	f.mark(eth.ID, "2500.00")
	return inst, eth
}
