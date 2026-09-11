//go:build integration

package demo_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/activity"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/demo"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/nativeasset"
	"github.com/nodal/controlplane/internal/nativemarket"
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := migrate.Up(ctx, testMigrateURL); err != nil {
		fmt.Fprintln(os.Stderr, "demo integration: migrate up:", err)
		return 1
	}
	var err error
	if testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "demo-itest", MaxConns: 10}); err != nil {
		fmt.Fprintln(os.Stderr, "demo integration: open pool:", err)
		return 1
	}
	defer testDB.Close()
	return m.Run()
}

func requireEnv(t *testing.T) {
	t.Helper()
	if testDB == nil {
		t.Skip("CP_TEST_DATABASE_URL / CP_TEST_MIGRATE_DATABASE_URL not set")
	}
}

// activeCaps lets the ledger commit the cross-domain postings a native trade
// needs. On a real sandbox tier this comes from the gate checker; the gate
// ceremony is not what this test is about.
type activeCaps map[valuedomain.CapabilityKey]bool

func (c activeCaps) ActiveConversionCapabilities(context.Context, db.Querier) (map[valuedomain.CapabilityKey]bool, error) {
	return c, nil
}

type harness struct {
	deps demo.Deps
	clk  *clock.Fake
}

func newHarness(t *testing.T) harness {
	t.Helper()
	requireEnv(t)
	clk := clock.NewFake(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))

	led := ledger.NewService(clk, "demo-itest")
	led.SetCapabilityResolver(activeCaps{valuedomain.CapNativeMarketTrading: true})
	credits := credit.NewService(led, clk)
	markets := nativemarket.NewService(led, credits, valuation.NewPriceStore(clk), audit.NewWriter(),
		instruments.NewRepository(), nativemarket.NewRiskGate(risk.NewStore(), clk), clk)
	markets.SetSafety(nativemarket.NewSafetyStore())

	seedRiskPolicy(t, clk.Now())

	return harness{
		clk: clk,
		deps: demo.Deps{
			DB:            testDB,
			Assets:        nativeasset.NewService(clk, nil),
			Markets:       markets,
			Credits:       credits,
			Clock:         clk,
			CreditAssetID: creditAsset(t),
			Environment:   "STAGING",
			SandboxTier:   true,
		},
	}
}

func creditAsset(t *testing.T) assets.AssetID {
	t.Helper()
	ctx := context.Background()
	var existing assets.AssetID
	if err := testDB.QueryRow(ctx, `SELECT id FROM assets WHERE kind = 'CREDIT'`).Scan(&existing); err == nil {
		return existing
	}
	created, err := assets.NewRepository().Create(ctx, testDB, assets.Asset{
		Chain: assets.InternalChain, Kind: assets.KindCredit,
		ValueDomain: valuedomain.InternalCredit,
		Symbol:      "CREDIT", Name: "Nodal Credit", Decimals: 6,
		RiskClass: assets.RiskUnsupported, Status: assets.StatusActive,
	})
	require.NoError(t, err)
	return created.ID
}

// seedRiskPolicy records the compiled-in GLOBAL policy, which is what
// riskPolicyAtBoot does on a non-production deployment. Without one the risk
// kernel refuses every internal trade, which is the correct failure and not the
// one this test is about.
func seedRiskPolicy(t *testing.T, at time.Time) {
	t.Helper()
	ctx := security.WithPrincipal(context.Background(), security.Principal{
		SubjectID: "demo-itest", ActorType: security.ActorSystem, AuthTime: at,
	})
	store := risk.NewStore()
	if _, _, err := store.EffectivePolicy(ctx, testDB, "", "", at); err == nil {
		return
	}
	require.NoError(t, testDB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := store.RecordPolicy(ctx, tx, risk.PolicyRecord{
				Scope: risk.ScopeGlobal, Version: "demo-itest-global",
				Rules:       []byte(risk.DefaultGlobalPolicyJSON),
				EffectiveAt: at.Add(-time.Hour),
				ActorType:   security.ActorSystem, ActorID: "demo-itest",
				Reason: "the compiled-in default limits, as a fresh deployment gets them",
			})
			return err
		}))
}

