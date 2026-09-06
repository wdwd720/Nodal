package eligibility

import (
	"sort"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
)

// Reason codes. Every failing dimension contributes its code; the decision
// carries the sorted, de-duplicated set.
const (
	ReasonPolicyMissing       = "ELIGIBILITY_POLICY_MISSING"
	ReasonPolicyUnknown       = "ELIGIBILITY_POLICY_UNKNOWN"
	ReasonInputInvalid        = "ELIGIBILITY_INPUT_INVALID"
	ReasonJurisdictionUnknown = "ELIGIBILITY_JURISDICTION_UNKNOWN"
	ReasonJurisdiction        = "ELIGIBILITY_JURISDICTION"
	ReasonRegionBlocked       = "ELIGIBILITY_REGION_BLOCKED"
	ReasonResidency           = "ELIGIBILITY_RESIDENCY"
	ReasonAge                 = "ELIGIBILITY_AGE"
	ReasonIdentityState       = "ELIGIBILITY_IDENTITY_STATE"
	ReasonSanctions           = "ELIGIBILITY_SANCTIONS"
	ReasonAccountStatus       = "ELIGIBILITY_ACCOUNT_STATUS"
	ReasonAccountRestriction  = "ELIGIBILITY_ACCOUNT_RESTRICTION"
	ReasonInstrumentRiskClass = "ELIGIBILITY_INSTRUMENT_RISK_CLASS"
	ReasonInstrumentStatus    = "ELIGIBILITY_INSTRUMENT_STATUS"
	ReasonAssetClass          = "ELIGIBILITY_ASSET_CLASS"
	ReasonVenue               = "ELIGIBILITY_VENUE"
	ReasonProvider            = "ELIGIBILITY_PROVIDER"
	ReasonCapability          = "ELIGIBILITY_CAPABILITY"
)

var allReasons = []string{
	ReasonPolicyMissing, ReasonPolicyUnknown, ReasonInputInvalid, ReasonJurisdictionUnknown, ReasonJurisdiction,
	ReasonRegionBlocked, ReasonResidency, ReasonAge, ReasonIdentityState, ReasonSanctions, ReasonAccountStatus,
	ReasonAccountRestriction, ReasonInstrumentRiskClass, ReasonInstrumentStatus, ReasonAssetClass, ReasonVenue,
	ReasonProvider, ReasonCapability,
}

// ReasonCodes returns every reason code this engine can emit, sorted.
func ReasonCodes() []string {
	out := append([]string(nil), allReasons...)
	sort.Strings(out)
	return out
}

// Input is the typed evaluation context. IDs are canonical UUID strings and
// take no part in the outcome; they are hashed into ContextHash and persisted
// with the decision. Country and region codes are upper-cased before
// evaluation.
type Input struct {
	AccountID string `json:"account_id"`
	UserID    string `json:"user_id"`
	IntentID  string `json:"intent_id"`

	Context ContextKind `json:"context"`

	IdentityState       string `json:"identity_state"`
	AgeVerified         bool   `json:"age_verified"`
	JurisdictionCountry string `json:"jurisdiction_country"`
	JurisdictionRegion  string `json:"jurisdiction_region"`
	ResidencyCountry    string `json:"residency_country"`
	SanctionsState      string `json:"sanctions_state"`

	AccountStatus accounts.Status `json:"account_status"`
	Restrictions  []string        `json:"restrictions"`

	InstrumentID        string           `json:"instrument_id"`
	InstrumentRiskClass assets.RiskClass `json:"instrument_risk_class"`
	InstrumentStatus    assets.Status    `json:"instrument_status"`
	AssetClass          string           `json:"asset_class"`
	Venue               string           `json:"venue"`
	Provider            string           `json:"provider"`

	// Capabilities maps capability name to whether the production gate is
	// ACTIVE for this environment right now (gates.Checker output).
	Capabilities map[string]bool `json:"capabilities"`

	// Now is the evaluation time supplied by the caller; the engine never
	// reads a clock.
	Now time.Time `json:"now"`
}

