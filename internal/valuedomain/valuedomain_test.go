package valuedomain

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Domains and rails
// ---------------------------------------------------------------------------

func TestDomain_EveryDeclaredDomainIsRegistered(t *testing.T) {
	require.Len(t, AllDomains(), 8, "PART IX names eight value domains")
	for _, d := range AllDomains() {
		require.True(t, d.Valid(), "%s must be registered", d)
		require.True(t, d.Rail().Valid(), "%s must map to a declared rail", d)
		require.NotEmpty(t, d.String())
	}
}

func TestDomain_UnknownDomainIsInertRatherThanPermissive(t *testing.T) {
	var bogus Domain = "TOTALLY_MADE_UP"
	require.False(t, bogus.Valid())
	require.False(t, bogus.IsEconomic())
	require.False(t, bogus.IsSpendable())
	require.False(t, bogus.IsInternalUnit())
	require.Equal(t, CapitalRail(""), bogus.Rail())
	require.False(t, bogus.Rail().Valid())
	require.False(t, bogus.Rail().Implemented(),
		"an unknown domain must not resolve to an implemented rail")
}

func TestDomain_OnlySimulatedIsNonEconomic(t *testing.T) {
	for _, d := range AllDomains() {
		if d == Simulated {
			require.False(t, d.IsEconomic(), "simulated capital has no economic substance")
			continue
		}
		require.True(t, d.IsEconomic(), "%s must be economic", d)
	}
}

func TestDomain_StagingDomainsAreNotSpendable(t *testing.T) {
	require.False(t, PayoutPending.IsSpendable(),
		"value committed to a payout must not also fund new activity")
	require.False(t, ExternalSettled.IsSpendable(),
		"value that has left the system must not fund new activity")
	require.True(t, InternalCredit.IsSpendable())
}

func TestDomain_ParseRoundTripsAndRejects(t *testing.T) {
	for _, d := range AllDomains() {
		got, err := ParseDomain(strings.ToLower(string(d)))
		require.NoError(t, err)
		require.Equal(t, d, got)
	}
	_, err := ParseDomain("HOSTED_MAGIC")
	require.Error(t, err)
}

func TestDomain_SortIsCanonicalRegardlessOfInputOrder(t *testing.T) {
	a := []Domain{ExternalSettled, InternalCredit, Simulated}
	b := []Domain{Simulated, ExternalSettled, InternalCredit}
	SortDomains(a)
	SortDomains(b)
	require.Equal(t, a, b)
	require.Equal(t, []Domain{InternalCredit, Simulated, ExternalSettled}, a)
}

func TestRail_UnimplementedRailsAreDeclaredButNotUsable(t *testing.T) {
	require.True(t, RailSecuritiesBroker.Valid())
	require.False(t, RailSecuritiesBroker.Implemented(),
		"declaring a rail must not make it executable (PART XXII)")
	require.True(t, RailPredictionDCM.Valid())
	require.False(t, RailPredictionDCM.Implemented())

	for _, r := range []CapitalRail{RailSimulated, RailNativeInternal, RailSelfCustodialOnchain} {
		require.True(t, r.Implemented(), "%s must be implemented", r)
	}
	// The hosted-partner rail is declared and has no adapter, because none can
	// be written against an unverified API. Reporting it implemented would let
	// the Settlement Compiler route to nothing.
	require.False(t, RailHostedPartner.Implemented(),
		"a rail with no provider adapter must not report itself implemented")
}

func TestRail_EveryRailDescribesItsAuthorityModel(t *testing.T) {
	for _, r := range AllRails() {
		require.NotEmpty(t, r.AuthoritativeBalanceSource(), "%s", r)
		require.NotEmpty(t, r.CustodyModel(), "%s", r)
		require.NotEmpty(t, r.ExecutionModel(), "%s", r)
		require.NotEmpty(t, r.SettlementModel(), "%s", r)
		require.NotEmpty(t, r.ReconciliationModel(), "%s", r)
	}
	require.Equal(t, "NODAL_LEDGER", RailNativeInternal.AuthoritativeBalanceSource())
	require.Equal(t, "PROVIDER", RailHostedPartner.AuthoritativeBalanceSource(),
		"Nodal must never be authoritative for partner-held value")
	require.Equal(t, "CHAIN", RailSelfCustodialOnchain.AuthoritativeBalanceSource())
}

