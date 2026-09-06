# Runbook: wallet / signing provider compromise

Severity: SEV1 (signing credential compromise; unauthorized signing) · Owner: SECURITY (OPERATIONS for containment, FINANCE for repair) · Related: [unknown-transaction.md](./unknown-transaction.md), [secret-exposure.md](./secret-exposure.md), [global-kill-and-reenable.md](./global-kill-and-reenable.md), [ledger-mismatch.md](./ledger-mismatch.md), `docs/api/providers/privy.md`, `docs/architecture/EXECUTION.md` §2–3

## Trigger

- An outbound transaction from a platform-controlled wallet that no `execution_attempts` row built (reconciliation `SUBMISSION_UNKNOWN` record classified `unauthorized`; [unknown-transaction.md](./unknown-transaction.md) step 5).
- `signing_rejection` / `wallet_policy_violation` CRITICAL security events in a burst, or a `signing_decisions` row `APPROVED` whose `inspected_tx_hash` does not match the transaction that landed.
- Provider breach notice, `provider_credential_change` outside a rotation ticket, provider dashboard showing signing requests not originating from `cmd/execution-worker`.
- A fill outside `MinOutputQuantity`/`MaxInputDebit` (RECONCILIATION.md §3: signing-boundary failure candidate).
- PENDING: emitters for `signing_rejection`, `wallet_policy_violation`, `provider_credential_change`; alarms. PENDING: `signing.Service` and the provider adapter (`internal/wallet` holds the `WalletProvider`/`SigningProvider` contracts and a fake; `internal/signing/inspect` is the pure inspector; no `internal/provider/privy`).

## Blast radius

- Every wallet delegated to the compromised provider credential: value can be moved without the platform's inspector ever seeing the transaction (the provider policy engine is defence in depth only; it cannot resolve address-lookup-table accounts, `docs/api/providers/privy.md`).
- The platform's own signing path is the bounded credential held only by `cmd/execution-worker`; if *that* credential leaked, see [secret-exposure.md](./secret-exposure.md) as well.
- Keeps running: observation, reconciliation, ledger posting, settlement of already-landed fills. Wallet-activity scanning is what detects the theft; never stop it.
- Customer funds on chain are irreversible once moved; containment is about stopping further signing and recording truth.

## Immediate actions (first 10 minutes)

1. Stop all new signing platform-wide: activate `GLOBAL_NEW_RISK_KILL` ([global-kill-and-reenable.md](./global-kill-and-reenable.md)) **and** `PROVIDER_DISABLE_NEW_ACTIONS(<wallet provider name>)` (SEVERE; `kill:activate`): `POST /admin/kill-switches {"kind":"PROVIDER_DISABLE_NEW_ACTIONS","scope_id":"<provider>","action":"activate","reason":"<INC-id>: wallet provider compromise suspected"}` (PENDING `cmd/api`). The provider switch stops new submissions but never `Status`, `Reconcile` or observation (PART 107).
2. Revoke the delegation at the provider for every affected wallet (provider console or API; `WalletProvider.VerifyDelegation` will then report it): this is the only action that stops an attacker who already holds the provider-side credential. Record the provider ticket id.
3. Rotate the platform's provider credentials via the `SecretRef` indirection (`aws-sm://` targets; PENDING resolver) and restart `cmd/execution-worker` (PENDING binary). Never edit images or commit secrets.
4. Mark affected wallets `SUSPENDED` through the wallet service (writes `wallet_status_transitions`; direct SQL fails `AU001`). Read-only check:
   ```sql
   SELECT id, account_id, provider, provider_wallet_id, chain, address, status, delegation_verified_at, signing_policy_version
     FROM wallets WHERE provider = '<provider>' ORDER BY status;
   ```
5. Freeze every account whose wallet shows unexplained outflow (`ACCOUNT_FREEZE(account_id)` + COMPLIANCE `POST /admin/accounts/{id}/status {"to":"FROZEN",...}`), so the reconciliation block and the account status agree.
6. Declare SEV1; page SECURITY lead, FINANCE, and the provider's incident contact.

## Diagnosis

