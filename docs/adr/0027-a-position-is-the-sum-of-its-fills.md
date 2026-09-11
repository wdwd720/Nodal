# ADR-0027 — A native position is the sum of its fills, and market safety is a policy somebody wrote down

Status: **Accepted** (2026-09-10)

Supersedes nothing. Constrains how the Nodal-native economy reports what a
person holds, what it was worth, and what would refuse their next trade.
Answers §§11–16, §35, §46, §47 and §51 of the product goal.

## Context

The internal market engine was complete and had no product around it.
`internal/nativemarket` priced and executed trades against a constant-product
curve with a virtual Credit reserve, the database enforced the invariant on
every fill, and `internal/credits` tracked the provenance of every Credit. What
did not exist:

- **No positions.** `position_lots` (migration 00104) is the hosted rail's
  tax-quality FIFO lot ledger and holds its basis in `bigint` **USD minor
  units**. A Credit-denominated native position cannot be expressed in it
  without inventing an exchange rate for a Credit, which §10 and
  `internal/valuedomain` forbid. Nothing else recorded what an account had paid
  for a native asset, so §15's portfolio — cost basis, realised and unrealised
  P&L, fees — had no source at all.
- **No price series a chart can read.** `asset_prices` (00105) carries this
  market's post-trade spot price, and it is what the prediction resolver reads,
  but it carries no volume and links to the trade that set it only through a
  text `raw_ref`. "What were the open, high, low and close of this market
  between 09:00 and 09:01" was unanswerable.
- **No discovery.** `GET /native-assets` returned assets that accept at least
  sells, with no price, no volume, no liquidity, no search, no sort and no
  cursor.
- **No timeline.** `GET /accounts/{id}/activity` reads the hosted rail's
  operational tables — intents, orders, venue fills, deposits, journal
  transactions, reconciliation records. None of §16's product events are in it.
- **Three of §47's limbs unbounded.** The risk kernel refuses a BUY that leaves
  one account holding too large a share of an asset's supply, or too much of its
  Credits with one creator. `surveillance.go` raises alerts for wash trading,
  rapid round-tripping, creator self-dealing and concentration, and deliberately
  does not block. Nothing bounded price impact, nothing set a floor under the
  liquidity a market may open with, and no automatic mechanism ever stopped a
  market that was moving violently.
- **No demo data**, so STAGING's markets page was empty and §12's "seed clearly
  marked sandbox/demo assets" had nothing behind it.

## Decision

### 1. Positions and the price series are DERIVED READ MODELS the database maintains

`native_positions` (migration 00772) and `native_market_prints` (00771) are
written by triggers on `native_market_fills` and, for the creator's allocation,
on `native_markets`. `cp_app` holds `SELECT` on `native_positions` and nothing
else, exactly as for `native_market_state` (00712): there is no application path
by which a cost basis can be set to something the trades do not support.

Three things stop them drifting from the books they restate:

1. A table CHECK states the invariant in SQL —
   `quantity = allocation_units + units_bought_total - units_sold_total` — so
   "the quantity is the sum of its fills" is enforced on every write rather than
   asserted in a comment. There are exactly two ways a customer acquires a
   native asset unit (the creator allocation minted at market creation, and a
   market fill), so the sum is complete.
2. `cp_native_positions_unreconciled()` compares every row against
   `ledger_balances`, the independent source neither the table nor its triggers
   write. It returns the disagreements and never repairs one side to match the
   other, the same treatment `VerifyReserves` gives the Credit reserve.
3. A print is validated against its fill. `cp_native_market_check_print()`
   recovers the pre-trade reserves from the fill's own amounts, recomputes all
   three prices with PostgreSQL's truncating integer division — the operation
   `nativemarket.ratioScaled` performs — and raises NM001 on any disagreement.
   A print cannot say the market traded somewhere it did not, whoever inserts it.

**Cost basis is AVERAGE, not FIFO.** The hosted rail uses FIFO because a tax
report needs lots. A native asset is closed-loop, non-redeemable internal value
with no tax event and no external price, so what a holder needs is one number
that cannot be gamed by the order in which sells are reported. Average cost also
makes the invariant above a single arithmetic identity rather than a join across
lots, and `native_market_fills` keeps the full history for anything that needs
the lots.

**The arithmetic is stated twice, in two languages.** `Position.ApplyBuy` and
`ApplySell` restate migration 00772's triggers in Go. Nothing in production
calls them; `TestIntegration_PositionsAgreeWithTheGoStatementOfTheArithmetic`
drives real fills through the engine and compares what the database wrote
against what the Go produced. Either statement alone proves only that it agrees
with itself. The rounding is the part worth stating twice: a partial exit
removes the truncated share of the basis so the basis left is never short of the
units left, and a full exit removes all of it so a closed position keeps no cost
attached to nothing.

