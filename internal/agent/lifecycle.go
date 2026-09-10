package agent

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
)

// AuditStream is the audit stream lifecycle changes are appended to.
// Per-agent evidence goes to audit.AgentStream(agentID); the operator view
// of the same event goes here.
const AuditStream = "admin"

// StepUpMaxAge is how recent a strong authentication must be for a promotion
// into a stage that deploys real capital.
const StepUpMaxAge = 15 * time.Minute

// ApprovalKindPromote is the admin_actions.kind a promotion approval carries.
const ApprovalKindPromote = "AGENT_PROMOTE"

// AuditEvent is the subset of internal/audit's Event this package emits. The
// composition root adapts it; the field names match so the adapter is a
// field copy. Declaring it here keeps internal/agent free of an import it
// does not need in order to test the lifecycle.
type AuditEvent struct {
	Stream        string
	ActorType     string
	ActorID       string
	Action        string
	ResourceType  string
	ResourceID    string
	Reason        string
	EvidenceRef   string
	PolicyVersion string
	CorrelationID string
	Payload       json.RawMessage
	OccurredAt    time.Time
}

// AuditAppender appends an audit event inside the caller's transaction. It
// must fail, and so roll the transition back, rather than drop the event.
type AuditAppender interface {
	Append(ctx context.Context, tx pgx.Tx, e AuditEvent) error
}

// Approval is the verified shape of an admin_actions row.
type Approval struct {
	ID         string
	Kind       string
	TargetID   string
	ProposedBy string
	ApprovedBy string
	ExpiresAt  time.Time
}

// ApprovalVerifier reads an approved admin action. internal/agent does not
// import internal/admin; the composition root supplies the adapter.
type ApprovalVerifier interface {
	VerifyApproved(ctx context.Context, q db.Querier, approvalID, kind, targetID string) (Approval, error)
}

// KillSwitchMirror mirrors a pause into the global kill-switch table so the
// risk kernel and the intent service observe it without consulting the agent
// subsystem (AGENT_RUNTIME.md §3). It is optional: the pause itself is
// authoritative in agent_pauses and every agent-runtime path consults that
// directly, so a deployment without a mirror still pauses correctly.
// internal/agent does not import internal/killswitch.
type KillSwitchMirror interface {
	// Activate raises AGENT_PAUSE(agentID). It runs inside the pause's
	// transaction so the mirror and the pause commit together.
	Activate(ctx context.Context, tx pgx.Tx, agentID AgentID, reason string) (switchID string, err error)
	// Release lowers it on resume.
	Release(ctx context.Context, tx pgx.Tx, agentID AgentID, reason string) error
}

// Lifecycle drives the PART 68 state machine. Every method rejects an AGENT
// principal before issuing any query: an agent can never move its own stage,
// pause or resume itself, or approve anything.
type Lifecycle struct {
	clk       clock.Clock
	store     Store
	audit     AuditAppender
	approvals ApprovalVerifier
	mirror    KillSwitchMirror
	build     string
}

// LifecycleDeps are the lifecycle service's collaborators.
type LifecycleDeps struct {
	Clock     clock.Clock
	Audit     AuditAppender
	Approvals ApprovalVerifier
	// Mirror is optional.
	Mirror KillSwitchMirror
	// BuildVersion is stamped on every transition; defaults to
	// config.BuildVersion.
	BuildVersion string
}

// NewLifecycle builds the service. Clock, Audit and Approvals are required:
// a promotion that cannot be audited or whose approval cannot be verified
// must not be possible at all.
func NewLifecycle(deps LifecycleDeps) (*Lifecycle, error) {
	switch {
	case deps.Clock == nil:
		return nil, errs.New(errs.CodeValidationFailed, "agent: lifecycle requires a clock")
	case deps.Audit == nil:
		return nil, errs.New(errs.CodeValidationFailed, "agent: lifecycle requires an audit appender")
	case deps.Approvals == nil:
		return nil, errs.New(errs.CodeValidationFailed, "agent: lifecycle requires an approval verifier")
	}
	build := deps.BuildVersion
	if build == "" {
		build = config.BuildVersion
	}
	return &Lifecycle{
		clk: deps.Clock, store: NewStore(), audit: deps.Audit,
		approvals: deps.Approvals, mirror: deps.Mirror, build: build,
	}, nil
}

