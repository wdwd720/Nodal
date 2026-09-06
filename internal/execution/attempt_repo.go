package execution

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
)

// AttemptRepository persists execution_attempts. Attempts are evidence:
// rows are only ever added to, and every status change is an explicit
// transition of AttemptTransitions recorded on the outbox.
type AttemptRepository struct {
	clk   clock.Clock
	emit  EventEmitter
	audit audit.Writer
}

// NewAttemptRepository wires an AttemptRepository (see NewRepository).
func NewAttemptRepository(clk clock.Clock, emit EventEmitter, aud audit.Writer) *AttemptRepository {
	if clk == nil {
		clk = clock.System()
	}
	return &AttemptRepository{clk: clk, emit: emit, audit: aud}
}

const attemptColumns = `id, order_id, plan_id::text, attempt_no, wallet_id::text, provider, coalesce(provider_request_id,''), quote_id::text,
	unsigned_tx_hash, coalesce(unsigned_tx_ref,''), signed_tx_hash, coalesce(tx_signature,''), coalesce(recent_blockhash,''), last_valid_block_height,
	coalesce(simulation_ref,''), simulation_ok, inspection_result, coalesce(signing_decision_id::text,''), status, coalesce(finality,''),
	submitted_at, coalesce(submit_response_ref,''), observed_at, confirmed_at, finalized_at, coalesce(error,''), correlation_id, created_at, updated_at`

func scanAttempt(row pgx.Row) (Attempt, error) {
	var a Attempt
	if err := row.Scan(&a.ID, &a.OrderID, &a.PlanID, &a.AttemptNo, &a.WalletID, &a.Provider, &a.ProviderRequestID, &a.QuoteID,
		&a.UnsignedTxHash, &a.UnsignedTxRef, &a.SignedTxHash, &a.TxSignature, &a.RecentBlockhash, &a.LastValidBlockHeight,
		&a.SimulationRef, &a.SimulationOK, &a.InspectionResult, &a.SigningDecisionID, &a.Status, &a.Finality,
		&a.SubmittedAt, &a.SubmitResponseRef, &a.ObservedAt, &a.ConfirmedAt, &a.FinalizedAt, &a.Error, &a.CorrelationID, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return Attempt{}, err
	}
	return a, nil
}

