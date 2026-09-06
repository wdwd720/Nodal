package notification

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/security"
)

type notificationKind struct{}

// ID identifies a notification.
type ID = id.ID[notificationKind]

// Kind is the notification category (matches the DB CHECK).
type Kind string

// Kinds.
const (
	KindFundingAvailable     Kind = "FUNDING_AVAILABLE"
	KindFundingFailed        Kind = "FUNDING_FAILED"
	KindFundingReversed      Kind = "FUNDING_REVERSED"
	KindTradeFilled          Kind = "TRADE_FILLED"
	KindTradeFailed          Kind = "TRADE_FAILED"
	KindAgentPaused          Kind = "AGENT_PAUSED"
	KindRiskLimitHit         Kind = "RISK_LIMIT_HIT"
	KindSecuritySessionEvent Kind = "SECURITY_SESSION_EVENT"
	KindReconciliationHold   Kind = "RECONCILIATION_HOLD"
)

// Severity of a notification.
type Severity string

// Severities.
const (
	SeverityInfo     Severity = "INFO"
	SeverityWarn     Severity = "WARN"
	SeverityCritical Severity = "CRITICAL"
)

func (k Kind) valid() bool {
	switch k {
	case KindFundingAvailable, KindFundingFailed, KindFundingReversed, KindTradeFilled, KindTradeFailed,
		KindAgentPaused, KindRiskLimitHit, KindSecuritySessionEvent, KindReconciliationHold:
		return true
	}
	return false
}

func (s Severity) valid() bool {
	return s == SeverityInfo || s == SeverityWarn || s == SeverityCritical
}

// Notification is one customer-facing message.
type Notification struct {
	ID            ID
	UserID        accounts.UserID
	AccountID     *accounts.AccountID
	Kind          Kind
	Severity      Severity
	Title         string
	Body          string
	ResourceType  string
	ResourceID    string
	Data          json.RawMessage
	DedupKey      string
	CorrelationID string
	CreatedAt     time.Time
	ReadAt        *time.Time
	DeliveredAt   *time.Time
	DeliveryError string
}

// Validate checks structural invariants.
func (n Notification) Validate() error {
	switch {
	case n.UserID.IsZero():
		return errs.New(errs.CodeValidationFailed, "notification user required")
	case !n.Kind.valid():
		return errs.Newf(errs.CodeValidationFailed, "unknown notification kind %q", n.Kind)
	case !n.Severity.valid():
		return errs.Newf(errs.CodeValidationFailed, "unknown severity %q", n.Severity)
	case n.Title == "" || n.Body == "":
		return errs.New(errs.CodeValidationFailed, "title and body required")
	case len(n.Title) > 200 || len(n.Body) > 4000:
		return errs.New(errs.CodeValidationFailed, "title/body too long")
	}
	if len(n.Data) > 0 && !json.Valid(n.Data) {
		return errs.New(errs.CodeValidationFailed, "data must be valid JSON")
	}
	return nil
}

// Provider delivers notifications outside the platform (email, push, ...).
// Implementations must be idempotent on Notification.ID.
type Provider interface {
	Deliver(ctx context.Context, n Notification) error
	Name() string
}

// Repository persists notifications.
type Repository struct{}

// NewRepository returns a Repository.
func NewRepository() *Repository { return &Repository{} }

const columns = `id, user_id, account_id, kind, severity, title, body, coalesce(resource_type,''), coalesce(resource_id,''), data, coalesce(dedup_key,''), coalesce(correlation_id,''), created_at, read_at, delivered_at, coalesce(delivery_error,'')`

func scan(row pgx.Row) (Notification, error) {
	var n Notification
	var data []byte
	if err := row.Scan(&n.ID, &n.UserID, &n.AccountID, &n.Kind, &n.Severity, &n.Title, &n.Body, &n.ResourceType, &n.ResourceID, &data,
		&n.DedupKey, &n.CorrelationID, &n.CreatedAt, &n.ReadAt, &n.DeliveredAt, &n.DeliveryError); err != nil {
		return Notification{}, err
	}
	n.Data = data
	return n, nil
}

