//go:build integration

package delegation

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/errs"
)

// legacyGrant records a grant as GrantCapability recorded one before the
// grant registered keys -- the capability, its transition and its audit
// event, and no key: what an account granted before then looks like.
func (f *fixture) legacyGrant(c customer, extra ...devnet.PublicKey) Capability {
	f.t.Helper()
	req := f.grantRequest(c, extra...)
	var out Capability
	require.NoError(f.t, f.inTx(func(_ context.Context, tx pgx.Tx) error {
		var err error
		out, _, err = f.svc.recordGrant(f.owner(c), tx, req)
		return err
	}))
	return out
}

// facade is the agent runtime's side of the fixture's deployment: the
// admission facade over the same chain, built from the settings alone.
func (f *fixture) facade() *AgentAdmission {
	f.t.Helper()
	a, err := NewAgentAdmission(AdmissionConfig{Clock: f.clk, Environment: config.EnvTest, Host: f.host,
		EnabledCapabilities: []string{"AGENT_SIGNING", "AGENT_WITHDRAWALS"}, BuildVersion: "delegation-itest"})
	require.NoError(f.t, err)
	return a
}

// registeredKeys is the account's registered delegated wallet keys by chain.
func (f *fixture) registeredKeys(c customer) map[devnet.ChainID]devnet.PublicKey {
	f.t.Helper()
	out := map[devnet.ChainID]devnet.PublicKey{}
	for _, chain := range Chains() {
		k, ok, err := registeredKey(f.ctx, testDB, c.account, chain)
		require.NoError(f.t, err)
		if ok {
			out[chain] = k
		}
	}
	return out
}

// TestIntegration_AGrantRegistersTheWalletKeyOnEveryChainItAllows: the
// owner's grant -- in the process that holds the signer -- registers the
// account's delegated wallet key on every chain the capability allows, in
// the grant's own transaction, so an admission on any of them finds it. A
// replayed grant registers nothing twice; a grant on an account whose
// registered key is not the signer's is refused and records nothing; and the
// agent's facade, which holds no key, admits from the registered key a swap
// the platform then signs for that very wallet and settles.
func TestIntegration_AGrantRegistersTheWalletKeyOnEveryChainItAllows(t *testing.T) {
	f := newFixture(t)
	c := f.customer(10_000_000)
	require.Empty(t, f.registeredKeys(c), "an account has no delegated wallet key before its owner grants")

	both := f.grantRequest(c)
	both.ChainAllowlist = []devnet.ChainID{devnethost.ChainSolana, devnethost.ChainEthereum}
	var capability Capability
	for attempt := 0; attempt < 2; attempt++ {
		require.NoError(t, f.inTx(func(_ context.Context, tx pgx.Tx) error {
			var existing bool
			var err error
			capability, existing, err = f.svc.GrantCapability(f.owner(c), tx, both)
			assert.Equal(t, attempt == 1, existing, "the second grant is the first, replayed")
			return err
		}))
	}
	keys := f.registeredKeys(c)
	require.Len(t, keys, 2, "a key on every chain the grant allows")
	for _, chain := range []devnet.ChainID{devnethost.ChainSolana, devnethost.ChainEthereum} {
		assert.Equal(t, f.signer.PublicKey(c.account, chain), keys[chain], "the signer's own key on %s", chain)
	}
	assert.Equal(t, 2, f.count(`SELECT count(*) FROM delegated_signer_keys WHERE account_id = $1 AND derivation = $2`,
		c.account, KeyDerivation), "registered once each, a replay adds nothing")

	// A signer with another master seed cannot claim the account's wallets:
	// its grant is refused, and nothing of it is recorded.
	stranger, err := NewDelegatedSigner(seedOf("another-master"), SignerConfig{Environment: config.EnvTest, Clock: f.clk,
		Policy: DefaultSigningPolicy(), Fees: HostFees(f.host)})
	require.NoError(t, err)
	f.rebuild(func(d *Deps) { d.Signer = stranger })
	err = f.inTx(func(_ context.Context, tx pgx.Tx) error {
		_, _, err := f.svc.GrantCapability(f.owner(c), tx, f.grantRequest(c))
		return err
	})
	requireCode(t, err, errs.CodeConflict)
	requireReason(t, err, ReasonSignerKeyMismatch)
	f.rebuild(func(d *Deps) { d.Signer = f.signer })
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM delegated_capabilities WHERE account_id = $1`, c.account),
		"the refused grant left no capability")
	assert.Equal(t, keys, f.registeredKeys(c), "and no key")

	// The agent's side holds no key: it plans from the registered one, and
	// the platform signs for exactly that wallet.
	agentSide := f.facade()
	var ticket SwapTicket
	require.NoError(t, f.inTx(func(_ context.Context, tx pgx.Tx) error {
		var err error
		ticket, err = agentSide.RequestSwap(f.agent(c), tx, f.swapRequest(c, capability.ID, 1, 1_000_000))
		return err
	}))
	require.NoError(t, ticket.Refusal)
	require.Equal(t, SwapAdmitted, ticket.Swap.Status)
	assert.Equal(t, keys[devnethost.ChainSolana], ticket.Swap.Wallet, "the wallet is the registered key")
	res, err := f.svc.ExecuteSwap(f.ctx, ticket.Swap.ID)
	require.NoError(t, err)
	assert.Equal(t, SwapSettled, res.Swap.Status, "failure %q", res.Swap.FailureCode)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM delegated_signing_decisions WHERE swap_id = $1 AND verdict = 'APPROVED'
		AND signer_public_key = $2`, ticket.Swap.ID, keys[devnethost.ChainSolana].String()))
}

