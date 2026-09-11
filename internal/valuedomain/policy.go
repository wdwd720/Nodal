package valuedomain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"github.com/nodal/controlplane/internal/errs"
)

// CapabilityKey names a production capability gate that must be ACTIVE before
// a rule may permit anything.
//
// It is a plain string rather than internal/gates.Capability so that this
// package stays a leaf: gates keys its composite capability on value domains
// and origins, and the dependency must run in exactly one direction.
type CapabilityKey string

// VerificationLevel is how thoroughly the person behind an account has been
// identified. It is deliberately separate from Nodal identity (PART XLVII): a
// user with a verified email and a passkey has a trustworthy Nodal identity
// and no financial identity whatsoever.
type VerificationLevel string

// Verification levels, in increasing order of assurance.
const (
	// VerificationNone is an account that has proven nothing.
	VerificationNone VerificationLevel = "NONE"

	// VerificationNodalIdentity is a verified email address and/or passkey.
	// It says a person can be reached and can log in. It says nothing about
	// who they are, where they are, or whether they may receive money.
	VerificationNodalIdentity VerificationLevel = "NODAL_IDENTITY"

	// VerificationPayoutKYC is identity verification performed by, or on
	// behalf of, a payout provider and accepted by that provider.
	VerificationPayoutKYC VerificationLevel = "PAYOUT_KYC"

	// VerificationEnhanced is enhanced due diligence, required for higher
	// value or higher risk activity where a provider demands it.
	VerificationEnhanced VerificationLevel = "ENHANCED"
)

var verificationRank = map[VerificationLevel]int{
	VerificationNone:          0,
	VerificationNodalIdentity: 1,
	VerificationPayoutKYC:     2,
	VerificationEnhanced:      3,
}

var allVerificationLevels = []VerificationLevel{
	VerificationNone, VerificationNodalIdentity, VerificationPayoutKYC, VerificationEnhanced,
}

// AllVerificationLevels returns every declared level in increasing order of
// assurance (a copy).
func AllVerificationLevels() []VerificationLevel {
	return append([]VerificationLevel(nil), allVerificationLevels...)
}

// Valid reports whether v is a declared level.
func (v VerificationLevel) Valid() bool {
	_, ok := verificationRank[v]
	return ok
}

func (v VerificationLevel) String() string { return string(v) }

// AtLeast reports whether v meets or exceeds need. An unknown level never
// satisfies anything, including itself, so a typo fails closed.
func (v VerificationLevel) AtLeast(need VerificationLevel) bool {
	have, ok := verificationRank[v]
	if !ok {
		return false
	}
	want, ok := verificationRank[need]
	if !ok {
		return false
	}
	return have >= want
}

// ParseVerificationLevel parses the canonical uppercase string form.
func ParseVerificationLevel(s string) (VerificationLevel, error) {
	v := VerificationLevel(strings.ToUpper(strings.TrimSpace(s)))
	if !v.Valid() {
		return "", errs.Newf(errs.CodeValidationFailed, "unknown verification level %q", s)
	}
	return v, nil
}

// OriginRule is the payout treatment of one credit origin under one policy
// version (PART IX, PART XX).
//
// A rule can only ever add requirements. There is no field that makes value
// payable without a capability, because Permits requires an active capability
// unconditionally.
type OriginRule struct {
	// PayoutAllowed is whether this origin may be paid out at all under this
	// policy version. False is the default and means no combination of
	// capability, verification or waiting makes the value withdrawable.
	PayoutAllowed bool `json:"payout_allowed"`

	// RequiredCapability is the gate that must be ACTIVE. It is mandatory
	// whenever PayoutAllowed is true: a rule with no capability is rejected by
	// Validate, so "policy says yes" can never be the only thing standing
	// between a user and a payment.
	RequiredCapability CapabilityKey `json:"required_capability,omitempty"`

	// RequiredVerification is the minimum verification level of the account.
	RequiredVerification VerificationLevel `json:"required_verification"`

	// MinHoldDays is how long value of this origin must age before it may be
	// paid out, counted from the moment the lot was created. It exists for
	// fraud and chargeback reasons and is independent of funding finality.
	MinHoldDays int `json:"min_hold_days"`
}

