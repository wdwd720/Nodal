// Package audit is the append-only, per-stream hash-chained audit log
// (goal PARTS 87, 89, 222, 223; POLICY_AUTHORITY §6).
//
// # Responsibilities
//
//   - CanonicalJSON: deterministic serialization used for every audit hash
//     and for admin params hashes. Object keys are sorted recursively (maps
//     and structs alike), HTML is not escaped, time.Time renders as
//     RFC3339Nano in UTC, []byte as base64, and numbers are accepted only as
//     integers of magnitude <= 2^53: any float with a fractional part or a
//     larger magnitude is rejected, because financial values are strings.
//     Hashing the same logical record therefore never changes because a
//     producer ordered its fields differently.
//   - Writer.Append: inside the caller's transaction, take
//     pg_advisory_xact_lock(hashtext(stream)), read the last (stream_seq,
//     content_hash) of the stream, and insert the next row with
//     content_hash = sha256(CanonicalJSON(record)) where the record is every
//     persisted column except id, recorded_at and content_hash. stream,
//     stream_seq and prev_hash are part of the hashed record, so the chain
//     link is bound into every hash. build_version is stamped from
//     config.BuildVersion (PART 223).
//   - Verifier: recompute every content hash, every prev_hash link and the
//     sequence contiguity of a stream (or of all streams in batches) and
//     report the first broken sequence number.
//   - Stream naming: AccountStream, AgentStream, AdminStream, SystemStream.
//
// Isolation: Append is correct under READ COMMITTED, where the advisory
// lock fully serializes appends to one stream. Under REPEATABLE READ or
// SERIALIZABLE the transaction snapshot predates the lock, so a concurrent
// append to the same stream surfaces as a serialization failure (SQLSTATE
// 40001) that db.InTx retries; it can never produce a fork or a gap because
// (stream, stream_seq) is unique.
//
// # What this package must never do
//
//   - Update or delete audit rows: the table has no UPDATE/DELETE grant for
//     cp_app and a forbid_mutation trigger; corrections are new events.
//   - Accept an event without stream, actor, action, resource and
//     occurred_at, or with an actor type that is not declared by
//     internal/security. AGENT actor events are accepted: they are evidence.
//   - Hash floats, unsorted keys, or local-time timestamps.
//   - Read the clock implicitly: occurred_at is supplied by the caller;
//     recorded_at is the database's.
//   - Hold a connection or pool: every call runs on the caller's transaction
//     or Querier.
package audit
