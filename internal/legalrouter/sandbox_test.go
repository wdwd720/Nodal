package legalrouter

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/valuedomain"
)

// The sandbox-tier policy validates, permits the internal economy under the
// real capabilities, and treats a payout the way the product needs: a
// verified account is permitted under PAYOUT_RESERVE, an unverified one is
// told verification is the next step, and nothing else is permitted.
func TestSandboxPolicy(t *testing.T) {
	t.Parallel()
	r, err := New(SandboxPolicy())
	require.NoError(t, err)

	key := func(product, verification string) Key {
		return Key{
			Jurisdiction: "UNKNOWN", Provider: "NONE", Rail: "NATIVE_INTERNAL", Product: product, Asset: "NONE",
			AgentAuthority: "NONE", ValueOrigin: "NONE", PayoutMode: "NONE", Compensation: "NONE",
			Verification: verification,
		}
	}
	active := map[valuedomain.CapabilityKey]bool{
		"CREDIT_PURCHASE": true, "MARKETPLACE": true, "NATIVE_ASSET_CREATION": true,
		valuedomain.CapNativeMarketTrading: true, valuedomain.CapPayoutReserve: true,
	}

	for _, product := range []string{ProductCreditPurchase, ProductInternalCommerce, ProductNativeAssetCreate, ProductNativeMarketTrade} {
		d := r.Route(key(product, "NODAL_IDENTITY"), active)
		assert.Equal(t, Allow, d.Outcome, product)
		assert.NotEmpty(t, d.RequiredCapability, "%s: permission is still behind a gate", product)
		assert.True(t, d.CapabilityActive)
		d = r.Route(key(product, "NODAL_IDENTITY"), nil)
		assert.False(t, d.CapabilityActive, "%s: a policy that says yes and a gate that is off must not execute", product)
	}

	verified := r.Route(key(ProductPayout, "PAYOUT_KYC"), active)
	assert.Equal(t, Allow, verified.Outcome)
	assert.Equal(t, valuedomain.CapPayoutReserve, verified.RequiredCapability)
	enhanced := r.Route(key(ProductPayout, "ENHANCED"), active)
	assert.Equal(t, Allow, enhanced.Outcome)

	unverified := r.Route(key(ProductPayout, "NODAL_IDENTITY"), active)
	assert.Equal(t, RequiresVerification, unverified.Outcome, "below PAYOUT_KYC a payout is a next step, not a permission")
	none := r.Route(key(ProductPayout, "NONE"), active)
	assert.Equal(t, RequiresVerification, none.Outcome)

	assert.Equal(t, Deny, r.Route(key(ProductHostedTrade, "ENHANCED"), active).Outcome, "nothing outside the internal economy is permitted")
	assert.Equal(t, Deny, r.Route(key(ProductSelfCustodialTrade, "ENHANCED"), active).Outcome)

	for _, rule := range SandboxPolicy().Rules {
		if rule.Outcome == Allow && rule.Match.Product != ProductSimulation {
			assert.Contains(t, rule.ApprovalReference, "NOT-AN-APPROVAL", "every permission says in words that it is not an approval")
		}
	}
}
