package admin

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/security"
)

// ExecFunc applies an approved action. It receives the canonical params
// whose hash was verified and runs inside a savepoint on the caller's
// transaction; its writes are rolled back when it returns an error. The
// returned JSON (nil allowed) is stored as execution_result.
//
// An executor registered by kind (rather than closed over one action) reads
// the action it is applying from the context with ExecutingAction.
type ExecFunc func(ctx context.Context, tx pgx.Tx, params json.RawMessage) (json.RawMessage, error)

// execCtxKey carries the action Execute is applying into its ExecFunc.
type execCtxKey struct{}

// ExecutingAction returns the action Execute is applying. It is set only for
// the duration of the ExecFunc call, and only by Execute, so an executor
// registered against a kind can name the record that authorized it — as an
// approval id, an evidence reference or an audit subject — without a second,
// unlocked read of a row Execute already holds FOR UPDATE.
//
// It is absent outside Execute, and an executor that needs it must fail
// rather than guess.
func ExecutingAction(ctx context.Context) (Action, bool) {
	a, ok := ctx.Value(execCtxKey{}).(Action)
	return a, ok
}

func withExecutingAction(ctx context.Context, a Action) context.Context {
	return context.WithValue(ctx, execCtxKey{}, a)
}

// Actions is the fixed contract (POLICY_AUTHORITY §7). Every method rejects
// AGENT principals before any query and requires the caller's transaction.
type Actions interface {
	Propose(ctx context.Context, tx pgx.Tx, p Proposal) (Action, error)
	Approve(ctx context.Context, tx pgx.Tx, id, note string) (Action, error)
	Reject(ctx context.Context, tx pgx.Tx, id, reason string) (Action, error)
	Execute(ctx context.Context, tx pgx.Tx, id string, exec ExecFunc) (Action, error)
}

// Audit action names emitted on the admin stream.
const (
	AuditProposed  = "admin_action.proposed"
	AuditApproved  = "admin_action.approved"
	AuditRejected  = "admin_action.rejected"
	AuditCancelled = "admin_action.cancelled"
	AuditExecuted  = "admin_action.executed"
	AuditFailed    = "admin_action.failed"
	AuditExpired   = "admin_action.expired"

	auditResourceType = "admin_action"
	maxStoredError    = 4000
)

// ErrParamsTampered is returned by Execute when the stored params no longer
// hash to the recorded params_hash.
var ErrParamsTampered = errs.New(errs.CodeConflict, "admin: stored params do not match params_hash")

// Service implements Actions against PostgreSQL. It holds no connection.
type Service struct {
	clk   clock.Clock
	audit audit.Writer
}

var _ Actions = (*Service)(nil)

// NewService wires the clock and the audit writer. A nil clock means the
// system clock; the writer is mandatory and every method fails closed
// without it.
func NewService(clk clock.Clock, w audit.Writer) *Service {
	if clk == nil {
		clk = clock.System()
	}
	return &Service{clk: clk, audit: w}
}

// now returns the injected time in UTC at the database's precision.
func (s *Service) now() time.Time { return s.clk.Now().UTC().Truncate(time.Microsecond) }

// caller extracts and validates the principal, refusing agents before any
// query.
func caller(ctx context.Context) (security.Principal, error) {
	p, ok := security.PrincipalFrom(ctx)
	if !ok {
		return security.Principal{}, errs.New(errs.CodeUnauthenticated, "admin: authentication required")
	}
	if p.ActorType == security.ActorAgent {
		return security.Principal{}, errs.New(errs.CodeForbidden, "admin: agents cannot reach administrative actions")
	}
	if err := p.Validate(); err != nil {
		return security.Principal{}, errs.Wrap(err, errs.CodeForbidden, "admin: invalid principal")
	}
	return p, nil
}

// refuseAgent is the check for entry points that do not need a principal.
func refuseAgent(ctx context.Context) error {
	if p, ok := security.PrincipalFrom(ctx); ok && p.ActorType == security.ActorAgent {
		return errs.New(errs.CodeForbidden, "admin: agents cannot reach administrative actions")
	}
	return nil
}

