package reconciliation

import (
	"time"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// Policy is the operator-tunable part of reconciliation. Everything else is
// fixed by the goal document. The zero value is invalid; use DefaultPolicy.
type Policy struct {
	// Version names the configuration so records can say which policy judged
	// them.
	Version string

	// MaterialThresholdUSDMinor is the USD value at or above which a
	// difference is material (PART 51: material resolutions need dual
	// control). Differences are valued through the injected USDValuer; when
	// no valuation is available the quantity thresholds below decide, and
	// anything above dust is material (fail closed).
	MaterialThresholdUSDMinor int64

	// DustQuantity is the per-asset absolute difference at or below which a
	// WALLET_BALANCE difference is dust (rent-related lamport changes,
	// rounding). Assets not listed use DefaultDustQuantity.
	DustQuantity map[assets.AssetID]money.Quantity
	// DefaultDustQuantity applies to assets with no explicit entry. Zero
	// means "no difference is dust", which is the safe default.
	DefaultDustQuantity money.Quantity

	// AutoPostDustAdjustment allows AutoCauseFeeDust to post a
	// RECONCILIATION_ADJUSTMENT transaction. When false a dust record is
	// resolved automatically with the difference recorded and no economic
	// effect (RECONCILIATION.md §4).
	AutoPostDustAdjustment bool

	// MaxUnresolvedAge is how long a material mismatch may stay unresolved
	// before global policy halts new trading (PART 158). Zero disables the
	// global halt; per-account blocking is unconditional.
	MaxUnresolvedAge time.Duration

	// ProvenAbsentMargin is how many blocks past LastValidBlockHeight the
	// chain must have advanced before absence is treated as proven (PART 48
	// step "if proven absent"). It is never zero: a transaction can still
	// land at exactly LastValidBlockHeight.
	ProvenAbsentMargin uint64

	// StaleBalanceSlotSkew is the largest slot difference between two
	// observers' balance reads that still counts as "the same moment". A
	// larger skew is re-queried instead of opening a mismatch.
	StaleBalanceSlotSkew uint64

	// FundingSettlementTimeout is how long a provider-confirmed deposit may
	// go without an observed chain receipt before the absence itself becomes
	// a mismatch (RECONCILIATION.md §5). Zero disables the rule.
	FundingSettlementTimeout time.Duration
}

// DefaultPolicy is the documented default: $1.00 materiality, no dust without
// an explicit per-asset entry, no automatic dust posting, a 24 h unresolved
// budget, a 32-block absence margin and a 150-slot balance skew tolerance.
func DefaultPolicy() Policy {
	return Policy{
		Version:                   "reconciliation/1",
		MaterialThresholdUSDMinor: 100,
		DustQuantity:              map[assets.AssetID]money.Quantity{},
		DefaultDustQuantity:       money.QuantityFromInt64(0),
		AutoPostDustAdjustment:    false,
		MaxUnresolvedAge:          24 * time.Hour,
		ProvenAbsentMargin:        32,
		StaleBalanceSlotSkew:      150,
		FundingSettlementTimeout:  2 * time.Hour,
	}
}

// Validate checks the policy is usable.
func (p Policy) Validate() error {
	problems := map[string]any{}
	if p.Version == "" {
		problems["version"] = "required"
	}
	if p.MaterialThresholdUSDMinor < 0 {
		problems["material_threshold_usd_minor"] = "must not be negative"
	}
	if p.DefaultDustQuantity.IsNegative() {
		problems["default_dust_quantity"] = "must not be negative"
	}
	for a, q := range p.DustQuantity {
		if q.IsNegative() {
			problems["dust_quantity["+a.String()+"]"] = "must not be negative"
		}
	}
	if p.MaxUnresolvedAge < 0 {
		problems["max_unresolved_age"] = "must not be negative"
	}
	if p.FundingSettlementTimeout < 0 {
		problems["funding_settlement_timeout"] = "must not be negative"
	}
	if p.ProvenAbsentMargin == 0 {
		problems["proven_absent_margin"] = "must be at least 1 block"
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "reconciliation: invalid policy").WithFields(problems)
	}
	return nil
}

// Dust returns the dust threshold for an asset.
func (p Policy) Dust(asset assets.AssetID) money.Quantity {
	if q, ok := p.DustQuantity[asset]; ok {
		return q
	}
	return p.DefaultDustQuantity
}

// IsDust reports whether |diff| is at or below the asset's dust threshold.
// A zero threshold makes nothing dust except a zero difference.
func (p Policy) IsDust(asset assets.AssetID, diff money.Quantity) bool {
	return diff.Abs().Cmp(p.Dust(asset)) <= 0
}

// Material decides whether a difference must be treated as material.
//
//   - a kind that is always material (internal drift, unknown submission) is
//     material whatever the numbers say;
//   - with a USD valuation, |usd| ≥ MaterialThresholdUSDMinor is material;
//   - without one, anything above the asset's dust threshold is material.
//
// The last rule is the fail-closed branch: an unvalued difference is never
// silently treated as small.
func (p Policy) Material(kind Kind, asset assets.AssetID, diff money.Quantity, usd *money.USD) bool {
	if kind.AlwaysMaterial() {
		return true
	}
	if diff.IsZero() {
		return false
	}
	if usd != nil {
		abs, err := usd.Abs()
		if err != nil {
			// money.MinUSD has no representable absolute value; a difference
			// that large is material by any measure.
			return true
		}
		return abs.Minor() >= p.MaterialThresholdUSDMinor
	}
	return !p.IsDust(asset, diff)
}
