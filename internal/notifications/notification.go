package notifications

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
)

type notificationKind struct{}

// ID identifies a notification.
type ID = id.ID[notificationKind]

// ParseID parses a notification id.
func ParseID(s string) (ID, error) { return id.Parse[notificationKind](s) }

// NewID mints one. Callers do not need it -- Emit assigns an id when the
// caller leaves it zero -- but a test that wants to name a notification before
// it exists does.
func NewID() ID { return id.New[notificationKind]() }

// Ref points at the thing a notification is about. The pair is written to
// resource_type / resource_id and is half of the idempotency key, so it is
// never decorative: a notification with no ref can only be deduplicated by its
// occurrence.
type Ref struct {
	Type string
	ID   string
}

func (r Ref) empty() bool { return r.Type == "" && r.ID == "" }

// Notification is one thing a person was told.
type Notification struct {
	ID        ID
	UserID    accounts.UserID
	AccountID *accounts.AccountID
	Kind      Kind
	Severity  Severity
	Title     string
	Body      string
	Ref       Ref
	// Occurrence distinguishes two notifications about the same ref: the
	// transition row id, the fill id, the session id. Emit is idempotent on
	// (user, kind, ref, occurrence), so an occurrence that repeats is a
	// notification that does not.
	Occurrence string
	// Data carries identifiers and state names for the client to render
	// without a second request. It never carries a balance: the stream and the
	// notification are hints, and canonical figures come from REST (PART 109).
	Data          json.RawMessage
	CorrelationID string
	OccurredAt    time.Time
	// Sandbox labels a notification produced on a sandbox tier. ADR-0023
	// requires a sandbox outcome to be labelled sandbox everywhere it is
	// stored and shown; Producer refuses to set it off a sandbox tier and
	// refuses to clear it on one.
	Sandbox bool

	ReadAt *time.Time
}

const (
	maxTitle = 200
	maxBody  = 4000
	// maxDedupKey bounds the composed key before it is hashed instead. The
	// column is unbounded text, but a key that is the message is a key that
	// changes when the message does, and idempotence would then depend on
	// copywriting.
	maxDedupKey = 200
)

