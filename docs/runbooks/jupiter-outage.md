# Runbook: Jupiter (execution provider / swap router) outage

Severity: SEV2 (provider outage) · Owner: OPERATIONS · Related: [submission-unknown.md](./submission-unknown.md), [rpc-disagreement.md](./rpc-disagreement.md), [duplicated-trade-suspicion.md](./duplicated-trade-suspicion.md), `docs/api/providers/jupiter.md`, `docs/architecture/EXECUTION.md` §4, §8

## Trigger

- Execution provider health `UNHEALTHY`/`DEGRADED` (role `EXECUTION`; error rate, p95, staleness thresholds in `provider.Thresholds`); `/admin/providers` or `provider_health_samples`.
- `execution_failure_rate_failures / execution_failure_rate_attempts` above threshold; `quote_latency`, `build_latency`, `submit_latency` p95 breaches; `unknown_submission_rate` rising (timeouts on `/execute`).
- `RISK_PROVIDER_HEALTH`/`RISK_QUOTE_AGE` rejections rising; settlement compiler returning `NO_VALID_PLAN`/`PROVIDER_DEGRADED`.
- Jupiter status/announcements: `api.jup.ag/swap/v2` errors, `x-api-key` rejection (key rotated or quota), a host or version change (`/swap/v1` is unmaintained; the platform pins hosts in configuration).
- **BLOCKED_EXTERNAL:** alarms, and a funded account on a real venue. The client and fake (`internal/provider/jupiter`) and the `test/contract/jupiter` fixtures exist. **PENDING `bindProviders`**, which returns an error in every production build, so the execution worker never starts. Also PENDING: the health tracker feeding `RISK_PROVIDER_HEALTH` end to end.

## Blast radius

- New submissions stop: an `UNHEALTHY` execution provider blocks new submissions by design (PART 79, 107); the kill switch `PROVIDER_DISABLE_NEW_ACTIONS(jupiter)` makes it explicit and sticky.
- Never stopped: `Status(signature)` reads, `Reconcile` endpoints, chain observation, reconciliation, fill posting, settlement of already-landed swaps. "A provider disable cannot make existing transactions invisible" (PART 107).
- In-flight attempts at the moment of failure: `/execute` returns "accepted", not "landed"; a timeout ⇒ `SUBMISSION_UNKNOWN` and the PART 48 recovery ([submission-unknown.md](./submission-unknown.md)). Reservations stay locked to their orders.
- Customers: manual quotes fail with a problem code; agents skip runs (`RISK_PROVIDER_HEALTH`); positions and balances are unaffected.

## Immediate actions (first 10 minutes)

