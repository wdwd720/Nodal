// Package gates implements production capability gates (goal PARTS 54, 55,
// 244; POLICY_AUTHORITY §1): the persisted, dual-authorized switch that
// decides whether a live-money capability such as LIVE_FUNDING or
// WITHDRAWALS may be exercised in a given environment.
//
// # Responsibilities
//
//   - Checker evaluates whether a capability is ACTIVE. It is a pure
//     function of deployment configuration, the persisted gate row and the
//     injected clock: all five conditions of POLICY_AUTHORITY §1 must hold
//     (config enabled, state ACTIVE, inside the effective window and not
//     revoked, required evidence present, at least two distinct approvers
//     none of whom proposed). Every failing condition yields its own Reason.
//   - Admin drives the state machine DISABLED → PENDING_APPROVAL → APPROVED →
//     ACTIVE, with ACTIVE → SUSPENDED (fast, single operator), SUSPENDED →
//     APPROVED (re-activation needs a distinct principal again), * → REVOKED
//     and APPROVED|ACTIVE → EXPIRED. Every transition is recorded in
//     capability_gate_transitions and appended to the audit log in the same
//     transaction. The legal transitions are an explicit table
//     (CanTransition) covered by an exhaustive test.
//   - Bootstrap persists a DISABLED row for every capability so that "fresh
//     deployment = everything DISABLED" is a stored fact, not an absence. It
//     has no caller yet; until it has one, the default rests on Evaluate
//     failing closed on an absent row and on the database refusing to create
//     a gate row in any state but DISABLED (migration 00701).
//
// # The database enforces the same rule
//
// Every check here also exists in PostgreSQL, and neither side may be
// dropped because the other has it. cp_app holds no UPDATE privilege on
// capability_gates.state, the approval chain, the evidence references or the
// validity window, and no INSERT on capability_gate_transitions; the only
// writer of state is cp_gate_transition, a SECURITY DEFINER function owned by
// the migration role that re-derives the legal-transition table, both
// dual-control rules and conditions 2-5 of POLICY_AUTHORITY §1 from the
// stored row, and writes the row and its transition together (migration
// 00701, DECISION_REGISTER D-043). applyTransition is this package's only
// route to it. The checks below run first and produce the operator-facing
// errors; the database is what holds when they are bypassed.
//
// # What this package must never do
//
//   - Let one environment variable activate a capability: configuration is
//     only condition 1 of 5, and an absent or non-ACTIVE row fails closed.
//   - Let an AGENT (or SERVICE) principal reach any Admin method: they are
//     rejected with FORBIDDEN before any query is issued.
//   - Let one principal both propose and approve, or both approve and
//     activate, an approval version: dual authorization is enforced in code
//     and re-verified by Checker on every evaluation.
//   - Report a capability ACTIVE when the row is missing, expired, revoked,
//     not yet effective, or missing required evidence.
//   - Call a model, read a clock implicitly, use randomness, or iterate a map
//     in an order that affects a decision.
//   - Persist a transition without an audit event, or vice versa.
//   - Write capability_gates with a statement of its own: every state change
//     goes through applyTransition, so the row and its history can never come
//     apart and the database re-checks what this package decided.
package gates
