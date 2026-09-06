# FINANCIAL MODEL

Status: design fixed 2026-09-05; implementation state tracked in `docs/build/REQUIREMENTS_TRACEABILITY.md`. This document is the contract for Stage 2 (financial core). Anything that contradicts it is a bug or requires a DECISION_REGISTER entry.

## 1. Three truths (PART 11)

| Truth | Where it lives | Examples |
|---|---|---|
| External truth | Solana chain, wallet provider, funding provider, venue | on-chain token balances, tx signatures, provider session status |
| Internal accounting truth | PostgreSQL: journal, balances, reservations, envelopes, deposits, holds | customer entitlement to 100 USDC, 500 USDC reserved for intent X |
| Economic valuation | computed on demand by `valuation` + `capital.BuyingPower`; never persisted as truth | portfolio value $1,234.56, buying power $900.00 |

Redis, ClickHouse, Temporal state, and frontend state are never accounting truth.

## 2. Asset-quantity ledger (PARTS 19–21)

The ledger is append-only double-entry **per asset**. A `JournalTransaction` may touch several assets; the balance equation holds independently for every asset inside it:

```
∀ posted transaction T, ∀ asset A in T:  Σ debit(T,A) == Σ credit(T,A)
```

USD is not an asset in this ledger. USD figures attached to entries (`usd_value_minor`) are **valuation metadata** for reporting/P&L and are never balanced.

### 2.1 Chart of ledger accounts

A `LedgerAccount` is identified by `(owner_type, owner_id, code, asset_id)` and holds exactly one asset.

| Code | Owner | Normal side | Meaning | May go negative |
|---|---|---|---|---|
| `WALLET` | CUSTOMER | DEBIT | customer's entitlement to asset units held in their embedded wallet under platform control | no |
| `CAPITAL` | CUSTOMER | CREDIT | capital contributed by the customer (funding) net of withdrawals | no |
| `TRADING_OUTFLOW` | CUSTOMER | DEBIT | asset units disposed through trades | no |
| `TRADING_INFLOW` | CUSTOMER | CREDIT | asset units acquired through trades | no |
| `FEES_NETWORK` | CUSTOMER | DEBIT | network fees paid (e.g. SOL) | no |
| `FEES_VENUE` | CUSTOMER | DEBIT | explicit venue fees | no |
| `FEES_PLATFORM` | CUSTOMER | DEBIT | platform fees paid by customer | no |
| `DEFICIT` | CUSTOMER | CREDIT | amount the customer owes the platform after a reversal exceeded their `WALLET` balance (PART 27). Credit-normal: it offsets assets the customer still holds elsewhere (e.g. `TRADING_OUTFLOW`), so `Σ debit-normal == Σ credit-normal` keeps holding (D-017) | no |
| `RECONCILIATION_ADJUSTMENT` | CUSTOMER | CREDIT | reason-coded corrections when external truth differs (dust, airdrop, rounding) | yes (bidirectional) |
| `PLATFORM_FEE_RECEIVABLE` | PLATFORM | DEBIT | fees owed to / collected by platform | no |
| `PLATFORM_FEE_REVENUE` | PLATFORM | CREDIT | platform fee revenue | no |
| `PLATFORM_ADJUSTMENT` | PLATFORM | CREDIT | platform-side offset for corrections | yes |

Balances are stored in **normal-side terms**: a debit-normal account's balance increases with debits.

### 2.2 Canonical postings

Funding settled (100 USDC observed on chain, reconciled):
```
Dr WALLET:USDC            100
Cr CAPITAL:USDC           100
```

Funding reversed (provider chargeback) when the customer still holds ≥ 100 USDC:
```
Dr CAPITAL:USDC           100
Cr WALLET:USDC            100
```
Funding reversed when the customer holds only 30 USDC because 70 USDC was already swapped into SOL (`WALLET` 30, `TRADING_OUTFLOW` 70, `CAPITAL` 100). A single posting `Dr CAPITAL 100 / Cr WALLET 100` would drive `WALLET` to −70 and is rejected by the negative-balance trigger. The funding service therefore posts two balanced transactions inside one database transaction:
```
T1 (kind = FUNDING_REVERSAL)          Dr CAPITAL:USDC   30      Cr WALLET:USDC    30
T2 (kind = FUNDING_REVERSAL_DEFICIT)  Dr CAPITAL:USDC   70      Cr DEFICIT:USDC   70
```
Net effect: `WALLET` −30 (to 0), `CAPITAL` −100 (to 0), `DEFICIT` +70 (credit-normal: the customer owes 70). Debit-normal balances (`WALLET` 0 + `TRADING_OUTFLOW` 70) still equal credit-normal balances (`CAPITAL` 0 + `DEFICIT` 70). No account is negative; the deficit is explicit. The account is set to `FROZEN`, a `withdrawal_hold` is not needed (balance is zero), a SEV alert `negative_deficit_accounts` is emitted, and the full trail (deposit transition, both journal transactions, audit event) is retained.

