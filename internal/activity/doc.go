// Package activity is the unified user activity timeline (product goal §16).
//
// # What it is
//
// One reverse-chronological feed over the facts that already happened in other
// domains' tables. It owns no table, writes nothing, and derives nothing: every
// item is a row somebody else wrote inside the transaction that made it true, so
// a feed entry cannot exist for something that did not happen and cannot be
// missing for something that did.
//
// That is the whole design constraint. An activity table fed by events would be
// a second copy of the financial record with its own failure mode -- a
// transaction that commits and an event that does not -- and §52's audit trail
// would then have two answers to "what happened to this account".
//
// # The extension point
//
// A Kind is one domain's contribution, and adding one is adding a Source to
// `sources` in sources.go plus a template in summary.go. A Source is:
//
//   - a Kind, which is what a caller filters by;
//   - one SQL statement selecting the columns in `sourceColumns`, for the
//     account in $1, from tables that domain already owns.
//
// Nothing else changes: the union, the cursor, the kind filter and the response
// shape are all driven from the slice. `TestSourcesAreWellFormed` holds every
// source to the column contract, and `TestEveryKindHasASource` and
// `TestEveryKindHasASummaryTemplate` make a Kind without an implementation a
// test failure rather than an empty feed.
//
// Kinds this package does NOT yet declare, because the domains that would raise
// them are being built alongside it: verification state changes, profile and
// security events, and agent actions. Each is a Source and a template when its
// domain lands; the orchestrator adds them here rather than in that domain, so
// the feed stays one query.
//
// # Temperature
//
// §46 forbids showing different kinds of value as though they were the same
// thing, so every amount carries what KIND of value it is:
//
//	ECONOMY    Nodal Credits: internal, closed-loop, not redeemable by default
//	REAL       money at a payment provider, in minor units of its currency
//	SIMULATED  value on a sandbox tier, or an object a demo seeder created
//
// SIMULATED wins over both others. On a sandbox tier no value is real by
// construction (ADR-0023), so the deployment stamps the whole feed; a demo
// object stamps its own items even where the rest is not stamped.
//
// # Summaries
//
// The human sentence is built here, from a fixed template per kind, never from
// user-supplied text and never in the browser. Two reasons: a summary composed
// in the client is a second implementation that eventually disagrees with the
// amounts beside it, and a summary that interpolated a creator's asset name
// without escaping would be a stored-XSS vector with a friendly name. The
// templates interpolate only amounts, symbols and enum values that come from
// columns with CHECK constraints.
package activity
