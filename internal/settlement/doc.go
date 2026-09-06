// Package settlement is the Settlement Compiler and its executor (goal PARTS
// 38, 39, 40, 46, 106, 107, 220, 228, 230; SETTLEMENT_COMPILER.md; ADR-0014).
// It is the only route from a typed intent to an external execution: no
// worker, API or script submits a transaction without a plan produced here.
//
// # Responsibilities
//
//   - Planner.Plan is a pure function: intent snapshot + account, buying
//     power, instrument, listings, asset policies, prices, holdings, system
//     health, eligibility and PRE_TRADE risk decisions, fee policy and Now in;
//     an ExecutionPlan or NO_VALID_PLAN out. It performs no I/O, reads no
//     clock, calls no model and iterates no map without sorting; the plan hash
//     is the determinism witness (identical inputs give identical hashes
//     across runs and goroutines).
//   - The V1 plan is the fixed step DAG of PART 38 (VALIDATE_ELIGIBILITY …
//     RELEASE_RESERVATION). Each step carries a semantic idempotency key, a
//     retry class, a timeout, a finality policy, a compensation policy and
//     typed evidence inputs. The reserved step types CONVERT, TRANSFER,
//     WAIT_FINALITY and TRADE exist in the type system for cross-chain plans
//     and are never emitted in V1 (PART 230).
//   - Sizing (§4): ACQUIRE_NOTIONAL converts USD into settlement-asset base
//     units under the stablecoin's current policy (NORMAL at face, DEGRADED at
//     market price with the collateral haircut, otherwise unavailable);
//     REDUCE_NOTIONAL and CLOSE_POSITION size from open holdings;
//     TARGET_EXPOSURE takes the delta against current marked exposure.
//   - Hard constraints are the strictest of the intent's constraints and the
//     risk kernel's resulting constraints; the plan never loosens either
//     (property test).
//   - NO_VALID_PLAN is a legitimate outcome carrying every applicable reason
//     code of §3; the planner never forces a route to satisfy UX (PART 40).
//   - PlanRepository persists plans and steps. Approve freezes a plan (the
//     execution_plans_guard trigger refuses later changes to frozen fields);
//     replanning creates version+1 and marks the previous plan SUPERSEDED.
//   - Executor runs an approved plan step by step with durable per-step
//     state in Postgres: RUNNING is persisted before any side effect,
//     SUCCEEDED/FAILED/UNKNOWN after. A restart resumes: SUCCEEDED steps are
//     skipped and a SUBMIT step found RUNNING or UNKNOWN is never re-submitted
//     — the order becomes SUBMISSION_UNKNOWN and the Recoverer decides
//     (adopt, proven absent, or RECONCILIATION_REQUIRED; EXECUTION.md §4).
//     Kill switches are checked for the plan's action class before
//     RESERVE_CAPITAL and SUBMIT and never after submission:
//     OBSERVE_FINALITY, RECONCILE, POST_LEDGER, UPDATE_POSITION and
//     RELEASE_RESERVATION run under any switch. Every step appends an audit
//     event; every external call archives request and response evidence.
//   - Dry-run (PART 220): a plan with DryRun set stops after
//     INSPECT_TRANSACTION and makes no reservation, order or attempt. The
//     dry-run executor is constructed without a Signer (NewDryRunExecutor has
//     no signer parameter), so it cannot reach a signing service.
//
// # What this package must never do
//
//   - Submit twice for one plan, or retry SUBMIT (UNKNOWN_EFFECT_WRITE)
//     without a status investigation. A transport timeout is not a failure.
//   - Perform I/O, read a clock, call a model, use randomness or floating
//     point, or depend on map order inside Planner.Plan.
//   - Emit CONVERT, TRANSFER, WAIT_FINALITY or TRADE steps, or plan across
//     chains: money on the wrong rail is NO_VALID_PLAN
//     {SETTLEMENT_ASSET_UNAVAILABLE}, not an implicit bridge.
//   - Loosen an intent constraint or a risk resulting constraint, or pick a
//     listing on a DISABLED venue, a listing that is not ACTIVE, or a
//     provider whose health forbids new actions.
//   - Change a frozen plan field after approval, or execute a plan that is
//     not APPROVED (or already EXECUTING).
//   - Block OBSERVE_FINALITY, RECONCILE, POST_LEDGER, UPDATE_POSITION or
//     RELEASE_RESERVATION on a kill switch, an agent pause or a provider
//     breaker (PART 107).
//   - Post a fill to the ledger twice, apply it to positions twice, consume
//     a reservation for a fill twice, or leave a reservation unsettled when
//     the plan finishes.
//   - Let a dry-run plan reach a signer, a submission, a reservation or an
//     order.
//   - Contain provider-specific logic or import a provider package.
package settlement
