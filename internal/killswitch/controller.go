package killswitch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/security"
)

// StepUpMaxAge is how recent a strong authentication must be for Release.
const StepUpMaxAge = 15 * time.Minute

// AuditStream is the audit stream every switch transition is appended to.
const AuditStream = "admin"

// ApprovalKindRelease is the admin_actions kind a SEVERE release must cite.
const ApprovalKindRelease = "KILL_SWITCH_RELEASE"

// ReleaseTargetID is the admin_actions target_id for releasing (kind, scope).
func ReleaseTargetID(kind Kind, scope string) string { return string(kind) + ":" + scope }

// ParseReleaseTargetID is the inverse of ReleaseTargetID: it recovers the
// (kind, scope) an approved KILL_SWITCH_RELEASE action names, so an executor
// can release exactly the switch the two approvers agreed on rather than one
// the request repeats. No kind contains a colon, so the first one separates
// them; the scope is normalized for the kind, which refuses a global scope
// where a specific id is required and vice versa.
func ParseReleaseTargetID(target string) (Kind, string, error) {
	name, scope, found := strings.Cut(target, ":")
	if !found {
		return "", "", errs.New(errs.CodeValidationFailed, `kill switch target must be "KIND:scope"`).
			WithField("target_id", target)
	}
	kind := Kind(name)
	if !kind.Valid() {
		return "", "", errs.Newf(errs.CodeValidationFailed, "unknown kill switch kind %q", name).
			WithField("target_id", target)
	}
	scope, err := kind.NormalizeScope(scope)
	if err != nil {
		return "", "", err
	}
	return kind, scope, nil
}

// Approval is what ApprovalVerifier returns for an approved admin action.
type Approval struct {
	ID         string
	Kind       string
	TargetID   string
	ProposedBy string
	ApprovedBy string
	ApprovedAt time.Time
	ExpiresAt  time.Time
}

// ApprovalVerifier checks that approvalID names an admin_actions row of the
// given kind and target that is APPROVED under dual control and not
// expired. internal/admin provides the real implementation.
type ApprovalVerifier interface {
	VerifyApproved(ctx context.Context, q db.Querier, approvalID, kind, targetID string) (Approval, error)
}

// AuditEvent is the subset of internal/audit's Event this package emits;
// field names match so the integrator's adapter is a field copy.
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
	Payload       json.RawMessage
	OccurredAt    time.Time
}

// AuditAppender appends an audit event inside the caller's transaction.
type AuditAppender interface {
	Append(ctx context.Context, tx pgx.Tx, e AuditEvent) error
}

// Controller activates and releases switches.
type Controller struct {
	clk       clock.Clock
	audit     AuditAppender
	approvals ApprovalVerifier
}

// NewController builds a Controller. Every dependency is required: a nil
// verifier would make SEVERE releases impossible silently instead of
// visibly at wiring time.
func NewController(clk clock.Clock, audit AuditAppender, approvals ApprovalVerifier) (*Controller, error) {
	if clk == nil {
		return nil, errs.New(errs.CodeValidationFailed, "clock is required")
	}
	if audit == nil {
		return nil, errs.New(errs.CodeValidationFailed, "audit appender is required")
	}
	if approvals == nil {
		return nil, errs.New(errs.CodeValidationFailed, "approval verifier is required")
	}
	return &Controller{clk: clk, audit: audit, approvals: approvals}, nil
}

// actor extracts the acting principal and rejects AGENT and SERVICE actors
// before any query (the transitions table admits USER, OPERATOR, SYSTEM).
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
		return security.Principal{}, errs.New(errs.CodeForbidden, "agents cannot reach kill switches").
			WithField("actor_type", string(p.ActorType))
	case security.ActorUser, security.ActorOperator, security.ActorSystem:
		return p, nil
	default:
		return security.Principal{}, errs.Newf(errs.CodeForbidden, "actor type %s cannot change kill switches", p.ActorType).
			WithField("actor_type", string(p.ActorType))
	}
}

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

