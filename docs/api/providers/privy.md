# Privy (embedded wallets, server-side / delegated signing) — verified integration notes

Role in this platform: **WalletProvider + SigningProvider** (user embedded Solana wallets, server-delegated signing under policy).
Overall status: **PARTIAL** — endpoints, policy schema, idempotency, expiry, signers, Go SDK existence are VERIFIED; the exact `privy-authorization-signature` payload/canonicalization spec and numeric rate limits are **UNVERIFIED** (pages fetched did not contain them).

## Verification record

All fetched 2026-09-05.

| Source | URL | Result |
|---|---|---|
| Docs index | https://docs.privy.io/llms.txt ; https://docs.privy.io/_llms/api-reference.md | OK |
| REST auth | https://docs.privy.io/api-reference/introduction ; https://docs.privy.io/basics/rest-api/setup.md | OK |
| Create wallet | https://docs.privy.io/api-reference/wallets/create | OK |
| Update wallet | https://docs.privy.io/api-reference/wallets/update.md | OK |
| Solana signTransaction (API ref) | https://docs.privy.io/api-reference/wallets/solana/sign-transaction.md | OK |
| Solana signAndSendTransaction (API ref) | https://docs.privy.io/api-reference/wallets/solana/sign-and-send-transaction.md | OK |
| Solana guides | https://docs.privy.io/wallets/using-wallets/solana/sign-a-transaction.md ; .../send-a-transaction.md | OK |
| Idempotency keys | https://docs.privy.io/api-reference/idempotency-keys.md | OK |
| Request expiry | https://docs.privy.io/api-reference/request-expiry.md | OK |
| Authorization signatures | https://docs.privy.io/api-reference/authorization-signatures.md | OK but **no payload spec** |
| Authorization keys | https://docs.privy.io/controls/authorization-keys/keys/create/key.md | OK |
| Key quorums | https://docs.privy.io/controls/key-quorum/overview.md ; .../key-quorum/sign.md | OK (no payload spec) |
| Policies | https://docs.privy.io/controls/policies/overview ; .../policies/create-a-policy.md ; .../policies/example-policies/solana.md | OK |
| Signers (delegation) | https://docs.privy.io/wallets/using-wallets/signers/overview ; .../signers/add-signers.md ; .../signers/use-signers.md | OK |
| Security architecture | https://docs.privy.io/security/wallet-infrastructure/architecture | OK |
| Webhooks | https://docs.privy.io/api-reference/webhooks/overview.md | OK |
| Go SDK | https://docs.privy.io/basics/go/installation.md ; .../go/setup.md ; https://pkg.go.dev/github.com/privy-io/go-sdk | OK |
| Failed | https://docs.privy.io/api-reference/wallets/rpc , https://docs.privy.io/controls/authorization-keys/overview (no key details), https://docs.privy.io/wallets/using-wallets/session-signers/overview (404) | |

## Auth and secrets

- Base URL `https://api.privy.io` (HTTPS only). Some user endpoints in guides use `https://auth.privy.io/api/v1/users/<did>` — both hosts appear in official docs.
- Every request: `Authorization: Basic base64(app_id:app_secret)` **and** `privy-app-id: <app_id>`; "Requests missing either of these headers will be rejected".
- `privy-authorization-signature: <sig>[,<sig>...]` — required for `PATCH`/`DELETE` on wallets/policies that have an `owner_id`, and for `POST /v1/wallets/{id}/rpc` "when owner is set". Multiple signatures comma-delimited.
- `privy-idempotency-key: <up to 256 chars, V4 UUID recommended>`; `privy-request-expiry: <unix ms>`.
- Authorization keys: "P-256 public-private keypairs"; generated in dashboard (on-device), NodeJS `generateP256KeyPair()` (DER), or `openssl ecparam -name prime256v1 -genkey`; register the **base64 DER public key**; returned `id` is used as `owner_id` / `signer_id`. Provider claim: "Neither Privy nor the secure enclave ever sees the private key". Keep the private key in a KMS/HSM; the Go SDK's `AuthorizationContext` supports external signers.

## Endpoints and schemas

### POST /v1/wallets — create
Body: `chain_type` (required; `solana`, `ethereum`, ... 16 values), `owner` `{user_id}` | `{public_key}` **or** `owner_id` (key-quorum id), `policy_ids[]` (**max 1**), `additional_signers[{signer_id, override_policy_ids[]}]`, `display_name` (<=100), `external_id` (<=64, `^[a-zA-Z0-9_-]+$`, write-once), `entity{id, type: user|organization}`. Response: `id`, `address`, `chain_type`, `policy_ids`, `owner_id`, `public_key`, `created_at` (unix **ms** number), `exported_at`, `imported_at`, `display_name`, `external_id`, `entity`, `additional_signers`, `authorization_threshold`, `archived_at`.

### PATCH /v1/wallets/{wallet_id} — update (owner signature required)
Body: `policy_ids[]` (max 1; 24-char ids), `additional_signers[{signer_id, override_policy_ids[] (max 1)}]`, `owner` | `owner_id`, `display_name`. This is the server-side path to attach a delegated signer.

