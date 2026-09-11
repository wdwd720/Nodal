# The Credit economy

> **2026-09-10.** Written from the code, not from an intention. Every claim below
> names the file, migration or test that makes it true, and where something is
> not built the section says so rather than describing what it would do.
> Companion to `docs/product/PROVIDER_BOUNDARY.md` (what Nodal does not do
> itself) and ADR-0027 (how a position and a price are recorded).

A **Nodal Credit** is internal platform value. It is not a bank deposit, not an
FDIC-insured cash balance, not automatically redeemable, not crypto and not a
blockchain token. Product goal §10 states that; `internal/valuedomain` enforces
it, and the ledger refuses a movement between value domains that no ACTIVE
capability permits.

---

## 1. The shape of the thing

There is exactly one Credit asset per deployment: a row in `assets` with
`kind = 'CREDIT'`, `chain = INTERNAL`, `value_domain = INTERNAL_CREDIT`, six
decimal places. Six, because a bonding-curve market has to price units far below
one Credit and a coarser scale would make rounding to zero a usable exploit.
Migration 00711 permits one; `credit.Service.AssetID` resolves it and returns
NOT_FOUND when the internal economy has not been provisioned.

Every Credit amount in the system — in the ledger, in the API, in a chart — is an
exact integer count of **base units** (`money.Quantity`, `numeric(38,0)`). There
is no float anywhere on this path; `internal/money`'s
`TestNoFloatingPointInSource` and `scripts/lintfin` refuse one.

One Credit is therefore `10^6` base units, and **the scale travels with the
figures**. `GET /v1/credits/pricing` returns `decimals`, `GET /v1/credits/balance`
returns `credit_decimals`, and `credit.PricingPolicy` carries the scale it prices
at as a hashed field that `NewPurchaseService` holds against the registered
asset. That is not decoration: the pricing policy used to convert money into
Credits with no scale at all, writing a count of whole Credits into the column,
the API field and the ledger entry that all mean base units, so $10.00 at 100
Credits per dollar bought 0.001 Credits while the purchase page said 1,000. Every
row agreed with every other row, which is why nothing caught it — the defect was
in the unit (F-151, D-093).

Three books, each with one job:

| Book | Table | Answers |
|---|---|---|
| The journal | `journal_transactions`, `journal_entries`, `ledger_balances` | how much value moved, between which accounts, in one balanced transaction |
| Provenance | `credit_lots`, `credit_lot_events`, `credit_lot_state` | where each remaining unit came from and what may be done with it |
| Read models | `native_positions`, `native_market_state`, `native_market_prints` | what a holder holds, what a market is worth, what it traded at |

Only the first two are authoritative. The third is derived, maintained by
database triggers, and checked against the first by reconciliation queries
(`VerifyReserves`, `cp_native_positions_unreconciled()`).

---

## 2. How a Credit comes into existence

A Credit is only ever created by `internal/credit` posting a journal transaction
and recording a lot beside it, in the same database transaction. A lot with no
journal transaction would be provenance for value that was never posted, and
migration 00711 makes `journal_transaction_id` NOT NULL so that cannot happen.

Every lot carries an **origin** — where the value came from — and a **finality** —
how sure we are it will stay. Both are on the lot forever; neither is editable
(`credit_lots` has a `forbid_mutation` trigger).

### The eleven origins

Recomputed from `internal/valuedomain/origin.go` and migration 00711's CHECK.
They agree, and `test/integration/enums` keeps them agreeing.

| Origin | Created by | What it means |
|---|---|---|
| `PURCHASED` | `credit.PurchaseService` on a settled card payment | somebody paid money for these |
| `PROMOTIONAL` | an operator grant; the sandbox demo seeder | nobody paid; the platform gave them |
| `REFUND` | a reversal that returns value | a correction, not an earning |
| `CREATOR_EARNING` | `internal/commerce` on a sale | a creator sold something |
| `DATA_SALE_EARNING` | `internal/commerce` | a data product sold |
| `AGENT_SERVICE_EARNING` | `internal/commerce` | an agent service sold |
| `MARKET_CREATOR_EARNING` | `internal/nativemarket` on every fill, to the asset's creator | the creator fee. Deliberately distinct from `CREATOR_EARNING` because its source is speculative trading |
| `MARKET_TRADING_PROCEEDS` | `internal/nativemarket` on a SELL, to the seller | what a holder got for selling |
| `COMPETITION_REWARD` | a competition payout | a prize |
| `ADMIN_ADJUSTMENT` | an operator correction | a hand adjustment, visible as such forever |
| `PROVIDER_SETTLEMENT` | a provider settlement | value that arrived from a provider rather than from a customer |

### The five finalities