// subjectUserID returns the principal's subject as a canonical users.id.
func subjectUserID(p security.Principal) (string, error) {
	u, err := id.ParseAny(p.SubjectID)
	if err != nil || u.IsZero() {
		return "", errs.New(errs.CodeForbidden, "admin: principal subject is not a user id")
	}
	return u.String(), nil
}

func mapAuth(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, security.ErrUnauthenticated):
		return errs.Wrap(err, errs.CodeUnauthenticated, "admin: authentication required")
	case errors.Is(err, security.ErrStepUpRequired):
		return errs.Wrap(err, errs.CodeStepUpRequired, "admin: recent strong authentication required")
	default:
		return errs.Wrap(err, errs.CodeForbidden, "admin: forbidden")
	}
}

func (s *Service) require(ctx context.Context, perm security.Permission) error {
	return mapAuth(security.RequireAt(ctx, perm, s.clk.Now))
}

func (s *Service) requireAny(ctx context.Context, perms ...security.Permission) error {
	return mapAuth(security.RequireAnyAt(ctx, s.clk.Now, perms...))
}

func (s *Service) requireStepUp(ctx context.Context, maxAge time.Duration) error {
	return mapAuth(security.RequireStepUp(ctx, maxAge, s.clk.Now))
}

func (s *Service) ready(tx pgx.Tx) error {
	if s.audit == nil {
		return errs.New(errs.CodeInternal, "admin: no audit writer configured")
	}
	if tx == nil {
		return errs.New(errs.CodeInternal, "admin: a transaction is required")
	}
	return nil
}

// actorPermissions returns the permissions that may act on an action of
// spec: the propose permission and, for dual-control kinds, the approve one.
func actorPermissions(spec KindSpec) []security.Permission {
	perms := []security.Permission{spec.ProposePermission}
	if spec.ApprovePermission != "" && spec.ApprovePermission != spec.ProposePermission {
		perms = append(perms, spec.ApprovePermission)
	}
	return perms
}

func specOf(k Kind) (KindSpec, error) {
	spec, ok := Spec(k)
	if !ok {
		return KindSpec{}, errs.Newf(errs.CodeInternal, "admin: stored action has undeclared kind %q", k)
	}
	return spec, nil
}

