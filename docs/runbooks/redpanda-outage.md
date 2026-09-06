# Runbook: Redpanda (event bus) outage

Severity: SEV2 (provider outage) · Owner: OPERATIONS · Related: [clickhouse-outage.md](./clickhouse-outage.md), [temporal-outage.md](./temporal-outage.md), [stale-market-data.md](./stale-market-data.md), ADR-0005

## Trigger

- Outbox age / relay lag alarm: rows in `outbox_events` with `published_at IS NULL` older than the alert threshold; `publish_attempts` climbing with `last_error` set.
- Consumer-group lag on internal topics (`ledger.transaction.posted`, `order.transitioned`, `fill.observed`, `reconciliation.record.transitioned`, `killswitch.changed`, `audit.event.appended`, `security.event`; `internal/event/topics.go`).
- Broker unreachable / TLS or SASL failures in the relay logs; managed-Redpanda status page.
- PENDING: the alarms themselves (ADR-0005 "alarms on outbox age and relay lag", `infra/terraform/modules/observability`); the relay reports lag through its `Observer` but no collector is wired. PENDING: the production broker client wiring (`franz-go` is a dependency; the relay and bus interface are in `internal/event` and are tested against the in-memory bus).

## Blast radius

- **No financial loss by design** (PART 117, ADR-0005): every money-affecting state change commits to Postgres together with its `outbox_events` row; publication is asynchronous and at-least-once. Ledger, reservations, orders, fills, reconciliation and audit keep committing.
- Degraded: SSE streams and read-model fan-out (`internal/stream` is bus-fed) lag; ClickHouse ingestion of decisions/telemetry stops; agent event triggers (PENDING: `internal/agent`) do not fire, so event-driven strategies pause naturally; notifications dispatch later.
- Market data and normalised chain events (not outboxed; PENDING: `internal/reality`) stop flowing: strategies depending on them skip with `STALE_DATA`/`MISSING_DEPENDENCY`, which is the intended fail-closed behaviour ([stale-market-data.md](./stale-market-data.md)).
- Not affected: risk decisions, execution, finality observation, reconciliation (these read Postgres and providers directly).

## Immediate actions (first 10 minutes)

1. Confirm it is the bus and not the database. Read-only:
   ```sql
   SELECT count(*) AS unpublished, min(recorded_at) AS oldest, max(publish_attempts) AS max_attempts
     FROM outbox_events WHERE published_at IS NULL;
   SELECT topic, count(*), max(publish_attempts), max(left(last_error, 120))
     FROM outbox_events WHERE published_at IS NULL GROUP BY topic ORDER BY 2 DESC;
   ```
   Rising `unpublished` with `last_error` naming the broker ⇒ bus outage. Rising `unpublished` with no relay errors ⇒ the relay itself is down (restart it).
2. Confirm the money path is healthy: `SELECT max(posted_at) FROM journal_transactions;` advancing; `/readyz` (PENDING: `cmd/api`) is 200 because readiness never depends on the bus.
3. Do **not** activate any kill switch for a bus outage alone. If trading depends on market data that is no longer arriving, the risk kernel already refuses with `RISK_STALE_DATA`; a `GLOBAL_NEW_RISK_KILL` is only warranted if you find that stale data is *not* being detected (that is a SEV1 defect, not a bus problem).
4. Check the broker: managed-Redpanda console/status page (PENDING: EB-014 account); locally `docker compose ps redpanda` and `make infra-logs`.
5. Announce: "bus outage, financial path unaffected, streams/analytics lagging since <oldest unpublished>".

## Diagnosis

- Relay behaviour: per-row retry with doubling backoff from `recorded_at` and a run-level backoff that protects against a hot loop (`internal/event/relay.go`); consecutive failing runs are expected during an outage.
- Partition/key ordering: the relay preserves per-`partition_key` order, so a stuck head-of-line row (poison payload) blocks its key only. Look for a single row with `publish_attempts` far above the rest.
- TLS/SASL: `REDPANDA_TLS` is required in STAGING/PROD (`config.Validate`); a certificate rotation on the broker shows as immediate failures after a deploy.
- Consumer side: inbox dedup (`inbox_messages` PK `(source, message_id)`) makes redelivery after recovery safe; consumers with an economic effect are idempotent by construction (PART 199). `SELECT status, count(*) FROM inbox_messages GROUP BY status;` — `FAILED` rows need a look.
- Disk/retention on the broker: retention deletion during a long outage loses *transport*, not truth; the outbox replays what it still holds.

## Containment and recovery

1. Restore the broker (managed provider incident, or `docker compose up -d redpanda` locally). No application change is needed for the relay to resume; it republishes every unpublished row.
2. Watch `unpublished` drain to zero and consumer lag recover. Expect a burst; consumers must not be scaled down during the drain.
3. If a poison row blocks a key: do not delete it. Fix the consumer/serialiser, or move the row aside via the `cp_ops` housekeeping path (ops may DELETE from outbox/inbox only for cleanup, and only with a ticket and after the payload is archived). Record it.
4. If the outage exceeded broker retention, expect ClickHouse gaps: open `stream_gaps` rows of kind `SILENCE`/`GAP` (PENDING: ingestion, `internal/reality`) and label affected backtest windows impure; nothing financial needs repair.
5. Verify SSE clients resynced (`resync` on gap is built into `internal/stream`).

## What NOT to do

- Never make a successful publish a precondition for a financial commit, and never "pause posting until the bus is back" (PART 117).
- Never publish outbox rows by hand or replay from a broker snapshot; the outbox is the source.
- Never truncate `outbox_events` or `inbox_messages` to clear a backlog.
- Never disable reconciliation or settlement because their downstream events are late.
- Never treat a stale SSE view as a reason to resubmit an order (the order state in Postgres is authoritative).

## Verification / exit criteria

- `SELECT count(*) FROM outbox_events WHERE published_at IS NULL` at or near zero and stable for 15 minutes; no `last_error` newer than the recovery time.
- Consumer lag at zero on every internal topic.
- `inbox_messages` shows no new `FAILED` rows; any pre-existing ones triaged.
- Market-data freshness back within policy ([stale-market-data.md](./stale-market-data.md) exit criteria) if the bus also carries market data.

## Post-incident

- Record outage window, oldest unpublished age reached and drain time; feed them into the alert thresholds (PENDING: alarms).
- Review any `cp_ops` DELETE performed and its ticket.
- Update `docs/build/REQUIREMENTS_TRACEABILITY.md` R-117-1 with the observed at-least-once behaviour; add a chaos test (`test/chaos/redpanda_down_test.go`) if absent.
