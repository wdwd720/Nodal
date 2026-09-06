# ADR-0009: Helius as the primary Solana data provider, with independent fallback

## Status

Accepted — implementation tracked in docs/build/REQUIREMENTS_TRACEABILITY.md

## Date

2026-09-05

## Context

The platform must observe external truth on Solana (PART 11.1): transaction status,
finality, balances, and streamed on-chain events. Observation feeds finality policy
(PART 45), reconciliation (PART 50), and the reality engine. The failure modes:

- Blindness: a single observation provider outage leaves submitted transactions in
  `SUBMISSION_STATE_UNKNOWN` indefinitely; the platform cannot tell a fill from a
  drop and cannot release reservations (PART 48).
- Provider label trusted as chain truth: a provider marking a transaction
  "confirmed" is not the same as the chain reaching the required commitment; the
  platform must not collapse `SUBMITTED`/`OBSERVED`/`CONFIRMED`/`FINALIZED`
  (PART 45, 78).
- Fake or replayed observations: webhooks are unauthenticated input unless verified;
  a duplicate delivery must not produce a second fill (PART 199).
- Provider lag misread as failure (or vice versa): stale data that is not detected
  drives wrong decisions (PART 174, 175).
- Conflict of interest: using the execution router (ADR-0008) to confirm its own
  fills removes an independent check.
- Credential misuse: an observation key that can also submit or sign collapses the
  observation/action boundary (PART 221).

## Decision

- Helius is the primary implementation of `SolanaDataProvider`: streamed on-chain
  events, transaction observation, and RPC (PART 78).
- An independent fallback RPC interface is part of the design, configured with a
  distinct provider and distinct credentials. Status recovery for any submitted
  transaction consults the fallback when the primary is unavailable or disagrees.
- Finality is decided by policy from chain commitment levels observed through RPC,
  not from a provider's convenience label. Provider observations are inputs; the
  chain is the authority.
- Every inbound observation passes through the inbox with a unique
  `(source, message_id)` before it has any effect. Webhook signatures or equivalent
  authentication must be checked; unverifiable deliveries are stored as evidence
  and ignored for state.
- Raw observations are archived (ADR-0007) with `provider_published_at` and
  `received_at`, so staleness is measurable and point-in-time queries are honest.
- Provider health (PART 79) is tracked per provider; disagreement between providers
  produces `RECONCILIATION_REQUIRED`, not a guess (PART 196).
- Observation credentials are separate from action credentials and cannot submit
  transactions.

### Explicitly not decided / deferred

- The fallback RPC vendor, and whether the platform later runs its own RPC node.
- The mix of webhooks, websockets, and polling for each observation class.
- Plan tier and production key (BLOCKERS EB-005).

## Consequences

### Positive

- Reconciliation has an observation path that is independent of the execution
  router.
- Staleness and disagreement are typed states the UI and operators can see.

### Negative

- Two providers to contract, monitor, and keep contract-tested.
- Fallback RPC has lower throughput; degraded mode must be designed, not assumed.

### Operational

- Provider health dashboards and staleness alarms (PART 131, 135, 174).
- Fixture refresh when either provider changes API shape.

## Alternatives considered

- Self-hosted validator/RPC nodes as primary: high operational cost for V1; remains
  a candidate for the fallback role later.
- Another RPC vendor as primary: equivalent; Helius chosen for its event streaming;
  other vendors are the natural fallback candidates.
- Relying on the execution router for status: rejected because it removes
  independence between "who quoted and built" and "who confirms".
- Public RPC endpoints only: rate-limited and unreliable; unacceptable for status
  recovery on money paths.

## Related

- ADR-0005, ADR-0007, ADR-0008, ADR-0012, ADR-0014.
- Goal PART 11, 45, 48, 50, 51, 74, 75, 78, 79, 174, 175, 196, 199, 221.
- BLOCKERS EB-005.

## Evidence required before this ADR's guarantees can be claimed VERIFIED

- Contract tests from recorded fixtures for status, balance, and event stream
  payloads, for both the primary and the fallback provider.
- Failover test: primary unreachable; status recovery for a submitted transaction
  completes through the fallback and the order state machine advances correctly.
- Disagreement test: primary reports confirmed, fallback reports not found; the
  order enters `RECONCILIATION_REQUIRED` and no ledger posting occurs.
- Staleness tests (PART 174): observations older than policy thresholds are flagged
  and block new-risk decisions that depend on them.
- Inbox duplicate test for webhook redelivery; signature verification test for
  forged deliveries.
- Crash test (PART 49) demonstrating that external truth is discovered through this
  provider path after restart.
- `SANDBOX_VERIFIED`/`CANARY_VERIFIED` reports before any live claim; evidence-matrix
  entry for "Reality Engine" per PART 239.
