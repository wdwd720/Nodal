// Package transfers moves SIMULATED value from one customer account to
// another (ADR-0020, docs/architecture/ADVANCED_FINANCIAL_MODEL.md §5, §6,
// §10; capability P2P_TRANSFERS, modes PAPER and SANDBOX; migration 00744).
//
// # The invariant
//
// A transfer changes who owns a liability, never the liability: for every
// asset, the sum over customers of WALLET is the same before and after any
// transfer or reversal. Each posting balances per party (the database
// enforces it for P2P_TRANSFER, SM002), and for a customer whose only
// activity in an asset is sandbox funding, transfers and reversals:
//
//	WALLET + TRANSFER_OUT − TRANSFER_IN − CAPITAL = 0
//
// Across customers Σ TRANSFER_OUT = Σ TRANSFER_IN, which is why Σ WALLET
// equals Σ CAPITAL and does not move. Reconcile proves both from the
// database, and that every APPLIED transfer effect has exactly one journal.
//
// # Send, step by step (all in the caller's transaction)
//
//  1. Authenticate: a non-agent USER who owns the sender account
//     (security.RequireAccountOwner). An agent is refused with the settlement
//     compiler's own answer (econ.Route: SIMULATED_TRANSFER_VALUE is
//     permanently forbidden to agents).
//  2. Serialize the sender (a transaction advisory lock), then replay on
//     (sender, idempotency key): same request, same transfer and outcome.
//  3. Resolve the recipient by account id or handle; eligibility: both
//     ACTIVE customer accounts, different, the asset SIMULATED and ACTIVE.
//  4. Authorize the mode (econ.Authorizer: gate, kill switches, LIVE refused).
//  5. Value the amount (par for a registry-declared USD-pegged simulated
//     unit, otherwise a fresh mark; never zero), and above the policy's
//     threshold demand a recent strong authentication (STEP_UP_REQUIRED).
//  6. Record the economic intent (TRANSFER_VALUE) and the transfer, PENDING.
//  7. Route the intent through the settlement compiler.
//  8. Apply the versioned, hashed transfer Policy (per-transfer, daily and
//     velocity limits read from this package's own table, recipient
//     allow/block rules, allowlist-only mode) and Risk Kernel V2 with the
//     Account, Identity, Balance, Velocity and Destination sections; the risk
//     decision is persisted whatever it says.
//  9. Screen it (ComplianceScreen; an error is a refusal).
//  10. Reserve the sender's funds (capital.Service.Reserve): the exact,
//     authoritative no-overspend check shared with every other spender.
//  11. Claim the effect (p2p:<transfer id>), post P2P_TRANSFER, consume the
//     reservation, archive the evidence, apply the effect, complete the
//     transfer and the intent, notify the recipient.
//
// A refusal after step 6 is a recorded decision: transfer and intent
// REJECTED with a stable code, risk decision and evidence written, nothing
// posted.
//
// # Reversal, under dual control (migration 00758)
//
// A COMPLETED transfer is reversed by two operators, never one. An OPERATOR
// holding ledger:post_correction, with a recent strong authentication,
// proposes the reversal with a stable reason code and a reason
// (ProposeReversal); nothing moves. A second, different operator with the same
// permission and a recent strong authentication approves it
// (ApproveReversal), and the approval executes the correction in the same
// transaction: a COMPENSATION journal with reversal_of and the proposal's
// reason code, the effect COMPENSATED, the transfer REVERSED naming the
// proposal. Anyone with the permission may reject a proposal instead
// (RejectReversal), the proposer withdrawing it included. At most one proposal
// of a transfer is open at a time and a proposal is decided once, so an
// approved proposal is consumed and a rejected one never executes. The
// reversal is refused when the recipient no longer holds the funds, and the
// proposal stays open. The database holds the same line: PT006 (self
// approval), PT007 (a reversal without an approved two-operator proposal, or
// an approval that did not reverse), PT008 (a proposal for a transfer that is
// not COMPLETED, or while another is open).
//
// # What this package must never do
//
//   - Move anything but SIMULATED value, or post anything but the §5
//     P2P_TRANSFER template and its exact COMPENSATION mirror.
//   - Let an agent send, reverse or configure a transfer, or let anyone but
//     the sender account's owner send one or anyone but operators holding
//     ledger:post_correction propose, approve or reject a reversal.
//   - Let one operator reverse a transfer alone: reverse without an approved
//     proposal by two distinct operators, let a proposer approve their own
//     proposal, or execute a rejected or an already approved proposal.
//   - Edit history: a reversal is a new journal; transfer rows change only
//     along PENDING -> COMPLETED | REJECTED and COMPLETED -> REVERSED, each
//     with a transition row (the database refuses anything else).
//   - Make a balance negative, or reverse a transfer whose funds the
//     recipient no longer holds.
//   - Value an asset at zero, or proceed on a missing price, a missing risk
//     section, an unreachable compliance screen or an unknown rule: every
//     missing input refuses.
//   - Put the memo anywhere but the transfer row: not in a journal
//     description, metadata or content hash, not in an event, not in a
//     notification.
//   - Read limits from anything but the database (never Redis, never memory),
//     hold mutable state at package level, read the wall clock or use
//     floating point.
package transfers
