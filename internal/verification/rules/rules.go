// Package rules is the versioned, deterministic rule table behind goal §21:
// age, country, state, jurisdiction and sanctions, as data rather than as a
// checkbox.
//
// # Why the table is here and not in the database
//
// These rules decide who may be verified at all, and a rule that can be changed
// by an UPDATE is a rule nobody reviews. They are code, they carry a version
// string, that version is written onto every verification_checks row that was
// judged by them, and changing any threshold is a new version plus a decision
// record. A decision made in March is therefore still explicable in June after
// the rules have changed twice — which is the same property
// `valuedomain.Policy` and `eligibility.Policy` already have.
//
// # Conservative on purpose
//
// The country allowlist is the United States alone, because that is what the
// only payout provider on this deployment says it supports
// (`payoutsandbox.Capabilities().SupportedCountries` is `["US"]`), and because
// offering verification somewhere no provider will pay is an onboarding flow
// that ends in a refusal after the person has handed over identity documents.
// Adding a country is a new version, a new provider capability, and a counsel
// determination (BLOCKERS B-02); it is not an edit to a list.
//
// # What this package must never do
//
//   - Read a clock, a database or a network. It is pure: same version, same
//     jurisdiction, same answer, forever.
//   - Store or accept a date of birth. It answers "what is the minimum age
//     here"; the provider answers "is this person that old".
//   - Infer a jurisdiction from an IP address.
package rules

import (
	"sort"
	"strings"
)

// Version is written onto every check judged by these rules. Changing any
// value below is a new version and a decision record, never an edit.
const Version = "verification-rules-v1-us-only"

// Jurisdiction is where a person is, at the two resolutions that matter. A
// country-level answer is not always the whole answer: PROVIDER_BOUNDARY §3
// records that Stripe's stablecoin payouts exclude New York and Hawaii and that
// Bridge excludes New York by principal address, and discovering that at the end
// of an onboarding flow is the expensive way to find out.
type Jurisdiction struct {
	// Country is an ISO 3166-1 alpha-2 code, upper case. Empty means unknown,
	// which is refused rather than guessed.
	Country string
	// Region is the subdivision code without its country prefix ("CA", "NY").
	// Empty means unknown, which is permitted only where the rules do not
	// depend on it.
	Region string
}

// Normalized upper-cases and trims both halves.
func (j Jurisdiction) Normalized() Jurisdiction {
	return Jurisdiction{
		Country: strings.ToUpper(strings.TrimSpace(j.Country)),
		Region:  strings.ToUpper(strings.TrimSpace(j.Region)),
	}
}

// String renders the jurisdiction canonically, for evidence and logs.
func (j Jurisdiction) String() string {
	n := j.Normalized()
	switch {
	case n.Country == "":
		return "UNKNOWN"
	case n.Region == "":
		return n.Country
	default:
		return n.Country + "-" + n.Region
	}
}

// Refusal is a machine-readable reason a jurisdiction or an age was refused.
type Refusal string

// Refusals.
const (
	// RefusalJurisdictionUnknown is an empty or unparseable country. It is not
	// a denial of that country; it is the absence of an answer, and it fails
	// closed.
	RefusalJurisdictionUnknown Refusal = "JURISDICTION_UNKNOWN"
	// RefusalCountryNotOffered is a country outside the allowlist.
	RefusalCountryNotOffered Refusal = "COUNTRY_NOT_OFFERED"
	// RefusalCountryProhibited is a country on the explicit denylist. It is
	// distinct from NOT_OFFERED because the two have different remedies: one
	// may be added by a business decision, the other may not be added at all.
	RefusalCountryProhibited Refusal = "COUNTRY_PROHIBITED"
	// RefusalRegionRestricted is a subdivision the product is not offered in.
	RefusalRegionRestricted Refusal = "REGION_RESTRICTED"
	// RefusalRegionUnknown is a country whose rules depend on the subdivision,
	// with no subdivision supplied.
	RefusalRegionUnknown Refusal = "REGION_UNKNOWN"
	// RefusalUnderAge is a person below the minimum age for their jurisdiction.
	RefusalUnderAge Refusal = "UNDER_AGE"
	// RefusalAgeUnknown is a provider that did not answer the age question.
	RefusalAgeUnknown Refusal = "AGE_UNKNOWN"
)

