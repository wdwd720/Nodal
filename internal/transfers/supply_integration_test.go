//go:build integration

package transfers

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/econ/econtest"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// TestIntegration_AStableAssetTransfersAndReversesAndSupplyReconciles: the
// flows 00763 allows for a platform stable asset and its guards must leave
// alone. A customer-to-customer transfer (P2P_TRANSFER) and its reversal
// (COMPENSATION) move units between holders and create none: both post, and
// the asset's holdings still equal what its issuer issued.
func TestIntegration_AStableAssetTransfersAndReversesAndSupplyReconciles(t *testing.T) {
	f := newFixture(t)
	var s econtest.Supply
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		s, err = econtest.RegisterSupply(ctx, tx)
		return err
	}))
	from, to := f.customer(), f.customer()
	f.fundAsset(from.account, s.Dollar.ID, 500_000_000)
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.ledger.Post(ctx, tx, s.Stable.Issuance(from.account, money.QuantityFromInt64(500_000_000), uuid.NewString(), f.clk.Now()))
		return err
	}))
	reconciles := func(what string) {
		t.Helper()
		held, err := econtest.Holdings(f.ctx, testDB, s.Stable.Asset.ID)
		require.NoError(t, err)
		outstanding, err := s.Stable.Outstanding(f.ctx, testDB, f.ledger)
		require.NoError(t, err)
		assert.Equal(t, "500000000", outstanding.String(), what)
		assert.True(t, held.Equal(outstanding), "%s: holdings %s, outstanding %s", what, held, outstanding)
	}

	req := f.req(from, to, 120_000_000)
	req.AssetID = s.Stable.Asset.ID
	res, err := f.send(from.p, req)
	require.NoError(t, err, "a transfer of a stable asset is an ordinary customer movement")
	assert.Equal(t, StatusCompleted, res.Transfer.Status)
	assert.Equal(t, "120000000", f.balanceOf(to.account, ledger.CodeWallet, s.Stable.Asset.ID).String())
	assert.Equal(t, "380000000", f.balanceOf(from.account, ledger.CodeWallet, s.Stable.Asset.ID).String())
	reconciles("after the transfer")

	_, err = f.reverse(f.op(), f.op(), res.Transfer.ID, "DUPLICATE_PAYMENT", "paid twice")
	require.NoError(t, err, "its reversal is a COMPENSATION, which the database allows too")
	assert.True(t, f.balanceOf(to.account, ledger.CodeWallet, s.Stable.Asset.ID).IsZero())
	assert.Equal(t, "500000000", f.balanceOf(from.account, ledger.CodeWallet, s.Stable.Asset.ID).String())
	reconciles("after the reversal")
}
