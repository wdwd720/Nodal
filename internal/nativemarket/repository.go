package nativemarket

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

const marketColumns = `id, asset_id, credit_asset_id, virtual_credit_reserve::text, initial_asset_reserve::text,
	platform_fee_bps, creator_fee_bps, status, activated_at, created_at, updated_at`

func scanMarket(row pgx.Row) (Market, error) {
	var (
		m               Market
		virt, initial   string
		platBPS, creBPS int
		status          string
	)
	if err := row.Scan(&m.ID, &m.AssetID, &m.CreditAssetID, &virt, &initial,
		&platBPS, &creBPS, &status, &m.ActivatedAt, &m.CreatedAt, &m.UpdatedAt); err != nil {
		return Market{}, err
	}
	var err error
	if m.Curve.VirtualCreditReserve, err = money.ParseQuantity(virt); err != nil {
		return Market{}, errs.Wrap(err, errs.CodeInternal, "nativemarket: virtual reserve is not an integer")
	}
	if m.Curve.InitialAssetReserve, err = money.ParseQuantity(initial); err != nil {
		return Market{}, errs.Wrap(err, errs.CodeInternal, "nativemarket: initial reserve is not an integer")
	}
	m.Fees = Fees{PlatformBPS: money.BPS(platBPS), CreatorBPS: money.BPS(creBPS)}
	m.Status = Status(status)
	m.CreatedAt, m.UpdatedAt = m.CreatedAt.UTC(), m.UpdatedAt.UTC()
	return m, nil
}

// Market returns one market.
func (s *Service) Market(ctx context.Context, q db.Querier, id MarketID) (Market, error) {
	m, err := scanMarket(q.QueryRow(ctx, `SELECT `+marketColumns+` FROM native_markets WHERE id = $1`, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Market{}, errs.New(errs.CodeNotFound, "native market not found").WithField("market_id", id.String())
		}
		return Market{}, mapError(err)
	}
	return m, nil
}

// MarketByAsset returns the market for an asset.
func (s *Service) MarketByAsset(ctx context.Context, q db.Querier, assetID assets.AssetID) (Market, error) {
	m, err := scanMarket(q.QueryRow(ctx, `SELECT `+marketColumns+` FROM native_markets WHERE asset_id = $1`, assetID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Market{}, errs.New(errs.CodeNotFound, "this asset has no market").
				WithField("asset_id", assetID.String())
		}
		return Market{}, mapError(err)
	}
	return m, nil
}

