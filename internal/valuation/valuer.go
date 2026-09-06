package valuation

import (
	"errors"
	"time"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// Rounding modes used by every valuation step. They are constants, not
// options, so that the number shown, the number reserved against and the
// number risk-checked are the same number.
const (
	// MarkRounding is applied when a quantity is marked to a price: first
	// in money.Notional (to quote-asset base units) and again in
	// money.QuoteQuantityToUSD (to cents). Half-even is unbiased over many
	// marks; a mark is a valuation, not an entitlement, so neither
	// direction is "safe" and the unbiased mode is the honest one.
	MarkRounding = money.RoundHalfEven
	// HaircutRounding is applied when a collateral factor is taken from a
	// non-negative mark. Rounding toward zero always yields less buying
	// power, never more (ADR-0015).
	HaircutRounding = money.RoundDown
)

// USDPeg is the peg currency an asset must declare to be valued at face.
const USDPeg = "USD"

// Gate says whether an asset may contribute to buying power at all.
type Gate string

// Gates, from least to most restrictive.
const (
	// GateEligible: the asset contributes value × collateral factor.
	GateEligible Gate = "ELIGIBLE"
	// GatePortfolioOnly: the asset is marked into portfolio value but
	// contributes nothing to buying power.
	GatePortfolioOnly Gate = "PORTFOLIO_ONLY"
	// GateExcluded: the asset is not valued and operations on it are blocked.
	GateExcluded Gate = "EXCLUDED"
)

// Basis says how an asset is valued.
type Basis string

// Valuation bases.
const (
	// BasisFace: one whole unit is one USD; no market price is consulted.
	BasisFace Basis = "FACE"
	// BasisMarket: a market price no older than the policy's maximum age.
	BasisMarket Basis = "MARKET"
)

// Reason codes attached to haircuts and exclusions.
const (
	ReasonCollateralFactor        = "COLLATERAL_FACTOR"
	ReasonAssetRestricted         = "ASSET_RESTRICTED"
	ReasonAssetDelisted           = "ASSET_DELISTED"
	ReasonAssetHalted             = "ASSET_HALTED"
	ReasonAssetStatusUnknown      = "ASSET_STATUS_UNKNOWN"
	ReasonStablecoinDegraded      = "STABLECOIN_DEGRADED"
	ReasonStablecoinRestricted    = "STABLECOIN_RESTRICTED"
	ReasonStablecoinHalted        = "STABLECOIN_HALTED"
	ReasonStablecoinStatusMissing = "STABLECOIN_STATUS_MISSING"
	ReasonPolicyMissing           = "POLICY_MISSING"
)

// Classification is the outcome of applying an AssetPolicy to an asset.
type Classification struct {
	Gate   Gate
	Basis  Basis
	Factor money.BPS // effective collateral factor; 0 unless GateEligible
	Reason string    // why the factor is below 100% or the gate is closed; "" when neither
}

// NeedsPrice reports whether marking under c requires a market price.
func (c Classification) NeedsPrice() bool {
	return c.Gate != GateExcluded && c.Basis == BasisMarket
}

// Classify applies the policy to the asset (FINANCIAL_MODEL §6, PART 26,
// PART 33). Asset status and stablecoin status are combined by taking the
// more restrictive gate:
//
//	asset status   ACTIVE, CLOSE_ONLY, DELISTING → ELIGIBLE
//	               RESTRICTED, DELISTED          → PORTFOLIO_ONLY
//	               HALTED, unknown               → EXCLUDED
//	stablecoin     NORMAL    → basis FACE (USD peg only), gate unchanged
//	               DEGRADED  → basis MARKET, gate unchanged, haircut reason
//	               RESTRICTED→ basis MARKET, gate ≤ PORTFOLIO_ONLY
//	               HALTED    → EXCLUDED
//	               unset/unknown on a stablecoin → gate ≤ PORTFOLIO_ONLY (fail closed)
//
// A missing policy is PORTFOLIO_ONLY at market with reason POLICY_MISSING.
// The collateral factor is clamped to [0, 100%] and forced to 0 whenever
// the gate is not ELIGIBLE, so a haircut can never increase buying power.
func Classify(asset assets.Asset, p AssetPolicy) Classification {
	if p.PolicyMissing {
		return Classification{Gate: GatePortfolioOnly, Basis: BasisMarket, Reason: ReasonPolicyMissing}
	}
	c := Classification{Gate: GateEligible, Basis: BasisMarket, Factor: clampFactor(p.CollateralFactor)}
	switch p.Status {
	case assets.StatusActive, assets.StatusCloseOnly, assets.StatusDelisting:
	case assets.StatusRestricted:
		c.Gate, c.Reason = GatePortfolioOnly, ReasonAssetRestricted
	case assets.StatusDelisted:
		c.Gate, c.Reason = GatePortfolioOnly, ReasonAssetDelisted
	case assets.StatusHalted:
		c.Gate, c.Reason = GateExcluded, ReasonAssetHalted
	default:
		c.Gate, c.Reason = GateExcluded, ReasonAssetStatusUnknown
	}
	if asset.IsStablecoin || p.StablecoinStatus != "" {
		switch p.StablecoinStatus {
		case StablecoinNormal:
			if asset.IsStablecoin && asset.PegCurrency == USDPeg {
				c.Basis = BasisFace
			}
		case StablecoinDegraded:
			if c.Gate == GateEligible {
				c.Reason = ReasonStablecoinDegraded
			}
		case StablecoinRestricted:
			if c.Gate == GateEligible {
				c.Gate, c.Reason = GatePortfolioOnly, ReasonStablecoinRestricted
			}
		case StablecoinHalted:
			if c.Gate != GateExcluded {
				c.Gate, c.Reason = GateExcluded, ReasonStablecoinHalted
			}
		default:
			if c.Gate == GateEligible {
				c.Gate, c.Reason = GatePortfolioOnly, ReasonStablecoinStatusMissing
			}
		}
	}
	if c.Gate != GateEligible {
		c.Factor = 0
	} else if c.Reason == "" && c.Factor < money.OneHundredPercent {
		c.Reason = ReasonCollateralFactor
	}
	return c
}

func clampFactor(f money.BPS) money.BPS {
	switch {
	case f < 0:
		return 0
	case f > money.OneHundredPercent:
		return money.OneHundredPercent
	}
	return f
}

// Mark is the valuation of one quantity of one asset.
type Mark struct {
	// Value is the marked USD value (portfolio value). Zero when excluded.
	Value money.USD
	// Contribution is Value × Factor, rounded toward zero. Zero unless
	// the gate is ELIGIBLE.
	Contribution money.USD
	Factor       money.BPS
	Gate         Gate
	Basis        Basis
	Reason       string
	// PriceRef names the price used: "face:USD" or "<source>@<observed_at>".
	PriceRef string
}

// Valuer performs exact marks. It holds no state; the zero value is usable.
type Valuer struct{}

// NewValuer returns a Valuer.
func NewValuer() *Valuer { return &Valuer{} }

// ValueQuantity marks qty base units of an asset with assetDecimals at price
// (quoted in a USD-pegged asset with quoteDecimals) to USD:
//
//	notional = money.Notional(qty, assetDecimals, price, quoteDecimals, MarkRounding)   // quote base units
//	usd      = money.QuoteQuantityToUSD(notional, quoteDecimals, MarkRounding)          // cents
//
// The intermediate is the exact quote-asset notional at the quote asset's
// own precision, the same figure the reservation layer works in; the
// second rounding is the parity conversion to cents. Whether the quote
// asset may be treated as USD is the caller's decision (see Classify).
// Overflow → OVERFLOW; an invalid price → VALIDATION_FAILED.
func (Valuer) ValueQuantity(qty money.Quantity, assetDecimals uint8, price money.Price, quoteDecimals uint8) (money.USD, error) {
	notional, err := money.Notional(qty, assetDecimals, price, quoteDecimals, MarkRounding)
	if err != nil {
		return money.USD{}, mapMoneyError(err, "mark to price")
	}
	usd, err := money.QuoteQuantityToUSD(notional, quoteDecimals, MarkRounding)
	if err != nil {
		return money.USD{}, mapMoneyError(err, "convert notional to USD")
	}
	return usd, nil
}

// FaceValue values qty base units of a USD-pegged asset with assetDecimals
// at parity (one whole unit = one USD), rounding to cents in MarkRounding.
func (Valuer) FaceValue(qty money.Quantity, assetDecimals uint8) (money.USD, error) {
	usd, err := money.QuoteQuantityToUSD(qty, assetDecimals, MarkRounding)
	if err != nil {
		return money.USD{}, mapMoneyError(err, "face value")
	}
	return usd, nil
}

// ApplyCollateral returns value × factor / 10_000 rounded toward zero. A
// negative value is rejected: haircuts apply to holdings, never to debts.
func (Valuer) ApplyCollateral(value money.USD, factor money.BPS) (money.USD, error) {
	if value.IsNegative() {
		return money.USD{}, errs.New(errs.CodeValidationFailed, "collateral factor cannot be applied to a negative value")
	}
	out, err := value.MulBPS(clampFactor(factor), HaircutRounding)
	if err != nil {
		return money.USD{}, mapMoneyError(err, "apply collateral factor")
	}
	return out, nil
}

// PriceRef renders the provenance of a price for output fields.
func PriceRef(p money.Price) string {
	return p.Source + "@" + p.At.UTC().Format(time.RFC3339Nano)
}

// Mark values a non-negative qty of asset under policy p. price is required
// when Classify(asset, p).NeedsPrice() and ignored otherwise; quoteDecimals
// are the decimals of the price's USD-pegged quote asset. The stablecoin
// contribution rule of FINANCIAL_MODEL §6 is realized here:
//
//	NORMAL     → Value = face,   Contribution = face × factor
//	DEGRADED   → Value = market, Contribution = market × factor
//	RESTRICTED → Value = market, Contribution = 0
//	HALTED     → Value = 0,      Contribution = 0 (excluded)
func (v Valuer) Mark(asset assets.Asset, p AssetPolicy, qty money.Quantity, price *money.Price, quoteDecimals uint8) (Mark, error) {
	if qty.IsNegative() {
		return Mark{}, errs.New(errs.CodeValidationFailed, "cannot mark a negative quantity")
	}
	c := Classify(asset, p)
	m := Mark{Factor: c.Factor, Gate: c.Gate, Basis: c.Basis, Reason: c.Reason}
	if c.Gate == GateExcluded {
		return m, nil
	}
	var err error
	switch c.Basis {
	case BasisFace:
		m.Value, err = v.FaceValue(qty, asset.Decimals)
		m.PriceRef = "face:" + USDPeg
	case BasisMarket:
		if price == nil {
			return Mark{}, errs.New(errs.CodeValidationFailed, "market price required to mark this asset").
				WithField("asset_id", asset.ID.String())
		}
		m.Value, err = v.ValueQuantity(qty, asset.Decimals, *price, quoteDecimals)
		m.PriceRef = PriceRef(*price)
	}
	if err != nil {
		return Mark{}, err
	}
	if c.Gate == GateEligible {
		m.Contribution, err = v.ApplyCollateral(m.Value, c.Factor)
		if err != nil {
			return Mark{}, err
		}
	}
	return m, nil
}

// mapMoneyError translates money sentinels to stable API codes.
func mapMoneyError(err error, op string) error {
	switch {
	case errors.Is(err, money.ErrOverflow):
		return errs.Wrap(err, errs.CodeOverflow, "valuation: "+op+": overflow")
	case errors.Is(err, money.ErrPrecisionLoss):
		return errs.Wrap(err, errs.CodePrecisionLoss, "valuation: "+op+": precision loss")
	case errors.Is(err, money.ErrInvalidPrice):
		return errs.Wrap(err, errs.CodeValidationFailed, "valuation: "+op+": invalid price")
	case errors.Is(err, money.ErrDivisionByZero), errors.Is(err, money.ErrInvalidRoundingMode):
		return errs.Wrap(err, errs.CodeInternal, "valuation: "+op)
	}
	return errs.Wrap(err, errs.CodeInternal, "valuation: "+op)
}
