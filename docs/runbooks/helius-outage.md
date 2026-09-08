# Runbook: Helius (primary chain observer / Solana data provider) outage

Severity: SEV2 (provider outage) · Owner: OPERATIONS · Related: [rpc-disagreement.md](./rpc-disagreement.md), [stale-market-data.md](./stale-market-data.md), [submission-unknown.md](./submission-unknown.md), `docs/api/providers/helius.md`, `docs/architecture/EXECUTION.md` §6

## Trigger

- Provider health for the `OBSERVATION`/`DATA` role reaches `UNHEALTHY` (error rate ≥ 50 % of calls in the window, p95 ≥ 8 s, or no successful sample for 30 s — `provider.DefaultThresholds`; production values are configuration) or `DEGRADED` for a sustained period.
- `ingest_checkpoints.status = 'DISCONNECTED'` for the Helius wallet/program event stream; `stream_gaps` rows of kind `RECONNECT`/`SILENCE` (`cmd/market-ingest-worker`).
- `confirmation_latency`/`finality_latency` histograms stretching; `data_freshness` breaching strategy `max_age_ms`; `RISK_STALE_DATA` rejections rising.
- Helius status page / rate-limit (`429`) or plan-gating errors; webhook deliveries stopping (they retry only 3× at 1 s and are then permanently lost, so they are a hint, never truth).
- BLOCKED_EXTERNAL: alarms; the health tracker (`internal/provider/health.go`), the Helius client (`internal/provider/helius`) and the fallback RPC (`internal/provider/solanarpc`) exist.

## Blast radius

- Observation degrades to single-observer mode: with Helius `UNHEALTHY` the agreement policy (`chain.DefaultPolicy`: `RequireBothFor = FINALIZED`) caps what the fallback alone may assert at `CONFIRMED` (`PrimaryOnly`/`SecondaryOnly` resolutions are `Degraded = true`). Consequences by finality policy (EXECUTION.md §5): UI provisional (`OBSERVED`) and position/ledger posting (`CONFIRMED`) continue on the fallback; **funding availability and withdrawals (`FINALIZED`) cannot be granted** until both observers agree again.
- Execution: new submissions are not blocked by an observation outage by itself (the execution provider is Jupiter), but the risk kernel consumes provider health (`RISK_PROVIDER_HEALTH`) and settlement marks `PROVIDER_DEGRADED`; the executor must not finalize on an unhealthy observer (PART 79: "do not falsely finalize").
- Market data streamed from Helius stops → strategies skip with `STALE_DATA` ([stale-market-data.md](./stale-market-data.md)).
- Keeps running: reconciliation (on the fallback observer), fill posting at `CONFIRMED`, ledger, settlement. Helius being `DISABLED` by an operator stops reads from *it* only; `Observe` class work is never blocked by any switch.

## Immediate actions (first 10 minutes)

1. Confirm the fallback is healthy and independent: `GET /admin/providers` or
   ```sql
   SELECT provider, role, state, error_rate_bps, p99_latency_ms, staleness_ms, reason_codes, evaluated_at
     FROM provider_health_samples WHERE evaluated_at > now - interval '10 minutes' ORDER BY evaluated_at DESC LIMIT 20;
   ```
   If the fallback RPC is also unhealthy, treat as a chain-observation blackout: activate `CHAIN_DISABLE_NEW_ACTIONS(solana)` (SEVERE; `kill:activate`) so no new submission is made blind; observation keeps being attempted.
2. Do **not** activate `PROVIDER_DISABLE_NEW_ACTIONS(helius)` for a plain outage: the health tracker already stops relying on it and hysteresis (`RecoverStreak`) brings it back safely. Activate it only if Helius returns *wrong* data (see [rpc-disagreement.md](./rpc-disagreement.md)); an operator `DISABLED` state is not lifted by observation (`TestTracker_DisableOverridesObservation`).
3. Check in-flight attempts that need finality: `SELECT status, finality, count(*) FROM execution_attempts WHERE status IN ('SUBMITTED','SUBMISSION_UNKNOWN','OBSERVED','CONFIRMED') GROUP BY 1,2;` — they will sit at `CONFIRMED` until both observers agree; that is correct.
4. Check deposits waiting for `FINALIZED`: `SELECT count(*) FROM deposits WHERE status IN ('SETTLEMENT_OBSERVED','RECONCILED');` — they will not become `AVAILABLE` during the outage; do not force them.
5. Announce: "primary observer degraded/unhealthy since <time>; fallback RPC serving; finality-dependent steps (funding availability) paused; trading continues at CONFIRMED-level observation".

