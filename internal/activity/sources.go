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
//	demo            boolean      the item is about a simulated object: one a demo
//	                             seeder created, or one the domain that wrote it
//	                             labelled sandbox (ADR-0023). Both mean the same
//	                             thing to a reader -- nothing here moved anywhere.
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

// ---------------------------------------------------------------------------
// The product domains (D-081).
//
// Six of these read tables keyed on a USER rather than an account, so each one
// joins `accounts` on the owner and pins it to the account in $1. Pinning
// rather than filtering matters: a person with three accounts would otherwise
// see three copies of one verification decision.
//
// Where the domain that wrote the row labelled it sandbox -- a verification
// session, a payout destination -- that label is the `demo` column, because to
// a reader "a demo seeder made this" and "this was a rehearsal" are the same
// fact: nothing moved anywhere. The rest report false, and a sandbox tier
// stamps the whole feed anyway (Feed.simulated).
// ---------------------------------------------------------------------------

const srcVerificationUpdated = `
	SELECT 'VERIFICATION_UPDATED'::text AS kind, t.occurred_at AS occurred_at, t.id AS id,
	       'compliance_profile'::text AS ref_type, t.user_id::text AS ref_id, t.to_state::text AS status,
	       0::numeric AS credits, 0::bigint AS money_minor, ''::text AS currency,
	       ''::text AS origin, ''::text AS side, ''::text AS symbol, 0::numeric AS asset_units,
	       coalesce(vs.sandbox, false) AS demo
	  FROM compliance_profile_transitions t
	  JOIN accounts acc ON acc.owner_user_id = t.user_id AND acc.id = $1
	  LEFT JOIN verification_sessions vs ON vs.id = t.session_id
	 WHERE acc.id = $1` + kindFilter + `'VERIFICATION_UPDATED' = ANY($2))`

const srcPayoutDestinationAdded = `
	SELECT 'PAYOUT_DESTINATION_ADDED'::text AS kind, pd.created_at AS occurred_at, pd.id AS id,
	       'payout_destination'::text AS ref_type, pd.id::text AS ref_id, pd.kind::text AS status,
	       0::numeric AS credits, 0::bigint AS money_minor, coalesce(pd.currency, '')::text AS currency,
	       ''::text AS origin, ''::text AS side, ''::text AS symbol, 0::numeric AS asset_units,
	       pd.sandbox AS demo
	  FROM payout_destinations pd
	 WHERE pd.account_id = $1` + kindFilter + `'PAYOUT_DESTINATION_ADDED' = ANY($2))`

const srcPayoutDestinationDisabled = `
	SELECT 'PAYOUT_DESTINATION_DISABLED'::text AS kind, t.occurred_at AS occurred_at, t.id AS id,
	       'payout_destination'::text AS ref_type, t.destination_id::text AS ref_id, t.to_status::text AS status,
	       0::numeric AS credits, 0::bigint AS money_minor, coalesce(pd.currency, '')::text AS currency,
	       ''::text AS origin, ''::text AS side, ''::text AS symbol, 0::numeric AS asset_units,
	       pd.sandbox AS demo
	  FROM payout_destination_transitions t
	  JOIN payout_destinations pd ON pd.id = t.destination_id
	 WHERE pd.account_id = $1
	   AND t.to_status IN ('DISABLED','REJECTED')` + kindFilter + `'PAYOUT_DESTINATION_DISABLED' = ANY($2))`

const srcTermsAccepted = `
	SELECT 'TERMS_ACCEPTED'::text AS kind, ta.accepted_at AS occurred_at, ta.id AS id,
	       'terms_acceptance'::text AS ref_type, ta.id::text AS ref_id, ta.document_id::text AS status,
	       0::numeric AS credits, 0::bigint AS money_minor, ''::text AS currency,
	       ''::text AS origin, ''::text AS side, ''::text AS symbol, 0::numeric AS asset_units,
	       false AS demo
	  FROM terms_acceptances ta
	  JOIN accounts acc ON acc.owner_user_id = ta.user_id AND acc.id = $1
	 WHERE acc.id = $1` + kindFilter + `'TERMS_ACCEPTED' = ANY($2))`

