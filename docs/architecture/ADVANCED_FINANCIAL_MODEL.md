# ADVANCED FINANCIAL MODEL

Status: contract fixed 2026-10-07 with migration `00731_advanced_financial_foundation.sql`. This is to the
advanced capabilities what `FINANCIAL_MODEL.md` is to the core: anything that contradicts it is a bug or
needs a `DECISION_REGISTER.md` entry. The decision to build these capabilities sandbox-first is
`docs/adr/0020-advanced-financial-capabilities-sandbox-first.md`.

## 1. The one rule everything else follows from

Every advanced capability — stable assets, margin and leverage, derivatives, equities, internal matching,
customer-to-customer transfers, principal trading, the spread and fee engine, performance fees, the
strategy marketplace, the code sandbox, delegated agent signing and withdrawals, cross-chain capital,
prediction markets — moves **only value in domain `SIMULATED`**, and that is enforced by the database, not
by convention:

| Guarantee | Where it holds |
|---|---|
| A `SIMULATED` asset is always domain `SIMULATED` | `assets_kind_domain_agree` |
| `SIMULATED` never moves to or from any other domain | `cp_check_domain_isolation` (00710, VD002) |
| A simulated-only journal kind touches only `SIMULATED` accounts | `journal_entries_simulated_only` (SM001) |
| A simulated-only journal balances **per party**, not only per asset | same trigger (SM002) |
| House and facility books hold only `SIMULATED` value | `ledger_accounts_segregated_books_are_simulated` |
| An economic intent cannot be `LIVE` | `economic_intents_mode_check` (only PAPER, SANDBOX, DEVNET) |
| `LIVE` is refused for every advanced capability | `gates.DecideMode` (`LIVE_NOT_IMPLEMENTED`), `AdvancedSpec.LiveImplemented = false` |

Live capital authority for these capabilities is therefore not *disabled*; it is *unrepresentable* in this
schema version. Enabling any of it is a code change, a migration and a gate ceremony — never a flag.

## 2. Package map and migration numbers

| Capability | Package | Migration | Subsystem constant |
|---|---|---|---|
| Shared kernel (intents, effects, facilities, authorizer, evidence) | `internal/econ` | 00731 | — |
| Risk Kernel V2 | `internal/risk` (`advanced.go`) | 00731 | — |
| Ledger V2 chart | `internal/ledger` (`advanced.go`) | 00731 | — |
| Canonical instrument model | `internal/instruments` | 00732 | — |
| Matching engine (pure) | `internal/matching` | — | — |
| Fixed-point options analytics (pure) | `internal/quant` | — | — |
| Spread / fee / performance-fee engines (pure) | `internal/fees` | — | — |
| Simulated chains (pure) | `internal/devnet` | — | — |
| Margin, leverage, lending, liquidation | `internal/margin` | 00740 | `MARGIN` |
| Futures, perpetuals, options | `internal/derivatives` | 00741 | `DERIVATIVES` |
| Orders, internal book, paper venue | `internal/orders` | 00742 | `ORDERS` |
| Equities | `internal/equities` | 00743 | `EQUITIES` |
| Customer-to-customer transfers | `internal/transfers` | 00744 | `TRANSFERS` |
| Treasury / principal book | `internal/treasury` | 00745 | `TREASURY` |
| Fee schedules and performance periods (persistence) | `internal/feeledger` | 00746 | `FEES` |
| Stable asset | `internal/stableasset` | 00747 | `STABLE_ASSET` |
| Bridges and cross-chain capital | `internal/bridge` | 00748 | `BRIDGE` |
| Prediction markets | `internal/predmarket` | 00749 | `PREDICTION_MARKETS` |
| Strategy marketplace | `internal/marketplace` | 00750 | `MARKETPLACE` |
| Sandboxed strategy runtime | `internal/sandboxrt` | 00751 | `SANDBOX_RUNTIME` |
| Delegated agent signing | `internal/delegation` | 00752 | `AGENT_AUTHORITY` |
| Delegated agent withdrawals | `internal/delegation` | 00753 | `WITHDRAWALS` |
| Sandbox funding (faucet) | `internal/econ` / `internal/sandboxfund` | — | `SANDBOX_FUNDING` |
| Buying power V2 | `internal/capital/buyingpower` | — | — |

