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
// An outcome share exists only as its market issued it (migration 00756): a
// share asset can appear in a journal entry only of those four kinds, on a
// customer's WALLET, TRADING_INFLOW or TRADING_OUTFLOW or on its own market
// facility's share accounts, or in a SIM_TRADE between customer accounts (the
// internal order book); every such transaction moves customer wallets exactly
// as much as the market's outstanding shares. No faucet grant, transfer,
// compensation or house book can create, hold, move or destroy a share, so
// one stray unit can never wedge a market.
//
// The conservation identity, checked from one database snapshot before and
// after every economic operation and by the PREDICTION_ESCROW reconciliation:
//
//	PREDICTION_ESCROW = PREDICTION_COLLATERAL
//	                  = PREDICTION_SHARES_OUTSTANDING(each outcome) + disclosed residual + unclaimed held payouts
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
// refuse new orders on every path, and -- when the deployment wires
// OutcomeBooks -- every resting order on the outcome books is cancelled in
// the same transaction. At settlement or void the market instrument goes
// HALTED too, or stays CLOSE_ONLY while a held payout awaits its claim.
//
// Market creation, resolution, disputes, rulings, voids and settlement are
// operator actions; opening, closing and requesting resolution may also be
// taken by the system scheduler. No agent takes any of them. Separation of
// duties, enforced here and again in the database: the creator of a market
// never resolves it or takes part in voiding it; an operator who recorded an
// oracle observation for a market never resolves it; the operator who raised
// a dispute and the operator whose resolution is disputed never rule on it. A
// void is proposed by one operator with a reason and evidence and approved by
// a different one; neither may be the market's creator or the author of its
// resolution in force, a proposal cannot outlive a new resolution, and a
// RESOLVED market inside its dispute window is disputed, not voided.
//
// Oracle reports are Ed25519-signed (PredictionOracleProvider for an
// external oracle; SandboxOracle is the deterministic sandbox signing key),
// verified against the key the market registered before they are accepted,
// and archived as evidence. Sandbox observations are durable: RecordObservation
// signs and appends each one, with an audit event, in the caller's
// transaction, and FetchReport reads the latest stored one back, verified. A
// report pushed by an external oracle is decoded with DecodeOracleReport.
// Replaying an accepted report changes nothing; a different report after
// resolution is refused unless the market is DISPUTED.
//
// Every economic action -- issuance, redemption, each holder's payout or
// refund, a held payout's claim -- is an econ intent
// (CREATE_PREDICTION_POSITION or RESOLVE_PREDICTION_POSITION) that is routed
// through the settlement compiler, authorized for its mode, evaluated by
// Risk V2 with its instrument and balance sections, backed by a capital
// reservation where it spends customer value, claimed as an economic effect
// under a deterministic key, posted to the ledger, applied, and evidenced, all
// in the caller's transaction. An issuance or redemption refused before
// execution leaves its intent REJECTED (and a risk refusal its decision) in
// that transaction: commit it to keep the refusal on record. Execution itself,
// and every lifecycle operation including a whole settlement or void, runs in
// a savepoint and is all or nothing: a failure leaves no partial state, even
// for a caller that commits afterwards.
//
// One holder never blocks a settlement or a void. A holder whose own payout
// the route or Risk V2 refuses for the holder's own reasons (a FROZEN or
// CLOSED account, an unverified identity, a jurisdiction) has their shares
// retired like everyone else and their amount HELD in the market's escrow,
// recorded per holder; they claim it with ClaimHeldPayout once eligible. Any
// other refusal stops the whole operation. Outcome shares still reserved by
// resting orders are released through OutcomeBooks when it is configured;
// otherwise the settlement or void is refused with every reservation named.
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
//   - Let an outcome share come into being, move or vanish other than by its
//     own market's issuance, redemption, payout or refund, or a trade between
//     customers; or let any book but a customer wallet and its market's
//     facility hold one.
//   - Match orders or run a second order book (OutcomeBooks is the order
//     book's, wired by the deployment; this package never imports it).
//   - Let an agent create, open, close, resolve, dispute, void or settle a
//     market, or apply any economic effect. An agent may only be the actor of
//     an issuance or redemption intent, which the kernel then evaluates.
//   - Void a market on one operator's word, or let anyone resolve, rule on or
//     void what they created, recorded, resolved or disputed themselves.
//   - Accept an oracle report it has not verified against the market's key,
//     or a report for another market; or keep oracle observations anywhere
//     but the database.
//   - Settle a market that is not RESOLVED, that is DISPUTED, whose dispute
//     window is open, or whose books do not reconcile; let one holder's
//     account block everyone else's payout; pay any share twice; or leave
//     outstanding shares behind, or escrow other than the residual and the
//     held payouts it names, after settlement.
//   - Retry an economic effect whose outcome is unknown, hold mutable state at
//     package level, read the wall clock, or use floating point.
package predmarket
