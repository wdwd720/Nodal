//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/security"
)

// TestIntegration_TheWorkerWiresDelegatedAuthorityOnlyWithASandboxChain: with
// the sandbox chain configured the worker builds the admission facade over
// the migrated database and the chain, and the facade answers -- refusing a
// caller that is not one of the account's agents; without it the worker
// wires none and says why; and a seed that is not one refuses to start.
func TestIntegration_TheWorkerWiresDelegatedAuthorityOnlyWithASandboxChain(t *testing.T) {
	url := os.Getenv("CP_TEST_DATABASE_URL")
	if url == "" || os.Getenv("CP_TEST_MIGRATE_DATABASE_URL") == "" {
		t.Skip("set CP_TEST_DATABASE_URL and CP_TEST_MIGRATE_DATABASE_URL")
	}
	ctx := context.Background()
	var seed [32]byte
	_, err := rand.Read(seed[:])
	require.NoError(t, err)
	network := "agw-" + strings.ReplaceAll(uuid.NewString(), "-", "")[20:]

	t.Run("with the sandbox chain", func(t *testing.T) {
		var errOut bytes.Buffer
		d, err := wire(ctx, exampleEnv(t, map[string]string{
			"CP_ENV": "LOCAL", "CP_DATABASE_APP_URL": url,
			"CP_SANDBOX_DEVNET_NETWORK": network, "CP_SANDBOX_DEVNET_SEED_REF": hex.EncodeToString(seed[:]),
		}), &errOut)
		require.NoError(t, err, errOut.String())
		defer d.db.Close()
		require.NotNil(t, d.delegated, "delegated authority is wired")
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

	t.Run("a seed that is not one", func(t *testing.T) {
		var errOut bytes.Buffer
		_, err := wire(ctx, exampleEnv(t, map[string]string{
			"CP_ENV": "LOCAL", "CP_DATABASE_APP_URL": url,
			"CP_SANDBOX_DEVNET_NETWORK": network, "CP_SANDBOX_DEVNET_SEED_REF": "not-a-seed",
		}), &errOut)
		require.Error(t, err, "a configured chain with an unusable seed fails the start, never wires a guess")
	})
}
