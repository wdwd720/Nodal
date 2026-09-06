# ADR-0015: Universal Buying-Power Engine as a dedicated domain

## Status

Accepted — implementation tracked in docs/build/REQUIREMENTS_TRACEABILITY.md

## Date

2026-09-05

## Context

"How much can this account deploy for this action right now?" is not "sum the
wallet balances" (PART 25). Treating it as a sum produces the following failures:

- Counting funding that is pending or reversible as spendable; a chargeback after
  the trade leaves the platform holding the loss (PART 27).
- Ignoring active reservations, so two concurrent intents (a manual trade and an
  agent trade) both see the full balance (PART 22, 23).
- Hard-coding `1 USDC = $1`; during a depeg the platform extends buying power that
  the collateral no longer supports (PART 26).
- Ignoring asset safety state, chain status, provider status, and venue
  eligibility, so an action is permitted that cannot settle (PART 33, 79).
- Caching the figure in Redis and reading it back as truth; a stale cache is a
  double-spend (PART 119).
- Implicit credit: "instant cross-chain buying power" or treasury-fronted settlement
  turns the platform into a lender without deciding to be one (PART 4).
- Inconsistent numerics across modules, so the number shown, the number reserved,
  and the number risk-checked differ by rounding.

## Decision

- A dedicated `internal/capital` (buying-power) domain computes the PART 25 output
  (`portfolio_value`, `buying_power`, `available_now`, `reserved`, `pending`,
  `withdrawable`, `underlying_balances`, `haircuts`, `restrictions`,
  `policy_version`, `as_of`) as a pure function of typed inputs: wallet balances,
  pending funding and reversibility holds, withdrawal holds, active reservations,
  asset collateral factors and safety state, stablecoin status, chain and provider
  status, venue eligibility, account restrictions, position exposure, risk policy,
  settlement feasibility, and quote requirements.
- Stablecoin status (`NORMAL`, `DEGRADED`, `RESTRICTED`, `HALTED`) and collateral
  factors are configurable policy (PART 26); no peg is hard-coded. Under
  `DEGRADED` the market price and a haircut apply; `RESTRICTED` permits no new
  risk; `HALTED` blocks affected operations.
- Funding reversibility is modelled as a hold with a policy-driven availability
  time; pending funding contributes to `pending`, not `available_now`.
- The engine's output is a valuation (PART 11.3). Reservation (PART 22) is the
  accounting act and happens in Postgres under the ledger (ADR-0001); the engine
  never reserves and never mutates.
- Every output carries `policy_version` and `as_of`, and is recorded with the
  intent and risk decision that consumed it.
- The output is not cached as financial truth. A UI cache with short TTL and
  invalidation is permitted and is labelled derived (PART 25, 110).
- All arithmetic uses the exact types in DECISION_REGISTER D-007 with explicit
  rounding modes; haircuts round toward less buying power.
- Margin, leverage, balance-sheet credit, and treasury-fronted settlement are not
  inputs and have no fields; buying power never exceeds what held assets support.

### Explicitly not decided / deferred

- Initial collateral factors, haircuts, and availability delays. These are policy
  values under risk approval.
- Price sources and their precedence for valuation.
- Valuation in non-USD quote assets.

## Consequences

### Positive

- One answer to "can this account spend?", consistent between UI, risk, and the
  settlement compiler.
- Stablecoin, funding, and provider risk are first-class inputs rather than
  afterthoughts.

### Negative

- The input snapshot is wide; assembling it is more work than reading a balance.
- Any new restriction type needs to be threaded through the engine.

### Operational

- Buying-power decomposition is exposed to operators for support cases.
- Policy changes to haircuts or factors are audited and versioned.

## Alternatives considered

- Wallet balance sum: the failure mode this ADR exists to prevent.
- Margin or leverage models: excluded from V1 by PART 4.
- Balance-sheet credit or treasury fronting: excluded by PART 4.
- Redis-held buying power as authority: forbidden by PART 119.
- Computing in the frontend from balances: frontend state is never truth
  (PART 11, 110).

## Related

- ADR-0001, ADR-0013, ADR-0014, ADR-0018.
- Goal PART 3, 4, 11, 20, 21, 22, 23, 24, 25, 26, 27, 33, 79, 110, 119, 174.
- DECISION_REGISTER D-007.

## Evidence required before this ADR's guarantees can be claimed VERIFIED

- Property tests: `buying_power <= portfolio_value`; `available_now >= 0`;
  `available_now + reserved + pending` reconciles to the underlying balances under
  the policy; haircuts never increase buying power.
- Stablecoin status tests: each of the four states produces the policy-defined
  contribution; a depeg scenario reduces buying power without code change.
- Funding tests: pending and reversible funding are excluded from `available_now`
  until the availability time; a reversal after availability produces a typed
  negative-balance handling path, not a silent overdraft.
- Concurrency torture test (PART 23) proving reservation, not the engine, is the
  serialization point.
- Staleness test: stale price inputs produce `STALE_MARKET_DATA`.
- Chaos test with Redis unavailable: correctness of buying power unaffected.
- Integration test that every persisted intent references the `policy_version` and
  `as_of` of the buying-power snapshot it used.
