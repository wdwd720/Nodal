package capital

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/observability"
)

// Reserver is the fixed reservation contract (FINANCIAL_MODEL §7). Every
// method runs inside the caller's transaction so intent state, reservation
// state and the outbox event commit together or not at all.
type Reserver interface {
	// Reserve sets aside r.Quantity of r.AssetID for r.AccountID (and
	// r.USDMinor of the envelope budget when r.EnvelopeID is set). It fails
	// with INSUFFICIENT_BUYING_POWER on any shortfall and is idempotent on
	// r.IdempotencyKey: a replay returns the existing reservation, a reuse
	// with different economics fails with INVALID_IDEMPOTENCY_REUSE.
	Reserve(ctx context.Context, tx pgx.Tx, r ReserveRequest) (Reservation, error)
	// Consume records qty / usdMinor as used by orderID. It is legal only
	// from ACTIVE; when the whole quantity is consumed the reservation
	// becomes CONSUMED. Use ConsumeFinal to close a partially filled
	// reservation and hand the remainder back.
	Consume(ctx context.Context, tx pgx.Tx, id ReservationID, qty money.Quantity, usdMinor int64, orderID string) (Reservation, error)
	// Release moves an ACTIVE reservation to RELEASED and returns the
	// unconsumed remainder to the account (and envelope).
	Release(ctx context.Context, tx pgx.Tx, id ReservationID, reason string) (Reservation, error)
	// LockForOrder binds an ACTIVE reservation to an order so expiry leaves
	// it alone until the order's fate is known.
	LockForOrder(ctx context.Context, tx pgx.Tx, id ReservationID, orderID string) error
	// ExpireDue moves up to limit ACTIVE, unlocked reservations whose
	// expires_at ≤ now to EXPIRED with release semantics and returns them.
	ExpireDue(ctx context.Context, tx pgx.Tx, now time.Time, limit int) ([]ReservationID, error)
}

// Expiry batch bounds for ExpireDue.
const (
	DefaultExpireBatch = 100
	MaxExpireBatch     = 1000
)

// Service implements Reserver against PostgreSQL. It holds no state beyond
// its dependencies; all truth is in the database.
type Service struct {
	clk  clock.Clock
	emit Emitter
}

var _ Reserver = (*Service)(nil)

// NewService wires a Service. Both dependencies are mandatory: a nil
// Emitter would silently drop the outbox row that downstream consumers
// (buying-power cache invalidation, intent state) depend on.
func NewService(clk clock.Clock, em Emitter) *Service {
	if clk == nil {
		panic("capital: NewService: nil clock")
	}
	if em == nil {
		panic("capital: NewService: nil emitter")
	}
	return &Service{clk: clk, emit: em}
}

