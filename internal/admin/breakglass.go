package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/security"
)

// MaxBreakGlassDuration bounds a single elevation (PART 93: narrow scope,
// expiry).
const MaxBreakGlassDuration = 4 * time.Hour

// AuditBreakGlassGranted is the admin-stream event recorded when a grant is
// executed; consumers audit every use of the elevation separately.
const AuditBreakGlassGranted = "break_glass.granted"

// BreakGlassParams are the typed params of a BREAK_GLASS_GRANT action.
type BreakGlassParams struct {
	// UserID is the users.id of the operator to elevate.
	UserID string `json:"user_id"`
	// Scope names what the elevation is for (an incident id, a kill switch,
	// a correction); it is recorded, not interpreted.
	Scope string `json:"scope"`
	// DurationSeconds is the lifetime of the elevation, at most
	// MaxBreakGlassDuration.
	DurationSeconds int64 `json:"duration_seconds"`
}

// Grant is the result of executing a BREAK_GLASS_GRANT action.
type Grant struct {
	ActionID  string    `json:"action_id"`
	UserID    string    `json:"user_id"`
	Scope     string    `json:"scope"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Active reports whether the grant is live at now.
func (g Grant) Active(now time.Time) bool { return !g.ExpiresAt.IsZero() && now.Before(g.ExpiresAt) }

// BreakGlass executes BREAK_GLASS_GRANT actions.
type BreakGlass struct {
	svc *Service
}

// NewBreakGlass wraps the action service.
func NewBreakGlass(svc *Service) *BreakGlass { return &BreakGlass{svc: svc} }

// Grant executes the approved BREAK_GLASS_GRANT action and returns the
// resulting time-boxed grant. The session layer then rotates the grantee's
// session with PrincipalWithBreakGlass.
func (b *BreakGlass) Grant(ctx context.Context, tx pgx.Tx, actionID string) (Grant, error) {
	current, err := b.svc.Get(ctx, tx, actionID)
	if err != nil {
		return Grant{}, err
	}
	if current.Kind != KindBreakGlassGrant {
		return Grant{}, errs.Newf(errs.CodeInvalidStateTransition, "admin: action %s is not a break-glass grant", current.Kind)
	}
	var grant Grant
	executed, err := b.svc.Execute(ctx, tx, actionID, func(ctx context.Context, tx pgx.Tx, params json.RawMessage) (json.RawMessage, error) {
		p, err := ParseBreakGlassParams(params)
		if err != nil {
			return nil, err
		}
		now := b.svc.now()
		grant = Grant{ActionID: current.ID.String(), UserID: p.UserID, Scope: p.Scope, ExpiresAt: now.Add(time.Duration(p.DurationSeconds) * time.Second)}
		out, err := json.Marshal(grant)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "admin: encode grant")
		}
		actor, _ := security.PrincipalFrom(ctx)
		if _, err := b.svc.audit.Append(ctx, tx, audit.Event{
			Stream:        audit.AdminStream,
			ActorType:     string(actor.ActorType),
			ActorID:       actor.SubjectID,
			Action:        AuditBreakGlassGranted,
			ResourceType:  "user",
			ResourceID:    grant.UserID,
			AfterHash:     hashBytes(out),
			RequestID:     observability.RequestID(ctx),
			CorrelationID: deref(current.CorrelationID),
			Reason:        current.Reason,
			EvidenceRef:   current.ID.String(),
			Payload:       out,
			OccurredAt:    now,
		}); err != nil {
			return nil, err
		}
		return out, nil
	})
	if err != nil {
		return Grant{}, err
	}
	if executed.Status != StatusExecuted {
		return Grant{}, errs.Newf(errs.CodeInternal, "admin: grant ended in status %s", executed.Status)
	}
	return grant, nil
}

// ParseBreakGlassParams decodes and validates the typed params. Unknown
// fields are rejected.
func ParseBreakGlassParams(params json.RawMessage) (BreakGlassParams, error) {
	dec := json.NewDecoder(bytes.NewReader(params))
	dec.DisallowUnknownFields()
	var p BreakGlassParams
	if err := dec.Decode(&p); err != nil {
		return BreakGlassParams{}, errs.Wrap(err, errs.CodeValidationFailed, "admin: break-glass params are malformed").WithField("field", "params")
	}
	if u, err := id.ParseAny(p.UserID); err != nil || u.IsZero() {
		return BreakGlassParams{}, errs.New(errs.CodeValidationFailed, "admin: break-glass user_id must be a user id").WithField("field", "params.user_id")
	}
	if p.Scope == "" || !validText(p.Scope) {
		return BreakGlassParams{}, errs.New(errs.CodeValidationFailed, "admin: break-glass scope is required").WithField("field", "params.scope")
	}
	if p.DurationSeconds <= 0 || time.Duration(p.DurationSeconds)*time.Second > MaxBreakGlassDuration {
		return BreakGlassParams{}, errs.Newf(errs.CodeValidationFailed, "admin: break-glass duration must be 1..%d seconds", int64(MaxBreakGlassDuration/time.Second)).WithField("field", "params.duration_seconds")
	}
	return p, nil
}

// ParseGrant decodes an execution_result produced by a BREAK_GLASS_GRANT.
func ParseGrant(result json.RawMessage) (Grant, error) {
	var g Grant
	if err := json.Unmarshal(result, &g); err != nil {
		return Grant{}, errs.Wrap(err, errs.CodeInternal, "admin: grant result is malformed")
	}
	if g.UserID == "" || g.ExpiresAt.IsZero() {
		return Grant{}, errs.New(errs.CodeInternal, "admin: grant result is incomplete")
	}
	g.ExpiresAt = g.ExpiresAt.UTC()
	return g, nil
}

// PrincipalWithBreakGlass returns a copy of p carrying the BREAK_GLASS role
// with BreakGlassUntil = g.ExpiresAt. It is a no-op copy (no elevation)
// when the grant is not for p's subject, when p is an agent, or when the
// grant has no expiry: a principal can never be elevated by someone else's
// grant. Liveness is judged by security at use time against the clock.
func PrincipalWithBreakGlass(p security.Principal, g Grant) security.Principal {
	c := p
	c.Roles = append([]security.Role(nil), p.Roles...)
	c.AccountIDs = append([]string(nil), p.AccountIDs...)
	c.AMR = append([]string(nil), p.AMR...)
	if p.BreakGlassUntil != nil {
		t := *p.BreakGlassUntil
		c.BreakGlassUntil = &t
	}
	if p.ActorType == security.ActorAgent || g.UserID == "" || g.ExpiresAt.IsZero() || !sameUser(p.SubjectID, g.UserID) {
		return c
	}
	until := g.ExpiresAt.UTC()
	c.BreakGlassUntil = &until
	if !c.HasRole(security.RoleBreakGlass) {
		c.Roles = append(c.Roles, security.RoleBreakGlass)
	}
	return c
}

// sameUser compares two user ids in canonical form.
func sameUser(a, b string) bool {
	ua, errA := id.ParseAny(a)
	ub, errB := id.ParseAny(b)
	if errA != nil || errB != nil {
		return false
	}
	return ua == ub && !ua.IsZero()
}
