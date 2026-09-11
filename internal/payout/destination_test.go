package payout

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// The rule §25 states and this package enforces: Nodal accepts a provider
// token and refuses anything that looks like the thing the token replaces.
//
// The check is heuristic and calibrated in the safe direction. A provider whose
// token is refused is a configuration problem somebody notices in a minute; a
// bank account number this system stores is a liability nobody notices for a
// year.
func TestValidateDestinationToken_RefusesTheThingItReplaces(t *testing.T) {
	t.Parallel()

	accepted := []string{
		"ba_1NXyzAbCdEfGhIjK",              // Stripe-shaped
		"dest-01923f7a-4c2e-7c1b-9f3a-abc", // a uuid handle
		"sandbox-handle-checking-001",
		"acct_1A2b3C|external_account_x",
		// A token that happens to contain a long digit run. It is accepted,
		// because refusing it would be a validator that rejects valid input at
		// random -- a UUID's last group is twelve characters and is all digits
		// about once in three hundred.
		"dest-01923f7a-4c2e-7c1b-9f3a-123456789012",
	}
	for _, token := range accepted {
		assert.NoErrorf(t, ValidateDestinationToken(token), "%q is a plausible provider token", token)
	}

	refused := map[string]string{
		"":                                       "empty",
		"   ":                                    "blank",
		"021000021":                              "a routing number",
		"4242424242424242":                       "a card number",
		"4242 4242 4242 4242":                    "a spaced card number",
		"4242-4242-4242-4242":                    "a hyphenated card number",
		"000123456789":                           "an account number",
		"GB82 WEST 1234 5698 7654 32":            "an IBAN",
		"DE89370400440532013000":                 "an IBAN without spaces",
		"my seed phrase is abandon abandon":      "a seed phrase",
		"-----BEGIN PRIVATE KEY-----":            "a private key",
		"here is my Private Key: 0xdeadbeefcafe": "a private key by name",
		// A card number embedded in a longer string: a thirteen-to-nineteen
		// digit run that also passes the Luhn checksum.
		"pay to card 4242424242424242": "an embedded card number",
	}
	for token, why := range refused {
		err := ValidateDestinationToken(token)
		require.Errorf(t, err, "%s must be refused", why)
		assert.Equalf(t, errs.CodeValidationFailed, errs.CodeOf(err), "%s", why)
	}

	assert.Error(t, ValidateDestinationToken(repeatA(300)), "an implausibly long token is refused")
}

func repeatA(n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = 'a'
	}
	return string(out)
}

func TestValidateMaskedDisplay(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"", "••••4242", "Chase ...4242", "Checking (4242)"} {
		assert.NoErrorf(t, ValidateMaskedDisplay(ok), "%q", ok)
	}
	for _, bad := range []string{"4242424242424242", "021000021", "<script>alert(1)</script>"} {
		err := ValidateMaskedDisplay(bad)
		require.Errorf(t, err, "%q must be refused", bad)
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	}
}

// The destination lifecycle, written out a second time so the implementation is
// compared against a statement of intent rather than against itself.
func TestDestinationLifecycle_EveryEdgeIsTheOneIntended(t *testing.T) {
	t.Parallel()
	want := map[DestinationStatus][]DestinationStatus{
		DestinationUnverified: {DestinationVerified, DestinationRejected, DestinationDisabled},
		DestinationVerified:   {DestinationDisabled, DestinationRejected},
		DestinationRejected:   {},
		DestinationDisabled:   {},
	}
	require.Len(t, want, len(AllDestinationStatuses()))
	for _, from := range AllDestinationStatuses() {
		legal := map[DestinationStatus]bool{}
		for _, to := range want[from] {
			legal[to] = true
		}
		for _, to := range AllDestinationStatuses() {
			assert.Equalf(t, legal[to], CanTransitionDestination(from, to), "%s -> %s", from, to)
		}
		assert.ElementsMatch(t, want[from], DestinationTransitionsFrom(from), "from %s", from)
	}

	// The absence that matters: a destination never comes back. Re-adding one
	// is a new row with its own creation time, which is what lets a cooldown on
	// a changed destination be a fact about the data.
	assert.False(t, CanTransitionDestination(DestinationDisabled, DestinationVerified))
	assert.False(t, CanTransitionDestination(DestinationRejected, DestinationVerified))
	assert.False(t, CanTransitionDestination(DestinationRejected, DestinationUnverified))

	// Only VERIFIED may receive value.
	for _, s := range AllDestinationStatuses() {
		assert.Equalf(t, s == DestinationVerified, s.Usable(), "%s", s)
	}
}

// Provenance is reported in consumption order, and the order is the credit
// package's, not a guess. It is deliberately NOT "purchased first": consumption
// runs from the most restricted origin to the least, and a payout draws its
// ELIGIBLE lots in that same order.
func TestProvenance_ReportsConsumptionOrder(t *testing.T) {
	t.Parallel()
	allocations := []Allocation{
		{Origin: valuedomain.OriginCreatorEarning, Quantity: money.QuantityFromInt64(300)},
		{Origin: valuedomain.OriginPurchased, Quantity: money.QuantityFromInt64(100)},
		{Origin: valuedomain.OriginPurchased, Quantity: money.QuantityFromInt64(50)},
		{Origin: valuedomain.OriginPromotional, Quantity: money.QuantityFromInt64(10)},
		{Origin: valuedomain.OriginPurchased, Quantity: money.QuantityFromInt64(7), Returned: true},
	}
	got := foldProvenance(allocations)

	require.Len(t, got, 4)
	assert.Equal(t, valuedomain.OriginPromotional, got[0].Origin, "the most restricted origin leaves first")
	assert.Equal(t, valuedomain.OriginPurchased, got[1].Origin)
	assert.Equal(t, "150", got[1].Quantity.String(), "slices of one origin are summed")
	assert.Equal(t, valuedomain.OriginCreatorEarning, got[2].Origin)
	assert.True(t, got[3].Returned, "returned slices come last and are never folded into the outstanding ones")
	assert.Equal(t, valuedomain.OriginPurchased, got[3].Origin)
	assert.Equal(t, "7", got[3].Quantity.String())

	for i := range got {
		assert.Equal(t, credit.ConsumptionRank(got[i].Origin), got[i].ConsumptionRank)
	}
	// The ranks of the outstanding slices are non-decreasing, which is the
	// ordering property a client renders.
	for i := 1; i < 3; i++ {
		assert.LessOrEqual(t, got[i-1].ConsumptionRank, got[i].ConsumptionRank)
	}
}

// A decision that reserved nothing still knows which lots it selected, which is
// what a quote and a refused request show.
func TestDecisionProvenance_IsBoundedByTheEligibleAmount(t *testing.T) {
	t.Parallel()
	d := Decision{
		Eligible: money.QuantityFromInt64(120),
		Lots: []credit.Lot{
			{Origin: valuedomain.OriginPurchased, Remaining: money.QuantityFromInt64(100)},
			{Origin: valuedomain.OriginCreatorEarning, Remaining: money.QuantityFromInt64(100)},
		},
	}
	got := DecisionProvenance(d)
	require.Len(t, got, 2)
	assert.Equal(t, valuedomain.OriginPurchased, got[0].Origin)
	assert.Equal(t, "100", got[0].Quantity.String())
	assert.Equal(t, valuedomain.OriginCreatorEarning, got[1].Origin)
	assert.Equal(t, "20", got[1].Quantity.String(), "only the eligible amount is drawn, not the whole lot")

	assert.Empty(t, DecisionProvenance(Decision{}), "a decision that permitted nothing draws on nothing")
}
