package httpapi

import (
	"context"
	"fmt"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/notifications"
)

// notificationsAdapter satisfies NotificationsPort over internal/notifications.
// It holds a Querier and a clock and nothing else: every rule about who may
// read what lives in the domain package, which is where it can be tested
// without an HTTP server.
type notificationsAdapter struct {
	q   db.Querier
	clk clock.Clock
}

// NewNotificationsPort returns the notification centre port.
func NewNotificationsPort(q db.Querier, clk clock.Clock) NotificationsPort {
	return notificationsAdapter{q: q, clk: clk}
}

func (a notificationsAdapter) List(ctx context.Context, userID accounts.UserID, f notifications.Filter, cursor string, limit int) (notifications.Page, error) {
	return notifications.List(ctx, a.q, userID, f, cursor, limit)
}

func (a notificationsAdapter) UnreadCount(ctx context.Context, userID accounts.UserID) (int, error) {
	return notifications.UnreadCount(ctx, a.q, userID)
}

func (a notificationsAdapter) MarkRead(ctx context.Context, userID accounts.UserID, id notifications.ID) (notifications.Notification, error) {
	return notifications.MarkRead(ctx, a.q, userID, id, a.clk.Now())
}

func (a notificationsAdapter) MarkAllRead(ctx context.Context, userID accounts.UserID) (int, error) {
	return notifications.MarkAllRead(ctx, a.q, userID, a.clk.Now())
}

func (a notificationsAdapter) Preferences(ctx context.Context, userID accounts.UserID) ([]notifications.Preference, error) {
	return notifications.LoadPreferences(ctx, a.q, userID)
}

func (a notificationsAdapter) SavePreferences(ctx context.Context, userID accounts.UserID, want map[notifications.Kind]bool) ([]notifications.Preference, error) {
	if err := notifications.SavePreferences(ctx, a.q, userID, want, a.clk.Now()); err != nil {
		return nil, err
	}
	return notifications.LoadPreferences(ctx, a.q, userID)
}

// meAuditReader satisfies MeAuditPort.
type meAuditReader struct{ q db.Querier }

// NewMeAuditPort returns the customer's own audit-trail port.
func NewMeAuditPort(q db.Querier) MeAuditPort { return meAuditReader{q: q} }

// meAuditSQL is one keyset page over two append-only tables.
//
// What it deliberately does not select: audit_events.actor_id (an operator's
// identity is not a customer's business), payload (it describes how the
// platform works and can name other records), evidence_ref, policy_version,
// before_hash/after_hash (the chain is verified by internal/audit, not by a
// browser), and every security_events row whose user_id is not the caller's.
// The two hash columns are what make the record tamper-evident; showing them
// here would invite a client to believe it had verified something.
const meAuditSQL = `
WITH sec AS (
    SELECT e.id::text                    AS entry_id,
           'SECURITY'::text              AS source,
           e.kind                        AS action,
           e.severity                    AS severity,
           ''::text                      AS resource_type,
           ''::text                      AS resource_id,
           ''::text                      AS actor_type,
           coalesce(host(e.ip), '')      AS ip,
           coalesce(e.user_agent, '')    AS user_agent,
           e.occurred_at                 AS occurred_at
      FROM security_events e
     WHERE e.user_id = $1
), acct AS (
    SELECT a.id::text,
           'ACCOUNT'::text,
           a.action,
           ''::text,
           coalesce(a.resource_type, ''),
           coalesce(a.resource_id, ''),
           a.actor_type,
           ''::text,
           ''::text,
           a.occurred_at
      FROM audit_events a
     WHERE $2::text[] IS NOT NULL AND a.stream = ANY($2::text[])
)
SELECT * FROM (SELECT * FROM sec UNION ALL SELECT * FROM acct) u
 WHERE ($3::timestamptz IS NULL OR (u.occurred_at, u.entry_id) < ($3::timestamptz, $4::text))
 ORDER BY u.occurred_at DESC, u.entry_id DESC
 LIMIT $5`

func (r meAuditReader) Audit(ctx context.Context, userID accounts.UserID, accountIDs []string, cursor string, limit int) (MeAuditPage, error) {
	if limit <= 0 || limit > maxPageLimit {
		limit = defaultPageLimit
	}
	c, ok, err := decodeCursor(cursor)
	if err != nil {
		return MeAuditPage{}, err
	}
	var atArg, idArg any
	if ok {
		atArg = c.At.UTC()
		idArg = c.ID
	}
	var streams any
	if len(accountIDs) > 0 {
		list := make([]string, 0, len(accountIDs))
		for _, a := range accountIDs {
			list = append(list, audit.AccountStream(a))
		}
		streams = list
	}
	rows, err := r.q.Query(ctx, meAuditSQL, userID, streams, atArg, idArg, limit+1)
	if err != nil {
		return MeAuditPage{}, fmt.Errorf("me audit: %w", err)
	}
	defer rows.Close()
	var out []MeAuditItem
	for rows.Next() {
		var it MeAuditItem
		var occurredAt time.Time
		if err := rows.Scan(&it.ID, &it.Source, &it.Action, &it.Severity, &it.ResourceType,
			&it.ResourceID, &it.ActorType, &it.IP, &it.UserAgent, &occurredAt); err != nil {
			return MeAuditPage{}, fmt.Errorf("me audit: %w", err)
		}
		it.OccurredAt = occurredAt.UTC()
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return MeAuditPage{}, fmt.Errorf("me audit: %w", err)
	}
	page := MeAuditPage{Items: out}
	if len(out) > limit {
		page.Items = out[:limit]
		last := page.Items[limit-1]
		page.NextCursor = encodeCursor(keysetCursor{At: last.OccurredAt, ID: last.ID})
	}
	return page, nil
}
