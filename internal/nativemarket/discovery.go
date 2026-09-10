package nativemarket

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/nativeasset"
)

// Market discovery (product goal §12 MARKETS PAGE, §35 SEARCH / DISCOVERY).
//
// One query answers the markets page: the market, its state, its asset, its
// 24-hour figures and whether it is demo data. It is one query rather than a
// list plus a fan-out of per-market reads because a page of twenty markets
// otherwise costs eighty round trips, and because the figures then describe
// eighty different instants.
//
// # What is NOT here
//
// "Market cap" is not computed. §12 permits an internal valuation "only if
// mathematically legitimate", and price × total supply is not: the marginal
// price of a constant-product pool is what the NEXT unit costs, and the pool
// could not buy back the float at it. The response carries the pieces --
// circulating supply, total supply, the marginal price, and the real Credit
// reserve, which is the entire amount that could ever be paid out -- and leaves
// the multiplication undone.
//
// A creator "handle" is a user id. Profiles are another domain's, and inventing
// a display name here would be a second source for it.

// SortBy names an ordering of the markets list.
type SortBy string

// The orderings the markets page offers.
const (
	// SortNewest is by market creation, newest first. It is the only STABLE
	// ordering: its key cannot change once a market exists, so paging through
	// it sees every market exactly once.
	SortNewest SortBy = "NEWEST"
	// SortVolume24h is by Credits traded in the last 24 hours.
	SortVolume24h SortBy = "VOLUME_24H"
	// SortChange24h is by the marginal price's move over the last 24 hours.
	SortChange24h SortBy = "CHANGE_24H"
	// SortLiquidity is by the pool's pricing depth, V + R.
	SortLiquidity SortBy = "LIQUIDITY"
	// SortPrice is by the current marginal price.
	SortPrice SortBy = "PRICE"
)

var allSorts = []SortBy{SortNewest, SortVolume24h, SortChange24h, SortLiquidity, SortPrice}

// AllSorts returns every declared ordering (a copy).
func AllSorts() []SortBy { return append([]SortBy(nil), allSorts...) }

// Valid reports whether s is declared.
func (s SortBy) Valid() bool {
	for _, x := range allSorts {
		if x == s {
			return true
		}
	}
	return false
}

// Stable reports whether paging through this ordering can see every row
// exactly once. Only SortNewest can: every other key is a live figure that
// moves when somebody trades, so a market that moves between pages may be seen
// twice or not at all. Saying so is better than a cursor that pretends.
func (s SortBy) Stable() bool { return s == SortNewest }

// MaxListLimit bounds one page of markets.
const MaxListLimit = 100

// ListRequest is one page of the markets page.
type ListRequest struct {
	// Statuses filters by market status. Empty means every status.
	Statuses []Status
	// Creator filters to one creator's markets. Zero means every creator.
	Creator accounts.AccountID
	// Query is a free-text search over the asset's name, symbol and
	// description. Empty means no text filter.
	Query string
	Sort  SortBy
	// Cursor is an opaque position from a previous page.
	Cursor string
	Limit  int
}

// MarketSummary is one row of the markets page.
type MarketSummary struct {
	MarketID      MarketID
	AssetID       assets.AssetID
	CreditAssetID assets.AssetID

	Name        string
	Symbol      string
	Description string
	ImageURL    string

	MarketStatus Status
	AssetStatus  nativeasset.Status
	Moderation   nativeasset.ModerationState
	// CreatorAccountID is the creator's account. It is the handle placeholder:
	// a display name belongs to the profile domain and is joined later.
	CreatorAccountID accounts.AccountID

	Curve Curve
	State State
	Fees  Fees

	// LastPrice is the current marginal price, scaled by PriceScale.
	LastPrice  money.Quantity
	PriceScale int
	// ReferencePrice24h is the marginal price at the start of the 24-hour
	// window, and Change24hBPS the move from it. HasChange24h is false when
	// the market has not traded in the window, which is different from having
	// moved nothing.
	ReferencePrice24h money.Quantity
	Change24hBPS      money.BPS
	HasChange24h      bool

	CreditVolume24h money.Quantity
	Trades24h       int64

	// LiquidityCredits is V + R, the depth the curve prices against.
	// RealCreditReserve is R alone: the only Credits that could ever leave.
	LiquidityCredits  money.Quantity
	RealCreditReserve money.Quantity

	MaxSupply         money.Quantity
	CirculatingSupply money.Quantity
	AssetDecimals     uint8

	// Demo marks an object a sandbox seeder created (§51, §12).
	Demo bool

	CreatedAt   time.Time
	ActivatedAt *time.Time
}

