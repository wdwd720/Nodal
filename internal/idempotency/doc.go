// Package idempotency implements the idempotency contract for money-affecting
// commands (PART 36, PART 173) on top of the idempotency_keys table.
//
// # Responsibilities
//
//   - Begin claims (actor, endpoint, key) inside the caller's transaction and
//     reports exactly one of: Acquired (this request owns the key), Replay (a
//     completed result for the same canonical request), InProgress (the same
//     request is still running elsewhere), or ErrKeyReuseConflict (the key was
//     used with a different request hash: a deterministic conflict, never a
//     silent reuse for a different financial intent).
//   - Complete records the semantic result (status, resource, body) so later
//     retries replay it. Business rejections that conclude the command (a 4xx
//     such as INSUFFICIENT_BUYING_POWER) are Complete, not Fail.
//   - Fail records that the command did NOT conclude (internal error, panic);
//     the key may then be re-acquired by a retry with the same request.
//   - Expired rows are treated exactly as if the operations cleanup job had
//     already deleted them: they can be re-acquired regardless of status.
//   - HashRequest canonicalises method, path and JSON body so semantically
//     identical retries hash identically.
//
// # What this package must never do
//
//   - Delete rows: cp_app has no DELETE privilege; retention is an ops job.
//   - Decide HTTP semantics: callers map Replay/InProgress/conflict to
//     responses (409 with Retry-After for InProgress) via internal/errs.
//   - Execute the command itself or hold state outside the transaction.
package idempotency