// Propose records a new action in PROPOSED state.
func (s *Service) Propose(ctx context.Context, tx pgx.Tx, p Proposal) (Action, error) {
	pr, err := caller(ctx)
	if err != nil {
		return Action{}, err
	}
	if err := s.ready(tx); err != nil {
		return Action{}, err
	}
	spec, ok := Spec(p.Kind)
	if !ok {
		return Action{}, errs.Newf(errs.CodeValidationFailed, "admin: unknown action kind %q", p.Kind).WithField("field", "kind")
	}
	if err := s.require(ctx, spec.ProposePermission); err != nil {
		return Action{}, err
	}
	if err := s.requireStepUp(ctx, spec.StepUpMaxAge); err != nil {
		return Action{}, err
	}
	uid, err := subjectUserID(pr)
	if err != nil {
		return Action{}, err
	}
	if err := validateProposal(p); err != nil {
		return Action{}, err
	}
	params, err := canonicalParams(p.Params)
	if err != nil {
		return Action{}, err
	}
	now := s.now()
	correlation := p.CorrelationID
	if correlation == "" {
		correlation = observability.CorrelationID(ctx)
	}
	a := Action{
		ID:               NewActionID(),
		Kind:             p.Kind,
		TargetType:       p.TargetType,
		TargetID:         p.TargetID,
		Params:           params,
		ParamsHash:       hashBytes(params),
		Reason:           p.Reason,
		RequiresDual:     spec.RequiresDual,
		Status:           StatusProposed,
		ProposedBy:       uid,
		ProposedAt:       now,
		ProposerStepUpAt: pr.AuthTime.UTC().Truncate(time.Microsecond),
		ExpiresAt:        now.Add(spec.Expiry),
		CorrelationID:    optional(correlation),
		UpdatedAt:        now,
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO admin_actions (
		    id, kind, target_type, target_id, params, params_hash, reason, requires_dual, status,
		    proposed_by_user_id, proposed_at, proposer_step_up_at, expires_at, correlation_id, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		a.ID, string(a.Kind), a.TargetType, a.TargetID, []byte(a.Params), a.ParamsHash, a.Reason, a.RequiresDual, string(a.Status),
		a.ProposedBy, a.ProposedAt, a.ProposerStepUpAt, a.ExpiresAt, a.CorrelationID, a.UpdatedAt)
	if err != nil {
		return Action{}, errs.Wrap(err, errs.CodeInternal, "admin: insert action")
	}
	if err := s.recordTransition(ctx, tx, a, StatusNone, uid, p.Reason, now); err != nil {
		return Action{}, err
	}
	if err := s.auditEvent(ctx, tx, pr.ActorType, uid, AuditProposed, nil, a, p.Reason, now); err != nil {
		return Action{}, err
	}
	return a, nil
}

func validateProposal(p Proposal) error {
	invalid := func(field, detail string) error {
		return errs.Newf(errs.CodeValidationFailed, "admin: %s %s", field, detail).WithField("field", field)
	}
	if p.TargetType == "" || !validText(p.TargetType) {
		return invalid("target_type", "is required")
	}
	if p.TargetID == "" || !validText(p.TargetID) {
		return invalid("target_id", "is required")
	}
	if !validText(p.Reason) {
		return invalid("reason", "must be valid UTF-8 without NUL")
	}
	if utf8.RuneCountInString(p.Reason) < MinReasonLength {
		return invalid("reason", "must be at least 8 characters")
	}
	if !validText(p.CorrelationID) {
		return invalid("correlation_id", "must be valid UTF-8 without NUL")
	}
	return nil
}

// Approve moves a PROPOSED dual-control action to APPROVED. The approver
// must differ from the proposer.
func (s *Service) Approve(ctx context.Context, tx pgx.Tx, actionID, note string) (Action, error) {
	pr, err := caller(ctx)
	if err != nil {
		return Action{}, err
	}
	if err := s.ready(tx); err != nil {
		return Action{}, err
	}
	if !validText(note) {
		return Action{}, errs.New(errs.CodeValidationFailed, "admin: note must be valid UTF-8 without NUL").WithField("field", "note")
	}
	a, err := lockAction(ctx, tx, actionID)
	if err != nil {
		return Action{}, err
	}
	spec, err := specOf(a.Kind)
	if err != nil {
		return Action{}, err
	}
	if !spec.RequiresDual || spec.ApprovePermission == "" {
		return Action{}, errs.Newf(errs.CodeInvalidStateTransition, "admin: kind %s does not take approval", a.Kind)
	}
	if err := s.require(ctx, spec.ApprovePermission); err != nil {
		return Action{}, err
	}
	if err := s.requireStepUp(ctx, spec.StepUpMaxAge); err != nil {
		return Action{}, err
	}
	uid, err := subjectUserID(pr)
	if err != nil {
		return Action{}, err
	}
	if uid == a.ProposedBy {
		return Action{}, errs.Wrap(security.ErrSelfApproval, errs.CodeForbidden, "admin: the approver must differ from the proposer")
	}
	if spec.ApproverIsNotTarget && sameUser(uid, a.TargetID) {
		return Action{}, errs.Wrap(security.ErrSelfApproval, errs.CodeForbidden,
			"admin: the approver must not be the person this action elevates")
	}
	now := s.now()
	if err := checkPending(a, StatusApproved, now); err != nil {
		return Action{}, err
	}
	before := a
	stepUp := pr.AuthTime.UTC().Truncate(time.Microsecond)
	a.Status = StatusApproved
	a.ApprovedBy = &uid
	a.ApprovedAt = &now
	a.ApproverStepUpAt = &stepUp
	a.ApprovalNote = optional(note)
	if err := s.updateStatus(ctx, tx, &a, before.Status,
		`approved_by_user_id = $3, approved_at = $4, approver_step_up_at = $5, approval_note = $6`,
		a.ApprovedBy, a.ApprovedAt, a.ApproverStepUpAt, a.ApprovalNote); err != nil {
		return Action{}, err
	}
	if err := s.recordTransition(ctx, tx, a, before.Status, uid, note, now); err != nil {
		return Action{}, err
	}
	if err := s.auditEvent(ctx, tx, pr.ActorType, uid, AuditApproved, &before, a, note, now); err != nil {
		return Action{}, err
	}
	return a, nil
}

// Reject moves a PROPOSED action to REJECTED. Holders of the kind's propose
// or approve permission may reject; no step-up is needed to refuse.
func (s *Service) Reject(ctx context.Context, tx pgx.Tx, actionID, reason string) (Action, error) {
	pr, err := caller(ctx)
	if err != nil {
		return Action{}, err
	}
	if err := s.ready(tx); err != nil {
		return Action{}, err
	}
	if !validText(reason) {
		return Action{}, errs.New(errs.CodeValidationFailed, "admin: reason must be valid UTF-8 without NUL").WithField("field", "reason")
	}
	a, err := lockAction(ctx, tx, actionID)
	if err != nil {
		return Action{}, err
	}
	spec, err := specOf(a.Kind)
	if err != nil {
		return Action{}, err
	}
	if err := s.requireAny(ctx, actorPermissions(spec)...); err != nil {
		return Action{}, err
	}
	uid, err := subjectUserID(pr)
	if err != nil {
		return Action{}, err
	}
	now := s.now()
	if a.Status != StatusProposed {
		return Action{}, errs.Newf(errs.CodeInvalidStateTransition, "admin: cannot reject an action in status %s", a.Status)
	}
	before := a
	a.Status = StatusRejected
	a.RejectedBy = &uid
	a.RejectedAt = &now
	a.RejectedReason = optional(reason)
	a.UpdatedAt = now
	if err := s.updateStatus(ctx, tx, &a, before.Status, `rejected_by_user_id = $3, rejected_at = $4, rejected_reason = $5`, a.RejectedBy, a.RejectedAt, a.RejectedReason); err != nil {
		return Action{}, err
	}
	if err := s.recordTransition(ctx, tx, a, before.Status, uid, reason, now); err != nil {
		return Action{}, err
	}
	if err := s.auditEvent(ctx, tx, pr.ActorType, uid, AuditRejected, &before, a, reason, now); err != nil {
		return Action{}, err
	}
	return a, nil
}

// Cancel lets the proposer withdraw their own PROPOSED action.
func (s *Service) Cancel(ctx context.Context, tx pgx.Tx, actionID, note string) (Action, error) {
	pr, err := caller(ctx)
	if err != nil {
		return Action{}, err
	}
	if err := s.ready(tx); err != nil {
		return Action{}, err
	}
	if !validText(note) {
		return Action{}, errs.New(errs.CodeValidationFailed, "admin: note must be valid UTF-8 without NUL").WithField("field", "note")
	}
	a, err := lockAction(ctx, tx, actionID)
	if err != nil {
		return Action{}, err
	}
	uid, err := subjectUserID(pr)
	if err != nil {
		return Action{}, err
	}
	if uid != a.ProposedBy {
		return Action{}, errs.New(errs.CodeForbidden, "admin: only the proposer can cancel an action")
	}
	now := s.now()
	if a.Status != StatusProposed {
		return Action{}, errs.Newf(errs.CodeInvalidStateTransition, "admin: cannot cancel an action in status %s", a.Status)
	}
	before := a
	a.Status = StatusCancelled
	a.UpdatedAt = now
	if err := s.updateStatus(ctx, tx, &a, before.Status, ""); err != nil {
		return Action{}, err
	}
	if err := s.recordTransition(ctx, tx, a, before.Status, uid, note, now); err != nil {
		return Action{}, err
	}
	if err := s.auditEvent(ctx, tx, pr.ActorType, uid, AuditCancelled, &before, a, note, now); err != nil {
		return Action{}, err
	}
	return a, nil
}

// Execute applies the action through exec. See ExecFunc for the savepoint
// semantics. On success the action is EXECUTED with the callback's result;
// on failure it is FAILED with the error text and the error is returned
// (wrapped, keeping its errs code) so the caller's transaction decides
// whether the FAILED record commits.
func (s *Service) Execute(ctx context.Context, tx pgx.Tx, actionID string, exec ExecFunc) (Action, error) {
	pr, err := caller(ctx)
	if err != nil {
		return Action{}, err
	}
	if err := s.ready(tx); err != nil {
		return Action{}, err
	}
	if exec == nil {
		return Action{}, errs.New(errs.CodeInternal, "admin: Execute requires a callback")
	}
	a, err := lockAction(ctx, tx, actionID)
	if err != nil {
		return Action{}, err
	}
	spec, err := specOf(a.Kind)
	if err != nil {
		return Action{}, err
	}
	if err := s.requireAny(ctx, actorPermissions(spec)...); err != nil {
		return Action{}, err
	}
	if err := s.requireStepUp(ctx, spec.StepUpMaxAge); err != nil {
		return Action{}, err
	}
	uid, err := subjectUserID(pr)
	if err != nil {
		return Action{}, err
	}
	now := s.now()
	if err := executable(a, spec, now); err != nil {
		return Action{}, err
	}
	params, err := canonicalParams(a.Params)
	if err != nil {
		return Action{}, errs.Wrap(err, errs.CodeConflict, "admin: stored params are not canonical JSON")
	}
	if !ParamsMatch(params, a.ParamsHash) {
		return Action{}, ErrParamsTampered.WithField("action_id", a.ID.String())
	}

	result, execErr := runExec(withExecutingAction(ctx, a), tx, exec, params)

	before := a
	a.ExecutedAt = &now
	a.UpdatedAt = now
	if execErr != nil {
		a.Status = StatusFailed
		a.ExecutionError = optional(truncate(execErr.Error(), maxStoredError))
		if err := s.updateStatus(ctx, tx, &a, before.Status, `executed_at = $3, execution_error = $4`, a.ExecutedAt, a.ExecutionError); err != nil {
			return Action{}, err
		}
		if err := s.recordTransition(ctx, tx, a, before.Status, uid, deref(a.ExecutionError), now); err != nil {
			return Action{}, err
		}
		if err := s.auditEvent(ctx, tx, pr.ActorType, uid, AuditFailed, &before, a, deref(a.ExecutionError), now); err != nil {
			return Action{}, err
		}
		return a, errs.Wrap(execErr, errs.CodeOf(execErr), "admin: action execution failed").WithField("action_id", a.ID.String())
	}
	a.Status = StatusExecuted
	a.ExecutionResult = result
	var stored []byte
	if result != nil {
		stored = []byte(result)
	}
	if err := s.updateStatus(ctx, tx, &a, before.Status, `executed_at = $3, execution_result = $4`, a.ExecutedAt, stored); err != nil {
		return Action{}, err
	}
	if err := s.recordTransition(ctx, tx, a, before.Status, uid, "", now); err != nil {
		return Action{}, err
	}
	if err := s.auditEvent(ctx, tx, pr.ActorType, uid, AuditExecuted, &before, a, a.Reason, now); err != nil {
		return Action{}, err
	}
	return a, nil
}

// runExec runs exec inside a savepoint and canonicalizes its result. Any
// error (from exec, from an invalid result, or from releasing the
// savepoint) rolls the savepoint back.
func runExec(ctx context.Context, tx pgx.Tx, exec ExecFunc, params json.RawMessage) (json.RawMessage, error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "admin: open savepoint")
	}
	result, execErr := exec(ctx, sp, params)
	var canonical json.RawMessage
	if execErr == nil && len(result) > 0 {
		canonical, err = audit.CanonicalJSON(result)
		if err != nil {
			execErr = errs.Wrap(err, errs.CodeInternal, "admin: execution result is not canonical JSON")
		}
	}
	if execErr != nil {
		_ = sp.Rollback(ctx) // the outer transaction stays usable; the caller may still commit the FAILED record
		return nil, execErr
	}
	if err := sp.Commit(ctx); err != nil {
		_ = sp.Rollback(ctx)
		return nil, errs.Wrap(err, errs.CodeInternal, "admin: release savepoint")
	}
	return canonical, nil
}

