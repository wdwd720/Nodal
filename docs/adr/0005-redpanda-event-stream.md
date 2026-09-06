# ADR-0005: Redpanda event streaming with a transactional outbox and inbox

## Status

Accepted — implementation tracked in docs/build/REQUIREMENTS_TRACEABILITY.md

## Date

2026-09-05

## Context

The platform moves three classes of high-volume data: market data, normalized
on-chain events, and internal domain events consumed by analytics, agents, and the
audit pipeline (PART 77, 117). The financial core must emit events about every
money-affecting state change without making those events part of the durability
path. The failure modes:

- Dual write: commit to Postgres, then publish to the broker. If the publish fails,
  downstream consumers never learn of a fill; if the process crashes between the
  two, the same. If the order is reversed, a published event may describe a
  transaction that rolled back.
- Publish as durability: making a successful publish a precondition for commit ties
  ledger availability to broker availability (PART 117 forbids this).
- Consumer duplication: at-least-once delivery means every consumer sees some
  messages twice; a consumer that posts a fee or updates a position on each delivery
  double-counts.
- Self-hosted Kafka operations: broker tuning, storage management, and upgrade risk
  for a small team; a broker outage that becomes a money-path outage.
- Ordering assumptions across partitions leading to out-of-order position updates.

## Decision

- Redpanda (Kafka protocol) accessed with franz-go behind an `EventBus` interface
  (DECISION_REGISTER D-008). Managed Redpanda in staging and production (BLOCKERS
  EB-008); a container locally (D-010).
- Transactional outbox: every domain event describing financial state is inserted
  into the `outbox` table in the same Postgres transaction as the state change. A
  relay (`SELECT ... FOR UPDATE SKIP LOCKED`) publishes at-least-once and marks
  `published_at`. Broker unavailability delays publication; it never blocks commit.
- Transactional inbox: every consumer that has an economic effect inserts
  `(source, message_id)` under a unique constraint in the same transaction as its
  effect; a duplicate short-circuits before the handler runs (PART 199).
- Every event carries the `Envelope` fields in CONVENTIONS: schema version, source,
  aggregate, correlation and causation IDs, `OccurredAt`/`RecordedAt`, and a
  dedup key. Consumers never assume cross-partition ordering; partition keys are the
  aggregate ID so per-aggregate order holds.
- Market data and normalized blockchain data flow through the same bus but are not
  wrapped in the outbox; they are not financial state.
- The in-memory bus exists only in a test package and is rejected in PROD by
  configuration validation.

### Explicitly not decided / deferred

- Topic naming, partition counts, and retention per topic.
- Adoption of a schema registry. The envelope carries `SchemaVersion`; registry
  integration is compatible but not required for V1.
- Whether ClickHouse ingestion uses a connector or a dedicated consumer.

## Consequences

### Positive

- Ledger durability is independent of broker health.
- Exactly-once economic effect is achieved by the outbox/inbox pair and idempotency
  keys, not by broker features (PART 46).
- High-volume market and chain data does not touch Postgres.

### Negative

- Publication latency is bounded by relay polling; sub-second but not synchronous.
- A second table pair (outbox, inbox) in the money database; both need retention
  and vacuum attention.

### Operational

- Alarms on outbox age and relay lag (PART 135).
- Consumer groups per binary; rebalances must not break inbox semantics.
- TLS and SASL required for the broker connection in PROD (config validation).

## Alternatives considered

- Self-hosted Apache Kafka: same protocol, higher operational burden (JVM, storage,
  controller management). Rejected on team capacity.
- Kinesis: managed, but weaker consumer semantics and ordering controls, no Kafka
  ecosystem for analytics ingestion; cloud lock-in for a portable interface.
- NATS JetStream: capable, but a smaller ecosystem for ClickHouse ingestion and
  fewer managed offerings.
- Postgres `LISTEN/NOTIFY` or outbox polling only: sufficient for low-volume domain
  events, insufficient for market data volumes; retained as the relay mechanism.
- SQS/SNS: no ordered replay, limited retention, no partition semantics.

## Related

- ADR-0001, ADR-0004, ADR-0006, ADR-0007, ADR-0009.
- Goal PART 11, 12, 31, 36, 46, 77, 117, 135, 184, 199.
- DECISION_REGISTER D-008, D-010; BLOCKERS EB-008.

## Evidence required before this ADR's guarantees can be claimed VERIFIED

- Integration test: an event is published if and only if its transaction commits;
  a rolled-back transaction leaves no outbox row.
- Broker-down test: with the broker unreachable, financial transactions still
  commit; when the broker returns, the relay publishes every pending row exactly
  once per row (at-least-once delivery, no loss).
- Inbox duplicate test: the same message delivered twice produces one economic
  effect; the handler is not invoked on the duplicate.
- Relay crash test: kill the relay mid-batch; no row is lost and no row is marked
  published without a successful publish.
- Property test that envelope canonical serialization is stable and that dedup keys
  are unique per logical event.
- PROD configuration validation test rejecting the in-memory bus and plaintext
  broker connections.
- Load test (k6) report for ingest throughput on the market-data topics.
