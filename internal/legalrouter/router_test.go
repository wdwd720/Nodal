package legalrouter

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/agentauthority"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// fullKey is a completely specified query. Every test starts from one and
// changes the dimension under test, so a failure names the dimension.
func fullKey() Key {
	return Key{
		Jurisdiction:   "US_CA",
		Provider:       "none",
		Rail:           string(valuedomain.RailNativeInternal),
		Product:        ProductNativeMarketTrade,
		Asset:          "DOGGU",
		AgentAuthority: agentauthority.LevelRecommendation.Name(),
		ValueOrigin:    string(valuedomain.OriginPurchased),
		PayoutMode:     PayoutModeNone,
		Compensation:   CompensationFlatFee,
		Verification:   string(valuedomain.VerificationNodalIdentity),
	}
}

func mustRouter(t *testing.T, p Policy) *Router {
	t.Helper()
	r, err := New(p)
	require.NoError(t, err)
	return r
}

// ---------------------------------------------------------------------------
// The conservative default
// ---------------------------------------------------------------------------

func TestConservativePolicy_IsValidAndDeniesEverythingInteresting(t *testing.T) {
	r := mustRouter(t, ConservativePolicy())

	for _, product := range []string{
		ProductNativeMarketTrade, ProductNativeAssetCreate, ProductCreditPurchase,
		ProductInternalCommerce, ProductPayout, ProductHostedTrade, ProductSelfCustodialTrade,
	} {
		k := fullKey()
		k.Product = product
		d := r.Route(k, nil)
		require.False(t, d.Permits(),
			"a fresh deployment must not permit %s", product)
		require.NotEmpty(t, d.ReasonCode)
	}
}

func TestConservativePolicy_PermitsSimulationForAnyone(t *testing.T) {
	r := mustRouter(t, ConservativePolicy())
	k := fullKey()
	k.Product = ProductSimulation
	k.Jurisdiction = "SOMEWHERE_ELSE"
	k.Verification = string(valuedomain.VerificationNone)

	d := r.Route(k, nil)
	require.True(t, d.Permits(),
		"the whole agent platform must be usable before anyone connects capital (JOURNEY A)")
	require.Equal(t, Allow, d.Outcome)
	require.NotEmpty(t, d.ApprovalReference)
}

func TestConservativePolicy_AssetCreationNeedsAPerson(t *testing.T) {
	r := mustRouter(t, ConservativePolicy())
	k := fullKey()
	k.Product = ProductNativeAssetCreate
	k.AgentAuthority = agentauthority.LevelRecommendation.Name()

	d := r.Route(k, nil)
	require.Equal(t, RequiresUserConfirmation, d.Outcome,
		"publishing under a creator's name is a user act, whatever prepared it")
	require.False(t, d.Permits(), "REQUIRES_USER_CONFIRMATION is not permission")
}

// ---------------------------------------------------------------------------
// Policy validation
// ---------------------------------------------------------------------------

func TestPolicy_MustEndWithACatchAllDeny(t *testing.T) {
	p := ConservativePolicy()
	p.Rules = p.Rules[:len(p.Rules)-1]
	err := p.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "not safe to evaluate")

	// A final rule that is a denial but not a catch-all is equally unsafe.
	p = ConservativePolicy()
	p.Rules[len(p.Rules)-1].Match = Key{Jurisdiction: "US_CA"}
	require.Error(t, p.Validate())
}

func TestPolicy_APermissionMustNameItsApproval(t *testing.T) {
	p := ConservativePolicy()
	p.Rules[0].ApprovalReference = ""
	err := p.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "not safe to evaluate")
}

// TestPolicy_AnAllowMustRequireACapability is the rule that stops a policy row
// from being the only thing between a user and real value.
func TestPolicy_AnAllowMustRequireACapability(t *testing.T) {
	p := Policy{
		Version: "test",
		Rules: []Rule{
			{
				Match: Key{Product: ProductPayout}, Outcome: Allow,
				ReasonCode: "SURE_WHY_NOT", ApprovalReference: "LEGAL-001",
			},
			{Outcome: Deny, ReasonCode: "DEFAULT"},
		},
	}
	err := p.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "not safe to evaluate")

	// With a capability named, it validates.
	p.Rules[0].RequiredCapability = "PAYOUT_CREATOR_EARNINGS"
	require.NoError(t, p.Validate())

	// Simulation is the one exception, because nothing of value moves.
	sim := Policy{
		Version: "test",
		Rules: []Rule{
			{
				Match: Key{Product: ProductSimulation}, Outcome: Allow,
				ReasonCode: "SIM", ApprovalReference: "PRODUCT-SIM-001",
			},
			{Outcome: Deny, ReasonCode: "DEFAULT"},
		},
	}
	require.NoError(t, sim.Validate())
}