`internal/prediction` (the agent forecast ledger) and `internal/commerce` (the creator marketplace) are
different things and are not touched.

## 3. Assets

A sandbox asset is `assets.KindSimulated`, value domain `SIMULATED`, on chain `nodal-sandbox` (mint
address = its own id) or on a simulated public chain `sbx-<name>` (mint address = the `internal/devnet`
mint). The same paper dollar on two simulated chains is two assets — that is what lets cross-chain
buying power tell them apart. Customer holdings of sandbox assets are `WALLET` balances, so the existing
`capital.Service.Reserve` reserves them unchanged.

## 4. Owners and the chart of accounts

Four owner types. `CUSTOMER` and `PLATFORM` as before; `HOUSE` (owner id = `house_accounts.id`) and
`FACILITY` (owner id = `facilities.id`, kind ∈ STABLE_ISSUER, LENDING_POOL, INSURANCE_FUND,
CLEARING_HOUSE, BRIDGE, PREDICTION_MARKET). A FACILITY code may only be held by a facility of its kind
(FA001). `econ.Registry.EnsureFacility` and `CreateHouseAccount` create the owners.

New codes (normal side; `±` = may go negative):

- Customer: `TRANSFER_OUT` D, `TRANSFER_IN` C, `FEES_SPREAD` D, `FEES_PERFORMANCE` D, `MARGIN_COLLATERAL` D,
  `MARGIN_LOAN` C, `INTEREST_EXPENSE` D, `DERIVATIVES_MARGIN` D, `REALIZED_PNL` C±, `OPTION_PREMIUM_PAID` D,
  `OPTION_PREMIUM_RECEIVED` C, `UNSETTLED_RECEIVABLE` D, `BRIDGE_IN_FLIGHT` D, `CORPORATE_ACTIONS` C±,
  `DIVIDENDS` C, `WITHDRAWN` D.
- Platform: `SPREAD_REVENUE` C, `PERFORMANCE_FEE_ACCRUED` D, `PERFORMANCE_FEE_DEFERRED` C,
  `PERFORMANCE_FEE_REVENUE` C (plus the existing `PLATFORM_FEE_RECEIVABLE` D / `PLATFORM_FEE_REVENUE` C).
- House: `HOUSE_INVENTORY` D, `HOUSE_CAPITAL` C, `HOUSE_TRADING_OUTFLOW` D, `HOUSE_TRADING_INFLOW` C, `HOUSE_FEES` D.
- Stable issuer: `STABLE_RESERVE` D, `STABLE_RESERVE_FUNDING` C, `STABLE_ISSUED` D, `STABLE_OUTSTANDING` C.
- Lending pool: `POOL_CASH` D, `POOL_CAPITAL` C, `LOAN_RECEIVABLE` D, `INTEREST_INCOME` C.
- Insurance fund: `INSURANCE_FUND_CASH` D, `INSURANCE_FUND_CAPITAL` C, `DEFICIT_RECEIVABLE` D.
- Clearing house: `CLEARING_CASH` D, `CLEARING_CAPITAL` C, `CLEARING_RESULT` C±.
- Bridge: `BRIDGE_LOCKED` D, `BRIDGE_LOCK_FUNDING` C, `BRIDGE_MINTED` D, `BRIDGE_OUTSTANDING` C.
- Prediction market: `PREDICTION_ESCROW` D, `PREDICTION_COLLATERAL` C, `PREDICTION_SHARES_ISSUED` D,
  `PREDICTION_SHARES_OUTSTANDING` C.

## 5. Posting rules

Every posting of a simulated-only kind **balances per party**: for each `(owner_type, owner_id, asset)`
Σdebits = Σcredits. Value moves between parties only as explicit pairs. The canonical templates
(`q` = quantity; customer `C`, other customer `C'`, platform `P`, facility `F`, house `H`):