func TestRail_OnlyExternalCustodyRailsRequireFinancialIdentity(t *testing.T) {
	require.False(t, RailSimulated.RequiresFinancialIdentity(),
		"JOURNEY A requires the whole agent platform to work with no financial account")
	require.False(t, RailNativeInternal.RequiresFinancialIdentity())
	require.True(t, RailHostedPartner.RequiresFinancialIdentity())
}

func TestRail_DomainsPartitionAcrossRails(t *testing.T) {
	seen := map[Domain]bool{}
	for _, r := range AllRails() {
		for _, d := range r.Domains() {
			require.False(t, seen[d], "%s appears on more than one rail", d)
			seen[d] = true
		}
	}
	require.Len(t, seen, len(AllDomains()), "every domain belongs to exactly one rail")
}

// ---------------------------------------------------------------------------
// Isolation — the load-bearing safety property
// ---------------------------------------------------------------------------

// everyCapabilityActive is the worst case: an attacker, or a misconfiguration,
// has turned on every capability the system knows about.
func everyCapabilityActive() map[CapabilityKey]bool {
	m := map[CapabilityKey]bool{}
	for _, c := range AllConversions() {
		m[c.RequiredCapability] = true
	}
	// Also anything that is not currently referenced by a conversion.
	for _, k := range []CapabilityKey{
		CapNativeMarketTrading, CapPayoutReserve, CapPayoutSettle, CapHostedTrading, CapHostedFunding,
	} {
		m[k] = true
	}
	return m
}

// TestIsolation_CreditsCanNeverReachRealCapital is acceptance test VAL-001.
//
// It asserts the promise the whole product rests on: no configuration, no
// approval, no capability, no policy version puts internal Credits and real
// customer capital in the same atomic movement. If this test ever goes green
// by being deleted or weakened, the closed-loop claim is false.
func TestIsolation_CreditsCanNeverReachRealCapital(t *testing.T) {
	realCapital := []Domain{HostedFiat, HostedCrypto, SelfCustodialCrypto}
	internalValue := []Domain{InternalCredit, InternalNativeAsset}

	for _, internal := range internalValue {
		for _, real := range realCapital {
			t.Run(string(internal)+"+"+string(real), func(t *testing.T) {
				forbidden, why := StructurallyForbidden(internal, real)
				require.True(t, forbidden, "%s and %s must be structurally forbidden", internal, real)
				require.NotEmpty(t, why)

				// Both directions, with every capability in the system on, and
				// whether or not the caller tries to declare the conversion.
				for _, d := range []ConversionKey{{internal, real}, {real, internal}} {
					require.Error(t, CheckConversion(d.From, d.To, everyCapabilityActive()),
						"%s must be rejected even with every capability active", d)

					k := d
					err := CheckPosting([]Domain{d.From, d.To}, &k, everyCapabilityActive())
					require.Error(t, err, "%s must be rejected even when declared", d)
					require.Contains(t, err.Error(), "may never move together")
				}

				// And there is no declared conversion offering a route.
				_, ok := LookupConversion(internal, real)
				require.False(t, ok, "no conversion may be declared from %s to %s", internal, real)
				_, ok = LookupConversion(real, internal)
				require.False(t, ok, "no conversion may be declared from %s to %s", real, internal)
			})
		}
	}
}

func TestIsolation_SimulatedNeverMixesWithAnything(t *testing.T) {
	for _, d := range AllDomains() {
		if d == Simulated {
			continue
		}
		forbidden, why := StructurallyForbidden(Simulated, d)
		require.True(t, forbidden, "SIMULATED must never mix with %s", d)
		require.Contains(t, why, "simulated")
		require.Error(t, CheckConversion(Simulated, d, everyCapabilityActive()))
		require.Error(t, CheckConversion(d, Simulated, everyCapabilityActive()))
	}
	require.NoError(t, CheckPosting([]Domain{Simulated}, nil, nil),
		"a wholly simulated transaction is fine")
}

