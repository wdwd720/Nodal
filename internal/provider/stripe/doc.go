// Package stripe is the Stripe fiat-to-crypto onramp adapter behind
// funding.FundingProvider (goal PART 29; docs/api/providers/stripe-crypto-onramp.md).
// It is the crypto onramp product only — never generic Stripe card payments,
// whose reversible-card / irreversible-crypto mismatch is exactly the risk
// PART 29 forbids.
//
// The adapter is written with net/http against the verified endpoints:
//
//	POST /v1/crypto/onramp_sessions        create (form-encoded, Idempotency-Key)
//	GET  /v1/crypto/onramp_sessions/:id    retrieve
//
// No Stripe SDK is used: the onramp API is absent from stable stripe-go
// (public preview), so the wire types here are hand-written from the
// official object reference and contain only documented fields. Monetary
// amounts are decimal strings on the wire and stay exact: they are
// validated syntactically here and converted to money.Quantity by the
// funding service at the settlement asset's precision. A JSON number where
// a string is documented is a malformed response, never parsed as a float.
//
// Webhooks: event type crypto.onramp_session.updated, verified from the
// Stripe-Signature header (scheme v1 only, HMAC-SHA256 over
// "<t>.<raw body>", constant-time comparison, 300 s tolerance, several v1
// values accepted while a secret is rolled). Session statuses map to the
// provider-neutral funding.ProviderStatus; the undocumented quote_ready is
// customer-action-required, and anything else is UNKNOWN so the funding
// service routes the deposit to REVIEW_REQUIRED instead of guessing.
//
// Modes (config.ProviderMode): "fake" is an in-process double that renders
// Stripe-shaped sessions and signed webhooks and is refused outside
// LOCAL/TEST/DEV; "sandbox" and "live" are the real client with test or
// live keys and a livemode consistency check on every object.
//
// Verification label: CODE_COMPLETE. The Stripe sandbox for the onramp is
// application-gated (docs/build/BLOCKERS.md EB-003: commercial approval and
// credentials outstanding), so SANDBOX_VERIFIED cannot be claimed and the
// integration is BLOCKED_EXTERNAL beyond CODE_COMPLETE + contract tests.
//
// This package must never:
//   - treat a redirect "success", a 2xx from the API, or a webhook receipt
//     as settlement: it reports provider status, the funding service
//     credits only after a reconciled chain receipt;
//   - accept a webhook without a verified v1 signature within tolerance,
//     or compare signatures with anything but hmac.Equal;
//   - parse an amount as a float, or invent fields the object reference
//     does not document;
//   - persist or log client_secret or the API key;
//   - retry POST /checkout blindly (UNKNOWN_EFFECT_WRITE) — the headless
//     checkout is not implemented in V1, only the hosted/embedded flow;
//   - run the fake mode in STAGING or PROD.
package stripe