// Reserve implements Reserver (see the package documentation for the
// locking order: totals row, then envelope row).
func (s *Service) Reserve(ctx context.Context, tx pgx.Tx, r ReserveRequest) (Reservation, error) {
	if err := r.Validate(); err != nil {
		return Reservation{}, err
	}
	account, err := accounts.ParseAccountID(r.AccountID)
	if err != nil {
		return Reservation{}, errs.Wrap(err, errs.CodeValidationFailed, "account_id must be a canonical uuid")
	}
	usd := money.USDFromMinor(r.USDMinor)
	now := s.clk.Now()

	// Replay without taking any lock.
	if existing, found, err := reservationByKey(ctx, tx, r.IdempotencyKey); err != nil {
		return Reservation{}, err
	} else if found {
		return replayReservation(existing, r, account, usd)
	}
	if err := guardAccountAndAsset(ctx, tx, account, r.AssetID); err != nil {
		return Reservation{}, err
	}

	// 1. Serialize on the (account, asset) totals row.
	totals, err := lockTotals(ctx, tx, account, r.AssetID)
	if err != nil {
		return Reservation{}, err
	}
	// 2. available = WALLET balance − reserved − Σ active holds.
	balance, err := walletBalance(ctx, tx, account, r.AssetID)
	if err != nil {
		return Reservation{}, err
	}
	held, err := sumActiveHolds(ctx, tx, account, r.AssetID, now)
	if err != nil {
		return Reservation{}, err
	}
	available := balance.Sub(totals.Reserved).Sub(held)
	if available.Cmp(r.Quantity) < 0 {
		return Reservation{}, errs.New(errs.CodeInsufficientBuyingPower, "insufficient available quantity").
			WithField("account_id", account.String()).
			WithField("asset_id", r.AssetID.String()).
			WithField("available", available.Max(money.Quantity{}).String()).
			WithField("requested", r.Quantity.String()).
			WithField("wallet_balance", balance.String()).
			WithField("reserved", totals.Reserved.String()).
			WithField("held", held.String())
	}

	// 3. Envelope budget, under its own row lock.
	var env *Envelope
	if r.EnvelopeID != nil {
		e, err := getEnvelope(ctx, tx, *r.EnvelopeID, true)
		if err != nil {
			return Reservation{}, err
		}
		if e.AccountID != account {
			return Reservation{}, errs.New(errs.CodeValidationFailed, "envelope belongs to a different account").
				WithField("envelope_id", e.ID.String())
		}
		if !e.UsableAt(now) {
			return Reservation{}, errs.Newf(errs.CodeInsufficientBuyingPower, "envelope is %s and cannot back new reservations", e.Status).
				WithField("envelope_id", e.ID.String()).
				WithField("envelope_status", string(e.Status)).
				WithField("effective_at", timeValue(&e.EffectiveAt)).
				WithField("expires_at", timeValue(e.ExpiresAt))
		}
		if e.Available.Cmp(usd) < 0 {
			return Reservation{}, errs.New(errs.CodeInsufficientBuyingPower, "insufficient envelope budget").
				WithField("envelope_id", e.ID.String()).
				WithField("available_usd_minor", e.Available.Minor()).
				WithField("requested_usd_minor", usd.Minor())
		}
		env = &e
	}

	// 4. Insert; a concurrent insert under the same key is a replay.
	res, inserted, err := s.insertReservation(ctx, tx, r, account, usd, now)
	if err != nil {
		return Reservation{}, err
	}
	if !inserted {
		existing, found, err := reservationByKey(ctx, tx, r.IdempotencyKey)
		if err != nil {
			return Reservation{}, err
		}
		if !found {
			return Reservation{}, dbErr("insert reservation", errors.New("conflict on idempotency_key but no row visible"))
		}
		return replayReservation(existing, r, account, usd)
	}
	if _, err := adjustTotals(ctx, tx, account, r.AssetID, r.Quantity, now); err != nil {
		return Reservation{}, err
	}
	if env != nil {
		if _, err := moveEnvelopeBudget(ctx, tx, env.ID, usd.Neg(), usd, money.USD{}); err != nil {
			return Reservation{}, err
		}
	}
	if err := s.emit.Emit(ctx, tx, TopicReservationCreated, newReservationEvent(res, r.Reason, now)); err != nil {
		return Reservation{}, fmt.Errorf("capital: emit %s: %w", TopicReservationCreated, err)
	}
	return res, nil
}

func (s *Service) insertReservation(ctx context.Context, tx pgx.Tx, r ReserveRequest, account accounts.AccountID, usd money.USD, now time.Time) (Reservation, bool, error) {
	res, err := scanReservation(tx.QueryRow(ctx, `INSERT INTO asset_reservations
			(id, account_id, asset_id, envelope_id, intent_id, actor_type, actor_id, quantity, consumed_quantity, usd_minor, consumed_usd_minor,
			 status, reason, idempotency_key, correlation_id, created_at, expires_at)
		VALUES ($1, $2, $3, $4, NULLIF($5, '')::uuid, $6, $7, $8::numeric, 0, $9, 0, 'ACTIVE', $10, $11, NULLIF($12, ''), $13, $14)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING `+reservationColumns,
		NewReservationID(), account, r.AssetID, r.EnvelopeID, r.IntentID, string(r.ActorType), r.ActorID, r.Quantity, usd,
		r.Reason, r.IdempotencyKey, observability.CorrelationID(ctx), now, now.Add(r.TTL)))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Reservation{}, false, nil
		}
		return Reservation{}, false, dbErr("insert reservation", err)
	}
	return res, true, nil
}

