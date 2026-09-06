# ADR-0008: Jupiter as the V1 Solana execution router, behind an adapter

## Status

Accepted — implementation tracked in docs/build/REQUIREMENTS_TRACEABILITY.md

## Date

2026-09-05

## Context

V1 executes spot swaps on Solana. Routing across DEX liquidity is a solved problem
that the platform should not rebuild. The choice of an external router creates
specific failure modes that the architecture must contain rather than trust away:

- Upstream compromise: a routing provider returns a transaction that, in addition
  to the swap, sets a token-account delegate, transfers SOL to an unknown address,
  or invokes an unapproved program. If the platform signs what it is handed, the
  provider's security is the platform's security (PART 34).
- Schema leakage: if the provider's request and response types are used as the
  platform's `Quote` and `ExecutionPlan`, every venue change becomes a rewrite and
  the provider's field semantics silently become financial semantics (PART 43).
- Integration from memory: writing against a remembered API version produces code
  that appears to work in tests and fails or mis-prices in production (PART 43).
- Quote staleness and slippage: a quote acquired seconds ago may no longer be
  executable; submitting it anyway produces worse fills or failures that the ledger
  must then explain (PART 42, 48).
- Token-2022 extensions: transfer fees, hooks, and freeze authorities change balance
  semantics; a swap that "succeeds" can deliver less than the plan assumed.
- Provider outage or rate limiting turning into a stuck order state.

## Decision

- Jupiter is the V1 execution router for Solana spot swaps. It is one
  implementation of the `ExecutionAdapter` contract (PART 41); the rest of the
  system sees the platform's own `Quote`, `ExecutionPlan`, and submission types.
- The adapter converts provider responses into the platform's `Quote` model
  (PART 42) with explicit `Price`, expiry, route identity, slippage bound, and
  source. Provider-specific fields stay inside the adapter package.
- Every transaction returned by the provider is untrusted input. It is deserialized,
  inspected against the approved plan (PART 34, 44), simulated where appropriate,
  and only then presented to the bounded signing service (ADR-0012). Unsupported
  token extensions are rejected by default.
- Raw request and response bodies are stored with the order (PART 183) and archived
  (ADR-0007).
- The official API documentation is consulted at implementation time; the adapter
  records the API version it targets, and contract tests use recorded fixtures.
- Provider health, retry classification, and circuit breaking follow PART 79, 106,
  107; a provider failure yields a typed error (`VENUE_UNAVAILABLE`,
  `QUOTE_EXPIRED`) and never a blind retry of a submission.
- The adapter's verification label starts at `CODE_COMPLETE` and advances only with
  evidence (PART 208).

### Explicitly not decided / deferred

- Which provider API surface and access tier is used (BLOCKERS EB-006).
- Priority-fee and compute-budget policy.
- A second router or direct DEX adapters as fallback. The interface admits them; no
  V1 commitment.
- Any non-Solana venue.

## Consequences

### Positive

- Access to aggregated liquidity without building routing.
- Venue substitution is an adapter change, not a core change.
- The inspection boundary means a provider compromise is bounded to a failed or
  rejected transaction, not value loss.

### Negative

- Dependence on one external provider for all V1 execution; an outage halts new
  trades (kill-switch semantics apply: settlement and reconciliation continue).
- Inspection rules must track Solana program and token-standard evolution.

### Operational

- Contract fixtures must be refreshed when the provider changes its API; drift
  detection is a test, not a hope.
- Provider canary (PART 207) runs before any capability moves to live.

## Alternatives considered

- Direct DEX integrations (individual AMMs): more code per venue, fragmented
  liquidity, same inspection requirement. Possible later as additional adapters.
- Internal order matching: excluded by PART 4 and ADR-0019.
- Centralized exchange execution: excluded by PART 4 (no CEX omnibus retail
  accounts) and by the custody model.
- Another aggregator as primary: no evidence of an advantage at V1; the adapter
  contract keeps the option open.

## Related

- ADR-0007, ADR-0009, ADR-0012, ADR-0014, ADR-0019.
- Goal PART 34, 39, 40, 41, 42, 43, 44, 45, 46, 48, 79, 105, 106, 107, 147, 148,
  152, 183, 207, 208.
- BLOCKERS EB-006, EB-003.

## Evidence required before this ADR's guarantees can be claimed VERIFIED

- Contract tests against recorded fixtures for quote, swap-transaction build, and
  error responses, including a schema-drift test that fails on unknown required
  fields.
- Adapter unit tests for quote expiry, slippage bound enforcement, and conversion to
  the platform `Quote` with exact numerics.
- Transaction inspector fuzz corpus (PART 152) seeded with provider-shaped
  transactions carrying injected delegate, authority, extra-transfer, and
  unknown-program instructions; all rejected.
- Circuit-breaker and retry-matrix tests (PART 106, 107) showing no submission is
  retried without status recovery.
- `SANDBOX_VERIFIED` and `CANARY_VERIFIED` reports with stored raw evidence, before
  any `LIVE_*` claim (PART 206–208).
- Evidence-matrix entry for "Jupiter" per PART 239.
