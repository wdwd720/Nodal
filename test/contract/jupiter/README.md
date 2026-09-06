# Jupiter Swap API V2 — contract tests

Provider contract tests (goal PART 148) for `internal/provider/jupiter` against recorded fixtures in `testdata/`.
Verification label: **CODE_COMPLETE**. Live verification is **BLOCKED_EXTERNAL (EB-011)**: no API key is available, so no fixture here was captured from `api.jup.ag`. Every fixture was authored from the official OpenAPI specification as recorded in `docs/api/providers/jupiter.md` (fetched 2026-09-05), and the transaction bytes were produced by `jupiter.BuildFakeRouteTransaction` (a real serialized Solana v0 transaction) and signed with a deterministic test wallet. `TestContract_FixtureIsReproducible` re-derives them on every run.

Run: `go test -count=1 -race ./test/contract/jupiter/...`

## Sources

| Item | URL |
|---|---|
| Swap V2 OpenAPI (authoritative for field types) | https://developers.jup.ag/docs/openapi-spec/swap/v2/swap.yaml |
| Order & Execute guide | https://developers.jup.ag/docs/swap/order-and-execute.md |
| Build guide | https://developers.jup.ag/docs/swap/build/index.md |
| Response codes | https://developers.jup.ag/docs/portal/responses.md |
| Rate limits | https://developers.jup.ag/docs/portal/rate-limits.md |
| API keys | https://developers.jup.ag/docs/portal/api-keys.md |
| Program ids (JUP6…) | https://github.com/jup-ag/instruction-parser |
| Transaction expiry semantics | https://solana.com/docs/core/transactions/confirmation |

## Cases

| Test | Fixture | Expectation |
|---|---|---|
| `Order_Valid` | `order_valid.json` | full projection, exact amounts, transaction decoded (fee payer, blockhash, CU limit/price), evidence hashed, `ValidateQuote` passes |
| `Order_QuoteOnly` | `order_quote_only.json` | no taker → `transaction: null`, `HasTransaction=false` |
| `Order_InvalidResponse_FloatAmount` | `order_float_amount.json` | `inAmount: 1000000.0` → `VALIDATION_FAILED` naming `inAmount`, not retried |
| `Order_MissingRequiredField` | `order_missing_required.json` | missing `otherAmountThreshold` → `VALIDATION_FAILED` naming the field |
| `Order_BuildErrorCode` | `order_build_error.json` | 200 with `errorCode: 2`, `transaction: ""` → `VALIDATION_FAILED` reason `INSUFFICIENT_SOL_FOR_GAS` |
| `Order_Timeout` | (hang) | `PROVIDER_UNAVAILABLE`, `timed_out=true`, bounded retries, each attempt archived |
| `Order_RateLimited_RetryAfter` | `rate_limited.txt` | 429 then 200: `Retry-After` waited exactly; exhausted → `RATE_LIMITED` with `RetryAfter` from `x-ratelimit-reset` |
| `Order_ServerError` | (503) | `PROVIDER_UNAVAILABLE` after `1 + MaxRetries` attempts; tracker `UNHEALTHY` |
| `Order_StaleQuote` | `order_rfq_expired.json` | `expireAt` in the past → `QUOTE_EXPIRED`; `lastValidBlockHeight` behind chain → `QUOTE_EXPIRED` |
| `Order_UnexpectedStatus` | (302/418/204) | `PROVIDER_UNAVAILABLE` / decode failure, never retried |
| `Order_400NoRoute` | `order_400_no_route.json` | `VENUE_LIQUIDITY_INSUFFICIENT` |
| `Order_AuthRejected` | (403) | `PROVIDER_UNAVAILABLE` + `PROVIDER_AUTH_REJECTED` security event |
| `Execute_Success` | `execute_success.json` | `Success`, signature equals the locally signed transaction, body carries only the three documented fields |
| `Execute_Timeout_SubmissionUnknown_NoSecondCall` | (hang) | `SUBMISSION_STATE_UNKNOWN` with the local signature; **exactly one HTTP call** |
| `Execute_Failed` | `execute_failed.json` | `Failed` returned as a result (nil error): caller reconciles on chain |
| `Execute_ErrorStatuses` | `execute_400_missing_order.json`, `execute_500.json`, `rate_limited.txt` | `-1` → `QUOTE_EXPIRED` (not submitted); 500 → `SUBMISSION_STATE_UNKNOWN`; 429/401 → not submitted; unexpected/undecodable/foreign signature → `SUBMISSION_STATE_UNKNOWN`; always one call |
| `Build_Valid` | `build_valid.json` | instructions, ALT map, `blockhashWithMetadata`, `priceImpactPct` fraction → bps, CU limit flagged unavailable |
| `Status_Unsupported` | — | `UNSUPPORTED`; no HTTP call |
| `Evidence_RedactedRequestFullResponse` | `order_valid.json` | `x-api-key` redacted in archived headers; full raw body archived; SHA-256 on the Order |
| `FakeMatchesLiveShape` | — | the Fake passes the same policy, signs/executes, and models "landed but response lost" |

