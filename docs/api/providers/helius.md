# Helius — verified integration notes

Role in this platform: **ChainObserver + SolanaDataProvider** (RPC, parsed transaction data, address-activity streams/webhooks, balances, priority fees).
Overall status: **VERIFIED_FROM_OFFICIAL_DOCS** for RPC/WebSocket/DAS/priority-fee/Sender/billing; **PARTIAL** for Enhanced Transactions (legacy) and Webhooks (host and full payload not shown on fetched pages).

## Verification record

All fetched 2026-09-05.

| Source | URL | Result |
|---|---|---|
| Docs index | https://www.helius.dev/docs/llms.txt | OK |
| Endpoints | https://www.helius.dev/docs/api-reference/endpoints.md | OK |
| Authentication | https://www.helius.dev/docs/api-reference/authentication.md | OK |
| RPC overview | https://www.helius.dev/docs/rpc | OK |
| RPC method index | https://www.helius.dev/docs/api-reference/rpc/http/llms.txt | OK |
| WebSocket overview | https://www.helius.dev/docs/rpc/websocket.md | OK |
| transactionSubscribe | https://www.helius.dev/docs/enhanced-websockets/transaction-subscribe | OK |
| Enhanced Tx (legacy) index | https://www.helius.dev/docs/api-reference/enhanced-transactions/llms.txt | OK |
| Enhanced Tx -> Parsed Events migration | https://www.helius.dev/docs/parsed-events/guides/migrate-from-enhanced-transactions.md | OK |
| Parsed Events quickstart / response / overview | https://www.helius.dev/docs/parsed-events/quickstart.md , .../parsed-events/parsed-response.md , .../api-reference/parsed-events/overview.md | OK |
| Webhooks index / FAQ / overview | https://www.helius.dev/docs/api-reference/webhooks/llms.txt , https://www.helius.dev/docs/faqs/webhooks.md , https://www.helius.dev/docs/webhooks.md | OK (no host shown) |
| DAS getAssetsByOwner / getTokenAccounts / fungible ext. | https://www.helius.dev/docs/api-reference/das/getassetsbyowner , .../das/gettokenaccounts , https://www.helius.dev/docs/das/fungible-token-extension.md , https://www.helius.dev/docs/das/get-tokens.md | OK |
| Wallet API balances | https://www.helius.dev/docs/wallet-api/balances.md | OK |
| getTransactionsForAddress | https://www.helius.dev/docs/rpc/gettransactionsforaddress.md | OK |
| Priority fee | https://www.helius.dev/docs/priority-fee-api , https://www.helius.dev/docs/api-reference/priority-fee/llms.txt | OK |
| Sender | https://www.helius.dev/docs/sending-transactions/sender.md | OK |
| Plans / billing | https://www.helius.dev/docs/billing/plans , https://www.helius.dev/docs/billing/llms.txt | OK |
| Error codes | https://www.helius.dev/docs/faqs/error-codes.md | OK |
| LaserStream guarantees | https://www.helius.dev/docs/laserstream/delivery-guarantees.md | OK |
| Failed | https://www.helius.dev/docs/api-reference/enhanced-transactions/parse-transactions(.md), .../webhooks/create-webhook.md (partial), .../webhooks/get-all-webhooks.md (no body) | 404 / no detail |

## Endpoint formats and auth

| Surface | URL | Notes |
|---|---|---|
| RPC mainnet | `https://mainnet.helius-rpc.com/?api-key=<KEY>` | standard Solana JSON-RPC + Helius methods |
| RPC devnet | `https://devnet.helius-rpc.com/?api-key=<KEY>` | |
| RPC beta gateway | `https://beta.helius-rpc.com/?api-key=<KEY>` | "Gatekeeper" beta |
| WebSocket | `wss://mainnet.helius-rpc.com/?api-key=<KEY>` (devnet / beta variants) | standard subscriptions + Helius extensions |
| Secure (frontend) RPC | `https://<id>-fast-mainnet.helius-rpc.com` | "IP rate-limited at 5 TPS" |
| Enhanced Tx (legacy) / Parsed Events | `https://mainnet.helius-rpc.com/v0/transactions` , `/v1/parsed-events/transactions` , `/v1/parsed-events/transaction-history` | mainnet only for Parsed Events |
| Wallet API | `https://api.helius.xyz/v1/wallet/{address}/balances` | `api-key` query **or** `X-Api-Key` header |
| Webhooks REST | `/v0/webhooks...?api-key=` (paths verified; **host not shown** on fetched pages — UNVERIFIED, historically `api.helius.xyz`) | |
| Sender | `https://sender.helius-rpc.com/fast` (+ regional `http://{slc,ewr,lon,fra,ams,sg,tyo}-sender.helius-rpc.com/fast`) | no key needed; key raises TPS |

