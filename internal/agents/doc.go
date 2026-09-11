// Package agents is the product surface of the agent architecture: creating an
// agent from a compiled strategy version, recording the authority and the
// limits its owner granted it, moving it through the lifecycle a person can
// reach (enable, pause, resume, disable, archive), and reporting — honestly —
// what is actually running.
//
// # It is not internal/agent
//
// The singular package is the RUNTIME: the trigger dispatcher, the ToolBroker,
// the budget enforcer, the evaluator's pause checks. It is an agent tree
// (.golangci.yml `agent-authority`, test/security's `agentTrees`) and is
// forbidden from importing signing, wallets, admin, gates, withdrawal and the
// kill switch, because code an untrusted proposal generator can reach must not
// be able to reach authority.
//
// This package is the opposite side of that boundary. It is called by the HTTP
// surface on behalf of a HUMAN, it never runs strategy code, and it never
// evaluates anything. It reads the runtime's tables and types and it writes the
// two things the runtime does not own: the user's grant of authority
// (agent_grants, 00786) and the lifecycle transitions a person asks for.
//
// # What it must never do
//
//   - Run an agent, or acquire a caller for anything that would. The runtime is
//     inert at three layers in this build: cmd/agent-worker wires no evaluator
//     and its EmitterFor answers UNSUPPORTED; the runtime's lifecycle and
//     emitter constructors have no production callers; and neither worker binary
//     is deployed on this tier. F-65's deferral of the kill-switch bridge rests
//     on all three being true. Building the management surface does not change
//     any of them, and test/security's watcher over that premise is what keeps
//     it that way — which is why nothing here names those constructors.
//   - Report an agent as running because it is enabled. Enabled means the owner
//     granted authority; running means an evaluator opened a run. RuntimeStatus
//     is derived from agent_runs and says NOT_DEPLOYED where that is the truth.
//   - Move Credits. A budget is a ceiling recorded on the grant. Credits move
//     only through internal/credit, and nothing here writes a ledger row.
//   - Grant an authority level this build does not support.
//     agentauthority.MaxSupportedLevel is the ceiling and levels 4-6 are refused
//     with the capability each would need.
//   - Bypass the lifecycle binding. Every state change is an INSERT into
//     agent_lifecycle_transitions; since 00750 the application holds no UPDATE
//     on agents at all, and a SECURITY DEFINER trigger writes the agent row from
//     the transition. There is no second way, by construction.
package agents