Swap fill: 100 USDC → 0.5 SOL, network fee 0.000005 SOL, platform fee 0.10 USDC:
```
Cr WALLET:USDC            100.10
Dr TRADING_OUTFLOW:USDC   100.00
Dr FEES_PLATFORM:USDC       0.10
Dr PLATFORM_FEE_RECEIVABLE:USDC 0.10   Cr PLATFORM_FEE_REVENUE:USDC 0.10
Dr WALLET:SOL               0.5
Cr TRADING_INFLOW:SOL       0.5
Cr WALLET:SOL               0.000005
Dr FEES_NETWORK:SOL         0.000005
```
USDC leg: Dr 100.20 / Cr 100.20. SOL leg: Dr 0.500005 / Cr 0.500005. One `JournalTransaction` (`kind = TRADE_FILL`, `reference = fill_id`, `idempotency_key = "fill:<fill_id>"`).

### 2.3 Immutability

`journal_transactions` and `journal_entries` have `BEFORE UPDATE OR DELETE` triggers that raise, and the application role has no UPDATE/DELETE grant on them. Corrections are new transactions with `reversal_of` set and `kind = COMPENSATION` / `CORRECTION` and a reason code. `content_hash` (sha256 of canonical JSON of the transaction and its entries) is computed at posting and later chained by the proof system.

### 2.4 Balances

`ledger_balances(ledger_account_id, balance, entry_count, version)` is maintained by an `AFTER INSERT` trigger on `journal_entries` inside the posting transaction. The trigger raises `LEDGER_NEGATIVE_BALANCE` if an account with `allow_negative = false` would go below zero. A periodic job (`ledger.VerifyBalances`) recomputes `Σ entries` per account and raises a SEV1 `ledger_integrity_violation` on drift.

Balanced-per-asset is enforced by a **deferred constraint trigger** evaluated at commit for every transaction that received entries. Application code enforces it again before insert (defense in depth).

Lock ordering: the posting service inserts entries ordered by `ledger_account_id` so the balance-row locks are acquired in a global order; deadlocks (40P01) are retried by `db.InTx`.

## 3. Capital reservation (PARTS 22–24)

Reservations are made in the **settlement asset's exact quantity** (V1: USDC base units) and, when an agent envelope is involved, in **USD minor units against the envelope budget** — both atomically.

```
asset_reservation_totals(account_id, asset_id) ← row lock (SELECT … FOR UPDATE)
available_quantity = ledger_balances[WALLET(account,asset)] − totals.reserved − Σ active withdrawal_holds(account,asset)
require available_quantity ≥ requested_quantity          else INSUFFICIENT_BUYING_POWER
if envelope: capital_envelopes[id] ← row lock; require available_usd ≥ usd_minor; available_usd −= usd_minor; reserved_usd += usd_minor
insert asset_reservations(status=ACTIVE, expires_at)
totals.reserved += requested_quantity
persist intent state (caller, same tx)
outbox: capital.reservation.created
commit
```

Read committed + row locks on the two rows serialize all reservations for a given (account, asset) and envelope — rigorously equivalent to SERIALIZABLE for this access pattern. The torture test also runs under `db.Serializable`.

Lifecycle: `ACTIVE → CONSUMED | RELEASED | EXPIRED`. `Consume` is only legal from ACTIVE (a released reservation can never support execution). Expiry never touches a reservation whose `locked_by_order_id` is set (orders in SUBMITTED/SUBMISSION_UNKNOWN keep their reservation until reconciliation decides).

`capital_envelopes` carries every field from PART 24. Authority fields (`allocation_usd_minor`, `max_*`, `allowed_*`, `policy_version`, `status`, `effective_at`, `expires_at`) change only through `capital.Administer`, which requires a non-agent principal and writes an audit event. The reserve/release/consume path is the only agent-reachable path and cannot touch authority fields.

## 4. Funding state (PARTS 27–28)

