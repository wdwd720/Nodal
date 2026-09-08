# Runbook: RPC / observer disagreement (PART 196)

Severity: SEV2 (chain-data disagreement); SEV1 if a material, unexplained financial mismatch results · Owner: OPERATIONS (RISK for policy) · Related: [helius-outage.md](./helius-outage.md), [submission-unknown.md](./submission-unknown.md), [ledger-mismatch.md](./ledger-mismatch.md), `docs/architecture/EXECUTION.md` §6, `internal/chain/agreement.go`

## Trigger

- `chain.Resolve` returns `State = DISAGREED` (`BlockDependent = true`): observers differ on found-ness or on any economic field (`Differences` lists them, sorted); a lagging secondary is a disagreement, not a tie-break.
- Reconciliation opens `kind = EXECUTION` or `WALLET_BALANCE` records with both observations in `observed` and `blocks_new_risk = true` (RECONCILIATION.md §3 step 2); orders move to `RECONCILIATION_REQUIRED`.
- `reconciliation_mismatches` counter rising with a chain-observation cause; `wallet_balance_observations` for one (wallet, asset) differing by `source` at the same slot.
- Helius says `CONFIRMED`/`FINALIZED` while the fallback RPC says not found (the PART 196 case), or vice versa.
- The reconciliation engine that records these is built (`internal/reconciliation`). **BLOCKED_EXTERNAL:** alarms. Implemented: `chain.AgreementPolicy`, `MultiObserver` (`internal/chain`), both observer adapters.

## Blast radius

- The dependent activity only: the affected order/attempt cannot advance (no fill posting until agreement or manual classification), the account's `blocks_new_risk` refuses new intents with `RISK_RECONCILIATION_PENDING`, and a deposit whose receipt is disputed stays below `SETTLEMENT_OBSERVED`.
- Nothing platform-wide unless disagreements are systemic (one observer returning wrong data broadly), in which case that observer is the incident.
- Keeps running: observation on both sides, reconciliation, ledger posting for agreed fills, settlement of unaffected orders.

## Immediate actions (first 10 minutes)

1. Do nothing to the disputed orders. The block is the control. Read-only:
   ```sql
   SELECT id, kind, scope_type, scope_id, account_id, status, material, blocks_new_risk, opened_at,
          observed -> 'primary' AS primary_obs, observed -> 'secondary' AS secondary_obs, difference
     FROM reconciliation_records WHERE status IN ('MISMATCH','INVESTIGATING') AND kind IN ('EXECUTION','WALLET_BALANCE')
     ORDER BY opened_at;
   SELECT id, order_id, status, tx_signature, last_valid_block_height, submitted_at FROM execution_attempts
    WHERE order_id IN (SELECT id FROM orders WHERE status = 'RECONCILIATION_REQUIRED');
   ```
2. Determine whether it is one observer being wrong at scale: count `DISAGREED` resolutions per hour and which side is the odd one out (`Differences`, `Primary`/`Secondary` in the record). If one observer is systematically wrong: `POST /admin/kill-switches {"kind":"PROVIDER_DISABLE_NEW_ACTIONS","scope_id":"<observer>","action":"activate","reason":"<INC-id>: returning inconsistent chain data"}` (SEVERE; `kill:activate`) and set its health `DISABLED` (`provider:disable`), so the agreement policy treats it as unavailable (single-observer mode, capped at `CONFIRMED`) instead of poisoning resolutions. Reads from it continue for evidence.
3. If both observers are healthy and disagree on a *single* signature, it is usually propagation lag or a reorg: wait one finality window before any manual step. `getBlockHeight` on both; a secondary behind the primary by more than the margin is lag.
4. If the disagreement is on wallet balances broadly (not a single transaction), check for a reorg or an RPC node serving a stale snapshot; compare `slot` in `wallet_balance_observations` per source.
5. Announce the affected orders/accounts and which observer is suspect.

## Diagnosis

