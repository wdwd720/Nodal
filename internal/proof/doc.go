// Package proof turns the per-stream audit hash chain of internal/audit into
// tamper-evident, independently verifiable history (goal PARTS 87, 88, 123,
// 201, 222, 223; POLICY_AUTHORITY §6): periodic signed Merkle checkpoints,
// WORM-archived checkpoint documents, a full verifier, and the PART 88 proof
// bundle for one fill or intent.
//
// # Merkle convention (RFC 6962, no node duplication)
//
//   - leaf hash:  sha256(0x00 || audit_events.content_hash)
//   - inner hash: sha256(0x01 || left || right)
//   - MTH(D[n]) for n > 1 splits at k = the largest power of two strictly
//     below n: sha256(0x01 || MTH(D[0:k]) || MTH(D[k:n])). An odd leaf is
//     therefore never duplicated; it is promoted. MTH({}) = sha256("").
//   - leaves of one checkpoint are the content hashes of every covered event
//     ordered by (stream bytewise, stream_seq): Go string order, never the
//     database collation.
//   - membership proofs are RFC 6962 audit paths from leaf to root and are
//     verified with the RFC 9162 §2.1.3.2 algorithm from (leaf index, leaf
//     count), with the sibling direction flags cross-checked.
//
// # Checkpoints
//
// Checkpointer.Run takes a transaction-scoped advisory lock (two workers
// never race), reads each stream's watermark from audit_checkpoint_streams,
// collects every event above it, builds the root, chains prev_root to the
// previous checkpoint's merkle_root, signs
// sha256(CanonicalJSON{seq, prev_root, merkle_root, streams_covered,
// leaf_count, build_version}) through a Signer (KMS ECDSA_SHA_256 over the
// DIGEST, or a local P-256 key outside STAGING/PROD), archives the canonical
// document plus signature with retention, and inserts the row. The archive
// write happens first: if the row insert then fails, the archived object is
// orphaned but harmless. Object keys embed the checkpoint id, so a retry
// never collides with the orphan, and the verifier only fetches objects
// named by committed rows.
//
// # Verification
//
// Verifier.VerifyAll recomputes every stream chain (audit.Verifier), verifies
// every checkpoint signature, the prev_root/prev_checkpoint_id chain, every
// Merkle root from the events themselves (with per-stream coverage
// contiguity), re-fetches every archived object and compares its sha256 and
// content with the row, and records an audit_verification_runs row. It stops
// at the first failure and names it (stream/seq, checkpoint, object, key id).
//
// # Signing keys and rotation (D-029)
//
// audit_checkpoints rows are append-only and outlive every key: a database
// verified today holds checkpoints signed months ago. Verification therefore
// resolves the verifying key by the key id recorded on each row, out of a
// KeySet an operator configures, rather than applying whichever key the
// process happens to hold. Trust is a set, not a single key:
//
//   - KeyActive signs new checkpoints and verifies old ones.
//   - KeyRetired was rotated out. It signs nothing further and still
//     verifies every checkpoint it signed; retiring a key says nothing about
//     the signatures it already made, so a routine rotation never turns
//     history unverifiable.
//   - KeyRevoked is withdrawn (compromised, or its signatures are no longer
//     accepted as evidence). Checkpoints naming it fail, and no key material
//     is needed to revoke.
//
// The three ways a checkpoint's key can fail are reported separately, and
// none of them is allowed to masquerade as another: FailCheckpointKeyUnknown
// (the id is not in the trusted set — typically a rotation whose key id was
// never added; nothing about the signature has been judged),
// FailCheckpointKeyRevoked (a deliberate withdrawal), and
// FailCheckpointSignature (a trusted key, and the signature does not verify:
// the only one of the three that is evidence of tampering). An audit system
// that reported "tampered" after a scheduled key rotation would train its
// operators to ignore it, so the distinction is load-bearing, not cosmetic.
//
// Resolving by id is also what keeps the check honest: a forged row cannot
// name a key of its own choosing and have it accepted, because only key ids
// an operator put in the set are ever used to verify.
//
// # What this package must never do
//
//   - Mutate or delete audit_events, audit_checkpoints,
//     audit_checkpoint_streams or audit_verification_runs rows: every table
//     is append-only with a forbid_mutation trigger and no UPDATE/DELETE
//     grant. A bad checkpoint is exposed by verification failing loudly,
//     never repaired by editing.
//   - Sign anything but the canonical checkpoint digest: Signer.Sign accepts
//     a 32-byte digest only and Checkpointer passes Document.Digest() and
//     nothing else.
//   - Claim verification without re-fetching the archived object and
//     recomputing roots from audit_events rows; stored hashes are compared
//     against recomputation, never trusted.
//   - Verify a checkpoint with any key but the one its row names, or accept
//     a key id no operator has trusted. Widening the signature check to make
//     history verify again is never the fix for a rotation gap: add the key
//     id to the trusted set.
//   - Report an untrusted or revoked key as a failed signature.
//   - Fabricate a proof bundle link: a missing row is reported as absent.
//   - Use a local signing key in STAGING/PROD, or log key material.
//   - Import internal/archive directly: the Archive interface here is wired
//     by the composition root.
package proof
