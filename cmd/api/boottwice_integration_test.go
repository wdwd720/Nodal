//go:build integration

package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
)

// Booting twice (D-086).
//
// Three things happen at boot on a sandbox tier and every one of them writes:
// the Credit asset (creditAssetAtBoot), the conservative GLOBAL risk policy
// (riskPolicyAtBoot, D-067) and the demo catalogue (demoDataAtBoot, D-068).
// Each claims to be idempotent, and each claims it for a different reason -- a
// lookup first, a unique version derived from a content hash, and a register of
// seed keys.
//
// A free instance redeploys and sleeps, so "boots twice" is the normal case and
// not an edge one. This boots the whole composition root twice against one
// database and counts rows, which is the only way those three claims are one
// claim rather than three comments.

// sandboxEnv is a LOCAL deployment declared a sandbox tier: the same single
// declaration render.yaml makes for STAGING (CP_API_LEGAL_POLICY=SANDBOX), which
// is what cfg.SandboxTier() reads and what every affordance keys off.
func sandboxEnv(t *testing.T, addr, appURL string) map[string]string {
	t.Helper()
	return map[string]string{
		"CP_ENV":                  "LOCAL",
		"CP_HTTP_ADDR":            addr,
		"CP_DATABASE_APP_URL":     appURL,
		"CP_DATABASE_MIGRATE_URL": os.Getenv("CP_TEST_MIGRATE_DATABASE_URL"),
		"CP_API_LEGAL_POLICY":     "SANDBOX",
		"CP_API_PAYOUT_POLICY":    "SANDBOX",
		// The capabilities render.yaml enables and sandbox-activates on STAGING.
		// Both lists, because configuration is condition 1 of the policy
		// authority and config.Validate refuses a sandbox gate for a capability
		// the deployment has not enabled -- a sandbox gate does not replace the
		// allowlist.
		//
		// They are here because without them the seeder's trades fail on the
		// capability's own refusal, which is D-068's correct outcome and not
		// what this test is measuring. With them the catalogue actually trades,
		// and sandboxGatesAtBoot becomes a fourth idempotence claim under test.
		"CP_API_ENABLED_CAPABILITIES": sandboxCapabilities,
		"CP_API_SANDBOX_GATES":        sandboxCapabilities,
	}
}

// sandboxCapabilities is render.yaml's STAGING list, verbatim.
const sandboxCapabilities = "CREDIT_PURCHASE,NATIVE_ASSET_CREATION,NATIVE_MARKET_TRADING," +
	"MARKETPLACE,PAYOUT_RESERVE,PAYOUT_SETTLE"

func countRows(t *testing.T, d *db.DB, sql string) int {
	t.Helper()
	var n int
	require.NoError(t, d.QueryRow(context.Background(), sql).Scan(&n))
	return n
}

// bootOnce runs the whole binary until it serves, then drains it the way
// SIGTERM does.
func bootOnce(t *testing.T, env map[string]string) {
	t.Helper()
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0o600)
	require.NoError(t, err)
	defer func() { _ = devNull.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	exit := make(chan int, 1)
	go func() { exit <- run(ctx, config.LookupFromMap(env), devNull) }()

	base := "http://" + env["CP_HTTP_ADDR"]
	client := &http.Client{Timeout: 5 * time.Second}
	waitReady(t, client, base)
	require.Equal(t, http.StatusOK, get(t, client, base+"/v1/readyz"))

	cancel()
	select {
	case code := <-exit:
		require.Equal(t, exitOK, code)
	case <-time.After(40 * time.Second):
		t.Fatal("the server did not shut down within the drain window")
	}
	// The listener has to be gone before the next boot binds the same address.
	res, gerr := client.Get(base + "/v1/healthz")
	if gerr == nil {
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
	}
	require.Error(t, gerr)
}

