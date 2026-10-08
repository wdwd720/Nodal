// Package livegate is the agent runtime's own read of the LIVE_AGENT_TRADING
// capability gate: a SELECT on capability_gates and nothing else.
//
// It is a package of its own so that internal/gates can test it against
// gates.Evaluate (TestIntegration_TheAgentGateReaderAgreesWithTheAuthority)
// without importing the agent runtime, which reaches internal/gates through
// internal/delegation and would make that test an import cycle. It imports
// only the database layer and must stay that way: no agent tree package may
// import internal/gates, and this one must never be able to change a gate.
package livegate

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
)

// Capability is the capability gate that must be ACTIVE in this environment
// before any agent may run in LIVE mode (PART 70).
const Capability = "LIVE_AGENT_TRADING"

// LiveTrading is a read of the LIVE_AGENT_TRADING gate at one instant. Reason
// names the condition that refused and is empty when Enabled; it is the string
// an operator sees next to a refused LIVE run, so "expired" must not be
// reported as "ACTIVE".
type LiveTrading struct {
	Enabled bool
	State   string
	Reason  string
}

// Refusal reasons. They deliberately match the wording of the equivalent
// conditions in internal/gates so that the two readers of one gate can be
// compared by anyone reading the logs, as well as by the agreement test.
const (
	ReasonNoGateRow       = "no gate row"
	ReasonStateNotActive  = "gate state is not ACTIVE"
	ReasonRevoked         = "gate is revoked"
	ReasonNoEffectiveAt   = "effective_at is not set"
	ReasonNotYetEffective = "effective_at is in the future"
	ReasonExpired         = "expires_at has passed"
)

// Read reports whether the LIVE_AGENT_TRADING gate permits live agent trading
// in env at now. The gate defaults to DISABLED and no environment variable can
// turn it on: only the dual-controlled gate workflow in internal/gates can,
// under an operator principal.
//
// It used to select effective_at and expires_at and then decide on `state`
// alone, discarding both -- so a gate whose window had closed still read as
// live here, while gates.Evaluate called the same row inactive (F-72). Nothing
// calls gates.Admin.ExpireDue, so the persisted state never catches up on its
// own: the divergence lasted for as long as the row sat there.
//
// The conditions below are the ones that can change after activation without a
// state transition. The quorum and evidence conditions gates.Evaluate also
// applies are not re-checked here, and need not be: Activate enforces them
// before it writes ACTIVE, and 00701 leaves cp_app no UPDATE on any column of
// this table except `version`, so no approver, evidence reference or window can
// be moved underneath us by the application at all.
func Read(ctx context.Context, q db.Querier, env string, now time.Time) (LiveTrading, error) {
	var (
		state       string
		effectiveAt *time.Time
		expiresAt   *time.Time
		revokedAt   *time.Time
	)
	err := q.QueryRow(ctx,
		`SELECT state, effective_at, expires_at, revoked_at FROM capability_gates
			WHERE capability = $1 AND environment = $2`,
		Capability, env).Scan(&state, &effectiveAt, &expiresAt, &revokedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Absent means disabled: fail closed.
		return LiveTrading{State: "DISABLED", Reason: ReasonNoGateRow}, nil
	case err != nil:
		return LiveTrading{}, errs.Wrap(err, errs.CodeInternal, "agent: read live trading gate")
	}
	now = now.UTC()
	refuse := func(reason string) (LiveTrading, error) {
		return LiveTrading{State: state, Reason: reason}, nil
	}
	switch {
	case state != "ACTIVE":
		return refuse(ReasonStateNotActive)
	case revokedAt != nil:
		return refuse(ReasonRevoked)
	case effectiveAt == nil:
		return refuse(ReasonNoEffectiveAt)
	case now.Before(*effectiveAt):
		return refuse(ReasonNotYetEffective)
	case expiresAt != nil && !now.Before(*expiresAt):
		return refuse(ReasonExpired)
	}
	return LiveTrading{Enabled: true, State: state}, nil
}