// Validate checks a rule's internal consistency.
func (r OriginRule) Validate() error {
	if !r.RequiredVerification.Valid() {
		return errs.Newf(errs.CodeValidationFailed, "invalid required verification level %q", r.RequiredVerification)
	}
	if r.MinHoldDays < 0 {
		return errs.New(errs.CodeValidationFailed, "min hold days cannot be negative")
	}
	if r.PayoutAllowed {
		if strings.TrimSpace(string(r.RequiredCapability)) == "" {
			return errs.New(errs.CodeValidationFailed,
				"a rule that allows payout must name the capability gate that authorises it")
		}
		if !r.RequiredVerification.AtLeast(VerificationPayoutKYC) {
			return errs.New(errs.CodeValidationFailed,
				"a rule that allows payout must require at least PAYOUT_KYC verification")
		}
	}
	if !r.PayoutAllowed && r.RequiredCapability != "" {
		return errs.New(errs.CodeValidationFailed,
			"a rule that forbids payout must not name a capability, which would imply the capability could enable it")
	}
	return nil
}

// Policy is a complete, versioned mapping from credit origin to payout
// treatment.
//
// It is data. A change of policy is a new version with its own identifier and
// its own evidence, recorded against every decision made under it, so that a
// decision made last month can still be explained by the rules that were in
// force when it was made.
type Policy struct {
	// Version is the policy identifier recorded on every decision. It must be
	// non-empty and is compared by exact string equality.
	Version string `json:"version"`

	// Rules must contain an entry for every declared origin. A missing origin
	// is a validation failure rather than an implicit denial, because a silent
	// default is exactly how an origin gets forgotten when a new one is added.
	Rules map[CreditOrigin]OriginRule `json:"rules"`
}

// DefaultPolicyVersion is the identifier of the fail-closed policy.
const DefaultPolicyVersion = "payout-policy-v1-closed"

// DefaultPolicy returns the policy a fresh deployment runs under: no origin
// may be paid out, by any user, under any capability.
//
// This is the code-default safe state required by PART I and PART LXIII. It is
// not a legal opinion and it is not a product decision — it is the absence of
// one. Enabling payout for an origin means persisting a new policy version
// through the approval path, not editing this function.
func DefaultPolicy() Policy {
	rules := make(map[CreditOrigin]OriginRule, len(allOrigins))
	for _, o := range allOrigins {
		rules[o] = OriginRule{
			PayoutAllowed:        false,
			RequiredVerification: VerificationNone,
		}
	}
	return Policy{Version: DefaultPolicyVersion, Rules: rules}
}

