package payout

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func stripeLike(av Availability) Capabilities {
	return Capabilities{
		SupportsCryptoPayout: true,
		SupportsLookup:       true,
		SupportedAssets:      []string{"USDC"},
		SupportedNetworks:    []string{"base", "polygon"},
		RecipientKinds:       []string{"individual", "sole_proprietor"},
		SupportedCountries:   []string{"US", "CA", "GB0"}, // GB0 is deliberately not GB
		ExcludedRegions:      map[string][]string{"US": {"NY", "HI"}},
		Availability:         av,
		ContractReference:    "test",
	}
}

func TestCanPayRecipient_TheOrdinaryYes(t *testing.T) {
	t.Parallel()
	ok, why := stripeLike(AvailabilityLive).CanPayRecipient(RecipientProfile{
		Country: "US", Region: "CA", Kind: "individual",
	})
	require.True(t, ok, "%v", why)
	require.Empty(t, why)
}

func TestCanPayRecipient_ExcludedRegion(t *testing.T) {
	t.Parallel()
	// The case that costs a user a passport if it is discovered late: New York
	// is inside a supported country and is not payable.
	ok, why := stripeLike(AvailabilityLive).CanPayRecipient(RecipientProfile{
		Country: "US", Region: "NY", Kind: "individual",
	})
	require.False(t, ok)
	require.Equal(t, []RecipientRefusal{RefusalRegionExcluded}, why)
}

func TestCanPayRecipient_UnknownRegionIsARefusalNotAPass(t *testing.T) {
	t.Parallel()
	// "We do not know which state" is not "any state". Passing here would send
	// a New Yorker into an onboarding flow that ends in a refusal.
	ok, why := stripeLike(AvailabilityLive).CanPayRecipient(RecipientProfile{
		Country: "US", Kind: "individual",
	})
	require.False(t, ok)
	require.Equal(t, []RecipientRefusal{RefusalRegionUnknown}, why)

	// And a country with no exclusions does not need one.
	ok, _ = stripeLike(AvailabilityLive).CanPayRecipient(RecipientProfile{
		Country: "CA", Kind: "individual",
	})
	require.True(t, ok)
}

func TestCanPayRecipient_UnsupportedCountryAndKind(t *testing.T) {
	t.Parallel()
	ok, why := stripeLike(AvailabilityLive).CanPayRecipient(RecipientProfile{
		Country: "DE", Kind: "company",
	})
	require.False(t, ok)
	// Every failing condition, not just the first, and in a stable order.
	require.Equal(t, []RecipientRefusal{RefusalCountryUnsupported, RefusalKindUnsupported}, why)
}

func TestCanPayRecipient_AnEmptyCountryListIsNotEverywhere(t *testing.T) {
	t.Parallel()
	// An adapter nobody has verified publishes no country list. Reading that
	// as "all countries" would let an unverified integration pay anyone.
	caps := stripeLike(AvailabilityLive)
	caps.SupportedCountries = nil
	ok, why := caps.CanPayRecipient(RecipientProfile{Country: "US", Region: "CA", Kind: "individual"})
	require.False(t, ok)
	require.Contains(t, why, RefusalCountryUnsupported)
}

func TestCanPayRecipient_UngrantedProductRefusesEveryone(t *testing.T) {
	t.Parallel()
	for _, av := range []Availability{
		AvailabilityUnknown, AvailabilityNotOffered, AvailabilityRequiresApplication,
		AvailabilityApplicationPending, AvailabilityApplicationDenied,
	} {
		ok, why := stripeLike(av).CanPayRecipient(RecipientProfile{
			Country: "US", Region: "CA", Kind: "individual",
		})
		require.False(t, ok, "%s", av)
		require.Contains(t, why, RefusalProductUnavailable, "%s", av)
	}
}

func TestCanPayRecipient_IncompleteProfileStopsThere(t *testing.T) {
	t.Parallel()
	// A profile that cannot be evaluated must not produce a list of guesses
	// about what else might be wrong with it.
	for _, p := range []RecipientProfile{
		{Kind: "individual"},
		{Country: "USA", Kind: "individual"},
		{Country: "US"},
	} {
		ok, why := stripeLike(AvailabilityLive).CanPayRecipient(p)
		require.False(t, ok)
		require.Equal(t, []RecipientRefusal{RefusalProfileIncomplete}, why, "%+v", p)
	}
}

func TestCanPayRecipient_MatchingIsCaseInsensitive(t *testing.T) {
	t.Parallel()
	ok, why := stripeLike(AvailabilityLive).CanPayRecipient(RecipientProfile{
		Country: "us", Region: "ca", Kind: "Individual",
	})
	require.True(t, ok, "%v", why)

	ok, why = stripeLike(AvailabilityLive).CanPayRecipient(RecipientProfile{
		Country: "us", Region: "ny", Kind: "individual",
	})
	require.False(t, ok)
	require.Equal(t, []RecipientRefusal{RefusalRegionExcluded}, why,
		"a lowercase state must not slip past an exclusion list")
}
