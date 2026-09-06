# Privy contract tests

`contract_test.go` drives `internal/provider/privy` against an `httptest`
server that emulates the Privy REST API (`docs/api/providers/privy.md`,
verified 2026-09-05) for the endpoints the adapter uses:

| Endpoint | Method | Used by |
|---|---|---|
| `/v1/wallets` | POST | `CreateWallet` |
| `/v1/wallets/{id}` | GET | `GetWallet`, `VerifyDelegation` |
| `/v1/wallets/{id}/rpc` | POST `signTransaction` | `SignTransaction` |
| `/v1/policies/{id}` | GET | `VerifyDelegation`, `Capabilities` |

Cases covered: valid, invalid (400), timeout, rate limit (429 + Retry-After),
5xx, unexpected state (response `method` mismatch, message altered by the
provider), missing required field (`signed_transaction`, `address`,
`id`), policy violation, wallet lookups that 404, credentials rejected
(401), and delegation verification against attached/unattached policies.

## What is documented vs. assumed

Documented on the fetched official pages (privy.md "Verification record"):

- Request headers `Authorization: Basic base64(app_id:app_secret)` and
  `privy-app-id`; `privy-idempotency-key` and `privy-request-expiry`
  semantics; `privy-authorization-signature` requirement for owner-gated
  wallet RPC.
- Wallet object fields (`id`, `address`, `chain_type`, `policy_ids`,
  `owner_id`, `additional_signers`, `created_at` ms, `archived_at`).
- Policy object shape (`version`, `chain_type`, `name`, `rules[]` with
  `method`/`action`/`conditions[]` of `field_source`/`field`/`operator`/
  `value`) and the evaluation semantics (every instruction must ALLOW,
  DENY wins, default DENY).
- `signTransaction` request `{method, params:{transaction, encoding:"base64"}}`
  and response `{method:"signTransaction", data:{signed_transaction, encoding}}`.
- 429 on rate limit; 24 h idempotency window for `/rpc`; `POLICY_VIOLATION`
  removes the idempotency record.

Assumed (marked UNVERIFIED in privy.md and reproduced here as plausible
shapes only):

- Error body format. The fake returns `{"error": "...", "code": "..."}`;
  the adapter only extracts a machine code from `code`/`error_code`/`error`
  and never surfaces the body.
- The `Retry-After` header on 429 (standard HTTP; not shown on the Privy
  pages).
- The exact bytes of `privy-authorization-signature`: the SDK computes it
  from an undocumented canonicalization, so the fake only asserts the header
  is present on `/rpc` and non-empty.
- Optional-account and lookup-table behavior of policy evaluation on real
  Jupiter v0 transactions (documented as unsupported); the fake does not
  evaluate policies at all — the platform inspector is authoritative.

Nothing here has been exercised against a live Privy app id
(`VerificationLabel` stays `CODE_COMPLETE`; EB-005 tracks sandbox and canary
verification).
