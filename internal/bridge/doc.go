// Package bridge is the sandbox cross-chain capital controller (goal §18;
// ADR-0020; docs/architecture/ADVANCED_FINANCIAL_MODEL.md §3-§7): routes that
// join a SIMULATED asset on one simulated chain to its bridged twin on
// another, legs that move an account's units between them through a bound
// state machine, the BRIDGE facility's books, the BRIDGE reconciliation and
// the bridge's part of Universal Buying Power V2. Every chain here is the
// in-process devnet behind internal/devnethost; every unit is simulated
// value. The ledger is the books; the chains are a simulation the books are
// reconciled against.
//
// # Provider
//
// BridgeProvider is the seam (Quote, Plan, Execute, Observe). DevnetProvider
// is deterministic: the lock is a BRIDGE_LOCK on the source chain (the units
// move into the chain's escrow address, which no key controls, under a
// single-use nonce derived from the leg id) with the hash of the leg's
// BridgePlan as its MEMO, signed by the holder's custodian (HolderSigner,
// implemented by stableasset.CustodialWalletSigner) after inspection; the
// release is a BRIDGE_RELEASE on the destination, built by
// devnet.BuildRelease only from a FINALIZED lock that is exactly the leg's,
// and signed by the Attester (the chains' bridge authority) only after
// InspectRelease proves it is exactly the expected release. The chain
// itself makes a release single-use per lock; the destination mint's
// authority is an address nobody holds, so only a release ever mints it.
//
// # States
//
// AVAILABLE -> RESERVED -> BRIDGE_REQUESTED -> SOURCE_FINALIZED ->
// MESSAGE_IN_FLIGHT -> DESTINATION_OBSERVED -> DESTINATION_FINALIZED ->
// RECONCILED, with FAILED (before the source is final, or after a refund)
// and MANUAL_REVIEW (a destination that cannot complete or reconcile on its
// own). Every change is a bound transition row (BR001 and the transition
// binding of migration 00748). Finality never reverts: no edge leads from a
// finalized state back to an earlier one. A lock confirmed and then reorged
// away before finality leaves the leg BRIDGE_REQUESTED; it fails only once
// the chain no longer holds it and its blockhash has expired below the
// finalized height, so it can never land. A release confirmed and then
// reorged away returns DESTINATION_OBSERVED to MESSAGE_IN_FLIGHT, and a new
// release is relayed only once no earlier one can still land.
//
// # Books (model §5, BRIDGE facility per route)
//
//	BRIDGE_SOURCE_FINALIZED       C: Dr BRIDGE_IN_FLIGHT(src) / Cr WALLET(src)
//	                              F: Dr BRIDGE_LOCKED(src)    / Cr BRIDGE_LOCK_FUNDING(src)
//	BRIDGE_DESTINATION_FINALIZED  C: Dr TRADING_OUTFLOW(src)  / Cr BRIDGE_IN_FLIGHT(src)
//	                                 Dr WALLET(dst)           / Cr TRADING_INFLOW(dst)
//	                              F: Dr BRIDGE_MINTED(dst)    / Cr BRIDGE_OUTSTANDING(dst)
//	BRIDGE_REFUND                 C: Dr WALLET(src)           / Cr BRIDGE_IN_FLIGHT(src)
//	                              F: Dr BRIDGE_LOCK_FUNDING(src) / Cr BRIDGE_LOCKED(src)
//
// Each posts only after the chain transaction it records is FINALIZED, once
// (deterministic idempotency keys), bound at COMMIT to exactly one leg (BR005)
// and keeping BRIDGE_LOCKED = BRIDGE_OUTSTANDING + Σ BRIDGE_IN_FLIGHT (BR003).
//
// # Workflow
//
// Bridge runs a leg. Transaction 1 (PrepareLeg): authentication, the
// CROSS_CHAIN_BUYING_POWER gate, the route's row lock, the economic intent
// (idempotent), the settlement compiler, account eligibility, the holder's
// chain balance net of its pending locks, Risk V2 (account, identity,
// balance, chain sections, the route's open BRIDGE records as breakers), the
// capital reservation of the units, the source effect claim, the plan and
// the holder's signed lock -- recorded before anything is submitted. The lock
// is submitted exactly once between transactions; transaction 2 records the
// outcome. An ambiguous outcome marks the effect and intent UNKNOWN and is
// never resubmitted: the chain's status by tx id settles it. Then (SANDBOX)
// blocks are produced and the leg is driven: SettleSource, PrepareRelease
// (recorded release attempt, then one submission), SettleDestination,
// ReconcileLeg. DEVNET legs are only observed (Observe, ObservePending).
// Release attempts are bounded per round; exhaustion, or a destination that
// says the lock was already released, is MANUAL_REVIEW, from which an
// operator resumes the relay (ResumeRelay), resolves a released leg
// (ResolveReview) or refunds (Refund: only when every release attempt failed
// or expired below finality; the refund mints replacement source units to
// the holder while the escrowed originals stay retired, so it is refused for
// an asset whose supply another subsystem reconciles, such as a platform
// stable asset).
//
// # Reconciliation and buying power
//
// ReconcileRoute compares the books with both chains at one instant (see
// RouteReconciliation); a mismatch is a material BRIDGE reconciliation record
// that blocks new risk and pauses the route (its own status, never a kill
// switch). BuyingPower counts each leg's value in exactly one place -- the
// ledger account holding it -- and in-flight value only at the haircut
// policy's factor (zero by default), refusing to answer when the legs and
// the ledger disagree.
//
// # What this package must never do
//
//   - Move or represent real value, connect to a real chain, RPC or bridge,
//     or use a public bridge's or token's name or brand.
//   - Post a source, destination or refund journal before the transaction it
//     records is FINALIZED, post one twice, or let a finalized leg go back.
//   - Count a leg's value on both chains, or on neither: in the books
//     (BR003), in the reconciliation, or in buying power.
//   - Resubmit a lock or a release whose outcome is unknown, relay a new
//     release while an earlier one can still land, or refund a leg any of
//     whose releases can still land or has landed.
//   - Expose, log, return or print a private key or seed: the attester's key
//     lives only in a closure, the holder's in its custodian, and each signs
//     only what its inspector has proved is the planned transaction.
//   - Let an agent move capital across chains, or let the circuit breaker
//     resume a route (only an operator does).
//   - Keep package-level mutable state, read the wall clock, or use floating
//     point.
package bridge
