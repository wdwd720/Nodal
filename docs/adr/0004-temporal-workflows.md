# ADR-0004: Temporal for durable multi-step processes only

## Status

Accepted — implementation tracked in docs/build/REQUIREMENTS_TRACEABILITY.md

## Date

2026-09-05

## Context

Several processes in the platform span hours or days, involve external callbacks,
timers, and human steps, and must survive process restarts: the funding lifecycle
(PART 28), reconciliation escalation, strategy and canary promotion (PART 69, 70),
controlled withdrawal (PART 94), and provider remediation. The failure modes of
building these by hand are well known:

- A cron loop plus a status column loses in-flight context on crash and re-executes
  steps that already had external effect (a second on-ramp charge, a second
  withdrawal request).
- Retry logic implemented ad hoc per process either retries money movement blindly
  (PART 46 forbids this) or never retries.
- No visibility into which step a given funding request is stuck on during an
  incident.

Temporal solves durability and visibility. It also introduces its own failure modes
that must be designed against:

- Workflow state drifting into "the balance". Workflow memory is not accounting
  truth (PART 11, 116).
- Activities retried after their external side effect succeeded, producing
  duplicate economic effect.
- Using a workflow per market tick, exploding history size and cost (PART 115).
- Non-deterministic workflow code breaking replay.

## Decision

- Temporal, through the Go SDK, behind a `WorkflowEngine` interface
  (DECISION_REGISTER D-009). Managed Temporal in staging and production (BLOCKERS
  EB-008); the auto-setup container locally (D-010).
- Used only for: funding lifecycle, reconciliation escalation, strategy promotion,
  canary promotion, controlled withdrawal (capability disabled in V1), provider
  remediation, and later cross-chain settlement. Not for the order hot path, not
  per market tick, not for risk evaluation.
- Workflow state is never balance truth. Every activity that touches money calls the
  same domain services as the synchronous path, with an idempotency key derived from
  the workflow's domain identifier, and reads Postgres state before acting.
- Workflow IDs are derived from domain IDs (for example the funding request ID) so
  that a duplicate start is rejected by Temporal rather than creating a second
  process.
- Activities are idempotent by construction: they check for an existing provider
  reference or ledger posting before issuing an external call, and they treat
  `IDEMPOTENCY_IN_PROGRESS` as retry-later.
- Replay determinism is enforced with the SDK's replay tests for every workflow.
- The in-memory or fake engine lives in a test package and is rejected in PROD by
  configuration validation.

### Explicitly not decided / deferred

- Temporal Cloud versus self-managed on the chosen cloud (EB-008).
- Namespace and task-queue layout.
- Whether periodic reconciliation sweeps use Temporal schedules or a plain worker
  loop. Either is acceptable provided the sweep reads truth from Postgres.

## Consequences

### Positive

- Durable timers, retries with backoff, and per-step visibility without bespoke
  state machines.
- Human-in-the-loop steps (promotion approvals, withdrawal authorization) are
  natural signals rather than polling.

### Negative

- A second stateful system to operate and version; SDK upgrades require replay
  compatibility care.
- Developers must learn workflow determinism constraints.

### Operational

- Worker capacity and task-queue backlogs need alarms (PART 135).
- Version workflows with the SDK's patching facilities; never edit a running
  workflow's logic without a version marker.

## Alternatives considered

- Hand-rolled state machine plus cron: rebuilds timers, retries, and visibility;
  historically the source of duplicated external effects.
- AWS Step Functions: JSON state language, weaker unit-test story, no Go-native
  workflow code; portable only within one cloud.
- Cadence: Temporal is its successor with the same model and active maintenance.
- Database job queue only: adequate for simple jobs; inadequate for multi-day sagas
  with human steps and external callbacks.
- Choreography over the event stream: control flow becomes implicit across consumers
  and is hard to reason about during incidents.

## Related

- ADR-0001, ADR-0003, ADR-0005, ADR-0016.
- Goal PART 11, 12, 28, 46, 69, 70, 94, 115, 116, 162, 163.
- DECISION_REGISTER D-009, D-010; BLOCKERS EB-008.

## Evidence required before this ADR's guarantees can be claimed VERIFIED

- Replay tests for every workflow type, run in CI.
- Activity idempotency tests: an activity retried after its external call succeeded
  produces no second provider request and no second ledger posting.
- Duplicate-start test: starting a workflow twice with the same domain ID yields
  one execution.
- Funding E2E (PART 162) including a `workflow-worker` kill and restart mid-workflow.
- An architectural test that no ledger posting or reservation is performed from
  workflow code except through the domain service interfaces.
- PROD configuration validation test rejecting the fake engine.
- Chaos test with Temporal unavailable: new trades continue (they do not depend on
  Temporal); funding lifecycle pauses and resumes without duplication.
