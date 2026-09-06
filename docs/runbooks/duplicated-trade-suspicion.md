# Runbook: duplicated trade suspicion

Severity: SEV1 (duplicate economic execution) · Owner: OPERATIONS (containment), FINANCE (repair) · Related: [submission-unknown.md](./submission-unknown.md), [ledger-mismatch.md](./ledger-mismatch.md), [jupiter-outage.md](./jupiter-outage.md), [global-kill-and-reenable.md](./global-kill-and-reenable.md)

## Trigger

- Two landed transactions for one plan: more than one `execution_attempts` row per `order_id` with a `tx_signature` in `SUBMITTED | OBSERVED | CONFIRMED | FINALIZED | ADOPTED`.
- Two `fills` rows for one order, or one `tx_signature` appearing on two fills.
- `provider_duplicate_events` rising together with fill anomalies (a duplicate provider event that was not deduplicated by the inbox).
- `duplicate_command_rejections` spike: usually benign (idempotency doing its job) but a leading indicator of a client retry storm.
- Customer report of a double trade; reconciliation `EXECUTION` mismatch where observed quantity is about twice expected.
- PENDING: alarm rules on these metrics; the instruments exist. The design invariants that make duplication impossible are on disk: `execution_attempts.tx_signature UNIQUE`, `UNIQUE (order_id, attempt_no)`, `fills UNIQUE (venue, external_fill_id)`, `orders UNIQUE (intent_id)`, `journal_transactions.idempotency_key = 'fill:<fill_id>'`.

## Blast radius

- The affected account(s): exposure may be double what the plan authorised; reservation may be over-consumed.
- If the cause is the executor or recoverer (a second attempt built while the first was `SUBMISSION_UNKNOWN`), every account with in-flight orders is exposed.
- Keeps running: reconciliation, fill posting, settlement, ledger. A real second fill is real economic activity and must be posted, never suppressed.

## Immediate actions (first 10 minutes)

1. Find the duplicates (read-only, `cp_readonly`):
   ```sql
   SELECT order_id, count(*) AS landed, array_agg(attempt_no ORDER BY attempt_no) AS attempts, array_agg(tx_signature) AS sigs
     FROM execution_attempts
    WHERE tx_signature IS NOT NULL AND status IN ('SUBMITTED','OBSERVED','CONFIRMED','FINALIZED','ADOPTED')
    GROUP BY order_id HAVING count(*) > 1;
   SELECT order_id, count(*) FROM fills GROUP BY order_id HAVING count(*) > 1;
   SELECT tx_signature, count(*) FROM fills WHERE tx_signature IS NOT NULL GROUP BY tx_signature HAVING count(*) > 1;
   ```
2. Freeze the affected account(s): `POST /admin/kill-switches {"kind":"ACCOUNT_FREEZE","scope_id":"<account_id>","action":"activate","reason":"<INC-id>: duplicate execution suspected"}` (STANDARD; `kill:activate`). PENDING: `cmd/api`.
3. If more than one account is affected, or the first query shows a second attempt created while the first was `SUBMISSION_UNKNOWN`, the executor is suspect: activate `GLOBAL_NEW_RISK_KILL` per [global-kill-and-reenable.md](./global-kill-and-reenable.md). If only one venue is involved, `VENUE_DISABLE(<venue>)` (STANDARD) is the narrower switch.
4. Classify quickly, without acting:
   - **two signatures on chain for one plan** → genuine duplicate (SEV1 confirmed);
   - **one signature observed twice** → duplicate observation; the `UNIQUE (venue, external_fill_id)` insert is a no-op and the ledger has one `TRADE_FILL`; SEV1 not confirmed;
   - **`PARTIALLY_FILLED` with cumulative quantities** → generic partial-fill modelling (EXECUTION.md §7), not duplication.
5. Post the order ids, classification and switch state to the incident channel.

## Diagnosis