func TestIsolation_SelfCustodialNeverMovesAtomicallyWithAnything(t *testing.T) {
	for _, d := range AllDomains() {
		if d == SelfCustodialCrypto {
			continue
		}
		forbidden, _ := StructurallyForbidden(SelfCustodialCrypto, d)
		require.True(t, forbidden,
			"Nodal does not control self-custodial value and must not claim atomicity with %s", d)
	}
}

func TestIsolation_DeclaredConversionNeedsItsCapability(t *testing.T) {
	// Buying a native asset with Credits: the canonical gated conversion.
	err := CheckConversion(InternalCredit, InternalNativeAsset, nil)
	require.Error(t, err, "no capabilities active means no native-market trade")
	require.Contains(t, err.Error(), string(CapNativeMarketTrading))

	require.NoError(t, CheckConversion(InternalCredit, InternalNativeAsset,
		map[CapabilityKey]bool{CapNativeMarketTrading: true}))

	// A different capability does not help.
	require.Error(t, CheckConversion(InternalCredit, InternalNativeAsset,
		map[CapabilityKey]bool{CapPayoutSettle: true}))
}

// TestIsolation_DirectionIsLoadBearing is the regression test for the defect
// the first draft of this package shipped: a direction-blind check let the
// deliberately ungated payout-return path authorise the gated payout-reserve
// path, so PAYOUT_RESERVE could be bypassed by anyone able to post a
// two-domain transaction.
func TestIsolation_DirectionIsLoadBearing(t *testing.T) {
	require.NoError(t, CheckConversion(PayoutPending, InternalCredit, nil),
		"the unwind direction is ungated by design")
	require.Error(t, CheckConversion(InternalCredit, PayoutPending, nil),
		"the reserve direction must still require PAYOUT_RESERVE")

	reserve := ConversionKey{InternalCredit, PayoutPending}
	unwind := ConversionKey{PayoutPending, InternalCredit}
	domains := []Domain{InternalCredit, PayoutPending}
	require.Error(t, CheckPosting(domains, &reserve, nil))
	require.NoError(t, CheckPosting(domains, &unwind, nil))
}

func TestIsolation_PayoutPathIsGatedAtBothSteps(t *testing.T) {
	require.Error(t, CheckConversion(InternalCredit, PayoutPending, nil))
	require.Error(t, CheckConversion(PayoutPending, ExternalSettled, nil))

	require.NoError(t, CheckConversion(InternalCredit, PayoutPending,
		map[CapabilityKey]bool{CapPayoutReserve: true}))
	require.NoError(t, CheckConversion(PayoutPending, ExternalSettled,
		map[CapabilityKey]bool{CapPayoutSettle: true}))

	// Holding only the reserve capability must not let value leave.
	require.Error(t, CheckConversion(PayoutPending, ExternalSettled,
		map[CapabilityKey]bool{CapPayoutReserve: true}))
	// And settle alone must not let value be reserved.
	require.Error(t, CheckConversion(InternalCredit, PayoutPending,
		map[CapabilityKey]bool{CapPayoutSettle: true}))
}

func TestIsolation_CrossDomainPostingMustDeclareItsConversion(t *testing.T) {
	domains := []Domain{InternalCredit, InternalNativeAsset}
	caps := map[CapabilityKey]bool{CapNativeMarketTrading: true}

	err := CheckPosting(domains, nil, caps)
	require.Error(t, err, "an undeclared cross-domain movement must be rejected")
	require.Contains(t, err.Error(), "declares no conversion")

	buy := ConversionKey{InternalCredit, InternalNativeAsset}
	require.NoError(t, CheckPosting(domains, &buy, caps))

	// A declaration that does not match the domains present is a lie.
	wrong := ConversionKey{InternalCredit, PayoutPending}
	err = CheckPosting(domains, &wrong, everyCapabilityActive())
	require.Error(t, err)
	require.Contains(t, err.Error(), "but touches value domains")
}

func TestIsolation_SingleDomainPostingMustNotClaimAConversion(t *testing.T) {
	require.NoError(t, CheckPosting([]Domain{InternalCredit}, nil, nil))
	lie := ConversionKey{InternalCredit, PayoutPending}
	require.Error(t, CheckPosting([]Domain{InternalCredit}, &lie, everyCapabilityActive()))
}

