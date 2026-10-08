//go:build integration

package bridge

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital/buyingpower"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// TestIntegration_TheV2LoaderCountsALegInFlightOnlyAsItsHaircut: buying power
// V2's loader, reading the ledger as the API does, finds a real leg's
// BRIDGE_IN_FLIGHT balance -- posted by that leg's own
// BRIDGE_SOURCE_FINALIZED journal, the only way such a balance can exist
// (BR005) -- values it at its mark and counts all of it as the pending-bridge
// haircut: while in flight it adds gross equity but no eligible collateral.
// Once the release is final on the destination, the in-flight balance and its
// haircut are gone and the units count as the destination wallet's.
func TestIntegration_TheV2LoaderCountsALegInFlightOnlyAsItsHaircut(t *testing.T) {
	f := newFixture(t)
	r := f.route
	c := f.customer()
	f.fund(r, c, 1_000_000_000) // 1,000 source units (6 decimals)

	// The sandbox dollar every mark is quoted in, and both sides of the route
	// marked at par.
	quote, err := assets.NewRepository().Create(f.ctx, testDB, assets.Asset{
		Chain: assets.SandboxChain, Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated,
		Symbol: "BQ" + uuid.NewString()[:6], Name: "bridge v2 quote (sandbox test asset)", Decimals: 6,
		RiskClass: assets.RiskSettlement, Status: assets.StatusActive,
	})
	require.NoError(t, err)
	prices := valuation.NewPriceStore(f.clk)
	mark := func(a assets.AssetID) {
		t.Helper()
		_, err := prices.RecordPrice(f.ctx, testDB, valuation.PriceObservation{AssetID: a, QuoteAssetID: quote.ID,
			Mantissa: money.QuantityFromInt64(1), Scale: 0, Source: "bridge-v2-itest", ObservedAt: f.clk.Now().Add(-time.Second)})
		require.NoError(t, err)
	}
	mark(r.SourceAssetID)
	mark(r.DestinationAssetID)
	loader, err := buyingpower.NewV2Loader(buyingpower.V2LoaderConfig{
		Clock: f.clk, Prices: prices, QuoteAssetID: quote.ID, MaxPriceAge: time.Minute,
	})
	require.NoError(t, err)
	compute := func() buyingpower.V2 {
		t.Helper()
		bp, err := loader.Compute(f.ctx, testDB, "CUSTOMER", c.id.String())
		require.NoError(t, err)
		return bp
	}

	// 400 units leave over the bridge; the destination holds the release back,
	// so once the lock is final the leg is in flight.
	res, err := f.bridge(r, c, 400_000_000, gates.ModeDevnet)
	require.NoError(t, err)
	lid := res.Leg.ID
	f.produce(r.SourceChain, 1)
	require.Equal(t, StateBridgeRequested, f.observe(lid).Status, "a confirmed lock is not final")
	require.NoError(t, f.host.DelayInclusion(f.ctx, r.DestinationChain, 5))
	f.produce(r.SourceChain, itDepth)
	require.Equal(t, StateMessageInFlight, f.observe(lid).Status)
	require.Equal(t, "400000000", f.inFlight(r, c), "the leg's own journal posted its in-flight balance")

	bp := compute()
	assert.Equal(t, money.USDFromMinor(1_000_00), bp.GrossEquity, "600 in the source wallet + 400 in flight, at par: %+v", bp)
	assert.Equal(t, money.USDFromMinor(400_00), bp.PendingBridgeHaircut, "everything in flight is haircut")
	assert.Equal(t, money.USDFromMinor(600_00), bp.EligibleCollateral, "in-flight value is never eligible by default")

	// The release lands and is final: nothing is in flight, and the units are
	// the destination wallet's.
	for i := 0; i < 20 && f.observe(lid).Status != StateReconciled; i++ {
		f.produce(r.DestinationChain, 1)
	}
	require.Equal(t, StateReconciled, f.observe(lid).Status)
	require.Equal(t, "0", f.inFlight(r, c))
	require.Equal(t, "400000000", f.wallet(c, r.DestinationAssetID))

	f.clk.Advance(time.Second)
	mark(r.SourceAssetID)
	mark(r.DestinationAssetID)
	bp = compute()
	assert.True(t, bp.PendingBridgeHaircut.IsZero(), "no leg is in flight: %+v", bp)
	assert.Equal(t, money.USDFromMinor(1_000_00), bp.GrossEquity, "the value was counted once at every step")
	assert.Equal(t, money.USDFromMinor(1_000_00), bp.EligibleCollateral, "final destination value is eligible")
}
