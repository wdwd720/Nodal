package nativemarket

import (
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/money"
)

var positionEpoch = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

// TestProp_PositionQuantityIsAlwaysTheSumOfItsFills drives random buy/sell
// sequences through the Go statement of migration 00772's arithmetic and
// asserts the invariant the table's CHECK enforces, after every step.
//
// The property is not "the numbers look plausible". It is the identity the
// database refuses to store a row without:
//
//	quantity = allocation + bought - sold
//
// plus the two rounding properties a CHECK cannot express: a partial exit never
// removes more basis than it should, and a full exit removes all of it.
func TestProp_PositionQuantityIsAlwaysTheSumOfItsFills(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		alloc := rapid.Int64Range(0, 1_000_000).Draw(rt, "allocation")
		p := Position{
			Quantity:        money.QuantityFromInt64(alloc),
			AllocationUnits: money.QuantityFromInt64(alloc),
		}
		require.True(rt, p.HoldsInvariant(), "a freshly allocated position must hold the invariant")

		steps := rapid.IntRange(1, 24).Draw(rt, "steps")
		for i := 0; i < steps; i++ {
			if rapid.Bool().Draw(rt, "buy") || p.Quantity.Sign() == 0 {
				units := money.QuantityFromInt64(rapid.Int64Range(1, 1_000_000).Draw(rt, "units"))
				credits := money.QuantityFromInt64(rapid.Int64Range(1, 5_000_000).Draw(rt, "credits"))
				fees := money.QuantityFromInt64(rapid.Int64Range(0, 1_000).Draw(rt, "fees"))
				p = p.ApplyBuy(units, credits, fees, positionEpoch)
			} else {
				max, err := p.Quantity.Int64()
				require.NoError(rt, err)
				units := money.QuantityFromInt64(rapid.Int64Range(1, max).Draw(rt, "sold"))
				proceeds := money.QuantityFromInt64(rapid.Int64Range(0, 5_000_000).Draw(rt, "proceeds"))
				fees := money.QuantityFromInt64(rapid.Int64Range(0, 1_000).Draw(rt, "sellfees"))
				before := p
				var serr error
				p, serr = p.ApplySell(units, proceeds, fees, positionEpoch)
				require.NoError(rt, serr)
				// A partial exit leaves the basis PROPORTIONALLY no smaller
				// than the units left: the pool of cost never runs ahead of
				// the pool of units.
				if p.Quantity.Sign() > 0 {
					left := new(big.Rat).SetFrac(p.CostBasisCredits.BigInt(), p.Quantity.BigInt())
					was := new(big.Rat).SetFrac(before.CostBasisCredits.BigInt(), before.Quantity.BigInt())
					assert.GreaterOrEqual(rt, left.Cmp(was), 0,
						"a partial exit must not leave the remaining units cheaper than they were")
				}
			}
			require.True(rt, p.HoldsInvariant(),
				"the invariant broke after step %d: %+v", i, p)
		}

		// A full exit closes the position and takes the whole basis with it.
		if p.Quantity.Sign() > 0 {
			closed, err := p.ApplySell(p.Quantity, money.QuantityFromInt64(1), money.Quantity{}, positionEpoch)
			require.NoError(rt, err)
			assert.True(rt, closed.Quantity.IsZero())
			assert.True(rt, closed.CostBasisCredits.IsZero(),
				"a closed position must carry no basis; migration 00772's CHECK refuses one that does")
			assert.True(rt, closed.HoldsInvariant())
		}
	})
}

// TestPosition_SellMoreThanHeldIsRefused: the read model never writes a
// negative quantity. A disagreement with the ledger must stay visible to the
// reconciliation query rather than being absorbed here.
func TestPosition_SellMoreThanHeldIsRefused(t *testing.T) {
	t.Parallel()
	p := Position{Quantity: q(10), AllocationUnits: q(10)}
	_, err := p.ApplySell(q(11), q(1), money.Quantity{}, positionEpoch)
	require.Error(t, err)
}

