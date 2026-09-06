# AGENT RUNTIME

Status: design fixed 2026-09-05; implementation tracked in `docs/build/REQUIREMENTS_TRACEABILITY.md`. Contract for Stage 10. Covers PARTS 9, 65–73, 159, 161, 177, 178, 197, 198.

An agent is the deployable binding of `(account, strategy version, capital envelope, mode)` that walks the lifecycle below. Agent code runs under a `security.Principal` with `ActorType = AGENT`, no roles, and only `agent:*` permissions. It can read through the ToolBroker, request model inference through the ToolBroker, commit predictions and create typed trade intents. Everything after intent creation — eligibility, risk, reservation, planning, quoting, inspection, signing, submission, reconciliation, ledger — is deterministic platform code that treats the intent exactly like a manual one. Even total model compromise cannot move value (PART 9).

## 1. Lifecycle (PART 68)

`agents.state` is exactly the PART 68 machine. `agents.stage` is the last ladder position reached; `state = stage` while active, or one of the side states.

```
DRAFT → COMPILED → VALIDATED → BACKTEST_ELIGIBLE → SHADOW → CANARY → LIMITED → LIVE
side states: PAUSED (returns to stage), FAILED, REVOKED (terminal), SUPERSEDED (terminal; a newer agent replaced it)
```

| Transition | Meaning | Actor |
|---|---|---|
| DRAFT → COMPILED | a `strategy_versions` row with status `COMPILED` exists for the agent's strategy | SYSTEM (compiler) |
| COMPILED → VALIDATED | owner accepted the rendered compiled form (`strategy_versions.status = ACCEPTED`); re-validation against current instruments/tools/risk policy passed | USER (owner) |
| VALIDATED → BACKTEST_ELIGIBLE | dependencies' data sources have `historical_use_permitted = YES`; a dataset manifest exists for the strategy's window | SYSTEM + OPERATOR |
| BACKTEST_ELIGIBLE → SHADOW | promotion gate 1 (§2) | OPERATOR (`agent:promote`) |
| SHADOW → CANARY | promotion gate 2; envelope bound; account kind CANARY (platform capital) | OPERATOR, dual control (`admin_actions` kind `AGENT_PROMOTE`) |
| CANARY → LIMITED | promotion gate 3; customer envelope with reduced caps | OPERATOR, dual control |
| LIMITED → LIVE | promotion gate 4; stronger approval: dual control + RISK role + `LIVE_AGENT_TRADING` capability ACTIVE | OPERATOR + RISK |
| any active → PAUSED | §3 | USER (owner), OPERATOR, SYSTEM (kill switch, budget, model failure, data gap) |
| PAUSED → stage | resume; re-runs gate checks of the current stage | USER/OPERATOR (never SYSTEM for pauses caused by a kill switch; those follow switch release) |
| any → FAILED | unrecoverable: strategy version REVOKED, envelope REVOKED, repeated evaluator errors | SYSTEM/OPERATOR |
| any → REVOKED | operator decision or security event | OPERATOR |
| any → SUPERSEDED | a new agent for the same strategy at the same account is promoted past this one | SYSTEM |

Stages cannot be skipped for autonomous real-capital strategies: `agent.CanTransition` only allows adjacent ladder steps, and `agent_lifecycle_transitions` rows are the only evidence of a promotion. Stage ⇒ run mode: `BACKTEST_ELIGIBLE → BACKTEST|PAPER`, `SHADOW → SHADOW`, `CANARY → CANARY`, `LIMITED → LIMITED`, `LIVE → LIVE`; earlier states never run. The DB CHECK on `agents` enforces the mapping and that CANARY/LIMITED/LIVE carry an `envelope_id`.

## 2. Promotion gates and required evidence (PART 69, 70)

