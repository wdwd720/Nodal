# Runbook: Temporal (workflow engine) outage

Severity: SEV2 (provider outage) · Owner: OPERATIONS · Related: [chargeback-reversal.md](./chargeback-reversal.md), [funding-provider-compromise.md](./funding-provider-compromise.md), [redpanda-outage.md](./redpanda-outage.md), ADR-0004

## Trigger

- Workflow-worker task-queue backlog or schedule-to-start latency alarm; Temporal frontend unreachable; TLS failures (`TEMPORAL_TLS` is mandatory in STAGING/PROD).
- Funding deposits not progressing: rows in `deposits` stuck in `PROVIDER_CONFIRMED`/`SETTLEMENT_OBSERVED` beyond `settlement_timeout` without a provider or chain cause.
- Reconciliation escalation workflows or promotion workflows not advancing.
- PENDING: alarms (ADR-0004 "worker capacity and task-queue backlogs need alarms"); PENDING: `cmd/workflow-worker` and the workflows themselves (the Temporal SDK is a dependency; the funding lifecycle driver is Temporal-free at the domain layer, MASTER_BUILD_STATE.md).

## Blast radius

- **No financial loss by design** (PART 116, ADR-0004): workflow state is orchestration only; every activity calls the same domain services as the synchronous path with an idempotency key derived from the domain id, and reads Postgres before acting.
- Paused: funding lifecycle progression (session → provider status → chain receipt → reconciliation → availability), reconciliation escalation to operators, strategy/canary promotion, the withdrawal workflow (capability DISABLED anyway), provider remediation.
- **Not affected:** trading. Intents, risk, settlement compilation, signing, submission, finality observation, fill posting and periodic/event-driven reconciliation do not depend on Temporal (ADR-0004: "not for the order hot path"). Deposits already `AVAILABLE` keep trading.
- Customer-visible: new deposits appear stuck; that is acceptable and must not be worked around by crediting early (PART 162: a redirect success never credits money).

## Immediate actions (first 10 minutes)

1. Confirm scope. Read-only:
   ```sql
   SELECT status, count(*), min(updated_at) FROM deposits
    WHERE status NOT IN ('AVAILABLE','FAILED','EXPIRED','CANCELLED','REVERSED') GROUP BY status;
   SELECT status, count(*) FROM reconciliation_records WHERE status = 'ESCALATED' GROUP BY status;
   ```
   Stuck rows across all providers with no `provider_events` errors ⇒ orchestration, not the provider.
2. Confirm the trading path is unaffected: `SELECT max(posted_at) FROM journal_transactions WHERE kind = 'TRADE_FILL';`, order transitions still occurring.
3. Do **not** activate a kill switch for a Temporal outage alone. Consider `FUNDING_DISABLE(*)` (SEVERE, `kill:activate`) only if customers keep creating sessions that cannot progress and the provider charges for abandoned sessions; weigh the slow release path before doing so.
4. Check the Temporal service: managed Temporal status (PENDING: EB-014); locally `docker compose ps temporal` and the Temporal UI on the compose port.
5. Announce: "orchestration outage; trading unaffected; funding and escalations paused since <time>; no manual credits".

## Diagnosis

- Worker vs server: workers alive but no progress ⇒ server/namespace; workers crash-looping ⇒ check for a non-deterministic workflow change without a version marker (ADR-0004), an SDK upgrade, or replay failures in worker logs.
- Task-queue backlog per queue in the Temporal UI/CLI; a single stuck workflow with retries exhausted usually means an activity returning a non-retryable domain error (for example `IDEMPOTENCY_IN_PROGRESS` is retry-later, `VALIDATION_FAILED` is not).
- Duplicate-start protection: workflow ids derive from domain ids (deposit id), so a "workflow already started" error during recovery is expected and harmless.
- Provider webhooks keep arriving during the outage and are persisted in `provider_events` / inbox regardless; the workflow catches up from Postgres state, not from replayed webhooks.

## Containment and recovery

1. Restore Temporal (managed incident, or `docker compose up -d temporal` locally); restart `cmd/workflow-worker` (PENDING) after the frontend is healthy.
2. Workflows resume from their last durable step. Because activities are idempotent against Postgres (they check for an existing provider reference or ledger posting before any external call), a retried activity produces no second provider request and no second `FUNDING_SETTLED` posting.
3. Watch `deposits` drain: `PROVIDER_CONFIRMED → SETTLEMENT_OBSERVED → RECONCILED → AVAILABLE`, each with a `deposit_transitions` row and a single `journal_transactions.kind = 'FUNDING_SETTLED'` per deposit (`idempotency_key` unique).
4. If a workflow is stuck on a step that genuinely completed externally (chain receipt exists, posting exists), the fix is to let the activity's idempotent read observe it; never signal the workflow to "skip" a money step.
5. If Temporal history is lost (namespace loss), no financial repair is needed: re-drive funding from Postgres state by starting workflows again with the domain-derived ids; `funding.Service` transitions refuse anything already done.

## What NOT to do

- Never credit a deposit manually because "the provider confirmed it"; only an observed and agreed chain receipt moves it to `SETTLEMENT_OBSERVED` (RECONCILIATION.md §5).
- Never run an activity's external call by hand (a second onramp charge or a second withdrawal request is exactly the failure mode Temporal exists to prevent).
- Never edit workflow history or reset a workflow past a money step.
- Never stop reconciliation because escalation workflows are down; the records still open and block new risk on their own.
- Never treat Temporal memory as a balance (PART 116).

## Verification / exit criteria

- Task-queue backlog at baseline; no workflows in a retry loop.
- No deposit older than `settlement_timeout` remains between `PROVIDER_CONFIRMED` and `AVAILABLE` without a reconciliation record explaining it.
- Exactly one `FUNDING_SETTLED` journal transaction per deposit that became `AVAILABLE` during recovery (`SELECT reference_id, count(*) FROM journal_transactions WHERE kind = 'FUNDING_SETTLED' GROUP BY reference_id HAVING count(*) > 1` is empty).
- `ESCALATED` reconciliation records have been picked up by operators.

## Post-incident

- Archive worker logs and the Temporal incident report; note any non-determinism error and the workflow version marker introduced.
- Verify the ADR-0004 evidence list: activity idempotency test, duplicate-start test, workflow-worker kill/restart in the funding E2E (PART 162); file the chaos test "Temporal unavailable: new trades continue, funding pauses and resumes without duplication" as a BLOCKERS item if absent.
- Update `docs/build/REQUIREMENTS_TRACEABILITY.md` R-116-1.
