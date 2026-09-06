// Package quote is the provider-neutral quote model (goal PART 42; PART 126
// fee disclosure; SETTLEMENT_COMPILER.md §7). A Quote is the immutable record
// of what a venue offered at one instant: what goes in, what is expected and
// guaranteed to come out, the effective price, the estimated costs, when it
// was received and when it stops being valid, plus hashes of the route and
// of the raw provider response so the evidence can be verified later.
//
// # Responsibilities
//
//   - Quote mirrors the quotes table exactly. Every amount is money.Quantity
//     in the asset's base units, every rate is money.BPS and the effective
//     price is money.Price; nothing is floating point.
//   - Validate checks structure, minimum_output <= expected_output, rates,
//     expiry after receipt, and that the route and raw-response hashes are
//     present and the route hash matches the stored route summary.
//   - IsExpired and IsFresh are the only staleness rules: a quote is expired
//     at and after ExpiresAt, and fresh only while it is not expired, was not
//     received in the future, and its age is at most the caller's maximum.
//   - Disclosure is the customer-facing breakdown (PART 42, PART 126):
//     expected receive, minimum receive, price impact, slippage, venue fee,
//     network estimate, platform fee, total estimated cost per asset, route
//     and expiry. Expected receive is the venue's number verbatim and the
//     platform fee is its own line.
//   - RouteHash is the deterministic digest of a route summary and
//     HashRaw the digest of raw provider bytes.
//   - Repository.Record inserts; Get and LatestForIntent read.
//
// # What this package must never do
//
//   - Alter a provider's numbers. Expected output, minimum output, price,
//     impact and fees are stored as quoted; the platform fee is disclosed
//     separately and never folded into the expected output. There is no
//     hidden spread anywhere in this package.
//   - Update or delete a quote: the table has no UPDATE/DELETE grant for the
//     application role and a forbid_mutation trigger; a new quote is a new
//     row.
//   - Decide whether a quote is acceptable for execution. Freshness and
//     expiry are reported; the settlement compiler and the risk kernel judge
//     them against the intent's constraints and policy.
//   - Call a provider, read a clock, or let a provider's schema leak: the
//     execution adapter builds the Quote and supplies the time.
package quote