## Auth and secrets

API key as `?api-key=` query parameter everywhere; only the Wallet API documents a header alternative (`X-Api-Key`). No OAuth, no signed requests. Dashboard supports IP restrictions per key ("Set up IP restrictions for your API keys in the dashboard"). Treat the key as a secret — it lives in the URL, so scrub query strings from access logs, tracing spans, and error messages; use separate keys per environment and rotate by regenerating in the dashboard. Webhook `authHeader` is a shared secret you choose; Helius echoes it verbatim in the `Authorization` header on delivery — store it hashed and compare in constant time.

Commitment: standard `processed` (~400 ms, may roll back) / `confirmed` (~2-3 s) / `finalized` (~15-30 s); docs recommend `confirmed` for most apps. Parsed Events default is **confirmed**; legacy Enhanced Transactions default was finalized.

## Helius-specific vs standard

Standard JSON-RPC (pass-through, see solana-rpc.md): getAccountInfo, getMultipleAccounts, getBalance, getProgramAccounts, getLatestBlockhash, getBlock, getBlockHeight, sendTransaction, getTransaction, getSignaturesForAddress, getSignatureStatuses, simulateTransaction, getTokenAccountBalance, getTokenAccountsByOwner, getSlot, getHealth; WS accountSubscribe, blockSubscribe, logsSubscribe, programSubscribe, rootSubscribe, signatureSubscribe, slotSubscribe, slotsUpdatesSubscribe, voteSubscribe.

Helius-only (lock-in risk): getTransactionsForAddress, getTransfersByAddress, getProgramAccountsV2, getTokenAccountsByOwnerV2, getPriorityFeeEstimate, all DAS methods (getAssetsByOwner, getTokenAccounts, searchAssets, getAsset...), WS transactionSubscribe and extended accountSubscribe (Developer+ plans), Enhanced Transactions / Parsed Events REST, Webhooks, Wallet API, Sender, LaserStream gRPC.

## Endpoints and schemas

### Enhanced Transactions API (legacy, "maintenance mode")

Status verbatim: "The Enhanced Transactions API is a legacy product in maintenance mode: it still works, but it is not receiving new parser types or feature work." "There is no forced cutoff." Successor: Parsed Events (open beta, paid plans, mainnet only).

`POST /v0/transactions?api-key=` body `{transactions: [sig...], commitment?: "finalized"|"confirmed"}`, max **100** signatures. Response item fields: description (string), type (string enum e.g. SWAP, TRANSFER), source (string e.g. JUPITER), fee (number, lamports), feePayer (string), signature (string), slot (number), timestamp (number, unix s), nativeTransfers[] `{fromUserAccount, toUserAccount, amount:number}`, tokenTransfers[] `{fromUserAccount, toUserAccount, fromTokenAccount, toTokenAccount, tokenAmount:number (decimal-adjusted), mint, tokenStandard}`, accountData[] `{account, nativeBalanceChange:number, tokenBalanceChanges[{userAccount, tokenAccount, mint, rawTokenAmount{tokenAmount:string, decimals:number}}]}`, transactionError, instructions[], events. **Use `accountData[].tokenBalanceChanges[].rawTokenAmount.tokenAmount` (string) for exact accounting; `tokenTransfers[].tokenAmount` is a float.** `GET /v0/addresses/{address}/transactions` for history (100 credits per call).

### Parsed Events (successor)

`POST /v1/parsed-events/transactions` body `{transactions: string[] (max 100), commitment?: "confirmed"(default)|"finalized", includeRawTransaction?: bool}`. Response: array of `{signature, parserStatus: "OK"|"ERROR", parsed, parserError{code,message}, rawTransaction?}` in input order. `parsed`: slot (number), blockTime (number|null), fee (number), feePayer (string|null), transactionStatus "OK"|"ERROR", error/decodedError, nativeTransfers[] (same shape), tokenTransfers[] `{fromUserAccount, toUserAccount, fromTokenAccount, toTokenAccount, rawTokenAmount: number, decimals: number, tokenStandard: "Fungible"|"UnknownStandard", mint}`, summary `{type: add_liquidity|create_account|create_token_account|remove_liquidity|swap|transfer, description, parsedData{protocol,...}}`, instructions[] `{instructionIndex, innerInstructionIndex, programId, programName, instructionName, decoded{args, accounts[{name,pubkey,isSigner,isWritable}]}, summary}`. **`rawTokenAmount` is a JSON number here** — values above 2^53 lose precision in JS/float parsers; parse with a big-integer-aware decoder or prefer RPC `uiTokenAmount.amount` strings for ledger-grade amounts.
`POST /v1/parsed-events/transaction-history` body `{address, limit 1-100 (default 100), paginationToken, sortOrder asc|desc, commitment, includeRawTransaction, beforeSignature, afterSignature, slot{gt,gte,lt,lte}, time{...}}` -> `{data[], paginationToken}`.

