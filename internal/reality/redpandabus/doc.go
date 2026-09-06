// Package redpandabus is the Kafka-protocol event.Bus of the control plane
// (goal PARTS 77, 117, 199; docs/architecture/POINT_IN_TIME.md §3–4), built
// on franz-go against Redpanda.
//
// # Responsibilities
//
//   - Publish produces one record synchronously with the idempotent
//     producer and acks from every in-sync replica: it returns only after
//     the broker acknowledged the write, which is what the outbox relay
//     relies on before marking a row published.
//   - Subscribe joins a consumer group per (topic, group) with auto-commit
//     disabled. A record's offset is committed only after the handler
//     returned nil; a failing handler is retried with backoff on the same
//     record, so delivery is at-least-once and consumers must dedup
//     (event.Inbox for Postgres consumers, dedup_id for stream consumers).
//   - Headers map one-to-one onto Kafka record headers; the message id is
//     the event_id header (falling back to topic/partition/offset).
//   - The partition key is the aggregate id, taken from internal/event's
//     topic registry rather than from a second scheme kept here:
//     ValidatePartitionKey reads Spec.AggregateType and refuses a key that
//     is not the aggregate id the headers name. Kafka orders within a
//     partition only, so this is the whole of the ordering guarantee.
//   - Loopback is the fake-mode bus for LOCAL, TEST and DEV: an in-process
//     transport with the same at-least-once semantics and the same partition
//     rule. Open selects between the two by config.ProviderMode and never
//     falls back from one to the other; NewLoopback refuses fake mode in
//     STAGING and PROD (config.Validate does too), and New refuses to return
//     a client whose brokers did not answer a ping.
//
// # What this package must never do
//
//   - Never be financial truth: nothing is durable here beyond broker
//     retention and nothing here is read as a balance (PART 117).
//   - Never commit an offset before the handler succeeded, and never drop a
//     record silently: a poison record blocks its partition and is logged,
//     it is not skipped.
//   - Never interpret payloads: bytes and headers pass through verbatim.
//   - Never run without TLS/SASL when the configuration requires them, and
//     never log SASL credentials.
//   - Never construct the loopback outside LOCAL/TEST/DEV, and never fall
//     back to it when the real broker is unreachable: a bus that appears to
//     work and delivers nothing is worse than one that will not start.
package redpandabus
