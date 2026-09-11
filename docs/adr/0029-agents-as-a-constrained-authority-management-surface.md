# ADR-0029 — Agents are a constrained-authority management surface; execution stays behind F-65

Status: **Accepted** (2026-09-10)

Supersedes nothing. Constrains how the agent architecture becomes a product
surface. Answers §17 (AGENTS) and §18 (AGENT CREATION UX) of the product goal,
and the parts of §46 and §62 that bear on them.

## Context

The agent architecture is built and it does not run. Three separate layers of
inertness, each verified rather than assumed:

1. **No deployed process.** Neither `cmd/agent-worker` nor
   `cmd/execution-worker` is deployed on the launch tier; the blueprint runs one
   web service.
2. **No evaluator inside the worker that exists.** `cmd/agent-worker` wires no
   evaluator and its `EmitterFor` returns `UNSUPPORTED`.
3. **No caller for the runtime at all.** The lifecycle and emitter constructors
   in `internal/agent` have no production caller anywhere in the tree.

F-65 rests on exactly that. Two kill-switch kinds — `AGENT_PAUSE` and
`MODEL_DISABLE` — reach nothing today, and the finding deliberately did **not**
build the bridge, because wiring a control into a subsystem that does not run
produces a control only a test can reach. `test/security` watches the premise:
the day the runtime acquires a production caller, that test fails and states
what has become owed.

Meanwhile the product needs agents to be a thing a person can see, create,
understand, limit and stop. §17 requires the four authority distinctions to be
legible and levels 4–6 to stay disabled; §18 requires natural language to become
a compiled strategy the user reads and approves *before* anything is activated.
None of that requires running an agent, and all of it requires writing rows the
runtime does not own.

Before this, there were no HTTP routes for strategies or agents at all.

## Decision

**A new package, `internal/agents`, is the management surface, and it is not the
runtime.** `internal/agent` (singular) stays what it is: an agent tree, forbidden
by depguard and by `test/security` from importing signing, wallets, admin, gates,
withdrawal, the kill switch, capital and risk policy, because code an untrusted
proposal generator can reach must not reach authority. `internal/agents` is
called by the HTTP surface on behalf of a human, runs no strategy code, and
evaluates nothing. It reads the runtime's tables and types; it writes two things
the runtime does not own — the owner's grant of authority, and the lifecycle
transitions a person asks for.

Five things follow from that split, and each is the part that could have gone
wrong:

1. **It acquires no caller for the runtime.** Nothing here constructs the
   runtime's lifecycle or emitter, and no file names those constructors (the
   watcher reads text, and a mention would read as a caller). Every state change
   is an INSERT into `agent_lifecycle_transitions`; since migration 00750 the
   application holds no UPDATE privilege on `agents` at all and a SECURITY
   DEFINER trigger writes the agent row from the transition's `to_*` columns.
   There is no second path, by construction, and F-65's premise is unchanged.

2. **Authority is a grant a person made, recorded separately** (migration
   00786, `agent_grants`). The columns on `agents` record what the PROMOTION
   LADDER granted, with an approval and hashed evidence behind each rung. What a
   user agreed to — a level, a Credit ceiling, a per-trade cap, a daily loss
   stop, a universe and a frequency — has a different grantor and a different
   revocation story, so it has a different table. The row is immutable but for
   `archived_at`: raising a budget is a new decision by the person who made the
   old one, not an UPDATE.

3. **Levels 4–6 are refused with the gate each would need.**
   `agentauthority.MaxSupportedLevel` is the ceiling, the column CHECK mirrors
   it, and the API answers `CAPABILITY_NOT_APPROVED` naming
   `AGENT_BOUNDED_DISCRETION`, `AGENT_AUTONOMOUS_SELECTION` or
   `AGENT_AUTONOMOUS_PORTFOLIO`. A refusal that read `VALIDATION_FAILED` would
   tell a user they made a typing mistake.

4. **Enabling an executing level needs the capability that already governs it.**
   Authority level 3 acts without a person confirming each action, so enabling a
   level-3 agent requires `LIVE_AGENT_TRADING` to be ACTIVE, and the refusal
   carries the gate's own reason. There is deliberately no new `AGENT_EXECUTION`
   gate beside it: a second name for the same authority is how a deployment ends
   up with one of them on.

5. **The API never reports an agent as running because it is enabled.**
   `runtime: { evaluator, executor, last_heartbeat, detail }` is derived from
   `agent_runs` and from the deployment's own declaration of which workers it
   runs. On this tier both are `NOT_DEPLOYED` with a sentence saying the agent is
   not being evaluated and nothing is scheduled. Evidence can only ever make the
   answer weaker than the deployment claims — never stronger.

