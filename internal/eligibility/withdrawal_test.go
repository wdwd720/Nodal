package eligibility

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

func qty(n int64) money.Quantity { return money.QuantityFromInt64(n) }

// holding is one settled, well-aged bucket of an origin, which is the case
// where the ONLY thing that can refuse is the policy, the capability or the
// verification level.
func holding(o valuedomain.CreditOrigin, n int64) OriginHolding {
	return OriginHolding{Origin: o, Quantity: qty(n), Finality: valuedomain.FinalitySettled, HeldDays: 400}
}

// baseInput is a deployment where nothing account-level or provider-level is
// refusing, so each test can turn exactly one thing off.
func baseInput(policy valuedomain.Policy, level valuedomain.VerificationLevel, holdings ...OriginHolding) WithdrawalInput {
	var gross money.Quantity
	for _, h := range holdings {
		gross = gross.Add(h.Quantity)
	}
	return WithdrawalInput{
		Policy:      policy,
		Verified:    level,
		ActiveCaps:  map[valuedomain.CapabilityKey]bool{valuedomain.CapPayoutReserve: true},
		Holdings:    holdings,
		PolicyValid: policy.Validate() == nil,
		Gross:       gross,
		Spendable:   gross,

		JurisdictionSupported: true,
		ProviderAvailable:     true,
		ProviderName:          "sandbox_payout",
		DestinationConfigured: true,
		DisclosureAccepted:    true,
	}
}

func bucketOf(e WithdrawalExplanation, o valuedomain.CreditOrigin) OriginBucket {
	for _, b := range e.Buckets {
		if b.Origin == o {
			return b
		}
	}
	return OriginBucket{}
}

func reasonsOf(b OriginBucket) []string {
	out := make([]string, 0, len(b.Reasons))
	for _, r := range b.Reasons {
		out = append(out, string(r))
	}
	return out
}

// Every declared origin gets a bucket, including the empty ones. An origin
// missing from the list would read as "we did not consider it".
func TestExplainWithdrawal_EveryOriginGetsABucket(t *testing.T) {
	t.Parallel()
	e := ExplainWithdrawal(baseInput(valuedomain.SandboxPolicy(), valuedomain.VerificationPayoutKYC))
	require.Len(t, e.Buckets, len(valuedomain.AllOrigins()))
	for i, o := range valuedomain.AllOrigins() {
		assert.Equalf(t, o, e.Buckets[i].Origin, "buckets must be in the canonical origin order")
		assert.Contains(t, reasonsOf(e.Buckets[i]), string(WithdrawalNoValue),
			"an empty bucket says why it is empty rather than showing an unexplained zero")
	}
	assert.False(t, e.Eligible)
}

// The default policy is fail-closed: no origin may be withdrawn, by anybody, at
// any verification level, under any capability. That is the shipped state and
// this asserts it has not drifted.
func TestExplainWithdrawal_TheDefaultPolicyRefusesEveryOrigin(t *testing.T) {
	t.Parallel()
	var holdings []OriginHolding
	for _, o := range valuedomain.AllOrigins() {
		holdings = append(holdings, holding(o, 1_000_000))
	}
	in := baseInput(valuedomain.DefaultPolicy(), valuedomain.VerificationEnhanced, holdings...)
	e := ExplainWithdrawal(in)

	assert.False(t, e.Eligible)
	assert.Equal(t, "0", e.WithdrawableNow.String())
	for _, o := range valuedomain.AllOrigins() {
		b := bucketOf(e, o)
		assert.Containsf(t, reasonsOf(b), string(WithdrawalOriginNotWithdrawable), "%s", o)
		assert.Equalf(t, "0", b.Withdrawable.String(), "%s", o)
		assert.Falsef(t, b.VerificationWouldSuffice,
			"%s is forbidden outright; telling a person verification would fix it would be a lie", o)
	}
	assert.False(t, e.VerificationWouldSuffice)
}

