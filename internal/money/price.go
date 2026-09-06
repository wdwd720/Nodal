package money

import (
	"fmt"
	"math/big"
	"strings"
	"time"
)

// MaxPriceScale is the largest Scale a valid Price may carry.
const MaxPriceScale int32 = 38

// Price is an observed price of one whole base asset unit expressed in a
// quote asset:
//
//	value = Mantissa × 10^-Scale quote units per 1 base unit
//
// For example 150.123456 USDC per SOL is {Mantissa: 150123456, Scale: 6,
// QuoteAsset: "USDC"}. A price always names its source and observation
// time so that staleness and provenance can be judged by the caller.
//
// Price is a plain struct so it can be built from persisted columns; call
// Validate (or construct via NewPrice / PriceFromDecimalString) before use.
// Every function in this package that consumes a Price validates it.
type Price struct {
	Mantissa   Quantity
	Scale      int32
	QuoteAsset string
	Source     string
	At         time.Time
}

// NewPrice constructs and validates a Price.
func NewPrice(mantissa Quantity, scale int32, quoteAsset, source string, at time.Time) (Price, error) {
	p := Price{Mantissa: mantissa, Scale: scale, QuoteAsset: quoteAsset, Source: source, At: at}
	if err := p.Validate(); err != nil {
		return Price{}, err
	}
	return p, nil
}

// PriceFromDecimalString builds a Price from a plain decimal such as
// "150.123456": the scale is the number of fractional digits (at most
// MaxPriceScale) and the mantissa is the digits with the point removed.
// The value is preserved exactly; nothing is rounded.
func PriceFromDecimalString(s, quoteAsset, source string, at time.Time) (Price, error) {
	parts, err := parseDecimalLiteral(s, maxDecimalInputLen)
	if err != nil {
		return Price{}, fmt.Errorf("%w: %w", ErrInvalidPrice, err)
	}
	if int64(len(parts.fracDigits)) > int64(MaxPriceScale) {
		return Price{}, fmt.Errorf("%w: %d fractional digits exceeds scale %d", ErrInvalidPrice, len(parts.fracDigits), MaxPriceScale)
	}
	digits := parts.intDigits + parts.fracDigits
	if parts.negative {
		digits = "-" + digits
	}
	mantissa, err := ParseQuantity(digits)
	if err != nil {
		return Price{}, fmt.Errorf("%w: %w", ErrInvalidPrice, err)
	}
	return NewPrice(mantissa, int32(len(parts.fracDigits)), quoteAsset, source, at) // #nosec G115 -- len(fracDigits) is in [0, MaxPriceScale] (38) by the check above, so int32 is exact
}

// Validate checks that Scale is within [0, MaxPriceScale], Mantissa is not
// negative, QuoteAsset and Source are non-blank, and At is set. Every
// failure wraps ErrInvalidPrice.
func (p Price) Validate() error {
	if p.Scale < 0 || p.Scale > MaxPriceScale {
		return fmt.Errorf("%w: scale %d outside [0, %d]", ErrInvalidPrice, p.Scale, MaxPriceScale)
	}
	if p.Mantissa.Sign() < 0 {
		return fmt.Errorf("%w: negative mantissa", ErrInvalidPrice)
	}
	if strings.TrimSpace(p.QuoteAsset) == "" {
		return fmt.Errorf("%w: empty quote asset", ErrInvalidPrice)
	}
	if strings.TrimSpace(p.Source) == "" {
		return fmt.Errorf("%w: empty source", ErrInvalidPrice)
	}
	if p.At.IsZero() {
		return fmt.Errorf("%w: zero timestamp", ErrInvalidPrice)
	}
	return nil
}

// String renders the price as "<decimal> <QuoteAsset>", e.g.
// "150.123456 USDC". Prices with an out-of-range scale render the raw
// mantissa and scale instead.
func (p Price) String() string {
	if p.Scale >= 0 && p.Scale <= MaxPriceScale {
		return p.Mantissa.ToDecimalString(uint8(p.Scale)) + " " + p.QuoteAsset
	}
	return fmt.Sprintf("%se-%d %s", p.Mantissa, p.Scale, p.QuoteAsset)
}

// Notional returns the value of qty base units of an asset with
// baseDecimals, priced at p, expressed in base units of the quote asset
// with quoteDecimals:
//
//	notional = qty × Mantissa × 10^(quoteDecimals − baseDecimals − Scale)
//
// The product is formed exactly in a big integer; when the exponent is
// negative the single division is rounded in the given mode. A negative
// qty (a sale, a short) yields a negative notional. The price is validated
// first and any failure wraps ErrInvalidPrice.
//
//	// 1.5 SOL (9 decimals) at 150.123456 USDC (6 decimals)
//	Notional(QuantityFromInt64(1_500_000_000), 9, p, 6, RoundHalfEven)  // 225185184 (= 225.185184 USDC)
func Notional(qty Quantity, baseDecimals uint8, p Price, quoteDecimals uint8, mode RoundingMode) (Quantity, error) {
	if err := mode.validate(); err != nil {
		return Quantity{}, err
	}
	if err := p.Validate(); err != nil {
		return Quantity{}, err
	}
	prod := new(big.Int).Mul(qty.ref(), p.Mantissa.ref())
	exp := int(quoteDecimals) - int(baseDecimals) - int(p.Scale)
	if exp >= 0 {
		return Quantity{v: prod.Mul(prod, pow10(exp))}, nil
	}
	r, err := divRound(prod, pow10(-exp), mode)
	if err != nil {
		return Quantity{}, err
	}
	return Quantity{v: r}, nil
}

// QuoteQuantityToUSD converts q base units of a quote asset with
// quoteDecimals into USD cents, rounding in the given mode when the asset
// has more than two decimals and failing with ErrOverflow if the result
// leaves the int64 range.
//
// This is a pure unit conversion at parity (1 quote unit = 1 USD). It makes
// no judgement about whether the asset is actually USD-pegged or eligible
// to be treated as such; that decision belongs to the caller.
//
//	QuoteQuantityToUSD(QuantityFromInt64(225185184), 6, RoundHalfEven)  // 22519 cents = 225.19 USD
func QuoteQuantityToUSD(q Quantity, quoteDecimals uint8, mode RoundingMode) (USD, error) {
	if err := mode.validate(); err != nil {
		return USD{}, err
	}
	d := int(quoteDecimals)
	if d <= usdDecimals {
		return usdFromBig(new(big.Int).Mul(q.ref(), pow10(usdDecimals-d)))
	}
	minor, err := divRound(q.ref(), pow10(d-usdDecimals), mode)
	if err != nil {
		return USD{}, err
	}
	return usdFromBig(minor)
}

// USDToQuoteQuantity converts u into base units of a quote asset with
// quoteDecimals at parity (1 USD = 1 quote unit). The conversion is exact
// whenever quoteDecimals >= 2; for coarser assets the result is rounded in
// the given mode. As with QuoteQuantityToUSD, peg eligibility is the
// caller's decision.
func USDToQuoteQuantity(u USD, quoteDecimals uint8, mode RoundingMode) (Quantity, error) {
	if err := mode.validate(); err != nil {
		return Quantity{}, err
	}
	d := int(quoteDecimals)
	minor := big.NewInt(u.minor)
	if d >= usdDecimals {
		return Quantity{v: minor.Mul(minor, pow10(d-usdDecimals))}, nil
	}
	r, err := divRound(minor, pow10(usdDecimals-d), mode)
	if err != nil {
		return Quantity{}, err
	}
	return Quantity{v: r}, nil
}