Code deployment is not capital deployment: a new build may run in production while every agent stays in SHADOW. Every promotion writes one `agent_lifecycle_transitions` row carrying: strategy version id and IR hash, evaluation dataset reference + hash, risk policy version + hash, performance snapshot id (mode-specific), error-rate summary, operational-health summary, the approval id, the evidence list `[{kind, ref, hash}]` and its aggregate `evidence_hash`. Missing evidence ⇒ `VALIDATION_FAILED`; the runtime never infers a gate from the UI.

| Gate | Required evidence (all rows must be present and hashed) |
|---|---|
| → SHADOW | compile success (`strategy_versions.status = ACCEPTED`); financial property tests green for the build version; at least one `backtests` row with `status = COMPLETED`, `leakage_test_passed = true`, `pit_validity = POINT_IN_TIME_VALID` over the evaluation dataset; `effect_set ⊆ allowed` re-derived; no CRITICAL security finding open for the build; `risk_policy_version` attached and hash-matched; no `strategy_dependencies` row whose data source is BLOCKED/DISABLED or with an open unrecoverable `stream_gaps` row in the evaluation window |
| → CANARY | all of the above plus: `performance_snapshots(mode = SHADOW)` covering ≥ the configured minimum window and intent count; zero SHADOW intents rejected for `RISK_*` policy violation beyond the configured tolerance; envelope created by `capital.Administer` on a CANARY account; ToolBroker budget report within limits |
| → LIMITED | `performance_snapshots(mode = CANARY)`; ≥ N real fills with `finality = FINALIZED`; every CANARY order reconciled (`reconciliation_records` MATCHED or RESOLVED, none material); zero `SUBMISSION_UNKNOWN` unresolved; zero unexpected submissions (every `execution_attempts` row maps to an approved plan of a CANARY intent of this agent); zero policy violations; prediction outcomes resolved for ≥ M predictions |
| → LIVE | everything for LIMITED at the LIMITED stage plus: dual-controlled `admin_actions` approved by a RISK-role principal with step-up; `LIVE_AGENT_TRADING` capability gate ACTIVE in this environment; calibration snapshot present; operator attestation reference. No automatic promotion exists at any stage (PART 70). |

Minimum windows/counts are configuration (`config.Agents.Promotion`) validated non-zero in PROD; they are never hardcoded to satisfy marketing.

## 3. Pause semantics (PART 71)

Pause is recorded in `agent_pauses` (one open row per agent) and mirrors a kill switch `AGENT_PAUSE(agent_id)` so the risk kernel and the intent service see it without consulting the agent subsystem. While paused: the trigger dispatcher creates no runs (`SKIPPED{AGENT_PAUSED}` is recorded for the first suppressed trigger per hour for observability); the intent service rejects new AGENT intents for that agent (`KILL_SWITCH_ACTIVE`); historical runs, predictions, intents and orders are untouched; orders already SUBMITTED continue observation, reconciliation, settlement, ledger posting and position updates (kill switches never block those action classes); cancelable open orders are cancelled only by the explicit `agent.CancelOpenOrders` workflow chosen at pause time (`open_orders_policy = CANCEL_CANCELABLE`), never implicitly. An external transaction is never abandoned because its strategy was paused. Resume requires a non-agent principal, records the resumer and re-checks the stage gate; envelope status is not changed by pause/resume (envelope authority is separate).

## 4. Capital binding

An agent binds to exactly one `capital_envelopes` row (`agents.envelope_id`; `capital_envelopes.agent_id` is a FK back to `agents`, added in 00501). The runtime reads the envelope snapshot for `EvalInput.Envelope` and passes `EnvelopeID` on every intent; the reservation path (`capital.Reserver.Reserve/Consume/Release`) is the only envelope write reachable from agent code and it can only decrease `available_usd_minor`. Authority fields (`allocation_usd_minor`, `max_*`, `allowed_*`, `policy_version`, `status`, `effective_at`, `expires_at`) change only through `capital.EnvelopeAdmin` under a non-agent principal; `internal/agent` does not import `capital` at all — it receives a `capital.EnvelopeReader` interface. IR `EnvelopeRequirements` are checked against the bound envelope at binding and at every promotion (`RISK_ENVELOPE_*` reasons on mismatch).

