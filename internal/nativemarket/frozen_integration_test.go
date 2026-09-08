//go:build integration

package nativemarket

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/nativeasset"
)

// A market that trades is a market whose economics are frozen.
//
// Two different columns decided two different things. `cp_native_market_apply_fill`
// accepts a fill when `status IN ('ACTIVE','CLOSE_ONLY')`, while
// `cp_native_market_curve_frozen` only starts guarding once `activated_at` is
// set — and it returns early, unconditionally, when that column is NULL. So a
// row with an ACTIVE status and a NULL activated_at would trade while its curve
// and both fee rates stayed editable: the constant product and the NM005 supply
// ceiling rewritable between fills (F-53).
//
// The service never writes that row — `SetStatus` sets both in one UPDATE — so
// this is the F-49 shape: an invariant that holds because the service is
// careful, in a database meant to hold it whatever the caller is.
//
// The state has to be built from a market that has NEVER been activated. Taking
// `activated_at` away from a live market is refused by the freeze trigger
// itself (NM003), which runs BEFORE the constraint — so a test written that way
// would pass on the trigger and prove nothing about the CHECK. That is the
// mistake this test was written with first.

func TestIntegration_AMarketCannotBecomeTradableWithoutFreezingItsEconomics(t *testing.T) {
	f := newFixture(t)
	marketID, assetID := f.unactivatedMarket(t)

	// The row the constraint exists to refuse: tradable, with nothing frozen.
	// cp_native_market_curve_frozen returns early here because activated_at is
	// NULL, so the CHECK is the only thing standing in the way.
	err := testMigrate.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE native_markets SET status = 'ACTIVE' WHERE id = $1`, marketID)
		return e
	})
	require.Error(t, err, "a market may not become tradable with its curve still editable")
	assert.Contains(t, err.Error(), "native_markets_tradable_is_frozen")

	// CLOSE_ONLY trades too — sells only, but a sell is priced by the same
	// curve, so it is exactly as much a reason to have frozen it.
	err = testMigrate.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE native_markets SET status = 'CLOSE_ONLY' WHERE id = $1`, marketID)
		return e
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "native_markets_tradable_is_frozen")

	// A real launch is still accepted. This goes through the service rather
	// than a hand-written UPDATE, because a bare status change is refused by
	// the AU001 audit binding as well -- it needs a transition row in the same
	// transaction. Between them the two controls say: a market becomes tradable
	// only by an act that is both recorded and frozen.
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, e := f.svc.SetStatus(ctx, tx, marketID, StatusActive, "launching for the freeze test")
			return e
		}), "a constraint that refused a real launch would have broken every one of them")

	var activatedAt *string
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT activated_at::text FROM native_markets WHERE id = $1`, marketID).Scan(&activatedAt))
	assert.NotNil(t, activatedAt, "the launch must have frozen the curve on its way past")

	// The asset half guards supply rather than price, and answers the same way.
	err = testMigrate.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE native_assets SET status = 'ACTIVE' WHERE asset_id = $1`, assetID)
		return e
	})
	require.Error(t, err, "an asset may not become tradable with its supply still editable")
	assert.Contains(t, err.Error(), "native_assets_tradable_is_frozen")
}

// unactivatedMarket builds an asset and a market that have never been live:
// PENDING, with no activation instant and no economics lock. That is the state
// every market passes through, and the only one from which the forbidden row
// can be constructed.
func (f *fixture) unactivatedMarket(t *testing.T) (MarketID, assets.AssetID) {
	t.Helper()
	suffix := uuid.NewString()[:6]
	var (
		marketID MarketID
		assetID  assets.AssetID
	)
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		a, _, err := f.assetSv.CreateDraft(ctx, tx, nativeasset.CreateRequest{
			CreatorAccountID: f.creator,
			Name:             "Unlaunched " + suffix,
			Symbol:           "UL" + suffix[:4],
			Description:      "an asset that has never been live",
			Supply: nativeasset.SupplyModel{
				MaxSupply:         qs("1000000000000000"),
				CreatorAllocation: qs("100000000000000"),
			},
		})
		if err != nil {
			return err
		}
		assetID = a.AssetID
		m, err := f.svc.Create(ctx, tx, CreateRequest{
			AssetID: a.AssetID, CreditAssetID: f.creditAsset, CreatorID: f.creator,
			PoolSupply: a.Supply.PoolSupply(), CreatorAllocation: a.Supply.CreatorAllocation,
			VirtualCreditReserve: qs("30000000000"),
			Fees:                 Fees{PlatformBPS: 100, CreatorBPS: 50},
			IdempotencyKey:       "unlaunched-" + suffix,
			EffectiveAt:          f.clk.Now(),
		})
		if err != nil {
			return err
		}
		marketID = m.ID
		return nil
	}))
	return marketID, assetID
}
