package credit

import (
	"math/big"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/money"
)

// creditAssetDecimals is the scale the CREDIT asset is registered with. It is
// declared in scripts/seedeconomy/main.go (Decimals: 6), asserted by
// internal/demo/seed_integration_test.go, documented in
// docs/product/CREDIT_ECONOMY.md ("six decimal places") and hardcoded a fourth
// time in the browser as apps/web/src/lib/credits.ts CREDIT_DECIMALS = 6.
//
// Every other producer and consumer of a Credit figure in this repository uses
// that scale: scripts/seedeconomy grants "25000000000" and calls it
// "25,000 Credits each"; its catalogue prices a research note at "300000000"
// and the browser renders that as 300 Credits. One Credit is 10^6 base units.
const creditAssetDecimals = 6

func oneCreditInBaseUnits() money.Quantity {
	return money.QuantityFromBigInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(creditAssetDecimals), nil))
}

// TestAudit_PricingPolicyIssuesBaseUnitsAsIfACreditHadNoDecimals is the
// reproduction for F-credits-payments-1.
//
// PricingPolicy.CreditsFor returns a money.Quantity, which internal/money
// documents as "an exact, arbitrary-precision integer number of asset base
// units", which openapi.yaml's Quantity schema documents as "Exact asset base
// units as an integer string", and which credit_fundings.credit_quantity and
// credit_lots.quantity both store and then hand to the ledger unscaled.
//
// creditsFor computes minor * CreditsPerMajorUnit / MinorUnitsPerMajorUnit and
// never multiplies by 10^decimals, so the number it produces is a count of
// CREDITS, written into a column that means BASE UNITS.
func TestAudit_PricingPolicyIssuesBaseUnitsAsIfACreditHadNoDecimals(t *testing.T) {
	t.Parallel()
	p := DefaultPricingPolicy()
	require.EqualValues(t, 100, p.CreditsPerMajorUnit,
		"the shipped policy, and the number GET /v1/credits/pricing publishes")

	// What a customer is told: the Buy Credits page renders
	// policy.credits_per_major_unit as "100 Credits per 1 USD"
	// (apps/web/src/pages/credits/BuyCredits.tsx:384), and
	// internal/httpapi/handlers_credits.go:17 states the same intent:
	// "a funding page can show \"$100 buys 10,000 Credits\"".
	//
	// So $10.00 must buy 1,000 Credits, which at six decimals is
	// 1,000 * 10^6 = 1,000,000,000 base units.
	promised := money.QuantityFromInt64(1000).Mul(oneCreditInBaseUnits())
	require.Equal(t, "1000000000", promised.String())

	got, err := p.CreditsFor(money.USDFromMinor(1000)) // $10.00
	require.NoError(t, err)

	// What the server actually issues, and what
	// internal/credit/purchase.go:294 writes onto the funding row and then
	// mints as a lot.
	require.Equal(t, "1000", got.String(),
		"CreditsFor returns the minor-unit count unscaled: one base unit per cent")

	// The defect, stated as the invariant it breaks: the quantity the pricing
	// policy issues must be the promised number of Credits expressed in the
	// CREDIT asset's base units.
	require.Equal(t, promised.String(), got.String(),
		"$10.00 at 100 Credits per dollar must issue 1,000 Credits = 1,000,000,000 base units; "+
			"the policy issues 1,000 base units = 0.001 Credits, short by a factor of 10^%d",
		creditAssetDecimals)
}

// TestAudit_TheSeededCatalogueIsUnbuyableAtTheShippedRate expresses the same
// defect in the units a person would notice.
//
// scripts/seedeconomy publishes a research note at "300000000" base units,
// which the browser renders as 300.000000 Credits. At the rate the pricing
// policy actually applies, a customer must pay $3,000,000 to afford it -- and
// the deployment's own pricing policy caps a single purchase at $10,000.
func TestAudit_TheSeededCatalogueIsUnbuyableAtTheShippedRate(t *testing.T) {
	t.Parallel()
	p := DefaultPricingPolicy()

	// The cheapest published product in scripts/seedeconomy's catalogue.
	researchNote, err := money.ParseQuantity("300000000")
	require.NoError(t, err)

	// The largest purchase the policy permits, $10,000.00.
	maxBuy, err := p.CreditsFor(money.USDFromMinor(p.MaxAmountMinor))
	require.NoError(t, err)
	require.Equal(t, "1000000", maxBuy.String(),
		"$10,000 buys 1,000,000 base units, which the browser renders as 1.000000 Credits")

	require.Equal(t, -1, maxBuy.Cmp(researchNote),
		"the maximum permitted purchase does not cover the cheapest seeded product")
}

// TestAudit_PricingValidateCannotCatchTheScaleError shows why the policy's own
// guard does not fire: Validate only asks whether the minimum purchase issues a
// POSITIVE quantity, and 100 base units is positive. There is no assertion
// anywhere that the quantity is denominated in the CREDIT asset's base units,
// because PricingPolicy never sees the asset.
func TestAudit_PricingValidateCannotCatchTheScaleError(t *testing.T) {
	t.Parallel()
	p := DefaultPricingPolicy()
	require.NoError(t, p.Validate())

	min, err := p.CreditsFor(money.USDFromMinor(p.MinAmountMinor)) // $1.00
	require.NoError(t, err)
	require.Equal(t, "100", min.String())
	require.Equal(t, 1, min.Sign(), "positive, so Validate is satisfied")

	// 100 base units of a six-decimal asset is 0.0001 Credits. Validate's own
	// sentence -- "it would take money and give nothing" -- is the condition
	// that has in fact occurred, at a scale Validate cannot see.
	require.Equal(t, -1, min.Cmp(oneCreditInBaseUnits()),
		"the minimum purchase buys less than one whole Credit")
}
