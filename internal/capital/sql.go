package capital

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// Column lists. NUMERIC values are exchanged as text so money.Quantity scans
// them exactly; nullable uuid/text columns are coalesced to "" so the Go
// structs stay pointer-free where absence is not meaningful.

const reservationColumns = `id, account_id, asset_id, envelope_id, coalesce(intent_id::text, ''), coalesce(locked_by_order_id::text, ''),
	actor_type, actor_id, quantity::text, consumed_quantity::text, usd_minor, consumed_usd_minor, status, reason,
	coalesce(release_reason, ''), idempotency_key, coalesce(correlation_id, ''), created_at, expires_at, consumed_at, released_at`

func scanReservation(row pgx.Row) (Reservation, error) {
	var r Reservation
	var env EnvelopeID
	if err := row.Scan(&r.ID, &r.AccountID, &r.AssetID, &env, &r.IntentID, &r.LockedByOrderID,
		&r.ActorType, &r.ActorID, &r.Quantity, &r.ConsumedQuantity, &r.USD, &r.ConsumedUSD, &r.Status, &r.Reason,
		&r.ReleaseReason, &r.IdempotencyKey, &r.CorrelationID, &r.CreatedAt, &r.ExpiresAt, &r.ConsumedAt, &r.ReleasedAt); err != nil {
		return Reservation{}, err
	}
	if !env.IsZero() {
		r.EnvelopeID = &env
	}
	return r, nil
}

const envelopeColumns = `id, account_id, agent_id::text, strategy_version_id::text, settlement_asset_id,
	allocation_usd_minor, available_usd_minor, reserved_usd_minor, deployed_usd_minor,
	realized_pnl_usd_minor, realized_loss_usd_minor, current_drawdown_usd_minor, daily_loss_usd_minor, daily_loss_reset_at,
	max_daily_loss_usd_minor, max_drawdown_usd_minor, max_single_trade_usd_minor, max_position_usd_minor,
	allowed_instruments::text[], allowed_asset_classes, allowed_venues,
	max_model_spend_usd_minor, max_data_spend_usd_minor, max_order_rate_per_hour, policy_version, status,
	effective_at, expires_at, version, created_by_actor_type, created_by_actor_id, created_at, updated_at`

func scanEnvelope(row pgx.Row) (Envelope, error) {
	var e Envelope
	if err := row.Scan(&e.ID, &e.AccountID, &e.AgentID, &e.StrategyVersionID, &e.SettlementAssetID,
		&e.Allocation, &e.Available, &e.Reserved, &e.Deployed,
		&e.RealizedPnL, &e.RealizedLoss, &e.CurrentDrawdown, &e.DailyLoss, &e.DailyLossResetAt,
		&e.MaxDailyLoss, &e.MaxDrawdown, &e.MaxSingleTrade, &e.MaxPosition,
		&e.AllowedInstruments, &e.AllowedAssetClasses, &e.AllowedVenues,
		&e.MaxModelSpend, &e.MaxDataSpend, &e.MaxOrderRatePerHour, &e.PolicyVersion, &e.Status,
		&e.EffectiveAt, &e.ExpiresAt, &e.Version, &e.CreatedByActorType, &e.CreatedByActorID, &e.CreatedAt, &e.UpdatedAt); err != nil {
		return Envelope{}, err
	}
	e.AllowedInstruments = normalizeList(e.AllowedInstruments)
	e.AllowedAssetClasses = normalizeList(e.AllowedAssetClasses)
	e.AllowedVenues = normalizeList(e.AllowedVenues)
	return e, nil
}

const holdColumns = `id, account_id, asset_id, quantity::text, reason, coalesce(deposit_id::text, ''), created_at, expires_at, released_at, coalesce(released_by, '')`

func scanHold(row pgx.Row) (WithdrawalHold, error) {
	var h WithdrawalHold
	if err := row.Scan(&h.ID, &h.AccountID, &h.AssetID, &h.Quantity, &h.Reason, &h.DepositID, &h.CreatedAt, &h.ExpiresAt, &h.ReleasedAt, &h.ReleasedBy); err != nil {
		return WithdrawalHold{}, err
	}
	return h, nil
}

// dbErr wraps an unexpected database failure. Domain errors are built at
// the call site; this is for infrastructure faults only.
func dbErr(op string, err error) error {
	return fmt.Errorf("capital: %s: %w", op, err)
}

// --- reservations ---------------------------------------------------------

