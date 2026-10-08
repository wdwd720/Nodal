// Package delegation is delegated agent authority over SIMULATED value
// (ADR-0020, docs/architecture/ADVANCED_FINANCIAL_MODEL.md, goal §16 and §17;
// migrations 00752 and 00753): delegated agent signing (capability
// AGENT_SIGNING) and delegated agent withdrawals (capability
// AGENT_WITHDRAWALS), in SANDBOX and DEVNET modes, on the simulated chains of
// internal/devnethost.
//
// # Capabilities
//
// A Capability is a customer's grant to one of the account's own agents, for
// that agent's own strategy: allowed instrument classes and action classes,
// lifetime, daily and single-trade notional limits, a destination allowlist,
// a chain allowlist, an expiry, a unique nonce, the environment and mode it
// was granted in, and whether every use needs a human approval. It is
// content-hashed and immutable except for revocation; only the account's
// owner grants one, and the owner (or an operator) revokes it. Every use
// writes a usage row that the database admits only inside the grant, one
// admission per capability at a time, so concurrent uses can never together
// exceed a limit.
//
// # Delegated signing
//
// An agent never touches a key. It asks for a swap against a registered
// sandbox venue, and the request travels Agent -> Intent -> Risk Kernel V2 ->
// Capital Reservation -> Settlement Plan -> Transaction Inspector -> Signing
// Policy -> Signing Provider:
//
//   - RequestSwap records the agent's economic intent (ACQUIRE_EXPOSURE or
//     REDUCE_EXPOSURE, idempotent on the capability and the request nonce),
//     routes it through the settlement compiler, evaluates Risk V2 with the
//     capability's facts as the authority section, reserves the pay asset,
//     records the capability usage and the settlement plan (a delivery-
//     versus-payment exchange with the venue at the venue's quote, bounded by
//     the agent's slippage). Every refusal is recorded.
//   - When the capability requires it, the account's owner approves or
//     declines the exact plan (bound by its hash).
//   - SignSwap compiles the transaction from the persisted plan, and the
//     DelegatedSigner -- the signing provider -- re-reads the capability, the
//     plan, the approval and the compiled message from the database,
//     validates the capability again, inspects the transaction field by field
//     (programs, destination, token accounts, amounts, slippage, fee, signer
//     set, plan hash memo), applies the signing policy, records its one
//     decision and only then signs with a key derived inside it. The venue
//     co-signs its leg after its own inspection.
//   - The signed transaction is submitted to the devnet host exactly once.
//     An unknown outcome marks the effect and intent UNKNOWN and is settled by
//     observing the chain by transaction id, never by resubmitting.
//   - Once the chain finalizes it, the finalized transaction is compared with
//     the signed one, the SIM_TRADE exchange is posted, the reservation is
//     consumed, the evidence archived and the effect applied.
//
// # Delegated withdrawals
//
// An agent may only PROPOSE a withdrawal; the settlement compiler forbids it
// to withdraw at every authority level. ProposeWithdrawal checks the agent's
// capability (PROPOSE_WITHDRAWAL, destination and chain allowlists, limits),
// records the service's WITHDRAW intent, routes and risk-evaluates it,
// reserves the funds, records the capability usage, and evaluates the
// customer's withdrawal policy (destination allowlist and age, account age,
// per-withdrawal, daily and velocity limits, risk score, compliance state,
// anomalies, agent scope). The policy auto-approves only in low-risk test
// mode (LOCAL, TEST or DEV, every auto bound met); otherwise the account's
// owner decides, with a fresh strong authentication when the policy asks for
// one. The approved destination and amount are frozen in a plan whose hash
// the approval binds. ExecuteWithdrawal then compiles, inspects, signs through
// the same provider, submits once, observes finality, reconciles and posts
// SANDBOX_WITHDRAWAL. An account freeze (account status or kill switch) stops
// a withdrawal at every step before submission.
//
// # Keys
//
// The DelegatedSigner derives each account's Ed25519 devnet key per chain
// from one master seed it holds (HMAC-SHA256, domain-separated), so nothing
// secret is stored and a restart with the same seed recovers every key. Only
// public keys are registered (delegated_signer_keys). No method returns,
// logs or formats a private key or seed; every signer formats as its public
// key or its name.
//
// # What this package must never do
//
//   - Let an agent sign, see a key, apply an effect, post to the ledger,
//     approve a withdrawal, or create, widen, extend or re-target a
//     capability.
//   - Sign anything the inspector has not proved is exactly the persisted
//     plan, sign twice for one swap or withdrawal, or trust any caller input
//     beyond identifiers and the bytes to be signed.
//   - Let a capability act across an account boundary, outside its scope,
//     after its expiry or revocation, or beyond any of its limits.
//   - Substitute a destination or change an amount after approval.
//   - Resubmit a transaction whose outcome is unknown, or post before the
//     chain has finalized what the posting records.
//   - Move or represent real value: every asset is SIMULATED and every
//     posting is a simulated-only kind.
//   - Keep package-level mutable state, read the wall clock, or use floating
//     point.
//   - Sign, submit, observe or settle a swap or withdrawal of another devnet
//     network than its host's (every one records the network it was admitted
//     on, 00760), or let a subject the sweep cannot handle stop the others or
//     be taken again before its backoff has passed.
package delegation