func TestPolicy_RejectsUnversionedEmptyAndUnknownOutcomes(t *testing.T) {
	require.Error(t, Policy{Rules: ConservativePolicy().Rules}.Validate())
	require.Error(t, Policy{Version: "v"}.Validate())

	p := ConservativePolicy()
	p.Rules[0].Outcome = "MAYBE"
	require.Error(t, p.Validate())

	p = ConservativePolicy()
	p.Rules[0].ReasonCode = ""
	require.Error(t, p.Validate())
}

func TestNew_RefusesAnUnsafePolicy(t *testing.T) {
	p := ConservativePolicy()
	p.Rules = p.Rules[:1]
	_, err := New(p)
	require.Error(t, err, "a router must never be constructed over a policy that can fall through")
}

// ---------------------------------------------------------------------------
// Matching
// ---------------------------------------------------------------------------

func approvedPayoutPolicy() Policy {
	return Policy{
		Version: "test-approved-creator-payout",
		Rules: []Rule{
			{
				Match: Key{
					Jurisdiction: "US_CA,US_NY",
					Product:      ProductPayout,
					ValueOrigin:  string(valuedomain.OriginCreatorEarning),
					PayoutMode:   PayoutModePartnerFiat,
					Verification: string(valuedomain.VerificationPayoutKYC),
				},
				Outcome:            Allow,
				ReasonCode:         "CREATOR_EARNINGS_PAYOUT_APPROVED",
				RequiredCapability: "PAYOUT_CREATOR_EARNINGS",
				ApprovalReference:  "LEGAL-CA-NY-2026-014",
				Detail:             "creator earnings may be paid to a KYC-verified user in these states",
			},
			{
				Match: Key{
					Product:      ProductPayout,
					ValueOrigin:  string(valuedomain.OriginCreatorEarning),
					Verification: string(valuedomain.VerificationNodalIdentity),
				},
				Outcome:           RequiresVerification,
				ReasonCode:        "PAYOUT_NEEDS_KYC",
				ApprovalReference: "LEGAL-CA-NY-2026-014",
				Detail:            "verify your identity to withdraw creator earnings",
			},
			{
				Match:      Key{Product: ProductPayout, ValueOrigin: string(valuedomain.OriginMarketTradingProceeds)},
				Outcome:    Deny,
				ReasonCode: "TRADING_PROCEEDS_NOT_APPROVED",
				Detail:     "proceeds of speculative internal trading are not approved for payout",
			},
			{Outcome: Deny, ReasonCode: "NO_APPROVAL_ON_RECORD"},
		},
	}
}

func payoutKey() Key {
	k := fullKey()
	k.Product = ProductPayout
	k.ValueOrigin = string(valuedomain.OriginCreatorEarning)
	k.PayoutMode = PayoutModePartnerFiat
	k.Verification = string(valuedomain.VerificationPayoutKYC)
	k.Provider = "partner-a"
	return k
}

func TestRoute_FirstMatchWinsAndIsTraceable(t *testing.T) {
	r := mustRouter(t, approvedPayoutPolicy())
	caps := map[valuedomain.CapabilityKey]bool{"PAYOUT_CREATOR_EARNINGS": true}

	d := r.Route(payoutKey(), caps)
	require.True(t, d.Permits())
	require.Equal(t, 0, d.RuleIndex, "an answer must be traceable to a line of policy")
	require.Equal(t, "LEGAL-CA-NY-2026-014", d.ApprovalReference)
	require.Equal(t, "test-approved-creator-payout", d.PolicyVersion)
	require.NotEmpty(t, d.PolicyHash)
	require.True(t, d.CapabilityActive)
}

func TestRoute_ACommaSeparatedSetMatchesAnyMember(t *testing.T) {
	r := mustRouter(t, approvedPayoutPolicy())
	caps := map[valuedomain.CapabilityKey]bool{"PAYOUT_CREATOR_EARNINGS": true}

	for _, state := range []string{"US_CA", "US_NY"} {
		k := payoutKey()
		k.Jurisdiction = state
		require.True(t, r.Route(k, caps).Permits(), "%s is in the approved set", state)
	}
	k := payoutKey()
	k.Jurisdiction = "US_TX"
	d := r.Route(k, caps)
	require.False(t, d.Permits(), "a state outside the approved set is not approved")
	require.Equal(t, "NO_APPROVAL_ON_RECORD", d.ReasonCode)
}

