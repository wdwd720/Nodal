# POLICY / AUTHORITY MODEL

Status: design fixed 2026-09-05; implementation tracked in `docs/build/REQUIREMENTS_TRACEABILITY.md`. Contract for Stage 3. Covers PARTS 9, 52–60, 63, 89, 91, 93, 128, 129, 164, 165, 181.

Core rule: **AI proposes. Deterministic systems authorize.** Every authority decision here is a pure function of persisted, versioned policy plus a typed input; none may call a model, read a clock implicitly, iterate a map without sorting, use floating point, or use randomness.

## 1. Production capability gates (PARTS 54–55, 244)

A capability is ACTIVE only when **all** of the following hold at evaluation time:

1. deployment configuration lists the capability as enabled for this environment (`config.Capabilities.Enabled`), **and**
2. the persisted gate row `(capability, environment)` has `state = ACTIVE`, **and**
3. `effective_at <= now < coalesce(expires_at, +inf)` and `revoked_at IS NULL`, **and**
4. required evidence references are non-empty for the capability class (LIVE_* and WITHDRAWALS require legal review, provider contract, risk approval, security approval), **and**
5. the approval chain shows at least two distinct approvers, neither of whom is the proposer (dual authorization).

One environment variable can therefore never enable live money. Fresh deployments start with every gate `DISABLED`.

State machine: `DISABLED → PENDING_APPROVAL → APPROVED → ACTIVE`, with `ACTIVE → SUSPENDED` (single operator with `kill:activate`, fast), `SUSPENDED → ACTIVE` (dual approval again), `* → REVOKED` (terminal for that approval version), `ACTIVE|APPROVED → EXPIRED` (by time). Every transition is recorded in `capability_gate_transitions` with the actor, reason, approval id, and evidence hash. Agents cannot reach this subsystem (`security.ActorType == AGENT` is rejected before any query).

## 2. Kill switches (PARTS 52–53, 165)

Kill switches stop **new risk**. They never stop reading external state, processing already-received fills, settlement, reconciliation, ledger posting, or policy-permitted risk-reducing cleanup.

Every guarded operation declares an `ActionClass`:

| ActionClass | Blocked by |
|---|---|
| `NEW_RISK` (open/increase exposure, new funding session, new agent intent) | any matching switch |
| `REDUCE_RISK` (close/reduce a position) | `INSTRUMENT_HALT`, `CHAIN_DISABLE_NEW_ACTIONS`, `PROVIDER_DISABLE_NEW_ACTIONS`, `ACCOUNT_FREEZE` only when policy says so (default: allowed) |
| `WITHDRAW` | `WITHDRAWALS_DISABLE`, `ACCOUNT_FREEZE`, `GLOBAL_NEW_RISK_KILL` |
| `OBSERVE`, `SETTLE`, `RECONCILE`, `LEDGER_POST`, `CANCEL` | never |

Switch kinds and scopes: `GLOBAL_NEW_RISK_KILL(*)`, `ACCOUNT_FREEZE(account_id)`, `AGENT_PAUSE(agent_id)`, `STRATEGY_VERSION_DISABLE(strategy_version_id)`, `VENUE_DISABLE(venue)`, `INSTRUMENT_CLOSE_ONLY(instrument_id)`, `INSTRUMENT_HALT(instrument_id)`, `CHAIN_DISABLE_NEW_ACTIONS(chain)`, `PROVIDER_DISABLE_NEW_ACTIONS(provider)`, `FUNDING_DISABLE(*)`, `WITHDRAWALS_DISABLE(*)`, `MODEL_DISABLE(model_id|*)`.

Activation is a single authenticated operator action with `kill:activate` and a reason; it takes effect on the next check (checks read Postgres inside the authorizing transaction; an in-process cache ≤ 1 s may serve pre-checks only). Release of a `SEVERE` switch (GLOBAL, CHAIN, PROVIDER, FUNDING, WITHDRAWALS) requires an approved `admin_actions` row under dual control plus step-up; release of a `STANDARD` switch requires `kill:release` and step-up.

## 3. Eligibility engine (PARTS 56–57)

`eligibility.Evaluate(policy, input) → Decision`. Input is a typed struct: identity state, age verified, jurisdiction country/region, residency, sanctions state, account status and restrictions, context kind (TRADE / FUNDING / WITHDRAWAL / AGENT_RUN / STRATEGY_PROMOTION), instrument risk class and status, asset class, venue, provider, and the capability states relevant to the context. Output: `{Eligible bool, PolicyVersion, ReasonCodes []string (sorted), EvaluatedAt, ContextHash}`; every trade/funding/withdrawal attempt persists its decision in `eligibility_decisions`.

