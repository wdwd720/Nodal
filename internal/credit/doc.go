// Package credit is the Nodal Credit ledger and the provenance of every unit
// in it (gola.md PARTS XI, XII, XX).
//
// # Why lots exist
//
// "The user has 18,450 Credits" does not answer "how much may they withdraw?",
// and a system that treats it as though it did will eventually pay out a
// promotional grant, or value backed by a card payment that is about to be
// charged back. Payout eligibility is a property of the individual units:
// where they came from (CreditOrigin) and how final the funding behind them is
// (FundingFinality). So Credits are held in immutable provenance lots,
// spending consumes specific lots in a defined order, and what is left over is
// what determines what can be withdrawn.
//
// Two users each holding 18,450 Credits may have completely different
// withdrawable amounts. That is not a bug to be smoothed over; it is the whole
// point.
//
// # Division of authority
//
//   - internal/ledger is authoritative for BALANCES. Every movement of Credits
//     is a journal transaction, subject to the balanced-per-asset trigger, the
//     negative-balance trigger and value-domain isolation.
//   - This package is authoritative for PROVENANCE. It never moves value on its
//     own; it records which lots a movement consumed, and refuses to consume
//     more of a lot than the lot holds.
//   - Both are written in the SAME database transaction, and the database
//     refuses a lot that does not tie back to a journal transaction touching
//     that account and asset (migration 00711, SQLSTATE CR004). A lot cannot
//     describe units the journal never moved.
//
// The two must agree: the sum of remaining lot quantities equals the account's
// CREDIT_BALANCE. VerifyProvenance checks it, and reconciliation runs it.
//
// # Consumption order
//
// Spending consumes the most restricted value first — promotional before
// purchased before earned — and oldest first within a rank. This is
// deliberately a structural ordering rather than "least payout-eligible
// first", because eligibility comes from a versioned policy, and ordering
// consumption by it would make a spend that happened last month replay
// differently after a policy change. Provenance has to be reproducible.
//
// The ordering is also the user-favourable one: ordinary spending burns
// restricted grants and leaves earned value intact, rather than silently
// destroying the balance a creator has been accumulating.
//
// # What this package must never do
//
//   - Move Credits without a journal transaction in the same database
//     transaction.
//   - Mutate a lot. Lots and lot events are append-only; remaining quantity is
//     a projection maintained by a trigger the application role cannot write.
//   - Decide payout eligibility on its own. It computes balances by category
//     given a policy; the policy is passed in, and internal/payout is what
//     decides.
//   - Let a finality change skip the transition table. REVERSED is terminal:
//     value that has been clawed back never comes back to life.
//   - Consume across accounts. A lot belongs to one account, permanently.
package credit
