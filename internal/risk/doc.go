// Package risk is the deterministic risk kernel (goal PARTS 58-60, 79, 174,
// 181, 224; POLICY_AUTHORITY §4). It decides whether an intent may proceed
// and what constraints the settlement compiler must respect, as a pure
// function of a versioned policy and a typed input snapshot.
//
// # Responsibilities
//
//   - Policy is the typed limits document stored in risk_policies.rules.
//     Every limit is money.USD, money.BPS, an integer count or a duration in
//     milliseconds; there are no floating point values anywhere. ParsePolicy
//     rejects unknown keys, fractional numbers where integers are expected
//     and unknown enum members.
//   - Compose(global, account, agent) builds the effective policy: the
//     strictest value wins for every limit and allowlists intersect. An
//     ACCOUNT or AGENT policy can therefore only tighten the GLOBAL one.
//   - Evaluate(policy, input) runs every check for the stage (PRE_TRADE,
//     FINAL, CONTINUOUS) and returns ALLOW or REJECT with every violated
//     reason code, sorted, plus the resulting constraints (maximum notional,
//     tightened slippage, fee and price-impact bounds, quote age, minimum
//     liquidity) and a hash of the decision.
//   - Kill switches are applied by action class: NEW_RISK is blocked by any
//     matching switch; REDUCE_RISK is blocked by INSTRUMENT_HALT,
//     CHAIN_DISABLE_NEW_ACTIONS, PROVIDER_DISABLE_NEW_ACTIONS, by
//     ACCOUNT_FREEZE when the policy says so, and by GLOBAL_NEW_RISK_KILL
//     unless the policy allows risk reduction during a kill.
//   - Store persists policies and decisions, composes the effective policy
//     from GLOBAL, ACCOUNT and AGENT rows, and counts orders from the
//     persisted trade_intents table so order-rate limiting never depends on
//     Redis.
//
// # Fail closed
//
// A missing or incomplete policy yields RISK_POLICY_MISSING; an unknown
// stage, action or a zero evaluation time yields RISK_INPUT_INVALID; unknown
// liquidity, an unknown provider health, an unknown kill-switch kind or a
// data dependency without a freshness bound are all treated as violations.
// DefaultGlobalPolicyJSON carries deliberately small initial limits and an
// empty venue allowlist; it requires risk sign-off before use.
//
// # What this package must never do
//
//   - Call a model or an LLM to decide permission (PART 58).
//   - Read a clock: EvaluatedAt and every age is derived from Input.Now.
//   - Depend on map iteration order, floating point or randomness (PART 224);
//     every list in the output is sorted and hashed canonically.
//   - Let an agent write policy: RecordPolicy refuses AGENT actors and AGENT
//     principals before any query (PART 60), and the created_by_actor_type
//     CHECK constraint refuses them again.
//   - Import capital, killswitch, reconciliation or any agent-facing package;
//     it consumes typed snapshots supplied by the intent service.
package risk
