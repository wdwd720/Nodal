// Package valuedomain is the type system for "what kind of value is this".
//
// It exists because the single most dangerous defect this product can ship is
// treating two balances as interchangeable when they are not. A promotional
// Credit, a Credit bought with a card that can still be charged back, a
// simulated dollar in a shadow portfolio, a dollar held by a licensed partner,
// and a lamport in a customer's own wallet are five different things with five
// different legal characters. The goal document (PART IX) requires them to be
// distinguishable by type, not by convention or by the name of a column.
//
// # Responsibilities
//
//   - ValueDomain classifies every asset, every ledger account and every
//     balance the system reports.
//   - CapitalRail classifies where value is actually held and executed, and
//     which party is authoritative for it.
//   - CreditOrigin records where a unit of internal Credit came from. Two
//     Credits with the same number and different origins are not the same
//     value, because payout eligibility is derived from origin.
//   - Policy maps origin to payout treatment. It is versioned data, never a
//     constant: counsel or a provider may approve a different mapping, and
//     when they do the change is a new policy version with evidence, not an
//     edit to this file.
//   - Isolation states which domains may appear together inside one journal
//     transaction, so that "Credits leaked into the real-capital ledger" is a
//     rejected transaction rather than an incident.
//
// # Fail closed
//
// DefaultPolicy makes every origin payout-ineligible. A fresh deployment can
// therefore pay nobody out, and enabling payout for an origin requires a
// persisted policy version whose activation is evidence-backed and
// dual-approved (see internal/gates). Absence of configuration never means
// permission.
//
// # What this package must never do
//
//   - Decide that two domains are equivalent because their numbers can be
//     added. Aggregation across domains is a product decision with legal
//     consequences and lives above this package (PART II).
//   - Import internal/gates, internal/ledger or any store. It is a leaf: pure
//     types and pure functions, so every other package can depend on it and
//     the compiler can enforce the vocabulary everywhere.
//   - Read a clock, a config value, an environment variable or a database.
//     A Policy is passed in; it is never discovered.
//   - Encode a legal conclusion. "MAY_BE_ELIGIBLE" means a policy could permit
//     it, not that any jurisdiction does.
package valuedomain