// TestIntegration_TheSeederRefusesWhereItMustRefuse.
func TestIntegration_TheSeederRefusesWhereItMustRefuse(t *testing.T) {
	h := newHarness(t)

	prod := h.deps
	prod.Environment = "PROD"
	_, err := demo.NewSeeder(prod)
	require.Error(t, err, "demo data must never exist in PROD")
	assert.Contains(t, err.Error(), "PROD")

	// Even with SandboxTier somehow true, PROD is refused.
	prod.SandboxTier = true
	_, err = demo.NewSeeder(prod)
	require.Error(t, err)

	notSandbox := h.deps
	notSandbox.SandboxTier = false
	_, err = demo.NewSeeder(notSandbox)
	require.Error(t, err, "a deployment that is not a sandbox tier has no place to put simulated data")

	nameless := h.deps
	nameless.Environment = ""
	_, err = demo.NewSeeder(nameless)
	require.Error(t, err, "a seeder that does not know where it is refuses to run")

	noCredit := h.deps
	noCredit.CreditAssetID = assets.AssetID{}
	_, err = demo.NewSeeder(noCredit)
	require.Error(t, err, "a market priced in nothing is not a market")

	// And the database refuses the evidence of a PROD run whatever Go did.
	_, err = testDB.Exec(context.Background(),
		`INSERT INTO demo_seed_rows (seed_key, kind, ref_id, environment)
		 VALUES ('x', 'ACCOUNT', gen_random_uuid(), 'PROD')`)
	require.Error(t, err, "migration 00774 must refuse a PROD demo row")
}

// TestIntegration_SeedingIsIdempotentAndEverythingItMakesIsLabelled is the
// property §51 asks for: staging data that is deterministic, distinguishable
// from real data, and created through the domain rather than around it.
func TestIntegration_SeedingIsIdempotentAndEverythingItMakesIsLabelled(t *testing.T) {
	h := newHarness(t)
	seeder, err := demo.NewSeeder(h.deps)
	require.NoError(t, err)
	ctx := context.Background()

	first, err := seeder.Seed(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, first.Created)
	require.Len(t, first.Markets, len(demo.Specs()))

	before := countDemoRows(t)
	assert.Positive(t, before)

	// Twice.
	second, err := seeder.Seed(ctx)
	require.NoError(t, err)
	assert.Empty(t, second.Created, "a second run must create nothing")
	assert.Equal(t, len(first.Created), len(second.Existing),
		"every object the first run created must be found by the second")
	assert.Equal(t, first.Markets, second.Markets, "the same markets, in the same order")
	assert.Equal(t, before, countDemoRows(t), "a second run must not add a row")

	// A third seeder, freshly constructed, finds the same objects: the
	// idempotence is in the register, not in the process.
	again, err := demo.NewSeeder(h.deps)
	require.NoError(t, err)
	third, err := again.Seed(ctx)
	require.NoError(t, err)
	assert.Empty(t, third.Created)

	// Every market it made is labelled, named and described as demo data, and
	// carries the environment it was seeded in.
	rows, err := testDB.Query(ctx,
		`SELECT na.name, na.symbol, na.description, ds.environment
		   FROM demo_seed_rows ds
		   JOIN native_markets m ON m.id = ds.ref_id
		   JOIN native_assets na ON na.asset_id = m.asset_id
		  WHERE ds.kind = 'NATIVE_MARKET'`)
	require.NoError(t, err)
	defer rows.Close()
	labelled := 0
	for rows.Next() {
		var name, symbol, description, env string
		require.NoError(t, rows.Scan(&name, &symbol, &description, &env))
		assert.Contains(t, name, demo.Prefix, "a demo asset must say so in its name")
		assert.Contains(t, symbol, demo.Prefix)
		assert.Contains(t, description, "Demo data", "a demo asset must say so in its description")
		assert.Equal(t, "STAGING", env)
		labelled++
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, len(demo.Specs()), labelled)

	// The markets are live, priced, and one of them deliberately has no trades
	// so an empty chart has a subject.
	page, err := h.deps.Markets.ListMarkets(ctx, testDB, nativemarket.ListRequest{
		Query: demo.Prefix, Limit: 50,
	})
	require.NoError(t, err)
	require.Len(t, page.Markets, len(demo.Specs()), "search must find every demo market")
	traded, untraded := 0, 0
	for _, m := range page.Markets {
		assert.True(t, m.Demo, "a demo market must be marked so the UI can label it")
		assert.Equal(t, nativemarket.StatusActive, m.MarketStatus)
		assert.True(t, m.LastPrice.IsPositive(), "%s has no price", m.Symbol)
		if m.Trades24h > 0 {
			traded++
			assert.True(t, m.HasChange24h)
			assert.True(t, m.CreditVolume24h.IsPositive())
		} else {
			untraded++
			assert.False(t, m.HasChange24h, "a market with no trades has no 24h change, not a change of zero")
		}
	}
	assert.Positive(t, traded, "demo markets must have price history")
	assert.Equal(t, 1, untraded, "exactly one demo market is deliberately untraded")

	// The trades produced real prints, so a chart has something to draw.
	prints, err := h.deps.Markets.RecentPrints(ctx, testDB, page.Markets[0].MarketID, 10)
	require.NoError(t, err)
	assert.NotNil(t, prints)
}