const srcAccountClosureRequested = `
	SELECT 'ACCOUNT_CLOSURE_REQUESTED'::text AS kind, cr.requested_at AS occurred_at, cr.id AS id,
	       'account_closure_request'::text AS ref_type, cr.id::text AS ref_id, 'PENDING'::text AS status,
	       0::numeric AS credits, 0::bigint AS money_minor, ''::text AS currency,
	       ''::text AS origin, ''::text AS side, ''::text AS symbol, 0::numeric AS asset_units,
	       false AS demo
	  FROM account_closure_requests cr
	  JOIN accounts acc ON acc.owner_user_id = cr.user_id AND acc.id = $1
	 WHERE acc.id = $1` + kindFilter + `'ACCOUNT_CLOSURE_REQUESTED' = ANY($2))`

const srcAccountClosureDecided = `
	SELECT 'ACCOUNT_CLOSURE_DECIDED'::text AS kind, t.occurred_at AS occurred_at, t.id AS id,
	       'account_closure_request'::text AS ref_type, t.request_id::text AS ref_id, t.to_state::text AS status,
	       0::numeric AS credits, 0::bigint AS money_minor, ''::text AS currency,
	       ''::text AS origin, ''::text AS side, ''::text AS symbol, 0::numeric AS asset_units,
	       false AS demo
	  FROM account_closure_request_transitions t
	  JOIN account_closure_requests cr ON cr.id = t.request_id
	  JOIN accounts acc ON acc.owner_user_id = cr.user_id AND acc.id = $1
	 WHERE acc.id = $1` + kindFilter + `'ACCOUNT_CLOSURE_DECIDED' = ANY($2))`

// The agent's NAME is deliberately absent from every agent branch. It is
// owner-supplied text bounded only by a length CHECK, and summary.go builds a
// sentence: a symbol has passed through nativeasset.Screen and an agent name
// has not, so the sentence says what happened and the reference says which
// agent it happened to.
const srcAgentCreated = `
	SELECT 'AGENT_CREATED'::text AS kind, ag.created_at AS occurred_at, ag.id AS id,
	       'agent'::text AS ref_type, ag.id::text AS ref_id, ag.state::text AS status,
	       0::numeric AS credits, 0::bigint AS money_minor, ''::text AS currency,
	       ''::text AS origin, ''::text AS side, ''::text AS symbol, 0::numeric AS asset_units,
	       false AS demo
	  FROM agents ag
	 WHERE ag.account_id = $1` + kindFilter + `'AGENT_CREATED' = ANY($2))`

const srcAgentPaused = `
	SELECT 'AGENT_PAUSED'::text AS kind, ap.paused_at AS occurred_at, ap.id AS id,
	       'agent'::text AS ref_type, ap.agent_id::text AS ref_id, ap.reason_code::text AS status,
	       0::numeric AS credits, 0::bigint AS money_minor, ''::text AS currency,
	       ''::text AS origin, ''::text AS side, ''::text AS symbol, 0::numeric AS asset_units,
	       false AS demo
	  FROM agent_pauses ap
	  JOIN agents ag ON ag.id = ap.agent_id
	 WHERE ag.account_id = $1` + kindFilter + `'AGENT_PAUSED' = ANY($2))`

const srcAgentResumed = `
	SELECT 'AGENT_RESUMED'::text AS kind, ap.resumed_at AS occurred_at, ap.id AS id,
	       'agent'::text AS ref_type, ap.agent_id::text AS ref_id, ap.reason_code::text AS status,
	       0::numeric AS credits, 0::bigint AS money_minor, ''::text AS currency,
	       ''::text AS origin, ''::text AS side, ''::text AS symbol, 0::numeric AS asset_units,
	       false AS demo
	  FROM agent_pauses ap
	  JOIN agents ag ON ag.id = ap.agent_id
	 WHERE ag.account_id = $1
	   AND ap.resumed_at IS NOT NULL` + kindFilter + `'AGENT_RESUMED' = ANY($2))`

const srcAgentDisabled = `
	SELECT 'AGENT_DISABLED'::text AS kind, t.occurred_at AS occurred_at, t.id AS id,
	       'agent'::text AS ref_type, t.agent_id::text AS ref_id, t.to_state::text AS status,
	       0::numeric AS credits, 0::bigint AS money_minor, ''::text AS currency,
	       ''::text AS origin, ''::text AS side, ''::text AS symbol, 0::numeric AS asset_units,
	       false AS demo
	  FROM agent_lifecycle_transitions t
	  JOIN agents ag ON ag.id = t.agent_id
	 WHERE ag.account_id = $1
	   AND t.to_state = 'REVOKED'` + kindFilter + `'AGENT_DISABLED' = ANY($2))`