// TestIntegration_AnAdmissionRefusesAnAccountWithNoRegisteredWalletKey: an
// admission derives and registers nothing. An account whose capability was
// recorded without its key -- a grant made before grants registered keys --
// is refused, swap and withdrawal alike, with
// DELEGATION_SIGNER_KEY_NOT_REGISTERED before anything is recorded: no
// intent, no subject, no key. The owner's next grant on the chain registers
// the key, and the same requests are admitted from it.
func TestIntegration_AnAdmissionRefusesAnAccountWithNoRegisteredWalletKey(t *testing.T) {
	f := newFixture(t)
	c := f.customer(100_000_000)
	f.policy(c, nil)
	dest := f.destination(c)
	legacy := f.legacyGrant(c, dest.Address)
	f.advance(2 * time.Hour)
	agentSide := f.facade()

	swapReq := f.swapRequest(c, legacy.ID, 1, 1_000_000)
	withdrawalReq := ProposalRequest{CapabilityID: legacy.ID, AccountID: c.account, Nonce: 1, DestinationID: dest.ID,
		AssetID: f.usd.ID, Amount: swapReq.AmountIn, Jurisdiction: "US-CA", CorrelationID: "corr-legacy-withdrawal"}
	for name, admit := range map[string]func(tx pgx.Tx) error{
		"the facade's swap": func(tx pgx.Tx) error {
			_, err := agentSide.RequestSwap(f.agent(c), tx, swapReq)
			return err
		},
		"the facade's withdrawal": func(tx pgx.Tx) error {
			_, err := agentSide.ProposeWithdrawal(f.agent(c), tx, withdrawalReq)
			return err
		},
		"the platform's own admission": func(tx pgx.Tx) error {
			_, err := f.svc.RequestSwap(f.agent(c), tx, swapReq)
			return err
		},
	} {
		err := f.inTx(func(_ context.Context, tx pgx.Tx) error { return admit(tx) })
		requireCode(t, err, errs.CodeForbidden)
		requireReason(t, err, ReasonSignerKeyNotRegistered)
		assert.Empty(t, f.registeredKeys(c), "%s registered no key", name)
	}
	assert.Zero(t, f.count(`SELECT count(*) FROM economic_intents WHERE account_id = $1`, c.account), "nothing was recorded")
	assert.Zero(t, f.count(`SELECT count(*) FROM delegated_swaps WHERE account_id = $1`, c.account))
	assert.Zero(t, f.count(`SELECT count(*) FROM delegated_withdrawals WHERE account_id = $1`, c.account))

	// The owner grants again on the chain: the key is registered, and the
	// refused requests are admitted from it, unchanged.
	f.grant(c, nil, dest.Address)
	keys := f.registeredKeys(c)
	require.Equal(t, map[devnet.ChainID]devnet.PublicKey{devnethost.ChainSolana: c.wallet}, keys)
	var swapTicket SwapTicket
	var withdrawalTicket WithdrawalTicket
	require.NoError(t, f.inTx(func(_ context.Context, tx pgx.Tx) error {
		var err error
		swapTicket, err = agentSide.RequestSwap(f.agent(c), tx, swapReq)
		return err
	}))
	require.NoError(t, f.inTx(func(_ context.Context, tx pgx.Tx) error {
		var err error
		withdrawalTicket, err = agentSide.ProposeWithdrawal(f.agent(c), tx, withdrawalReq)
		return err
	}))
	require.NoError(t, swapTicket.Refusal)
	require.NoError(t, withdrawalTicket.Refusal)
	assert.Equal(t, SwapAdmitted, swapTicket.Swap.Status)
	assert.Equal(t, c.wallet, swapTicket.Swap.Wallet)
	assert.Equal(t, WithdrawalApproved, withdrawalTicket.Withdrawal.Status, "auto-approved in test mode under the policy")
	assert.Equal(t, c.wallet, withdrawalTicket.Withdrawal.Wallet)
}