Policy is a versioned typed document (`eligibility_policies.rules`): allowed countries; blocked regions per country; minimum age; required identity and sanctions states; per-context requirements (e.g. WITHDRAWAL requires VERIFIED identity + CLEAR sanctions + capability WITHDRAWALS active); provider restrictions (country/region blocks per provider); instrument rules (which risk classes are allowed per jurisdiction). Missing policy, unknown jurisdiction, unknown identity state, or an unrecognised rule key **fail closed** with reason `ELIGIBILITY_POLICY_UNKNOWN`.

## 4. Deterministic risk kernel (PARTS 58–60, 181)

`risk.Evaluate(policy, input) → Decision` is a pure function. `input` carries: intent (action, notional, instrument, constraints), stage (`PRE_TRADE`, `FINAL`, `CONTINUOUS`), account snapshot (buying power output, current position and exposure per instrument/asset class, daily realized loss, drawdown, order count in the current window, unresolved reconciliation mismatches), envelope snapshot if autonomous, market snapshot (quote: price impact bps, slippage bps, fee bps, quote age ms, liquidity estimate; asset policy status; provider health), kill-switch summary, and `Now`.

Decision persisted in `risk_decisions`: policy version + hash, snapshots, decision (`ALLOW`/`REJECT`), sorted reason codes (`RISK_MAX_SINGLE_TRADE`, `RISK_MAX_POSITION`, `RISK_CONCENTRATION`, `RISK_NATIVE_MARKET_CONCENTRATION`, `RISK_CREATOR_CONCENTRATION`, `RISK_DAILY_LOSS`, `RISK_MAX_DRAWDOWN`, `RISK_ORDER_RATE`, `RISK_SLIPPAGE`, `RISK_FEE`, `RISK_PRICE_IMPACT`, `RISK_QUOTE_AGE`, `RISK_LIQUIDITY`, `RISK_ASSET_STATUS`, `RISK_VENUE_STATUS`, `RISK_PROVIDER_HEALTH`, `RISK_STALE_DATA`, `RISK_RECONCILIATION_PENDING`, `RISK_ENVELOPE_EXHAUSTED`, `RISK_ENVELOPE_INSTRUMENT_NOT_ALLOWED`, `RISK_ENVELOPE_VENUE_NOT_ALLOWED`, `RISK_KILL_SWITCH`, `RISK_POLICY_MISSING`), and `resulting_constraints` (tightened slippage/fee/impact bounds and maximum notional the plan must respect).

A Nodal-native trade goes through a second entry point, `risk.EvaluateNativeTrade(policy, input)`, and the input carries a `NativeMarketSnapshot` instead of a market snapshot. It has to: every other limit in the kernel is denominated in USD, and a Credit has no approved external value (PART LIV), so a USD limit on a native position would need an exchange rate nobody set. The two limits it evaluates are the ones PART XXXII names for this economy — `max_native_market_concentration_bps` (units held against the asset's total supply) and `max_creator_concentration_bps` (Credits committed to one creator against the account's whole Credit position) — compared as `part * 10000 > whole * limit`, with no division and no float. A policy missing either yields `RISK_POLICY_MISSING`; an input with no native snapshot yields `RISK_INPUT_INVALID` rather than ALLOW. A SELL is never evaluated: both limits constrain holding too much, and refusing an exit would trap a holder in the position the limit exists to discourage.

Order-rate limiting is enforced here from persisted counts (`intents` table), independent of any Redis limiter. Policies are versioned rows in `risk_policies` (scope GLOBAL / ACCOUNT / AGENT); the effective policy is the composition GLOBAL ∧ ACCOUNT ∧ AGENT with the strictest value winning for every limit. Agents can never write `risk_policies` (`created_by_actor_type <> 'AGENT'` CHECK + permission matrix + package import rules).

Determinism test: a golden corpus of inputs must produce byte-identical decision JSON across 1,000 runs and across `-race` runs.

## 5. Admin dual control (PARTS 91, 93, 128, 129, 164)