// actor extracts the acting principal and refuses anything that may not touch
// the agent lifecycle. It runs before any query.
func actor(ctx context.Context) (security.Principal, error) {
	p, ok := security.PrincipalFrom(ctx)
	if !ok {
		return security.Principal{}, errs.New(errs.CodeUnauthenticated, "authentication required")
	}
	if err := p.Validate(); err != nil {
		return security.Principal{}, errs.Wrap(err, errs.CodeForbidden, "invalid principal")
	}
	switch p.ActorType {
	case security.ActorAgent:
		return security.Principal{}, errs.New(errs.CodeForbidden, "agents cannot change an agent lifecycle").
			WithField("actor_type", string(p.ActorType))
	case security.ActorUser, security.ActorOperator, security.ActorSystem:
		return p, nil
	default:
		return security.Principal{}, errs.Newf(errs.CodeForbidden, "actor type %s cannot change an agent lifecycle", p.ActorType).
			WithField("actor_type", string(p.ActorType))
	}
}

// authError maps the security package's sentinels onto stable codes.
func authError(err error) error {
	switch {
	case errors.Is(err, security.ErrUnauthenticated):
		return errs.Wrap(err, errs.CodeUnauthenticated, "authentication required")
	case errors.Is(err, security.ErrStepUpRequired):
		return errs.Wrap(err, errs.CodeStepUpRequired, "recent strong authentication required")
	default:
		return errs.Wrap(err, errs.CodeForbidden, "forbidden")
	}
}

func (l *Lifecycle) require(ctx context.Context, perm security.Permission) error {
	if err := security.RequireAt(ctx, perm, l.clk.Now); err != nil {
		return authError(err)
	}
	return nil
}

// CreateAgent is the request side of Create.
type CreateAgent struct {
	AccountID         string
	StrategyID        string
	StrategyVersionID string
	Name              string
	RiskPolicyVersion string
	CorrelationID     string
}