// executable checks status and expiry for Execute.
func executable(a Action, spec KindSpec, now time.Time) error {
	if a.Expired(now) {
		return errs.Newf(errs.CodeInvalidStateTransition, "admin: action expired at %s", a.ExpiresAt.Format(time.RFC3339))
	}
	switch a.Status {
	case StatusApproved:
		if spec.RequiresDual && (a.ApprovedBy == nil || *a.ApprovedBy == a.ProposedBy) {
			return errs.New(errs.CodeForbidden, "admin: dual-control action lacks a distinct approver")
		}
		return nil
	case StatusProposed:
		if spec.RequiresDual {
			return errs.New(errs.CodeInvalidStateTransition, "admin: dual-control action is not approved")
		}
		return nil
	default:
		return errs.Newf(errs.CodeInvalidStateTransition, "admin: cannot execute an action in status %s", a.Status)
	}
}

// checkPending verifies that a can move to next at now.
func checkPending(a Action, next Status, now time.Time) error {
	if a.Expired(now) {
		return errs.Newf(errs.CodeInvalidStateTransition, "admin: action expired at %s", a.ExpiresAt.Format(time.RFC3339))
	}
	if !CanTransition(a.Status, next) {
		return errs.Newf(errs.CodeInvalidStateTransition, "admin: cannot move from %s to %s", a.Status, next)
	}
	return nil
}

