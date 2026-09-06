// Package adminplane is the operator console's authority model: the single
// derived answer to "what may this principal do with this record, right now?"
// for every controlled administrative action, capability gate, kill switch
// and reconciliation resolution (goal PARTS 91, 93, 128, 129, 163, 164, 165;
// POLICY_AUTHORITY §1, §2, §5).
//
// It exists because an operator console has to decide what to render before
// the server has been asked. A console that guesses is a console that offers
// a button which fails, or — far worse — hides a refusal behind an affordance
// that looks like it worked. This package removes the guess: it computes the
// affordance from the same tables the enforcing packages use, and its tests
// prove the affordance and the enforcement agree on every case, including the
// verdict's error code.
//
// # Responsibilities
//
//   - Decide reproduces, in order and without side effects, the checks
//     internal/admin performs for Propose, Approve, Reject, Cancel and
//     Execute, returning both a boolean and the errs.Code the enforcing call
//     would return. The order matters: it is what makes the reported reason
//     the true reason.
//   - Elevation reports whether a principal holds a live BREAK_GLASS
//     elevation and until when, so a console can show the clock on an
//     elevation instead of discovering its expiry through a refusal.
//   - Surfaces enumerates the operator surfaces and, per surface, the
//     permissions that make it readable and the writes it offers with the
//     permission, step-up window and approval each write needs.
//   - Authority packages all of the above — the permission matrix, the action
//     kind table, the gate actions, the kill-switch severities — into one
//     JSON document (Export) that the console loads instead of restating any
//     of it in TypeScript. The golden test writes that document into
//     apps/admin, so a change to the Go policy that the console has not
//     absorbed fails the Go build, not a user's afternoon.
//
// # What this package must never do
//
//   - Enforce anything. Every Decide result is advisory. The authoritative
//     refusal is still internal/admin, internal/gates, internal/killswitch,
//     internal/reconciliation and the database CHECK constraints, and no
//     caller may skip them because Decide said yes.
//   - Restate a permission, a kind, a step-up window or an expiry. Every
//     value here is read from internal/security and internal/admin; the
//     golden tests fail if a literal creeps in.
//   - Be more permissive than the enforcing layer. The agreement test treats
//     an allowed-but-refused verdict as a failure, because that is the bug
//     that produces a dead button, and a denied-but-accepted verdict as a
//     failure too, because that is the bug that hides a real capability.
//   - Hold a connection, a key or provider state, or read the clock
//     implicitly: every entry point takes the instant it judges against.
//   - Offer a balance edit. There is no such action kind, no such surface and
//     no such write; the tests assert it stays that way.
package adminplane