- Enumerate unauthorized activity per wallet: chain observer `SearchWalletActivity(wallet, since, limit)` on **both** observers (Helius adapter `internal/provider/helius`, fallback `internal/provider/solanarpc`), compared against `execution_attempts.tx_signature`, `deposits.tx_signature`, `withdrawals.tx_signature`. Everything unmatched is an unknown transaction record (PENDING: reconciliation engine; until then an engineer runs the observers and files the list as evidence).
- Signing evidence: `SELECT attempt_id, decision, reason_codes, inspector_version, requested_by_service, provider_sign_ref, decided_at FROM signing_decisions WHERE wallet_id IN (...) AND decided_at > '<window start>';` — an unauthorized transaction has **no** `APPROVED` row; a transaction that has one but differs from `inspected_tx_hash` means the provider signed something else (provider-side compromise).
- Provider logs: signing requests by API key / idempotency key / source IP; compare with `cmd/execution-worker` egress.
- Decode each unauthorized transaction (`getTransaction` with `maxSupportedTransactionVersion: 0`): fee payer, signers, destinations, `Approve`/`SetAuthority` instructions (a delegation grant is worse than a transfer: revoke it on chain via the provider if possible).
- Was the platform credential or the provider itself compromised? Platform credential ⇒ requests carry our API key from foreign IPs; provider ⇒ signatures with no request at all.

## Containment and recovery

1. Every unauthorized transaction becomes a material reconciliation record resolved as `unauthorized` under `RECONCILIATION_RESOLVE_MATERIAL` (OPERATIONS/FINANCE propose, break-glass `reconciliation:approve` approves), with a compensating posting that moves the lost quantity from `WALLET` to a loss/`PLATFORM_ADJUSTMENT` classification per FINANCE policy — the customer's entitlement decision is a business/legal decision recorded as a separate `LEDGER_CORRECTION`. Balances are never edited (PART 129, 195).
2. Re-establish wallets only with a **new** delegation on a fresh provider credential; `wallets.delegation_verified_at` must be re-set by `VerifyDelegation`; production startup fails closed until the signing capability is `VERIFIED` (PART 96).
3. Release `PROVIDER_DISABLE_NEW_ACTIONS` and `GLOBAL_NEW_RISK_KILL` only through the dual-controlled `KILL_SWITCH_RELEASE` path with evidence: provider incident report, credential rotation log, list of resolved records, audit verification.
4. If the provider cannot demonstrate containment, keep the switch active and suspend `LIVE_MANUAL_TRADING`/`LIVE_AGENT_TRADING` gates (`POST /admin/gates/{capability}/suspend`, `kill:activate`); re-activation is a fresh dual-controlled approval with updated provider contract evidence (PRODUCTION_GATES.md §7).

## What NOT to do

- Never "rescue" funds by signing a sweep transaction outside the inspector path, even from an operator seat; there is no blind-signing path and none may be created during an incident (ADR-0012).
- Never disable wallet-activity reconciliation or the fallback observer to reduce noise.
- Never accept the provider's "no unauthorized signing occurred" without your own chain scan on two observers.
- Never edit `wallets.status`, `signing_decisions` or ledger rows directly.
- Never rotate a secret by committing a new value or baking it into an image (SECURITY.md §14).

## Verification / exit criteria

- Zero wallet activity on any platform wallet without a matching attempt/deposit/withdrawal over the last full reconciliation cycle on both observers.
- All affected wallets `REVOKED` or re-delegated with fresh `delegation_verified_at`; new credential in place; old credential confirmed revoked at the provider.
- Every unknown transaction record `RESOLVED_MANUAL` with `approval_id`, evidence and compensating posting; `blocks_new_risk = false`.
- Audit chain verifies; `make verify-audit` (PENDING) output archived.
- Switches released via the approval path; gates in their intended state.

## Post-incident

- Archive: chain scans (raw observations, `raw_ref`), provider incident report, signing decisions, credential rotation log, reconciliation records and postings, audit exports.
- Security review: THREAT_MODEL.md §3.5 "wallet / signing provider" row moves from designed to whatever the evidence shows; re-score residual risk #1.
- Update `docs/build/BLOCKERS.md` EB-002/EB-005 and `docs/build/REQUIREMENTS_TRACEABILITY.md` R-095-1, R-096-1; file `test/security/agent_raw_sign_attempt_test.go` and the fuzz/golden inspector tests as blockers if absent.