// The sandbox policy permits purchased and earned value once verified, and
// never permits granted, refunded, adjusted or provider-settled value. Both
// halves are asserted per origin, with the REASON each gives.
func TestExplainWithdrawal_TheSandboxPolicyPerOrigin(t *testing.T) {
	t.Parallel()
	var holdings []OriginHolding
	for _, o := range valuedomain.AllOrigins() {
		holdings = append(holdings, holding(o, 1_000_000))
	}
	policy := valuedomain.SandboxPolicy()
	e := ExplainWithdrawal(baseInput(policy, valuedomain.VerificationPayoutKYC, holdings...))

	withdrawable := map[valuedomain.CreditOrigin]bool{
		valuedomain.OriginPurchased:             true,
		valuedomain.OriginCreatorEarning:        true,
		valuedomain.OriginDataSaleEarning:       true,
		valuedomain.OriginAgentServiceEarning:   true,
		valuedomain.OriginMarketCreatorEarning:  true,
		valuedomain.OriginMarketTradingProceeds: true,
		// COMPETITION_REWARD is deliberately absent: a prize is a grant and a
		// grant never leaves (D-095, F-157).
	}
	var total int64
	for _, o := range valuedomain.AllOrigins() {
		b := bucketOf(e, o)
		if withdrawable[o] {
			assert.Equalf(t, "1000000", b.Withdrawable.String(), "%s should be withdrawable", o)
			assert.Emptyf(t, b.Reasons, "%s", o)
			assert.Equal(t, valuedomain.VerificationPayoutKYC, b.RequiredVerification, o)
			assert.Equal(t, valuedomain.CapPayoutReserve, b.RequiredCapability, o)
			total += 1_000_000
			continue
		}
		assert.Equalf(t, "0", b.Withdrawable.String(), "%s must not be withdrawable", o)
		assert.Containsf(t, reasonsOf(b), string(WithdrawalOriginNotWithdrawable), "%s", o)
	}
	assert.True(t, e.Eligible)
	assert.Equal(t, money.QuantityFromInt64(total).String(), e.WithdrawableNow.String())
}

// The difference §19 exists to preserve: "you cannot" and "you have not
// verified yet" are different answers, and the second is a next step.
func TestExplainWithdrawal_VerificationIsANextStepNotADenial(t *testing.T) {
	t.Parallel()
	policy := valuedomain.SandboxPolicy()
	in := baseInput(policy, valuedomain.VerificationNodalIdentity,
		holding(valuedomain.OriginPurchased, 5_000),
		holding(valuedomain.OriginPromotional, 9_000))
	e := ExplainWithdrawal(in)

	purchased := bucketOf(e, valuedomain.OriginPurchased)
	assert.Equal(t, "0", purchased.Withdrawable.String())
	assert.Equal(t, []string{string(WithdrawalRequiresVerification)}, reasonsOf(purchased))
	assert.True(t, purchased.VerificationWouldSuffice,
		"the value is there, its provenance is approved, and the only obstacle is identity")

	promotional := bucketOf(e, valuedomain.OriginPromotional)
	assert.Contains(t, reasonsOf(promotional), string(WithdrawalOriginNotWithdrawable))
	assert.False(t, promotional.VerificationWouldSuffice,
		"verifying does not make a promotional grant withdrawable, and the product must not imply it does")

	assert.True(t, e.VerificationWouldSuffice)
	assert.Equal(t, valuedomain.VerificationPayoutKYC, e.RequiredVerification)
	assert.Equal(t, valuedomain.VerificationNodalIdentity, e.CurrentVerification)
	assert.False(t, e.Eligible)
}

