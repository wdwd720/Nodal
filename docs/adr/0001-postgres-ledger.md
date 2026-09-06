# ADR-0001: PostgreSQL is the sole financial ledger of record

## Status

Accepted — implementation tracked in docs/build/REQUIREMENTS_TRACEABILITY.md

## Date

2026-09-05

## Context

The platform's internal accounting truth (goal PART 11.2) is the set of ledger
accounts, journal transactions and entries, reservations, capital envelopes,
financial event references, and reconciliation records (PART 19). Every other store
in the system is derived. The failure modes this store must be designed against are
concrete:

- Oversubscription under contention: 100 agents each reserving $500 against $10,000
  must yield exactly 20 reservations (PART 23). Redis locks, process mutexes, and
  optimistic caches all admit windows in which two reservations see the same
  "available" value.
- Torn writes: a reservation persisted without its intent state, or a ledger posting
  persisted without its outbox event, leaves the system unable to explain a balance
  or unable to notify downstream consumers. The reservation sequence in PART 22 must
  be all-or-nothing.
- Silent balance edits: an operator or attacker with database access updating a
  posted row destroys the audit trail. PART 129 forbids editing balances; corrections
  must be compensating entries.
- Truth drift: analytics stores, caches, workflow state, and frontend state are
  eventually consistent or lossy. If any of them answers "can this account spend?",
  a lagging replica becomes a double-spend (PART 11).
- Non-exact arithmetic: floating-point money accumulates rounding error that cannot
  be reconciled against on-chain integer balances (PART 17).
- Unrecoverable history: a rollback that drops a ledger table, or a migration that
  rewrites posted rows, is indistinguishable from fraud in an audit.

## Decision

- PostgreSQL (version 16 locally; RDS PostgreSQL Multi-AZ in production) is the only
  authoritative store for the PART 19 entities, the transactional outbox and inbox,
  and idempotency records.
- The ledger is append-only double-entry. Posted journal rows are immutable; changes
  are compensating transactions. The application role has no `UPDATE`/`DELETE` grant
  on posted-ledger tables and triggers raise on any attempt (CONVENTIONS, Migrations).
- Capital reservation is a single database transaction using `SERIALIZABLE`
  isolation (retried on `40001`/`40P01`) or explicit row locks proven equivalent, in
  the order given by PART 22. Financial state change and outbox event commit together.
- Access is explicit SQL via pgx v5 and sqlc-generated queries, with hand-written SQL
  permitted for multi-statement transactional paths (DECISION_REGISTER D-004). No ORM.
- Numerics per D-007: USD minor units `BIGINT`, asset quantities `NUMERIC(38,0)`,
  never floating point.
- Migrations are forward-only for ledger tables; the runner refuses to migrate down
  below the ledger-protected version (D-005).
- Separate database roles for migration, application, read-only support, and
  break-glass (PART 101). Generic ledger mutation is never exposed by application
  code.

### Explicitly not decided / deferred

- Table partitioning or archival of cold journal history.
- Use of read replicas for non-financial queries (permitted in principle; not
  designed here).
- Any future move to a purpose-built ledger engine. The repository interfaces
  (`Querier`, transactional posting service) are the seam; no timeline.

## Consequences

### Positive

- One transaction boundary covers reservation, intent state, and event emission.
- Constraints, foreign keys, and unique indexes enforce invariants in the store,
  not only in code.
- Mature backup, PITR, and Multi-AZ failover on the chosen cloud (PART 139, 141).

### Negative

- Write throughput is bounded by a single primary; this is accepted for V1 (ADR-0017)
  and the hot path is trade intents, not market ticks.
- `SERIALIZABLE` retries add latency under contention and require retry-safe code.

### Operational

- Connection pooling with hard limits is mandatory (PART 139).
- Restore drills are part of the evidence, not optional (PART 141, 219).
- Any schema change to a busy table needs lock and backfill analysis (PART 140).

## Alternatives considered

- TigerBeetle: a purpose-built double-entry database with strong guarantees, but it
  is a second system of record beside Postgres (two truths, cross-store atomicity
  problems for intent/outbox), lacks SQL for reconciliation joins, and its
  operational story is unfamiliar to the team. Goal PART 13 lists it as premature.
- Event log (Redpanda/Kafka) as the ledger: replayable but has no constraints, no
  serializable reads, and would make "available balance" a consumer-side derivation.
- Redis: explicitly forbidden as ledger or reservation authority (PART 119).
- CockroachDB / Spanner-class databases: serializable and multi-region, but V1 has
  no need for global writes (PART 138), and the cost and operational model are
  heavier than RDS.
- Blockchain-based ledger: PART 13 rejects blockchain audit "merely for marketing";
  tamper evidence is provided by ADR-0007 instead.

## Related

- ADR-0003, ADR-0005, ADR-0006, ADR-0007, ADR-0015, ADR-0017.
- Goal PART 6, 11, 12, 13, 17, 19, 20, 21, 22, 23, 31, 36, 101, 129, 138–141, 172.
- DECISION_REGISTER D-001, D-004, D-005, D-007, D-008.

## Evidence required before this ADR's guarantees can be claimed VERIFIED

- `TestProp_JournalBalanced`: every journal transaction's entries sum to zero per
  asset (property test, `internal/ledger`).
- Integration test that `UPDATE`/`DELETE` on posted ledger rows fails under the
  application role and under a direct SQL attempt with trigger protection.
- Concurrency torture test per PART 23, run with `-race`, thousands of randomized
  iterations, in CI.
- Crash test per PART 49 demonstrating exactly one fill, one position change, and
  correct postings after a crash between external submission and local persistence.
- Migration test: up, down refused below ledger-protected version, up again; ledger
  row count unchanged.
- Restore drill report with measured RPO/RTO (PART 205, 219), attached to the
  readiness report.
- Evidence-matrix entry for "Ledger" and "Reservation" per PART 239.
