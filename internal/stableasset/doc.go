// Package stableasset is the sandbox stable asset controller (goal §5;
// ADR-0020; docs/architecture/ADVANCED_FINANCIAL_MODEL.md §3-§7): a platform
// stable asset that exists only as SIMULATED value on a simulated chain,
// issued by a STABLE_ISSUER facility under a fully reserved policy. Names are
// generic (PlatformStableAsset, StableAssetController): nothing here is, or
// may be presented as, a public brand or a real-money token.
//
// # Registry
//
// A PlatformStableAsset names one SIMULATED asset on sbx-solana whose
// mint_address is the devnet mint (a hash-derived address nobody holds), its
// decimals and network, its status (ACTIVE <-> PAUSED, ACTIVE|PAUSED ->
// RETIRED, every change a bound transition row), its reserve policy (the
// eligible reserve assets, each CASH_EQUIVALENT or TOKENIZED with an
// eligibility factor; only cash equivalents pay for a mint or a redemption),
// its issuance cap, its mint authority mode (PLATFORM_SIGNER: the key lives
// only in IssuerSigner) and burn authority mode (HOLDER_SIGNED: the holder's
// custodied key), its redemption mode (INSTANT or QUEUED), its target peg (an
// exact price in a quote asset), its oracle source and whether it publishes
// reserve snapshots. The collateral composition is reported per snapshot.
//
// # Reserve ledger
//
// ReserveLedger states every figure's origin. Ledger balances of the issuer
// facility: issued supply (STABLE_OUTSTANDING = STABLE_ISSUED), the
// cash-equivalent and the tokenized reserve (STABLE_RESERVE per asset,
// mirrored by STABLE_RESERVE_FUNDING). Derived from request states, never
// booked: pending mint (= reserved-but-not-final supply), the receivable
// (customers' reserve funds held by capital reservations for those mints),
// pending burn, and pending redemption (payouts of burns not yet posted).
// Reserve is valued exactly (Value, never rounded) at verified prices from
// the asset's oracle source -- a cash equivalent at the lower of its price
// and par, the peg's own quote asset at par -- times its eligibility factor.
// A reserve asset with no fresh price counts zero.
//
// # The invariant
//
// Issued circulating supply at the peg never exceeds the verified eligible
// reserve. It is enforced (1) in Go at admission, under the asset's row lock,
// counting every mint not yet final with its incoming reserve and every
// unposted redemption's payout as gone (admitMint); (2) again in the posting
// transaction under the same row lock (cap and the issuance's own backing at
// its plan's price); (3) by the database at COMMIT for every
// STABLE_ISSUANCE / STABLE_REDEMPTION journal (migration 00747): the cap
// (SV003), each issuance's full backing at par and factor in one eligible
// cash equivalent (SV004), each redemption paying at most the peg value of
// what it retires (SV005). Those per-transaction rules imply the aggregate
// one at par; prices only ever lower the reserve, and a price-driven
// shortfall trips the circuit breaker.
//
// # Workflows
//
// Mint: authenticated actor, eligible account, legal route, reserve price
// verification, idempotency key, cap and reserve admission, Risk V2
// (account, identity, balance, chain, settlement sections), capital
// reservation of the reserve payment, effect claim, canonical hashed mint
// plan and signing plan, transaction inspection (TOKEN_MINT_TO, mint,
// recipient, amount, MEMO = plan hash, signer set = mint authority only, fee
// payer) and signing by IssuerSigner -- all in transaction 1, which records
// the signed bytes with status SUBMITTING. The submission happens between
// transactions, exactly once. Transaction 2 records its outcome; an ambiguous
// outcome (including a devnet timeout that landed) marks the effect UNKNOWN
// and the request SUBMISSION_UNKNOWN and is never resubmitted:
// ReconcileUnknown settles it from the chain's status by tx id. Finality is
// observed (SANDBOX produces the blocks itself), and only a FINALIZED mint is
// posted (STABLE_ISSUANCE, model §5), with the reservation consumed, the
// evidence archived, the effect applied, the post-issuance reserve verified
// and the supply reconciled, in transaction 3. A FAILED or expired mint fails
// the effect and releases the reservation; nothing is posted.
//
// Burn is the exact inverse with the same exactly-once structure:
// TOKEN_BURN signed by the holder's key held by CustodialWalletSigner, a
// burn bounded by both the holder's ledger balance (capital reservation) and
// its chain balance, STABLE_REDEMPTION after finality (or queued in QUEUED
// mode and paid by ProcessRedemptions).
//
// # Monitoring, reconciliation, snapshots
//
// RunMonitor checks the market price against the peg, the reserve coverage
// and the supply (ledger STABLE_OUTSTANDING vs devnet supply vs Σ customer
// WALLET); findings are stable_peg_alerts rows, and a deviation above the
// threshold, coverage under 100% or a supply mismatch trips the circuit
// breaker: the asset's own PAUSED status with a bound transition and a
// reason. A mismatch is also a STABLE_RESERVE reconciliation record that
// blocks new risk. TakeSnapshot writes a machine-verifiable reserve and
// supply snapshot (canonical JSON, hash-linked, Ed25519-signed with an
// injected key, archived as evidence); VerifySnapshotChain verifies a chain
// of them from the snapshots and the public key alone. A snapshot is not an
// attestation and says so in its body.
//
// # What this package must never do
//
//   - Move or represent real value, use a public stablecoin's name or brand,
//     or call a snapshot an attestation or an audit.
//   - Post an issuance or a redemption before the chain transaction is
//     FINALIZED, post one twice, or post one for a failed or expired
//     transaction.
//   - Resubmit a transaction whose outcome is unknown, or any transaction a
//     second time: an unknown outcome is resolved only from the chain's
//     status by transaction id.
//   - Admit a mint that would leave projected supply above the issuance cap
//     or above the verified eligible reserve, or count a reserve unit twice
//     (customer funds count only as the receivable of their own pending
//     mint, and only until it posts).
//   - Expose, log, return or print a private key or seed: the mint authority,
//     the custodial wallets and the snapshot key live only in closures, and
//     a signer signs only what its inspector has proved matches the plan.
//   - Let an agent mint, burn or administer an asset, or let the circuit
//     breaker resume an asset (only an operator does).
//   - Activate a CAPABILITY_DISABLE kill switch (the breaker uses the asset's
//     own status), keep package-level mutable state, read the wall clock, or
//     use floating point.
package stableasset
