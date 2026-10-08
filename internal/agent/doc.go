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
//   - The only money-adjacent writes reachable from agent code are proposals:
//     a typed trade intent through IntentEmitter, and -- when the account's
//     owner delegated authority to the agent -- a delegated swap request or
//     withdrawal proposal through DelegatedEmitter. Eligibility, risk, capital
//     reservation, planning, quoting, inspection, signing and submission are
//     performed afterwards by code the agent cannot influence, and treat an
//     agent intent exactly like a manual one.
//   - It reaches internal/delegation only through DelegatedAuthority, which
//     *delegation.AgentAdmission satisfies: admit a swap request, record a
//     withdrawal proposal, read the agent's own capabilities. It never holds
//     a delegation.Service, so it can never sign, execute, approve or decide
//     a delegated action, grant or revoke a capability, or read a key. A
//     delegated request carries this run's AGENT principal, a nonce derived
//     from the run and the action (never chosen by the evaluator), and passes
//     every bound an intent passes: pause, the order-rate budget, the
//     strategy's effect set and the run's mode (PAPER only, never a run bound
//     to an envelope). It gives a swap's ACQUIRE/REDUCE label no meaning of
//     its own; delegation's admission alone judges it. A refusal, whoever
//     refused, is recorded on the run and ends it; it is never retried.
//   - Content read through a tool is DATA (PART 67). It can never grant an
//     effect, add a tool, raise a budget, move a lifecycle stage or approve
//     anything. Untrusted content reaches a model only inside an UNTRUSTED
//     segment, and the evaluator only ever sees typed, schema-checked values.
package agent