// DefaultMinimumAge is the floor everywhere. Eighteen is the age of majority in
// the United States and the age every identity provider in PROVIDER_BOUNDARY §5
// attests against by default (D-058).
const DefaultMinimumAge = 18

// minimumAgeByCountry raises the floor for a whole country. Empty: no country
// on the allowlist has a national minimum above eighteen, and adding one is a
// version bump.
var minimumAgeByCountry = map[string]int{}

// minimumAgeByRegion raises the floor inside a country, keyed "<country>-<region>".
//
// The three United States entries are ages of MAJORITY, not gambling or
// financial-product ages: Alabama and Nebraska are nineteen, Mississippi is
// twenty-one. They are here rather than absent because a person below the age
// of majority in their own state cannot form the contract this product rests on,
// and because the conservative direction of a wrong threshold is upward. They
// are not a legal opinion (D-058); counsel confirms them and BLOCKERS carries
// the item.
var minimumAgeByRegion = map[string]int{
	"US-AL": 19,
	"US-NE": 19,
	"US-MS": 21,
}

// allowedCountries is the country allowlist. An allowlist, never a denylist as
// the primary control: a country nobody has considered is refused rather than
// permitted.
var allowedCountries = map[string]bool{
	"US": true,
}

// prohibitedCountries is an explicit denylist, kept even though the allowlist
// already refuses everything not on it.
//
// It is redundant today and deliberately so. The allowlist is a business
// decision that will widen; the denylist is a sanctions fact that must survive
// that widening, and the failure mode it guards against is somebody adding a
// region to the allowlist without re-deriving the exclusions. These are the
// jurisdictions subject to comprehensive United States sanctions programmes as
// recorded for D-059; the authoritative list is OFAC's, it changes, and keeping
// it current is an operational obligation named in BLOCKERS rather than a
// property of this file.
var prohibitedCountries = map[string]bool{
	"CU": true, // Cuba
	"IR": true, // Iran
	"KP": true, // North Korea
	"SY": true, // Syria
	"RU": true, // Russia — comprehensive restrictions on financial services
	"BY": true, // Belarus
}

// restrictedRegions are subdivisions where the PRODUCT is not offered, keyed by
// country.
//
// Deliberately empty. Which United States states restrict a closed-loop credit
// that becomes convertible at withdrawal is a counsel determination and not an
// engineering one (BLOCKERS B-02), and inventing a list would be exactly the
// fabricated legal conclusion the goal forbids. The subdivision exclusions that
// DO exist today are the payout provider's own — Stripe's stablecoin payouts
// exclude NY and HI, Bridge excludes NY — and those are applied from
// `payout.Capabilities.ExcludedRegions` at the point of payout, not here,
// because they are facts about a rail rather than about a person (D-059).
var restrictedRegions = map[string][]string{}

// regionRequiredCountries are countries whose rules depend on the subdivision,
// so a missing subdivision is refused rather than assumed to be a permissive
// one.
var regionRequiredCountries = map[string]bool{
	"US": true,
}

