# ADR-0010: Typed, hashed Strategy IR as the only executable strategy form

## Status

Accepted — implementation tracked in docs/build/REQUIREMENTS_TRACEABILITY.md

## Date

2026-09-05

## Context

Strategies enter the platform two ways: natural language compiled by a model, and a
TypeScript SDK used by developers (PART 61). If those two paths produced different
runtime artefacts, or if either produced something that is executed directly, the
following failures become possible:

- Model output executed as-is: a hallucinated or injected instruction becomes a
  trade decision with no intermediate representation a human can inspect
  (PART 64, 67).
- Ambiguity between what the user asked for and what runs; no way to show the user
  the compiled form for approval.
- Unbounded loops, recursion, or data dependencies that make a strategy evaluation
  unbounded in time or cost (PART 62, 197, 198).
- Effect escalation through text: a strategy description that says "and also
  withdraw" must be structurally incapable of producing a withdrawal (PART 63).
- Irreproducibility: a decision that cannot be replayed from the strategy artefact
  and point-in-time inputs cannot be audited or backtested honestly (PART 80, 224).
- Version drift: the strategy that was reviewed and promoted differs from the one
  running, with no hash to prove otherwise.

## Decision

- The only executable strategy artefact is a Strategy IR document with the fields in
  PART 62: schema version, strategy ID and version, immutable hash, owner, triggers,
  signals, conditions, actions, dependencies, risk-policy reference, model budget,
  data budget, capital-envelope requirements, effect set, lineage, and build
  timestamp.
- Both authoring paths converge on the same IR. The natural-language compiler
  produces IR through the pipeline in PART 64 (schema-constrained response, parse,
  structural, type, effect, and risk-compatibility validation, human-visible
  compiled form). The TypeScript SDK produces IR directly. Neither path has a
  runtime privilege the other lacks.
- The IR is data. There is no `eval`, no shell, no file access, no unbounded loop
  construct. Evaluation is bounded by construction (finite trigger set, finite
  signal graph, per-evaluation budgets).
- The IR declares its effect set. The compiler rejects forbidden effects; the
  runtime independently rejects them again (PART 63).
- The risk-policy reference in the IR is resolved at runtime to an independently
  controlled policy; the IR cannot carry or override risk limits (PART 60).
- The IR hash is computed over a canonical serialization and is recorded on every
  prediction and intent the strategy produces, giving lineage from decision to
  artefact.
- Every compiled IR version is immutable; changes produce a new version and go
  through promotion gates (PART 68–70).

### Explicitly not decided / deferred

- The concrete V1 vocabulary of triggers, signals, conditions, and actions. It will
  be small and grow through the golden corpus (PART 170).
- Serialization format (canonical JSON versus Protobuf). Either must be canonical
  and hash-stable.
- SDK packaging and distribution.

## Consequences

### Positive

- Strategies are inspectable, diffable, and reproducible.
- Effect bounds are a property of the artefact, verifiable before promotion.
- Backtests and live runs execute the same IR through the same evaluator
  (PART 225).

### Negative

- Expressiveness is limited to what the IR vocabulary supports; users who want
  arbitrary code are declined in V1 (ADR-0011).
- The natural-language compiler will fail on requests outside the vocabulary; the
  product must surface that honestly (PART 112).

### Operational

- The golden corpus is a release gate; a change to the compiler that alters any
  golden output requires review.

## Alternatives considered

- Arbitrary user code (Python/Node/Rust): excluded by PART 4 and ADR-0011.
- Sandboxed scripting (Lua, WASM): still arbitrary code; effect bounds would depend
  on the sandbox rather than the artefact. Deferred to the PART 232 shape.
- Text DSL executed by an interpreter: acceptable as an authoring front end only; the
  runtime representation must still be typed IR.
- Direct model function-calling per tick: nondeterministic, expensive, and not
  reproducible; violates PART 58 and PART 224.

## Related

- ADR-0002, ADR-0011, ADR-0012, ADR-0013, ADR-0014.
- Goal PART 9, 10, 60, 61, 62, 63, 64, 65, 68, 69, 70, 80, 170, 176, 197, 198,
  224, 225.
- BLOCKERS EB-011, EB-015.

## Evidence required before this ADR's guarantees can be claimed VERIFIED

- Golden compiler corpus tests (PART 170): input to IR snapshots for both
  authoring paths, with identical IR for equivalent inputs.
- Property test that the IR hash is invariant under serialization field order and
  changes whenever any semantic field changes.
- Effect-validation tests: every forbidden effect in PART 63 is rejected by the
  compiler and, independently, by the runtime when injected past the compiler.
- Bounded-evaluation tests: a maximal IR evaluates within its declared budgets;
  budget exhaustion yields a typed error, not a hang.
- Fuzz target for the IR parser with a committed corpus.
- Reproducibility test: same IR hash plus same point-in-time inputs produce
  byte-identical predictions and intents across runs.
- Promotion test: a modified IR cannot run under a previously approved hash.