// ExpireDue marks every PROPOSED or APPROVED action whose expires_at is at
// or before now as EXPIRED, recording a transition and an audit event for
// each. Rows locked by a concurrent transition are skipped and picked up on
// the next run. It needs no principal (workers call it) but refuses agents.
func (s *Service) ExpireDue(ctx context.Context, tx pgx.Tx, now time.Time) (int, error) {
	if err := refuseAgent(ctx); err != nil {
		return 0, err
	}
	if err := s.ready(tx); err != nil {
		return 0, err
	}
	actorType, actorID := security.ActorSystem, "system"
	if p, ok := security.PrincipalFrom(ctx); ok && p.Validate() == nil {
		actorType, actorID = p.ActorType, p.SubjectID
	}
	now = now.UTC().Truncate(time.Microsecond)
	rows, err := tx.Query(ctx, `SELECT `+actionColumns+` FROM admin_actions
		WHERE status IN ('PROPOSED', 'APPROVED') AND expires_at <= $1
		ORDER BY expires_at, id FOR UPDATE SKIP LOCKED`, now)
	if err != nil {
		return 0, errs.Wrap(err, errs.CodeInternal, "admin: select due actions")
	}
	due, err := collectActions(rows)
	if err != nil {
		return 0, err
	}
	for _, a := range due {
		before := a
		a.Status = StatusExpired
		a.UpdatedAt = now
		if err := s.updateStatus(ctx, tx, &a, before.Status, ""); err != nil {
			return 0, err
		}
		if err := s.recordTransition(ctx, tx, a, before.Status, actorID, "expired", now); err != nil {
			return 0, err
		}
		if err := s.auditEvent(ctx, tx, actorType, actorID, AuditExpired, &before, a, "expired", now); err != nil {
			return 0, err
		}
	}
	return len(due), nil
}