// TestIsolation_UnwindPathIsNeverBlocked encodes PART XXXII: stopping new risk
// must not strand value that is already in flight.
func TestIsolation_UnwindPathIsNeverBlocked(t *testing.T) {
	conv, ok := LookupConversion(PayoutPending, InternalCredit)
	require.True(t, ok)
	require.True(t, conv.AlwaysPermitted(),
		"returning a failed payout to the user must not require a capability")

	require.NoError(t, CheckConversion(PayoutPending, InternalCredit, nil),
		"with every capability revoked, reserved Credits must still be returnable")
}

func TestIsolation_ThreeDomainsAlwaysRejected(t *testing.T) {
	k := ConversionKey{InternalCredit, PayoutPending}
	err := CheckPosting([]Domain{InternalCredit, PayoutPending, ExternalSettled}, &k, everyCapabilityActive())
	require.Error(t, err)
	require.Contains(t, err.Error(), "at most two value domains")
}

func TestIsolation_UndeclaredPairIsRejected(t *testing.T) {
	// Not structurally forbidden, but nobody declared it either.
	err := CheckConversion(InternalNativeAsset, PayoutPending, everyCapabilityActive())
	require.Error(t, err)
	require.Contains(t, err.Error(), "no declared conversion",
		"a native asset must be sold for Credits before any payout can see it")
	require.Error(t, CheckConversion(PayoutPending, InternalNativeAsset, everyCapabilityActive()))
}

func TestIsolation_EmptyAndUnknownInputsFailClosed(t *testing.T) {
	require.Error(t, CheckPosting(nil, nil, everyCapabilityActive()))
	require.Error(t, CheckPosting([]Domain{"NOT_A_DOMAIN"}, nil, everyCapabilityActive()))
	require.Error(t, CheckPosting([]Domain{InternalCredit, "NOT_A_DOMAIN"}, nil, everyCapabilityActive()))
	require.Error(t, CheckConversion("NOT_A_DOMAIN", InternalCredit, everyCapabilityActive()))
	require.Error(t, CheckConversion(InternalCredit, "NOT_A_DOMAIN", everyCapabilityActive()))
}

// TestProp_IsolationIsExhaustiveOverEveryPairAndCapabilitySubset walks the
// entire ordered domain × domain × capability-subset space. It is small enough
// to be exhaustive rather than sampled, so it is a proof rather than evidence.
func TestProp_IsolationIsExhaustiveOverEveryPairAndCapabilitySubset(t *testing.T) {
	caps := []CapabilityKey{
		CapNativeMarketTrading, CapPayoutReserve, CapPayoutSettle, CapHostedTrading, CapHostedFunding,
	}
	subsets := 1 << len(caps)
	domains := AllDomains()

	for i := 0; i < subsets; i++ {
		active := map[CapabilityKey]bool{}
		for b, c := range caps {
			if i&(1<<b) != 0 {
				active[c] = true
			}
		}
		for _, from := range domains {
			for _, to := range domains {
				err := CheckConversion(from, to, active)
				forbidden, _ := StructurallyForbidden(from, to)

				switch {
				case from == to:
					require.NoError(t, err, "single-domain %s rejected", from)
				case forbidden:
					require.Error(t, err,
						"structurally forbidden %s->%s permitted with capability subset %d", from, to, i)
				default:
					conv, declared := LookupConversion(from, to)
					if !declared {
						require.Error(t, err, "undeclared %s->%s permitted", from, to)
						continue
					}
					if conv.AlwaysPermitted() || active[conv.RequiredCapability] {
						require.NoError(t, err, "declared+capable %s->%s rejected (subset %d)", from, to, i)
					} else {
						require.Error(t, err, "declared but uncapable %s->%s permitted (subset %d)", from, to, i)
					}
				}
			}
		}
	}
}

func TestIsolation_MatrixDescriptionCoversEveryConversion(t *testing.T) {
	desc := DescribeIsolation()
	for _, c := range AllConversions() {
		require.Contains(t, desc, string(c.From))
		require.Contains(t, desc, string(c.To))
		require.Contains(t, desc, c.Why)
	}
	require.Contains(t, desc, "unwind path", "the ungated return path must be visibly labelled")
}