// replayReservation implements idempotency: the same economics return the
// existing row (whatever its current status), anything else is a reuse.
func replayReservation(existing Reservation, r ReserveRequest, account accounts.AccountID, usd money.USD) (Reservation, error) {
	same := existing.AccountID == account &&
		existing.AssetID == r.AssetID &&
		existing.Quantity.Equal(r.Quantity) &&
		existing.USD.Equal(usd) &&
		sameEnvelope(existing.EnvelopeID, r.EnvelopeID)
	if !same {
		return Reservation{}, errs.New(errs.CodeInvalidIdempotencyReuse, "idempotency key was already used for a different reservation").
			WithField("idempotency_key", r.IdempotencyKey).
			WithField("reservation_id", existing.ID.String())
	}
	return existing, nil
}

func sameEnvelope(a, b *EnvelopeID) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	}
	return *a == *b
}

// Consume implements Reserver. When the consumed quantity reaches the
// reserved quantity the reservation is finalized as CONSUMED.
func (s *Service) Consume(ctx context.Context, tx pgx.Tx, rid ReservationID, qty money.Quantity, usdMinor int64, orderID string) (Reservation, error) {
	return s.consume(ctx, tx, rid, qty, money.USDFromMinor(usdMinor), orderID, false)
}

// ConsumeFinal records a last (possibly zero) consumption and marks the
// reservation CONSUMED: the consumed part stays deployed, the unconsumed
// remainder returns to the account totals and to the envelope's available
// budget. A reservation with nothing consumed cannot be finalized as
// CONSUMED; release it instead.
func (s *Service) ConsumeFinal(ctx context.Context, tx pgx.Tx, rid ReservationID, qty money.Quantity, usdMinor int64, orderID string) (Reservation, error) {
	return s.consume(ctx, tx, rid, qty, money.USDFromMinor(usdMinor), orderID, true)
}

