package legalrouter

import "github.com/nodal/controlplane/internal/valuedomain"

// SandboxPolicy is the policy a sandbox tier evaluates: a deployment that is
// not PROD, holds no live provider (config refuses one anywhere else), and has
// declared itself so with CP_API_LEGAL_POLICY=SANDBOX.
//
// It is NOT a legal opinion, and every rule says so in its approval reference.
// It differs from DevelopmentPolicy in one product: a payout is permitted --
// under the PAYOUT_RESERVE capability, which on a sandbox tier can only be a
// SANDBOX gate, and only for an account whose verification is PAYOUT_KYC or
// higher, which on a sandbox tier can only come from the sandbox verification
// provider. Below that level the router answers REQUIRES_VERIFICATION, which
// the compiler records as a next step the user can take rather than a flat
// denial. That is the whole withdrawal journey -- verify, then request
// conversion, then a provider that moves nothing -- exercised by the real
// router, the real compiler and the real gates, on a deployment where every
// value is a rehearsal.
//
// A production deployment cannot load this: config.Validate refuses the name
// in PROD and cmd/api refuses to build the router there, for the same reason
// the development policy is refused in STAGING.
func SandboxPolicy() Policy {
	const ref = "NOT-AN-APPROVAL-SANDBOX-TIER-ONLY"
	allow := func(product string, cap valuedomain.CapabilityKey, why string) Rule {
		return Rule{
			Match:              Key{Product: product},
			Outcome:            Allow,
			ReasonCode:         "SANDBOX_TIER_POLICY",
			Detail:             why + " This is a sandbox-tier policy and is not a legal determination.",
			ApprovalReference:  ref,
			RequiredCapability: cap,
		}
	}
	return Policy{
		Version: "legal-router-v1-sandbox-tier",
		Rules: []Rule{
			{
				Match:             Key{Product: ProductSimulation},
				Outcome:           Allow,
				ReasonCode:        "SIMULATION_HAS_NO_ECONOMIC_SUBSTANCE",
				Detail:            "simulated capital may be used by anyone, anywhere; nothing of value moves",
				ApprovalReference: "PRODUCT-SIM-001",
			},
			allow(ProductCreditPurchase, "CREDIT_PURCHASE",
				"buying Credits is permitted so the sandbox tier can be funded against the provider's test mode."),
			allow(ProductInternalCommerce, "MARKETPLACE",
				"the internal marketplace is permitted so the creator economy can be exercised."),
			allow(ProductNativeAssetCreate, "NATIVE_ASSET_CREATION",
				"creating a native asset is permitted so the creation and moderation flow can be exercised."),
			allow(ProductNativeMarketTrade, valuedomain.CapNativeMarketTrading,
				"trading an internal market is permitted so the curve, the risk gate and the ledger can be exercised."),
			{
				Match: Key{
					Product:      ProductPayout,
					Verification: string(valuedomain.VerificationPayoutKYC) + "," + string(valuedomain.VerificationEnhanced),
				},
				Outcome:    Allow,
				ReasonCode: "SANDBOX_TIER_POLICY",
				Detail: "a payout is permitted for a verified account so the conversion boundary can be exercised " +
					"against a provider that moves nothing. This is a sandbox-tier policy and is not a legal determination.",
				ApprovalReference:  ref,
				RequiredCapability: valuedomain.CapPayoutReserve,
			},
			{
				Match:      Key{Product: ProductPayout},
				Outcome:    RequiresVerification,
				ReasonCode: "PAYOUT_REQUIRES_VERIFICATION",
				Detail: "a payout requires a verified financial profile; verification is the next step, " +
					"and it does not change what the Credits are",
				ApprovalReference: ref,
			},
			{
				Outcome:    Deny,
				ReasonCode: "NO_APPROVAL_ON_RECORD",
				Detail:     "nothing permits this; the absence of a rule is a refusal, not an omission",
			},
		},
	}
}
