# SETTLEMENT COMPILER

Status: design fixed 2026-09-05; implementation tracked in `docs/build/REQUIREMENTS_TRACEABILITY.md`. Contract for Stage 5. Covers PARTS 35, 38–42, 46, 106, 220, 228–230.

## 1. Purpose

The Settlement Compiler turns a typed `TradeIntent` plus account, buying-power, instrument, venue, and system-health state into an immutable, typed `ExecutionPlan`, or into `NO_VALID_PLAN`. It is deterministic, contains no provider-specific logic, never calls a model, and is fully testable without any network.

```
TradeIntent + AccountState + BuyingPower + Instrument/Listings + AssetPolicies + SystemHealth + Eligibility + Risk(PRE_TRADE) + FeePolicy + Now
        │
        ▼  settlement.Planner.Plan (pure)
ExecutionPlan{steps DAG, hard constraints, estimated costs, selected listing, settlement asset, policy versions, hash}
        │
        ▼  settlement.Executor.Run (durable, resumable)
step-by-step execution through provider-neutral adapters, each transition persisted in execution_plan_steps
```

## 2. Intent (PART 35)

```go
// internal/intent
type Action string // ACQUIRE_NOTIONAL | REDUCE_NOTIONAL | CLOSE_POSITION | TARGET_EXPOSURE ; BUY_EVENT_OUTCOME is declared but Validate() rejects it (disabled)
type Constraints struct {
  MaxSlippageBPS, MaxFeeBPS, MaxPriceImpactBPS money.BPS
  MaxPrice *money.Price; MinReceive *money.Quantity
  AllowedVenues []string; QuoteFreshness time.Duration; ExecutionDeadline time.Time
}
type TradeIntent struct {
  ID IntentID; AccountID string; ActorType security.ActorType; ActorID string
  AgentID, StrategyVersionID, PredictionID *string
  Action Action; InstrumentID instruments.InstrumentID
  NotionalUSD *money.USD; TargetExposureUSD *money.USD; Quantity *money.Quantity
  Constraints Constraints; Deadline time.Time; RequestedAt time.Time
  IdempotencyKey, CorrelationID string; Mode Mode
}
func (t TradeIntent) Validate() error   // structural + action/field consistency + constraints sanity; AGENT intents require PredictionID and StrategyVersionID
```

The intent state machine (`RECEIVED → ELIGIBILITY_CHECKED → RISK_CHECKED → RESERVED → PLANNED → EXECUTING → COMPLETED`, side states `REJECTED, EXPIRED, CANCELLED, FAILED, NO_VALID_PLAN`) is an explicit transition table in `intent.Transitions`; illegal transitions fail with `INVALID_STATE_TRANSITION`.

## 3. Planner input and output

```go
// internal/settlement
type PlannerInput struct {
  Intent         intent.TradeIntent
  Account        AccountState            // ID, Status, Restrictions, Mode, WalletID, CostBasisMethod
  BuyingPower    capital.BuyingPower
  Instrument     instruments.Instrument
  Listings       []instruments.VenueListing   // candidates after eligibility filtering
  AssetPolicies  map[assets.AssetID]valuation.AssetPolicy
  Health         SystemHealth            // Providers map[string]provider.Health; Chains map[string]ChainStatus; ActiveKillSwitches []killswitch.Switch; ReconciliationBlocksNewRisk bool
  Eligibility    eligibility.Decision
  Risk           risk.Decision           // stage PRE_TRADE
  FeePolicy      fees.Policy
  Now            time.Time
  PlannerVersion string
}
type Planner interface { Plan(in PlannerInput) (Plan, error) }   // error is *errs.Error NO_VALID_PLAN carrying reason codes, or VALIDATION_FAILED
```

`Plan` and `Step` mirror `execution_plans` / `execution_plan_steps` exactly (see migration 00202). `Plan.Hash` is `sha256(audit.CanonicalJSON(plan without Hash, Status, timestamps))`. Approving a plan freezes it; any replanning creates `version+1` and marks the previous plan `SUPERSEDED`.