// Create inserts a new attempt with attempt_no = max + 1 for the order,
// allocated under the order's row lock; UNIQUE (order_id, attempt_no) is
// the backstop. The status must be BUILT (an attempt exists once a
// transaction was built).
func (r *AttemptRepository) Create(ctx context.Context, tx pgx.Tx, a Attempt) (Attempt, error) {
	if err := r.check(); err != nil {
		return Attempt{}, err
	}
	if a.Status == "" {
		a.Status = AttemptBuilt
	}
	if a.Status != AttemptBuilt {
		return Attempt{}, errs.New(errs.CodeValidationFailed, "execution: new attempts start in BUILT").WithField("status", string(a.Status))
	}
	if err := a.Validate(); err != nil {
		return Attempt{}, err
	}
	o, err := lockOrder(ctx, tx, a.OrderID)
	if err != nil {
		return Attempt{}, err
	}
	if o.Status.Terminal() {
		return Attempt{}, errs.Newf(errs.CodeInvalidStateTransition, "order is %s; no new attempt may be built", o.Status).
			WithField("order_id", o.ID.String())
	}
	now := r.clk.Now()
	created, err := scanAttempt(tx.QueryRow(ctx, `INSERT INTO execution_attempts (id, order_id, plan_id, attempt_no, wallet_id, provider, provider_request_id, quote_id,
			unsigned_tx_hash, unsigned_tx_ref, recent_blockhash, last_valid_block_height, simulation_ref, simulation_ok, status, correlation_id, created_at, updated_at)
		VALUES ($1,$2,$3::uuid,(SELECT coalesce(max(attempt_no),0)+1 FROM execution_attempts WHERE order_id = $2),$4::uuid,$5,$6,$7::uuid,
			$8,$9,$10,$11,$12,$13,$14,$15,$16,$16)
		RETURNING `+attemptColumns,
		a.ID, a.OrderID, a.PlanID, a.WalletID, a.Provider, nullable(a.ProviderRequestID), a.QuoteID,
		a.UnsignedTxHash, nullable(a.UnsignedTxRef), nullable(a.RecentBlockhash), a.LastValidBlockHeight, nullable(a.SimulationRef), a.SimulationOK,
		string(a.Status), a.CorrelationID, now))
	if err != nil {
		if db.IsUniqueViolation(err) {
			return Attempt{}, errs.New(errs.CodeConflict, "execution: attempt number allocated concurrently").WithField("order_id", a.OrderID.String())
		}
		return Attempt{}, dbErr("insert attempt", err)
	}
	if err := r.emitAttempt(ctx, tx, created, "", created.Status, "attempt built", now); err != nil {
		return Attempt{}, err
	}
	payload := map[string]any{
		"attempt_id": created.ID.String(), "order_id": created.OrderID.String(), "plan_id": created.PlanID, "attempt_no": created.AttemptNo,
		"provider": created.Provider, "quote_id": created.QuoteID, "unsigned_tx_ref": created.UnsignedTxRef,
		"recent_blockhash": created.RecentBlockhash, "last_valid_block_height": created.LastValidBlockHeight,
	}
	if err := appendAudit(ctx, r.audit, tx, o.AccountID.String(), "", "", AuditAttemptCreated, "execution_attempt", created.ID.String(),
		"attempt built", created.UnsignedTxRef, created.CorrelationID, "", payload, now); err != nil {
		return Attempt{}, err
	}
	return created, nil
}

// Get returns an attempt.
func (r *AttemptRepository) Get(ctx context.Context, q db.Querier, attemptID AttemptID) (Attempt, error) {
	a, err := scanAttempt(q.QueryRow(ctx, `SELECT `+attemptColumns+` FROM execution_attempts WHERE id = $1`, attemptID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Attempt{}, errs.New(errs.CodeNotFound, "attempt not found").WithField("attempt_id", attemptID.String())
		}
		return Attempt{}, dbErr("get attempt", err)
	}
	return a, nil
}

// ListForOrder returns an order's attempts by attempt number.
func (r *AttemptRepository) ListForOrder(ctx context.Context, q db.Querier, orderID OrderID) ([]Attempt, error) {
	return r.list(ctx, q, `SELECT `+attemptColumns+` FROM execution_attempts WHERE order_id = $1 ORDER BY attempt_no`, orderID)
}

// ListRecoverable returns every attempt whose fate is not yet known
// (SUBMITTED, SUBMISSION_UNKNOWN, OBSERVED, CONFIRMED), oldest first, for
// the recovery worker. The partial index execution_attempts_unknown_idx
// serves it.
func (r *AttemptRepository) ListRecoverable(ctx context.Context, q db.Querier, limit int) ([]Attempt, error) {
	if limit <= 0 {
		limit = 100
	}
	return r.list(ctx, q, `SELECT `+attemptColumns+` FROM execution_attempts
		WHERE status IN ('SUBMITTED','SUBMISSION_UNKNOWN','OBSERVED','CONFIRMED') ORDER BY created_at, id LIMIT $1`, limit)
}

func (r *AttemptRepository) list(ctx context.Context, q db.Querier, sql string, args ...any) ([]Attempt, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, dbErr("list attempts", err)
	}
	defer rows.Close()
	var out []Attempt
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, dbErr("scan attempt", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, dbErr("list attempts", err)
	}
	return out, nil
}

