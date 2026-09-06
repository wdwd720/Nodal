// Package buyingpower is the universal buying-power engine (FINANCIAL_MODEL
// §6, ADR-0015, goal PARTS 25–27): it answers "what value may this account
// deploy for this purpose right now?" as a pure function of typed inputs.
//
// # Responsibilities
//
//   - LoadSnapshot reads the inputs through SQL: WALLET ledger balances per
//     asset, active reservation totals, active withdrawal holds, pending
//     deposits (PROVIDER_CONFIRMED, SETTLEMENT_OBSERVED, RECONCILED at their
//     expected quantity), withdrawal-eligible AVAILABLE deposits, in-flight
//     withdrawals, the account status and asset metadata; kill switches and
//     reconciliation blocks come from small reader interfaces so the real
//     sources can be wired later without touching the arithmetic.
//
//   - Evaluate turns a Snapshot into the BuyingPower output deterministically
//     (assets are processed in id order, every rounding is explicit):
//
//     portfolio_value = Σ mark(balance) over assets that are not HALTED
//     buying_power    = Σ contribution(balance − reserved − holds, floored at 0)
//     available_now   = the part of buying_power from settlement assets
//     (stablecoin, risk class SETTLEMENT) that needs no conversion
//     reserved        = Σ mark(reserved)      pending = Σ mark(pending)
//     withdrawable    = min(available_now, Σ mark(withdrawal-eligible settled
//     funding) − Σ mark(in-flight withdrawals)), and 0 when any restriction
//     exists
//
//     where mark and contribution follow valuation.Classify/Mark: NORMAL
//     stablecoins at face × collateral factor, DEGRADED at market × factor,
//     RESTRICTED into portfolio value only, HALTED excluded with a
//     restriction. A stale price, a missing policy or a halted asset removes
//     that asset from buying power and adds an asset-scoped restriction;
//     an account that is FROZEN, RESTRICTED or CLOSED, an active new-risk
//     kill switch, or an open reconciliation block adds an account-scoped
//     restriction and zeroes buying_power and available_now.
//
//   - Purpose tightens what counts as blocking: DISPLAY never blocks, TRADE
//     is blocked by account-scoped restrictions, WITHDRAWAL is blocked by
//     any restriction at all. Figures are the same for every purpose so the
//     UI, the risk kernel and the settlement compiler see one number.
//
//   - policy_version is a hash of the engine version and every (asset,
//     policy_version) pair consulted; as_of is the injected clock's now.
//
// The withdrawable figure is a documented V1 simplification: it does not
// trace which specific units left through trades, so trading profits are
// never withdrawable beyond settled, withdrawal-eligible funding. That is
// conservative, and withdrawals are a DISABLED capability in V1.
//
// # What this package must never do
//
//   - Be cached or persisted as financial truth. Every call recomputes from
//     the ledger and policies; the API layer may cache the rendered response
//     for at most a couple of seconds, labeled derived.
//   - Reserve, post, hold or otherwise mutate. Reservation is the accounting
//     act and happens in internal/capital under row locks; this engine only
//     reads.
//   - Extend credit. There are no margin, leverage, treasury-fronting or
//     cross-chain fields: buying power never exceeds what held, settled,
//     unreserved assets support after haircuts.
//   - Sum wallet balances. A balance is an input, never the answer.
//   - Fail open. Missing policy, stale price, unknown status, misconfigured
//     quote asset: each reduces the figure or returns an error, never
//     increases it.
//   - Use floating point, iterate maps in undefined order for financial
//     results, or read the wall clock directly.
package buyingpower
