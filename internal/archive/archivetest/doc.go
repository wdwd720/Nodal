// Package archivetest provides Memory, an in-process archive.ObjectArchive
// with the same integrity and retention semantics as the S3 adapter:
// objects are versioned per key, a version under retention cannot be
// deleted before its date, and Get verifies bytes against the sha256
// metadata. Corrupt flips bytes of a stored object so integrity checks can
// be exercised.
//
// It is test-only and must never be imported by production code; the
// composition root wires archive.S3 from config.
package archivetest
