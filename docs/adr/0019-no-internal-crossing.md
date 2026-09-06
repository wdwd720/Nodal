# ADR-0019: No internal order matching, crossing, or principal trading

## Status

Accepted — implementation tracked in docs/build/REQUIREMENTS_TRACEABILITY.md

## Date

2026-09-05

## Context

Goal PART 4 excludes from V1: internal customer order matching, customer-to-customer
transfers, principal trading against customers, and hidden internal crossing.
PART 3 states the product is not an exchange. The failure modes of crossing orders
internally, even as an optimisation:

- Regulatory: matching customer orders against each other is exchange or
  alternative-trading-system activity; trading against customers from inventory is
  dealing. Both carry licensing and best-execution obligations that are unresolved
  (BLOCKERS EB-009, EB-011).
- Conflict of interest: a platform that can fill a customer internally has an
  incentive to do so at a price worse than the external market, and no external
  evidence to prove otherwise.
- Opacity: an internal cross has no chain transaction signature; the fill's
  existence rests on the platform's own database. Reconciliation against external
  truth (PART 50) has nothing to reconcile.
- Netting hides errors: if opposing intents are netted and only the residual is
  sent to the venue, a bug in netting is invisible on chain and unbounded in the
  ledger.
- Customer-to-customer transfers are money transmission; a transfer rail inside the
  platform creates obligations the platform has not accepted.
- Market integrity (PART 182): internal fills could be used to paint prices or
  volumes the external market never saw.

## Decision

- Every fill results from a transaction on an external venue through an execution
  adapter (PART 41, ADR-0008). There is no internal order book, no matching engine,
  and no netting of opposing customer intents before external submission.
- The platform never trades as principal against a customer. No platform-owned
  inventory account exists in the customer trading path; operational wallets
  (fees, gas) are ledger accounts with no venue interaction on behalf of customers.
- No customer-to-customer transfer endpoint, intent type, or ledger posting rule
  exists in V1.
- Every posted fill references an external transaction signature and provider
  identifiers (PART 46); a posting without external evidence is rejected by the
  ledger service, not merely discouraged.
- The intent, plan, and venue interfaces are generic so a future licensed venue
  could be added as an adapter. Internal matching itself is not an adapter and is
  not reserved a slot; a future decision to add it requires a superseding ADR and
  legal determination.

### Explicitly not decided / deferred

- Integration with a licensed venue or broker in the future; it would be an
  external adapter like any other.
- Same-account intent batching (combining two intents from one customer into one
  external transaction). This is not a cross, but it is deferred; if adopted, each
  leg must still map to external evidence and the ledger must post per leg.
- Fee models (PART 126), which are independent of execution venue.

## Consequences

### Positive

- Every economic effect has an external proof; reconciliation is complete by
  construction.
- No dealing or exchange obligations are created by the execution path.
- Best-execution questions are answered by the venue's fill and stored quote
  evidence, not by the platform's word.

### Negative

- No spread capture or fee savings from internal matching; every trade pays venue
  costs and waits for external finality.
- Customers cannot move assets to each other inside the platform.

### Operational

- The invariant "every fill has a signature" is monitored; any violation is a P1
  reconciliation alert.

## Alternatives considered

- Internal matching for spread savings: rejected on regulatory, conflict, and
  evidence grounds.
- Dark-pool style crossing with external price reference: still an internal fill
  with no external proof; rejected.
- Platform market-making desk: principal trading, excluded by PART 4.
- Internal transfers as a convenience feature: money transmission; excluded by
  PART 4.

## Related

- ADR-0008, ADR-0014, ADR-0016, ADR-0018.
- Goal PART 3, 4, 10, 41, 46, 50, 126, 163, 182.
- BLOCKERS EB-009, EB-011.

## Evidence required before this ADR's guarantees can be claimed VERIFIED

- Ledger invariant test: every posted fill row carries a non-empty external
  transaction signature and provider reference; a posting attempt without them is
  rejected with a typed error.
- `test/security` architectural test: no package named or behaving as a matching
  engine or internal order book exists; no intent type for customer-to-customer
  transfer is registered.
- Full-balance reconciliation E2E (PART 163): the sum of ledger changes per asset
  over a window equals the sum of on-chain changes across customer wallets.
- Settlement compiler golden corpus (ADR-0014) containing no plan step that fills
  internally; the plan-validator rejects a synthetic "internal fill" step.
- Market-integrity review (PART 182) recorded with the compliance gates.
