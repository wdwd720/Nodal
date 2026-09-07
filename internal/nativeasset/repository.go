package nativeasset

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

const assetColumns = `asset_id, creator_account_id, name, symbol, description, coalesce(image_url,''), metadata,
	status, max_supply::text, creator_allocation::text, treasury_allocation::text,
	internal_only, transferable, cashout_eligible, creator_earning_eligible, market_proceeds_eligible,
	minimum_age, jurisdiction_policy, marketing_restrictions,
	content_moderation_state, moderation_notes, economics_locked_at, activated_at, created_at, updated_at`

func scanAsset(row pgx.Row) (Asset, error) {
	var (
		a                              Asset
		meta                           []byte
		maxSupply, creatorAlloc, treas string
		status, moderation             string
	)
	if err := row.Scan(&a.AssetID, &a.CreatorAccountID, &a.Name, &a.Symbol, &a.Description, &a.ImageURL, &meta,
		&status, &maxSupply, &creatorAlloc, &treas,
		&a.Policy.InternalOnly, &a.Policy.Transferable, &a.Policy.CashoutEligible,
		&a.Policy.CreatorEarningEligible, &a.Policy.MarketProceedsEligible,
		&a.Policy.MinimumAge, &a.Policy.JurisdictionPolicy, &a.Policy.MarketingRestrictions,
		&moderation, &a.ModerationNotes, &a.EconomicsLockedAt, &a.ActivatedAt,
		&a.CreatedAt, &a.UpdatedAt); err != nil {
		return Asset{}, err
	}
	a.Status = Status(status)
	a.Moderation = ModerationState(moderation)
	var err error
	if a.Supply.MaxSupply, err = money.ParseQuantity(maxSupply); err != nil {
		return Asset{}, errs.Wrap(err, errs.CodeInternal, "nativeasset: max supply is not an integer")
	}
	if a.Supply.CreatorAllocation, err = money.ParseQuantity(creatorAlloc); err != nil {
		return Asset{}, errs.Wrap(err, errs.CodeInternal, "nativeasset: creator allocation is not an integer")
	}
	if a.Supply.TreasuryAllocation, err = money.ParseQuantity(treas); err != nil {
		return Asset{}, errs.Wrap(err, errs.CodeInternal, "nativeasset: treasury allocation is not an integer")
	}
	if len(meta) > 0 {
		if err := json.Unmarshal(meta, &a.Metadata); err != nil {
			return Asset{}, errs.Wrap(err, errs.CodeInternal, "nativeasset: metadata is not valid JSON")
		}
	}
	a.CreatedAt, a.UpdatedAt = a.CreatedAt.UTC(), a.UpdatedAt.UTC()
	return a, nil
}

// Get returns one native asset.
func (s *Service) Get(ctx context.Context, q db.Querier, assetID assets.AssetID) (Asset, error) {
	a, err := scanAsset(q.QueryRow(ctx, `SELECT `+assetColumns+` FROM native_assets WHERE asset_id = $1`, assetID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Asset{}, errs.New(errs.CodeNotFound, "native asset not found").
				WithField("asset_id", assetID.String())
		}
		return Asset{}, mapError(err)
	}
	return a, nil
}

// getForUpdate reads a row and locks it for the rest of the transaction, so a
// status change cannot race another.
func (s *Service) getForUpdate(ctx context.Context, tx pgx.Tx, assetID assets.AssetID) (Asset, error) {
	a, err := scanAsset(tx.QueryRow(ctx,
		`SELECT `+assetColumns+` FROM native_assets WHERE asset_id = $1 FOR UPDATE`, assetID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Asset{}, errs.New(errs.CodeNotFound, "native asset not found").
				WithField("asset_id", assetID.String())
		}
		return Asset{}, mapError(err)
	}
	return a, nil
}

// existingNamesAndSymbols is the corpus Screen compares a proposal against.
//
// Rejected assets are excluded because they never went live and holding their
// names hostage would let a spammer squat every good symbol by submitting and
// being refused.
func (s *Service) existingNamesAndSymbols(ctx context.Context, q db.Querier) (names, symbols []string, err error) {
	rows, qerr := q.Query(ctx, `SELECT name, symbol FROM native_assets WHERE status <> 'REJECTED'`)
	if qerr != nil {
		return nil, nil, mapError(qerr)
	}
	defer rows.Close()
	for rows.Next() {
		var n, sym string
		if err := rows.Scan(&n, &sym); err != nil {
			return nil, nil, mapError(err)
		}
		names = append(names, n)
		symbols = append(symbols, sym)
	}
	return names, symbols, mapError(rows.Err())
}

// ListByCreator returns a creator's assets, newest first.
func (s *Service) ListByCreator(ctx context.Context, q db.Querier, creator accounts.AccountID, limit int) ([]Asset, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := q.Query(ctx,
		`SELECT `+assetColumns+` FROM native_assets WHERE creator_account_id = $1
		  ORDER BY created_at DESC LIMIT $2`, creator, limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	return collect(rows)
}

// ListTradable returns assets whose markets currently accept at least sells.
func (s *Service) ListTradable(ctx context.Context, q db.Querier, limit int) ([]Asset, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := q.Query(ctx,
		`SELECT `+assetColumns+` FROM native_assets WHERE status IN ('ACTIVE','CLOSE_ONLY')
		  ORDER BY activated_at DESC NULLS LAST, created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	return collect(rows)
}

func collect(rows pgx.Rows) ([]Asset, error) {
	var out []Asset
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, mapError(err)
		}
		out = append(out, a)
	}
	return out, mapError(rows.Err())
}

// SetModeration records a moderation verdict.
//
// It cannot be used to un-reject a live asset into approval without a reason,
// and it never changes lifecycle status: a rejected verdict on an ACTIVE asset
// is a signal for an operator to halt it, not an automatic halt, because
// automatically halting a market on a content signal would make moderation a
// denial-of-service vector against creators.
func (s *Service) SetModeration(ctx context.Context, tx pgx.Tx, assetID assets.AssetID, state ModerationState, notes string) (Asset, error) {
	if !state.Valid() {
		return Asset{}, errs.Newf(errs.CodeValidationFailed, "unknown moderation state %q", state)
	}
	if notes == "" {
		return Asset{}, errs.New(errs.CodeValidationFailed, "a moderation verdict must carry its reasoning")
	}
	a, err := scanAsset(tx.QueryRow(ctx,
		`UPDATE native_assets SET content_moderation_state = $2, moderation_notes = $3
		  WHERE asset_id = $1 RETURNING `+assetColumns, assetID, string(state), notes))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Asset{}, errs.New(errs.CodeNotFound, "native asset not found").
				WithField("asset_id", assetID.String())
		}
		return Asset{}, mapError(err)
	}
	return a, nil
}
