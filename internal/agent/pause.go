package agent

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
)

// PauseReason mirrors the agent_pauses.reason_code CHECK.
type PauseReason string

// The declared pause reasons.
const (
	PauseOwnerRequest           PauseReason = "OWNER_REQUEST"
	PauseOperator               PauseReason = "OPERATOR"
	PauseKillSwitch             PauseReason = "KILL_SWITCH"
	PauseBudgetExhausted        PauseReason = "BUDGET_EXHAUSTED"
	PauseModelUnavailable       PauseReason = "MODEL_UNAVAILABLE"
	PauseDataGap                PauseReason = "DATA_GAP"
	PauseRiskViolation          PauseReason = "RISK_VIOLATION"
	PauseReconciliationMismatch PauseReason = "RECONCILIATION_MISMATCH"
	PauseSecurity               PauseReason = "SECURITY"
)

var allPauseReasons = []PauseReason{
	PauseOwnerRequest, PauseOperator, PauseKillSwitch, PauseBudgetExhausted,
	PauseModelUnavailable, PauseDataGap, PauseRiskViolation,
	PauseReconciliationMismatch, PauseSecurity,
}

// PauseReasons returns every declared reason code.
func PauseReasons() []PauseReason { return append([]PauseReason(nil), allPauseReasons...) }

// Valid reports whether r is declared.
func (r PauseReason) Valid() bool {
	for _, v := range allPauseReasons {
		if v == r {
			return true
		}
	}
	return false
}

// String renders the reason code.
func (r PauseReason) String() string { return string(r) }

// SystemMayRaise reports whether the SYSTEM actor may raise this pause.
// Owner and operator requests are by definition not automatic.
func (r PauseReason) SystemMayRaise() bool {
	return r != PauseOwnerRequest && r != PauseOperator
}

// OpenOrdersPolicy is the explicit choice made at pause time about orders
// that are already open (PART 71). There is no implicit cancellation: an
// external transaction is never abandoned because its strategy was paused.
type OpenOrdersPolicy string

// The two policies.
const (
	// LeaveOpenOrders leaves every open order alone. Observation,
	// reconciliation, settlement, ledger posting and position updates all
	// continue; a kill switch never blocks those action classes.
	LeaveOpenOrders OpenOrdersPolicy = "LEAVE"
	// CancelCancelableOrders asks the explicit cancellation workflow to
	// cancel only those open orders a venue can still cancel. Orders that
	// have been submitted and cannot be cancelled continue to finality.
	CancelCancelableOrders OpenOrdersPolicy = "CANCEL_CANCELABLE"
)

// Valid reports whether p is declared.
func (p OpenOrdersPolicy) Valid() bool {
	return p == LeaveOpenOrders || p == CancelCancelableOrders
}

// String renders the policy.
func (p OpenOrdersPolicy) String() string { return string(p) }

// Pause is one agent_pauses row. At most one may be open per agent.
type Pause struct {
	ID                 PauseID
	AgentID            AgentID
	ReasonCode         PauseReason
	Reason             string
	KillSwitchID       string
	OpenOrdersPolicy   OpenOrdersPolicy
	CancelWorkflowID   string
	PausedByActorType  security.ActorType
	PausedByActorID    string
	PausedAt           time.Time
	ResumedAt          *time.Time
	ResumedByActorType security.ActorType
	ResumedByActorID   string
	ResumeReason       string
	CorrelationID      string
}

// Open reports whether the pause is still in force.
func (p Pause) Open() bool { return p.ResumedAt == nil }

// PauseRequest is the caller's side of Lifecycle.Pause. OpenOrdersPolicy has
// no default: choosing what happens to money already in flight is always an
// explicit decision.
type PauseRequest struct {
	ReasonCode       PauseReason
	Reason           string
	OpenOrdersPolicy OpenOrdersPolicy
	// KillSwitchID is required when ReasonCode is KILL_SWITCH.
	KillSwitchID string
	// CancelWorkflowID is the workflow that will cancel cancelable orders. It
	// is recorded only with the CANCEL_CANCELABLE policy; the runtime never
	// cancels an order itself.
	CancelWorkflowID string
	CorrelationID    string
}

// MinReasonLength keeps a pause auditable: "x" is not a reason.
const MinReasonLength = 8

// Validate reports the structural reasons the request cannot be recorded.
func (r PauseRequest) Validate() error {
	fields := map[string]any{}
	fail := func(k, msg string) {
		if _, dup := fields[k]; !dup {
			fields[k] = msg
		}
	}
	if !r.ReasonCode.Valid() {
		fail("reason_code", "unknown pause reason")
	}
	if len(r.Reason) < MinReasonLength {
		fail("reason", "must be at least 8 characters")
	}
	if !r.OpenOrdersPolicy.Valid() {
		fail("open_orders_policy", "must be LEAVE or CANCEL_CANCELABLE; there is no default")
	}
	if r.ReasonCode == PauseKillSwitch && r.KillSwitchID == "" {
		fail("kill_switch_id", "required when the reason is KILL_SWITCH")
	}
	if r.OpenOrdersPolicy == LeaveOpenOrders && r.CancelWorkflowID != "" {
		fail("cancel_workflow_id", "only the CANCEL_CANCELABLE policy names a workflow")
	}
	if len(fields) == 0 {
		return nil
	}
	return errs.New(errs.CodeValidationFailed, "agent: invalid pause request").WithFields(fields)
}

