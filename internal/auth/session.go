package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/nodal/controlplane/internal/security"
)

// Session is one server-side login. The raw token is never stored: TokenHash
// is hex(sha256(token)). Times are UTC.
type Session struct {
	ID        string // UUIDv7, canonical form
	TokenHash string // hex(sha256(raw token)); never the raw token
	SubjectID string
	ActorType security.ActorType
	Roles     []security.Role
	// AccountIDs the subject may act on. Exactly one for an AGENT.
	AccountIDs []string

	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time // absolute lifetime; never extended by touch or rotation

	// AuthTime and AMR are copied from the identity provider's claims and
	// drive security.RequireStepUp.
	AuthTime time.Time
	AMR      []string

	IP          string
	UserAgent   string
	DeviceLabel string

	RevokedAt *time.Time
	// RotatedFrom is the ID of the session this one replaced, if any.
	RotatedFrom string
	// BreakGlassUntil is the expiry of a break-glass elevation carried by
	// this session (PART 93). A session obtains one either by rotation
	// (Rotate, when the holder's own privileges change) or from an executed
	// BREAK_GLASS_GRANT admin action applied with Manager.Elevate — a grant
	// is issued to someone else by two other people, who cannot hand the
	// grantee a rotated token, so that path writes the expiry onto the
	// sessions the grantee already holds. Liveness is judged against the
	// clock at every use, never at issue time.
	BreakGlassUntil *time.Time
}

// Validate checks structural invariants (shared with security.Principal).
func (s Session) Validate() error {
	if s.ID == "" {
		return errors.New("auth: session has empty id")
	}
	if len(s.TokenHash) != tokenHashHexLen || !isLowerHex(s.TokenHash) {
		return errors.New("auth: session token hash is not hex(sha256)")
	}
	if s.CreatedAt.IsZero() || s.ExpiresAt.IsZero() || s.LastSeenAt.IsZero() {
		return errors.New("auth: session timestamps must be set")
	}
	if !s.ExpiresAt.After(s.CreatedAt) {
		return errors.New("auth: session expiry must be after creation")
	}
	if s.LastSeenAt.Before(s.CreatedAt) {
		return errors.New("auth: session last-seen precedes creation")
	}
	return s.principal().Validate()
}

// IsRevoked reports whether the session has been revoked.
func (s Session) IsRevoked() bool { return s.RevokedAt != nil }

// principal builds the unvalidated principal view.
func (s Session) principal() security.Principal {
	if s.ActorType == security.ActorAgent {
		acct := ""
		if len(s.AccountIDs) == 1 {
			acct = s.AccountIDs[0]
		}
		p := security.AgentPrincipal(s.SubjectID, acct)
		if len(s.AccountIDs) != 1 {
			p.AccountIDs = append([]string(nil), s.AccountIDs...)
		}
		// A malformed agent row keeps its roles so Validate fails closed.
		p.Roles = append([]security.Role(nil), s.Roles...)
		p.BreakGlassUntil = cloneTime(s.BreakGlassUntil)
		p.SessionID = s.ID
		p.AuthTime = s.AuthTime
		p.AMR = append([]string(nil), s.AMR...)
		return p
	}
	return security.Principal{
		SubjectID:       s.SubjectID,
		ActorType:       s.ActorType,
		Roles:           append([]security.Role(nil), s.Roles...),
		AccountIDs:      append([]string(nil), s.AccountIDs...),
		SessionID:       s.ID,
		AuthTime:        s.AuthTime,
		AMR:             append([]string(nil), s.AMR...),
		BreakGlassUntil: cloneTime(s.BreakGlassUntil),
	}
}

// Principal converts a validated session into the request principal. It
// returns an error for a session that fails Validate, so a corrupt row can
// never become authority.
func (s Session) Principal() (security.Principal, error) {
	if err := s.Validate(); err != nil {
		return security.Principal{}, fmt.Errorf("%w: %v", ErrInvalidSession, err)
	}
	return s.principal(), nil
}

// Summary is the device-listing view of a session (PART 192): no hash, no
// roles, nothing a client could use to escalate.
type Summary struct {
	ID          string
	CreatedAt   time.Time
	LastSeenAt  time.Time
	ExpiresAt   time.Time
	IP          string
	UserAgent   string
	DeviceLabel string
	RevokedAt   *time.Time
}

// Summary returns the listing view.
func (s Session) Summary() Summary {
	return Summary{
		ID: s.ID, CreatedAt: s.CreatedAt, LastSeenAt: s.LastSeenAt, ExpiresAt: s.ExpiresAt,
		IP: s.IP, UserAgent: s.UserAgent, DeviceLabel: s.DeviceLabel, RevokedAt: cloneTime(s.RevokedAt),
	}
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

// Querier is the read/write handle a store method runs on: a pool, a
// connection or a transaction. It is structurally identical to db.Querier
// so those values satisfy it without conversion.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// SessionStore persists sessions. Implementations key rows by TokenHash and
// never see a raw token. Get returns revoked and expired rows unchanged:
// validity is the Manager's decision, so it can distinguish the failure.
// Touch and Revoke stamp the store's notion of "now" (transaction time in
// Postgres). Revoke is idempotent; both return ErrSessionNotFound for an
// unknown id. Create returns ErrSessionExists on an id or hash collision.
type SessionStore interface {
	Create(ctx context.Context, tx pgx.Tx, s Session) error
	Get(ctx context.Context, q Querier, tokenHash string) (Session, error)
	Touch(ctx context.Context, q Querier, id string) error
	Revoke(ctx context.Context, q Querier, id string) error
	RevokeAllForSubject(ctx context.Context, q Querier, subjectID string) (int, error)
	ListForSubject(ctx context.Context, q Querier, subjectID string) ([]Session, error)
	// Elevate stamps a break-glass elevation on every session of subjectID
	// that is usable at now — an OPERATOR session, not revoked, not past
	// its absolute expiry — setting BreakGlassUntil to until and adding the
	// BREAK_GLASS role, and returns how many rows it changed. It replaces
	// any elevation already on those rows rather than extending it, and it
	// never touches a session belonging to anyone else, a USER session or a
	// revoked one. It is the only write that changes a live session's
	// privileges; see Manager.Elevate for the bounds it is checked against.
	Elevate(ctx context.Context, q Querier, subjectID string, until, now time.Time) (int, error)
}
