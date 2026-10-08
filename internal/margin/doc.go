// Package margin is the deterministic margin, leverage, borrowing and
// liquidation engine of the advanced capabilities, for PAPER and SANDBOX
// positions only (ADR-0020; docs/architecture/ADVANCED_FINANCIAL_MODEL.md;
// migration 00740; subsystem econ.SubsystemMargin; capabilities
// gates.Margin and gates.Leverage).
//
// # Responsibilities
//
// The pure engine (no I/O; same input, same output, byte for byte):
//
//   - Evaluate computes an account's canonical equity, exactly, in the
//     numeraire's base units:
//
//     equity = eligible collateral value + realized pnl + unrealized pnl
//     − borrow liabilities − accrued fees − accrued interest
//
//     together with gross and net exposure, initial margin (margin used),
//     maintenance, the warning threshold, the liquidation buffer, capacity
//     and margin available (never above capacity), and effective leverage
//     (gross exposure / equity, in BPS, rounded up).
//
//   - ComputeHaircut composes the collateral haircut from volatility class,
//     liquidity, oracle freshness (a stale or missing mark is a 100%
//     haircut), chain/venue tier, concentration, settlement delay and
//     cross-chain delay: additive, capped at 10000, monotone in every factor.
//
//   - Policy is the versioned, hashed rule book: margin rates per risk class,
//     instrument risk weights, leverage caps per mode (CASH, ISOLATED,
//     CROSS), concentration limits, the warning buffer, the liquidation
//     slippage cap and fee, the partial-liquidation target and the step
//     budget.
//
//   - PlanLiquidation decides one deterministic liquidation step: wait for a
//     price, wait for the venue, sell the smallest set of holdings that
//     restores the target (riskiest first), sell everything, apply numeraire
//     collateral, declare bankruptcy, or hand the case to a person.
//
//   - RunStress runs gap, oracle, venue, fill, spread, bridge and depeg
//     scenarios and simulates the liquidation each would trigger.
//
//   - DerivativeExposure is the contract with the derivatives lane, which
//     this package never imports.
//
// The service runs margin accounts against the database through the shared
// kernel (econ intents, effects, authorizer, facilities, evidence), Risk
// Kernel V2, capital reservations, the price store and the ledger, posting
// only the simulated-only templates of the model's §5: MARGIN_COLLATERAL_MOVE,
// MARGIN_BORROW, MARGIN_REPAY, MARGIN_INTEREST, SIM_TRADE, LIQUIDATION,
// DEFICIT_COVERAGE and FACILITY_FUNDING.
//
// # What this package must never do
//
//   - Move real value. Every posting uses a simulated-only kind; the
//     numeraire, the collateral, the lending pool and the insurance fund are
//     SIMULATED, and the database refuses anything else (SM001, SM002, MG006).
//   - Run in LIVE or DEVNET: a margin account is PAPER or SANDBOX, in Go and
//     in the CHECK on margin_accounts.mode.
//   - Use floating point, read the wall clock implicitly, or hold mutable
//     state at package level.
//   - Let new leverage through on a stale or missing mark, let a release
//     leave equity below maintenance, or let a CASH account borrow.
//   - Liquidate on a stale or missing mark, sell more than a holding, sell
//     beyond the slippage cap, or exceed a case's step budget: a case that
//     cannot progress goes to MANUAL_REVIEW and never loops.
//   - Apply an economic effect twice, or retry one whose outcome is UNKNOWN.
//   - Let an agent apply an effect, open an account or release collateral.
//   - Stop risk reduction (repayment, posting collateral, liquidation,
//     deficit coverage) because a kill switch or an inactive gate stops new
//     risk.
//   - Change a status without a transition row describing that exact change.
package margin