- Policy semantics to apply (`chain.DefaultPolicy`): `RequireBothFor = FINALIZED`; on a commitment-level conflict with identical economics, report the chain RPC's observation at the **lower** level (`PreferChainRPC`, never raises finality); on any economic difference, `DISAGREED`; single-observer answers cap at `CONFIRMED`; `NOT_FOUND` on both with height > `last_valid_block_height` + margin is proven absence.
- Pull raw evidence from both: `getTransaction(sig, maxSupportedTransactionVersion: 0)` on the fallback; the Helius adapter's observation; store both under `raw_ref`. Decode token deltas for the wallet independently.
- Reorg: a `CONFIRMED` transaction later absent from a finalized block ⇒ `MISMATCH` and compensating postings under the standard flow (EXECUTION.md §5); confirm with `getSignatureStatuses` at `finalized` commitment.
- Token-2022 or ALT decoding differences between vendors can look like economic disagreement; check `assets.token_extensions` and whether one observer failed to resolve address-lookup-table accounts (`docs/api/providers/solana-rpc.md`).
- Duplicate or partial observation: one observer reporting inner-instruction transfers the other omits changes `venue_fee`/`network_fee` fields; the fill economics must come from the agreed observation only.

## Containment and recovery

1. Lag or transient: the next reconciliation pass resolves `AGREED`; the engine upgrades finality without economic change (`RESOLVED_AUTOMATIC{finality upgrade}`) and the block lifts. No operator action.
2. Genuine economic disagreement that persists: OPERATIONS moves the record to `INVESTIGATING`, obtains a third independent read (another RPC endpoint or an explorer, evidence only), and proposes `RECONCILIATION_RESOLVE_MATERIAL` with the classification and both observations as evidence; a break-glass `reconciliation:approve` holder approves; resolve via `POST /admin/reconciliation/records/{id}/resolve` with `approval_id` and, if a fill or compensation must be posted, the `compensation` body ([ledger-mismatch.md](./ledger-mismatch.md) steps 2–4). The fill is inserted from the agreed/decided observation under `UNIQUE (venue, external_fill_id)`.
3. Orders in `RECONCILIATION_REQUIRED` return to the normal path only through the engine after resolution (adopt the landed transaction or expire the attempt); never by editing `orders.status`.
4. A disabled observer is re-enabled (`provider:enable`) and its `PROVIDER_DISABLE_NEW_ACTIONS` released via the dual-controlled path after the vendor explains the inconsistency and a sample of resolutions agree again.

## What NOT to do

- Never pick the optimistic observation (the one that says "landed" or "more output") to unblock a customer (PART 196).
- Never lower `RequireBothFor`, widen the proven-absent margin, or set `PreferPrimary` during an incident.
- Never post a fill from one observer's data while the other disagrees.
- Never build a new attempt for an order whose previous attempt is disputed (PART 48 step 8: no new attempt under uncertainty).
- Never disable both observers or reconciliation to "stop the noise".

## Verification / exit criteria

- No `DISAGREED` resolutions in the last full periodic cycle; records from the window `MATCHED`, `RESOLVED_AUTOMATIC` or `RESOLVED_MANUAL` with approval and evidence.
- No order in `RECONCILIATION_REQUIRED`; `blocks_new_risk = false` for affected accounts; `RISK_RECONCILIATION_PENDING` rejections stop.
- Both observers `HEALTHY` and independent; any disabled observer re-enabled with the vendor's explanation on file.
- Ledger invariants zero drift for affected accounts.

## Post-incident

- Archive both raw observations per disputed signature, the resolution records and the vendor explanation.
- If the cause was a decoding difference, the fix and a golden fixture are a release blocker; update `docs/api/providers/{helius,solana-rpc}.md` verification records.
- Update `docs/build/REQUIREMENTS_TRACEABILITY.md` R-196-1 (record `TestAgreementPolicy_Matrix`, `TestMultiObserver_LaggingSecondaryIsDisagreement`, `TestProp_NeverAgreedWhenAnyFieldDiffers` as passing if they are) and add `test/chaos/rpc_disagreement_test.go` to BLOCKERS if absent.
