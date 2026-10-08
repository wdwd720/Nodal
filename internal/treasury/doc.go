// Package treasury is the principal / treasury book of the advanced financial
// capabilities (ADR-0020; ADVANCED_FINANCIAL_MODEL.md §2 TREASURY and §5,
// migration 00745; capability PRINCIPAL_TRADING; modes PAPER and SANDBOX).
// Every unit it moves is SIMULATED.
//
// # The house book
//
// A treasury book is a house book (econ.Registry.CreateHouseAccount, ledger
// owner type HOUSE) registered with a class -- PRINCIPAL for liquidity
// provision, market making and hedging, TREASURY for inventory rebalancing
// and treasury conversion -- a valuation asset every PnL figure is stated in,
// and a signing policy. A house book is a completely separate owner: its value
// lives only in HOUSE_INVENTORY, HOUSE_CAPITAL, HOUSE_TRADING_* and HOUSE_FEES
// accounts of its own house id, so customer and house assets never share an
// account, and the house can only ever spend its own HOUSE_INVENTORY.
//
// # Activities
//
//   - Liquidity provision and market making: house orders on the internal
//     book through orders.Service.SubmitHouseOrder, one-sided or as a two-sided
//     quote (both legs post-only GTC limits).
//   - Hedging and inventory rebalancing: house orders on the internal book, or
//     immediate fills on the house paper venue.
//   - Treasury conversion: an immediate fill on the house paper venue between
//     the valuation asset and another sandbox asset, at a fresh reference mark,
//     priced through a fees.Schedule built from the policy, with the spread and
//     every fee disclosed (fees.Disclosure, verified before posting) and booked
//     to HOUSE_FEES.
//
// House-versus-customer crossing stays refused by the order book's default
// rule. SetCrossingRule is the only way the treasury lets a book allow it, and
// it records an operator, a reason and a customer-facing disclosure that says
// the activity is simulated, with archived evidence and an audit event.
//
// # Controls before any house order or fill
//
// Every command runs under the treasury book's row lock, so two commands of
// one house book are serialized and every limit is evaluated against
// everything the earlier command committed. In order:
//
//  1. Authority: the book's signing policy (no keys; key custody NONE_SANDBOX)
//     names the one house service actor that may act for it and whether
//     operators may trade; administrative actions are an operator's alone;
//     customers, agents and anonymous system actors never act for a house book
//     (and the database refuses such a row, TY002).
//  2. Capability: econ.Authorizer for PRINCIPAL_TRADING (and INTERNAL_MATCHING
//     for book orders). A CAPABILITY_DISABLE switch scoped to
//     PRINCIPAL_TRADING, or the global kill, stops every new house order and
//     fill. It never cancels a resting order and never trades to reduce risk:
//     cancels stay available.
//  3. Treasury limits (the versioned, hashed risk policy): the daily loss limit
//     (realized PnL of the UTC day plus every open loss, at fresh marks), the
//     maximum inventory per asset (holdings plus every working buy plus this
//     one) and the per-instrument exposure (the mark value of the base held
//     plus the working buy notional on the instrument plus this one). They bind
//     risk-increasing activity -- a buy of a risky asset -- only; a sell
//     reduces risk and is never blocked by a loss.
//  4. Risk Kernel V2: the limits enter as circuit breakers beside the account,
//     identity, instrument, balance and oracle sections and the kill switches;
//     the decision, its canonical input and the limit evaluation are persisted
//     (treasury_risk_decisions) and a rejection is recorded as a REJECTED
//     action with stable reason codes.
//
// # Position register and PnL
//
// Every fill of the house book -- internal-book trades (ingested exactly once
// by Sync from book_trades), paper fills and fundings -- is a position event
// in a gap-free chain per house book. Realized PnL is at average cost, the
// released cost rounded up once (never overstating a gain); paper spread and
// fees are expensed when paid. Unrealized PnL is quantity at a fresh mark,
// rounded down, less the remaining cost. Report and MarkToMarket state both.
//
// # What this package must never do
//
//   - Move value that is not SIMULATED, post to anything but the house book's
//     own HOUSE accounts, or make customer value available to a house
//     strategy.
//   - Let a customer, an agent or an unnamed actor act for a house book, or
//     hold or read a signing key.
//   - Place a house order, or make a paper fill, without the signing policy,
//     the capability, the treasury limits and Risk Kernel V2 all allowing it
//     under the book's lock; let a breached limit allow new risk; or let a kill
//     switch cancel, liquidate or otherwise trade.
//   - Loosen the house-versus-customer crossing rule without an operator and a
//     recorded, simulated-labelled disclosure.
//   - Post without a claimed effect, apply an effect without its journal, or
//     retry an effect whose outcome is unknown.
//   - Round a PnL figure in the house's favour, use floating point, read the
//     wall clock implicitly, or hold mutable state at package level.
package treasury