## 5. ToolBroker (PART 66)

Agents never hold API secrets or URLs. Every read and every model call goes through `agent.ToolBroker`, which enforces, in order: (1) **permission** — the tool's effect must be in the run's effect set and the tool must be ACTIVE and not under `PROVIDER_DISABLE_NEW_ACTIONS`/`MODEL_DISABLE`; (2) **budgets** (§6); (3) **rate limits** — per tool and per agent from persisted counters in `tool_invocations` (Redis may pre-check, Postgres decides); (4) **egress policy** — the tool adapter may only connect to hosts listed in `tools.egress_hosts`, enforced by an HTTP transport allowlist and a network policy; there is no generic HTTP tool; (5) **provenance** — one `tool_invocations` row per call: `tool_id`, `tool_version`, `agent_run_id`, `request_hash`, `output_hash`, `output_ref` (archive URI), `source` (provider), `provider_published_at`, `received_at`, `decision_available_at`, `cost_usd_minor`, `latency_ms`, `success`, `error_code`, `rate_limited`, `mode`. Outputs are returned to the evaluator as `strategy.Observation` values with their six timestamps; the broker never returns raw provider responses to the agent. Model calls additionally write `model_calls` (PART 65) and are always schema-constrained; the broker assembles the three-segment prompt (STRATEGY_IR.md §10) — the agent supplies data, never prompt text.

## 6. Budgets (PART 197)

| Budget | Source of limit | Enforced from | Exhaustion behaviour |
|---|---|---|---|
| capital | `capital_envelopes` | reservation row locks | `INSUFFICIENT_BUYING_POWER` / `RISK_ENVELOPE_EXHAUSTED`; run records skip |
| data | `min(IR.DataBudget, envelope.max_data_spend_usd_minor)` per run/day | `SUM(tool_invocations.cost_usd_minor)` for the UTC day, inside the broker's transaction | tool call refused `BUDGET_EXHAUSTED{DATA}`; dependency treated as missing |
| model | `min(IR.ModelBudget, envelope.max_model_spend_usd_minor)`; calls/run, calls/day, tokens/call | `model_calls` sums + counts | call refused `BUDGET_EXHAUSTED{MODEL}`; required ⇒ run SKIPPED; agent auto-PAUSED after the configured number of consecutive refusals |
| order count | `min(IR.Envelope.MaxIntentsPerHour, envelope.max_order_rate_per_hour)`; also risk kernel `RISK_ORDER_RATE` | `trade_intents` count in the window | intent refused `RATE_LIMITED`; run SKIPPED |

Each is checked independently; none relies on another to fail first. A looping model or a burst of events cannot create unbounded provider charges because the day limits are persisted counters and the broker refuses before dialing.

## 7. Model failure, reasoning storage, mode

