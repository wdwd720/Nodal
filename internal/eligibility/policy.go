package eligibility

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
)

// ContextKind is the kind of action being checked. Values mirror the
// eligibility_decisions.context_kind CHECK constraint.
type ContextKind string

// Context kinds.
const (
	ContextTrade             ContextKind = "TRADE"
	ContextFunding           ContextKind = "FUNDING"
	ContextWithdrawal        ContextKind = "WITHDRAWAL"
	ContextAgentRun          ContextKind = "AGENT_RUN"
	ContextStrategyPromotion ContextKind = "STRATEGY_PROMOTION"
)

var allContexts = []ContextKind{ContextTrade, ContextFunding, ContextWithdrawal, ContextAgentRun, ContextStrategyPromotion}

// Valid reports whether c is a known context kind.
func (c ContextKind) Valid() bool {
	for _, k := range allContexts {
		if k == c {
			return true
		}
	}
	return false
}

// Identity states mirror compliance_profiles.identity_state.
const (
	IdentityUnverified = "UNVERIFIED"
	IdentityPending    = "PENDING"
	IdentityVerified   = "VERIFIED"
	IdentityRejected   = "REJECTED"
	IdentityExpired    = "EXPIRED"
)

// Sanctions states mirror compliance_profiles.sanctions_state.
const (
	SanctionsUnknown = "UNKNOWN"
	SanctionsClear   = "CLEAR"
	SanctionsHit     = "HIT"
	SanctionsReview  = "REVIEW"
)

var (
	identityStates  = []string{IdentityUnverified, IdentityPending, IdentityVerified, IdentityRejected, IdentityExpired}
	sanctionsStates = []string{SanctionsUnknown, SanctionsClear, SanctionsHit, SanctionsReview}
	riskClasses     = []string{string(assets.RiskSettlement), string(assets.RiskMajor), string(assets.RiskStandard), string(assets.RiskSpeculative), string(assets.RiskUnsupported)}
)

// DefaultRiskClassKey is the key in InstrumentRule.RiskClassesByCountry that
// applies to every country without an explicit entry.
const DefaultRiskClassKey = "*"

// Policy is the typed eligibility rules document stored in
// eligibility_policies.rules. Every list is an explicit allowlist or
// blocklist: an empty allowlist allows nothing. Version is the row version
// (eligibility_policies.version) and is never part of the rules JSON.
type Policy struct {
	Version string `json:"-"`

	// AllowedCountries lists ISO 3166-1 alpha-2 jurisdiction countries that
	// may use the platform at all.
	AllowedCountries []string `json:"allowed_countries"`
	// BlockedRegions lists, per country, the ISO 3166-2 subdivision codes
	// (for example U.S. states) that are blocked even though the country is
	// allowed. A user whose region is unknown in a country with blocked
	// regions is ineligible (ELIGIBILITY_JURISDICTION_UNKNOWN).
	BlockedRegions map[string][]string `json:"blocked_regions"`
	// AllowedResidencyCountries restricts residency. Empty means "the same
	// as allowed_countries".
	AllowedResidencyCountries []string `json:"allowed_residency_countries"`

	// MinimumAge is the default minimum age; MinimumAgeByCountry overrides it
	// per country. AgeVerificationAttests is the age that a positive age
	// verification from the compliance provider attests (for example 18): a
	// jurisdiction requiring more than the verification attests fails closed.
	MinimumAge             int            `json:"minimum_age"`
	MinimumAgeByCountry    map[string]int `json:"minimum_age_by_country"`
	AgeVerificationAttests int            `json:"age_verification_attests"`

	// Baseline allowed states for every context; a ContextRule may replace
	// them with a stricter list.
	IdentityStates       []string `json:"identity_states"`
	SanctionsStates      []string `json:"sanctions_states"`
	AccountStatuses      []string `json:"account_statuses"`
	BlockingRestrictions []string `json:"blocking_restrictions"`

	// Contexts holds one rule per ContextKind. A context without a rule is
	// unavailable (ELIGIBILITY_POLICY_UNKNOWN).
	Contexts map[string]ContextRule `json:"contexts"`
	// Providers holds one rule per provider name. A provider without a rule
	// is unavailable (ELIGIBILITY_PROVIDER).
	Providers map[string]ProviderRule `json:"providers"`
	// Instruments holds the product rules.
	Instruments InstrumentRule `json:"instruments"`
}

