package payout

import (
	"sort"
	"strings"

	"github.com/nodal/controlplane/internal/errs"
)

// RecipientProfile is what a provider needs to be true about the person or
// business receiving value.
//
// It is deliberately small. These are the facts that decide whether a payout
// is possible at all, asked before anyone is sent into an onboarding flow they
// cannot finish.
type RecipientProfile struct {
	// Country is the ISO 3166-1 alpha-2 code of the recipient's country.
	Country string
	// Region is the subdivision within Country -- a US state, say. It is
	// required only when the provider excludes some, and an empty value is
	// then a refusal rather than a pass: "we do not know which state" is not
	// "any state".
	Region string
	// Kind is the legal kind of recipient: an individual, a sole proprietor, a
	// company. The vocabulary is the provider's and Capabilities carries the
	// list it accepts.
	Kind string
}

// Validate checks the profile is answerable at all.
func (p RecipientProfile) Validate() error {
	if len(strings.TrimSpace(p.Country)) != 2 {
		return errs.Newf(errs.CodeValidationFailed,
			"payout: %q is not an ISO 3166-1 alpha-2 country code", p.Country)
	}
	if strings.TrimSpace(p.Kind) == "" {
		return errs.New(errs.CodeValidationFailed, "payout: a recipient profile needs a kind")
	}
	return nil
}

// RecipientRefusal is a machine-readable reason a provider cannot pay a
// recipient. Refusals are returned in a stable order.
type RecipientRefusal string

// Recipient refusals.
const (
	// RefusalProductUnavailable: the provider has not granted this account
	// the product, so no recipient is payable.
	RefusalProductUnavailable RecipientRefusal = "PROVIDER_PRODUCT_UNAVAILABLE"
	// RefusalCountryUnsupported: the provider does not pay this country.
	RefusalCountryUnsupported RecipientRefusal = "RECIPIENT_COUNTRY_UNSUPPORTED"
	// RefusalRegionExcluded: the country is supported and this subdivision is
	// not.
	RefusalRegionExcluded RecipientRefusal = "RECIPIENT_REGION_EXCLUDED"
	// RefusalRegionUnknown: the provider excludes some subdivisions and we do
	// not know which one this recipient is in.
	RefusalRegionUnknown RecipientRefusal = "RECIPIENT_REGION_UNKNOWN"
	// RefusalKindUnsupported: the provider does not pay this kind of
	// recipient, e.g. a company where only individuals are supported.
	RefusalKindUnsupported RecipientRefusal = "RECIPIENT_KIND_UNSUPPORTED"
	// RefusalProfileIncomplete: the profile cannot be evaluated.
	RefusalProfileIncomplete RecipientRefusal = "RECIPIENT_PROFILE_INCOMPLETE"
)

// RecipientKindIndividual is the recipient kind every caller in this build
// asks about.
//
// Nodal has one kind of account holder today: a person. The vocabulary is the
// provider's, `Capabilities.RecipientKinds` carries the list it accepts, and a
// business payout is a product decision nobody has made -- so the value is
// named once here rather than written as a literal at each of the three places
// that ask, which is how one of them ends up asking a different question from
// the others (F-269).
const RecipientKindIndividual = "individual"

// RefusalCodes is the string form of a refusal list, for an error field or a
// response.
func RefusalCodes(rs []RecipientRefusal) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, string(r))
	}
	return out
}

var recipientRefusalRank = map[RecipientRefusal]int{
	RefusalProfileIncomplete:  0,
	RefusalProductUnavailable: 1,
	RefusalCountryUnsupported: 2,
	RefusalRegionUnknown:      3,
	RefusalRegionExcluded:     4,
	RefusalKindUnsupported:    5,
}

// CanPayRecipient reports whether this provider could pay a recipient with
// this profile, and every reason it could not.
//
// It is a pure function of published provider facts, and it exists to be asked
// EARLY. Stripe will refuse a company, a recipient in an unsupported country,
// or one in New York -- but it refuses at the end of an onboarding flow, after
// the user has entered their identity documents. Asking here means telling
// somebody "this cannot work for you" before they hand over a passport, which
// is both kinder and less identity data in the world.
//
// Every failing condition is returned, not just the first, and in a stable
// order, so a stored refusal can be compared with a later one.
func (c Capabilities) CanPayRecipient(p RecipientProfile) (bool, []RecipientRefusal) {
	var out []RecipientRefusal
	add := func(r RecipientRefusal) { out = append(out, r) }

	if err := p.Validate(); err != nil {
		add(RefusalProfileIncomplete)
		// Nothing below can be evaluated against a profile this incomplete.
		return false, out
	}
	if !c.Availability.Usable() {
		add(RefusalProductUnavailable)
	}
	country := strings.ToUpper(strings.TrimSpace(p.Country))

	// An empty country list means the provider publishes none, which is not
	// permission for every country. It is the absence of an answer, and the
	// only safe reading is that no recipient has been confirmed payable.
	if len(c.SupportedCountries) == 0 || !containsFold(c.SupportedCountries, country) {
		add(RefusalCountryUnsupported)
	} else if excl := c.excludedRegionsFor(country); len(excl) > 0 {
		region := strings.ToUpper(strings.TrimSpace(p.Region))
		switch {
		case region == "":
			add(RefusalRegionUnknown)
		case containsFold(excl, region):
			add(RefusalRegionExcluded)
		}
	}
	if !c.SupportsRecipientKind(p.Kind) {
		add(RefusalKindUnsupported)
	}
	if len(out) == 0 {
		return true, nil
	}
	sort.SliceStable(out, func(i, j int) bool {
		return recipientRefusalRank[out[i]] < recipientRefusalRank[out[j]]
	})
	return false, out
}

// excludedRegionsFor returns the subdivisions of a country the provider will
// not pay.
func (c Capabilities) excludedRegionsFor(country string) []string {
	if c.ExcludedRegions == nil {
		return nil
	}
	return c.ExcludedRegions[strings.ToUpper(strings.TrimSpace(country))]
}
