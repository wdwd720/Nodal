package wallet_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/wallet"
)

func TestStatus_Transitions(t *testing.T) {
	t.Parallel()
	assert.True(t, wallet.CanTransition(wallet.StatusActive, wallet.StatusSuspended))
	assert.True(t, wallet.CanTransition(wallet.StatusActive, wallet.StatusRevoked))
	assert.True(t, wallet.CanTransition(wallet.StatusSuspended, wallet.StatusActive))
	assert.True(t, wallet.CanTransition(wallet.StatusSuspended, wallet.StatusRevoked))
	assert.False(t, wallet.CanTransition(wallet.StatusRevoked, wallet.StatusActive), "REVOKED is terminal")
	assert.False(t, wallet.CanTransition(wallet.StatusActive, wallet.StatusActive))
	assert.False(t, wallet.CanTransition(wallet.StatusActive, "BOGUS"))
	assert.True(t, wallet.StatusActive.Valid())
	assert.False(t, wallet.Status("x").Valid())
}

func TestWallet_Validate(t *testing.T) {
	t.Parallel()
	good := wallet.Wallet{
		AccountID: accounts.NewAccountID(), Provider: "privy", ProviderWalletID: "w1", Chain: "solana-mainnet",
		Address: "So11111111111111111111111111111111111111112", Kind: wallet.KindEmbeddedDelegated, Status: wallet.StatusActive,
	}
	require.NoError(t, good.Validate())
	assert.False(t, good.DelegationVerified())
	cases := map[string]func(w *wallet.Wallet){
		"account":  func(w *wallet.Wallet) { w.AccountID = accounts.AccountID{} },
		"provider": func(w *wallet.Wallet) { w.Provider = "" },
		"pid":      func(w *wallet.Wallet) { w.ProviderWalletID = "" },
		"chain":    func(w *wallet.Wallet) { w.Chain = "" },
		"address":  func(w *wallet.Wallet) { w.Address = "" },
		"kind":     func(w *wallet.Wallet) { w.Kind = "CUSTODIAL" },
		"status":   func(w *wallet.Wallet) { w.Status = "GONE" },
		"caps":     func(w *wallet.Wallet) { w.Capabilities = []byte("{not json") },
	}
	for name, mutate := range cases {
		w := good
		mutate(&w)
		err := w.Validate()
		require.Error(t, err, name)
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), name)
	}
}

func TestStatusChange_Validate(t *testing.T) {
	t.Parallel()
	good := wallet.StatusChange{To: wallet.StatusSuspended, ActorType: security.ActorOperator, ActorID: "op", Reason: "hold"}
	require.NoError(t, good.Validate())
	assert.Contains(t, good.String(), "SUSPENDED")

	agent := good
	agent.ActorType = security.ActorAgent
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(agent.Validate()), "agents never change wallet authority")
	unknown := good
	unknown.ActorType = "ROBOT"
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(unknown.Validate()))
	noReason := good
	noReason.Reason = ""
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(noReason.Validate()))
	noActor := good
	noActor.ActorID = ""
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(noActor.Validate()))
	badTo := good
	badTo.To = "X"
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(badTo.Validate()))
}

func TestSignRequest_Validate(t *testing.T) {
	t.Parallel()
	good := wallet.SignRequest{ProviderWalletID: "w", Chain: "solana-mainnet", UnsignedTx: []byte{1}, IdempotencyKey: "k", Purpose: wallet.PurposeSwap}
	require.NoError(t, good.Validate())
	for name, mutate := range map[string]func(r *wallet.SignRequest){
		"wallet":  func(r *wallet.SignRequest) { r.ProviderWalletID = "" },
		"chain":   func(r *wallet.SignRequest) { r.Chain = "" },
		"tx":      func(r *wallet.SignRequest) { r.UnsignedTx = nil },
		"key":     func(r *wallet.SignRequest) { r.IdempotencyKey = "" },
		"purpose": func(r *wallet.SignRequest) { r.Purpose = "WITHDRAW" },
	} {
		r := good
		mutate(&r)
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(r.Validate()), name)
	}
}

func TestCapabilityAndDelegation(t *testing.T) {
	t.Parallel()
	assert.True(t, wallet.Capability{DelegatedSigning: wallet.CapabilityVerified}.DelegationVerified())
	assert.False(t, wallet.Capability{DelegatedSigning: wallet.CapabilityUnverified}.DelegationVerified())
	assert.True(t, wallet.DelegationStatus{State: wallet.DelegationVerified}.Verified())
	assert.False(t, wallet.DelegationStatus{State: wallet.DelegationRevoked}.Verified())
	id := wallet.NewWalletID()
	parsed, err := wallet.ParseWalletID(id.String())
	require.NoError(t, err)
	assert.Equal(t, id, parsed)
}
