package nativemarket

// Reproductions for the independent adversarial audit of the `markets` area
// (goal §54) that need no database. Each asserts the invariant the area CLAIMS,
// so it FAILS on the audited tree (productization @ b5aa697).

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/money"
)

// --- F-markets-6 -------------------------------------------------------------

// TestAudit_AMarketWhoseSpotTruncatesToZeroIsNotOpenable.
//
// PriceScale is 18 and the spot price is `(V+R)·10^18 / Y`, truncated. Nothing
// bounds Y0 against V: `SupplyModel.Validate` has no ceiling on max_supply,
// `POST /v1/native-assets` accepts any integer string with decimals up to 18,
// and `checkOpeningLiquidity` only bounds V from BELOW (the compiled-in floor
// is 1,000 Credits = 1e9 base units). So an asset of 10 billion tokens at 18
// decimals — Y0 = 1e28 — launched at that floor has a marginal price that
// truncates to ZERO, and stays zero as it trades.
//
// Three §47 controls read that zero and fail OPEN, and the portfolio reads it
// and reports a total loss on a position bought seconds ago:
//
//   - PriceImpactBPS returns 0 when SpotBefore is 0, so the 9,000 bps ceiling
//     never binds — this order's true impact is (x'/x)²−1 = 12,500 bps.
//   - Fill.SlippageBPS returns 0 for the same reason.
//   - applyBreaker returns early when the reference price is 0, so no circuit
//     breaker can ever arm on this market.
//   - MarketValueCredits(quantity, 0) is 0, so GET /v1/me/portfolio reports
//     market_value_credits 0 and unrealized_pnl_credits −(the whole basis).
func TestAudit_AMarketWhoseSpotTruncatesToZeroIsNotOpenable(t *testing.T) {
	t.Parallel()

	// The compiled-in opening-liquidity floor, and a supply an ordinary token
	// would have: 10^10 tokens at 18 decimals.
	v := ConservativeSafetyPolicy().MinOpeningLiquidityCredits
	require.NotNil(t, v)
	require.Equal(t, "1000000000", v.String())
	y0 := qs("10000000000000000000000000000")

	c := Curve{VirtualCreditReserve: *v, InitialAssetReserve: y0}
	require.NoError(t, c.Validate())
	st := State{AssetReserve: y0}

	// checkOpeningLiquidity permits it: the only bound is on V.
	require.NoError(t, checkOpeningLiquidity(ConservativeSafetyPolicy(), CreateRequest{
		VirtualCreditReserve: *v,
	}))

	assert.NotEqual(t, "0", SpotPrice(c, st).String(),
		"a market must not open at a marginal price that truncates to zero at PriceScale %d", PriceScale)

	// 500 Credits into a 1,000-Credit pool: x'/x = 1.5, so the true impact is
	// (1.5)² − 1 = 125% = 12,500 bps, past the 9,000 bps ceiling.
	fill, err := QuoteBuy(c, st, qs("500000000"), Fees{})
	require.NoError(t, err)
	require.True(t, fill.AssetsOut.IsPositive())

	policy := ConservativeSafetyPolicy()
	svc := &Service{}
	serr := svc.checkSafety(policy, Market{Curve: c}, ExecuteRequest{Side: Buy}, fill, accounts.AccountID{})
	assert.Error(t, serr,
		"the price-impact ceiling (%d bps) must refuse an order whose true impact is 12,500 bps; "+
			"PriceImpactBPS reported %d and SlippageBPS reported %d because the spot truncated to zero",
		int(*policy.MaxPriceImpactBPS), int(PriceImpactBPS(fill)), int(fill.SlippageBPS()))

	// And the buyer's own portfolio: units bought for 500,000,000 Credit base
	// units, marked at the post-trade marginal price.
	value := MarketValueCredits(fill.AssetsOut, fill.SpotAfter)
	assert.True(t, value.IsPositive(),
		"a position bought for %s Credit base units must not be marked at %s; "+
			"the portfolio reports unrealized_pnl_credits of -%s on a trade that just happened",
		fill.CreditsIn.String(), value.String(), fill.CreditsIn.String())

	// The circuit breaker cannot arm either: its reference is a recorded
	// spot_price_before, which is this zero.
	assert.NotEqual(t, money.BPS(0), moveBPS(fill.SpotBefore, fill.SpotAfter),
		"the circuit breaker measures a move of zero on a market that just moved 125 per cent")
}
