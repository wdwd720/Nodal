//go:build integration

package stableasset

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// TestProp_IntegrationSupplyNeverExceedsEligibleReserve: over random
// sequences of real mints, burns, reserve price moves and monitor runs --
// through the database, the ledger and the devnet host -- every admitted mint
// leaves issued supply covered by the verified eligible reserve, a
// redemption never breaks coverage that held, the books and the chain never
// disagree, and whenever the monitor finds coverage below 100% the asset is
// paused.
func TestProp_IntegrationSupplyNeverExceedsEligibleReserve(t *testing.T) {
	f := newFixture(t)
	admitted, redeemed := 0, 0
	defer func() {
		require.Positive(t, admitted, "the property must have exercised real issuance")
		require.Positive(t, redeemed, "and real redemptions")
	}()
	rapid.Check(t, func(rt *rapid.T) {
		a := f.register()
		c := f.customer()
		f.fund(c, f.cash, 10_000_000)
		outstanding := a.account(ledger.CodeStableOutstanding, a.AssetID)
		steps := rapid.IntRange(1, 4).Draw(rt, "steps")
		for i := 0; i < steps; i++ {
			switch rapid.IntRange(0, 3).Draw(rt, "op") {
			case 0:
				q := rapid.Int64Range(1, 3_000_000).Draw(rt, "mint")
				res, err := f.mint(c, a, q)
				if err == nil && res.Request.Status == MintIssued {
					admitted++
					rl := f.reserveLedger(a)
					require.True(rt, rl.Covered(), "an admitted mint left supply %s above eligible reserve %s",
						rl.SupplyValue, rl.EligibleReserve)
				}
			case 1:
				held, err := money.ParseQuantity(f.wallet(c, a.AssetID))
				require.NoError(rt, err)
				if !held.IsPositive() {
					continue
				}
				n, err := held.Int64()
				require.NoError(rt, err)
				before := f.reserveLedger(a).Covered()
				res, err := f.burn(c, a, rapid.Int64Range(1, n).Draw(rt, "burn"))
				if err == nil && res.Request.Status == BurnRedeemed {
					redeemed++
					if before {
						require.True(rt, f.reserveLedger(a).Covered(), "a redemption broke coverage")
					}
				}
			case 2:
				f.clk.Advance(time.Second)
				f.price(f.cash, rapid.Int64Range(800_000, 1_100_000).Draw(rt, "cash_price"), 6)
			case 3:
				rep := f.monitor(a)
				if !rep.Reserve.Covered() {
					require.Equal(rt, StatusPaused, f.asset(a).Status, "a shortfall must pause issuance")
				}
			}
			require.Equal(rt, f.balance(outstanding), money.QuantityFromInt64(int64(f.chainSupply(a))).String(),
				"the books and the chain disagree")
		}
	})
}