// Get returns one action. Any holder of ReadPermissions may read.
func (s *Service) Get(ctx context.Context, q db.Querier, actionID string) (Action, error) {
	if _, err := caller(ctx); err != nil {
		return Action{}, err
	}
	if err := s.requireAny(ctx, readPermissions...); err != nil {
		return Action{}, err
	}
	aid, err := ParseActionID(actionID)
	if err != nil {
		return Action{}, errs.New(errs.CodeValidationFailed, "admin: malformed action id").WithField("field", "id")
	}
	return getAction(ctx, q, aid)
}

// ListPending returns unexpired PROPOSED and APPROVED actions, oldest first,
// at most limit (100 when non-positive).
func (s *Service) ListPending(ctx context.Context, q db.Querier, limit int) ([]Action, error) {
	if _, err := caller(ctx); err != nil {
		return nil, err
	}
	if err := s.requireAny(ctx, readPermissions...); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 100
	}
	rows, err := q.Query(ctx, `SELECT `+actionColumns+` FROM admin_actions
		WHERE status IN ('PROPOSED', 'APPROVED') AND expires_at > $1
		ORDER BY proposed_at, id LIMIT $2`, s.now(), limit)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "admin: list pending actions")
	}
	return collectActions(rows)
}

// updateStatus writes a.Status plus optional extra column assignments ($3..
// bound to args) guarded by the expected previous status, and refreshes
// a.UpdatedAt from the row (the set_updated_at trigger owns that column) so
// the returned Action equals what a later read sees.
func (s *Service) updateStatus(ctx context.Context, tx pgx.Tx, a *Action, from Status, extra string, args ...any) error {
	sql := `UPDATE admin_actions SET status = $2`
	if extra != "" {
		sql += ", " + extra
	}
	sql += ` WHERE id = $1 AND status = $` + strconv.Itoa(3+len(args)) + ` RETURNING updated_at`
	all := append([]any{a.ID, string(a.Status)}, args...)
	all = append(all, string(from))
	var updated time.Time
	err := tx.QueryRow(ctx, sql, all...).Scan(&updated)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return errs.New(errs.CodeConflict, "admin: action changed concurrently")
	case db.IsCheckViolation(err):
		return errs.Wrap(err, errs.CodeForbidden, "admin: transition violates a table constraint").WithField("constraint", db.ConstraintName(err))
	case err != nil:
		return errs.Wrap(err, errs.CodeInternal, "admin: update action")
	}
	a.UpdatedAt = updated.UTC()
	return nil
}