// TestRoute_PolicySaysYesAndTheGateSaysNo is the interaction that matters most:
// the policy is the standing position and the gate is whether it is switched on
// today, and both have to agree.
func TestRoute_PolicySaysYesAndTheGateSaysNo(t *testing.T) {
	r := mustRouter(t, approvedPayoutPolicy())

	d := r.Route(payoutKey(), nil)
	require.False(t, d.Permits())
	require.Equal(t, Deny, d.Outcome)
	require.Equal(t, "CAPABILITY_NOT_ACTIVE", d.ReasonCode)
	require.False(t, d.CapabilityActive)
	require.Equal(t, valuedomain.CapabilityKey("PAYOUT_CREATOR_EARNINGS"), d.RequiredCapability,
		"the refusal must name what would have to be turned on")
	require.Equal(t, 0, d.RuleIndex,
		"the matched rule is still reported, so an operator sees which of policy and gate refused")
}

func TestRoute_DistinguishesUnverifiedFromForbidden(t *testing.T) {
	r := mustRouter(t, approvedPayoutPolicy())
	caps := map[valuedomain.CapabilityKey]bool{"PAYOUT_CREATOR_EARNINGS": true}

	unverified := payoutKey()
	unverified.Verification = string(valuedomain.VerificationNodalIdentity)
	d := r.Route(unverified, caps)
	require.Equal(t, RequiresVerification, d.Outcome, "this user has a next step")
	require.False(t, d.Permits())

	speculative := payoutKey()
	speculative.ValueOrigin = string(valuedomain.OriginMarketTradingProceeds)
	d = r.Route(speculative, caps)
	require.Equal(t, Deny, d.Outcome, "this one does not")
	require.Equal(t, "TRADING_PROCEEDS_NOT_APPROVED", d.ReasonCode)
}

func TestRoute_AMalformedKeyIsRefusedRatherThanGuessed(t *testing.T) {
	r := mustRouter(t, approvedPayoutPolicy())

	k := payoutKey()
	k.Jurisdiction = ""
	d := r.Route(k, nil)
	require.False(t, d.Permits())
	require.Equal(t, "MALFORMED_KEY", d.ReasonCode)
	require.Equal(t, -1, d.RuleIndex)
	require.Contains(t, d.Detail, "jurisdiction")

	k = payoutKey()
	k.Asset = Wildcard
	d = r.Route(k, nil)
	require.False(t, d.Permits())
	require.Equal(t, "MALFORMED_KEY", d.ReasonCode,
		"a query may not wildcard a dimension; only a rule may")
}

func TestRoute_IsDeterministic(t *testing.T) {
	r := mustRouter(t, approvedPayoutPolicy())
	caps := map[valuedomain.CapabilityKey]bool{"PAYOUT_CREATOR_EARNINGS": true}
	first := r.Route(payoutKey(), caps)
	for i := 0; i < 200; i++ {
		require.Equal(t, first, r.Route(payoutKey(), caps))
	}
}

// ---------------------------------------------------------------------------
// Hashing
// ---------------------------------------------------------------------------

func TestPolicy_HashIsStableAndOrderSensitive(t *testing.T) {
	p := approvedPayoutPolicy()
	h1, err := p.Hash()
	require.NoError(t, err)
	for i := 0; i < 50; i++ {
		h, err := p.Hash()
		require.NoError(t, err)
		require.Equal(t, h1, h)
	}

	// Reordering the rules changes the meaning of a first-match-wins policy,
	// so it must change the hash.
	q := approvedPayoutPolicy()
	q.Rules[0], q.Rules[1] = q.Rules[1], q.Rules[0]
	h2, err := q.Hash()
	require.NoError(t, err)
	require.NotEqual(t, h1, h2, "rule order is part of the policy's meaning")
}

func TestPolicy_CanonicalFillsEveryDimension(t *testing.T) {
	b, err := approvedPayoutPolicy().Canonical()
	require.NoError(t, err)
	var c canonicalPolicy
	require.NoError(t, json.Unmarshal(b, &c))
	require.Len(t, c.Rules, 4)
	for _, r := range c.Rules {
		require.Len(t, r.Match, len(dimensions),
			"every dimension must be present in the canonical form, wildcarded if unconstrained")
		for _, d := range dimensions {
			require.NotEmpty(t, r.Match[d.name])
		}
	}
}

func TestOutcome_OnlyAllowPermits(t *testing.T) {
	require.True(t, Allow.Permits())
	for _, o := range AllOutcomes() {
		if o == Allow {
			continue
		}
		require.False(t, o.Permits(), "%s must not be read as permission", o)
	}
	require.False(t, Outcome("PROBABLY").Valid())
}