Sensitive operator actions are rows in `admin_actions` with `kind` (e.g. `CAPABILITY_GATE_APPROVE`, `KILL_SWITCH_RELEASE`, `LEDGER_CORRECTION`, `RECONCILIATION_RESOLVE_MATERIAL`, `ENVELOPE_AUTHORITY_CHANGE`, `ACCOUNT_UNFREEZE`, `WITHDRAWAL_APPROVE`), typed `params` with a `params_hash`, a mandatory reason, `proposed_by`, and `requires_dual`. Approval requires a different principal holding the corresponding approve permission and a recent step-up; execution replays the exact `params_hash`; actions expire. There is no balance-edit action: financial repair is only `LEDGER_CORRECTION`, which posts a reason-coded compensating journal transaction.

Break-glass is a time-boxed principal flag (`BreakGlassUntil`) granted through an `admin_actions` row of kind `BREAK_GLASS_GRANT` (narrow scope, expiry, reason, notification), and every use is audited.

## 6. Audit events (PART 89, 87 preview)

`audit_events` is append-only and hash-chained **per stream** (`account:<id>`, `admin`, `system`, `agent:<id>`): each row stores `content_hash = sha256(canonical JSON of the record without prev/content hashes)` and `prev_hash` of the previous row in the same stream, computed under `pg_advisory_xact_lock(hashtext(stream))` inside the writer's transaction. The audit worker (Stage 13) periodically builds a Merkle root over all rows since the last checkpoint, signs it with KMS, and archives to S3 Object Lock. `make verify-audit` recomputes every chain and root.

Every money-affecting path writes an audit event in the same transaction as its state change: eligibility decision, risk decision, reservation, plan approval, signing request/decision, submission, fill, reconciliation resolution, ledger posting, funding transition, gate/kill/admin transitions, session security events.

## 7. Go contracts (fixed)

```go
// internal/gates
type Capability string; type GateState string
type Checker interface { IsActive(ctx, q db.Querier, c Capability) (Verdict, error) }   // Verdict{Active bool; Reason string; ApprovalVersion int; EvidenceHashes []string}
type Admin interface { Propose(ctx, tx, c Capability, req Proposal) (Gate, error); Approve(ctx, tx, c Capability, approvalID string) (Gate, error); Activate(...); Suspend(...); Revoke(...) } // all reject AGENT principals; dual control enforced
// internal/killswitch
type Kind string; type ActionClass string
type Action struct { Class ActionClass; AccountID, AgentID, StrategyVersionID, Venue, InstrumentID, Chain, Provider, ModelID string }
type Checker interface { Check(ctx, q db.Querier, a Action) error }   // KILL_SWITCH_ACTIVE with field "switch"
type Controller interface { Activate(ctx, tx, kind Kind, scope, reason string) (Switch, error); Release(ctx, tx, kind Kind, scope, reason string, approvalID *string) (Switch, error); Active(ctx, q) ([]Switch, error) }
// internal/eligibility
type Policy struct{...typed...}; func ParsePolicy(json.RawMessage) (Policy, error) // rejects unknown keys
func Evaluate(p Policy, in Input) Decision
type Store interface { CurrentPolicy(ctx, q, at time.Time) (Policy, string, error); RecordDecision(ctx, tx, Decision) error }
// internal/risk
type Policy struct{...typed...}; func Compose(global, account, agent *Policy) Policy
func Evaluate(p Policy, in Input) Decision
type Store interface { EffectivePolicy(ctx, q, accountID, agentID string, at time.Time) (Policy, PolicyRef, error); RecordDecision(ctx, tx, Decision) (DecisionID, error) }
// internal/admin
type Actions interface { Propose(ctx, tx, Proposal) (Action, error); Approve(ctx, tx, id, note string) (Action, error); Reject(...); Execute(ctx, tx, id string, exec func(ctx, tx, params json.RawMessage) (json.RawMessage, error)) (Action, error) }
// internal/audit
type Event struct { Stream, ActorType, ActorID, Action, ResourceType, ResourceID string; BeforeHash, AfterHash []byte; RequestID, CorrelationID, PolicyVersion, Reason string; SourceIP, Device, EvidenceRef string; Payload json.RawMessage; OccurredAt time.Time }
type Writer interface { Append(ctx, tx pgx.Tx, e Event) (Appended, error) }   // computes content_hash + prev_hash under per-stream advisory lock
func CanonicalJSON(v any) ([]byte, error)                                    // deterministic: sorted keys, no HTML escaping, RFC3339Nano UTC
```