func (s *Service) recordTransition(ctx context.Context, tx pgx.Tx, a Action, from Status, actorID, note string, now time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO admin_action_transitions (id, action_id, from_status, to_status, actor_id, note, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		id.New[transitionKind](), a.ID, string(from), string(a.Status), actorID, optional(truncate(note, maxStoredError)), now)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "admin: insert transition")
	}
	return nil
}

// auditPayload is the typed payload of every admin-stream event.
type auditPayload struct {
	ActionID     string    `json:"action_id"`
	Kind         Kind      `json:"kind"`
	TargetType   string    `json:"target_type"`
	TargetID     string    `json:"target_id"`
	FromStatus   Status    `json:"from_status"`
	ToStatus     Status    `json:"to_status"`
	ParamsHash   string    `json:"params_hash"`
	RequiresDual bool      `json:"requires_dual"`
	ProposedBy   string    `json:"proposed_by"`
	ApprovedBy   *string   `json:"approved_by"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func (s *Service) auditEvent(ctx context.Context, tx pgx.Tx, actorType security.ActorType, actorID, action string, before *Action, after Action, reason string, now time.Time) error {
	from := StatusNone
	var beforeHash []byte
	if before != nil {
		from = before.Status
		h, err := snapshotHash(*before)
		if err != nil {
			return errs.Wrap(err, errs.CodeInternal, "admin: audit before hash")
		}
		beforeHash = h
	}
	afterHash, err := snapshotHash(after)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "admin: audit after hash")
	}
	payload, err := json.Marshal(auditPayload{
		ActionID: after.ID.String(), Kind: after.Kind, TargetType: after.TargetType, TargetID: after.TargetID,
		FromStatus: from, ToStatus: after.Status, ParamsHash: hex.EncodeToString(after.ParamsHash),
		RequiresDual: after.RequiresDual, ProposedBy: after.ProposedBy, ApprovedBy: after.ApprovedBy, ExpiresAt: after.ExpiresAt,
	})
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "admin: audit payload")
	}
	correlation := deref(after.CorrelationID)
	if correlation == "" {
		correlation = observability.CorrelationID(ctx)
	}
	_, err = s.audit.Append(ctx, tx, audit.Event{
		Stream:        audit.AdminStream,
		ActorType:     string(actorType),
		ActorID:       actorID,
		Action:        action,
		ResourceType:  auditResourceType,
		ResourceID:    after.ID.String(),
		BeforeHash:    beforeHash,
		AfterHash:     afterHash,
		RequestID:     observability.RequestID(ctx),
		CorrelationID: correlation,
		Reason:        truncate(reason, maxStoredError),
		Payload:       payload,
		OccurredAt:    now,
	})
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "admin: audit event")
	}
	return nil
}

const actionColumns = `id::text, kind, target_type, target_id, params, params_hash, reason, requires_dual, status,
	proposed_by_user_id::text, proposed_at, proposer_step_up_at,
	approved_by_user_id::text, approved_at, approver_step_up_at, approval_note,
	rejected_by_user_id::text, rejected_at, rejected_reason,
	executed_at, execution_result, execution_error, expires_at, correlation_id, updated_at`

type scanner interface {
	Scan(dest ...any) error
}

func scanAction(row scanner) (Action, error) {
	var (
		a              Action
		idText, kind   string
		status         string
		params, result []byte
		approvedAt     *time.Time
		approverStepUp *time.Time
		rejectedAt     *time.Time
		executedAt     *time.Time
		proposedAt     time.Time
		proposerStepUp time.Time
		expiresAt      time.Time
		updatedAt      time.Time
		approvedBy     *string
		rejectedBy     *string
		approvalNote   *string
		rejectedReason *string
		executionError *string
		correlationID  *string
		proposedByText string
		requiresDual   bool
		targetType     string
		targetID       string
		reason         string
		paramsHash     []byte
		pgxErr         error
	)
	pgxErr = row.Scan(&idText, &kind, &targetType, &targetID, &params, &paramsHash, &reason, &requiresDual, &status,
		&proposedByText, &proposedAt, &proposerStepUp,
		&approvedBy, &approvedAt, &approverStepUp, &approvalNote,
		&rejectedBy, &rejectedAt, &rejectedReason,
		&executedAt, &result, &executionError, &expiresAt, &correlationID, &updatedAt)
	if pgxErr != nil {
		return Action{}, pgxErr
	}
	aid, err := ParseActionID(idText)
	if err != nil {
		return Action{}, errs.Wrap(err, errs.CodeInternal, "admin: stored action id is malformed")
	}
	a = Action{
		ID: aid, Kind: Kind(kind), TargetType: targetType, TargetID: targetID, Params: params, ParamsHash: paramsHash,
		Reason: reason, RequiresDual: requiresDual, Status: Status(status), ProposedBy: proposedByText,
		ProposedAt: proposedAt.UTC(), ProposerStepUpAt: proposerStepUp.UTC(),
		ApprovedBy: approvedBy, ApprovedAt: utcPtr(approvedAt), ApproverStepUpAt: utcPtr(approverStepUp), ApprovalNote: approvalNote,
		RejectedBy: rejectedBy, RejectedAt: utcPtr(rejectedAt), RejectedReason: rejectedReason,
		ExecutedAt: utcPtr(executedAt), ExecutionError: executionError,
		ExpiresAt: expiresAt.UTC(), CorrelationID: correlationID, UpdatedAt: updatedAt.UTC(),
	}
	if result != nil {
		a.ExecutionResult = json.RawMessage(result)
	}
	return a, nil
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func getAction(ctx context.Context, q db.Querier, aid ActionID) (Action, error) {
	a, err := scanAction(q.QueryRow(ctx, `SELECT `+actionColumns+` FROM admin_actions WHERE id = $1`, aid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Action{}, errs.New(errs.CodeNotFound, "admin: action not found")
	}
	if err != nil {
		return Action{}, errs.Wrap(err, errs.CodeInternal, "admin: read action")
	}
	return a, nil
}

// lockAction parses actionID and reads the row FOR UPDATE.
func lockAction(ctx context.Context, tx pgx.Tx, actionID string) (Action, error) {
	aid, err := ParseActionID(actionID)
	if err != nil {
		return Action{}, errs.New(errs.CodeValidationFailed, "admin: malformed action id").WithField("field", "id")
	}
	a, err := scanAction(tx.QueryRow(ctx, `SELECT `+actionColumns+` FROM admin_actions WHERE id = $1 FOR UPDATE`, aid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Action{}, errs.New(errs.CodeNotFound, "admin: action not found")
	}
	if err != nil {
		return Action{}, errs.Wrap(err, errs.CodeInternal, "admin: lock action")
	}
	return a, nil
}

func collectActions(rows pgx.Rows) ([]Action, error) {
	defer rows.Close()
	var out []Action
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "admin: scan action")
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "admin: iterate actions")
	}
	return out, nil
}