| Finality | Spendable? | Could ever be paid out? | Why |
|---|---|---|---|
| `UNFUNDED` | yes | yes | nothing was ever at risk of reversal — a grant, or an earning funded by value already inside |
| `REVERSIBLE` | yes | **no** | a card issuer can still reclaim the funding. Spending it is the ordinary product experience and the platform carries that risk knowingly; paying it out would turn a chargeback into an uncollateralised loss |
| `SETTLED` | yes | yes | the funding is final |
| `DISPUTED` | **no** | no | frozen while the dispute runs. If it closes without taking the money — a won dispute, reinstated funds, or an early-fraud-warning inquiry that closed — the freeze lifts and the funding returns to `REVERSIBLE`, to the window it was already in. It does **not** settle: settlement is a clock closing, not a card network agreeing (D-094) |
| `REVERSED` | **no** | no | the funding was clawed back |

`FundingFinality.Spendable()` and `.PayoutEligible()` are the two functions;
the second is a *floor*, and origin policy applies on top of it.

### What a payment buys, and what ends one nobody finished

`credit.PricingPolicy` is the only thing that decides how many Credits a payment
issues. It reads no clock, no database and no request body beyond the amount, so
the same amount under the same policy version always issues the same quantity —
and there is no field anywhere in the API that carries a Credit amount into a
purchase. `GET /v1/credits/pricing` publishes every term of the conversion
(`credits_per_major_unit`, `minor_units_per_major_unit`, `decimals`, `rounding`
and the bounds) so a page can show the same figure the server will issue rather
than deriving half of it.

