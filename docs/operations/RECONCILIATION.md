# RECONCILIATION OPERATIONS

Status: operating procedure for the reconciliation subsystem designed in `docs/architecture/RECONCILIATION.md`. **CORRECTION (F-51):** the reconciliation engine IS implemented — `internal/reconciliation` has `NewEngine`, `RunPeriodic` and `RunFull`, `cmd/reconciliation-worker` drives them on a ticker, and the admin read endpoints exist. This line said it was not, which is stale in the pessimistic direction and just as misleading: a reader following this runbook would not look for a subsystem that is running. What is genuinely absent is the RESOLUTION path — `cmd/api` wires `Reconcile: nil`, so both resolve endpoints answer UNSUPPORTED while the worker continues to raise records and block new risk; the tables, state machine constraints, admin API routes, and the blocking semantics it will rely on exist. Every step below that needs the engine is marked PENDING so operators never assume a control that does not run yet.

## 1. What operators see

| Surface | Today | Notes |
|---|---|---|
| `reconciliation_records` table (migration 00301) | exists | status CHECK, `material`, `blocks_new_risk`, manual-resolution CHECK (operator + reason + evidence required) |
| `reconciliation_transitions` (immutable) + transition binding (00603, SQLSTATE `AU001`) | exists | a status change without a transition row in the same transaction is refused at COMMIT |
| `GET /v1/admin/reconciliation/records` and `POST …/{recordId}/resolve` | in `openapi/openapi.yaml`; server handlers PENDING (`cmd/api`) | resolution requires reason + evidence; material resolutions require an approved `admin_actions` row of kind `RECONCILIATION_RESOLVE_MATERIAL` (dual control, exists in `internal/admin`) |
| Metrics `reconciliation_mismatches`, `oldest_unresolved_mismatch`, `unknown_submissions` | instruments defined in `internal/observability`; emitters PENDING | alarms PENDING (Terraform observability module) |
| Risk-kernel input `unresolved material mismatches` | consumed by `internal/risk` (`RISK_RECONCILIATION_PENDING`) | the reader that counts records is PENDING (`reconciliation.BlockReader`) |

## 2. Modes and cadence (target)

| Mode | Trigger | Owner binary |
|---|---|---|
| EVENT_DRIVEN | after every execution attempt reaches terminal/unknown; after every deposit ≥ PROVIDER_CONFIRMED; after every withdrawal submission | `cmd/reconciliation-worker` consuming outbox events (PENDING) |
| PERIODIC | every 60 s per provider window; every 5 min per active account | scheduler in `cmd/reconciliation-worker` (PENDING) |
| FULL | hourly and on demand (`make reconcile-full` PENDING) | same |

Checkpoints per (mode, scope) are persisted so restarts resume; resolved records are never reopened by a rerun.

## 3. Triage procedure for a MISMATCH

1. **Read the record**: `expected`, `observed`, `difference`, `material`, `blocks_new_risk`. Never resolve from memory of what "should" have happened.
2. **Classify** using the kind:
   - `EXECUTION`: compare the attempt's persisted signed transaction hash with the on-chain transaction; check both observers agreed (agreement policy, `docs/architecture/EXECUTION.md` §6). A fill outside the plan's bounds is a **signing-boundary incident**: escalate SEV1 (`docs/runbooks/duplicated-trade-suspicion.md`, `unknown-transaction.md`).
   - `WALLET_BALANCE`: check in-flight attempts whose fills are not yet posted; dust within the per-asset threshold may be `RESOLVED_AUTOMATIC` once the engine supports it; anything larger stays manual.
   - `FUNDING`: compare provider session status with the observed chain credit; a provider "success" without a chain receipt after the settlement timeout is a provider incident (`docs/runbooks/funding-provider-compromise.md`).
   - `LEDGER_INTERNAL` / `POSITION_LEDGER`: an internal consistency drift is a SEV1 `ledger_integrity_violation`; freeze new risk globally (`GLOBAL_NEW_RISK_KILL`) and follow `docs/runbooks/ledger-mismatch.md`.
   - `SUBMISSION_UNKNOWN`: follow `docs/runbooks/submission-unknown.md`; never re-submit.
3. **Investigate**: move the record to `INVESTIGATING` (transition row required), attach evidence references (transaction signatures, provider ids, archive object refs).
4. **Resolve**:
   - Non-material: `RESOLVED_MANUAL` with operator id, reason, evidence ref (the DB CHECK enforces all three).
   - Material: propose `RECONCILIATION_RESOLVE_MATERIAL` through the admin actions API; a second, distinct principal approves under step-up; execute with the same params hash. If a financial correction is needed, it is a reason-coded compensating `JournalTransaction` (`ledger.Kind` `RECONCILIATION_ADJUSTMENT` or `COMPENSATION`) referenced from the record. **Balances are never edited.**
   - Escalate (`ESCALATED`) when evidence is incomplete or observers disagree.
5. **Confirm the block is lifted**: after resolution `blocks_new_risk` must be false and the account's next intent must pass the risk kernel's `RISK_RECONCILIATION_PENDING` check (PENDING until the engine wires the reader).

## 4. Automatic resolution policy (target)

Only enumerated causes may resolve automatically: fee dust below `auto_resolve_threshold`, finality upgrades (OBSERVED → FINALIZED with no economic change), duplicate provider events already processed by the inbox. Everything else is manual. The allowlist lives in configuration, not code, and every automatic resolution still writes a transition row and an audit event.

## 5. Evidence and audit

Every transition is appended to the audit stream of the affected account (or `system` for internal drift). `make verify-audit` (PENDING: `cmd/audit-worker verify`) recomputes the chains; an operator resolving a record must be able to point to the audit event proving the resolution and its approval.

## 6. Related

`docs/architecture/RECONCILIATION.md`, `docs/runbooks/` (ledger-mismatch, unknown-transaction, submission-unknown, rpc-disagreement, funding-provider-compromise), `docs/architecture/POLICY_AUTHORITY.md` §5, migrations 00301 and 00603, `internal/admin` (kind `RECONCILIATION_RESOLVE_MATERIAL`).
