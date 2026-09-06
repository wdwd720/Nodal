// Package privy is the WalletProvider / SigningProvider adapter for Privy
// server-delegated embedded Solana wallets (docs/api/providers/privy.md; goal
// PARTS 96, 97, 208). It is built on the pinned github.com/privy-io/go-sdk
// v0.15.0 and uses the names that module actually exports (verified with
// `go doc`, not the docs site):
//
//	privyclient.NewPrivyClient(PrivyClientOptions{AppID, AppSecret, APIUrl, HTTPClient, RequestExpiry})
//	client.Wallets.New / .Get                     (generated WalletService, POST/GET /v1/wallets)
//	client.Wallets.Solana.SignTransactionBytes    (POST /v1/wallets/{id}/rpc method signTransaction)
//	client.Policies.Get                           (GET /v1/policies/{id})
//	privyclient.WithIdempotencyKey / WithAuthorizationContext / WithRequestOptions
//	authorization.AuthorizationContext{PrivateKeys | Signers}
//	option.WithMaxRetries(0), option.WithRequestTimeout
//
// # Responsibility
//
//   - CreateWallet: POST /v1/wallets with chain_type solana, the platform key
//     quorum as owner, the configured policy attached, external_id = account
//     id and an idempotency key derived from (account, chain).
//   - GetWallet / VerifyDelegation: read the wallet and its policy and verify
//     that delegated signing is set up exactly as configured: our signer is
//     the owner or an additional signer, the configured policy is attached,
//     its rules allow signTransaction only for the configured programId
//     allow-list and deny everything else (no signAndSend, no export).
//   - Capabilities: the PART 96 probe — the same policy verification without
//     a wallet. VerificationLabel is CODE_COMPLETE: no live app id has been
//     exercised in this build (BLOCKED_EXTERNAL EB-005 for sandbox/canary).
//   - SignTransaction: signTransaction under privy-idempotency-key =
//     the execution attempt id; the signed bytes are checked to carry the
//     identical message and a valid signature by the wallet before they are
//     returned.
//
// # Retry classification (PART 106) and what was found
//
// The SDK forwards the idempotency key as the privy-idempotency-key header;
// privy.md documents a 24 h replay window for /rpc, including cached 5xx
// responses. The SDK itself guarantees nothing about idempotency and the
// error body format is undocumented, so SignTransaction is classified
// UNKNOWN_EFFECT_WRITE: the signing service never retries it blindly; it
// re-checks its own signing_results row under the attempt lock first.
// CreateWallet is IDEMPOTENT_WRITE (keyed); reads are SAFE_RETRY. SDK-side
// automatic retries are disabled (WithMaxRetries(0)) so retry policy lives
// in one place.
//
// # Known provider limitation
//
// Privy policy evaluation cannot resolve address-lookup-table accounts, so
// the provider policy only bounds programId on static keys. The platform's
// own inspector (internal/signing/inspect) is the authority; the provider
// policy is defense in depth.
//
// # What this package must never do
//
//   - Sign without the platform inspector: only internal/signing calls
//     SignTransaction.
//   - Log or return the app secret, the authorization private key, request
//     bodies or raw error bodies (errors carry a status and a code only).
//   - Retry a signing request on its own.
//   - Be constructed with ProviderMode fake, or without TLS in STAGING/PROD.
//   - Let Privy SDK types escape this package.
package privy