`deposits` carries the state machine `CREATED → SESSION_CREATED → CUSTOMER_ACTION_REQUIRED → PROVIDER_PROCESSING → PROVIDER_CONFIRMED → SETTLEMENT_OBSERVED → RECONCILED → AVAILABLE` with side states `FAILED, EXPIRED, CANCELLED, REVERSED, REVIEW_REQUIRED`. The legal transition table lives in `funding.Transitions` and every transition is inserted into `deposit_transitions` with actor, reason, evidence reference and timestamp; illegal transitions return `INVALID_STATE_TRANSITION`.

Distinct flags: `buying_power_eligible` (set at AVAILABLE under policy) and `withdrawal_eligible` (set only after `reversible_until` has passed and `fraud_state = CLEARED|NONE`). A reversal after AVAILABLE posts the compensating transactions in §2.2, creates `withdrawal_holds` as needed, freezes the account if a deficit exists, emits `funding.reversed` and a SEV alert, and records the full trail.

## 5. Positions (PART 85)

`position_lots` (acquisition lots, FIFO by default; method recorded per account policy) and `lot_dispositions` (quantity, proceeds, fees, realized P&L in USD minor units, valuation source). Holdings per (account, asset) = Σ open lot quantity and must equal the `WALLET` ledger balance (checked by `positions.VerifyAgainstLedger`). Unrealized P&L is valuation, computed on read.

## 6. Valuation and buying power (PARTS 25–26)

`asset_prices` are observations (mantissa/scale/quote/source/observed_at/received_at). `asset_policies` is an append-only history of `{status, collateral_factor_bps, stablecoin_status, policy_version, effective_at}` per asset; the current row is the latest effective. Stablecoin status drives contribution: NORMAL → `collateral_factor_bps` of face; DEGRADED → market price × haircut; RESTRICTED → contributes to portfolio value but not to buying power; HALTED → excluded and operations on the asset blocked.

`capital.BuyingPowerEngine.Compute(ctx, accountID, purpose)` returns exactly:

```
portfolio_value, buying_power, available_now, reserved, pending, withdrawable   (USD minor units)
underlying_balances[]  {asset, quantity, usd_value, price_ref, status}
haircuts[]             {asset, factor_bps, reason}
restrictions[]         {code, detail}
policy_version, as_of
```

Inputs: ledger balances, reservation totals, withdrawal holds, deposits in non-final states (pending), asset policies, prices with max age, account status, kill switches (Stage 3), provider health (Stage 6). Output is never cached as truth; the API layer may cache the rendered response ≤ 2 s keyed by account with invalidation on outbox events.

## 7. Go interface contracts (fixed)

