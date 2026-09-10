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

	for _, o := range []CreditOrigin{OriginPromotional, OriginRefund, OriginAdminAdjustment, OriginProviderSettlement} {
		assert.False(t, p.Rules[o].PayoutAllowed, "%s must never be withdrawable, sandbox or not", o)
	}
	for _, o := range []CreditOrigin{OriginPurchased, OriginMarketTradingProceeds, OriginCreatorEarning} {
		r := p.Rules[o]
		assert.True(t, r.PayoutAllowed, o)
		assert.Equal(t, CapPayoutReserve, r.RequiredCapability, o)
		assert.Equal(t, VerificationPayoutKYC, r.RequiredVerification, o)
	}
	// The default is still the default: nothing here touches it.
	for o, r := range DefaultPolicy().Rules {
		assert.False(t, r.PayoutAllowed, o)
	}
}
