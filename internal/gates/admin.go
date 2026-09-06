package gates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
)

// StepUpMaxAge is how recent a strong authentication must be for Approve,
// Activate and Resume.
const StepUpMaxAge = 15 * time.Minute

// AuditStream is the audit stream every gate transition is appended to.
const AuditStream = "admin"

// AuditEvent is the subset of internal/audit's Event that this package
// emits. The integrator adapts it to the real audit writer; the field names
// match so the adapter is a field copy.
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

// AuditAppender appends an audit event inside the caller's transaction. It
// must fail (and so roll the transition back) rather than drop the event.
type AuditAppender interface {
	Append(ctx context.Context, tx pgx.Tx, e AuditEvent) error
}

// Admin drives the gate state machine for one environment. Every method
// rejects AGENT and SERVICE principals before issuing any query, checks the
// permission named in POLICY_AUTHORITY §1, applies the explicit transition
// table, and records a capability_gate_transitions row plus an audit event
// in the caller's transaction.
type Admin struct {
	env   string
	clk   clock.Clock
	audit AuditAppender
}

// NewAdmin builds an Admin. All three dependencies are required.
func NewAdmin(env string, clk clock.Clock, audit AuditAppender) (*Admin, error) {
	if !validEnvironment(env) {
		return nil, errs.Newf(errs.CodeValidationFailed, "unknown environment %q", env)
	}
	if clk == nil {
		return nil, errs.New(errs.CodeValidationFailed, "clock is required")
	}
	if audit == nil {
		return nil, errs.New(errs.CodeValidationFailed, "audit appender is required")
	}
	return &Admin{env: env, clk: clk, audit: audit}, nil
}

// Environment returns the environment the admin operates on.
func (a *Admin) Environment() string { return a.env }

// actor extracts the acting principal and rejects anything that may not
// touch this subsystem. It runs before any query. Only USER, OPERATOR and
// SYSTEM actors are admitted (the transitions table CHECK mirrors this).
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
		return security.Principal{}, errs.New(errs.CodeForbidden, "agents cannot reach capability gates").
			WithField("actor_type", string(p.ActorType))
	case security.ActorUser, security.ActorOperator, security.ActorSystem:
		return p, nil
	default:
		return security.Principal{}, errs.Newf(errs.CodeForbidden, "actor type %s cannot change capability gates", p.ActorType).
			WithField("actor_type", string(p.ActorType))
	}
}

// authError maps the security package's sentinel errors onto stable codes.
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

func (a *Admin) require(ctx context.Context, perm security.Permission) error {
	if err := security.RequireAt(ctx, perm, a.clk.Now); err != nil {
		return authError(err)
	}
	return nil
}

func (a *Admin) requireStepUp(ctx context.Context) error {
	if err := security.RequireStepUp(ctx, StepUpMaxAge, a.clk.Now); err != nil {
		return authError(err)
	}
	return nil
}

func rolesOf(p security.Principal) string {
	roles := make([]string, 0, len(p.Roles))
	for _, r := range p.Roles {
		roles = append(roles, string(r))
	}
	sort.Strings(roles)
	return strings.Join(roles, ",")
}

func invalidTransition(g *Gate, to GateState) error {
	return errs.Newf(errs.CodeInvalidStateTransition, "gate %s cannot move from %s to %s", g.Capability, g.State, to).
		WithField("capability", string(g.Capability)).
		WithField("from", string(g.State)).
		WithField("to", string(to))
}

// load fetches and locks the gate row; a missing row is NOT_FOUND.
func (a *Admin) load(ctx context.Context, tx pgx.Tx, c Capability) (*Gate, error) {
	g, err := loadGate(ctx, tx, c, a.env, true)
	if err != nil {
		return nil, err
	}
	if g == nil {
		return nil, errs.New(errs.CodeNotFound, "gate not found; run Bootstrap").
			WithField("capability", string(c)).WithField("environment", a.env)
	}
	return g, nil
}

