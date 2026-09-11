package valuedomain

// SandboxPolicyVersion identifies the payout policy a sandbox tier runs under.
const SandboxPolicyVersion = "payout-policy-v1-sandbox-tier"

// SandboxPolicy is the payout policy of a sandbox tier: a deployment that is
// not PROD and has declared itself so. It exists so the withdrawal journey can
// be exercised end to end -- eligibility computed per lot, verification
// required, a capability required, a provider that moves nothing -- with the
// real engine and the real rules.
//
// It is NOT a product or legal decision about which origins may be paid out.
// It is the shape one would have: value a person paid for or earned inside
// the economy may be withdrawn once verified; value that was granted, refunded,
// adjusted or settled by a provider may not. Counsel and the provider decide
// the real policy, and that policy is a new version persisted through the
// approval path, never an edit here. DefaultPolicy remains what every
// deployment runs under until then, including PROD, which config.Validate
// refuses to point at this one.
func SandboxPolicy() Policy {
	withdrawable := OriginRule{
		PayoutAllowed:        true,
		RequiredCapability:   CapPayoutReserve,
		RequiredVerification: VerificationPayoutKYC,
		MinHoldDays:          0,
	}
	closed := OriginRule{PayoutAllowed: false, RequiredVerification: VerificationNone}
	return Policy{
		Version: SandboxPolicyVersion,
		Rules: map[CreditOrigin]OriginRule{
			OriginPurchased:             withdrawable,
			OriginCreatorEarning:        withdrawable,
			OriginDataSaleEarning:       withdrawable,
			OriginAgentServiceEarning:   withdrawable,
			OriginMarketCreatorEarning:  withdrawable,
			OriginMarketTradingProceeds: withdrawable,
			// Granted, refunded, adjusted or provider-settled value is never
			// withdrawable, even in a sandbox: a promotional grant that could
			// leave the system would be the first rule somebody copied.
			OriginPromotional:        closed,
			OriginRefund:             closed,
			OriginAdminAdjustment:    closed,
			OriginProviderSettlement: closed,
			// A competition prize is a grant (D-095, F-157). Nobody paid for
			// it and nobody earned it -- COMPETITION_REWARD is not
			// EarnedByUser(), and origin.go calls it "a prize or reward from a
			// platform competition" -- so it belongs with the grants and not
			// with the six origins docs/product/CREDIT_ECONOMY.md section 4
			// lists. It was withdrawable here, which is the rule above stated
			// and then broken one line later: a platform that can mint prizes
			// and let them leave has a payout path whose only gate is a
			// competition it runs itself.
			OriginCompetitionReward: closed,
		},
	}
}
