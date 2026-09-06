// Package pgstore is the PostgreSQL implementation of auth.SessionStore over
// the sessions table (migrations 00010 and 00011).
//
// Rows are keyed by the SHA-256 of the opaque token; the raw token never
// reaches this package. SubjectID must be the users.id UUID (the login
// handler resolves the identity-provider subject to a user first), and
// AccountIDs are derived from account ownership at read time so a stale
// session can never grant access to an account the user no longer owns.
//
// This package must never: store or log a raw token, extend a session's
// absolute expiry, or accept AGENT sessions (agents get in-process
// principals, never cookies).
package pgstore

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/auth"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/security"
)

// Store implements auth.SessionStore.
type Store struct{}

// New returns a Store.
func New() *Store { return &Store{} }

var _ auth.SessionStore = (*Store)(nil)

const sessionColumns = `s.id, encode(s.token_hash, 'hex'), s.user_id, s.actor_type, s.roles, s.created_at, s.last_seen_at, s.expires_at,
	s.auth_time, s.amr, coalesce(host(s.ip), ''), coalesce(s.user_agent, ''), coalesce(s.device_label, ''), s.rotated_from, s.revoked_at, s.break_glass_until,
	coalesce((SELECT array_agg(a.id::text ORDER BY a.created_at, a.id) FROM accounts a WHERE a.owner_user_id = s.user_id AND a.status <> 'CLOSED'), '{}')`

// Create inserts a session. The session's SubjectID must be a users.id UUID.
func (st *Store) Create(ctx context.Context, tx pgx.Tx, s auth.Session) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if s.ActorType == security.ActorAgent {
		return errors.New("pgstore: agent sessions are never persisted")
	}
	hash, err := hex.DecodeString(s.TokenHash)
	if err != nil {
		return fmt.Errorf("pgstore: token hash: %w", err)
	}
	roles := make([]string, 0, len(s.Roles))
	for _, r := range s.Roles {
		roles = append(roles, string(r))
	}
	var ip *netip.Addr
	if s.IP != "" {
		if a, perr := netip.ParseAddr(s.IP); perr == nil {
			ip = &a
		}
	}
	var rotatedFrom *string
	if s.RotatedFrom != "" {
		rotatedFrom = &s.RotatedFrom
	}
	_, err = tx.Exec(ctx, `INSERT INTO sessions
		(id, token_hash, user_id, actor_type, roles, created_at, last_seen_at, expires_at, auth_time, amr, ip, user_agent, device_label, rotated_from, revoked_at, break_glass_until)
		VALUES ($1, $2, $3::uuid, $4, $5, $6, $7, $8, $9, $10, $11, NULLIF($12,''), NULLIF($13,''), $14::uuid, $15, $16)`,
		s.ID, hash, s.SubjectID, string(s.ActorType), roles, s.CreatedAt.UTC(), s.LastSeenAt.UTC(), s.ExpiresAt.UTC(),
		s.AuthTime.UTC(), s.AMR, ip, s.UserAgent, s.DeviceLabel, rotatedFrom, s.RevokedAt, s.BreakGlassUntil)
	if err != nil {
		if db.IsUniqueViolation(err) {
			return auth.ErrSessionExists
		}
		return fmt.Errorf("pgstore: create session: %w", err)
	}
	return nil
}

func scanSession(row pgx.Row) (auth.Session, error) {
	var (
		s          auth.Session
		roles      []string
		userID     string
		actorType  string
		rotated    *string
		accountIDs []string
	)
	if err := row.Scan(&s.ID, &s.TokenHash, &userID, &actorType, &roles, &s.CreatedAt, &s.LastSeenAt, &s.ExpiresAt,
		&s.AuthTime, &s.AMR, &s.IP, &s.UserAgent, &s.DeviceLabel, &rotated, &s.RevokedAt, &s.BreakGlassUntil, &accountIDs); err != nil {
		return auth.Session{}, err
	}
	s.SubjectID = userID
	s.ActorType = security.ActorType(actorType)
	s.Roles = make([]security.Role, 0, len(roles))
	for _, r := range roles {
		s.Roles = append(s.Roles, security.Role(r))
	}
	if rotated != nil {
		s.RotatedFrom = *rotated
	}
	s.AccountIDs = accountIDs
	if s.AMR == nil {
		s.AMR = []string{}
	}
	return s, nil
}

// Get returns the session for a token hash, including revoked/expired rows.
func (st *Store) Get(ctx context.Context, q auth.Querier, tokenHash string) (auth.Session, error) {
	hash, err := hex.DecodeString(tokenHash)
	if err != nil {
		return auth.Session{}, auth.ErrSessionNotFound
	}
	s, err := scanSession(q.QueryRow(ctx, `SELECT `+sessionColumns+` FROM sessions s WHERE s.token_hash = $1`, hash))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return auth.Session{}, auth.ErrSessionNotFound
		}
		return auth.Session{}, fmt.Errorf("pgstore: get session: %w", err)
	}
	return s, nil
}

// Touch stamps last_seen_at with the transaction time.
func (st *Store) Touch(ctx context.Context, q auth.Querier, id string) error {
	tag, err := q.Exec(ctx, `UPDATE sessions SET last_seen_at = now() WHERE id = $1::uuid AND revoked_at IS NULL`, id)
	if err != nil {
		return fmt.Errorf("pgstore: touch: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return auth.ErrSessionNotFound
	}
	return nil
}

// Revoke marks the session revoked (idempotent).
func (st *Store) Revoke(ctx context.Context, q auth.Querier, id string) error {
	tag, err := q.Exec(ctx, `UPDATE sessions SET revoked_at = coalesce(revoked_at, now()), revoke_reason = coalesce(revoke_reason, 'revoked') WHERE id = $1::uuid`, id)
	if err != nil {
		return fmt.Errorf("pgstore: revoke: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return auth.ErrSessionNotFound
	}
	return nil
}

// RevokeAllForSubject revokes every active session of a user.
func (st *Store) RevokeAllForSubject(ctx context.Context, q auth.Querier, subjectID string) (int, error) {
	tag, err := q.Exec(ctx, `UPDATE sessions SET revoked_at = now(), revoke_reason = 'revoke_all' WHERE user_id = $1::uuid AND revoked_at IS NULL`, subjectID)
	if err != nil {
		return 0, fmt.Errorf("pgstore: revoke all: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ListForSubject lists a user's sessions, newest first (bounded).
func (st *Store) ListForSubject(ctx context.Context, q auth.Querier, subjectID string) ([]auth.Session, error) {
	rows, err := q.Query(ctx, `SELECT `+sessionColumns+` FROM sessions s WHERE s.user_id = $1::uuid ORDER BY s.created_at DESC, s.id DESC LIMIT 200`, subjectID)
	if err != nil {
		return nil, fmt.Errorf("pgstore: list: %w", err)
	}
	defer rows.Close()
	var out []auth.Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("pgstore: list scan: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// PurgeExpired deletes rows whose absolute expiry passed more than retention
// ago. It requires a connection with DELETE privilege (the ops role); the
// application role cannot delete sessions.
func (st *Store) PurgeExpired(ctx context.Context, q auth.Querier, retention time.Duration) (int, error) {
	tag, err := q.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now() - $1::interval`, retention.String())
	if err != nil {
		return 0, fmt.Errorf("pgstore: purge: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
