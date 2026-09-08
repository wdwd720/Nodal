# Runbook: unknown transaction on a platform wallet

Severity: SEV1 (unauthorized signing candidate; unexplained financial mismatch) · Owner: SECURITY (classification), OPERATIONS (containment), FINANCE (repair) · Related: [wallet-provider-compromise.md](./wallet-provider-compromise.md), [submission-unknown.md](./submission-unknown.md), [ledger-mismatch.md](./ledger-mismatch.md), `docs/architecture/RECONCILIATION.md` §3

## Trigger

- Periodic reconciliation finds wallet activity that matches no `execution_attempts`, `deposits` or `withdrawals` row and opens a record `kind = SUBMISSION_UNKNOWN` (scope: the wallet/account), `material = true`, `blocks_new_risk = true`, SEV1 candidate alert (RECONCILIATION.md §3).
- `unknown_submissions` counter; a `WALLET_BALANCE` mismatch whose `difference` equals an unmatched on-chain transfer.
- Do not confuse with `SUBMISSION_UNKNOWN` **attempt status** (our own submission with an unknown outcome): that is [submission-unknown.md](./submission-unknown.md). This runbook is for transactions we never built.
- The reconciliation engine is built (`internal/reconciliation`, `cmd/reconciliation-worker`). PENDING: the wallet-activity scan loop (`SearchWalletActivity` exists on both observers; nothing drives it on a schedule) and the `signing_rejection`/`wallet_policy_violation` emitters. **BLOCKED_EXTERNAL:** alarms.

## Blast radius

- Inbound (airdrop, dust, external deposit, refund): no loss; the `WALLET` ledger balance is now below the chain balance until classified; `RECONCILIATION_ADJUSTMENT` may absorb dust under policy; anything larger needs manual classification.
- Outbound: value left a platform wallet without the inspector's approval. Assume signing compromise until proven otherwise ([wallet-provider-compromise.md](./wallet-provider-compromise.md)).
- The account: `blocks_new_risk` refuses new intents (`RISK_RECONCILIATION_PENDING`); open orders continue to finality; reconciliation and ledger keep running.

## Immediate actions (first 10 minutes)

1. Freeze the account (STANDARD; `kill:activate`): `POST /admin/kill-switches {"kind":"ACCOUNT_FREEZE","scope_id":"<account_id>","action":"activate","reason":"<INC-id>: unknown wallet transaction"}`; COMPLIANCE sets `POST /admin/accounts/{accountId}/status {"to":"FROZEN","reason":"..."}`.
2. Pull the record and check our own tables for the signature (read-only):
   ```sql
   SELECT id, scope_type, scope_id, account_id, asset_id, observed, difference, status, opened_at
     FROM reconciliation_records WHERE kind = 'SUBMISSION_UNKNOWN' AND status IN ('OPEN','MISMATCH','INVESTIGATING','ESCALATED');
   SELECT 'attempt' AS src, id::text FROM execution_attempts WHERE tx_signature = '<sig>'
   UNION ALL SELECT 'deposit', id::text FROM deposits WHERE tx_signature = '<sig>'
   UNION ALL SELECT 'withdrawal', id::text FROM withdrawals WHERE tx_signature = '<sig>';
   SELECT id, account_id, provider, address, status, delegation_verified_at FROM wallets WHERE address = '<wallet address>';
   ```
   A hit means the scan matched wrongly (engine defect, still investigate); no hit confirms an unknown transaction.
3. Fetch the transaction from **both** observers (`getTransaction` with `maxSupportedTransactionVersion: 0` on the fallback; the Helius adapter) and archive both raw responses. Apply the agreement policy: disagreement ⇒ [rpc-disagreement.md](./rpc-disagreement.md) in parallel.
4. Direction and signer: if the platform wallet is the **fee payer or a required signer** and value left it ⇒ outbound unauthorized ⇒ immediately run [wallet-provider-compromise.md](./wallet-provider-compromise.md) (global kill, provider disable, delegation revoke). If the wallet only received value ⇒ inbound; continue here.
5. Check for `Approve`/`SetAuthority`/`CloseAccount` instructions in the decoded transaction even for "inbound" transactions (a delegation grant can hide in a transfer); any authority change ⇒ treat as outbound compromise.
6. Announce classification so far, account, wallet, signature and switch state.

## Diagnosis

