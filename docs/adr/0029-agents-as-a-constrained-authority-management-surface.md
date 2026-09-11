# ADR-0029 — Agents are a constrained-authority management surface; execution stays behind F-65

Status: **Accepted** (2026-09-10), amended 2026-09-11 (the compiler and the
acceptance route; see "Amendment" below)

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

**The compiler is a declared, reported absence** — on every tier that has none,
which as of the amendment below is every tier that is not a sandbox tier.
`POST /v1/strategies/{id}/compile` records a `compile_attempts` row on every
path, including the path where this deployment has no compiler: outcome
`MODEL_UNAVAILABLE` (the honest member of a CHECK written before this surface
existed) with failure code `COMPILER_UNAVAILABLE`, `parse_result NOT_ATTEMPTED`,
`stage_reached PROMPT`, and no strategy version. No IR is fabricated and nothing
is inferred from the user's description. The seam is two interfaces — a
`CompilerBackend` that `*strategy.Compiler` satisfies exactly, and a
`RefsLoader` for the registry its TYPE and RISK_COMPAT stages validate against —
and a compile is attempted only when BOTH are present.

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
  is created only from one. That remains true off a sandbox tier, and the API
  still says it in words, on the strategy page, before a user writes a
  description. On a sandbox tier the amendment below supplies a compiler, and
  the whole of §18 becomes reachable there.
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

## Amendment (2026-09-11): the compiler a sandbox tier has, and the act that accepts what it produced

Two things were missing, and they were missing together.

### The acceptance route (F-255, D-128)

`agents.Service.Create` refuses an agent whose strategy version is not
`ACCEPTED` with `accepted_by_user_id` and `accepted_at` set; migration 00500
pairs the status and the columns in a CHECK; the compiler is forbidden from
returning an accepted version, because "a backend that could return it would be
approving on the user's behalf, which is exactly what goal §18's review step
exists to stop". Every one of those was in place and **nothing could write the
row**: there was no route, no service method and no SQL anywhere that set the
status to `ACCEPTED`. An agent was unreachable on every deployment of this
build, even with a working compiler.

`POST /v1/strategies/{strategyId}/versions/{version}/accept` is that act.

* **Owner only.** `RequireAccountOwner`, and a stranger gets `NOT_FOUND` — the
  answer `Get` already gives, for the reason it gives it. An operator's
  `account:read_any` does not substitute: approving a strategy is not a read.
* **The version is named by number.** `strategy_versions` is
  `UNIQUE (strategy_id, version)`, so the pair is a complete key, and the number
  is what the review screen shows. A request built from what is on the screen
  names what is on the screen.
* **The body echoes the `ir_hash` that was on the screen.** Without it, "accept
  version 2" would mean "accept whatever version 2 is when this request
  arrives". Versions are immutable, so the hash cannot drift under a caller who
  read *this* version; what the echo catches is a caller who read a different
  one, which is exactly what a second compile landing between the reading and
  the pressing produces. A mismatch is `CONFLICT` and writes nothing.
* **Step-up at the boundary**, unlike `enable`, which demands it in the domain.
  The asymmetry is deliberate: the five lifecycle actions share one operation id
  and a step-up in front of `pause` argues with the operator during an incident.
  Acceptance has no emergency twin — it is a single-purpose route whose entire
  content is a person saying "I read this and I approve it", and it is the gate
  every later grant of authority rests on.
* **A second acceptance of the same version is a replay**, not a conflict: the
  caller asked for a state the row is already in, by the same person, and
  refusing would make a lost response look like an error.
* It writes an audit event on the owner's account stream and **grants nothing**.

### The structured compiler of a sandbox tier (F-256, F-257, D-129)