**Unrealised P&L is marked at the MARGINAL price and the response says when.**
Market value and unrealised P&L are computed at read time from the market's own
reserves, because they depend on a price every other trader can move; storing
them would mean rewriting every holder's row on every fill and the row would
still be stale between fills. Marking at the marginal price is optimistic for a
large holder — selling moves the price down — and it is still the honest choice,
because the alternative, marking each holder at their own exit price, means no
two portfolio pages agree about the same asset. The order ticket quotes the real
exit against current state, which is where a user finds out what their exit is
worth. `GET /v1/me/portfolio` carries an explicit `as_of`.

### 2. Charts are computed from prints, and a gap stays a gap

`GET /v1/native-markets/{id}/candles` computes OHLCV in SQL over a window
bounded to 1,500 buckets, aligned by `date_bin` to a fixed epoch so two requests
over overlapping windows agree bucket for bucket. The open is the pre-trade spot
of the bucket's first print and the close the post-trade spot of its last; the
high and low look at **both** sides of every print, because between two trades
the market sat at a price nobody traded at and a candle built from trade prices
alone understates the range.

**Buckets with no trades are absent, never filled forward.** §14 asks for an
honest empty state when there is not enough history, and a flat-filled candle
invents a trade that did not happen.

### 3. Market safety is its own versioned policy, because it is about a VENUE

Four limits (migration 00773, `native_market_safety_policies`): a per-order
price-impact ceiling, a per-order slippage ceiling, a circuit breaker (a move
past a threshold within a window), a floor under the virtual reserve a market
may open with, and whether a creator may buy their own asset.

They are **not** `risk.Policy` columns. A risk policy composes
GLOBAL ∧ ACCOUNT ∧ AGENT and answers "how much risk may this ACCOUNT take".
Three of these belong to a market: the breaker to the venue, the opening floor
to the market being opened, and the creator rule to a relationship between an
account and an asset. Composing them per account would let an account-scoped row
loosen a venue rule, which is the wrong direction for every one of them. They
get their own document with the same discipline — one row per version,
immutable, hashed over its canonical rendering, chosen by `effective_at`,
recorded with an actor and a reason. The two risk-kernel concentration limits
stay where they are and both documents are reported together as "limits in
force" on the asset detail response, which is §47's *expose* limb made literal.

Three sub-decisions inside this one:

- **Impact and slippage bind a BUY only.** Both numbers describe the cost of the
  caller's own size. Refusing an exit because the exit is large traps the holder,
  and a control that can trap a holder is worse than the manipulation it
  prevents. A seller's protection against their own size is `MinOutput`, which
  they set and the engine honours. It is the same reasoning `risk.go` already
  applies to the concentration limits.
- **A breaker pauses to CLOSE_ONLY, not HALTED**, so the runaway stops (no new
  exposure) and every holder can still sell. The trade that trips it **stands**:
  it was legal when it was priced, and refusing it after the fact would make the
  user's own confirmation a lie. What a breaker stops is the next one.
- **The refusal code is `VENUE_LIQUIDITY_INSUFFICIENT`.** On a constant-product
  pool, "this order moves the price too far" and "there is not enough liquidity
  for this size" are the same measurement seen from two sides, and that code
  already means the second. No new code was added.

### 4. A compiled-in conservative policy, with two deliberately permissive defaults

A deployment that has recorded no safety policy runs
`ConservativeSafetyPolicy()`, which is a complete policy rather than an absence —
the same shape as `valuedomain.DefaultPolicy` and `legalrouter.ConservativePolicy`.
Two of its values are not the strict ones, and both are recorded rather than
quietly chosen:

- **The circuit breaker is DISARMED** (`circuit_breaker_move_bps: 0`). A trip
  moves a market to CLOSE_ONLY and only a person moves it back. A freshly
  launched constant-product market on a virtual reserve legitimately moves
  several hundred percent in minutes — the first few buyers *are* the price
  discovery — so any threshold low enough to catch manipulation catches every
  launch, and a deployment with nobody watching would strand its holders on the
  first good day. The mechanism is built, enforced and tested at its edges; a
  deployment with an operator arms it with `scripts/marketsafety`.
