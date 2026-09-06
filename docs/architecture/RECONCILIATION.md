# RECONCILIATION

Status: design fixed 2026-09-05; implementation tracked in `docs/build/REQUIREMENTS_TRACEABILITY.md`. Contract for Stage 7. Covers PARTS 21, 48–52, 107, 158, 163, 172, 195, 196.

Reconciliation is core logic, not a batch afterthought. External truth (chain, providers) is compared with internal accounting truth (ledger, reservations, positions, deposits) continuously, and every difference becomes a first-class record with an explicit lifecycle. Kill switches, agent pauses, and provider circuit breakers never stop reconciliation.

## 1. Three modes (PART 50)

| Mode | Trigger | Scope | Compares |
|---|---|---|---|
| `EVENT_DRIVEN` | after every execution attempt reaches a terminal or unknown state; after every funding transition ≥ PROVIDER_CONFIRMED; after every withdrawal submission | one order / deposit / withdrawal | expected fills vs observed transaction; expected fee vs actual fee; expected settlement quantity vs observed |
| `PERIODIC` | scheduler (default every 60 s per provider window, every 5 min per active account) | provider/account windows since last checkpoint | provider execution events vs internal fills; wallet activity vs known attempts (detects unknown transactions); deposits pending vs provider status |
| `FULL` | scheduler (default hourly) and on demand | every active account, every asset | expected wallet balance (`WALLET` ledger balance) vs observed on-chain balance; Σ open lots vs `WALLET` balance; Σ journal entries vs `ledger_balances`; Σ active reservations vs `asset_reservation_totals` |

Checkpoints per (mode, scope) are persisted so periodic runs resume after restarts and never re-open resolved records.

## 2. Record lifecycle (PART 51)

```
OPEN → MATCHED
OPEN → MISMATCH → INVESTIGATING → RESOLVED_AUTOMATIC | RESOLVED_MANUAL | ESCALATED
ESCALATED → INVESTIGATING → RESOLVED_MANUAL
```

`RESOLVED_AUTOMATIC` is allowed only for enumerated, policy-listed causes with deterministic fixes: fee dust below `auto_resolve_threshold` (posts `RECONCILIATION_ADJUSTMENT`), finality upgrade (OBSERVED → FINALIZED, no economic change), duplicate provider event (inbox already processed). Everything else requires a human: `RESOLVED_MANUAL` needs operator id, reason, evidence reference, and — when `material = true` (difference ≥ `material_threshold_usd_minor` or any unknown transaction) — an approved `admin_actions` row (dual control). A manual resolution that changes financial state does so **only** through a compensating `JournalTransaction` (`kind = RECONCILIATION_ADJUSTMENT` or `COMPENSATION`) referenced from the record; balances are never edited.

`blocks_new_risk = true` on any open record whose scope is an account with a material mismatch, an unknown transaction, or an order in `SUBMISSION_UNKNOWN`/`RECONCILIATION_REQUIRED`. The risk kernel reads the count of such records (`RISK_RECONCILIATION_PENDING`); global policy may halt all new trading if the oldest unresolved material mismatch exceeds `max_unresolved_age` (PART 158).

## 3. Execution reconciliation (PARTS 48, 49)

For each attempt in `SUBMITTED | SUBMISSION_UNKNOWN | OBSERVED | CONFIRMED`:

1. `Status(signature)` from the execution adapter; `GetTransaction(signature)` from the chain observer **and** the fallback RPC.
2. Apply the agreement policy (EXECUTION.md §6). Disagreement → record `MISMATCH` with both observations, `blocks_new_risk = true`.
3. If a transaction is found: decode it, extract the actual token deltas for the wallet, and produce an `ExternalExecutionEvent`. Insert the fill under `UNIQUE (venue, external_fill_id)` — a duplicate observation is a no-op. Compare against the plan's expectations; a fill outside `MinOutputQuantity`/`MaxInputDebit` is a `MISMATCH` (a signing-boundary failure candidate → SEV1 `unauthorized signing` investigation).
4. If the transaction is proven absent (both observers `NOT_FOUND` and block height beyond `LastValidBlockHeight` + margin): attempt → `EXPIRED`; order → `PLANNED` (eligible for a new attempt under the same plan) or `FAILED_FINAL` if the intent deadline passed; reservation stays until the order is terminal.
5. Post ledger and update positions from the fill (idempotent on fill id); release the unused reservation remainder; record the audit trail. This path is the same code the executor uses, so a crash at any point resumes here.