func getReservation(ctx context.Context, q db.Querier, rid ReservationID, forUpdate bool) (Reservation, error) {
	sql := `SELECT ` + reservationColumns + ` FROM asset_reservations WHERE id = $1`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	r, err := scanReservation(q.QueryRow(ctx, sql, rid))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Reservation{}, errs.New(errs.CodeNotFound, "reservation not found").WithField("reservation_id", rid.String())
		}
		return Reservation{}, dbErr("get reservation", err)
	}
	return r, nil
}

// reservationByKey returns the reservation created under key, if any.
func reservationByKey(ctx context.Context, q db.Querier, key string) (Reservation, bool, error) {
	r, err := scanReservation(q.QueryRow(ctx, `SELECT `+reservationColumns+` FROM asset_reservations WHERE idempotency_key = $1`, key))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Reservation{}, false, nil
		}
		return Reservation{}, false, dbErr("reservation by idempotency key", err)
	}
	return r, true, nil
}

// --- totals ---------------------------------------------------------------

// lockTotals upserts the (account, asset) totals row and locks it FOR
// UPDATE. Every reservation on the pair serializes on this lock.
func lockTotals(ctx context.Context, tx pgx.Tx, account accounts.AccountID, asset assets.AssetID) (ReservationTotals, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO asset_reservation_totals (account_id, asset_id) VALUES ($1, $2) ON CONFLICT (account_id, asset_id) DO NOTHING`, account, asset); err != nil {
		return ReservationTotals{}, dbErr("upsert totals", err)
	}
	t := ReservationTotals{AccountID: account, AssetID: asset}
	if err := tx.QueryRow(ctx, `SELECT reserved::text, version, updated_at FROM asset_reservation_totals WHERE account_id = $1 AND asset_id = $2 FOR UPDATE`, account, asset).
		Scan(&t.Reserved, &t.Version, &t.UpdatedAt); err != nil {
		return ReservationTotals{}, dbErr("lock totals", err)
	}
	return t, nil
}

// adjustTotals adds delta (which may be negative) to the locked totals row.
// The CHECK (reserved >= 0) is the last line of defense; hitting it means a
// bug, so it is surfaced as an infrastructure error, never swallowed.
func adjustTotals(ctx context.Context, tx pgx.Tx, account accounts.AccountID, asset assets.AssetID, delta money.Quantity, now time.Time) (money.Quantity, error) {
	if delta.IsZero() {
		var cur money.Quantity
		if err := tx.QueryRow(ctx, `SELECT reserved::text FROM asset_reservation_totals WHERE account_id = $1 AND asset_id = $2 FOR UPDATE`, account, asset).Scan(&cur); err != nil {
			return money.Quantity{}, dbErr("lock totals", err)
		}
		return cur, nil
	}
	var reserved money.Quantity
	if err := tx.QueryRow(ctx, `UPDATE asset_reservation_totals SET reserved = reserved + $3::numeric, version = version + 1, updated_at = $4
		WHERE account_id = $1 AND asset_id = $2 RETURNING reserved::text`, account, asset, delta, now).Scan(&reserved); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return money.Quantity{}, dbErr("adjust totals", errors.New("totals row missing"))
		}
		return money.Quantity{}, dbErr("adjust totals", err)
	}
	return reserved, nil
}

// walletBalance reads the CUSTOMER/WALLET ledger balance for (account,
// asset) directly from the balance projection. A missing ledger account or
// balance row is a zero balance.
func walletBalance(ctx context.Context, q db.Querier, account accounts.AccountID, asset assets.AssetID) (money.Quantity, error) {
	var bal money.Quantity
	err := q.QueryRow(ctx, `SELECT coalesce(b.balance, 0)::text
		FROM ledger_accounts la
		LEFT JOIN ledger_balances b ON b.ledger_account_id = la.id
		WHERE la.owner_type = 'CUSTOMER' AND la.owner_id = $1 AND la.code = 'WALLET' AND la.asset_id = $2`, account, asset).Scan(&bal)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return money.Quantity{}, nil
		}
		return money.Quantity{}, dbErr("wallet balance", err)
	}
	return bal, nil
}

// sumActiveHolds returns Σ quantity of holds active at now.
func sumActiveHolds(ctx context.Context, q db.Querier, account accounts.AccountID, asset assets.AssetID, now time.Time) (money.Quantity, error) {
	var sum money.Quantity
	err := q.QueryRow(ctx, `SELECT coalesce(sum(quantity), 0)::text FROM withdrawal_holds
		WHERE account_id = $1 AND asset_id = $2 AND released_at IS NULL AND (expires_at IS NULL OR expires_at > $3)`, account, asset, now).Scan(&sum)
	if err != nil {
		return money.Quantity{}, dbErr("sum holds", err)
	}
	return sum, nil
}

// --- envelopes ------------------------------------------------------------

func getEnvelope(ctx context.Context, q db.Querier, eid EnvelopeID, forUpdate bool) (Envelope, error) {
	sql := `SELECT ` + envelopeColumns + ` FROM capital_envelopes WHERE id = $1`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	e, err := scanEnvelope(q.QueryRow(ctx, sql, eid))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Envelope{}, errs.New(errs.CodeNotFound, "envelope not found").WithField("envelope_id", eid.String())
		}
		return Envelope{}, dbErr("get envelope", err)
	}
	return e, nil
}

// envelopeAccount reads only the owning account of an envelope, for the
// tenant check that must precede any lock.
func envelopeAccount(ctx context.Context, q db.Querier, eid EnvelopeID) (accounts.AccountID, error) {
	var a accounts.AccountID
	if err := q.QueryRow(ctx, `SELECT account_id FROM capital_envelopes WHERE id = $1`, eid).Scan(&a); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return accounts.AccountID{}, errs.New(errs.CodeNotFound, "envelope not found").WithField("envelope_id", eid.String())
		}
		return accounts.AccountID{}, dbErr("envelope account", err)
	}
	return a, nil
}

// moveEnvelopeBudget applies deltas to the three budget-flow columns of a
// locked envelope. Deltas must sum to zero (the flow is a permutation of the
// allocation), which the caller guarantees; the CHECK constraints reject
// any negative result.
func moveEnvelopeBudget(ctx context.Context, tx pgx.Tx, eid EnvelopeID, dAvailable, dReserved, dDeployed money.USD) (Envelope, error) {
	e, err := scanEnvelope(tx.QueryRow(ctx, `UPDATE capital_envelopes SET
			available_usd_minor = available_usd_minor + $2,
			reserved_usd_minor  = reserved_usd_minor  + $3,
			deployed_usd_minor  = deployed_usd_minor  + $4,
			version = version + 1
		WHERE id = $1 RETURNING `+envelopeColumns, eid, dAvailable, dReserved, dDeployed))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Envelope{}, errs.New(errs.CodeNotFound, "envelope not found").WithField("envelope_id", eid.String())
		}
		return Envelope{}, dbErr("move envelope budget", err)
	}
	return e, nil
}

// insertEnvelopeChange appends the audit row for an authority change. The
// table's CHECK refuses AGENT actors; callers have already refused them.
func insertEnvelopeChange(ctx context.Context, tx pgx.Tx, eid EnvelopeID, changes map[string]FieldChange, actorType security.ActorType, actorID, reason, approvalID string, at time.Time) error {
	body, err := json.Marshal(changes)
	if err != nil {
		return dbErr("encode envelope change", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO capital_envelope_changes (id, envelope_id, changes, actor_type, actor_id, reason, approval_id, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, '')::uuid, $8)`,
		id.New[id.Any](), eid, body, string(actorType), actorID, reason, approvalID, at); err != nil {
		return dbErr("insert envelope change", err)
	}
	return nil
}