- Attempt timeline per order: `SELECT attempt_no, status, tx_signature, submitted_at, last_valid_block_height, error, created_at FROM execution_attempts WHERE order_id = '<id>' ORDER BY attempt_no;`. A second attempt is legal only after the first is `EXPIRED` (proven absent: both observers `NOT_FOUND` and block height beyond `last_valid_block_height` + margin) and a fresh risk `FINAL` decision exists (`risk_decisions` with `stage = FINAL` after the expiry).
- Signing evidence: `SELECT attempt_id, decision, reason_codes, inspector_version, decided_at, provider_sign_ref FROM signing_decisions WHERE plan_id = '<plan_id>' ORDER BY decided_at;`. Two `APPROVED` decisions for one plan with overlapping validity is the defect signature.
- Ledger: `SELECT id, kind, reference_id, idempotency_key, posted_at FROM journal_transactions WHERE reference_type = 'fill' AND reference_id IN (<fill ids>);` — exactly one per fill id.
- Reservation: `SELECT id, status, quantity, consumed_quantity, locked_by_order_id FROM asset_reservations WHERE id = (SELECT reservation_id FROM orders WHERE id = '<order_id>');` — `consumed_quantity` above `quantity` is impossible by CHECK; a second fill therefore consumed from somewhere else or the executor bypassed `capital.Service`.
- Provider side: execution provider status for both signatures (Jupiter `/execute` returns "accepted", not "landed"; `docs/api/providers/jupiter.md`); `provider_events` and `inbox_messages` for duplicate deliveries (`SELECT source, message_id, status, received_at FROM inbox_messages WHERE message_id IN (...)`).
- Audit: `audit_events WHERE stream = 'account:<id>'` around the two submissions; correlation ids tie the intent, plan, attempts and fills together.
- PENDING: the executor/recoverer (`internal/execution` has records and repositories only) and the reconciliation engine; the crash test (PART 49, R-049-1) and unknown-submission recovery tests are the controls that would have prevented this.

## Containment and recovery

1. **Genuine duplicate.** Both fills are external truth: they must be posted (`TRADE_FILL`) and positions updated; reconciliation opens an `EXECUTION` mismatch (material). Customer impact is repaired by a `COMPENSATION` journal transaction under `LEDGER_CORRECTION` (FINANCE proposes with `ledger:post_correction`, step-up ≤ 5 min; a break-glass `ledger:approve_correction` holder approves) and the record is resolved via `RECONCILIATION_RESOLVE_MATERIAL` ([ledger-mismatch.md](./ledger-mismatch.md) steps 2–4). Reduce the unintended exposure only through a normal `REDUCE_RISK` intent under the customer's or operator's authority, never by editing positions.
2. **Duplicate observation.** No repair. Verify one journal transaction and one fill; resolve the record as `RESOLVED_AUTOMATIC{duplicate provider event}` if the engine did not already.
3. **Executor defect.** Keep the global kill until the fix and its regression test (a second attempt is impossible while any attempt for the plan is non-terminal) are merged and deployed; release via the dual-controlled path.
4. Release `ACCOUNT_FREEZE` (`kill:release` + step-up) only after the compensation has posted and the record is `RESOLVED_MANUAL`; `ACCOUNT_UNFREEZE` admin action (COMPLIANCE, step-up) restores `accounts.status`.

## What NOT to do

- Never "undo" the second trade by submitting an opposite swap from an operator seat; that is new risk and it needs the customer's authority or an operator intent under the normal path.
- Never delete the second fill or attempt (immutable; `fills_guard`, `LG003`), and never set an attempt to `FAILED` to make the counts match.
- Never re-submit after a timeout (PART 48); the recoverer adopts or proves absent.
- Never edit `asset_reservations` or `ledger_balances`.
- Never release the global kill before the executor regression test is green.

## Verification / exit criteria

- Query 1 in Immediate actions returns no rows outside the known incident orders; those orders are terminal (`SETTLED` or `FAILED_FINAL`) with every fill posted.
- The reconciliation record is `RESOLVED_MANUAL` with `approval_id` and `compensating_journal_transaction_id`.
- `capital.VerifyReservationTotals` and `positions.VerifyAgainstLedger` report zero drift for the account.
- Root-cause regression test merged (crash test PART 49 and "never a duplicate submission for the same plan").

## Post-incident

- Archive the account audit stream slice, both signatures' raw observations (`raw_ref`), signing decisions and the compensation transaction.
- Security review: rule out that the second signing decision came from outside `cmd/execution-worker` (depguard restricts `internal/signing`) — if it did, escalate to [wallet-provider-compromise.md](./wallet-provider-compromise.md).
- Update `docs/build/REQUIREMENTS_TRACEABILITY.md` R-048-1, R-049-1, R-046-* with the incident; add a BLOCKERS entry for the missing chaos test if absent.
