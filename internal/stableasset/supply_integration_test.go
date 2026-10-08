//go:build integration

package stableasset

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/econ/econtest"
)

// TestIntegration_AStableAssetIssuesAndRedeemsAndItsSupplyReconciles: the
// controller's own flows, which 00763 allows and the guards must leave alone.
// A registered stable asset is supply-controlled from its first moment (its
// controller is its STABLE_ISSUER); a mint posts STABLE_ISSUANCE and a burn
// STABLE_REDEMPTION, and after each the issuer's supply reconciliation --
// holdings against what was issued, against the chain -- matches.
func TestIntegration_AStableAssetIssuesAndRedeemsAndItsSupplyReconciles(t *testing.T) {
	f := newFixture(t)
	a := f.register()
	controller, err := econ.SupplyController(f.ctx, testDB, a.AssetID)
	require.NoError(t, err)
	assert.Equal(t, econ.ControllerStableIssuer, controller)

	c := f.customer()
	f.fund(c, f.cash, 2_000_000)
	f.mustMint(c, a, 1_000_000)
	reconciles := func(want, what string) {
		t.Helper()
		r := f.reconcile(a)
		assert.True(t, r.Matched(), "%s: %+v", what, r)
		assert.Equal(t, want, r.LedgerOutstanding.String(), what)
		held, err := econtest.Holdings(f.ctx, testDB, a.AssetID)
		require.NoError(t, err)
		assert.Equal(t, want, held.String(), "%s: the holdings are what was issued", what)
	}
	reconciles("1000000", "after the mint")

	res, err := f.burn(c, a, 300_000)
	require.NoError(t, err)
	require.Equal(t, BurnRedeemed, res.Request.Status, "failure %q", res.Request.FailureCode)
	assert.Equal(t, "700000", f.wallet(c, a.AssetID))
	reconciles("700000", "after the burn")
}
