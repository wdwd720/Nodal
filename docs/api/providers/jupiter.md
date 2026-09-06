# Jupiter Swap API — verified integration notes

Role in this platform: **ExecutionAdapter** (Solana swap routing, transaction build, optional managed execution).
Overall status: **VERIFIED_FROM_OFFICIAL_DOCS** for Swap API V2 (`/swap/v2`); **PARTIAL** for legacy Metis V1 (`/swap/v1`); referral program ID **UNVERIFIED**.

## Verification record

All fetched 2026-09-05 unless noted. `dev.jup.ag` now 301-redirects to `developers.jup.ag`; the old `/docs/swap-api/*` and `/docs/api-setup`, `/docs/api-rate-limit`, `/docs/misc/changelog` paths return **404** after redirect (do not link to them).

| Source | URL | Result |
|---|---|---|
| Docs index (llms.txt) | https://developers.jup.ag/docs/llms.txt | OK |
| Swap V2 OpenAPI spec | https://developers.jup.ag/docs/openapi-spec/swap/v2/swap.yaml | OK (authoritative for field types) |
| Order & Execute guide | https://developers.jup.ag/docs/swap/order-and-execute.md | OK |
| Build (raw instructions) guide | https://developers.jup.ag/docs/swap/build/index.md | OK |
| Metis -> Build migration | https://developers.jup.ag/docs/swap/migration/metis-to-build.md | OK |
| Legacy V1 quote page | https://developers.jup.ag/docs/swap/get-quote | OK (carries "no longer actively maintained" notice) |
| Legacy V1 swap page | https://developers.jup.ag/docs/swap/build-swap-transaction | OK (partial field list only) |
| Slippage / RTSE | https://developers.jup.ag/docs/swap/advanced/slippage.md | OK |
| API keys | https://developers.jup.ag/docs/portal/api-keys.md | OK |
| Rate limits | https://developers.jup.ag/docs/portal/rate-limits.md | OK |
| Plans | https://developers.jup.ag/docs/portal/plans.md | OK |
| Response codes | https://developers.jup.ag/docs/portal/responses.md | OK |
| Platform migration | https://developers.jup.ag/docs/portal/migration.md | OK |
| Transaction landing (tx.jup.ag) | https://developers.jup.ag/docs/transaction/submit.md | OK |
| Development basics | https://developers.jup.ag/docs/get-started/development-basics.md | OK |
| Changelog | https://developers.jup.ag/changelog | OK (monthly digests) |
| Referral program | https://developers.jup.ag/docs/tool-kits/referral-program.md | OK but no program ID |
| Program IDs | https://github.com/jup-ag/instruction-parser (official jup-ag org) | OK |
| References page | https://developers.jup.ag/docs/resources/references.md | Fetched; contains no program IDs |
| Referral repo Anchor.toml | https://raw.githubusercontent.com/TeamRaccoons/referral/{main,master}/Anchor.toml | 404 both |

## Versioning and status (critical)

- Current API: **Swap API V2**, base URL `https://api.jup.ag/swap/v2`. Changelog dates the V2 launch to **March 2026**; the Developer Platform (new gateway/keys) launched **April 6, 2026**; payment-setup grace period ended **June 30, 2026**.
- Legacy **Metis V1** (`https://api.jup.ag/swap/v1/quote`, `/swap`, `/swap-instructions`): official pages state verbatim "Metis Swap API is no longer actively maintained and has been superseded by Swap V2" and "PLEASE USE THE METIS SWAP API AT YOUR OWN DISCRETION". No sunset date is published. **Do not build new integrations on V1.**
- `quote-api.jup.ag/v6` ("Quote V6"): changelog records sunset in **August 2025**.
- `lite-api.jup.ag`: replaced by *keyless* requests on `api.jup.ag` (0.5 RPS); migration page says it "will be progressively deprecated ... rate limits reduced progressively until it is fully retired".
- Router rename **iris -> metis** happened May 2026 (affects the `router` field value).
- Network: "Jupiter is deployed on Solana **mainnet** only." No devnet API.

## Auth and secrets

