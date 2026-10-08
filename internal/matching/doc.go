// Package matching is a deterministic, in-memory central limit order book
// (CLOB) engine. A Book holds the orders of one instrument and is driven by
// three commands -- NewOrder, Cancel and Replace -- each of which produces an
// ordered list of sequenced events. It is the matching core behind the
// internal simulated spot book, the paper/sandbox derivatives books and the
// binary prediction-market books; it is pure state-machine code, and
// persistence, risk checks and ledger posting happen elsewhere, driven by
// the events it emits.
//
// # Responsibility
//
//   - Price-time priority matching: the best price first, and first come
//     first served within a price, through an intrusive FIFO queue per price
//     level (O(1) cancel through an OrderID index, O(log L) level lookup).
//     A trade always executes at the maker's resting price.
//   - LIMIT and MARKET orders; GTC, IOC and FOK; post-only; reduce-only caps;
//     self-trade prevention; Cancel; Replace with queue-priority rules.
//   - Gap-free sequence numbers on every command (CommandSeq), every event
//     (Seq) and every trade (TradeSeq).
//   - Event sourcing: the Sequencer journals every command it applies in the
//     total order it assigned; Replay of that journal reproduces identical
//     events and state; Snapshot encodes the full state canonically,
//     StateHash is its SHA-256, and Restore followed by the rest of the
//     journal equals a full replay.
//   - Queries: best bid and ask, aggregated depth, any order's state, and an
//     owner's open orders.
//
// # Units and bounds
//
// Prices are int64 ticks and quantities int64 lots, both strictly positive
// (a MARKET order's optional protection price may be 0, meaning none). The
// caller converts exact decimal instrument terms to ticks and lots and back.
// MaxPriceTicks, MaxQtyLots, MaxNotionalTickLots, MaxRestingLotsPerSide and
// MaxIDBytes bound every input so that no int64 arithmetic can overflow, in
// the engine or in a caller handling what it reports; see their comment.
//
// # Determinism
//
// A Book's events and state are a pure function of the sequence of commands
// applied to it. Timestamps are supplied by the caller on each command and
// copied onto its events; they never influence matching. Nothing is derived
// from map iteration order: maps are used only for lookup, and every output
// that enumerates (snapshots, open orders, depth) sorts or walks ordered
// structures. Replaying the same commands therefore yields byte-identical
// events, snapshots and state hashes.
//
// # Semantics where venue conventions differ
//
//   - OrderID is the order's engine-wide identity. The first NewOrder that
//     carries a well-formed OrderID consumes it whatever the outcome --
//     accepted, or rejected for any reason -- and any later NewOrder with it
//     is rejected with DUPLICATE_ORDER_ID, so a retried submission can never
//     create a second order. Consumed OrderIDs are retained for the life of
//     the book and are part of its snapshot.
//   - ClientOrderID is optional. A non-empty one must be unique per OwnerID
//     among accepted orders: it is consumed only when the order is accepted
//     (a rejected order does not consume it, so the client can correct and
//     resubmit with a new OrderID) and retained for the life of the book,
//     including after the order is filled or cancelled; it is part of the
//     snapshot. The book's memory therefore grows with the number of orders
//     it has ever seen; a book is meant to be rotated with its instrument's
//     session or lifetime.
//   - A MARKET order is rejected with MARKET_NO_LIQUIDITY when, on entry,
//     there is no opposite order within its protection price (if any): a
//     market order with nothing to execute against is refused, not
//     acknowledged. Otherwise it sweeps levels in priority order up to its
//     protection price and the unfilled remainder is cancelled
//     (MARKET_REMAINDER). It never rests, so its TIF must be IOC or FOK.
//   - FOK is checked before any side effect: the fillable quantity is counted
//     by walking the opposite side exactly as matching would (price-time
//     order, within the limit or protection price, skipping the resting
//     orders that CANCEL_MAKER would remove and stopping at the first
//     self-trade under CANCEL_TAKER or CANCEL_BOTH). If it covers the order,
//     the order executes completely; otherwise it is accepted and cancelled
//     at once with zero fills (CANCELLED, reason FOK) and the book is
//     untouched. Like IOC, FOK is a time-in-force outcome rather than a
//     rejection.
//   - IOC fills what it can and cancels the remainder (IOC_REMAINDER), with
//     zero fills when nothing crosses.
//   - Post-only orders, and Replaces of them, are rejected
//     (POST_ONLY_WOULD_CROSS) if they would match on entry, including when
//     the only crossing order is the owner's own.
//   - Reduce-only: the caller supplies ReduceOnlyLimitLots from its position.
//     On entry, the quantity above it is cancelled (REDUCE_ONLY_CAP, a
//     partial cancellation that leaves the order open), so the order can
//     never fill more than the limit and never displays quantity it could not
//     trade; then the order proceeds normally with what is left, including
//     FOK and IOC handling. A Replace may not raise its quantity above the
//     limit. A limit of 0 is rejected (REDUCE_ONLY_NO_POSITION).
//   - Self-trade prevention compares OwnerID and applies the incoming
//     order's mode: CANCEL_TAKER (the default) cancels the incoming
//     remainder, CANCEL_MAKER cancels the resting order and keeps matching,
//     CANCEL_BOTH cancels both. Trades executed before the self-match stand.
//   - Replace keeps the OrderID. NewQtyLots is the new total quantity
//     including fills; at or below the filled quantity it is rejected
//     (REPLACE_QTY_NOT_ABOVE_FILLED). Reducing the quantity at the same price
//     keeps queue priority; any price change or quantity increase loses it.
//     A Replace whose new price crosses the opposite side is treated as a
//     cancel plus a new aggressive GTC limit order under the same OrderID:
//     the order leaves its level, trades as the taker (subject to its STP
//     mode), and any remainder rests at the back of the new level. A Replace
//     with the same price and quantity is accepted and changes nothing.
//   - Cancel and Replace address an order by OrderID and must come from its
//     OwnerID (NOT_OWNER otherwise). A never-accepted OrderID is
//     UNKNOWN_ORDER; a filled or cancelled order is ORDER_NOT_OPEN.
//
// # Event order within one command
//
// NewOrder: ACCEPTED; CANCELLED (REDUCE_ONLY_CAP) if capped; then either
// CANCELLED (FOK), or for each match TRADE, the maker's PARTIAL_FILL or
// FULL_FILL, the taker's, and the maker level's BOOK_DELTA (self-trade
// prevention emits CANCELLED and BOOK_DELTA for a maker it removes and
// CANCELLED for the taker), followed by RESTING and BOOK_DELTA for a resting
// remainder or CANCELLED for a cancelled one. Cancel: CANCELLED, BOOK_DELTA.
// Replace: REPLACED, then BOOK_DELTA for each level whose aggregate changed,
// with any matching events and the RESTING of a re-queued order in between.
// A rejected command emits exactly one REJECTED event.
//
// # Concurrency
//
// A Book is single-threaded by design: matching is a sequential state
// machine. Sequencer serializes concurrent callers in front of one Book.
//
// # Never
//
// This package must never read a clock, draw randomness, use floating point,
// perform I/O, log, keep package-level mutable state, let map iteration order
// reach its output, post to the ledger or persist anything, or panic on any
// command or snapshot supplied by a caller.
package matching