// Update applies evidence fields and, when p.Status is set, a status
// transition checked against AttemptTransitions. The row is locked; the
// status update is compare-and-set on the status read under the lock.
func (r *AttemptRepository) Update(ctx context.Context, tx pgx.Tx, attemptID AttemptID, p AttemptPatch) (Attempt, error) {
	if err := r.check(); err != nil {
		return Attempt{}, err
	}
	a, err := scanAttempt(tx.QueryRow(ctx, `SELECT `+attemptColumns+` FROM execution_attempts WHERE id = $1 FOR UPDATE`, attemptID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Attempt{}, errs.New(errs.CodeNotFound, "attempt not found").WithField("attempt_id", attemptID.String())
		}
		return Attempt{}, dbErr("lock attempt", err)
	}
	from := a.Status
	to := from
	if p.Status != nil {
		to = *p.Status
		if !to.Valid() {
			return Attempt{}, errs.New(errs.CodeValidationFailed, "execution: unknown attempt status").WithField("status", string(to))
		}
		if to != from {
			if err := checkAttemptTransition(a.ID, from, to); err != nil {
				return Attempt{}, err
			}
		}
	}
	if p.Finality != nil && !p.Finality.Valid() {
		return Attempt{}, errs.New(errs.CodeValidationFailed, "execution: unknown finality level").WithField("finality", string(*p.Finality))
	}
	var finality *string
	if p.Finality != nil {
		s := string(*p.Finality)
		finality = &s
	}
	now := r.clk.Now()
	updated, err := scanAttempt(tx.QueryRow(ctx, `UPDATE execution_attempts SET
			status = $2,
			provider_request_id = coalesce($3, provider_request_id),
			unsigned_tx_hash = coalesce($4, unsigned_tx_hash),
			unsigned_tx_ref = coalesce($5, unsigned_tx_ref),
			recent_blockhash = coalesce($6, recent_blockhash),
			last_valid_block_height = coalesce($7, last_valid_block_height),
			simulation_ref = coalesce($8, simulation_ref),
			simulation_ok = coalesce($9, simulation_ok),
			inspection_result = coalesce($10, inspection_result),
			signing_decision_id = coalesce($11::uuid, signing_decision_id),
			finality = coalesce($12, finality),
			submitted_at = coalesce($13, submitted_at),
			submit_response_ref = coalesce($14, submit_response_ref),
			observed_at = coalesce($15, observed_at),
			confirmed_at = coalesce($16, confirmed_at),
			finalized_at = coalesce($17, finalized_at),
			error = coalesce($18, error)
		WHERE id = $1 AND status = $19 RETURNING `+attemptColumns,
		a.ID, string(to), p.ProviderRequestID, p.UnsignedTxHash, p.UnsignedTxRef, p.RecentBlockhash, p.LastValidBlockHeight,
		p.SimulationRef, p.SimulationOK, p.InspectionResult, p.SigningDecisionID, finality, p.SubmittedAt, p.SubmitResponseRef,
		p.ObservedAt, p.ConfirmedAt, p.FinalizedAt, p.Error, string(from)))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Attempt{}, errs.New(errs.CodeConflict, "execution: attempt changed concurrently").WithField("attempt_id", a.ID.String())
		}
		return Attempt{}, dbErr("update attempt", err)
	}
	if to != from {
		if err := r.emitAttempt(ctx, tx, updated, from, to, p.Reason, now); err != nil {
			return Attempt{}, err
		}
		o, err := scanOrder(tx.QueryRow(ctx, `SELECT `+orderColumns+` FROM orders WHERE id = $1`, updated.OrderID))
		if err != nil {
			return Attempt{}, dbErr("get order for attempt audit", err)
		}
		payload := map[string]any{
			"attempt_id": updated.ID.String(), "order_id": updated.OrderID.String(), "attempt_no": updated.AttemptNo,
			"from": string(from), "to": string(to), "tx_signature": updated.TxSignature, "finality": string(updated.Finality), "error": updated.Error,
		}
		if err := appendAudit(ctx, r.audit, tx, o.AccountID.String(), "", "", AuditAttemptTransition, "execution_attempt", updated.ID.String(),
			p.Reason, updated.SubmitResponseRef, updated.CorrelationID, "", payload, now); err != nil {
			return Attempt{}, err
		}
	}
	return updated, nil
}

