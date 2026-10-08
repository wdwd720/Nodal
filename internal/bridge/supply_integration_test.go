//go:build integration

package bridge

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/econ/econtest"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// TestIntegration_ABridgedTwinMovesOnlyAsItsBridgeReleasesItAndItsSupplyReconciles:
// the bridge is a flow 00763 allows and the guards must leave alone. A route's
// twin is supply-controlled from the moment the route exists (its controller
// is the route's BRIDGE); the release mints it (BRIDGE_DESTINATION_FINALIZED,
// the controller's own kind), a customer may pass it on (P2P_TRANSFER), and a
// failed release is refunded on the source side (BRIDGE_REFUND, the source
// role). Every one of them posts, and the twin's holdings always equal what
// its bridge released.
func TestIntegration_ABridgedTwinMovesOnlyAsItsBridgeReleasesItAndItsSupplyReconciles(t *testing.T) {
	f := newFixture(t)
	r := f.route
	controller, err := econ.SupplyController(f.ctx, testDB, r.DestinationAssetID)
	require.NoError(t, err)
	assert.Equal(t, econ.ControllerBridge, controller)
	source, err := econ.SupplyController(f.ctx, testDB, r.SourceAssetID)
	require.NoError(t, err)
	assert.Empty(t, source, "a route's source asset is no controller's")

	reconciles := func(want, what string) {
		t.Helper()
		held, err := econtest.Holdings(f.ctx, testDB, r.DestinationAssetID)
		require.NoError(t, err)
		assert.Equal(t, want, f.facility(r, ledger.CodeBridgeOutstanding, r.DestinationAssetID), what)
		assert.Equal(t, want, held.String(), "%s: the twin's holdings are what its bridge released", what)
	}

	c, d := f.customer(), f.customer()
	f.fund(r, c, 1_000_000)
	f.mustBridge(r, c, 400_000)
	reconciles("400000", "after the release")

	// The twin passes from one customer to another.
	q := money.QuantityFromInt64(150_000)
	twin := r.DestinationAssetID
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := f.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindP2PTransfer, IdempotencyKey: "itest-p2p:" + ref,
			Reference: ledger.FinancialEventReference{Type: "itest_p2p", ID: ref}, EffectiveAt: f.clk.Now(),
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(c.id, ledger.CodeTransferOut, twin), Side: ledger.Debit, Quantity: q},
				{Account: ledger.CustomerAccount(c.id, ledger.CodeWallet, twin), Side: ledger.Credit, Quantity: q},
				{Account: ledger.CustomerAccount(d.id, ledger.CodeWallet, twin), Side: ledger.Debit, Quantity: q},
				{Account: ledger.CustomerAccount(d.id, ledger.CodeTransferIn, twin), Side: ledger.Credit, Quantity: q},
			},
		})
		return err
	}))
	assert.Equal(t, "150000", f.wallet(d, twin))
	reconciles("400000", "after a transfer between customers")

	// A release that cannot land is refunded at the source; no twin is minted.
	require.NoError(t, f.host.SetMintFrozen(f.ctx, r.DestinationChain, r.DestinationMint, true))
	res, err := f.bridge(r, c, 200_000, gates.ModeSandbox)
	require.NoError(t, err)
	require.Equal(t, StateManualReview, res.Leg.Status)
	out, err := f.svc.Refund(f.ctx, res.Leg.ID, operator, "the destination mint is frozen")
	require.NoError(t, err)
	assert.Equal(t, 1, f.journals(out.Leg, ledger.KindBridgeRefund))
	reconciles("400000", "after a refund")
	assert.True(t, f.reconcile(r).Matched())
}
