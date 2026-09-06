package event

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
)

// Outcome is the result of Inbox.Process.
type Outcome int

const (
	// Processed: the message was new (or previously FAILED) and fn ran.
	Processed Outcome = iota + 1
	// Duplicate: the message was already processed; fn did not run.
	Duplicate
)

// String renders the outcome.
func (o Outcome) String() string {
	switch o {
	case Processed:
		return "PROCESSED"
	case Duplicate:
		return "DUPLICATE"
	}
	return fmt.Sprintf("Outcome(%d)", int(o))
}

// Status values of inbox_messages.status.
type Status string

const (
	StatusReceived  Status = "RECEIVED"
	StatusProcessed Status = "PROCESSED"
	StatusFailed    Status = "FAILED"
)

// MaxInboxKeyLength bounds source and message_id.
const MaxInboxKeyLength = 512

// InProgressRetryAfter is the Retry-After hint on IDEMPOTENCY_IN_PROGRESS.
const InProgressRetryAfter = time.Second

// Record is a stored inbox row, for observability and tests.
type Record struct {
	Source        string
	MessageID     string
	SchemaVersion int
	ReceivedAt    time.Time
	ProcessedAt   *time.Time
	Status        Status
	Error         *string
	PayloadHash   *string
}

// Inbox implements exactly-once processing of at-least-once deliveries on
// top of inbox_messages (PART 31, PART 199).
type Inbox struct {
	clk clock.Clock
	obs Observer
}

// NewInbox returns an Inbox stamping received_at/processed_at from clk. A
// nil clk means the system clock.
func NewInbox(clk clock.Clock) *Inbox {
	if clk == nil {
		clk = clock.System()
	}
	return &Inbox{clk: clk, obs: NopObserver{}}
}

// WithObserver returns a copy of the Inbox reporting duplicates to obs.
func (i *Inbox) WithObserver(obs Observer) *Inbox {
	if obs == nil {
		obs = NopObserver{}
	}
	return &Inbox{clk: i.clk, obs: obs}
}

// HashPayload returns the lowercase hex SHA-256 of b for payload_hash.
func HashPayload(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

const (
	insertInboxSQL = `
INSERT INTO inbox_messages (source, message_id, schema_version, received_at, status, payload_hash)
VALUES ($1, $2, $3, $4, 'RECEIVED', $5)
ON CONFLICT (source, message_id) DO NOTHING
RETURNING status`

	selectInboxSQL = `
SELECT status, payload_hash FROM inbox_messages WHERE source = $1 AND message_id = $2`

	lockInboxSQL = selectInboxSQL + ` FOR UPDATE NOWAIT`

	markProcessedSQL = `
UPDATE inbox_messages
SET status = 'PROCESSED', processed_at = $3, error = NULL, schema_version = $4,
    payload_hash = COALESCE($5, payload_hash)
WHERE source = $1 AND message_id = $2`

	markFailedInboxSQL = `
INSERT INTO inbox_messages (source, message_id, schema_version, received_at, status, error)
VALUES ($1, $2, $3, $4, 'FAILED', $5)
ON CONFLICT (source, message_id) DO UPDATE
SET status = 'FAILED', error = EXCLUDED.error
WHERE inbox_messages.status <> 'PROCESSED'`

	getInboxSQL = `
SELECT source, message_id, schema_version, received_at, processed_at, status, error, payload_hash
FROM inbox_messages WHERE source = $1 AND message_id = $2`
)

// Process records (source, messageID) and runs fn exactly once for it.
//
//   - New message: a RECEIVED row is inserted in tx, fn runs, the row becomes
//     PROCESSED. If fn returns an error it is returned unchanged and the
//     caller's transaction (row included) rolls back; call MarkFailed
//     afterwards to record the failure.
//   - PROCESSED: fn does not run and Duplicate is returned.
//   - FAILED: the row is locked and fn runs again (re-processable).
//   - In flight elsewhere (uncommitted insert, locked FAILED row, or a
//     committed RECEIVED row): an IDEMPOTENCY_IN_PROGRESS *errs.Error with
//     RetryAfter is returned; the caller rolls back and retries later.
//
// Under REPEATABLE READ or SERIALIZABLE a message committed by another
// transaction after this transaction's snapshot is reported as in progress
// rather than Duplicate; retrying observes the final state.
func (i *Inbox) Process(ctx context.Context, tx pgx.Tx, source, messageID string, schemaVersion int, fn func(ctx context.Context, tx pgx.Tx) error) (Outcome, error) {
	return i.ProcessHashed(ctx, tx, source, messageID, schemaVersion, "", fn)
}

// ProcessHashed is Process that also records payloadHash (see HashPayload,
// PART 30 step 4). A duplicate whose stored hash differs from payloadHash is
// a CONFLICT: the same message id arrived with different content, which is
// never acknowledged silently.
func (i *Inbox) ProcessHashed(ctx context.Context, tx pgx.Tx, source, messageID string, schemaVersion int, payloadHash string, fn func(ctx context.Context, tx pgx.Tx) error) (Outcome, error) {
	if err := validateInboxArgs(source, messageID, schemaVersion); err != nil {
		return 0, err
	}
	if fn == nil {
		return 0, errs.New(errs.CodeInternal, "event: inbox fn is nil")
	}
	if tx == nil {
		return 0, errs.New(errs.CodeInternal, "event: nil transaction")
	}
	now := i.clk.Now().UTC()

	var status string
	err := tx.QueryRow(ctx, insertInboxSQL, source, messageID, schemaVersion, now, nullable(payloadHash)).Scan(&status)
	switch {
	case err == nil:
		return i.run(ctx, tx, source, messageID, schemaVersion, payloadHash, fn)
	case errors.Is(err, pgx.ErrNoRows):
		// Conflict with a committed row: inspect it.
	case db.IsLockTimeout(err):
		return 0, inProgress(err)
	default:
		return 0, errs.Wrap(err, errs.CodeInternal, "event: inbox insert failed")
	}

	var storedHash *string
	err = tx.QueryRow(ctx, selectInboxSQL, source, messageID).Scan(&status, &storedHash)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Inserted and committed by another transaction after our snapshot.
		return 0, inProgress(err)
	case err != nil:
		return 0, errs.Wrap(err, errs.CodeInternal, "event: inbox lookup failed")
	}
	switch Status(status) {
	case StatusProcessed:
		if err := checkHash(source, messageID, storedHash, payloadHash); err != nil {
			return 0, err
		}
		i.obs.OnDuplicate(source)
		return Duplicate, nil
	case StatusReceived:
		return 0, inProgress(nil)
	case StatusFailed:
		// Lock for re-processing; NOWAIT so a concurrent re-processor is
		// reported as in progress instead of blocking the caller.
		err = tx.QueryRow(ctx, lockInboxSQL, source, messageID).Scan(&status, &storedHash)
		switch {
		case db.IsLockTimeout(err):
			return 0, inProgress(err)
		case errors.Is(err, pgx.ErrNoRows):
			return 0, inProgress(err)
		case err != nil:
			return 0, errs.Wrap(err, errs.CodeInternal, "event: inbox lock failed")
		}
		switch Status(status) {
		case StatusProcessed:
			if err := checkHash(source, messageID, storedHash, payloadHash); err != nil {
				return 0, err
			}
			i.obs.OnDuplicate(source)
			return Duplicate, nil
		case StatusReceived:
			return 0, inProgress(nil)
		}
		return i.run(ctx, tx, source, messageID, schemaVersion, payloadHash, fn)
	}
	return 0, errs.Newf(errs.CodeInternal, "event: inbox row has unknown status %q", status)
}

