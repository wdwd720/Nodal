// Package fees prices what the platform charges and states it so that every
// charged unit can be found in the audit record (PART 126). It has three
// parts, all pure computation: no I/O, no clock reads, no global state.
//
// # Platform fee policy (Policy)
//
// The original explicit platform fee: basis points of an input quantity,
// bounded by optional minimum and maximum amounts, in a named fee asset,
// versioned and hashed. internal/settlement prices fills with it. The
// default policy (Zero) charges nothing.
//
// # Transparent spread and fee schedule (Schedule, Quote)
//
// A Schedule is a versioned, validated, hashable price list for orders
// quoted in one asset: an explicit spread on the reference price (a BUY
// executes at ref × (1 + s), a SELL at ref × (1 − s)), a fixed fee per
// order, a percentage fee, a venue-fee pass-through, a strategy fee for
// strategy-originated orders, a network cost estimate (possibly in another
// asset), a slippage reserve that is HELD and never charged, a subscription
// discount that reduces the percentage fee only, volume tiers that override
// the percentage fee and/or the spread, a minimum fee, a basis-point cap and
// an absolute cap on the explicit fees, and a rounding mode per component.
//
// Schedule.Quote prices one order. Every basis-point component applies to
// the exact reference notional (quantity × reference price), so no charge is
// levied on another charge. Each amount is formed exactly and rounded once,
// in the mode its schedule names. Totals are assembled from the itemized
// components only, and the identities hold exactly:
//
//	BUY:  total debit  = notional at reference + spread + explicit fees (+ network cost in the quote asset)
//	SELL: total credit = notional at reference − spread − explicit fees (− network cost in the quote asset)
//
// A network cost in another asset is disclosed in that asset and never
// converted. The minimum fee raises the explicit fees with a separate
// MINIMUM_FEE top-up; a cap always wins over the minimum, and reduces the
// platform's own fees (percentage, strategy, fixed) before the venue
// pass-through.
//
// Rounding defaults (DefaultRounding) are in the CUSTOMER's favour: amounts
// the customer pays (a BUY's notional, the spread, every basis-point fee, the
// fee cap) round down, the amount a customer receives (a SELL's notional)
// rounds up, and the reserve rounds up so the hold covers the whole
// tolerance. A mode that can round against the customer is refused unless
// the schedule sets AllowPlatformFavourable, and the disclosure states every
// mode used and flags each one that can favour the platform. The display-only
// effective price rounds up for a BUY and down for a SELL.
//
// Quote.Disclosure is the canonical, hashable audit record (schedule version
// and hash, the complete input, every component with its rate and rounding
// mode, the totals). VerifyQuote is what an auditor runs: it re-derives every
// component from the schedule and input, proves conservation, bounds and
// rounding, and requires the quote to be exactly the one the engine computes.
// VerifyDisclosure does the same from the record alone. Quote.RevenueSplit
// tells a ledger how to book spread revenue, explicit fee revenue, strategy
// fee revenue and venue and network pass-through to separate accounts.
//
// # Performance fee calculation (PerformancePeriod, PerformanceStatement)
//
// A sandbox calculation and accounting statement for a managed account's
// performance fee. It never charges: performance fees are excluded from live
// operation (REQUIREMENTS_TRACEABILITY R-004-5), nothing here posts or moves
// money, and every statement records Mode SANDBOX_CALCULATION. All amounts
// are money.USD and every NAV is gross of performance fees. The rules, each
// rounding in the investor's favour:
//
//   - Flows adjust the high-water mark so they never create or destroy
//     performance: a deposit raises it by the deposit; a withdrawal scales it
//     by NAV after / NAV before (the fraction of NAV that remains), rounded
//     up. The previous mark must be at least the opening NAV.
//   - Gross PnL = closing NAV − opening NAV − net flows.
//   - Hurdle threshold = HWM_adjusted × (1 + annual hurdle × elapsed / 365
//     days), elapsed measured exactly from the period start (Actual/365
//     Fixed), rounded up. Flows are not time-weighted: the flow-adjusted mark
//     earns the hurdle for the whole elapsed period.
//   - Fee base = min(max(0, closing − max(HWM_adjusted, threshold)),
//     max(0, gross PnL)), so the fee never exceeds the rate applied to what
//     the period actually earned.
//   - Fee = fee base × rate, rounded down. Net NAV = closing − fee. On
//     crystallization (at period end), new HWM = max(HWM_adjusted, net NAV).
//   - Accrue values the period at a snapshot as if it ended then and appends
//     one line for the change in the running accrual; a fall in NAV appends a
//     compensating REVERSAL line. Lines are never edited.
//   - When several strategies contribute gross PnL, the fee is attributed pro
//     rata to the positive contributors, rounded down to the cent, and the
//     remaining cents go to the largest remainders (ties by strategy
//     reference), so the attribution sums exactly to the fee.
//
// PerformanceStatement is deterministic and has a canonical JSON form and a
// sha256 hash. VerifyPerformanceStatement replays a statement from the agreed
// terms and the valuations it records and requires it byte for byte, which
// proves every accrual, reversal, fee and attribution line follows from them.
//
// # What this package must never do
//
//   - Hide a spread. The spread is its own disclosed line with its rate,
//     amount and the execution price it produces; the reference price, the
//     execution price and the effective all-in price are all disclosed.
//   - Charge an undisclosed component. A total is only ever the sum of
//     itemized components, and VerifyQuote rejects any quote whose totals,
//     lines or record differ from what its schedule and input produce.
//   - Let rounding favour the platform unless the schedule says so
//     explicitly (AllowPlatformFavourable) and the disclosure shows it.
//   - Charge a slippage reserve, convert a network cost between assets, or
//     levy one charge on another.
//   - Charge, post or move a performance fee, charge a performance fee on a
//     deposit, let a withdrawal reset the high-water mark, charge below the
//     flow-adjusted mark or hurdle, or edit a statement line instead of
//     appending a compensating one.
//   - Use floating point (a test scans the source), read a clock, hold global
//     mutable state, or let an AGENT actor change a policy or schedule.
package fees
