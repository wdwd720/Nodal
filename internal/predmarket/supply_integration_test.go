//go:build integration

package predmarket

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/econ/econtest"
	"github.com/nodal/controlplane/internal/money"
)

// TestIntegration_AMarketCollateralisedInAStableAssetSettlesAndSupplyReconciles:
// a flow 00763 allows and the guards must leave alone. A prediction market
// whose collateral is a platform stable asset escrows, redeems, pays out and
// refunds it in its own kinds (PREDICTION_SET_ISSUANCE,
// PREDICTION_SET_REDEMPTION, PREDICTION_PAYOUT, PREDICTION_REFUND -- allowed
// for an asset that is a market's collateral), and a share trade moves it
// between customers (SIM_TRADE). Every one of them posts; the market's escrow
// identity holds, and the stable asset's holdings still equal what its issuer
// issued.
func TestIntegration_AMarketCollateralisedInAStableAssetSettlesAndSupplyReconciles(t *testing.T) {
	f := newFixture(t)
	var s econtest.Supply
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		s, err = econtest.RegisterSupply(ctx, tx)
		return err
	}))
	f.collateral = s.Stable.Asset.ID
	issued := money.Quantity{}
	holder := func(sets int64) accounts.AccountID {
		t.Helper()
		acct := f.customer(0)
		q := units(sets)
		f.fund(acct, s.Dollar.ID, sets*shareUnitBaseUnits)
		require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.ledger.Post(ctx, tx, s.Stable.Issuance(acct, q, uuid.NewString(), f.clk.Now()))
			return err
		}))
		issued = issued.Add(q)
		return acct
	}
	reconciles := func(what string) {
		t.Helper()
		held, err := econtest.Holdings(f.ctx, testDB, s.Stable.Asset.ID)
		require.NoError(t, err)
		outstanding, err := s.Stable.Outstanding(f.ctx, testDB, f.ledger)
		require.NoError(t, err)
		assert.Equal(t, issued.String(), outstanding.String(), what)
		assert.True(t, held.Equal(outstanding), "%s: holdings %s, outstanding %s", what, held, outstanding)
	}

	a, b := holder(20), holder(20)
	m := f.openMarket("YES", "NO")
	assert.Equal(t, s.Stable.Asset.ID, m.CollateralAssetID)
	f.issue(m, a, 6)
	f.issue(m, b, 2)
	f.trade(a, b, m.Outcomes[0].ShareAssetID, 5_000_000, 3_000_000)
	_, err := f.redeem(m, b, 1)
	require.NoError(t, err, "a complete set redeems for collateral")
	reconciles("after issuance, a trade and a redemption")
	f.resolve(m, "YES")
	out, err := f.trySettle(m, f.third)
	require.NoError(t, err)
	assert.Equal(t, "7000000", out.Settlement.TotalPaid.String(), "seven winning shares pay seven units of the stable asset")
	f.requireConserved(m, money.Quantity{})
	reconciles("after the payout")

	voided := f.openMarket("UP", "DOWN")
	f.issue(voided, a, 3)
	f.closeMarket(voided)
	refund, err := f.tryVoid(f.get(voided), f.resolver)
	require.NoError(t, err)
	assert.Equal(t, "3000000", refund.Settlement.TotalPaid.String(), "a binary void refunds the escrow exactly")
	f.requireConserved(voided, money.Quantity{})
	reconciles("after the refund")
}