func (c *Controller) record(ctx context.Context, tx pgx.Tx, p security.Principal, s *Switch, verb, reason, approvalID string, now time.Time, expect *int64) error {
	t := Transition{
		ID: NewTransitionID(), SwitchID: s.ID, Kind: s.Kind, ScopeID: s.ScopeID, ToActive: s.Active,
		ActorType: p.ActorType, ActorID: p.SubjectID, Reason: reason, ApprovalID: approvalID, OccurredAt: now,
		// The row carries what it makes true, and the version it expected --
		// the compare-and-swap 00753 moved out of saveSwitch. Filled here in one
		// place because every caller already computed the switch it wants.
		ToSeverity: s.Severity, ReleaseReason: s.ReleaseReason,
	}
	if expect != nil {
		t.FromVersion = expect
	}
	if err := insertTransition(ctx, tx, t); err != nil {
		return err
	}
	payload, err := json.Marshal(struct {
		Kind         Kind     `json:"kind"`
		Scope        string   `json:"scope"`
		Active       bool     `json:"active"`
		Severity     Severity `json:"severity"`
		TransitionID string   `json:"transition_id"`
		ApprovalID   string   `json:"approval_id,omitempty"`
	}{s.Kind, s.ScopeID, s.Active, s.Severity, t.ID.String(), approvalID})
	if err != nil {
		return fmt.Errorf("killswitch: encode audit payload: %w", err)
	}
	if err := c.audit.Append(ctx, tx, AuditEvent{
		Stream: AuditStream, ActorType: string(p.ActorType), ActorID: p.SubjectID,
		Action: "kill_switch." + verb, ResourceType: "kill_switch", ResourceID: s.ID.String(),
		Reason: reason, EvidenceRef: approvalID, PolicyVersion: string(s.Severity),
		Payload: payload, OccurredAt: now,
	}); err != nil {
		return fmt.Errorf("killswitch: audit %s: %w", verb, err)
	}
	return nil
}

// Activate is the fast path (PART 53, 93): one operator with kill:activate
// and a reason. No step-up, no approval, no delay. The row is created or
// re-activated, a transition is recorded and the audit event appended, all
// in tx; the switch is seen by every Check that starts after tx commits.
// Activating an already-active switch is a no-op that returns the current
// row (no transition), so a repeated press during an incident cannot fail.
func (c *Controller) Activate(ctx context.Context, tx pgx.Tx, kind Kind, scope, reason string) (Switch, error) {
	p, err := actor(ctx)
	if err != nil {
		return Switch{}, err
	}
	if err := security.RequireAt(ctx, security.PermKillActivate, c.clk.Now); err != nil {
		return Switch{}, authError(err)
	}
	scope, err = kind.NormalizeScope(scope)
	if err != nil {
		return Switch{}, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Switch{}, errs.New(errs.CodeValidationFailed, "reason is required").WithField("reason", "required")
	}
	now := c.clk.Now()
	s, err := loadSwitch(ctx, tx, kind, scope, true)
	if err != nil {
		return Switch{}, err
	}
	if s != nil && s.Active {
		return *s, nil
	}
	if s == nil {
		s = &Switch{ID: NewSwitchID(), Kind: kind, ScopeID: scope}
	}
	s.Active = true
	s.Severity = kind.Severity()
	s.Reason = reason
	s.ActivatedBy = p.SubjectID
	t := now
	s.ActivatedAt = &t
	s.ReleasedBy, s.ReleasedAt, s.ReleaseApprovalID, s.ReleaseReason = "", nil, "", ""
	// A switch that does not exist yet is INSERTed already active, and its birth
	// transition applies nothing: the row is written to record the arrival, and
	// 00753's trigger returns early on a row with no from_version.
	//
	// An existing switch is moved BY the transition. `expect` is the version the
	// caller last saw, and the trigger refuses the row if the switch has moved
	// underneath -- the compare-and-swap saveSwitch used to carry.
	var expect *int64
	if s.Version == 0 {
		if err = insertActive(ctx, tx, s); err != nil {
			return Switch{}, err
		}
	} else {
		v := s.Version
		expect = &v
	}
	if err := c.record(ctx, tx, p, s, "activate", reason, "", now, expect); err != nil {
		return Switch{}, err
	}
	if err := reloadSwitch(ctx, tx, s); err != nil {
		return Switch{}, err
	}
	return *s, nil
}