Unknown transactions (wallet activity not matching any attempt) open a `SUBMISSION_UNKNOWN`-kind record, block new risk for the account, and raise a SEV1 candidate alert; resolution requires manual classification (`adopted-as-fill`, `external-deposit`, `unauthorized`) with evidence.

## 4. Wallet balance reconciliation (PART 50)

For each (wallet, asset): observed balance (both observers, agreement policy) vs `WALLET` ledger balance **adjusted for in-flight attempts** (attempts in SUBMITTED/OBSERVED whose fills are not yet posted contribute expected deltas). Differences within `dust_threshold` (per asset, e.g. rent-related lamport changes) are `RESOLVED_AUTOMATIC` with a `RECONCILIATION_ADJUSTMENT` posting only if policy allows automatic dust posting; otherwise recorded as `MATCHED_WITH_DUST` metadata and left. Larger differences are `MISMATCH`.

## 5. Funding reconciliation (PARTS 27, 162)

For each deposit ≥ `PROVIDER_CONFIRMED` and < `AVAILABLE`: provider status vs internal status; expected quantity vs observed on-chain credit to the destination wallet (`GetTransaction` / wallet activity). Only an observed and agreed chain receipt moves a deposit to `SETTLEMENT_OBSERVED`; `RECONCILED` requires provider status and chain receipt to agree on quantity (within policy tolerance for provider fees) and the ledger posting `FUNDING_SETTLED` to exist. A provider "success" without chain receipt after `settlement_timeout` opens a `FUNDING` mismatch.

## 6. Internal consistency (PART 21)

`ledger.VerifyBalances` (Σ entries == balances), `positions.VerifyAgainstLedger` (Σ open lots == WALLET balance per asset), `capital.VerifyReservationTotals` (Σ active reservations == totals). Any drift is a `LEDGER_INTERNAL` mismatch, `material = true`, SEV1 `ledger_integrity_violation`.

## 7. Operator flow (PART 163)

Simulated in `test/e2e/reconciliation_test.go`: internal expected 100 USDC, external observed 99.99 USDC → record `MISMATCH` (material by threshold) → account `blocks_new_risk` → new intent rejected with `RECONCILIATION_REQUIRED` → operator inspects evidence (both observations, ledger, fills) via the admin API → proposes `RECONCILIATION_RESOLVE_MATERIAL` with reason and evidence → second operator approves → compensating journal posts 0.01 USDC `RECONCILIATION_ADJUSTMENT` → record `RESOLVED_MANUAL` → block lifted → audit chain contains every step.

## 8. Go contracts (fixed)

```go
// internal/reconciliation
type Kind string; type Mode string; type Status string
type Record struct { /* mirrors reconciliation_records */ }
type Engine interface {
  ReconcileAttempt(ctx, attemptID string) (Record, error)            // EVENT_DRIVEN execution
  ReconcileDeposit(ctx, depositID string) (Record, error)            // EVENT_DRIVEN funding
  RunPeriodic(ctx, provider string, since time.Time) ([]Record, error)
  RunFull(ctx, accountID string) ([]Record, error)
  VerifyInternal(ctx) ([]Record, error)
}
type Resolver interface {
  Investigate(ctx, tx, recordID, operatorID, note string) (Record, error)
  ResolveAutomatic(ctx, tx, recordID string, cause AutoCause) (Record, error)         // only enumerated causes
  ResolveManual(ctx, tx, recordID string, res ManualResolution) (Record, error)       // ManualResolution{OperatorID, Reason, EvidenceRef, ApprovalID *string, Compensation *ledger.Posting}; material ⇒ ApprovalID required
  Escalate(ctx, tx, recordID, reason string) (Record, error)
}
type BlockReader interface { BlocksNewRisk(ctx, q, accountID string) (bool, []Record, error); OldestUnresolvedMaterial(ctx, q) (*time.Time, error) }
```

## 9. Tests required before Stage 7 exit

Crash/unknown-submission E2E (PART 49); duplicate observation ⇒ one fill (property over N duplicate deliveries); observer disagreement ⇒ MISMATCH and block; dust auto-resolution posts exactly one adjustment; material manual resolution without approval is rejected; kill switch active ⇒ reconciliation still runs and posts; internal consistency drift detection (inject a direct `ledger_balances` change as migrate role in a test) ⇒ SEV1 record.
