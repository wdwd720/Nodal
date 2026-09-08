# Runbook: funding provider compromise (fiat-to-USDC onramp)

Severity: SEV1 when a credit path is affected (forged or replayed events reaching the ledger); SEV2 for provider outage or suspicious-but-contained events · Owner: SECURITY (compromise), OPERATIONS (outage), FINANCE (repair) · Related: [chargeback-reversal.md](./chargeback-reversal.md), [secret-exposure.md](./secret-exposure.md), [temporal-outage.md](./temporal-outage.md), `docs/api/providers/stripe-crypto-onramp.md`

## Trigger

- `webhook_signature_failed` bursts. `internal/webhook` writes one `security_events` row at severity HIGH for every event whose signature does not verify:

  ```sql
  SELECT date_trunc('minute', occurred_at) AS minute, count(*), detail->>'reason'
    FROM security_events
   WHERE kind = 'webhook_signature_failed' AND detail->>'provider' = '<provider>'
     AND occurred_at > now - interval '6 hours'
   GROUP BY 1, 3 ORDER BY 1 DESC;
  ```

  This line used to say the emitter was PENDING and to look for
  `provider_events.signature_verified = false` rows instead. That query returns
  nothing however bad the incident is: an event whose signature fails is
  rejected *before* any `provider_events` row is written, so the column is
  `true` on every row that exists. A detection query that cannot fire is worse
  than no query at all, because it answers (F-55).
- Replayed `provider_event_id`s (`provider_duplicate_events` rising).
- A deposit that reached `AVAILABLE` without a chain receipt (`deposits.tx_signature IS NULL` or `observed_quantity IS NULL` while `status = 'AVAILABLE'`) — impossible by design (RECONCILIATION.md §5) and therefore a SEV1 signal.
- Provider breach notice; webhook secret or API key rotation not initiated by us (`provider_credential_change`, PENDING emitter -- nothing writes this kind today); provider dashboard sessions with unknown origin.
- Provider outage: onramp sessions failing to create, webhooks stopped, status polls erroring; provider health `UNHEALTHY` in `/admin/providers` / `provider_health_samples` role `FUNDING`.
- BLOCKED_EXTERNAL: alarms. Implemented: Stripe client and webhook verifier (`internal/provider/stripe`, `internal/webhook`), `test/contract/stripe`, funding state machine and reversal (`internal/funding`).

## Blast radius

- Credit path: money becomes buying power only after `PROVIDER_CONFIRMED → SETTLEMENT_OBSERVED (chain receipt, both observers agree) → RECONCILED (provider status and chain agree on quantity, FUNDING_SETTLED posted) → AVAILABLE`. A forged webhook can at most move a deposit to `PROVIDER_CONFIRMED` and open a `FUNDING` mismatch after `settlement_timeout`; it cannot credit the ledger. If it did, the state machine is broken: SEV1.
- New sessions: `FUNDING_DISABLE(*)` stops new provider sessions (`NEW_RISK` class). In-flight deposits continue to be observed and reconciled.
- Customer data: the provider owns payment/KYC data (PART 162); the platform holds no card fields (PART 191), so a provider breach exposes provider-side PII, not platform ledger data.
- Keeps running: trading on already-`AVAILABLE` balances, reconciliation, ledger, reversals ([chargeback-reversal.md](./chargeback-reversal.md)).

## Immediate actions (first 10 minutes)

1. Stop new funding sessions: `POST /admin/kill-switches {"kind":"FUNDING_DISABLE","scope_id":"*","action":"activate","reason":"<INC-id>: funding provider compromise/outage"}` (SEVERE; `kill:activate`). Also `PROVIDER_DISABLE_NEW_ACTIONS(<funding provider>)` if you need to stop status polling writes but keep observation (it never blocks reads).
2. Rotate the webhook signing secret and API key through the `SecretRef` indirection (BLOCKED_EXTERNAL `aws-sm://` resolver); the verifier fails closed on an unknown secret, so rotate at the provider **first**, then the platform, and expect a short window of `signature_verified = false` rows which are safe (they are ignored, never processed).
3. Read the webhook evidence (read-only):
   ```sql
   SELECT provider, event_type, signature_verified, verification_error, processing_status, count(*), min(received_at), max(received_at)
     FROM provider_events WHERE received_at > now - interval '24 hours' GROUP BY 1,2,3,4,5 ORDER BY 6 DESC;
   SELECT id, account_id, status, expected_quantity, observed_quantity, tx_signature, provider_confirmed_at, settlement_observed_at, reconciled_at, available_at
     FROM deposits WHERE updated_at > now - interval '24 hours' ORDER BY updated_at DESC;
   ```
