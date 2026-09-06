package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/security"
)

// Defaults applied by NewManager when a ManagerConfig field is zero.
const (
	DefaultTTL           = 12 * time.Hour
	DefaultIdleTimeout   = 30 * time.Minute
	DefaultTouchInterval = time.Minute
)

// ManagerConfig tunes session lifetimes. Zero values take the defaults;
// negative values are rejected. IdleTimeout cannot be disabled.
type ManagerConfig struct {
	// TTL is the absolute session lifetime from issuance.
	TTL time.Duration
	// IdleTimeout invalidates a session not seen for this long.
	IdleTimeout time.Duration
	// TouchInterval rate-limits LastSeenAt writes: a session is touched at
	// most once per interval.
	TouchInterval time.Duration
	// Now is the injected clock (a clock.Clock's Now method value fits).
	// Defaults to the UTC wall clock.
	Now func() time.Time
}

// Manager issues, validates, rotates and revokes sessions.
type Manager struct {
	store SessionStore
	cfg   ManagerConfig
}

// NewManager validates cfg, applies defaults and returns a Manager.
func NewManager(store SessionStore, cfg ManagerConfig) (*Manager, error) {
	if store == nil {
		return nil, errors.New("auth: nil session store")
	}
	if cfg.TTL < 0 || cfg.IdleTimeout < 0 || cfg.TouchInterval < 0 {
		return nil, errors.New("auth: negative duration in manager config")
	}
	if cfg.TTL == 0 {
		cfg.TTL = DefaultTTL
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = DefaultIdleTimeout
	}
	if cfg.TouchInterval == 0 {
		cfg.TouchInterval = DefaultTouchInterval
	}
	if cfg.IdleTimeout > cfg.TTL {
		return nil, errors.New("auth: idle timeout longer than ttl")
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Manager{store: store, cfg: cfg}, nil
}

// Now returns the manager's current time in UTC.
func (m *Manager) Now() time.Time { return m.cfg.Now().UTC() }

// IssueParams describes the session to create. Roles and AccountIDs are the
// authorization state decided by the caller from the Identity and the
// user directory; the identity provider never dictates roles.
type IssueParams struct {
	SubjectID       string
	ActorType       security.ActorType
	Roles           []security.Role
	AccountIDs      []string
	AuthTime        time.Time
	AMR             []string
	IP              string
	UserAgent       string
	DeviceLabel     string
	BreakGlassUntil *time.Time
}

// Issued is a freshly created session together with the raw token that
// must go into the cookie and nowhere else.
type Issued struct {
	Session Session
	Token   string
}

// Issue creates and persists a session inside tx.
func (m *Manager) Issue(ctx context.Context, tx pgx.Tx, p IssueParams) (Issued, error) {
	now := m.Now()
	s := Session{
		SubjectID:       p.SubjectID,
		ActorType:       p.ActorType,
		Roles:           append([]security.Role(nil), p.Roles...),
		AccountIDs:      append([]string(nil), p.AccountIDs...),
		CreatedAt:       now,
		LastSeenAt:      now,
		ExpiresAt:       now.Add(m.cfg.TTL),
		AuthTime:        p.AuthTime,
		AMR:             append([]string(nil), p.AMR...),
		IP:              p.IP,
		UserAgent:       p.UserAgent,
		DeviceLabel:     p.DeviceLabel,
		BreakGlassUntil: cloneTime(p.BreakGlassUntil),
	}
	return m.create(ctx, tx, s)
}

func (m *Manager) create(ctx context.Context, tx pgx.Tx, s Session) (Issued, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return Issued{}, fmt.Errorf("auth: session id: %w", err)
	}
	s.ID = id.String()
	raw, err := NewToken()
	if err != nil {
		return Issued{}, err
	}
	s.TokenHash = HashToken(raw)
	if err := s.Validate(); err != nil {
		return Issued{}, err
	}
	if err := m.store.Create(ctx, tx, s); err != nil {
		return Issued{}, fmt.Errorf("auth: create session: %w", err)
	}
	return Issued{Session: s, Token: raw}, nil
}

// Check decides whether s is usable at now. It never consults the store.
func (m *Manager) Check(s Session, now time.Time) error {
	if err := s.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSession, err)
	}
	if s.IsRevoked() {
		return ErrSessionRevoked
	}
	if !now.Before(s.ExpiresAt) {
		return ErrSessionExpired
	}
	if now.Sub(s.LastSeenAt) > m.cfg.IdleTimeout {
		return ErrSessionIdle
	}
	return nil
}

