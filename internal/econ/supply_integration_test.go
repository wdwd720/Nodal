//go:build integration

package econ_test

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/econ/econtest"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
)

// This file is the external test package: it uses econtest, which imports
// econ. The package's TestMain (integration_test.go) has migrated the
// database before any test runs; these tests open their own pool on it.

var (
	supplyDBOnce sync.Once
	supplyDB     *db.DB
	supplyDBErr  error
)

func database(t *testing.T) *db.DB {
	t.Helper()
	url := os.Getenv("CP_TEST_DATABASE_URL")
	if url == "" || os.Getenv("CP_TEST_MIGRATE_DATABASE_URL") == "" {
		t.Skip("set CP_TEST_DATABASE_URL and CP_TEST_MIGRATE_DATABASE_URL")
	}
	supplyDBOnce.Do(func() {
		supplyDB, supplyDBErr = db.Open(context.Background(), db.Config{URL: url, AppName: "econ-supply-itest", MaxConns: 5})
	})
	require.NoError(t, supplyDBErr)
	return supplyDB
}

func inTx(t *testing.T, fn func(ctx context.Context, tx pgx.Tx) error) {
	t.Helper()
	require.NoError(t, database(t).InTx(context.Background(), db.TxOptions{Isolation: pgx.ReadCommitted}, fn))
}

// supplyWorld is a platform stable asset, a twin of it (which makes the
// stable asset a route's source), a twin of a plain source asset, a stable
// asset reserved in that twin, and the plain assets.
type supplyWorld struct {
	dollar       assets.Asset
	stable       econtest.Stable // plain: no role but its own
	sourceStable econtest.Stable // the source of a route
	twin         econtest.Twin   // plain: no role but its own
	reserveTwin  econtest.Twin   // the reserve of a stable asset
	source       assets.Asset    // a route's source, no controller's
}

func newSupplyWorld(t *testing.T) supplyWorld {
	t.Helper()
	var w supplyWorld
	inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if w.dollar, err = econ.EnsureSandboxDollar(ctx, tx); err != nil {
			return err
		}
		if w.stable, err = econtest.RegisterStable(ctx, tx, w.dollar.ID); err != nil {
			return err
		}
		if w.sourceStable, err = econtest.RegisterStable(ctx, tx, w.dollar.ID); err != nil {
			return err
		}
		if _, err = econtest.RegisterTwin(ctx, tx, w.sourceStable.Asset); err != nil {
			return err
		}
		if w.source, err = econtest.SourceAsset(ctx, tx); err != nil {
			return err
		}
		if w.twin, err = econtest.RegisterTwin(ctx, tx, w.source); err != nil {
			return err
		}
		other, err := econtest.SourceAsset(ctx, tx)
		if err != nil {
			return err
		}
		if w.reserveTwin, err = econtest.RegisterTwin(ctx, tx, other); err != nil {
			return err
		}
		_, err = econtest.RegisterStable(ctx, tx, w.reserveTwin.Asset.ID)
		return err
	})
	return w
}

func requireSupplyRefusal(t *testing.T, err error, asset assets.AssetID, controller string) {
	t.Helper()
	require.Error(t, err)
	e, ok := errs.As(err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, errs.CodeAssetRestricted, e.Code, "%v", err)
	assert.Equal(t, econ.RefusalSupplyControlled, e.Fields["refusal"])
	assert.Equal(t, controller, e.Fields["controller"])
	assert.Equal(t, asset.String(), e.Fields["asset_id"])
}

