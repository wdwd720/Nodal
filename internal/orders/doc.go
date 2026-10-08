// Package orders is the internal order book and the paper venue of the
// advanced financial capabilities (ADR-0020; ADVANCED_FINANCIAL_MODEL.md §2
// ORDERS, migration 00742; capability INTERNAL_MATCHING for the book and
// SPREAD_ENGINE for disclosed spreads; modes PAPER and SANDBOX). Every unit it
// moves is SIMULATED, and every surface labels the book and the venue as
// simulated.
//
// # The persistent book
//
// One book per instrument (SPOT_PAIR, EQUITY, EVENT_OUTCOME), driven by the
// pure engine of internal/matching. Prices and quantities are converted
// exactly between the instrument's terms and the engine's ticks and lots, and
// anything inexact is refused with a stable reason (BookTerms). Every command
// of a book takes the book's row lock (book_states FOR NO KEY UPDATE) in its own
// transaction, is appended to the gap-free, hash-chained journal
// (book_commands, one sequence per book), applied to the engine, and its
// events, trades and order states are persisted; a snapshot is taken every
// SnapshotInterval commands. The in-memory engine is a cache of the journal:
// before each command it is compared with the cursor the row lock returned,
// and it is rebuilt from the latest snapshot and the journal's tail whenever it
// is missing or disagrees, the result being required to hash to the hash the
// journal recorded. VerifyJournal proves from the database alone that the
// journal replays to byte-identical events and state.
//
// # Economics of every fill
//
// A customer order is the execution of one economic intent (ACQUIRE_EXPOSURE
// for a buy, REDUCE_EXPOSURE for a sell; TAKE_LIQUIDITY or PROVIDE_LIQUIDITY
// in its terms). Its stages are Accept (intent, settlement compiler), Approve
// (econ.Authorizer, Risk Kernel V2), Reserve (capital.Reserve: quantity x
// limit price plus the most its fees can be for a buy, the base quantity for
// a sell), Execute (book lock, house crossing policy, effect, engine, trades)
// and Complete; Service.Submit runs them atomically and Pipeline one
// transaction each. Every trade is a SIM_TRADE posting between maker and
// taker per §5, each party balanced on its own books, with the explicit maker
// and taker fees of the book's fees.Schedule booked to PLATFORM_FEE_REVENUE and
// no spread (the price is the counterparty's); it consumes both reservations,
// and every cancellation releases what is left.
//
// Every fee is booked through the fee ledger (internal/feeledger) in the
// fill's own transaction (feebooking.go): the book's maker and taker schedules
// are published there when it opens and must still be the versions in force
// whenever an order is approved, executed or filled; each paying party's fee
// is disclosed to it (RecordQuote), posted inside the trade's journal as
// exactly the disclosure's entries, and registered against that journal and
// the trade's effect (RecordEmbedded). An order on a fee-charging book needs
// SPREAD_ENGINE as well as INTERNAL_MATCHING. House books (SubmitHouseOrder)
// reserve their own HOUSE_INVENTORY and by default never trade with a
// customer: the crossing policy decides exactly with a dry run on a copy of
// the book, and only a versioned crossing rule can allow it.
//
// # The paper venue
//
// PaperVenue fills a market order at once against a fresh reference mark
// through the instrument's fees.Schedule -- the fee-ledger version in force
// under its key at the fill instant -- with the spread and every fee
// disclosed, verified (fees.VerifyQuote, fees.VerifyDisclosure) before it is
// posted, booked to its own revenue account, recorded and booked in the fee
// ledger, and stored with the fill. It implements IntentExecutor for callers
// that own their intent's lifecycle.
//
// # What this package must never do
//
//   - Move value that is not SIMULATED, or label a book or a fill as anything
//     but simulated.
//   - Trust the in-memory engine over the journal, apply a command without the
//     book's row lock, or let the journal, the book cursor and the snapshots
//     disagree (the database refuses it too: OB001, OB002, OB005).
//   - Overfill an order, let an order trade with its own owner, or let a house
//     book trade with a customer without a versioned crossing rule (refused in
//     SQL as well as here).
//   - Round a price, a quantity or a notional, or charge anything that is not
//     an itemized, disclosed component of a schedule; the internal book
//     charges no spread at all.
//   - Charge a fee or a spread the fee ledger has not disclosed to its payer
//     and booked against the journal that charges it, or under a schedule
//     that is not the fee-ledger version in force (the database refuses an
//     unbooked fee too: FW001).
//   - Post without a claimed effect, apply an effect without its journal, or
//     retry an effect whose outcome is unknown.
//   - Let an agent apply an effect, write a transition row, open a book, set
//     a crossing rule or place a house order.
//   - Hold connections, configuration or mutable state at package level, read
//     the wall clock implicitly, or use floating point.
//   - Let a finished order whose intent cannot be completed stop
//     CompleteTerminal from completing the others, or take it again before
//     its backoff has passed (upkeep_failures, 00760).
package orders
