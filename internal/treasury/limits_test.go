package treasury

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
)

func limitFixture() (Policy, assets.AssetID, instruments.InstrumentID) {
	p := samplePolicy()
	return p, p.MaxInventory[0].Asset, p.MaxInstrumentExposure[0].Instrument
}

func held(v int64) *money.Quantity { x := q(v); return &x }

func TestEvaluateLimits_EachLimitAndItsCode(t *testing.T) {
	p, asset, inst := limitFixture()
	base := LimitRequest{
		RiskIncreasing: true, Asset: asset, Quantity: q(1_000_000), Notional: q(100_000_000), Instrument: &inst,
		Exposure: Exposure{Held: q(1_000_000), HeldValue: held(100_000_000)},
	}
	ok := EvaluateLimits(p, base)
	assert.Empty(t, ok.Codes)
	assert.Equal(t, "2000000", ok.InventoryAfter.String())

	cases := map[string]struct {
		mutate func(r *LimitRequest)
		code   string
	}{
		"daily loss past the limit": {func(r *LimitRequest) { r.Loss.RealizedToday = q(-50_000_001) }, ReasonDailyLossLimit},
		"open losses count in full": {func(r *LimitRequest) { r.Loss.OpenLosses = q(-30_000_000); r.Loss.RealizedToday = q(-20_000_001) }, ReasonDailyLossLimit},
		"a missing mark":            {func(r *LimitRequest) { r.Loss.MissingMarks = []assets.AssetID{assets.NewAssetID()} }, ReasonMarkUnavailable},
		"inventory over the cap":    {func(r *LimitRequest) { r.Exposure.WorkingBuys = q(3_000_001) }, ReasonInventoryCap},
		"an asset with no cap":      {func(r *LimitRequest) { r.Asset = assets.NewAssetID() }, ReasonAssetNotPermitted},
		"exposure over the cap":     {func(r *LimitRequest) { r.Exposure.InstrumentWorkingBuys = q(900_000_001) }, ReasonInstrumentExposure},
		"an instrument with no cap": {func(r *LimitRequest) { i := instruments.NewInstrumentID(); r.Instrument = &i }, ReasonInstrumentNotPermitted},
		"held value without a mark": {func(r *LimitRequest) { r.Exposure.HeldValue = nil }, ReasonMarkUnavailable},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := base
			c.mutate(&r)
			ev := EvaluateLimits(p, r)
			assert.Contains(t, ev.Codes, c.code)
			assert.NotEmpty(t, ev.Breakers)
			// The same request reducing risk is never bound by a limit.
			r.RiskIncreasing = false
			assert.Empty(t, EvaluateLimits(p, r).Codes)
		})
	}
	// Exactly at the limits is allowed.
	edge := base
	edge.Loss.RealizedToday = q(-50_000_000)
	edge.Exposure.WorkingBuys = q(3_000_000)
	assert.Empty(t, EvaluateLimits(p, edge).Codes)
}

// TestProp_LimitsAreMonotonic: asking for more never turns a refusal into an
// acceptance, and a larger loss never does either. A limit that relaxed with
// size would let a large order through where a small one is stopped.
func TestProp_LimitsAreMonotonic(t *testing.T) {
	p, asset, inst := limitFixture()
	rapid.Check(t, func(t *rapid.T) {
		r := LimitRequest{
			RiskIncreasing: true, Asset: asset, Instrument: &inst,
			Quantity: q(rapid.Int64Range(1, 10_000_000).Draw(t, "qty")),
			Notional: q(rapid.Int64Range(0, 2_000_000_000).Draw(t, "notional")),
			Exposure: Exposure{
				Held: q(rapid.Int64Range(0, 6_000_000).Draw(t, "held")), WorkingBuys: q(rapid.Int64Range(0, 6_000_000).Draw(t, "working")),
				InstrumentWorkingBuys: q(rapid.Int64Range(0, 1_000_000_000).Draw(t, "instWorking")),
				HeldValue:             held(rapid.Int64Range(0, 1_000_000_000).Draw(t, "heldValue")),
			},
			Loss: LossState{RealizedToday: q(rapid.Int64Range(-100_000_000, 100_000_000).Draw(t, "realized")),
				OpenLosses: q(-rapid.Int64Range(0, 100_000_000).Draw(t, "open"))},
		}
		before := EvaluateLimits(p, r)
		bigger := r
		bigger.Quantity = r.Quantity.Add(q(rapid.Int64Range(0, 5_000_000).Draw(t, "moreQty")))
		bigger.Notional = r.Notional.Add(q(rapid.Int64Range(0, 1_000_000_000).Draw(t, "moreNotional")))
		bigger.Loss.OpenLosses = r.Loss.OpenLosses.Sub(q(rapid.Int64Range(0, 50_000_000).Draw(t, "moreLoss")))
		after := EvaluateLimits(p, bigger)
		for _, c := range before.Codes {
			if !contains(after.Codes, c) {
				t.Fatalf("code %s vanished when the request grew: %v -> %v", c, before.Codes, after.Codes)
			}
		}
		// Deterministic: the same request, the same evaluation.
		assert.Equal(t, before, EvaluateLimits(p, r))
	})
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
