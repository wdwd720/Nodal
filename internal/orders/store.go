package orders

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/matching"
)

// orderRow is a book_orders row.
type orderRow struct {
	ID                  OrderID
	BookID              BookID
	InstrumentID        instruments.InstrumentID
	OwnerType           OwnerType
	OwnerID             string
	AccountID           *accounts.AccountID
	HouseID             *econ.HouseAccountID
	IntentID            *econ.IntentID
	IdempotencyKey      string
	RequestHash         []byte
	ClientOrderID       string
	Side                Side
	Type                OrderType
	TIF                 TimeInForce
	STP                 STPMode
	Liquidity           LiquidityIntent
	PostOnly            bool
	ReduceOnly          bool
	StrategyOriginated  bool
	Mode                gates.Mode
	PriceTicks          int64
	QtyLots             int64
	FilledLots          int64
	RemainingLots       int64
	CancelledLots       int64
	ReduceOnlyLimitLots int64
	Status              Status
	RejectReason        string
	CancelReason        string
	ReservationID       string
	AcceptedCommandSeq  *int64
	LastCommandSeq      *int64
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

const orderColumns = `id, book_id, instrument_id, owner_type, owner_id::text, account_id, house_account_id, intent_id,
	idempotency_key, request_hash, coalesce(client_order_id, ''), side, order_type, time_in_force, stp_mode,
	liquidity_intent, post_only, reduce_only, strategy_originated, mode, price_ticks, qty_lots, filled_lots, remaining_lots,
	cancelled_lots, reduce_only_limit_lots, status, coalesce(reject_reason, ''), coalesce(cancel_reason, ''),
	coalesce(reservation_id::text, ''), accepted_command_seq, last_command_seq, created_at, updated_at`

func scanOrder(row pgx.Row) (orderRow, error) {
	var o orderRow
	err := row.Scan(&o.ID, &o.BookID, &o.InstrumentID, &o.OwnerType, &o.OwnerID, &o.AccountID, &o.HouseID, &o.IntentID,
		&o.IdempotencyKey, &o.RequestHash, &o.ClientOrderID, &o.Side, &o.Type, &o.TIF, &o.STP, &o.Liquidity,
		&o.PostOnly, &o.ReduceOnly, &o.StrategyOriginated, &o.Mode, &o.PriceTicks, &o.QtyLots, &o.FilledLots, &o.RemainingLots, &o.CancelledLots,
		&o.ReduceOnlyLimitLots, &o.Status, &o.RejectReason, &o.CancelReason, &o.ReservationID, &o.AcceptedCommandSeq,
		&o.LastCommandSeq, &o.CreatedAt, &o.UpdatedAt)
	if err != nil {
		return orderRow{}, err
	}
	o.CreatedAt, o.UpdatedAt = o.CreatedAt.UTC(), o.UpdatedAt.UTC()
	return o, nil
}

// loadOrder reads one order, optionally locking it.
func loadOrder(ctx context.Context, q db.Querier, oid OrderID, lock bool) (orderRow, error) {
	sql := `SELECT ` + orderColumns + ` FROM book_orders WHERE id = $1`
	if lock {
		sql += bookLock
	}
	o, err := scanOrder(q.QueryRow(ctx, sql, oid))
	if errors.Is(err, pgx.ErrNoRows) {
		return orderRow{}, errs.New(errs.CodeNotFound, "orders: order not found").WithField("order_id", oid.String())
	}
	if err != nil {
		return orderRow{}, dbErr("read order", err)
	}
	return o, nil
}

// lockOrders locks and returns the orders with these ids, keyed by id.
func lockOrders(ctx context.Context, tx pgx.Tx, ids []string) (map[string]orderRow, error) {
	out := make(map[string]orderRow, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `SELECT `+orderColumns+` FROM book_orders WHERE id = ANY($1::uuid[]) ORDER BY id`+bookLock, ids)
	if err != nil {
		return nil, dbErr("lock orders", err)
	}
	defer rows.Close()
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, dbErr("scan order", err)
		}
		out[o.ID.String()] = o
	}
	return out, rows.Err()
}

