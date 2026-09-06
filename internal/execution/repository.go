package execution

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
)

// Repository persists orders, order transitions and fills. It holds no
// connection: every method runs on the caller's Querier or transaction, so an
// order transition, its transition row, its outbox event and its audit event
// commit together or not at all.
type Repository struct {
	clk   clock.Clock
	emit  EventEmitter
	audit audit.Writer
}

// NewRepository wires a Repository. Every dependency is mandatory: without
// the outbox a transition would be invisible downstream, without the audit
// writer it would be unaccounted for.
func NewRepository(clk clock.Clock, emit EventEmitter, aud audit.Writer) *Repository {
	if clk == nil {
		clk = clock.System()
	}
	return &Repository{clk: clk, emit: emit, audit: aud}
}

const orderColumns = `id, intent_id::text, plan_id::text, account_id, instrument_id::text, venue_listing_id::text, side, mode, status,
	input_asset_id, input_quantity::text, output_asset_id, min_output_quantity::text, filled_input_quantity::text, filled_output_quantity::text,
	reservation_id::text, quote_id::text, coalesce(rejection_code, ''), correlation_id, terminal_at, created_at, updated_at`

func scanOrder(row pgx.Row) (Order, error) {
	var o Order
	if err := row.Scan(&o.ID, &o.IntentID, &o.PlanID, &o.AccountID, &o.InstrumentID, &o.VenueListingID, &o.Side, &o.Mode, &o.Status,
		&o.InputAssetID, &o.InputQuantity, &o.OutputAssetID, &o.MinOutputQuantity, &o.FilledInputQuantity, &o.FilledOutputQuantity,
		&o.ReservationID, &o.QuoteID, &o.RejectionCode, &o.CorrelationID, &o.TerminalAt, &o.CreatedAt, &o.UpdatedAt); err != nil {
		return Order{}, err
	}
	return o, nil
}

func dbErr(op string, err error) error {
	if e, ok := errs.As(err); ok {
		return e
	}
	if db.IsRetryable(err) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return errs.Wrap(err, errs.CodeInternal, "execution: "+op)
}

// Create inserts a new order in status CREATED and records the creation on
// the outbox and the audit stream. The order's own status transitions then
// go through Transition.
func (r *Repository) Create(ctx context.Context, tx pgx.Tx, o Order) (Order, error) {
	if err := r.check(); err != nil {
		return Order{}, err
	}
	if o.Status == "" {
		o.Status = OrderCreated
	}
	if o.Status != OrderCreated {
		return Order{}, errs.New(errs.CodeValidationFailed, "execution: new orders start in CREATED").WithField("status", string(o.Status))
	}
	if err := o.Validate(); err != nil {
		return Order{}, err
	}
	now := r.clk.Now()
	if o.FilledInputQuantity.IsNegative() || o.FilledOutputQuantity.IsNegative() {
		return Order{}, errs.New(errs.CodeValidationFailed, "execution: filled quantities must not be negative")
	}
	row := tx.QueryRow(ctx, `INSERT INTO orders (id, intent_id, plan_id, account_id, instrument_id, venue_listing_id, side, mode, status,
			input_asset_id, input_quantity, output_asset_id, min_output_quantity, filled_input_quantity, filled_output_quantity,
			reservation_id, quote_id, rejection_code, correlation_id, created_at, updated_at)
		VALUES ($1,$2::uuid,$3::uuid,$4,$5::uuid,$6::uuid,$7,$8,$9,$10,$11::numeric,$12,$13::numeric,0,0,$14::uuid,$15::uuid,NULL,$16,$17,$17)
		RETURNING `+orderColumns,
		o.ID, o.IntentID, o.PlanID, o.AccountID, o.InstrumentID, o.VenueListingID, string(o.Side), string(o.Mode), string(o.Status),
		o.InputAssetID, o.InputQuantity.String(), o.OutputAssetID, o.MinOutputQuantity.String(),
		o.ReservationID, o.QuoteID, o.CorrelationID, now)
	created, err := scanOrder(row)
	if err != nil {
		if db.IsUniqueViolation(err) {
			return Order{}, errs.New(errs.CodeConflict, "execution: an order already exists for this intent").
				WithField("intent_id", o.IntentID)
		}
		return Order{}, dbErr("insert order", err)
	}
	if err := r.emitOrder(ctx, tx, created, "", OrderCreated, TransitionEvidence{Reason: "order created"}, "", now); err != nil {
		return Order{}, err
	}
	if err := appendAudit(ctx, r.audit, tx, created.AccountID.String(), "", "", AuditOrderCreated, "order", created.ID.String(),
		"order created", "", created.CorrelationID, "", orderAuditPayload(created), now); err != nil {
		return Order{}, err
	}
	return created, nil
}

