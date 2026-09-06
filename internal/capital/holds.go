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
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
)

// PlaceHold records a withdrawal hold. Holds are placed by the funding
// service (reversibility windows, chargebacks, reviews) and reduce the
// quantity available to both reservations and withdrawals while active.
func (s *Service) PlaceHold(ctx context.Context, tx pgx.Tx, h WithdrawalHold) (WithdrawalHold, error) {
	var problems []string
	if h.AccountID.IsZero() {
		problems = append(problems, "account_id required")
	}
	if h.AssetID.IsZero() {
		problems = append(problems, "asset_id required")
	}
	if !h.Quantity.IsPositive() {
		problems = append(problems, "quantity must be positive")
	}
	if strings.TrimSpace(h.Reason) == "" {
		problems = append(problems, "reason required")
	}
	if h.DepositID != "" {
		if _, err := id.ParseAny(h.DepositID); err != nil {
			problems = append(problems, "deposit_id must be a canonical uuid")
		}
	}
	now := s.clk.Now()
	if h.ExpiresAt != nil && !h.ExpiresAt.After(now) {
		problems = append(problems, "expires_at must be in the future")
	}
	if len(problems) > 0 {
		return WithdrawalHold{}, errs.New(errs.CodeValidationFailed, "invalid withdrawal hold").WithField("problems", problems)
	}
	if h.ID.IsZero() {
		h.ID = NewWithdrawalHoldID()
	}
	created, err := scanHold(tx.QueryRow(ctx, `INSERT INTO withdrawal_holds (id, account_id, asset_id, quantity, reason, deposit_id, created_at, expires_at)
		VALUES ($1, $2, $3, $4::numeric, $5, NULLIF($6, '')::uuid, $7, $8) RETURNING `+holdColumns,
		h.ID, h.AccountID, h.AssetID, h.Quantity, h.Reason, h.DepositID, now, h.ExpiresAt))
	if err != nil {
		if db.IsUniqueViolation(err) {
			return WithdrawalHold{}, errs.Wrap(err, errs.CodeConflict, "hold already exists").WithField("hold_id", h.ID.String())
		}
		return WithdrawalHold{}, dbErr("insert hold", err)
	}
	if err := s.emit.Emit(ctx, tx, TopicHoldPlaced, newHoldEvent(created, now)); err != nil {
		return WithdrawalHold{}, fmt.Errorf("capital: emit %s: %w", TopicHoldPlaced, err)
	}
	return created, nil
}

// ReleaseHold releases an active hold. Releasing twice is
// INVALID_STATE_TRANSITION, never a silent no-op.
func (s *Service) ReleaseHold(ctx context.Context, tx pgx.Tx, hid WithdrawalHoldID, releasedBy string) (WithdrawalHold, error) {
	if strings.TrimSpace(releasedBy) == "" {
		return WithdrawalHold{}, errs.New(errs.CodeValidationFailed, "released_by required")
	}
	now := s.clk.Now()
	cur, err := scanHold(tx.QueryRow(ctx, `SELECT `+holdColumns+` FROM withdrawal_holds WHERE id = $1 FOR UPDATE`, hid))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return WithdrawalHold{}, errs.New(errs.CodeNotFound, "hold not found").WithField("hold_id", hid.String())
		}
		return WithdrawalHold{}, dbErr("lock hold", err)
	}
	if cur.ReleasedAt != nil {
		return WithdrawalHold{}, errs.New(errs.CodeInvalidStateTransition, "hold is already released").
			WithField("hold_id", hid.String()).WithField("released_at", timeValue(cur.ReleasedAt))
	}
	released, err := scanHold(tx.QueryRow(ctx, `UPDATE withdrawal_holds SET released_at = $2, released_by = $3 WHERE id = $1 RETURNING `+holdColumns, hid, now, releasedBy))
	if err != nil {
		return WithdrawalHold{}, dbErr("release hold", err)
	}
	if err := s.emit.Emit(ctx, tx, TopicHoldReleased, newHoldEvent(released, now)); err != nil {
		return WithdrawalHold{}, fmt.Errorf("capital: emit %s: %w", TopicHoldReleased, err)
	}
	return released, nil
}

// ActiveHolds lists the holds binding (account, asset) at now.
func (s *Service) ActiveHolds(ctx context.Context, q db.Querier, accountID string, asset assets.AssetID, now time.Time) ([]WithdrawalHold, error) {
	account, err := accounts.ParseAccountID(accountID)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeValidationFailed, "account_id must be a canonical uuid")
	}
	rows, err := q.Query(ctx, `SELECT `+holdColumns+` FROM withdrawal_holds
		WHERE account_id = $1 AND asset_id = $2 AND released_at IS NULL AND (expires_at IS NULL OR expires_at > $3)
		ORDER BY created_at, id`, account, asset, now)
	if err != nil {
		return nil, dbErr("list holds", err)
	}
	defer rows.Close()
	out := []WithdrawalHold{}
	for rows.Next() {
		h, err := scanHold(rows)
		if err != nil {
			return nil, dbErr("scan hold", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, dbErr("iterate holds", err)
	}
	return out, nil
}