// TestIntegration_TheSharedCheckNamesEachController: a platform stable asset
// is its STABLE_ISSUER's, a twin its BRIDGE's; a route's source and the
// sandbox dollar are no controller's, however they are used. The refusal is
// ASSET_RESTRICTED with the refusal, controller and asset fields, and names
// what was refused and why.
func TestIntegration_TheSharedCheckNamesEachController(t *testing.T) {
	w := newSupplyWorld(t)
	ctx := context.Background()
	q := database(t)
	for _, tc := range []struct {
		asset      assets.AssetID
		controller string
	}{
		{w.stable.Asset.ID, econ.ControllerStableIssuer}, {w.sourceStable.Asset.ID, econ.ControllerStableIssuer},
		{w.twin.Asset.ID, econ.ControllerBridge}, {w.reserveTwin.Asset.ID, econ.ControllerBridge},
		{w.source.ID, ""}, {w.dollar.ID, ""},
	} {
		got, err := econ.SupplyController(ctx, q, tc.asset)
		require.NoError(t, err)
		assert.Equal(t, tc.controller, got, tc.asset.String())

		err = econ.RefuseSupplyControlled(ctx, q, tc.asset, "test: a use", "the reason")
		if tc.controller == "" {
			assert.NoError(t, err)
			continue
		}
		requireSupplyRefusal(t, err, tc.asset, tc.controller)
		assert.Contains(t, err.Error(), "test: a use is refused")
		assert.Contains(t, err.Error(), "the reason")

		err = econ.RefuseFacilityFunding(ctx, q, ledger.FacilityLendingPool, tc.asset)
		requireSupplyRefusal(t, err, tc.asset, tc.controller)
		assert.Contains(t, err.Error(), "LENDING_POOL")
	}
	all, err := econ.SupplyControllers(ctx, q, []assets.AssetID{w.stable.Asset.ID, w.twin.Asset.ID, w.source.ID, w.dollar.ID, w.stable.Asset.ID})
	require.NoError(t, err)
	assert.Equal(t, map[assets.AssetID]string{w.stable.Asset.ID: econ.ControllerStableIssuer, w.twin.Asset.ID: econ.ControllerBridge}, all)
	none, err := econ.SupplyControllers(ctx, q, nil)
	require.NoError(t, err)
	assert.Empty(t, none)
}

// TestIntegration_SupplyKindsAgreeWithTheDatabase holds every row of the
// per-kind table (supply_kinds_test.go) against the database's own rule,
// cp_supply_controlled_kind_allowed (00763): a kind allowed for every
// supply-controlled asset is allowed for a stable asset and a twin, a refused
// one for neither, and a kind allowed by role only for the asset's own
// controller or for an asset holding the role -- a route's source, a stable
// asset's reserve.
func TestIntegration_SupplyKindsAgreeWithTheDatabase(t *testing.T) {
	w := newSupplyWorld(t)
	// Of the role kinds: which a stable asset and a twin with no role but
	// their own controller's are allowed, and the role that allows it to any
	// other supply-controlled asset.
	byRole := map[ledger.Kind]struct {
		stable, twin bool
		role         string
	}{
		ledger.KindStableIssuance:             {true, false, "reserve"},
		ledger.KindStableRedemption:           {true, false, "reserve"},
		ledger.KindBridgeSourceFinalized:      {false, false, "source"},
		ledger.KindBridgeDestinationFinalized: {false, true, "source"},
		ledger.KindBridgeRefund:               {false, false, "source"},
		ledger.KindPredictionSetIssuance:      {false, false, "collateral"},
		ledger.KindPredictionSetRedemption:    {false, false, "collateral"},
		ledger.KindPredictionPayout:           {false, false, "collateral"},
		ledger.KindPredictionRefund:           {false, false, "collateral"},
	}
	allowed := func(asset assets.AssetID, kind ledger.Kind) bool {
		var ok bool
		require.NoError(t, database(t).QueryRow(context.Background(), `SELECT cp_supply_controlled_kind_allowed($1, $2)`, asset, kind).Scan(&ok))
		return ok
	}
	for _, row := range supplyKindTable() {
		stable, twin := allowed(w.stable.Asset.ID, row.kind), allowed(w.twin.Asset.ID, row.kind)
		switch row.rule {
		case allowedForAll:
			assert.True(t, stable && twin, "%s is allowed for every supply-controlled asset", row.kind)
		case refusedForAll:
			assert.False(t, stable || twin, "%s is refused for every supply-controlled asset", row.kind)
			assert.False(t, allowed(w.sourceStable.Asset.ID, row.kind) || allowed(w.reserveTwin.Asset.ID, row.kind),
				"%s is refused whatever role the asset has", row.kind)
		case allowedByRole:
			want, ok := byRole[row.kind]
			require.True(t, ok, "%s: a role kind needs its expectation here", row.kind)
			assert.Equal(t, want.stable, stable, "%s for a stable asset with no role", row.kind)
			assert.Equal(t, want.twin, twin, "%s for a twin with no role", row.kind)
			switch want.role {
			case "source":
				assert.True(t, allowed(w.sourceStable.Asset.ID, row.kind), "%s for a stable asset that is a route's source", row.kind)
			case "reserve":
				assert.True(t, allowed(w.reserveTwin.Asset.ID, row.kind), "%s for a twin that is a stable asset's reserve", row.kind)
			case "collateral":
				// A prediction market collateralised in a stable asset is the
				// predmarket package's allowed-flow test
				// (TestIntegration_AMarketCollateralisedInAStableAssetSettlesAndSupplyReconciles).
			}
		}
	}
}