func (s *Service) consume(ctx context.Context, tx pgx.Tx, rid ReservationID, qty money.Quantity, usd money.USD, orderID string, final bool) (Reservation, error) {
	if qty.IsNegative() || usd.IsNegative() {
		return Reservation{}, errs.New(errs.CodeValidationFailed, "consumed quantity and usd must not be negative")
	}
	if !final && qty.IsZero() && usd.IsZero() {
		return Reservation{}, errs.New(errs.CodeValidationFailed, "nothing to consume")
	}
	if err := validateOrderID(orderID); err != nil {
		return Reservation{}, err
	}
	now := s.clk.Now()
	r, err := getReservation(ctx, tx, rid, true)
	if err != nil {
		return Reservation{}, err
	}
	if err := checkReservationTransition(r, ReservationConsumed); err != nil {
		return Reservation{}, err
	}
	if r.LockedByOrderID != "" && orderID != "" && orderID != r.LockedByOrderID {
		return Reservation{}, errs.New(errs.CodeConflict, "reservation is locked by another order").
			WithField("reservation_id", r.ID.String()).
			WithField("locked_by_order_id", r.LockedByOrderID)
	}
	consumedQty := r.ConsumedQuantity.Add(qty)
	if consumedQty.Cmp(r.Quantity) > 0 {
		return Reservation{}, errs.New(errs.CodeValidationFailed, "consumption exceeds reserved quantity").
			WithField("reservation_id", r.ID.String()).
			WithField("remaining", r.Remaining().String()).
			WithField("requested", qty.String())
	}
	consumedUSD, err := r.ConsumedUSD.Add(usd)
	if err != nil {
		return Reservation{}, moneyErr(err)
	}
	if consumedUSD.Cmp(r.USD) > 0 {
		remaining, _ := r.RemainingUSD()
		return Reservation{}, errs.New(errs.CodeValidationFailed, "consumption exceeds reserved usd").
			WithField("reservation_id", r.ID.String()).
			WithField("remaining_usd_minor", remaining.Minor()).
			WithField("requested_usd_minor", usd.Minor())
	}
	finalize := final || consumedQty.Equal(r.Quantity)
	if finalize && consumedQty.IsZero() {
		return Reservation{}, errs.New(errs.CodeValidationFailed, "nothing was consumed; release the reservation instead").
			WithField("reservation_id", r.ID.String())
	}

	// Account pool: the consumed part leaves via the ledger fill; on
	// finalization the unconsumed remainder is handed back too.
	remainingQty := r.Quantity.Sub(consumedQty)
	remainingUSD, err := r.USD.Sub(consumedUSD)
	if err != nil {
		return Reservation{}, moneyErr(err)
	}
	totalsDelta := qty
	if finalize {
		totalsDelta = totalsDelta.Add(remainingQty)
	}
	if totalsDelta.IsPositive() {
		if _, err := adjustTotals(ctx, tx, r.AccountID, r.AssetID, totalsDelta.Neg(), now); err != nil {
			return Reservation{}, err
		}
	}
	// Envelope budget: reserved → deployed for the consumed usd, and on
	// finalization reserved → available for the remainder.
	if r.EnvelopeID != nil {
		dAvailable := money.USD{}
		if finalize {
			dAvailable = remainingUSD
		}
		dReservedAbs, err := usd.Add(dAvailable)
		if err != nil {
			return Reservation{}, moneyErr(err)
		}
		if !dReservedAbs.IsZero() {
			if _, err := moveEnvelopeBudget(ctx, tx, *r.EnvelopeID, dAvailable, dReservedAbs.Neg(), usd); err != nil {
				return Reservation{}, err
			}
		}
	}
	status := ReservationActive
	if finalize {
		status = ReservationConsumed
	}
	updated, err := scanReservation(tx.QueryRow(ctx, `UPDATE asset_reservations SET
			consumed_quantity  = $2::numeric,
			consumed_usd_minor = $3,
			locked_by_order_id = coalesce(NULLIF($4, '')::uuid, locked_by_order_id),
			status             = $5,
			consumed_at        = CASE WHEN $5 = 'CONSUMED' THEN $6::timestamptz ELSE consumed_at END
		WHERE id = $1 AND status = 'ACTIVE' RETURNING `+reservationColumns,
		r.ID, consumedQty, consumedUSD, orderID, string(status), now))
	if err != nil {
		return Reservation{}, dbErr("update consumed reservation", err)
	}
	if err := s.emit.Emit(ctx, tx, TopicReservationConsumed, newReservationEvent(updated, "", now)); err != nil {
		return Reservation{}, fmt.Errorf("capital: emit %s: %w", TopicReservationConsumed, err)
	}
	return updated, nil
}

// Release implements Reserver.
func (s *Service) Release(ctx context.Context, tx pgx.Tx, rid ReservationID, reason string) (Reservation, error) {
	if strings.TrimSpace(reason) == "" {
		return Reservation{}, errs.New(errs.CodeValidationFailed, "release reason required")
	}
	r, err := getReservation(ctx, tx, rid, true)
	if err != nil {
		return Reservation{}, err
	}
	return s.finish(ctx, tx, r, ReservationReleased, reason, s.clk.Now())
}

// finish applies release semantics (RELEASED or EXPIRED): the unconsumed
// remainder returns to the account totals and to the envelope's available
// budget; whatever was consumed stays deployed.
func (s *Service) finish(ctx context.Context, tx pgx.Tx, r Reservation, to ReservationStatus, reason string, now time.Time) (Reservation, error) {
	if err := checkReservationTransition(r, to); err != nil {
		return Reservation{}, err
	}
	remainingQty := r.Remaining()
	remainingUSD, err := r.RemainingUSD()
	if err != nil {
		return Reservation{}, moneyErr(err)
	}
	if remainingQty.IsPositive() {
		if _, err := adjustTotals(ctx, tx, r.AccountID, r.AssetID, remainingQty.Neg(), now); err != nil {
			return Reservation{}, err
		}
	}
	if r.EnvelopeID != nil && remainingUSD.IsPositive() {
		if _, err := moveEnvelopeBudget(ctx, tx, *r.EnvelopeID, remainingUSD, remainingUSD.Neg(), money.USD{}); err != nil {
			return Reservation{}, err
		}
	}
	updated, err := scanReservation(tx.QueryRow(ctx, `UPDATE asset_reservations SET status = $2, release_reason = $3, released_at = $4
		WHERE id = $1 AND status = 'ACTIVE' RETURNING `+reservationColumns, r.ID, string(to), reason, now))
	if err != nil {
		return Reservation{}, dbErr("finish reservation", err)
	}
	topic := TopicReservationReleased
	if to == ReservationExpired {
		topic = TopicReservationExpired
	}
	if err := s.emit.Emit(ctx, tx, topic, newReservationEvent(updated, reason, now)); err != nil {
		return Reservation{}, fmt.Errorf("capital: emit %s: %w", topic, err)
	}
	return updated, nil
}