// Get returns an order.
func (r *Repository) Get(ctx context.Context, q db.Querier, orderID OrderID) (Order, error) {
	o, err := scanOrder(q.QueryRow(ctx, `SELECT `+orderColumns+` FROM orders WHERE id = $1`, orderID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Order{}, errs.New(errs.CodeNotFound, "order not found").WithField("order_id", orderID.String())
		}
		return Order{}, dbErr("get order", err)
	}
	return o, nil
}

// GetByIntent returns the order of an intent (orders are unique per intent).
func (r *Repository) GetByIntent(ctx context.Context, q db.Querier, intentID string) (Order, error) {
	o, err := scanOrder(q.QueryRow(ctx, `SELECT `+orderColumns+` FROM orders WHERE intent_id = $1::uuid`, intentID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Order{}, errs.New(errs.CodeNotFound, "order not found").WithField("intent_id", intentID)
		}
		return Order{}, dbErr("get order by intent", err)
	}
	return o, nil
}

// GetByPlan returns the order executing a plan, if one exists yet.
func (r *Repository) GetByPlan(ctx context.Context, q db.Querier, planID string) (Order, error) {
	o, err := scanOrder(q.QueryRow(ctx, `SELECT `+orderColumns+` FROM orders WHERE plan_id = $1::uuid ORDER BY created_at DESC LIMIT 1`, planID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Order{}, errs.New(errs.CodeNotFound, "order not found").WithField("plan_id", planID)
		}
		return Order{}, dbErr("get order by plan", err)
	}
	return o, nil
}

func lockOrder(ctx context.Context, tx pgx.Tx, orderID OrderID) (Order, error) {
	o, err := scanOrder(tx.QueryRow(ctx, `SELECT `+orderColumns+` FROM orders WHERE id = $1 FOR UPDATE`, orderID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Order{}, errs.New(errs.CodeNotFound, "order not found").WithField("order_id", orderID.String())
		}
		return Order{}, dbErr("lock order", err)
	}
	return o, nil
}

// Transition moves an order to a new status. It locks the row, checks
// OrderTransitions, inserts the immutable order_transitions row (which the
// deferred trigger of migration 00603 requires), updates the order and
// enqueues the outbox and audit events, all in tx. REJECTED requires
// ev.RejectionCode.
func (r *Repository) Transition(ctx context.Context, tx pgx.Tx, orderID OrderID, to OrderStatus, ev TransitionEvidence) (Order, error) {
	if err := r.check(); err != nil {
		return Order{}, err
	}
	if !to.Valid() {
		return Order{}, errs.New(errs.CodeValidationFailed, "execution: unknown order status").WithField("to", string(to))
	}
	o, err := lockOrder(ctx, tx, orderID)
	if err != nil {
		return Order{}, err
	}
	return r.transitionLocked(ctx, tx, o, to, ev, "")
}

