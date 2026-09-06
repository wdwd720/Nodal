// Package valuation is the economic-valuation layer of the financial core
// (FINANCIAL_MODEL §6, goal PARTS 25, 26, 33): price observations, the
// append-only asset policy history (status, collateral factor, stablecoin
// status, maximum price age) and the arithmetic that turns an exact asset
// quantity into a USD mark and a buying-power contribution.
//
// # Responsibilities
//
//   - PolicyStore: the current AssetPolicy for an asset at a point in time
//     (latest effective, unexpired row). A missing policy is never an
//     absence of restriction: Current returns a fail-closed policy (status
//     RESTRICTED, collateral factor 0, PolicyMissing = true) so callers value
//     the asset for display only and never extend buying power against it.
//     RecordPolicy appends history and refuses AGENT actors.
//   - PriceStore: the latest price observation for an (asset, quote) pair
//     that is no older than the caller's maximum age, otherwise
//     STALE_MARKET_DATA. RecordPrice is idempotent on
//     (asset, quote, source, observed_at).
//   - Valuer: exact marks. ValueQuantity uses money.Notional followed by
//     money.QuoteQuantityToUSD, both with RoundHalfEven (MarkRounding).
//     Collateral haircuts round toward zero (HaircutRounding), i.e. toward
//     less buying power. Classify and Mark implement the stablecoin
//     contribution rule: NORMAL contributes face × collateral factor,
//     DEGRADED contributes market price × collateral factor, RESTRICTED
//     contributes to portfolio value only, HALTED is excluded and blocks
//     operations on the asset.
//
// # What this package must never do
//
//   - Be accounting truth. A mark is a valuation of an entitlement that lives
//     in the ledger; nothing here changes a balance, a reservation or a hold,
//     and nothing here is persisted as a customer's value.
//   - Hard-code a peg. Face valuation of a stablecoin is granted only by a
//     NORMAL stablecoin status in the current policy of a registry asset
//     whose peg currency is USD; every other state marks to market.
//   - Fail open. No policy, an unknown status, an unset stablecoin status
//     on a stablecoin, or a price older than the policy's maximum age all
//     reduce buying power (to zero for that asset), never increase it.
//   - Use floating point or round implicitly. Every rounding step names a
//     money.RoundingMode.
//   - Let an AGENT actor write a policy or decide which price source is
//     authoritative.
package valuation
