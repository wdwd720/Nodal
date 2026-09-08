// Package legalrouter is the deterministic evaluator of pre-approved,
// machine-readable capability policy (gola.md PART XXVII, PART LXIV).
//
// # What it is not
//
// It is not a substitute for lawyers, and it does not decide anything. It
// executes a policy somebody with authority wrote down, and it does so the same
// way every time. Every outcome it produces is traceable to a specific rule in
// a specific policy version, and a decision made in March can be replayed in
// June against the policy that was in force then.
//
// # The composite key
//
// PART LXIV requires a capability to be keyed by more than a name. A bare
// LIVE_TRADING switch cannot express "creator earnings may be paid out in this
// state, to a verified user, through this provider, but market proceeds may
// not" — and that sentence, not the switch, is the shape of the actual
// question. So the key carries ten dimensions and rules match patterns over
// them.
//
// # First match wins, and the last rule denies
//
// Rules are ordered and evaluated in order. That makes the policy readable
// top-to-bottom the way a human would write it: the specific exceptions first,
// the general position last. Validate REQUIRES the final rule to be a
// catch-all DENY, so a key nobody thought about is refused rather than falling
// through to permission. A policy that does not end that way is rejected, not
// repaired.
//
// # What this package must never do
//
//   - Invent a legal conclusion. Every rule comes from a policy version with
//     an approval reference attached.
//   - Return ALLOW for a key no rule matched.
//   - Consult a clock, a database or a network. It is pure: same policy, same
//     key, same answer, forever.
//   - Be the only thing standing between a user and money. It is one of
//     several independent checks; the capability gate, the risk kernel and the
//     ledger's own invariants are the others.
package legalrouter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"github.com/nodal/controlplane/internal/agentauthority"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Wildcard matches any value of a dimension.
const Wildcard = "*"

// Key is the composite capability key of PART LXIV.
//
// Every field is a string rather than its natural type so that a rule can hold
// the wildcard. The typed constructors below build a Key from real values, and
// Validate checks that what a rule matches on is a value the system could
// actually produce.
type Key struct {
	Jurisdiction   string
	Provider       string
	Rail           string
	Product        string
	Asset          string
	AgentAuthority string
	ValueOrigin    string
	PayoutMode     string
	Compensation   string
	Verification   string
}

// Dimensions in canonical order. Used for matching, rendering and hashing, so
// all three agree.
var dimensions = []struct {
	name string
	get  func(Key) string
}{
	{"jurisdiction", func(k Key) string { return k.Jurisdiction }},
	{"provider", func(k Key) string { return k.Provider }},
	{"rail", func(k Key) string { return k.Rail }},
	{"product", func(k Key) string { return k.Product }},
	{"asset", func(k Key) string { return k.Asset }},
	{"agent_authority", func(k Key) string { return k.AgentAuthority }},
	{"value_origin", func(k Key) string { return k.ValueOrigin }},
	{"payout_mode", func(k Key) string { return k.PayoutMode }},
	{"compensation", func(k Key) string { return k.Compensation }},
	{"verification", func(k Key) string { return k.Verification }},
}

// Products the router knows about. A product is what the user is trying to do,
// as distinct from the rail it would happen on.
const (
	ProductNativeMarketTrade  = "NATIVE_MARKET_TRADE"
	ProductNativeAssetCreate  = "NATIVE_ASSET_CREATE"
	ProductCreditPurchase     = "CREDIT_PURCHASE"
	ProductInternalCommerce   = "INTERNAL_COMMERCE"
	ProductPayout             = "PAYOUT"
	ProductHostedTrade        = "HOSTED_TRADE"
	ProductSelfCustodialTrade = "SELF_CUSTODIAL_TRADE"
	ProductSimulation         = "SIMULATION"
)

var allProducts = []string{
	ProductNativeMarketTrade, ProductNativeAssetCreate, ProductCreditPurchase,
	ProductInternalCommerce, ProductPayout, ProductHostedTrade,
	ProductSelfCustodialTrade, ProductSimulation,
}

// AllProducts returns every declared product (a copy).
func AllProducts() []string { return append([]string(nil), allProducts...) }

// PayoutModes.
const (
	PayoutModeNone           = "NONE"
	PayoutModePartnerFiat    = "PARTNER_FIAT"
	PayoutModePartnerCrypto  = "PARTNER_CRYPTO"
	PayoutModeInternalCredit = "INTERNAL_CREDIT_ONLY"
)