// SetSignature records the signed transaction hash and its signature. The
// signature is unique across all attempts: the same signed bytes can never
// be attributed to two attempts. A repeat with the same values is a no-op;
// a different signature on an attempt that already has one is CONFLICT.
func (r *AttemptRepository) SetSignature(ctx context.Context, tx pgx.Tx, attemptID AttemptID, txSignature string, signedTxHash []byte) (Attempt, error) {
	if err := r.check(); err != nil {
		return Attempt{}, err
	}
	txSignature = strings.TrimSpace(txSignature)
	if txSignature == "" || len(signedTxHash) == 0 {
		return Attempt{}, errs.New(errs.CodeValidationFailed, "execution: signature and signed transaction hash are required")
	}
	a, err := scanAttempt(tx.QueryRow(ctx, `SELECT `+attemptColumns+` FROM execution_attempts WHERE id = $1 FOR UPDATE`, attemptID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Attempt{}, errs.New(errs.CodeNotFound, "attempt not found").WithField("attempt_id", attemptID.String())
		}
		return Attempt{}, dbErr("lock attempt", err)
	}
	if a.TxSignature != "" {
		if a.TxSignature == txSignature {
			return a, nil
		}
		return Attempt{}, errs.New(errs.CodeConflict, "execution: attempt already carries a different signature").
			WithField("attempt_id", a.ID.String())
	}
	updated, err := scanAttempt(tx.QueryRow(ctx, `UPDATE execution_attempts SET tx_signature = $2, signed_tx_hash = $3 WHERE id = $1 RETURNING `+attemptColumns,
		a.ID, txSignature, signedTxHash))
	if err != nil {
		if db.IsUniqueViolation(err) {
			return Attempt{}, errs.New(errs.CodeConflict, "execution: transaction signature already belongs to another attempt").
				WithField("tx_signature", txSignature)
		}
		return Attempt{}, dbErr("set attempt signature", err)
	}
	return updated, nil
}

// FindBySignature returns the attempt carrying a transaction signature.
func (r *AttemptRepository) FindBySignature(ctx context.Context, q db.Querier, txSignature string) (Attempt, error) {
	a, err := scanAttempt(q.QueryRow(ctx, `SELECT `+attemptColumns+` FROM execution_attempts WHERE tx_signature = $1`, txSignature))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Attempt{}, errs.New(errs.CodeNotFound, "attempt not found").WithField("tx_signature", txSignature)
		}
		return Attempt{}, dbErr("find attempt by signature", err)
	}
	return a, nil
}

func (r *AttemptRepository) emitAttempt(ctx context.Context, tx pgx.Tx, a Attempt, from, to AttemptStatus, reason string, now time.Time) error {
	env, err := envelope(event.TopicExecutionAttemptTransitioned, event.AggregateExecutionAttempt, a.ID.String(), a.CorrelationID, a.OrderID.String(), now,
		AttemptTransitionedEvent{
			AttemptID: a.ID.String(), OrderID: a.OrderID.String(), PlanID: a.PlanID, AttemptNo: a.AttemptNo, From: from, To: to,
			TxSignature: a.TxSignature, Finality: a.Finality, Reason: reason, OccurredAt: now,
		})
	if err != nil {
		return err
	}
	if err := r.emit.Enqueue(ctx, tx, string(event.TopicExecutionAttemptTransitioned), env); err != nil {
		return dbErr("enqueue attempt event", err)
	}
	return nil
}

func (r *AttemptRepository) check() error {
	if r.emit == nil {
		return errs.New(errs.CodeInternal, "execution: outbox emitter is not configured")
	}
	if r.audit == nil {
		return errs.New(errs.CodeInternal, "execution: audit writer is not configured")
	}
	return nil
}
