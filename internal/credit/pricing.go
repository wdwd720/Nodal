package credit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// PricingPolicy converts an amount of real money into a number of Credits.
//
// It exists because of one sentence in the goal document: "Never trust
// browser-provided Credit amount." A client that can say how many Credits it
// is buying can say nine million, and the only thing standing between that
// request and a ledger issuance would be a validation somebody remembered to
// write. Here there is nothing to remember: the request carries an amount of
// money and a policy version, and the Credit quantity is a function of those
// two things. There is no field anywhere in the API that carries a Credit
// amount into a purchase.
//
// It is versioned and hashed for the same reason valuedomain.Policy is. A
// purchase made in March under 100 Credits per dollar must still be
// explicable in June after the rate changed twice, and "the rate was
// different then" is only an explanation if the rate that applied is recorded
// on the purchase.
type PricingPolicy struct {
	// Version is recorded on every funding and is compared by exact string
	// equality.
	Version string `json:"version"`

	// Currency is the ISO 4217 code of the money side. A policy prices one
	// currency; selling Credits in EUR is a second policy, not a conversion.
	Currency string `json:"currency"`

	// CreditsPerMajorUnit is how many Credits one major unit of Currency buys
	// -- Credits per dollar, not per cent. It is an integer because a
	// fractional rate is a float in disguise, and the goal document forbids
	// floats in money.
	CreditsPerMajorUnit int64 `json:"credits_per_major_unit"`

	// MinorUnitsPerMajorUnit is 100 for USD. It is stated rather than assumed
	// so that a zero-decimal currency does not silently multiply by 100.
	MinorUnitsPerMajorUnit int64 `json:"minor_units_per_major_unit"`

	// MinAmount and MaxAmount bound a single purchase, in minor units.
	// MaxAmount is not a risk control -- fraud policy is -- it is a
	// fat-finger control, and it bounds the arithmetic below.
	MinAmountMinor int64 `json:"min_amount_minor"`
	MaxAmountMinor int64 `json:"max_amount_minor"`

	// Rounding is how a payment that does not divide evenly into whole
	// Credits is resolved. It is part of the hash because changing it changes
	// what a user receives.
	Rounding money.RoundingMode `json:"rounding"`
}

// DefaultPricingVersion identifies the shipped policy.
const DefaultPricingVersion = "credit-pricing-v1"

// DefaultPricingPolicy returns the policy a deployment runs under unless it is
// given another one: 100 Credits per US dollar, between one dollar and ten
// thousand.
//
// Note what this is NOT. Everywhere else in this system the shipped default is
// the refusing one -- DefaultPolicy pays nobody out, ConservativePolicy denies
// every native market. Pricing is deliberately different, because pricing is
// not a safety control. What stops Credits being sold is the CREDIT_PURCHASE
// capability gate, which is high-risk, dual-controlled and inactive on a fresh
// deployment. A pricing policy that priced nothing would add no safety on top
// of that gate and would make every end-to-end test assert against a
// configuration no deployment will ever run. So the default here is a real
// policy that is inert until a gate somewhere else is opened.
func DefaultPricingPolicy() PricingPolicy {
	return PricingPolicy{
		Version:                DefaultPricingVersion,
		Currency:               "USD",
		CreditsPerMajorUnit:    100,
		MinorUnitsPerMajorUnit: 100,
		MinAmountMinor:         100,
		MaxAmountMinor:         1_000_000,
		Rounding:               money.RoundDown,
	}
}

// Validate checks the policy's internal consistency.
//
// The interesting check is the last one. A policy whose minimum purchase buys
// zero Credits would take a user's money and issue nothing, and it would do so
// only at the bottom of the range where nobody tests. Refusing it here means
// that class of policy cannot be loaded at all.
func (p PricingPolicy) Validate() error {
	if strings.TrimSpace(p.Version) == "" {
		return errs.New(errs.CodeValidationFailed, "credit: a pricing policy needs a version")
	}
	if len(strings.TrimSpace(p.Currency)) != 3 {
		return errs.Newf(errs.CodeValidationFailed, "credit: %q is not an ISO 4217 currency code", p.Currency)
	}
	if p.CreditsPerMajorUnit <= 0 {
		return errs.New(errs.CodeValidationFailed, "credit: a pricing policy must buy a positive number of Credits per unit")
	}
	if p.MinorUnitsPerMajorUnit <= 0 {
		return errs.New(errs.CodeValidationFailed, "credit: minor units per major unit must be positive")
	}
	if p.MinAmountMinor <= 0 {
		return errs.New(errs.CodeValidationFailed, "credit: the minimum purchase must be positive")
	}
	if p.MaxAmountMinor < p.MinAmountMinor {
		return errs.New(errs.CodeValidationFailed, "credit: the maximum purchase is below the minimum")
	}
	if !p.Rounding.Valid() {
		return errs.Newf(errs.CodeValidationFailed, "credit: unknown rounding mode %v", p.Rounding)
	}
	q, err := p.creditsFor(p.MinAmountMinor)
	if err != nil {
		return err
	}
	if q.Sign() <= 0 {
		return errs.Newf(errs.CodeValidationFailed,
			"credit: pricing policy %s issues no Credits for its own minimum purchase of %s; it would take money and give nothing",
			p.Version, money.USDFromMinor(p.MinAmountMinor))
	}
	return nil
}