- **A creator may buy their own asset.** `internal/nativemarket` already decided
  that question in `doc.go`: an automated market maker has no order book, so the
  counterparty is always the pool and there is no matched self-trade to prevent;
  what exists is a pattern, and "a detector that halts a market on a heuristic is
  a denial-of-service vector against creators". `surveillance.go` raises
  `CREATOR_SELF_DEALING` and does not block, and
  `TestIntegration_SurveillanceRaisesAlertsWithoutBlocking` holds that line. §47
  says "prevent **or expose**"; this build exposes, and a deployment that would
  rather prevent records a policy with the flag false, whereupon every creator
  BUY is refused with `ASSET_RESTRICTED`.

The price-impact and slippage ceilings are 9,000 basis points, which on this
curve means one order may be at most about 38% of the pool's effective reserve:
impact is `(x'/x)² − 1`, so 9,000 bps is `x'/x = 1.378`.

### 5. The activity timeline owns no table

`internal/activity` reads other domains' tables into one reverse-chronological
feed. It writes nothing and derives nothing: every item is a row somebody else
wrote inside the transaction that made it true, so a feed entry cannot exist for
something that did not happen and cannot be missing for something that did. An
activity table fed by events would be a second copy of the financial record with
its own failure mode — a transaction that commits and an event that does not.

The union is **one compiled-in constant** and the kind filter is a bound
parameter each branch tests against its own literal kind, which PostgreSQL
evaluates as a one-time filter. A union assembled per request from a filtered
slice is safe in fact and not *provable* by
`TestSQLInjection_EveryStatementIsBuiltFromConstants`, and an unprovable
statement in a financial system is one somebody has to re-audit by eye at every
change. `nativemarket`'s market-discovery statement is built the same way: every
filter is present and disabled by a NULL parameter, and the only part that
varies is a sort key selected from a closed compiled-in map.

Every amount carries its **unit** and its **temperature** — `ECONOMY` for
closed-loop Credits and native assets, `REAL` for money at a payment provider,
`SIMULATED` on a sandbox tier or for a demo object, with `SIMULATED` winning over
both. §46 forbids showing different kinds of value as synonyms, and a client that
must render the unit can render the number, which is why the server-built summary
sentence names the action and the fields carry the amounts.

### 6. Demo data goes through the front door

`internal/demo` creates its catalogue through `nativeasset.CreateDraft` →
`SetStatus` → `Activate` → `nativemarket.Create` → `SetStatus` →
`credit.Issue` → `nativemarket.Execute`: content screening, the moderation
verdict, the single mint, the ledger posting, the risk kernel, the safety policy
and the constant-product trigger. The only raw SQL is the INSERT into
`demo_seed_rows`, which is the label, not the object. It refuses PROD three
times — in `NewSeeder`, in `cmd/api`, and in migration 00774's CHECK — and runs
only on a sandbox tier.

Demo Credits are `PROMOTIONAL`, which no payout policy in this build permits to
be withdrawn, including the sandbox one. So they can be spent inside the product
and can never leave it, which is what demo money should be. The moderation
verdict is the screener's: the demo copy is written to pass on its own and the
seeder refuses to continue if it does not, because approving its own content
would be manufacturing a moderation decision.

## Consequences

- Seven routes: `GET /v1/native-markets`, `.../{id}/summary`, `.../{id}/candles`,
  `.../{id}/trades`, `GET /v1/me/portfolio`, `GET /v1/me/activity`.
  `GET /v1/accounts/{id}/activity` is unchanged and stays the hosted rail's
  operational timeline (D-066).
- `internal/nativemarket.Execute` now writes a print and evaluates the safety
  policy inside the trade's own transaction, and `checkRisk` reports whether the
  account can pay so an order larger than the balance keeps
  `LEDGER_NEGATIVE_BALANCE` as its reason rather than acquiring a second one.
- A non-PROD deployment records the compiled-in GLOBAL risk policy at boot if it
  has none, through `risk.Store.RecordPolicy` as the SYSTEM actor
  `config:bootstrap`. PROD stays manual, because those numbers are starter
  values nobody signed off and a control that LOOKS decided is worse than none
  (D-067).
- The restore drill and `docs/operations/BACKUP_RESTORE.md` need their schema
  version claim updated to 775 after the merge.

## Evidence

Migrations 00771–00775; `internal/nativemarket/{prints,positions,discovery,safety}.go`
and their tests; `internal/activity`; `internal/demo`;
`internal/httpapi/{ports,wiring,handlers_native,handlers_portfolio,handlers_activity}_markets*.go`;
`cmd/api/marketsurfaces.go`; `scripts/demodata`; `scripts/marketsafety`;
`docs/product/CREDIT_ECONOMY.md`; D-063 to D-068.