// --- guards ---------------------------------------------------------------

// guardAccountAndAsset refuses reservations for accounts and assets that
// permit no new transactions at all. RESTRICTED accounts and CLOSE_ONLY /
// RESTRICTED / DELISTING assets pass: a reservation may back a risk-reducing
// disposal, and the risk kernel decides direction.
func guardAccountAndAsset(ctx context.Context, q db.Querier, account accounts.AccountID, asset assets.AssetID) error {
	var accStatus accounts.Status
	if err := q.QueryRow(ctx, `SELECT status FROM accounts WHERE id = $1`, account).Scan(&accStatus); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errs.New(errs.CodeNotFound, "account not found").WithField("account_id", account.String())
		}
		return dbErr("account status", err)
	}
	if !accStatus.AllowsRiskReduction() {
		return errs.Newf(errs.CodeAccountFrozen, "account is %s", accStatus).
			WithField("account_id", account.String()).WithField("account_status", string(accStatus))
	}
	var assetStatus assets.Status
	if err := q.QueryRow(ctx, `SELECT status FROM assets WHERE id = $1`, asset).Scan(&assetStatus); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errs.New(errs.CodeNotFound, "asset not found").WithField("asset_id", asset.String())
		}
		return dbErr("asset status", err)
	}
	if !assetStatus.AllowsReducingExposure() {
		return errs.Newf(errs.CodeAssetRestricted, "asset is %s", assetStatus).
			WithField("asset_id", asset.String()).WithField("asset_status", string(assetStatus))
	}
	return nil
}
