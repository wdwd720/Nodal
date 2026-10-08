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
//     underlying's spot from valuation asset_prices when one is fresh. A mark
//     is recorded in order and on time: never observed in the future, never
//     older than the policy's MaxMarkLag behind now, never older than the
//     instrument's latest recorded observation (the database keeps marks in
//     order too, DV005). Every run and the final settlement read only marks
//     recorded by their own cutoff -- a variation-margin run the marks
//     recorded by the run, funding those recorded by the interval's end, the
//     final settlement those recorded by expiry -- so nobody chooses an
//     expiry or a funding price with hindsight.
//   - Trading (fill.go, intents.go). OpenPosition and ClosePosition are paper
//     fills at a fresh mark plus disclosed slippage, applied by executeFill,
//     the one path a trade takes (unexported: nothing outside the package
//     applies a fill). A fill is accepted only on the Risk V2 decision that
//     allowed exactly it -- an ALLOW for its own account, intent and
//     instrument under the DERIVATIVES capability, made no later than the
//     fill, backing no other trade (DV006). Every intent-driven operation
//     follows the economic operation of ADVANCED_FINANCIAL_MODEL §6:
//     idempotent intent, settlement-compiler route, capability/mode
//     authorization, Risk V2 with the instrument, oracle (staleness and
//     mark/index divergence) and margin sections, capital reservation of
//     margin and premium, effects claimed and applied exactly once, postings,
//     evidence. A close is never refused by the risk kernel for a stale
//     oracle, but it is priced only at a mark fresh within the instrument's
//     oracle max age: without one it is refused (STALE_MARKET_DATA,
//     MARK_STALE), and the refusal is recorded and archived as evidence.
//   - Collections (collect.go). A customer's obligation -- a loss on a
//     reducing trade, variation margin, funding, a final settlement, an
//     assignment -- is met from the margin a reduction frees, then from the
//     customer's available funds (WALLET less every capital reservation and
//     withdrawal hold), reserved through capital.Reserve and consumed for
//     exactly what was taken, then from the margin the position keeps. What
//     none of them covers is the package's default path: a shortfall the
//     clearing house absorbs, recorded on the cash flow and the position and
//     reported by CLEARING reconciliation. A collection never takes funds
//     reserved or held for something else; the database refuses a cash flow
//     that took from a wallet without a consumed reservation covering it
//     (DV011).
//   - Runs (runs.go): variation margin and funding per (instrument, interval),
//     each claimed by a UNIQUE row so a replay or a concurrent run settles
//     once.
//   - Final settlement (settle.go, override.go). The settlement price is never
//     a caller's: it is the settlement rule, SettlementRuleStatement, applied
//     to the recorded marks. EXPIRY_MARK v1: of the instrument's marks
//     observed at or before expiry and recorded at or before expiry (an
//     option's: those carrying an index), take the one observed last (ties:
//     recorded last, then the greatest id); it must be fresh at expiry,
//     observed no more than the instrument's oracle max age before it; a
//     future settles at that mark, an option at that mark's index expressed
//     at the option's price scale and aligned to its tick grid, half-even.
//     A caller may only confirm the price (SettleRequest.Price): any other
//     refuses the settlement. Without a fresh mark nothing settles by the
//     rule; the contract waits for a dual-controlled override: a price one
//     OPERATOR proposes after expiry with a reason and evidence, on the tick
//     grid within the policy's SettlementOverrideMaxBPS of the rule's
//     reference price (the price the rule derives from the same mark, fresh
//     or not), approved by a second, distinct OPERATOR, whose approval
//     settles the contract at exactly that price. The database binds both
//     (DV008, DV009, DV010). Every settlement records its rule, its mark and
//     its override, and its evidence states the rule word for word.
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
//   - Terms (terms.go). Every computation reads one Spec per instrument: its
//     current terms. They are frozen while the instrument has an open
//     position (RequireTermsAmendable; the database refuses the change,
//     DV007), and every operation that trades, runs, settles or exercises
//     share-locks the terms row before it reads them, so an amendment and a
//     first open cannot pass each other.
//   - Reads (reads.go): keyset-paged listings of marks, positions, cash flows
//     and funding accruals, and the settlement record, so the package's
//     tables have one reader.
//   - Reconciliation kind CLEARING (clearing.go): the clearing house's books
//     against the cash-flow register.
//
// # Concurrency
//
// Every operation that can move value through the clearing house -- trades,
// fills, runs, settlement and its overrides, exercise, funding the clearing
// house -- and every reconciliation first takes the clearing house's
// transaction-scoped lock (serializeClearing), before any other lock, so no
// two of them can wait on each other in opposite orders and reconciliation
// compares the register and the books at one quiescent moment. The
// instrument's terms row is share-locked next, then positions are row-locked
// in one global order (opened_at, id); a collection's capital reservation
// takes the account's reservation-totals lock last. Exactly-once does not
// depend on the locks: it is the UNIQUE claim rows and the effect register.
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
//   - Apply a trade the risk kernel did not allow, for that account, intent
//     and instrument (DV006).
//   - Settle at a price a caller chose, at a mark recorded after expiry, or at
//     an override one operator alone decided (DV008, DV010).
//   - Record a mark out of order or later than the policy's lag, or let a
//     run read a mark recorded after its cutoff.
//   - Price a close, a run or a settlement at a stale mark, or let a stale or
//     missing mark price new risk. (The risk kernel never refuses closing for
//     a stale oracle; a close without a fresh price is refused and recorded,
//     not filled at an old one.)
//   - Take an obligation from funds reserved or held for something else
//     (DV011), or post a negative balance: what cannot be collected is a
//     recorded shortfall.
//   - Value open positions under terms other than the ones they were opened
//     under (DV007).
//   - Round anywhere it does not say so, or use floating point.
//   - Post a model value. Analytics are reference values for display; no
//     settlement, exercise, margin posting or fill is ever priced from them.
//   - Retry an effect whose outcome is UNKNOWN, or apply an effect on behalf
//     of an agent.
//   - Hold state at package level or read the wall clock (clock.Clock is
//     injected).
package derivatives