// commit persists the mutated gate, its transition and the audit event.
func (a *Admin) commit(ctx context.Context, tx pgx.Tx, p security.Principal, g *Gate, from GateState, verb, reason string, now time.Time) error {
	if err := saveGate(ctx, tx, g); err != nil {
		return err
	}
	digest := g.EvidenceDigest()
	t := Transition{
		ID: NewTransitionID(), GateID: g.ID, From: from, To: g.State,
		ActorType: p.ActorType, ActorID: p.SubjectID, Reason: reason,
		EvidenceHash: digest[:], OccurredAt: now,
	}
	if err := insertTransition(ctx, tx, t); err != nil {
		return err
	}
	payload, err := json.Marshal(struct {
		Capability      Capability `json:"capability"`
		Environment     string     `json:"environment"`
		From            GateState  `json:"from"`
		To              GateState  `json:"to"`
		ApprovalVersion int        `json:"approval_version"`
		TransitionID    string     `json:"transition_id"`
		Approvers       []string   `json:"approvers"`
	}{g.Capability, g.Environment, from, g.State, g.ApprovalVersion, t.ID.String(), g.DistinctApprovers()})
	if err != nil {
		return fmt.Errorf("gates: encode audit payload: %w", err)
	}
	if err := a.audit.Append(ctx, tx, AuditEvent{
		Stream: AuditStream, ActorType: string(p.ActorType), ActorID: p.SubjectID,
		Action: "capability_gate." + verb, ResourceType: "capability_gate", ResourceID: g.ID.String(),
		Reason: reason, EvidenceRef: g.EvidenceDigestHex(), PolicyVersion: fmt.Sprintf("approval_version=%d", g.ApprovalVersion),
		Payload: payload, OccurredAt: now,
	}); err != nil {
		return fmt.Errorf("gates: audit %s: %w", verb, err)
	}
	return nil
}

// Propose opens a new approval version: DISABLED|REVOKED|EXPIRED →
// PENDING_APPROVAL. Requires gate:propose. High-risk capabilities must carry
// all four evidence references. The proposer is recorded as the PROPOSE
// entry of the approval chain and may not approve or activate the version.
func (a *Admin) Propose(ctx context.Context, tx pgx.Tx, c Capability, req Proposal) (Gate, error) {
	p, err := actor(ctx)
	if err != nil {
		return Gate{}, err
	}
	if err := a.require(ctx, security.PermGatePropose); err != nil {
		return Gate{}, err
	}
	if !c.Valid() {
		return Gate{}, errs.Newf(errs.CodeValidationFailed, "unknown capability %q", c)
	}
	now := a.clk.Now()
	req, err = req.normalize(c, now)
	if err != nil {
		return Gate{}, err
	}
	g, err := loadGate(ctx, tx, c, a.env, true)
	if err != nil {
		return Gate{}, err
	}
	if g == nil {
		// An absent row is semantically DISABLED; persist it so the
		// transition has a from-row.
		if _, err := insertDisabled(ctx, tx, c, a.env); err != nil {
			return Gate{}, err
		}
		if g, err = loadGate(ctx, tx, c, a.env, true); err != nil {
			return Gate{}, err
		}
		if g == nil {
			return Gate{}, fmt.Errorf("gates: %s/%s vanished after insert", c, a.env)
		}
	}
	if !CanTransition(g.State, StatePendingApproval) {
		return Gate{}, invalidTransition(g, StatePendingApproval)
	}
	from := g.State
	g.State = StatePendingApproval
	g.ApprovalVersion++
	g.LegalReviewRef = req.LegalReviewRef
	g.ProviderContractRef = req.ProviderContractRef
	g.RiskApprovalRef = req.RiskApprovalRef
	g.SecurityApprovalRef = req.SecurityApprovalRef
	g.EvidenceHashes = req.EvidenceHashes
	g.EffectiveAt, g.ExpiresAt, g.RevokedAt, g.RevokeReason = nil, nil, nil, ""
	if !req.EffectiveAt.IsZero() {
		t := req.EffectiveAt
		g.EffectiveAt = &t
	}
	if !req.ExpiresAt.IsZero() {
		t := req.ExpiresAt
		g.ExpiresAt = &t
	}
	g.ProposedBy = p.SubjectID
	g.Approvers = []Approver{{
		UserID: p.SubjectID, Role: rolesOf(p), At: now, Step: StepPropose,
		EvidenceHash: g.EvidenceDigestHex(), Note: req.Reason,
	}}
	if err := a.commit(ctx, tx, p, g, from, "propose", req.Reason, now); err != nil {
		return Gate{}, err
	}
	return *g, nil
}

// approveRule is the dual-control rule for Approve and Resume: the subject
// must not be the proposer of the approval version.
func approveRule(g *Gate, subject string) error {
	if g.ProposedBy == "" {
		return errs.New(errs.CodeForbidden, "approval version has no proposer; dual control impossible").
			WithField("capability", string(g.Capability))
	}
	if subject == g.ProposedBy {
		return errs.New(errs.CodeForbidden, "proposer cannot approve their own proposal").
			WithField("capability", string(g.Capability))
	}
	return nil
}

// activateRule is the dual-control rule for Activate: the subject must be
// distinct from the proposer and from the principal whose approval (or
// resumption) put the gate into APPROVED. It applies to every capability:
// Checker condition 5 demands two distinct approvers for all of them, so a
// weaker rule here would only produce gates that are ACTIVE by state yet
// inactive by evaluation.
func activateRule(g *Gate, subject string) error {
	if err := approveRule(g, subject); err != nil {
		return err
	}
	last, ok := g.lastApprover()
	if !ok {
		return errs.New(errs.CodeForbidden, "approval version has no approver").
			WithField("capability", string(g.Capability))
	}
	if last.UserID == subject {
		return errs.New(errs.CodeForbidden, "the approving principal cannot also activate; a distinct principal is required").
			WithField("capability", string(g.Capability)).WithField("approver", last.UserID)
	}
	return nil
}

