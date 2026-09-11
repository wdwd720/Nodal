package eligibility

import (
	"sort"

	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// The withdrawal explanation (goal §19, §22, §23).
//
// `Evaluate` above answers "may this identity act in this context at all" from
// an operator-recorded policy. That is one of the four independent questions a
// withdrawal has to pass, and on its own it cannot tell a person anything
// useful: it does not know that their Credits are half promotional, that the
// capability is off, or that the provider will not send less than a dollar.
//
// `ExplainWithdrawal` composes the other three with it and produces the answer
// §19 asks for — "show eligible / ineligible value" — broken down by the origin
// each unit came from, with a reason per bucket that names something the person
// or the platform can actually do.
//
// It is a pure function for the same reason `Evaluate` is: no clock, no
// database, no provider call. Everything it needs is supplied by the caller,
// so the same inputs always produce the same explanation and a screenshot of it
// can be reproduced later.
//
// # What it must never do
//
// Decide that value may leave. It explains what the policy, the gates, the
// verification level and the provider have already decided; every number in it
// comes from `credit.Balances` and `valuedomain.Policy`, and it recomputes
// none of them.

// WithdrawalReason is a machine-readable reason a person cannot withdraw, or
// cannot withdraw a particular origin bucket. Each one names something that
// could change, and the API renders the sentence.
type WithdrawalReason string

// Withdrawal reasons, in the order they are reported.
const (
	// WithdrawalRequiresVerification is the one §19 exists to distinguish from
	// every other refusal: the value is there and its provenance is approved,
	// and the only thing missing is an identity decision. It is a NEXT STEP,
	// not a denial, and the product says so — "verify your identity to enable
	// withdrawal eligibility", never "verify to turn your Credits into cash".
	WithdrawalRequiresVerification WithdrawalReason = "REQUIRES_VERIFICATION"
	// WithdrawalOriginNotWithdrawable is value whose provenance the payout
	// policy does not permit to leave, at any verification level. A
	// promotional grant is the archetype.
	WithdrawalOriginNotWithdrawable WithdrawalReason = "ORIGIN_NOT_WITHDRAWABLE"
	// WithdrawalCapabilityInactive is a capability gate that is not ACTIVE.
	// Nothing the person does changes it; it is a platform state and the
	// explanation says so rather than blaming them.
	WithdrawalCapabilityInactive WithdrawalReason = "CAPABILITY_INACTIVE"
	// WithdrawalJurisdictionRestricted is where the person is.
	WithdrawalJurisdictionRestricted WithdrawalReason = "JURISDICTION_RESTRICTED"
	// WithdrawalAccountRestricted is a freeze, a compliance hold or a
	// restriction recorded against the account.
	WithdrawalAccountRestricted WithdrawalReason = "ACCOUNT_RESTRICTED"
	// WithdrawalProviderUnavailable is no payout provider, or one that cannot
	// pay this person. It is the honest state of a deployment with no
	// conversion contract.
	WithdrawalProviderUnavailable WithdrawalReason = "PROVIDER_UNAVAILABLE"
	// WithdrawalTermsNotAccepted is the withdrawal disclosure, unsigned. §48
	// puts it at the moment somebody asks to take value out and deliberately
	// not at signup, so this is the normal state of a person who has never
	// withdrawn -- it is a step, like verification, and the product says so.
	//
	// It does not zero the buckets. The value IS eligible; what is missing is a
	// signature, and reporting nothing withdrawable would tell a person their
	// money is stuck when a document they have not been shown yet is the whole
	// of it.
	WithdrawalTermsNotAccepted WithdrawalReason = "TERMS_NOT_ACCEPTED"
	// WithdrawalMinimumNotMet is eligible value below what the provider will
	// send. It is judged NET of fees, because sub-minimum dust is destroyed
	// rather than returned (PROVIDER_BOUNDARY §3).
	WithdrawalMinimumNotMet WithdrawalReason = "MINIMUM_NOT_MET"
	// WithdrawalFundingNotSettled is value whose funding can still be clawed
	// back — a card payment inside its dispute window, or an earning derived
	// from one. Waiting fixes it, which is why it is a reason of its own
	// rather than folded into the origin.
	//
	// That sentence was false for every earning in this system until D-124.
	// internal/commerce and internal/nativemarket minted proceeds and fees
	// REVERSIBLE unconditionally, and the only writer that promotes a lot out
	// of REVERSIBLE reads `credit_fundings.lot_id`, which an earning has never
	// had — so this reason was shown on value whose finality nothing could
	// move, on a bucket held for a year, with "waiting" as the advice (F-230).
	// A derived lot now names the lots that funded it, is minted at the least
	// final finality among them, and is promoted by
	// credit.Service.SettleDerived when every one of them settles. Waiting
	// fixes it because something is now waiting on something.
	WithdrawalFundingNotSettled WithdrawalReason = "FUNDING_NOT_SETTLED"
	// WithdrawalHoldPeriodNotElapsed is value that has not aged long enough.
	WithdrawalHoldPeriodNotElapsed WithdrawalReason = "HOLD_PERIOD_NOT_ELAPSED"
	// WithdrawalNoValue is an empty bucket. It is reported so that a bucket is
	// never shown with no reason at all, which reads as an unexplained zero.
	WithdrawalNoValue WithdrawalReason = "NO_VALUE"
	// WithdrawalPolicyInvalid is a payout policy that does not validate. It
	// fails closed and names itself rather than looking like an origin
	// refusal.
	WithdrawalPolicyInvalid WithdrawalReason = "POLICY_INVALID"
)

var allWithdrawalReasons = []WithdrawalReason{
	WithdrawalPolicyInvalid,
	WithdrawalAccountRestricted,
	WithdrawalJurisdictionRestricted,
	WithdrawalProviderUnavailable,
	WithdrawalOriginNotWithdrawable,
	WithdrawalCapabilityInactive,
	WithdrawalRequiresVerification,
	WithdrawalTermsNotAccepted,
	WithdrawalFundingNotSettled,
	WithdrawalHoldPeriodNotElapsed,
	WithdrawalMinimumNotMet,
	WithdrawalNoValue,
}

// WithdrawalReasonCodes returns every reason this explanation can emit, in
// report order (a copy).
func WithdrawalReasonCodes() []WithdrawalReason {
	return append([]WithdrawalReason(nil), allWithdrawalReasons...)
}

var withdrawalReasonRank = func() map[WithdrawalReason]int {
	m := make(map[WithdrawalReason]int, len(allWithdrawalReasons))
	for i, r := range allWithdrawalReasons {
		m[r] = i
	}
	return m
}()

// OriginHolding is what an account holds of one provenance, as
// `credit.Balances` broke it down. The caller supplies it; this package
// computes nothing about a ledger.
type OriginHolding struct {
	Origin valuedomain.CreditOrigin
	// Quantity is everything the account holds of this origin.
	Quantity money.Quantity
	// Finality is the least final funding state among those units. Least,
	// because a bucket containing one reversible lot is a bucket that is not
	// wholly final, and the conservative reading is the true one.
	Finality valuedomain.FundingFinality
	// HeldDays is the age of the YOUNGEST lot in the bucket, for the same
	// reason: the bucket clears a hold period when all of it does.
	HeldDays int
}

// WithdrawalInput is everything the explanation needs. Every field is supplied:
// the policy, the verification level, the capability set, the jurisdiction
// verdict and the provider's answer are deployment and account facts, and
// asking the client for them would let the client choose them.
type WithdrawalInput struct {
	Policy      valuedomain.Policy
	Verified    valuedomain.VerificationLevel
	ActiveCaps  map[valuedomain.CapabilityKey]bool
	Holdings    []OriginHolding
	PolicyValid bool

	// The four figures `credit.Balances` reports, carried through unchanged so
	// the explanation and the balance endpoint cannot disagree.
	Gross          money.Quantity
	Spendable      money.Quantity
	Frozen         money.Quantity
	PayoutEligible money.Quantity

	// JurisdictionSupported is the verification rule table's verdict on where
	// the person is. False adds JURISDICTION_RESTRICTED to every bucket.
	JurisdictionSupported bool
	// AccountRestrictions are the restriction codes recorded against the
	// account and the compliance profile. Non-empty adds ACCOUNT_RESTRICTED.
	AccountRestrictions []string
	// AccountFrozen is an account status that stops everything.
	AccountFrozen bool

	// ProviderAvailable is whether a configured payout provider can actually
	// pay this person. False is the honest state of a deployment with no
	// conversion contract, and it is not the person's fault.
	ProviderAvailable bool
	ProviderName      string
	// MinimumQuantity is the provider's minimum expressed in Credits, or the
	// zero quantity when the provider publishes none. Zero is "no minimum
	// published", never "any amount will do": a caller that cannot compute it
	// leaves it zero and MINIMUM_NOT_MET is simply not reported.
	MinimumQuantity money.Quantity

	// DestinationConfigured is whether the person has a usable payout
	// destination. It contributes to the top-level verdict without being a
	// per-origin reason, because it is a fact about the exit and not about the
	// value.
	DestinationConfigured bool

	// DisclosureAccepted is whether the person has accepted the current
	// WITHDRAWAL_DISCLOSURE. Like DestinationConfigured it is a fact about the
	// person and not about the value, so it lowers the verdict and reports a
	// reason without changing a single bucket.
	//
	// It is false when the caller could not establish it, so a deployment that
	// has not wired the terms registry reports the disclosure as outstanding
	// rather than silently permitting a withdrawal against an unsigned one.
	DisclosureAccepted bool

	// Sandbox marks an explanation computed under sandbox policy, a sandbox
	// verification or a sandbox provider. It is repeated on every response.
	Sandbox bool
}

// OriginBucket is one provenance and what may leave it.
type OriginBucket struct {
	Origin valuedomain.CreditOrigin
	// Quantity is what is held; Withdrawable is what could leave right now.
	Quantity     money.Quantity
	Withdrawable money.Quantity
	// Reasons is why the rest cannot, in report order.
	Reasons []WithdrawalReason
	// The rule that produced the answer, so a screen can say "verified
	// accounts may withdraw this" without a second copy of the policy.
	PayoutAllowed        bool
	RequiredVerification valuedomain.VerificationLevel
	RequiredCapability   valuedomain.CapabilityKey
	MinHoldDays          int
	// VerificationWouldSuffice is true when raising the verification level is
	// the ONLY thing standing between this bucket and its money.
	VerificationWouldSuffice bool
}

// WithdrawalExplanation is the answer to "how much of my balance may leave,
// and why not the rest".
type WithdrawalExplanation struct {
	// Eligible is whether ANY value can leave right now.
	Eligible bool
	// WithdrawableNow is the sum of the buckets' withdrawable amounts. It is
	// derived here rather than taken from the caller so it cannot disagree
	// with the buckets it is shown beside.
	WithdrawableNow money.Quantity

	Gross          money.Quantity
	Spendable      money.Quantity
	Frozen         money.Quantity
	PayoutEligible money.Quantity
	// Ineligible is Gross minus PayoutEligible, restated so a client does not
	// have to subtract.
	Ineligible money.Quantity

	// Buckets is one entry per declared origin, in the canonical origin order,
	// including the empty ones. An origin missing from the list would read as
	// "we did not consider it".
	Buckets []OriginBucket
	// Reasons is the union of the buckets' reasons plus the account-level
	// ones, de-duplicated and in report order.
	Reasons []WithdrawalReason
	// VerificationWouldSuffice is true when verification is the only thing
	// standing between this person and some of their money.
	VerificationWouldSuffice bool

	CurrentVerification  valuedomain.VerificationLevel
	RequiredVerification valuedomain.VerificationLevel

	MinimumQuantity       money.Quantity
	ProviderName          string
	ProviderAvailable     bool
	DestinationConfigured bool
	JurisdictionSupported bool

	PolicyVersion string
	PolicyHash    string
	Sandbox       bool
}

// ExplainWithdrawal composes the four independent decisions into one answer.
//
// The account-level refusals — a frozen or restricted account, an unsupported
// jurisdiction, no usable provider — apply to every bucket, because none of
// them can be worked around by holding a different kind of Credit. The
// per-origin refusals come from the payout policy, evaluated with exactly the
// inputs `valuedomain.Policy.Permits` takes, so this function and the payout
// engine reach the same verdict from the same rules.
func ExplainWithdrawal(in WithdrawalInput) WithdrawalExplanation {
	hash, err := in.Policy.Hash()
	if err != nil {
		hash = ""
	}
	out := WithdrawalExplanation{
		Gross:                 in.Gross,
		Spendable:             in.Spendable,
		Frozen:                in.Frozen,
		PayoutEligible:        in.PayoutEligible,
		Ineligible:            in.Gross.Sub(in.PayoutEligible),
		CurrentVerification:   in.Verified,
		RequiredVerification:  highestRequiredVerification(in.Policy),
		MinimumQuantity:       in.MinimumQuantity,
		ProviderName:          in.ProviderName,
		ProviderAvailable:     in.ProviderAvailable,
		DestinationConfigured: in.DestinationConfigured,
		JurisdictionSupported: in.JurisdictionSupported,
		PolicyVersion:         in.Policy.Version,
		PolicyHash:            hash,
		Sandbox:               in.Sandbox,
	}

	// Account-level refusals. They apply to every bucket because no choice of
	// provenance escapes them.
	var accountLevel []WithdrawalReason
	if !in.PolicyValid {
		accountLevel = append(accountLevel, WithdrawalPolicyInvalid)
	}
	if in.AccountFrozen || len(in.AccountRestrictions) > 0 {
		accountLevel = append(accountLevel, WithdrawalAccountRestricted)
	}
	if !in.JurisdictionSupported {
		accountLevel = append(accountLevel, WithdrawalJurisdictionRestricted)
	}
	if !in.ProviderAvailable {
		accountLevel = append(accountLevel, WithdrawalProviderUnavailable)
	}
	blocked := len(accountLevel) > 0

	held := make(map[valuedomain.CreditOrigin]OriginHolding, len(in.Holdings))
	for _, h := range in.Holdings {
		held[h.Origin] = h
	}

	reasons := newReasonSet()
	for _, r := range accountLevel {
		reasons.add(r)
	}

	total := money.Quantity{}
	for _, origin := range valuedomain.AllOrigins() {
		h := held[origin]
		h.Origin = origin
		bucket := explainOrigin(in, h, accountLevel, blocked)
		total = total.Add(bucket.Withdrawable)
		for _, r := range bucket.Reasons {
			reasons.add(r)
		}
		if bucket.VerificationWouldSuffice {
			out.VerificationWouldSuffice = true
		}
		out.Buckets = append(out.Buckets, bucket)
	}
	out.WithdrawableNow = total

	// The minimum is judged on the WHOLE withdrawable amount rather than per
	// bucket: a payout draws across origins, and refusing each bucket
	// separately for being under a dollar would refuse a ten-dollar payout
	// made of ten one-dollar origins.
	if total.IsPositive() && in.MinimumQuantity.IsPositive() && total.Cmp(in.MinimumQuantity) < 0 {
		reasons.add(WithdrawalMinimumNotMet)
		out.Eligible = false
	} else {
		out.Eligible = total.IsPositive()
	}
	// The disclosure, last, and on the same footing as the minimum: the buckets
	// keep their amounts and the verdict does not. A person is entitled to see
	// what would leave BEFORE they are asked to sign the document about value
	// leaving -- showing them zero until they sign would be the refusal §19
	// says not to make.
	if !in.DisclosureAccepted {
		reasons.add(WithdrawalTermsNotAccepted)
		out.Eligible = false
	}
	out.Reasons = reasons.sorted()
	return out
}

// explainOrigin evaluates one bucket against the same rule the payout engine
// uses, and reports every failing condition rather than the first.
func explainOrigin(in WithdrawalInput, h OriginHolding, accountLevel []WithdrawalReason, blocked bool) OriginBucket {
	rule := in.Policy.Rule(h.Origin)
	bucket := OriginBucket{
		Origin:               h.Origin,
		Quantity:             h.Quantity,
		Withdrawable:         money.Quantity{},
		PayoutAllowed:        rule.PayoutAllowed,
		RequiredVerification: rule.RequiredVerification,
		RequiredCapability:   rule.RequiredCapability,
		MinHoldDays:          rule.MinHoldDays,
	}
	reasons := newReasonSet()
	for _, r := range accountLevel {
		reasons.add(r)
	}
	if !h.Quantity.IsPositive() {
		reasons.add(WithdrawalNoValue)
		bucket.Reasons = reasons.sorted()
		return bucket
	}

	permitted, permitReasons := in.Policy.Permits(valuedomain.PermitInput{
		Origin:      h.Origin,
		Finality:    h.Finality,
		Domain:      valuedomain.InternalCredit,
		Verified:    in.Verified,
		HeldDays:    h.HeldDays,
		ActiveCaps:  in.ActiveCaps,
		PolicyValid: in.PolicyValid,
	})
	for _, r := range permitReasons {
		if mapped, ok := mapPermitReason(r); ok {
			reasons.add(mapped)
		}
	}
	if permitted && !blocked {
		bucket.Withdrawable = h.Quantity
	}

	// Would verifying alone fix it? Re-run the identical evaluation with the
	// level raised and nothing else changed. That is the difference between
	// "you cannot" and "not yet, and here is the step" (§19).
	if !permitted && rule.PayoutAllowed && !in.Verified.AtLeast(rule.RequiredVerification) {
		raised, _ := in.Policy.Permits(valuedomain.PermitInput{
			Origin:      h.Origin,
			Finality:    h.Finality,
			Domain:      valuedomain.InternalCredit,
			Verified:    rule.RequiredVerification,
			HeldDays:    h.HeldDays,
			ActiveCaps:  in.ActiveCaps,
			PolicyValid: in.PolicyValid,
		})
		bucket.VerificationWouldSuffice = raised && !blocked
	}
	bucket.Reasons = reasons.sorted()
	return bucket
}

// mapPermitReason translates the payout policy's vocabulary into the
// user-facing one. Two of its reasons are deliberately dropped: UNKNOWN_ORIGIN
// and UNKNOWN_FUNDING_FINALITY describe a malformed input rather than anything
// a person could act on, and POLICY_INVALID already has its own code.
func mapPermitReason(r valuedomain.PermitReason) (WithdrawalReason, bool) {
	switch r {
	case valuedomain.ReasonOriginForbidden, valuedomain.ReasonDomainNotWithdrawable:
		return WithdrawalOriginNotWithdrawable, true
	case valuedomain.ReasonCapabilityNotActive:
		return WithdrawalCapabilityInactive, true
	case valuedomain.ReasonVerificationTooLow:
		return WithdrawalRequiresVerification, true
	case valuedomain.ReasonFundingNotFinal:
		return WithdrawalFundingNotSettled, true
	case valuedomain.ReasonHoldPeriodNotElapsed:
		return WithdrawalHoldPeriodNotElapsed, true
	case valuedomain.ReasonPolicyInvalid:
		return WithdrawalPolicyInvalid, true
	}
	return "", false
}

// A bucket whose finality the caller did not state is passed through unchanged
// rather than defaulted. `Policy.Permits` reads an undeclared finality as
// UNKNOWN_FUNDING_FINALITY and as not payout-eligible, so the bucket refuses on
// FUNDING_NOT_SETTLED — which is the honest answer. Substituting a default here
// would be this package inventing a fact about somebody's money, and UNFUNDED,
// the tempting default, is payout-eligible.

// highestRequiredVerification is the strictest level any permitting rule
// demands. It is what a screen shows as "you will need", and it is derived from
// the policy rather than assumed to be PAYOUT_KYC.
func highestRequiredVerification(p valuedomain.Policy) valuedomain.VerificationLevel {
	best := valuedomain.VerificationNone
	for _, r := range p.Rules {
		if !r.PayoutAllowed {
			continue
		}
		if r.RequiredVerification.AtLeast(best) {
			best = r.RequiredVerification
		}
	}
	return best
}

type withdrawalReasonSet struct {
	seen map[WithdrawalReason]struct{}
}

func newReasonSet() *withdrawalReasonSet {
	return &withdrawalReasonSet{seen: map[WithdrawalReason]struct{}{}}
}

func (s *withdrawalReasonSet) add(r WithdrawalReason) { s.seen[r] = struct{}{} }

func (s *withdrawalReasonSet) sorted() []WithdrawalReason {
	out := make([]WithdrawalReason, 0, len(s.seen))
	for r := range s.seen {
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return withdrawalReasonRank[out[i]] < withdrawalReasonRank[out[j]]
	})
	return out
}
