package fees

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/money"
)

// Policy is a versioned platform fee schedule.
type Policy struct {
	Version        string
	PlatformFeeBPS money.BPS
	// MinFee and MaxFee bound the fee in FeeAsset base units; zero disables the bound.
	MinFee   money.Quantity
	MaxFee   money.Quantity
	FeeAsset assets.AssetID
	// Rounding names the mode applied when bps × quantity is inexact.
	Rounding money.RoundingMode
}

// Zero returns the default policy: no platform fee. The version string makes
// "no fee" an explicit, auditable choice rather than an absence.
func Zero(feeAsset assets.AssetID) Policy {
	return Policy{Version: "fee-zero-v1", PlatformFeeBPS: 0, FeeAsset: feeAsset, Rounding: money.RoundHalfEven}
}

// Validate checks policy consistency.
func (p Policy) Validate() error {
	switch {
	case p.Version == "":
		return errors.New("fees: version required")
	case p.PlatformFeeBPS < 0 || p.PlatformFeeBPS > 10_000:
		return errors.New("fees: platform fee bps must be within 0..10000")
	case p.MinFee.Sign() < 0 || p.MaxFee.Sign() < 0:
		return errors.New("fees: bounds must not be negative")
	case !p.MaxFee.IsZero() && p.MinFee.Cmp(p.MaxFee) > 0:
		return errors.New("fees: min fee exceeds max fee")
	case p.FeeAsset.IsZero():
		return errors.New("fees: fee asset required")
	case !p.Rounding.Valid() || p.Rounding == money.RoundExact:
		return errors.New("fees: a concrete rounding mode is required")
	}
	return nil
}

// Hash is a stable identifier of the policy content.
func (p Policy) Hash() string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%s|%s|%s|%s", p.Version, p.PlatformFeeBPS, p.MinFee.String(), p.MaxFee.String(), p.FeeAsset.String(), p.Rounding.String())))
	return hex.EncodeToString(sum[:])
}

// Breakdown is the disclosed fee composition of a quote or fill.
type Breakdown struct {
	PolicyVersion  string
	PolicyHash     string
	PlatformFee    money.Quantity // in FeeAsset base units
	PlatformFeeBPS money.BPS
	FeeAsset       assets.AssetID
	Bounded        string // "", "min", or "max" when a bound applied
}

// Compute returns the platform fee for an input quantity denominated in the
// policy's fee asset. The caller is responsible for supplying a quantity in
// FeeAsset; conversion into another asset is a valuation concern.
func (p Policy) Compute(input money.Quantity) (Breakdown, error) {
	if err := p.Validate(); err != nil {
		return Breakdown{}, err
	}
	if input.Sign() < 0 {
		return Breakdown{}, errors.New("fees: input quantity must not be negative")
	}
	b := Breakdown{PolicyVersion: p.Version, PolicyHash: p.Hash(), PlatformFeeBPS: p.PlatformFeeBPS, FeeAsset: p.FeeAsset}
	if p.PlatformFeeBPS == 0 && p.MinFee.IsZero() {
		b.PlatformFee = money.Quantity{}
		return b, nil
	}
	fee, err := input.MulBPSChecked(p.PlatformFeeBPS, p.Rounding)
	if err != nil {
		return Breakdown{}, fmt.Errorf("fees: compute: %w", err)
	}
	if !p.MinFee.IsZero() && fee.Cmp(p.MinFee) < 0 {
		fee = p.MinFee
		b.Bounded = "min"
	}
	if !p.MaxFee.IsZero() && fee.Cmp(p.MaxFee) > 0 {
		fee = p.MaxFee
		b.Bounded = "max"
	}
	if fee.Cmp(input) > 0 {
		return Breakdown{}, errors.New("fees: fee exceeds input quantity")
	}
	b.PlatformFee = fee
	return b, nil
}