A purchase that nobody finishes used to be permanent. Every state before capture
counts against the money-at-risk ceiling, and nothing moved a funding out of one:
a provider call that failed left a committed row with no reference that no sweep
looked at, and an abandoned checkout's provider reports "still waiting" forever.
Two abandoned $1,000 checkouts exhausted the launch tier's $2,000 ceiling and
every honest purchase after them was refused `AT_CAPACITY` with no remedy (F-153,
F-90's failure through a different door). Three passes now run on `cmd/api`'s
credit ticker, and on `cmd/reconciliation-worker` where a deployment has one:

| Pass | What it asks | What it does |
|---|---|---|
| `SettleDue` | has this funding's reversibility window closed | REVERSIBLE → SETTLED, and promotes the lot |
| `ReconcileDue` | what did the provider do with a purchase in flight past fifteen minutes | adopts the provider's answer, which is what mints a purchase whose `succeeded` event was swallowed (F-154) |
| `ExpireInFlight` | has a pre-capture purchase been open past a day | cancels it at the provider, then CANCELED here |

`ExpireInFlight` asks before it ends anything, and cancels the PaymentIntent at
the provider before cancelling the funding: a funding marked CANCELED over a live
intent is a card that can still be charged against a terminal funding that will
never mint. `CAPTURE_PENDING` is deliberately not expired — once the provider is
processing, cancelling is no longer ours to do.

Whether a purchase was a rehearsal is recorded on it. `credit_fundings.provider_mode`
is written once, at creation, from the mode of the provider that opened the
payment, and the API's `sandbox` flag is rendered from that row. It used to be the
deployment's CURRENT mode stamped on every purchase the API returned, so going
live re-labelled every sandbox purchase ever made as real value (F-158, D-096).

### How long REVERSIBLE lasts, and the sandbox tier (D-086)

`REVERSIBLE` becomes `SETTLED` when the reversibility window closes.
`credit.PurchaseService.SettleDue` makes that move and `cmd/api` sweeps on a
ticker, because the launch tier deploys no worker to do it (F-90) and a
deployment where nothing settles turns the money-at-risk ceiling into a lifetime
cumulative cap.

The window is `CP_CREDIT_SETTLEMENT_WINDOW`, 720 hours on STAGING, and it is
right: it is the card chargeback window, and a captured payment really is
reversible for that long.

On a **sandbox tier** it is two minutes, compiled in and keyed off
`cfg.SandboxTier()`. The reason is the sentence at the top of this section read
in the other direction: only `SETTLED` value is payout-eligible under any policy
in this build, including the sandbox one, so with the real window nothing bought
on the tier whose entire purpose is rehearsing the product could ever be
withdrawn — and the failure was invisible, because it is a clock rather than a
refusal. Two minutes is long enough to observe `REVERSIBLE`, which matters and
which a rehearsal must not skip, and short enough that a person testing the
product does not go and do something else. The sweep ticks every twenty seconds
there, because a two-minute window swept every fifteen minutes is a
fifteen-minute window.

It is not a second environment variable, for the reason `CreditConfig` already
gives for the real one: a risk determination read straight from the environment
is outside `scripts/configcheck` and outside the configuration hash. A sandbox
window is not a risk determination at all — nothing was charged and nothing can
be charged back — so it is a property of the tier, stated once in
`cmd/api/creditsettle.go`, where PROD is refused by construction (ADR-0023).

---

## 3. What a Credit can do inside the product

### Buy a Nodal-native asset

`POST /v1/native-markets/{id}/quotes` prices a hypothetical trade and records
what the user was shown. It is evidence, never a price source:
`POST .../orders` re-prices against current state, and the user's protection
against the market moving is `min_output`, which is the number they actually
agreed to.

A trade is **one database transaction**, in this order:

1. re-read and lock the market, re-price against current state;
2. the **risk kernel** — the two concentration limits of §47, from the versioned
   GLOBAL ∧ ACCOUNT risk policy — refuses a BUY that would leave the account
   holding too large a share of the asset's supply, or with too much of its
   Credits committed to one creator. A SELL is never refused;
3. the **market-safety policy** — price impact, slippage, and the creator rule
   (ADR-0027) — refuses a BUY the market is too small for. A SELL is never
   refused, because refusing an exit traps a holder;
4. the **journal posting**, where the ledger's balance, negative-balance and
   value-domain checks run. A native trade is a declared conversion between
   `INTERNAL_CREDIT` and `INTERNAL_NATIVE_ASSET` and is refused unless
   `NATIVE_MARKET_TRADING` is ACTIVE;
5. **provenance**: on a BUY the buyer's lots are consumed **most restricted
   first** — promotional before purchased before earned, oldest first within a
   rank — and only lots at a spendable finality (`RequireSpendableFinality`).
   That ordering is structural rather than "least payout-eligible first" on
   purpose: eligibility comes from a versioned policy, and ordering consumption
   by it would make a spend replay differently after a policy change. It is also
   the user-favourable order, because ordinary spending burns grants and leaves
   earned value intact. On a SELL the proceeds are recorded as a new
   `MARKET_TRADING_PROCEEDS` lot and the creator fee as `MARKET_CREATOR_EARNING`,
   both at `REVERSIBLE` finality because whatever funded them may still be
   reversible;
6. the **fill**, which is the only thing that moves market state. A trigger
   re-derives the reserves from the fill's own amounts and refuses a stale
   version, a negative reserve, supply that was never minted, or anything that
   would put the pool below its constant product;
7. the **price**, the **print**, the **position** and the **audit row**, all
   inside the same transaction.

If any step fails, none of the economic effect occurs.

### Buy from the internal marketplace

`internal/commerce`: Credits move from buyer to seller and the seller's lot
carries an earning origin. Requires `MARKETPLACE` ACTIVE; the gate is resolved
from the database on every purchase, so pulling it stops sales without a restart.

### Nothing else

There is no other path by which a Credit leaves an account, except a payout.

---

## 4. What can never leave

A payout is the only exit, and it does not reserve "500 Credits" — it reserves
**specific units from specific lots** (`payout_allocations`), so cancelling
returns exactly what it took. Without that, a user could launder a promotional
grant into an earning by reserving a payout and cancelling it.

Three independent things must all say yes:

1. **Finality.** `REVERSIBLE`, `DISPUTED` and `REVERSED` value can never be paid
   out, whatever the policy says.
2. **Origin policy** (`valuedomain.Policy`, versioned and hashed):
   - `DefaultPolicy` — what every deployment runs unless told otherwise —
     permits **no origin at all**. It is fail-closed by construction, not by
     omission.
   - `SandboxPolicy` — reachable only on a sandbox tier (ADR-0023) — permits
     `PURCHASED` and the five earning origins once the account reaches
     `PAYOUT_KYC`, and refuses `PROMOTIONAL`, `REFUND`, `ADMIN_ADJUSTMENT`,
     `PROVIDER_SETTLEMENT` and `COMPETITION_REWARD` outright. A granted Credit
     that could leave the system would be the first rule somebody copied — and a
     competition prize is a grant: nobody paid for it and nobody earned it. It
     was withdrawable here until D-095, which is the sentence above stated and
     then broken one line later, and it gave a platform a payout path gated only
     by a competition it runs itself (F-157).
3. **Capabilities and verification.** The permitted origins require
   `PAYOUT_RESERVE` ACTIVE and a verification level the account actually holds.

So: **promotional, refunded, adjusted, provider-settled and prize value can never
leave this system under any policy in this build.** That is what makes the sandbox
demo catalogue safe — its Credits are `PROMOTIONAL`, spendable inside the
product and unable to leave it
(`TestIntegration_DemoCreditsCanBeSpentAndCanNeverLeave`).

`GET /v1/credits/balance` and `GET /v1/me/portfolio` never return one number.
They return gross, spendable, frozen, reversed, payout-eligible and ineligible,
broken down by origin and by finality, with the **reasons** each ineligible lot
was refused — because "18,450 Credits" does not answer "how much may I
withdraw", and §46 forbids showing those four words as synonyms.

Every one of those buckets is rendered. `reversed` was returned and shown
nowhere, which is how a chargeback that stranded units inside the lot it reversed
stayed invisible (F-152, F-156); Home and Portfolio show it, and `SegmentedBar`
draws whatever its segments do not account for, so the next bucket a caller
forgets is visible instead of silently missing.

A clawback takes the units the reversed funding minted and nothing else. What the
lot still holds is destroyed; what was already spent out of it is the recorded
`DEFICIT` the account owes. It used to consume in ordinary consumption order,
which meant a chargeback of a card payment destroyed whichever lot sorted first —
a promotional grant, every time — while the charged-back lot kept its units
(F-152).

---

## 5. Temperature: three kinds of value, never one number

Every amount the product surfaces return carries what **kind** of value it is:

| Temperature | Means |
|---|---|
| `ECONOMY` | closed-loop Nodal Credits and native assets. Internal, not a deposit, not redeemable by default |
| `REAL` | money at a payment provider, in minor units of its currency |
| `SIMULATED` | value on a sandbox tier, or attached to an object a demo seeder created. It moves nothing anywhere |

`SIMULATED` wins over both others: on a sandbox tier no real value can move by
construction (ADR-0023 — provider mode `live` is refused outside PROD), so the
deployment stamps the whole feed, and a demo object stamps its own items even
where the rest is not stamped.

---

## 6. What a native asset is worth

A native asset is `INTERNAL_ONLY`, and migration 00712 has a CHECK that refuses
`internal_only = false`: turning it off is a schema change plus a legal decision,
not a column write.

Its price is the **marginal price** of its market's pool — what the *next* base
unit costs — at `10^18` scale. The portfolio marks holdings at it and says so.

**No market capitalisation is computed anywhere.** Product goal §12 permits an
internal valuation "only if mathematically legitimate", and price × total supply
is not: the pool could not buy back the float at the marginal price. The API
returns the pieces — circulating supply, total supply, the marginal price, and
the **real Credit reserve**, which is the entire amount that could ever be paid
out of that pool — and leaves the multiplication undone.

Cost basis, realised P&L and fees come from `native_positions`, which only
triggers write and which a CHECK holds to
`quantity = allocation + bought − sold`. §15 is explicit that P&L must not be
inferred from balance differences, and there is no code path here that could.

---

## 7. What is not built

Stated so nobody has to discover it:

- **No agent attribution on a position.** No agent path reaches a native trade
  in this build, so a position has no agent field and none is invented.
- **The circuit breaker is disarmed by default.** The mechanism exists, is
  enforced and is tested; the compiled-in policy sets its threshold to zero for
  the reason recorded in D-065, and a deployment with an operator arms it with
  `go run ./scripts/marketsafety`.
- **Creator self-dealing is exposed, not prevented.** `surveillance.go` raises
  `CREATOR_SELF_DEALING` and does not block, because an automated market maker
  has no order book and therefore no matched self-trade to prevent. A deployment
  that would rather prevent records a safety policy with
  `creator_may_buy_own_asset: false`.
- **No security events in the activity feed.** Verification, profile and agent
  kinds landed with D-081: `internal/activity` declares eighteen kinds,
  including `VERIFICATION_UPDATED`, `TERMS_ACCEPTED`, `ACCOUNT_CLOSURE_REQUESTED`,
  `ACCOUNT_CLOSURE_DECIDED`, `PAYOUT_DESTINATION_ADDED`,
  `PAYOUT_DESTINATION_DISABLED` and the four agent lifecycle kinds. Security
  events are absent deliberately, and `internal/activity/doc.go` says why:
  `internal/notifications` already tells a person about a new sign-in, and a
  page that reads `security_events` is a different surface with a different
  retention policy from a timeline of what happened to an account's value.
- **No Credit purchase has ever been made against a live provider.**
  `CREDIT_PURCHASE` is not ACTIVE in any deployment; see
  `docs/build/BLOCKERS.md` and `docs/audit/LAUNCH_GATE_MATRIX.md`.

## Evidence

`internal/valuedomain` (origins, finalities, policies); `internal/credit`
(issue, consume, balances, purchase); `internal/ledger` (the journal and its
triggers); `internal/nativemarket` (the curve, risk, safety, prints, positions);
`internal/payout` (reservation by lot); migrations 00711, 00712, 00713, 00729,
00743, 00771, 00772, 00773, 00774, 00793; ADR-0023, ADR-0027; D-063 to D-068,
D-086, D-093 to D-096; findings F-151 to F-159.
