# Stripe fiat-to-crypto onramp — verified integration notes

Role in this platform: **FundingProvider** (fiat -> USDC/SOL on Solana into a user wallet). This is the crypto onramp product only, not card payments.
Overall status: **VERIFIED_FROM_OFFICIAL_DOCS** for API surface, statuses, webhook event, signature verification; **PARTIAL** for Go SDK support (no stable package found) and status enum completeness.

## Verification record

All fetched 2026-09-05.

| Source | URL | Result |
|---|---|---|
| Onramp overview / availability | https://docs.stripe.com/crypto/onramp | OK |
| Stripe-hosted guide (currencies, regions, session mint) | https://docs.stripe.com/crypto/onramp/standalone-onramp-guide | OK |
| Embedded guide (states, webhook, quotes, errors, sandbox) | https://docs.stripe.com/crypto/onramp/embedded | OK |
| Session object | https://docs.stripe.com/api/crypto/onramp_sessions/object | OK |
| Create session | https://docs.stripe.com/api/crypto/onramp_sessions/create | OK |
| Endpoint index | https://docs.stripe.com/api/crypto/onramp_sessions | OK |
| Checkout (headless) | https://docs.stripe.com/api/crypto/onramp_sessions/checkout | OK |
| Refresh executable quote | https://docs.stripe.com/api/crypto/onramp_sessions/quote | OK |
| Quotes | https://docs.stripe.com/api/crypto/onramp_quotes/retrieve | OK |
| Beta -> public upgrade notes | https://docs.stripe.com/crypto/onramp/upgrade-onramp-integration | OK |
| Webhooks + signature verification | https://docs.stripe.com/webhooks | OK |
| Snapshot event types | https://docs.stripe.com/api/events/types | OK — **no `crypto.*` events listed** |
| Thin event types | https://docs.stripe.com/api/v2/core/events/event-types | OK — none |
| stripe-go CHANGELOG (top) | https://raw.githubusercontent.com/stripe/stripe-go/master/CHANGELOG.md | OK: newest `86.4.1 - 2026-09-01`; no `CryptoOnrampSession` entry |
| stripe-go releases | https://github.com/stripe/stripe-go/releases | OK: only `v86.x-alpha` notes mention `CryptoOnrampSessionTransactionDetails` |
| pkg.go.dev | https://pkg.go.dev/search?q=cryptoonrampsession ; https://pkg.go.dev/github.com/stripe/stripe-go/v86/cryptoonrampsession | 0 results / 404 |
| Failed | https://docs.stripe.com/crypto/onramp/api-onramp , /crypto/onramp/quotes-api , /crypto/using-the-api , raw crypto_onrampsession.go | 404 |

## Availability status (as documented)

- Product is live and "**Public preview**" (all three integrations). Access requires submitting an onramp application (reviewed "within 48 hours"); **testing environments are also gated** by approval.
- No deprecation notice on any fetched page. The beta->public API change is dated 2023-06-21 (renamed fields; see below).
- Regions: Stripe-hosted "available in the US and EU"; embedded "only available in the EU and the US (excluding Hawaii)". Footnotes: **USDC (Solana) is not supported in the EU**; XLM/USDC-Stellar/Avalanche/Polygon not in New York. `customer_ip_address` pre-check returns 400 `crypto_onramp_unsupportable_customer` / `crypto_onramp_unsupported_country`.
- Reversibility/chargeback model verbatim: "Stripe acts as the merchant of record for these onramp transactions and assumes full liability for all fraud and disputes. We also handle all regulatory requirements, KYC verifications, and sanctions screening." The platform never touches fiat; the on-chain delivery is irreversible; there is no documented refund/reversal endpoint for a delivered session. `settlement_speed`: `instant` (default, "crypto is delivered when payment is confirmed") vs `standard` ("when payment settles").
- Stripe may disable session creation during fraud events: 400 `crypto_onramp_disabled`.

## Auth and secrets

Standard Stripe secret key via HTTP basic auth (`-u sk_...:`) on `https://api.stripe.com`; publishable key only for the browser widget (`StripeOnramp(pk_...)`). `client_secret` (`cos_..._secret_...`) drives one session — "Don't log it, embed it in URLs, or expose it to anyone other than the customer." Legacy beta accounts select behaviour via `Stripe-Version: <date>;crypto_onramp_beta=v2` (current accounts do not need it). Webhook signing secret `whsec_...` per endpoint (different for test/live).

## Endpoints and schemas

Object name: `crypto.onramp_session`, id prefix `cos_`. **All monetary amounts are decimal strings** (e.g. `"0.123400000000000000"`, `"100.00"`); never parse as float.

| Endpoint | Purpose |
|---|---|
| `POST /v1/crypto/onramp_sessions` | Create session (one per customer visit) |
| `GET /v1/crypto/onramp_sessions/:id` | Retrieve |
| `GET /v1/crypto/onramp_sessions` | List |
| `POST /v1/crypto/onramp_sessions/:id/quote` | "Refreshes an executable quote" (headless) -> status `quote_ready` |
| `POST /v1/crypto/onramp_sessions/:id/checkout` | "Completes a headless CryptoOnrampSession ... confirm the payment and execute the quote"; param `mandate_data` |
| `GET /v1/crypto/onramp_quotes` | Non-binding estimate quotes (the embedded guide still shows `/v1/crypto/onramp/quotes`; the upgrade page says the path changed to `/v1/crypto/onramp_quotes` — use the latter) |