// ContextRule is the per-context requirement set. Empty identity, sanctions
// and account-status lists inherit the policy baseline; every other list is
// an explicit allowlist or blocklist for that context.
type ContextRule struct {
	IdentityStates       []string `json:"identity_states"`
	SanctionsStates      []string `json:"sanctions_states"`
	AccountStatuses      []string `json:"account_statuses"`
	BlockingRestrictions []string `json:"blocking_restrictions"`
	// RequiredCapabilities must all be ACTIVE in Input.Capabilities.
	RequiredCapabilities []string `json:"required_capabilities"`
	// AssetClasses and Venues are allowlists checked when the input carries
	// the dimension (TRADE always carries both).
	AssetClasses []string `json:"asset_classes"`
	Venues       []string `json:"venues"`
}

// ProviderRule restricts a funding, execution or compliance provider.
type ProviderRule struct {
	// Contexts the provider may be used in; empty means any context.
	Contexts         []string            `json:"contexts"`
	BlockedCountries []string            `json:"blocked_countries"`
	BlockedRegions   map[string][]string `json:"blocked_regions"`
}

// InstrumentRule is the product rule set.
type InstrumentRule struct {
	// RiskClassesByCountry maps a country (or DefaultRiskClassKey) to the
	// asset risk classes that may be traded there.
	RiskClassesByCountry map[string][]string `json:"risk_classes_by_country"`
	// Statuses lists the instrument statuses that are eligible for a new
	// action; the risk kernel separately distinguishes increasing from
	// reducing exposure.
	Statuses []string `json:"statuses"`
}

// Missing reports whether p is the zero "no policy recorded" value.
func (p Policy) Missing() bool { return p.Version == "" }