// TestIntegration_BootingTwiceCreatesEverythingOnceOnASandboxTier.
func TestIntegration_BootingTwiceCreatesEverythingOnceOnASandboxTier(t *testing.T) {
	appURL := os.Getenv("CP_TEST_DATABASE_URL")
	if appURL == "" {
		t.Skip("CP_TEST_DATABASE_URL not set; provision one with `go run ./scripts/testdb -name boottwice`")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, db.Config{URL: appURL, AppName: "boot-twice-itest", MaxConns: 4})
	require.NoError(t, err)
	defer pool.Close()

	addr := freeAddr(t)
	env := sandboxEnv(t, addr, appURL)

	bootOnce(t, env)

	// One Credit asset. The schema permits exactly one per deployment (00711),
	// so the count is also the assertion that the boot did not fight it.
	credits := countRows(t, pool, `SELECT count(*) FROM assets WHERE kind = 'CREDIT'`)
	require.Equal(t, 1, credits,
		"the sandbox tier registers its unit of account; without it the demo seeder is a permanent no-op")

	// One GLOBAL risk policy. The version is derived from the policy's own
	// content hash, so a second boot is a CONFLICT on the unique version rather
	// than a second row saying the same thing (D-067).
	policies := countRows(t, pool, `SELECT count(*) FROM risk_policies WHERE scope = 'GLOBAL'`)
	require.Equal(t, 1, policies)

	// The demo catalogue, registered under deterministic seed keys (D-068).
	seeded := countRows(t, pool, `SELECT count(*) FROM demo_seed_rows`)
	require.Positive(t, seeded, "a sandbox tier seeds its demo catalogue")
	markets := countRows(t, pool, `SELECT count(*) FROM native_markets`)
	require.Positive(t, markets)
	fills := countRows(t, pool, `SELECT count(*) FROM native_market_fills`)
	require.Positive(t, fills,
		"the catalogue trades, so an empty-chart state and a thin market both have a subject")
	gates := countRows(t, pool, `SELECT count(*) FROM capability_gates WHERE state = 'SANDBOX'`)
	require.Equal(t, 6, gates, "the six capabilities the deployment lists, sandbox-activated once")

	// Again, against the same database, the way a redeploy does.
	env["CP_HTTP_ADDR"] = freeAddr(t)
	bootOnce(t, env)

	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM assets WHERE kind = 'CREDIT'`),
		"a second boot registers no second unit of account")
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM risk_policies WHERE scope = 'GLOBAL'`),
		"a second boot records no second GLOBAL policy; the version is the content hash")
	assert.Equal(t, seeded, countRows(t, pool, `SELECT count(*) FROM demo_seed_rows`),
		"a second boot seeds nothing new")
	assert.Equal(t, markets, countRows(t, pool, `SELECT count(*) FROM native_markets`))
	assert.Equal(t, fills, countRows(t, pool, `SELECT count(*) FROM native_market_fills`),
		"and trades nothing again; a seeded market is not re-traded on every restart")
	assert.Equal(t, gates, countRows(t, pool, `SELECT count(*) FROM capability_gates WHERE state = 'SANDBOX'`),
		"a gate already in SANDBOX is left alone, not moved again")

	// Every seeded object says it is one, and says which environment made it.
	// A convention can be imitated; a register cannot (D-068).
	var unlabelled int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM native_markets m
		  WHERE NOT EXISTS (SELECT 1 FROM demo_seed_rows d WHERE d.kind = 'NATIVE_MARKET' AND d.ref_id = m.id)`,
	).Scan(&unlabelled))
	assert.Zero(t, unlabelled, "every market on a sandbox tier's fresh database is a labelled demo market")

	var prod int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM demo_seed_rows WHERE environment = 'PROD'`).Scan(&prod))
	assert.Zero(t, prod, "the CHECK forbids it and the seeder refuses it; this is the third refusal")
}

// TestIntegration_ADeploymentThatIsNotASandboxTierSeedsNothing.
//
// The risk policy is recorded on every non-PROD deployment, because a database
// that refuses every internal trade teaches people to work around the control
// (D-067). The Credit asset and the demo catalogue are not: both are sandbox
// affordances, and a LOCAL deployment gets them from scripts/seedeconomy, run by
// a person who knows what they are creating.
func TestIntegration_ADeploymentThatIsNotASandboxTierSeedsNothing(t *testing.T) {
	appURL := os.Getenv("CP_TEST_DATABASE_URL")
	if appURL == "" {
		t.Skip("CP_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, db.Config{URL: appURL, AppName: "boot-plain-itest", MaxConns: 4})
	require.NoError(t, err)
	defer pool.Close()

	before := countRows(t, pool, `SELECT count(*) FROM demo_seed_rows`)
	assets := countRows(t, pool, `SELECT count(*) FROM assets WHERE kind = 'CREDIT'`)

	addr := freeAddr(t)
	env := sandboxEnv(t, addr, appURL)
	// The single declaration, withdrawn. Everything else is unchanged.
	delete(env, "CP_API_LEGAL_POLICY")
	delete(env, "CP_API_PAYOUT_POLICY")
	// Without the declaration the sandbox gates are refused outright rather
	// than ignored, so they come out too: config.Validate is not this test's
	// subject.
	delete(env, "CP_API_SANDBOX_GATES")
	delete(env, "CP_API_ENABLED_CAPABILITIES")
	bootOnce(t, env)

	assert.Equal(t, before, countRows(t, pool, `SELECT count(*) FROM demo_seed_rows`),
		"no sandbox tier, no demo data")
	assert.Equal(t, assets, countRows(t, pool, `SELECT count(*) FROM assets WHERE kind = 'CREDIT'`),
		"no sandbox tier, no Credit asset registered from a config file")
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM risk_policies WHERE scope = 'GLOBAL'`),
		"the risk policy is recorded on every non-PROD deployment, and still only once")
}
