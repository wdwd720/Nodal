// Package archive is the object-storage adapter of the control plane (goal
// PARTS 75, 123, 124; docs/architecture/POINT_IN_TIME.md §2). It stores raw
// provider payloads, evidence and audit artifacts as immutable objects whose
// SHA-256 is computed on the way in, sent to the store as an integrity
// checksum, kept as object metadata and returned to the caller.
//
// # Responsibilities
//
//   - ObjectArchive is the provider-neutral contract: Put, Get, Head, List and
//     Delete over (bucket, key[, version]). S3 is the production
//     implementation (aws-sdk-go-v2) and also speaks to MinIO (path-style
//     addressing, custom endpoint, static credentials resolved from
//     config.SecretRefs); on AWS it uses the task IAM role when no static
//     keys are configured.
//   - Retention: a PutRequest may carry an Object Lock retention (COMPLIANCE
//     or GOVERNANCE until a date). The audit bucket is written that way so a
//     locked object cannot be deleted before its retention expires; Delete
//     of a locked version reports ErrRetentionLocked.
//   - Layout builds the PART 124 key layout
//     raw/<provider>/<event_type>/v<schema>/YYYY/MM/DD/HH/<received_ns>-<dedup>.<ext>
//     and the provenance metadata (provider, event type, source event id,
//     schema version, platform-received / ingested timestamps, sha256) that
//     lets the origin of every object be reconstructed from the bucket
//     alone.
//   - archivetest.Memory is the in-process fake with the same retention and
//     integrity semantics, for tests only.
//
// # What this package must never do
//
//   - Never interpret a payload: bytes go in and the same bytes come out;
//     meaning belongs to internal/reality and the domain packages.
//   - Never return an object whose bytes do not hash to the sha256 recorded
//     in its metadata: Get verifies and fails with ErrIntegrity.
//   - Never silently overwrite retention: a retention shorter than the
//     bucket default is passed through to the store, which is the authority
//     on whether that is allowed; this package never strips a lock.
//   - Never log or embed credentials: keys are resolved through
//     config.Resolver at construction and handed to the SDK only.
//   - Never be financial truth: an object reference is evidence for a
//     Postgres row, not a balance.
package archive
