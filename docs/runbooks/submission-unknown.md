# Runbook: submission unknown (the timeout rule, PART 48)

Severity: SEV2 (elevated unknown submissions); SEV1 if observers disagree or a fill lands outside plan bounds · Owner: OPERATIONS · Related: [jupiter-outage.md](./jupiter-outage.md), [rpc-disagreement.md](./rpc-disagreement.md), [unknown-transaction.md](./unknown-transaction.md), [duplicated-trade-suspicion.md](./duplicated-trade-suspicion.md), `docs/architecture/EXECUTION.md` §4

## Trigger

- `unknown_submissions` counter; `unknown_submission_rate_unknown / unknown_submission_rate_submissions` above threshold (PART 135 SEV2 "elevated unknown submissions").
- Any `execution_attempts.status = 'SUBMISSION_UNKNOWN'` older than one blockhash validity window (~60–90 s, `last_valid_block_height`) plus the policy margin; any order in `SUBMISSION_UNKNOWN` or `RECONCILIATION_REQUIRED`.
- Executor crash/restart with attempts in `SUBMITTING`/`SUBMITTED` (the PART 49 crash scenario).
- The executor/recoverer that performs steps 3–8 below (`internal/execution`, driven by `cmd/execution-worker`) and the reconciliation engine (`internal/reconciliation`, `cmd/reconciliation-worker`) both exist; `internal/chain` has proven-absence resolution. **BLOCKED_EXTERNAL:** alarms, and a real venue to submit against.

## Blast radius

- The order: its reservation stays `ACTIVE` and `locked_by_order_id` set; no duplicate is ever submitted for the same plan; the account's capital is held until the outcome is known. If the transaction landed, the customer has the position; if it did not, nothing changed.
- The account: while the order is non-terminal the reconciliation record (if opened) sets `blocks_new_risk`; other orders and accounts are unaffected.
- Elevated rate: indicates the execution provider or RPC path is timing out ([jupiter-outage.md](./jupiter-outage.md)); the switch `PROVIDER_DISABLE_NEW_ACTIONS(jupiter)` stops adding to the backlog.
- Keeps running: observation, reconciliation, settlement of resolved attempts, ledger posting.

## Immediate actions (first 10 minutes)

1. Do nothing to the order. **Transport timeout is not execution failure.** Confirm the invariants hold (read-only):
   ```sql
   SELECT a.id, a.order_id, a.attempt_no, a.status, a.tx_signature, a.last_valid_block_height, a.submitted_at, a.provider,
          r.status AS reservation_status, r.locked_by_order_id
     FROM execution_attempts a JOIN orders o ON o.id = a.order_id JOIN asset_reservations r ON r.id = o.reservation_id
    WHERE a.status = 'SUBMISSION_UNKNOWN' ORDER BY a.submitted_at;
   SELECT order_id, count(*) FROM execution_attempts WHERE status NOT IN ('FAILED','EXPIRED','INSPECTION_REJECTED','SIGNING_REJECTED')
    GROUP BY order_id HAVING count(*) > 1; -- must be empty: never two live attempts per plan
   ```
   A reservation not `ACTIVE`/locked, or a second live attempt, is a SEV1 executor defect: [duplicated-trade-suspicion.md](./duplicated-trade-suspicion.md).
2. If the rate is elevated (many attempts, one provider): `POST /admin/kill-switches {"kind":"PROVIDER_DISABLE_NEW_ACTIONS","scope_id":"jupiter","action":"activate","reason":"<INC-id>: submit timeouts"}` (SEVERE; `kill:activate`).
3. Confirm both chain observers are healthy; recovery needs them ([helius-outage.md](./helius-outage.md)).
4. Let the recoverer run. Where it cannot classify the attempt, an engineer performs the observation steps in Diagnosis **read-only** and files the evidence; classification is then done through the reconciliation resolution path, never by editing rows.
5. Announce counts, provider, and that no manual resubmission will occur.

## Diagnosis

The recovery decision tree (EXECUTION.md §4, steps 3–8), with the signed transaction bytes persisted so the signature is known:

