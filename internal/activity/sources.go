package activity

import "strings"

// Source is one domain's contribution to the feed.
//
// SQL is one branch of feedQuery's union. Adding a domain to the timeline is
// three edits and nothing else:
//
//  1. a Kind in activity.go,
//  2. a `const src…` below selecting sourceColumns for the account in $1, with
//     the kind predicate that lets PostgreSQL skip the branch when a caller has
//     filtered it out,
//  3. its entry in `sources`, its name in feedQuery's concatenation, and a
//     template in summary.go.
//
// TestSourcesAreWellFormed holds every source to the column contract and
// TestEveryKindHasASource makes a Kind without one a failure, so a half-added
// domain is a red test rather than an empty feed.
//
// # Why the query is one compiled-in constant and not built per request
//
// test/security's TestSQLInjection_EveryStatementIsBuiltFromConstants proves
// that no statement the API can execute is assembled from anything but source
// text. A union assembled at request time from a filtered slice is safe in fact
// -- the parts are all package constants -- and not PROVABLE by a scanner, and
// an unprovable statement in a financial system is one somebody has to re-audit
// by eye every time it changes. So the union is a constant, every branch is
// always present, and the kind filter is a bound parameter each branch tests
// against its own literal kind. PostgreSQL evaluates that as a one-time filter
// and skips the branch, so the filtered read still touches one table.
type Source struct {
	Kind Kind
	SQL  string
}

// sourceColumns is the contract every branch's projection satisfies.
//
// The types are pinned by casts in each branch because a UNION ALL takes the
// type of its first branch: an uncast NULL in one and a bigint in another is a
// query that works until somebody reorders the concatenation.
//
//	kind            text         the Kind this branch produces
//	occurred_at     timestamptz  when it happened, by the domain's own clock
//	id              uuid         the domain row's id; the cursor's tiebreak
//	ref_type        text         what kind of row it is
//	ref_id          text         its id, as text
//	status          text         the domain's state, '' where it has none
//	credits         numeric      Credit base units, 0 where none
//	money_minor     bigint       provider money in minor units, 0 where none
//	currency        text         '' unless money_minor is meaningful
//	origin          text         Credit provenance where known, '' otherwise
//	side            text         BUY / SELL for a trade, '' otherwise
//	symbol          text         a native asset's ticker, '' otherwise
//	asset_units     numeric      base units of that asset, 0 otherwise
//	demo            boolean      the object was created by a demo seeder
const sourceColumns = `kind, occurred_at, id, ref_type, ref_id, status, credits, money_minor,
	currency, origin, side, symbol, asset_units, demo`

// kindFilter is the predicate every branch carries. $2 is the caller's kind
// list, or NULL for "every kind".
const kindFilter = ` AND ($2::text[] IS NULL OR `

const srcCreditPurchase = `
	SELECT 'CREDIT_PURCHASE'::text AS kind, f.created_at AS occurred_at, f.id,
	       'credit_funding'::text AS ref_type, f.id::text AS ref_id, f.state::text AS status,
	       f.credit_quantity::numeric AS credits, f.paid_amount_minor::bigint AS money_minor,
	       f.paid_currency::text AS currency, 'PURCHASED'::text AS origin,
	       ''::text AS side, ''::text AS symbol, 0::numeric AS asset_units, false AS demo
	  FROM credit_fundings f
	 WHERE f.account_id = $1` + kindFilter + `'CREDIT_PURCHASE' = ANY($2))`

const srcCreditReversal = `
	SELECT 'CREDIT_REVERSAL'::text AS kind, t.occurred_at, t.id,
	       'credit_funding'::text AS ref_type, t.funding_id::text AS ref_id, t.to_state::text AS status,
	       f.credit_quantity::numeric AS credits, f.paid_amount_minor::bigint AS money_minor,
	       f.paid_currency::text AS currency, 'PURCHASED'::text AS origin,
	       ''::text AS side, ''::text AS symbol, 0::numeric AS asset_units, false AS demo
	  FROM credit_funding_transitions t
	  JOIN credit_fundings f ON f.id = t.funding_id
	 WHERE f.account_id = $1
	   AND t.to_state IN ('REVERSED','REFUNDED','DISPUTED')` + kindFilter + `'CREDIT_REVERSAL' = ANY($2))`

const srcNativeTrade = `
	SELECT 'NATIVE_TRADE'::text AS kind, fl.created_at AS occurred_at, fl.id,
	       'native_market_fill'::text AS ref_type, fl.id::text AS ref_id, ''::text AS status,
	       (CASE WHEN fl.side = 'BUY' THEN fl.credits_in ELSE fl.credits_out END)::numeric AS credits,
	       0::bigint AS money_minor, ''::text AS currency,
	       (CASE WHEN fl.side = 'BUY' THEN '' ELSE 'MARKET_TRADING_PROCEEDS' END)::text AS origin,
	       fl.side::text AS side, na.symbol::text AS symbol,
	       (CASE WHEN fl.side = 'BUY' THEN fl.assets_out ELSE fl.assets_in END)::numeric AS asset_units,
	       (ds.seed_key IS NOT NULL) AS demo
	  FROM native_market_fills fl
	  JOIN native_markets nm ON nm.id = fl.market_id
	  JOIN native_assets na ON na.asset_id = nm.asset_id
	  LEFT JOIN demo_seed_rows ds ON ds.kind = 'NATIVE_MARKET' AND ds.ref_id = nm.id
	 WHERE fl.account_id = $1` + kindFilter + `'NATIVE_TRADE' = ANY($2))`