| Kind | Entries |
|---|---|
| `SANDBOX_FUNDING` | C: Dr WALLET / Cr CAPITAL |
| `FACILITY_FUNDING` | F: Dr <asset code> / Cr <capital code> (e.g. POOL_CASH/POOL_CAPITAL) |
| `HOUSE_FUNDING` | H: Dr HOUSE_INVENTORY / Cr HOUSE_CAPITAL |
| `P2P_TRANSFER` | C: Dr TRANSFER_OUT / Cr WALLET; C': Dr WALLET / Cr TRANSFER_IN |
| `SIM_TRADE` (spot, paper, matched, prediction shares, equities on settlement) | buyer: Cr WALLET(quote) / Dr TRADING_OUTFLOW(quote), Dr WALLET(base) / Cr TRADING_INFLOW(base); seller mirrored; a house side uses the HOUSE_* codes |
| disclosed spread inside a trade | C: Dr FEES_SPREAD / Cr WALLET; P: Dr PLATFORM_FEE_RECEIVABLE / Cr SPREAD_REVENUE |
| explicit fee inside a trade | C: Dr FEES_PLATFORM / Cr WALLET; P: Dr PLATFORM_FEE_RECEIVABLE / Cr PLATFORM_FEE_REVENUE |
| `MARGIN_COLLATERAL_MOVE` | C: Dr MARGIN_COLLATERAL / Cr WALLET (post); reverse to release |
| `MARGIN_BORROW` | C: Dr WALLET / Cr MARGIN_LOAN; F(pool): Dr LOAN_RECEIVABLE / Cr POOL_CASH |
| `MARGIN_INTEREST` | C: Dr INTEREST_EXPENSE / Cr MARGIN_LOAN; F(pool): Dr LOAN_RECEIVABLE / Cr INTEREST_INCOME |
| `MARGIN_REPAY` | C: Dr MARGIN_LOAN / Cr WALLET; F(pool): Dr POOL_CASH / Cr LOAN_RECEIVABLE |
| `LIQUIDATION` | the forced reduce-only trade, posted like SIM_TRADE, kind LIQUIDATION |
| `DEFICIT_COVERAGE` | C: Dr MARGIN_LOAN / Cr DEFICIT; F(pool): Dr POOL_CASH / Cr LOAN_RECEIVABLE; F(insurance): Dr DEFICIT_RECEIVABLE / Cr INSURANCE_FUND_CASH |
| `DERIVATIVE_MARGIN_MOVE` | C: Dr DERIVATIVES_MARGIN / Cr WALLET (post); reverse to release |
| `VARIATION_MARGIN`, `FUNDING_PAYMENT`, `FUTURE_SETTLEMENT` | payer C: Dr REALIZED_PNL / Cr WALLET (or Cr DERIVATIVES_MARGIN); F(clearing): Dr CLEARING_CASH / Cr CLEARING_RESULT. Receiver mirrored. Internally matched books net CLEARING_RESULT to zero |
| `OPTION_PREMIUM` | buyer: Dr OPTION_PREMIUM_PAID / Cr WALLET; writer: Dr WALLET / Cr OPTION_PREMIUM_RECEIVED (or the clearing house as counterparty) |
| `OPTION_SETTLEMENT` | long: Dr WALLET intrinsic / Cr REALIZED_PNL, Dr REALIZED_PNL / Cr OPTION_PREMIUM_PAID; short mirrored; via clearing house |
| `EQUITY_SETTLEMENT` | C: Dr WALLET / Cr UNSETTLED_RECEIVABLE (the trade date posted Dr UNSETTLED_RECEIVABLE / Cr TRADING_INFLOW for the asset received) |
| `CORPORATE_ACTION` | split: C: Dr WALLET(shares) / Cr CORPORATE_ACTIONS(shares); reverse split and cash-in-lieu the opposite plus Dr WALLET(cash) / Cr CORPORATE_ACTIONS(cash); dividend: Dr WALLET(cash) / Cr DIVIDENDS |
| `STABLE_ISSUANCE` | C: Cr WALLET(reserve) / Dr TRADING_OUTFLOW(reserve), Dr WALLET(stable) / Cr TRADING_INFLOW(stable); F(issuer): Dr STABLE_RESERVE / Cr STABLE_RESERVE_FUNDING (reserve asset), Dr STABLE_ISSUED / Cr STABLE_OUTSTANDING (stable asset) |
| `STABLE_REDEMPTION` | the exact inverse |
| `BRIDGE_SOURCE_FINALIZED` | C: Dr BRIDGE_IN_FLIGHT(src) / Cr WALLET(src); F(bridge): Dr BRIDGE_LOCKED(src) / Cr BRIDGE_LOCK_FUNDING(src) |
| `BRIDGE_DESTINATION_FINALIZED` | C: Dr TRADING_OUTFLOW(src) / Cr BRIDGE_IN_FLIGHT(src), Dr WALLET(dst) / Cr TRADING_INFLOW(dst); F(bridge): Dr BRIDGE_MINTED(dst) / Cr BRIDGE_OUTSTANDING(dst) |
| `BRIDGE_REFUND` | C: Dr WALLET(src) / Cr BRIDGE_IN_FLIGHT(src); F(bridge): Dr BRIDGE_LOCK_FUNDING(src) / Cr BRIDGE_LOCKED(src) |
| `PREDICTION_SET_ISSUANCE` | C: Cr WALLET(cash) / Dr TRADING_OUTFLOW(cash), Dr WALLET(each outcome) / Cr TRADING_INFLOW(each outcome); F(market): Dr PREDICTION_ESCROW / Cr PREDICTION_COLLATERAL (cash), Dr PREDICTION_SHARES_ISSUED / Cr PREDICTION_SHARES_OUTSTANDING (each outcome) |
| `PREDICTION_SET_REDEMPTION`, `PREDICTION_PAYOUT`, `PREDICTION_REFUND` | the inverse flows; a payout retires every share and pays the winning outcome 1 unit of collateral per share |
| `PERFORMANCE_FEE_ACCRUAL` | P: Dr PERFORMANCE_FEE_ACCRUED / Cr PERFORMANCE_FEE_DEFERRED (reversal: the opposite) |
| `PERFORMANCE_FEE_CRYSTALLIZATION` | C: Dr FEES_PERFORMANCE / Cr WALLET; P: Dr PERFORMANCE_FEE_DEFERRED / Cr PERFORMANCE_FEE_ACCRUED, Dr PLATFORM_FEE_RECEIVABLE / Cr PERFORMANCE_FEE_REVENUE |
| `SANDBOX_WITHDRAWAL` | C: Dr WITHDRAWN / Cr WALLET |

