package idempotency

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
)

// Status values stored in idempotency_keys.status.
type Status string

const (
	StatusInProgress Status = "IN_PROGRESS"
	StatusCompleted  Status = "COMPLETED"
	StatusFailed     Status = "FAILED"
)

// MaxKeyLength mirrors the table CHECK constraint.
const MaxKeyLength = 255

// Sentinel errors. Callers map them to errs codes (INVALID_IDEMPOTENCY_REUSE,
// IDEMPOTENCY_IN_PROGRESS, VALIDATION_FAILED) at the API boundary.
var (
	// ErrKeyReuseConflict: same key, different canonical request.
	ErrKeyReuseConflict = errors.New("idempotency: key already used with a different request")
	// ErrNotInProgress: Complete/Fail called for a key that is not IN_PROGRESS.
	ErrNotInProgress = errors.New("idempotency: key is not in progress")
	// ErrInvalidArgument: empty identifiers, oversized key, non-positive TTL or invalid JSON body.
	ErrInvalidArgument = errors.New("idempotency: invalid argument")
	// ErrInconsistent: the row vanished between insert and re-read (concurrent
	// cleanup); the caller should retry the transaction.
	ErrInconsistent = errors.New("idempotency: key row disappeared during Begin")
)

// Begun is the sealed result of Begin: exactly one of Acquired, Replay or
// InProgress.
type Begun interface{ begun() }

// Acquired means this request owns the key and must run the command, then call
// Complete or Fail.
type Acquired struct {
	ExpiresAt time.Time
}

// Replay means the same request already completed; return the stored result.
type Replay struct {
	ResponseStatus int
	ResourceType   string
	ResourceID     string
	ResponseBody   json.RawMessage // nil when none was stored
	CompletedAt    time.Time
}

// InProgress means the same request is running elsewhere; respond 409 with
// Retry-After and let the client retry.
type InProgress struct {
	StartedAt time.Time
	ExpiresAt time.Time
}

func (Acquired) begun()   {}
func (Replay) begun()     {}
func (InProgress) begun() {}

// Record is the stored row, for inspection and tests.
type Record struct {
	ActorID        string
	Endpoint       string
	Key            string
	RequestHash    string
	Status         Status
	ResponseStatus *int
	ResourceType   *string
	ResourceID     *string
	ResponseBody   json.RawMessage
	CreatedAt      time.Time
	CompletedAt    *time.Time
	ExpiresAt      time.Time
}

// Store implements the contract. It holds no connection: every method takes
// the caller's transaction (Begin/Complete/Fail) or a Querier (Get).
type Store struct {
	now func() time.Time
}

// NewStore takes the clock used for created_at/expires_at and expiry checks
// (the integrator wires clock.Clock.Now). nil means time.Now in UTC.
func NewStore(now func() time.Time) *Store {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Store{now: func() time.Time { return now().UTC() }}
}

// Begin claims the key for this request inside tx. See package doc for the
// four outcomes. It must run inside a transaction: the re-read locks the row.
func (s *Store) Begin(ctx context.Context, tx pgx.Tx, actorID, endpoint, key, requestHash string, ttl time.Duration) (Begun, error) {
	if err := validateIdentity(actorID, endpoint, key); err != nil {
		return nil, err
	}
	if requestHash == "" {
		return nil, fmt.Errorf("%w: request hash is empty", ErrInvalidArgument)
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("%w: ttl must be positive", ErrInvalidArgument)
	}
	if tx == nil {
		return nil, fmt.Errorf("%w: nil transaction", ErrInvalidArgument)
	}

	// A concurrent cleanup could delete the row between the insert and the
	// re-read; that is the only path to the loop's second iteration.
	for attempt := 0; attempt < 3; attempt++ {
		now := s.now()
		expiresAt := now.Add(ttl)

		tag, err := tx.Exec(ctx, `
			INSERT INTO idempotency_keys (actor_id, endpoint, key, request_hash, status, created_at, expires_at)
			VALUES ($1, $2, $3, $4, 'IN_PROGRESS', $5, $6)
			ON CONFLICT (actor_id, endpoint, key) DO NOTHING`,
			actorID, endpoint, key, requestHash, now, expiresAt)
		if err != nil {
			return nil, fmt.Errorf("idempotency: insert: %w", err)
		}
		if tag.RowsAffected() == 1 {
			return Acquired{ExpiresAt: expiresAt}, nil
		}

		var (
			existingHash   string
			status         Status
			responseStatus *int
			resourceType   *string
			resourceID     *string
			body           []byte
			createdAt      time.Time
			completedAt    *time.Time
			rowExpiresAt   time.Time
		)
		err = tx.QueryRow(ctx, `
			SELECT request_hash, status, response_status, resource_type, resource_id, response_body, created_at, completed_at, expires_at
			FROM idempotency_keys
			WHERE actor_id = $1 AND endpoint = $2 AND key = $3
			FOR UPDATE`,
			actorID, endpoint, key).
			Scan(&existingHash, &status, &responseStatus, &resourceType, &resourceID, &body, &createdAt, &completedAt, &rowExpiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("idempotency: read: %w", err)
		}

		// Expired rows are indistinguishable from deleted ones.
		if !rowExpiresAt.After(now) {
			return s.reacquire(ctx, tx, actorID, endpoint, key, requestHash, now, expiresAt)
		}
		if existingHash != requestHash {
			return nil, fmt.Errorf("%w: endpoint=%s", ErrKeyReuseConflict, endpoint)
		}
		switch status {
		case StatusCompleted:
			r := Replay{ResourceType: deref(resourceType), ResourceID: deref(resourceID)}
			if responseStatus != nil {
				r.ResponseStatus = *responseStatus
			}
			if len(body) > 0 {
				r.ResponseBody = json.RawMessage(body)
			}
			if completedAt != nil {
				r.CompletedAt = completedAt.UTC()
			}
			return r, nil
		case StatusInProgress:
			return InProgress{StartedAt: createdAt.UTC(), ExpiresAt: rowExpiresAt.UTC()}, nil
		case StatusFailed:
			return s.reacquire(ctx, tx, actorID, endpoint, key, requestHash, now, expiresAt)
		default:
			return nil, fmt.Errorf("idempotency: unknown status %q", status)
		}
	}
	return nil, ErrInconsistent
}

