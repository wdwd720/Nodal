// Package agent is the agent runtime: the lifecycle of a deployable
// (account, strategy version, capital envelope, mode) binding, the trigger
// dispatcher that opens runs, the ToolBroker that is the only path from
// agent code to the outside world, the budget enforcer, and the pause
// machinery (PARTS 66-71, 159, 177, 178, 197, 198).
//
// What this package must never do (PART 9, PART 63, AGENT_RUNTIME.md §11):
//
//   - It never holds or resolves a secret, a key, a wallet handle or a
//     provider URL. Tool adapters receive their credentials from the
//     composition root and the broker never exposes them to the evaluator or
//     to a model prompt.
//   - It never signs, moves value, withdraws, approves anything, resolves a
//     reconciliation, or changes a risk policy, a capability gate, a kill
//     switch or a capital envelope's authority fields. It therefore does not
//     import internal/{signing,wallet,admin,gates,withdrawal,killswitch,
//     capital,risk/policy,execution,funding}; the boundary is enforced by
//     depguard and by test/security/authority_boundary_test.go.
//   - The only money-adjacent write reachable from agent code is proposing a
//     typed trade intent through IntentEmitter. Eligibility, risk, capital
//     reservation, planning, quoting, inspection, signing and submission are
//     performed afterwards by code the agent cannot influence, and treat an
//     agent intent exactly like a manual one.
//   - Content read through a tool is DATA (PART 67). It can never grant an
//     effect, add a tool, raise a budget, move a lifecycle stage or approve
//     anything. Untrusted content reaches a model only inside an UNTRUSTED
//     segment, and the evaluator only ever sees typed, schema-checked values.
package agent