// Validate checks that the policy names a version and covers every origin.
func (p Policy) Validate() error {
	if strings.TrimSpace(p.Version) == "" {
		return errs.New(errs.CodeValidationFailed, "policy version is required")
	}
	if len(p.Rules) == 0 {
		return errs.New(errs.CodeValidationFailed, "policy has no rules")
	}
	var missing []string
	for _, o := range allOrigins {
		rule, ok := p.Rules[o]
		if !ok {
			missing = append(missing, string(o))
			continue
		}
		if err := rule.Validate(); err != nil {
			return errs.Newf(errs.CodeValidationFailed, "origin %s: %s", o, err.Error())
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return errs.Newf(errs.CodeValidationFailed,
			"policy %s does not cover origin(s) %s; every origin must be explicit",
			p.Version, strings.Join(missing, ", "))
	}
	for o := range p.Rules {
		if !o.Valid() {
			return errs.Newf(errs.CodeValidationFailed, "policy %s contains unknown origin %q", p.Version, o)
		}
	}
	return nil
}

// Rule returns the rule for an origin. An origin the policy does not cover
// returns a forbidding rule, so a malformed policy denies rather than permits.
func (p Policy) Rule(o CreditOrigin) OriginRule {
	r, ok := p.Rules[o]
	if !ok {
		return OriginRule{PayoutAllowed: false, RequiredVerification: VerificationNone}
	}
	return r
}

// PermitReason is a machine-readable reason a payout permission decision came
// out the way it did. Reasons are returned in a stable order.
type PermitReason string

// Permit reasons.
const (
	ReasonOriginForbidden       PermitReason = "ORIGIN_NOT_PAYOUT_ELIGIBLE"
	ReasonCapabilityNotActive   PermitReason = "REQUIRED_CAPABILITY_NOT_ACTIVE"
	ReasonVerificationTooLow    PermitReason = "VERIFICATION_LEVEL_TOO_LOW"
	ReasonFundingNotFinal       PermitReason = "FUNDING_NOT_FINAL"
	ReasonHoldPeriodNotElapsed  PermitReason = "HOLD_PERIOD_NOT_ELAPSED"
	ReasonUnknownOrigin         PermitReason = "UNKNOWN_ORIGIN"
	ReasonUnknownFinality       PermitReason = "UNKNOWN_FUNDING_FINALITY"
	ReasonPolicyInvalid         PermitReason = "POLICY_INVALID"
	ReasonDomainNotWithdrawable PermitReason = "VALUE_DOMAIN_NOT_WITHDRAWABLE"
)

// PermitInput is everything Permits needs. Every field is supplied by the
// caller; this package reads no clock and no store.
type PermitInput struct {
	Origin CreditOrigin
	// OriginFloor is the most restricted origin anywhere in this value's
	// provenance: its own origin when nothing funded it, and the most
	// restricted floor among its parents when something did (D-131).
	//
	// It has no permissive zero value. An empty or undeclared floor is read as
	// UNKNOWN_ORIGIN and refuses, exactly as an empty Origin does, because a
	// caller that has not established where value ultimately came from has not
	// established that it may leave. Every lot the database returns carries one:
	// `credit_lot_state.origin_floor` is NOT NULL and trigger-maintained.
	OriginFloor CreditOrigin
	Finality    FundingFinality
	Domain      Domain
	Verified    VerificationLevel
	HeldDays    int
	ActiveCaps  map[CapabilityKey]bool
	PolicyValid bool
}

// Permits reports whether value described by in may be paid out under p, and
// why not when it may not.
//
// It is a pure function: same inputs, same answer, forever. The reasons are
// returned in the declaration order above rather than the order they were
// discovered, so a decision record hashes identically no matter how the
// evaluation was compiled.
//
// Every failing condition is reported, not just the first. An operator asking
// "why can this user not withdraw" deserves the whole answer.
func (p Policy) Permits(in PermitInput) (bool, []PermitReason) {
	var reasons []PermitReason
	add := func(r PermitReason) { reasons = append(reasons, r) }

	if !in.PolicyValid {
		add(ReasonPolicyInvalid)
	}
	// One reason for both, because a caller that supplied neither has the same
	// problem twice and a duplicated reason is a decision record that hashes
	// differently for no reason anybody can explain.
	if !in.Origin.Valid() || !in.OriginFloor.Valid() {
		add(ReasonUnknownOrigin)
	}
	if !in.Finality.Valid() {
		add(ReasonUnknownFinality)
	}
	// Only internal credit is ever a payout source. Simulated value has no
	// substance; hosted and self-custodial value leaves through its own rail's
	// withdrawal mechanism, not through the Credit payout engine.
	if in.Domain != InternalCredit {
		add(ReasonDomainNotWithdrawable)
	}

	// BOTH the origin and the floor. A derived lot is as withdrawable as the
	// least withdrawable thing that funded it: MARKET_TRADING_PROCEEDS out of a
	// promotional grant is a promotional grant that has been round-tripped, and
	// goal §23 forbids the round trip from changing the answer (D-131).
	//
	// The floor's rule is consulted only for PayoutAllowed. The capability, the
	// verification level and the hold period belong to the value as it is now,
	// and asking a grant's rule for them would demand a capability no rule that
	// forbids payout is even allowed to name (OriginRule.Validate).
	rule := p.Rule(in.Origin)
	floorRule := p.Rule(in.OriginFloor)
	if !rule.PayoutAllowed || !floorRule.PayoutAllowed {
		add(ReasonOriginForbidden)
	}
	if rule.PayoutAllowed {
		// Capability is checked only when the rule would otherwise permit, so
		// the reason list of a forbidden origin does not also complain about a
		// capability that could never have helped.
		if !in.ActiveCaps[rule.RequiredCapability] {
			add(ReasonCapabilityNotActive)
		}
		if !in.Verified.AtLeast(rule.RequiredVerification) {
			add(ReasonVerificationTooLow)
		}
		if in.HeldDays < rule.MinHoldDays {
			add(ReasonHoldPeriodNotElapsed)
		}
	}
	if !in.Finality.PayoutEligible() {
		add(ReasonFundingNotFinal)
	}

	if len(reasons) == 0 {
		return true, nil
	}
	sortReasons(reasons)
	return false, reasons
}

var reasonRank = map[PermitReason]int{
	ReasonPolicyInvalid:         0,
	ReasonUnknownOrigin:         1,
	ReasonUnknownFinality:       2,
	ReasonDomainNotWithdrawable: 3,
	ReasonOriginForbidden:       4,
	ReasonCapabilityNotActive:   5,
	ReasonVerificationTooLow:    6,
	ReasonHoldPeriodNotElapsed:  7,
	ReasonFundingNotFinal:       8,
}

func sortReasons(rs []PermitReason) {
	sort.SliceStable(rs, func(i, j int) bool { return reasonRank[rs[i]] < reasonRank[rs[j]] })
}

// canonicalPolicy is the hashed form. Rules are emitted as a sorted slice
// because Go map iteration order is not stable and a policy hash that changes
// between runs is worse than no hash at all.
type canonicalPolicy struct {
	Version string                `json:"version"`
	Rules   []canonicalPolicyRule `json:"rules"`
}

type canonicalPolicyRule struct {
	Origin               string `json:"origin"`
	PayoutAllowed        bool   `json:"payout_allowed"`
	RequiredCapability   string `json:"required_capability"`
	RequiredVerification string `json:"required_verification"`
	MinHoldDays          int    `json:"min_hold_days"`
}

// Canonical returns the deterministic serialised form of the policy.
func (p Policy) Canonical() ([]byte, error) {
	origins := make([]CreditOrigin, 0, len(p.Rules))
	for o := range p.Rules {
		origins = append(origins, o)
	}
	sort.Slice(origins, func(i, j int) bool { return origins[i] < origins[j] })
	c := canonicalPolicy{Version: p.Version, Rules: make([]canonicalPolicyRule, 0, len(origins))}
	for _, o := range origins {
		r := p.Rules[o]
		c.Rules = append(c.Rules, canonicalPolicyRule{
			Origin:               string(o),
			PayoutAllowed:        r.PayoutAllowed,
			RequiredCapability:   string(r.RequiredCapability),
			RequiredVerification: string(r.RequiredVerification),
			MinHoldDays:          r.MinHoldDays,
		})
	}
	b, err := json.Marshal(c)
	if err != nil {
		return nil, errs.Newf(errs.CodeInternal, "canonicalise policy: %v", err)
	}
	return b, nil
}

// Hash is the hex sha256 of the canonical form, recorded on every decision so
// that a stored policy version can be proven to be the one that was applied.
func (p Policy) Hash() (string, error) {
	b, err := p.Canonical()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