- Signing evidence: `SELECT * FROM signing_decisions WHERE wallet_id = '<wallet_id>' AND decided_at BETWEEN <slot time − 10 min> AND <slot time + 10 min>;` — an `APPROVED` decision with `inspected_tx_hash` equal to the landed transaction means our attempt row is missing (engine/executor crash, [submission-unknown.md](./submission-unknown.md) adoption path); a `REJECTED` decision followed by the transaction landing means the provider signed a rejected transaction (SEV1 provider compromise); no decision means the signer was used outside the platform.
- Provider signing logs for the wallet and time window (provider console).
- Audit: `audit_events WHERE stream = 'account:<id>' AND occurred_at BETWEEN...` for any signing request/submission action; `system` stream for worker activity.
- Inbound classification: exchange withdrawal to the customer's platform address (external-deposit), airdrop/dust (below `dust_threshold` ⇒ automatic under policy), refund from a venue program, a mistaken send by a third party (funds are held; return requires a withdrawal path that is DISABLED in V1 — a legal/compliance decision).
- Token-2022 mints or unknown programs: an inbound token with unsupported extensions must not become a tradable balance (`assets.token_extensions`, `HasUnsupportedExtensions`); it stays as an unclassified observation.

## Containment and recovery

Every unknown transaction is material; resolution needs an approved `RECONCILIATION_RESOLVE_MATERIAL` action and one of three classifications (RECONCILIATION.md §3):

1. **adopted-as-fill**: our attempt did land but the row is missing/unlinked ⇒ the engine adopts it (`ADOPTED`), inserts the fill under `UNIQUE (venue, external_fill_id)`, posts `TRADE_FILL`, releases the unused reservation remainder (PART 195: canonical financial event and audit, never a position overwrite).
2. **external-deposit**: a compensating posting credits `WALLET` against `CAPITAL` (or a holding classification per FINANCE policy) via the `compensation` body of `POST /admin/reconciliation/records/{id}/resolve` with `approval_id`; buying-power eligibility follows the funding policy, never automatic.
3. **unauthorized**: [wallet-provider-compromise.md](./wallet-provider-compromise.md) owns containment; the record is resolved with a loss posting after SECURITY sign-off.

Steps for 2 and 3: OPERATIONS/FINANCE (`reconciliation:resolve`) proposes `POST /admin/actions {"kind":"RECONCILIATION_RESOLVE_MATERIAL","target_type":"reconciliation_record","target_id":"<record_id>","params":{...},"reason":"<INC-id>: classified <kind>"}` (step-up ≤ 15 min, expires 4 h); a break-glass `reconciliation:approve` holder approves `POST /admin/actions/{id}/approve`; then the resolve call. Release `ACCOUNT_FREEZE` (`kill:release` + step-up) and run `ACCOUNT_UNFREEZE` (COMPLIANCE) only after `RESOLVED_MANUAL`.

## What NOT to do

- Never adjust `ledger_balances` to match the chain; classify, then post.
- Never sweep or return funds by signing a transaction outside the inspector path; withdrawals are a gated domain (DISABLED in V1).
- Never resolve as `external-deposit` without proof of the sender's intent when the amount is material; park it as `INVESTIGATING` instead.
- Never silence the wallet-activity scan or raise `dust_threshold` to make records stop.
- Never unfreeze before the record is resolved and, for outbound cases, before the wallet is re-delegated on fresh credentials.

## Verification / exit criteria

- Record `RESOLVED_MANUAL` with classification, `approval_id`, `resolution_evidence_ref` (both raw observations, decode, signing-decision query output) and `compensating_journal_transaction_id` where a posting was made.
- `WALLET` ledger balance equals the agreed on-chain balance for the (wallet, asset) on the next `FULL` reconciliation; `blocks_new_risk = false`.
- For outbound cases, [wallet-provider-compromise.md](./wallet-provider-compromise.md) exit criteria met.
- Audit chain verifies for the account stream.

## Post-incident

- Archive the observations, decode, provider logs, the record and its transitions, and the posting.
- Security review: THREAT_MODEL.md §3.6 rows; if the transaction was unauthorized, residual risk #1 is re-scored and EB-002/EB-005 updated.
- Update `docs/build/REQUIREMENTS_TRACEABILITY.md` R-050-1, R-195-1; add `test/integration/missing_internal_txn_repair_test.go` to BLOCKERS if absent.