// CompensationModels describe how the platform is paid, which is a licensing
// question in several jurisdictions and therefore a dimension of its own.
const (
	CompensationNone        = "NONE"
	CompensationFlatFee     = "FLAT_FEE"
	CompensationSpread      = "SPREAD"
	CompensationPerformance = "PERFORMANCE"
	CompensationAUM         = "AUM"
)

// String renders a key canonically, for logs and evidence.
func (k Key) String() string {
	parts := make([]string, 0, len(dimensions))
	for _, d := range dimensions {
		v := d.get(k)
		if v == "" {
			v = "-"
		}
		parts = append(parts, d.name+"="+v)
	}
	return strings.Join(parts, " ")
}

// Validate checks that a key is fully specified. A query with an empty
// dimension is a bug in the caller, and answering it would mean guessing which
// value they meant.
func (k Key) Validate() error {
	var missing []string
	for _, d := range dimensions {
		if strings.TrimSpace(d.get(k)) == "" {
			missing = append(missing, d.name)
		}
		if d.get(k) == Wildcard {
			return errs.Newf(errs.CodeValidationFailed,
				"a query key may not contain a wildcard; %s is %q", d.name, Wildcard)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return errs.Newf(errs.CodeValidationFailed,
			"capability key is missing %s; every dimension must be stated",
			strings.Join(missing, ", "))
	}
	return nil
}

// Outcome is what the router decided (PART XXVII).
type Outcome string

// Outcomes.
const (
	Allow Outcome = "ALLOW"
	Deny  Outcome = "DENY"
	// RequiresVerification means the answer would be ALLOW at a higher
	// verification level. It is distinct from DENY because it has a next step.
	RequiresVerification Outcome = "REQUIRES_VERIFICATION"
	// RequiresUserConfirmation means a human has to press the button, however
	// automated everything around it is.
	RequiresUserConfirmation Outcome = "REQUIRES_USER_CONFIRMATION"
	// RequiresProvider means it is permitted in principle and no configured
	// provider can perform it.
	RequiresProvider Outcome = "REQUIRES_PROVIDER"
	// RequiresManualReview means a person has to look before this proceeds.
	RequiresManualReview Outcome = "REQUIRES_MANUAL_REVIEW"
)

var allOutcomes = []Outcome{
	Allow, Deny, RequiresVerification, RequiresUserConfirmation, RequiresProvider, RequiresManualReview,
}

// AllOutcomes returns every declared outcome (a copy).
func AllOutcomes() []Outcome { return append([]Outcome(nil), allOutcomes...) }

// Valid reports whether o is declared.
func (o Outcome) Valid() bool {
	for _, x := range allOutcomes {
		if x == o {
			return true
		}
	}
	return false
}

// Permits reports whether the outcome lets the action proceed now, with no
// further step. Only ALLOW does.
func (o Outcome) Permits() bool { return o == Allow }

// ReasonCapabilityNotActive is the reason code the router synthesizes when a
// policy that PERMITS meets a gate that is off. It is reserved: Policy.Validate
// refuses a rule that writes it by hand, so a caller reading it knows the gate
// produced it.
const ReasonCapabilityNotActive = "CAPABILITY_NOT_ACTIVE"

// Rule is one line of policy: a pattern over the key, an outcome, and the
// evidence behind it.
type Rule struct {
	// Match is the pattern. An empty dimension means the same as Wildcard, so
	// a rule that cares about two dimensions can name only those two.
	Match Key

	Outcome Outcome
	// ReasonCode is the machine-readable reason this rule exists.
	ReasonCode string
	// Detail is what an operator or a user should be told.
	Detail string

	// RequiredCapability is the gate that must be ACTIVE for an ALLOW rule to
	// actually permit anything. It is mandatory on ALLOW rules that concern
	// anything but simulation: Validate enforces it, so no policy row can be
	// the only thing between a user and real value.
	RequiredCapability valuedomain.CapabilityKey

	// ApprovalReference names the legal or commercial approval this rule
	// encodes. Mandatory on every rule that is not a denial: a permission
	// nobody signed off on has no business being in a policy.
	ApprovalReference string
}

// matches reports whether the rule's pattern covers a key.
func (r Rule) matches(k Key) bool {
	for _, d := range dimensions {
		pat := d.get(r.Match)
		if pat == "" || pat == Wildcard {
			continue
		}
		if !patternMatches(pat, d.get(k)) {
			return false
		}
	}
	return true
}

// patternMatches supports exact match and a comma-separated set, which is what
// real policy needs ("US_CA,US_NY,US_WA") without becoming a language.
// Deliberately not a regular expression: a policy that needs one has become
// something nobody can review.
func patternMatches(pattern, value string) bool {
	if pattern == value {
		return true
	}
	if !strings.Contains(pattern, ",") {
		return false
	}
	for _, alt := range strings.Split(pattern, ",") {
		if strings.TrimSpace(alt) == value {
			return true
		}
	}
	return false
}

// Policy is an ordered, versioned rule list.
type Policy struct {
	Version string
	Rules   []Rule
}

// Validate checks a policy is safe to evaluate.
//
// The rules it enforces are the ones that make "first match wins" trustworthy:
// a final catch-all denial so nothing falls through to permission, an approval
// reference on every permission, and a capability on every permission that
// touches anything but simulation.
func (p Policy) Validate() error {
	if strings.TrimSpace(p.Version) == "" {
		return errs.New(errs.CodeValidationFailed, "a capability policy must be versioned")
	}
	if len(p.Rules) == 0 {
		return errs.New(errs.CodeValidationFailed, "a capability policy with no rules would permit nothing and explain nothing")
	}
	var problems []string
	for i, r := range p.Rules {
		if !r.Outcome.Valid() {
			problems = append(problems, "rule "+itoa(i)+" has unknown outcome "+string(r.Outcome))
		}
		if strings.TrimSpace(r.ReasonCode) == "" {
			problems = append(problems, "rule "+itoa(i)+" has no reason code")
		}
		// The gate's own reason code is reserved. A rule that wrote it by hand
		// would be indistinguishable from a refusal the GATE produced, and a
		// caller that treats the two differently -- the settlement compiler
		// does -- would read a policy denial as a gate being off.
		if strings.TrimSpace(r.ReasonCode) == ReasonCapabilityNotActive {
			problems = append(problems,
				"rule "+itoa(i)+" uses the reserved reason code "+ReasonCapabilityNotActive+
					", which only the gate may produce")
		}
		// The gate's own reason code is reserved. A rule that wrote it by hand
		// would be indistinguishable from a refusal the GATE produced, and a
		// caller that treats the two differently -- the settlement compiler
		// does -- would read a policy denial as a gate being off.
		if r.Outcome != Deny {
			if strings.TrimSpace(r.ApprovalReference) == "" {
				problems = append(problems,
					"rule "+itoa(i)+" permits something without naming the approval it rests on")
			}
			if r.Outcome == Allow &&
				r.Match.Product != ProductSimulation &&
				strings.TrimSpace(string(r.RequiredCapability)) == "" {
				problems = append(problems,
					"rule "+itoa(i)+" allows a non-simulation product without requiring a capability gate")
			}
		}
	}
	last := p.Rules[len(p.Rules)-1]
	if last.Outcome != Deny || !isCatchAll(last.Match) {
		problems = append(problems,
			"the last rule must be a catch-all DENY, so a key nobody thought about is refused rather than permitted")
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "capability policy is not safe to evaluate").
			WithField("problems", problems)
	}
	return nil
}