// Release deactivates a switch. It requires kill:release (a live BREAK_GLASS
// elevation) and a step-up within StepUpMaxAge. A SEVERE switch (see
// Kind.Severity) additionally requires approvalID to name an APPROVED,
// dual-controlled KILL_SWITCH_RELEASE admin action for this exact (kind,
// scope), verified through the ApprovalVerifier. An approvalID given for a
// STANDARD switch is verified too, never recorded unverified.
func (c *Controller) Release(ctx context.Context, tx pgx.Tx, kind Kind, scope, reason string, approvalID *string) (Switch, error) {
	p, err := actor(ctx)
	if err != nil {
		return Switch{}, err
	}
	if err := security.RequireAt(ctx, security.PermKillRelease, c.clk.Now); err != nil {
		return Switch{}, authError(err)
	}
	if err := security.RequireStepUp(ctx, StepUpMaxAge, c.clk.Now); err != nil {
		return Switch{}, authError(err)
	}
	scope, err = kind.NormalizeScope(scope)
	if err != nil {
		return Switch{}, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Switch{}, errs.New(errs.CodeValidationFailed, "reason is required").WithField("reason", "required")
	}
	now := c.clk.Now()
	severity := kind.Severity()
	approval := ""
	if approvalID != nil {
		approval = strings.TrimSpace(*approvalID)
	}
	if severity == SeveritySevere && approval == "" {
		return Switch{}, errs.Newf(errs.CodeForbidden, "releasing SEVERE switch %s requires an approved %s admin action", kind, ApprovalKindRelease).
			WithField("switch", string(kind)).WithField("scope", scope).WithField("severity", string(severity))
	}
	if approval != "" {
		if _, perr := id.ParseAny(approval); perr != nil {
			return Switch{}, errs.New(errs.CodeValidationFailed, "approval id must be a uuid").WithField("approval_id", "invalid")
		}
		if err := c.verifyApproval(ctx, tx, approval, kind, scope, now); err != nil {
			return Switch{}, err
		}
	}
	s, err := loadSwitch(ctx, tx, kind, scope, true)
	if err != nil {
		return Switch{}, err
	}
	if s == nil {
		return Switch{}, errs.New(errs.CodeNotFound, "kill switch not found").WithField("switch", string(kind)).WithField("scope", scope)
	}
	if !s.Active {
		return Switch{}, errs.Newf(errs.CodeInvalidStateTransition, "kill switch %s is not active", kind).
			WithField("switch", string(kind)).WithField("scope", scope)
	}
	s.Active = false
	s.ReleasedBy = p.SubjectID
	t := now
	s.ReleasedAt = &t
	s.ReleaseApprovalID = approval
	s.ReleaseReason = reason
	expect := s.Version
	if err := c.record(ctx, tx, p, s, "release", reason, approval, now, &expect); err != nil {
		return Switch{}, err
	}
	if err := reloadSwitch(ctx, tx, s); err != nil {
		return Switch{}, err
	}
	return *s, nil
}

// verifyApproval asks the verifier and re-checks the returned approval
// (defense in depth): right kind and target, approved by someone other than
// the proposer, not expired.
func (c *Controller) verifyApproval(ctx context.Context, q db.Querier, approvalID string, kind Kind, scope string, now time.Time) error {
	target := ReleaseTargetID(kind, scope)
	ap, err := c.approvals.VerifyApproved(ctx, q, approvalID, ApprovalKindRelease, target)
	if err != nil {
		if _, ok := errs.As(err); ok {
			return err
		}
		return errs.Wrap(err, errs.CodeForbidden, "release approval could not be verified").WithField("approval_id", approvalID)
	}
	switch {
	case ap.ID != approvalID:
		return errs.New(errs.CodeForbidden, "release approval id mismatch").WithField("approval_id", approvalID)
	case ap.Kind != ApprovalKindRelease:
		return errs.New(errs.CodeForbidden, "release approval has the wrong kind").WithField("approval_id", approvalID).WithField("kind", ap.Kind)
	case ap.TargetID != target:
		return errs.New(errs.CodeForbidden, "release approval targets a different switch").WithField("approval_id", approvalID).WithField("target_id", ap.TargetID)
	case ap.ApprovedBy == "" || ap.ProposedBy == "" || ap.ApprovedBy == ap.ProposedBy:
		return errs.New(errs.CodeForbidden, "release approval is not dual-controlled").WithField("approval_id", approvalID)
	case !ap.ExpiresAt.IsZero() && !now.Before(ap.ExpiresAt):
		return errs.New(errs.CodeForbidden, "release approval has expired").WithField("approval_id", approvalID)
	}
	return nil
}

// Active lists every active switch, sorted by (kind, scope).
func (c *Controller) Active(ctx context.Context, q db.Querier) ([]Switch, error) {
	return listActive(ctx, q)
}