// The four account- and platform-level refusals apply to every bucket, because
// no choice of provenance escapes them. Each is separately turned off.
func TestExplainWithdrawal_AccountAndPlatformRefusalsApplyEverywhere(t *testing.T) {
	t.Parallel()
	policy := valuedomain.SandboxPolicy()
	build := func(mutate func(*WithdrawalInput)) WithdrawalExplanation {
		in := baseInput(policy, valuedomain.VerificationPayoutKYC, holding(valuedomain.OriginPurchased, 5_000))
		mutate(&in)
		return ExplainWithdrawal(in)
	}

	cases := []struct {
		name   string
		mutate func(*WithdrawalInput)
		want   WithdrawalReason
	}{
		{"frozen account", func(in *WithdrawalInput) { in.AccountFrozen = true }, WithdrawalAccountRestricted},
		{"compliance hold", func(in *WithdrawalInput) { in.AccountRestrictions = []string{"COMPLIANCE_HOLD"} }, WithdrawalAccountRestricted},
		{"unsupported jurisdiction", func(in *WithdrawalInput) { in.JurisdictionSupported = false }, WithdrawalJurisdictionRestricted},
		{"no provider", func(in *WithdrawalInput) { in.ProviderAvailable = false }, WithdrawalProviderUnavailable},
		{"invalid policy", func(in *WithdrawalInput) { in.PolicyValid = false }, WithdrawalPolicyInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := build(c.mutate)
			assert.False(t, e.Eligible)
			assert.Equal(t, "0", e.WithdrawableNow.String())
			assert.Contains(t, e.Reasons, c.want)
			for _, b := range e.Buckets {
				assert.Containsf(t, b.Reasons, c.want, "origin %s must carry the account-level refusal too", b.Origin)
			}
		})
	}

	// The capability is different: it is per-origin, because a policy can name
	// a different gate per origin.
	e := build(func(in *WithdrawalInput) { in.ActiveCaps = nil })
	assert.False(t, e.Eligible)
	assert.Contains(t, reasonsOf(bucketOf(e, valuedomain.OriginPurchased)), string(WithdrawalCapabilityInactive))
}

// Finality and hold period are reasons of their own, because waiting fixes
// them and nothing else does.
func TestExplainWithdrawal_WaitingIsItsOwnReason(t *testing.T) {
	t.Parallel()
	policy := valuedomain.SandboxPolicy()

	reversible := baseInput(policy, valuedomain.VerificationPayoutKYC, OriginHolding{
		Origin: valuedomain.OriginPurchased, Quantity: qty(5_000),
		Finality: valuedomain.FinalityReversible, HeldDays: 400,
	})
	e := ExplainWithdrawal(reversible)
	assert.Contains(t, reasonsOf(bucketOf(e, valuedomain.OriginPurchased)), string(WithdrawalFundingNotSettled))
	assert.Equal(t, "0", e.WithdrawableNow.String())

	// An undeclared finality is not defaulted into something permissive: it is
	// refused, because UNFUNDED — the tempting default — is payout-eligible.
	unknown := baseInput(policy, valuedomain.VerificationPayoutKYC, OriginHolding{
		Origin: valuedomain.OriginPurchased, Quantity: qty(5_000), HeldDays: 400,
	})
	e = ExplainWithdrawal(unknown)
	assert.Contains(t, reasonsOf(bucketOf(e, valuedomain.OriginPurchased)), string(WithdrawalFundingNotSettled))
	assert.Equal(t, "0", e.WithdrawableNow.String())
}

// The provider minimum is judged on the WHOLE withdrawable amount, not per
// bucket: a payout draws across origins, and refusing each bucket for being
// under a dollar would refuse a ten-dollar payout made of ten one-dollar
// origins.
func TestExplainWithdrawal_TheMinimumIsJudgedOnTheWhole(t *testing.T) {
	t.Parallel()
	policy := valuedomain.SandboxPolicy()
	in := baseInput(policy, valuedomain.VerificationPayoutKYC,
		holding(valuedomain.OriginPurchased, 400),
		holding(valuedomain.OriginCreatorEarning, 400))
	in.MinimumQuantity = qty(1_000)
	e := ExplainWithdrawal(in)
	assert.False(t, e.Eligible)
	assert.Contains(t, e.Reasons, WithdrawalMinimumNotMet)
	assert.Equal(t, "800", e.WithdrawableNow.String(), "the buckets still say what they hold")

	in.MinimumQuantity = qty(800)
	e = ExplainWithdrawal(in)
	assert.True(t, e.Eligible)
	assert.NotContains(t, e.Reasons, WithdrawalMinimumNotMet)

	// A provider that publishes no minimum is "unknown", never "any amount
	// will do" — but with nothing to compare against, the reason is simply not
	// reported rather than invented.
	in.MinimumQuantity = money.Quantity{}
	e = ExplainWithdrawal(in)
	assert.True(t, e.Eligible)
	assert.NotContains(t, e.Reasons, WithdrawalMinimumNotMet)
}