### Step DAG for a V1 spot swap (PART 38)

```
VALIDATE_ELIGIBILITY → EVALUATE_RISK → RESERVE_CAPITAL → RESOLVE_VENUE_LISTING → LOCATE_SETTLEMENT_ASSET
→ ACQUIRE_QUOTE → VALIDATE_QUOTE → FINAL_RISK_CHECK → BUILD_TRANSACTION → INSPECT_TRANSACTION → REQUEST_SIGNATURE
→ SUBMIT → OBSERVE_FINALITY → RECONCILE → POST_LEDGER → UPDATE_POSITION → RELEASE_RESERVATION
```

Each step carries: `SemanticIdempotencyKey = "<plan_id>:<seq>:<type>"`, `RetryClass` (SAFE_RETRY for reads and pure checks; IDEMPOTENT_WRITE for ledger/position/reservation steps keyed by fill or plan; UNKNOWN_EFFECT_WRITE for SUBMIT), `Timeout`, `FinalityPolicy` (which finality level the step waits for, from the instrument's settlement rules and the action class), `CompensationPolicy` (`RELEASE_RESERVATION`, `NONE`, `RECONCILE_ONLY`).

Reserved step types `CONVERT`, `TRANSFER`, `WAIT_FINALITY`, `TRADE` exist in the type system for future cross-chain plans; the planner never emits them in V1 (test asserts this).

### NO_VALID_PLAN reasons (PART 40)

`SETTLEMENT_ASSET_UNAVAILABLE` (money on the wrong rail), `DEADLINE_IMPOSSIBLE`, `QUOTE_TOO_EXPENSIVE`, `VENUE_DISABLED`, `LIQUIDITY_INSUFFICIENT`, `PROVIDER_DEGRADED`, `ELIGIBILITY_FAILED`, `RISK_REJECTED`, `INSTRUMENT_STATUS` (buy on CLOSE_ONLY/RESTRICTED/HALTED; any on HALTED), `KILL_SWITCH`, `RECONCILIATION_BLOCKED`, `NOTIONAL_BELOW_MINIMUM`, `NOTIONAL_ABOVE_MAXIMUM`, `NO_ELIGIBLE_LISTING`. The planner never forces a route to satisfy UX.

## 4. Sizing and constraints

For `ACQUIRE_NOTIONAL` of `N` USD: settlement-asset quantity = `money.Notional`-style exact conversion of `N` into settlement asset base units using the stablecoin's current policy (NORMAL → face; DEGRADED → market price with haircut); the plan's `HardConstraints` carry `MaxInputQuantity` (reserved quantity), `MinOutputQuantity` (from constraints and quote), `MaxSlippageBPS`, `MaxFeeBPS`, `MaxPriceImpactBPS`, `MaxNetworkFee`, `QuoteMaxAge`, `Deadline`, and the risk kernel's `resulting_constraints`, taking the strictest of intent and risk values. `REDUCE_NOTIONAL` / `CLOSE_POSITION` size from open lots; `TARGET_EXPOSURE` computes the delta against current marked exposure and becomes an acquire or reduce, or `NO_VALID_PLAN{DELTA_BELOW_MINIMUM}`.

## 5. Executor (PART 46, 47, 48)

`Executor.Run(ctx, planID)` loads the plan and executes steps in dependency order:

- persist `RUNNING` before side effects, `SUCCEEDED/FAILED/UNKNOWN` after, always in Postgres;
- a crash and restart resumes: SUCCEEDED steps are skipped; a `SUBMIT` step in `RUNNING`/`UNKNOWN` never re-submits — it enters the unknown-submission recovery path (see EXECUTION.md §4);
- every step writes an audit event; every external call stores request/response evidence references;
- order state transitions follow the PART 47 table and emit outbox events;
- kill-switch checks are evaluated for `NEW_RISK` before `SUBMIT`; after submission, `OBSERVE_FINALITY`, `RECONCILE`, `POST_LEDGER`, `UPDATE_POSITION` are never blocked by kill switches or agent pause;
- dry-run mode (`plan.dry_run = true`) executes everything up to and including `INSPECT_TRANSACTION` and then stops; it can never call the signing service (enforced by a type-level guard: the dry-run executor is constructed without a signer).

## 6. Provider-neutral adapter contract (PART 41)

```go
// internal/execution
type ExecutionAdapter interface {
  Name() string
  Quote(ctx context.Context, req QuoteRequest) (quote.Quote, error)
  ValidateQuote(ctx context.Context, q quote.Quote) error
  Build(ctx context.Context, req BuildRequest) (UnsignedAction, error)
  Submit(ctx context.Context, req SignedSubmission) (SubmissionResult, error)   // transport timeout → *errs.Error SUBMISSION_STATE_UNKNOWN
  Status(ctx context.Context, ref ExternalReference) (ExecutionStatus, error)
  Cancel(ctx context.Context, ref ExternalReference) error                     // UNSUPPORTED for atomic-swap venues
  Reconcile(ctx context.Context, scope ReconcileScope) ([]ExternalExecutionEvent, error)
}
type UnsignedAction struct { Chain string; Bytes []byte; Hash []byte; RecentBlockhash string; LastValidBlockHeight uint64; ExpiresAt time.Time; ProviderRequestID string; RawRef string }
type SubmissionResult struct { ExternalRef ExternalReference; AcceptedAt time.Time; RawRef string }
type ExecutionStatus struct { State ExternalState /* PENDING|OBSERVED|CONFIRMED|FINALIZED|FAILED|EXPIRED|NOT_FOUND */; Fills []ExternalExecutionEvent; Slot uint64; Error string; RawRef string }
type ExternalExecutionEvent struct { Venue, ExternalFillID, TxSignature string; Slot uint64; InputAsset, OutputAsset assets.AssetID; InputQty, OutputQty, NetworkFee, VenueFee money.Quantity; NetworkFeeAsset assets.AssetID; ObservedAt time.Time; Finality string; RawRef string }
```

Provider retry matrix (PART 106) is codified as `RetryClass` on each adapter method: `Quote/ValidateQuote/Status/Reconcile` = SAFE_RETRY; `Build` = SAFE_RETRY (pure construction); `Submit` = UNKNOWN_EFFECT_WRITE (never retried without status investigation); `Cancel` = IDEMPOTENT_WRITE only where the venue guarantees it.

## 7. Fee policy (PART 126)

`fees.Policy{Version, PlatformFeeBPS money.BPS, MinFee, MaxFee money.Quantity, FeeAsset assets.AssetID}`; default `PlatformFeeBPS = 0` until configured. The quote shows venue fee, network estimate, platform fee, and total estimated cost separately; the fill posts the platform fee as its own journal entries. No hidden spread exists anywhere in the code path (test: expected output on the quote equals the venue's quoted output; platform fee is an explicit separate deduction).

## 8. Tests required before Stage 5 exit

- Planner golden corpus (JSON inputs → expected plan hash or NO_VALID_PLAN reasons), ≥ 40 cases including each NO_VALID_PLAN reason, each action type, CLOSE_ONLY sell-allowed/buy-rejected, DEGRADED stablecoin sizing, deadline impossible, provider degraded.
- Determinism: same input → identical plan hash across 1,000 iterations and under `-race`.
- Property: for any valid intent and state, plan hard constraints are never looser than intent constraints or risk `resulting_constraints`.
- Executor resume tests: crash after each step (fault injection) → resume completes exactly once with no duplicate side effects (uses fake adapter with effect counters).
- Dry-run can never reach the signer (compile-time constructor guard + runtime test).