- Header: `x-api-key: <key>` on `api.jup.ag` (and on `tx.jup.ag`). Keys are created in the Developer Platform portal (https://developers.jup.ag/portal); "API keys on the new platform cannot be fully viewed after creation".
- Keyless access exists (0.5 RPS / 30 RPM) for testing only.
- Key permissions are an allow-list of products; "The Swap permission also covers `/tx/v1/submit`".
- Rotation guidance: create new key, move traffic, delete old.

## Endpoints and schemas (Swap API V2)

### GET `/swap/v2/order` — quote + assembled transaction (managed execution path)

Request query parameters (from OpenAPI):

| Param | Type | Req | Notes |
|---|---|---|---|
| inputMint / outputMint | string | yes | mint addresses |
| amount | **string** | yes | smallest unit (atomic integer as string) |
| taker | string | no | signer wallet; omitted -> quote-only, `transaction` is null |
| receiver | string | no | must differ from taker |
| swapMode | string | no | only `ExactIn` supported |
| slippageBps | integer | no | 0-10000; default auto (RTSE) |
| referralAccount | string | no | requires referralFee |
| referralFee | number | no | bps, **50-255**; requires referralAccount |
| payer | string | no | gas payer; restricts routing to Metis (disables JupiterZ, Dflow) |
| priorityFeeLamports / jitoTipLamports | number | no | overrides |
| broadcastFeeType | string | no | `maxCap` or `exactFee` |
| excludeRouters | string | no | csv of metis, jupiterz, dflow, okx |
| excludeDexes | string | no | csv (Metis only) |

Response 200 (string-vs-number is exact per OpenAPI):

| Field | Type | Notes |
|---|---|---|
| mode | string | `ultra` (no optional params) or `manual` |
| inAmount, outAmount, otherAmountThreshold | **string** | atomic integers — parse as big integers, never float |
| inUsdValue, outUsdValue, swapUsdValue | number | informational only |
| priceImpact | number | percentage points ("divide by 100 for decimal") |
| priceImpactPct | **string** | **deprecated** on /order; use priceImpact |
| slippageBps, feeBps | number | |
| platformFee | object | `{amount: string, feeBps: number, feeMint: string}` |
| routePlan | RoutePlanStep[] | `{swapInfo{ammKey,label,inputMint,outputMint,inAmount:string,outAmount:string}, percent:number, bps:number, usdValue:number}` |
| router | string | winner: metis, jupiterz, dflow, okx (`swapType` deprecated) |
| transaction | string or null | base64 unsigned v0 tx; **null** if no taker; **empty string** if build error (see errorCode) |
| lastValidBlockHeight | **string** | hard expiry for aggregator routes |
| expireAt, quoteId, maker | string | RFQ (JupiterZ) routes only; `expireAt` is the quote expiry |
| signatureFeeLamports, prioritizationFeeLamports, rentFeeLamports | number | plus `*FeePayer` string-or-null fields |
| gasless | boolean | fees paid by another wallet |
| requestId | string | **must** be passed to /execute |
| totalTime | number | ms |
| errorCode / errorMessage / error | number or null / string or null | aggregator: 1 insufficient funds, 2 insufficient SOL for gas, 3 below gasless minimum; JupiterZ: 1 insufficient balance, 2 missing ATA, 3 quote build failure |

Note: the guide page says `transaction` is "empty string if quote-only" while the OpenAPI says null when no taker and empty string on error. Treat both null and "" as "no transaction" and branch on `errorCode`.

### POST `/swap/v2/execute` — submit signed transaction (Jupiter lands, confirms, retries)

Body: `signedTransaction` (string, base64, required), `requestId` (string, required), `lastValidBlockHeight` (OpenAPI: **string**, guide page: number — send the value returned by /order unchanged).

Response 200: `status` ("Success"|"Failed"), `signature` string, `slot` **string**, `error` string-or-null, `code` number (0 success; -1 missing cached order, -2 invalid signed tx, -3 invalid message bytes; -1000..-1004 aggregator failures; -2000..-2004 RFQ failures), `totalInputAmount`, `totalOutputAmount`, `inputAmountResult`, `outputAmountResult` (all **string**, atomic), `swapEvents[]` `{inputMint, inputAmount:string, outputMint, outputAmount:string}`. 400: `{error, code}`; 500: `{signature, error}`.

JupiterZ routes require **partial signing** (a market-maker is an additional signer) — use `partiallySignTransaction`-style signing, not a full re-serialize that drops other signatures.

### GET `/swap/v2/build` — raw instructions (self-assembled tx; Metis routing only; no Jupiter fee)

Params: inputMint, outputMint, amount (**string**), taker (required), slippageBps (0-10000 or `"rtse"`, default 50), mode (`fast`), dexes | excludeDexes (mutually exclusive), platformFeeBps (integer 0-10000, requires feeAccount), feeAccount (token account), maxAccounts (1-64, default 64), payer, wrapAndUnwrapSol (bool, default true), destinationTokenAccount | nativeDestinationAccount, blockhashSlotsToExpiry (1-300, default 150), tipAmount (string lamports), computeUnitPricePercentile (`medium`|`high`|`veryHigh` or 0-10000), forJitoBundle (bool).

Response: inAmount/outAmount/otherAmountThreshold (**string**), slippageBps (number), priceImpactPct (**string**, decimal e.g. "0.001" = 0.1%), routePlan[], computeBudgetInstructions[], setupInstructions[], swapInstruction, cleanupInstruction (nullable), otherInstructions[], tipInstruction (nullable), addressesByLookupTableAddress (object: ALT address -> addresses; nullable), blockhashWithMetadata `{blockhash: number[], lastValidBlockHeight: number, fetchedAt{secs_since_epoch, nanos_since_epoch}}`. Instruction shape: `{programId, accounts[{pubkey,isSigner,isWritable}], data: base64}`.

Caveats from docs: `/build` output "cannot use `/execute`" (no requestId); response includes CU price but **not** CU limit — "You need to simulate the transaction to determine the correct limit" (simulate at 1.4M, set ~1.2x); "V2 instructions do not emit fee events"; routePlan moved from `percent` to `bps` (both present).

### Legacy Metis V1 (record only; do not build on)

- `GET /swap/v1/quote`: params inputMint, outputMint, amount (number in docs), slippageBps, restrictIntermediateTokens, instructionVersion; response inAmount/outAmount/otherAmountThreshold/priceImpactPct **string**, slippageBps/contextSlot/timeTaken number, platformFee null-or-object, routePlan[], mostReliableAmmsQuoteReport. Other params (onlyDirectRoutes, maxAccounts, dexes, excludeDexes, platformFeeBps, asLegacyTransaction) are **not listed on the current page** — UNVERIFIED for V1.
- `POST /swap/v1/swap`: page shows only `quoteResponse`, `userPublicKey`, `dynamicComputeUnitLimit`, `dynamicSlippage`, `prioritizationFeeLamports.priorityLevelWithMaxLamports{maxLamports, priorityLevel}`. Response: `swapTransaction` (base64 string), `lastValidBlockHeight` (number), `prioritizationFeeLamports` (number), `computeUnitLimit` (number), `prioritizationType.computeBudget{microLamports, estimatedMicroLamports}`, `dynamicSlippageReport{slippageBps, otherAmount, simulatedIncurredSlippageBps, amplificationRatio: string, categoryName, heuristicMaxSlippageBps}`, `simulationError` (null or object). wrapAndUnwrapSol, useSharedAccounts, feeAccount, trackingAccount, jitoTipLamports, asLegacyTransaction, destinationTokenAccount, skipUserAccountsRpcCalls, blockhashSlotsToExpiry: **UNVERIFIED on current page**.
- `POST /swap/v1/swap-instructions`: response keys tokenLedgerInstruction, computeBudgetInstructions, setupInstructions, swapInstruction, cleanupInstruction, addressLookupTableAddresses (string[]; V2 replaces with `addressesByLookupTableAddress` map).
- Migration mapping: `userPublicKey` -> `taker` (required); `swapMode` removed (ExactIn only); base `/swap/v1` -> `/swap/v2`.

### Transaction landing service `POST https://tx.jup.ag` (JSON-RPC `sendTransaction`)

Params `[base64Tx, {encoding:"base64", skipPreflight:true (preflight unsupported), maxRetries:0, swqosOnly?:bool}]`; requires SOL tip >= 1,000,000 lamports to one of 16 tip accounts; tx <= 1232 bytes; `x-api-key` required. Error `-1015` for invalid config; "Transaction must include a Jupiter tip instruction" if no tip. Success = accepted/forwarded, **not landed**; confirm with your own RPC using blockhash + lastValidBlockHeight.

## On-chain program IDs and fees

- Aggregator v6: `JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4` (jup-ag/instruction-parser, mapped to parser 6.0.7). Older: `JUP4Fb2cqiRUcaTHdrPC8h2gNsA2ETXiPDD33WcGuJB` (v4), `JUP5pEAZeHdHrLxh5UCwAbpjGwYKKoquCpda2hfP4u8` (v5), `JUP5cHjnnCx2DppVsufsLrXs8EBZeEZzGtEK9Gdz6ow` (v5.1). Not listed on developers.jup.ag itself. A secondary deployment `JUPyiwrYJFskUPiHa7hkeR8VUtAeFoSYbKedZNsDvCN` surfaced only via third-party search — **UNVERIFIED**.
- Fees on /order: Jupiter platform fee shown in quote (0 bps JUP purchases and pegged assets, 2 bps SOL-Stable, 5 bps LST-Stable, 10 bps other, 50 bps new tokens); integrator referral via `referralAccount` + `referralFee` (50-255 bps), "20% of integrator fee goes to Jupiter". On /build: no Jupiter fee; `platformFeeBps` + `feeAccount` only.
- Referral program: open-source on-chain program (github.com/TeamRaccoons/referral), dashboard https://referral.jup.ag/, `default_share_bps` on the Project. **Program ID UNVERIFIED** (not on any fetched official page).

## Errors, rate limits, retries

HTTP: 200; 400 bad params; 401 invalid/unknown key; 403 missing permission or firewall; 404; 429 rate limited; 5xx "Retry with backoff". Every response carries `x-api-gateway-request-id` (log it). JSON error body shape is **not formally documented** on the responses page; observed shapes: /order 400 `{requestId, error}`; /build 400 `{error}`; /execute 400 `{error, code}`.

Rate limits (per **organisation**, 60-second sliding window; keys/teams do not add capacity): Keyless 0.5 RPS; Free 1 RPS ($0); Developer 10 RPS ($25/mo, 25M credits); Launch 50 RPS ($100/mo); Pro 150 RPS ($500/mo). `/swap/v2/execute` has a **separate bucket** (Keyless 20, Free 50, Paid 100 RPS) and costs 0 credits. Headers (success and 429 only): `x-ratelimit-remaining` (can go negative), `x-ratelimit-current`, `x-ratelimit-reset` (unix ts). "There is no extra cooldown or lockout after a 429"; wait until reset. 429 body: `[API Gateway] Too many requests` (plan) vs `Too many requests` (firewall).

On-chain slippage error code (0x1771 / 6001) is **not** documented on the fetched slippage page — UNVERIFIED; handle any `/execute` `status:"Failed"` by re-quoting.

Write-endpoint classification:

| Endpoint | Class | Rationale |
|---|---|---|
| GET /order, GET /build, legacy /quote, /swap, /swap-instructions | SAFE_RETRY | Build-only; each retry yields a new requestId/blockhash. Discard the old one. |
| POST /execute | UNKNOWN_EFFECT_WRITE | Broadcasts a signed tx. The chain de-duplicates by signature, but a timeout leaves landing unknown; reconcile via `getSignatureStatuses` on `signature` before retrying, and never re-sign with a new blockhash without confirming the first attempt expired. |
| POST tx.jup.ag sendTransaction | UNKNOWN_EFFECT_WRITE | Same as above; success != landed. |

## Webhooks/streams

None for the Swap API. (RFQ webhook API exists for market makers only — out of scope.)

## Sandbox/test availability

None. Mainnet only. Use keyless tier (0.5 RPS) or a Free key against mainnet with tiny amounts; simulate with `simulateTransaction` before signing.

## Open questions / unverified

1. Referral program on-chain ID and SDK package name — not on official docs (repo Anchor.toml 404).
2. V1 parameter list beyond the four shown — current pages omit them; do not assume they still work.
3. `/execute.lastValidBlockHeight` type conflict (string in OpenAPI, number in guide) — pass through the /order string.
4. No formal sunset date for `/swap/v1`; no SLA statement for `/execute` confirmation latency.
5. Secondary aggregator program ID (`JUPy...`) unverified.