// TestIntegration_DemoCreditsCanBeSpentAndCanNeverLeave: the seeder's Credits
// are PROMOTIONAL, which every payout policy in this build refuses -- including
// the sandbox one, which is the whole point of demo money.
func TestIntegration_DemoCreditsCanBeSpentAndCanNeverLeave(t *testing.T) {
	h := newHarness(t)
	seeder, err := demo.NewSeeder(h.deps)
	require.NoError(t, err)
	ctx := context.Background()
	_, err = seeder.Seed(ctx)
	require.NoError(t, err)

	trader := demoAccount(t, "trader-a")

	var origins []string
	rows, err := testDB.Query(ctx, `SELECT DISTINCT origin FROM credit_lots WHERE account_id = $1`, trader)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var o string
		require.NoError(t, rows.Scan(&o))
		origins = append(origins, o)
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, origins)
	for _, o := range origins {
		origin := valuedomain.CreditOrigin(o)
		// A demo trader holds what the seeder granted and whatever a sale gave
		// back. Neither may ever be withdrawn.
		assert.Contains(t, []valuedomain.CreditOrigin{
			valuedomain.OriginPromotional, valuedomain.OriginMarketTradingProceeds,
		}, origin, "the seeder minted an origin it should not have")
		assert.False(t, valuedomain.SandboxPolicy().Rule(origin).PayoutAllowed,
			"%s must not be withdrawable even on a sandbox tier", origin)
		assert.False(t, valuedomain.DefaultPolicy().Rule(origin).PayoutAllowed,
			"%s must not be withdrawable under the fail-closed default", origin)
	}

	// And they bought something: the demo trader holds a position with a real
	// cost basis, and it reconciles against the ledger.
	positions, err := h.deps.Markets.PortfolioPositions(ctx, testDB, trader)
	require.NoError(t, err)
	require.NotEmpty(t, positions, "a demo trader must actually hold something")
	for _, p := range positions {
		assert.True(t, p.Position.HoldsInvariant())
		assert.True(t, p.Position.CostBasisCredits.IsPositive())
		assert.True(t, p.Demo, "a position in a demo market is simulated")
	}
	bad, err := h.deps.Markets.VerifyPositions(ctx, testDB)
	require.NoError(t, err)
	assert.Empty(t, bad)
}