// Approve records the first approval: PENDING_APPROVAL → APPROVED. Requires
// gate:approve (a live BREAK_GLASS elevation), a step-up within
// StepUpMaxAge, and a principal other than the proposer.
func (a *Admin) Approve(ctx context.Context, tx pgx.Tx, c Capability, note string) (Gate, error) {
	return a.approveStep(ctx, tx, c, note, StateApproved, StepApprove, "approve")
}

// Resume returns a SUSPENDED gate to APPROVED. It never re-activates by
// itself: a principal distinct from the resumer must call Activate, so
// re-enabling after an emergency is again dual-authorized. Same
// requirements as Approve.
func (a *Admin) Resume(ctx context.Context, tx pgx.Tx, c Capability, note string) (Gate, error) {
	return a.approveStep(ctx, tx, c, note, StateApproved, StepResume, "resume")
}

func (a *Admin) approveStep(ctx context.Context, tx pgx.Tx, c Capability, note string, to GateState, step, verb string) (Gate, error) {
	p, err := actor(ctx)
	if err != nil {
		return Gate{}, err
	}
	if err := a.require(ctx, security.PermGateApprove); err != nil {
		return Gate{}, err
	}
	if err := a.requireStepUp(ctx); err != nil {
		return Gate{}, err
	}
	if !c.Valid() {
		return Gate{}, errs.Newf(errs.CodeValidationFailed, "unknown capability %q", c)
	}
	note = strings.TrimSpace(note)
	if note == "" {
		return Gate{}, errs.New(errs.CodeValidationFailed, "note is required").WithField("note", "required")
	}
	g, err := a.load(ctx, tx, c)
	if err != nil {
		return Gate{}, err
	}
	expectFrom := StatePendingApproval
	if step == StepResume {
		expectFrom = StateSuspended
	}
	if g.State != expectFrom || !CanTransition(g.State, to) {
		return Gate{}, invalidTransition(g, to)
	}
	if err := approveRule(g, p.SubjectID); err != nil {
		return Gate{}, err
	}
	now := a.clk.Now()
	from := g.State
	g.State = to
	g.Approvers = append(g.Approvers, Approver{
		UserID: p.SubjectID, Role: rolesOf(p), At: now, Step: step,
		EvidenceHash: g.EvidenceDigestHex(), Note: note,
	})
	if err := a.commit(ctx, tx, p, g, from, verb, note, now); err != nil {
		return Gate{}, err
	}
	return *g, nil
}

// Activate records the second approval: APPROVED → ACTIVE. Requires
// gate:approve, a step-up within StepUpMaxAge, and a principal distinct
// from both the proposer and the approving principal (activateRule). A
// high-risk capability must still carry every evidence reference and the
// window must not already be expired. effective_at is set to now if unset.
func (a *Admin) Activate(ctx context.Context, tx pgx.Tx, c Capability, note string) (Gate, error) {
	p, err := actor(ctx)
	if err != nil {
		return Gate{}, err
	}
	if err := a.require(ctx, security.PermGateApprove); err != nil {
		return Gate{}, err
	}
	if err := a.requireStepUp(ctx); err != nil {
		return Gate{}, err
	}
	if !c.Valid() {
		return Gate{}, errs.Newf(errs.CodeValidationFailed, "unknown capability %q", c)
	}
	note = strings.TrimSpace(note)
	if note == "" {
		return Gate{}, errs.New(errs.CodeValidationFailed, "note is required").WithField("note", "required")
	}
	g, err := a.load(ctx, tx, c)
	if err != nil {
		return Gate{}, err
	}
	if !CanTransition(g.State, StateActive) {
		return Gate{}, invalidTransition(g, StateActive)
	}
	if err := activateRule(g, p.SubjectID); err != nil {
		return Gate{}, err
	}
	now := a.clk.Now()
	if IsHighRisk(c) {
		if missing := g.MissingEvidence(); len(missing) > 0 {
			return Gate{}, errs.New(errs.CodeInvalidStateTransition, "high-risk gate is missing evidence references; re-propose").
				WithField("capability", string(c)).WithField("missing", missing)
		}
	}
	if g.ExpiresAt != nil && !now.Before(*g.ExpiresAt) {
		return Gate{}, errs.New(errs.CodeInvalidStateTransition, "approval version has expired; re-propose").
			WithField("capability", string(c))
	}
	from := g.State
	g.State = StateActive
	if g.EffectiveAt == nil {
		t := now
		g.EffectiveAt = &t
	}
	g.Approvers = append(g.Approvers, Approver{
		UserID: p.SubjectID, Role: rolesOf(p), At: now, Step: StepActivate,
		EvidenceHash: g.EvidenceDigestHex(), Note: note,
	})
	if err := a.commit(ctx, tx, p, g, from, "activate", note, now); err != nil {
		return Gate{}, err
	}
	return *g, nil
}