1. Make the stop explicit and sticky (the tracker's hysteresis could otherwise flap during a partial outage): `POST /admin/kill-switches {"kind":"PROVIDER_DISABLE_NEW_ACTIONS","scope_id":"jupiter","action":"activate","reason":"<INC-id>: execution provider outage"}` (SEVERE; `kill:activate`). If only one venue route is broken, `VENUE_DISABLE(<venue>)` (STANDARD) is narrower.
2. Count what is in flight and leave it alone (read-only):
   ```sql
   SELECT status, count(*), min(submitted_at) FROM execution_attempts
    WHERE provider = 'jupiter' AND status IN ('SUBMITTING','SUBMITTED','SUBMISSION_UNKNOWN','OBSERVED','CONFIRMED') GROUP BY status;
   SELECT status, count(*) FROM orders WHERE status IN ('SUBMITTING','SUBMITTED','SUBMISSION_UNKNOWN','RECONCILIATION_REQUIRED') GROUP BY status;
   ```
3. Confirm observation is healthy (Helius and fallback RPC): the recovery for `SUBMISSION_UNKNOWN` depends on the chain observers, not on Jupiter ([helius-outage.md](./helius-outage.md) if not).
4. Check whether it is an outage or a contract change: `401/403` on `x-api-key` ⇒ key/quota ([secret-exposure.md](./secret-exposure.md) if the key leaked); schema errors (`inAmount`/`outAmount` strings, `lastValidBlockHeight` string vs number, `priceImpactPct` deprecation) ⇒ provider drift, the adapter fails closed (`validate.go`) and must not be "fixed" live.
5. Announce: "execution provider disabled since <time>; no new submissions; in-flight attempts recovering via chain observation; balances unaffected".

## Diagnosis

- Health samples: `SELECT state, error_rate_bps, p99_latency_ms, staleness_ms, reason_codes, evaluated_at FROM provider_health_samples WHERE provider = 'jupiter' ORDER BY evaluated_at DESC LIMIT 20;`.
- Failure class per attempt: `execution_attempts.error` and `submit_response_ref` (archived raw response with hash; `raw_response_ref` evidence per EXECUTION.md §8). Definitive rejection (provider says invalid/expired **and** signature not on chain) ⇒ `FAILED` and reservation released; transport timeout/ambiguous ⇒ `SUBMISSION_UNKNOWN`.
- Quote path vs execute path: `/order` (quote+build) failing is harmless (no side effect); `/execute` timeouts are `UNKNOWN_EFFECT_WRITE` and never retried blindly (`provider.RetryClass.MayRetryBlindly = false`).
- Blockhash expiry: attempts whose `last_valid_block_height` is below the current height (both observers) are proven absent and can expire; do not rebuild until the provider is healthy again.
- Simulation: if `simulation_ok = false` spikes, the route provider may be returning transactions the inspector rejects (`INSPECTION_REJECTED` statuses) — that is a [wallet-provider-compromise.md](./wallet-provider-compromise.md)-adjacent signal (route provider tampering, THREAT_MODEL.md §3.5) rather than an outage.

## Containment and recovery

1. Leave the executor/recoverer to resolve `SUBMISSION_UNKNOWN` attempts per PART 48 (**PENDING `bindProviders`**: the recoverer cannot run until it exists, so an engineer follows [submission-unknown.md](./submission-unknown.md) with observer reads and files evidence).
2. Orders whose attempts expired and whose intent deadline has not passed go back to `PLANNED`; they get a new attempt with a fresh blockhash **only after** the provider is healthy, the switch is released, and a fresh risk `FINAL` decision against a fresh quote (EXECUTION.md §4 step 7). Orders past their deadline become `FAILED_FINAL` and their reservations release.
3. When Jupiter recovers: watch the tracker return to `HEALTHY` through hysteresis; verify a quote round-trip in a CANARY account with the fake disabled; then release `PROVIDER_DISABLE_NEW_ACTIONS(jupiter)` via the dual-controlled `KILL_SWITCH_RELEASE` path ([global-kill-and-reenable.md](./global-kill-and-reenable.md), same steps with the provider kind/scope), attaching the provider incident and the canary evidence.
4. If the outage is a breaking API change: adapter change + contract fixtures updated from the official docs (`docs/api/providers/jupiter.md` re-verified with fetch dates) + `make contract` green before release; the verification label must not exceed what was tested.

## What NOT to do

- Never re-submit signed bytes or rebuild an attempt while the previous one is `SUBMISSION_UNKNOWN` (PART 48 rule 2).
- Never retry `/execute` blindly; never treat "accepted"/`tx.jup.ag` acknowledgement as landed.
- Never release the reservation of an order with a non-terminal attempt.
- Never switch to the unmaintained `/swap/v1` host, a third-party proxy, or hand-built routes to keep trading.
- Never relax slippage/fee/compute limits in policy to make failing routes pass; `RISK_SLIPPAGE`/`RISK_FEE` exist to refuse exactly that.

## Verification / exit criteria

- No attempts in `SUBMITTING`/`SUBMISSION_UNKNOWN` older than one blockhash validity window (~60–90 s) plus margin without a reconciliation record; every order from the window is terminal or back to `PLANNED` legitimately.
- Reservations for terminal orders released (`asset_reservations.status IN ('CONSUMED','RELEASED')`, `locked_by_order_id` cleared).
- Provider `HEALTHY` for the hysteresis window; canary quote/submit succeeded and reconciled `AGREED`.
- Switch released through the approval path; `execution_failure_rate` back to baseline.

## Post-incident

- Archive health samples, raw provider responses (`submit_response_ref`), the recovery outcome per attempt (adopted/expired/failed) and the canary evidence.
- Update `docs/api/providers/jupiter.md` verification record if any endpoint behaviour changed; update `docs/build/REQUIREMENTS_TRACEABILITY.md` R-043-1, R-048-1, R-107-1; add `test/chaos/submit_timeout_test.go` to BLOCKERS if absent.