// DedupKey is the value Emit deduplicates on: the kind, the ref and the
// occurrence, and nothing else. It is exported because the follower asserts
// against it and because a caller reasoning about "will this notify twice"
// should be able to compute the answer.
func DedupKey(kind Kind, ref Ref, occurrence string) string {
	key := strings.Join([]string{string(kind), ref.Type, ref.ID, occurrence}, "|")
	if len(key) <= maxDedupKey {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	return string(kind) + "|sha256:" + hex.EncodeToString(sum[:])
}

// Validate checks structural invariants. It refuses a legacy kind outright:
// the product surface writes its own vocabulary, and the nine names 00640
// declared belong to internal/notification.
func (n Notification) Validate() error {
	switch {
	case n.UserID.IsZero():
		return errs.New(errs.CodeValidationFailed, "notification: a recipient is required")
	case !n.Kind.Valid():
		return errs.Newf(errs.CodeValidationFailed, "notification: unknown kind %q", n.Kind)
	case !n.Kind.IsProduct():
		return errs.Newf(errs.CodeValidationFailed,
			"notification: %q is declared by internal/notification and is not the product surface's to write", n.Kind)
	case n.Severity != "" && !n.Severity.Valid():
		return errs.Newf(errs.CodeValidationFailed, "notification: unknown severity %q", n.Severity)
	case strings.TrimSpace(n.Title) == "" || strings.TrimSpace(n.Body) == "":
		return errs.New(errs.CodeValidationFailed, "notification: a title and a body are required")
	case len(n.Title) > maxTitle || len(n.Body) > maxBody:
		return errs.New(errs.CodeValidationFailed, "notification: title or body too long")
	case n.Ref.empty() && n.Occurrence == "":
		return errs.New(errs.CodeValidationFailed,
			"notification: a ref or an occurrence is required, because one of them is what makes Emit idempotent")
	case (n.Ref.Type == "") != (n.Ref.ID == ""):
		return errs.New(errs.CodeValidationFailed, "notification: a ref carries both a type and an id or neither")
	}
	if len(n.Data) > 0 && !json.Valid(n.Data) {
		return errs.New(errs.CodeValidationFailed, "notification: data must be valid JSON")
	}
	return nil
}

// Emitted is what Emit did.
type Emitted struct {
	// Notification is the row as it now stands: the one just written, or the
	// one that was already there.
	Notification Notification
	// Created is true only when this call wrote the row. A follower publishes
	// to the realtime hub on Created and stays silent otherwise, which is why
	// re-reading a window is free.
	Created bool
	// Suppressed is true when the person switched this kind off. No row was
	// written and none will be; the state change still happened and its own
	// record (the transition row, the audit event) is untouched.
	Suppressed bool
}

// Producer writes notifications inside the caller's transaction.
type Producer struct {
	now     func() time.Time
	sandbox bool
}

// NewProducer returns a Producer. sandbox must be cfg.SandboxTier(): it is
// stamped on every notification the producer writes, so a sandbox deployment's
// notification centre says sandbox on every row without any caller
// remembering to.
func NewProducer(now func() time.Time, sandbox bool) *Producer {
	if now == nil {
		now = time.Now
	}
	return &Producer{now: now, sandbox: sandbox}
}

// Emit writes n inside tx.
//
// The transaction is the caller's on purpose: a notification exists if and only
// if the state change that caused it committed. Emit therefore never opens a
// transaction, never retries, and never talks to anything outside the database.
// Publishing to the realtime hub is the caller's job, AFTER the commit, because
// a hub is memory and memory cannot be rolled back.
func (p *Producer) Emit(ctx context.Context, tx pgx.Tx, n Notification) (Emitted, error) {
	// Everything that can be decided without the database is decided first, so
	// a caller can prove the refusals in a unit test and so a malformed
	// notification never opens a statement.
	if n.Sandbox && !p.sandbox {
		return Emitted{}, errs.New(errs.CodeValidationFailed,
			"notification: a sandbox notification cannot be written by a deployment that is not a sandbox tier")
	}
	n.Sandbox = p.sandbox
	if n.Severity == "" {
		n.Severity = n.Kind.defaultSeverity()
	}
	if n.OccurredAt.IsZero() {
		n.OccurredAt = p.now()
	}
	n.OccurredAt = n.OccurredAt.UTC()
	if err := n.Validate(); err != nil {
		return Emitted{}, err
	}
	if tx == nil {
		return Emitted{}, errs.New(errs.CodeInternal, "notification: Emit requires the caller's transaction")
	}
	if n.Kind.Suppressible() {
		off, err := suppressed(ctx, tx, n.UserID, n.Kind)
		if err != nil {
			return Emitted{}, err
		}
		if off {
			return Emitted{Suppressed: true}, nil
		}
	}
	if n.ID.IsZero() {
		n.ID = id.New[notificationKind]()
	}
	data := n.Data
	if len(data) == 0 {
		data = json.RawMessage(`{}`)
	}
	dedup := DedupKey(n.Kind, n.Ref, n.Occurrence)

	// delivered_at is stamped at insert because for this product the in-app
	// record IS the delivery. It also keeps internal/notification's dispatcher
	// -- which claims rows WHERE delivered_at IS NULL AND delivery_error IS
	// NULL -- from ever touching a product row, whatever a future deployment
	// decides to run.
	row := tx.QueryRow(ctx, `INSERT INTO notifications
			(id, user_id, account_id, kind, severity, title, body, resource_type, resource_id,
			 data, dedup_key, correlation_id, created_at, delivered_at, sandbox)
		VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),NULLIF($9,''),$10,$11,NULLIF($12,''),$13,$13,$14)
		ON CONFLICT (user_id, dedup_key) WHERE dedup_key IS NOT NULL DO NOTHING
		RETURNING `+columns,
		n.ID, n.UserID, n.AccountID, n.Kind, n.Severity, n.Title, n.Body, n.Ref.Type, n.Ref.ID,
		[]byte(data), dedup, n.CorrelationID, n.OccurredAt, n.Sandbox)
	created, err := scan(row)
	if err == nil {
		return Emitted{Notification: created, Created: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Emitted{}, fmt.Errorf("notification: emit: %w", err)
	}
	existing, err := scan(tx.QueryRow(ctx,
		`SELECT `+columns+` FROM notifications WHERE user_id = $1 AND dedup_key = $2`, n.UserID, dedup))
	if err != nil {
		return Emitted{}, fmt.Errorf("notification: emit: load the row already there: %w", err)
	}
	return Emitted{Notification: existing}, nil
}

const columns = `id, user_id, account_id, kind, severity, title, body, coalesce(resource_type,''), coalesce(resource_id,''),
	data, coalesce(correlation_id,''), created_at, read_at, sandbox`

func scan(row pgx.Row) (Notification, error) {
	var n Notification
	var data []byte
	if err := row.Scan(&n.ID, &n.UserID, &n.AccountID, &n.Kind, &n.Severity, &n.Title, &n.Body,
		&n.Ref.Type, &n.Ref.ID, &data, &n.CorrelationID, &n.OccurredAt, &n.ReadAt, &n.Sandbox); err != nil {
		return Notification{}, err
	}
	n.Data = data
	return n, nil
}