// ParsePolicy decodes a rules document. It rejects unknown keys at every
// level, malformed values, non-integer numbers where integers are expected,
// trailing data and unknown enum members, and normalises every list (sorted,
// de-duplicated) so that semantically equal documents hash identically.
func ParsePolicy(raw json.RawMessage) (Policy, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return Policy{}, errs.New(errs.CodeValidationFailed, "eligibility: policy rules must be a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	var p Policy
	if err := dec.Decode(&p); err != nil {
		return Policy{}, errs.Wrap(err, errs.CodeValidationFailed, "eligibility: policy rules do not parse: "+err.Error())
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Policy{}, errs.New(errs.CodeValidationFailed, "eligibility: policy rules contain trailing data")
	}
	p.normalize()
	if err := p.Validate(); err != nil {
		return Policy{}, err
	}
	return p, nil
}

// MustParsePolicy is ParsePolicy for compiled-in documents; it panics on error.
func MustParsePolicy(raw json.RawMessage) Policy {
	p, err := ParsePolicy(raw)
	if err != nil {
		panic("eligibility: MustParsePolicy: " + err.Error())
	}
	return p
}

// Hash returns the hex SHA-256 of the canonical JSON of the rules (Version
// excluded). It is the value stored in eligibility_policies.rules_hash.
func (p Policy) Hash() string { return hashOf(p) }

// CanonicalJSON renders the normalised rules document.
func (p Policy) CanonicalJSON() ([]byte, error) { return canonicalJSON(p) }

func (p *Policy) normalize() {
	p.AllowedCountries = sortedUnique(p.AllowedCountries)
	p.BlockedRegions = normalizeListMap(p.BlockedRegions)
	p.AllowedResidencyCountries = sortedUnique(p.AllowedResidencyCountries)
	if p.MinimumAgeByCountry == nil {
		p.MinimumAgeByCountry = map[string]int{}
	}
	p.IdentityStates = sortedUnique(p.IdentityStates)
	p.SanctionsStates = sortedUnique(p.SanctionsStates)
	p.AccountStatuses = sortedUnique(p.AccountStatuses)
	p.BlockingRestrictions = sortedUnique(p.BlockingRestrictions)
	if p.Contexts == nil {
		p.Contexts = map[string]ContextRule{}
	}
	for k, c := range p.Contexts {
		c.IdentityStates = sortedUnique(c.IdentityStates)
		c.SanctionsStates = sortedUnique(c.SanctionsStates)
		c.AccountStatuses = sortedUnique(c.AccountStatuses)
		c.BlockingRestrictions = sortedUnique(c.BlockingRestrictions)
		c.RequiredCapabilities = sortedUnique(c.RequiredCapabilities)
		c.AssetClasses = sortedUnique(c.AssetClasses)
		c.Venues = sortedUnique(c.Venues)
		p.Contexts[k] = c
	}
	if p.Providers == nil {
		p.Providers = map[string]ProviderRule{}
	}
	for k, r := range p.Providers {
		r.Contexts = sortedUnique(r.Contexts)
		r.BlockedCountries = sortedUnique(r.BlockedCountries)
		r.BlockedRegions = normalizeListMap(r.BlockedRegions)
		p.Providers[k] = r
	}
	p.Instruments.RiskClassesByCountry = normalizeListMap(p.Instruments.RiskClassesByCountry)
	p.Instruments.Statuses = sortedUnique(p.Instruments.Statuses)
}

// Validate checks every enum member and code format. It never depends on the
// order in which map entries are visited: problems are sorted before they
// are reported.
func (p Policy) Validate() error {
	var problems []string
	add := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }

	for _, c := range p.AllowedCountries {
		if !validCountry(c) {
			add("allowed_countries: %q is not an upper-case ISO 3166-1 alpha-2 code", c)
		}
	}
	for c, regions := range p.BlockedRegions {
		if !validCountry(c) {
			add("blocked_regions: key %q is not an upper-case ISO 3166-1 alpha-2 code", c)
		}
		for _, r := range regions {
			if !validRegion(r) {
				add("blocked_regions[%s]: %q is not an upper-case region code", c, r)
			}
		}
	}
	for _, c := range p.AllowedResidencyCountries {
		if !validCountry(c) {
			add("allowed_residency_countries: %q is not an upper-case ISO 3166-1 alpha-2 code", c)
		}
	}
	if p.MinimumAge < 0 || p.MinimumAge > 150 {
		add("minimum_age: %d out of range", p.MinimumAge)
	}
	for c, age := range p.MinimumAgeByCountry {
		if !validCountry(c) {
			add("minimum_age_by_country: key %q is not an upper-case ISO 3166-1 alpha-2 code", c)
		}
		if age < 0 || age > 150 {
			add("minimum_age_by_country[%s]: %d out of range", c, age)
		}
	}
	if p.AgeVerificationAttests < 0 || p.AgeVerificationAttests > 150 {
		add("age_verification_attests: %d out of range", p.AgeVerificationAttests)
	}
	validateStates("identity_states", p.IdentityStates, identityStates, add)
	validateStates("sanctions_states", p.SanctionsStates, sanctionsStates, add)
	validateAccountStatuses("account_statuses", p.AccountStatuses, add)
	validateNonEmpty("blocking_restrictions", p.BlockingRestrictions, add)

	for k, c := range p.Contexts {
		if !ContextKind(k).Valid() {
			add("contexts: %q is not a context kind", k)
		}
		prefix := "contexts[" + k + "]."
		validateStates(prefix+"identity_states", c.IdentityStates, identityStates, add)
		validateStates(prefix+"sanctions_states", c.SanctionsStates, sanctionsStates, add)
		validateAccountStatuses(prefix+"account_statuses", c.AccountStatuses, add)
		validateNonEmpty(prefix+"blocking_restrictions", c.BlockingRestrictions, add)
		validateNonEmpty(prefix+"required_capabilities", c.RequiredCapabilities, add)
		validateNonEmpty(prefix+"asset_classes", c.AssetClasses, add)
		validateNonEmpty(prefix+"venues", c.Venues, add)
	}
	for name, r := range p.Providers {
		if strings.TrimSpace(name) == "" {
			add("providers: empty provider name")
		}
		prefix := "providers[" + name + "]."
		for _, c := range r.Contexts {
			if !ContextKind(c).Valid() {
				add("%scontexts: %q is not a context kind", prefix, c)
			}
		}
		for _, c := range r.BlockedCountries {
			if !validCountry(c) {
				add("%sblocked_countries: %q is not an upper-case ISO 3166-1 alpha-2 code", prefix, c)
			}
		}
		for c, regions := range r.BlockedRegions {
			if !validCountry(c) {
				add("%sblocked_regions: key %q is not an upper-case ISO 3166-1 alpha-2 code", prefix, c)
			}
			for _, reg := range regions {
				if !validRegion(reg) {
					add("%sblocked_regions[%s]: %q is not an upper-case region code", prefix, c, reg)
				}
			}
		}
	}
	for c, classes := range p.Instruments.RiskClassesByCountry {
		if c != DefaultRiskClassKey && !validCountry(c) {
			add("instruments.risk_classes_by_country: key %q is neither %q nor a country code", c, DefaultRiskClassKey)
		}
		validateStates("instruments.risk_classes_by_country["+c+"]", classes, riskClasses, add)
	}
	for _, s := range p.Instruments.Statuses {
		if !assets.Status(s).Valid() {
			add("instruments.statuses: %q is not an instrument status", s)
		}
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return errs.New(errs.CodeValidationFailed, "eligibility: invalid policy rules").WithField("problems", problems)
}

func validateStates(field string, got, allowed []string, add func(string, ...any)) {
	for _, s := range got {
		if !contains(allowed, s) {
			add("%s: %q is not a recognized value", field, s)
		}
	}
}

func validateAccountStatuses(field string, got []string, add func(string, ...any)) {
	for _, s := range got {
		if !accounts.Status(s).Valid() {
			add("%s: %q is not an account status", field, s)
		}
	}
}

func validateNonEmpty(field string, got []string, add func(string, ...any)) {
	for _, s := range got {
		if strings.TrimSpace(s) == "" || s != strings.TrimSpace(s) {
			add("%s: entries must be non-empty and trimmed", field)
			return
		}
	}
}

func validCountry(c string) bool {
	if len(c) != 2 {
		return false
	}
	return c[0] >= 'A' && c[0] <= 'Z' && c[1] >= 'A' && c[1] <= 'Z'
}

func validRegion(r string) bool {
	if r == "" || len(r) > 8 {
		return false
	}
	for i := 0; i < len(r); i++ {
		ch := r[i]
		if (ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') && ch != '-' {
			return false
		}
	}
	return true
}

func sortedUnique(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func normalizeListMap(m map[string][]string) map[string][]string {
	out := make(map[string][]string, len(m))
	for k, v := range m {
		out[k] = sortedUnique(v)
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// DefaultPolicyJSON is the compiled-in policy used only to seed a fresh
// deployment. It is deliberately conservative: the country allowlist is
// EMPTY, so every evaluation is ineligible (ELIGIBILITY_JURISDICTION) until
// an operator records a real policy through Store.RecordPolicy after legal
// review (blockers EB-001, EB-006). Production must never run on this
// document; tests use fixture policies under testdata/policies.
const DefaultPolicyJSON = `{
  "allowed_countries": [],
  "blocked_regions": {},
  "allowed_residency_countries": [],
  "minimum_age": 18,
  "minimum_age_by_country": {},
  "age_verification_attests": 18,
  "identity_states": ["VERIFIED"],
  "sanctions_states": ["CLEAR"],
  "account_statuses": ["ACTIVE"],
  "blocking_restrictions": ["COMPLIANCE_HOLD"],
  "contexts": {
    "TRADE": {
      "required_capabilities": ["LIVE_MANUAL_TRADING"],
      "blocking_restrictions": ["NO_TRADING"],
      "asset_classes": [],
      "venues": []
    },
    "FUNDING": {
      "required_capabilities": ["LIVE_FUNDING"],
      "blocking_restrictions": ["NO_FUNDING"]
    },
    "WITHDRAWAL": {
      "identity_states": ["VERIFIED"],
      "sanctions_states": ["CLEAR"],
      "required_capabilities": ["WITHDRAWALS"],
      "blocking_restrictions": ["NO_WITHDRAWALS"]
    },
    "AGENT_RUN": {
      "required_capabilities": ["LIVE_AGENT_TRADING"],
      "blocking_restrictions": ["NO_TRADING", "NO_AGENTS"]
    },
    "STRATEGY_PROMOTION": {
      "required_capabilities": ["LIVE_AGENT_TRADING"],
      "blocking_restrictions": ["NO_AGENTS"]
    }
  },
  "providers": {},
  "instruments": {
    "risk_classes_by_country": {},
    "statuses": ["ACTIVE"]
  }
}`