4. Assert the invariant: `SELECT count(*) FROM deposits WHERE status IN ('RECONCILED','AVAILABLE') AND (tx_signature IS NULL OR observed_quantity IS NULL OR journal_transaction_id IS NULL);` must be 0. If not: `GLOBAL_NEW_RISK_KILL` ([global-kill-and-reenable.md](./global-kill-and-reenable.md)) and freeze the affected accounts; this is SEV1.
5. Check whether reversals are also arriving (a compromise often shows as disputes later): [chargeback-reversal.md](./chargeback-reversal.md).
6. Announce switch state, whether any credit path was affected, and the provider ticket id.

## Diagnosis

- Signature verification: the handler verifies before persisting (`internal/webhook`, `internal/provider/stripe/webhook.go`); `provider_events.raw_ref` holds the raw request in the archive for forensic comparison; replay is stopped by `UNIQUE (provider, provider_event_id)` and the inbox PK.
- Ordering: provider events are not ordered (`docs/api/providers/stripe-crypto-onramp.md`); the state machine only advances on chain receipt, so out-of-order or forged status events cannot credit. The event type is `crypto.onramp_session.updated`; anything else is ignored.
- Chain receipt: `deposits.tx_signature` and `observed_quantity` come from both observers under the agreement policy; a "success" without receipt after `settlement_timeout` opens a `FUNDING` mismatch (RECONCILIATION.md §5). List them: `SELECT * FROM reconciliation_records WHERE kind = 'FUNDING' AND status IN ('MISMATCH','INVESTIGATING','ESCALATED');` (PENDING engine).
- Provider status polling (`SAFE_RETRY` reads) continues under any switch; compare provider status with internal status per deposit.
- Outage vs compromise: outage shows as transport errors/timeouts in the health tracker (`provider_health_samples`, role `FUNDING`); compromise shows as *valid-looking* events that fail verification or contradict the chain.

## Containment and recovery

1. Compromise: keep `FUNDING_DISABLE` until the provider confirms containment, secrets are rotated on both sides, and `signature_verified = true` events resume with matching chain receipts. Any deposit credited without receipt (should be none) is reversed with `funding.Service.Reverse` semantics under a dual-controlled `LEDGER_CORRECTION` and the account frozen ([chargeback-reversal.md](./chargeback-reversal.md)).
2. Outage: nothing financial to repair. Deposits stuck between `PROVIDER_CONFIRMED` and `AVAILABLE` resume when webhooks/polls return; if webhooks were lost (Stripe does not guarantee delivery order; Helius-style "permanently lost" semantics apply to some providers), the periodic reconciliation compares provider status vs internal status and advances on chain receipt (PENDING: engine).
3. Release `FUNDING_DISABLE` via the dual-controlled `KILL_SWITCH_RELEASE` path (`admin_actions` proposed by `kill:activate`, approved by break-glass `kill:release`, then `POST /admin/kill-switches {... "action":"release","approval_id":...}`), attaching the provider incident report and the rotation log.
4. If the provider's terms or liability allocation changed (EB-003, EB-004), the `LIVE_FUNDING` gate needs a new approval version with updated `provider_contract_ref` before any release.

## What NOT to do

- Never process an event whose signature failed, "because we can see it in the dashboard"; use the provider's status API (a `SAFE_RETRY` read) instead.
- Never credit a deposit from a provider status alone; a redirect or a `success` webhook never credits money (PART 162).
- Never loosen `settlement_timeout` or the quantity tolerance to make mismatches disappear.
- Never disable the webhook verifier or the inbox dedup to "catch up".
- Never rotate secrets by editing images or committing values.

## Verification / exit criteria

- No `signature_verified = false` events after rotation except the expected rotation window; no duplicate `provider_event_id` processed twice (`provider_duplicate_events` flat).
- Invariant query in Immediate actions step 4 returns 0; every `AVAILABLE` deposit has one `FUNDING_SETTLED` posting.
- No open `FUNDING` reconciliation record older than `settlement_timeout` without an owner.
- `FUNDING_DISABLE` released through the approval path; provider health `HEALTHY`.

## Post-incident

- Archive raw webhook payloads (`raw_ref`), verification errors, provider incident report, rotation log, reconciliation records.
- Security review: THREAT_MODEL.md §3.5 "funding provider" and F3/F4; add `test/security/webhook_forgery_test.go` and `TestProp_DuplicateWebhookOneEffect` to BLOCKERS if absent.
- Update `docs/build/REQUIREMENTS_TRACEABILITY.md` R-162-1, R-053-10; re-run the provider verification checklist in `docs/api/providers/README.md`.