Create parameters (all optional; form-encoded):

| Param | Type | Notes |
|---|---|---|
| wallet_addresses[<network>] | object of strings | keys per network (`solana`, `ethereum`, `bitcoin`, `polygon`, `stellar`, ...; `destination_tags` for tag networks); requires `destination_network(s)`; Stripe validates the address |
| lock_wallet_address | boolean | locks suggested address (400 `crypto_onramp_no_wallet_address_to_lock` if none given) |
| destination_networks[] / destination_network | enum | `avalanche, base, bitcoin, celo, ethereum, optimism, polygon, solana, stellar, sui, tempo, worldchain`; users "cannot override" the list |
| destination_currencies[] / destination_currency | enum | `avax, btc, eth, matic, sol, usdc, usdt, wld, xlm` |
| source_currency | enum | `usd`, `eur` |
| source_amount | string | fiat decimal; "We don't support fractional pennies"; mutually exclusive with destination_amount |
| destination_amount | string | crypto decimal at full precision; requires destination_currency + destination_network |
| customer_ip_address | string | IPv4/IPv6 geo pre-check |
| customer_information{email, first_name, last_name, dob{year,month,day}, address{country,line1,line2,city,state,postal_code}} | object | KYC prefill (SSN cannot be prefilled); create page labels this `kyc_details` |
| settlement_speed | enum | `instant` (default) / `standard` |
| metadata | map | |

Response object: `id`, `object`, `client_secret`, `created` (unix s), `kyc_details_provided` (bool), `livemode`, `metadata`, `redirect_url` (hosted flow), `status`, `transaction_details{destination_amount, destination_currency, destination_network, destination_currencies[], destination_networks[], fees{network_fee_monetary, transaction_fee_monetary} (strings), lock_wallet_address, source_amount, source_currency, transaction_id (e.g. `cxt_...`, set once fulfilled), wallet_address, wallet_addresses{...}}`.

Status values (embedded guide, verbatim): `initialized` ("newly minted ... customer hasn't used it yet"); `rejected` ("KYC failure, sanctions screening issues, fraud checks"); `requires_payment` ("completed onboarding or sign-in and gets to the payment page. If they attempt payment and fail, they stay in this state"); `fulfillment_processing` ("successfully completed payment. We haven't delivered the crypto"); `fulfillment_complete` ("we have confirmed delivery"). The object reference lists exactly these five, **but** the `/quote` example response returns `"status": "quote_ready"` — treat the enum as open and map unknown values to a review state.

Quotes response: `id`, `object: "crypto.onramp.quotes"`, `rate_fetched_at` (float unix), `livemode`, `source_currency`, `source_amount`, `destination_network_quotes{<network>: [{id, destination_currency, destination_amount, destination_network, fees{network_fee_monetary, transaction_fee_monetary}, source_total_amount}]}`. Estimates only; "customer might see a slightly different quote in the onramp widget".

Field renames since beta (2023-06-21): `supported_destination_currencies`->`destination_currencies`, `supported_destination_networks`->`destination_networks`, `source_exchange_amount`->`source_amount`, `destination_exchange_amount`->`destination_amount`.

## Webhooks/streams

- Event type: **`crypto.onramp_session.updated`** ("every time the status of an onramp session changes post creation. We don't send an event when a new session is created"). `data.object` is the full `crypto.onramp_session`. Note: this type is absent from the public snapshot and thin event lists; select it in the Dashboard/event-destination for the onramp-enabled account.
- Front-end events: `onramp_ui_loaded`, `onramp_session_updated`, `onramp_ui_modal_opened/closed` — UI only, never authoritative.
- Signature verification (verbatim): header `Stripe-Signature: t=<ts>,v1=<hex>,v0=<test>`; `signed_payload = timestamp + "." + raw body`; HMAC-SHA256 with the endpoint secret; "ignore all schemes that aren't v1"; constant-time compare; "default tolerance of 5 minutes" (300 s) — "Don't use a tolerance value of 0"; multiple `v1` signatures during secret rolling (<=24 h). "Stripe requires the raw body of the request".
- Delivery: retries "for up to three days with an exponential back off in live mode" (sandbox: 3 tries over a few hours); "doesn't guarantee the delivery of events in the order"; duplicates possible — dedupe on `event.id`; return 2xx fast; TLS 1.2+; up to 16 endpoints. Because ordering is not guaranteed, always re-fetch the session (`GET .../:id`) and apply status monotonically.

## Errors, rate limits, retries