func (i *Inbox) run(ctx context.Context, tx pgx.Tx, source, messageID string, schemaVersion int, payloadHash string, fn func(ctx context.Context, tx pgx.Tx) error) (Outcome, error) {
	if err := fn(ctx, tx); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, markProcessedSQL, source, messageID, i.clk.Now().UTC(), schemaVersion, nullable(payloadHash))
	if err != nil {
		return 0, errs.Wrap(err, errs.CodeInternal, "event: inbox mark processed failed")
	}
	if tag.RowsAffected() != 1 {
		return 0, errs.New(errs.CodeInternal, "event: inbox row vanished before mark processed")
	}
	return Processed, nil
}

// MarkFailed records that processing of (source, messageID) did not conclude,
// in q's own transaction or connection, so the failure is visible after the
// consumer's transaction rolled back. PROCESSED rows are never touched. The
// stored error is the client-safe code and detail of an *errs.Error, or the
// error text otherwise, bounded in length.
func (i *Inbox) MarkFailed(ctx context.Context, q db.Querier, source, messageID string, schemaVersion int, cause error) error {
	if err := validateInboxArgs(source, messageID, schemaVersion); err != nil {
		return err
	}
	if q == nil {
		return errs.New(errs.CodeInternal, "event: nil querier")
	}
	_, err := q.Exec(ctx, markFailedInboxSQL, source, messageID, schemaVersion, i.clk.Now().UTC(), safeErrorText(cause))
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "event: inbox mark failed")
	}
	return nil
}

// Get returns the stored row.
func (i *Inbox) Get(ctx context.Context, q db.Querier, source, messageID string) (Record, bool, error) {
	var (
		rec         Record
		processedAt *time.Time
		status      string
	)
	err := q.QueryRow(ctx, getInboxSQL, source, messageID).Scan(
		&rec.Source, &rec.MessageID, &rec.SchemaVersion, &rec.ReceivedAt, &processedAt, &status, &rec.Error, &rec.PayloadHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, errs.Wrap(err, errs.CodeInternal, "event: inbox get failed")
	}
	rec.ReceivedAt = rec.ReceivedAt.UTC()
	if processedAt != nil {
		t := processedAt.UTC()
		rec.ProcessedAt = &t
	}
	rec.Status = Status(status)
	return rec, true, nil
}

func validateInboxArgs(source, messageID string, schemaVersion int) error {
	fields := map[string]any{}
	fail := func(k, msg string) { fields[k] = msg }
	checkText(fail, "source", source, true, MaxInboxKeyLength)
	checkText(fail, "message_id", messageID, true, MaxInboxKeyLength)
	if schemaVersion < 1 {
		fail("schema_version", "must be >= 1")
	}
	if len(fields) == 0 {
		return nil
	}
	return errs.New(errs.CodeValidationFailed, "event: invalid inbox arguments").WithFields(fields)
}

func inProgress(cause error) error {
	return errs.Wrap(cause, errs.CodeIdempotencyInProgress, "event: message is being processed by another transaction").
		WithRetryAfter(InProgressRetryAfter)
}

func checkHash(source, messageID string, stored *string, got string) error {
	if stored == nil || *stored == "" || got == "" || *stored == got {
		return nil
	}
	return errs.New(errs.CodeConflict, "event: message id reused with a different payload").
		WithField("source", source).WithField("message_id", messageID)
}

// safeErrorText renders an error for storage: code and client-safe detail
// for *errs.Error, the text otherwise, bounded to maxLastError bytes.
func safeErrorText(err error) string {
	if err == nil {
		return "unknown error"
	}
	var s string
	if e, ok := errs.As(err); ok {
		s = string(e.Code)
		if e.Detail != "" {
			s += ": " + e.Detail
		}
	} else {
		s = err.Error()
	}
	if len(s) > maxLastError {
		s = s[:maxLastError]
	}
	return s
}
