// Package devnethost is the shared, persistent host of the simulated public
// chains (ADR-0020, docs/architecture/ADVANCED_FINANCIAL_MODEL.md). It owns
// one devnet.Network with the two chains every devnet-mode subsystem uses --
// sbx-solana and sbx-ethereum -- for sandbox stable-asset issuance and burns,
// bridging, and (later) delegated signing.
//
// # What the chain is, and what it is not
//
// The chain is a simulation, not a ledger of truth. The platform ledger
// (internal/ledger) is the books. The chain is the "external truth" that
// reconciliation compares the books against: what a submitted mint, burn,
// lock or release actually did once it was included and finalized. Nothing
// read from this package is booked directly. A subsystem observes a FINAL
// outcome here, posts its own journal through the ledger, and a
// reconciliation (kinds STABLE_RESERVE, BRIDGE, DEVNET_CHAIN) compares the
// two.
//
// # Persistence and replay
//
// Every call that can change a chain is one event in the append-only
// devnet_events table (migration 00747): every accepted submission with its
// exact raw bytes, chain, tx id and submission time; every block production
// with the tip it reached; every operator call (mint creation, faucet
// airdrop) and every fault. A submission the chain refuses changes nothing
// and is not recorded; a transaction the chain already holds is acknowledged
// without a new event, exactly as the chain does it.
//
// An event is written in its own short transaction immediately after the call
// is applied in memory, never in a caller's transaction: the chain is the
// external system, and a caller's rollback must not be able to erase a
// submission the chain already accepted. The write takes a
// transaction-scoped advisory lock on the network, first replays whatever
// other processes appended, then appends at the next sequence number. The
// position (network, seq) is the event's idempotency key, and the history is
// gapless (SQLSTATE DH001) and a SHA-256 hash chain (each event commits to
// its predecessor). If the write fails after the call was applied in memory,
// the host discards its network and rebuilds it from what is committed, and a
// submission caught in that window is reported SUBMISSION_STATE_UNKNOWN with
// its tx id -- the caller observes Status, it never resubmits blindly.
//
// Open rebuilds the network deterministically by replaying the history from
// GENESIS (which records the chain configuration; a different configuration
// is refused). Replay verifies, for every event, the hash chain, that the
// call reproduces what it recorded (the same tx id and outcome for a
// submission, the same tip for a block production, the same removal count
// for a reorg) and that the network's StateHash afterwards equals the one
// recorded when the event was first applied. A history that does not replay
// exactly is refused: the host fails closed rather than run a chain that
// differs from the one the platform reconciled against.
//
// Every read first catches up with events committed by other processes, so
// two hosts on one database observe one chain.
//
// # Concurrency
//
// One mutex serialises every call. View gives a consistent multi-read
// snapshot (a supply and the statuses that explain it, at one instant).
//
// # Fault injection
//
// DropNext, TimeoutButLand, TimeoutAndDrop, DelayInclusion, ClearFaults,
// Reorg and SetMintFrozen pass through to the chain only in LOCAL, TEST and
// DEV; anywhere else they are FORBIDDEN. A fault is journaled like any other
// call, so a replay reproduces what it did.
//
// # What this package must never do
//
//   - Be treated as a source of truth for capital. Its balances, supplies
//     and statuses describe a simulation that reconciliation compares the
//     ledger against; nothing here is ever posted as-is.
//   - Hold, select or authorise keys, or sign anything. Callers submit bytes
//     that their own signers produced.
//   - Write its history in a caller's transaction, or let an accepted
//     submission go unrecorded and unreported: a failed write is either a
//     refusal (nothing happened) or SUBMISSION_STATE_UNKNOWN.
//   - Run on a history it cannot replay exactly, or on one produced under a
//     different chain configuration.
//   - Resubmit, retry or "repair" a transaction on anyone's behalf.
//   - Inject a fault outside LOCAL, TEST and DEV.
//   - Read the wall clock, use randomness, keep package-level state, or use
//     floating point.
package devnethost