// Authenticate resolves a raw token to a valid session, touching
// LastSeenAt at most once per TouchInterval. Store failures are returned
// as-is (not as ErrInvalidSession) so callers fail closed with a 5xx rather
// than treating an outage as an anonymous request.
func (m *Manager) Authenticate(ctx context.Context, q Querier, rawToken string) (Session, error) {
	if err := ValidateToken(rawToken); err != nil {
		return Session{}, err
	}
	s, err := m.store.Get(ctx, q, HashToken(rawToken))
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return Session{}, err
		}
		return Session{}, fmt.Errorf("auth: load session: %w", err)
	}
	now := m.Now()
	if err := m.Check(s, now); err != nil {
		return Session{}, err
	}
	if now.Sub(s.LastSeenAt) >= m.cfg.TouchInterval {
		if err := m.store.Touch(ctx, q, s.ID); err != nil {
			return Session{}, fmt.Errorf("auth: touch session: %w", err)
		}
		s.LastSeenAt = now
	}
	return s, nil
}

// Rotation is the privilege state of the replacement session produced by
// Rotate. Use RotationFrom to start from the current state.
type Rotation struct {
	Roles           []security.Role
	AccountIDs      []string
	AuthTime        time.Time
	AMR             []string
	BreakGlassUntil *time.Time
}

// RotationFrom copies the privilege state of s so a caller can change only
// what the privilege change affects.
func RotationFrom(s Session) Rotation {
	return Rotation{
		Roles:           append([]security.Role(nil), s.Roles...),
		AccountIDs:      append([]string(nil), s.AccountIDs...),
		AuthTime:        s.AuthTime,
		AMR:             append([]string(nil), s.AMR...),
		BreakGlassUntil: cloneTime(s.BreakGlassUntil),
	}
}

// Rotate revokes current and issues a replacement with r's privilege state
// in the same transaction (PART 192: rotation on privilege change). The
// replacement keeps the subject, actor type, device facts and — so rotation
// can never extend a login — the original absolute ExpiresAt. The session is
// re-read from the store inside tx and must be valid there: a caller's
// stale copy of a session that has since been revoked cannot be rotated.
func (m *Manager) Rotate(ctx context.Context, tx pgx.Tx, current Session, r Rotation) (Issued, error) {
	now := m.Now()
	stored, err := m.store.Get(ctx, tx, current.TokenHash)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return Issued{}, err
		}
		return Issued{}, fmt.Errorf("auth: load session: %w", err)
	}
	if stored.ID != current.ID {
		return Issued{}, ErrSessionNotFound
	}
	current = stored
	if err := m.Check(current, now); err != nil {
		return Issued{}, err
	}
	if err := m.store.Revoke(ctx, tx, current.ID); err != nil {
		return Issued{}, fmt.Errorf("auth: revoke rotated session: %w", err)
	}
	next := Session{
		SubjectID:       current.SubjectID,
		ActorType:       current.ActorType,
		Roles:           append([]security.Role(nil), r.Roles...),
		AccountIDs:      append([]string(nil), r.AccountIDs...),
		CreatedAt:       now,
		LastSeenAt:      now,
		ExpiresAt:       current.ExpiresAt,
		AuthTime:        r.AuthTime,
		AMR:             append([]string(nil), r.AMR...),
		IP:              current.IP,
		UserAgent:       current.UserAgent,
		DeviceLabel:     current.DeviceLabel,
		RotatedFrom:     current.ID,
		BreakGlassUntil: cloneTime(r.BreakGlassUntil),
	}
	return m.create(ctx, tx, next)
}

// Revoke invalidates one session (logout, or session:revoke_own /
// session:revoke_any after the caller has authorized it).
func (m *Manager) Revoke(ctx context.Context, q Querier, id string) error {
	if err := m.store.Revoke(ctx, q, id); err != nil {
		return fmt.Errorf("auth: revoke session: %w", err)
	}
	return nil
}

// RevokeAllForSubject invalidates every session of a subject ("log out
// everywhere", credential compromise) and returns how many were live.
func (m *Manager) RevokeAllForSubject(ctx context.Context, q Querier, subjectID string) (int, error) {
	n, err := m.store.RevokeAllForSubject(ctx, q, subjectID)
	if err != nil {
		return 0, fmt.Errorf("auth: revoke sessions: %w", err)
	}
	return n, nil
}

// ListForSubject returns the listing view of a subject's sessions, newest
// first, for the device/session screen.
func (m *Manager) ListForSubject(ctx context.Context, q Querier, subjectID string) ([]Summary, error) {
	ss, err := m.store.ListForSubject(ctx, q, subjectID)
	if err != nil {
		return nil, fmt.Errorf("auth: list sessions: %w", err)
	}
	out := make([]Summary, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.Summary())
	}
	return out, nil
}