// ---------------------------------------------------------------------------
// Origins, finality
// ---------------------------------------------------------------------------

func TestOrigin_AllElevenOriginsDeclared(t *testing.T) {
	require.Len(t, AllOrigins(), 11, "PART IX names eleven credit origins")
	for _, o := range AllOrigins() {
		require.True(t, o.Valid())
		got, err := ParseCreditOrigin(strings.ToLower(string(o)))
		require.NoError(t, err)
		require.Equal(t, o, got)
	}
	_, err := ParseCreditOrigin("FREE_MONEY")
	require.Error(t, err)
}

func TestOrigin_EarnedDistinguishesSupplyFromSpeculation(t *testing.T) {
	require.True(t, OriginCreatorEarning.EarnedByUser())
	require.True(t, OriginDataSaleEarning.EarnedByUser())
	require.True(t, OriginAgentServiceEarning.EarnedByUser())
	require.True(t, OriginMarketCreatorEarning.EarnedByUser())

	require.False(t, OriginMarketTradingProceeds.EarnedByUser(),
		"speculative proceeds are not earnings and must be separable for payout policy")
	require.False(t, OriginPurchased.EarnedByUser())
	require.False(t, OriginPromotional.EarnedByUser())
}

func TestFinality_ReversibleValueIsSpendableButNeverPayable(t *testing.T) {
	require.True(t, FinalityReversible.Spendable(),
		"buying Credits with a card must be usable immediately; that is the product")
	require.False(t, FinalityReversible.PayoutEligible(),
		"PART XI: reversible funding must never become payout-eligible")

	require.True(t, FinalitySettled.PayoutEligible())
	require.True(t, FinalityUnfunded.PayoutEligible(),
		"nothing external backs unfunded value, so nothing external can reclaim it")

	require.False(t, FinalityDisputed.Spendable())
	require.False(t, FinalityDisputed.PayoutEligible())
	require.False(t, FinalityReversed.Spendable())
	require.False(t, FinalityReversed.PayoutEligible())
}

func TestFinality_TransitionTableIsExplicit(t *testing.T) {
	require.True(t, CanTransitionFinality(FinalityReversible, FinalitySettled))
	require.True(t, CanTransitionFinality(FinalityReversible, FinalityReversed))
	require.True(t, CanTransitionFinality(FinalitySettled, FinalityDisputed),
		"a card network can dispute after a processor calls it settled")
	require.True(t, CanTransitionFinality(FinalityDisputed, FinalitySettled),
		"a dispute the platform wins must be able to resolve back to settled")

	require.False(t, CanTransitionFinality(FinalityReversed, FinalitySettled),
		"reversed value must never come back to life")
	require.False(t, CanTransitionFinality(FinalityUnfunded, FinalitySettled),
		"unfunded value has no external funding to settle")
	require.False(t, CanTransitionFinality(FinalitySettled, FinalityReversed),
		"settled funding is reversed only by first being disputed")
}

// ---------------------------------------------------------------------------
// Policy
// ---------------------------------------------------------------------------

func TestPolicy_DefaultForbidsEveryOrigin(t *testing.T) {
	p := DefaultPolicy()
	require.NoError(t, p.Validate())
	require.Equal(t, DefaultPolicyVersion, p.Version)
	for _, o := range AllOrigins() {
		require.False(t, p.Rule(o).PayoutAllowed, "%s must be forbidden on a fresh deployment", o)
	}
}

func TestPolicy_DefaultPermitsNothingEvenForAPerfectUser(t *testing.T) {
	p := DefaultPolicy()
	for _, o := range AllOrigins() {
		ok, reasons := p.Permits(PermitInput{
			Origin:      o,
			Finality:    FinalitySettled,
			Domain:      InternalCredit,
			Verified:    VerificationEnhanced,
			HeldDays:    3650,
			ActiveCaps:  everyCapabilityActive(),
			PolicyValid: true,
		})
		require.False(t, ok, "origin %s paid out under the fail-closed policy", o)
		require.Contains(t, reasons, ReasonOriginForbidden)
	}
}

