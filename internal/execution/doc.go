// Package execution is the execution record layer of the control plane
// (goal PARTS 41, 45, 46, 47, 48, 106, 107, 227, 228; EXECUTION.md §4, §5,
// §7; SETTLEMENT_COMPILER.md §6). It owns the provider-neutral adapter
// contract and the durable records every submission leaves behind: orders,
// order transitions, execution attempts and fills.
//
// # Responsibilities
//
//   - ExecutionAdapter is the only shape a venue integration may take. Its
//     types (QuoteRequest, UnsignedAction, SignedSubmission, SubmissionResult,
//     ExternalReference, ExecutionStatus, ExternalExecutionEvent,
//     ReconcileScope, ExternalState) are provider-neutral; provider schemas
//     never leave the provider package. Every method carries a
//     provider.RetryClass (MethodRetryClass): reads and pure construction are
//     SAFE_RETRY, Cancel is IDEMPOTENT_WRITE only where the venue guarantees
//     it, and Submit is UNKNOWN_EFFECT_WRITE — a transport timeout on Submit
//     is reported as SUBMISSION_STATE_UNKNOWN and is never a failure.
//   - Order and OrderTransitions are the explicit PART 47 state machine.
//     Repository.Transition refuses anything not in the table, writes the
//     immutable order_transitions row that migration 00603 requires in the
//     same transaction, and appends the outbox and audit events.
//   - Fills are the economic truth of an execution. RecordFill inserts under
//     UNIQUE (venue, external_fill_id): a redelivered fill returns the stored
//     row with Existing set and changes nothing, so N deliveries produce one
//     economic effect. Cumulative filled quantities move the order to
//     PARTIALLY_FILLED or FILLED (PART 228), and a fill arriving while a
//     cancel is requested wins (PART 227). journal_transaction_id and
//     position_applied_at are set exactly once; the fills_guard trigger is the
//     second line of defense.
//   - Attempts are immutable evidence of every build/inspect/sign/submit
//     cycle. attempt_no is allocated per order under the order row lock, the
//     transaction signature is unique across the table, and ListRecoverable
//     returns the attempts whose fate is not yet known (SUBMITTED,
//     SUBMISSION_UNKNOWN, OBSERVED, CONFIRMED) for the recovery worker.
//   - FinalityPolicy maps an action class to the finality level it requires
//     (EXECUTION.md §5) and Satisfies compares observed against required.
//     SUBMITTED, OBSERVED, CONFIRMED and FINALIZED are never collapsed into
//     "success".
//   - Evidence stores request/response bodies through an ArchiveWriter and
//     returns the reference and SHA-256 that the records carry.
//
// # What this package must never do
//
//   - Retry Submit, or any UNKNOWN_EFFECT_WRITE, without a status
//     investigation. A timeout leaves the order in SUBMISSION_UNKNOWN with its
//     reservation locked; it never leads to a second submission for the same
//     plan (PART 48).
//   - Record a fill twice, or let a duplicate delivery change an order's
//     cumulative quantities a second time.
//   - Change a fill's economic fields, unset journal_transaction_id or
//     position_applied_at once set, or delete a fill, attempt or transition.
//   - Move an order to CANCELLED on anything but external confirmation, or
//     let a cancel request override a fill that has already happened.
//   - Change an order's status outside Repository.Transition (so no status
//     change exists without its transition row, outbox event and audit event
//     in the same transaction).
//   - Contain provider-specific logic, import a provider package, or let
//     provider types appear in its API.
//   - Block observation, status reads or reconciliation on a kill switch or a
//     provider breaker (PART 107): nothing in this package consults either.
//   - Use floating point for any quantity, or read the wall clock directly.
package execution
