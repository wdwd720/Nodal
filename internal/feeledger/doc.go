// Package feeledger is the persistence and accounting of the transparent
// spread and fee engine and of the sandbox performance fee (ADR-0020;
// ADVANCED_FINANCIAL_MODEL.md §2 FEES and §5, migration 00746; capabilities
// SPREAD_ENGINE and PERFORMANCE_FEES; modes PAPER and SANDBOX). internal/fees
// is the pure engine; this package stores what it says, books it, and makes
// every charged unit discoverable afterwards. Every unit it moves is SIMULATED.
//
// # Fee schedules
//
// A fee schedule (fees.Schedule: explicit spread, fixed fee, percentage fee,
// venue pass-through, network fee, slippage reserve, subscription discount,
// volume tiers, strategy fee, minimum and caps) is published under a key in
// consecutive versions, each immutable, stored in its canonical form with its
// hash, effective from an instant that is never in the past and always after
// the previous version's. EffectiveSchedule answers which version priced an
// instant.
//
// # Quote disclosures
//
// RecordQuote prices an order under the schedule effective at an instant,
// verifies the quote (fees.VerifyQuote) and its disclosure
// (fees.VerifyDisclosure), and stores the disclosure: reference price,
// execution price, spread, every explicit fee, the network estimate, the
// effective all-in price and the totals, with the canonical bytes and their
// hash. VerifyQuoteRecord re-proves a stored disclosure from the record alone.
//
// # Booking
//
// A charge books every component on its own pair of accounts
// (ChargeEntries): the spread on the payer's FEES_SPREAD against the
// platform's SPREAD_REVENUE, the platform fee and the strategy fee (each its
// own pair) on FEES_PLATFORM against PLATFORM_FEE_REVENUE, a venue
// pass-through on FEES_VENUE and a network cost on FEES_NETWORK (pass-throughs
// are external and not platform revenue). RecordEmbedded registers the charges
// a subsystem posted inside its own journal (a trade's SIM_TRADE, a
// liquidation's LIQUIDATION) and applied by its own effect: the journal is
// verified to carry exactly them. That is how every fee a deployment charges
// is booked. The POSTED path -- the fee ledger posting a charge on its own,
// under its own FEES effect, after reserving the payer's wallet -- exists
// unexported (book) until a fee charged on its own needs it. Reverse
// compensates a booking by a COMPENSATION journal of exactly its amounts.
// fee_bookings and fee_quote_disclosures are the audit record: SpreadAudit
// lists every charged spread with its disclosure, schedule and journal.
//
// # How other subsystems call the fee ledger
//
// Every fee and spread a subsystem charges goes through here, in the
// subsystem's own transaction, exactly once per charge:
//
//  1. The schedule. A subsystem whose charges are fixed by its own frozen
//     configuration (an internal book's maker and taker schedules, the margin
//     policy's liquidation fee) calls EnsureSchedule: that exact schedule is
//     the version in force under its key, or nothing is charged. A subsystem
//     whose prices an operator manages (the paper venue) charges whatever
//     version is in force at the fill instant (EffectiveSchedule).
//  2. The disclosure. RecordQuote, once per payer, under a deterministic
//     idempotency key, bound to the payer.
//  3. The charge. Inside the subsystem's own journal (ChargeEntries, the
//     disclosure's charges, every component on its own pair) applied by its
//     own effect, then RecordEmbedded, which proves the journal carries
//     exactly the disclosure.
//
// A charge of nothing is not booked (ReasonNothingToBook). A replay returns
// the stored disclosure and booking. A charge of a risk-reducing operation (a
// liquidation's fee) says so (RiskReducing): a kill switch never stops risk
// reduction, so it does not stop that charge either.
//
// # Performance fees (sandbox accounting, never charged live)
//
// A performance fee period belongs to an (account, strategy) pair and chains
// from the previous crystallized period (its net NAV and high-water mark).
// Flows (deposits and withdrawals) and valuations are recorded in time order;
// each valuation runs the period through fees.PerformancePeriod and books the
// change in the running accrual (PERFORMANCE_FEE_ACCRUAL; a fall is a
// compensating REVERSAL line, never an edit). Crystallization at the period
// end posts the true-up and the charge (PERFORMANCE_FEE_CRYSTALLIZATION),
// attributes the fee to strategies, and stores the deterministic statement
// with its hash. A crystallization is reversed by a COMPENSATION of exactly
// the fee; an open period can be cancelled, reversing its accrual.
//
// # What this package must never do
//
//   - Charge anything a stored, verified disclosure does not itemize, charge a
//     disclosure twice, or book two components to one account pair.
//   - Change a published schedule, publish one that takes effect in the past,
//     or let an agent publish one.
//   - Book a fee whose disclosure no longer verifies, whose quote expired, or
//     whose payer is not the disclosure's, or register a journal that does
//     not carry exactly the disclosed charges.
//   - Charge a performance fee on a deposit, below the high-water mark or the
//     hurdle, edit an accrual or statement line instead of appending a
//     compensating one, or let a period chain break.
//   - Move value that is not SIMULATED, post without a claimed effect, apply an
//     effect without its journal, or retry an effect whose outcome is unknown.
//   - Use floating point, read the wall clock implicitly, or hold mutable
//     state at package level.
package feeledger
