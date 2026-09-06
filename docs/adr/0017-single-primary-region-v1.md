# ADR-0017: Single primary write region for V1; no active-active money writes

## Status

Accepted — implementation tracked in docs/build/REQUIREMENTS_TRACEABILITY.md

## Date

2026-09-05

## Context

Goal PART 138 forbids global active-active financial writes before there is a real
need and a rigorous architecture. The failure modes of multi-primary money:

- Write conflicts on the same envelope from two regions, each seeing sufficient
  available balance; conflict resolution after the fact is a double-spend with a
  merge policy.
- Split brain during partition: two primaries accept reservations; when the
  partition heals, one set must be undone, but the external transactions they
  authorised are already on chain.
- Replication lag treated as consistency: a reader in region B sees a balance that
  region A has already reserved.
- Complexity that consumes the V1 budget and cannot be tested to the standard the
  ledger requires (PART 23, 49).

Single-region has its own failure modes that must be mitigated rather than ignored:

- Regional outage means unavailability. This is accepted: the platform prefers a
  halt to an inconsistent ledger. Kill-switch semantics (PART 52) still allow
  reconciliation to run when the region returns.
- Data loss on primary failure. Mitigated by synchronous Multi-AZ standby, PITR,
  and cross-region backup copies (PART 139, 141).
- Failover during in-flight submissions producing duplicate external transactions.
  Mitigated by the order state machine and status recovery (PART 46, 48, 172).

## Decision

- One primary write region on the chosen cloud, provisioned with Terraform
  (PART 137). RDS PostgreSQL Multi-AZ with synchronous standby, encryption at rest,
  automated backups, and PITR (PART 139).
- Backups are copied to a second region on a schedule; the archive bucket
  (ADR-0007) is in the primary region with replication deferred (see below).
- Design objective for committed financial transactions: RPO 0 within the supported
  failure model (single-AZ or single-instance failure). Broader disaster recovery
  relies on backups and PITR; the RPO for that case is the backup interval and is
  documented, not assumed (PART 205).
- RTO is documented only after a restore drill measures it (PART 141, 219). No
  number is claimed before that.
- Database failure semantics (PART 172): the application fails closed on primary
  unavailability; no money path proceeds on a cached or replica read.
- Read replicas, if introduced, serve only non-money reads (analytics-adjacent
  queries, support views) and are never consulted by capital, ledger, risk,
  eligibility, or execution packages.
- No global active-active writes in V1. Any future multi-region design requires a
  new ADR with a proof that reservation semantics survive partition.

### Explicitly not decided / deferred

- Which cloud region (BLOCKERS EB-007).
- A warm standby in a second region with promotion runbook; possible later, after
  the restore drill establishes baseline numbers.
- Regions for managed Temporal, Redpanda, and ClickHouse (EB-008); they follow the
  primary region.
- Archive bucket cross-region replication.

## Consequences

### Positive

- Reservation atomicity holds without distributed coordination.
- Operational surface is small enough to drill and understand.

### Negative

- A regional outage halts new risk for its duration. This is the accepted trade.
- Latency for users far from the region; mitigated at the edge for reads, never
  for writes.

### Operational

- Restore drills are scheduled and their results attached to the readiness report.
- Multi-AZ failover is tested with in-flight traffic before production.

## Alternatives considered

- Active-active across regions: excluded by PART 138 for the reasons above.
- Globally distributed SQL (CockroachDB, Spanner): PART 13 lists custom consensus as
  premature; the cost and semantics are unjustified without global write need.
- Active-passive with asynchronous replica promotion: viable later; requires a
  proof that reconciliation converges external state after promotion, which the
  restore drill and reconciliation tests (PART 141, 163) will establish first.

## Related

- ADR-0001, ADR-0004, ADR-0005, ADR-0007, ADR-0016.
- Goal PART 12, 13, 52, 137, 138, 139, 141, 172, 205, 219.
- BLOCKERS EB-007, EB-008.

## Evidence required before this ADR's guarantees can be claimed VERIFIED

- Restore drill report (PART 219): database restored from backup and from PITR to a
  point in time, application pointed at the restored copy, reconciliation converged
  external state; measured RTO and RPO recorded.
- Multi-AZ failover test with in-flight submissions: no duplicate external
  transaction; every order reaches a typed terminal or `RECONCILIATION_REQUIRED`
  state.
- Database-unavailable test (PART 172): every money endpoint fails closed with a
  typed error; nothing is served from cache or replica.
- Architectural test: money packages have no replica connection string.
- Terraform plan review confirming Multi-AZ, encryption, backup retention, and
  cross-region backup copy.
- Backup-verification job report on a schedule.