const srcNativeAssetCreated = `
	SELECT 'NATIVE_ASSET_CREATED'::text AS kind, na.created_at AS occurred_at, na.asset_id AS id,
	       'native_asset'::text AS ref_type, na.asset_id::text AS ref_id, na.status::text AS status,
	       0::numeric AS credits, 0::bigint AS money_minor, ''::text AS currency,
	       ''::text AS origin, ''::text AS side, na.symbol::text AS symbol,
	       na.max_supply::numeric AS asset_units, (ds.seed_key IS NOT NULL) AS demo
	  FROM native_assets na
	  LEFT JOIN demo_seed_rows ds ON ds.kind = 'NATIVE_ASSET' AND ds.ref_id = na.asset_id
	 WHERE na.creator_account_id = $1` + kindFilter + `'NATIVE_ASSET_CREATED' = ANY($2))`

const srcPayoutRequested = `
	SELECT 'PAYOUT_REQUESTED'::text AS kind, pr.created_at AS occurred_at, pr.id,
	       'payout_request'::text AS ref_type, pr.id::text AS ref_id, pr.state::text AS status,
	       pr.requested_quantity::numeric AS credits, 0::bigint AS money_minor,
	       ''::text AS currency, ''::text AS origin, ''::text AS side,
	       ''::text AS symbol, 0::numeric AS asset_units, false AS demo
	  FROM payout_requests pr
	 WHERE pr.account_id = $1` + kindFilter + `'PAYOUT_REQUESTED' = ANY($2))`

const srcPayoutStateChanged = `
	SELECT 'PAYOUT_STATE_CHANGED'::text AS kind, t.occurred_at, t.id,
	       'payout_request'::text AS ref_type, t.request_id::text AS ref_id, t.to_state::text AS status,
	       pr.requested_quantity::numeric AS credits, 0::bigint AS money_minor,
	       ''::text AS currency, ''::text AS origin, ''::text AS side,
	       ''::text AS symbol, 0::numeric AS asset_units, false AS demo
	  FROM payout_request_transitions t
	  JOIN payout_requests pr ON pr.id = t.request_id
	 WHERE pr.account_id = $1` + kindFilter + `'PAYOUT_STATE_CHANGED' = ANY($2))`

const srcAdminAdjustment = `
	SELECT 'ADMIN_ADJUSTMENT'::text AS kind, cl.created_at AS occurred_at, cl.id,
	       'credit_lot'::text AS ref_type, cl.id::text AS ref_id, cl.initial_finality::text AS status,
	       cl.quantity::numeric AS credits, 0::bigint AS money_minor,
	       ''::text AS currency, cl.origin::text AS origin, ''::text AS side,
	       ''::text AS symbol, 0::numeric AS asset_units, false AS demo
	  FROM credit_lots cl
	 WHERE cl.account_id = $1 AND cl.origin = 'ADMIN_ADJUSTMENT'` + kindFilter + `'ADMIN_ADJUSTMENT' = ANY($2))`

// unionAll separates the branches.
const unionAll = "\n  UNION ALL\n"

// feedQuery is the whole statement, compiled in.
//
// $1 account, $2 kinds (NULL for all), $3 cursor instant, $4 cursor id,
// $5 limit. The cursor is a keyset over (occurred_at, id), which is why every
// branch supplies both and why the tiebreak is a uuid rather than an offset:
// an OFFSET moves under an insert and a keyset does not.
const feedQuery = `WITH items AS (` +
	srcCreditPurchase + unionAll +
	srcCreditReversal + unionAll +
	srcNativeTrade + unionAll +
	srcNativeAssetCreated + unionAll +
	srcPayoutRequested + unionAll +
	srcPayoutStateChanged + unionAll +
	srcAdminAdjustment + `
)
SELECT kind, occurred_at, id, ref_type, ref_id, status,
       credits::text, money_minor, currency, origin, side, symbol, asset_units::text, demo
  FROM items
 WHERE (occurred_at, id) < ($3, $4)
 ORDER BY occurred_at DESC, id DESC
 LIMIT $5`

// sources is the registry, in Kind declaration order so a reader can check the
// two lists against each other.
var sources = []Source{
	{Kind: KindCreditPurchase, SQL: srcCreditPurchase},
	{Kind: KindCreditReversal, SQL: srcCreditReversal},
	{Kind: KindNativeTrade, SQL: srcNativeTrade},
	{Kind: KindNativeAssetCreated, SQL: srcNativeAssetCreated},
	{Kind: KindPayoutRequested, SQL: srcPayoutRequested},
	{Kind: KindPayoutStateChanged, SQL: srcPayoutStateChanged},
	{Kind: KindAdminAdjustment, SQL: srcAdminAdjustment},
}

// Sources returns the registry (a copy), so a test can hold every source to the
// column contract without this slice being writable from outside.
func Sources() []Source { return append([]Source(nil), sources...) }

// Query returns the compiled-in statement, for the test that proves every
// source is actually in it.
func Query() string { return feedQuery }

// columnNames is sourceColumns split into the identifiers it names, for the
// test that holds every branch to the contract.
func columnNames() []string {
	var out []string
	for _, f := range strings.Split(sourceColumns, ",") {
		if name := strings.TrimSpace(f); name != "" {
			out = append(out, name)
		}
	}
	return out
}
