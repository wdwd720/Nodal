// Package nativemarket is the deterministic internal market engine (gola.md
// PARTS XIV, XV, XVI).
//
// One market model, chosen and documented in curve.go: constant product with a
// virtual Credit reserve. PART XIV asks for exactly one production-quality
// model rather than three incomplete ones, and the reasoning for this one — and
// against the alternatives — is stated there rather than left implicit.
//
// # A trade is one transaction
//
// PART XV is explicit: if any part of a trade fails, none of the economic
// effect occurs, and atomicity must not be emulated with event handlers.
// Execute therefore does all of this in the caller's single database
// transaction:
//
//	re-read and lock the market
//	re-price against CURRENT state, never against the quote's
//	check the caller's minimum output
//	post the journal transaction (which is where balance, sign, negative-balance
//	  and value-domain isolation are enforced)
//	consume the buyer's Credit lots, or issue the seller's proceeds
//	write the fill, which is what moves market state
//
// Market state is a projection that only a trigger writes. A fill carries the
// version it was priced against and the reserves it claims to produce; the
// trigger re-derives the reserves from the fill's own amounts, refuses a stale
// version, refuses a negative reserve, refuses supply that was never minted,
// and refuses anything that would put the pool below its constant product. So
// the invariant is enforced by PostgreSQL and not only by the Go that computes
// it (migration 00712).
//
// # Quotes are not execution
//
// A quote records what the market said at a version. It is stored, immutable
// and auditable, and it is not a promise. Execution re-prices, and a quote
// whose version no longer matches is refused rather than honoured — PART XIV's
// "execution revalidates market state" and "do not accept stale quotes".
//
// The caller's protection against the market moving underneath them is
// MinOutput, which is checked against the freshly computed fill. That is the
// number a user actually agreed to.
//
// # Integrity
//
// An automated market maker has no order book, so classic self-trade
// prevention has nothing to match against: the counterparty is always the
// pool. What does exist, and what surveillance.go looks for, is rapid round
// tripping to manufacture volume, creator self-dealing, holder concentration
// and anomalous volume. These raise alerts rather than blocking trades,
// because a detector that halts a market on a heuristic is a denial-of-service
// vector against creators; blocking is reserved for the states an operator or
// the asset's own lifecycle has chosen (HALTED, CLOSE_ONLY), which the
// database enforces.
//
// # What this package must never do
//
//   - Move market state by any route other than inserting a fill.
//   - Price a trade against a stored quote instead of current state.
//   - Let a market trade while its asset is not tradable in that direction.
//   - Mint or burn asset units. Supply is minted once, at market creation, and
//     conservation is checked by the trigger on every fill.
//   - Use floating point anywhere. Every quantity is an exact integer.
//   - Treat Credits and native assets as the same domain: every trade is a
//     declared cross-domain conversion and requires NATIVE_MARKET_TRADING.
package nativemarket