// orderByKey returns the order an owner submitted under an idempotency key.
func orderByKey(ctx context.Context, q db.Querier, ownerID, key string) (orderRow, bool, error) {
	o, err := scanOrder(q.QueryRow(ctx, `SELECT `+orderColumns+` FROM book_orders WHERE owner_id = $1::uuid AND idempotency_key = $2`, ownerID, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return orderRow{}, false, nil
	}
	if err != nil {
		return orderRow{}, false, dbErr("read order by key", err)
	}
	return o, true, nil
}

// insertOrder writes an order's birth row. A birth needs no transition row:
// only a change of status does.
func insertOrder(ctx context.Context, q db.Querier, o orderRow) error {
	var clientID any
	if o.ClientOrderID != "" {
		clientID = o.ClientOrderID
	}
	var reservation any
	if o.ReservationID != "" {
		reservation = o.ReservationID
	}
	_, err := q.Exec(ctx, `INSERT INTO book_orders (id, book_id, instrument_id, owner_type, owner_id, account_id, house_account_id,
		intent_id, idempotency_key, request_hash, client_order_id, side, order_type, time_in_force, stp_mode, liquidity_intent,
		post_only, reduce_only, strategy_originated, mode, price_ticks, qty_lots, filled_lots, remaining_lots, cancelled_lots,
		reduce_only_limit_lots, status, reject_reason, cancel_reason, reservation_id, accepted_command_seq, last_command_seq)
		VALUES ($1,$2,$3,$4,$5::uuid,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,
		        NULLIF($28,''),NULLIF($29,''),$30::uuid,$31,$32)`,
		o.ID, o.BookID, o.InstrumentID, o.OwnerType, o.OwnerID, o.AccountID, o.HouseID, o.IntentID, o.IdempotencyKey,
		o.RequestHash, clientID, o.Side, o.Type, o.TIF, o.STP, o.Liquidity, o.PostOnly, o.ReduceOnly, o.StrategyOriginated,
		o.Mode, o.PriceTicks, o.QtyLots, o.FilledLots, o.RemainingLots, o.CancelledLots, o.ReduceOnlyLimitLots, o.Status,
		o.RejectReason, o.CancelReason, reservation, o.AcceptedCommandSeq, o.LastCommandSeq)
	if err != nil {
		return dbErr("insert order", err)
	}
	return nil
}

// updateOrder writes an order's new state. A change of status writes its
// transition row first, naming both endpoints, in the same transaction (the
// database refuses the change at COMMIT otherwise, AU001).
func updateOrder(ctx context.Context, tx pgx.Tx, before, after orderRow, commandSeq *int64, actor econ.Actor, reason string, at time.Time) error {
	if before.Status != after.Status {
		if !CanTransition(before.Status, after.Status) {
			return errs.Newf(errs.CodeInvalidStateTransition, "orders: an order cannot move %s -> %s", before.Status, after.Status).
				WithField("order_id", before.ID.String())
		}
		if _, err := tx.Exec(ctx, `INSERT INTO book_order_transitions (id, order_id, from_status, to_status, command_seq, reason,
			actor_type, actor_id, occurred_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			id.New[id.Any](), before.ID, before.Status, after.Status, commandSeq, reasonOr(reason, string(after.Status)),
			actor.Type, actor.ID, at); err != nil {
			return dbErr("record order transition", err)
		}
	}
	if sameState(before, after) {
		return nil
	}
	var reservation any
	if after.ReservationID != "" {
		reservation = after.ReservationID
	}
	if _, err := tx.Exec(ctx, `UPDATE book_orders SET status = $2, price_ticks = $3, qty_lots = $4, filled_lots = $5,
		remaining_lots = $6, cancelled_lots = $7, reduce_only_limit_lots = $8, reject_reason = NULLIF($9,''),
		cancel_reason = NULLIF($10,''), reservation_id = $11::uuid, accepted_command_seq = $12, last_command_seq = $13
		WHERE id = $1`,
		after.ID, after.Status, after.PriceTicks, after.QtyLots, after.FilledLots, after.RemainingLots, after.CancelledLots,
		after.ReduceOnlyLimitLots, after.RejectReason, after.CancelReason, reservation, after.AcceptedCommandSeq,
		after.LastCommandSeq); err != nil {
		return dbErr("update order", err)
	}
	return nil
}

func sameState(a, b orderRow) bool {
	return a.Status == b.Status && a.PriceTicks == b.PriceTicks && a.QtyLots == b.QtyLots && a.FilledLots == b.FilledLots &&
		a.RemainingLots == b.RemainingLots && a.CancelledLots == b.CancelledLots &&
		a.ReduceOnlyLimitLots == b.ReduceOnlyLimitLots && a.RejectReason == b.RejectReason &&
		a.CancelReason == b.CancelReason && a.ReservationID == b.ReservationID &&
		eqSeq(a.AcceptedCommandSeq, b.AcceptedCommandSeq) && eqSeq(a.LastCommandSeq, b.LastCommandSeq)
}

func eqSeq(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// withEngineState returns o updated to the engine's record of it. A rejected
// order keeps the price and quantity it asked for: the engine keeps nothing
// of a rejected order but its reason.
func withEngineState(o orderRow, st matching.OrderState, seq int64) orderRow {
	s := seq
	o.LastCommandSeq = &s
	if st.Status == matching.StatusRejected {
		o.Status = StatusRejected
		o.RejectReason = string(st.RejectReason)
		o.FilledLots, o.RemainingLots, o.CancelledLots = 0, 0, 0
		return o
	}
	o.Status = statusOf(st.Status)
	o.PriceTicks = st.PriceTicks
	o.QtyLots = st.QtyLots
	o.FilledLots = st.FilledLots
	o.RemainingLots = st.RemainingLots
	o.CancelledLots = st.CancelledLots
	o.ReduceOnlyLimitLots = st.ReduceOnlyLimitLots
	o.CancelReason = string(st.CancelReason)
	if o.AcceptedCommandSeq == nil {
		a := seq
		o.AcceptedCommandSeq = &a
	}
	return o
}

func reasonOr(reason, fallback string) string {
	if reason == "" {
		return fallback
	}
	return reason
}

func int64p(v int64) *int64 { return &v }
