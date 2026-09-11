package rules

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The rule table is conservative on purpose, and these tests say in what
// direction. A mistake in this file must make somebody harder to verify, never
// easier.

func TestJurisdiction_TheAllowlistIsTheControl(t *testing.T) {
	t.Parallel()

	ok, refusals := CheckJurisdiction(Jurisdiction{Country: "US", Region: "CA"})
	assert.True(t, ok)
	assert.Empty(t, refusals)

	// An unknown jurisdiction is the absence of an answer, not a denial of a
	// particular country, and it fails closed.
	for _, j := range []Jurisdiction{{}, {Country: "usa"}, {Country: "U"}, {Country: "1S"}} {
		ok, refusals = CheckJurisdiction(j)
		assert.Falsef(t, ok, "%+v", j)
		assert.Equalf(t, []Refusal{RefusalJurisdictionUnknown}, refusals, "%+v", j)
	}

	// A country nobody has considered is refused rather than permitted.
	ok, refusals = CheckJurisdiction(Jurisdiction{Country: "GB", Region: "LND"})
	assert.False(t, ok)
	assert.Equal(t, []Refusal{RefusalCountryNotOffered}, refusals)

	// A sanctioned country is refused for a DIFFERENT reason, because the two
	// have different remedies: one may be added by a business decision and the
	// other may not be added at all.
	ok, refusals = CheckJurisdiction(Jurisdiction{Country: "IR"})
	assert.False(t, ok)
	assert.Equal(t, []Refusal{RefusalCountryProhibited, RefusalCountryNotOffered}, refusals)

	// The United States needs a subdivision, because its rules depend on one.
	ok, refusals = CheckJurisdiction(Jurisdiction{Country: "US"})
	assert.False(t, ok)
	assert.Equal(t, []Refusal{RefusalRegionUnknown}, refusals)
}

func TestJurisdiction_NormalisesAndRenders(t *testing.T) {
	t.Parallel()
	n := Jurisdiction{Country: " us ", Region: " ca "}.Normalized()
	assert.Equal(t, "US", n.Country)
	assert.Equal(t, "CA", n.Region)
	assert.Equal(t, "US-CA", n.String())
	assert.Equal(t, "US", Jurisdiction{Country: "US"}.String())
	assert.Equal(t, "UNKNOWN", Jurisdiction{}.String())
}

// Age thresholds. Every entry can only RAISE a floor, so a wrong entry refuses
// somebody who could have been verified rather than verifying somebody who
// could not.
func TestAge_ThresholdsOnlyEverRise(t *testing.T) {
	t.Parallel()

	age, regionKnown := MinimumAge(Jurisdiction{Country: "US", Region: "CA"})
	assert.Equal(t, DefaultMinimumAge, age)
	assert.True(t, regionKnown)

	for region, want := range map[string]int{"AL": 19, "NE": 19, "MS": 21} {
		age, regionKnown = MinimumAge(Jurisdiction{Country: "US", Region: region})
		assert.Equalf(t, want, age, "US-%s", region)
		assert.True(t, regionKnown)
		assert.Greaterf(t, age, DefaultMinimumAge, "US-%s must raise the floor, never lower it", region)
	}

	// A country whose rules depend on a subdivision, with none supplied,
	// reports that the answer is incomplete — and CheckAge then compares
	// against the HIGHEST floor anywhere in that country.
	age, regionKnown = MinimumAge(Jurisdiction{Country: "US"})
	assert.Equal(t, DefaultMinimumAge, age)
	assert.False(t, regionKnown)
	assert.Equal(t, 21, HighestMinimumAge("US"), "the strictest United States subdivision")

	ok, refusals := CheckAge(Jurisdiction{Country: "US"}, 19)
	assert.False(t, ok, "with no subdivision the strictest floor applies")
	assert.Equal(t, []Refusal{RefusalUnderAge}, refusals)
	ok, _ = CheckAge(Jurisdiction{Country: "US"}, 21)
	assert.True(t, ok)
}

func TestAge_UnknownIsNotAPass(t *testing.T) {
	t.Parallel()
	for _, attested := range []int{0, -1} {
		ok, refusals := CheckAge(Jurisdiction{Country: "US", Region: "CA"}, attested)
		assert.False(t, ok)
		assert.Equal(t, []Refusal{RefusalAgeUnknown}, refusals,
			"a provider that attested nothing has not attested that somebody is old enough")
	}
	ok, refusals := CheckAge(Jurisdiction{Country: "US", Region: "MS"}, 18)
	assert.False(t, ok)
	assert.Equal(t, []Refusal{RefusalUnderAge}, refusals)
	ok, _ = CheckAge(Jurisdiction{Country: "US", Region: "MS"}, 21)
	assert.True(t, ok)
}

// The launch shape, asserted rather than assumed: the allowlist is the United
// States alone, and it matches what the only payout provider on this deployment
// says it supports. Widening either without the other is what this catches.
func TestTheLaunchJurisdictionIsUSOnlyAndSaysSo(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []string{"US"}, AllowedCountries())
	assert.NotEmpty(t, ProhibitedCountries(), "the sanctions denylist must not be empty")
	for _, c := range ProhibitedCountries() {
		ok, _ := CheckJurisdiction(Jurisdiction{Country: c, Region: "XX"})
		assert.Falsef(t, ok, "%s is on the denylist and must be refused", c)
	}
	// Deliberately empty: which subdivisions restrict this product is a
	// counsel determination, and the provider's own exclusions are applied
	// from its capabilities rather than invented here.
	assert.Empty(t, RestrictedRegions("US"))
	assert.NotEmpty(t, Version, "every decision records the rule version that produced it")
}
