# Runbook: ledger mismatch

Severity: SEV1 (ledger integrity violation; unexplained financial mismatch; global money-impacting reconciliation drift) · Owner: FINANCE (OPERATIONS for containment) · Related: [global-kill-and-reenable.md](./global-kill-and-reenable.md), [database-corruption.md](./database-corruption.md), [unknown-transaction.md](./unknown-transaction.md), [rpc-disagreement.md](./rpc-disagreement.md), [chargeback-reversal.md](./chargeback-reversal.md)

## Trigger

- SEV1 `ledger_integrity_violation`: a reconciliation record `kind = LEDGER_INTERNAL` (Σ `journal_entries` ≠ `ledger_balances`, Σ open lots ≠ `WALLET` balance, Σ active reservations ≠ `asset_reservation_totals`), always `material = true` (RECONCILIATION.md §6).
- SEV1 unexplained mismatch: `reconciliation_mismatches` counter with a `kind = WALLET_BALANCE | EXECUTION | FUNDING | POSITION_LEDGER` record where `material = true` and no enumerated automatic cause applies.
- SEV1 global drift: `oldest_unresolved_mismatch` gauge above risk policy `max_unresolved_age` (PART 158).
- `ledger_posting_errors` counter rising (postings rejected by triggers `LG001`–`LG005` or `LEDGER_NEGATIVE_BALANCE`).
- BLOCKED_EXTERNAL: alarm rules for these metrics (`infra/terraform/modules/observability`); the instruments exist in `internal/observability/metrics.go`. The reconciliation engine that opens the records is built (`internal/reconciliation`, driven by `cmd/reconciliation-worker`). PENDING: the verifiers `ledger.VerifyBalances`, `positions.VerifyAgainstLedger` and `capital.VerifyReservationTotals` exist but nothing schedules them.

## Blast radius

- `LEDGER_INTERNAL`: the truth store itself is in doubt; every balance, buying-power figure and reservation derived from it is suspect until the drift is explained. Treat as platform-wide.
- Single-account `WALLET_BALANCE` / `EXECUTION` mismatch: that account's `blocks_new_risk = true` is set by the engine; the kernel rejects new intents with `RISK_RECONCILIATION_PENDING`. Other accounts are unaffected.
- Keeps running regardless: reconciliation, settlement of in-flight orders, fill posting, audit. A kill switch never stops any of these (PART 52).

## Immediate actions (first 10 minutes)

1. Scope it. Read-only as `cp_readonly`:
   ```sql
   SELECT id, kind, mode, scope_type, scope_id, account_id, asset_id, status, material, blocks_new_risk,
          opened_at, expected, observed, difference
     FROM reconciliation_records
    WHERE status IN ('MISMATCH','INVESTIGATING','ESCALATED')
    ORDER BY material DESC, opened_at;
   ```
2. If any record is `kind = LEDGER_INTERNAL`, or mismatches span more than one account without a common external cause: activate `GLOBAL_NEW_RISK_KILL` per [global-kill-and-reenable.md](./global-kill-and-reenable.md) (`POST /admin/kill-switches {"kind":"GLOBAL_NEW_RISK_KILL","scope_id":"*","action":"activate","reason":"<INC-id>: ledger integrity"}`).
3. If it is one account: `POST /admin/kill-switches {"kind":"ACCOUNT_FREEZE","scope_id":"<account_id>","action":"activate","reason":"..."}` (STANDARD, `kill:activate`), and ask COMPLIANCE to set `POST /admin/accounts/{accountId}/status {"to":"FROZEN","reason":"..."}` so the account status and the switch agree.
4. Rule out direct mutation of the journal (the application role cannot, the migrate role can):
   ```sql
   SELECT relname, n_tup_upd, n_tup_del FROM pg_stat_user_tables
    WHERE relname IN ('journal_transactions','journal_entries','ledger_balances','asset_reservation_totals');
   SELECT usename, application_name, client_addr, backend_start FROM pg_stat_activity WHERE usename = 'cp_migrate';
   ```
   `journal_transactions`/`journal_entries` must show `n_tup_upd = 0`, `n_tup_del = 0` (`ledger_balances` is updated by its trigger, so non-zero there is normal). Any `cp_migrate` session outside a deployment window is an [admin-compromise.md](./admin-compromise.md) or [database-corruption.md](./database-corruption.md) case.
5. Recompute the drift yourself for the flagged ledger accounts (balances are stored in normal-side terms; `ledger_accounts.normal_side` is the reference):
   ```sql
   SELECT b.ledger_account_id, b.balance, b.entry_count,
          SUM(CASE WHEN e.side = a.normal_side THEN e.quantity ELSE -e.quantity END) AS recomputed, count(e.id) AS entries
     FROM ledger_balances b JOIN ledger_accounts a ON a.id = b.ledger_account_id
     LEFT JOIN journal_entries e ON e.ledger_account_id = b.ledger_account_id
    WHERE b.ledger_account_id IN (<ids from the record's scope>)
    GROUP BY b.ledger_account_id, b.balance, b.entry_count;
   ```
6. Announce scope, switch state and the record ids in the incident channel.

## Diagnosis

