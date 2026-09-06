// Package admin implements controlled administrative actions under dual
// control (goal PARTS 91, 93, 128, 129, 164; POLICY_AUTHORITY §5) on top of
// the admin_actions and admin_action_transitions tables.
//
// # Responsibilities
//
//   - A closed table of action kinds (Kinds, Spec): for every kind, whether
//     it requires dual control, which permission may propose and which may
//     approve, how fresh the step-up authentication must be, and how long a
//     proposal stays actionable before it expires.
//   - The action state machine PROPOSED → APPROVED → EXECUTED | FAILED, with
//     REJECTED, CANCELLED and EXPIRED as terminal side exits (CanTransition).
//     Every transition inserts an admin_action_transitions row and appends
//     an audit.Event on the "admin" stream in the same transaction.
//   - Propose: a principal holding the kind's propose permission, with a
//     recent step-up, a reason of at least MinReasonLength characters and
//     typed JSON-object params. params_hash = sha256(audit.CanonicalJSON(params)).
//   - Approve: a different principal holding the kind's approve permission,
//     with a recent step-up, while the proposal is PROPOSED and unexpired.
//     Self-approval is refused here and by the table CHECK constraint.
//   - Execute: APPROVED (or PROPOSED for a kind that does not require dual
//     control), unexpired, and only after the stored params re-hash to the
//     recorded params_hash. The caller's callback runs inside a savepoint on
//     the caller's transaction; a failure rolls the callback's writes back,
//     records FAILED with the error, and returns the error so the caller's
//     transaction decides whether the FAILED record commits.
//   - VerifyApproved: the read other packages (kill-switch release, gate
//     activation, material reconciliation resolution, ledger correction)
//     call to confirm that an approval id is an APPROVED or EXECUTED action
//     of the expected kind and target that has not expired.
//   - Break-glass: BREAK_GLASS_GRANT is an ordinary dual-control kind whose
//     execution yields a time-boxed Grant; PrincipalWithBreakGlass applies it
//     to a principal (PART 93). Every use of the elevation is audited by the
//     package that consumes it.
//
// Principals: Principal.SubjectID must be the users.id UUID of the human
// operator (admin_actions references users). Anything else is refused.
//
// # What this package must never do
//
//   - Offer a balance-editing action. There is no BALANCE or PATCH kind and
//     the test suite asserts it; financial repair is only LEDGER_CORRECTION,
//     which posts a reason-coded compensating journal transaction (PART 129).
//   - Let the proposer approve their own action, or let one principal both
//     propose and approve a dual-control kind (approver ≠ proposer, enforced
//     in code and by the database CHECK).
//   - Serve an AGENT principal: every entry point returns FORBIDDEN before
//     touching the database.
//   - Execute an action whose stored params no longer hash to params_hash,
//     whose status is not APPROVED (or PROPOSED for non-dual kinds), or
//     whose expiry has passed.
//   - Skip the transition row or the audit event for any state change.
//   - Read the clock implicitly: the clock is injected.
package admin