## Fixture fields: documented vs assumed

**Documented** (types exactly per OpenAPI; string-vs-number is load-bearing):

- `/order`: `mode`, `inAmount`/`outAmount`/`otherAmountThreshold` (**string**), `inUsdValue`/`outUsdValue`/`swapUsdValue` (number, never parsed), `priceImpact` (number, percentage points), `priceImpactPct` (string, deprecated, never used), `slippageBps`/`feeBps` (number), `platformFee{amount:string,feeBps,feeMint}`, `routePlan[].swapInfo{ammKey,label,inputMint,outputMint,inAmount:string,outAmount:string}`, `routePlan[].percent/bps/usdValue`, `router`, `transaction` (string | null | ""), `lastValidBlockHeight` (**string**), `expireAt`/`quoteId`/`maker` (RFQ only), `signatureFeeLamports`/`prioritizationFeeLamports`/`rentFeeLamports` (number) + `*FeePayer` (string|null), `gasless`, `requestId`, `totalTime`, `errorCode`/`errorMessage`/`error`.
- `/execute` request: `signedTransaction` (base64), `requestId`, `lastValidBlockHeight` (sent as the `/order` string unchanged). Response: `status` (`Success`|`Failed`), `signature`, `slot` (**string**), `error`, `code` (0; -1 missing cached order; -2 invalid signed tx; -3 invalid message bytes; -1000..-1004 aggregator failures; -2000..-2004 RFQ failures), `totalInputAmount`/`totalOutputAmount`/`inputAmountResult`/`outputAmountResult` (string), `swapEvents[]`. 400 `{error, code}`; 500 `{signature, error}`.
- `/build`: `inAmount`/`outAmount`/`otherAmountThreshold` (string), `slippageBps` (number), `priceImpactPct` (string decimal fraction, `"0.001"` = 0.1%), `routePlan[]`, instruction lists with `{programId, accounts[{pubkey,isSigner,isWritable}], data: base64}`, `addressesByLookupTableAddress`, `blockhashWithMetadata{blockhash:number[], lastValidBlockHeight:number, fetchedAt{secs_since_epoch,nanos_since_epoch}}`; no compute-unit limit is returned.
- Headers: `x-api-key`, `x-api-gateway-request-id`, `x-ratelimit-remaining` (may be negative), `x-ratelimit-current`, `x-ratelimit-reset` (unix ts). 429 body `[API Gateway] Too many requests`.
- HTTP statuses 200/400/401/403/404/429/5xx and the retry classification (`/order`, `/build` SAFE_RETRY; `/execute` UNKNOWN_EFFECT_WRITE).

**Assumed / unverified** (marked in code comments too):

- `Retry-After` header: standard HTTP, not mentioned in the Jupiter docs; honored when present, otherwise `x-ratelimit-reset` drives the wait.
- `expireAt` format: OpenAPI types it as a string without a format; the client reads all-digit values as unix seconds (milliseconds above 10^12) and anything else as RFC 3339. The fixture uses unix seconds.
- Wall-clock validity of aggregator (non-RFQ) orders: not documented; `ExpiresAt = ReceivedAt + AssumedOrderTTL` (30 s) with `ExpiresAtAssumed=true`; the hard expiry is `lastValidBlockHeight`.
- The 400 JSON error body shapes (`{requestId,error}`, `{error,code}`) are "observed", not formally documented; the liquidity / expiry classification of free-text messages is a substring heuristic, and unmatched messages stay `VALIDATION_FAILED`.
- Whether an `/execute` 400 with a code in the -1000..-1004 / -2000..-2004 ranges implies the transaction was never broadcast: not stated → treated as `SUBMISSION_STATE_UNKNOWN`. Gateway-level 401/403/404/429 are treated as "not broadcast".
- Route-plan `ammKey`/`label`/`maker` values, request ids, USD values, `slot` numbers and the input mint (`jupitertest.MintUSDC` is a deterministic stand-in, not the mainnet USDC address) are synthetic.
- The transaction is a shape-compatible placeholder (compute budget, idempotent ATA create, `route`-discriminated instruction under `JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4`), not a route Jupiter would emit. **Blocker SB-007**: its discriminator, account order and data layout were written from memory and have NOT been checked against the published Jupiter v6 IDL or a recorded mainnet swap. These fixtures therefore prove nothing about the Jupiter instruction layout in `internal/signing/inspect` — both came from the same unverified source, so agreement between them is not evidence — and neither may be treated as verified before a canary trade.
- `computeUnitLimit` on `/order` responses: not documented in V2; the client derives it from the transaction's ComputeBudget instructions (`ComputeUnitLimitSource="transaction"`).