```go
// internal/ledger
type Side string // DEBIT | CREDIT
type Code string // WALLET, CAPITAL, TRADING_OUTFLOW, ... (see §2.1)
type OwnerType string // CUSTOMER | PLATFORM
type Kind string // FUNDING_SETTLED, FUNDING_REVERSAL, FUNDING_REVERSAL_DEFICIT, TRADE_FILL, FEE, WITHDRAWAL_SETTLED, COMPENSATION, CORRECTION, RECONCILIATION_ADJUSTMENT, SEED (LOCAL/TEST only)
type AccountRef struct { OwnerType OwnerType; OwnerID string; Code Code; AssetID assets.AssetID }
type Entry struct { Account AccountRef; Side Side; Quantity money.Quantity; USDValueMinor *int64; PriceRef *string }
type Posting struct { Kind Kind; IdempotencyKey string; Reference FinancialEventReference; EffectiveAt time.Time; Description string; CorrelationID string; ReversalOf *TransactionID; Entries []Entry; Metadata map[string]any }
type FinancialEventReference struct { Type string; ID string }  // e.g. {"fill", "<uuid>"}, {"deposit", "<uuid>"}, {"reconciliation_record", "<uuid>"}
type Poster interface {
  Post(ctx context.Context, tx pgx.Tx, p Posting) (PostResult, error)          // validates balanced-per-asset, orders entries, inserts, returns TransactionID + ContentHash; idempotent on IdempotencyKey (returns Existing=true)
}
type Reader interface {
  Balance(ctx, q db.Querier, ref AccountRef) (money.Quantity, error)
  BalancesForOwner(ctx, q, ownerType, ownerID) ([]AccountBalance, error)
  Transaction(ctx, q, id TransactionID) (Transaction, error)
  ListTransactions(ctx, q, filter, cursor, limit) ([]Transaction, string, error)
}
func VerifyBalances(ctx, q db.Querier) ([]Drift, error)

// internal/capital
type ReserveRequest struct { AccountID string; AssetID assets.AssetID; Quantity money.Quantity; USDMinor int64; EnvelopeID *EnvelopeID; IntentID string; ActorType security.ActorType; ActorID string; IdempotencyKey string; TTL time.Duration; Reason string }
type Reserver interface {
  Reserve(ctx, tx pgx.Tx, r ReserveRequest) (Reservation, error)    // INSUFFICIENT_BUYING_POWER on shortfall; idempotent on IdempotencyKey
  Consume(ctx, tx pgx.Tx, id ReservationID, qty money.Quantity, usdMinor int64, orderID string) (Reservation, error)
  Release(ctx, tx pgx.Tx, id ReservationID, reason string) (Reservation, error)
  LockForOrder(ctx, tx pgx.Tx, id ReservationID, orderID string) error
  ExpireDue(ctx, tx pgx.Tx, now time.Time, limit int) ([]ReservationID, error)
}
type EnvelopeAdmin interface { Create(ctx, tx, Envelope) (Envelope, error); Update(ctx, tx, EnvelopeID, EnvelopeAuthorityPatch) (Envelope, error); SetStatus(...); Get(...) } // requires non-agent principal; audited
type BuyingPowerEngine interface { Compute(ctx, q db.Querier, accountID string, purpose Purpose) (BuyingPower, error) }

// internal/funding
type Status string // CREATED ... AVAILABLE, FAILED, EXPIRED, CANCELLED, REVERSED, REVIEW_REQUIRED
func CanTransition(from, to Status) bool
type Transitioner interface { Transition(ctx, tx pgx.Tx, id DepositID, to Status, ev TransitionEvidence) (Deposit, error) }
type Settler interface { RecordSettlement(ctx, tx, id DepositID, observed money.Quantity, sig string, slot int64) error; MarkAvailable(ctx, tx, id, policy AvailabilityPolicy) error; Reverse(ctx, tx, id, reason string, evidence string) (ReversalResult, error) }

// internal/positions
type LotEngine interface { Acquire(ctx, tx, AcquireLot) (Lot, error); Dispose(ctx, tx, Disposal) ([]LotDisposition, RealizedPnL, error); Holdings(ctx, q, accountID) ([]Holding, error); VerifyAgainstLedger(ctx, q, accountID) ([]Drift, error) }

// internal/valuation
type PriceSource interface { Latest(ctx, q, assetID, quoteAssetID assets.AssetID, maxAge time.Duration, now time.Time) (money.Price, error) } // STALE_MARKET_DATA when too old/missing
type PolicyReader interface { Current(ctx, q, assetID assets.AssetID, at time.Time) (AssetPolicy, error) }
```

## 8. Invariant test matrix (PART 21) — every row must have an automated test

| Invariant | DB level | App level | Property test |
|---|---|---|---|
| Σdebits == Σcredits per (tx, asset) | deferred constraint trigger | `ledger.validatePosting` | `TestProp_PostingBalanced` |
| posted entries immutable | triggers + no grants | no mutation API exists | `TestLedger_UpdateDeleteForbidden` (as cp_app and via trigger) |
| corrections are compensating transactions | `reversal_of` FK | `Poster.Post` with `Kind=COMPENSATION` only | `TestLedger_CorrectionIsNewTransaction` |
| no binary float in financial quantities | NUMERIC(38,0)/BIGINT | `money` types only | `lintfin` + `TestNoFloatingPointInSource` |
| no reservation exceeds available at commit | row locks | `Reserve` check inside tx | `TestProp_ReservationsNeverOversubscribe`, torture test |
| agent cannot increase own capital / change risk / withdraw / sign | — | `security` permission matrix, package import rules | `TestAgentPrincipalCannot*`, depguard |
| one idempotency key ⇒ ≤ 1 economic effect | PK on idempotency_keys; unique on journal idempotency_key | `idempotency.Store` | `TestProp_DuplicateCommandsOneEffect` |
| one provider event ⇒ ≤ 1 canonical event | inbox PK | `event.Inbox.Process` | `TestProp_DuplicateWebhookOneEffect` |
| one external fill ⇒ ≤ 1 internal fill | unique(venue, external_fill_id) | execution stage | Stage 6 |
| released reservation cannot support execution | status check | `Consume` rejects non-ACTIVE | `TestReservation_ConsumeAfterReleaseRejected` |
| deficit never silent | `allow_negative=false` trigger | deficit posting path | `TestFunding_ReversalCreatesDeficitAndFreezes` |
