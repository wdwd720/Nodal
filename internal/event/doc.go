// Package event is the transactional outbox/inbox and the event bus
// abstraction of the control plane (goal PART 31, PART 77, PART 117,
// PART 199).
//
// # Responsibilities
//
//   - Envelope is the canonical domain event: typed identifier, event type,
//     schema version, source, aggregate, correlation/causation ids, UTC
//     timestamps and a JSON payload. Validate enforces the contract and
//     CanonicalBytes produces the deterministic JSON (sorted keys, no HTML
//     escaping) that is hashed, stored in the outbox and published on the
//     bus.
//   - The Topic registry names every domain topic, its schema version, the
//     aggregate it is keyed by and the partition-key rule. An event can only
//     be enqueued on a registered topic with a matching type and version.
//   - Outbox.Enqueue writes rows into outbox_events inside the caller's
//     transaction, so a financial state change and its event are committed
//     or rolled back together. A dedup_key collision surfaces as a CONFLICT
//     *errs.Error so the caller's transaction rolls back.
//   - Relay claims unpublished rows with SELECT ... FOR UPDATE SKIP LOCKED,
//     publishes them through a Bus and marks published_at. Failures increment
//     publish_attempts, record last_error and back off exponentially. A row
//     whose predecessor for the same (topic, partition key) is still
//     unpublished is deferred so per-key order is preserved.
//   - Inbox.Process records an inbound message under the (source, message_id)
//     primary key before running the consumer's function in the same
//     transaction. Duplicates never run the function again; failed messages
//     can be re-processed; a message that is in flight elsewhere is reported
//     as IDEMPOTENCY_IN_PROGRESS.
//   - Bus is the transport interface (Redpanda in production, the in-memory
//     bus of internal/event/eventtest for LOCAL/TEST/DEV). Observer is the
//     optional metrics hook so observability can be wired without this
//     package importing it.
//
// # Delivery semantics
//
// Publication is at-least-once. The relay publishes and then marks the row
// in the same database transaction; a crash between the two re-publishes the
// event on the next run. Consumers therefore MUST dedup through Inbox.Process
// keyed by the event id (header event_id).
//
// Per-key ordering holds, including across concurrent relay instances, for
// producers that serialize writes to an aggregate — which every financial
// transaction does through row locks or SERIALIZABLE isolation. Claiming
// with FOR UPDATE SKIP LOCKED means a batch is the oldest rows nobody else
// holds rather than a contiguous run of the outbox, so a relay can hold a row
// in the middle of another relay's partition; blockedSQL therefore asks of
// EVERY claimed row whether an older row of its key is still unpublished
// elsewhere, and the publish loop defers the rest of a partition as soon as
// one of its rows is held back. See the comment on blockedSQL for the two
// details that carry it, and
// TestIntegration_RelayNeverPublishesPastARowHeldByAnotherInstance for the
// regression that fixed it (D-036, 2026-09-06); before that fix the check
// consulted only the oldest batch row of each partition and a partition split
// across instances could publish out of order.
//
// Ordering is per key, never global: two different aggregates have no
// relative order, and redelivery can still repeat an event, which consumers
// absorb through the inbox.
//
// # What this package must never do
//
//   - Never make successful publication a prerequisite of Postgres
//     durability: Enqueue only inserts rows; nothing in this package talks to
//     the bus inside a financial transaction (PART 117).
//   - Never process an inbound event outside the inbox: every consumer
//     side effect runs inside Inbox.Process, in the transaction that records
//     the message, so duplicate delivery never produces a duplicate economic
//     effect (PART 31, PART 199).
//   - Never mutate recorded_at, occurred_at or the payload of an outbox row;
//     retries only touch published_at, publish_attempts and last_error.
//   - Never delete outbox or inbox rows: cp_app has no DELETE privilege and
//     retention is an operations job.
//   - Never treat the bus, Redis or memory as financial truth, and never
//     interpret payloads: this package moves bytes, domain packages own
//     meaning.
//   - Never carry floating-point values; payload numbers are passed through
//     verbatim as JSON number text and money is encoded as strings by the
//     producers (internal/money).
package event