// CanResume reports whether an actor of this type may lift a pause. PART 71:
// resume is restricted to USER and OPERATOR. Never SYSTEM, never an agent.
// An automatic pause therefore needs a person to clear it, which is the
// point: the condition that raised it must be judged, not merely elapsed.
func CanResume(a security.ActorType) bool {
	return a == security.ActorUser || a == security.ActorOperator
}

const selectOpenPauseSQL = `
SELECT id, agent_id, reason_code, reason, coalesce(kill_switch_id::text, ''), open_orders_policy,
       coalesce(cancel_workflow_id, ''), paused_by_actor_type, paused_by_actor_id, paused_at,
       resumed_at, coalesce(resumed_by_actor_type, ''), coalesce(resumed_by_actor_id, ''),
       coalesce(resume_reason, ''), coalesce(correlation_id, '')
  FROM agent_pauses
 WHERE agent_id = $1 AND resumed_at IS NULL`

// PGPauseChecker reads the open pause of an agent. It is the authority the
// broker, the dispatcher and the intent emitter consult, so a pause committed
// by an operator is visible to every path that starts afterwards, including a
// run already in flight (each of its steps re-reads).
type PGPauseChecker struct{}

var _ PauseChecker = PGPauseChecker{}

// NewPauseChecker returns the PostgreSQL pause checker.
func NewPauseChecker() PGPauseChecker { return PGPauseChecker{} }

// OpenPause returns the open pause of the agent, if any.
func (PGPauseChecker) OpenPause(ctx context.Context, q db.Querier, agentID AgentID) (Pause, bool, error) {
	var (
		p                   Pause
		reason, policy      string
		pausedType, resType string
	)
	err := q.QueryRow(ctx, selectOpenPauseSQL, agentID).Scan(
		&p.ID, &p.AgentID, &reason, &p.Reason, &p.KillSwitchID, &policy,
		&p.CancelWorkflowID, &pausedType, &p.PausedByActorID, &p.PausedAt,
		&p.ResumedAt, &resType, &p.ResumedByActorID, &p.ResumeReason, &p.CorrelationID,
	)
	switch {
	case isNoRows(err):
		return Pause{}, false, nil
	case err != nil:
		return Pause{}, false, errs.Wrap(err, errs.CodeInternal, "agent: read open pause")
	}
	p.ReasonCode = PauseReason(reason)
	p.OpenOrdersPolicy = OpenOrdersPolicy(policy)
	p.PausedByActorType = security.ActorType(pausedType)
	p.ResumedByActorType = security.ActorType(resType)
	return p, true, nil
}

const insertPauseSQL = `
INSERT INTO agent_pauses (
    id, agent_id, reason_code, reason, kill_switch_id, open_orders_policy, cancel_workflow_id,
    paused_by_actor_type, paused_by_actor_id, paused_at, correlation_id
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`

const resumePauseSQL = `
UPDATE agent_pauses
   SET resumed_at = $2, resumed_by_actor_type = $3, resumed_by_actor_id = $4, resume_reason = $5
 WHERE agent_id = $1 AND resumed_at IS NULL`

// insertPause records a new open pause. The partial unique index
// agent_pauses_open_idx makes a second open pause a unique violation, which
// is mapped to CONFLICT.
func insertPause(ctx context.Context, tx pgx.Tx, p Pause) error {
	_, err := tx.Exec(ctx, insertPauseSQL,
		p.ID, p.AgentID, string(p.ReasonCode), p.Reason, nullUUID(p.KillSwitchID),
		string(p.OpenOrdersPolicy), nullText(p.CancelWorkflowID),
		string(p.PausedByActorType), p.PausedByActorID, p.PausedAt, nullText(p.CorrelationID),
	)
	if err != nil {
		if db.IsUniqueViolation(err) {
			return errs.Newf(errs.CodeConflict, "agent: %s is already paused", p.AgentID).
				WithField("agent_id", p.AgentID.String())
		}
		return errs.Wrap(err, errs.CodeInternal, "agent: record pause")
	}
	return nil
}

// closePause lifts the open pause of an agent.
func closePause(ctx context.Context, tx pgx.Tx, agentID AgentID, at time.Time, actorType security.ActorType, actorID, reason string) error {
	tag, err := tx.Exec(ctx, resumePauseSQL, agentID, at, string(actorType), actorID, nullText(reason))
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "agent: close pause")
	}
	if tag.RowsAffected() == 0 {
		return errs.Newf(errs.CodeInvalidStateTransition, "agent: %s has no open pause", agentID).
			WithField("agent_id", agentID.String())
	}
	return nil
}