### POST /v1/wallets/{wallet_id}/rpc — Solana signing
Headers: Basic auth, `privy-app-id`, optional `privy-authorization-signature`, `privy-idempotency-key`, `privy-request-expiry`.

| method | body | response `data` |
|---|---|---|
| `signTransaction` | `{method:"signTransaction", params:{transaction:"<base64 serialized tx>", encoding:"base64"}}` | `{signed_transaction: base64, encoding:"base64"}` |
| `signAndSendTransaction` | `{method:"signAndSendTransaction", caip2, params:{transaction, encoding:"base64"}, sponsor?: bool, reference_id?: <=64 chars, optimistic_broadcast?: bool}` | `{hash: signature, signed_transaction, caip2, transaction_id?, reference_id?}` |
| `signMessage` | listed in policy methods | not fetched |

`caip2`: mainnet `solana:5eykt4UsFv8P8NJdTREpY1vzqKqZKvdp`, devnet `solana:EtWTRABZaYq6iMfeYKouRu166VU2xqa1`, testnet `solana:4uhcVJyU9pJkvQyS88uRDiswHXSCkY3z`. Semantics verbatim: "a successful response indicates that the transaction has been broadcasted to the network" — "Transactions may still fail confirmation." `optimistic_broadcast` "skips network preflight checks; you're responsible for validation and rebroadcasting". Some external wallets "only support signing" and return a raw 64-byte signature instead of a full tx. Whether a partially-signed multi-signer tx (e.g. Jupiter RFQ) is accepted is **UNVERIFIED**.

### Policies — POST /v1/policies (+ attach via `policy_ids`)
Schema: `version: "1.0"`, `name`, `chain_type` (`ethereum|solana|tron|sui|xrpl`), `rules[]`, optional `owner{public_key}` | `owner_id` (changes then require owner signature). Rule: `{name, method, action: ALLOW|DENY, conditions[]}`; condition `{field_source, field, operator, value}`. Operators: `eq, neq, lt, lte, gt, gte, in, in_condition_set, contains, starts_with, ends_with`. Methods: `signTransaction`, `signAndSendTransaction`, `signMessage`, `*`.

Solana field sources (verbatim table):

| field_source | fields |
|---|---|
| `solana_system_program_instruction` | `instructionName`, `Transfer.to`, `Transfer.from`, `Transfer.lamports` |
| `solana_token_program_instruction` | `instructionName`, `Transfer.source`, `Transfer.destination`, `Transfer.amount`, `Burn.amount`, `MintTo.amount` (examples also use `TransferChecked.mint`, `TransferChecked.amount`) |
| `solana_program_instruction` | `programId` |

Evaluation (verbatim): "every Instruction in a Solana transaction is evaluated against the rules of the policy. Every instruction must evaluate to an ALLOW action"; "DENY actions take precedence over ALLOW"; "If no rules resolve, the policy will default to DENY". Recommended shape: allow-list `programId in [ComputeBudget, Token, Token-2022, ATA, Jupiter v6, System]`, cap `Transfer.lamports lte N`, restrict SPL `TransferChecked.mint in [USDC]` and `destination in [...]`, final `{method:"*", conditions:[], action:"DENY"}`. **Critical limitation (verbatim): "Policy evaluation does not support resolving addresses from Address Lookup Tables."** Jupiter v0 transactions use ALTs, so account-based allow-lists on swap legs may fail closed; `programId` allow-listing on static keys works. CPI/inner-instruction evaluation, Token-2022 extension awareness, and amount limits per-window (rate/velocity) are **not documented**.

### Signers / delegated actions
Client-side consent: user calls `addSigners({address, signers:[{signerId: <key quorum id>, policyIds?: [id]}]})` (React/RN/Swift/Kotlin/Flutter) — the user must be authenticated in-app; "each signer can only have one override policy". Server-side: PATCH wallet `additional_signers` signed by the owner. Using it: call the wallet `rpc` endpoint with `privy-authorization-signature` produced by the signer's key; the user object marks such wallets `delegated: true` (`GET /api/v1/users/<did>` on `auth.privy.io`). Revocation/expiry: not on fetched pages (UNVERIFIED); key quorums (m-of-n, one level of nesting, thresholds "enforced within Privy's TEE infrastructure") can gate sensitive operations.

## Errors, rate limits, retries

- 429 on rate limit; guidance "implement retry logic with exponential backoff". Only published number: wallet pregeneration 240 users/min. **General RPS limits UNVERIFIED** — ask the vendor.
- Idempotency (verbatim): supported on POST `/wallets`, `/rpc`, `/transfer`, `/swap`, `/earn`, import; window **24 hours**; "returns the stored response without re-executing"; **RPC endpoints cache all responses including 5xx for 24 h** (wallet actions drop 5xx so they can be retried); `POLICY_VIOLATION` deletes the record; same key + different body -> 400.
- Request expiry: `privy-request-expiry` unix ms; defaults 15 min (standard) / 72 h (intents); past deadline -> `request_expired` error. Include it in every signed request.
- Error body format: not documented on fetched pages (UNVERIFIED).