The `RefsLoader` half of the seam above had **no implementation anywhere**, so
the pair could never be satisfied and "until both exist, a strategy cannot be
compiled here" was true by construction rather than by configuration. A second
absence sat underneath it: `risk.DefaultGlobalPolicyJSON` sets
`"allowed_venues": []`, nothing ever listed a venue, and the RISK_COMPAT stage
refuses any strategy whose envelope names a venue outside the allowlist — so
even a model-backed compiler could never have produced a version.

`internal/provider/compilersandbox` is a compiler for a sandbox tier, beside
`payoutsandbox` and `verifysandbox` and for the same two reasons they are first
class providers: production wiring may not import a test double, and a component
that refuses to exist in PROD has to say so somewhere PROD compiles.

**It reads a DECLARED strategy and never the description.** The grammar is the
smallest one that produces a valid IR and covers §18's list: one instrument and
one venue from the registry; an entry rule and an exit rule, each either a
comparator on the instrument's mid price against a threshold or "on every
evaluation"; three risk limits; a capital floor; an evaluation interval and an
hourly ceiling on trade intents; and `PAPER`, which is the only mode this build
compiles. Every amount is an exact USD **minor-unit** string. Unknown fields are
refused rather than ignored, so a misspelled key is a named refusal instead of a
setting that silently did not apply.

An absent or incomplete strategy compiles to a refusal that names **every** field
it needed — failure code `STRUCTURED_CONSTRAINTS_REQUIRED`, `stage_reached
PROMPT`, no version — which is the same honest shape `COMPILER_UNAVAILABLE` has.
Nothing is defaulted, because a default is an inference about what somebody
meant.

What it produces is a real `ir.IR`, validated by the same `strategy.Validate` the
model path runs, with lineage source `STRUCTURED_SANDBOX` (00811), a zero model
budget and no providers, effects derived by `ir`'s own rules, and a rationale
that names, element by element, which stated field each part came from.

The IR requires a committed prediction before a trade intent, and the caller
stated no forecast. The compiler does not invent one: the prediction it emits
claims nothing, in the direction that cannot flatter — no direction, probability
zero, no expected gain, a loss not ruled out, and a maximum downside of the whole
position. A zero downside would have been the comfortable choice and would have
been a claim.

**PROD is refused three times over.** `compilersandbox.New` refuses `EnvProd`;
`cmd/api` constructs it only when `cfg.SandboxTier()`, which is
`CP_API_LEGAL_POLICY=SANDBOX`, a value `config.Validate` already refuses in PROD;
and migration 00812 gives `strategy_versions` a `sandbox` flag and an
`environment`, pairs `STRUCTURED_SANDBOX` with the flag, and refuses the pair
`(sandbox, PROD)` in any database. Both columns are immutable under the guard
00500 installed, so `cp_app` cannot clear the label on a row it wrote.

Everything built this way is labelled a rehearsal where it is stored and where it
is shown: the version carries `sandbox`, the agent carries it (read from its
version, never stored on the agent, so the two cannot disagree), and the web
renders both at `data-temp="simulated"`.

### What did not change

`internal/agents` still acquires no caller for the runtime, and no file here
names those constructors. `enable` still walks DRAFT → COMPILED → VALIDATED →
BACKTEST_ELIGIBLE in PAPER mode and stops. Levels 4–6 are still refused with the
capability each would need. No Credits move when an agent is created. F-65's
deferral is undisturbed, `test/security` is still green, and
`agentRuntimeDeployment()` still answers `NOT_DEPLOYED` — so Scenario D's
"inspect decisions" step shows the honest empty state on every tier of this
build, sandbox or not (D-130).

### Amendment evidence

Migrations 00811, 00812, 00813; `internal/provider/compilersandbox` with its
unit suite; `internal/agents/structured.go` and `accept.go` with
`accept_integration_test.go`; `cmd/api/compiler.go` and
`compiler_integration_test.go`; `internal/httpapi/handlers_strategies.go` and
the `authz.go` row; `apps/web/src/pages/agents/AgentNew.tsx` and
`apps/web/e2e/scenarios/d-agent.spec.ts`.
