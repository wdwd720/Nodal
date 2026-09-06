package wallettest_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/provider"
	"github.com/nodal/controlplane/internal/signing/inspect"
	"github.com/nodal/controlplane/internal/signing/signingtest"
	"github.com/nodal/controlplane/internal/wallet"
	"github.com/nodal/controlplane/internal/wallet/wallettest"
)

func TestFake_RefusesProductionLikeEnvironments(t *testing.T) {
	t.Parallel()
	for _, env := range []config.Environment{config.EnvStaging, config.EnvProd} {
		_, err := wallettest.New(env)
		require.Error(t, err, env)
		assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	}
	for _, env := range []config.Environment{config.EnvLocal, config.EnvTest, config.EnvDev} {
		_, err := wallettest.New(env)
		require.NoError(t, err, env)
	}
}

func TestFake_DeterministicWalletsAndSigning(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, err := wallettest.New(config.EnvTest)
	require.NoError(t, err)
	g, err := wallettest.New(config.EnvTest)
	require.NoError(t, err)
	acct := accounts.NewAccountID().String()

	w1, err := f.CreateWallet(ctx, acct, "solana-devnet")
	require.NoError(t, err)
	w2, err := g.CreateWallet(ctx, acct, "solana-devnet")
	require.NoError(t, err)
	assert.Equal(t, w1.ProviderWalletID, w2.ProviderWalletID, "same seed → same wallet")
	assert.Equal(t, w1.Address, w2.Address)
	assert.Equal(t, wallet.KindEmbeddedDelegated, w1.Kind)
	assert.Equal(t, wallet.StatusActive, w1.Status)
	h, err := wallettest.New(config.EnvTest, wallettest.WithSeed("other"))
	require.NoError(t, err)
	w3, err := h.CreateWallet(ctx, acct, "solana-devnet")
	require.NoError(t, err)
	assert.NotEqual(t, w1.Address, w3.Address, "different seed → different key")

	got, err := f.GetWallet(ctx, w1.ProviderWalletID)
	require.NoError(t, err)
	assert.Equal(t, w1.Address, got.Address)
	_, err = f.GetWallet(ctx, "privy-123")
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
	_, err = f.CreateWallet(ctx, "not-a-uuid", "solana-devnet")
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	st, err := f.VerifyDelegation(ctx, w1.ProviderWalletID)
	require.NoError(t, err)
	assert.True(t, st.Verified())
	capability, err := f.Capabilities(ctx)
	require.NoError(t, err)
	assert.True(t, capability.DelegationVerified())
	assert.Equal(t, provider.CodeComplete, capability.VerificationLabel)
	f.SetDelegationVerified(false)
	st, err = f.VerifyDelegation(ctx, w1.ProviderWalletID)
	require.NoError(t, err)
	assert.False(t, st.Verified())
	f.SetDelegationVerified(true)

	// Sign a real fixture whose fee payer is this wallet.
	s := signingtest.NewSwap()
	s.WalletKey = f.KeyFor(w1.ProviderWalletID)
	s.Derive()
	raw := s.Golden(inspect.VersionV0)
	req := wallet.SignRequest{ProviderWalletID: w1.ProviderWalletID, Chain: "solana-devnet", UnsignedTx: raw, IdempotencyKey: "attempt-1", Purpose: wallet.PurposeSwap}
	res, err := f.SignTransaction(ctx, req)
	require.NoError(t, err)
	require.NoError(t, f.Verify(w1.ProviderWalletID, res.SignedTx))
	assert.Len(t, res.Signature, 64)
	assert.Equal(t, provider.IdempotentWrite, res.RetryClass)
	tx, err := solana.TransactionFromBytes(res.SignedTx)
	require.NoError(t, err)
	assert.Equal(t, solana.SignatureFromBytes(res.Signature), tx.Signatures[0])

	// Same idempotency key → identical result, still recorded.
	again, err := f.SignTransaction(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, res.SignedTx, again.SignedTx)
	assert.Equal(t, 2, f.SignCalls())
	assert.Equal(t, req.IdempotencyKey, f.Calls()[1].IdempotencyKey)

	// A transaction whose fee payer is another wallet is refused.
	other := signingtest.NewSwap()
	_, err = f.SignTransaction(ctx, wallet.SignRequest{ProviderWalletID: w1.ProviderWalletID, Chain: "solana-devnet", UnsignedTx: other.Golden(inspect.VersionLegacy), IdempotencyKey: "attempt-2", Purpose: wallet.PurposeSwap})
	assert.Equal(t, errs.CodeSigningRejected, errs.CodeOf(err))
	_, err = f.SignTransaction(ctx, wallet.SignRequest{ProviderWalletID: w1.ProviderWalletID, Chain: "solana-devnet", UnsignedTx: []byte{1, 2}, IdempotencyKey: "attempt-3", Purpose: wallet.PurposeSwap})
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = f.SignTransaction(ctx, wallet.SignRequest{})
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

func TestFake_FailAndTimeout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, err := wallettest.New(config.EnvLocal)
	require.NoError(t, err)
	f.FailWith(errs.New(errs.CodeProviderUnavailable, "down"))
	_, err = f.Capabilities(ctx)
	assert.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))
	f.FailWith(nil)
	_, err = f.Capabilities(ctx)
	require.NoError(t, err)

	f.Delay(time.Second)
	tctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_, err = f.GetWallet(tctx, "fake-abc")
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded))
	assert.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))
}