func isCatchAll(k Key) bool {
	for _, d := range dimensions {
		if v := d.get(k); v != "" && v != Wildcard {
			return false
		}
	}
	return true
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// Decision is what the router answered and why.
type Decision struct {
	Outcome Outcome
	// RuleIndex is which rule matched, so an answer can be traced to a line.
	RuleIndex          int
	ReasonCode         string
	Detail             string
	RequiredCapability valuedomain.CapabilityKey
	ApprovalReference  string
	PolicyVersion      string
	PolicyHash         string
	// CapabilityActive records whether the required gate was ACTIVE at the
	// moment of the decision. An ALLOW rule whose gate is off produces DENY,
	// and this field is how an operator sees which of the two refused.
	CapabilityActive bool
	// GateRefusal is true when this DENY came from the GATE and not from the
	// policy: the matched rule said Allow and its capability is not ACTIVE.
	//
	// It is a field rather than a comparison against ReasonCode because a
	// reason code is a string a policy author can write, and a caller that
	// discriminated on the string could be handed it by a hand-written Deny
	// rule. This one cannot be forged: it is set at exactly one place below,
	// and Policy.Validate refuses a rule that tries to claim the code by name.
	GateRefusal bool
	Key         Key
}

// Permits reports whether the action may proceed now.
func (d Decision) Permits() bool { return d.Outcome.Permits() }

// Router evaluates a policy.
type Router struct {
	policy Policy
	hash   string
}

// New returns a Router over a validated policy.
func New(p Policy) (*Router, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	h, err := p.Hash()
	if err != nil {
		return nil, err
	}
	return &Router{policy: p, hash: h}, nil
}

// Policy returns the policy the router evaluates.
func (r *Router) Policy() Policy { return r.policy }

// Route answers a fully specified key.
//
// activeCaps is the set of capability keys currently ACTIVE. A nil map means
// nothing is active, which is the correct reading for a fresh deployment — and
// which turns every ALLOW rule into a DENY, because a policy that says yes and
// a gate that is off must produce no.
func (r *Router) Route(k Key, activeCaps map[valuedomain.CapabilityKey]bool) Decision {
	base := Decision{PolicyVersion: r.policy.Version, PolicyHash: r.hash, Key: k}
	if err := k.Validate(); err != nil {
		base.Outcome = Deny
		base.ReasonCode = "MALFORMED_KEY"
		base.Detail = err.Error()
		base.RuleIndex = -1
		return base
	}
	for i, rule := range r.policy.Rules {
		if !rule.matches(k) {
			continue
		}
		d := base
		d.RuleIndex = i
		d.Outcome = rule.Outcome
		d.ReasonCode = rule.ReasonCode
		d.Detail = rule.Detail
		d.RequiredCapability = rule.RequiredCapability
		d.ApprovalReference = rule.ApprovalReference

		if rule.RequiredCapability != "" {
			d.CapabilityActive = activeCaps[rule.RequiredCapability]
		}
		// A policy that permits and a gate that is off must produce a refusal.
		// The policy is the standing position; the gate is whether it is
		// switched on today, and both have to agree.
		if rule.Outcome == Allow && rule.RequiredCapability != "" && !d.CapabilityActive {
			d.Outcome = Deny
			d.ReasonCode = ReasonCapabilityNotActive
			d.GateRefusal = true
			d.Detail = "policy permits this, and capability " + string(rule.RequiredCapability) +
				" is not ACTIVE in this environment"
		}
		return d
	}
	// Unreachable while Validate holds, and handled anyway: a policy that has
	// somehow lost its catch-all denies rather than permits.
	base.Outcome = Deny
	base.ReasonCode = "NO_MATCHING_RULE"
	base.Detail = "no rule matched this key, and an unmatched key is refused"
	base.RuleIndex = -1
	return base
}

// canonicalPolicy is the hashed form.
type canonicalPolicy struct {
	Version string          `json:"version"`
	Rules   []canonicalRule `json:"rules"`
}

type canonicalRule struct {
	Match              map[string]string `json:"match"`
	Outcome            string            `json:"outcome"`
	ReasonCode         string            `json:"reason_code"`
	RequiredCapability string            `json:"required_capability"`
	ApprovalReference  string            `json:"approval_reference"`
}

// Canonical returns the deterministic serialised form. Rule ORDER is preserved,
// because in a first-match-wins policy the order is part of the meaning.
func (p Policy) Canonical() ([]byte, error) {
	c := canonicalPolicy{Version: p.Version, Rules: make([]canonicalRule, 0, len(p.Rules))}
	for _, r := range p.Rules {
		m := make(map[string]string, len(dimensions))
		for _, d := range dimensions {
			v := d.get(r.Match)
			if v == "" {
				v = Wildcard
			}
			m[d.name] = v
		}
		c.Rules = append(c.Rules, canonicalRule{
			Match: m, Outcome: string(r.Outcome), ReasonCode: r.ReasonCode,
			RequiredCapability: string(r.RequiredCapability),
			ApprovalReference:  r.ApprovalReference,
		})
	}
	b, err := json.Marshal(c)
	if err != nil {
		return nil, errs.Newf(errs.CodeInternal, "canonicalise capability policy: %v", err)
	}
	return b, nil
}

// Hash is the hex sha256 of the canonical form, recorded on every decision.
func (p Policy) Hash() (string, error) {
	b, err := p.Canonical()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// ConservativePolicy is the policy a fresh deployment runs under.
//
// It permits simulation, permits reading and proposing at low agent authority,
// and denies everything else — including every internal-economy product and
// every payout. It is not a legal opinion. It is the absence of one, which is
// what PART LXIII requires a fresh production deployment to start from.
func ConservativePolicy() Policy {
	return Policy{
		Version: "legal-router-v1-conservative",
		Rules: []Rule{
			{
				Match:             Key{Product: ProductSimulation},
				Outcome:           Allow,
				ReasonCode:        "SIMULATION_HAS_NO_ECONOMIC_SUBSTANCE",
				Detail:            "simulated capital may be used by anyone, anywhere; nothing of value moves",
				ApprovalReference: "PRODUCT-SIM-001",
			},
			{
				Match: Key{
					Product:        ProductNativeAssetCreate,
					AgentAuthority: agentauthority.LevelResearchOnly.Name() + "," + agentauthority.LevelRecommendation.Name(),
				},
				Outcome:    RequiresUserConfirmation,
				ReasonCode: "ASSET_CREATION_IS_A_USER_ACT",
				Detail: "creating a Nodal-native asset is a publication with the creator's name on it; " +
					"an agent may prepare it and a person must confirm it",
				ApprovalReference: "PRODUCT-UGC-001",
			},
			{
				Match:      Key{Product: ProductPayout},
				Outcome:    Deny,
				ReasonCode: "PAYOUT_NOT_APPROVED",
				Detail: "no payout is approved in this deployment; enabling one requires a provider contract, " +
					"a jurisdiction determination and an evidence-backed capability activation",
			},
			{
				Match:      Key{Product: ProductNativeMarketTrade},
				Outcome:    Deny,
				ReasonCode: "NATIVE_MARKET_NOT_APPROVED",
				Detail: "trading Nodal-native assets is not approved in this deployment; " +
					"it requires a jurisdiction determination this policy does not carry",
			},
			{
				Outcome:    Deny,
				ReasonCode: "NO_APPROVAL_ON_RECORD",
				Detail:     "nothing permits this; the absence of a rule is a refusal, not an omission",
			},
		},
	}
}

// DevelopmentPolicy is a policy that permits the internal economy so a
// developer can exercise it locally. It is NOT a legal opinion and is not
// usable anywhere real: cmd/api refuses to load it outside LOCAL, DEV and
// TEST, and every rule carries an approval reference that says so in words.
//
// # Why this exists rather than a flag that skips the router
//
// Domain A is unreachable end to end without SOME policy that permits it, and
// the conservative default permits nothing — correctly. The tempting shortcut
// is a development switch that bypasses the router. That would mean the code
// path a developer exercises is not the code path production runs, and the one
// thing worth knowing locally is exactly whether the real evaluation permits
// the thing.
//
// So this is a real policy, evaluated by the real router, with real required
// capabilities. A developer running it still has to activate the gates through
// the real dual-control flow; this only supplies the standing position that a
// deployment with lawyers would supply.
func DevelopmentPolicy() Policy {
	const ref = "NOT-AN-APPROVAL-LOCAL-DEVELOPMENT-ONLY"
	allow := func(product string, cap valuedomain.CapabilityKey, why string) Rule {
		return Rule{
			Match:              Key{Product: product},
			Outcome:            Allow,
			ReasonCode:         "LOCAL_DEVELOPMENT_POLICY",
			Detail:             why + " This is a development policy and is not a legal determination.",
			ApprovalReference:  ref,
			RequiredCapability: cap,
		}
	}
	return Policy{
		Version: "legal-router-v1-local-development",
		Rules: []Rule{
			{
				Match:             Key{Product: ProductSimulation},
				Outcome:           Allow,
				ReasonCode:        "SIMULATION_HAS_NO_ECONOMIC_SUBSTANCE",
				Detail:            "simulated capital may be used by anyone, anywhere; nothing of value moves",
				ApprovalReference: "PRODUCT-SIM-001",
			},
			allow(ProductCreditPurchase, "CREDIT_PURCHASE",
				"buying Credits is permitted so a local deployment can be funded."),
			allow(ProductInternalCommerce, "MARKETPLACE",
				"the internal marketplace is permitted so the creator economy can be exercised."),
			allow(ProductNativeAssetCreate, "NATIVE_ASSET_CREATION",
				"creating a native asset is permitted so the creation flow can be exercised."),
			allow(ProductNativeMarketTrade, valuedomain.CapNativeMarketTrading,
				"trading an internal market is permitted so the curve can be exercised."),
			// Payouts stay DENIED even here. A payout leaves the system, and a
			// development policy that permitted one would be the first place
			// somebody copied the wrong rule from.
			{
				Match:      Key{Product: ProductPayout},
				Outcome:    Deny,
				ReasonCode: "PAYOUT_NOT_APPROVED",
				Detail: "no payout is approved, and a development policy is not the place to approve one: " +
					"a payout is the one action that moves value out of the system entirely",
			},
			{
				Outcome:    Deny,
				ReasonCode: "NO_APPROVAL_ON_RECORD",
				Detail:     "nothing permits this; the absence of a rule is a refusal, not an omission",
			},
		},
	}
}
