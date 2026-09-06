package withdrawal

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// Store is the persistence contract of the Service; Repository is the
// PostgreSQL implementation and tests may substitute an in-memory one.
type Store interface {
	Create(ctx context.Context, tx pgx.Tx, w Withdrawal, ev TransitionEvidence) (created Withdrawal, isNew bool, err error)
	Get(ctx context.Context, q db.Querier, id WithdrawalID) (Withdrawal, error)
	ListRecent(ctx context.Context, q db.Querier, accountID accounts.AccountID, assetID assets.AssetID, since time.Time) ([]Withdrawal, error)
	Transition(ctx context.Context, tx pgx.Tx, id WithdrawalID, to Status, ev TransitionEvidence) (Withdrawal, error)
}

// Repository persists withdrawals. Every change is audited on the account
// stream; there is no registered withdrawal outbox topic in V1 (the gate is
// DISABLED), so nothing is enqueued.
type Repository struct {
	clk   clock.Clock
	audit audit.Writer
}

var _ Store = (*Repository)(nil)

// NewRepository builds a Repository.
func NewRepository(clk clock.Clock, aud audit.Writer) (*Repository, error) {
	if clk == nil || aud == nil {
		return nil, errs.New(errs.CodeValidationFailed, "withdrawal: clock and audit writer are required")
	}
	return &Repository{clk: clk, audit: aud}, nil
}

const columns = `id, account_id, asset_id, quantity::text, destination_address, destination_validated, status, requested_by_user_id,
	step_up_verified_at, approval_id::text, capability_check_ref, reservation_id::text, tx_signature, journal_transaction_id,
	idempotency_key, correlation_id, created_at, updated_at`