Error body `{error:{type, code, message}}`. Documented codes on create: `crypto_onramp_disabled`, `crypto_onramp_unsupported_country`, `crypto_onramp_unsupportable_customer`, `customer_ip_address`, `crypto_onramp_invalid_source_destination_pair`, `crypto_onramp_incomplete_destination_currency_and_network_pair`, `crypto_onramp_invalid_destination_currency_and_network_pair`, `crypto_onramp_missing_source_currency`, `crypto_onramp_invalid_source_amount`, `crypto_onramp_missing_destination_currency`, `crypto_onramp_invalid_destination_amount`, `crypto_onramp_invalid_destination_currencies_and_networks`, `crypto_onramp_conflicting_destination_currency`, `crypto_onramp_conflicting_destination_network`, `crypto_onramp_wallet_addresses_not_all_networks_supported`, `crypto_onramp_no_wallet_address_to_lock`, `crypto_onramp_merchant_not_properly_setup`. Onramp-specific rate limits: not documented (Stripe-wide limits apply). Stripe-wide `Idempotency-Key` header on POST (https://docs.stripe.com/api/idempotent_requests) was **not re-fetched in this pass**.

| Endpoint | Class | Rationale |
|---|---|---|
| POST /v1/crypto/onramp_sessions | IDEMPOTENT_WRITE with `Idempotency-Key`; otherwise UNKNOWN_EFFECT_WRITE | Duplicate sessions are harmless but orphan client_secrets; always send a key. |
| POST .../:id/quote | SAFE_RETRY | Replaces the executable quote; no money movement. |
| POST .../:id/checkout | UNKNOWN_EFFECT_WRITE | Confirms payment and executes the quote; retry only after `GET` shows status unchanged and with the same `Idempotency-Key`. |
| GET session / list / quotes | SAFE_RETRY | |

## Sandbox/test availability

Sandbox available only **after application approval**. Sandbox values: OTP `000000`; SSN `000000000`; address line 1 `address_full_match`; card `4242424242424242`; "Sandbox transaction amounts are overridden by our pre-decided limits." Webhooks testable with `stripe listen --forward-to`.

## Go SDK

- Module `github.com/stripe/stripe-go/v86`, latest **v86.4.1 (2026-09-01)** per CHANGELOG.
- Docs (embedded guide) state verbatim: "Our official libraries don't contain built-in support for the API endpoints because the Onramp API is in public preview."
- Release notes for `v86.x.0-alpha.*` mention `CryptoOnrampSessionTransactionDetails` enums (usdt, celo, tempo) and a search snippet claims `CryptoOnrampSession` with `Checkout/Get/List/New/Quote` — but pkg.go.dev returns **0 packages** for `cryptoonrampsession` and `/v86/cryptoonrampsession` 404s. **Conclusion: stable stripe-go has no onramp types (UNVERIFIED in alpha only).** Implement with `stripe.BackendRaw`/`RawRequest` or plain `net/http` + own structs; use `webhook.ConstructEvent` from stripe-go for signature verification (generic, version-independent).

## Worked flow (Solana USDC top-up, from the official examples)

1. Server: `POST /v1/crypto/onramp_sessions` with `-H "Idempotency-Key: <uuid>"`, `wallet_addresses[solana]=<user wallet>`, `destination_networks[]=solana`, `destination_currencies[]=usdc`, `destination_network=solana`, `destination_currency=usdc`, optional `destination_amount=<decimal string>`, `lock_wallet_address=true`, `customer_ip_address=<ip>`, `metadata[account_id]=<internal id>`. Persist `id`, `status`, `created`; never persist `client_secret` beyond the session's lifetime.
2. Client: mount with `client_secret` (embedded) or redirect to `redirect_url` (hosted).
3. Server: on `crypto.onramp_session.updated`, verify `Stripe-Signature` (v1, 300 s tolerance, raw body), dedupe on `event.id`, then `GET /v1/crypto/onramp_sessions/:id` and apply the status transition only if it advances (`initialized` -> `requires_payment` -> `fulfillment_processing` -> `fulfillment_complete`, or -> `rejected`).
4. On `fulfillment_complete`, read `transaction_details.transaction_id`, `destination_amount` (string), `destination_currency`, `destination_network`, `wallet_address`; reconcile against the on-chain USDC transfer observed by the chain observer (Stripe does not return the Solana signature in the documented object — match on amount + wallet + time window).
5. Poll `GET` as a fallback when no webhook arrives within the expected window; sessions stuck in `initialized`/`requires_payment` are user-abandoned, not errors.

Example fulfilled response fields (from the checkout example): `"status": "fulfillment_processing"`, `"source_amount": "100.00"`, `"destination_amount": "0.029133919178255537"`, `"fees": {"network_fee_monetary": "0.07", "transaction_fee_monetary": "4.04"}`, `"transaction_id": "cxt_1ABC2DEF3ghi4jkl5"`.

## Open questions / unverified

1. Whether `quote_ready` (and other headless statuses) will be added to the documented enum.
2. Whether the alpha stripe-go onramp types will graduate to a stable release.
3. Exact Solana network-fee model and USDC decimals in `destination_amount` (examples show 6-dp strings for USDC).
4. Session expiry/TTL for `initialized` sessions is undocumented.
5. Whether `crypto.onramp_session.updated` requires explicit opt-in selection ("Selection required" flag not visible).