// A market pause reaches the people it can cost something: anyone who has
// traded it, and the creator of its asset, who may never have traded their own
// market and is the person a delisting matters most to. It is the population
// internal/notifications tells, plus that creator.
const srcNativeMarketPaused = `
	SELECT 'NATIVE_MARKET_PAUSED'::text AS kind, t.occurred_at AS occurred_at, t.id AS id,
	       'native_market'::text AS ref_type, t.market_id::text AS ref_id, t.to_status::text AS status,
	       0::numeric AS credits, 0::bigint AS money_minor, ''::text AS currency,
	       ''::text AS origin, ''::text AS side, na.symbol::text AS symbol, 0::numeric AS asset_units,
	       (ds.seed_key IS NOT NULL) AS demo
	  FROM native_market_transitions t
	  JOIN native_markets nm ON nm.id = t.market_id
	  JOIN native_assets na ON na.asset_id = nm.asset_id
	  LEFT JOIN demo_seed_rows ds ON ds.kind = 'NATIVE_MARKET' AND ds.ref_id = nm.id
	 WHERE t.to_status IN ('CLOSE_ONLY','HALTED','FROZEN','DELISTED')
	   AND (na.creator_account_id = $1
	        OR EXISTS (SELECT 1 FROM native_market_fills fl
	                    WHERE fl.market_id = t.market_id AND fl.account_id = $1))` +
	kindFilter + `'NATIVE_MARKET_PAUSED' = ANY($2))`

// unionAll separates the branches.
const unionAll = "\n  UNION ALL\n"

// feedQuery is the whole statement, compiled in.
//
// $1 account, $2 kinds (NULL for all), $3 cursor instant, $4 cursor id,
// $5 limit. The cursor is a keyset over (occurred_at, id), which is why every
// branch supplies both and why the tiebreak is a uuid rather than an offset:
// an OFFSET moves under an insert and a keyset does not.
// The branches, in three groups.
//
// The grouping is for the READER first -- what happened to the value, what
// happened to the account's standing, what happened to the things it runs --
// and it is also what keeps test/security's constant-expression scanner able to
// prove the statement. That analysis walks the concatenation tree and gives up
// past a depth of 32, and `a + b + c + ...` nests once per operand, so a single
// flat chain of eighteen branches and seventeen separators is a statement
// nothing can prove. Three shallow chains joined by a shallow one is the same
// string and a provable one.
const feedValueSources = srcCreditPurchase + unionAll +
	srcCreditReversal + unionAll +
	srcNativeTrade + unionAll +
	srcNativeAssetCreated + unionAll +
	srcPayoutRequested + unionAll +
	srcPayoutStateChanged + unionAll +
	srcAdminAdjustment

const feedAccountSources = srcVerificationUpdated + unionAll +
	srcPayoutDestinationAdded + unionAll +
	srcPayoutDestinationDisabled + unionAll +
	srcTermsAccepted + unionAll +
	srcAccountClosureRequested + unionAll +
	srcAccountClosureDecided

const feedAgentSources = srcAgentCreated + unionAll +
	srcAgentPaused + unionAll +
	srcAgentResumed + unionAll +
	srcAgentDisabled + unionAll +
	srcNativeMarketPaused

const feedQuery = `WITH items AS (` +
	feedValueSources + unionAll +
	feedAccountSources + unionAll +
	feedAgentSources + `
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
	{Kind: KindVerificationUpdated, SQL: srcVerificationUpdated},
	{Kind: KindPayoutDestinationAdded, SQL: srcPayoutDestinationAdded},
	{Kind: KindPayoutDestinationDisabled, SQL: srcPayoutDestinationDisabled},
	{Kind: KindTermsAccepted, SQL: srcTermsAccepted},
	{Kind: KindAccountClosureRequested, SQL: srcAccountClosureRequested},
	{Kind: KindAccountClosureDecided, SQL: srcAccountClosureDecided},
	{Kind: KindAgentCreated, SQL: srcAgentCreated},
	{Kind: KindAgentPaused, SQL: srcAgentPaused},
	{Kind: KindAgentResumed, SQL: srcAgentResumed},
	{Kind: KindAgentDisabled, SQL: srcAgentDisabled},
	{Kind: KindNativeMarketPaused, SQL: srcNativeMarketPaused},
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