A reversal is `COMPENSATION` with `reversal_of` set and a reason code; history is never edited.

## 6. The economic operation, step by step

Every mutating operation of every subsystem has this shape, inside one `db.InTx` unless a step is
external (a simulated-chain submission), in which case the external step is between two transactions and
its outcome can be `UNKNOWN`:

1. **Authenticate and scope** (HTTP layer: principal, `security.RequireAccountOwner`).
2. **Intent** — `econ.IntentStore.Submit` (idempotent on account + key, content-hashed). Agents may only
   create intents; they never apply effects.
3. **Route** — `econ.Route` → `settlement.Compile`. Agent authority ladder, legal router, simulated domain.
   `SIMULATED_TRANSFER_VALUE` and `SIMULATED_WITHDRAW` are forbidden to agents at every level.
4. **Authorize the mode** — `econ.Authorizer.Require(cap, mode)`: `gates.DecideMode` over the gate checker
   and kill switches. LIVE is refused. In STAGING/PROD the gate must be ACTIVE. `CAPABILITY_DISABLE` (scope
   = capability) and the global kill stop new activity.
5. **Risk** — build a `risk.AdvancedInput` with every section the intent kind requires
   (`risk.RequiredSections`), `risk.EvaluateAdvanced(policy, input)`, persist with
   `risk.Store.RecordAdvancedDecision`, refuse with `decision.RejectionError()` (`RISK_REJECTED` + stable
   reason codes). A missing required section is a rejection, never a pass.