// Create inserts a notification inside the caller's transaction. A duplicate
// (user, dedup_key) returns the existing row with Existing=true so a replayed
// domain event never notifies twice.
func (r *Repository) Create(ctx context.Context, tx pgx.Tx, n Notification) (Notification, bool, error) {
	if err := n.Validate(); err != nil {
		return Notification{}, false, err
	}
	if n.ID.IsZero() {
		n.ID = id.New[notificationKind]()
	}
	data := n.Data
	if len(data) == 0 {
		data = json.RawMessage(`{}`)
	}
	row := tx.QueryRow(ctx, `INSERT INTO notifications (id, user_id, account_id, kind, severity, title, body, resource_type, resource_id, data, dedup_key, correlation_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),NULLIF($9,''),$10,NULLIF($11,''),NULLIF($12,''))
		ON CONFLICT (user_id, dedup_key) WHERE dedup_key IS NOT NULL DO NOTHING
		RETURNING `+columns,
		n.ID, n.UserID, n.AccountID, n.Kind, n.Severity, n.Title, n.Body, n.ResourceType, n.ResourceID, []byte(data), n.DedupKey, n.CorrelationID)
	created, err := scan(row)
	if err == nil {
		return created, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Notification{}, false, fmt.Errorf("notification: create: %w", err)
	}
	existing, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM notifications WHERE user_id = $1 AND dedup_key = $2`, n.UserID, n.DedupKey))
	if err != nil {
		return Notification{}, false, fmt.Errorf("notification: load existing: %w", err)
	}
	return existing, true, nil
}

// Page of notifications with an opaque cursor.
type Page struct {
	Items      []Notification
	NextCursor string
}

// ListForUser returns the caller's notifications, newest first. The principal
// in ctx must be the user (tenant isolation) or an operator with account:read_any.
func (r *Repository) ListForUser(ctx context.Context, q db.Querier, userID accounts.UserID, cursor string, limit int) (Page, error) {
	p, ok := security.PrincipalFrom(ctx)
	if !ok {
		return Page{}, errs.New(errs.CodeUnauthenticated, "no principal")
	}
	if p.SubjectID != userID.String() {
		if err := security.Require(ctx, security.PermAccountReadAny); err != nil {
			return Page{}, errs.Wrap(err, errs.CodeForbidden, "notifications belong to another user")
		}
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var (
		afterAt time.Time
		afterID string
	)
	if cursor != "" {
		if _, err := fmt.Sscanf(cursor, "%d|%s", new(int64), &afterID); err != nil {
			return Page{}, errs.New(errs.CodeValidationFailed, "invalid cursor")
		}
		var unix int64
		_, _ = fmt.Sscanf(cursor, "%d|", &unix)
		afterAt = time.UnixMicro(unix).UTC()
	}
	var rows pgx.Rows
	var err error
	if cursor == "" {
		rows, err = q.Query(ctx, `SELECT `+columns+` FROM notifications WHERE user_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2`, userID, limit+1)
	} else {
		rows, err = q.Query(ctx, `SELECT `+columns+` FROM notifications WHERE user_id = $1 AND (created_at, id::text) < ($2, $3) ORDER BY created_at DESC, id DESC LIMIT $4`, userID, afterAt, afterID, limit+1)
	}
	if err != nil {
		return Page{}, fmt.Errorf("notification: list: %w", err)
	}
	defer rows.Close()
	var out []Notification
	for rows.Next() {
		n, err := scan(rows)
		if err != nil {
			return Page{}, fmt.Errorf("notification: list scan: %w", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return Page{}, err
	}
	page := Page{Items: out}
	if len(out) > limit {
		page.Items = out[:limit]
		last := page.Items[limit-1]
		page.NextCursor = fmt.Sprintf("%d|%s", last.CreatedAt.UnixMicro(), last.ID.String())
	}
	return page, nil
}

// MarkRead sets read_at for one of the caller's notifications.
func (r *Repository) MarkRead(ctx context.Context, q db.Querier, userID accounts.UserID, notificationID ID, now time.Time) error {
	p, ok := security.PrincipalFrom(ctx)
	if !ok || p.SubjectID != userID.String() {
		return errs.New(errs.CodeForbidden, "only the recipient can mark a notification read")
	}
	tag, err := q.Exec(ctx, `UPDATE notifications SET read_at = coalesce(read_at, $3) WHERE id = $1 AND user_id = $2`, notificationID, userID, now.UTC())
	if err != nil {
		return fmt.Errorf("notification: mark read: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errs.New(errs.CodeNotFound, "notification not found")
	}
	return nil
}

// UnreadCount for the caller.
func (r *Repository) UnreadCount(ctx context.Context, q db.Querier, userID accounts.UserID) (int, error) {
	var n int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE user_id = $1 AND read_at IS NULL`, userID).Scan(&n); err != nil {
		return 0, fmt.Errorf("notification: unread: %w", err)
	}
	return n, nil
}

// Dispatcher delivers undelivered notifications through a Provider. It is
// safe to run from several workers: rows are claimed with SKIP LOCKED.
type Dispatcher struct {
	repo     *Repository
	provider Provider
}

// NewDispatcher returns a Dispatcher.
func NewDispatcher(repo *Repository, provider Provider) *Dispatcher {
	return &Dispatcher{repo: repo, provider: provider}
}

// RunOnce delivers up to limit notifications and returns how many succeeded.
func (d *Dispatcher) RunOnce(ctx context.Context, database *db.DB, limit int, now time.Time) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	delivered := 0
	err := database.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+columns+` FROM notifications WHERE delivered_at IS NULL AND delivery_error IS NULL ORDER BY created_at LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
		if err != nil {
			return err
		}
		var batch []Notification
		for rows.Next() {
			n, err := scan(rows)
			if err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, n)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, n := range batch {
			if derr := d.provider.Deliver(ctx, n); derr != nil {
				if _, err := tx.Exec(ctx, `UPDATE notifications SET delivery_error = $2 WHERE id = $1`, n.ID, truncate(derr.Error(), 500)); err != nil {
					return err
				}
				continue
			}
			if _, err := tx.Exec(ctx, `UPDATE notifications SET delivered_at = $2 WHERE id = $1`, n.ID, now.UTC()); err != nil {
				return err
			}
			delivered++
		}
		return nil
	})
	return delivered, err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// InAppProvider marks delivery complete without external side effects: the
// in-app record itself is the delivery. Production composes it with an
// external provider through Multi.
type InAppProvider struct{}

// Deliver is a no-op.
func (InAppProvider) Deliver(context.Context, Notification) error { return nil }

// Name returns the provider name.
func (InAppProvider) Name() string { return "in-app" }

// Multi delivers through every provider; the first failure is returned.
type Multi []Provider

// Deliver fans out.
func (m Multi) Deliver(ctx context.Context, n Notification) error {
	for _, p := range m {
		if err := p.Deliver(ctx, n); err != nil {
			return fmt.Errorf("%s: %w", p.Name(), err)
		}
	}
	return nil
}

// Name returns the composite name.
func (m Multi) Name() string { return "multi" }