// The reasons come back in a stable order however the evaluation was compiled,
// so an explanation hashes identically twice.
func TestExplainWithdrawal_IsDeterministic(t *testing.T) {
	t.Parallel()
	var holdings []OriginHolding
	for _, o := range valuedomain.AllOrigins() {
		holdings = append(holdings, holding(o, 7))
	}
	in := baseInput(valuedomain.SandboxPolicy(), valuedomain.VerificationNone, holdings...)
	in.AccountRestrictions = []string{"COMPLIANCE_HOLD"}
	first := ExplainWithdrawal(in)
	for i := 0; i < 20; i++ {
		assert.Equal(t, first, ExplainWithdrawal(in))
	}
	// And the reasons are in the declared report order, not discovery order.
	ranks := make([]int, 0, len(first.Reasons))
	for _, r := range first.Reasons {
		ranks = append(ranks, withdrawalReasonRank[r])
	}
	for i := 1; i < len(ranks); i++ {
		assert.LessOrEqual(t, ranks[i-1], ranks[i], "reasons must come back in report order")
	}
}

func TestWithdrawalReasonCodes_AreClosedAndUnique(t *testing.T) {
	t.Parallel()
	codes := WithdrawalReasonCodes()
	seen := map[WithdrawalReason]bool{}
	for _, c := range codes {
		assert.False(t, seen[c], "duplicate reason %s", c)
		seen[c] = true
		assert.NotEmpty(t, string(c))
	}
	// The seven the product goal names must all be present.
	for _, want := range []WithdrawalReason{
		WithdrawalRequiresVerification, WithdrawalOriginNotWithdrawable, WithdrawalCapabilityInactive,
		WithdrawalJurisdictionRestricted, WithdrawalAccountRestricted, WithdrawalProviderUnavailable,
		WithdrawalMinimumNotMet,
	} {
		assert.True(t, seen[want], "%s is missing from the declared reasons", want)
	}
}

// TestExplainWithdrawal_TheDisclosureIsAStepAndNotARefusal (D-083).
//
// §48 puts the withdrawal disclosure at the moment somebody asks to take value
// out, deliberately not at signup, so an unsigned disclosure is the normal state
// of everybody who has never withdrawn. Reporting zero withdrawable for all of
// them would tell a person their money is stuck when a document they have not
// been shown yet is the whole of it.
func TestExplainWithdrawal_TheDisclosureIsAStepAndNotARefusal(t *testing.T) {
	t.Parallel()
	policy := valuedomain.SandboxPolicy()
	in := baseInput(policy, valuedomain.VerificationPayoutKYC, holding(valuedomain.OriginPurchased, 1_000_000))
	in.DisclosureAccepted = false
	e := ExplainWithdrawal(in)

	assert.False(t, e.Eligible, "nothing may leave until the disclosure is accepted")
	assert.Contains(t, reasonStrings(e.Reasons), string(WithdrawalTermsNotAccepted))
	assert.Equal(t, "1000000", e.WithdrawableNow.String(),
		"the value is eligible; what is missing is a signature, and the figure has to say so")
	assert.Equal(t, "1000000", bucketOf(e, valuedomain.OriginPurchased).Withdrawable.String())
	assert.NotContains(t, reasonsOf(bucketOf(e, valuedomain.OriginPurchased)), string(WithdrawalTermsNotAccepted),
		"it is a fact about the person, not about any origin, so no bucket carries it")

	// Accepted, and the verdict is the one the buckets already supported.
	in.DisclosureAccepted = true
	accepted := ExplainWithdrawal(in)
	assert.True(t, accepted.Eligible)
	assert.NotContains(t, reasonStrings(accepted.Reasons), string(WithdrawalTermsNotAccepted))
}

func reasonStrings(rs []WithdrawalReason) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, string(r))
	}
	return out
}
