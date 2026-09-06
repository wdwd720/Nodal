package stripe

import (
	"strings"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/funding"
	"github.com/nodal/controlplane/internal/money"
)

// MaxAmountLength bounds a wire amount (Stripe examples are ≤ 20 chars;
// "0.123400000000000000" is 20).
const MaxAmountLength = 40

// ValidateAmount checks that s is a plain, non-negative decimal string:
// digits, optionally one "." and digits; no sign, exponent, whitespace,
// or empty integer part. It is applied to every amount the provider sends
// before the value is trusted anywhere.
func ValidateAmount(s string) error {
	if s == "" {
		return errs.New(errs.CodeValidationFailed, "stripe: amount is empty")
	}
	if len(s) > MaxAmountLength {
		return errs.New(errs.CodeValidationFailed, "stripe: amount is too long")
	}
	intPart, fracPart, hasDot := strings.Cut(s, ".")
	if intPart == "" || !digits(intPart) {
		return errs.Newf(errs.CodeValidationFailed, "stripe: amount %q is not a plain decimal", s)
	}
	if hasDot && (fracPart == "" || !digits(fracPart)) {
		return errs.Newf(errs.CodeValidationFailed, "stripe: amount %q is not a plain decimal", s)
	}
	return nil
}

func digits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// ParseAmount converts a validated wire amount to exact base units at the
// given precision; fractional digits beyond decimals are PRECISION_LOSS,
// never rounded.
func ParseAmount(s string, decimals uint8) (money.Quantity, error) {
	if err := ValidateAmount(s); err != nil {
		return money.Quantity{}, err
	}
	return funding.ParseDecimalAmount(s, decimals)
}

// CurrencyDecimals returns the on-chain precision of a destination
// (network, currency) pair this deployment settles. Only Solana USDC (6)
// and SOL (9) are known; anything else is unsupported and the funding
// service escalates rather than guessing a scale.
func CurrencyDecimals(network, currency string) (uint8, bool) {
	switch strings.ToLower(network) + "/" + strings.ToLower(currency) {
	case "solana/usdc":
		return 6, true
	case "solana/sol":
		return 9, true
	}
	return 0, false
}
