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
		versionBefore, versionAfter                int64
	)
	// seq IS the version the fill produced: 00712's apply trigger moves the
	// market to it, and the column is the market's own sequence. Reading it
	// here is what makes a replay report the same state_version_after as the
	// original -- the alternative, recomputing it from current state, would
	// report today's version for a trade that happened yesterday (F-195).
	err := q.QueryRow(ctx,
		`SELECT id, market_id, account_id, side, credits_in::text, credits_out::text, assets_in::text, assets_out::text,
		        credits_to_pool::text, platform_fee::text, creator_fee::text,
		        real_credit_reserve_after::text, asset_reserve_after::text,
		        state_version_before, seq
		   FROM native_market_fills WHERE idempotency_key = $1`, key).
		Scan(&res.FillID, &res.MarketID, &owner, &side, &creditsIn, &creditsOut, &assetsIn, &assetsOut,
			&toPool, &platformFee, &creatorFee, &realAfter, &assetAfter,
			&versionBefore, &versionAfter)
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
		StateBefore:   State{Version: versionBefore},
		StateAfter: State{
			RealCreditReserve: parse(realAfter),
			AssetReserve:      parse(assetAfter),
			Version:           versionAfter,
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

// Holding is one place in an asset's concentration, and it does not say whose.
//
// # Why there is no holder here
//
// The concentration display of PART LIV answers "how much of this is held by
// how few", and that question is answered by the SHAPE of the distribution.
// Who occupies each place is a different fact, and it was being handed to every
// signed-in caller: `top_holders[]` rendered {account_id, quantity} behind
// `native_asset:read`, which every customer role holds, so anyone could read
// any market's largest positions by account and watch them move trade by trade
// (F-197). prints.go states the rule this package already follows for the tape
// it owns -- "who did it is not the public's business" -- and a position is a
// stronger version of the same fact than a print.
//
// So a row is a rank, a quantity and a share, and the one row a caller may see
// named is their own, which they already know (D-111). An operator who needs
// the names needs them for surveillance, which is a permission
// (`native_market:surveil`) and a route of its own, not a field on the page
// every customer reads.
type Holding struct {
	// Rank is 1 for the largest holder of the asset, 2 for the next, and so on
	// down the page. It is assigned over ALL holders, so rank 10 on a page of
	// ten means there are more.
	Rank int
	// Quantity is the holding in the asset's base units.
	Quantity money.Quantity
	// ShareBPS is this holding as a share of every unit accounts hold, in basis
	// points, truncated.
	//
	// The denominator is the sum of customer balances, not the circulating
	// supply: units still in the pool are held by nobody, and a creator's
	// allocation is minted outside the curve, so a share of circulating supply
	// is a number that can exceed one hundred per cent. This one cannot.
	ShareBPS money.BPS
	// IsYou marks the caller's own row, and is set only by HoldersFor.
	IsYou bool
	// AccountID is set ONLY on the caller's own row, by HoldersFor, and is zero
	// everywhere else including on every row Holders returns. Telling somebody
	// their own account id is telling them nothing they did not send.
	AccountID accounts.AccountID
}

// Holders returns the shape of an asset's concentration, naming nobody.
//
// It reads the ledger's balance projection rather than keeping a second tally.
func (s *Service) Holders(ctx context.Context, q db.Querier, assetID assets.AssetID, limit int) ([]Holding, error) {
	return s.holders(ctx, q, assetID, limit, accounts.AccountID{})
}

// HoldersFor is Holders with the caller's own row marked.
//
// The caller is not a filter and does not change which rows come back: it marks
// the one row the caller already knows about, so a person can find themselves
// on a page that names nobody.
func (s *Service) HoldersFor(ctx context.Context, q db.Querier, assetID assets.AssetID, limit int, caller accounts.AccountID) ([]Holding, error) {
	return s.holders(ctx, q, assetID, limit, caller)
}

func (s *Service) holders(ctx context.Context, q db.Querier, assetID assets.AssetID, limit int, caller accounts.AccountID) ([]Holding, error) {
	if limit <= 0 || limit > 500 {
		limit = 25
	}
	// The rank and the denominator are computed by the database over EVERY
	// holder, before the limit is applied: a rank counted in Go would be a rank
	// within the page, and a share taken over the page would make the tenth
	// holder of a thousand look like a tenth of the asset. owner_id breaks ties
	// so the ordering -- and therefore the rank -- is the same on every read.
	rows, err := q.Query(ctx,
		`SELECT row_number() OVER (ORDER BY b.balance DESC, la.owner_id) AS rank,
		        la.owner_id::text, b.balance::text,
		        (sum(b.balance) OVER ())::text AS held_total
		   FROM ledger_accounts la
		   JOIN ledger_balances b ON b.ledger_account_id = la.id
		  WHERE la.asset_id = $1 AND la.owner_type = 'CUSTOMER'
		    AND la.code = 'NATIVE_ASSET_BALANCE' AND b.balance > 0
		  ORDER BY b.balance DESC, la.owner_id
		  LIMIT $2`, assetID, limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []Holding
	for rows.Next() {
		var (
			rank            int64
			owner, bal, tot string
		)
		if err := rows.Scan(&rank, &owner, &bal, &tot); err != nil {
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
		total, perr := money.ParseQuantity(tot)
		if perr != nil {
			return nil, errs.Wrap(perr, errs.CodeInternal, "nativemarket: held total is not an integer")
		}
		h := Holding{Rank: int(rank), Quantity: qty, ShareBPS: shareBPS(qty, total)}
		if !caller.IsZero() && acct == caller {
			h.IsYou, h.AccountID = true, acct
		}
		out = append(out, h)
	}
	return out, mapError(rows.Err())
}

// shareBPS is part/total in basis points, truncated, saturating at 100%.
//
// Truncated because a share rounded up is a concentration figure that overstates
// somebody's position, and this number is read as a risk measure.
func shareBPS(part, total money.Quantity) money.BPS {
	if total.Sign() <= 0 || part.Sign() <= 0 {
		return 0
	}
	scaled := part.Mul(money.QuantityFromInt64(int64(money.OneHundredPercent)))
	out, err := scaled.Div(total, money.RoundDown)
	if err != nil {
		return 0
	}
	v, err := out.Int64()
	if err != nil || money.BPS(v) > money.OneHundredPercent {
		return money.OneHundredPercent
	}
	return money.BPS(v)
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