| Endpoint | Class | Rationale |
|---|---|---|
| POST /v1/wallets | IDEMPOTENT_WRITE (with key) | 24 h replay. |
| POST rpc `signTransaction` | SAFE_RETRY (no key) / IDEMPOTENT_WRITE (key) | Signing has no chain effect; but a cached 5xx under a key blocks retries — use a fresh key to retry. |
| POST rpc `signAndSendTransaction` | IDEMPOTENT_WRITE (key, mandatory) ; UNKNOWN_EFFECT_WRITE without key | Broadcasts; without a key a timeout is unresolvable except by chain lookup of the expected signature. |
| PATCH /v1/wallets/{id} | IDEMPOTENT_WRITE | Content-idempotent; needs owner signature + expiry. |
| POST /v1/policies | UNKNOWN_EFFECT_WRITE | Not in the idempotency list; retries create duplicate policies. |

## Webhooks/streams

Delivered via **Svix**: headers `svix-id`, `svix-timestamp`, `svix-signature`; verify with SDK (`webhooks().verify()`) or Svix rules. Events include `transaction.broadcasted`, `transaction.confirmed`, `transaction.failed`, `transaction.execution_reverted`, `transaction.replaced`, `transaction.still_pending`, `transaction.provider_error`, `wallet.funds_deposited`, `wallet.funds_withdrawn`, `wallet.private_key_export`, `wallet.archived/restored`. "At least once"; retry schedule "Immediately, 5 seconds, 5 minutes, 30 minutes, 2 hours, 5 hours, 10 hours, 10 hours"; endpoint disabled after 5 days of failures; dedupe on `idempotency_key`. Treat as advisory; confirm on chain.

## Sandbox/test availability

Solana devnet/testnet via `caip2`; no separate Privy sandbox documented — use a dedicated dev app id. Test policies on devnet first (policy evaluation is chain-agnostic).

## Key management claims (provider claims, not verified security semantics)

AWS Nitro Enclaves TEE; keys split with Shamir 2-of-2 (enclave share + auth share); "Only the TEE can decrypt the enclave share and combine it with the auth share to temporarily reconstitute the wallet"; authorization signature "verified against the authorization public key" inside the TEE; SSS library "heavily audited". No SOC2/ISO statement on the fetched page; no explicit "Privy cannot unilaterally sign" statement.

## Go SDK

`go get github.com/privy-io/go-sdk` — **v0.15.0 (2026-07-27), Apache-2.0**, docs say Go 1.23+. Docs show `privy.NewPrivyClient(privy.PrivyClientOptions{AppID, AppSecret})` with `authorization.AuthorizationContext{PrivateKeys, UserJwts}`; pkg.go.dev shows `NewClient(opts)`, `NewEmbeddedWalletService`, `NewIntentService` (`Rpc()`, `Transfer()`), `NewKeyQuorumService` — naming differs between docs and package index; pin the version and verify against `go doc`. SDK computes the authorization signature for you — prefer it over hand-rolling the (undocumented) canonicalization.

## Example policy (shape from the official Solana examples; addresses are placeholders)

```json
{
  "version": "1.0",
  "name": "delegated-swap-wallet",
  "chain_type": "solana",
  "rules": [
    {"name": "allow-known-programs", "method": "signAndSendTransaction", "action": "ALLOW",
     "conditions": [{"field_source": "solana_program_instruction", "field": "programId", "operator": "in",
                     "value": ["ComputeBudget111111111111111111111111111111", "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA",
                               "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb", "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL",
                               "JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4", "11111111111111111111111111111111"]}]},
    {"name": "cap-sol-transfer", "method": "signAndSendTransaction", "action": "DENY",
     "conditions": [{"field_source": "solana_system_program_instruction", "field": "Transfer.lamports", "operator": "gt", "value": "1000000000"}]},
    {"name": "usdc-only-token-moves", "method": "signAndSendTransaction", "action": "DENY",
     "conditions": [{"field_source": "solana_token_program_instruction", "field": "TransferChecked.mint", "operator": "neq", "value": "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"}]},
    {"name": "deny-everything-else", "method": "*", "conditions": [], "action": "DENY"}
  ]
}
```

Notes: every instruction must resolve to ALLOW, so the program allow-list must include every program a Jupiter route touches (wrapped-SOL setup, ATA creation, compute budget) — Jupiter routes CPI into many DEX programs from the aggregator, but only top-level `programId`s are documented as evaluated (inner-instruction handling is UNVERIFIED). The `ComputeBudget111...` and System (`1111...`) IDs above are well-known but were not printed on the pages fetched this pass — confirm before shipping. Test the policy on devnet with a real Jupiter-shaped v0 transaction because ALT-resolved accounts are invisible to conditions.

## Open questions / unverified

1. Exact authorization-signature payload/canonicalization/encoding spec (needed for KMS-backed signing outside the SDK).
2. Numeric API rate limits; error body schema.
3. ALT support in policy evaluation (currently unsupported) — blocks account allow-lists on Jupiter v0 txs.
4. Signer expiry/revocation API; partial-signature handling for multi-signer txs.
5. Whether `signMessage` policies can gate Jupiter RFQ/off-chain order signing.
