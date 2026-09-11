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
// reproduction for F-credits-payments-1, kept as the regression for F-151.
//
// The assertions that pinned the defect -- "CreditsFor returns the minor-unit
// count unscaled" and the catalogue being unbuyable -- are inverted rather than
// deleted, because the arithmetic they describe is exactly what must not come
// back.
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

	// What the server issues, and what internal/credit/purchase.go writes onto
	// the funding row and then mints as a lot. It was "1000" -- the minor-unit
	// count, unscaled, one base unit per cent.
	require.NotEqual(t, "1000", got.String(),
		"the minor-unit count unscaled is the defect: one base unit per cent")
	require.EqualValues(t, creditAssetDecimals, p.Decimals,
		"the policy prices at the scale the CREDIT asset is registered with, and NewPurchaseService refuses it otherwise")

	// The invariant: the quantity the pricing policy issues must be the
	// promised number of Credits expressed in the CREDIT asset's base units.
	require.Equal(t, promised.String(), got.String(),
		"$10.00 at 100 Credits per dollar must issue 1,000 Credits = 1,000,000,000 base units; "+
			"the policy issues 1,000 base units = 0.001 Credits, short by a factor of 10^%d",
		creditAssetDecimals)
}

// TestAudit_TheSeededCatalogueIsUnbuyableAtTheShippedRate expressed the same
// defect in the units a person would notice, and is inverted here into the
// property it was measuring: the catalogue a deployment seeds has to be
// affordable at the rate that deployment sells at.
//
// scripts/seedeconomy publishes a research note at "300000000" base units,
// which the browser renders as 300.000000 Credits. Under the defect the largest
// purchase the policy permitted -- $10,000 -- bought 1,000,000 base units,
// rendered as 1.000000 Credits, so a customer had to pay $3,000,000 for a
// 300-Credit note the policy would itself have refused to sell them.
func TestAudit_TheSeededCatalogueIsBuyableAtTheShippedRate(t *testing.T) {
	t.Parallel()
	p := DefaultPricingPolicy()

	// The cheapest published product in scripts/seedeconomy's catalogue.
	researchNote, err := money.ParseQuantity("300000000")
	require.NoError(t, err)

	// Three dollars, at 100 Credits per dollar, is 300 Credits.
	threeDollars, err := p.CreditsFor(money.USDFromMinor(300))
	require.NoError(t, err)
	require.Equal(t, researchNote.String(), threeDollars.String(),
		"a 300-Credit product costs three dollars, not three million")

	// And the largest purchase the policy permits buys a million Credits.
	maxBuy, err := p.CreditsFor(money.USDFromMinor(p.MaxAmountMinor))
	require.NoError(t, err)
	require.Equal(t, money.QuantityFromInt64(1_000_000).Mul(oneCreditInBaseUnits()).String(), maxBuy.String(),
		"$10,000 at 100 Credits per dollar is 1,000,000 Credits")
	require.Equal(t, 1, maxBuy.Cmp(researchNote),
		"the maximum permitted purchase covers the cheapest seeded product many times over")
}

// TestAudit_PricingValidateCannotCatchTheScaleError showed why the policy's own
// guard did not fire: Validate only asks whether the minimum purchase issues a
// POSITIVE quantity, and 100 base units is positive. There was no assertion
// anywhere that the quantity was denominated in the CREDIT asset's base units,
// because PricingPolicy never saw the asset.
//
// Inverted. PricingPolicy now carries the scale it prices at, so the arithmetic
// is right here; and the half Validate still cannot check -- that the scale is
// the scale of the asset THIS DEPLOYMENT registered -- is checked where the
// deployment is known, by NewPurchaseService against the assets table
// (TestIntegration_APurchaseServiceRefusesAPolicyAtTheWrongScale).
func TestAudit_PricingValidateSeesTheScaleItPricesAt(t *testing.T) {
	t.Parallel()
	p := DefaultPricingPolicy()
	require.NoError(t, p.Validate())

	min, err := p.CreditsFor(money.USDFromMinor(p.MinAmountMinor)) // $1.00
	require.NoError(t, err)
	require.Equal(t, 1, min.Sign(), "positive, so Validate is satisfied")

	// 100 base units of a six-decimal asset is 0.0001 Credits, and that is what
	// the minimum purchase used to buy. Validate's own sentence -- "it would
	// take money and give nothing" -- was the condition that had in fact
	// occurred, at a scale Validate could not see.
	require.NotEqual(t, "100", min.String(), "100 base units is 0.0001 Credits, not 100 Credits")
	require.Equal(t, 1, min.Cmp(oneCreditInBaseUnits()),
		"the minimum purchase buys a hundred whole Credits")
	require.Equal(t,
		money.QuantityFromInt64(100).Mul(oneCreditInBaseUnits()).String(), min.String(),
		"$1.00 at 100 Credits per dollar is 100 Credits, in base units")
}