// transitionLocked applies a transition to an order already locked in tx.
func (r *Repository) transitionLocked(ctx context.Context, tx pgx.Tx, o Order, to OrderStatus, ev TransitionEvidence, causationID string) (Order, error) {
	if err := checkTransition(o.ID, o.Status, to); err != nil {
		return Order{}, err
	}
	if to == OrderRejected && strings.TrimSpace(ev.RejectionCode) == "" {
		return Order{}, errs.New(errs.CodeValidationFailed, "execution: REJECTED requires a rejection code").WithField("order_id", o.ID.String())
	}
	now := r.clk.Now()
	from := o.Status
	actorType, actorID := auditActor(ev.ActorType, ev.ActorID)
	tid := NewTransitionID()
	if _, err := tx.Exec(ctx, `INSERT INTO order_transitions (id, order_id, from_status, to_status, actor_type, actor_id, reason, evidence_ref, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		tid, o.ID, string(from), string(to), actorType, actorID, nullable(ev.Reason), nullable(ev.EvidenceRef), now); err != nil {
		return Order{}, dbErr("insert order transition", err)
	}
	var terminalAt *time.Time
	if to.Terminal() {
		t := now
		terminalAt = &t
	}
	rejection := nullable(ev.RejectionCode)
	if to != OrderRejected && ev.RejectionCode == "" && o.RejectionCode != "" {
		rejection = &o.RejectionCode
	}
	updated, err := scanOrder(tx.QueryRow(ctx, `UPDATE orders SET status = $2, rejection_code = $3, terminal_at = coalesce(terminal_at, $4)
		WHERE id = $1 AND status = $5 RETURNING `+orderColumns, o.ID, string(to), rejection, terminalAt, string(from)))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Order{}, errs.New(errs.CodeConflict, "execution: order changed concurrently").WithField("order_id", o.ID.String())
		}
		return Order{}, dbErr("update order status", err)
	}
	if err := r.emitOrder(ctx, tx, updated, from, to, ev, causationID, now); err != nil {
		return Order{}, err
	}
	payload := map[string]any{
		"order_id": updated.ID.String(), "from": string(from), "to": string(to), "transition_id": tid.String(),
		"rejection_code": ev.RejectionCode, "filled_input_quantity": updated.FilledInputQuantity.String(),
		"filled_output_quantity": updated.FilledOutputQuantity.String(), "plan_id": updated.PlanID, "intent_id": updated.IntentID,
	}
	if err := appendAudit(ctx, r.audit, tx, updated.AccountID.String(), ev.ActorType, ev.ActorID, AuditOrderTransition, "order", updated.ID.String(),
		ev.Reason, ev.EvidenceRef, updated.CorrelationID, ev.RequestID, payload, now); err != nil {
		return Order{}, err
	}
	return updated, nil
}

func (r *Repository) emitOrder(ctx context.Context, tx pgx.Tx, o Order, from, to OrderStatus, ev TransitionEvidence, causationID string, now time.Time) error {
	env, err := envelope(event.TopicOrderTransitioned, event.AggregateOrder, o.ID.String(), o.CorrelationID, causationID, now, OrderTransitionedEvent{
		OrderID: o.ID.String(), IntentID: o.IntentID, PlanID: o.PlanID, AccountID: o.AccountID.String(), From: from, To: to,
		Reason: ev.Reason, RejectionCode: ev.RejectionCode, EvidenceRef: ev.EvidenceRef,
		FilledInputQuantity: o.FilledInputQuantity, FilledOutputQuantity: o.FilledOutputQuantity, OccurredAt: now,
	})
	if err != nil {
		return err
	}
	if err := r.emit.Enqueue(ctx, tx, string(event.TopicOrderTransitioned), env); err != nil {
		return dbErr("enqueue order event", err)
	}
	return nil
}

func orderAuditPayload(o Order) map[string]any {
	return map[string]any{
		"order_id": o.ID.String(), "intent_id": o.IntentID, "plan_id": o.PlanID, "instrument_id": o.InstrumentID,
		"venue_listing_id": o.VenueListingID, "side": string(o.Side), "mode": string(o.Mode), "status": string(o.Status),
		"input_asset_id": o.InputAssetID.String(), "input_quantity": o.InputQuantity.String(),
		"output_asset_id": o.OutputAssetID.String(), "min_output_quantity": o.MinOutputQuantity.String(),
		"reservation_id": o.ReservationID, "quote_id": o.QuoteID,
	}
}

// ListTransitions returns an order's transitions oldest first.
func (r *Repository) ListTransitions(ctx context.Context, q db.Querier, orderID OrderID) ([]Transition, error) {
	rows, err := q.Query(ctx, `SELECT id, order_id, from_status, to_status, actor_type, actor_id, coalesce(reason,''), coalesce(evidence_ref,''), occurred_at
		FROM order_transitions WHERE order_id = $1 ORDER BY occurred_at, id`, orderID)
	if err != nil {
		return nil, dbErr("list order transitions", err)
	}
	defer rows.Close()
	var out []Transition
	for rows.Next() {
		var t Transition
		if err := rows.Scan(&t.ID, &t.OrderID, &t.From, &t.To, &t.ActorType, &t.ActorID, &t.Reason, &t.EvidenceRef, &t.OccurredAt); err != nil {
			return nil, dbErr("scan order transition", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, dbErr("list order transitions", err)
	}
	return out, nil
}

// --- fills ------------------------------------------------------------------

const fillColumns = `id, order_id, attempt_id, account_id, venue, external_fill_id, coalesce(tx_signature,''), slot,
	input_asset_id, input_quantity::text, output_asset_id, output_quantity::text,
	network_fee_quantity::text, network_fee_asset_id, venue_fee_quantity::text, venue_fee_asset_id, platform_fee_quantity::text, platform_fee_asset_id,
	effective_price_mantissa::text, effective_price_scale, source, finality, observed_at, received_at, coalesce(raw_ref,''),
	coalesce(journal_transaction_id::text,''), position_applied_at, created_at`

func scanFill(row pgx.Row) (Fill, error) {
	var f Fill
	if err := row.Scan(&f.ID, &f.OrderID, &f.AttemptID, &f.AccountID, &f.Venue, &f.ExternalFillID, &f.TxSignature, &f.Slot,
		&f.InputAssetID, &f.InputQuantity, &f.OutputAssetID, &f.OutputQuantity,
		&f.NetworkFeeQuantity, &f.NetworkFeeAssetID, &f.VenueFeeQuantity, &f.VenueFeeAssetID, &f.PlatformFeeQuantity, &f.PlatformFeeAssetID,
		&f.EffectivePriceMantissa, &f.EffectivePriceScale, &f.Source, &f.Finality, &f.ObservedAt, &f.ReceivedAt, &f.RawRef,
		&f.JournalTransactionID, &f.PositionAppliedAt, &f.CreatedAt); err != nil {
		return Fill{}, err
	}
	return f, nil
}

// RecordFill inserts a fill under UNIQUE (venue, external_fill_id) and
// applies it to the order's cumulative quantities and status (PART 228). A
// redelivery of an already-stored fill returns the stored row with Existing
// set and touches nothing else, so N deliveries produce exactly one economic
// effect. A fill arriving while the order is CANCEL_REQUESTED wins: the order
// moves to PARTIALLY_FILLED or FILLED (PART 227). A fill for an order in a
// status that does not accept fills, or one that would overfill the order,
// is refused with RECONCILIATION_REQUIRED and nothing is written; the caller
// opens a reconciliation record with the external event as evidence.
func (r *Repository) RecordFill(ctx context.Context, tx pgx.Tx, f Fill) (Fill, error) {
	if err := r.check(); err != nil {
		return Fill{}, err
	}
	if err := f.Validate(); err != nil {
		return Fill{}, err
	}
	if f.JournalTransactionID != "" || f.PositionAppliedAt != nil {
		return Fill{}, errs.New(errs.CodeValidationFailed, "execution: a new fill cannot carry posting or position markers")
	}
	now := r.clk.Now()

	// Replay check first, without touching the order: duplicates must be
	// cheap and must never block on the order row.
	existing, found, err := fillByExternalID(ctx, tx, f.Venue, f.ExternalFillID)
	if err != nil {
		return Fill{}, err
	}
	if found {
		if existing.OrderID != f.OrderID {
			return Fill{}, errs.New(errs.CodeConflict, "execution: external fill id already recorded for another order").
				WithField("venue", f.Venue).WithField("external_fill_id", f.ExternalFillID).WithField("order_id", existing.OrderID.String())
		}
		existing.Existing = true
		return existing, nil
	}

	o, err := lockOrder(ctx, tx, f.OrderID)
	if err != nil {
		return Fill{}, err
	}
	// A concurrent delivery may have committed between the replay check and
	// the lock; under the lock its row is visible.
	if existing, found, err := fillByExternalID(ctx, tx, f.Venue, f.ExternalFillID); err != nil {
		return Fill{}, err
	} else if found {
		if existing.OrderID != f.OrderID {
			return Fill{}, errs.New(errs.CodeConflict, "execution: external fill id already recorded for another order").
				WithField("venue", f.Venue).WithField("external_fill_id", f.ExternalFillID).WithField("order_id", existing.OrderID.String())
		}
		existing.Existing = true
		return existing, nil
	}
	if o.AccountID != f.AccountID {
		return Fill{}, errs.New(errs.CodeValidationFailed, "execution: fill account does not match the order").WithField("order_id", o.ID.String())
	}
	if o.InputAssetID != f.InputAssetID || o.OutputAssetID != f.OutputAssetID {
		return Fill{}, errs.New(errs.CodeReconciliationRequired, "execution: fill assets do not match the order").
			WithField("order_id", o.ID.String()).WithField("external_fill_id", f.ExternalFillID)
	}
	filledIn := o.FilledInputQuantity.Add(f.InputQuantity)
	filledOut := o.FilledOutputQuantity.Add(f.OutputQuantity)
	next, err := NextStatusAfterFill(o.Status, filledIn, o.InputQuantity)
	if err != nil {
		if errs.HasCode(err, errs.CodeInvalidStateTransition) {
			return Fill{}, errs.Wrap(err, errs.CodeReconciliationRequired, "execution: fill for an order that does not accept fills").
				WithField("order_id", o.ID.String()).WithField("status", string(o.Status))
		}
		return Fill{}, err
	}

	row := tx.QueryRow(ctx, `INSERT INTO fills (id, order_id, attempt_id, account_id, venue, external_fill_id, tx_signature, slot,
			input_asset_id, input_quantity, output_asset_id, output_quantity,
			network_fee_quantity, network_fee_asset_id, venue_fee_quantity, venue_fee_asset_id, platform_fee_quantity, platform_fee_asset_id,
			effective_price_mantissa, effective_price_scale, source, finality, observed_at, received_at, raw_ref)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::numeric,$11,$12::numeric,$13::numeric,$14,$15::numeric,$16,$17::numeric,$18,$19::numeric,$20,$21,$22,$23,$24,$25)
		ON CONFLICT (venue, external_fill_id) DO NOTHING
		RETURNING `+fillColumns,
		f.ID, f.OrderID, f.AttemptID, f.AccountID, f.Venue, f.ExternalFillID, nullable(f.TxSignature), f.Slot,
		f.InputAssetID, f.InputQuantity.String(), f.OutputAssetID, f.OutputQuantity.String(),
		f.NetworkFeeQuantity.String(), f.NetworkFeeAssetID, f.VenueFeeQuantity.String(), f.VenueFeeAssetID, f.PlatformFeeQuantity.String(), f.PlatformFeeAssetID,
		f.EffectivePriceMantissa.String(), f.EffectivePriceScale, string(f.Source), string(f.Finality), f.ObservedAt.UTC(), now, nullable(f.RawRef))
	inserted, err := scanFill(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Lost a race with a concurrent delivery of the same fill after
			// our replay check: the other transaction's row is the truth.
			existing, found, err := fillByExternalID(ctx, tx, f.Venue, f.ExternalFillID)
			if err != nil {
				return Fill{}, err
			}
			if !found {
				return Fill{}, errs.New(errs.CodeConflict, "execution: concurrent fill delivery").WithField("external_fill_id", f.ExternalFillID)
			}
			existing.Existing = true
			return existing, nil
		}
		return Fill{}, dbErr("insert fill", err)
	}

	if _, err := tx.Exec(ctx, `UPDATE orders SET filled_input_quantity = $2::numeric, filled_output_quantity = $3::numeric WHERE id = $1`,
		o.ID, filledIn.String(), filledOut.String()); err != nil {
		return Fill{}, dbErr("update order fills", err)
	}
	o.FilledInputQuantity, o.FilledOutputQuantity = filledIn, filledOut
	status := o.Status
	if next != o.Status {
		ev := TransitionEvidence{Reason: "fill " + f.ExternalFillID, EvidenceRef: f.RawRef, CausationID: inserted.ID.String()}
		updated, err := r.transitionLocked(ctx, tx, o, next, ev, inserted.ID.String())
		if err != nil {
			return Fill{}, err
		}
		status = updated.Status
	}
	env, err := envelope(event.TopicFillObserved, event.AggregateOrder, o.ID.String(), o.CorrelationID, "", now, FillObservedEvent{
		FillID: inserted.ID.String(), OrderID: o.ID.String(), AttemptID: attemptIDText(inserted.AttemptID), AccountID: o.AccountID.String(),
		Venue: inserted.Venue, ExternalFillID: inserted.ExternalFillID, TxSignature: inserted.TxSignature,
		InputAssetID: inserted.InputAssetID.String(), InputQuantity: inserted.InputQuantity,
		OutputAssetID: inserted.OutputAssetID.String(), OutputQuantity: inserted.OutputQuantity,
		Source: inserted.Source, Finality: inserted.Finality, OrderStatus: status, ObservedAt: inserted.ObservedAt, OccurredAt: now,
	})
	if err != nil {
		return Fill{}, err
	}
	if err := r.emit.Enqueue(ctx, tx, string(event.TopicFillObserved), env); err != nil {
		return Fill{}, dbErr("enqueue fill event", err)
	}
	payload := map[string]any{
		"fill_id": inserted.ID.String(), "order_id": o.ID.String(), "venue": inserted.Venue, "external_fill_id": inserted.ExternalFillID,
		"tx_signature": inserted.TxSignature, "input_quantity": inserted.InputQuantity.String(), "output_quantity": inserted.OutputQuantity.String(),
		"network_fee_quantity": inserted.NetworkFeeQuantity.String(), "platform_fee_quantity": inserted.PlatformFeeQuantity.String(),
		"source": string(inserted.Source), "finality": string(inserted.Finality), "order_status": string(status),
	}
	if err := appendAudit(ctx, r.audit, tx, o.AccountID.String(), "", "", AuditFillRecorded, "fill", inserted.ID.String(),
		"fill recorded", inserted.RawRef, o.CorrelationID, "", payload, now); err != nil {
		return Fill{}, err
	}
	return inserted, nil
}

func attemptIDText(a AttemptID) string {
	if a.IsZero() {
		return ""
	}
	return a.String()
}

func fillByExternalID(ctx context.Context, q db.Querier, venue, externalID string) (Fill, bool, error) {
	f, err := scanFill(q.QueryRow(ctx, `SELECT `+fillColumns+` FROM fills WHERE venue = $1 AND external_fill_id = $2`, venue, externalID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Fill{}, false, nil
		}
		return Fill{}, false, dbErr("get fill by external id", err)
	}
	return f, true, nil
}

// GetFill returns a fill.
func (r *Repository) GetFill(ctx context.Context, q db.Querier, fillID FillID) (Fill, error) {
	f, err := scanFill(q.QueryRow(ctx, `SELECT `+fillColumns+` FROM fills WHERE id = $1`, fillID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Fill{}, errs.New(errs.CodeNotFound, "fill not found").WithField("fill_id", fillID.String())
		}
		return Fill{}, dbErr("get fill", err)
	}
	return f, nil
}

// ListFills returns an order's fills in observation order.
func (r *Repository) ListFills(ctx context.Context, q db.Querier, orderID OrderID) ([]Fill, error) {
	rows, err := q.Query(ctx, `SELECT `+fillColumns+` FROM fills WHERE order_id = $1 ORDER BY observed_at, id`, orderID)
	if err != nil {
		return nil, dbErr("list fills", err)
	}
	defer rows.Close()
	var out []Fill
	for rows.Next() {
		f, err := scanFill(rows)
		if err != nil {
			return nil, dbErr("scan fill", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, dbErr("list fills", err)
	}
	return out, nil
}

// MarkFillPosted records the journal transaction that posted the fill. It
// is set-once: a repeat with the same transaction id is a no-op, a repeat
// with a different one is FILL_IMMUTABLE (the fills_guard trigger enforces
// the same rule).
func (r *Repository) MarkFillPosted(ctx context.Context, tx pgx.Tx, fillID FillID, journalTxID string) error {
	if err := r.check(); err != nil {
		return err
	}
	if journalTxID == "" {
		return errs.New(errs.CodeValidationFailed, "execution: journal transaction id required")
	}
	tag, err := tx.Exec(ctx, `UPDATE fills SET journal_transaction_id = $2::uuid WHERE id = $1 AND journal_transaction_id IS NULL`, fillID, journalTxID)
	if err != nil {
		return mapFillGuard(err, "mark fill posted")
	}
	if tag.RowsAffected() == 1 {
		f, err := r.GetFill(ctx, tx, fillID)
		if err != nil {
			return err
		}
		return appendAudit(ctx, r.audit, tx, f.AccountID.String(), "", "", AuditFillPosted, "fill", fillID.String(),
			"fill posted to ledger", journalTxID, "", "", map[string]any{"fill_id": fillID.String(), "journal_transaction_id": journalTxID}, r.clk.Now())
	}
	f, err := r.GetFill(ctx, tx, fillID)
	if err != nil {
		return err
	}
	if f.JournalTransactionID == journalTxID {
		return nil
	}
	return errs.New(errs.CodeFillImmutable, "execution: fill is already posted under a different journal transaction").
		WithField("fill_id", fillID.String()).WithField("journal_transaction_id", f.JournalTransactionID)
}

// MarkPositionApplied records that the position engine applied the fill.
// Set-once with the same semantics as MarkFillPosted.
func (r *Repository) MarkPositionApplied(ctx context.Context, tx pgx.Tx, fillID FillID, at time.Time) error {
	if err := r.check(); err != nil {
		return err
	}
	if at.IsZero() {
		at = r.clk.Now()
	}
	tag, err := tx.Exec(ctx, `UPDATE fills SET position_applied_at = $2 WHERE id = $1 AND position_applied_at IS NULL`, fillID, at.UTC())
	if err != nil {
		return mapFillGuard(err, "mark position applied")
	}
	if tag.RowsAffected() == 1 {
		f, err := r.GetFill(ctx, tx, fillID)
		if err != nil {
			return err
		}
		return appendAudit(ctx, r.audit, tx, f.AccountID.String(), "", "", AuditFillPositionApply, "fill", fillID.String(),
			"fill applied to position", "", "", "", map[string]any{"fill_id": fillID.String(), "position_applied_at": at.UTC()}, r.clk.Now())
	}
	f, err := r.GetFill(ctx, tx, fillID)
	if err != nil {
		return err
	}
	if f.PositionAppliedAt != nil {
		return nil
	}
	return errs.New(errs.CodeFillImmutable, "execution: position marker could not be set").WithField("fill_id", fillID.String())
}

// mapFillGuard maps the fills_guard trigger (SQLSTATE LG003 with a
// FILL_IMMUTABLE message) to FILL_IMMUTABLE.
func mapFillGuard(err error, op string) error {
	if db.SQLState(err) == "LG003" && strings.Contains(err.Error(), "FILL_IMMUTABLE") {
		return errs.Wrap(err, errs.CodeFillImmutable, "execution: fill is immutable")
	}
	return dbErr(op, err)
}

func (r *Repository) check() error {
	if r.emit == nil {
		return errs.New(errs.CodeInternal, "execution: outbox emitter is not configured")
	}
	if r.audit == nil {
		return errs.New(errs.CodeInternal, "execution: audit writer is not configured")
	}
	return nil
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ErrRequiresExternalConfirmation is the cause of the VALIDATION_FAILED
// error ConfirmCancelled returns without an external reference.
var ErrRequiresExternalConfirmation = fmt.Errorf("execution: CANCELLED requires external confirmation")
