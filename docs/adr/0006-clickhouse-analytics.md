# ADR-0006: ClickHouse for analytics and normalized event history, never for balances

## Status

Accepted — implementation tracked in docs/build/REQUIREMENTS_TRACEABILITY.md

## Date

2026-09-05

## Context

Market history, normalized on-chain events, strategy decision analysis, backtests,
performance attribution, and telemetry-oriented datasets are columnar, append-heavy,
scan-heavy workloads (PART 118). Running them on the money database creates the
following failure modes:

- Lock and I/O contention between a full-history scan and a capital reservation;
  connection-pool exhaustion by analytics queries starving the trade path.
- Table bloat and vacuum pressure on tables that also hold ledger rows.
- The temptation to answer "available balance" from the store that already has the
  history, which is eventually consistent, deduplicated lazily, and ingested from
  an at-least-once stream (PART 11, 76, 118).
- Backtest leakage: a query that reads data by `occurred_at` rather than by when the
  platform actually had it (`received_at`/`available_at`) produces impossible
  backtests (PART 82, 226).
- Misreading merge-tree semantics: rows in ClickHouse may be duplicated until a
  background merge; a sum over such rows is not exact.

## Decision

- ClickHouse is the store for market history, normalized events, strategy decision
  records for analysis, backtest inputs and outputs, and telemetry-oriented data.
  Managed ClickHouse in staging and production (BLOCKERS EB-008); a container locally
  (DECISION_REGISTER D-010).
- ClickHouse is populated from the event stream (ADR-0005) and from the raw archive
  (ADR-0007). It is never written to directly by the trade path.
- ClickHouse is never consulted for available balance, buying power, reservation,
  eligibility, risk, or capability decisions. The packages `internal/ledger`,
  `internal/capital`, `internal/risk`, `internal/eligibility`, `internal/execution`,
  and `internal/signing` do not import the ClickHouse client.
- Every row carries point-in-time columns (`occurred_at`, `provider_published_at`,
  `received_at`, `available_at`) so the reality engine (PART 74) and backtests
  (PART 80–82) can query "what did the platform know at time T".
- Any user-facing figure sourced from ClickHouse is labelled as analytics, not
  accounting, and is never displayed where the UI shows balances (PART 110, 112).
- Ingestion is at-least-once; tables are designed for idempotent insert with a
  dedup key, and exact sums are computed with `FINAL` or equivalent only where the
  cost is accepted and the result is still labelled analytics.

### Explicitly not decided / deferred

- Table engines, partitioning, TTL, and retention per dataset.
- Whether backtests read ClickHouse directly or Parquet snapshots exported to S3.
- The product-analytics pipeline (PART 203).

## Consequences

### Positive

- The money database stays small and predictable.
- Strategy analysis and calibration (PART 73) can scan years of history cheaply.
- Point-in-time correctness is a schema property, not a query convention.

### Negative

- A third stateful system; ingestion lag must be measured and displayed.
- Two representations of the same event (Postgres outbox row, ClickHouse row);
  discrepancies must be measured by a reconciliation job, not assumed absent.

### Operational

- Ingestion lag and dropped-row alarms.
- ClickHouse unavailability must not block the trade path; a chaos test proves it.

## Alternatives considered

- Postgres with TimescaleDB: one fewer system, but the analytics workload competes
  with money transactions on the same instance and the scale ceiling is lower.
- Redshift / BigQuery / Snowflake: warehouse latency and cost profile suit batch
  reporting, not high-ingest time series with sub-second dashboards; the platform
  is on one cloud, and Redshift remains a possible batch target later.
- DuckDB: excellent embedded analytics, no shared serving layer for the API.
- Elasticsearch/OpenSearch: text-oriented; poor fit for exact numeric analytics.

## Related

- ADR-0001, ADR-0005, ADR-0007.
- Goal PART 11, 12, 74, 75, 76, 80, 81, 82, 86, 110, 112, 118, 203, 226.
- DECISION_REGISTER D-010; BLOCKERS EB-008.

## Evidence required before this ADR's guarantees can be claimed VERIFIED

- `test/security` architectural test: the listed money packages have no import path
  to the ClickHouse client.
- Point-in-time validity tests (PART 82): a backtest query at time T returns no row
  with `available_at > T`.
- Chaos test: ClickHouse unreachable; manual and agent trade E2E (PART 160, 161)
  still pass.
- Ingestion reconciliation job report: outbox event count versus ClickHouse row
  count per topic and window, with measured lag.
- Frontend test that analytics-sourced figures render with the analytics label and
  never in the balance components (PART 110).
- Backtest reproducibility test (PART 80): identical inputs and IR produce identical
  outputs across two runs.
