package notifications

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
)

// DefaultPageLimit and MaxPageLimit bound a page of the notification centre.
const (
	DefaultPageLimit = 50
	MaxPageLimit     = 200
)

// Filter narrows a page of the notification centre.
type Filter struct {
	// UnreadOnly restricts the page to notifications with no read_at.
	UnreadOnly bool
	// Kinds restricts the page to these kinds. Empty means every kind.
	Kinds []Kind
}

// Page is one cursor page, newest first.
type Page struct {
	Items      []Notification
	NextCursor string
}

// requireSelf is the tenant check, made in this package as well as at the
// boundary. A notification is addressed to one person: there is no
// account:read_any escape hatch here, deliberately, because an operator who
// needs to know what the platform did reads the audit trail, and what a
// customer was shown is not the same question.
func requireSelf(ctx context.Context, userID accounts.UserID) error {
	p, ok := security.PrincipalFrom(ctx)
	if !ok {
		return errs.New(errs.CodeUnauthenticated, "notification: no principal")
	}
	if p.ActorType == security.ActorAgent {
		return errs.New(errs.CodeForbidden, "notification: an agent has no notification centre")
	}
	if p.SubjectID != userID.String() {
		return errs.New(errs.CodeForbidden, "notification: notifications belong to the person they were addressed to")
	}
	return nil
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// List returns the caller's notifications, newest first.
func List(ctx context.Context, q db.Querier, userID accounts.UserID, f Filter, cursor string, limit int) (Page, error) {
	if err := requireSelf(ctx, userID); err != nil {
		return Page{}, err
	}
	if limit <= 0 || limit > MaxPageLimit {
		limit = DefaultPageLimit
	}
	kinds := make([]string, 0, len(f.Kinds))
	for _, k := range f.Kinds {
		if !k.IsProduct() {
			return Page{}, errs.Newf(errs.CodeValidationFailed, "notification: unknown kind %q", k)
		}
		kinds = append(kinds, string(k))
	}
	at, afterID, err := decodeCursor(cursor)
	if err != nil {
		return Page{}, err
	}

	// One statement with optional predicates rather than four assembled ones:
	// a NULL parameter turns its clause off, so the query plan and the
	// injection surface are the same whatever the caller asked for.
	var kindArg any
	if len(kinds) > 0 {
		kindArg = kinds
	}
	var atArg any
	var idArg any
	if afterID != "" {
		atArg = at
		idArg = afterID
	}
	rows, err := q.Query(ctx, `SELECT `+columns+` FROM notifications
		WHERE user_id = $1
		  AND ($2::boolean IS NOT TRUE OR read_at IS NULL)
		  AND ($3::text[] IS NULL OR kind = ANY($3::text[]))
		  AND ($4::timestamptz IS NULL OR (created_at, id::text) < ($4::timestamptz, $5::text))
		ORDER BY created_at DESC, id DESC
		LIMIT $6`, userID, f.UnreadOnly, kindArg, atArg, idArg, limit+1)
	if err != nil {
		return Page{}, fmt.Errorf("notification: list: %w", err)
	}
	defer rows.Close()
	var out []Notification
	for rows.Next() {
		n, err := scan(rows)
		if err != nil {
			return Page{}, fmt.Errorf("notification: list: %w", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("notification: list: %w", err)
	}
	page := Page{Items: out}
	if len(out) > limit {
		page.Items = out[:limit]
		last := page.Items[limit-1]
		page.NextCursor = encodeCursor(last.OccurredAt, last.ID.String())
	}
	return page, nil
}

// Since returns the caller's notifications WRITTEN strictly after at, oldest
// first, and reports whether there were more than limit of them. It is the
// durable half of Last-Event-ID resume: the realtime hub is one process's
// memory, and this is the table that outlives it.
//
// The filter is inserted_at, not created_at. created_at is the instant the
// thing happened, which the follower copies from the source row, so a capture
// from five minutes ago written thirty seconds ago is stamped five minutes ago
// -- and a client asking "what have I missed since I left" was told nothing
// about it, forever. What a reconnecting client is asking about is when the ROW
// appeared, and that is a separate column now (D-104, F-186).
func Since(ctx context.Context, q db.Querier, userID accounts.UserID, at time.Time, limit int) ([]Notification, bool, error) {
	if err := requireSelf(ctx, userID); err != nil {
		return nil, false, err
	}
	if limit <= 0 || limit > MaxPageLimit {
		limit = DefaultPageLimit
	}
	rows, err := q.Query(ctx, `SELECT `+columns+` FROM notifications
		WHERE user_id = $1 AND inserted_at > $2
		ORDER BY inserted_at, id LIMIT $3`, userID, at.UTC(), limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("notification: since: %w", err)
	}
	defer rows.Close()
	var out []Notification
	for rows.Next() {
		n, err := scan(rows)
		if err != nil {
			return nil, false, fmt.Errorf("notification: since: %w", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("notification: since: %w", err)
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

// MarkRead stamps read_at on one of the caller's notifications. It is
// idempotent: a second call keeps the first instant, because "when did you see
// this" has one answer.
func MarkRead(ctx context.Context, q db.Querier, userID accounts.UserID, notificationID ID, now time.Time) (Notification, error) {
	if err := requireSelf(ctx, userID); err != nil {
		return Notification{}, err
	}
	n, err := scan(q.QueryRow(ctx,
		`UPDATE notifications SET read_at = coalesce(read_at, $3) WHERE id = $1 AND user_id = $2 RETURNING `+columns,
		notificationID, userID, now.UTC()))
	if isNoRows(err) {
		return Notification{}, errs.New(errs.CodeNotFound, "notification not found")
	}
	if err != nil {
		return Notification{}, fmt.Errorf("notification: mark read: %w", err)
	}
	return n, nil
}

// MarkAllRead stamps read_at on every unread notification the caller has and
// returns how many it changed.
func MarkAllRead(ctx context.Context, q db.Querier, userID accounts.UserID, now time.Time) (int, error) {
	if err := requireSelf(ctx, userID); err != nil {
		return 0, err
	}
	tag, err := q.Exec(ctx,
		`UPDATE notifications SET read_at = $2 WHERE user_id = $1 AND read_at IS NULL`, userID, now.UTC())
	if err != nil {
		return 0, fmt.Errorf("notification: mark all read: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// UnreadCount is the badge.
func UnreadCount(ctx context.Context, q db.Querier, userID accounts.UserID) (int, error) {
	if err := requireSelf(ctx, userID); err != nil {
		return 0, err
	}
	var n int
	if err := q.QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE user_id = $1 AND read_at IS NULL`, userID).Scan(&n); err != nil {
		return 0, fmt.Errorf("notification: unread count: %w", err)
	}
	return n, nil
}

// encodeCursor and decodeCursor carry the keyset position (created_at, id).
// The pair is the ordering key, so the cursor is exactly the ordering key and
// nothing else -- an offset would skip a row whenever one arrives mid-page,
// which for a notification centre is the normal case rather than the edge.
//
// The wire form is base64 of a small JSON object, matching the convention
// internal/httpapi/readmodel.go already established: a client cannot construct
// one from a guessed id, and the shape can change without breaking clients.
type pageCursor struct {
	At time.Time `json:"at"`
	ID string    `json:"id"`
}

func encodeCursor(at time.Time, rowID string) string {
	b, err := json.Marshal(pageCursor{At: at.UTC(), ID: rowID})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (time.Time, string, error) {
	if s == "" {
		return time.Time{}, "", nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, "", errs.New(errs.CodeValidationFailed, "notification: cursor is not a valid pagination cursor")
	}
	var c pageCursor
	if err := json.Unmarshal(b, &c); err != nil || c.ID == "" {
		return time.Time{}, "", errs.New(errs.CodeValidationFailed, "notification: cursor is not a valid pagination cursor")
	}
	if _, err := ParseID(c.ID); err != nil {
		return time.Time{}, "", errs.New(errs.CodeValidationFailed, "notification: cursor is not a valid pagination cursor")
	}
	return c.At.UTC(), c.ID, nil
}