### getTransactionsForAddress (Helius RPC extension)

Params: address (required), transactionDetails `signatures`(default)|`full` (up to 1,000 tx per call), sortOrder desc|asc, limit (<=1000), paginationToken, commitment finalized|confirmed (no processed), maxSupportedTransactionVersion (set 0), encoding json|jsonParsed|base64|base58, filters.slot / filters.blockTime ranges, filters.status succeeded|failed|any, filters.tokenAccounts none|balanceChanged|all (includes the owner's ATA activity), filters.tokenTransfer{with, direction in|out|any, mint, amount range}. Full mode returns standard transaction+meta objects — same string amount semantics as `getTransaction`.

### DAS balances

- `getAssetsByOwner` params: ownerAddress, page/limit (<=1000) or before/after cursor, sortBy{sortBy: created|recent_action|updated|none, sortDirection}, options{showFungible, showNativeBalance, showZeroBalance, showUnverifiedCollections, showCollectionMetadata, showGrandTotal}. Response: last_indexed_slot, total, limit, page, items[] (id, interface, content, ownership, ...). With showFungible, items carry `token_info{symbol, balance, supply, decimals, token_program, associated_token_address, price_info{price_per_token, total_price, currency:"USDC"}, mint_extensions?}`. **balance/supply are shown as JSON numbers in examples; string-vs-number not formally specified** — precision caveat as above.
- `getTokenAccounts` params: owner | mint, page, limit, cursor/before/after, options.showZeroBalance. Response: total, limit, cursor, token_accounts[] `{address, mint, owner, amount: integer (JSON number), delegated_amount: integer, frozen: bool}`. `token_extensions` not in schema.
- Wallet API `GET /v1/wallet/{address}/balances` (100 credits): query page, limit (1-100), showZeroBalance, showNative, showNfts; response balances[] `{mint, symbol, name, balance: number (already decimal-adjusted), decimals, pricePerToken, usdValue, logoUri, tokenProgram: "spl-token"|"token-2022"}`, nfts[], totalUsdValue (page only), pagination{page, limit, hasMore}. Convenience only — not for exact accounting.

### Priority fee — `getPriorityFeeEstimate` (1 credit)

Params: `transaction` (base58/base64 serialized) **or** `accountKeys[]`; options `{priorityLevel: Min|Low|Medium|High|VeryHigh|UnsafeMax, recommended: bool, includeAllPriorityFeeLevels: bool, transactionEncoding: Base58|Base64, lookbackSlots: 1-150, includeVote: bool, evaluateEmptySlotAsZero: bool}`. Response `priorityFeeEstimate` (number, **micro-lamports per CU**) and/or `priorityFeeLevels{min, low, medium, high, veryHigh, unsafeMax}`.

### Sender (transaction submission)

JSON-RPC `sendTransaction` with `["<base64>", {encoding:"base64", skipPreflight:true, maxRetries:0}]`; every tx must include a tip (>= 0.001 SOL Sender Max; 0.000005 SOL SWQOS-only) to a designated tip account (list on the Sender page, e.g. `4ACfpUFoaSD9bfPdeu6DBt89gB6ENTeHBXCAi87NhDEE`) **and** a priority fee. Default 50 TPS; no credits consumed; success = accepted/routed, **not landed** — confirm via RPC.

## Webhooks/streams

Webhooks (`POST /v0/webhooks`, `GET /v0/webhooks`, `GET|PUT|PATCH|DELETE /v0/webhooks/{webhookID}`; all `?api-key=`): body `webhookURL` (https), `webhookType` `enhanced|raw|discord|enhancedDevnet|rawDevnet|discordDevnet`, `accountAddresses[]` (up to **100,000**), `transactionTypes[]` (empty = all), `authHeader` (string), `txnStatus`, `encoding`. Response: webhookID, wallet, webhookURL, webhookType, accountAddresses, transactionTypes, authHeader, active. Delivery: "Helius echoes this value in the `Authorization` header" — verify it with constant-time compare. Semantics verbatim: retries "up to 3 times with a 1-second gap" on 5xx, 4xx (except 403), timeout, connection/DNS failure; after that "the event is **permanently lost**"; endpoint must return 200 **within 1 second**; "Helius may deliver the same webhook event multiple times" (dedupe on signature); fired "immediately after a matching transaction is confirmed"; enhanced webhooks omit failed txs, raw include them; devnet via `*Devnet` types. 1 credit per event. Payload: enhanced = array of Enhanced Transaction objects (schema above); raw = array of raw transactions `{blockTime, meta, slot, transaction}`. **Webhooks are a best-effort notifier — backfill with getTransactionsForAddress.**

Enhanced WebSocket `transactionSubscribe` (Developer/Business/Professional): filter `{vote, failed, signature, accountInclude[], accountExclude[], accountRequired[], tokenAccounts: balanceChanged|all|none}` (up to 50,000 addresses per array); options `{commitment, encoding base58|base64|jsonParsed, transactionDetails full|signatures|accounts|none, showRewards, maxSupportedTransactionVersion (required for accounts/full)}`; notification method `transactionNotification`. Keepalive: 10-minute inactivity timer — "send a ping at least once per minute"; reconnect with exponential backoff (1 s -> 30 s + jitter). Billing: 2 credits per 0.1 MB streamed (uncompressed). No replay on reconnect — use LaserStream (gRPC, "exactly-once", 24 h replay, Business+ mainnet) or history backfill to close gaps.

## Errors, rate limits, retries

HTTP 401 (bad/missing key), 429 (rate limit; no documented headers), 500, 503 (retry after delay), 504 (reduce scope/paginate). JSON-RPC error codes not Helius-documented (see solana-rpc.md). Plans: Free 1M credits, 10 RPS RPC, 2 RPS DAS/Enhanced; Developer $49 10M, 50/10/10 RPS; Business $499 100M, 200/50/50; Professional $999 200M, 500/100/100. Credits: standard RPC 1, DAS 10, getProgramAccounts 10, Enhanced Transactions 100, priority fee 1, webhook event 1, Wallet API balances 100, staked sendTransaction 1.

Write classification:

| Endpoint | Class | Rationale |
|---|---|---|
| sendTransaction (RPC or Sender) | UNKNOWN_EFFECT_WRITE | Chain dedupes by signature; success != landed; reconcile via getSignatureStatuses. |
| POST /v0/webhooks | UNKNOWN_EFFECT_WRITE | No idempotency key; retry creates duplicate webhooks -> list-then-create by URL. |
| PUT/PATCH/DELETE /v0/webhooks/{id} | IDEMPOTENT_WRITE | Content-idempotent by id. |
| All reads (RPC, DAS, Parsed Events, priority fee) | SAFE_RETRY | Costs credits per retry. |

## Sandbox/test availability

Devnet RPC/WSS and devnet webhooks exist. Parsed Events, LaserStream (Free tier), and Wallet API: mainnet only / plan-gated.

## Worked request shapes (from the official examples)

Parsed Events (successor to Enhanced Transactions):
```http
POST https://mainnet.helius-rpc.com/v1/parsed-events/transactions?api-key=<KEY>
{"transactions": ["<signature>"], "commitment": "finalized", "includeRawTransaction": false}
```
Check `parserStatus == "OK"` per item; fall back to RPC `getTransaction` (jsonParsed, `maxSupportedTransactionVersion: 0`) for `ERROR` items and for exact amounts.

Address activity stream:
```json
{"jsonrpc":"2.0","id":1,"method":"transactionSubscribe","params":[
  {"accountInclude":["<wallet>","<wallet ATA>"],"failed":false,"vote":false,"tokenAccounts":"balanceChanged"},
  {"commitment":"confirmed","encoding":"jsonParsed","transactionDetails":"full","showRewards":false,"maxSupportedTransactionVersion":0}]}
```
Ping at least once per minute; on reconnect, backfill the gap with `getTransactionsForAddress` (`filters.slot.gt = last seen slot`, `sortOrder: "asc"`, `transactionDetails: "full"`).

Priority fee before signing:
```json
{"jsonrpc":"2.0","id":1,"method":"getPriorityFeeEstimate","params":[{"transaction":"<base64>","options":{"transactionEncoding":"Base64","recommended":true}}]}
```
Result `priorityFeeEstimate` is micro-lamports per compute unit; multiply by the CU limit for the lamport cost.

## Open questions / unverified

1. Webhooks REST host not shown on any fetched page (paths verified). Confirm `api.helius.xyz` at implementation time.
2. Exact `/v0/transactions` host: enhanced-transactions index states `mainnet.helius-rpc.com`; older integrations used `api.helius.xyz` — verify both resolve.
3. Numeric precision for DAS `balance`/`amount` and Parsed Events `rawTokenAmount` (JSON numbers).
4. No documented 429 headers or per-key vs per-project scoping.
5. Webhook payload size limits and ordering guarantees undocumented.