// CreditsFor returns the Credits a payment of amount buys under p.
//
// This is the only function in the system that decides how many Credits a
// purchase issues. It reads no clock, no database and no request body beyond
// the amount, so the same amount under the same policy version always issues
// the same quantity -- which is what makes a funding reconstructible years
// later from two recorded fields.
func (p PricingPolicy) CreditsFor(amount money.USD) (money.Quantity, error) {
	if err := p.Validate(); err != nil {
		return money.Quantity{}, err
	}
	minor := amount.Minor()
	if minor < p.MinAmountMinor {
		return money.Quantity{}, errs.Newf(errs.CodeValidationFailed,
			"credit: the minimum Credit purchase is %s", money.USDFromMinor(p.MinAmountMinor))
	}
	if minor > p.MaxAmountMinor {
		return money.Quantity{}, errs.Newf(errs.CodeValidationFailed,
			"credit: the maximum Credit purchase is %s", money.USDFromMinor(p.MaxAmountMinor))
	}
	return p.creditsFor(minor)
}

// creditsFor is the arithmetic without the bounds checks, so Validate can use
// it on the minimum without recursing through Validate again.
func (p PricingPolicy) creditsFor(minor int64) (money.Quantity, error) {
	// Credits = minor * creditsPerMajorUnit / minorUnitsPerMajorUnit, done in
	// big.Int. The intermediate product overflows int64 at around ninety
	// billion cents times a hundred, which MaxAmountMinor makes unreachable --
	// but MulDiv is exact regardless, so the bound is a product decision here
	// and not a correctness one.
	return money.QuantityFromInt64(minor).MulDiv(
		money.QuantityFromInt64(p.CreditsPerMajorUnit),
		money.QuantityFromInt64(p.MinorUnitsPerMajorUnit),
		p.Rounding,
	)
}

// canonicalPricing is the hashed form: every field, in declaration order, with
// the rounding mode rendered as its name rather than its integer value so that
// reordering the RoundingMode constants cannot silently change a stored hash.
type canonicalPricing struct {
	Version                string `json:"version"`
	Currency               string `json:"currency"`
	CreditsPerMajorUnit    int64  `json:"credits_per_major_unit"`
	MinorUnitsPerMajorUnit int64  `json:"minor_units_per_major_unit"`
	MinAmountMinor         int64  `json:"min_amount_minor"`
	MaxAmountMinor         int64  `json:"max_amount_minor"`
	Rounding               string `json:"rounding"`
}

// Canonical returns the deterministic serialised form.
func (p PricingPolicy) Canonical() ([]byte, error) {
	b, err := json.Marshal(canonicalPricing{
		Version:                p.Version,
		Currency:               strings.ToUpper(strings.TrimSpace(p.Currency)),
		CreditsPerMajorUnit:    p.CreditsPerMajorUnit,
		MinorUnitsPerMajorUnit: p.MinorUnitsPerMajorUnit,
		MinAmountMinor:         p.MinAmountMinor,
		MaxAmountMinor:         p.MaxAmountMinor,
		Rounding:               p.Rounding.String(),
	})
	if err != nil {
		return nil, errs.Newf(errs.CodeInternal, "credit: canonicalise pricing policy: %v", err)
	}
	return b, nil
}

// Hash is the hex sha256 of the canonical form, recorded on every funding so
// that a stored version string can be proven to name the rules that were
// actually applied.
func (p PricingPolicy) Hash() (string, error) {
	b, err := p.Canonical()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
