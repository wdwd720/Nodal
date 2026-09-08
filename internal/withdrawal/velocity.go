package withdrawal

import (
	"time"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// VelocityPolicy bounds withdrawals per account and asset (PART 94). A
// zero quantity or count disables that bound; Window must be positive when
// any rolling bound is set. Quantities are base units of the asset.
type VelocityPolicy struct {
	MaxPerRequest     money.Quantity
	MaxPerWindow      money.Quantity
	MaxCountPerWindow int
	Window            time.Duration
}

// Validate checks the policy is coherent.
func (p VelocityPolicy) Validate() error {
	switch {
	case p.MaxPerRequest.IsNegative() || p.MaxPerWindow.IsNegative() || p.MaxCountPerWindow < 0 || p.Window < 0:
		return errs.New(errs.CodeValidationFailed, "withdrawal: velocity bounds must not be negative")
	case (p.MaxPerWindow.IsPositive() || p.MaxCountPerWindow > 0) && p.Window <= 0:
		return errs.New(errs.CodeValidationFailed, "withdrawal: velocity window is required with rolling bounds")
	}
	return nil
}

// UnboundedVelocity is a policy that permits any quantity at any rate.
//
// It is the zero value, and it is given a name because the zero value reads as
// a bound and is not one: `Check` returns nil for every request when
// `MaxPerRequest` is not positive and both window limits are zero. The
// production wiring passed `VelocityPolicy{}` under a comment saying "the bounds
// stay at zero (no rolling allowance)", which is the opposite of what it does
// (F-68).
//
// It is acceptable only while the WITHDRAWALS capability gate is DISABLED, which
// refuses the request several checks earlier. The gate is the load-bearing
// control; this is not a second one. Naming it is the point: a reviewer reading
// the composition root sees a value that says it bounds nothing, rather than an
// empty struct that looks like a default.
func UnboundedVelocity() VelocityPolicy { return VelocityPolicy{} }

// PermitsEverything reports whether the policy bounds nothing at all.
func (p VelocityPolicy) PermitsEverything() bool {
	return !p.MaxPerRequest.IsPositive() && !p.MaxPerWindow.IsPositive() && p.MaxCountPerWindow == 0
}

// Check applies the policy to a new request of quantity against the
// account's active withdrawals (same asset) created within the window
// ending at now. It fails with WITHDRAWAL_VELOCITY_LIMIT.
func (p VelocityPolicy) Check(quantity money.Quantity, recent []Withdrawal, now time.Time) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if p.MaxPerRequest.IsPositive() && quantity.Cmp(p.MaxPerRequest) > 0 {
		return errs.New(errs.CodeWithdrawalVelocityLimit, "withdrawal: quantity exceeds the per-request limit").
			WithField("limit", "per_request").WithField("max", p.MaxPerRequest.String())
	}
	if !p.MaxPerWindow.IsPositive() && p.MaxCountPerWindow == 0 {
		return nil
	}
	since := now.Add(-p.Window)
	total, count := money.Quantity{}, 0
	for _, w := range recent {
		if !w.Status.Active() || w.CreatedAt.Before(since) {
			continue
		}
		total = total.Add(w.Quantity)
		count++
	}
	if p.MaxCountPerWindow > 0 && count+1 > p.MaxCountPerWindow {
		return errs.New(errs.CodeWithdrawalVelocityLimit, "withdrawal: too many withdrawals in the window").
			WithField("limit", "count_per_window").WithField("max", p.MaxCountPerWindow).WithField("window", p.Window.String())
	}
	if p.MaxPerWindow.IsPositive() && total.Add(quantity).Cmp(p.MaxPerWindow) > 0 {
		return errs.New(errs.CodeWithdrawalVelocityLimit, "withdrawal: quantity exceeds the rolling window limit").
			WithField("limit", "quantity_per_window").WithField("max", p.MaxPerWindow.String()).WithField("window", p.Window.String())
	}
	return nil
}
