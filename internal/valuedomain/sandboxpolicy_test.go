package valuedomain

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The sandbox payout policy is a complete, valid policy that never lets
// granted, refunded, adjusted or provider-settled value leave, and requires
// PAYOUT_KYC and the reserve capability for everything it does allow.
func TestSandboxPolicy(t *testing.T) {
	t.Parallel()
	p := SandboxPolicy()
	require.NoError(t, p.Validate())
	assert.NotEqual(t, DefaultPolicyVersion, p.Version)
	assert.Len(t, p.Rules, len(AllOrigins()), "every origin has a rule")

	// Every origin, not a sample. The sample was how COMPETITION_REWARD spent
	// its life withdrawable while the document said six origins were (F-157):
	// a test that names three permitted origins and four closed ones passes
	// whatever the other four do.
	withdrawable := map[CreditOrigin]bool{
		OriginPurchased:             true,
		OriginCreatorEarning:        true,
		OriginDataSaleEarning:       true,
		OriginAgentServiceEarning:   true,
		OriginMarketCreatorEarning:  true,
		OriginMarketTradingProceeds: true,
	}
	require.Len(t, withdrawable, 6,
		"docs/product/CREDIT_ECONOMY.md section 4: PURCHASED and the five earning origins")
	for _, o := range AllOrigins() {
		r := p.Rules[o]
		if !withdrawable[o] {
			assert.False(t, r.PayoutAllowed, "%s must never be withdrawable, sandbox or not", o)
			continue
		}
		assert.True(t, r.PayoutAllowed, o)
		assert.Equal(t, CapPayoutReserve, r.RequiredCapability, o)
		assert.Equal(t, VerificationPayoutKYC, r.RequiredVerification, o)
	}
	// The default is still the default: nothing here touches it.
	for o, r := range DefaultPolicy().Rules {
		assert.False(t, r.PayoutAllowed, o)
	}
}
