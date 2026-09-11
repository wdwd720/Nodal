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

	// Decimals is the scale of the CREDIT asset this policy prices, and it is
	// the field whose absence made every purchase issue a millionth of what it
	// promised (F-151).
	//
	// A CreditQuantity is asset BASE UNITS everywhere it appears --
	// money.Quantity documents it, the OpenAPI Quantity schema documents it,
	// credit_fundings.credit_quantity stores it and the ledger entry moves it
	// -- while CreditsPerMajorUnit is a count of whole CREDITS per dollar.
	// Converting between the two needs 10^decimals, and the policy had no
	// decimals to multiply by: it wrote a count of Credits into a field that
	// means base units, so $10.00 at 100 Credits per dollar bought 1,000 base
	// units, which is 0.001 Credits.
	//
	// It is hashed like every other field, because changing the scale changes
	// what a payment buys and a funding recorded under the old scale must
	// still be explicable. NewPurchaseService refuses to build when this
	// disagrees with the CREDIT asset the deployment actually registered, so
	// the two cannot drift.
	Decimals uint8 `json:"decimals"`
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
		Decimals:               DefaultCreditDecimals,
	}
}

// DefaultCreditDecimals is the scale the CREDIT asset is registered with
// everywhere in this repository: scripts/seedeconomy, cmd/api's sandbox-tier
// registration, internal/demo's fixture and docs/product/CREDIT_ECONOMY.md all
// say six, and a bonding-curve market needs to price units far below one
// Credit.
//
// It is a default and not an assumption. NewPurchaseService reads the scale of
// the asset the deployment actually registered and refuses to build a service
// whose policy prices a different one.
const DefaultCreditDecimals uint8 = 6

// MaxCreditDecimals bounds the scale a policy may declare. It is the widest
// scale internal/assets permits, and it keeps the arithmetic below to a size
// numeric(38,0) can hold for any amount the policy admits.
const MaxCreditDecimals uint8 = 18

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
	if p.Decimals > MaxCreditDecimals {
		return errs.Newf(errs.CodeValidationFailed,
			"credit: a pricing policy may not price a %d-decimal asset; the widest supported scale is %d",
			p.Decimals, MaxCreditDecimals)
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
	// BASE UNITS = minor * creditsPerMajorUnit * 10^decimals / minorUnitsPerMajorUnit.
	//
	// The 10^decimals is the whole of F-151. Without it the expression yields a
	// count of whole Credits and is written into a column, an API field and a
	// ledger entry that all mean base units, so $10.00 at 100 Credits per
	// dollar issued 1,000 base units -- 0.001 Credits -- while the Buy Credits
	// page told the customer they were getting 1,000 Credits. internal/payout
	// already scaled both directions (creditsToMoney / moneyToCredits); this is
	// the same conversion, in the direction that issues.
	//
	// Done in big.Int throughout: the intermediate product of a ten-thousand
	// dollar purchase at six decimals is 10^14, which fits an int64 and would
	// not at a wider scale, and MulDiv is exact at any size.
	return money.QuantityFromInt64(minor).MulDiv(
		money.QuantityFromInt64(p.CreditsPerMajorUnit).ScaleUp(p.Decimals),
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
	Decimals               uint8  `json:"decimals"`
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
		Decimals:               p.Decimals,
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
