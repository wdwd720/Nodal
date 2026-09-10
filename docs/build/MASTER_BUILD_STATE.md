# MASTER BUILD STATE

> **READ THIS FILE FIRST when resuming in a new session.**
>
> **The goal changed.** The current source goal is `gola.md` (repo root, 102 parts, 25 stages):
> an independent adversarial audit of the existing system, a migration to the final Nodal
> architecture, and production proof. The previous goal document
> (`ULTIMATE MASTER GOAL — Production Universal Financial Control Plane.md`) built what is now
> Domain B and Domain C; its history is preserved below from "## 2. Completed milestones" onward and
> is still accurate about that work.
>
> Companion files: `FINAL_ARCHITECTURE_MIGRATION.md`, `CURRENT_SYSTEM_INVENTORY.md`,
> `KEEP_MODIFY_REPLACE_MATRIX.md`, `BLOCKERS.md`, `DECISION_REGISTER.md`,
> `REQUIREMENTS_TRACEABILITY.md`, and `../audit/INDEPENDENT_AUDIT.md`.

---

# PART -1 — STRIPE PROVIDER ACTIVATION WORKSTREAM (pgf.md)

Started 2026-09-08 from `pgf.md` at the repository root. It is a workstream inside the existing
architecture, not a new goal: everything in PART 0 onward remains accurate.

## Where the perishable facts live

Read these first when resuming; they hold what a browser session established and cannot be
rediscovered from the code.

- `docs/providers/STRIPE_CAPABILITY_MATRIX.md` — what Stripe's current documentation actually says,
  per feature, with citations and an evidence date.
- `docs/providers/STRIPE_ACCOUNT_STRUCTURE.md` — which account, why, and the risk that choice carries.
- `docs/providers/STRIPE_BUSINESS_MODEL_REVIEW.md` — Nodal's activities against Stripe's published
  restricted-business policy.
- `docs/providers/STRIPE_BROWSER_SETUP.md` — every browser action and its outcome, including what was
  deliberately not touched.
- `docs/providers/STRIPE_INTEGRATION_STATE.md` — what the code does and what is not done.
- `docs/providers/WALLET_INTEGRATION_STATE.md` — where a payout lands and who is authoritative.
- `docs/providers/STRIPE_PRODUCTION_CHECKLIST.md` — the ordered path to a real payment.

## The four decisions that shaped everything

1. **Nodal runs on the existing Actorvia live account** (`acct_1REGPQALyMyuBFc1`), by the owner's
   explicit direction, against Stripe's own written rule that independent projects use separate
   accounts. A separate Nodal account was created and then abandoned; it still exists, empty and
   unactivated, as `acct_1UDZsoARym5YyR1Q`.

2. **Credits are sold through PaymentIntents, not Checkout.** Actorvia's live webhook already
   subscribes to `checkout.session.completed`, and Stripe delivers each event to every subscribed
   endpoint. Checkout would post every Nodal purchase to Actorvia's billing handler.

3. **Every provider event is classified before it is acted on.** On a shared account, Nodal's
   endpoint receives another product's events. Foreign-event rejection is a correctness requirement
   here, not a defensive nicety, and the environment half of it is what stops a staging deployment
   acting on production purchases.

4. **The payout rail is USDC on Base or Polygon, and Stripe holds the destination.** Not Solana.
   Goal Section 23 anticipated exactly this. Stripe also performs the payout KYC, because the
   product requires `dashboard: express` and that setting makes Stripe the requirements collector.

## What landed

| Subject | State |
|---|---|
| `credit.PurchaseProvider` abstraction and registry | **done** |
| `credit.PricingPolicy` — versioned, hashed, server-side | **done** |
| `internal/provider/stripecredit` — PaymentIntents adapter | **done**, CODE_COMPLETE |
| `internal/provider/stripesig` — one shared signature verifier | **done** |
| `credit.PurchaseService` — events to ledger effects | **done** |
| Funding states CANCELED and MANUAL_REVIEW (migration 00728) | **done** |
| `credit_fundings.reversible_at` (migration 00729) | **done** |
| `payout.Capabilities` — assets, networks, KYC ownership, availability | **done** |
| `internal/provider/stripepayout` — stablecoin payout adapter | **done**, and refuses to submit |
| Config slots `CREDIT_PURCHASE` and `PAYOUT` | **done** |
| PAY-001 … PAY-006 as integration tests | **done** |
| HTTP endpoints, `cmd/api` wiring | **done** — `cmd/api/wire_credit.go` builds the adapter, `POST /payments` is declared and mounted, the webhook is mounted at `/v1/webhooks/stripe_credit`. This row read "not started" until 2026-09-10 (F-111) |
| Customer-facing purchase UI | **not started** — `apps/web` has no purchase flow |
| Connect connected accounts | **blocked** on B-11 |
| Payout destination-change hold | **not built**, and blocks `PAYOUT_SETTLE` |

## Exact next action

1. The owner runs `stripe login` and decides Nodal's production webhook URL
   (`STRIPE_BROWSER_SETUP.md` §5 and §6).
2. ~~Wire the adapters in `cmd/api`; expose the purchase endpoints; mount the webhook handler.~~
   **Done.** All three exist and are mounted; what is missing is the customer-facing UI.
   Corrected 2026-09-10: this was the first instruction a resuming session read, and it
   named work that had already landed (F-111).
3. Run Stage 2 of `STRIPE_PRODUCTION_CHECKLIST.md` against Stripe test mode.
4. Nothing past Stage 2 without the owner: every remaining item is a business-model declaration, a
   legal attestation, or an application whose denial can affect Actorvia's live capabilities.

## New external blockers

B-09 restricted-business review, B-10 stablecoin payout private preview, B-11 Connect platform
profile. B-04 narrowed and B-06 is answered for this rail. See `BLOCKERS.md`.

---

# PART 0 — CURRENT GOAL (gola.md) AND STATE

Baseline frozen at `b8da0c4`. Everything below this line describes work done against `gola.md`.

## 0.1 The finding that shapes everything

The repository implemented the previous goal completely and soundly: 40 of 40 integration packages,
the migration, e2e, chaos and six contract suites pass on freshly provisioned databases. Measured
against `gola.md` that is Domain B (simulated capital) and Domain C (real capital).

**Domain A — the Nodal-native economy — did not exist at all.** Not partially: a grep for
`ValueDomain`, `CapitalRail`, `CreditOrigin`, native assets, a market engine, payout eligibility, a
legal router or agent authority levels returned zero files. That absence, not a defect list, is the
migration.

## 0.2 What has landed

| Stage | Subject | State |
|---|---|---|
| 0 | Baseline freeze, inventory, KEEP/MODIFY matrix | **done** |
| 1 | Independent adversarial audit | **done, and continuing alongside each stage** |
| 2 | ValueDomain / CapitalRail / provenance, enforced in Go and SQL | **done** (`internal/valuedomain`, migration 00710) |
| 3 | Accounting hardening | **partial** — domain isolation added to the ledger; reservations pre-existed and were verified |
| 4 | Credit ledger, provenance lots, funding lifecycle | **done** (`internal/credit`, migration 00711) |
| 5 | Native asset registry, moderation, lifecycle | **done** (`internal/nativeasset`, migration 00712) |
| 6 | Native market engine (constant product, virtual reserve) | **done** (`internal/nativemarket`, migration 00712) |
| 7 | Market surveillance | **done** (`nativemarket/surveillance.go`) |
| 8 | Internal commerce / creator economy | **done** (`internal/commerce`, migration 00715) |
| 9 | Payout eligibility, provider architecture, reconciliation | **done** (`internal/payout`, migration 00713) |
| 10 | Hosted partner rail | **not started** — and see BLOCKERS B-05 |
| 11 | Self-custodial onchain rail | **kept as-is**, re-classified as one rail among several |
| 12 | Rails unified behind FinancialIntent | **done** — in front of every Domain A command AND every Domain B/C trade intent |
| 13 | LegalCapabilityRouter, composite capability key | **done** (`internal/legalrouter`, gates extended, migration 00714) |
| 14 | Agent authority levels | **done** (`internal/agentauthority`) |
| 15 | Reality / Prediction / Proof integration with Domain A | **done** — prices, instrument, audit events; see below |
| 16 | Frontend | **done for Domain A** (5 pages incl. Create Asset and trading, honesty rules enforced, 69 browser tests) |
| 17 | Admin tooling for Domain A | **done** (10 administrative action kinds + executors; the tenth, NATIVE_MARKET_LAUNCH, is F-28) |
| 18 | Infrastructure / IAM hardening | pre-existing, audited |
| 19 | Property testing / fuzzing | **partial** — curve fuzzer (4.7M execs), exhaustive isolation property, credit torture test |
| 20 | Chaos / fault injection | **done for Domain A** (3 tests, 2 negative controls; whole suite 10/10 with nothing skipped) |
| 21 | Load | **done** — committed write path measured through a real gate ceremony; it found F-26 and F-27 |
| 22 | Provider sandbox integration where externally possible | **done to the limit of what is possible** — 5 contract suites, 53 cases, against documented fixtures; every real sandbox is application-gated or has no contract |
| 23 | Final independent re-audit | **in progress** — F-23, F-24 and F-25 came from it |
| 24 | Launch evidence package | **deliberately not started** — see the readiness report §5 |

API surface: 17 Domain A endpoints added to the OpenAPI contract, regenerated, implemented, wired
into `cmd/api`, and covered by the existing deny-by-default authorization invariants. Permissions:
61, up from 42 at the baseline; capabilities: 20, of which 18 are high-risk. (Was "59 … of which 9". The high-risk figure had been wrong since F-16 moved MARKETPLACE and the rest of the internal economy across, and nothing checked it — `TestDocs_CountsMatchTheCode` does now.)

### Stage 8 as built

`internal/commerce` + migration 00715. The requirement PART XVII actually turns on is one sentence —
creator revenue must carry provenance distinct from speculative trading proceeds — and everything
else follows from making that sentence structurally true:

- A product's **kind** is the only input to the provenance decision, the mapping is a fixed table,
  and the resulting origin is stored on the order rather than re-derived. No request body anywhere
  in the API has a field for an origin.
- **Self-dealing is refused twice.** Buying from yourself would convert Credits no policy will ever
  release into creator-earning provenance one day might, which is the most valuable thing an
  attacker could do here. Go refuses it with a comprehensible error; two named CHECK constraints
  refuse it for the schema owner.
- **The order is not the authority on what was paid.** A deferred trigger reads the journal entries
  and refuses at COMMIT any order whose stated price or proceeds the posting does not show (IC001).
- **Terms freeze on publication** (IC002) and orders are immutable.
- An earning is issued **REVERSIBLE, not SETTLED**: the Credits behind it may still be inside a card
  dispute window, and an earning cannot be more final than the money behind it. It is spendable,
  which is what a marketplace needs, and not payout-eligible, which is the conservative half.
- A purchase is a **single-domain movement** and commits with no capability active. If it ever
  needed one, that would mean a conversion had crept into the path.

JOURNEY C can now be walked end to end: buy Credits → list a dataset → sell it → hold
`DATA_SALE_EARNING` → request a payout and be told, per unit of provenance, what the policy permits.
The last step still ends in a refusal on a fresh deployment, which is correct: no payout capability
is active and the default policy permits no origin.

### Stage 12 as built

`internal/settlement/financialintent.go` and `compiler.go`, plus
`internal/httpapi/wiring_compiler.go`.

**The problem.** `V1Planner` compiles one rail shape: a self-custodial on-chain spot swap. That was
the whole system when it was written and is not the whole system now. Domain A executes on Nodal's
own ledger, Domain B against simulated markets, and a payout leaves the system entirely — four
settlement models with four authoritative balance sources. A planner that knows one of them cannot
be "the only route from a typed intent to execution", which is the property the compiler exists to
have.

**What was built.**

- **`FinancialIntent`** (PART XXV): the typed request every manual and agent action produces, with a
  DECLARED capital domain, an action type from PART XXV's list, and a typed subject. It sits beside
  `IntentSnapshot` rather than replacing it: the V1 projection is an instrument, a USD notional and a
  venue, which cannot describe "buy 500 Credits of this creator's token" without most of its fields
  becoming meaningless.
- **`Compile`** (PART XXVI): pure, deterministic, and total over a routing table that
  `ValidateCompiler` proves covers every declared action exactly once. It determines value domain,
  legal rail, provider, executor, required capabilities, required verification, required
  confirmation, quote and reservation requirements, risk evaluation, agent authority,
  authoritative balance source and reconciliation method — and returns every applicable refusal at
  once, sorted, so a caller is never told one problem per attempt.
- **The refusals that matter.** An unimplemented rail is refused before any policy question, so
  hosted trading cannot be routed to machinery that does not exist however permissive a policy is.
  An agent can never request a payout, at any level, with every capability active. A declared domain
  that disagrees with the action is a refusal, never a correction.
- **Wired in front of every Domain A command**: native-market execute, native-asset create, internal
  purchase and payout create all compile first. A refusal carries the policy version, the rule index
  that produced it, and the capability that would have to be activated.

**Why internal rails get a Route and not a seventeen-step Plan.** Durable per-step state exists
because an external settlement can be half-done — submitted, result unknown. An internal-ledger
settlement is one database transaction that either commits or does not. Wrapping it in a DAG would
add failure modes rather than remove them. What must not be skipped is everything IN FRONT of
execution, and that is what `Route` carries.

**What this does not replace.** The ledger still refuses a cross-domain posting with no capability;
`internal/commerce` still refuses a purchase with `MARKETPLACE` off; the payout engine still decides
eligibility per unit of provenance. A gate that exists only at the edge is one a worker walks
around, so the compiler is an addition and never a substitution — F-15 is the finding that made that
rule concrete.

**Still on the V1 planner.** External spot swaps (Domain C) are unchanged: the compiler routes them
to `ExecutorExternalPlan` and `V1Planner` does its job. Compiling a Domain C intent through
`FinancialIntent` end to end — replacing the direct `IntentSnapshot` path — is the remaining half of
this stage and is not done.

### Stage 17 as built

Nine administrative action kinds and their executors, on the existing
dual-control machinery in `internal/admin`.

**The problem.** Every control in Domain A existed and none of them had an
operator interface. A market could be halted, an asset delisted, a seller
suspended and a stuck payout resolved — by running SQL. A control reachable
only by hand-written SQL has no audit trail, no dual-control story and no
reason attached, which in an incident is barely a control.

**The rule the table encodes.** Stopping is one operator; restarting is two.

| Kind | Signatures | Why |
|---|---|---|
| `NATIVE_MARKET_HALT` / `CLOSE_ONLY` / `FREEZE` | one | A control that needs two signatures to stop an incident is one nobody reaches for at 3am. PART XXXII: halting new risk must never be harder than taking it. |
| `NATIVE_MARKET_RESUME` | two | Restarting is the direction that adds exposure. |
| `NATIVE_ASSET_MODERATION_VERDICT` | one | A content judgement. Recording APPROVED does not start trading. |
| `NATIVE_ASSET_DELIST` | one | Risk-reducing and terminal. |
| `COMMERCE_SELLER_SUSPEND` / `PRODUCT_WITHDRAW` | one | Risk-reducing. Suspension stops new orders and touches nothing already earned. |
| `PAYOUT_MANUAL_REVIEW_RESOLVE` | two | It decides what happens to money somebody is waiting for. |

**What the payout resolver deliberately cannot do.** There is no resolution
that declares a payout SETTLED. The provider is authoritative for settlement,
and an operator who could assert it by hand could close a ticket by claiming
money moved. The resolutions are FAIL (return the exact reserved units to the
exact lots), REJECT and RETRY — and RETRY is refused outright if the request
has ever been in a state where a provider call may have happened, because
sending it back to VERIFIED is how a payout gets paid twice.

**One new permission.** `native_market:resume` is the approve half of restarting
a market and is a dual-control permission: no standing role holds it, so it
requires a live break-glass elevation, exactly like releasing a kill switch.
Permissions are now 61.

**What the console already does.** `apps/admin`'s propose form is generated from
`authority.json`, which now lists all nine kinds, and it carries a free-form
params field. So every one of these is proposable, approvable and executable
from the existing dual-control queue today — including the two that take
params. What is missing is kind-SPECIFIC UI: a market picker instead of a
pasted uuid, a moderation-state dropdown instead of hand-written JSON. That is
Stage 16 work and a usability risk, not a missing control.

### Stage 16 as built

Four pages, and one rule made structural.

**The rule.** PART LII says Home must distinguish Nodal Economy, Simulated
Capital and Real Capital, and that balances must never be misrepresented. The
enforcement is not a layout convention:

- **No query hook produces a combined figure.** There is nothing in
  `apps/web/src/api/queries.ts` that adds a Credit amount to a USD amount or
  converts one into the other, so no component can render the total by
  accident.
- **A source-scanning test refuses a page that shows both.**
  `no page converts Credits into a currency` fails if a page renders a Credit
  figure and a `<Usd>` together, because PART LIV forbids inventing an exchange
  rate nobody approved.
- **A browser test reads what actually rendered.**
  `no page puts a Credit figure and a currency figure together` walks the four
  pages and refuses a currency amount on any page that quotes Credits — which
  covers strings arriving from the API, where a source scan cannot look.

**The pages.**

| Page | What it exists to say |
|---|---|
| Nodal Economy | Credits are not one number. Held, spendable and payout-eligible are separate figures with the origin and finality breakdown behind them, because eligibility is decided per origin. |
| Marketplace | A price is in Credits; the platform's share is its own figure, not folded into the price; and a seller sees which provenance a sale produces before they list. |
| Native Markets | Supply, the REAL reserve as distinct from the virtual one, holder concentration, both fees, asset status — and prices in Credits, never a currency. |
| Payouts | The eligible figure next to the total, the reasons for the difference, and "verification would suffice" as the different answer it is. |

**Honesty copy is enforced, not aspirational.** Four new constants
(`THREE_POTS_NOTE`, `CREDITS_DISCLOSURE`, `NATIVE_PRICE_NOTE`,
`NATIVE_ASSET_RISK`) with tests that a page showing Credits must say what they
are, and a page showing a user-created asset must carry the risk statement.

**One real defect fell out of building it** — F-20: any failure to read
`/v1/me`, including a rate limit, told the customer they were signed out. That
was baseline behaviour, it was self-concealing, and it took the fix to make the
cause visible.

**Create Asset (PART LIII)** is two steps, not one form. Step one collects;
step two shows the creator the exact numbers that become permanent — in base
units, with what each one means — and asks them to confirm THAT, with an
idempotency key created at the moment of confirming so a double-click cannot
publish two assets. It says plainly that this creates a DRAFT, that somebody
else decides whether it is published, and that Nodal has made no judgement
about it as an investment.

**Trading (PART LIV)** is deliberately shaped around the fact that a quote is a
record and not an offer. The customer is never asked to agree to the quote:
they state the minimum they will accept, which travels with the order and is
checked against a freshly computed fill. The minimum field starts EMPTY and is
never defaulted to the quoted output, because a default equal to the quote is a
slippage tolerance of zero wearing a protection's clothes, and every trade
would fail.

**Not built.** Nothing for Domain A. The remaining frontend gaps are outside
the internal economy.

### Stages 20 and 21 as built

**Chaos (Stage 20).** `test/chaos/internal_economy_test.go`. The internal
economy has a property the external rails do not: every financial act is a
SINGLE database transaction. There is no submit, no provider and therefore no
legitimate half-done state, which makes the invariant sharper than "recover
correctly" — after a fault there must be NOTHING.

Three tests, each killing the exact backend running the transaction after every
write and before COMMIT, and each asserting three things: no partial effect, no
drift (`VerifyProvenance`, `VerifyEarnings`, `VerifyReserves` all still hold),
and that the same command retried still succeeds. That third assertion matters
as much as the others: a guard that survives a fault by being permanently
broken afterwards has not survived it.

Both new guards carry named negative controls, and **both were observed
failing**. The first attempt at one of them did not:

> The first `commerce_partial_write` control consumed Credit lots with no
> posting behind them, and the test PASSED with the control active — because
> SQLSTATE CR004 makes that drift unrepresentable. A control that cannot fail
> proves nothing, so it was replaced by the mirror image (post the movement,
> skip the consumption), which fires.

The whole chaos suite now runs 10/10 with **nothing skipped**, once
`CP_TEST_REDPANDA_BROKERS` and `CP_TEST_ARCHIVE_ENDPOINT` are supplied; it had
been running 8 with 2 skips.

**Load (Stage 21).** `test/load/internal_economy.js`, and an honest partial
result. Measured on this host against the real binary: 3,048 requests at
101 req/s, p95 6.5 ms on the successful reads, no 5xx, and every refusal
delivered as problem+json.

What it measured is the READ surface and the REFUSAL path: 182 purchase
attempts refused `CAPABILITY_NOT_APPROVED` because the MARKETPLACE gate is off,
and 18 refused `IDEMPOTENCY_IN_PROGRESS` because the shared-key iterations
raced — the idempotency store doing its job under contention.

What it did NOT measure is the committed write path, because that needs the
MARKETPLACE gate ACTIVE, and MARKETPLACE is high risk: three distinct
principals, a step-up and four evidence references. A load script that
activated its own gate would be a load script that switched off a control to
get a number.

The script's first draft reported all of its 409s as "price changed". They were
idempotency conflicts. It now counts refusals **by the code the backend gave**,
and its threshold is on "never a 5xx" and "always problem+json" rather than on
`http_req_failed`, which counts an expected refusal as a failure and teaches an
operator to ignore a red threshold.

### Making Domain A reachable in development

Two pieces, both fail-closed, added because the internal economy was otherwise
unexercisable outside a test binary:

- **`scripts/seedeconomy`** seeds the Credit asset, 25,000 PROMOTIONAL/UNFUNDED
  Credits for each dev customer, a registered seller and three published
  products spanning all three earning provenances. It issues PROMOTIONAL rather
  than PURCHASED deliberately: nobody paid for them, and PURCHASED is the origin
  a payout policy is most likely to permit. It activates **no** gate and prints
  the operator steps instead.
- **`CP_API_LEGAL_POLICY=DEVELOPMENT`** loads `legalrouter.DevelopmentPolicy()`,
  which permits the internal economy through the REAL router with the REAL
  required capabilities. It is refused in STAGING and PROD — as an error, not a
  silent downgrade — and it still denies payouts, because a development policy
  must never be where somebody discovers payouts were switched on.

### Stage 15 as built — Reality, Prediction and Proof over Domain A

All three subsystems already existed and already worked. What did not exist was
any way for them to SEE the Nodal-native economy, and that was the whole gap:

- **no prices.** `asset_prices` had nothing from a native market, so
  `prediction.PGPriceReader` could not resolve an outcome on one.
- **no instrument.** Predictions, strategy IR and the rest of the platform name
  tradable things by instrument id. A native market had no row, so it could not
  be named at all and the prediction ledger was Domain B and C only.
- **no audit events.** `internal/commerce` and `internal/nativemarket` wrote
  none, so the Merkle checkpoints and PART 88 proof bundles covered every rail
  EXCEPT the economy this product is built on.

Three integrations, each inside the transaction of the act it describes,
because being in that transaction is the substance of the claim rather than a
detail of it:

1. **Reality.** Every fill publishes the market's post-trade SPOT price, and
   market creation publishes the opening price. `observed_at` equals
   `received_at` deliberately: Nodal is the venue and observed the trade by
   executing it, so a gap between the two would be an invented provider lag,
   and an invented lag is exactly what lets a backtest believe a price was
   knowable before it existed. `raw_ref` carries the fill id, so no price
   exists without the trade that set it.

   The published number is `SpotAfter` — the marginal price the NEXT trader
   faces — not `EffectivePrice`, which moves with the size of this particular
   order. A series built from effective prices would score order sizing rather
   than the market.

2. **Prediction.** A market is registered as a `SPOT_PAIR` instrument of
   (native asset / Credit). Predictions on Domain A then go through the REAL
   ledger and the REAL resolver, with no Domain A special case anywhere in
   `internal/prediction`.

3. **Proof.** A fill is appended to the trader's audit stream; a purchase is
   appended to BOTH parties' streams, because a purchase is one event to the
   buyer and a different one to the earner and each is entitled to prove their
   own half without being handed the other's history.

**Two things this DOES NOT do**, stated rather than left to inference:

- The instrument is created `HALTED` and its status is not mirrored from the
  market afterwards. A market is created PENDING, so ACTIVE would be false at
  that moment; of the two ways to be wrong, a registry that understates
  tradability is the safe one. Mirroring is named work.
- Domain A does not go through `internal/reality`'s ingest pipeline. That
  pipeline exists for external feeds — a raw archive, provider clocks, dedup,
  gap detection — and pushing our own database through it would mean inventing
  a "provider" for ourselves. What Domain A takes from PART XLII is the
  timestamp discipline, which is the part that is actually true here.

### One defect this work introduced and one it exposed

`asset_prices` identity is `(asset, quote, source, observed_at)` and the table
is append-only — `cp_app` holds no UPDATE grant, which is deliberate. The first
attempt to publish prices tried to upsert, and the missing grant caught it. The
second attempt nudged colliding stamps forward by a nanosecond, which
`timestamptz` stores as the same instant. The step is a microsecond, and it can
only move a price LATER, never earlier — the conservative direction.

`TestIntegration_ATradeIsADeclaredCrossDomainConversion` read "the most recent
NATIVE_TRADE row in the database" and was only correct while no other test
traded later on a faster clock. It is now scoped to its own fill.

### Stage 12's second half — Domain B and C through the same compiler

`wiring_compiler.go` put every Domain A command through one place. Trade
intents did not go through it: `PostIntents` validated its body and called
`intent.Submit`, and everything deciding whether the action was permitted lived
further down — the eligibility engine, the risk evaluator, the execution
planner, each sound on its own. "Sound on its own, in several places" is
exactly what PART XXVI says is not enough.

Every intent is now compiled first. Two independent questions decide the action
type, and keeping them independent is the point:

- **Is this real capital?** The MODE, and nothing else. BACKTEST, PAPER and
  SHADOW are simulated whatever the instrument is; CANARY, LIMITED and LIVE are
  real. PART 159 makes the submitter state the mode precisely so this is never
  inferred from the UI or the account.
- **Which real rail?** The instrument's base asset value domain, read from the
  registry. Guessing it from a symbol or a chain name is how a hosted balance
  gets settled as though it were self-custodial.

Three things worth recording:

1. **The first version skipped the instrument lookup for simulated modes**, on
   the reasoning that a simulation is simulated whatever it is about. Every
   PAPER intent was then refused with `SUBJECT_ASSET_NOT_STATED`: the legal
   policy is keyed by asset even for simulation, because a simulation of a
   prohibited asset is still a question the policy is entitled to answer.
2. **The compile happens INSIDE `runCommand`**, not before it. PART 36 makes a
   business rejection a recorded conclusion, so replaying the key reproduces
   the refusal instead of asking the policy again — otherwise one idempotency
   key gives two different answers across a gate activation.
3. **`Ports.SettlementPolicy` is a struct, not an interface.** Its zero value
   is the conservative deployment, so a caller that forgets to wire it refuses
   real capital rather than permitting it. A nil interface would have done the
   opposite — which stopped being hypothetical immediately: the assignment in
   `Wire` did not land at all, every deployment silently got the conservative
   policy, and the refusal test passed either way because it could not tell
   the two apart. That is F-24, and its fix is two tests that can.

`TARGET_EXPOSURE` is the one action whose direction this layer cannot know —
"make my exposure X" is a buy or a sell depending on a position the HTTP layer
does not hold. It is routed as a buy, visibly, and that is harmless only while
both sides of a rail route identically.
`TestProfiles_BuyAndSellAgreeOnEveryExternalRail` fails on the day that stops
being true, rather than the mistake being discovered by whoever gets the
misrouted refusal.

### A readiness document's citations are now checked, not promised

`REQUIREMENTS_TRACEABILITY.md` states that "all 609 Go test-function references
resolve to a `func Test`/`func Fuzz` that exists", hand-verified on one
afternoon. A hand-verified claim about 609 things goes false without anybody
knowing which week it happened, and this project has already been bitten by
that shape twice (F-14, F-18).

`test/docs/references_test.go` checks it instead, for the five documents a
reviewer would actually use to decide readiness. Its first run found one: the
readiness report offered `TestIsolation_CrossDomainPostingMustDeclareItself`,
which does not exist, as evidence; the function is called
`TestIsolation_CrossDomainPostingMustDeclareItsConversion`. The property was
proven, the citation was not. That is F-25.

Two deliberate limits, both about telling an assertion from a quotation:

- It is NOT applied to the whole `docs/` tree. `THREAT_MODEL.md` and
  `SECURITY.md` quote the names of tests they assert do NOT exist, and a check
  that could not tell the difference would force them to lie to satisfy it.
- Inside the five, a name is exempt when its PARAGRAPH says in words that the
  thing is missing. A findings register has to be able to name a citation that
  pointed at nothing. The rule that results is the one worth having: a document
  may name an absent test only while stating the absence.

The check was observed failing on a deliberately broken citation before being
believed.

### Stage 22, and what "where externally possible" actually leaves

The goal says provider sandbox integration WHERE EXTERNALLY POSSIBLE. Measured
against that qualifier, this is finished rather than not started, and the
distinction is worth stating precisely because "not started" reads like a gap
somebody could close.

Five contract suites run the REAL adapters against `httptest` fixtures built
from verified provider documentation, and all pass:

    ok  test/contract/eventtopics   ok  test/contract/helius
    ok  test/contract/jupiter       ok  test/contract/privy
    ok  test/contract/solanarpc     ok  test/contract/stripe

What is NOT possible, with the reason in each case:

- **A live Stripe onramp sandbox** is application-gated; the application is not
  approved (EB-003). The adapter is CODE_COMPLETE and CONTRACT_TESTED, and its
  own README says `SANDBOX_VERIFIED` is unreachable until then.
- **Helius, Jupiter and the wallet provider** need production credentials and
  commercial terms (EB-005, EB-010, EB-011).
- **A payout provider has no contract suite at all**, and cannot: PART XVIII
  forbids implementing a vendor against endpoints nobody has verified, and
  there is no payout vendor (B-01). `payouttest.Sandbox` is an in-house double
  that models the CONTRACT rather than any vendor's API, which is the only
  honest thing to build before the vendor exists.

So Stage 22 is not blocked on work. It is blocked on eight external items that
are already enumerated in `BLOCKERS.md`, and writing more adapter code against
guessed endpoints would make the blockers less visible rather than more.

### Stage 21 finished, and the two P1s it found

The committed write path could not be load tested because MARKETPLACE is high
risk and "a load script that activated its own gate would be one that switched
off a control to get a number". That reasoning is right about a SCRIPT and wrong
as a conclusion: what was needed was an operator tool that DRIVES the control
rather than going around it.

`scripts/gateceremony` is that tool. It calls `gates.Admin`, so the state
machine, the permission split (propose needs `gate:propose`, which RISK holds;
approve and activate need `gate:approve`, which no standing role holds and only
a live BREAK_GLASS elevation grants), the step-up, the four evidence references
and the refusal of self-approval all apply. It requires three DISTINCT principal
identifiers, every evidence reference as an argument with no default, and
LOCAL/DEV/TEST with a local database host. Its output says, and the gate's own
reason records, that three development identities agreed rather than three
people.

With the gate genuinely active the first run measured **p95 30.07 s**, five
commits out of two hundred, three 500s and 124 requests killed by the statement
timeout. `pg_stat_activity` said it plainly: nine of ten pool connections
`idle in transaction`, waiting on the client. That is **F-27** — the capability
check inside a financial transaction read through the connection POOL, so every
write held one connection while reaching for another, and a dozen concurrent
writes deadlocked the pool.

Getting that far needed **F-26** fixed first: `cmd/api` supplied neither a
verification resolver (so every account was `VerificationNone`, and every Domain
A action requires `NODAL_IDENTITY`) nor `MARKETPLACE` in the capability
resolver's list (so the gate could be ACTIVE and every purchase still refused
`CAPABILITY_NOT_ACTIVE`). Both fail closed, which is exactly why they survived:
the refusals were indistinguishable from a correct fresh deployment's. The whole
internal economy was unreachable in every deployment, and no test could see it
because every test supplies a MAP as the capability resolver.

Three defects stacked so the third was unobservable until the first two were
fixed. After the fixes: p95 469 ms, 10.3 ms on successful requests, 31 purchases
committed — the entire seeded balance — and the buyer's Credits down by exactly
those 31 purchases.

### The re-audit kept finding the same shape (F-28)

F-26 and F-27 were found by RUNNING the internal economy instead of reading it.
Continuing that method into the other half of Domain A found a third:

`POST /v1/native-assets` creates a DRAFT, and nothing in any deployment could
move it. `nativeasset.Activate` and `nativemarket.Create` had no caller outside
tests — no route, no action kind, no executor — so no operator could ever open a
market. The moderation executor's own comment said "activating a market is a
separate act behind its own capability", and that act had never been built.

Every existing test built its market by calling the services directly. That is
why nobody noticed: **a path only tests can walk looks finished from inside the
tests.** It is the same shape as F-26, and worth naming as a class rather than
three incidents — the audit question that keeps paying is not "is this correct"
but "can anybody actually reach it".

The chain now has both halves, kept apart on purpose: the creator submits
(`POST /native-assets/{assetId}/submit`, their own draft only, NOT_FOUND for a
stranger), and two operators launch (`NATIVE_MARKET_LAUNCH`, dual control,
five-minute step-up, one-hour expiry, keyed by the approval so one approval
mints one supply). Launching is the only action in the internal economy that
mints, and the economics lock behind it.

### The class, made mechanical (F-29 and `test/reachability`)

F-26, F-28 and F-29 are one defect three times: a path only tests can walk looks
finished from inside the tests. Every one was correct, covered and unreachable.

`test/reachability` asks the question mechanically. Every exported method on a
financial service that takes a transaction must have a caller in `internal/`,
`cmd/` or `scripts/`, or an entry saying why not. A caller in `test/` does not
count — that is the point. Eleven methods are exempt and every reason is
external (B-01, B-04, B-06); a second test refuses any exemption whose reason
does not name a blocker `BLOCKERS.md` defines.

Its first version could not fail, because it matched `.Method(ctx` and `Cancel`,
`Create`, `Execute` and `SetStatus` are names four of the five packages share.
It now requires a transaction as the second argument AND the calling file to
import the declaring package, and it was observed failing on both defects it was
written for.

It runs in the fast tier alongside `test/docs`, because an unreachable control
and a broken citation should be caught by the same run that catches a broken
package.

### The scans that had never been run, and the one that had never passed

`make sast` fails today for anyone who runs it, and did before this session:
its flags require every `#nosec` to name the rule it silences AND state the
invariant that makes it safe, and seven suppressions said only `#nosec G101` or
nothing. A scan target that fails is a scan nobody runs, so the flags were
doing the opposite of their job.

All seven are now justified or removed. The best of them was
`internal/nativeasset/moderation.go`, flagged for Trojan Source because it
contains bidirectional control characters — it is the code that REFUSES them in
user-supplied asset names. They are now `\u202a`-style escapes: the characters are
gone from the source and a reviewer can see which codepoint each one is instead
of an invisible glyph.

`make secrets` was red on `internal/gen/api/api.gen.go`, which embeds the
OpenAPI document as base64'd gzip; adding two endpoints was enough to trip the
entropy rule. `internal/gen` is allowlisted by path with the reason.

Also run on this commit: `govulncheck` (nothing this code calls), `trivy config`
(0 HIGH/CRITICAL), the SBOM, and `terraform validate` across dev, staging and
prod with `fmt -check` clean.

### The backup drill now proves something about Domain A

Its fixture was users, accounts, one asset and 25 journal transactions. Every
Domain A table restored EMPTY, so "row counts match" compared zero with zero,
and the readiness report had to record the drill as not covering the new tables.

`scripts/restoredrill/domaina.go` seeds a live internal economy through the REAL
services — Credits in provenance lots, a native asset, its market, a trade, a
seller, a product and a purchase. Going through the services rather than writing
rows is the point: what the restore has to survive is data shaped the way the
application makes it, satisfying the deferred balance triggers, CR004, AU001,
IC001 and the terms-frozen guard by having been written through them.

The restored copy now carries 1 fill, 1 order, 4 credit lots, 2 lot events, 2
published prices, 1 market, 1 instrument and 3 audit events, with row counts and
journal hashes matching on both sides.

### F-32: the browser tests proved the routes existed

The five internal-economy pages were covered by a test asserting the heading was
visible, that there was exactly one of it, and that no formatter had given up.
The heading renders before any request is made, so all three pass on a page
whose every panel is an error.

They were. `openapi.yaml` declares the asset and product list endpoints as
`{items: [...]}` objects and the committed client called `validatedList`, which
throws on anything that is not an array — so both lists rendered "This response
could not be trusted" on every load.

The first attempt to strengthen the test ALSO passed against the broken client:
Playwright retries an assertion until it passes, and `toHaveCount(0)` passes the
instant it is evaluated, which was before the query resolved. A negative
assertion with no positive signal before it proves nothing. It now waits for
`networkidle` and for every spinner to clear, and was observed failing with the
committed client's parsing restored.