// TestIntegration_TheActivityFeedSeesWhatTheSeederDid drives internal/activity
// over data no test wrote by hand.
func TestIntegration_TheActivityFeedSeesWhatTheSeederDid(t *testing.T) {
	h := newHarness(t)
	seeder, err := demo.NewSeeder(h.deps)
	require.NoError(t, err)
	ctx := context.Background()
	_, err = seeder.Seed(ctx)
	require.NoError(t, err)

	trader := demoAccount(t, "trader-a")
	creator := demoAccount(t, "creator")

	// A sandbox tier: every amount is SIMULATED and says so.
	feed := activity.NewFeed(true)
	page, err := feed.Activity(ctx, testDB, activity.Request{AccountID: trader, Limit: 50})
	require.NoError(t, err)
	require.NotEmpty(t, page.Items)

	var trades int
	for i, it := range page.Items {
		assert.NotEmpty(t, it.Summary, "every item carries a sentence")
		assert.NotEmpty(t, it.Reference.Type)
		assert.NotEmpty(t, it.Reference.ID)
		assert.True(t, it.Simulated)
		for _, a := range it.Amounts {
			assert.Equal(t, activity.TemperatureSimulated, a.Temperature,
				"nothing on a sandbox tier may be reported as real or as economy value")
			assert.NotEmpty(t, a.Value)
		}
		if i > 0 {
			assert.False(t, page.Items[i-1].OccurredAt.Before(it.OccurredAt),
				"the feed must be newest first")
		}
		if it.Kind == activity.KindNativeTrade {
			trades++
			assert.Contains(t, it.Summary, "internal market")
			assert.Equal(t, "native_market_fill", it.Reference.Type)
		}
	}
	assert.Positive(t, trades, "the demo trader's trades must be in their timeline")

	// The kind filter narrows, and an unknown kind is an error rather than an
	// empty page a user would read as "nothing happened".
	only, err := feed.Activity(ctx, testDB, activity.Request{
		AccountID: trader, Kinds: []activity.Kind{activity.KindNativeTrade}, Limit: 50,
	})
	require.NoError(t, err)
	require.NotEmpty(t, only.Items)
	for _, it := range only.Items {
		assert.Equal(t, activity.KindNativeTrade, it.Kind)
	}
	assert.Equal(t, trades, len(only.Items))

	_, err = feed.Activity(ctx, testDB, activity.Request{
		AccountID: trader, Kinds: []activity.Kind{"NOT_A_KIND"},
	})
	require.Error(t, err)

	// The creator's timeline carries the assets they created, and nobody
	// else's trades.
	creatorPage, err := feed.Activity(ctx, testDB, activity.Request{
		AccountID: creator, Kinds: []activity.Kind{activity.KindNativeAssetCreated}, Limit: 50,
	})
	require.NoError(t, err)
	assert.Len(t, creatorPage.Items, len(demo.Specs()))
	for _, it := range creatorPage.Items {
		assert.Contains(t, it.Summary, "Created the native asset")
		assert.True(t, it.Simulated)
	}

	// Paging sees every item exactly once and terminates.
	seen := map[string]int{}
	cursor := ""
	for pages := 0; ; pages++ {
		require.Less(t, pages, 200, "paging did not terminate")
		p, perr := feed.Activity(ctx, testDB, activity.Request{AccountID: trader, Limit: 2, Cursor: cursor})
		require.NoError(t, perr)
		for _, it := range p.Items {
			seen[it.Kind.String()+":"+it.ID]++
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	assert.Equal(t, len(page.Items), len(seen))
	for k, n := range seen {
		assert.Equal(t, 1, n, "%s appeared %d times while paging", k, n)
	}

	// A live deployment reports the same items without the sandbox stamp --
	// except the demo objects, which stamp themselves.
	live := activity.NewFeed(false)
	livePage, err := live.Activity(ctx, testDB, activity.Request{
		AccountID: trader, Kinds: []activity.Kind{activity.KindNativeTrade}, Limit: 50,
	})
	require.NoError(t, err)
	require.NotEmpty(t, livePage.Items)
	for _, it := range livePage.Items {
		assert.True(t, it.Simulated, "a trade on a demo market is simulated whatever the deployment is")
	}
}

func demoAccount(t *testing.T, name string) accounts.AccountID {
	t.Helper()
	var id accounts.AccountID
	require.NoError(t, testDB.QueryRow(context.Background(),
		`SELECT ref_id FROM demo_seed_rows WHERE seed_key = $1`, "demo:account:"+name).Scan(&id))
	return id
}

func countDemoRows(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, testDB.QueryRow(context.Background(), `SELECT count(*) FROM demo_seed_rows`).Scan(&n))
	return n
}