func (s *Store) reacquire(ctx context.Context, tx pgx.Tx, actorID, endpoint, key, requestHash string, now, expiresAt time.Time) (Begun, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE idempotency_keys
		SET request_hash = $4, status = 'IN_PROGRESS', response_status = NULL, resource_type = NULL,
		    resource_id = NULL, response_body = NULL, completed_at = NULL, created_at = $5, expires_at = $6
		WHERE actor_id = $1 AND endpoint = $2 AND key = $3`,
		actorID, endpoint, key, requestHash, now, expiresAt)
	if err != nil {
		return nil, fmt.Errorf("idempotency: re-acquire: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return nil, ErrInconsistent
	}
	return Acquired{ExpiresAt: expiresAt}, nil
}

// Complete records the concluded result for an IN_PROGRESS key. body must be
// valid JSON or empty (stored as NULL).
func (s *Store) Complete(ctx context.Context, tx pgx.Tx, actorID, endpoint, key string, responseStatus int, resourceType, resourceID string, body []byte) error {
	if err := validateIdentity(actorID, endpoint, key); err != nil {
		return err
	}
	if tx == nil {
		return fmt.Errorf("%w: nil transaction", ErrInvalidArgument)
	}
	jsonBody, err := bodyParam(body)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE idempotency_keys
		SET status = 'COMPLETED', response_status = $4, resource_type = NULLIF($5, ''), resource_id = NULLIF($6, ''),
		    response_body = $7, completed_at = $8
		WHERE actor_id = $1 AND endpoint = $2 AND key = $3 AND status = 'IN_PROGRESS'`,
		actorID, endpoint, key, responseStatus, resourceType, resourceID, jsonBody, s.now())
	if err != nil {
		return fmt.Errorf("idempotency: complete: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrNotInProgress
	}
	return nil
}

// Fail records that the command did not conclude. The key becomes
// re-acquirable by a retry carrying the same request hash.
func (s *Store) Fail(ctx context.Context, tx pgx.Tx, actorID, endpoint, key string, responseStatus int, body []byte) error {
	if err := validateIdentity(actorID, endpoint, key); err != nil {
		return err
	}
	if tx == nil {
		return fmt.Errorf("%w: nil transaction", ErrInvalidArgument)
	}
	jsonBody, err := bodyParam(body)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE idempotency_keys
		SET status = 'FAILED', response_status = $4, response_body = $5, completed_at = $6
		WHERE actor_id = $1 AND endpoint = $2 AND key = $3 AND status = 'IN_PROGRESS'`,
		actorID, endpoint, key, responseStatus, jsonBody, s.now())
	if err != nil {
		return fmt.Errorf("idempotency: fail: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrNotInProgress
	}
	return nil
}

// Get reads the stored row. found is false when the key is unknown.
func (s *Store) Get(ctx context.Context, q db.Querier, actorID, endpoint, key string) (rec Record, found bool, err error) {
	if err := validateIdentity(actorID, endpoint, key); err != nil {
		return Record{}, false, err
	}
	var body []byte
	err = q.QueryRow(ctx, `
		SELECT actor_id, endpoint, key, request_hash, status, response_status, resource_type, resource_id,
		       response_body, created_at, completed_at, expires_at
		FROM idempotency_keys WHERE actor_id = $1 AND endpoint = $2 AND key = $3`,
		actorID, endpoint, key).
		Scan(&rec.ActorID, &rec.Endpoint, &rec.Key, &rec.RequestHash, &rec.Status, &rec.ResponseStatus,
			&rec.ResourceType, &rec.ResourceID, &body, &rec.CreatedAt, &rec.CompletedAt, &rec.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, fmt.Errorf("idempotency: get: %w", err)
	}
	if len(body) > 0 {
		rec.ResponseBody = json.RawMessage(body)
	}
	rec.CreatedAt = rec.CreatedAt.UTC()
	rec.ExpiresAt = rec.ExpiresAt.UTC()
	if rec.CompletedAt != nil {
		t := rec.CompletedAt.UTC()
		rec.CompletedAt = &t
	}
	return rec, true, nil
}

func validateIdentity(actorID, endpoint, key string) error {
	switch {
	case actorID == "":
		return fmt.Errorf("%w: actor id is empty", ErrInvalidArgument)
	case endpoint == "":
		return fmt.Errorf("%w: endpoint is empty", ErrInvalidArgument)
	case key == "":
		return fmt.Errorf("%w: key is empty", ErrInvalidArgument)
	case len(key) > MaxKeyLength:
		return fmt.Errorf("%w: key exceeds %d bytes", ErrInvalidArgument, MaxKeyLength)
	}
	return nil
}

// bodyParam converts a response body into the jsonb parameter: NULL for
// empty, the raw JSON otherwise. Invalid JSON is rejected before it can
// poison a replay.
func bodyParam(body []byte) (any, error) {
	if len(body) == 0 {
		return nil, nil
	}
	if !json.Valid(body) {
		return nil, fmt.Errorf("%w: response body is not valid JSON", ErrInvalidArgument)
	}
	return json.RawMessage(body), nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