- **Which side is wrong.** A `LEDGER_INTERNAL` drift is internal (balances vs entries); a `WALLET_BALANCE` mismatch compares the `WALLET` ledger balance adjusted for in-flight attempts against both chain observers (`wallet_balance_observations`, `source` per observer). Check `execution_attempts` in `SUBMITTED | SUBMISSION_UNKNOWN | OBSERVED | CONFIRMED` for the account: an unposted fill explains a temporary difference and is not a mismatch until the attempt is terminal.
- **Unknown activity.** If `observed` shows a wallet delta with no matching attempt or deposit, this is [unknown-transaction.md](./unknown-transaction.md), not a ledger bug.
- **Observer disagreement.** If the two observers differ, follow [rpc-disagreement.md](./rpc-disagreement.md) first; a ledger figure cannot be corrected against uncertain external truth.
- **Reversal/deficit.** A `DEFICIT` credit balance after a funding reversal is expected (FINANCIAL_MODEL.md §2.2); confirm via `journal_transactions WHERE kind IN ('FUNDING_REVERSAL','FUNDING_REVERSAL_DEFICIT')` before treating it as drift.
- **Audit trail.** `audit_events WHERE stream = 'account:<id>' ORDER BY stream_seq` around `opened_at`; `make verify-audit` (run `audit.Verifier.VerifyStream` meanwhile). A chain break is evidence of tampering or corruption.
- **Dust.** Differences below the asset `dust_threshold` are `RESOLVED_AUTOMATIC` with a `RECONCILIATION_ADJUSTMENT`, or recorded as `MATCHED_WITH_DUST`; they are never SEV1.

## Containment and recovery

Financial repair is always a new, balanced, reason-coded journal transaction; balances are never edited (PART 129, 195).

1. FINANCE (holding `reconciliation:resolve`) moves the record to `INVESTIGATING` with a note (`Resolver.Investigate`) and collects evidence: both observations, ledger slice, fills, attempts, audit slice; store them in the evidence archive and quote their hashes.
2. Propose the material resolution (step-up ≤ 15 min; expires 4 h):
   ```http
   POST /admin/actions
   {"kind":"RECONCILIATION_RESOLVE_MATERIAL","target_type":"reconciliation_record","target_id":"<record_id>",
    "params":{"record_id":"<record_id>","compensation":{"reason_code":"<code>","entries":[...]}},"reason":"<INC-id>: <cause>"}
   ```
3. A different principal holding `reconciliation:approve` (break-glass; see [global-kill-and-reenable.md](./global-kill-and-reenable.md) step 1 for the grant) approves: `POST /admin/actions/{actionId}/approve`.
4. Resolve, quoting the approval; the service posts the compensating `JournalTransaction` (`kind = RECONCILIATION_ADJUSTMENT` or `COMPENSATION`, `reversal_of` where applicable) and links it in `compensating_journal_transaction_id`:
   ```http
   POST /admin/reconciliation/records/{recordId}/resolve
   {"reason":"<INC-id>:...","evidence_ref":"<archive uri>","approval_id":"<actionId>",
    "compensation":{"reason_code":"<code>","entries":[{"account_code":"...","asset":"<asset_id>","side":"DEBIT","quantity":"..."},...]}}
   ```
   If the correction is not tied to a reconciliation record, use `LEDGER_CORRECTION` instead (FINANCE proposes with `ledger:post_correction`, step-up ≤ 5 min; a break-glass `ledger:approve_correction` holder approves; expires 4 h).
5. Once every material record is `RESOLVED_MANUAL`, `blocks_new_risk` clears for the account; release `ACCOUNT_FREEZE` (`kill:release` + step-up) and, if used, the global kill via its dual-controlled path. `ACCOUNT_UNFREEZE` is a single COMPLIANCE operator admin action with step-up.
6. If drift recurs or the cause is storage-level, go to [database-corruption.md](./database-corruption.md).

`internal/reconciliation` (`Resolver`) and the resolve endpoint are wired; `admin.Actions` and the ledger poster with the `RECONCILIATION_ADJUSTMENT`/`COMPENSATION` kinds exist. The API refuses a `COMPENSATION` resolution by name as `UNSUPPORTED`: posting a compensating transaction from an HTTP handler is a decision for the ledger path, not the reconciliation one.

## What NOT to do

- Never `UPDATE ledger_balances` or `journal_*` rows, in any role. The triggers raise `LG003`; doing it as the migrate role is a reportable security incident.
- Never resolve a material record without an approved `RECONCILIATION_RESOLVE_MATERIAL` action; the service rejects it (`ManualResolution.ApprovalID` required when `material`).
- Never disable, pause or restart reconciliation to make the alert stop.
- Never "correct" against a single observer's view when the observers disagree.
- Never delete or hide the record; every lifecycle step is an immutable `reconciliation_transitions` row.

## Verification / exit criteria

- `ledger.VerifyBalances`, `positions.VerifyAgainstLedger`, `capital.VerifyReservationTotals` report zero drift (PENDING: nothing schedules them; an engineer runs them from a Go program).
- Every material record is `RESOLVED_MANUAL` with `approval_id`, `resolution_evidence_ref` and `compensating_journal_transaction_id` set; `blocks_new_risk = false`.
- `oldest_unresolved_mismatch` is below policy; `RISK_RECONCILIATION_PENDING` rejections stop.
- Audit chain verifies across the incident window.

## Post-incident

- Export the account audit stream(s) and the `admin` stream slice, the reconciliation record with its transitions, and the compensating journal transactions (with `content_hash`) into the WORM archive.
- Security event review: confirm no `cp_migrate` sessions, no break-glass use outside the approved actions.
- Update `docs/build/REQUIREMENTS_TRACEABILITY.md` R-021-*, R-050/051 with the incident as evidence; add a BLOCKERS entry if the verifiers are still unscheduled.
- If the root cause was an executor or funding defect, its regression test is a release blocker.