// LockForOrder implements Reserver. Locking twice with the same order is a
// no-op; a different order is a CONFLICT.
func (s *Service) LockForOrder(ctx context.Context, tx pgx.Tx, rid ReservationID, orderID string) error {
	if orderID == "" {
		return errs.New(errs.CodeValidationFailed, "order_id required")
	}
	if err := validateOrderID(orderID); err != nil {
		return err
	}
	now := s.clk.Now()
	r, err := getReservation(ctx, tx, rid, true)
	if err != nil {
		return err
	}
	if r.Status != ReservationActive {
		return errs.Newf(errs.CodeInvalidStateTransition, "reservation is %s; only ACTIVE reservations can be locked", r.Status).
			WithField("reservation_id", r.ID.String()).WithField("from", string(r.Status))
	}
	if r.LockedByOrderID == orderID {
		return nil
	}
	if r.LockedByOrderID != "" {
		return errs.New(errs.CodeConflict, "reservation is locked by another order").
			WithField("reservation_id", r.ID.String()).WithField("locked_by_order_id", r.LockedByOrderID)
	}
	updated, err := scanReservation(tx.QueryRow(ctx, `UPDATE asset_reservations SET locked_by_order_id = $2::uuid
		WHERE id = $1 AND status = 'ACTIVE' AND locked_by_order_id IS NULL RETURNING `+reservationColumns, r.ID, orderID))
	if err != nil {
		return dbErr("lock reservation", err)
	}
	if err := s.emit.Emit(ctx, tx, TopicReservationLocked, newReservationEvent(updated, "", now)); err != nil {
		return fmt.Errorf("capital: emit %s: %w", TopicReservationLocked, err)
	}
	return nil
}