- **Model failure (PART 177):** `ModelProvider` errors (`PROVIDER_UNAVAILABLE`, `RATE_LIMITED`, schema `VALIDATION_FAILED`, `KILL_SWITCH_ACTIVE` for `MODEL_DISABLE`) never produce a synthetic output. If the IR marks the model as required, the run ends `SKIPPED{MODEL_UNAVAILABLE}` and no intent is created; if optional, evaluation continues with the model output absent and any condition referencing it is false. After `config.Agents.ModelFailurePauseAfter` consecutive failures the agent is auto-PAUSED with reason `MODEL_UNAVAILABLE`. There is no heuristic fallback path.
- **Reasoning storage (PART 178):** an `agent_runs` row stores structured `rationale` (the model's schema field), `evidence` (tool invocation ids, model call ids, freshness checks per dependency), `signals` and `condition_results`, the decision output (`skip_reason` or `intent_id`) and model metadata via `model_calls`. Hidden chain-of-thought is never requested or stored.
- **Mode (PART 159):** `mode` is a NOT NULL column on `agent_runs`, `predictions`, `trade_intents`, `orders`, `tool_invocations`, `performance_snapshots` and `cost_accounting`; a guard trigger forbids changing it on `agent_runs`, and the intent trigger requires prediction, run and intent modes to agree. Mode is copied from `agents.mode` at run creation and never inferred.

## 8. Prediction ledger (PART 72, 73)

Before any consequential autonomous trade the run commits a `predictions` row: `agent_id`, `agent_run_id`, `strategy_version_id`, `account_id`, `mode`, `instrument_id`, `horizon_ms`, `direction` (UP/DOWN/FLAT), `probability_direction` (scale-6 decimal in [0,1]), `expected_return_bps`, `downside_probability`, `max_downside_bps`, `confidence`, `information_set_hash` (sha256 over the sorted `(dependency, tool_invocation_id, output_hash, decision_available_at)` tuples of the snapshot), `decision_available_at` (= max `decision_available_at` in the information set), `committed_at`, `rationale`, `evidence_refs`, `signals`. Predictions are immutable. **Prediction predates execution** is enforced three times: the intent service requires `PredictionID` on AGENT intents (`intent.Validate`), the `trade_intents` BEFORE INSERT trigger checks `predictions.committed_at <= trade_intents.requested_at` and that agent, strategy version and mode agree, and the FK `trade_intents.prediction_id → predictions(id)`.

Calibration is computed later by `prediction.Calibrator`, never by the run: `prediction_outcomes` resolves each prediction at `horizon_end_at` from `asset_prices` (valuation source recorded) into realized direction, realized return bps, realized max drawdown bps, Brier and log loss (scale-8 decimals; log loss uses a bounded epsilon, no float), regime label. `calibration_snapshots` aggregates per `(strategy_version, mode, window, regime, probability bucket)`: count, mean predicted, realized frequency, mean Brier, mean log loss, expected vs realized return, confidence vs absolute error. A high return never counts as good calibration; the two are separate columns in performance snapshots and are never combined.

## 9. Agent trade end-to-end (PART 161)

| # | Step | Package | Persisted evidence |
|---|---|---|---|
| 1 | normalized event arrives from the stream | `internal/reality` | `normalized_events` (ClickHouse), `ingest_checkpoints` |
| 2 | trigger dispatcher matches ON_EVENT/ON_INTERVAL, dedups, rate-checks, opens a run | `internal/agent` (dispatcher) | `agent_runs(status = STARTED, mode, envelope_id, trigger_dedup_key)` |
| 3 | dependencies gathered as a point-in-time snapshot through the broker | `internal/agent` (ToolBroker) + `internal/reality` (Snapshotter) | `tool_invocations`, freshness checks in `agent_runs.evidence` |
| 4 | `strategy.Evaluate`; model calls satisfied through the broker, second evaluation | `internal/strategy`, `internal/model` | `model_calls`, `agent_runs.signals/condition_results` |
| 5 | structured prediction committed | `internal/prediction` | `predictions` (same tx as run update) |
| 6 | typed intent emitted with idempotency key `run:<agent_run_id>:<action>` | `internal/agent` → `internal/intent` | `trade_intents(actor_type = AGENT, prediction_id, strategy_version_id, mode)` |
| 7 | eligibility (context `TRADE`, agent context flags) | `internal/eligibility` | `eligibility_decisions` |
| 8 | risk PRE_TRADE with envelope snapshot | `internal/risk` | `risk_decisions` |
| 9 | capital reservation against asset and envelope | `internal/capital` | `asset_reservations`, envelope counters |
| 10 | settlement plan | `internal/settlement` | `execution_plans`, `execution_plan_steps` |
| 11 | quote | `internal/execution` adapter | `quotes` |
| 12 | build + inspect transaction; FINAL risk | `internal/execution`, `internal/risk` | `execution_attempts(inspection_result)`, `risk_decisions(stage = FINAL)` |
| 13 | bounded signing | `internal/signing` (never reachable from agent code) | `signing_decisions` |
| 14 | submission + finality observation | `internal/execution` | `execution_attempts`, `fills` |
| 15 | reconciliation | `internal/reconciliation` | `reconciliation_records` |
| 16 | ledger posting, position update, reservation release | `internal/ledger`, `internal/positions`, `internal/capital` | `journal_transactions`, `position_lots`, `lot_dispositions` |
| 17 | performance and cost attribution | `internal/performance` | `cost_accounting`, later `performance_snapshots` |
| 18 | audit event at every step above | `internal/audit` | `audit_events(stream = agent:<id>)` |

At no point does a model, a prompt or an agent process see a wallet key, a signing token or a provider secret; steps 7–16 are identical to the manual flow.

## 10. Go contracts (fixed)

```go
// internal/strategy
type StrategyID = id.ID[strategyKind]; type VersionID = id.ID[versionKind]
type Compiler interface {
    CompileNL(ctx context.Context, req NLCompileRequest) (CompileResult, error)   // bounded attempts; every attempt persisted
    CompileDocument(ctx context.Context, req DocumentCompileRequest) (CompileResult, error)  // SDK/clone path: PARSE onward
}
type NLCompileRequest struct { StrategyID StrategyID; OwnerAccountID, OwnerUserID string; Text string; RiskPolicyVersion string; IdempotencyKey string }
type CompileResult struct { Version *Version; Attempts []CompileAttempt; Outcome string /* SUCCESS | REJECTED | NEEDS_CLARIFICATION | MODEL_UNAVAILABLE */; Codes []string }
func Validate(ir ir.IR, refs ValidationRefs) ValidationReport
func DeriveEffects(ir ir.IR) []ir.Effect
func Hash(ir ir.IR) ([]byte, error)
func Render(ir ir.IR) (string, error)
func Evaluate(in EvalInput) (EvalOutput, error)    // STRATEGY_IR.md §8
type Store interface { CreateVersion(ctx, tx pgx.Tx, v Version) (Version, error); Get(ctx, q db.Querier, id VersionID) (Version, error); RecordAttempt(ctx, tx, a CompileAttempt) error; SetStatus(ctx, tx, id VersionID, to Status, actor security.Principal, reason string) error }

// internal/agent
type AgentID = id.ID[agentKind]; type State string; type Mode string
func CanTransition(from, to State) bool
type Lifecycle interface {   // every method rejects ActorType AGENT before any query
    Create(ctx, tx pgx.Tx, req CreateAgent) (Agent, error)
    Promote(ctx, tx pgx.Tx, id AgentID, to State, ev PromotionEvidence) (Agent, error)
    Pause(ctx, tx pgx.Tx, id AgentID, req PauseRequest) (Pause, error); Resume(ctx, tx, id AgentID, reason string) (Agent, error)
    Revoke(ctx, tx, id AgentID, reason string) (Agent, error); Supersede(ctx, tx, id, by AgentID) (Agent, error)
}
type PromotionEvidence struct { StrategyVersionID string; IRHash []byte; DatasetRef string; DatasetHash []byte; RiskPolicyVersion string; RiskPolicyHash []byte; PerformanceSnapshotID, BacktestID *string; ErrorRateBPS money.BPS; OperationalHealth json.RawMessage; ApprovalID *string; Evidence []EvidenceRef; Reason string }
type Dispatcher interface { OnEvent(ctx, ev reality.NormalizedEvent) error; OnTick(ctx, now time.Time) error }   // creates runs; never evaluates inline
type Runner interface { Run(ctx context.Context, runID RunID) (RunResult, error) }   // one bounded evaluation; resumable; idempotent per run
type ToolBroker interface { Invoke(ctx context.Context, call ToolCall) (strategy.Observation, error); Budgets(ctx, q db.Querier, id AgentID, now time.Time) (BudgetSnapshot, error) }
type ToolCall struct { RunID RunID; Dependency ir.Dependency; Params map[string]string; Effect ir.Effect; Deadline time.Time }
type IntentEmitter interface { Emit(ctx, tx pgx.Tx, t intent.TradeIntent) (intent.TradeIntent, error) }   // the only money-adjacent write agent code can reach

// internal/model
type ModelProvider interface { Name() string; Complete(ctx context.Context, req Request) (Response, error) }
type Request struct { TemplateVersion string; SystemPolicy string; ToolResults []Segment; Untrusted []Segment; OutputSchema json.RawMessage; MaxOutputTokens int64; Deadline time.Time; Purpose string /* COMPILE | RUNTIME */ }
type Segment struct { Kind string /* TOOL_RESULT | UNTRUSTED */; Label string; ProvenanceRef string; Content string }
type Response struct { Provider, ModelID string; Structured json.RawMessage; StopReason string; Usage Usage; RequestedAt, RespondedAt time.Time; InputHash, OutputHash []byte; RawRef string }
type Usage struct { InputTokens, OutputTokens int64; Cost money.USD }
// Fake provider in internal/model/modeltest replays recorded responses; rejected in PROD by config validation.

// internal/prediction
type PredictionID = id.ID[predictionKind]
type Prediction struct { ID PredictionID; AgentID, RunID, StrategyVersionID, AccountID string; Mode agent.Mode; InstrumentID instruments.InstrumentID; HorizonMS int64; Direction string; ProbabilityDirection, DownsideProbability, Confidence ir.Decimal; ExpectedReturnBPS, MaxDownsideBPS money.BPS; InformationSetHash []byte; DecisionAvailableAt time.Time; Rationale, EvidenceRefs, Signals json.RawMessage }
func (p Prediction) Validate() error       // ranges, scale, DecisionAvailableAt <= now supplied by caller
type Ledger interface { Commit(ctx, tx pgx.Tx, p Prediction, now time.Time) (PredictionID, error); Get(ctx, q, id PredictionID) (Prediction, error); DueForResolution(ctx, q, now time.Time, limit int) ([]Prediction, error); RecordOutcome(ctx, tx, o Outcome) error }
type Calibrator interface { Compute(ctx, q db.Querier, scope CalibrationScope, now time.Time) ([]CalibrationRow, error) }
func Brier(p ir.Decimal, outcome bool) ir.Decimal; func LogLoss(p ir.Decimal, outcome bool, eps ir.Decimal) ir.Decimal   // fixed point
```

## 11. What agent code may never import

`internal/agent`, `internal/strategy`, `internal/model`, `internal/prediction` and every tool adapter under `internal/agent/tools` must not import: `internal/signing`, `internal/wallet`, `internal/admin`, `internal/capital` (they receive `capital.EnvelopeReader` and the reservation is performed by `internal/intent`), `internal/risk` (policy mutation; the evaluator receives policy values through `EvalInput`), `internal/gates`, `internal/killswitch` (controller; the checker is consulted by the intent service), `internal/funding`, `internal/ledger` writers, `internal/execution`, `internal/config` secret resolvers, and any provider SDK. Enforced by depguard rules in `.golangci.yml` and `test/security/TestAgentImportBoundary`; the AGENT principal's permission set is `agent:run`, `agent:tool_invoke`, `prediction:commit`, `trade:create_agent_intent` and nothing else (`TestAgentPrincipalCannot*`).

## 12. Tests required before Stage 10 exit

- Lifecycle table test: every non-adjacent ladder transition and every AGENT-actor transition returns `INVALID_STATE_TRANSITION`/`FORBIDDEN`; each gate refuses with missing evidence.
- Pause: paused agent produces no runs and no intents; a SUBMITTED order of a paused agent still reaches SETTLED and posts to the ledger (fault-injected observer).
- Budgets: each of the four budgets exhausted independently refuses exactly the right call; provider fake counts zero dials after refusal.
- Prediction-predates-intent: an intent whose prediction is committed later, or with mismatched mode/agent, is rejected by trigger `AG002/AG003` and by `intent.Validate`.
- Runner resume: crash after each step of §9 (steps 2–6) resumes with exactly one run row, one prediction, one intent.
- Import boundary and permission tests; model failure never produces an intent; egress allowlist test with a tool configured for a host outside the list fails closed.
