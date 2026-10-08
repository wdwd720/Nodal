// Package margin is the deterministic margin, leverage, borrowing and
// liquidation engine of the advanced capabilities, for PAPER and SANDBOX
// positions only (ADR-0020; docs/architecture/ADVANCED_FINANCIAL_MODEL.md;
// migrations 00740 and 00754; subsystem econ.SubsystemMargin; capabilities
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
//     slippage cap and fee, the partial-liquidation target and the budget of
//     trading steps a case gets per window.
//
//   - PlanLiquidation decides one deterministic liquidation step: wait for a
//     price, wait for the venue, sell the smallest set of holdings that
//     restores the target (riskiest first), sell everything, apply numeraire
//     collateral, sweep the customer's free wallet balance into the loans,
//     declare bankruptcy, or hand the case to a person. The budget bounds
//     sales only: applying, sweeping and bankruptcy are terminal accounting
//     and are decided whatever the budget.
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
// MARGIN_BORROW, MARGIN_REPAY, MARGIN_INTEREST, SIM_TRADE, LIQUIDATION (the
// fee as the explicit-fee pair FEES_PLATFORM / PLATFORM_FEE_RECEIVABLE /
// PLATFORM_FEE_REVENUE), DEFICIT_COVERAGE and FACILITY_FUNDING, and the
// repayment of a covered deficit (MARGIN_REPAY: Dr DEFICIT / Cr WALLET, Dr
// INSURANCE_FUND_CASH / Cr DEFICIT_RECEIVABLE). Every intent-driven
// operation -- borrowing, repaying, buying on margin, posting and releasing
// collateral, repaying a deficit -- is an econ intent, routed through the
// settlement compiler and executed only when the route MayExecuteNow.
//
// A case in MANUAL_REVIEW leaves it only through a resolution one operator
// proposes and a second, distinct operator approves (resume with a fresh
// budget window, or declare bankrupt once nothing is left); no interest runs
// while a person holds the case. Covering a deficit RESTRICTS the customer
// account that owes it (accounts.Repository.Transition), the status every
// new-risk, withdrawal and transfer path refuses, while RepayDeficit pays the
// fund back. ProcessLiquidationQueue commits each account's step in its own
// transaction. Every operation that posts to a facility's books locks the
// facility rows first, lending pool before insurance fund.
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
//     beyond the slippage cap, or sell beyond a case's budget of trading
//     steps: a case that cannot progress goes to MANUAL_REVIEW and never
//     loops -- and never stays there for want of a way out, nor goes there
//     when only applying, sweeping or bankruptcy is left.
//   - Move a case out of MANUAL_REVIEW without an approved resolution of a
//     second, distinct operator, or declare a bankruptcy while anything is
//     left to sell, apply or sweep (MG009, MG010).
//   - Charge interest while a case is in MANUAL_REVIEW or the account is
//     BANKRUPT (MG007), or declare a bankruptcy before the customer's free
//     numeraire balance has repaid what it can.
//   - Take wallet value except through a capital reservation, or loosen a
//     customer account's status: margin restricts an ACTIVE account that
//     owes a covered deficit and never lifts a restriction or a freeze.
//   - Execute an intent the settlement compiler permits only after a
//     confirmation, or post to a facility's books before locking its row
//     (lending pool first).
//   - Apply an economic effect twice, retry one whose outcome is UNKNOWN, or
//     apply one with any journal but the one its key posted (MG008).
//   - Let an agent apply an effect, open an account, or post or release
//     collateral.
//   - Stop risk reduction (repayment, posting collateral, liquidation,
//     deficit coverage) because a kill switch or an inactive gate stops new
//     risk.
//   - Change a status without a transition row describing that exact change.
package margin
