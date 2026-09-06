# ADR-0014: Deterministic Settlement Compiler as the only route from intent to execution

## Status

Accepted — implementation tracked in docs/build/REQUIREMENTS_TRACEABILITY.md

## Date

2026-09-05

## Context

Goal PART 10 defines one control path from a typed intent to audit evidence and
states that no alternate money path is permitted. The Settlement Compiler
(PART 38) is the component that turns an intent plus current state into an ordered,
typed `ExecutionPlan`. Without it, the following failures appear:

- Per-venue imperative execution code, each copy re-implementing (and eventually
  skipping) eligibility, risk, reservation, inspection, or reconciliation steps.
- Steps skipped under pressure: a hotfix that "just submits" bypasses final risk
  validation or inspection because the sequence lived in code paths rather than in
  a plan that is checked for completeness.
- Uninspectable plans: an operator cannot answer "what exactly will this intent do"
  before it executes.
- Nondeterministic planning: the same intent and state yielding different plans on
  different runs, making backtests and incident replay meaningless (PART 224, 225).
- A model choosing execution steps: an injection vector into the money path.
- Silent fallbacks when no valid plan exists (PART 40): the platform must say
  `NO_VALID_PLAN` with reasons, not improvise.
- Partial execution ambiguity (PART 228): without a plan with explicit step
  boundaries, the state after a crash cannot be located.

## Decision

- The Settlement Compiler is a deterministic, typed, pure function:
  `TradeIntent` + account state + buying-power state (ADR-0015) + instrument
  metadata + eligible venues + system health → `ExecutionPlan` or a typed
  `NoValidPlan` with reason codes. It performs no I/O and calls no model; it is
  testable without an LLM (PART 38).
- The `ExecutionPlan` is an ordered list of typed steps covering, at minimum, the
  sequence in PART 38: eligibility, risk, reservation, venue resolution, settlement
  asset location, quote acquisition and validation, final risk check, transaction
  build and inspection, signature request, submission, finality observation,
  reconciliation, ledger posting, position update, reservation release. Mandatory
  steps cannot be omitted; the plan validator rejects incomplete plans.
- The plan is hashed over its canonical serialization. The hash is carried on the
  order, on the signing request (ADR-0012), and on every audit event, so that what
  was approved is provably what executed.
- Steps are executed by the execution worker under the order state machine
  (PART 47) and the timeout rule (PART 48). Every step has a durable state
  transition; a crash between steps leaves a recoverable, typed state
  (`SUBMISSION_STATE_UNKNOWN` where appropriate), never an ambiguous one.
- The same compiler and evaluator are used by the execution simulator for backtests
  (PART 83, 225); simulation substitutes adapters, not planning logic.
- There is no API, worker, or script that submits a transaction without a plan
  produced by this compiler. This is the "no alternate money path" rule.

### Explicitly not decided / deferred

- Multi-leg and cross-chain plans (PART 230). The step vocabulary is extensible;
  V1 plans are single-venue Solana swaps.
- Venue scoring or optimisation heuristics beyond eligibility and health.
- Batching of multiple intents from one account into one plan.

## Consequences

### Positive

- One place encodes the money path; review of that place is review of the path.
- Plans are inspectable by operators and reproducible in tests.
- Partial execution is always locatable to a step.

### Negative

- Adding a venue or asset class requires extending the typed step vocabulary and
  the golden corpus; there is no shortcut.
- The compiler's input snapshot must be assembled carefully; a stale snapshot is a
  stale plan (mitigated by final risk validation on the fresh quote).

### Operational

- Plan rejection reasons are metrics (PART 133) and are shown to the user honestly
  (PART 112).

## Alternatives considered

- Per-venue execution functions: duplicated safety logic, drift, and no single plan
  artefact. Rejected.
- Using the workflow engine as the planner: Temporal orchestrates long-running
  processes (ADR-0004); putting plan semantics into workflow code would place the
  hot path and the money-path definition inside a replayed history. Rejected.
- Model-generated execution plans: nondeterministic and injectable. Rejected.
- Implicit plans (state machine only): a state machine without an explicit plan
  cannot be validated for completeness before execution starts.

## Related

- ADR-0008, ADR-0009, ADR-0012, ADR-0013, ADR-0015, ADR-0019.
- Goal PART 3, 10, 35, 38, 39, 40, 41, 46, 47, 48, 49, 83, 112, 133, 170, 224,
  225, 228, 230.
- DECISION_REGISTER D-007.

## Evidence required before this ADR's guarantees can be claimed VERIFIED

- Golden corpus (PART 170): intent and state snapshots to plan snapshots, run in
  CI; any change in output requires review.
- Property test: determinism (same inputs produce byte-identical plans and hashes).
- Plan-validator tests: a plan missing any mandatory step is rejected; step order
  constraints are enforced.
- `NoValidPlan` coverage: a test per reason code (no eligible venue, restricted
  asset, insufficient buying power, degraded system health, quote unavailable).
- Crash test (PART 49) exercised at every step boundary, with recovery to a typed
  state and exactly one economic effect.
- `test/security` architectural test: no package other than the execution worker
  calls the submission adapter, and the worker requires a plan hash.
- Simulation reuse test (PART 225): the backtest simulator and live path share the
  compiler package.
- Evidence-matrix entry for "Reconciliation" and the execution path per PART 239.
