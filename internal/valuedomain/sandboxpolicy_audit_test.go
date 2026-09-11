package valuedomain

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestAudit_SandboxPolicyPermitsAnOriginTheDocumentSaysItRefuses reproduces
// F-credits-payments-7.
//
// docs/product/CREDIT_ECONOMY.md section 4 states, of SandboxPolicy: "permits
// PURCHASED and the five earning origins once the account reaches PAYOUT_KYC,
// and refuses PROMOTIONAL, REFUND, ADMIN_ADJUSTMENT and PROVIDER_SETTLEMENT
// outright", and then draws the conclusion the whole section rests on: "A
// granted Credit that could leave the system would be the first rule somebody
// copied."
//
// The five earning origins are the five CreditOrigin values whose source is an
// activity inside the product: CREATOR_EARNING, DATA_SALE_EARNING,
// AGENT_SERVICE_EARNING, MARKET_CREATOR_EARNING and MARKET_TRADING_PROCEEDS.
// So the document describes six permitted origins.
//
// SandboxPolicy permits seven. COMPETITION_REWARD -- "a prize or reward from a
// platform competition" (origin.go:50), a grant by every definition the package
// uses, and not EarnedByUser() -- is withdrawable.
func TestAudit_SandboxPolicyPermitsAnOriginTheDocumentSaysItRefuses(t *testing.T) {
	t.Parallel()
	p := SandboxPolicy()

	documented := map[CreditOrigin]bool{
		OriginPurchased:             true,
		OriginCreatorEarning:        true,
		OriginDataSaleEarning:       true,
		OriginAgentServiceEarning:   true,
		OriginMarketCreatorEarning:  true,
		OriginMarketTradingProceeds: true,
	}
	var permitted []CreditOrigin
	for _, o := range AllOrigins() {
		if p.Rule(o).PayoutAllowed {
			permitted = append(permitted, o)
		}
	}

	assert.True(t, p.Rule(OriginCompetitionReward).PayoutAllowed,
		"COMPETITION_REWARD is withdrawable under SandboxPolicy")
	assert.False(t, OriginCompetitionReward.EarnedByUser(),
		"and it is not one of the earning origins the document names")

	for _, o := range permitted {
		assert.True(t, documented[o],
			"SandboxPolicy permits %s, which docs/product/CREDIT_ECONOMY.md section 4 does not list", o)
	}
}
