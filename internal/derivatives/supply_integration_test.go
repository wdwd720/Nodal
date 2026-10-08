//go:build integration

package derivatives

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ/econtest"
	"github.com/nodal/controlplane/internal/instruments"
)

// TestIntegration_NoContractSettlesInASupplyControlledAsset (migration 00763):
// a contract settled in a platform stable asset or a bridged twin could post
// its initial margin (DERIVATIVE_MARGIN_MOVE is allowed) and then never pay a
// premium, a variation margin, a funding payment or its final settlement --
// kinds the database refuses for such an asset. OpenPosition refuses it by
// name before the intent is written, and FundClearingHouse refuses capital in
// it (FACILITY_FUNDING). A contract on the stable asset's price, settled in
// another asset, moves no unit of it and opens.
func TestIntegration_NoContractSettlesInASupplyControlledAsset(t *testing.T) {
	f := newFixture(t)
	var s econtest.Supply
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		s, err = econtest.RegisterSupply(ctx, tx)
		return err
	}))
	a := f.account()
	f.fund(a, usdUnits(100_000))
	for _, c := range s.Controlled() {
		perp := f.create(f.termsIn(instruments.TypePerpetual, c.Asset))
		f.mark(perp, 65_000, 65_000)
		err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.svc.OpenPosition(ctx, tx, OpenRequest{IntentRequest: f.req(a, perp), Side: Long, Quantity: q(1)})
			return err
		})
		econtest.RequireRefusal(t, err, c.Asset, c.Controller)
		assert.Zero(t, f.count(`SELECT count(*) FROM economic_intents WHERE account_id = $1`, a), "no intent was written")
		assert.Zero(t, f.count(`SELECT count(*) FROM derivative_positions WHERE instrument_id = $1`, perp))

		key := "itest-" + uuid.NewString()
		err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.svc.FundClearingHouse(ctx, tx, c.Asset, usdUnits(1_000), key)
			return err
		})
		econtest.RequireRefusal(t, err, c.Asset, c.Controller)
		assert.Zero(t, f.count(`SELECT count(*) FROM economic_effects WHERE effect_key = $1`, "deriv:clearing-funding:"+key))
	}

	// The stable asset as the underlying: positions are contracts, never
	// units of it, and every flow is in the settlement asset.
	var spot instruments.InstrumentID
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		ins, err := f.repo.CreateSpotPair(ctx, tx, instruments.SpotPairSpec{
			Base: s.Stable.Asset.ID, Quote: f.usd, Settlement: f.usd, CanonicalName: s.Stable.Asset.Symbol + "/DVUSD " + uuid.NewString(),
			RiskClass: assets.RiskSettlement, Status: assets.StatusActive, ActiveFrom: start.Add(-time.Hour),
		})
		spot = ins.ID
		return err
	}))
	terms := f.termsIn(instruments.TypePerpetual, f.usd)
	terms.UnderlyingInstrumentID = &spot
	pegged := f.create(terms)
	f.mark(pegged, 1, 1)
	out := f.open(a, pegged, Long, 1)
	require.NotNil(t, out.Position)
	assert.Equal(t, f.usd, out.Position.SettlementAssetID)
}