func TestPolicy_ValidateRejectsAnIncompletePolicy(t *testing.T) {
	p := DefaultPolicy()
	delete(p.Rules, OriginMarketTradingProceeds)
	err := p.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), string(OriginMarketTradingProceeds),
		"a newly added origin must not be silently defaulted")
}

func TestPolicy_ValidateRejectsPermissionWithoutACapability(t *testing.T) {
	p := DefaultPolicy()
	p.Rules[OriginCreatorEarning] = OriginRule{
		PayoutAllowed:        true,
		RequiredVerification: VerificationPayoutKYC,
	}
	err := p.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "capability gate")
}

func TestPolicy_ValidateRejectsPermissionWithoutKYC(t *testing.T) {
	p := DefaultPolicy()
	p.Rules[OriginCreatorEarning] = OriginRule{
		PayoutAllowed:        true,
		RequiredCapability:   "PAYOUT_CREATOR_EARNINGS",
		RequiredVerification: VerificationNodalIdentity,
	}
	err := p.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "PAYOUT_KYC",
		"a Nodal login must never be sufficient to receive money")
}

func TestPolicy_ValidateRejectsAMisleadingForbiddenRule(t *testing.T) {
	p := DefaultPolicy()
	p.Rules[OriginPromotional] = OriginRule{
		PayoutAllowed:        false,
		RequiredCapability:   "PAYOUT_PROMOTIONAL",
		RequiredVerification: VerificationNone,
	}
	require.Error(t, p.Validate(),
		"naming a capability on a forbidden rule implies the capability could enable it")
}

// approvedCreatorPolicy is what a future, evidence-backed activation looks
// like: creator earnings become payable, everything else stays shut.
func approvedCreatorPolicy() Policy {
	p := DefaultPolicy()
	p.Version = "payout-policy-v2-creator-earnings"
	p.Rules[OriginCreatorEarning] = OriginRule{
		PayoutAllowed:        true,
		RequiredCapability:   "PAYOUT_CREATOR_EARNINGS",
		RequiredVerification: VerificationPayoutKYC,
		MinHoldDays:          30,
	}
	return p
}

func TestPolicy_AnApprovedOriginStillNeedsEveryOtherCondition(t *testing.T) {
	p := approvedCreatorPolicy()
	require.NoError(t, p.Validate())

	base := PermitInput{
		Origin:      OriginCreatorEarning,
		Finality:    FinalitySettled,
		Domain:      InternalCredit,
		Verified:    VerificationPayoutKYC,
		HeldDays:    30,
		ActiveCaps:  map[CapabilityKey]bool{"PAYOUT_CREATOR_EARNINGS": true},
		PolicyValid: true,
	}
	ok, reasons := p.Permits(base)
	require.True(t, ok, "fully satisfied request denied: %v", reasons)

	t.Run("capability off", func(t *testing.T) {
		in := base
		in.ActiveCaps = nil
		ok, reasons := p.Permits(in)
		require.False(t, ok)
		require.Contains(t, reasons, ReasonCapabilityNotActive)
	})
	t.Run("verification too low", func(t *testing.T) {
		in := base
		in.Verified = VerificationNodalIdentity
		ok, reasons := p.Permits(in)
		require.False(t, ok)
		require.Contains(t, reasons, ReasonVerificationTooLow)
	})
	t.Run("hold not elapsed", func(t *testing.T) {
		in := base
		in.HeldDays = 29
		ok, reasons := p.Permits(in)
		require.False(t, ok)
		require.Contains(t, reasons, ReasonHoldPeriodNotElapsed)
	})
	t.Run("funding still reversible", func(t *testing.T) {
		in := base
		in.Finality = FinalityReversible
		ok, reasons := p.Permits(in)
		require.False(t, ok)
		require.Contains(t, reasons, ReasonFundingNotFinal)
	})
	t.Run("wrong domain", func(t *testing.T) {
		in := base
		in.Domain = HostedFiat
		ok, reasons := p.Permits(in)
		require.False(t, ok)
		require.Contains(t, reasons, ReasonDomainNotWithdrawable)
	})
	t.Run("a different origin is unaffected", func(t *testing.T) {
		in := base
		in.Origin = OriginMarketTradingProceeds
		ok, reasons := p.Permits(in)
		require.False(t, ok, "approving creator earnings must not approve trading proceeds")
		require.Contains(t, reasons, ReasonOriginForbidden)
	})
	t.Run("promotional credits can never cash out", func(t *testing.T) {
		// Acceptance test VAL-002.
		in := base
		in.Origin = OriginPromotional
		in.Finality = FinalityUnfunded
		ok, reasons := p.Permits(in)
		require.False(t, ok)
		require.Contains(t, reasons, ReasonOriginForbidden)
	})
}

