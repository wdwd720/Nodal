//go:build integration

package sandboxchain

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/devnethost"
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
		fmt.Fprintln(os.Stderr, "sandboxchain integration: migrate up:", err)
		return 1
	}
	var err error
	if testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "sandboxchain-itest", MaxConns: 10}); err != nil {
		fmt.Fprintln(os.Stderr, "sandboxchain integration: open:", err)
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

func network() string { return "sbxchain-" + strings.ToLower(uuid.NewString()[:8]) }

// TestIntegration_ASecondBinaryWithTheSameSeedSeesTheSameChain: two opens of
// one network with one seed -- what cmd/api and the advanced worker do --
// share the chain state and the keys.
func TestIntegration_ASecondBinaryWithTheSameSeedSeesTheSameChain(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC))
	net, seed := network(), mustSeed(t, seedHex)
	first, err := Open(ctx, testDB, clk, config.EnvTest, net, seed)
	require.NoError(t, err)
	tip, err := first.Host.ProduceBlocks(ctx, devnethost.ChainSolana, 3)
	require.NoError(t, err)

	second, err := Open(ctx, testDB, clk, config.EnvTest, net, seed)
	require.NoError(t, err, "the same seed replays the same history")
	again, err := second.Host.Tip(ctx, devnethost.ChainSolana)
	require.NoError(t, err)
	assert.Equal(t, tip.Hash, again.Hash, "both see the blocks the first produced")
	assert.Equal(t, first.Keys.Attester.PublicKey(), second.Keys.Attester.PublicKey())
	assert.Equal(t, first.Keys.Issuer.PublicKey(), second.Keys.Issuer.PublicKey())
}

// TestIntegration_AWrongSeedFailsClosedOnAnExistingNetwork: a binary given a
// different seed would sign with keys the chain does not know; the host's
// genesis pins the attester, so opening refuses instead.
func TestIntegration_AWrongSeedFailsClosedOnAnExistingNetwork(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC))
	net := network()
	_, err := Open(ctx, testDB, clk, config.EnvTest, net, mustSeed(t, seedHex))
	require.NoError(t, err)

	other := []byte(seedHex)
	other[0] = '9'
	_, err = Open(ctx, testDB, clk, config.EnvTest, net, mustSeed(t, string(other)))
	require.Error(t, err, "a history recorded under another attester is refused")
}

func TestIntegration_OpenRefusesAMissingSeed(t *testing.T) {
	requireEnv(t)
	_, err := Open(context.Background(), testDB, clock.NewFake(time.Now()), config.EnvTest, network(), Seed{})
	require.Error(t, err)
}
