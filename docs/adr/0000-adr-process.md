# ADR-0000: Architecture Decision Record process

## Status

Accepted — implementation tracked in docs/build/REQUIREMENTS_TRACEABILITY.md

## Date

2026-09-05

## Context

The platform is a financial control plane whose safety depends on a small number of
structural commitments (where money truth lives, who may sign, what code may run
live). Those commitments are easy to erode one pull request at a time unless each
one is written down with its failure modes, its boundaries, and the evidence that
would prove it holds. Goal PART 210 lists the decisions that must have an ADR.
Goal PART 209 forbids aspirational documentation presented as implemented fact.

Two distinct questions must never be conflated in this directory:

1. Has the decision been made? (ADR status)
2. Has the decision been implemented and proven? (traceability status)

An ADR answers only the first. The second is answered by
`docs/build/REQUIREMENTS_TRACEABILITY.md`, using the goal's verification labels.

## Decision

### Numbering and naming

- Files are `NNNN-short-kebab-title.md`, four-digit zero-padded, monotonically
  increasing, never reused. `0000` is this process document.
- `README.md` in this directory is the index and must be updated in the same
  change as any new or superseded ADR.
- The internal codename never appears in ADR text. Refer to "the platform". The one
  permitted exception is quoting the goal's V1 exclusion list, which names an
  excluded token by codename.

### Status vocabulary

| Status | Meaning |
|---|---|
| `Proposed` | Drafted; not yet agreed. Must not be cited as a constraint. |
| `Accepted — implementation tracked in docs/build/REQUIREMENTS_TRACEABILITY.md` | Agreed as intended architecture. Says nothing about implementation state. |
| `Superseded by ADR-NNNN` | Replaced. The file stays; the decision text is not edited. |
| `Deprecated` | No longer applies and nothing replaces it. |
| `Rejected` | Considered and declined; kept so the reasoning is not lost. |

Implementation and verification state is expressed only with the goal's labels
(`CODE_COMPLETE`, `CONTRACT_TESTED`, `SANDBOX_VERIFIED`, `CANARY_VERIFIED`,
`LIVE_VERIFIED`, `BLOCKED_EXTERNAL`) and only in the traceability file or the
readiness report, never in an ADR status line. An ADR may be `Accepted` while every
requirement it implies is unimplemented.

### Required sections (template)

```markdown
# ADR-NNNN: Title

## Status
<one value from the vocabulary above>

## Date
YYYY-MM-DD

## Context
The concrete financial, security, or operational failure modes that motivate the
decision. Name attackers, race conditions, and data-loss scenarios specifically.

## Decision
What is decided, precisely. Include a subsection "Explicitly not decided / deferred"
listing what this ADR does not settle.

## Consequences
### Positive
### Negative
### Operational

## Alternatives considered
Each alternative with the reason it was rejected for V1.

## Related
Other ADRs, goal PART numbers, DECISION_REGISTER IDs, BLOCKERS IDs.

## Evidence required before this ADR's guarantees can be claimed VERIFIED
The named tests, reports, and reviews that must exist. Until they exist, the
guarantees in the Decision section are intent, not fact.
```

### Rules

- Plain engineering prose. No marketing language.
- Never state that something is implemented, tested, or live inside an ADR.
- A change to an `Accepted` decision is a new ADR that supersedes the old one, plus a
  DECISION_REGISTER entry. Typo fixes are permitted in place.
- Every ADR that constrains code must name at least one test or report in its
  evidence section, so that the traceability file has something to point at.
- Length target 60–140 lines. Longer material belongs in `docs/architecture/`.

### Explicitly not decided / deferred

- Tooling to lint ADR structure automatically. Until it exists, review is manual.
- Mirroring ADRs into the readiness report; it cites the traceability file instead.

## Consequences

### Positive

- Reviewers can reject a change by pointing at a numbered constraint.
- The distinction between "decided" and "proven" is structural, not a matter of
  careful wording.

### Negative

- Overhead per architectural change. This is intended.

### Operational

- The index in `README.md` is the entry point for auditors and new engineers.

## Alternatives considered

- Wiki pages: not versioned with code, drift silently. Rejected.
- Decisions only in `DECISION_REGISTER.md`: the register records deviations compactly
  without failure modes, alternatives, or evidence requirements. Both are kept.

## Related

- Goal PART 7, 209, 210, 237, 239, 243.
- `docs/build/DECISION_REGISTER.md`, `docs/build/BLOCKERS.md`,
  `docs/build/MASTER_BUILD_STATE.md`.

## Evidence required before this ADR's guarantees can be claimed VERIFIED

- `docs/adr/README.md` lists every ADR required by PART 210 with a status.
- `docs/build/REQUIREMENTS_TRACEABILITY.md` exists and references ADR numbers.
- A review checklist item in CI or the PR template requiring an ADR for changes to
  ledger, capital, risk, signing, capability-gate, or strategy-runtime boundaries.
