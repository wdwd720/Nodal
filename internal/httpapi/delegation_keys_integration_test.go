//go:build integration

package httpapi

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/delegation"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/money"
)

// The owner's grant over HTTP registers the account's delegated wallet key,
// in the process that holds the delegated signer; the agent runtime's
// facade, built from the deployment's settings and the chain alone, plans
// from that key; and the platform signs and settles what it admitted for
// exactly that wallet.
func TestIntegration_DelegationGrantRegistersTheWalletKeyTheAgentsAdmissionReads(t *testing.T) {
	w := newDgWorld(t)
	c := w.customer(1_000_000_000)
	registered := func() []string {
		rows, err := w.d.Query(w.ctx, `SELECT chain_id || '=' || public_key FROM delegated_signer_keys WHERE account_id = $1
			ORDER BY chain_id`, c.account)
		require.NoError(t, err)
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			require.NoError(t, rows.Scan(&s))
			out = append(out, s)
		}
		require.NoError(t, rows.Err())
		return out
	}
	require.Empty(t, registered(), "no key before the owner grants")

	capability := w.grant(c, nil)
	require.Equal(t, []string{string(devnethost.ChainSolana) + "=" + c.wallet.String()}, registered(),
		"the grant registered the signer's own key on the chain it allows")

	admission, err := delegation.NewAgentAdmission(delegation.AdmissionConfig{Clock: w.clk, Environment: config.EnvTest,
		Host: w.host, EnabledCapabilities: []string{"AGENT_SIGNING", "AGENT_WITHDRAWALS"}, BuildVersion: "httpapi-delegation-itest"})
	require.NoError(t, err)
	cid, err := delegation.ParseCapabilityID(capability.Id.String())
	require.NoError(t, err)
	w.priceTok()
	var ticket delegation.SwapTicket
	require.NoError(t, w.d.InTx(w.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(_ context.Context, tx pgx.Tx) error {
		var err error
		ticket, err = admission.RequestSwap(w.agent(c), tx, delegation.SwapRequest{CapabilityID: cid, AccountID: c.account,
			Nonce: 1, Action: delegation.ActionAcquireExposure, VenueID: w.venue.ID, PayAssetID: w.usd.ID, ReceiveAssetID: w.tok.ID,
			AmountIn: money.QuantityFromInt64(100_000_000), MaxSlippageBPS: 100, Jurisdiction: "US-CA",
			CorrelationID: "corr-" + uuid.NewString()})
		return err
	}))
	require.NoError(t, ticket.Refusal)
	require.Equal(t, delegation.SwapAdmitted, ticket.Swap.Status)
	assert.Equal(t, c.wallet, ticket.Swap.Wallet, "the facade planned from the registered key")

	res, err := w.svc.ExecuteSwap(w.ctx, ticket.Swap.ID)
	require.NoError(t, err)
	assert.Equal(t, delegation.SwapSettled, res.Swap.Status, "failure %q", res.Swap.FailureCode)
	assert.Equal(t, []string{string(devnethost.ChainSolana) + "=" + c.wallet.String()}, registered(),
		"admitting and executing registered nothing more")
}
