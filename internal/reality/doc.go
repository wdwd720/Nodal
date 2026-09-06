// Package reality is the point-in-time reality engine (goal PARTS 74–79,
// 120, 122, 124, 174, 175, 199, 200, 226; docs/architecture/POINT_IN_TIME.md).
// It turns raw provider payloads into normalized events that carry
// knowledge-time semantics, and it records everything a later decision or
// backtest needs in order to know what was knowable when.
//
// # Responsibilities
//
//   - RawArchive writes every raw payload to object storage before it is
//     interpreted and indexes it in raw_archive_objects (URI, sha256,
//     provider, source event id, dedup key, schema version, the timestamps
//     known at ingest, retention class and until). Verify re-hashes an
//     object and raises ARCHIVE_INTEGRITY_VIOLATION on any difference.
//   - Normalizer produces NormalizedEvent values with the six timestamps.
//     source_event_at and provider_published_at come from the provider and
//     are untrusted; platform_received_at, normalized_at,
//     feature_available_at and decision_available_at come from the
//     platform clock and an explicit AvailabilityPolicy, and always satisfy
//     platform_received_at <= normalized_at <= feature_available_at <=
//     decision_available_at.
//   - Dedup ids follow the data source's declared strategy (PROVIDER_ID or
//     COMPOSITE_HASH). Duplicates are not an error anywhere: the raw object
//     is archived once, the bus is at-least-once, ClickHouse replaces on
//     (source, event_type, dedup_id) and Postgres consumers use event.Inbox.
//   - Detector and PgCheckpointStore track the ingest position per (data
//     source, stream, partition, consumer) and record GAP, SILENCE,
//     RECONNECT, REPLAY and ORDERING_ANOMALY rows in stream_gaps.
//     Continuity answers whether a window overlaps an OPEN or UNRECOVERABLE
//     gap so a strategy or backtest can refuse to pretend continuity.
//   - PgDataSourceStore is the licensing and retention registry
//     (data_sources): persistence is BLOCKED unless historical use is
//     permitted, both here and by the database CHECK.
//   - ClickHouseStore applies the Appendix A DDL and inserts normalized
//     events and market prices in batches with exact numerics (Int128
//     mantissas, decimal strings in payloads). QueryNormalized and
//     QueryMarketPrices only ever return rows with decision_available_at <=
//     the caller's as-of instant.
//   - HealthSampler and PgHealthStore write provider_health_samples so
//     risk, settlement and the snapshotter share one view of provider health.
//   - Pipeline is the Ingestor: archive → normalize → publish → sink →
//     checkpoint, at-least-once.
//
// # What this package must never do
//
//   - Never treat ClickHouse (or the bus, or memory) as accounting truth:
//     nothing here is read by a balance, reservation, risk or settlement
//     decision, and ClickHouse rows can always be rebuilt from the archive.
//   - Never let a provider timestamp drive knowledge time: freshness and
//     availability are computed from platform_received_at and the
//     platform's own clocks; provider clocks are stored beside them for
//     evidence only.
//   - Never return an observation with decision_available_at later than
//     the as-of instant of a historical query (PART 226).
//   - Never set feature_available_at or decision_available_at earlier than
//     platform_received_at, and never leave them implicit: the policy that
//     sets them is an explicit, versioned input.
//   - Never transform evidence away: the raw object is kept whether or not
//     the event normalizes, deduplicates or is later flagged.
//   - Never grant persistence to a data source whose historical use rights
//     are NO or UNKNOWN, and never register a data source with an AGENT
//     actor.
//   - Never carry floating-point values: quantities are money.Quantity,
//     payload numbers are strings, ClickHouse financial columns are
//     Int128/UInt/Decimal.
//   - Never pretend continuity across a gap: an open or unrecoverable gap
//     is surfaced, not skipped.
package reality