1. `Status(signature)` on the execution provider (`SAFE_RETRY` read; Jupiter "accepted" is not "landed").
2. `GetTransaction(signature)` on the primary observer (Helius) **and** the fallback RPC (`getTransaction` with `maxSupportedTransactionVersion: 0`; `getSignatureStatuses` with `searchTransactionHistory`), and `GetBlockHeight` on both.
3. `SearchWalletActivity(wallet, since = attempt.created_at)` on both observers for a transaction matching the plan's expected instructions (catches a landed transaction under a signature the provider never returned).
4. Resolve with `chain.AgreementPolicy`:
   - **found and `AGREED`** ⇒ adopt: attempt `ADOPTED`, observe finality, produce the `ExternalExecutionEvent`, insert the fill (`UNIQUE (venue, external_fill_id)`), post `TRADE_FILL`, update positions, release the unused reservation remainder — the same code path as the normal fill (RECONCILIATION.md §3 step 5);
   - **proven absent** (both `NOT_FOUND` **and** block height > `last_valid_block_height` + margin) ⇒ attempt `EXPIRED`; order back to `PLANNED` if the intent deadline has not passed (a new attempt with a fresh blockhash under the same plan, `attempt_no + 1`, only after a fresh risk `FINAL` decision), else `FAILED_FINAL` and the reservation releases;
   - **disagreement or incomplete evidence** (one observer down, height not yet past validity, economics differ) ⇒ order `RECONCILIATION_REQUIRED`, record `MISMATCH` with both observations, `blocks_new_risk = true`, no new attempt: [rpc-disagreement.md](./rpc-disagreement.md).
5. A found fill outside `MinOutputQuantity`/`MaxInputDebit` is a signing-boundary failure candidate: SEV1, [wallet-provider-compromise.md](./wallet-provider-compromise.md).

Evidence to keep per attempt: both raw observations (`raw_ref`), block heights, `last_valid_block_height`, the provider status response (`submit_response_ref`), timestamps.

## Containment and recovery

1. Adopted: verify exactly one fill and one `TRADE_FILL` journal transaction for the order; `fills.position_applied_at` set; reservation `CONSUMED` with remainder released.
2. Expired: verify `asset_reservations` released only when the order is terminal; a `PLANNED` order's next attempt must show a new `risk_decisions` `FINAL` row and a new quote.
3. `RECONCILIATION_REQUIRED`: resolution only through `RECONCILIATION_RESOLVE_MATERIAL` (dual control) with evidence; the engine then adopts or expires; `orders.status` is never edited.
4. Elevated-rate incidents: release `PROVIDER_DISABLE_NEW_ACTIONS` via the dual-controlled `KILL_SWITCH_RELEASE` path after the provider is healthy and the backlog is resolved ([jupiter-outage.md](./jupiter-outage.md)).
5. Crash case (PART 49): on executor restart the recoverer resumes from persisted state; if a crash happened after the transaction landed but before success persisted, the outcome is discovery and adoption, never a second submission.

## What NOT to do

- Never re-submit the signed bytes, rebuild a new attempt, or "retry the POST" while an attempt is `SUBMISSION_UNKNOWN` (PART 48; PART 247 anti-pattern "retry POST after timeout").
- Never release the reservation by hand, and never mark the attempt `FAILED` on a provider error without chain proof of absence past `last_valid_block_height`.
- Never treat a single observer's `NOT_FOUND` as absence; never treat "accepted" as landed; never treat HTTP 200 as settlement.
- Never edit `orders`, `execution_attempts` or `fills` rows; transitions come from the executor/recoverer with audit and outbox events in the same transaction.
- Never disable observation or reconciliation to reduce RPC load during a backlog.

## Verification / exit criteria

- No `SUBMISSION_UNKNOWN` attempt older than the validity window + margin without a reconciliation record; every affected order terminal (`SETTLED`/`FAILED_FINAL`) or legitimately `PLANNED`.
- For adopted attempts: one fill, one journal transaction, positions applied, reservation consumed/released; audit stream shows the recovery steps.
- `unknown_submission_rate` back to baseline; provider switch released through its path.
- The invariant queries in Immediate actions return no violations.

## Post-incident

- Archive per-attempt evidence and the recovery outcome table (adopted / expired / reconciliation-required counts).
- If the cause was a provider timeout profile, tune adapter timeouts in configuration (not code) and record it in `docs/api/providers/jupiter.md`.
- Update `docs/build/REQUIREMENTS_TRACEABILITY.md` R-048-1, R-049-1; the crash test (`test/chaos/crash_after_submit_test.go`) and `test/chaos/submit_timeout_test.go` are production-readiness blockers if still absent (BLOCKERS entry).
