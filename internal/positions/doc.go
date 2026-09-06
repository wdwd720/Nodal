// Package positions is the position engine (FINANCIAL_MODEL §5, goal PARTS
// 85 and 125): acquisition lots, FIFO dispositions, realized P&L and the
// lot-level record that later tax reporting needs.
//
// # Responsibilities
//
//   - Acquire: open a lot for an exact asset quantity with its cost basis
//     (price paid plus the fees allocated to the acquisition), the basis and
//     valuation sources, the acquisition reference (fill, deposit,
//     reconciliation adjustment), venue, wallet and journal transaction.
//   - Dispose: consume open lots first-in-first-out (acquired_at, id) under
//     row locks, splitting a lot when only part of it is disposed, and write
//     one immutable lot_dispositions row per lot touched. Basis is allocated
//     to a disposition by cumulative proportional rounding
//     (round(basis × consumed_after / original) − round(basis ×
//     consumed_before / original), RoundHalfEven), so the final piece of a
//     lot absorbs the rounding remainder and the sum of a lot's disposition
//     basis plus its remaining basis is exactly its cost basis. Proceeds and
//     fees of a disposal are spread over the lots it touches the same way,
//     so their sums are exact too. Realized P&L per row is
//     proceeds − fees − basis.
//   - Holdings: open quantity, remaining cost basis, lot count and an
//     average basis per whole unit (a display string) per asset.
//   - VerifyAgainstLedger: the sum of open lot quantity per asset must equal
//     the WALLET ledger balance; every difference is reported as a Drift.
//     Asset units paid as network fees are dispositions (proceeds 0), so a
//     caller that posts a fee to the ledger must also dispose it here or the
//     verifier will report the gap.
//   - RealizedPnL: totals and per-asset breakdown of dispositions in a
//     half-open time window.
//
// # What this package must never do
//
//   - Edit balances. Lots describe how an entitlement was acquired; the
//     entitlement itself lives in the ledger. Nothing here inserts journal
//     rows, changes ledger_balances, reservations or holds, and a drift
//     between lots and ledger is reported, never "fixed" by adjusting lots.
//   - Be a valuation. Unrealized P&L is computed on read by the valuation
//     layer from open lots and a current mark; it is never stored here.
//   - Mutate or delete a disposition. Corrections are new rows with
//     correction_of set.
//   - Use floating point, round implicitly, or lose a cent: every
//     allocation names a money.RoundingMode and conserves totals exactly.
//   - Claim tax-filing status. The records are tax quality (PART 125); a
//     filing claim needs a partner (EB-016).
package positions