// ExpireDue implements Reserver. Rows are taken FOR UPDATE SKIP LOCKED in a
// global (account, asset) order so concurrent expirers neither block each
// other nor deadlock; a reservation locked by an order is never touched.
func (s *Service) ExpireDue(ctx context.Context, tx pgx.Tx, now time.Time, limit int) ([]ReservationID, error) {
	if limit <= 0 {
		limit = DefaultExpireBatch
	}
	if limit > MaxExpireBatch {
		limit = MaxExpireBatch
	}
	rows, err := tx.Query(ctx, `SELECT `+reservationColumns+` FROM asset_reservations
		WHERE status = 'ACTIVE' AND expires_at <= $1 AND locked_by_order_id IS NULL
		ORDER BY account_id, asset_id, expires_at, id LIMIT $2 FOR UPDATE SKIP LOCKED`, now, limit)
	if err != nil {
		return nil, dbErr("select due reservations", err)
	}
	var due []Reservation
	for rows.Next() {
		r, err := scanReservation(rows)
		if err != nil {
			rows.Close()
			return nil, dbErr("scan due reservation", err)
		}
		due = append(due, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, dbErr("iterate due reservations", err)
	}
	ids := make([]ReservationID, 0, len(due))
	for _, r := range due {
		if _, err := s.finish(ctx, tx, r, ReservationExpired, "expired", now); err != nil {
			return nil, err
		}
		ids = append(ids, r.ID)
	}
	return ids, nil
}

// Get returns a reservation by id.
func (s *Service) Get(ctx context.Context, q db.Querier, rid ReservationID) (Reservation, error) {
	return getReservation(ctx, q, rid, false)
}

// ListActive returns the ACTIVE reservations for (account, asset), oldest
// first.
func (s *Service) ListActive(ctx context.Context, q db.Querier, accountID string, asset assets.AssetID) ([]Reservation, error) {
	account, err := accounts.ParseAccountID(accountID)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeValidationFailed, "account_id must be a canonical uuid")
	}
	rows, err := q.Query(ctx, `SELECT `+reservationColumns+` FROM asset_reservations
		WHERE account_id = $1 AND asset_id = $2 AND status = 'ACTIVE' ORDER BY created_at, id`, account, asset)
	if err != nil {
		return nil, dbErr("list active reservations", err)
	}
	defer rows.Close()
	out := []Reservation{}
	for rows.Next() {
		r, err := scanReservation(rows)
		if err != nil {
			return nil, dbErr("scan reservation", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, dbErr("iterate reservations", err)
	}
	return out, nil
}

// Availability is a non-locking snapshot of the reservation arithmetic for
// (account, asset). It is for display and the buying-power engine; Reserve
// recomputes it under the totals lock and never trusts a snapshot.
type Availability struct {
	WalletBalance money.Quantity
	Reserved      money.Quantity
	Held          money.Quantity
	Available     money.Quantity // WalletBalance − Reserved − Held, floored at zero
	AsOf          time.Time
}

// Availability returns the current snapshot for (account, asset).
func (s *Service) Availability(ctx context.Context, q db.Querier, accountID string, asset assets.AssetID, now time.Time) (Availability, error) {
	account, err := accounts.ParseAccountID(accountID)
	if err != nil {
		return Availability{}, errs.Wrap(err, errs.CodeValidationFailed, "account_id must be a canonical uuid")
	}
	var a Availability
	if a.WalletBalance, err = walletBalance(ctx, q, account, asset); err != nil {
		return Availability{}, err
	}
	if err := q.QueryRow(ctx, `SELECT coalesce((SELECT reserved FROM asset_reservation_totals WHERE account_id = $1 AND asset_id = $2), 0)::text`, account, asset).Scan(&a.Reserved); err != nil {
		return Availability{}, dbErr("read totals", err)
	}
	if a.Held, err = sumActiveHolds(ctx, q, account, asset, now); err != nil {
		return Availability{}, err
	}
	a.Available = a.WalletBalance.Sub(a.Reserved).Sub(a.Held).Max(money.Quantity{})
	a.AsOf = now
	return a, nil
}

// Undeploy returns usd of deployed envelope capital to available, for
// example when a position is closed and its cost basis comes back. Profit
// or loss on the position is applied separately through ApplyRealizedPnL;
// available can never exceed allocation because deployed capital was taken
// from it in the first place.
func (s *Service) Undeploy(ctx context.Context, tx pgx.Tx, eid EnvelopeID, usd money.USD) (Envelope, error) {
	if !usd.IsPositive() {
		return Envelope{}, errs.New(errs.CodeValidationFailed, "undeploy amount must be positive")
	}
	now := s.clk.Now()
	e, err := getEnvelope(ctx, tx, eid, true)
	if err != nil {
		return Envelope{}, err
	}
	if e.Deployed.Cmp(usd) < 0 {
		return Envelope{}, errs.New(errs.CodeValidationFailed, "undeploy exceeds deployed capital").
			WithField("envelope_id", e.ID.String()).
			WithField("deployed_usd_minor", e.Deployed.Minor()).
			WithField("requested_usd_minor", usd.Minor())
	}
	updated, err := moveEnvelopeBudget(ctx, tx, eid, usd, money.USD{}, usd.Neg())
	if err != nil {
		return Envelope{}, err
	}
	if err := s.emit.Emit(ctx, tx, TopicEnvelopeUndeployed, newEnvelopeEvent(updated, nil, "", now)); err != nil {
		return Envelope{}, fmt.Errorf("capital: emit %s: %w", TopicEnvelopeUndeployed, err)
	}
	return updated, nil
}

func validateOrderID(orderID string) error {
	if orderID == "" {
		return nil
	}
	if _, err := id.ParseAny(orderID); err != nil {
		return errs.New(errs.CodeValidationFailed, "order_id must be a canonical uuid")
	}
	return nil
}
