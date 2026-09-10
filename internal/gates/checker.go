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
	// sandboxAllowed reads a SANDBOX row as active. Set only by WithSandbox,
	// which cmd/api calls exactly when the deployment is a sandbox tier.
	sandboxAllowed bool
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
		return EvaluateWith(nil, false, c.sandboxAllowed, c.clk.Now()), nil
	}
	g, err := loadGate(ctx, q, cap, c.env, false)
	if err != nil {
		return Verdict{}, err
	}
	return EvaluateWith(g, true, c.sandboxAllowed, c.clk.Now()), nil
}

// ActiveSet answers for MANY capabilities in ONE query.
//
// IsActive per capability means one round trip per capability, and the
// production resolver asks about a dozen on every financial posting. Twelve
// round trips inside a transaction is twelve chances to be waiting on the
// database while holding a lock, and it was measurably worse than that: the
// resolver read through the POOL while the caller held a transaction from the
// same pool, so a dozen concurrent postings could hold every connection and
// each wait for one more (F-27).
//
// The configuration check still runs first and per capability, so a capability
// configuration does not enable is inactive without the row being consulted at
// all — no database read can be what enables a capability.
func (c *Checker) ActiveSet(ctx context.Context, q db.Querier, caps []Capability) (map[Capability]Verdict, error) {
	out := make(map[Capability]Verdict, len(caps))
	need := false
	for _, cap := range caps {
		switch {
		case !cap.Valid():
			out[cap] = Verdict{Reason: ReasonNoGateRow}
		case !c.enabled(cap):
			out[cap] = EvaluateWith(nil, false, c.sandboxAllowed, c.clk.Now())
		default:
			need = true
		}
	}
	if !need {
		return out, nil
	}
	rows, err := List(ctx, q, c.env)
	if err != nil {
		return nil, err
	}
	byCap := make(map[Capability]*Gate, len(rows))
	for i := range rows {
		byCap[rows[i].Capability] = &rows[i]
	}
	now := c.clk.Now()
	for _, cap := range caps {
		if _, done := out[cap]; done {
			continue
		}
		out[cap] = EvaluateWith(byCap[cap], true, c.sandboxAllowed, now)
	}
	return out, nil
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
