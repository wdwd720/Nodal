// Package derivatives is the derivatives subsystem of the advanced financial
// capabilities (goal §7; ADVANCED_FINANCIAL_MODEL §5; ADR-0020): dated
// futures, perpetual futures and options, in PAPER and SANDBOX mode only,
// cleared through one CLEARING_HOUSE facility.
//
// # Responsibilities
//
//   - Novation. The counterparty of every customer position is the clearing
//     house. Every cash flow -- option premium, realized PnL on a reducing
//     trade, variation margin, perpetual funding, final settlement, exercise
//     and assignment -- moves customer <-> clearing house through the posting
//     templates documented in postings.go, each balanced per party and posted
//     with a simulated-only journal kind. Positions are contracts in
//     derivative_positions, never ledger assets; only cash flows and initial
//     margin (WALLET <-> DERIVATIVES_MARGIN) reach the ledger.
//   - Exact accounting (pnl.go). Every value is an integer number of base
//     units of the settlement asset: an instrument is admitted only when one
//     tick of one contract is a whole number of them, and every trade price,
//     mark, strike and settlement price is on the tick grid. Entry price is an
//     exact cost; the share of a partial close is rounded half-even once and
//     the remainder stays, so realized PnL telescopes exactly. Funding is
//     rounded once per contract per interval, so a balanced book's funding
//     conserves exactly.
//   - Marks (marks.go): mark and index per instrument; the index is the
//     underlying's spot from valuation asset_prices when one is fresh.
//   - Trading (fill.go, intents.go). OpenPosition and ClosePosition are paper
//     fills at the mark plus disclosed slippage; ApplyFill is the one path a
//     trade takes, so a fill matched in an internal order book applies the
//     same way. Every intent-driven operation follows the economic operation
//     of ADVANCED_FINANCIAL_MODEL §6: idempotent intent, settlement-compiler
//     route, capability/mode authorization, Risk V2 with the instrument,
//     oracle (staleness and mark/index divergence) and margin sections,
//     capital reservation of margin and premium, effects claimed and applied
//     exactly once, postings, evidence.
//   - Runs (runs.go, settle.go): variation margin and funding per (instrument,
//     interval) and final settlement per instrument, each claimed by a UNIQUE
//     row so a replay or a concurrent run settles once.
//   - Options (options.go, exercise.go, analytics.go): exact intrinsic value
//     (quant.IntrinsicCall/Put), holder exercise of American options, automatic
//     exercise at expiry, deterministic FIFO assignment, and Black–Scholes
//     reference analytics (price, implied volatility, Greeks) labelled as
//     such.
//   - Risk (margin.go, reader.go): the margin model -- linear rates by risk
//     class, the perpetual's expected-funding buffer, the expiry gap, a long
//     option's premium-only risk, a written option's scenario-grid worst loss
//     plus a minimum -- per-account RiskContribution, and the
//     buyingpower.MarginReader of Buying Power V2.
//   - Reconciliation kind CLEARING (clearing.go): the clearing house's books
//     against the cash-flow register.
//
// # Concurrency
//
// Every operation that can move value through the clearing house -- trades,
// fills, runs, settlement, exercise, funding the clearing house -- and every
// reconciliation first takes the clearing house's transaction-scoped lock
// (serializeClearing), before any other lock, so no two of them can wait on
// each other in opposite orders and reconciliation compares the register and
// the books at one quiescent moment. Positions are then row-locked in one
// global order (opened_at, id). Exactly-once does not depend on the lock: it
// is the UNIQUE claim rows and the effect register.
//
// # What this package must never do
//
//   - Move real value. Every asset it settles in is SIMULATED (Spec refuses
//     anything else, the database refuses it again), and LIVE is not a mode
//     it implements.
//   - Post a cash flow that does not pass through the clearing house, or one
//     that is missing from the cash-flow register (the database refuses the
//     register row that disagrees with its journal, DV004).
//   - Change a position without a fill explaining it (DV001), edit history,
//     or move a position's status without a transition row.
//   - Round anywhere it does not say so, or use floating point.
//   - Post a model value. Analytics are reference values for display; no
//     settlement, exercise, margin posting or fill is ever priced from them.
//   - Let a stale or missing mark price new risk; it never refuses closing
//     because a mark is stale.
//   - Retry an effect whose outcome is UNKNOWN, or apply an effect on behalf
//     of an agent.
//   - Hold state at package level or read the wall clock (clock.Clock is
//     injected).
package derivatives
