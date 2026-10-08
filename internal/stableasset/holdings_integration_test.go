//go:build integration

package stableasset

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// TestIntegration_SupplyReconcilesWhereverTheUnitsAreHeld: a stable asset's
// units are outstanding supply wherever they sit -- here posted as margin
// collateral, which takes them out of the customer's wallet. The supply
// reconciliation counts every holding (ledger.HoldingCodes), so it still
// matches; it once summed customer wallets only and reported a material
// mismatch the moment units were bridged or posted as collateral.
func TestIntegration_SupplyReconcilesWhereverTheUnitsAreHeld(t *testing.T) {
	f := newFixture(t)
	a := f.register()
	c := f.customer()
	f.fund(c, f.cash, 2_000_000)
	f.mustMint(c, a, 1_000_000)

	q := money.QuantityFromInt64(400_000)
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := f.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindMarginCollateralMove, IdempotencyKey: "itest-collateral:" + ref,
			Reference: ledger.FinancialEventReference{Type: "itest_collateral_move", ID: ref}, EffectiveAt: f.clk.Now(),
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(c.id, ledger.CodeMarginCollateral, a.AssetID), Side: ledger.Debit, Quantity: q},
				{Account: ledger.CustomerAccount(c.id, ledger.CodeWallet, a.AssetID), Side: ledger.Credit, Quantity: q},
			},
		})
		return err
	}))

	r := f.reconcile(a)
	assert.Equal(t, "600000", r.CustomerWalletSum.String())
	assert.Equal(t, "400000", r.HeldElsewhere.String(), "the collateral is counted as held")
	assert.True(t, r.WalletsMatch)
	assert.True(t, r.Matched(), "%+v", r)
}
