//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/sandboxchain"
	"github.com/nodal/controlplane/internal/security"
)

// TestIntegration_TheWorkerWiresDelegatedAuthorityOnlyWithASandboxChain:
// with the sandbox chain configured -- its network, and nothing else -- the
// worker builds the admission facade over the migrated database and the
// chain a seed holder opened, and the facade answers, refusing a caller that
// is not one of the account's agents. The chain it holds is read only: it
// names the attester the network's genesis recorded and refuses every
// write. A network nobody has opened is refused and left unwritten; a seed
// reference in the environment is never read, not even to be parsed; and
// without a network the worker wires nothing and says why.
func TestIntegration_TheWorkerWiresDelegatedAuthorityOnlyWithASandboxChain(t *testing.T) {
	url := os.Getenv("CP_TEST_DATABASE_URL")
	if url == "" || os.Getenv("CP_TEST_MIGRATE_DATABASE_URL") == "" {
		t.Skip("set CP_TEST_DATABASE_URL and CP_TEST_MIGRATE_DATABASE_URL")
	}
	ctx := context.Background()
	platformDB, err := db.Open(ctx, db.Config{URL: url, AppName: "agent-worker-itest-platform"})
	require.NoError(t, err)
	defer platformDB.Close()
	events := func(network string) int {
		var n int
		require.NoError(t, platformDB.QueryRow(ctx, `SELECT count(*) FROM devnet_events WHERE network = $1`, network).Scan(&n))
		return n
	}

	// The seed holder -- cmd/api, here -- opens the network, writing its
	// genesis under the attester its seed derives.
	var seed sandboxchain.Seed
	_, err = rand.Read(seed[:])
	require.NoError(t, err)
	network := "agw-" + strings.ReplaceAll(uuid.NewString(), "-", "")[20:]
	_, err = sandboxchain.Open(ctx, platformDB, clock.System(), config.EnvLocal, network, seed)
	require.NoError(t, err)
	require.Equal(t, 1, events(network), "the seed holder wrote the genesis")

	t.Run("with the sandbox chain", func(t *testing.T) {
		var errOut bytes.Buffer
		d, err := wire(ctx, exampleEnv(t, map[string]string{
			"CP_ENV": "LOCAL", "CP_DATABASE_APP_URL": url,
			"CP_SANDBOX_DEVNET_NETWORK": network, "CP_SANDBOX_DEVNET_SEED_REF": "-",
		}), &errOut)
		require.NoError(t, err, errOut.String())
		defer d.db.Close()
		require.NotNil(t, d.delegated, "delegated authority is wired from the network alone")
		assert.Empty(t, d.delegatedReason)

		// The facade is live over the database, and it is an agent's door
		// only: no principal, or an agent the account does not have, is
		// refused.
		account, err := accounts.ParseAccountID(id.New[id.Any]().String())
		require.NoError(t, err)
		_, err = d.delegated.AgentCapabilities(ctx, d.db, account)
		require.Error(t, err)
		assert.Equal(t, errs.CodeUnauthenticated, errs.CodeOf(err))
		stranger := security.WithPrincipal(ctx, security.AgentPrincipal(id.New[id.Any]().String(), account.String()))
		_, err = d.delegated.AgentCapabilities(stranger, d.db, account)
		require.Error(t, err)
		assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
		assert.Equal(t, 1, events(network), "wiring the worker wrote nothing to the chain")
	})

	t.Run("the worker's chain is read only", func(t *testing.T) {
		host, err := openChainReader(ctx, platformDB, clock.System(), config.EnvLocal, network)
		require.NoError(t, err)
		attester := sandboxchain.DeriveKeys(seed).Attester.PublicKey()
		for _, c := range host.Config().Chains {
			assert.Equal(t, attester, c.BridgeAuthority, "the attester the genesis recorded, its public key only")
		}
		require.NoError(t, host.Health(ctx, devnethost.ChainSolana), "it reads the chain")
		_, err = host.ProduceBlocks(ctx, devnethost.ChainSolana, 1)
		require.Error(t, err)
		assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err), "%v", err)
		err = host.Airdrop(ctx, devnethost.ChainSolana, devnet.NewKeypairFromSeed([32]byte{7}).PublicKey(), 1_000)
		require.Error(t, err)
		assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err), "%v", err)
		assert.Equal(t, 1, events(network), "the refused writes left the history as it was")
		require.NoError(t, host.Health(ctx, devnethost.ChainSolana))
	})

	t.Run("a network nobody has opened", func(t *testing.T) {
		unopened := "agw-" + strings.ReplaceAll(uuid.NewString(), "-", "")[20:]
		var errOut bytes.Buffer
		_, err := wire(ctx, exampleEnv(t, map[string]string{
			"CP_ENV": "LOCAL", "CP_DATABASE_APP_URL": url,
			"CP_SANDBOX_DEVNET_NETWORK": unopened, "CP_SANDBOX_DEVNET_SEED_REF": "-",
		}), &errOut)
		require.Error(t, err, "the worker cannot open what its seed holder has not")
		assert.Contains(t, err.Error(), "no recorded history")
		assert.Zero(t, events(unopened), "and writes no genesis of its own")
	})

	t.Run("a seed reference it never reads", func(t *testing.T) {
		var errOut bytes.Buffer
		d, err := wire(ctx, exampleEnv(t, map[string]string{
			"CP_ENV": "LOCAL", "CP_DATABASE_APP_URL": url,
			"CP_SANDBOX_DEVNET_NETWORK": network, "CP_SANDBOX_DEVNET_SEED_REF": "not-a-seed",
		}), &errOut)
		require.NoError(t, err, "a reference the worker never resolves cannot fail its start: %s", errOut.String())
		defer d.db.Close()
		assert.NotNil(t, d.delegated)
		assert.Contains(t, errOut.String(), envSandboxSeedRef, "the worker warns that it was offered one")
	})

	t.Run("without it", func(t *testing.T) {
		var errOut bytes.Buffer
		d, err := wire(ctx, exampleEnv(t, map[string]string{
			"CP_ENV": "LOCAL", "CP_DATABASE_APP_URL": url,
			"CP_SANDBOX_DEVNET_NETWORK": "-", "CP_SANDBOX_DEVNET_SEED_REF": "-",
		}), &errOut)
		require.NoError(t, err, errOut.String())
		defer d.db.Close()
		assert.Nil(t, d.delegated)
		assert.Equal(t, delegatedNotWired, d.delegatedReason)
	})
}
