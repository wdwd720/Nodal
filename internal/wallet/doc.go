// Package wallet is the wallet and signing provider abstraction of the
// execution stack (goal PART 96; EXECUTION.md §3).
//
// # Responsibility
//
//   - Wallet mirrors the wallets table: an account's provider-managed,
//     delegated embedded wallet with its lifecycle status (ACTIVE, SUSPENDED,
//     REVOKED), delegation evidence (delegation_ref, delegation_verified_at,
//     signing_policy_version) and the provider's capability probe.
//   - WalletProvider (create, get, verify delegation, capability probe) and
//     SigningProvider (sign an already inspected transaction under an
//     idempotency key) are the only two interfaces the platform assumes of a
//     provider. No security semantics are assumed beyond what the provider
//     verifiably offers: Capability.DelegatedSigning must be VERIFIED before
//     production signing starts (fail closed, enforced by internal/signing).
//   - Repository persists wallets. Status changes require a reason and a
//     non-agent actor and always write a wallet_status_transitions row in the
//     same transaction (migration 00615 binds the two with the 00603 trigger
//     mechanism, so a bare status update is refused at COMMIT).
//
// # What this package must never do
//
//   - Sign anything without going through internal/signing: SigningProvider
//     is consumed only by the signing service after inspection.
//   - Hold, log or expose key material, provider secrets, authorization keys
//     or signing tokens; the provider adapters resolve secrets themselves.
//   - Let an AGENT actor create, suspend, revoke or re-delegate a wallet.
//   - Use a wallet address or provider wallet id as primary identity.
//   - Be imported by internal/agent or internal/strategy (depguard, lintfin).
package wallet