func scan(row pgx.Row) (Withdrawal, error) {
	var (
		w                                               Withdrawal
		qty, status                                     string
		approval, capRef, reservation, sig, correlation *string
	)
	if err := row.Scan(&w.ID, &w.AccountID, &w.AssetID, &qty, &w.DestinationAddress, &w.DestinationValidated, &status, &w.RequestedByUserID,
		&w.StepUpVerifiedAt, &approval, &capRef, &reservation, &sig, &w.JournalTransactionID,
		&w.IdempotencyKey, &correlation, &w.CreatedAt, &w.UpdatedAt); err != nil {
		return Withdrawal{}, err
	}
	q, err := money.ParseQuantity(qty)
	if err != nil {
		return Withdrawal{}, err
	}
	w.Quantity, w.Status = q, Status(status)
	w.ApprovalID, w.CapabilityCheckRef, w.ReservationID = deref(approval), deref(capRef), deref(reservation)
	w.TxSignature, w.CorrelationID = deref(sig), deref(correlation)
	return w, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func dbErr(op string, err error) error {
	if db.IsRetryable(err) {
		return err
	}
	return errs.Wrap(err, errs.CodeInternal, "withdrawal: "+op)
}

// Create inserts a REQUESTED withdrawal, idempotent on IdempotencyKey.
func (r *Repository) Create(ctx context.Context, tx pgx.Tx, w Withdrawal, ev TransitionEvidence) (Withdrawal, bool, error) {
	if err := ev.Validate(); err != nil {
		return Withdrawal{}, false, err
	}
	if w.ID.IsZero() {
		w.ID = NewWithdrawalID()
	}
	now := r.clk.Now().UTC()
	tag, err := tx.Exec(ctx, `INSERT INTO withdrawals (id, account_id, asset_id, quantity, destination_address, destination_validated, status,
			requested_by_user_id, capability_check_ref, idempotency_key, correlation_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4::numeric, $5, $6, 'REQUESTED', $7, NULLIF($8, ''), $9, NULLIF($10, ''), $11, $11)
		ON CONFLICT (idempotency_key) DO NOTHING`,
		w.ID, w.AccountID, w.AssetID, w.Quantity.String(), w.DestinationAddress, w.DestinationValidated,
		w.RequestedByUserID, w.CapabilityCheckRef, w.IdempotencyKey, w.CorrelationID, now)
	if err != nil {
		return Withdrawal{}, false, dbErr("insert", err)
	}
	stored, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM withdrawals WHERE idempotency_key = $1`, w.IdempotencyKey))
	if err != nil {
		return Withdrawal{}, false, dbErr("read back", err)
	}
	if tag.RowsAffected() == 0 {
		if stored.AccountID != w.AccountID {
			return Withdrawal{}, false, errs.New(errs.CodeInvalidIdempotencyReuse, "withdrawal: idempotency key belongs to another account")
		}
		return stored, false, nil
	}
	if err := r.record(ctx, tx, stored, "withdrawal.requested", ev, now); err != nil {
		return Withdrawal{}, false, err
	}
	return stored, true, nil
}

// Get returns a withdrawal by id.
func (r *Repository) Get(ctx context.Context, q db.Querier, wid WithdrawalID) (Withdrawal, error) {
	w, err := scan(q.QueryRow(ctx, `SELECT `+columns+` FROM withdrawals WHERE id = $1`, wid))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Withdrawal{}, errs.New(errs.CodeNotFound, "withdrawal not found").WithField("withdrawal_id", wid.String())
		}
		return Withdrawal{}, dbErr("get", err)
	}
	return w, nil
}

// ListRecent returns the account's withdrawals of an asset created since a
// time, oldest first (velocity input).
func (r *Repository) ListRecent(ctx context.Context, q db.Querier, accountID accounts.AccountID, assetID assets.AssetID, since time.Time) ([]Withdrawal, error) {
	rows, err := q.Query(ctx, `SELECT `+columns+` FROM withdrawals WHERE account_id = $1 AND asset_id = $2 AND created_at >= $3 ORDER BY created_at, id`,
		accountID, assetID, since.UTC())
	if err != nil {
		return nil, dbErr("list recent", err)
	}
	defer rows.Close()
	var out []Withdrawal
	for rows.Next() {
		w, err := scan(rows)
		if err != nil {
			return nil, dbErr("scan", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// Transition moves a withdrawal under a row lock, writing the immutable
// withdrawal_transitions row (which migration 00603 requires) and an audit
// event. Illegal moves are INVALID_STATE_TRANSITION.
func (r *Repository) Transition(ctx context.Context, tx pgx.Tx, wid WithdrawalID, to Status, ev TransitionEvidence) (Withdrawal, error) {
	if err := ev.Validate(); err != nil {
		return Withdrawal{}, err
	}
	if !to.Valid() {
		return Withdrawal{}, errs.Newf(errs.CodeValidationFailed, "withdrawal: unknown status %q", to)
	}
	cur, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM withdrawals WHERE id = $1 FOR UPDATE`, wid))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Withdrawal{}, errs.New(errs.CodeNotFound, "withdrawal not found").WithField("withdrawal_id", wid.String())
		}
		return Withdrawal{}, dbErr("lock", err)
	}
	if !CanTransition(cur.Status, to) {
		return Withdrawal{}, errs.Newf(errs.CodeInvalidStateTransition, "withdrawal %s -> %s is not allowed", cur.Status, to).
			WithField("from", string(cur.Status)).WithField("to", string(to))
	}
	now := r.clk.Now().UTC()
	if _, err := tx.Exec(ctx, `INSERT INTO withdrawal_transitions (id, withdrawal_id, from_status, to_status, actor_type, actor_id, reason, evidence_ref, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), $9)`,
		NewTransitionID(), wid, cur.Status, to, string(ev.ActorType), ev.ActorID, ev.Reason, ev.EvidenceRef, now); err != nil {
		if db.IsCheckViolation(err) {
			return Withdrawal{}, errs.Wrap(err, errs.CodeForbidden, "withdrawal: actor type refused by the transitions table")
		}
		return Withdrawal{}, dbErr("insert transition", err)
	}
	updated, err := scan(tx.QueryRow(ctx, `UPDATE withdrawals SET status = $2,
			step_up_verified_at = CASE WHEN $2 = 'STEP_UP_VERIFIED' THEN $3 ELSE step_up_verified_at END
		WHERE id = $1 RETURNING `+columns, wid, to, now))
	if err != nil {
		return Withdrawal{}, dbErr("update", err)
	}
	if err := r.record(ctx, tx, updated, "withdrawal.transitioned", ev, now); err != nil {
		return Withdrawal{}, err
	}
	return updated, nil
}

func (r *Repository) record(ctx context.Context, tx pgx.Tx, w Withdrawal, action string, ev TransitionEvidence, now time.Time) error {
	payload, err := json.Marshal(map[string]any{
		"withdrawal_id": w.ID.String(), "account_id": w.AccountID.String(), "asset_id": w.AssetID.String(),
		"quantity": w.Quantity.String(), "status": string(w.Status), "destination_address": w.DestinationAddress,
		"reason": ev.Reason,
	})
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "withdrawal: encode audit payload")
	}
	_, err = r.audit.Append(ctx, tx, audit.Event{
		Stream: audit.AccountStream(w.AccountID.String()), ActorType: string(ev.ActorType), ActorID: ev.ActorID,
		Action: action, ResourceType: "withdrawal", ResourceID: w.ID.String(),
		RequestID: ev.RequestID, CorrelationID: ev.CorrelationID, Reason: ev.Reason, EvidenceRef: ev.EvidenceRef,
		Payload: payload, OccurredAt: now,
	})
	return err
}
