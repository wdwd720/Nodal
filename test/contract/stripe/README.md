# Stripe fiat-to-crypto onramp — contract suite

Runs the real adapter (`internal/provider/stripe`, plain `net/http`) against
`httptest` fixtures. Source of truth for every field: the verification record in
`docs/api/providers/stripe-crypto-onramp.md` (fetched 2026-09-05). No Stripe SDK is
used because the onramp API is absent from stable `stripe-go`.

Status: **CODE_COMPLETE + CONTRACT_TESTED**. `SANDBOX_VERIFIED` is impossible until
the onramp application is approved (`docs/build/BLOCKERS.md` EB-003: the Stripe
sandbox is application-gated) — the integration is **BLOCKED_EXTERNAL** beyond this
suite.

## Cases (goal PART 148)

| # | Case | Test | Expected adapter behaviour |
|---|---|---|---|
| 1 | valid response | `TestContract_CreateSession_Valid` | documented form params sent with `Idempotency-Key`; object mapped; retry resends the same key |
| 2 | invalid response | `TestContract_InvalidResponse` | HTML, wrong object, truncated, or a JSON **number** amount → `PROVIDER_UNAVAILABLE` `provider_error=malformed_response`; never parsed as float |
| 3 | timeout | `TestContract_Timeout` | bounded by `Options.Timeout` → `PROVIDER_UNAVAILABLE` `provider_error=timeout`; health tracker records the failure |
| 4 | rate limit | `TestContract_RateLimited` | 429 → `RATE_LIMITED` with `RetryAfter` from `Retry-After` |
| 5 | 5xx | `TestContract_ServerError` | `PROVIDER_UNAVAILABLE` |
| 6 | duplicate webhook | `TestContract_DuplicateWebhook` | deterministic identity (`evt_…`); dedupe is the inbox's job (`internal/webhook` `TestProp_DuplicateWebhookOneEffect`) |
| 7 | unknown webhook event | `TestContract_UnknownWebhookEvent` | signed, parsed, `SessionKnown=false` → recorded IGNORED |
| 8 | schema missing required field | `TestContract_MissingRequiredField` | API: `malformed_response`; webhook: `webhook.ErrMalformed` |
| 9 | stale quote | `TestContract_NoQuoteOrCheckoutSurface` | **not applicable**: only the hosted/embedded flow is implemented; `/quote` and `/checkout` are never called |
| 10 | provider unexpected state | `TestContract_UnexpectedStatus` | open enum: `quote_ready` → customer action; unknown → `UNKNOWN` (funding escalates to REVIEW_REQUIRED); livemode flip → `INTERNAL` |
| — | forgery / replay | `TestContract_WebhookForgeryAndReplay` | wrong secret, missing header, stale `t=`, live event in sandbox: all refused |
| — | documented 4xx codes | `TestContract_ClientErrors` | `crypto_onramp_unsupported_country` → `ELIGIBILITY_JURISDICTION`; other `crypto_onramp_*` → `VALIDATION_FAILED`; 401/403 → `INTERNAL`; 404 → `NOT_FOUND` |

## Fixture fields: documented vs assumed

`testdata/session_*.json` mirror the `crypto.onramp_session` object reference.

| Field | Status | Note |
|---|---|---|
| `id` (`cos_` prefix), `client_secret`, `created`, `kyc_details_provided`, `livemode`, `metadata`, `redirect_url`, `status` | **documented** | object reference |
| `transaction_details.{destination_amount, destination_currency, destination_network, destination_currencies, destination_networks, lock_wallet_address, source_amount, source_currency, transaction_id, wallet_address, wallet_addresses}` | **documented** | object reference; amounts are decimal strings |
| `transaction_details.fees.{network_fee_monetary, transaction_fee_monetary}` | **documented** | strings, from the checkout example |
| statuses `initialized`, `rejected`, `requires_payment`, `fulfillment_processing`, `fulfillment_complete` | **documented** | embedded guide |
| status `quote_ready` | **observed, undocumented** | appears in the `/quote` example response only |
| webhook type `crypto.onramp_session.updated`, `data.object` = full session | **documented** | embedded guide (absent from the public event-type lists) |
| `Stripe-Signature` `t=`/`v1=`, HMAC-SHA256 over `t.body`, 300 s tolerance, multiple `v1` | **documented** | webhooks guide |
| `object: "crypto.onramp_session"` echoed on the session | **assumed** | the docs name the object; the JSON `object` value is inferred from Stripe's convention |
| `evt_` id prefix, `object: "event"`, `created`, `livemode` on the event envelope | **assumed** | Stripe-wide event envelope, not restated on the onramp pages |
| `null` (vs absent) for unset `destination_amount`, `transaction_id`, `fees` | **assumed** | either shape parses identically |
| `wallet_addresses` keyed by the `destination_network` value | **assumed** | used only as a fallback when `wallet_address` is absent |
| USDC = 6 decimals, SOL = 9 decimals on Solana | **assumed** (chain facts, not Stripe docs) | `CurrencyDecimals`; anything else is unsupported |
| error envelope `{error:{type, code, message, param}}` | **documented** (type/code/message); `param` assumed | Stripe-wide |
| `Retry-After` on 429 | **assumed** | HTTP convention; onramp rate limits are undocumented |
| `Idempotency-Key` header semantics on create | **documented as Stripe-wide**; not re-fetched for the onramp | treated as IDEMPOTENT_WRITE |

Nothing in the fixtures is invented beyond the rows marked **assumed**, and every
assumed row is either harmless if wrong (parses either way) or fails closed.