// Decision is the persisted outcome of one evaluation.
type Decision struct {
	Eligible      bool      `json:"eligible"`
	PolicyVersion string    `json:"policy_version"`
	PolicyHash    string    `json:"policy_hash"`
	ReasonCodes   []string  `json:"reason_codes"`
	EvaluatedAt   time.Time `json:"evaluated_at"`
	// ContextHash is the SHA-256 of the canonical JSON of (policy version,
	// policy hash, normalised input).
	ContextHash string `json:"context_hash"`
	// Hash is the SHA-256 of the canonical JSON of the decision itself with
	// Hash blank: the determinism witness.
	Hash string `json:"hash"`
}

// CanonicalJSON renders the decision deterministically.
func (d Decision) CanonicalJSON() ([]byte, error) { return canonicalJSON(d) }

// ComputeHash returns the hash a decision with these fields must carry.
func (d Decision) ComputeHash() string {
	d.Hash = ""
	return hashOf(d)
}

type contextEnvelope struct {
	PolicyVersion string `json:"policy_version"`
	PolicyHash    string `json:"policy_hash"`
	Input         Input  `json:"input"`
}

// Evaluate is the pure eligibility function: no I/O, no clock, no
// randomness, no map-order dependence. It returns every failing reason.
func Evaluate(p Policy, in Input) Decision {
	in = in.normalized()
	rs := reasonSet{}
	if in.Now.IsZero() {
		rs.add(ReasonInputInvalid)
	}
	if p.Missing() {
		rs.add(ReasonPolicyMissing)
	} else {
		evaluateRules(p, in, rs)
	}
	d := Decision{
		Eligible:      len(rs) == 0,
		PolicyVersion: p.Version,
		ReasonCodes:   rs.sorted(),
		EvaluatedAt:   in.Now,
	}
	if !p.Missing() {
		d.PolicyHash = p.Hash()
	}
	d.ContextHash = hashOf(contextEnvelope{PolicyVersion: p.Version, PolicyHash: d.PolicyHash, Input: in})
	d.Hash = d.ComputeHash()
	return d
}

func evaluateRules(p Policy, in Input, rs reasonSet) {
	ctxRule, hasCtx := p.Contexts[string(in.Context)]
	if !in.Context.Valid() || !hasCtx {
		rs.add(ReasonPolicyUnknown)
	}

	country := in.JurisdictionCountry
	if country == "" {
		rs.add(ReasonJurisdictionUnknown)
	} else {
		if !contains(p.AllowedCountries, country) {
			rs.add(ReasonJurisdiction)
		}
		if blocked := p.BlockedRegions[country]; len(blocked) > 0 {
			switch {
			case in.JurisdictionRegion == "":
				rs.add(ReasonJurisdictionUnknown)
			case contains(blocked, in.JurisdictionRegion):
				rs.add(ReasonRegionBlocked)
			}
		}
	}

	residency := p.AllowedResidencyCountries
	if len(residency) == 0 {
		residency = p.AllowedCountries
	}
	if in.ResidencyCountry == "" || !contains(residency, in.ResidencyCountry) {
		rs.add(ReasonResidency)
	}

	requiredAge := p.MinimumAge
	if v, ok := p.MinimumAgeByCountry[country]; ok {
		requiredAge = v
	}
	if requiredAge > 0 && (!in.AgeVerified || p.AgeVerificationAttests < requiredAge) {
		rs.add(ReasonAge)
	}

	switch {
	case !contains(identityStates, in.IdentityState):
		rs.add(ReasonPolicyUnknown)
	case !contains(firstNonEmpty(ctxRule.IdentityStates, p.IdentityStates), in.IdentityState):
		rs.add(ReasonIdentityState)
	}
	switch {
	case !contains(sanctionsStates, in.SanctionsState):
		rs.add(ReasonPolicyUnknown)
	case !contains(firstNonEmpty(ctxRule.SanctionsStates, p.SanctionsStates), in.SanctionsState):
		rs.add(ReasonSanctions)
	}
	switch {
	case !in.AccountStatus.Valid():
		rs.add(ReasonPolicyUnknown)
	case !contains(firstNonEmpty(ctxRule.AccountStatuses, p.AccountStatuses), string(in.AccountStatus)):
		rs.add(ReasonAccountStatus)
	}

	for _, r := range in.Restrictions {
		if contains(p.BlockingRestrictions, r) || contains(ctxRule.BlockingRestrictions, r) {
			rs.add(ReasonAccountRestriction)
		}
	}
	for _, c := range ctxRule.RequiredCapabilities {
		if !in.Capabilities[c] {
			rs.add(ReasonCapability)
		}
	}

	// Dimension completeness per context: an input that omits a dimension the
	// context needs cannot be evaluated and fails closed.
	switch in.Context {
	case ContextTrade:
		if in.InstrumentRiskClass == "" || in.InstrumentStatus == "" || in.AssetClass == "" || in.Venue == "" {
			rs.add(ReasonPolicyUnknown)
		}
	case ContextFunding, ContextWithdrawal:
		if in.Provider == "" {
			rs.add(ReasonPolicyUnknown)
		}
	}

	if in.InstrumentRiskClass != "" {
		if !contains(riskClasses, string(in.InstrumentRiskClass)) {
			rs.add(ReasonPolicyUnknown)
		} else {
			classes, ok := p.Instruments.RiskClassesByCountry[country]
			if !ok {
				classes = p.Instruments.RiskClassesByCountry[DefaultRiskClassKey]
			}
			if !contains(classes, string(in.InstrumentRiskClass)) {
				rs.add(ReasonInstrumentRiskClass)
			}
		}
	}
	if in.InstrumentStatus != "" {
		switch {
		case !in.InstrumentStatus.Valid():
			rs.add(ReasonPolicyUnknown)
		case !contains(p.Instruments.Statuses, string(in.InstrumentStatus)):
			rs.add(ReasonInstrumentStatus)
		}
	}
	if in.AssetClass != "" && !contains(ctxRule.AssetClasses, in.AssetClass) {
		rs.add(ReasonAssetClass)
	}
	if in.Venue != "" && !contains(ctxRule.Venues, in.Venue) {
		rs.add(ReasonVenue)
	}
	if in.Provider != "" {
		rule, ok := p.Providers[in.Provider]
		switch {
		case !ok:
			rs.add(ReasonProvider)
		case len(rule.Contexts) > 0 && !contains(rule.Contexts, string(in.Context)):
			rs.add(ReasonProvider)
		case contains(rule.BlockedCountries, country):
			rs.add(ReasonProvider)
		default:
			if blocked := rule.BlockedRegions[country]; len(blocked) > 0 {
				if in.JurisdictionRegion == "" || contains(blocked, in.JurisdictionRegion) {
					rs.add(ReasonProvider)
				}
			}
		}
	}
}