// Create records a new agent in DRAFT. A DRAFT agent has no mode, no
// envelope and no authority: it cannot run and cannot propose anything.
func (l *Lifecycle) Create(ctx context.Context, tx pgx.Tx, req CreateAgent) (Agent, error) {
	p, err := actor(ctx)
	if err != nil {
		return Agent{}, err
	}
	if err := l.require(ctx, security.PermStrategyWrite); err != nil {
		return Agent{}, err
	}
	if err := security.RequireAccount(ctx, req.AccountID); err != nil {
		return Agent{}, authError(err)
	}
	now := l.clk.Now()
	a := Agent{
		ID:                 NewAgentID(),
		AccountID:          req.AccountID,
		StrategyID:         req.StrategyID,
		StrategyVersionID:  req.StrategyVersionID,
		Name:               req.Name,
		Stage:              StageDraft,
		State:              StateDraft,
		RiskPolicyVersion:  req.RiskPolicyVersion,
		Version:            1,
		CreatedByActorType: string(p.ActorType),
		CreatedByActorID:   p.SubjectID,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if err := a.Validate(); err != nil {
		return Agent{}, err
	}
	if err := insertAgent(ctx, tx, a); err != nil {
		return Agent{}, err
	}
	// The creation itself is a transition from nothing into DRAFT.
	t := Transition{
		ID: NewTransitionID(), AgentID: a.ID,
		FromState: StateDraft, ToState: StateDraft, FromStage: StageDraft, ToStage: StageDraft,
		ActorType: p.ActorType, ActorID: p.SubjectID, Reason: "agent created",
		StrategyVersionID: req.StrategyVersionID, RiskPolicyVersion: req.RiskPolicyVersion,
		BuildVersion: l.build, CorrelationID: req.CorrelationID, OccurredAt: now,
	}
	if err := insertTransition(ctx, tx, t); err != nil {
		return Agent{}, err
	}
	if err := l.appendAudit(ctx, tx, p, a, "agent.created", "agent created", "", req.CorrelationID, now, t); err != nil {
		return Agent{}, err
	}
	return a, nil
}

// Promote moves the agent one rung up the ladder. It refuses a skipped rung,
// an AGENT actor, missing or unhashed evidence, and — for CANARY, LIMITED and
// LIVE — a missing or self-approved admin action.
//
// Dual control is honored structurally: `agent:promote` is what the
// proposing operator holds, `agent:promote_approve` is a dual-control
// permission that no standing role grants, and the approval row itself must
// name a different approver from its proposer. One principal therefore cannot
// both propose and approve.
func (l *Lifecycle) Promote(ctx context.Context, tx pgx.Tx, agentID AgentID, to State, ev PromotionEvidence) (Agent, error) {
	p, err := actor(ctx)
	if err != nil {
		return Agent{}, err
	}
	if err := l.require(ctx, security.PermAgentPromote); err != nil {
		return Agent{}, err
	}
	target := Stage(to)
	if !target.Valid() {
		return Agent{}, errs.Newf(errs.CodeInvalidStateTransition, "agent: %s is not a ladder position", to).
			WithField("to", string(to))
	}
	a, err := l.store.GetForUpdate(ctx, tx, agentID)
	if err != nil {
		return Agent{}, err
	}
	if !CanTransition(a.State, to) {
		return Agent{}, errs.Newf(errs.CodeInvalidStateTransition, "agent: %s cannot move from %s to %s", agentID, a.State, to).
			WithField("from", string(a.State)).WithField("to", string(to))
	}
	if a.State == StatePaused {
		return Agent{}, errs.New(errs.CodeInvalidStateTransition, "agent: resume before promoting").
			WithField("from", string(a.State))
	}
	if err := ev.Validate(target); err != nil {
		return Agent{}, err
	}
	if RequiresApproval(target) {
		if err := security.RequireStepUp(ctx, StepUpMaxAge, l.clk.Now); err != nil {
			return Agent{}, authError(err)
		}
		if err := l.verifyApproval(ctx, tx, ev.ApprovalID, agentID, p, target); err != nil {
			return Agent{}, err
		}
	}
	if ev.StrategyVersionID != "" && a.StrategyVersionID != "" && ev.StrategyVersionID != a.StrategyVersionID {
		return Agent{}, errs.New(errs.CodeValidationFailed, "agent: evidence names a different strategy version").
			WithField("strategy_version_id", ev.StrategyVersionID)
	}

	mode := ev.Mode
	if mode == "" {
		mode = DefaultModeForStage(target)
	}
	now := l.clk.Now()
	next := a
	next.Stage = target
	next.State = to
	next.Mode = mode
	if ev.StrategyVersionID != "" {
		next.StrategyVersionID = ev.StrategyVersionID
	}
	if ev.EnvelopeID != "" {
		next.EnvelopeID = ev.EnvelopeID
	}
	if ev.RiskPolicyVersion != "" {
		next.RiskPolicyVersion = ev.RiskPolicyVersion
	}
	next.UpdatedAt = now
	if err := next.Validate(); err != nil {
		return Agent{}, err
	}
	t, err := l.transitionFor(a, next, p, ev, now)
	if err != nil {
		return Agent{}, err
	}
	if err := l.commitTransition(ctx, tx, p, next, t, "agent.promoted", ev.Reason, ev.ApprovalID, ev.CorrelationID, now); err != nil {
		return Agent{}, err
	}
	return next, nil
}

// verifyApproval checks the admin_actions row backing a promotion: right
// kind, right target, dual-controlled, unexpired, and not proposed by the
// principal performing the promotion.
func (l *Lifecycle) verifyApproval(ctx context.Context, q db.Querier, approvalID string, agentID AgentID, p security.Principal, to Stage) error {
	ap, err := l.approvals.VerifyApproved(ctx, q, approvalID, ApprovalKindPromote, agentID.String())
	if err != nil {
		if _, ok := errs.As(err); ok {
			return err
		}
		return errs.Wrap(err, errs.CodeForbidden, "agent: promotion approval could not be verified").
			WithField("approval_id", approvalID)
	}
	now := l.clk.Now()
	switch {
	case ap.ID != approvalID:
		return errs.New(errs.CodeForbidden, "agent: promotion approval id mismatch").WithField("approval_id", approvalID)
	case ap.Kind != ApprovalKindPromote:
		return errs.New(errs.CodeForbidden, "agent: approval has the wrong kind").
			WithField("approval_id", approvalID).WithField("kind", ap.Kind)
	case ap.TargetID != agentID.String():
		return errs.New(errs.CodeForbidden, "agent: approval targets a different agent").
			WithField("approval_id", approvalID).WithField("target_id", ap.TargetID)
	case ap.ProposedBy == "" || ap.ApprovedBy == "" || ap.ProposedBy == ap.ApprovedBy:
		return errs.New(errs.CodeForbidden, "agent: promotion approval is not dual-controlled").
			WithField("approval_id", approvalID)
	case ap.ApprovedBy == p.SubjectID:
		// The approver may not also execute the promotion: that would let one
		// principal hold both halves of the two-person rule.
		return errs.Newf(errs.CodeForbidden, "agent: %s approved this promotion and may not also perform it", p.SubjectID).
			WithField("approval_id", approvalID)
	case !ap.ExpiresAt.IsZero() && !now.Before(ap.ExpiresAt):
		return errs.New(errs.CodeForbidden, "agent: promotion approval has expired").WithField("approval_id", approvalID)
	}
	if to == StageLive && !p.HasRole(security.RoleRisk) && !p.HasRole(security.RoleAdmin) {
		return errs.New(errs.CodeForbidden, "agent: promotion into LIVE requires the RISK role").
			WithField("to", to.String())
	}
	return nil
}

// Pause suspends the agent. The open-orders policy is explicit: PART 71
// never cancels implicitly, and an external transaction is never abandoned
// because its strategy was paused.
func (l *Lifecycle) Pause(ctx context.Context, tx pgx.Tx, agentID AgentID, req PauseRequest) (Pause, error) {
	p, err := actor(ctx)
	if err != nil {
		return Pause{}, err
	}
	if err := req.Validate(); err != nil {
		return Pause{}, err
	}
	if p.ActorType == security.ActorSystem && !req.ReasonCode.SystemMayRaise() {
		return Pause{}, errs.Newf(errs.CodeForbidden, "agent: SYSTEM cannot raise a %s pause", req.ReasonCode).
			WithField("reason_code", req.ReasonCode.String())
	}
	if p.ActorType != security.ActorSystem {
		if err := l.require(ctx, security.PermAgentPause); err != nil {
			return Pause{}, err
		}
	}
	a, err := l.store.GetForUpdate(ctx, tx, agentID)
	if err != nil {
		return Pause{}, err
	}
	if p.ActorType == security.ActorUser {
		if err := security.RequireAccount(ctx, a.AccountID); err != nil {
			return Pause{}, authError(err)
		}
	}
	if !CanTransition(a.State, StatePaused) {
		return Pause{}, errs.Newf(errs.CodeInvalidStateTransition, "agent: %s cannot be paused from %s", agentID, a.State).
			WithField("from", string(a.State))
	}
	now := l.clk.Now()
	pause := Pause{
		ID: NewPauseID(), AgentID: agentID, ReasonCode: req.ReasonCode, Reason: req.Reason,
		KillSwitchID: req.KillSwitchID, OpenOrdersPolicy: req.OpenOrdersPolicy,
		CancelWorkflowID: req.CancelWorkflowID, PausedByActorType: p.ActorType,
		PausedByActorID: p.SubjectID, PausedAt: now, CorrelationID: req.CorrelationID,
	}
	if l.mirror != nil && req.ReasonCode != PauseKillSwitch {
		switchID, err := l.mirror.Activate(ctx, tx, agentID, req.Reason)
		if err != nil {
			return Pause{}, err
		}
		if switchID != "" && pause.KillSwitchID == "" {
			pause.KillSwitchID = switchID
		}
	}
	if err := insertPause(ctx, tx, pause); err != nil {
		return Pause{}, err
	}
	next := a
	next.State = StatePaused
	next.UpdatedAt = now
	t := Transition{
		ID: NewTransitionID(), AgentID: agentID,
		FromState: a.State, ToState: StatePaused, FromStage: a.Stage, ToStage: a.Stage,
		ActorType: p.ActorType, ActorID: p.SubjectID,
		Reason:            req.ReasonCode.String() + ": " + req.Reason,
		StrategyVersionID: a.StrategyVersionID, RiskPolicyVersion: a.RiskPolicyVersion,
		BuildVersion: l.build, CorrelationID: req.CorrelationID, OccurredAt: now,
	}
	if err := l.commitTransition(ctx, tx, p, next, t, "agent.paused", req.Reason, "", req.CorrelationID, now); err != nil {
		return Pause{}, err
	}
	return pause, nil
}

// Resume lifts the pause and returns the agent to its stage. PART 71: only a
// USER or an OPERATOR may resume — never SYSTEM, never an agent. A pause
// raised automatically therefore needs a person to judge that the condition
// is gone.
func (l *Lifecycle) Resume(ctx context.Context, tx pgx.Tx, agentID AgentID, reason string) (Agent, error) {
	p, err := actor(ctx)
	if err != nil {
		return Agent{}, err
	}
	if !CanResume(p.ActorType) {
		return Agent{}, errs.Newf(errs.CodeForbidden, "agent: %s cannot resume an agent; only USER or OPERATOR may", p.ActorType).
			WithField("actor_type", string(p.ActorType))
	}
	if err := l.require(ctx, security.PermAgentPause); err != nil {
		return Agent{}, err
	}
	if len(reason) < MinReasonLength {
		return Agent{}, errs.New(errs.CodeValidationFailed, "agent: resume needs a reason of at least 8 characters").
			WithField("reason", "too short")
	}
	a, err := l.store.GetForUpdate(ctx, tx, agentID)
	if err != nil {
		return Agent{}, err
	}
	if p.ActorType == security.ActorUser {
		if err := security.RequireAccount(ctx, a.AccountID); err != nil {
			return Agent{}, authError(err)
		}
	}
	if a.State != StatePaused {
		return Agent{}, errs.Newf(errs.CodeInvalidStateTransition, "agent: %s is %s, not PAUSED", agentID, a.State).
			WithField("from", string(a.State))
	}
	now := l.clk.Now()
	if err := closePause(ctx, tx, agentID, now, p.ActorType, p.SubjectID, reason); err != nil {
		return Agent{}, err
	}
	if l.mirror != nil {
		if err := l.mirror.Release(ctx, tx, agentID, reason); err != nil {
			return Agent{}, err
		}
	}
	next := a
	next.State = a.Stage.State()
	next.UpdatedAt = now
	if err := next.Validate(); err != nil {
		return Agent{}, err
	}
	t := Transition{
		ID: NewTransitionID(), AgentID: agentID,
		FromState: StatePaused, ToState: next.State, FromStage: a.Stage, ToStage: a.Stage,
		ActorType: p.ActorType, ActorID: p.SubjectID, Reason: reason,
		StrategyVersionID: a.StrategyVersionID, RiskPolicyVersion: a.RiskPolicyVersion,
		BuildVersion: l.build, OccurredAt: now,
	}
	if err := l.commitTransition(ctx, tx, p, next, t, "agent.resumed", reason, "", "", now); err != nil {
		return Agent{}, err
	}
	return next, nil
}

// Fail moves the agent to FAILED. Only SYSTEM and OPERATOR may: a failure is
// a statement about the platform's own observation, not a customer request.
func (l *Lifecycle) Fail(ctx context.Context, tx pgx.Tx, agentID AgentID, reason string) (Agent, error) {
	p, err := actor(ctx)
	if err != nil {
		return Agent{}, err
	}
	if p.ActorType == security.ActorUser {
		return Agent{}, errs.New(errs.CodeForbidden, "agent: only OPERATOR or SYSTEM may fail an agent").
			WithField("actor_type", string(p.ActorType))
	}
	return l.sideTransition(ctx, tx, p, agentID, StateFailed, "agent.failed", reason, "")
}

// Revoke terminates the agent. Only an operator may.
func (l *Lifecycle) Revoke(ctx context.Context, tx pgx.Tx, agentID AgentID, reason string) (Agent, error) {
	p, err := actor(ctx)
	if err != nil {
		return Agent{}, err
	}
	if p.ActorType != security.ActorOperator {
		return Agent{}, errs.New(errs.CodeForbidden, "agent: only an OPERATOR may revoke an agent").
			WithField("actor_type", string(p.ActorType))
	}
	if err := l.require(ctx, security.PermAgentPromote); err != nil {
		return Agent{}, err
	}
	return l.sideTransition(ctx, tx, p, agentID, StateRevoked, "agent.revoked", reason, "")
}

// Supersede terminates the agent because a newer one replaced it.
func (l *Lifecycle) Supersede(ctx context.Context, tx pgx.Tx, agentID, by AgentID) (Agent, error) {
	p, err := actor(ctx)
	if err != nil {
		return Agent{}, err
	}
	if by.IsZero() || by == agentID {
		return Agent{}, errs.New(errs.CodeValidationFailed, "agent: superseding agent must be a different agent")
	}
	return l.sideTransition(ctx, tx, p, agentID, StateSuperseded, "agent.superseded",
		"superseded by "+by.String(), by.String())
}

// sideTransition performs FAILED / REVOKED / SUPERSEDED.
func (l *Lifecycle) sideTransition(ctx context.Context, tx pgx.Tx, p security.Principal, agentID AgentID,
	to State, action, reason, supersededBy string,
) (Agent, error) {
	if len(reason) < MinReasonLength {
		return Agent{}, errs.New(errs.CodeValidationFailed, "agent: a lifecycle change needs a reason of at least 8 characters").
			WithField("reason", "too short")
	}
	a, err := l.store.GetForUpdate(ctx, tx, agentID)
	if err != nil {
		return Agent{}, err
	}
	if !CanTransition(a.State, to) {
		return Agent{}, errs.Newf(errs.CodeInvalidStateTransition, "agent: %s cannot move from %s to %s", agentID, a.State, to).
			WithField("from", string(a.State)).WithField("to", string(to))
	}
	now := l.clk.Now()
	next := a
	next.State = to
	next.UpdatedAt = now
	if to == StateFailed {
		next.FailureReason = reason
	}
	if supersededBy != "" {
		next.SupersededByAgentID = supersededBy
	}
	if err := next.Validate(); err != nil {
		return Agent{}, err
	}
	t := Transition{
		ID: NewTransitionID(), AgentID: agentID,
		FromState: a.State, ToState: to, FromStage: a.Stage, ToStage: a.Stage,
		ActorType: p.ActorType, ActorID: p.SubjectID, Reason: reason,
		StrategyVersionID: a.StrategyVersionID, RiskPolicyVersion: a.RiskPolicyVersion,
		BuildVersion: l.build, OccurredAt: now,
	}
	if err := l.commitTransition(ctx, tx, p, next, t, action, reason, "", "", now); err != nil {
		return Agent{}, err
	}
	return next, nil
}

// transitionFor assembles the promotion transition row from the evidence.
func (l *Lifecycle) transitionFor(from, to Agent, p security.Principal, ev PromotionEvidence, now time.Time) (Transition, error) {
	evidenceJSON, err := ev.EvidenceJSON()
	if err != nil {
		return Transition{}, err
	}
	health := ev.OperationalHealth
	if len(health) == 0 {
		health = json.RawMessage(`{}`)
	}
	rate := int(ev.ErrorRateBPS)
	t := Transition{
		ID: NewTransitionID(), AgentID: from.ID,
		FromState: from.State, ToState: to.State, FromStage: from.Stage, ToStage: to.Stage,
		ActorType: p.ActorType, ActorID: p.SubjectID, Reason: ev.Reason,
		ApprovalID:            ev.ApprovalID,
		StrategyVersionID:     firstNonEmpty(ev.StrategyVersionID, to.StrategyVersionID),
		IRHash:                ev.IRHash,
		DatasetRef:            ev.DatasetRef,
		DatasetHash:           ev.DatasetHash,
		RiskPolicyVersion:     firstNonEmpty(ev.RiskPolicyVersion, to.RiskPolicyVersion),
		RiskPolicyHash:        ev.RiskPolicyHash,
		BacktestID:            ev.BacktestID,
		PerformanceSnapshotID: ev.PerformanceSnapshotID,
		CalibrationSnapshotID: ev.CalibrationSnapshotID,
		ErrorRateBPS:          &rate,
		OperationalHealth:     health,
		Evidence:              evidenceJSON,
		EvidenceHash:          ev.Hash(),
		BuildVersion:          l.build,
		CorrelationID:         ev.CorrelationID,
		OccurredAt:            now,
	}
	return t, nil
}

// commitTransition writes the transition row, the new agent state and the
// audit event in the caller's transaction. Migration 00690 refuses the state
// update at COMMIT unless the transition row is present with the same target
// state, so a bare UPDATE can never slip through.
func (l *Lifecycle) commitTransition(ctx context.Context, tx pgx.Tx, p security.Principal, next Agent,
	t Transition, action, reason, approvalID, correlationID string, now time.Time,
) error {
	// What the transition GRANTS is filled in from the agent it produces, in one
	// place, because every caller already computed `next` correctly and none of
	// them should have to remember a second copy.
	//
	// This is not bookkeeping. Since 00750 the transition row is the only way
	// `mode` and `envelope_id` can change at all -- the application holds no
	// UPDATE on them -- so a transition that failed to carry them would be a
	// promotion that granted nothing, silently. That is why they are set here
	// rather than at each call site.
	t.ToMode = next.Mode
	t.ToEnvelopeID = next.EnvelopeID
	t.ToStrategyVersionID = next.StrategyVersionID
	t.ToRiskPolicyVersion = next.RiskPolicyVersion
	t.ToSupersededByAgentID = next.SupersededByAgentID
	t.ToFailureReason = next.FailureReason
	if err := insertTransition(ctx, tx, t); err != nil {
		return err
	}
	if err := updateAgentState(ctx, tx, next); err != nil {
		return err
	}
	return l.appendAudit(ctx, tx, p, next, action, reason, approvalID, correlationID, now, t)
}

func (l *Lifecycle) appendAudit(ctx context.Context, tx pgx.Tx, p security.Principal, a Agent,
	action, reason, approvalID, correlationID string, now time.Time, t Transition,
) error {
	payload, err := json.Marshal(struct {
		AgentID      string `json:"agent_id"`
		FromState    string `json:"from_state"`
		ToState      string `json:"to_state"`
		FromStage    string `json:"from_stage"`
		ToStage      string `json:"to_stage"`
		Mode         string `json:"mode,omitempty"`
		EnvelopeID   string `json:"envelope_id,omitempty"`
		TransitionID string `json:"transition_id"`
		ApprovalID   string `json:"approval_id,omitempty"`
		EvidenceHash string `json:"evidence_hash,omitempty"`
	}{
		a.ID.String(), string(t.FromState), string(t.ToState), string(t.FromStage), string(t.ToStage),
		string(a.Mode), a.EnvelopeID, t.ID.String(), approvalID, HashOf(t.EvidenceHash),
	})
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "agent: encode audit payload")
	}
	return l.audit.Append(ctx, tx, AuditEvent{
		Stream: AuditStream, ActorType: string(p.ActorType), ActorID: p.SubjectID,
		Action: action, ResourceType: "agent", ResourceID: a.ID.String(),
		Reason: reason, EvidenceRef: approvalID, PolicyVersion: a.RiskPolicyVersion,
		CorrelationID: correlationID, Payload: payload, OccurredAt: now,
	})
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