**The compiler is a declared, reported absence.** `POST /v1/strategies/{id}/compile`
records a `compile_attempts` row on every path, including the path where this
deployment has no compiler: outcome `MODEL_UNAVAILABLE` (the honest member of a
CHECK written before this surface existed) with failure code
`COMPILER_UNAVAILABLE`, `parse_result NOT_ATTEMPTED`, `stage_reached PROMPT`, and
no strategy version. No IR is fabricated and nothing is inferred from the user's
description. The seam is two interfaces — a `CompilerBackend` that
`*strategy.Compiler` satisfies exactly, and a `RefsLoader` for the registry its
TYPE and RISK_COMPAT stages validate against — and a compile is attempted only
when BOTH are present.

## Why this and not the alternatives

- *Putting the management surface in `internal/agent`.* It is the obvious place
  and it is the wrong one twice: the package is import-restricted precisely
  because agent code runs there, and any HTTP-facing service in it would either
  inherit restrictions it does not need or force them to be loosened. And it
  would have meant constructing the runtime's lifecycle from `cmd/api`, which
  breaks F-65's premise for no gain — the management surface needs the
  transition TABLE, not the runtime service.
- *Wiring a compiler with a model key and an empty registry.* Tempting, because
  the model provider slot (`CP_PROVIDERS_MODEL_*`) already exists. It would
  reject every instrument a user named and blame the user: an empty
  `ValidationRefs` fails the TYPE stage on every reference. A refusal that says
  "this deployment cannot compile" is honest; a refusal that says "your strategy
  is invalid" when the registry is empty is not.
- *Making `enable` reach a real-capital stage.* CANARY, LIMITED and LIVE need a
  bound capital envelope, hashed promotion evidence and a dual-controlled admin
  approval, and the schema enforces all three. Exposing that ladder as a customer
  button would either bypass those or pretend to satisfy them. `enable` walks
  DRAFT → COMPILED → VALIDATED → BACKTEST_ELIGIBLE in PAPER mode, one rung per
  transition row, and stops. The operator ladder is unchanged and is not on this
  surface.
- *`disable` as a reversible toggle.* The state machine has no backwards edge on
  the ladder — reducing authority is PAUSE or REVOKE — and 00734 refuses a
  transition that claims to leave a terminal state. So `pause` is the reversible
  stop and `disable` is REVOKED, final; an owner who wants the agent back makes a
  new grant they have to read again. `archive` is not a lifecycle transition at
  all: it stamps the grant and removes the agent from the default list.
- *Moving Credits when an agent is created.* A budget is a ceiling on Credits at
  risk. Reserving them at creation would be a financial movement with no economic
  event behind it, and would make the user's spendable balance depend on how many
  agents they had drafted. `internal/credit` remains the only thing that moves
  Credits; `budget_used` is derived from the intents an agent's runs actually
  created.

## Consequences

- Nine routes exist where there were none: four for strategies, four for agents
  (one of them the five-action lifecycle), and two for operators. The customer
  role reaches them with `strategy:write` / `strategy:read`, which it already
  holds; no permission was minted.
- **The operator pause route is floored on `kill:activate`, not `agent:pause`.**
  The CUSTOMER role holds `agent:pause` — it is how an owner stops their own
  agent — so an admin route floored on it would have been reachable by every
  customer. `internal/agents` then demands `agent:pause` and an OPERATOR actor,
  so the route floor says who may reach it and the domain says who may do it.
- `enable` demands a recent strong authentication in the domain service rather
  than at the boundary, because the five actions share one operation id and a
  step-up in front of `pause` is a control that argues with the operator during
  an incident.
- A deployment with no compiler cannot produce a strategy version, and an agent
  is created only from one. The whole surface is therefore reachable and honest
  and produces no agents on this tier — which the API says in words, on the
  strategy page, before a user writes a description.
- F-65's deferral is undisturbed and still watched. Nothing here is the bridge,
  and the day a worker is deployed the honest answer changes in one place:
  `agentRuntimeDeployment()` in `cmd/api`.

## Evidence

Migration 00786; `internal/agents` (`doc.go`, `authority.go`, `grant.go`,
`events.go`, `runtime.go`, `service.go`, `store.go`, `strategies.go`) with its
unit and integration suites; `internal/httpapi/handlers_agents.go`,
`handlers_strategies.go` and `handlers_agents_test.go`;
`internal/httpapi/authz.go`; `cmd/api/agents.go` and the agents block in
`cmd/api/wire.go`; `test/integration/enums`;
`test/security/deferred_bridge_test.go` still green.