func firstNonEmpty(a, b []string) []string {
	if len(a) > 0 {
		return a
	}
	return b
}

// normalized upper-cases codes, trims identifiers, sorts restrictions and
// replaces nil collections with empty ones so equal inputs hash equally.
func (in Input) normalized() Input {
	in.Context = ContextKind(strings.TrimSpace(string(in.Context)))
	in.IdentityState = strings.TrimSpace(in.IdentityState)
	in.SanctionsState = strings.TrimSpace(in.SanctionsState)
	in.JurisdictionCountry = strings.ToUpper(strings.TrimSpace(in.JurisdictionCountry))
	in.JurisdictionRegion = strings.ToUpper(strings.TrimSpace(in.JurisdictionRegion))
	in.ResidencyCountry = strings.ToUpper(strings.TrimSpace(in.ResidencyCountry))
	in.AccountStatus = accounts.Status(strings.TrimSpace(string(in.AccountStatus)))
	in.InstrumentRiskClass = assets.RiskClass(strings.TrimSpace(string(in.InstrumentRiskClass)))
	in.InstrumentStatus = assets.Status(strings.TrimSpace(string(in.InstrumentStatus)))
	in.AssetClass = strings.TrimSpace(in.AssetClass)
	in.Venue = strings.TrimSpace(in.Venue)
	in.Provider = strings.TrimSpace(in.Provider)
	in.Restrictions = sortedUnique(in.Restrictions)
	caps := make(map[string]bool, len(in.Capabilities))
	for k, v := range in.Capabilities {
		caps[k] = v
	}
	in.Capabilities = caps
	in.Now = in.Now.UTC()
	return in
}

type reasonSet map[string]struct{}

func (r reasonSet) add(code string) { r[code] = struct{}{} }

func (r reasonSet) sorted() []string {
	out := make([]string, 0, len(r))
	for c := range r {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}