## Diagnosis

- Which Helius surface failed: RPC (`getTransaction`, `getSignatureStatuses`, `getBlockHeight`), Enhanced/Parsed transactions (legacy/beta, plan-gated), DAS, `transactionSubscribe`/LaserStream (proprietary), webhooks. `docs/api/providers/helius.md` lists the hosts and their caveats; API key in the query string means a leaked URL is a leaked key ([secret-exposure.md](./secret-exposure.md)).
- Rate limits and plan gating show as `429`/`403` with a healthy status page; a real outage shows as timeouts/5xx.
- Balance amounts: never take Parsed Events' `rawTokenAmount` (JSON number) for ledger figures; the adapters use RPC `uiTokenAmount.amount` strings — irrelevant to the outage but relevant when switching surfaces during recovery.
- Backfill need: webhook events lost during the outage must be recovered via `getTransactionsForAddress`/`SearchWalletActivity` by periodic reconciliation, not re-requested from the webhook queue.

## Containment and recovery

1. Let the tracker recover Helius through hysteresis (`RecoverStreak` consecutive successes); no operator release is needed unless someone set `DISABLED`/activated a switch.
2. If `CHAIN_DISABLE_NEW_ACTIONS` was activated (both observers down), release it via the dual-controlled `KILL_SWITCH_RELEASE` path once at least the fallback is healthy and the agreement policy can resolve in-flight attempts.
3. Trigger periodic reconciliation for the outage window on every active wallet (`Engine.RunPeriodic(provider, since)`, PENDING engine) so any activity missed by the stream is found; expect `finality upgrade` auto-resolutions (`OBSERVED → FINALIZED`, no economic change).
4. Funding availability resumes automatically once both observers agree on the receipts.
5. Replay the market-data gap from the raw archive if `supports_replay` (`internal/reality`); otherwise record `stream_gaps` as `UNRECOVERABLE` and label affected backtests.

## What NOT to do

- Never lower `RequireBothFor` from `FINALIZED` to keep withdrawals/funding availability flowing on one observer (PART 196: never pick the optimistic answer).
- Never mark attempts `FINALIZED` by hand or "trust Jupiter's accepted" as landed.
- Never disable reconciliation to reduce fallback RPC load; reduce polling cadence through configuration instead.
- Never switch the fallback to the same vendor as the primary; independence is the point (POINT_IN_TIME.md §5).
- Never re-request lost webhooks as a source of truth; backfill through observer reads.

## Verification / exit criteria

- Helius `HEALTHY` for at least the hysteresis window; `PrimaryOnly`/`SecondaryOnly` resolutions no longer being recorded.
- In-flight attempts from the window reached `FINALIZED` with `AGREED` resolutions; deposits waiting on finality became `AVAILABLE` with agreed receipts.
- Periodic reconciliation for the window produced no unresolved `MISMATCH`; any `SUBMISSION_UNKNOWN` records resolved per [submission-unknown.md](./submission-unknown.md).
- Ingestion checkpoints `ACTIVE`; gaps resolved or labelled.

## Post-incident

- Archive health samples, the provider incident, the reconciliation run summary and any gap records.
- Re-verify the Helius notes (`docs/api/providers/helius.md` is `PARTIAL`): if a host or surface changed, update the record rather than deleting it.
- Update `docs/build/REQUIREMENTS_TRACEABILITY.md` R-078-1, R-079-1, R-107-1; add the "observer disagreement ⇒ MISMATCH and block" and single-observer degradation tests to BLOCKERS if not recorded as passing.
