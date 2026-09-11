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
// SandboxPolicy permitted seven. COMPETITION_REWARD -- "a prize or reward from
// a platform competition" (origin.go), a grant by every definition the package
// uses, and not EarnedByUser() -- was withdrawable. D-095 marks it closed, and
// the assertion that pinned it is inverted below rather than deleted.
func TestAudit_SandboxPolicyPermitsOnlyTheOriginsTheDocumentLists(t *testing.T) {
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

	assert.False(t, p.Rule(OriginCompetitionReward).PayoutAllowed,
		"COMPETITION_REWARD was withdrawable under SandboxPolicy and is a grant")
	assert.False(t, OriginCompetitionReward.EarnedByUser(),
		"and it is not one of the earning origins the document names")

	for _, o := range permitted {
		assert.True(t, documented[o],
			"SandboxPolicy permits %s, which docs/product/CREDIT_ECONOMY.md section 4 does not list", o)
	}
	assert.Len(t, permitted, len(documented),
		"the document lists six permitted origins and the policy must permit exactly those")
}