// marketForUpdate reads and locks a market for the rest of the transaction, so
// two orders on the same market serialise rather than racing the version check.
func (s *Service) marketForUpdate(ctx context.Context, tx pgx.Tx, id MarketID) (Market, error) {
	m, err := scanMarket(tx.QueryRow(ctx,
		`SELECT `+marketColumns+` FROM native_markets WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Market{}, errs.New(errs.CodeNotFound, "native market not found").WithField("market_id", id.String())
		}
		return Market{}, mapError(err)
	}
	return m, nil
}

// State returns a market's current reserves and version.
func (s *Service) State(ctx context.Context, q db.Querier, id MarketID) (State, error) {
	return s.state(ctx, q, id)
}

func (s *Service) state(ctx context.Context, q db.Querier, id MarketID) (State, error) {
	var st State
	var real, asset string
	err := q.QueryRow(ctx,
		`SELECT real_credit_reserve::text, asset_reserve::text, version
		   FROM native_market_state WHERE market_id = $1`, id).Scan(&real, &asset, &st.Version)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return State{}, errs.New(errs.CodeNotFound, "native market state not found").
				WithField("market_id", id.String())
		}
		return State{}, mapError(err)
	}
	if st.RealCreditReserve, err = money.ParseQuantity(real); err != nil {
		return State{}, errs.Wrap(err, errs.CodeInternal, "nativemarket: real reserve is not an integer")
	}
	if st.AssetReserve, err = money.ParseQuantity(asset); err != nil {
		return State{}, errs.Wrap(err, errs.CodeInternal, "nativemarket: asset reserve is not an integer")
	}
	return st, nil
}

func (s *Service) marketAndState(ctx context.Context, q db.Querier, id MarketID) (Market, State, error) {
	m, err := s.Market(ctx, q, id)
	if err != nil {
		return Market{}, State{}, err
	}
	st, err := s.state(ctx, q, id)
	if err != nil {
		return Market{}, State{}, err
	}
	return m, st, nil
}

// creatorOf returns the account that created a native asset. The creator fee
// is paid to them on every trade, so this is on the hot path and is a single
// indexed lookup.
func (s *Service) creatorOf(ctx context.Context, q db.Querier, assetID assets.AssetID) (accounts.AccountID, error) {
	var id accounts.AccountID
	err := q.QueryRow(ctx, `SELECT creator_account_id FROM native_assets WHERE asset_id = $1`, assetID).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return id, errs.New(errs.CodeNotFound, "native asset not found").
				WithField("asset_id", assetID.String())
		}
		return id, mapError(err)
	}
	return id, nil
}

// fillByIdempotencyKey returns the trade a repeated request already performed.
//
// The market moves on every trade, so a retried order that was allowed through
// a second time would execute at a different price and take the user's Credits
// twice. This is the check that makes a retry safe.
//
// It returns the fill's account so the caller can ask whose replay this is.
// The key is globally unique on this table and the projection did not include
// account_id at all, so no caller COULD have asked -- and another account's
// trade was what came back (F-106).
func (s *Service) fillByIdempotencyKey(ctx context.Context, q db.Querier, key string) (ExecuteResult, accounts.AccountID, bool, error) {
	var (
		res                                        ExecuteResult
		owner                                      accounts.AccountID
		side                                       string
		creditsIn, creditsOut, assetsIn, assetsOut string
		toPool, platformFee, creatorFee            string
		realAfter, assetAfter                      string
	)
	err := q.QueryRow(ctx,
		`SELECT id, market_id, account_id, side, credits_in::text, credits_out::text, assets_in::text, assets_out::text,
		        credits_to_pool::text, platform_fee::text, creator_fee::text,
		        real_credit_reserve_after::text, asset_reserve_after::text
		   FROM native_market_fills WHERE idempotency_key = $1`, key).
		Scan(&res.FillID, &res.MarketID, &owner, &side, &creditsIn, &creditsOut, &assetsIn, &assetsOut,
			&toPool, &platformFee, &creatorFee, &realAfter, &assetAfter)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ExecuteResult{}, accounts.AccountID{}, false, nil
		}
		return ExecuteResult{}, accounts.AccountID{}, false, mapError(err)
	}
	parse := func(s string) money.Quantity {
		v, perr := money.ParseQuantity(s)
		if perr != nil {
			return money.Quantity{}
		}
		return v
	}
	res.Existing = true
	res.Fill = Fill{
		Side:          Side(side),
		CreditsIn:     parse(creditsIn),
		CreditsOut:    parse(creditsOut),
		AssetsIn:      parse(assetsIn),
		AssetsOut:     parse(assetsOut),
		CreditsToPool: parse(toPool),
		PlatformFee:   parse(platformFee),
		CreatorFee:    parse(creatorFee),
		StateAfter: State{
			RealCreditReserve: parse(realAfter),
			AssetReserve:      parse(assetAfter),
		},
	}
	return res, owner, true, nil
}

// StoredQuote returns a recorded quote.
func (s *Service) StoredQuote(ctx context.Context, q db.Querier, id QuoteID) (Quote, error) {
	var (
		qt                             Quote
		side                           string
		input, output, platFee, creFee string
		spot, effective                string
		slippage                       int
	)
	err := q.QueryRow(ctx,
		`SELECT id, market_id, account_id, side, input_amount::text, expected_output::text,
		        platform_fee::text, creator_fee::text, spot_price_before::text, effective_price::text,
		        slippage_bps, state_version, expires_at, created_at
		   FROM native_market_quotes WHERE id = $1`, id).
		Scan(&qt.ID, &qt.MarketID, &qt.AccountID, &side, &input, &output, &platFee, &creFee,
			&spot, &effective, &slippage, &qt.StateVersion, &qt.ExpiresAt, &qt.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Quote{}, errs.New(errs.CodeNotFound, "quote not found").WithField("quote_id", id.String())
		}
		return Quote{}, mapError(err)
	}
	qt.Side = Side(side)
	qt.SlippageBPS = money.BPS(slippage)
	for dst, src := range map[*money.Quantity]string{
		&qt.InputAmount: input, &qt.ExpectedOutput: output,
		&qt.PlatformFee: platFee, &qt.CreatorFee: creFee,
		&qt.SpotPriceBefore: spot, &qt.EffectivePrice: effective,
	} {
		v, perr := money.ParseQuantity(src)
		if perr != nil {
			return Quote{}, errs.Wrap(perr, errs.CodeInternal, "nativemarket: quote holds a non-integer amount")
		}
		*dst = v
	}
	qt.ExpiresAt, qt.CreatedAt = qt.ExpiresAt.UTC(), qt.CreatedAt.UTC()
	return qt, nil
}

// Holding is one account's position in a native asset.
type Holding struct {
	AccountID accounts.AccountID
	Quantity  money.Quantity
}

// Holders returns the largest holders of an asset, which is what the
// concentration display of PART LIV shows. It reads the ledger's balance
// projection rather than keeping a second tally.
func (s *Service) Holders(ctx context.Context, q db.Querier, assetID assets.AssetID, limit int) ([]Holding, error) {
	if limit <= 0 || limit > 500 {
		limit = 25
	}
	rows, err := q.Query(ctx,
		`SELECT la.owner_id::text, b.balance::text
		   FROM ledger_accounts la
		   JOIN ledger_balances b ON b.ledger_account_id = la.id
		  WHERE la.asset_id = $1 AND la.owner_type = 'CUSTOMER'
		    AND la.code = 'NATIVE_ASSET_BALANCE' AND b.balance > 0
		  ORDER BY b.balance DESC
		  LIMIT $2`, assetID, limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []Holding
	for rows.Next() {
		var owner, bal string
		if err := rows.Scan(&owner, &bal); err != nil {
			return nil, mapError(err)
		}
		acct, perr := accounts.ParseAccountID(owner)
		if perr != nil {
			return nil, errs.Wrap(perr, errs.CodeInternal, "nativemarket: holder id is not an account id")
		}
		qty, perr := money.ParseQuantity(bal)
		if perr != nil {
			return nil, errs.Wrap(perr, errs.CodeInternal, "nativemarket: holding is not an integer")
		}
		out = append(out, Holding{AccountID: acct, Quantity: qty})
	}
	return out, mapError(rows.Err())
}

// VerifyReserves checks the control-account invariant of the Credit reserve.
//
// Every market's Credits sit in ONE platform ledger account, because the ledger
// keys a platform account by (code, asset) and every market quotes against the
// same Credit asset. Per-market attribution therefore lives in
// native_market_state, which makes the pair a classic control account and
// subsidiary ledger — and makes this the check that keeps them honest:
//
//	SUM(native_market_state.real_credit_reserve) == MARKET_RESERVE balance
//
// A divergence means a trade moved Credits without moving market state, or the
// reverse. Either is a reconciliation incident, never something to correct
// silently by writing one side to match the other.
func (s *Service) VerifyReserves(ctx context.Context, q db.Querier, creditAssetID assets.AssetID) error {
	var stateSum, ledgerBalance string
	err := q.QueryRow(ctx,
		`SELECT
		   coalesce((SELECT sum(st.real_credit_reserve)
		               FROM native_market_state st
		               JOIN native_markets m ON m.id = st.market_id
		              WHERE m.credit_asset_id = $1), 0)::text,
		   coalesce((SELECT b.balance
		               FROM ledger_accounts la
		               JOIN ledger_balances b ON b.ledger_account_id = la.id
		              WHERE la.owner_type = 'PLATFORM' AND la.code = 'MARKET_RESERVE'
		                AND la.asset_id = $1), 0)::text`,
		creditAssetID).Scan(&stateSum, &ledgerBalance)
	if err != nil {
		return mapError(err)
	}
	if stateSum != ledgerBalance {
		return errs.Newf(errs.CodeReconciliationRequired,
			"native market reserves do not reconcile: market states hold %s, the ledger holds %s",
			stateSum, ledgerBalance).
			WithField("market_state_sum", stateSum).
			WithField("ledger_balance", ledgerBalance)
	}
	return nil
}
