// Package jupiter is the Solana swap execution provider client for the
// Jupiter Swap API V2 (https://api.jup.ag/swap/v2), goal PARTs 41-43, 48,
// 106, 148 and 183.
//
// # Responsibility
//
//   - Client talks to the three documented V2 endpoints and nothing else:
//     GET /order (quote + unsigned transaction), POST /execute (submit a
//     signed transaction) and GET /build (raw instructions). Every request
//     and response is archived through RawArchive before it is interpreted,
//     and the archive reference and SHA-256 hash travel on the returned
//     objects (PART 183).
//   - The method set mirrors the provider-neutral adapter contract in
//     docs/architecture/SETTLEMENT_COMPILER.md section 6 (Order/Quote,
//     ValidateQuote, Build, Execute/Submit, Status) using local request and
//     response structs. The integrator wraps a Service into
//     execution.ExecutionAdapter; this package never imports
//     internal/execution or internal/settlement.
//   - Fake is the in-process double for LOCAL/TEST/DEV. It produces
//     deterministic orders whose UnsignedTransaction is a real serialized
//     Solana v0 transaction (compute budget, idempotent ATA creation and a
//     placeholder route instruction under the configured Jupiter program id)
//     so the signing inspector and the executor can consume it, and it
//     supports fault injection (timeout, expired, slippage exceeded, rate
//     limited, server error).
//   - Every amount is decoded from its JSON string token into money.Quantity.
//     JSON numbers are accepted only where the OpenAPI specification types
//     the field as a number, and even then only integer tokens; a fractional
//     or exponent token in any amount field is VALIDATION_FAILED naming the
//     field. No float type appears in this package.
//   - Retry policy (PART 106): Order and Build are SAFE_RETRY with bounded,
//     jittered retries on 429/5xx/transport errors honoring Retry-After and
//     x-ratelimit-reset. Execute is UNKNOWN_EFFECT_WRITE: exactly one HTTP
//     attempt, ever, and any transport timeout, ambiguous body or unexpected
//     status returns SUBMISSION_STATE_UNKNOWN (PART 48). Status is
//     UNSUPPORTED: V2 documents no status endpoint; transaction status
//     comes from the chain observers.
//   - Error mapping: 400 with a documented body -> VALIDATION_FAILED,
//     QUOTE_EXPIRED or VENUE_LIQUIDITY_INSUFFICIENT; 401/403 ->
//     PROVIDER_UNAVAILABLE plus a SecurityEventSink notification; 429 ->
//     RATE_LIMITED with RetryAfter; 5xx and transport -> PROVIDER_UNAVAILABLE
//     for reads and SUBMISSION_STATE_UNKNOWN for Execute. Each HTTP attempt
//     is sampled into the provider.Tracker for health.
//
// Everything implemented here is taken from docs/api/providers/jupiter.md
// (verified 2026-09-05 against the official OpenAPI specification). Fields
// and behaviors the documentation leaves open are marked ASSUMED or
// UNVERIFIED in code comments and in test/contract/jupiter/README.md.
// VerificationLabel is CODE_COMPLETE: live keys are BLOCKED_EXTERNAL
// (EB-011), so nothing here has been exercised against api.jup.ag.
//
// # This package must never
//
//   - Retry Execute. A timed-out or ambiguous submission is reported as
//     SUBMISSION_STATE_UNKNOWN and left to the unknown-submission recovery
//     path (EXECUTION.md section 4). There is no code path that sends the
//     same signed transaction twice, and no code path that re-signs.
//   - Let Jupiter wire types leak. Every wire struct is unexported; callers
//     see Order, ExecuteResult, BuildResult and their provider-neutral
//     fields only. Raw payloads are available solely through the archive
//     reference.
//   - Trust outAmount, otherAmountThreshold, the route or the transaction
//     without inspection. ValidateQuote checks freshness, mints, taker,
//     minimum output, slippage, price impact and block-height sanity; the
//     transaction bytes are decoded to confirm the fee payer, blockhash and
//     compute budget, but the signing inspector remains the authority on
//     what the transaction does.
//   - Log, archive or return the API key. The x-api-key header is replaced
//     by the redaction marker in archived request headers and is held as an
//     observability.Secret.
//   - Parse, compute or render an amount, fee, slippage or price impact with
//     floating point, or accept a float-formatted amount.
//   - Build on /swap/v1 (Metis, unmaintained) or invent fields that the V2
//     specification does not document.
//   - Hold global mutable state or read the wall clock directly; the Clock
//     and every dependency are injected.
package jupiter