6. **Reserve** — `capital.Service.Reserve` on the customer's `WALLET` of the sandbox asset (reservations,
   totals, and holds work unchanged on simulated assets).
7. **Claim the effect** — `econ.EffectStore.Begin(key, subsystem, kind, request)`. Deterministic key per
   logical operation. A replay returns the first outcome: APPLIED means done, UNKNOWN means *do not retry*.
8. **Execute** — the subsystem's own engine (matching, margin math, devnet submission...).
9. **Post** — `ledger.Service.Post` with a template from §5.
10. **Apply** — `econ.EffectStore.Apply(journal id, reservation id, external ref, evidence)`.
11. **Evidence** — `econ.RecordEvidence` (canonical JSON into the write-once archive, same transaction).
12. **Outbox and audit** — written by `econ` for every intent and effect transition
    (`econ.intent.transitioned`, `econ.effect.transitioned`, audit actions `econ.intent.*`, `econ.effect.*`).
13. **Reconcile** — UNKNOWN effects are on `EffectStore.ListUnresolved`; facility books have their own
    reconciliation kinds (`STABLE_RESERVE`, `BRIDGE`, `PREDICTION_ESCROW`, `CLEARING`, `DEVNET_CHAIN`,
    `ECONOMIC_EFFECT`).

## 7. Exactly once

`economic_effects.effect_key` is UNIQUE; the request hash is stored and a different request under the same
key is `INVALID_IDEMPOTENCY_REUSE`; a ledger-posting effect cannot be APPLIED without its journal
transaction (`economic_effects_applied_has_journal`); a journal transaction can back only one effect
(`economic_effects_journal_once`); the status machine (PENDING→APPLIED|UNKNOWN|FAILED, UNKNOWN→APPLIED|FAILED,
APPLIED→COMPENSATED) is enforced in SQL (EF001) and every change needs a transition row naming both
endpoints (AU001); identity is frozen (EF002). The ledger's own idempotency key makes the posting itself
idempotent as well.

## 8. Risk Kernel V2

21 composable checks (identity, eligibility, instrument, jurisdiction, account, balance, margin,
concentration, liquidity, volatility, oracle freshness, venue health, chain health, settlement, velocity,
destination, strategy, authority, code sandbox, capital reservation, circuit breaker). Each reads only its
section and returns stable `RISK_*` codes. Stale or unavailable marks block risk-increasing intents and
never block risk reduction. A withdrawal or transfer that would leave equity below maintenance is
`RISK_WITHDRAW_BREACHES_MARGIN`. Decisions are hashed and byte-for-byte deterministic
(`TestProp_AdvancedDeterminism`).

## 9. Capability gates and modes

15 new gates (all high risk) plus the existing `PREDICTION_MARKETS`. `gates.AdvancedSpecs` states per
capability the simulated modes it implements, where it runs ungated (LOCAL, TEST, DEV), and everything a
live activation would require (evidence kinds, approvers, validity, capital ceiling = 0, jurisdictions,
provider readiness). `LiveImplemented` is false for every one.

## 10. Invariants every subsystem must prove with tests

- Per-party balance and simulated-only rule hold for every posting it makes (the database enforces them;
  a subsystem test must exercise a real posting through the database).
- Its conservation identity, stated in its own terms (supply ≤ reserve; Σ customer liabilities constant
  under transfers; escrow = shares outstanding; locked = outstanding + in flight; Σ funding paid = Σ
  received in an internal book; no overfill in matching).
- Exactly once: a replay of every mutating operation produces no second effect.
- No double spend: concurrent operations on the same capital cannot both succeed beyond availability.
- Fail closed: missing or stale inputs refuse rather than proceed.