func TestPolicy_ReasonsAreDeterministicAndOrdered(t *testing.T) {
	p := approvedCreatorPolicy()
	in := PermitInput{
		Origin:      OriginCreatorEarning,
		Finality:    FinalityReversible,
		Domain:      HostedFiat,
		Verified:    VerificationNone,
		HeldDays:    0,
		ActiveCaps:  nil,
		PolicyValid: true,
	}
	ok, first := p.Permits(in)
	require.False(t, ok)
	for i := 0; i < 50; i++ {
		_, again := p.Permits(in)
		require.Equal(t, first, again, "reason list must not depend on map iteration order")
	}
	require.Equal(t, []PermitReason{
		ReasonDomainNotWithdrawable,
		ReasonCapabilityNotActive,
		ReasonVerificationTooLow,
		ReasonHoldPeriodNotElapsed,
		ReasonFundingNotFinal,
	}, first)
}

func TestPolicy_InvalidPolicyDeniesAndSaysSo(t *testing.T) {
	p := approvedCreatorPolicy()
	ok, reasons := p.Permits(PermitInput{
		Origin: OriginCreatorEarning, Finality: FinalitySettled, Domain: InternalCredit,
		Verified: VerificationPayoutKYC, HeldDays: 999,
		ActiveCaps:  map[CapabilityKey]bool{"PAYOUT_CREATOR_EARNINGS": true},
		PolicyValid: false,
	})
	require.False(t, ok)
	require.Contains(t, reasons, ReasonPolicyInvalid)
}

func TestPolicy_HashIsStableAcrossMapOrderAndSensitiveToChange(t *testing.T) {
	p := approvedCreatorPolicy()
	h1, err := p.Hash()
	require.NoError(t, err)
	for i := 0; i < 100; i++ {
		h, err := p.Hash()
		require.NoError(t, err)
		require.Equal(t, h1, h)
	}

	q := approvedCreatorPolicy()
	r := q.Rules[OriginCreatorEarning]
	r.MinHoldDays = 31
	q.Rules[OriginCreatorEarning] = r
	h2, err := q.Hash()
	require.NoError(t, err)
	require.NotEqual(t, h1, h2, "a changed rule must change the policy hash")
}

func TestPolicy_CanonicalIsValidJSONWithSortedOrigins(t *testing.T) {
	b, err := approvedCreatorPolicy().Canonical()
	require.NoError(t, err)
	var c canonicalPolicy
	require.NoError(t, json.Unmarshal(b, &c))
	require.Len(t, c.Rules, len(AllOrigins()))
	for i := 1; i < len(c.Rules); i++ {
		require.Less(t, c.Rules[i-1].Origin, c.Rules[i].Origin)
	}
}

func TestVerificationLevel_OrderingAndUnknownFailsClosed(t *testing.T) {
	require.True(t, VerificationEnhanced.AtLeast(VerificationPayoutKYC))
	require.True(t, VerificationPayoutKYC.AtLeast(VerificationPayoutKYC))
	require.False(t, VerificationNodalIdentity.AtLeast(VerificationPayoutKYC))
	require.False(t, VerificationNone.AtLeast(VerificationNodalIdentity))

	var bogus VerificationLevel = "TRUST_ME"
	require.False(t, bogus.Valid())
	require.False(t, bogus.AtLeast(VerificationNone),
		"an unrecognised level must not satisfy even the lowest requirement")
	require.False(t, VerificationEnhanced.AtLeast(bogus))
}
