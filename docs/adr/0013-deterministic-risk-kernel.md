# ADR-0013: Deterministic, versioned Risk Kernel with no model in the loop

## Status

Accepted — implementation tracked in docs/build/REQUIREMENTS_TRACEABILITY.md

## Date

2026-09-05

## Context

Every money-affecting action passes the risk kernel twice: after the intent is
formed and again after a quote is in hand, before signing (PART 10). If that check
is fuzzy, injectable, or unrecorded, every other control is weakened. The failure
modes:

- A model asked "is this trade acceptable?": nondeterministic, subject to prompt
  injection, and impossible to replay (PART 58).
- Risk rules embedded in agent prompts or strategy text: the party being limited
  controls the limit (PART 60).
- Self-modification: an agent requesting "increase my max position" and a runtime
  that honours it.
- Unrecorded decisions: a fill that cannot be tied to the policy version, inputs,
  and reason codes that permitted it cannot be audited.
- Stale inputs: a decision computed on a stale price or an outdated envelope
  permits exposure the current state would forbid (PART 174).
- Kill switches implemented as "stop everything", which blinds settlement and
  reconciliation and leaves money in limbo (PART 52).
- Unversioned policy edits: a limit changed in place with no approval record.

## Decision

- The risk kernel is a deterministic function of typed inputs: the intent, the
  quote where present, the account and envelope snapshot, market values with
  timestamps and sources, and a versioned `RiskPolicy`. It performs no I/O of its
  own and never calls a model. The same inputs always produce the same decision
  and the same decision hash.
- `RiskPolicy` is versioned and hashed. Mutation happens only through the admin
  plane (PART 128) under the approval and audit rules there; agents, strategies,
  and the API surface used by customers cannot mutate policy. The IR's policy
  reference (ADR-0010) is resolved at runtime to an independently stored policy.
- Every `RiskDecision` is persisted before the reservation it authorises, with the
  fields in PART 58: policy version and hash, relevant account and market values,
  decision, reason codes, timestamp, intent, quote, and resulting constraints.
- Layers per PART 59 (global, account, envelope, instrument, venue) are evaluated
  in a fixed order; the most restrictive constraint wins.
- Kill switches (PART 52, 53) stop new risk only. Reading external state, processing
  received fills, settlement, reconciliation, ledger posting, and permitted
  risk-reducing cleanup continue.
- Input staleness is an input: a decision on data older than policy thresholds
  fails with `STALE_MARKET_DATA` rather than proceeding.
- The kernel uses only exact numerics (DECISION_REGISTER D-007) and
  overflow-checked arithmetic.

### Explicitly not decided / deferred

- The V1 limit values (position, daily loss, drawdown, order rate). These are
  policy content under risk approval, not architecture.
- Whether policies are expressed as a data document or as versioned Go code. A
  data document is preferred; either must be hashed and versioned.
- Portfolio-level correlation or VaR-style measures; V1 layers are limit-based.

## Consequences

### Positive

- Decisions are replayable in backtests and incident reviews.
- Prompt injection cannot reach the permission decision.
- Policy changes are auditable events with approvers.

### Negative

- No adaptive or learned risk behaviour in the permission path; learned signals may
  inform policy content offline, never the decision at run time.
- Every new input the kernel needs must be threaded through as typed data.

### Operational

- Policy activation is a gated change with a version bump and audit event.
- Decision volume and rejection reasons are metrics (PART 132, 133).

## Alternatives considered

- Model-in-the-loop risk assessment: rejected outright by PART 58.
- Rules inside agent prompts or strategy text: rejected; the limited party would
  control the limit.
- External rules-engine service: adds latency and a network dependency to the hot
  path and moves the auditable artefact outside the repository.
- Limits only in the capital envelope: the envelope is one layer; global and
  account layers must exist independently of any agent.

## Related

- ADR-0010, ADR-0012, ADR-0014, ADR-0015, ADR-0016.
- Goal PART 10, 24, 52, 53, 58, 59, 60, 128, 132, 133, 174, 224.
- DECISION_REGISTER D-007.

## Evidence required before this ADR's guarantees can be claimed VERIFIED

- Property tests: determinism (identical inputs produce identical decision and
  hash); monotonicity (increasing notional never turns a rejection into an
  approval under the same policy); overflow safety on extreme values.
- Golden decision corpus: recorded inputs and expected decisions per policy version,
  run in CI.
- Test that an agent-role principal and a customer principal receive `FORBIDDEN`
  on every policy-mutation endpoint.
- Integration test that a `RiskDecision` row exists before the corresponding
  reservation row and that the reservation references it.
- Kill-switch E2E (PART 165): new submissions rejected with `KILL_SWITCH_ACTIVE`;
  in-flight fills, reconciliation, and ledger posting complete.
- Staleness test: inputs older than threshold produce `STALE_MARKET_DATA`.
- `test/security` architectural test: `internal/risk` imports no model client.
- Evidence-matrix entry for "Risk" per PART 239.
