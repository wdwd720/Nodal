package credit

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/money"
)

// The answers are BASE UNITS, which is what a money.Quantity is everywhere
// else in this system: the column, the API field and the ledger entry all mean
// base units, and the policy used to return a count of whole Credits into them
// (F-151). One Credit at the shipped scale is 10^6 base units, so $100.00 at
// 100 Credits per dollar is 10,000 Credits, which is 10^10 base units.
func TestPricingPolicy_DefaultIsValidAndPricesTheObviousCase(t *testing.T) {
	t.Parallel()
	p := DefaultPricingPolicy()
	require.NoError(t, p.Validate())
	require.EqualValues(t, 6, p.Decimals, "the scale the CREDIT asset is registered with")

	q, err := p.CreditsFor(money.USDFromMinor(10000)) // $100.00
	require.NoError(t, err)
	require.Equal(t, "10000000000", q.String(), "10,000 Credits at six decimals")

	q, err = p.CreditsFor(money.USDFromMinor(100)) // $1.00, the minimum
	require.NoError(t, err)
	require.Equal(t, "100000000", q.String(), "100 Credits at six decimals")
}

// A policy that prices a zero-decimal Credit issues whole units, and the same
// arithmetic produces them: the scale is a field, not an assumption.
func TestPricingPolicy_TheScaleIsTheAssetsAndNotAConstant(t *testing.T) {
	t.Parallel()
	p := DefaultPricingPolicy()
	p.Decimals = 0
	require.NoError(t, p.Validate())
	q, err := p.CreditsFor(money.USDFromMinor(10000)) // $100.00
	require.NoError(t, err)
	require.Equal(t, "10000", q.String(), "an indivisible Credit: 10,000 of them")

	p.Decimals = 2
	q, err = p.CreditsFor(money.USDFromMinor(10000))
	require.NoError(t, err)
	require.Equal(t, "1000000", q.String(), "the same 10,000 Credits, two decimals wide")
}

func TestPricingPolicy_ClientCannotInfluenceTheAnswer(t *testing.T) {
	t.Parallel()
	// This is the property the whole type exists for. There is no argument to
	// CreditsFor that says how many Credits to issue: the only input is money.
	// A client asking for nine million Credits has nowhere to put the request.
	p := DefaultPricingPolicy()
	a, err := p.CreditsFor(money.USDFromMinor(500))
	require.NoError(t, err)
	b, err := p.CreditsFor(money.USDFromMinor(500))
	require.NoError(t, err)
	require.Equal(t, a.String(), b.String(), "the same amount always buys the same Credits")
	require.Equal(t, "500000000", a.String(), "$5.00 buys 500 Credits, in base units")
}

func TestPricingPolicy_BoundsAreEnforced(t *testing.T) {
	t.Parallel()
	p := DefaultPricingPolicy()

	_, err := p.CreditsFor(money.USDFromMinor(99))
	require.Error(t, err)
	require.Contains(t, err.Error(), "minimum")

	_, err = p.CreditsFor(money.USDFromMinor(p.MaxAmountMinor + 1))
	require.Error(t, err)
	require.Contains(t, err.Error(), "maximum")
}

func TestPricingPolicy_APolicyThatTakesMoneyAndGivesNothingIsRefused(t *testing.T) {
	t.Parallel()
	// One INDIVISIBLE Credit per dollar (Decimals is zero), minimum one cent,
	// rounding down: a customer pays a cent and receives zero Credits. The
	// failure is invisible in testing because nobody tests the bottom of the
	// range, so it is refused at load.
	p := PricingPolicy{
		Version: "bad", Currency: "USD",
		CreditsPerMajorUnit: 1, MinorUnitsPerMajorUnit: 100,
		MinAmountMinor: 1, MaxAmountMinor: 100000,
		Rounding: money.RoundDown, Decimals: 0,
	}
	err := p.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "would take money and give nothing")
}

func TestPricingPolicy_ValidationRejectsIncoherentPolicies(t *testing.T) {
	t.Parallel()
	base := DefaultPricingPolicy()

	cases := map[string]func(p *PricingPolicy){
		"no version":        func(p *PricingPolicy) { p.Version = "" },
		"bad currency":      func(p *PricingPolicy) { p.Currency = "DOLLARS" },
		"zero rate":         func(p *PricingPolicy) { p.CreditsPerMajorUnit = 0 },
		"negative rate":     func(p *PricingPolicy) { p.CreditsPerMajorUnit = -100 },
		"zero minor units":  func(p *PricingPolicy) { p.MinorUnitsPerMajorUnit = 0 },
		"zero minimum":      func(p *PricingPolicy) { p.MinAmountMinor = 0 },
		"max below min":     func(p *PricingPolicy) { p.MaxAmountMinor = 1 },
		"unset rounding":    func(p *PricingPolicy) { p.Rounding = 0 },
		"invalid rounding":  func(p *PricingPolicy) { p.Rounding = money.RoundingMode(99) },
		"negative minimum":  func(p *PricingPolicy) { p.MinAmountMinor = -1 },
		"negative maximum":  func(p *PricingPolicy) { p.MaxAmountMinor = -1 },
		"no minor unit set": func(p *PricingPolicy) { p.MinorUnitsPerMajorUnit = -100 },
		"scale too wide":    func(p *PricingPolicy) { p.Decimals = MaxCreditDecimals + 1 },
	}
	for name, mutate := range cases {
		p := base
		mutate(&p)
		require.Error(t, p.Validate(), name)
	}
}

func TestPricingPolicy_HashIsStableAndCoversEveryField(t *testing.T) {
	t.Parallel()
	base := DefaultPricingPolicy()
	h1, err := base.Hash()
	require.NoError(t, err)
	h2, err := base.Hash()
	require.NoError(t, err)
	require.Equal(t, h1, h2, "a hash that changes between runs is worse than no hash")

	// Every field must move the hash, or a policy could be changed without the
	// change being provable from a stored funding row.
	for name, mutate := range map[string]func(p *PricingPolicy){
		"version":     func(p *PricingPolicy) { p.Version = "other" },
		"currency":    func(p *PricingPolicy) { p.Currency = "EUR" },
		"rate":        func(p *PricingPolicy) { p.CreditsPerMajorUnit = 200 },
		"minor units": func(p *PricingPolicy) { p.MinorUnitsPerMajorUnit = 1000 },
		"minimum":     func(p *PricingPolicy) { p.MinAmountMinor = 200 },
		"maximum":     func(p *PricingPolicy) { p.MaxAmountMinor = 2_000_000 },
		"rounding":    func(p *PricingPolicy) { p.Rounding = money.RoundHalfUp },
		"decimals":    func(p *PricingPolicy) { p.Decimals = 8 },
	} {
		p := base
		mutate(&p)
		h, err := p.Hash()
		require.NoError(t, err, name)
		require.NotEqual(t, h1, h, "changing %s did not change the hash", name)
	}
}

func TestPricingPolicy_RoundingIsHashedByNameNotByOrdinal(t *testing.T) {
	t.Parallel()
	// The RoundingMode constants are an iota block. If somebody reorders them,
	// a policy hashed by ordinal would silently keep its old hash while
	// meaning something different. Hashing the name makes the reorder visible.
	p := DefaultPricingPolicy()
	c, err := p.Canonical()
	require.NoError(t, err)
	require.Contains(t, string(c), `"rounding":"down"`)
}