// AllowedCountries returns the country allowlist, sorted (a copy).
func AllowedCountries() []string {
	out := make([]string, 0, len(allowedCountries))
	for c := range allowedCountries {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// ProhibitedCountries returns the explicit denylist, sorted (a copy).
func ProhibitedCountries() []string {
	out := make([]string, 0, len(prohibitedCountries))
	for c := range prohibitedCountries {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// RestrictedRegions returns the subdivisions the product is not offered in for
// a country, sorted (a copy). Empty for every country today; see the variable.
func RestrictedRegions(country string) []string {
	out := append([]string(nil), restrictedRegions[strings.ToUpper(strings.TrimSpace(country))]...)
	sort.Strings(out)
	return out
}

// MinimumAge returns the minimum age for a jurisdiction, and whether the answer
// depended on a subdivision that was not supplied.
//
// The rule is "the highest applicable floor wins". A region entry can only
// raise the country's figure, and the country's can only raise the default:
// there is no path by which a table entry LOWERS a minimum age, which is the
// property that makes a mistake in this file conservative.
func MinimumAge(j Jurisdiction) (age int, regionKnown bool) {
	n := j.Normalized()
	age = DefaultMinimumAge
	if v, ok := minimumAgeByCountry[n.Country]; ok && v > age {
		age = v
	}
	if n.Region == "" {
		return age, !regionRequiredCountries[n.Country]
	}
	if v, ok := minimumAgeByRegion[n.Country+"-"+n.Region]; ok && v > age {
		age = v
	}
	return age, true
}

// HighestMinimumAge returns the highest floor anywhere in a country, used when
// the subdivision is unknown and the answer must still fail closed.
func HighestMinimumAge(country string) int {
	c := strings.ToUpper(strings.TrimSpace(country))
	age := DefaultMinimumAge
	if v, ok := minimumAgeByCountry[c]; ok && v > age {
		age = v
	}
	for key, v := range minimumAgeByRegion {
		if strings.HasPrefix(key, c+"-") && v > age {
			age = v
		}
	}
	return age
}

// CheckJurisdiction reports whether a jurisdiction may be verified at all, and
// why not when it may not. Every failing condition is reported, not only the
// first, and the reasons come back in the declaration order of Refusal so the
// answer hashes identically however it was evaluated.
func CheckJurisdiction(j Jurisdiction) (bool, []Refusal) {
	n := j.Normalized()
	var refusals []Refusal
	add := func(r Refusal) { refusals = append(refusals, r) }

	if len(n.Country) != 2 || !isAlpha(n.Country) {
		add(RefusalJurisdictionUnknown)
		return false, refusals
	}
	if prohibitedCountries[n.Country] {
		add(RefusalCountryProhibited)
	}
	if !allowedCountries[n.Country] {
		add(RefusalCountryNotOffered)
	}
	if regionRequiredCountries[n.Country] && n.Region == "" {
		add(RefusalRegionUnknown)
	}
	for _, r := range restrictedRegions[n.Country] {
		if r == n.Region {
			add(RefusalRegionRestricted)
			break
		}
	}
	if len(refusals) == 0 {
		return true, nil
	}
	sortRefusals(refusals)
	return false, refusals
}

// CheckAge reports whether an attested age clears the floor for a jurisdiction.
//
// The input is the age a PROVIDER attested — "this person is at least N" — and
// never a date of birth. A provider that attested nothing supplies zero, which
// is AGE_UNKNOWN rather than a pass.
func CheckAge(j Jurisdiction, attestedAtLeast int) (bool, []Refusal) {
	if attestedAtLeast <= 0 {
		return false, []Refusal{RefusalAgeUnknown}
	}
	n := j.Normalized()
	need, regionKnown := MinimumAge(n)
	if !regionKnown {
		// The country's floor depends on the subdivision and there is none, so
		// the only safe comparison is against the highest floor in that
		// country. A person who clears that clears every subdivision of it.
		need = HighestMinimumAge(n.Country)
	}
	if attestedAtLeast < need {
		return false, []Refusal{RefusalUnderAge}
	}
	return true, nil
}

var refusalRank = map[Refusal]int{
	RefusalJurisdictionUnknown: 0,
	RefusalCountryProhibited:   1,
	RefusalCountryNotOffered:   2,
	RefusalRegionRestricted:    3,
	RefusalRegionUnknown:       4,
	RefusalAgeUnknown:          5,
	RefusalUnderAge:            6,
}

func sortRefusals(rs []Refusal) {
	sort.SliceStable(rs, func(i, j int) bool { return refusalRank[rs[i]] < refusalRank[rs[j]] })
}

func isAlpha(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return true
}
