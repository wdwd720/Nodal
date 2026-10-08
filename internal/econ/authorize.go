package econ

import (
	"context"
	"strings"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/killswitch"
)

// GateReader answers whether a capability's gate is ACTIVE. *gates.Checker
// satisfies it.
type GateReader interface {
	IsActive(ctx context.Context, q db.Querier, c gates.Capability) (gates.Verdict, error)
	Environment() string
}

// KillSwitchReader returns the active kill switches. *killswitch.Checker
// satisfies it.
type KillSwitchReader interface {
	Snapshot(ctx context.Context, q db.Querier) (killswitch.Snapshot, error)
}

// Authorizer decides, inside the caller's transaction, whether an advanced
// capability may be exercised in a mode in this deployment.
type Authorizer struct {
	gates GateReader
	kills KillSwitchReader
}

// NewAuthorizer wires an Authorizer. Both readers are required: an
// authorizer that could not see the kill switches would authorize through
// one.
func NewAuthorizer(g GateReader, k KillSwitchReader) *Authorizer {
	if g == nil || k == nil {
		panic("econ: NewAuthorizer requires a gate reader and a kill-switch reader")
	}
	return &Authorizer{gates: g, kills: k}
}

// Decide returns the full mode decision, every reason included.
func (a *Authorizer) Decide(ctx context.Context, q db.Querier, c gates.Capability, m gates.Mode) (gates.ModeDecision, error) {
	verdict, err := a.gates.IsActive(ctx, q, c)
	if err != nil {
		return gates.ModeDecision{}, err
	}
	snap, err := a.kills.Snapshot(ctx, q)
	if err != nil {
		return gates.ModeDecision{}, err
	}
	killed := snap.Has(killswitch.CapabilityDisable, string(c)) || snap.Global()
	return gates.DecideMode(gates.ModeRequest{
		Capability:   c,
		Environment:  a.gates.Environment(),
		Mode:         m,
		GateActive:   verdict.Active,
		KillSwitched: killed,
	}), nil
}

// Require returns nil when the capability may be exercised, and otherwise
// CAPABILITY_NOT_APPROVED (or KILL_SWITCH_ACTIVE) carrying every reason.
func (a *Authorizer) Require(ctx context.Context, q db.Querier, c gates.Capability, m gates.Mode) error {
	d, err := a.Decide(ctx, q, c, m)
	if err != nil {
		return err
	}
	if d.Allowed {
		return nil
	}
	code := errs.CodeCapabilityNotApproved
	for _, r := range d.Reasons {
		if r == gates.ReasonCapabilityKillSwitched {
			code = errs.CodeKillSwitchActive
		}
	}
	return errs.Newf(code, "%s may not be exercised in mode %s here: %s", c, m, strings.Join(d.Reasons, ", ")).
		WithField("capability", string(c)).
		WithField("mode", string(m)).
		WithField("reasons", d.Reasons)
}