Running these needs a built frontend and a live API:

    (cd apps/web && CP_WEB_API_TARGET=http://127.0.0.1:18100        ./node_modules/.bin/playwright test --grep renders)

### F-33, and a pattern worth naming

69 browser tests, and not one of them finished anything. They covered rendering,
navigation, accessibility and the honesty rules -- every one a statement about a
page at rest. That is the F-26 gap one layer up: a path the tests never walk
looks finished from inside the tests.

There is now a test that buys a product and asserts the customer's Credits fell
by exactly the price. It found three defects in ITSELF first, and each is a
pattern rather than a slip -- reading one product's price while clicking
another's button, asserting a table an earlier order had already put there, and
branching on a condition before the page had re-rendered.

That last one is the third occurrence in this session:

  - F-32's first fix asserted an absence before the query resolved;
  - this test's refusal branch read an empty card;
  - and this test's outcome check ran before the mutation re-rendered.

**An assertion about an absence, or a branch on a condition, needs a positive
signal before it -- otherwise it is only measuring how fast the test runs.**

Running the browser suite needs a fully configured deployment: a settlement
asset (CP_API_SETTLEMENT_CHAIN/MINT), or three tests fail on buying power,
holdings and withdrawals which are disabled without one. 70/70 with it.

### F-34: the risk kernel had never run anywhere

The readiness report admitted that `Route.RequiresRiskEvaluation` was recorded
and never consumed. Asking the F-26 question of `internal/risk` rather than of
Domain A gave a worse answer: NOTHING consumed it. No migration, script or
endpoint had ever written a `risk_policies` row, so the table was empty in every
deployment, `EffectivePolicy` answered `ErrNoPolicy` everywhere, and
`risk_decisions` had never received a row. The kernel's own comment described
`DefaultGlobalPolicyJSON` as "used only to seed a fresh deployment" for a
seeding step that did not exist.

Two of the limits PART XXXII names did not exist either -- native-market
concentration and creator concentration. Neither can be denominated in USD,
because a Credit has no approved external value, so both are integer ratios.

**Choosing the denominators was the whole difficulty, and the first two choices
were both degenerate.** A share of the FLOAT refuses the first buyer of every
market (they hold all of it). A creator's share of native SPEND refuses every
account's first native purchase (it is 100% of their native spend). Both were
replaced with denominators that mean the same thing on day one and day one
thousand: total supply, fixed at mint and unmovable by any participant, and the
account's whole Credit position, spent plus still spendable.

Four parts landed: the limits; the caller (`nativemarket.Execute`, after pricing
and before posting, refusing with `RISK_CONCENTRATION` and never refusing a
SELL); the seeding step (`scripts/riskpolicy`, which refuses to write the
compiled-in starter limits outside LOCAL/DEV/TEST); and the evidence -- an ALLOW
recorded inside the trade's transaction, a REJECT recorded by the httpapi
adapter AFTER the transaction it aborted, because a second pool connection taken
while holding the first is exactly F-27.

Under the real default limits five of the fifty-two `internal/nativemarket`
integration tests began failing, every one of them buying a genuinely
concentrated position. They were changed to buy positions inside the limits,
each with a comment saying why. The alternative -- a fixture policy that relaxes
the limits it is meant to prove -- is the test equivalent of turning the control
off, and it is worth naming as a temptation because it would have been three
lines instead of five edits.

`make seed-economy`, the CI e2e job and the restore drill all record a policy
now; the drill's 118-table row-count comparison covers `risk_policies` and
`risk_decisions` with rows in them.

### F-35: `make lint` had never passed either

Third target found red on arrival this session, after `make sast` and the
backup drill's Domain A coverage. Seventy findings, from three causes:

  - `fmt-check` red on fifteen COMMITTED files, none of them written here;
  - `misspell` set to `locale: US` against a repository written largely in
    British English -- 56 findings, one of them CHEQUE, a payout instrument
    kind whose spelling is the name of the thing. `locale: UK` gives 692,
    because the prose genuinely mixes. The locale is now unset: misspell still
    catches typos and stops adjudicating a house style nobody wrote down;
  - fourteen real findings, all fixed, including six unchecked type assertions
    that would panic rather than fail, three doc comments orphaned by
    insertions (the original symbol left undocumented, the new one introduced
    by a sentence about something else), and a bidi control character in the
    test that proves bidi names are refused -- invisible to gosec, which builds
    only the default tag set.

The Trojan Source finding was the third in this project, each found by a
different tool and each in a file the others do not read -- and a sweep found
the character in two readiness documents, including the paragraph describing its
removal from the code. `test/source` now reads every text file whatever its
extension and whatever build tag guards it, refuses the nine bidi embedding,
override and isolate codepoints, and was observed failing on a planted one. It
runs in the fast tier beside test/docs and test/reachability.

And the two formatters disagreed: `scripts/fmtcheck` ran plain `gofumpt -l`
while `.golangci.yml` sets `extra.group-params`, so `make fmt` could produce a
file `make lint` rejects. fmtcheck passes `-extra` now, and the 25 files that
gap was hiding are formatted.

`make lint` exits 0.

### Five agents, read-only, and what they found (F-36 to F-40)

The first wave of parallel work in this session was five read-only auditors:
two on worker reachability, three adversarial on the compiler-and-gates spine,
the money spine, and the API authorization surface. Every claim below was
re-verified by reading the code before anything was changed -- an agent's report
is a lead, not a fact.

**F-36, the worst thing found in this project so far.** `RequireAccount` returns
nil for any principal holding `account:read_any`, and that was the ONLY tenant
check on the write routes as well as the read ones. RoleAdmin is every
permission except the dual-control and agent-only sets, so it holds that read
override AND `native_market:trade`, `commerce:buy`, `payout:create`,
`withdrawal:create`. One ADMIN session could trade, buy and reserve a payout out
of any customer's balance -- no second signature, no admin action, and an audit
trail that looks exactly like the customer having done it themselves. Fourteen
write routes. Fixed with an ownership-only scope, a source-level check that no
mutating handler can use the read one, and an HTTP test that also asserts the
operator can still READ that account and still trade out of their own.

**F-37, found by F-36's test failing for the wrong reason.** Proving the
operator was refused needed a real request through the real port, and the first
honest run -- by the account's own owner -- returned 400 "an order needs
effective_at". `POST /v1/native-markets/{id}/orders` had never worked in any
deployment. The handler does not stamp the time (correctly) and the adapter did
not either, while the payout and commerce adapters both do, three files away.

The lesson is a new wrinkle on the old one: fifty-two integration tests, seventy
browser tests, a load script and a chaos suite all exercised the domain service
exhaustively, and none of them sent this request. **The tests walked a path
BESIDE the real one.**

**F-38.** The compiler distinguished "the gate is off" from "the policy said no"
by comparing a reason code against a string literal. A reason code is a string a
policy author writes, so a hand-authored DENY carrying "CAPABILITY_NOT_ACTIVE"
-- with no RequiredCapability, so nothing else fired either -- produced a Route
with no reasons and Permitted true. Now the router records the fact in a field a
Rule cannot set, and Validate reserves the code.

**F-39.** `scripts/lintfin` enforces "no float in a money path" over a directory
list written before Domain A existed. Credit, nativemarket, commerce, payout and
valuedomain were never scanned. No float was there; the control was not either.

**F-40.** This register said migration 00711's CR004 "refuses a lot EVENT whose
journal transaction never touched the account". It does not -- CR004 is raised
only by the trigger on `credit_lots` INSERT, and `cp_credit_lot_apply_event`
never reads the column, which is nullable. A false claim about a database
guarantee, in the document whose job is to be believed.

**Also de-tautologised.** `TestGolden_KillSwitchKindsValid` iterated the
declared kind list and asserted each was a declared kind. It now reads the kinds
the corpus actually names, which is what it always claimed to do.

### Wave two, and the one that changes an answer (F-41 to F-46)

Four more read-only auditors: the SQL surface and its grants, the browser
interface against the honesty rules, the documents against the code, and the
reality/prediction spine. Everything below was re-verified by reading before
anything changed.

**F-43 is the one that matters.** `gates.IsHighRisk` returns true for
MARKETPLACE (F-16 put it there, because it gates the minting of the only
withdrawable creator-earning provenance). `cp_gate_is_high_risk` listed
seventeen capabilities and Go listed eighteen, and the one they disagreed about
was that one. Migration 00701 calls the SQL copy "the line that holds when the
Go check is bypassed" -- GT003, four evidence references, three distinct
principals. For MARKETPLACE that line was absent. Nothing was exploitable
through the API because gates.Admin asks Go first; what was missing is exactly
what the second copy exists for.

The compliance document asserted the opposite of the code, and asserted it as a
CORRECTION to an earlier table -- the form a reader trusts most. Migration 00716
restores the parity and a test now drives both lists and compares them.

**F-44 is the worst thing a user could see.** `nativemarket.PriceScale` is 18.
The web app rendered `spot_price` and `effective_price` with a formatter that
only inserts thousands separators, so a price of a few thousandths of a Credit
was displayed as a sixteen-digit number of Credits -- on the field labelled
Price, marked emphasis, and on the effective price of a live quote. The API
returns `price_scale` on both responses and nothing read it.

**F-41**: three by-id reads answered more than they should -- a membership
oracle on GET /payouts/{id} whose own sibling has answered NOT_FOUND since F-29,
and two unpublished records (a DRAFT product with its fee split, a DRAFT asset
with its moderation notes) readable by any customer. The tests needed the ports
wired into the harness first, which is F-37 arriving a second time: a test
asserting a refusal would have passed against a route that answered UNSUPPORTED.

**F-45**: the browser suite's own unit tests had been red -- on the purchase
spec written earlier in this session, which parsed Credits into doubles and
subtracted them as its central assertion. Fourth target found red on arrival,
after make sast, the backup drill and make lint.

**F-46**: the nine-page browser check asserted absences before the page loaded.
F-32's fix had been applied to the five internal-economy pages and not to these.

**F-42 is OPEN and stays open.** Migration 00603's header says the application
role cannot set the transaction-local flag that binds a state change to its
audit row. It can -- internal/reconciliation does exactly that on a cp.-prefixed
key -- and sixteen of the seventeen guarded tables also hold unrestricted UPDATE
for that role. Two cheaper repairs were tried against this project's own
PostgreSQL 16 and rejected with evidence: an xmin check does not survive the
savepoints the admin executor uses, and a created_at check does not survive the
fake clocks the suites use. The real fix is capability_gates' treatment applied
to sixteen more tables, and that is not something to start at the end of a
session.

### The database as a boundary, tested rather than described (F-47 to F-50)

Four findings from reading migrations/ and docker/postgres/init as an attacker
would, and one more the fix for them uncovered.

**F-47 is the one I got wrong, and the suite caught it.** cp_readonly and cp_ops
can read identity_pii and sessions, because the role bootstrap grants a blanket
default SELECT while migration 00010's grant list deliberately withholds both.
That much is true and checked against a live database.

I revoked them. test/integration/migrations/privileges_test.go states the
opposite contract in a doc comment -- "cp_readonly and cp_ops can SELECT
everything and write nothing" -- and failed. It also lists sessions under
opsHousekeeping, where cp_ops performs retention cleanup, which a DELETE ...
WHERE cannot do without SELECT on the columns it filters on. The revoke would
have broken a documented operational job.

So the finding is not a privilege leak. It is that TWO DELIBERATE STATEMENTS IN
THIS REPOSITORY CONTRADICT EACH OTHER and the contradiction is invisible because
one of them silently wins. Resolving it is a policy decision about personal
data with an operational constraint attached, and it is recorded OPEN with both
sides rather than settled unilaterally in a migration.

The reusable lesson: an auditor's framing was acted on without checking whether
another part of the repository stated a different contract. The suite is what
caught it, which is the suite working.

**F-49**: LG001 asks the ACCOUNT ROW whether it may go negative, and the column
was NOT NULL DEFAULT false with cp_app holding INSERT -- so whoever created an
account chose its own exemption from the guard. The application never did; the
check simply was not where the guard reads it.

**F-48**: five SECURITY DEFINER trigger bodies did not pin pg_temp, while 00701
names that exact hazard for the sixth.

**F-50 was found by a test that a comment claimed already existed.** Two
comments promised Go/SQL parity tests -- `internal/assets` for the kind/domain
rule, migration 00711 for the finality table -- and neither test had ever been
written. Writing them made the claims true; the assets one FAILED ON ITS FIRST
RUN.

`assets_kind_domain_agree` accepts an asset with NO value domain. For a CREDIT
row with value_domain NULL, `true AND NULL` is NULL, the other disjuncts are
false, and NULL OR false is NULL -- and a CHECK constraint fails only on FALSE.
A CREDIT asset with no domain inserts cleanly as cp_app. Contained rather than
harmless: cp_ledger_account_domain raises VD001 for such an asset, so it cannot
hold an account, but it can exist and be referenced.

**Any CHECK whose expression can be NULL is a CHECK that accepts.** That
constraint had been read by several people and cited in a doc comment as the
thing Go mirrors.

The register's "deliberately NOT raised" entry about the chart of accounts being
duplicated between Go and SQL has been struck out: it said "a maintainability
risk, not a defect", and it became both -- F-43 in a security control and F-49 in
a ledger invariant. Four parity tests now drive both copies. What is still
uncompared is the transaction-kind list and the credit-origin list.

### F-52 closed: the frozen account has a button now

The reconciliation worker raises records and blocks_new_risk is read by buying
power, so an automated check removes an account's capacity to trade. cmd/api set
Reconcile: nil, so both admin endpoints answered UNSUPPORTED and
Engine.ResolveManual had no caller outside its own tests. A deployment could
freeze somebody and had no button; the only recourse was a hand-written UPDATE,
which is what the admin plane exists to replace. F-29 one level up.

cmd/api now builds a RESOLUTION-SHAPED engine -- no observers, no adapters, and
deliberately NO LEDGER -- and httpapi.NewReconciliationPort serves both routes.

Three domain rules the fix respects rather than works around, each found by the
suite refusing an earlier version:

  1. INVESTIGATING comes before RESOLVED_MANUAL, because somebody must have
     looked. A worker-raised record is MISMATCH or ESCALATED, so a resolve-only
     endpoint could not clear a single record the worker produces -- fixed in
     name only. The adapter moves those two into INVESTIGATING first, carrying
     the operator's own reason.
  2. A record may change status at most once per transaction, which is what
     binds each transition to exactly one audit row. So the investigate commits
     first and the resolution follows. A failure between them leaves the record
     INVESTIGATING, which still blocks: nothing is lost and repeating the call
     finishes it.
  3. An OPEN record is not operator-resolvable, because OPEN means the engine
     has not decided there is a difference. The adapter does not force a path;
     the test asserts the refusal.

A compensating posting is refused BY NAME. It is the only way a resolution
changes financial state, and building one from an API request means choosing a
posting kind, an idempotency key, a reference and an owner per entry. The engine
carries no ledger, so the refusal and the domain agree even if somebody later
changes only one of them.

## 0.3 Next exact work, in order

1. **Stages 22–24** — the provider sandbox, the re-audit and the evidence package.

The PART LXXII adversarial list is complete: thirty of thirty, item by item in the readiness report
§3a. The last three (out-of-order webhook, cross-account wash trading, contradictory provider status)
closed after this document last said they were open, and closing item 23 found F-23.

## 0.4 Verification commands that matter

    make lint               # fmtcheck (-extra), vet, staticcheck, golangci-lint, lintfin -- green since F-35
    make integration        # scripts/inttest: every //go:build integration package, one fresh DB each
    make integration-race   # the same over the financial core, with -race
    go test -count=1 -run FuzzCurve -fuzz FuzzCurve_NeverBreaksTheInvariant -fuzztime=60s ./internal/nativemarket/

**A killed `make integration` wedges the next one.** `scripts/inttest` runs each
package with its own pool -- nativemarket alone opens 30 connections -- and
Postgres here allows 200. Killing a sweep mid-run leaves orphaned `go test`
children holding their pools, and the next sweep then blocks inside `db.Open`
waiting for a slot rather than failing: it creates a database or two and stops,
with no locks, no active queries and no error. An hour was lost to this before
the cause was clear. Check `SELECT count(*) FROM pg_stat_activity` before
blaming the tree, and let a sweep finish or wait for its children to exit.

**Run every suite twice against the same database before believing it.** This project has now been
bitten three times by a fixture that passes on a fresh database and fails on the second run — most
recently by a test that created a Credit asset per call when the schema permits exactly one.

## 0.5 Failing tests

None. `go build ./...`, `go vet ./...`, `go vet -tags=integration ./...`, the short unit suite, and
the full integration sweep are green at the time of writing.

---

# PREVIOUS GOAL — HISTORY (accurate for Domains B and C)

## 1. Current milestone

**STAGE 0 — FORENSIC REPOSITORY AUDIT → STAGE 1 — FOUNDATION CONTRACTS**

Stage 0 findings (2026-09-05):

- Repository was **greenfield**: the only file present was the goal document. No git history, no code, no research diligence file (`deep-research-report*.md` searched at repo root and `C:\Dev\*`; the two hits in sibling projects are unrelated to this product).
- Host toolchain at session start: Node 24.14.0, pnpm 10.34.5, Python 3.11.9, git 2.53, Docker Desktop installed but daemon stopped, winget available. **Missing:** Go, make, terraform, psql, sqlc, buf, golangci-lint, staticcheck, govulncheck, gosec, k6, gitleaks, trivy, syft.
- Network access to proxy.golang.org and registry.npmjs.org confirmed.
- Actions taken: `git init -b main`, local git identity set, Docker Desktop launched, winget installs of Go 1.27.1 / ezwinports.make / Hashicorp.Terraform started.

Stage 0 exit criteria: toolchain installed, baseline build runnable, persistent build state exists (this file + 3 companions). **MET 2026-09-05**: Go 1.27.0 verified (SHA-256 checked zip at `C:/Dev/tools/go`; the winget MSI is stuck behind a UAC prompt the operator may approve or dismiss), Docker stack healthy (7 services), Go tools in `./bin`, git initialised, build-state docs present.

Stage 1 (foundation contracts) started 2026-09-05 with six parallel subagents: `internal/money`, `internal/{id,clock,errs}`, `internal/{config,observability}`, `internal/{db,idempotency}` + `cmd/migrate` + migrations 00001–00003, tooling (`scripts/tool`, `scripts/fuzzall`, `scripts/maketargets`, `.golangci.yml`, CI workflows), `internal/{security,auth}`. Contracts fixed in `docs/architecture/CONVENTIONS.md`.

Stage 2 (financial core) design fixed in `docs/architecture/FINANCIAL_MODEL.md`. Draft migrations 00010 (identity/accounts/sessions) and 00100–00105 (assets, ledger, capital, funding, positions, valuation) were applied to a scratch database and the ledger triggers were exercised: balanced posting commits; unbalanced (LG002), entry-less (LG002), negative-balance (LG001), asset-mismatch (LG004), and mutation (LG003) attempts are rejected; `cp_app` can post but cannot update `ledger_balances` (SECURITY DEFINER trigger). Drafts move into `migrations/` once the db agent's 00001–00003 land.

## 2. Completed milestones

| Milestone | Date | Evidence |
|---|---|---|
| Repository audit (greenfield confirmed) | 2026-09-05 | this file §1 |
| Persistent execution memory created | 2026-09-05 | `docs/build/*.md` (traceability: 389 rows) |
| Stage 0 exit: toolchain + local infra + build state | 2026-09-05 | `go version`, `docker compose ps` healthy, `./bin/*` tools |
| `internal/money` (exact numerics) | 2026-09-05 | 74 tests / 1610 subtests, 5 fuzzers 10 s clean, `-race` green |
| `internal/{id,clock,errs}` | 2026-09-05 | 74 tests, fuzz clean, `-race` green |
| `internal/{config,observability}` + `.env.example` | 2026-09-05 | 67 tests, every PROD validation rule tested, fuzz clean |
| `internal/{db,db/migrate,idempotency}`, `cmd/migrate`, migrations 00001–00003 | 2026-09-05 | 32 unit + 19 integration tests `-race` green; checksum verify; ProtectedVersion guard |
| Migrations 00010–00301 (identity, audit, provider events, assets, ledger, capital, funding, positions, valuation, gates, kill switches, policies, admin, instruments, intents/quotes, plans/orders, wallets/execution, reconciliation) | 2026-09-05 | applied via `cmd/migrate up` + `verify: ok`; ledger triggers exercised manually (LG001–LG005) |
| `internal/{assets,accounts}` registries | 2026-09-05 | compile + vet clean (integration tests pending in Stage 2 wave) |
| Tooling: `scripts/{tool,fuzzall,maketargets,lintfin,images,testdb,fmtcheck,supplychain}`, `.golangci.yml`, `.gitleaks.toml`, CI + release workflows, `build/Dockerfile` | 2026-09-05 | `go test ./scripts/...` green; all 13 pinned tools installed with checksum verification |
| Architecture docs: SYSTEM, FINANCIAL_MODEL, POLICY_AUTHORITY, SETTLEMENT_COMPILER, EXECUTION, RECONCILIATION, STRATEGY_IR, AGENT_RUNTIME, POINT_IN_TIME, CONVENTIONS; 19 ADRs | 2026-09-05 | `docs/architecture/*`, `docs/adr/*` |
| `internal/security` (RBAC matrix, tenant scoping, step-up, agent principal) + `internal/auth` (sessions, OIDC+PKCE, dev IdP, chi middleware) | 2026-09-05 | 84 tests / 509 subtests `-race` green; golangci-lint 0 issues; flaky "tampered cookie" case fixed (D-014 records the go-oidc deviation) |
| Migrations 00500–00601 (strategies, agents, predictions/tools, reality, backtests/performance) | 2026-09-05 | `cmd/migrate up` + `verify: ok` on `controlplane_test` |

| `internal/auth/pgstore` (Postgres SessionStore) + migration 00011 (break-glass column) | 2026-09-05 | integration tests on isolated DB; exposed and fixed the default-privilege defect (D-016) |
| Migration renumbering: audit_events → 00106, provider_events → 00107 (protected range) | 2026-09-05 | migration suite green incl. foundation rollback on a foundation-only DB |
| Privilege audit test (`test/integration/migrations/privileges_test.go`) | 2026-09-05 | green: cp_app no DELETE anywhere, no UPDATE on append-only tables; ops housekeeping allowlist explicit |
| `internal/provider` (health states with hysteresis, retry classes, verification labels) | 2026-09-05 | unit + property + concurrency tests `-race` |
| `internal/fees` (explicit platform fee policy, default zero) | 2026-09-05 | unit + property tests `-race`; lint clean |
| Provider API notes (`docs/api/providers/*`, 7 files, dated URLs) | 2026-09-05 | Jupiter/Solana RPC/Stripe API/Anthropic verified from official docs; Helius/Privy partial |
| `internal/instruments` (venues, exposures, spot pairs, listings, external ids, status transitions) | 2026-09-05 | integration tests on isolated DB `-race` green |
| `proto/controlplane/signing/v1` + buf config + generated Go (`internal/gen/proto`) | 2026-09-05 | `buf lint` clean; generated code builds; `make proto` / `make proto-breaking` targets |
| `internal/event` (envelope, topics registry, transactional outbox, relay with per-key ordering, inbox dedup, in-memory bus with fault injection) | 2026-09-06 | 62 tests incl. duplicate-delivery property (N∈[1,50]) and crash-between-publish-and-mark; `-race` green; fuzz 20 s clean |
| `internal/valuation`, `internal/positions` (FIFO lots, exact basis conservation), `internal/capital/buyingpower` | 2026-09-06 | 40 tests incl. `TestProp_BasisConserved`, `TestProp_BuyingPowerBounds`; re-verified on a fresh DB after D-016 |
| Migration 00602 (FIAT asset kind for USD quote references; ledger rejects fiat accounts) | 2026-09-06 | applied + verify ok; linear numbering adopted (D-015 addendum 2) |
| Migration 00603 (state changes of gates, kill switches, accounts, assets, instruments, deposits, withdrawals, intents, orders, reconciliation records, admin actions require a matching transition row in the same transaction; SQLSTATE AU001) | 2026-09-06 | closes threat-model gap "bare gate-state update"; `transition_binding_test.go` |
| `internal/gates` (10 capabilities, 7 states, five-condition activation, dual control) + `internal/killswitch` (12 kinds, action-class matrix, fast activate / gated release) | 2026-09-06 | 45 tests incl. 4 properties; re-verified on fresh DB with 00603 |
| `docs/security/SECURITY.md`, `docs/threat-model/THREAT_MODEL.md` | 2026-09-06 | implemented vs designed claims cite files on disk; top-10 residual risks listed |
| `internal/ledger` (per-asset double entry, canonical content hash, idempotent posting, deficit pattern, swap/funding builders, VerifyBalances) | 2026-09-06 | 28 unit (4 properties) + 12 integration tests; 50-goroutine concurrency; re-verified on fresh DB; D-017/D-018 recorded |
| Migration 00604 (column-level UPDATE(status) on ledger_accounts for cp_app) | 2026-09-06 | migration suite + ledger suite green |
| `internal/capital` (transactional reservations, envelopes with authority audit, holds, PnL/drawdown, verification) | 2026-09-06 | 43 tests; **PART 23 torture: 100 × $500 vs $10,000 → exactly 20/80 in every iteration** under row-lock and SERIALIZABLE, asset and envelope variants (25 iterations locally; CI target 1000); property `capital conserved`; re-verified on fresh DB |
| BuyingPower types unified: `capital` re-exports `capital/buyingpower` types (no duplicate contract) | 2026-09-06 | compile-checked |
| `internal/audit` (canonical JSON, per-stream hash chain under advisory lock, verifier, in-memory test writer) + `internal/admin` (dual-control actions, VerifyApproved, break-glass grant, savepointed Execute) | 2026-09-06 | 25 unit (+ 20 s fuzz) + 18 integration tests; 100-goroutine same-stream append; tamper detection; re-verified on fresh DB |
| `openapi/openapi.yaml` (OpenAPI 3.1: auth/sessions, accounts, buying power, holdings, ledger, activity, export, markets, quote preview, intents, orders, funding, withdrawals, SSE, webhooks, admin gates/kill switches/actions/reconciliation/instruments/providers, health/version) + `docs/api/README.md` + generated strict server (`internal/gen/api`) | 2026-09-06 | spec generates and builds (`make openapi-server`) |
| Repository-wide lint sweep: `golangci-lint --build-tags=integration ./...` 0 issues; gofumpt clean; `go test -race ./...` 35/35; integration 32/32 on fresh DB | 2026-09-06 | lint agent report; 76 findings fixed without weakening tests |
| `REQUIREMENTS_TRACEABILITY.md` refreshed from on-disk evidence: 389 rows → VERIFIED 81, IMPLEMENTED 49, IN_PROGRESS 138, NOT_STARTED 103, DEFERRED 18 | 2026-09-06 | every VERIFIED row names an on-disk test function and a recorded pass |
| pnpm workspace + `packages/generated-client` (openapi-typescript schema, openapi-fetch client with idempotency/correlation/problem+json handling, node tests) + CI drift jobs for OpenAPI server/client and proto | 2026-09-06 | `pnpm generate/typecheck/test` green |
| `internal/notification` + migration 00640 (in-app notifications written in the causing transaction, dedup on replay, tenant-isolated listing, immutable content, SKIP LOCKED dispatcher with provider abstraction) | 2026-09-06 | integration tests on fresh DB `-race`; `db.IsImmutableRow` now recognises SQLSTATE LG003 from every domain trigger |
| `internal/compliance` (profile repository; SYSTEM/OPERATOR writers only; before/after-hashed audit events; fail-closed validation) | 2026-09-06 | integration test on fresh DB `-race`; lint 0 |
| `internal/intent` (typed intents, exhaustive transition table, canonical content hash, idempotent submit with tenant/agent guards, transition + outbox + audit in one tx) + `internal/quote` (immutable quotes, freshness/expiry, fee disclosure with no hidden spread) + migration 00605 (content hash + identity/economics immutability trigger IN001) | 2026-09-06 | intent 19 unit + 8 integration, quote 6 unit + 1 integration; re-verified on fresh DB `-race`; lint 0 |
| `internal/funding` (PART 28 state machine with provider-owned skip-forward, settlement/availability/reversal postings incl. deficit two-transaction pattern + account FREEZE + alert row, lifecycle driver), `internal/withdrawal` (boundary: human actor type, step-up, WITHDRAWALS gate always refused today, destination + velocity policy), `internal/webhook` (raw-persist → verify → replay window → hash → provider_events + inbox → dispatch in one tx; forged/stale/oversized/unknown handled), `internal/provider/stripe` (onramp sessions, Stripe-Signature v1, exact decimal amounts, fake mode rejected outside LOCAL/TEST/DEV), `test/contract/stripe` (6 fixtures + README) | 2026-09-06 | 85 unit + 46 integration tests incl. `TestFunding_ReversalCreatesDeficitAndFreezes`, `TestProp_DuplicateWebhookOneEffect`; re-verified on fresh DB `-race`; lint 0 |
| `internal/provider/jupiter` (Swap API V2 `/order`, `/execute`, `/build`; exact string amounts; execute = UNKNOWN_EFFECT_WRITE with exactly one HTTP attempt; raw evidence with redaction; deterministic fake producing real serialized transactions) + `test/contract/jupiter` (21 cases, 12 fixtures, README labelling documented vs assumed) | 2026-09-06 | 45 unit + 4 fuzz (20 s clean, one real parser finding fixed) + 1 property + 21 contract; lint 0 |
| `internal/chain` (ChainObserver/SolanaDataProvider contracts, agreement policy that never picks the optimistic answer, MultiObserver with degraded capping, `ProvenAbsent`, chaintest simulator with slot/blockhash-expiry/fault injection) + `internal/provider/solanarpc` (strict JSON-RPC: `UseNumber`, float rejection, retries only on SAFE_RETRY reads, archive-before-parse, exact delta arithmetic) + `internal/provider/helius` (composes solanarpc; DAS balances; `x/net/websocket` stream with polling backfill) + `test/contract/{solanarpc,helius}` (35 fixtures, READMEs) | 2026-09-06 | 93 test funcs incl. 2 agreement properties, `TestProp_DeltasSumToFee`, `FuzzParseTransactionResponse` 20 s clean; re-verified `-race`; lint 0 |
| D-014 completed: `internal/auth/oidc` verifies ID tokens with `coreos/go-oidc` (discovery + issuer refusal, alg allow-list, signature, iss/aud/exp/nbf) over a package-local go-jose KeySet (rate-limited JWKS refresh, unknown-kid vs bad-signature distinguished, `use=enc` filtered); nonce/azp/auth_time/amr/acr/step-up remain explicit; PKCE exchange via `oauth2`; 1 MiB body cap + timeout transport | 2026-09-06 | 62 test funcs / 150 subtests `-race`; every original negative test unchanged; identity integration green; lint 0 |
| `internal/signing/inspect` (pure decoder for legacy + v0 with ALT resolution; all 16 EXECUTION §2 checks; hand-written SPL/System/ComputeBudget/ATA layouts cross-checked against spec vectors; 57-case mutation table; 2 fuzzers 30 s clean) + `internal/signing` (service rebuilds expectations from persisted rows only, one provider call per attempt under concurrent replay, `signing_decisions` + `signing_results` + security event on rejection, gRPC server + in-process client) + `internal/wallet` (+ `wallettest` deterministic ed25519 fake) + `internal/provider/privy` (SDK v0.15.0 names verified by `go doc`; policy with `programId` allow-lists) + `test/contract/privy` + migrations 00615 (wallet transitions + binding) / 00616 (`signing_results`) | 2026-09-06 | 47 test funcs + 2 fuzz; re-verified `-race` on fresh DB; lint 0. **Caveat SB-007:** Jupiter v6 instruction layout reproduced from memory (UNVERIFIED) |
| Outbox retry deadline persisted (migration 00642 `next_attempt_at` + indexes; relay claims by deadline and sets failure_time + backoff(attempts)); relay integration test corrected to failure-based timing | 2026-09-06 | event suite green on fresh DB; a permanently failing row can no longer hot-loop |
| `infra/terraform` (12 modules: kms with separate ECC signing key, network with private data subnets + endpoints + flow logs, rds Multi-AZ/PITR/force_ssl + roles bootstrap SQL honoring D-016, redis, s3-evidence with COMPLIANCE Object Lock, secrets with per-task-role matrix asserted by `check` blocks, ecs-cluster + immutable ECR, ecs-service per binary with read-only rootfs/non-root/circuit breaker, app-config `CP_*` map, waf-edge, observability alarms/SNS, iam-deploy GitHub OIDC) + dev/staging/prod environments (prod refuses fake modes and short Object Lock) + `docs/operations/DEPLOYMENT.md` | 2026-09-06 | `terraform validate` success in all three environments (integrator re-ran prod), `fmt` clean, trivy config 0 HIGH/CRITICAL; **never planned/applied** (EB-012) |
| `docs/runbooks/` — index + 19 runbooks (PART 157 set, global kill + re-enable, submission unknown), each with trigger/blast radius/first 10 minutes/diagnosis/containment/what-not-to-do/exit criteria/post-incident, naming real admin routes, kill-switch kinds, admin-action kinds, and read-only SQL; unimplemented controls marked PENDING | 2026-09-06 | 1,498 lines; codename absent |
| `test/security` (agent trees never import authority packages; signing service imported only by the execution boundary; agent permission set closed; PROD/STAGING refuse fake providers, seed, debug auth; LOCAL defaults never production-valid) | 2026-09-06 | `make security` is no longer vacuous; `-race` green |
| `test/load/*.js` (portfolio reads, quote load, reservation contention with idempotent replay, 200 SSE clients) + README | 2026-09-06 | `k6 inspect` valid; **no measurements yet** (no API binary) |
| Capital fixture updated for migration 00605 (`trade_intents.content_hash` mandatory); full capital suite green again on fresh DB | 2026-09-06 | `go test -race -tags=integration ./internal/capital/` |
| `docs/operations/RECONCILIATION.md` (operator triage/resolution procedure; engine marked PENDING) + restore drill added to the CI integration job | 2026-09-06 | doc; CI still unexecuted (SB-004) |
| Permission matrix extended (D-021): `envelope:authority_write`/`envelope:approve`, `agent:promote`/`agent:promote_approve`, `withdrawal:review`, `break_glass:approve`; admin kind table now uses dedicated approve-side permissions; golden matrix at 42 permissions | 2026-09-06 | security + admin unit tests `-race` green |
| `scripts/seed` (`make seed`): LOCAL/DEV/TEST-only, local-host-only dev data — devnet USDC/SOL + USD fiat reference, SOL/USDC on JUPITER, conservative policies, prices, dev identities with accounts (admin gets ADMIN operator role), 10,000 fake USDC SEED posting; idempotent | 2026-09-06 | run twice against the migrated local `controlplane` DB (second run: "already present") |
| `internal/stream` (SSE hub: bus-fed, per-tenant filtering, monotonic ids, bounded replay with `resync` on gap, slow-consumer drop, heartbeat; payload summarised to ids/states only) | 2026-09-06 | unit tests incl. memory-bus attach and live SSE framing `-race` green; lint 0 |
| `internal/ratelimit` (fixed-window limiter; Redis store with atomic Lua INCR+PEXPIRE; memory store; chi middleware with RateLimit/Retry-After headers and problem+json; fail-open option; not financial authority) | 2026-09-06 | unit + property + concurrency + Redis integration against local compose `-race` green |
| `internal/identity` + migration 00641 (OIDC login: single-use persisted state/nonce/PKCE, first-login user + CUSTOMER account creation, roles from `operator_roles` never from claims, step-up enforcement, session issue via `auth.Manager` + `pgstore`, audit + security events, logout) | 2026-09-06 | integration tests on fresh DB `-race` green (first login, replay refused, expiry, step-up, operator roles, logout); lint 0 |
| Backup/restore drill (`scripts/restoredrill`, `internal/testkit/localdb`, `docs/operations/BACKUP_RESTORE.md`) | 2026-09-06 | local run OK: pg_dump → pg_restore → migrate verify → 89 tables row-count match, 0 balance drift, journal hash match (7.8 s); production procedure documented, unexercised (EB-012) |
| `internal/eligibility` (typed policy, fail-closed evaluate, decision store) + `internal/risk` (typed policy, Compose strictest-wins, pure kernel with kill-switch matrix, resulting constraints, order-rate from persisted intents) | 2026-09-06 | 43 test functions / 472 subtests; 137 golden fixtures (50 + 87); 1,000-iteration determinism per fixture; re-verified on fresh DB |

## 3. Current work (2026-09-06, wave 2 — Stages 4–8, six parallel agents)

| Agent scope | Packages | Migration numbers reserved | Status |
|---|---|---|---|
| Intent + quote models, idempotent submit | `internal/intent`, `internal/quote` | 00605 used | **landed + re-verified** |
| Execution records (orders/attempts/fills, adapter contract, finality) + Settlement Compiler (planner, plan repo, resumable executor, dry-run) | `internal/execution`, `internal/settlement` | 00610–00614 | in progress (executor core; build currently broken by the in-progress file) |
| Transaction decode + inspector (all 16 checks, fuzzed), signing service (+ gRPC adapter), wallet contracts, Privy adapter, contract tests | `internal/signing`, `internal/wallet`, `internal/provider/privy`, `test/contract/privy` | 00615–00619 | in progress (inspect done; wallet/privy/contract tests) |
| Funding state machine + postings, withdrawal boundary, webhook pipeline, Stripe onramp adapter, contract tests | `internal/funding`, `internal/withdrawal`, `internal/webhook`, `internal/provider/stripe`, `test/contract/stripe` | none needed | **landed + re-verified** |
| Chain observation contracts + agreement policy, Helius and fallback RPC adapters, contract tests | `internal/chain`, `internal/provider/helius`, `internal/provider/solanarpc`, `test/contract/{helius,solanarpc}` | — | in progress (helius currently fails `-race`) |
| Jupiter Swap V2 client + fake, contract tests | `internal/provider/jupiter`, `test/contract/jupiter` | — | **landed + re-verified** |
| Terraform AWS V1 + DEPLOYMENT.md | `infra/terraform`, `docs/operations/DEPLOYMENT.md` | — | in progress (43 files on disk; validate/scan pending) |
| Incident runbooks (PART 157 + global kill + submission unknown) | `docs/runbooks` | — | in progress |
| D-014: switch ID-token verification to go-oidc | `internal/auth/oidc`, `internal/auth/authtest` | — | in progress |
| Stage 9: Strategy IR + deterministic evaluator + effect system + NL compiler (Anthropic adapter, schema-constrained, provenance) + TypeScript SDK with Go/TS hash-parity fixtures + PART 170 golden corpus | `internal/strategy`, `internal/model`, `packages/strategy-sdk` | 00650–00654 | in progress |
| Stage 11: ObjectArchive (S3/MinIO, Object Lock), raw archive + normalizer (six timestamps), ClickHouse store, checkpoints/gaps/dedup, look-ahead leakage test, Redpanda bus (franz-go), market-ingest worker | `internal/archive`, `internal/reality` (+ `redpandabus`), `cmd/market-ingest-worker` | 00600 used; 00660–00664 unused | **complete in-package**: unit + integration tiers green twice on one database and one broker, franz-go producer/consumer landed and verified against live Redpanda, `cmd/market-ingest-worker` landed |
| Stage 13: Merkle checkpoints over the audit chain, KMS/local ECDSA signer, WORM archive of checkpoints, `verify` with tamper tests, PART 88 proof bundle, `cmd/audit-worker` (`make verify-audit`) | `internal/proof`, `cmd/audit-worker` | 00670–00674 | in progress |

Landed and re-verified since the table was written: chain observers (Helius/RPC), signing/wallet/Privy, Terraform, runbooks. Still running: execution/settlement (build currently broken by its in-progress executor), go-oidc, and the three new stages above.

Session interruptions: four API-limit cutoffs so far; every agent was resumed by message and completed or is completing. Each landed package is re-verified by the integrator on a fresh isolated database before being recorded above.

**2026-09-06 resume (wave 3, six agents).** Three agents had been cut off mid-task; an on-disk inventory showed all three had landed more than their last message claimed, so each was resumed with a précis of what was actually on disk rather than restarted:

| Agent scope | On-disk state at resume | What remained |
|---|---|---|
| Execution + settlement | `executor_steps.go` present; package builds; tests compile | **COMPLETE + integrator-verified on a fresh DB** |
| Jupiter timeout hardening | `hangTimeout = 2s` + 10s harness defaults applied in both `internal/provider/jupiter/client_test.go` and `test/contract/jupiter/contract_test.go` | **COMPLETE — 15 contended passes per timeout test, 0 flakes** |
| Strategy IR + TS SDK | `internal/strategy/ir` has decimal/effects/hash/ir/parse/schema (76K, compiles) | zero tests exist; compiler; TS SDK; G115 lint |
| Reality + archive | 9 files; `parseEventID` undefined — sole repo build break | close the build break; look-ahead leakage test |
| Audit proof | 8 files, compiles | adversarial reject tests; QF1002 lint |
| **Reconciliation (Stage 7, new)** | not started | whole package + `cmd/reconciliation-worker` |
| **HTTP API composition root (Stage 8, new)** | `openapi/openapi.yaml` + generated chi strict-server in `internal/gen/api` already exist; chi v5.3.2 pinned | `cmd/api` + `internal/httpapi`: StrictServerInterface handlers, problem+json, Idempotency-Key, SSE, deny-by-default authz with a route-coverage test, fake-provider rejection in STAGING/PROD |

**Wave 3, second half — six agents, launched as earlier ones completed and freed capacity (the standing rule is at most six concurrent):**

| Agent scope | Packages | Migrations reserved | Status |
|---|---|---|---|
| Reconciliation engine (Stage 7) | `internal/reconciliation`, `cmd/reconciliation-worker` | 00680–00684 | running |
| HTTP API composition root (Stage 8) | `cmd/api`, `internal/httpapi` | — | running |
| Strategy compiler + TS SDK (Stage 9, items 2–3) | `internal/strategy`, `internal/model`, `packages/strategy-sdk` | 00650–00654 | running; IR already VERIFIED |
| Reality + archive (Stage 11) — **replacement agent** | `internal/reality`, `internal/archive`, `cmd/market-ingest-worker` | 00660–00664 | running |
| Agent runtime + prediction ledger (Stage 10) | `internal/agent`, `internal/prediction`, `cmd/agent-worker` | 00690–00694 | running |
| Execution + Temporal workflow workers | `cmd/execution-worker`, `cmd/workflow-worker`, `internal/workflows` | 00700–00704 | running |

Stage 10 and the workers were launched once Stage 13 (proof) finished and was verified. Stage 10 carries the system's defining security property: every tool effect in `tools` is a READ or a model call, there is deliberately no write effect, and an agent's only route to action is proposing an intent that independently-owned code then validates, risk-checks, reserves capital for and executes. `test/security/authority_boundary_test.go` enforces that structurally by parsing imports, and the agent was told explicitly that the test is not its to weaken.

Launched after execution/settlement landed and were verified, since the API composition root depends on them and on nothing the other four agents are still writing. It is the biggest single unblocker left: `test/load/*.js` (four k6 scripts, all passing `k6 inspect`) still has **no measurements at all** because there is no API binary to point them at.

Reconciliation was launched now because its dependencies (`internal/execution` records, `internal/chain` agreement policy) have landed and its schema already exists at migration 00301. Reserved migration range 00680–00684.

Deferred until this wave lands (to avoid `go.mod`/interface churn): D-014 switch to `go-oidc` (go-jose now pinned), `internal/reconciliation` engine (needs execution records), composition roots (`cmd/api`, workers), OpenAPI + generated client.
- Stage 2 wave (agents running): `internal/event` (outbox/inbox/relay), `internal/ledger`, `internal/capital` (+ PART 23 torture test), `internal/{positions,valuation,capital/buyingpower}`. `internal/funding` follows once the ledger poster exists.
- Stage 3 wave (agents running): `internal/{gates,killswitch}`, `internal/{eligibility,risk}`, `internal/{admin,audit}`.
- Design drafts in flight: `docs/architecture/{STRATEGY_IR,AGENT_RUNTIME,POINT_IN_TIME}.md` + migrations 00500–00601; verified provider API notes under `docs/api/providers/`.

## 3b. Integrator-verified this session (2026-09-06)

Every row below was re-verified by the integrator on a database the agent did not use, and — since a suite that passed only once concealed a real design defect today — **every integration suite was run at least twice against one database with nothing cleaned between runs**.

| Stage | Packages | Evidence |
|---|---|---|
| 4–6 execution + settlement | `internal/execution`, `internal/settlement` | fresh DB, integration+race, exit 0 (`settlement 107.598s`, `execution 77.124s`). Includes the crash-resume test at every step boundary: 17 steps × 3 phases = 51 fault-injected runs |
| 7 reconciliation | `internal/reconciliation`, `cmd/reconciliation-worker` | twice on one fresh DB (`44.278s` then `45.299s`). PART 49 crash recovery, PART 163 e2e, kill-switch-never-stops-reconciliation all pass |
| 9 strategy IR + compiler + SDK | `internal/strategy`, `internal/model`, `packages/strategy-sdk` | `ok strategy 2.253s`, `ok strategy/ir 13.561s`, `ok model 1.155s`, `ok test/security 1.645s`; TypeScript 29/29 with typecheck clean, independently reproducing the Go golden hash |
| 11 reality + archive | `internal/reality`, `internal/archive`, `cmd/market-ingest-worker` | twice on one DB, all green. Look-ahead leakage passes 100 rapid property cases; S3 Object Lock refusal verified against real MinIO |
| 13 audit proof | `internal/proof`, `cmd/audit-worker` | three consecutive runs on one DB; 11 tamper subtests each rejecting on their own reason; 5 key-rotation subtests |

**Verified later in the session:**

| Scope | Packages | Evidence |
|---|---|---|
| Execution + Temporal workflow workers | `cmd/execution-worker`, `cmd/workflow-worker`, `internal/workflows` | twice on one fresh DB (`execution-worker 15.073s / 12.970s`, `workflows 16.539s / 16.293s`). Timeout-is-not-failure, unresolved-submission-is-paused-not-failed (reservation stays ACTIVE), duplicate delivery serial + 8 concurrent, crash resume at all three SUBMIT phases, kill-switch halts trading but never settlement, and workflow replay against histories recorded from the live Temporal server |
| Redpanda bus (real franz-go client) | `internal/reality/redpandabus`, `cmd/market-ingest-worker` | `redpandabus 16.352s`; 7 integration cases incl. broker unreachable at startup, broker lost mid-stream (container paused), rebalance during consumption, and producer-error-never-reported-as-success. Redpanda returned to `healthy` on its own after the pause test |

**The replay test was proven non-vacuous by its own author**: injecting a single extra `workflow.Sleep` produced a determinism failure, and removing it restored green. That is the standard — a determinism guard that cannot fail protects nothing.

**Outbox relay host — the gap that made the event architecture inert.** `event.Relay` is implemented and tested, but **no binary ran it**, so every `order.transitioned`, `fill.observed` and `capital.*` event was written to `outbox_events` and never left the table. The API's SSE stream had no producer and the execution worker's bus waker was inert. `cmd/relay-worker` is now being built, and it fails closed rather than degrading to a loopback: a relay that appears to run and publishes nowhere is worse than one that will not start, because the outbox drains and the events vanish.

**Deferred deliberately: `go mod tidy`.** Both franz-go modules are still marked `// indirect` although `redpandabus` now imports `kgo` directly. The marker is only a comment and the build is correct. Tidying while agents are mid-write risks rewriting `go.mod`/`go.sum` underneath them, so it waits for the end of the wave.

**Stage 10 agent runtime — VERIFIED, and it closed a real authority hole.** Twice on a fresh DB (`agent 7.260s / 8.366s`, `prediction 1.811s / 1.906s`). Migration 00690 was genuinely needed: `agents` was the one lifecycle table not covered by 00603's transition binding, so `cp_app` could have run `UPDATE agents SET state='LIVE'` and moved an agent onto customer capital with **no transition row, no approval and no evidence**, bypassing the CHECKs already on `agent_lifecycle_transitions`. `TestBareStateUpdateIsRefused` now proves that raises AU001. The agent also fixed seven defects its own tests exposed, including refusal rows being rolled back (so a refused tool call left no evidence at all), paused agents disappearing from the dispatcher entirely, and silent int64→int32 truncation writing basis points.

**Depguard widened after that report, with a negative control.** The `agent-authority` rule covered only `internal/agent` and `internal/strategy`; it now also covers `internal/prediction`, `internal/model` and `cmd/agent-worker`. `test/security` scans those trees too, but only depguard forbids `internal/capital` and `internal/risk/policy`, so the two guards are not interchangeable. Verified before widening that none of the three imports a denied package, and verified after that the rule bites: a temporary `internal/capital` import in `internal/prediction` was rejected by name.

**FIRST LOAD MEASUREMENTS — see `test/load/README.md` for the full table.** `public_surface` 16,527 req/s with liveness p95 1.62 ms and readiness p95 4.56 ms including a database round trip; `portfolio_read` 0.00% failed over 10,929 requests with holdings p95 24.07 ms; `sse_clients` 200 concurrent connections with 100% receiving a frame. `quote_load` and `reservation_contention` remain unmeasurable because `POST /quotes/preview` correctly answers 503 with no venue adapter wired — blocked on provider credentials, not on code. Three defects in the load scripts themselves were found by running them, including an SSE check that could never pass against an endpoint that was working correctly.

**Outbox relay host landed, and it found an ordering defect in shared code.** `cmd/relay-worker` verified twice on one fresh DB (`3.496s / 3.592s`). The defect it reported, now fixed as D-036: `Relay.claim` uses `FOR UPDATE SKIP LOCKED`, so a batch is "the oldest rows nobody else holds", not a contiguous run of the outbox. The order check consulted only each partition's oldest batch row, so when another instance held a row in the MIDDLE of a partition, the rows behind it published straight past it and **a consumer would have seen an aggregate's 4th event before its 2nd.** The check now runs per row and excludes the batch's own predecessors; the regression test lives in `internal/event`, where the defect lived, and was verified non-vacuous.

**Repo-wide state: `go build ./...` clean, `go mod verify` all modules verified, 81 packages pass `-race`, `golangci-lint --build-tags=integration ./...` reports 0 issues.** First point in the session with no red anywhere.

**The relay's ordering property is now defended by tests that can fail (D-037).** The single-publisher lease was flipped to default-off on the merits once D-036 removed the break it was covering: it could never be airtight, and it caps drain rate while adding minutes of failover latency to the one worker whose purpose is keeping lag low. Two concurrent-instance ordering tests replace it, and the integrator confirmed they are not decorative — stubbing the D-036 guard makes both fail naming the out-of-order partitions. Both also assert that two instances demonstrably shared a partition, because a concurrency test that never actually contends proves nothing.

**Two guard fixes found by the post-landing sweep:**
- Migration 00700 (`execution_plan_leases`, authored by the **execution + workflow workers** agent under its reserved 00700–00704, not by the relay agent — the integrator initially misattributed it and the relay agent corrected the record) had a real `DROP TABLE` in a protected Down section. Made a no-op. The author's reasoning was sound (lease rows are operational, not financial truth) but the rule is deliberately blanket: the moment one table is exempt because its author judged it operational, the next author argues the same about one that is not. Separately, a rollback dropping that table while workers run would strip every in-flight plan of its lease mid-execution, which is an incident rather than a revert. **The lesson belongs to the execution-worker agent:** its own suite and `golangci-lint` were both clean, because the protected-migration guard lives in `internal/db/migrate`'s tests, outside its scope's test run. A landing is not verified until a repo-wide sweep has run.
- That guard scanned the raw Down text including comments, so a migration could not explain the rule in the words the rule names — a Down saying "must not DROP, DELETE, TRUNCATE or ALTER" tripped its own check. It now strips `--` comments before the keyword scan, verified with a negative control that a real `DROP TABLE` still fails.

**The local `controlplane` dev database was five migrations behind** (00642, 00670, 00671, 00690, 00700), which is why `relay-worker status` failed against it. Applied; `migrate verify` reports all applied migrations match the embedded files.

**Two safety properties worth naming, because both are enforced twice.** Reconciliation refuses agent resolution in Go *and* the database refuses `AGENT` as a resolver. Material resolution requires a real approval, and the tests cover the subtle attacks rather than only the obvious one: no approval, an approval belonging to a different record, an unknown approval, and a resolver who is not the authenticated principal.

**Cross-package defects the integrator found and fixed this session**, none of which any single package's tests could have caught, because each package was internally consistent: nine unregistered capital outbox topics (D-032, now guarded by a source-parsing test with a verified negative control), the Temporal SDK's ten missing transitive requirements (D-030), franz-go's separate `kmsg` module (D-031), a test deadline leaking onto success paths in three packages (D-028), and a ClickHouse healthcheck that had reported a healthy server unhealthy 3,471 times (D-027).

## 3c. Goal stopping criteria — re-audited against the goal document, STILL NOT MET

PART 249 lists 14 conditions for the work to be complete. Audited directly against the repository rather than inferred from progress:

| # | Condition | State |
|---|---|---|
| 1 | all implementable V1 systems exist | **NO** — no customer web app, no admin plane |
| 2 | all locally executable critical tests pass | YES — 81 packages `-race`, lint 0 issues, modules verified |
| 3 | provider integrations at strongest verifiable level | PARTIAL — SB-007 Jupiter layout still UNVERIFIED |
| 4 | external blockers machine-gated | LARGELY — workers refuse to start rather than half-wire |
| 5 | safety-critical invariants have automated tests | LARGELY |
| 6 | critical failure scenarios exercised | **NO** — no chaos suite, no cross-process E2E |
| 7 | the web application is complete | **NO** — `apps/` did not exist |
| 8 | infrastructure exists | PARTIAL — 9 binaries, not all have ECS services |
| 9 | CI/CD exists | PARTIAL — workflows authored, never run (SB-004, no remote) |
| 10 | observability exists | YES |
| 11 | operator tooling exists | PARTIAL — worker CLIs and runbooks yes, no admin console |
| 12 | documentation reflects reality | LARGELY |
| 13 | readiness report states what is authorized for live capital | **NO** — Stage 19 not started, and the goal says do not write it prematurely |
| 14 | PART 238 test matrix with actual results | PARTIAL — chaos, E2E and several rows unfilled |

**RE-AUDIT after waves 4 and 5, read from PART 249 directly rather than inferred from progress:**

| # | Condition | State now |
|---|---|---|
| 1 | all implementable V1 systems exist | **YES** — nine binaries, customer web app, admin console |
| 2 | all locally executable critical tests pass | **YES** — 82/82 packages `-race`, lint 0 issues, modules verified |
| 3 | provider integrations at strongest verifiable level | PARTIAL — SB-007, the Jupiter v6 layout, is still UNVERIFIED |
| 4 | external blockers machine-gated | **YES** — workers refuse to start rather than half-wire; fakes refused in STAGING/PROD |
| 5 | safety-critical invariants have automated tests | **YES** |
| 6 | critical failure scenarios exercised | **YES** — chaos, cross-process E2E, PART 48/49 recovery |
| 7 | the web application is complete | **YES** — 51 Playwright tests, integrator-verified |
| 8 | infrastructure exists | **YES** — all nine services, `validate` + `trivy` clean in three environments |
| 9 | CI/CD exists | PARTIAL — workflows authored and reviewed; **never executed** (SB-004, no remote) |
| 10 | observability exists | **YES** |
| 11 | operator tooling exists | **YES** — worker CLIs, admin console, runbooks, `scripts/devrun` |
| 12 | documentation reflects reality | **NO** — traceability still reports 146 IN_PROGRESS / 111 NOT_STARTED after entire stages landed |
| 13 | readiness report states what is authorized for live capital | **NO** — not written |
| 14 | PART 238 matrix with actual results | **NO** — not written |

Conditions 3 and 9 are externally blocked and correctly gated. **Three conditions fail on work that is ours to do**, and all three are about telling the truth rather than building more: a traceability document that is wrong in the optimistic direction is what a reviewer would use to decide what is safe to switch on.

Wave 6 launched against exactly those: a traceability refresh (condition 12) and the PART 238 matrix as `docs/build/ADVERSARIAL_VALIDATION.md` (condition 14). **Stage 19's readiness report is deliberately last**, because the goal says not to write it prematurely and a report claiming readiness before the matrix exists would be the fabrication the document forbids.

**Wave 4 launched against exactly these gaps (four agents):** `apps/web` (Stage 14, all nine PART 111 pages plus the PART 112 honesty constraints), `test/chaos` + `test/e2e` + security extensions (Stage 18 and the PART 238 matrix), `infra/terraform` service definitions for all nine binaries plus CI review (Stage 16), and `apps/admin` (Stage 15, RBAC and dual control).

Stage 19 is deliberately last. The goal says the readiness report must not be written prematurely, and a report claiming readiness before chaos and E2E results exist would be exactly the kind of fabrication the document forbids.

### Stage 16 infrastructure — VERIFIED, and it found four deploy-day defects

Independently re-verified by the integrator: `terraform fmt -check -recursive` exit 0; `validate` returns "Success! The configuration is valid." in dev, staging and prod; `trivy config --severity HIGH,CRITICAL --exit-code 1` exit 0.

Every one of these would have surfaced only on deploy day, and each was invisible to the Go test suite:

| Defect | Consequence had it shipped |
|---|---|
| `cmd/relay-worker` had **no ECS service, no ECR repository, no secrets row and no alarms** | the outbox would have had no publisher: the SSE stream, execution-worker wake-ups and every event-derived read model frozen, with nothing failing loudly |
| `command` was empty for all workers | the distroless entrypoint is the bare binary, so each worker printed usage and exited 2 — the deployment circuit breaker reads that as a crash loop. **Every worker service would have crash-looped** |
| ALB health check on `/readyz` | routes are mounted under `/v1`, so **no target would ever have entered service** |
| No `CP_API_*` variables anywhere in Terraform | `CP_API_SETTLEMENT_CHAIN`/`_MINT` are required in STAGING/PROD, so **`cmd/api` would have refused to start** |

Also added: per-service least privilege (relay-worker and migrate hold no bucket and no key; audit-worker alone holds `kms:Sign` and the WORM bucket), resource policies that deny every principal outside each secret's matrix so an over-broad identity policy still cannot read it, API autoscaling, and alarms chosen for what an operator would do about them — relay lag with `treat_missing_data = breaching` so a dead relay pages, blocked partitions as the outbox's dead-letter signal, and RDS connections measured against the `cp_app` role's limit rather than instance `max_connections`, because the role limit is the binding constraint.

**Honestly scoped:** `terraform plan` against AWS was **not** run — no credentials. The agent ran a local-backend plan that evaluated every variable validation, module and local before stopping at credential resolution, and negative-tested each new validation rule individually. Nothing AWS-side is verified. CI workflows still have never executed (SB-004, no remote).

**Makefile defects found and fixed by the integrator:** `make property` named `./test/property/...`, which does not exist — `go test` treats a missing package path as a hard error, so the target failed outright rather than running the property tests it names. Now `./internal/...` only, and it runs green across 62 packages. `make dev` invoked `scripts/devrun`, which did not exist; written, and it refuses any environment other than LOCAL/DEV.

### Wave 5 (2026-09-06, later session) — Stages 14 and 15 land; five defects found by running things

The previous Claude Code process exited and took its agents with it; their work was intact on disk, so each scope was handed to a fresh agent with a verified inventory rather than restarted.

**Stage 14 customer web app — VERIFIED by the integrator.** `npx playwright test` against the real API and real Postgres: **51 passed (46.0s)**, covering all nine PART 111 routes for accessibility and PART 112 honesty, plus "no dead controls anywhere", narrow-viewport reflow, and "no capability that is off is shown as a zero". Typecheck exit 0, production build clean, 31 unit tests green. The agent's first run was 18 failed / 33 passed; what it found were **real product defects**, not test noise: a WCAG AA contrast failure at 4.42:1 against the 4.5:1 threshold on the colour used for every label and timestamp, so every route failed; an invalid `<dl>` structure; and horizontal scrolling at 390px on six of nine routes, up to 445px of overflow, from a CSS grid blowout. It also found **four honesty tests asserting against the boot screen instead of the page** — they read `body.innerText` immediately after navigation and were covering nothing. Notably, the repo's own float-arithmetic guard rejected its first fix attempt and it rewrote the fix rather than weakening the guard.

**Stage 15 admin plane — VERIFIED by the integrator**, twice on one fresh database (`httpapi 2.356s / 2.163s`, `adminplane 7.889s / 7.465s`). The agent's central finding is the one worth remembering: **`internal/httpapi`'s existing admin integration tests used in-memory fakes for every admin port, and a fake happily answers "approved" to a proposer approving their own action.** The dual-control guarantee was untested. It wired the real `admin.Service` and `killswitch.Controller` over a real database into the real router, and proved the bypasses over HTTP: a proposer refused **while holding both the propose and the approve permission**, so identity is the only thing left refusing; a principal with propose but not approve refused at the approve path (a real gap — the route floor is the union of both sides, so an ADMIN genuinely reaches it and only the domain turns them away); elevation expiry at an hour, a second, and exactly at the deadline; and a release approval that cannot travel to another switch. Each has a positive control. It also found `format.ts` documenting a coverage guarantee whose test did not exist — "that guarantee was fiction".

**Both of the integrator's contract changes today were completely untested until that agent covered them** (D-038, D-039), including the percent-encoded brace form that was the one case my first implementation let through.

**Three fuzz- and sweep-found defects fixed by the integrator:**
- `internal/archive.ParseKey` accepted unpadded date partitions (`2026/9/5/13`) that re-render zero-padded, so a key did not contain its own partition. Padding is not cosmetic: archive keys are range-scanned by prefix and only sort in time order when padded, so one unpadded key falls outside every time-bounded listing — including a retention sweep and an audit reconstruction. Now rejected, along with dates like `2026/02/31` that `time.Date` would silently normalise into a different day.
- `FuzzVerifySignatureHeader` found that an uppercase hex signature verifies. **The verifier is right and the test was wrong**: it hex-decodes both sides and compares with `hmac.Equal`, so an uppercase rendering *is* the genuine signature. Tightening the verifier would have been the wrong fix — for a signature, byte equality is the property and canonical-string equality is a fragile approximation of it. This is the opposite call from identifiers (D-038, D-039), where canonicalisation is right precisely because controls key off the raw string.
- Two `internal/workflows` tests failed in a parallel sweep and passed in isolation and three times under deliberate load: Temporal's test environment defaults to a 3s wall-clock budget and they took ~1.95s. Raised to 2 minutes — a deadline that is not the property under test is made generous (D-028).

**Repo state:** `go build ./...` clean, `go mod verify` all modules verified, 81 of 82 packages pass `-race`. The one failure is `test/security`, where an agent is writing the eight negative controls its own guard demands. `make property` runs green across 62 packages.

**The admin agent made three scoped commits** (`f2f9292`, `1d6a730`, `bbc12f8`) covering only its own files. The repository still has 1,321 untracked files, so SB-008 — the entire build existing only in this working tree — remains open and is still the highest-severity operational risk.

### Stage 18 adversarial security — VERIFIED, and the negative-control discipline held

Re-verified by the integrator: `go test -count=1 -tags=integration ./test/security/` twice against one fresh database (`6.554s`, `6.161s`), `golangci-lint --build-tags=integration ./...` → **0 issues** repo-wide, **82 of 82 packages pass `-race`**.

**All 16 declared breaks were proven to make the suite fail**, each in its own test — the eight new controls plus a re-sweep of the eight that existed. That is the property that makes this suite worth anything: every guard has been shown capable of failing.

The SQL control deserves naming: it is a **constant-derivation analysis, not a grep**. It answers "could this statement string have come from anything but source text in this repo?" through concatenation, package constants, locals, `strings.Join`, `strings.Builder`, printf verbs (numeric verbs accept anything; `%s %q %v %T` require a constant argument), constant map and slice lookups, and function parameters resolved through every call site in the package. Result: **368 Postgres call sites across 128 packages, zero findings**, with `internal/testkit` and `internal/reality` excluded for stated reasons and a second test asserting neither is reachable from `internal/httpapi` or `cmd/api`.

**Two findings, handled differently and deliberately:**
- **Connection strings were not redacted from logs** (D-041). Fixed by the integrator in both halves — key denylist and a userinfo value rule — keeping host, port, database and username so a connection log line stays readable. The agent correctly did *not* assert this, since PART 190 does not list connection strings; widening a specification is an integrator call.
- **Request binding runs before authorization** (D-042), so an anonymous caller gets 400 rather than 401 for a malformed identifier. **Accepted, not fixed.** The binder touches nothing stateful and the only thing the status reveals — that a route exists and its parameter shapes — is already published in the OpenAPI document. Changing it would require a pre-routing authorization decision, i.e. a second source of truth about which routes are public, which is a far worse failure mode than a status code that reveals nothing.

**One environment hazard worth keeping:** the agent's scratchpad environment file was overwritten mid-session by another process, silently repointing its test DSN at `controlplane_test_adminplane`, so some exploratory runs wrote into a database it did not own. It caught this, switched to inline DSNs, and re-ran everything against its own database with the DSN echoed. **The integrator's verification was unaffected because every verification run in this session provisions its own fresh database** — which is exactly why that rule exists.

### Traceability refreshed against the repository — and it found real gaps (2026-09-06)

`docs/build/REQUIREMENTS_TRACEABILITY.md` now reflects reality: **NOT_STARTED 102→14, IN_PROGRESS 139→59, VERIFIED 81→226, BLOCKED_EXTERNAL 0→13.** 274 of 389 rows had a cell corrected and 203 changed state, **every one upward and none lowered**, which is what you would expect when the document had simply stopped being updated rather than been wrong. Method was mechanical, not narrative: an index of all 1,887 `func Test*`/`func Fuzz*` declarations was built and every row machine-checked, and **all 609 Go test references in the file resolve to a declaration that exists.** The header rule — never VERIFIED without named evidence — was already holding; the staleness was entirely `planned:` prefixes and states.

**Gaps it found that nothing else had surfaced.** These matter more than the tally:
- **No `ExecutionAdapter` implementation exists at all.** `bindProviders` errors for every provider mode and the Jupiter client is never wrapped, so the only implementation is a test fake. This is why quotes answer 503, and it is the reason the execution path cannot be exercised end to end against a venue.
- **SB-007 defeats its own guard.** `TestLayout_JupiterDiscriminators` pins exactly the layout SB-007 declares unverified, so the test cannot detect the error it exists to catch. A guard that encodes the assumption it is guarding is worse than no guard, because it reads as coverage.
- **Nothing persists a strategy.** `internal/strategy` has no database code, nothing writes `compile_attempts`, and the OpenAPI contract has no strategy route — a compiled strategy cannot be saved, versioned or deployed.
- **Two undocumented deviations from recorded decisions:** `apps/web` is Vite + React Router, not the Next.js App Router D-011 chose, and there is no zod validation at the API boundary; and although `make sqlc` exists, there is no `sqlc.yaml` and every repository is hand-written SQL. Neither deviation was in the decision register — the register is supposed to be where a departure from a decision gets argued, not where it goes unmentioned.
- `internal/notification` has no emitters, so no notification is ever produced. Stage 12 (`internal/backtest`, `internal/performance`) is genuinely unbuilt. `cmd/agent-worker` has no tests. Only 4 of PART 130's 11 security-event kinds are emitted. There is no completeness sweep proving every fill eventually maps.

**BLOCKED_EXTERNAL was granted narrowly**, only where BLOCKERS.md states the software is complete: 13 rows across EB-003, EB-005, EB-010 and EB-012. It was deliberately withheld from R-043-1, because EB-011 itself says the adapter wrapping is still pending — which is the distinction between "waiting on someone else" and "not finished".

### The repository is now on a remote, and CI has executed for the first time

1,340 files committed as `673d9bf` and pushed to `https://github.com/wdwd720/Nodal.git`. **SB-008 and SB-004 are closed.** Before pushing: `gitleaks detect` reports no leaks, and the 26 pre-existing findings were confirmed to be credential-shaped fixtures in tests that prove refusal, masking and redaction — AWS's documented example key, the canonical jwt.io token, truncated PEM stubs. They are allowlisted by path with the blind spot stated in `.gitleaks.toml`, and a **negative control confirmed realistic secrets are still caught in production paths**, so the allowlist has not blinded the scan.

`ci.yml` is running against a real commit for the first time. A remote existing is not the same as a green run — until one completes, `ci.yml` and `release.yml` remain unverified, and condition 9 stays PARTIAL.

### CI is green — first time in the project's history (run 34062522222, commit 5143d0e)

**All 18 jobs pass.** It took six runs, and each one surfaced defects that no amount of reading the workflow files would have found:

| Round | What it exposed |
|---|---|
| 1 | pnpm version declared in two places at once; `make staticcheck` had never passed anywhere (33 findings, all in generated code or in method names the generated interface dictates); the integration job pointed every package at one shared database |
| 2 | the TypeScript client was stale after the integrator's own spec change — **the drift check caught the integrator's miss**; `make e2e-web` named both the package and the script wrong so it had never run; `make infra-up` waited on a one-shot container that exits 0, and `--wait` treats any exit as failure; `make proto` could not find plugins that buf execs by name |
| 3 | chaos and e2e pointed at the shared database both suites refuse by design; web-e2e never started the API the browser talks to |
| 4 | `make sast` red at 53 gosec findings — see below; a settlement test double ordered by map iteration; an `Exited()` assertion true on Windows and false on Linux |
| 5–6 | a database name hardcoded in the workflow that disagreed with the tool that creates it |

**The two findings worth remembering.**

The gosec triage found that ~45 findings already carried written justifications — in `//nolint:gosec`, a *golangci-lint* directive that standalone gosec ignores entirely. That is why one linter was green and the other red on identical code. Verifying each stated invariant against source rather than converting them mechanically found **two justifications that were fiction about code that was safe anyway** (a claimed 0..38 bound where the database constraint is 0..18), and **one real defect**: a slippage fallback clamped only at the low end, where a fixture above 65535 basis points would wrap silently and a test written to assert "rejected" would encode a different, valid value and pass while proving nothing.

The settlement one is the sharpest. `TestExecutor_SubmitTimeoutLost_ProvenAbsent_NewAttemptOnce` — the PART 48 unknown-submission test — failed in a way that looked exactly like a real ordering bug in recovery. It was not: `MemAttempts.All()` iterates a map and sorted only on `CreatedAt`, and two attempts of one plan are routinely created in the same tick, so the tie was broken by Go's randomized map iteration. **A non-deterministic fake in the most safety-critical test in the package is its own hazard**: it spends a reviewer's attention on logic that was never wrong, and trains people to re-run until green.

### CORRECTION: "18 jobs green" did not mean what the integrator said it meant

The Stage 19 readiness work checked the CI jobs against what they actually execute, and found the green run was hollow in the place that matters most. Verified independently:

- **The `integration` job ran ZERO packages.** Its list is `./test/integration/...` minus the migration suite, and that directory contains *only* the migration suite. `go list` returns an empty set, the loop body never executed, and the job exited 0 in under a tenth of a second.
- **All 40 `//go:build integration` packages under `internal/` and `cmd/` ran in no CI job at all** — ledger, capital, settlement, execution, reconciliation, signing among them. The entire database-backed proof of the financial core existed only as manual local runs.
- **`make race` omits `-tags=integration`**, so no database-backed concurrency test had ever been run under the race detector. That is precisely where a race costs money: the capital reservation lock, the ledger's balanced-per-asset triggers, settlement's resumable executor, the outbox claim under `SKIP LOCKED`.
- **The chaos job finished in 0.427s against 28.5s locally**, because `CP_TEST_REDPANDA_BROKERS` and `CP_TEST_ARCHIVE_ENDPOINT` are never set and the broker-stall and archive-refusal tests `t.Skip()` silently. The broker-stall test is the one that found D-034, where a stalled broker made publishing look successful.
- **Branch protection is unavailable on this repository's plan**, so nothing enforces CI even when it is red.

**The integrator reported condition 9 met on the strength of a green badge and was wrong to.** A job that runs nothing passes, and passing is not the same as checking. Fixed: the integration job now enumerates packages from the build tag itself (so a new one is picked up without editing the workflow) and fails loudly if the enumeration returns fewer than two; a new step races the seven financial-core packages *with* the integration tag; and the chaos job sets the two variables its fault-injection tests need. Verified locally before pushing — capital 95.2s, ledger 3.2s, settlement 75.3s under `-race -tags=integration`, no data races.

### Stopping criteria — 12 of 13 met

| # | Condition | State |
|---|---|---|
| 1 | all implementable V1 systems exist | YES |
| 2 | all locally executable critical tests pass | YES |
| 3 | provider integrations at strongest verifiable level | **PARTIAL — SB-007**, the Jupiter v6 layout, still UNVERIFIED |
| 4 | external blockers machine-gated | YES |
| 5 | safety-critical invariants have automated tests | YES |
| 6 | critical failure scenarios exercised | YES |
| 7 | the web application is complete | YES |
| 8 | infrastructure exists | YES |
| 9 | CI/CD exists | **PARTIAL — the green run was hollow and the integrator said otherwise; corrected below** |
| 10 | observability exists | YES |
| 11 | operator tooling exists | YES |
| 12 | documentation reflects reality | YES |
| 13 | readiness report states what is authorized for live capital | **NO — the last one, and now the right time to write it** |

Condition 13 was deliberately held until now, because the goal says not to write it prematurely and because a readiness report assembled before the PART 238 matrix and a green CI run would have been exactly the fabrication the document forbids. Both now exist.

## 3d. FINAL stopping-criteria audit — commit `1c6fe23`, every condition re-verified

Not inherited from an earlier audit. Each row was re-checked against the repository at this commit, after the two control-gap fixes.

| # | Condition | Verdict | Evidence gathered at `1c6fe23` |
|---|---|---|---|
| 1 | all implementable V1 systems exist | **MET** | 9 binaries, 2 web apps, 54 internal packages, 41 migrations |
| 2 | all locally executable critical tests pass | **MET** | `go build ./...` clean; **82 packages `-race`, 0 failures**; golangci-lint 0 issues; staticcheck exit 0; `make sast` exit 0; gitleaks no leaks |
| 3 | provider integrations at strongest verifiable level | **MET** — see below | SB-007 verified against the published IDL for the right program id; the guard now derives from it and both negative controls fire. What remains needs the chain, which is not "available" without a node and a live program |
| 4 | external blockers machine-gated | **MET** | `config.RuleNoFakeProviders` refuses fake providers in STAGING/PROD; 5 binaries refuse to start rather than half-wire |
| 5 | safety-critical invariants have automated tests | **MET** | all eight named invariants resolve to a test that exists: PART 49 crash recovery, timeout-is-not-failure, kill-switch-never-stops-reconciliation, agent-can-never-resolve, no-balance-edit-endpoint, one-env-var-cannot-enable-live-money, forged-gate-activation-refused, severe-kill-switch-releasable |
| 6 | critical failure scenarios exercised | **MET** | 8 chaos, 7 cross-process E2E, 40 security tests with 16 proven negative controls |
| 7 | the web application is complete | **MET** | 11 pages, 3 Playwright spec files, 51 tests passing against the real API |
| 8 | infrastructure exists | **MET** | 63 Terraform files, 3 environments, all validate and scan clean; services for all 9 binaries |
| 9 | CI/CD exists | **MET** | `ci.yml` green on `af28dd8` running all 40 integration packages, the financial core raced with the integration tag, and chaos with 0 skips. `release.yml` still unproven (tag-triggered, no tags) |
| 10 | observability exists | **MET** | 11 files in `internal/observability`; metrics, tracing, structured logging with secret redaction incl. connection strings (D-041) |
| 11 | operator tooling exists | **MET** | 20 runbooks, worker CLIs, admin console, `scripts/devrun`, `scripts/restoredrill` |
| 12 | documentation reflects reality | **NOT MET as stated; corrected 2026-09-10** | The evidence in this cell was invented. It read "traceability re-derived from source: 264 VERIFIED / 69 IMPLEMENTED / 71 IN_PROGRESS / 34 BLOCKED_EXTERNAL / 28 NOT_STARTED, all 609 test references". All five figures were wrong and they summed to 466 against a 389-row document; `264 VERIFIED` appears nowhere else in this repository and matches no version of the file in its history, which has never been anything but 225 or 226. The phrase "re-derived from source" named the method that would have produced the right answer. Counted 2026-09-10: **389 rows** — 225 VERIFIED, 60 IN_PROGRESS, 59 IMPLEMENTED, 18 DEFERRED_OUT_OF_SCOPE, 14 NOT_STARTED, 13 BLOCKED_EXTERNAL — and **910** distinct Go test names cited, not 609. The property that every cited name resolves to a declaration that exists IS machine-checked, by `TestDocs_EveryTestTheyNameExists`; only the numbers were hand-written, and now `TestDocs_TraceabilitySummaryMatchesItsRows` derives them (F-111). |
| 13 | readiness report states what is authorized for live capital | **MET** | `docs/PRODUCTION_READINESS_REPORT.md`, 693 lines, opening line `Platform status: NOT_READY. Capital authority: DISABLED.` |

### Condition 3, resolved to the level that is actually available

The wording is "the **strongest verifiable level available**", and the earlier audit was right that SB-007 failed it: the Jupiter v6 IDL is public, so checking the layout against it needs no credential and had simply not been done.

**It has now been done.** The IDL was fetched from `jup-ag/jupiter-cpi` at commit `12bc5f67b94a2c3edc74d6e721a19442124a0bad` and committed at `internal/signing/inspect/testdata/jupiter_v6_idl.json`. What ties that document to the program rather than to a name is that the same repository's `src/lib.rs` declares `JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4`, exactly the program id the inspector accepts.

**Everything the inspector relies on matched**: `route` (9 accounts) and `sharedAccountsRoute` (13) in exact order and optionality, a single signer in each, the route arguments including `slippageBps: u16` and `platformFeeBps: u8`, `RoutePlanStep`'s fields, and **all 39 Swap variants by ordinal and payload size**. The layout reproduced from memory was correct — but nobody knew that, which was the whole problem.

**The circularity is gone, which matters more than the result.** `TestLayout_JupiterDiscriminators` pinned the same values the code assumed, and the provider fake mirrored them, so three artifacts agreed because they shared one unverified source. `jupiter_idl_test.go` derives its expectations from the committed IDL instead. **Both negative controls were run**: corrupting one payload size (Symmetry 16→8) and deleting one variant each make it fail with a message naming the variant.

**What remains, and why it does not fail this condition.** An IDL is a published artifact, not the chain, so nothing here proves the deployed program still matches it; that needs the on-chain IDL account or a decoded mainnet transaction, i.e. a node and a live program — not "available" in this build. Swap ordinals ≥39 postdate this IDL and stay from memory, with a test asserting they are absent so a newer IDL forces a real check. Both are recorded in BLOCKERS.md, and a canary trade still requires the on-chain confirmation.

**Verdict: 13 of 13 met at the level available in this build.** The system remains `NOT_READY` for live capital and the readiness report says so on its first line — that is not a contradiction: the conditions ask that the work be done and honestly reported, not that the platform be authorized. Two residual items are recorded rather than closed: on-chain confirmation of the Jupiter layout, and `release.yml`, which is tag-triggered and has never run.

## 4. Next exact work (ordered)

### RESUME HERE — checkpoint 2026-09-10, after fourteen findings from eleven parallel audits

Eleven read-only audits ran in parallel over the financial kernel, the state
machines, the provider path, configuration, agent authority, the launch tier,
observability, the frontends, authorization, the test suite and the
documentation. **Their claims were re-verified here before anything was
changed**, and that mattered: two were overstated and one was wrong. The
capacity negative-ceiling report concluded the money-at-risk ceiling could be
silently disabled — it could not, only its test was passing for the wrong
reason. That correction is recorded in F-104 rather than quietly dropped.

F-100 to F-113 are in §9 with what each one cost. Eight are P1. Every fix was
observed failing first.

**What to do next, in order.**

1. **F-105's remaining half.** `security_events` is bounded per minute now and
   still unprunable, so any steady rate eventually fills a 500 MB database. The
   complete fix is ADR-0020's: partition the table and detach, never delete
   under a disabled trigger. `login_attempts` is the same class, with a purge
   that lives in a worker this deployment does not run.
2. ~~**The agent resurrection.**~~ Closed as F-114 by migration 00734: a
   transition row may not claim to leave a terminal state.
3. ~~**Birth controls.**~~ Closed as F-121 and F-122 by migrations 00735-00737:
   `admin_actions`, `agents`, `credit_fundings` and `payout_requests` all now
   refuse a privileged birth state, joining `capability_gates`. What remains
   open is `wallets`, `assets` and `instruments`, which
   `TestIntegration_NothingIsBornFinished` names in an assertion that fails when
   one of them is closed, so the list cannot go stale.
4. **F-42 and F-47**, unchanged. F-109 applied the privilege treatment to the
   four tables whose columns are money; the rest of F-42's list stands.
5. **Nothing pages anyone about anything** (F-118, PART). The software half is
   done: alerts now log, and both composition roots construct the real
   instruments. What remains is a destination and something that runs on a
   timer — a cron on the Render blueprint, or a ticker in `cmd/api` the way
   `runCreditSettlement` already is. Also worth doing before that Terraform is
   ever applied: the five application alarms set `treat_missing_data =
   "notBreaching"`, so a metric that never arrives reads OK rather than
   INSUFFICIENT_DATA.

`docs/audit/LAUNCH_GATE_MATRIX.md` remains the honest summary of what stands
between this repository and real money. All five launch flags are still false.

### The previous checkpoint — 2026-09-09, after reconciling the provider work AND clearing §4

Three commits since `80edf58`'s successor. The audit ingested the provider
workstream, re-verified it rather than adopting it, and then cleared the §4
queue it had been paused on.

**Findings F-83 to F-99.** Fifteen fixed, one partial, one open by decision, and
F-93 as the inventory of what was verified and deliberately left. Six were P1:
a planted OIDC callback that signed the victim in as the attacker (F-87); every
unauthenticated rate-limit bucket collapsed into one (F-88); a money-at-risk
ceiling that could only rise and would have refused every purchase forever at
$2,000 of lifetime sales (F-90); a 50-account cohort ceiling enforced nowhere
(F-91); and the Stripe call inside the purchase transaction, which meant the row
was not persisted before the call and eight concurrent purchases could each pass
one ceiling (F-96).

**§4 is clear.** Item 1 (F-94, migration 00731): sixteen bindings, not ten,
and the recount changed the finding — F-78's exploit is not reachable
elsewhere, but nothing anywhere read a transition row's ORIGIN, and 00712's
"membership is exactly as strong as equality" was not. Item 2 (F-95): the
inventory was exactly current, nine constraints are now compared against the
list that declares them, three were deliberately left unpaired, and there is no
live drift. Item 3: ADR-0020, because retention on tables that refuse DELETE is
partition detachment and that is a decision before it is a commit.

**What to do next, in order.**

1. **F-85** — the request body is read into memory before the rate limiter
   runs, on a 512 MB single instance. The fix is middleware reordering on the
   money path, and the ordering is load-bearing in the other direction too:
   `captureBody` is what makes the raw body available to the webhook signature
   check. It wants its own change and its own test.
2. **F-84** — authorization after parameter binding. Authorising on the chi
   route pattern before the generated wrapper runs is the shape; it is a change
   to the boundary's structure.
3. **The rest of F-93's inventory**, each with its recorded reason: the
   schema-owner credential in `cmd/api`, the warn-and-disable on a wrong Stripe
   account, the replica-count assumption, the Neon pool timeouts.
4. **F-42 and F-47**, unchanged and still open.
5. **ADR-0020's implementation** when a retention period is actually decided.

`docs/audit/LAUNCH_GATE_MATRIX.md` is new and is the honest summary of what
stands between this repository and real money. `SOFTWARE_COMPLETE` is false for
the reasons in item 1 to 3 above; the other four launch flags are false for
reasons no engineering can move.

### The previous checkpoint — 2026-09-09, after reconciling the provider workstream

The `/goal` audit was paused at `80edf58` for a provider/deployment workstream.
That workstream landed 58 commits, deployed the service to Render's free tier
against Neon and Stripe's sandbox, and produced
`docs/operations/PROVIDER_ACTIVATION_CHECKPOINT.md`. The audit has now **ingested
and independently re-verified it** rather than adopting its conclusions.

**What the reconciliation found.** Eleven findings, F-83 to F-93, four of them
P1. Much of the checkpoint held and is credited in `AUDIT_FINDINGS.md`: the
signature verification, the database-enforced replay dedup, exactly-once across
all four crash boundaries, and fail-closed under database unavailability are all
as described. What did not hold:

- **F-87 (P1)** a planted OIDC callback signed the victim's browser in as the
  attacker — `state` was a server-side lookup key and was never bound to a
  browser. Fixed, with the exploit as a test in two suites.
- **F-88 (P1)** every unauthenticated rate-limit bucket was one shared counter,
  because the limiter keyed on the load balancer's address. Fixed, and a
  production-like deployment must now declare its proxy networks.
- **F-90 (P1)** nothing settled on a tier with no worker, so the money-at-risk
  ceiling was a lifetime cumulative cap that would have refused every Credit
  purchase forever at $2,000 of lifetime sales. Fixed.
- **F-91 (P1)** the 50-account launch-cohort ceiling was configured, logged as
  in force, and enforced nowhere. Fixed.
- **F-83, F-86, F-89, F-92** and the partials **F-84, F-85**. See the register.
- **F-93** is the inventory of what was verified and deliberately not fixed,
  each with its reason.

**The next thing on this surface**, and the reason two F-93 rows are joined: the
Stripe call sits inside the purchase transaction, and the capacity ceiling is not
authoritative under concurrency. Neither can be fixed alone — raising
isolation while a 20-second provider call is inside the transaction makes a
serialization retry re-call the provider, and an advisory lock instead holds one
of eight pool connections across that call. The provider call comes out of the
transaction first; then the ceiling can be made authoritative.

**Then §4 as it stood**, with its counts recomputed from source rather than
assumed:

**§4 items 1–3 are done in this batch.** 1 and 2 are fixed (F-94, F-95);
3 produced the ADR it needed (ADR-0020) rather than a worker, because the tables
involved refuse DELETE and the honest scheme is partition detachment. What
follows is the recount as it stood when the work started, kept because it is the
evidence for what was done.

1. **Transition bindings: 16, not ten.** 17 destination-only
   `cp_require_transition` triggers were created; 00726 replaced one (`agents`).
   The F-78 exploit is **not** reachable on the other 16:
   `agent_lifecycle_transitions` is the only transitions table in the schema with
   a `from_X = to_X OR ...` CHECK, and 11 of the 16 have no CHECK at all. What is
   present on all 16 is the weaker property that the recorded ORIGIN is whatever
   the writer said. **A separate and sharper item came out of the recount:**
   migration 00712 `CREATE OR REPLACE`d `cp_require_transition` so the flag
   ACCUMULATES (`current || '|' || new_state`, checked with `= ANY(string_to_
   array(flagged,'|'))`), so two transition rows for one entity in one
   transaction mean either destination licenses the change. That applies to all
   16 and wants a database test to confirm reachability.
2. **Unpaired enum CHECKs: 130** (not 131 — 00726 paired one). The
   inventory in `test/integration/enums` is exactly current as of 00730: 00728
   widened `credit_fundings_state_check`, which is already paired and already
   agrees with `credit.AllFundingStates()`; 00730's two CHECKs are arithmetic,
   not enums; 00729 added none.
3. **Retention classes: still three unenforced**, and F-93 adds a fourth
   problem — `CP_RETENTION_LOGIN_ATTEMPT_DAYS` is set to 90 on a table
   designed for 2, has no validation rule, and its purge cannot run on this tier
   at all.
4. **F-42 and F-47** remain open and unchanged.

### The previous checkpoint — 2026-09-08, at the end of the F-71..F-81 batch

The `/goal` run against `gola.md` (independent adversarial audit + final
architecture migration + production proof) was **paused deliberately** at this
point for a separate provider-integration workstream. It is not blocked and
nothing is half-applied: every change in the batch is landed, tested and
recorded.

**State at the pause.**

- Findings F-71 through F-81 are fixed and recorded in `AUDIT_FINDINGS.md`, each
  with the control observed failing before it was believed. F-31 is closed with
  the diagnosis it had been waiting for.
- Migrations 00725, 00726 and 00727 applied. Schema version **727**; the restore
  drill was re-run at 727 (118 tables, row counts identical, zero balance drift,
  journal hashes equal).
- `make lint`, `make unit`, `apps/admin` (26) and `apps/web` (35) npm suites, and
  the 50-package `make integration` sweep with the external stack live and
  `CP_TEST_REQUIRE_EXTERNAL_DEPS=1`.

**The next step, exactly.** Continue F-69's inventory, which is the running list
of what six independent audits found and what has not been fixed. Every item
still open there carries its own recorded reason; the three that are real work
rather than deferred-with-cause are:

1. **The other ten transition bindings.** F-78 fixed `agents` by binding the
   EDGE (`<from>><to>`) instead of the destination. `capability_gates`,
   `kill_switches`, `accounts`, `assets`, `instruments`, `deposits`,
   `withdrawals`, `trade_intents`, `orders` and `reconciliation_records` still
   compare the destination alone. Whether the same composition is reachable
   depends on each table's own CHECKs, so this is a migration per table with its
   own exploit test — deliberately not done on the strength of the analogy.
2. **The 131 unpaired enum CHECKs** named by
   `test/integration/enums.TestIntegration_NoEnumCheckAppearsUnnoticed`. Each is
   a list the schema holds with nothing verifying it against the code. The right
   number is zero.
3. **The three retention classes with no enforcement**:
   `CP_RETENTION_SOCIAL_DATA_DAYS`, `CP_RETENTION_MODEL_IO_DAYS`,
   `CP_RETENTION_OPERATIONAL_LOG_DAYS`. Harder than F-79's: the tables they
   would cover carry `forbid_mutation` triggers that refuse DELETE outright, so
   this is partition management or archival-then-drop and belongs in an ADR
   first.

Still open and unchanged: **F-42** (the AU001 binding trusts a transaction-local
setting any caller can set) and **F-47** (two deliberate statements about who may
read encrypted PII contradict each other). Both are recorded with their full
reasoning in `AUDIT_FINDINGS.md`.

### The original stage plan (historical; most of it has landed)

1. Land wave 2 (§3): re-verify every package on a fresh isolated DB, fold deviations into DECISION_REGISTER, wire `execution` ↔ `intent`/`quote` types and `chain` observers into the executor/recoverer.
2. `internal/reconciliation` (Stage 7): event-driven/periodic/full modes, record lifecycle, unknown-submission recovery (PART 48), PART 49 crash test, PART 163 operator flow, internal consistency verifiers wired to SEV1 metrics.
3. Composition roots: `cmd/api` (chi, `/v1` REST per PART 108, problem+json, Idempotency-Key, SSE), `cmd/execution-worker`, `cmd/reconciliation-worker`, `cmd/workflow-worker` (Temporal funding/reconciliation-escalation workflows), `cmd/audit-worker` (Merkle checkpoints, KMS signing, WORM archive, `verify`), `cmd/market-ingest-worker`, `cmd/agent-worker`; `openapi/openapi.yaml` + oapi-codegen strict server + generated TS client; provider wiring by `config.ProviderMode` with fake rejection in STAGING/PROD.
4. Stage 9–12: `internal/strategy` (IR, effect system, NL compiler via Anthropic SDK with schema-constrained output, TS SDK in `packages/strategy-sdk`), `internal/agent` (lifecycle, ToolBroker, budgets, pause), `internal/prediction` (+ calibration), `internal/reality` (raw archive, normalizer, ClickHouse, gap detection), `internal/backtest`, `internal/performance`.
5. Stage 13: `internal/proof` (Merkle batches, KMS signatures, archive manifests, `make verify-audit`).
6. Stage 14–15: `apps/web` (Next.js; every PART 111 page; Playwright), admin plane.
7. Stage 16–17: Terraform (`infra/terraform`), CI activation on a remote (EB-016), release signing.
8. Stage 18–19: adversarial validation matrix (chaos, load, security, restore) and `PRODUCTION_READINESS_REPORT.md`.
9. Housekeeping: D-014 (`go-oidc` verifier), security permissions for envelope/break-glass/withdrawal approvals (admin agent recommendation), outbox `next_attempt_at` column (event agent recommendation), `cmd/migrate create` help text.

## 5. Failing tests

Recorded honestly; nothing below is claimed as passing that was not observed passing.

| Package | Symptom | Owner | Status |
|---|---|---|---|
| `internal/reality` | `normalizer.go:327: undefined: parseEventID` — `go build ./...` fails on this package and no other | reality/archive agent | RESOLVED before the handover; `go build ./internal/reality/...` clean |
| `internal/reality` (integration tier only) | 3 integration tests rejected their own fixtures: `TestProp_ClickHouse_SnapshotNeverReturnsFutureKnowledge` and `TestIntegration_Pipeline_ArchiveNormalizePublishCheckpoint` (`reality: invalid normalized event`), `TestIntegration_RawArchive_IndexesDedupsAndVerifies` (`archive: invalid provenance`) | reality agent (replacement) | **RESOLVED 2026-09-06** — see the takeover note below; validation unchanged, fixtures and one adapter fixed |

**Reality/archive handover — a new failure mode.** The original reality agent was terminated by **usage-credit exhaustion pinned to its model** (`claude-fable-5-1`), not by an ordinary session limit. That distinction matters operationally: resuming such an agent by message just re-runs it on the model that has no credits and fails again. The recipe is instead to spawn a replacement with an explicit `model` and hand it a verified on-disk inventory. Its work was left in good shape — both packages build, and the whole unit tier passes (`ok internal/reality 1.250s`, `ok internal/reality/redpandabus 1.778s`, `ok internal/archive 1.155s`) — precisely because it had been landing compiling increments. Still to do there: the three integration failures, `cmd/market-ingest-worker` (0 files), 2 errcheck findings on unchecked `rows.Close`, 2 unformatted test files, and 5 British spellings.

**Reality/archive takeover — COMPLETE 2026-09-06.** The replacement agent finished the scope. Verified on the handover database (`controlplane_test_verify_reality`), `go test -count=1 -race -tags=integration ./internal/reality/... ./internal/archive/... ./cmd/market-ingest-worker/...` → `ok reality 3.993s`, `ok reality/redpandabus 1.524s`, `ok archive 1.238s`, `ok market-ingest-worker 1.129s`; unit tier green; `golangci-lint`, `gofumpt`, `goimports`, `staticcheck` and `lintfin` all clean over the three packages. The property test does real work: `[rapid] OK, passed 100 tests` against live ClickHouse.

*The three failures were two defects and one test-isolation bug, and no validation rule was relaxed.*

1. **`reality: invalid normalized event`** — the fixture was wrong, the check was right. `Timestamps.Validate` requires `decision_available_at ≥ feature_available_at`, which is exactly POINT_IN_TIME.md §1 (`decision_available_at = max(received, normalized, feature) + pipeline_latency`). The property test drew a decision latency in `[0, 2s]` from `platform_received_at` while the `newEvent` helper hardcoded `feature_available_at = received + 2ms`, so any drawn latency under 2 ms produced an event claiming a decision could use a datum before the feature it derives from existed. Fixed in the helper: the intermediate platform instants are now placed inside `[received, decision]`, so every generated event is contract-valid for any latency including zero. Property strength is unchanged — `decision_available_at` itself is untouched, so the expected set is identical.
2. **`archive: invalid provenance`** — the adapter was wrong, the check was right. `chain.RawObject.EventType` is a JSON-RPC method name (`getTransaction`), and `reality.ChainArchive` passed it straight into an object key, where the layout alphabet is `^[a-z0-9][a-z0-9_.-]{0,63}$`. Fixed at the boundary with the new `archive.NormalizeSegment`, which folds camelCase to snake_case and fails closed on anything it cannot express rather than truncating: `getTransaction` is archived as `get_transaction`. `reality.RawObject.Validate` now checks the same alphabet so the error names the field instead of surfacing an opaque provenance error from deep in the layout.
3. **`TestIntegration_Pipeline_ArchiveNormalizePublishCheckpoint`** was not a third defect — it only failed on the *second* run against a database. `raw_archive_objects` is `UNIQUE (provider, event_type, dedup_key)`, deliberately global so a redelivered provider payload is never archived twice whichever data source consumed it. The fixtures hardcoded `sig10`, `sig1/w1` etc., so a re-run collided with rows an earlier run left behind. The unique key was not widened; the fixtures now carry a per-run token, matching what `TestIntegration_Pipeline_RunOverSolanaFakeProvider` already did. Verified re-runnable: three consecutive runs against one database, nothing cleaned between them, all green, including against the already-polluted handover database.

Also landed: `cmd/market-ingest-worker` (`run`/`schema`/`sources`/`gaps`/`verify`, + unit tests), smoke-tested end to end against the live local stack — `schema` applied the Appendix A DDL to ClickHouse, `sources` registered the source with the restrictive PART 120 defaults (`historical_use_permitted=UNKNOWN`, `persistence_capability=BLOCKED`), `gaps` and `verify` returned clean reports, and both refusal paths fail closed rather than downgrading (no wallets → refuses; chain observer in fake mode → refuses, since `chaintest` is test-only and must never be linked into a binary). `reality.RawArchive.VerifySweep` was added to give POINT_IN_TIME.md §2's "Verify re-hashes objects on a schedule" an actual scheduler; it reports every corrupted object in its window rather than stopping at the first, and treats a missing object as a violation, never a shorter clean report.

The Object Lock (WORM) path is genuinely exercised, not merely coded: `TestIntegration_S3_AuditBucketObjectLockRefusesEarlyDeletion` runs against real MinIO and asserts `ObjectLockEnabled`, a COMPLIANCE `PutObject`, `HEAD` returning the retention, `DELETE` of the locked version refused as `archive.ErrRetentionLocked`/`CodeForbidden`, and the version still readable. Observed passing, not skipped.

**Redpanda bus — RESOLVED 2026-09-06, and it caught a real hang.** The blocker above (`franz-go/pkg/kmsg` absent from `go.mod`/`go.sum`) was cleared by the integrator under D-031, and `internal/reality/redpandabus` now has the franz-go producer and consumer its `doc.go` always described. Verified against the live local broker, whole package twice with nothing cleaned between: `ok internal/reality/redpandabus 15.812s / 15.823s`.

- **Partition rule read from the registry, not restated.** `ValidatePartitionKey` looks the topic up in `internal/event`'s registry and refuses a key that is not the `aggregate_id` the headers name, or an `aggregate_type` that is not the one `Spec` declares. Kafka orders within a partition and the key picks the partition, so a wrong key is the dangerous case: it publishes perfectly, and only shows up later as one aggregate's events arriving out of order. `Loopback` enforces the identical rule, so a producer that keys wrongly fails in the test that would otherwise have blessed it. An unregistered topic (reality's normalized-event stream, keyed by `dedup_id`) still needs a non-empty key, because a null key round-robins.
- **Producer**: idempotent, `acks=all`, synchronous; `Publish` returns only after every in-sync replica has the record, which is what the relay assumes before marking an outbox row published.
- **Consumer**: one client per (topic, group), auto-commit off, `BlockRebalanceOnPoll` so a rebalance can only happen between polls, one goroutine per partition with records handled in strict offset order, and a commit of only the contiguous handled prefix per partition — so an offset never moves past a record that was not handled.
- **`Open` is the only choice point** and never substitutes one bus for the other: fake mode gets `Loopback`, everything else gets the real client, and `New` will not return a client whose brokers did not answer a ping. `cmd/market-ingest-worker` refuses to start on an unreachable broker rather than downgrading (verified: `event bus: PROVIDER_UNAVAILABLE: redpandabus: brokers are unreachable`).

**The defect the tests found — worth knowing before anyone else writes a franz-go producer.** The broker-loss test pauses the Redpanda container and publishes into it. The first run did not fail: `ProduceSync` blocked for **634 seconds**, until the container was unpaused, and then returned `nil`. Neither `RecordDeliveryTimeout` nor `ProduceRequestTimeout` bounds that case — the first is only evaluated for a batch that is *not* in flight, and the second is a field the broker honors, which a paused broker does not. A stalled connection therefore leaves the batch in flight forever, and `ProduceSync`'s context only aborts buffering, not an in-flight batch. Fixed by bounding `Publish` itself: `Produce` with a promise and a `select` on the deadline. A timeout is reported as a **failure** even though the record may still land later; that asymmetry is deliberate, because an unknown outcome reported as failure costs a duplicate that at-least-once delivery and `event_id` dedup already absorb, while an unknown outcome reported as success costs the event.

Integration coverage against live Redpanda (7 tests) and unit coverage (6): round trip with headers and key preserved; per-partition ordering with 8 keys interleaved over 3 partitions, asserting each key lands on exactly one partition **and** that the keys demonstrably spread over more than one, so the ordering assertions are not vacuous; duplicates delivered serially and concurrently plus a handler that fails once, all yielding exactly one effect; a produce failure never reported as success (oversized record, cancelled context, empty key, publish after close); a consumer killed mid-record, where the next member of the group resumes at the uncommitted offset handling exactly the remainder, skipping nothing and repeating nothing; a second member joining mid-stream, fed continuously so the rebalance is actually exercised rather than passing because the incumbent had already drained the topic; and the broker-loss case above. `Loopback` and all of its tests are unchanged and still pass.

**Do not weaken validation to make those three tests pass.** They are the look-ahead leakage guarantee. A backtest built on a leaking reality store produces confident, profitable and entirely fictional results, which is the most expensive possible way for this system to be wrong.

**`internal/proof` — RESOLVED and VERIFIED 2026-09-06.** Independently re-verified by the integrator: three consecutive runs on one fresh database (`controlplane_test_verify_proof2`), nothing cleaned between them, all green (`proof 4.352s / 6.502s / 7.991s`, `audit-worker 1.139s / 1.210s / 1.173s`). Repeatability was **not** bought by weakening detection, which was the specific risk: all 11 tamper subtests still reject on their own distinct reason, and `c3: signature forged with another key` is still reported as `checkpoint_signature`, not downgraded to an unknown-key result. `TestIntegration_KeyRotation` adds five subtests, including "both keys trusted: a rotation is not an incident", "the retired key is dropped from the set: unknown, not tampered", and "a trusted key still catches a forged signature". The fix introduces a key set mapping key id to status — `active`, `retired` (rotated out, still verifies what it signed) and `revoked` (refused before any cryptography, overriding active/retired) — with the verifier resolving by `audit_checkpoints.signing_key_id`. Three failure reports are kept strictly separate: `checkpoint_key_unknown` means a trust gap where the signature was never judged, `checkpoint_key_revoked` is its own reason, and `checkpoint_signature` is the only one that means tampering. Recorded as D-029. The original defect, for the record:

**The defect as found — the single most important finding of this wave.** The agent's evidence was real but incomplete: it provisioned a fresh database, ran once, and dropped it. Running the same command twice against one database fails the second time:

```
signature does not verify over the canonical digest: proof: signature was made with a different key:
row names "local-test:5e26e6997f497ee9f8d345227d5e0fb5", verifier holds "local-test:eb337df7ca79bf80f5fd30c9210e6eaf"
```

Each test process mints a fresh ephemeral signing key, while `audit_checkpoints` rows persist because they are append-only audit records. Verification walks all history and reaches checkpoints signed by an earlier process. **This is not merely test hygiene.** The checkpoint row already names its own key id and the verifier ignores it, applying whatever single key it currently holds. That is the key-rotation failure mode: the moment a KMS key rotates, every checkpoint signed by the previous key becomes unverifiable and `make verify-audit` reports tampering where there is none. An audit system that cries tamper after a routine rotation is worse than none, because the one time it is right nobody will believe it. Required fix: resolve the verifying key by the id recorded on the row against a set of trusted keys; make unknown-key-id a distinct rejection reason from signature-mismatch; keep every existing tamper test rejecting on its specific reason; prove repeatability by running the suite twice on one database.

**`internal/strategy/ir` — VERIFIED (IR only).** `go test -count=2 -race ./internal/strategy/...` → `ok internal/strategy/ir 25.665s`, repeatable. The agent fixed all three outstanding lint findings with real fixes rather than suppressions, and its tests caught three genuine production bugs: `SemanticHashOfJSON` accepted JSON `null` and returned a real-looking digest that could have been stored as `strategy_versions.ir_hash`; `ParseDecimalString` accepted `.5` while rejecting `1.`; and the model-facing schema was invalid at the provider boundary, since the structured-output subset requires `additionalProperties: false` literally. Hash stability is proven across 8 child processes, not just goroutines, because Go randomizes map order per process. **Still open in that scope: the NL compiler, `internal/model`, and the TypeScript SDK.** The SDK belongs beside `packages/generated-client` as `packages/strategy-sdk`, not inside it, because it mirrors the IR rather than the HTTP API.

**91-package `-race` baseline, 2026-09-06 (excluding `internal/reality`, which does not build): 62 packages `ok`, 1 `FAIL`.** The sole real failure was `TestContract_Timeout` in `test/contract/solanarpc` — a 40ms client deadline that also governed a call the test expects to succeed. Diagnosed as a genuine test defect, not load noise, fixed under D-028, and re-verified `-count=5 -race` under deliberate background load: `ok test/contract/solanarpc 11.329s`. The same defect class was then swept for and found latent in `test/contract/stripe` and `internal/chain`; both hardened and re-verified `-count=3 -race` (`ok stripe 7.259s`, `ok chain 6.829s`, `ok chaintest 1.514s`). `internal/provider/solanarpc` and `internal/db` were inspected and are safe as written.

Repo-wide as of the 2026-09-06 post-resume check: `go build ./...` fails only on `internal/reality`; `go vet` over every other package is clean, which means every other package's test files compile. `internal/settlement` now builds (its `executor_steps.go` landed). The two Jupiter timeout tests that were load-sensitive are now **VERIFIED under contention**: three rounds of `-count=5 -race` run concurrently with a whole-`internal/` race load (load confirmed still running each round) gave 15 contended passes per timeout test and 0 flakes, across `TestClient_Order_Timeout`, `TestClient_Execute_TimeoutIsUnknownAndNeverRetried`, `TestContract_Order_Timeout` and `TestContract_Execute_Timeout_SubmissionUnknown_NoSecondCall`.

**`internal/settlement` + `internal/execution` — VERIFIED by the integrator on a freshly provisioned database** (`controlplane_test_verify_settle`, not the agent's own): `go test -count=1 -race -tags=integration ./internal/settlement/... ./internal/execution/...` → exit 0, `ok settlement 107.598s`, `ok execution 77.124s`. The agent's own runs (`settlement 244.579s`/`246.454s`, `execution 2.229s`/`111.604s`, golangci-lint 0 issues) are corroborated, not merely accepted. Includes `TestExecutor_ResumeAfterCrash_EveryStepBoundary`: 17 steps × 3 phases = 51 fault-injected runs, each asserting exactly one landed transaction, one submission, one fill, one ledger posting, one position update, reservation settled and order SETTLED — the PART 49 property, at every step boundary.

Open lint findings (1), inside an in-progress package and assigned to its owning agent: G115 integer-overflow conversion in `internal/strategy/ir/decimal.go`. The two `internal/archive` "artefacts" spellings are gone, and `golangci-lint run --build-tags=integration ./internal/reality/... ./internal/archive/... ./cmd/market-ingest-worker/...` reports `0 issues`.

## 6. Architectural changes made

Recorded in `DECISION_REGISTER.md`, D-001 through D-045. The most recent are
D-044 (a transition row licenses only the change it describes — the audit
binding compares the edge rather than the destination) and D-045 (retention
passes live in `audit-worker` under a `cp_ops` DSN rather than in a ninth
binary).

## 7. Migrations applied

00001 through **00739**, 71 files, all embedded in `migrations.FS` and
checksum-verified by `internal/db/migrate`. An applied migration is never
edited; a correction is a new file. `go run ./cmd/migrate status` is
authoritative, and `test/docs.TestDocs_CountsMatchTheCode` fails when a document
falls behind the schema.

The current head is 00727 (one signing decision per execution attempt). 00725
and 00726 landed in the same batch: a skip reason for a run stopped because its
agent is no longer runnable, and the edge-binding of `agents.state` and
`agents.stage`.

## 8. External blockers

See `BLOCKERS.md`. Summary: no provider credentials (Stripe onramp, Privy, Helius, Jupiter API key, AWS), no legal/licensing decisions. All live capability gates default DISABLED.

## 9. Unresolved defects

`docs/audit/FINAL_CHECKPOINT_2026-09-10.md` is the seventeen-item final output
for this session: HEAD, findings, schema, every test tier with its result, live
staging evidence, provider evidence classified, the financial invariants, the
restore drill, the $0 tier's ceilings, AWS readiness, the capability gate, the
external blockers, the five launch flags, the human actions and where to resume.
Read it before this file if you want the state; read this file for how it got
there.

`docs/audit/AUDIT_FINDINGS.md` is the register: **129 findings**, of which three
are open (F-47, F-69, F-93), one is open as a host limitation (F-125), four are
partial (F-65, F-84, F-95, F-118) and the rest are fixed.

**No P1 is unfixed.** F-93 is the last P1 not marked fixed and it is an
inventory row across six provider audits, whose constituent items are tracked
individually. F-105 closed with 00740 and F-42 with 00741 — the latter after
four sessions open and three fixes tried and rejected.

F-100 through F-129 landed on 2026-09-10: **30 findings, 17 P1, 9 P2, 4 P3**,
from eleven parallel read-only audits whose claims were re-verified here before
anything was changed, plus two the fuzz tier found on its own. Every P1 was
observed failing before it was believed:

- **F-100** a funding parked for a person was un-parked by the next webhook, and
  a refund followed by a late success minted Credits for money that was returned
- **F-101** one transition row licensed a second, unrelated edge, because the
  edge encoding's delimiters are in band and a state name is unconstrained text
- **F-102** two write routes scoped through the read-grade helper, so one ADMIN
  session could cancel any customer's intent and move any seller's product
- **F-103** five configuration rules permitted what the deployment cannot
  survive, including live provider credentials in DEV
- **F-105** an unauthenticated caller chose how many permanent, undeletable rows
  the service wrote, on a deployment whose database ceiling halts every
  financial action (PARTIAL: the table is still unprunable)
- **F-106** three money tables handed one account's record to another on a
  reused idempotency key
- **F-107** the seller set the platform's own commission, and a payout the
  provider may have paid could be cancelled by its owner
- **F-108** two failed RPCs were read as proof a transaction never happened
- **F-109** the transition binding covers the state column and nothing else, so
  the application role could rewrite an amount, a destination, or the definition
  of what counts as money
- **F-112** a supported configuration removed the `__Host-` prefix, re-opening
  the takeover F-87 closed
- **F-113** the amount a provider says it refunded was computed twice and read
  never
- **F-114** a revoked agent could return to live capital with no approval and no
  evidence, because it keeps its stage and both promotion CHECKs short-circuit
  when the stage does not move
- **F-115** the branch whose comment says "do not resubmit" was the one that
  resubmitted, and the provider a payout goes to was a caller argument compared
  to nothing

Three recurring shapes are worth carrying forward. **A fixture is a claim**:
six separate suites encoded the defect they were meant to catch — fourteen
providers on live credentials in a "valid production" config, a cookie domain in
the same fixture, a seller-set platform fee in the HTTP journey, outcomes
resolved before their horizons closed, an as-of price read answering with
unreceived rows, and an agent fixture inserting an admin action born APPROVED.
Each was found by closing the control, not by reading the fixture. **A guard
matched on a name**: F-102's write-scope check knew one helper of four, and the
completeness test that replaced it found a fifth on its first run. **A test can
pass because a different guard fired**: F-108's first test did exactly that and
was only caught by re-running it against the unfixed code, which is why that
step is not optional.

| Finding | Priority | Why it is still open |
|---|---|---|
| F-42 | P2 | The AU001 binding trusts a transaction-local setting any caller with the application credential can set. The remedy is privilege work on the state columns, and F-78 has now narrowed what that work has to cover for `agents`. |
| F-47 | P2 | Two deliberate statements about who may read encrypted PII contradict each other. It is a policy decision, not a code fix, and an agent has misread the migration comment as the code twice. |
| F-69 | P2 | The inventory itself. It shrinks as its items are fixed; §4 names the three that are real work. |

Everything else is FIXED or PART, with the evidence named in the finding.

## 10. Production-capability state

This table listed **ten** capabilities. `gates.AllCapabilities()` declares
**twenty**, and the ten it omitted are the entire internal economy — including
`CREDIT_PURCHASE`, the capability this deployment exists to activate. Section 3
of this same file says "capabilities: 20, of which 18 are high-risk", and that
sentence is machine-checked and passing, so the document contradicted its own
verified number 1,570 lines later. Two further rows were stale in their notes:
`LIVE_FUNDING` said "no gate DB yet" (the table has existed since migration
00150) and `MARKETPLACE` said "out of V1" (F-16 and F-43 moved it to high risk
because it gates internal commerce). Corrected 2026-09-10, and
`TestDocs_CountsMatchTheCode` now derives the row count from
`gates.AllCapabilities()` so it cannot silently fall behind again (F-111).

**This table lists 20 capabilities.**

| Capability | Risk | State | Notes |
|---|---|---|---|
| LIVE_FUNDING | high | DISABLED | provider approval outstanding (EB-003) |
| LIVE_MANUAL_TRADING | high | DISABLED | |
| LIVE_AGENT_TRADING | high | DISABLED | |
| WITHDRAWALS | high | DISABLED | the gate is the best-built control in the schema (00701) |
| SOCIAL_DATA_PERSISTENCE | low | DISABLED | data-licensing unknown |
| MARKETPLACE | high | DISABLED | gates internal commerce; reclassified by F-16/F-43 |
| CROSS_CHAIN | high | DISABLED | out of V1 |
| PREDICTION_MARKETS | high | DISABLED | out of V1 |
| SECURITIES | high | DISABLED | out of V1 |
| CEX_TRADING | high | DISABLED | out of V1 |
| CREDIT_PURCHASE | high | DISABLED | the one this deployment exists to activate; see `docs/audit/LAUNCH_GATE_MATRIX.md` |
| NATIVE_ASSET_CREATION | low | DISABLED | |
| NATIVE_MARKET_TRADING | high | DISABLED | |
| PAYOUT_RESERVE | high | DISABLED | |
| PAYOUT_SETTLE | high | DISABLED | |
| HOSTED_TRADING | high | DISABLED | |
| HOSTED_FUNDING | high | DISABLED | |
| AGENT_BOUNDED_DISCRETION | high | DISABLED | above `agentauthority.MaxSupportedLevel` |
| AGENT_AUTONOMOUS_SELECTION | high | DISABLED | above `agentauthority.MaxSupportedLevel` |
| AGENT_AUTONOMOUS_PORTFOLIO | high | DISABLED | above `agentauthority.MaxSupportedLevel` |

Every row reads DISABLED for the same reason rather than twenty reasons:
`cp_gate_born_disabled` (00701) refuses any gate born in another state, and no
activation ceremony has been performed in any environment. The ceremony needs
three distinct principals and evidence references, and what it is waiting on is
in `LAUNCH_GATE_MATRIX.md`.

Platform status: **NOT_READY**. Capital authority: **DISABLED**.

## 11. Provider integration status

| Provider | Role | State | API notes (`docs/api/providers/`, verified 2026-09-05) |
|---|---|---|---|
| Stripe crypto onramp | FundingProvider | **CODE_COMPLETE + CONTRACT_TESTED → BLOCKED_EXTERNAL (EB-003)** | API verified; product is public preview and application-gated (sandbox too); `stripe-go` has no onramp package → raw HTTP with decimal-string amounts; webhook `crypto.onramp_session.updated`; Stripe is merchant of record for fraud/disputes |
| Privy (delegated embedded wallets) | WalletProvider / SigningProvider | **CODE_COMPLETE + CONTRACT_TESTED → BLOCKED_EXTERNAL (EB-005)**; SignTransaction classified UNKNOWN_EFFECT_WRITE (idempotent replay is a documentation claim, not verified) | PARTIAL docs; policy engine cannot resolve address-lookup-table accounts → use `programId` allow-lists; sign via official Go SDK v0.15.0; rate limits unpublished |
| Jupiter | ExecutionAdapter (Solana) | **CODE_COMPLETE + CONTRACT_TESTED → BLOCKED_EXTERNAL (EB-011)**; adapter wrapping into `execution.ExecutionAdapter` pending | `/swap/v1` deprecated; current is Swap V2 (`/order` + `/execute`, `/build`) with `x-api-key`, mainnet only; amounts are strings |
| Helius | SolanaDataProvider / ChainObserver | **CODE_COMPLETE + CONTRACT_TESTED → BLOCKED_EXTERNAL (EB-010)** | PARTIAL docs; Enhanced Transactions in maintenance mode, Parsed Events beta returns numbers (must parse exactly); webhooks retry 3× then drop → never truth, periodic reconciliation mandatory |
| Fallback Solana RPC | ChainObserver (secondary) | **CODE_COMPLETE + CONTRACT_TESTED** (public RPC endpoints need no contract; production uses a paid fallback provider, config-driven) | verified; `maxSupportedTransactionVersion: 0` required for v0 txs; blockhash validity ~151 blocks; Token-2022 extension program ids enumerated |
| Anthropic Claude | ModelProvider | NOT_STARTED | verified; use `claude-opus-5` with `output_config.format` JSON schema for the NL compiler (Fable 5.1 rejects forced `tool_choice`); no idempotency header; Go SDK v1.71.0 |
| Redpanda | EventBus | **LOCAL_STACK_VERIFIED**; no managed account (EB-014) | `internal/reality/redpandabus`, started by `docker-compose.yml`, exercised in the integration job with `CP_TEST_REQUIRE_EXTERNAL_DEPS=1` |
| Temporal | WorkflowEngine | **LOCAL_STACK_VERIFIED**; no managed account (EB-014) | `internal/workflows` + `cmd/workflow-worker`; TLS is now applied rather than only validated (F-103) |
| S3 (+Object Lock) | ObjectArchive | **LOCAL_STACK_VERIFIED** against MinIO; Object Lock itself is BLOCKED_EXTERNAL (EB-012) | `internal/archive`; the deployment runs `CP_ARCHIVE_BACKEND=postgres` (00730), so the Object Lock rule does not fire there |
| ClickHouse | analytics store | **LOCAL_STACK_VERIFIED**; no managed account (EB-014) | `internal/reality/clickhouse` |

The last four read NOT_STARTED until 2026-09-10 and had not been for a long
time: all four are pinned in `go.mod`, started by `docker-compose.yml`, wired to
named packages and exercised in CI's integration job with
`CP_TEST_REQUIRE_EXTERNAL_DEPS=1`, which turns a skipped dependency into a
failure. LOCAL_EXTERNAL_STACK is the honest class for them — a real broker, a
real Temporal, a real object store and a real ClickHouse, none of them a managed
account this project holds (F-111).

## 12. Test matrix (locally executable today)

Five rows of this table asserted the opposite of what this same file establishes
elsewhere, and had done for long enough that a reader could have taken any of
them for current. They are corrected in place rather than deleted, with what was
wrong named, because a table that quietly becomes right teaches nobody how it
became wrong (F-111). Every "last recorded result" is a DATE and a claim about
that date; none of them is a promise about now.

| Tier | Command | Last recorded result |
|---|---|---|
| build | `go build ./...` | green 2026-09-06 (between waves; in-progress packages may transiently break it) |
| unit + race | `go test -count=1 -race ./...` | 2026-09-06 (later run, 61 packages): 58 green; `internal/settlement` failing while its agent finishes the executor; two Jupiter timeout tests load-sensitive (pass in isolation ×3; hardening requested) |
| property | `make property` (rapid `TestProp_*` in money, ledger, capital, positions, buyingpower, event, risk, eligibility, killswitch, provider, fees, ratelimit, audit, intent, quote) | green with unit |
| fuzz | `make fuzz` (`FuzzParseUSD`, `FuzzParseQuantity`, `FuzzQuantityFromDecimalString`, `FuzzParseUSDRound`, `FuzzScanQuantity`, `FuzzParse` (id), `FuzzParseSecretRef`, `FuzzEnvelopeJSON`, `FuzzCanonicalJSON`, `FuzzValidate` (intent), `FuzzRouteHash`) | 10–20 s per target clean (per-package reports) |
| integration | `make integration` (`scripts/inttest`, one fresh database per package) | **51/51 packages green, 2026-09-10.** This row said 32/32 until then, from a sweep predating `scripts/inttest`; section 6 of this same file already said "the 50-package sweep" (F-111) |
| migration | `test/integration/migrations` (clean apply, checksums, tamper, guarded rollback, role privileges, transition binding) | green |
| concurrency torture (PART 23) | `internal/capital` `CP_TORTURE_ITERATIONS=25` | 20/80 every iteration, both isolation modes |
| restore drill (PART 141/219) | `make restore-drill` | OK, `dist/restore-drill.json` |
| lint | `make lint` (+ `golangci-lint --build-tags=integration ./...`) | 0 issues at last sweep |
| contract | `make contract` (`test/contract/{stripe,jupiter,helius,solanarpc,privy}` and the topic registry) | green. "helius/solanarpc/privy pending" was wrong on 2026-09-10: all three exist and pass, and section 5 of this file lists them (F-111) |
| security | `make security` (`test/security`: authority-boundary import rules, closed agent permission set, PROD refuses fakes/seed/debug auth) | green 2026-09-06; API-level IDOR/CSRF/SSRF/webhook-forgery cases join once `cmd/api` exists |
| load | `make load` (`test/load/*.js`, k6) | scripts valid (`k6 inspect`); **unmeasured** — not for want of a binary. "no API binary yet" was wrong on 2026-09-10, and this file says so in three other places; what is missing is a run against a deployed target (F-111) |
| e2e / chaos tiers | `make e2e`, `make chaos` | both directories exist and both are CI jobs. "directories not yet created" was wrong on 2026-09-10 (F-111) |
| CI | `.github/workflows/ci.yml` | has run; SB-004 (no remote) is closed. "authored; never executed" was wrong on 2026-09-10, and section 3b of this file records the first green run (F-111). **Whether it is green TODAY is not asserted here**: nothing in this repository can check that, and a claim about a remote run's state is exactly the kind this file has been wrong about |

## 13. Session log

- **2026-09-05 S1**: Stage 0 audit; toolchain install; build-state docs; repo init.
- **2026-09-05/06 S1 (continued)**: Stage 1 foundation (money/id/clock/errs/config/observability/db/migrate/idempotency/security/auth/event), Stage 2 financial core (ledger/capital/positions/valuation/buying power), Stage 3 authority (gates/killswitch/eligibility/risk/admin/audit), migrations 00001–00605 + 00640/00641, privilege model D-016, transition binding 00603, OpenAPI + generated server/client, restore drill, identity login flow, notifications, compliance profiles, rate limiting, SSE stream, dev seed, ADRs, security/threat-model docs, provider API notes, traceability refresh. Two session rate-limit interruptions recovered by resuming agents. Wave 2 (Stages 4–8) in flight.
- **2026-09-06 S1 (wave 3)**: Fourth rate-limit cutoff. Resumed all six agents by message after inventorying disk (settlement, Jupiter and strategy had each landed more than their last message reported). Launched the Stage 7 reconciliation agent. Corrected §5, which still claimed "no tests exist yet" — it now records the single real build break (`internal/reality`), the outstanding Jupiter re-verification, and the three open lint findings. Standing lesson reconfirmed: inventory disk before telling a resumed agent what to do, because a cut-off agent's last narrated step understates what it actually wrote.
- **2026-09-08 S2 (documentation as a control surface)**: F-54 through F-57. The
  numbers and the operating documents were audited the way the code has been,
  on the principle that a readiness decision is made from them. **F-54**: five
  derivable counts had gone stale, including a restore drill last run at schema
  version 604, a hundred and eleven migrations ago;
  `TestDocs_CountsMatchTheCode` now derives each from the thing it describes,
  and the migration-version claim fails on staleness so a drill cannot fall
  behind the schema unnoticed — it failed twice while this session added
  migrations, and both times the fix was to re-run the drill, not to edit the
  number. **F-55**: 140 `PENDING` markers across 21 runbooks, every one naming a
  package, binary or route that exists; the index told an operator `cmd/api` had
  no serving binary, and `funding-provider-compromise.md` gave a webhook-forgery
  detection query that cannot return a row however bad the incident is. Markers
  are now `PENDING` (missing here) or `BLOCKED_EXTERNAL` (needs an account or a
  credential), and both claims are checked by
  `TestDocs_NothingMarkedPendingAlreadyExists` and
  `TestDocs_EveryRunbookRouteIsServed`. **F-56**: `provider_events` — the
  table 00107 calls "the evidence record" — had no trigger and a table-wide
  UPDATE grant, so `cp_app` could rewrite `payload_hash` and `signature_verified`
  on a verified event; migration 00719 narrows the grant by column and adds a
  guard trigger, and the test was observed passing all twelve mutations with the
  guard removed. **F-57**: five subsystems' integration tests skipped silently in
  CI while fifteen VERIFIED traceability rows cited them as evidence, and
  `make property` could not compile the database-backed properties it was cited
  as proving. `internal/testkit/deps` makes a missing dependency a failure in any
  job that promised the stack; the integration job now runs `make infra-up`;
  `scripts/inttest` gained `-run` and `make property` uses it. All ten
  database-backed property packages pass in 2m39s, and `internal/reality` runs 36
  tests with zero skips including both look-ahead-leakage tests.

  The recurring class this session: **a document is a control, and a control
  nobody executes decays to a claim.** Every fix above replaced a careful read
  with a check that runs.

- **2026-09-08 S2 (continued) — the invariants under the documents**: F-58
  through F-62. The first batch made the documents true; this one goes after
  what they describe. **F-58**: `GT003` — the guard 00716 calls "the line
  that holds when the Go check is bypassed" — had five raise sites and no
  assertion anywhere; it has seven now. The five value-domain SQLSTATEs appeared
  in no Go file at all, so a posting refused for moving Credits into real
  capital came back `INTERNAL`. Four documented codes were raised by nothing;
  00722 withdraws them and records what actually enforces each on the schema
  object itself, and `TestIntegration_EveryDocumentedSQLStateIsRaised` now fails
  on any code that is neither raised nor withdrawn — it caught a fifth case
  in migration 00720, written the same afternoon. **F-59**: the prediction
  resolver bounded its price reads from above and not from below, so a dead feed
  scored every open prediction FLAT, permanently. **F-60**: a calibration
  snapshot reported the count that survived its own inner join, so a promotion
  decision could cite a sample of six drawn from six hundred; 00721 adds the
  window's population and what was scored of it. **F-61**: `position_lots` could
  be refilled by a single UPDATE that satisfied every constraint, on the table
  holding cost basis — while `credit_lots`, doing the same job for Credits,
  has had a guard all along. **F-62**: the reality pipeline handed `Normalize` a
  fresh clock while the archive returned the original meta, so ClickHouse's
  version column stayed put and the column it replaces moved.

  Two things worth keeping from how this went. Migration 00720's header
  documented a `PL002` that was really a CHECK raising 23514 — F-58's own
  defect, committed in the next migration after writing it up, and caught by the
  check written for it. And the first GT003 test drove `cp_app`, which is
  refused by privilege before the guard is reached: it would have passed on
  SQLSTATE 42501 and proven nothing about the control it names.

- **2026-09-08 S2 (third batch) — an adversarial re-audit of what this
  session had not touched**: F-63 through F-69. Six read-only audits ran over
  auth/identity, killswitch/eligibility, capital/withdrawal, signing/execution,
  admin dual control and agent authority, each asked for the defect classes this
  codebase keeps producing rather than for a general review. Every claim was
  verified against the source before action, which is how one audit's framing
  was caught: it read a comment in 00717 *describing* a revoke that F-47 took
  back out as the revoke itself — the second time an agent has misread that
  same comment.

  **F-63 is my own.** Correcting F-55's 140 stale markers, three true ones were
  replaced with false claims that things were wired, all by the reasoning F-55
  exists to name: the directory is there, so the thing is done. The sharpest told
  an incident responder that revoking a compromised principal's sessions was
  served by `cmd/api`; `RevokeAllForSubject` has no caller outside its package.
  The same read found that **nothing revokes a session when a role is revoked** —
  roles are frozen into the session row at login — so clearing a suspect's
  ADMIN leaves it live for twelve hours.

  **F-64 (P1)**: `requires_dual`, `kind` and `target_id` decide what dual control
  means and none was protected, so an approval could be repointed at another
  target after both signatures; and `VerifyApproved` read `requires_dual` from
  the row while every other consumer read the code. **F-66 (P1)**: a future
  `auth_time` — copied verbatim from the ID token and validated nowhere —
  satisfied every step-up window for the session's whole life; the generated
  decision vectors moved 65 cases from ALLOWED to STEP_UP_REQUIRED, all of the
  one principal whose clock sits 48 hours ahead. **F-67 (P1)**: the signing
  recovery path signed the bytes it was handed rather than the bytes the decision
  approved — latent, because `bindProviders` refuses to start in every
  production build, and found before that path was turned on.

  **F-65** and **F-68** are the same shape as F-55 in different places: two kill
  switches an operator would reach for in an incident that reach nothing, and a
  production velocity policy that bounds nothing under a comment saying it bounds
  everything.

  **F-69 is the inventory**: fourteen more things the audits found, verified, and
  left, each with the reason. "Not fixed" without a reason is indistinguishable
  from "not noticed", and most of them are unreachable-subsystem work where a fix
  would produce exactly the control-only-a-test-can-reach this session has spent
  itself removing.

  Three of the batch's own fixes were caught being wrong by controls already in
  the tree: freezing `params` would have made the tamper check unreachable; a
  test forged an `admin_action_transitions` row to reach a branch and the
  package's audit-count invariant refused it within seconds; and the marker check
  refused two rewrites for naming a package that exists instead of the thing that
  does not.

- **2026-09-08 S2 (a control nobody reaches, in nine more places)**: F-71 through
  F-79, and F-31 closed with the diagnosis it had been waiting three sessions
  for. Migrations 00725 and 00726.

  **F-78 (P1)** is the one that matters. 00690's header says it stops "a bare
  `UPDATE agents SET state = 'LIVE'` by the application role" from moving an
  agent onto real customer capital with no evidence. It compares the transition
  row's destination and nothing else — not `from_state`, and not `stage` at
  all — while both promotion CHECKs on the transitions table open with
  `from_stage = to_stage OR ...`, because a pause must not have to carry
  promotion evidence. Those compose: insert a row claiming the stage did not
  move, then move it. Observed committing from the application pool, promoting a
  CANARY agent to LIVE with no approval_id and no evidence at all. 00726 binds
  the edge rather than the destination, so a row licenses a change only if it
  says where the change started, and `stage` gets a binding of its own.

  **F-72 (P1)**: `internal/agent` may not import `internal/gates`, so it reads
  `capability_gates` itself — selecting `effective_at` and `expires_at` and
  deciding on `state` alone. Nothing calls `Admin.ExpireDue`, so the persisted
  state never catches up, and an expired LIVE_AGENT_TRADING gate read as live to
  the agent worker for as long as the row sat there. The two readers are now
  compared against the same row at the same instant, across every condition.

  **F-74** is the class this session keeps finding, in the tests themselves:
  three tests named `...MirrorsTheDatabaseCheck` that opened no database. They
  asserted a hardcoded length and then that every member of a list is a member of
  that list — true however far the CHECK had drifted. The schema holds 163
  enum CHECK constraints and **none** was compared against a Go declaration by
  anything. `test/integration/enums` now compares 32 of them plus the
  stage-to-mode mapping in `agents_check3`, and names the 131 that remain so a
  new enum column cannot appear unnoticed.

  **F-71** and **F-76** are the affordance layer: the console offered the grantee
  of a break-glass elevation a live Approve button because the rule that refuses
  them was unreachable in every fixture, and six exported route guards look like
  the API's authorization while the API mounts none of them. **F-73**: revoking
  an agent stopped new runs and let every run already open finish, including its
  intent — the dispatcher asked `Runnable()` and the runner never did.
  **F-75**: the intent emitter treated its envelope reader as optional.
  **F-79**: `login_attempts` kept a plaintext OIDC nonce and PKCE verifier
  forever, under a migration saying the ops role purged them; `audit-worker`
  now runs the pass, on a `cp_ops` pool it refuses to start without.

  **F-31**, open and unreproduced since it was written, reproduced itself in this
  session's third sweep: `13 failed, first error: ... canceling statement due to
  lock timeout (SQLSTATE 55P03)`. The recorded hypothesis was right, the system
  was doing the correct thing, and the test's claim — that all hundred
  buyers succeed inside one five-second lock wait — was a claim about the
  machine. The buyer retries a CONFLICT now, bounded and counted.

  **F-77** is smaller and worth the sentence: the Redpanda suite created a topic
  per test and deleted none, so after 134 of them the broker refused to create
  any more and the next run failed with an error about the partition count —
  which is the one thing that was not wrong. CI never sees it; only the person
  running the suite repeatedly does.

  **F-80** and **F-81** close the last two actionable items of F-69's inventory.
  Two admin action kinds with live executors had never been run by anything, one
  of them the only Domain A kind that is dual-controlled in both directions;
  both tests failed the first time they ran, on properties worth having (a live
  asset is stepped down through CLOSE_ONLY or HALTED before it can be delisted,
  and `payout.Create` records a REJECTED request rather than erroring when the
  eligibility decision denies). **F-81**: an execution attempt could carry two
  signing decisions, because `Sign` is check-then-insert and `attempt_id` had a
  plain index — `findDecision` conceded it in its own `ORDER BY created_at
  DESC ... LIMIT 1`. Migration 00727 makes the index unique.

  The recurring class, stated once more because it keeps arriving in new
  disguises: **a rule that cannot be reached is not enforced, whatever the code
  says**. F-71's branch was unreachable because every fixture used a target id
  that was not a user id. F-74's comparisons were unreachable because no test
  opened a database. F-78's evidence requirement was reachable and simply
  side-stepped by a row that lied about where it started.