// TestPosition_RealisedPnLIsProceedsLessBasisRemoved, worked by hand.
//
// Buy 1,000 units for 2,000 Credits (2 Credits a unit, fees included), then
// sell 400 for 1,000. The basis removed is 400/1000 of 2,000 = 800, so the
// realised gain is 1,000 - 800 = 200 and 1,200 of basis stays with the 600
// units still held.
func TestPosition_RealisedPnLIsProceedsLessBasisRemoved(t *testing.T) {
	t.Parallel()
	p := Position{}.ApplyBuy(q(1_000), q(2_000), q(20), positionEpoch)
	assert.Equal(t, "1000", p.Quantity.String())
	assert.Equal(t, "2000", p.CostBasisCredits.String())

	after, err := p.ApplySell(q(400), q(1_000), q(10), positionEpoch)
	require.NoError(t, err)
	assert.Equal(t, "600", after.Quantity.String())
	assert.Equal(t, "1200", after.CostBasisCredits.String())
	assert.Equal(t, "200", after.RealizedPnLCredits.String())
	assert.Equal(t, "30", after.FeesPaidCredits.String(), "fees are disclosed, never re-applied")

	// A loss is a real outcome and keeps its sign.
	loss, err := after.ApplySell(q(600), q(100), money.Quantity{}, positionEpoch)
	require.NoError(t, err)
	assert.Equal(t, "-900", loss.RealizedPnLCredits.String(), "200 gained, then 1,100 lost")
	assert.True(t, loss.CostBasisCredits.IsZero())
}

// TestPosition_AverageCostAndMarketValue: both are scaled by PriceScale so a
// cost and a price are directly comparable, and both round DOWN so neither
// flatters a portfolio.
func TestPosition_AverageCostAndMarketValue(t *testing.T) {
	t.Parallel()
	// 3 Credits for 2 units is 1.5 Credits a unit at eighteen places.
	p := Position{}.ApplyBuy(q(2), q(3), money.Quantity{}, positionEpoch)
	avg, ok := p.AverageCostCredits()
	require.True(t, ok)
	assert.Equal(t, "1500000000000000000", avg.String())

	closed, err := p.ApplySell(q(2), q(3), money.Quantity{}, positionEpoch)
	require.NoError(t, err)
	_, ok = closed.AverageCostCredits()
	assert.False(t, ok, "a closed position has no average cost; zero would read as free")

	// 7 units at 1.5 Credits is 10.5, which is 10 base units, not 11.
	assert.Equal(t, "10", MarketValueCredits(q(7), qs("1500000000000000000")).String())
	assert.Equal(t, "0", MarketValueCredits(money.Quantity{}, qs("1500000000000000000")).String())
	assert.Equal(t, "0", MarketValueCredits(q(7), money.Quantity{}).String())
}

// TestValue_UnrealisedIsMarketValueLessBasis, with the market marked at its
// own reserves.
func TestValue_UnrealisedIsMarketValueLessBasis(t *testing.T) {
	t.Parallel()
	m := Market{
		ID:     NewMarketID(),
		Status: StatusActive,
		Curve:  Curve{VirtualCreditReserve: q(1_000), InitialAssetReserve: q(1_000)},
	}
	st := State{RealCreditReserve: q(1_000), AssetReserve: q(500)}
	// spot = (1000 + 1000) / 500 = 4 Credits a unit.
	p := Position{}.ApplyBuy(q(100), q(300), money.Quantity{}, positionEpoch)
	v := Value(p, m, st, positionEpoch)

	assert.Equal(t, "4000000000000000000", v.SpotPrice.String())
	assert.Equal(t, "400", v.MarketValue.String())
	assert.Equal(t, "100", v.UnrealizedPnL.String(), "400 of value against 300 of cost")
	assert.Equal(t, "100", v.TotalPnL.String(), "nothing realised yet")
	assert.Equal(t, positionEpoch, v.AsOf)
	assert.Equal(t, StatusActive, v.MarketStatus)

	// Realised P&L adds to the total rather than replacing it.
	p.RealizedPnLCredits = q(-40)
	v = Value(p, m, st, positionEpoch)
	assert.Equal(t, "60", v.TotalPnL.String())
}