// Suspend is the fast path: ACTIVE → SUSPENDED by a single operator holding
// kill:activate, with no step-up and no approval. The approval chain is
// kept so Resume + Activate can restore the version under dual control.
func (a *Admin) Suspend(ctx context.Context, tx pgx.Tx, c Capability, reason string) (Gate, error) {
	p, err := actor(ctx)
	if err != nil {
		return Gate{}, err
	}
	if err := a.require(ctx, security.PermKillActivate); err != nil {
		return Gate{}, err
	}
	if !c.Valid() {
		return Gate{}, errs.Newf(errs.CodeValidationFailed, "unknown capability %q", c)
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Gate{}, errs.New(errs.CodeValidationFailed, "reason is required").WithField("reason", "required")
	}
	g, err := a.load(ctx, tx, c)
	if err != nil {
		return Gate{}, err
	}
	if !CanTransition(g.State, StateSuspended) {
		return Gate{}, invalidTransition(g, StateSuspended)
	}
	now := a.clk.Now()
	from := g.State
	g.State = StateSuspended
	if err := a.commit(ctx, tx, p, g, from, "suspend", reason, now); err != nil {
		return Gate{}, err
	}
	return *g, nil
}

// Revoke ends the current approval version: any state but REVOKED →
// REVOKED. Requires gate:approve. Revocation is risk-reducing, so like
// Suspend it needs no step-up. A new Propose starts the next version.
func (a *Admin) Revoke(ctx context.Context, tx pgx.Tx, c Capability, reason string) (Gate, error) {
	p, err := actor(ctx)
	if err != nil {
		return Gate{}, err
	}
	if err := a.require(ctx, security.PermGateApprove); err != nil {
		return Gate{}, err
	}
	if !c.Valid() {
		return Gate{}, errs.Newf(errs.CodeValidationFailed, "unknown capability %q", c)
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Gate{}, errs.New(errs.CodeValidationFailed, "reason is required").WithField("reason", "required")
	}
	g, err := a.load(ctx, tx, c)
	if err != nil {
		return Gate{}, err
	}
	if !CanTransition(g.State, StateRevoked) {
		return Gate{}, invalidTransition(g, StateRevoked)
	}
	now := a.clk.Now()
	from := g.State
	g.State = StateRevoked
	t := now
	g.RevokedAt = &t
	g.RevokeReason = reason
	if err := a.commit(ctx, tx, p, g, from, "revoke", reason, now); err != nil {
		return Gate{}, err
	}
	return *g, nil
}

// ExpireDue moves every APPROVED or ACTIVE gate of the environment whose
// expires_at is at or before now to EXPIRED, recording a SYSTEM transition
// and audit event for each. It is meant for a periodic worker; Checker
// already treats an expired window as inactive, so this only makes the
// persisted state catch up. An attached AGENT or SERVICE principal is
// rejected; otherwise no principal is needed.
func (a *Admin) ExpireDue(ctx context.Context, tx pgx.Tx, now time.Time) ([]Gate, error) {
	p := security.Principal{ActorType: security.ActorSystem, SubjectID: "gates.expire_due"}
	if _, ok := security.PrincipalFrom(ctx); ok {
		var err error
		if p, err = actor(ctx); err != nil {
			return nil, err
		}
	}
	now = now.UTC()
	rows, err := tx.Query(ctx, `SELECT `+gateColumns+` FROM capability_gates
		WHERE environment = $1 AND state IN ($2, $3) AND expires_at IS NOT NULL AND expires_at <= $4
		ORDER BY capability FOR UPDATE`, a.env, string(StateApproved), string(StateActive), now)
	if err != nil {
		return nil, fmt.Errorf("gates: select due: %w", err)
	}
	var due []Gate
	for rows.Next() {
		g, err := scanGate(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("gates: scan due: %w", err)
		}
		due = append(due, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gates: select due: %w", err)
	}
	out := make([]Gate, 0, len(due))
	for i := range due {
		g := &due[i]
		if !CanTransition(g.State, StateExpired) {
			return nil, invalidTransition(g, StateExpired)
		}
		from := g.State
		g.State = StateExpired
		reason := fmt.Sprintf("expires_at %s reached", g.ExpiresAt.UTC().Format(time.RFC3339Nano))
		if err := a.commit(ctx, tx, p, g, from, "expire", reason, now); err != nil {
			return nil, err
		}
		out = append(out, *g)
	}
	return out, nil
}
