package gates

import (
	"context"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
)

// Checker evaluates capability gates for one environment. It is safe for
// concurrent use and holds no mutable state.
type Checker struct {
	env     string
	enabled func(Capability) bool
	clk     clock.Clock
}

// NewChecker builds a Checker. enabled is condition 1 of POLICY_AUTHORITY §1
// (deployment configuration, e.g. config.Capabilities.Enabled); a nil
// enabled treats every capability as not enabled, which fails closed.
func NewChecker(env string, enabled func(Capability) bool, clk clock.Clock) (*Checker, error) {
	if !validEnvironment(env) {
		return nil, errs.Newf(errs.CodeValidationFailed, "unknown environment %q", env)
	}
	if clk == nil {
		return nil, errs.New(errs.CodeValidationFailed, "clock is required")
	}
	if enabled == nil {
		enabled = func(Capability) bool { return false }
	}
	return &Checker{env: env, enabled: enabled, clk: clk}, nil
}

// Environment returns the environment the checker evaluates.
func (c *Checker) Environment() string { return c.env }

// IsActive reports whether cap is ACTIVE right now: all five conditions of
// POLICY_AUTHORITY §1 must hold. A missing row, an unknown capability and a
// capability that configuration does not enable are all inactive with a
// distinct Reason; only I/O failures are returned as errors. The
// configuration check runs first and skips the query when it fails, so no
// database round-trip can ever be what enables a capability.
func (c *Checker) IsActive(ctx context.Context, q db.Querier, cap Capability) (Verdict, error) {
	if !cap.Valid() {
		return Verdict{Reason: ReasonNoGateRow}, nil
	}
	if !c.enabled(cap) {
		return Evaluate(nil, false, c.clk.Now()), nil
	}
	g, err := loadGate(ctx, q, cap, c.env, false)
	if err != nil {
		return Verdict{}, err
	}
	return Evaluate(g, true, c.clk.Now()), nil
}

// RequireActive is IsActive as a guard: nil when active, otherwise
// CAPABILITY_NOT_APPROVED with fields capability, environment, reason and
// state. Every live-money path calls this before doing work.
func (c *Checker) RequireActive(ctx context.Context, q db.Querier, cap Capability) error {
	v, err := c.IsActive(ctx, q, cap)
	if err != nil {
		return err
	}
	if v.Active {
		return nil
	}
	return errs.Newf(errs.CodeCapabilityNotApproved, "capability %s is not active in %s", cap, c.env).
		WithField("capability", string(cap)).
		WithField("environment", c.env).
		WithField("reason", v.Reason).
		WithField("state", string(v.State))
}
