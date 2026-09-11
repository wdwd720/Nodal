package httpapi

import (
	"context"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/notifications"
)

// NotificationsPort is the customer's notification centre (internal/notifications).
//
// Every method takes the user id rather than reading it from the principal, so
// the boundary states whose notifications it is asking for and the domain
// package checks that answer against the principal itself. Two independent
// statements of the same fact is the point: a handler that got the wrong user
// is refused rather than served.
type NotificationsPort interface {
	List(ctx context.Context, userID accounts.UserID, f notifications.Filter, cursor string, limit int) (notifications.Page, error)
	UnreadCount(ctx context.Context, userID accounts.UserID) (int, error)
	MarkRead(ctx context.Context, userID accounts.UserID, id notifications.ID) (notifications.Notification, error)
	MarkAllRead(ctx context.Context, userID accounts.UserID) (int, error)
	Preferences(ctx context.Context, userID accounts.UserID) ([]notifications.Preference, error)
	SavePreferences(ctx context.Context, userID accounts.UserID, want map[notifications.Kind]bool) ([]notifications.Preference, error)
}

// MeAuditPort is the customer's own audit trail (product goal §52): what
// happened to their sign-ins and to their accounts, and nothing else.
//
// It is deliberately narrower than internal/audit. The hash-chained
// audit_events stream carries operator identities, policy versions, evidence
// references and payloads that describe how the platform works; a customer is
// entitled to the record of what was done to THEM, which is a different
// document with the same underlying rows.
type MeAuditPort interface {
	Audit(ctx context.Context, userID accounts.UserID, accountIDs []string, cursor string, limit int) (MeAuditPage, error)
}

// MeAuditPage is one cursor page of the customer's own history.
type MeAuditPage struct {
	Items      []MeAuditItem
	NextCursor string
}

// MeAuditItem is one entry. Nothing on it identifies another person: an
// operator appears as an actor TYPE and never as an actor id.
type MeAuditItem struct {
	ID           string
	Source       string // SECURITY | ACCOUNT
	Action       string
	Severity     string
	ResourceType string
	ResourceID   string
	ActorType    string
	IP           string
	UserAgent    string
	OccurredAt   time.Time
}
