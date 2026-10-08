// Package predmarket implements paper and sandbox prediction markets
// (docs/architecture/ADVANCED_FINANCIAL_MODEL.md §5, decision D-053, ADR-0020).
// It is unrelated to internal/prediction, the agent forecast ledger.
//
// # The model
//
// A market is an EVENT_OUTCOME instrument (outcomes in its terms, expiry =
// close time), one PREDICTION_MARKET facility whose ledger books are that
// market's segregated escrow, and one SIMULATED share asset per outcome
// (chain nodal-sandbox, six decimals: 1,000,000 base units are one share).
// Each outcome also gets its own EVENT_OUTCOME instrument (base = the share,
// quote = the collateral) for the internal order book to trade; this package
// never matches orders itself.
//
// Collateral is a USD-pegged six-decimal SIMULATED asset (the sandbox dollar):
// one collateral unit is 1,000,000 base units, and one share pays at most one
// unit. Value moves only as ledger postings of the simulated-only kinds:
//
//   - PREDICTION_SET_ISSUANCE: N collateral units buy N complete sets (one
//     share of every outcome).
//   - PREDICTION_SET_REDEMPTION: N complete sets return N collateral units.
//   - PREDICTION_PAYOUT: settlement pays every share of the winning outcome
//     exactly one unit and retires every share of every outcome.
//   - PREDICTION_REFUND: a void pays each share floor(unit / outcome count)
//     collateral base units, rounded down once per holder; the remainder stays
//     on the facility's books as a residual the settlement record discloses.
//
// The conservation identity, checked from one database snapshot before and
// after every economic operation and by the PREDICTION_ESCROW reconciliation:
//
//	PREDICTION_ESCROW = PREDICTION_COLLATERAL
//	                  = PREDICTION_SHARES_OUTSTANDING(each outcome) + disclosed residual
//	PREDICTION_SHARES_ISSUED(each outcome) = PREDICTION_SHARES_OUTSTANDING(each outcome)
//	                                       = Σ customer WALLET(that outcome's share)
//
// # The lifecycle
//
// DRAFT → OPEN → CLOSED → RESOLVING → RESOLVED → SETTLED, any state but
// SETTLED and VOIDED → VOIDED (refunds), and RESOLVED → DISPUTED → RESOLVED
// or VOIDED. The table is stated once in Go (CanTransition) and once in SQL
// (cp_prediction_market_transition_legal), and a parity test compares them.
// Times are read from an injected clock: nothing opens before open time,
// nothing is issued or redeemed at or after close time, nothing resolves
// before close, and nothing settles before resolution plus the dispute
// window. At close the outcome instruments go HALTED and the market
// instrument CLOSE_ONLY, so Risk V2's instrument check and the order book
// refuse new orders on every path; at settlement or void the market
// instrument goes HALTED too.
//
// Market creation, resolution, disputes, rulings, voids and settlement are
// operator actions; opening, closing and requesting resolution may also be
// taken by the system scheduler. No agent takes any of them. The creator of a
// market can never resolve or void it, and the operator who raised a dispute
// can never rule on it.
// Oracle reports are Ed25519-signed (PredictionOracleProvider; SandboxOracle
// is the deterministic sandbox implementation), verified against the key the
// market registered before they are accepted, and archived as evidence.
// Replaying an accepted report changes nothing; a different report after
// resolution is refused unless the market is DISPUTED.
//
// Every economic action -- issuance, redemption, each holder's payout or
// refund -- is an econ intent (CREATE_PREDICTION_POSITION or
// RESOLVE_PREDICTION_POSITION) that is routed through the settlement
// compiler, authorized for its mode, evaluated by Risk V2 with its instrument
// and balance sections, backed by a capital reservation where it spends
// customer value, claimed as an economic effect under a deterministic key,
// posted to the ledger, applied, and evidenced, all in the caller's
// transaction. An issuance or redemption refused before execution leaves its
// intent REJECTED (and a risk refusal its decision) in that transaction:
// commit it to keep the refusal on record. Execution itself, and every
// lifecycle operation including a whole settlement or void, runs in a
// savepoint and is all or nothing: a failure leaves no partial state, even
// for a caller that commits afterwards.
//
// The econ intent lifecycle changes one intent's status several times in one
// transaction; migration 00733 lets the intent and effect bindings accumulate
// edges for exactly that, each step still licensed by its own transition row.
// Markets and instruments never change status twice in one transaction.
//
// # What this package must never do
//
//   - Move real value, or accept a LIVE mode: markets exist in PAPER or
//     SANDBOX only, collateral and shares are SIMULATED, and every posting is
//     a simulated-only kind that the database checks per party.
//   - Match orders or run a second order book.
//   - Let an agent create, open, close, resolve, dispute, void or settle a
//     market, or apply any economic effect. An agent may only be the actor of
//     an issuance or redemption intent, which the kernel then evaluates.
//   - Accept an oracle report it has not verified against the market's key,
//     or a report for another market.
//   - Settle a market that is not RESOLVED, that is DISPUTED, whose dispute
//     window is open, or whose books do not reconcile; pay any share twice;
//     or leave escrow or outstanding shares behind after settlement.
//   - Retry an economic effect whose outcome is unknown, hold mutable state at
//     package level, read the wall clock, or use floating point.
package predmarket