// MarketPage is one page of summaries with its continuation.
type MarketPage struct {
	Markets    []MarketSummary
	NextCursor string
	// Stable repeats SortBy.Stable for the ordering this page was built with,
	// so a caller does not have to know the rule to render the caveat.
	Stable bool
}

// listCursor is an opaque "(sort key, market id)" position. The key is a
// decimal string because the sort keys are numerics of very different scales --
// an epoch, a Credit amount, a price at eighteen decimal places -- and a float
// would round the large ones into each other.
type listCursor struct {
	Key string `json:"k"`
	ID  string `json:"i"`
}

func encodeListCursor(c listCursor) string {
	b, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeListCursor(s string) (listCursor, bool, error) {
	if strings.TrimSpace(s) == "" {
		return listCursor{}, false, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return listCursor{}, false, errs.New(errs.CodeValidationFailed, "cursor is not a valid pagination cursor")
	}
	var c listCursor
	if err := json.Unmarshal(b, &c); err != nil {
		return listCursor{}, false, errs.New(errs.CodeValidationFailed, "cursor is not a valid pagination cursor")
	}
	if _, ok := new(big.Rat).SetString(c.Key); !ok {
		return listCursor{}, false, errs.New(errs.CodeValidationFailed, "cursor is not a valid pagination cursor")
	}
	if _, err := ParseMarketID(c.ID); err != nil {
		return listCursor{}, false, errs.New(errs.CodeValidationFailed, "cursor is not a valid pagination cursor")
	}
	return c, true, nil
}

// sortKeyExpressions are the SQL each ordering ranks by. They are selected from
// this closed map and never built from caller text.
//
// Every key is a numeric so one cursor shape works for all of them:
//   - NEWEST is epoch seconds with microseconds, which is exactly as precise as
//     the timestamptz it comes from.
//   - PRICE and CHANGE_24H recompute the marginal price the same way the Go
//     curve does -- truncating integer division of (V+R)·10^18 by Y -- so the
//     order the database returns is the order of the prices the response shows.
var sortKeyExpressions = map[SortBy]string{
	SortNewest:    `extract(epoch from m.created_at)::numeric`,
	SortVolume24h: `coalesce(v24.credit_volume, 0)::numeric`,
	SortLiquidity: `(m.virtual_credit_reserve + st.real_credit_reserve)::numeric`,
	SortPrice:     `div((m.virtual_credit_reserve + st.real_credit_reserve) * 1000000000000000000::numeric, st.asset_reserve)`,
	SortChange24h: `CASE WHEN coalesce(p24.reference_price, 0) = 0 THEN -1000000000::numeric ELSE
	                  (div((m.virtual_credit_reserve + st.real_credit_reserve) * 1000000000000000000::numeric, st.asset_reserve)
	                   - p24.reference_price) * 10000 / p24.reference_price END`,
}

// escapeLike neutralises the LIKE metacharacters in a search term.
//
// Without it a query of "%" matches everything and a query of "_" matches every
// single-character symbol, which is a search box that answers a question nobody
// asked. The escape character is declared to PostgreSQL by the ESCAPE clause in
// the query.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// Window24h is how far back the "24h" figures look.
const Window24h = 24 * time.Hour

// ListMarkets returns one page of the markets page.
func (s *Service) ListMarkets(ctx context.Context, q db.Querier, r ListRequest) (MarketPage, error) {
	return s.listMarkets(ctx, q, r, MarketID{}, assets.AssetID{})
}

// listMarkets is ListMarkets with two optional identity filters, so the detail
// row and the list row come out of the same projection and can never disagree
// about a figure.
//
// # Why every filter is a bound parameter rather than an assembled WHERE
//
// test/security proves that no statement this API executes is built from
// anything but source text. A WHERE clause assembled from a filtered list of
// predicates is safe in fact and not PROVABLE, and an unprovable statement in a
// financial system is one somebody has to re-audit by eye at every change. So
// the statement is one constant with every filter present and each one disabled
// by a NULL parameter, and the only part that varies is the sort key, which is
// selected from a closed compiled-in map and never built from caller text.
func (s *Service) listMarkets(ctx context.Context, q db.Querier, r ListRequest, marketFilter MarketID, assetFilter assets.AssetID) (MarketPage, error) {
	if r.Sort == "" {
		r.Sort = SortNewest
	}
	if !r.Sort.Valid() {
		return MarketPage{}, errs.Newf(errs.CodeValidationFailed,
			"unknown sort %q; the sorts are NEWEST, VOLUME_24H, CHANGE_24H, LIQUIDITY and PRICE", r.Sort)
	}
	var statuses []string
	for _, st := range r.Statuses {
		if !st.Valid() {
			return MarketPage{}, errs.Newf(errs.CodeValidationFailed, "unknown market status %q", st)
		}
		statuses = append(statuses, string(st))
	}
	limit := r.Limit
	if limit <= 0 || limit > MaxListLimit {
		limit = 25
	}
	cursor, hasCursor, err := decodeListCursor(r.Cursor)
	if err != nil {
		return MarketPage{}, err
	}

	var creator, market, asset, cursorID any
	if !r.Creator.IsZero() {
		creator = r.Creator
	}
	if !marketFilter.IsZero() {
		market = marketFilter
	}
	if !assetFilter.IsZero() {
		asset = assetFilter
	}
	var plain, like, cursorKey any
	if term := strings.TrimSpace(r.Query); term != "" {
		if len(term) > MaxQueryLength {
			term = term[:MaxQueryLength]
		}
		plain, like = term, escapeLike(term)
	}
	if hasCursor {
		cursorKey, cursorID = cursor.Key, cursor.ID
	}

	now := s.clk.Now().UTC()
	sql := listQueryHead + sortKeyExpressions[r.Sort] + listQueryTail
	rows, err := q.Query(ctx, sql,
		now.Add(-Window24h), statuses, creator, plain, like, market, asset, cursorKey, cursorID, limit+1)
	if err != nil {
		return MarketPage{}, mapError(err)
	}
	defer rows.Close()
	var (
		out  []MarketSummary
		keys []string
	)
	for rows.Next() {
		var (
			key                                   string
			sum                                   MarketSummary
			marketStatus, assetStatus, moderation string
			virt, initial, real, assetRes         string
			maxSupply, vol24, ref24               string
			imageURL                              *string
			platBPS, creBPS                       int
			decimals                              int16
		)
		if err := rows.Scan(&key, &sum.MarketID, &sum.AssetID, &sum.CreditAssetID,
			&marketStatus, &sum.CreatedAt, &sum.ActivatedAt,
			&virt, &initial, &platBPS, &creBPS,
			&real, &assetRes, &sum.State.Version,
			&sum.Name, &sum.Symbol, &sum.Description, &imageURL, &assetStatus, &moderation,
			&sum.CreatorAccountID, &maxSupply, &decimals, &sum.Demo,
			&vol24, &sum.Trades24h, &ref24); err != nil {
			return MarketPage{}, mapError(err)
		}
		if decimals < 0 || decimals > 18 {
			return MarketPage{}, errs.Newf(errs.CodeInternal, "nativemarket: asset decimals %d out of range", decimals)
		}
		sum.AssetDecimals = uint8(decimals)
		sum.MarketStatus = Status(marketStatus)
		sum.AssetStatus = nativeasset.Status(assetStatus)
		sum.Moderation = nativeasset.ModerationState(moderation)
		sum.Fees = Fees{PlatformBPS: money.BPS(platBPS), CreatorBPS: money.BPS(creBPS)}
		if imageURL != nil {
			sum.ImageURL = *imageURL
		}
		for dst, src := range map[*money.Quantity]string{
			&sum.Curve.VirtualCreditReserve: virt, &sum.Curve.InitialAssetReserve: initial,
			&sum.State.RealCreditReserve: real, &sum.State.AssetReserve: assetRes,
			&sum.MaxSupply: maxSupply, &sum.CreditVolume24h: vol24, &sum.ReferencePrice24h: ref24,
		} {
			v, perr := money.ParseQuantity(src)
			if perr != nil {
				return MarketPage{}, errs.Wrap(perr, errs.CodeInternal, "nativemarket: a summary field is not an integer")
			}
			*dst = v
		}
		sum.CreatedAt = sum.CreatedAt.UTC()
		sum.PriceScale = PriceScale
		sum.LastPrice = SpotPrice(sum.Curve, sum.State)
		sum.LiquidityCredits = sum.Curve.VirtualCreditReserve.Add(sum.State.RealCreditReserve)
		sum.RealCreditReserve = sum.State.RealCreditReserve
		sum.CirculatingSupply = sum.Curve.InitialAssetReserve.Sub(sum.State.AssetReserve)
		if sum.ReferencePrice24h.Sign() > 0 {
			sum.HasChange24h = true
			sum.Change24hBPS = changeBPS(sum.ReferencePrice24h, sum.LastPrice)
		}
		out = append(out, sum)
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return MarketPage{}, mapError(err)
	}
	page := MarketPage{Markets: out, Stable: r.Sort.Stable()}
	if len(out) > limit {
		page.Markets = out[:limit]
		last := page.Markets[len(page.Markets)-1]
		page.NextCursor = encodeListCursor(listCursor{Key: keys[limit-1], ID: last.MarketID.String()})
	}
	return page, nil
}

// MaxQueryLength bounds a search term. A longer one is truncated rather than
// refused: somebody who pasted a paragraph into a search box asked a question,
// and the first 128 characters of it are the question.
const MaxQueryLength = 128

// changeBPS is the SIGNED move from a to b, in basis points. Unlike moveBPS it
// keeps the sign, because a market that fell 20% and one that rose 20% are not
// the same market.
func changeBPS(a, b money.Quantity) money.BPS {
	if a.Sign() == 0 {
		return 0
	}
	diff := b.Sub(a)
	scaled := diff.Mul(money.QuantityFromInt64(int64(money.OneHundredPercent)))
	out, err := scaled.Div(a, money.RoundDown)
	if err != nil {
		return 0
	}
	v, err := out.Int64()
	if err != nil {
		if diff.IsNegative() {
			return -1_000 * money.OneHundredPercent
		}
		return 1_000 * money.OneHundredPercent
	}
	return money.BPS(v)
}

const summaryColumns = `m.id AS market_id, m.asset_id, m.credit_asset_id, m.status AS market_status,
	m.created_at, m.activated_at,
	m.virtual_credit_reserve::text AS virt, m.initial_asset_reserve::text AS initial,
	m.platform_fee_bps, m.creator_fee_bps,
	st.real_credit_reserve::text AS real_reserve, st.asset_reserve::text AS asset_reserve, st.version,
	a.name, a.symbol, a.description, a.image_url, a.status AS asset_status,
	a.content_moderation_state, a.creator_account_id, a.max_supply::text AS max_supply,
	reg.decimals,
	(ds.seed_key IS NOT NULL) AS is_demo,
	coalesce(v24.credit_volume, 0)::text AS volume_24h, coalesce(v24.trades, 0) AS trades_24h,
	coalesce(p24.reference_price, 0)::text AS reference_price_24h`

const summaryOut = `r.market_id, r.asset_id, r.credit_asset_id, r.market_status, r.created_at, r.activated_at,
	r.virt, r.initial, r.platform_fee_bps, r.creator_fee_bps,
	r.real_reserve, r.asset_reserve, r.version,
	r.name, r.symbol, r.description, r.image_url, r.asset_status, r.content_moderation_state,
	r.creator_account_id, r.max_supply, r.decimals, r.is_demo,
	r.volume_24h, r.trades_24h, r.reference_price_24h`

// listQueryHead and listQueryTail bracket the sort key, which is the only part
// of this statement that varies and is selected from sortKeyExpressions.
//
// The parameters, in order:
//
//	$1  the start of the 24-hour window
//	$2  statuses to include, NULL for every status
//	$3  a creator to filter to, NULL for every creator
//	$4  a search term for the tsquery, NULL for no text filter
//	$5  the same term with LIKE metacharacters escaped
//	$6  one market id, NULL for every market
//	$7  one asset id, NULL for every asset
//	$8  the cursor's sort key, NULL for the first page
//	$9  the cursor's market id
//	$10 the page size
const listQueryHead = `WITH r AS (
  SELECT `

const listQueryTail = ` AS sort_key, ` + summaryColumns + `
    FROM native_markets m
    JOIN native_market_state st ON st.market_id = m.id
    JOIN native_assets a ON a.asset_id = m.asset_id
    JOIN assets reg ON reg.id = m.asset_id
    LEFT JOIN demo_seed_rows ds ON ds.kind = 'NATIVE_MARKET' AND ds.ref_id = m.id
    LEFT JOIN LATERAL (
      SELECT coalesce(sum(pr.credit_volume), 0) AS credit_volume, count(*) AS trades
        FROM native_market_prints pr
       WHERE pr.market_id = m.id AND pr.printed_at >= $1
    ) v24 ON true
    LEFT JOIN LATERAL (
      SELECT pr.spot_price_before AS reference_price
        FROM native_market_prints pr
       WHERE pr.market_id = m.id AND pr.printed_at >= $1
       ORDER BY pr.printed_at, pr.seq
       LIMIT 1
    ) p24 ON true
   WHERE ($2::text[] IS NULL OR m.status = ANY($2))
     AND ($3::uuid IS NULL OR a.creator_account_id = $3)
     AND ($6::uuid IS NULL OR m.id = $6)
     AND ($7::uuid IS NULL OR m.asset_id = $7)
     AND ($4::text IS NULL OR (
            to_tsvector('simple', a.name || ' ' || a.symbol || ' ' || a.description)
              @@ plainto_tsquery('simple', $4)
         OR upper(a.symbol) LIKE upper($5) || '%' ESCAPE '\'
         OR a.name ILIKE '%' || $5 || '%' ESCAPE '\'
     ))
)
SELECT r.sort_key::text, ` + summaryOut + `
  FROM r
 WHERE ($8::numeric IS NULL OR (r.sort_key, r.market_id) < ($8::numeric, $9::uuid))
 ORDER BY r.sort_key DESC, r.market_id DESC
 LIMIT $10`

// MarketSummaryByID returns one market's summary row, for the asset detail
// screen (§13).
func (s *Service) MarketSummaryByID(ctx context.Context, q db.Querier, marketID MarketID) (MarketSummary, error) {
	page, err := s.listMarkets(ctx, q, ListRequest{Sort: SortNewest, Limit: 1}, marketID, assets.AssetID{})
	if err != nil {
		return MarketSummary{}, err
	}
	if len(page.Markets) == 0 {
		return MarketSummary{}, errs.New(errs.CodeNotFound, "native market not found").
			WithField("market_id", marketID.String())
	}
	return page.Markets[0], nil
}

// MarketSummaryByAsset is MarketSummaryByID for an asset id.
func (s *Service) MarketSummaryByAsset(ctx context.Context, q db.Querier, assetID assets.AssetID) (MarketSummary, error) {
	page, err := s.listMarkets(ctx, q, ListRequest{Sort: SortNewest, Limit: 1}, MarketID{}, assetID)
	if err != nil {
		return MarketSummary{}, err
	}
	if len(page.Markets) == 0 {
		return MarketSummary{}, errs.New(errs.CodeNotFound, "this asset has no market").
			WithField("asset_id", assetID.String())
	}
	return page.Markets[0], nil
}
