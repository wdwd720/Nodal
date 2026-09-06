# Solana JSON-RPC (fallback / canonical chain source) — verified integration notes

Role in this platform: **ChainObserver + SolanaDataProvider fallback**, and the canonical definitions that every Solana adapter must honour (commitment, expiry, versioned tx, Token-2022).
Overall status: **VERIFIED_FROM_OFFICIAL_DOCS** for all listed methods and semantics; JSON-RPC numeric error codes **UNVERIFIED** (not on fetched official pages).

## Verification record

All fetched 2026-09-05.

| Topic | URL | Result |
|---|---|---|
| RPC overview / commitment | https://solana.com/docs/rpc | OK |
| HTTP request format | https://solana.com/docs/rpc/http | OK |
| JSON structures | https://solana.com/docs/rpc/json-structures | OK |
| getTransaction | https://solana.com/docs/rpc/http/gettransaction | OK |
| simulateTransaction | https://solana.com/docs/rpc/http/simulatetransaction | OK |
| sendTransaction | https://solana.com/docs/rpc/http/sendtransaction | OK |
| getSignatureStatuses | https://solana.com/docs/rpc/http/getsignaturestatuses | OK |
| getLatestBlockhash | https://solana.com/docs/rpc/http/getlatestblockhash | OK |
| isBlockhashValid | https://solana.com/docs/rpc/http/isblockhashvalid | OK |
| getBlockHeight | https://solana.com/docs/rpc/http/getblockheight | OK |
| getBalance | https://solana.com/docs/rpc/http/getbalance | OK |
| getTokenAccountsByOwner | https://solana.com/docs/rpc/http/gettokenaccountsbyowner | OK |
| getSignaturesForAddress | https://solana.com/docs/rpc/http/getsignaturesforaddress | OK |
| Transaction confirmation / expiry | https://solana.com/docs/core/transactions/confirmation | OK |
| Versioned transactions | https://solana.com/docs/core/transactions/versions | OK |
| Address lookup tables | https://solana.com/docs/advanced/lookup-tables | OK |
| Token-2022 extension list | https://solana.com/docs/tokens/extensions | OK |
| Token-2022 behaviours + program ID | https://www.solana-program.com/docs/token-2022/extensions | OK |
| Token program + ATA IDs | https://www.solana-program.com/docs/token | OK |
| ATA program ID | https://raw.githubusercontent.com/solana-program/associated-token-account/main/README.md | OK |
| ALT program ID | https://raw.githubusercontent.com/solana-program/address-lookup-table/main/README.md | OK |
| No program IDs found | https://solana.com/docs/core/programs , https://solana.com/docs/tokens , https://solana.com/docs/tokens/basics | fetched; IDs not printed |

## Transport and envelope

JSON-RPC 2.0 over HTTP POST, `Content-Type: application/json`, body `{jsonrpc:"2.0", id, method, params:[...]}`. Most reads return `{context:{slot:u64, apiVersion:string}, value:...}`. `minContextSlot` (number) on most methods = "the minimum slot that the request can be evaluated at" (node returns an error if behind). u64 fields (lamports, slots, block heights) are JSON numbers — safe below 2^53 for lamports today but decode into uint64, not float64.

## Auth and secrets

The JSON-RPC protocol itself has no authentication; access control is entirely provider-side (Helius query-string key, other providers' URL tokens or headers). Treat the full RPC URL as a secret. Never send private keys to an RPC — `sendTransaction` takes a fully-signed transaction; signing happens in the wallet/signing provider (see privy.md).

## Commitment semantics (verbatim definitions)

- `processed`: "The node's most recent processed block. This is the newest view, but it can still be rolled back."
- `confirmed`: "A block directly voted on by a supermajority of stake, meaning more than two-thirds of the network's active stake."
- `finalized`: "A block the cluster recognizes as finalized with maximum lockout."
- Default when omitted: "typically `finalized`". `getTransaction` and `getSignaturesForAddress` **do not accept `processed`**.
- Rule from docs: "ALWAYS set the `preflightCommitment` parameter to the same commitment level used to fetch your transaction's blockhash." Fetch blockhashes at `confirmed` ("very low chance of belonging to a dropped fork").

## Transaction expiry

- BlockhashQueue stores the 300 most recent blockhashes; a tx is valid only if its blockhash is within the most recent **151** ("max processing age", 0-indexed) — "about 60 to 90 seconds" at 400-600 ms slots.
- `getLatestBlockhash.value.lastValidBlockHeight` (u64) is the last **block height** (not slot) at which the blockhash is valid. Expiry check: poll `getBlockHeight` at `confirmed` until it exceeds `lastValidBlockHeight`, or call `isBlockhashValid`. Block height < slot because skipped slots produce no block.
- Durable nonces are the documented alternative for offline signing / long-lived intents.
- Resend strategy: "Keep resending a transaction to a RPC node on a frequent interval" until confirmed or expired.

## Endpoints and schemas

### getTransaction(signature, config)
Config: `commitment` confirmed|finalized (default finalized), `maxSupportedTransactionVersion` (0 or **1** — docs now list 1 "to include v1 transactions"; "If you omit this parameter, only legacy transactions will be returned — any versioned transaction will result in an error"), `encoding` json (default)|jsonParsed|base64|base58|binary. Result null or: slot (u64), blockTime (i64|null), version ("legacy"|number; omitted if param unset), transaction (object or [string, encoding]), meta: err (object|string|null; null = success), fee (u64 lamports), preBalances/postBalances (u64[] indexed to accountKeys), innerInstructions (array|null), preTokenBalances/postTokenBalances (array|null) items `{accountIndex:u8, mint, owner, programId, uiTokenAmount{amount: **string**, decimals:u8, uiAmount: number|null, uiAmountString: string}}`, logMessages, rewards, loadedAddresses `{writable[], readonly[]}` (resolved ALT addresses), returnData, computeUnitsConsumed (u64), costUnits (u64). **Use `uiTokenAmount.amount` (string) for exact accounting; never `uiAmount`.**

### getSignatureStatuses([signatures<=256], {searchTransactionHistory})
Default searches only the recent status cache ("all active slots plus MAX_RECENT_BLOCKHASHES rooted slots"); set `searchTransactionHistory:true` to hit the ledger. Value items null or `{slot, confirmations: usize|null (null = finalized), err, confirmationStatus: processed|confirmed|finalized|null, status (deprecated)}`. A `null` entry means unknown/not seen — not failed.

### getLatestBlockhash / isBlockhashValid / getBlockHeight / getBalance
- getLatestBlockhash({commitment, minContextSlot}) -> `{blockhash: string, lastValidBlockHeight: u64}`.
- isBlockhashValid(blockhash, {commitment, minContextSlot}) -> bool.
- getBlockHeight({commitment, minContextSlot}) -> u64 "Block height the node used to answer this request."
- getBalance(pubkey, {commitment, minContextSlot}) -> value u64 lamports.

### simulateTransaction(tx, config)
Config: commitment (default finalized; simulate at the same level you will send), encoding base64 (recommended)|base58|binary(deprecated), `sigVerify` (bool) **conflicts with** `replaceRecentBlockhash` (bool), minContextSlot, `innerInstructions` (bool -> CPI list), `accounts{addresses[], encoding}`. Result: err (null = ok), logs[], accounts|null, unitsConsumed, returnData, innerInstructions (null unless requested), replacementBlockhash `{blockhash, lastValidBlockHeight}` when replaced. Use for CU-limit sizing (Jupiter /build requires it).

### sendTransaction(signedTx, config)
Config: encoding base58 (default, capped 1232 bytes)|base64 (recommended), `skipPreflight` (default false), `preflightCommitment` (default finalized), `maxRetries` (usize; RPC-side rebroadcast attempts), `minContextSlot`. Result: first signature (base58). Verbatim: "A successful response from this method does not guarantee the transaction is processed or confirmed by the cluster." Preflight = signature verification + simulation at preflightCommitment. The RPC "will reasonably retry" until the blockhash expires. Confirm with getSignatureStatuses.

### getTokenAccountsByOwner(owner, filter, config)
Filter `{mint}` **or** `{programId}` — a wallet can hold accounts under both Token and Token-2022; query each program ID (or by mint) to get all balances. Config: commitment, minContextSlot, dataSlice, encoding (jsonParsed for structured). jsonParsed item: `{pubkey, account{lamports, owner, executable, rentEpoch, space, data{program, parsed{info{mint, owner, state, isNative, tokenAmount{amount:string, decimals, uiAmount, uiAmountString}, delegate, delegatedAmount, closeAuthority, extensions[] (Token-2022)}}, space}}}`.

### getSignaturesForAddress(address, config)
Config: commitment confirmed|finalized, minContextSlot, limit 1-1000 (default 1000), `before` (search backwards from), `until`. Items `{signature, slot, err, memo, blockTime|null, confirmationStatus}` ordered **newest to oldest**. Page with `before` = last signature of the previous page.

## Versioned transactions and lookup tables

- Versions: `legacy` and `0` ("added support for Address Lookup Tables"); docs also now mention v1 in `maxSupportedTransactionVersion`. "All RPC requests that return a transaction should specify ... `maxSupportedTransactionVersion`" — requests **WILL** fail on v0 txs otherwise. Always send 0 (or the highest you can decode).
- Legacy tx address list is "effectively capped at 32 addresses"; ALTs raise it to 64 per tx; one table holds up to 256 addresses; addresses referenced by 1-byte index; requires v0. Decoding/simulating a v0 tx needs the ALT account contents (`getAddressLookupTable`); `getTransaction.meta.loadedAddresses` gives resolved addresses. Policy engines that only read static keys (e.g. Privy) cannot see ALT-resolved accounts.
- Address Lookup Table program: `AddressLookupTab1e1111111111111111111111111`.

## Token programs and Token-2022 extensions

Program IDs: SPL Token `TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA`; Token-2022 `TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb`; Associated Token Account `ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL`. Token docs warn: "when the source and destination of a `Transfer` are the same, the `Transfer` will always succeed" — validate distinct accounts.

Full Token-2022 extension list (solana.com): TransferFeeConfig, TransferFeeAmount, MintCloseAuthority, ConfidentialTransferMint, ConfidentialTransferAccount, DefaultAccountState, ImmutableOwner, MemoTransfer, NonTransferable, InterestBearingConfig, CpiGuard, PermanentDelegate, TransferHook, MetadataPointer, TokenMetadata, GroupPointer, TokenGroup, GroupMemberPointer, TokenGroupMember, ConfidentialMintBurn, ScaledUiAmount, Pausable.

Extensions that change transfer semantics (behaviour quotes from solana-program.com):

| Extension | Effect on an adapter |
|---|---|
| Transfer Fee | "some amount is withheld on the recipient account"; transfers "require using `transfer_checked` or `transfer_checked_with_fee`". Received amount < sent amount; reconcile from post balances. |
| Transfer Hook | Program CPI on every transfer; "extra accounts ... automatically resolved on-chain" — tx needs the hook's extra accounts; hook may reject. |
| Confidential Transfer | Balances/amounts encrypted on-chain; standard balance reads do not reflect them. Treat as unsupported. |
| Permanent Delegate | Authority "can burn or transfer any amount of tokens" from any account — custody is not exclusive. |
| Non-Transferable | "soul-bound" — transfers fail. |
| Default Account State | New accounts frozen until thawed — first deposit to a fresh ATA may be frozen. |
| CPI Guard | Under CPI, "the signing authority must be the account delegate"; routes that CPI-transfer from user ATAs can fail. |
| Interest-Bearing / Scaled UI Amount | "entirely cosmetic": raw amount never changes; `uiAmount` differs from raw/10^decimals. Account on raw amounts. |
| Required Memo (MemoTransfer) | "incoming transfers must have an accompanying memo instruction right before the transfer". |
| Pausable | "aborts all transfers, mints, and burns when the `paused` flag is flipped". |
| Immutable Owner | Ownership cannot be reassigned (safety property for ATAs). |

Read `parsed.info.extensions[]` on token accounts and mint extensions before routing any Token-2022 mint; deny-list Transfer Hook, Confidential Transfer, Permanent Delegate, Non-Transferable, Pausable by default.

## Errors, rate limits, retries

`meta.err` shapes: simple string (e.g. `"BlockhashNotFound"`) or variant object `{"InstructionError":[index, error]}`. Numeric JSON-RPC error codes (-32002 blockhash not found/simulation failed, -32004 block not available, -32005 node unhealthy, -32602 invalid params) appear only in third-party references (Quicknode, Syndica) — **UNVERIFIED against solana.com**; treat any `-32xxx` on send as retryable only after a fresh simulate. Rate limits are provider-specific (see helius.md); public `api.mainnet-beta.solana.com` is not for production.

Write classification: `sendTransaction` = **UNKNOWN_EFFECT_WRITE** (chain-idempotent per signature; re-sending the identical signed bytes is safe until expiry; re-signing with a new blockhash before the first has provably expired risks double execution). All other listed methods = SAFE_RETRY (`simulateTransaction` has no state effect).

## Webhooks/streams

No webhooks in the protocol. Standard WebSocket subscriptions (https://solana.com/docs/rpc/websocket, listed in the Helius method index fetched 2026-09-05): `accountSubscribe`, `blockSubscribe`, `logsSubscribe`, `programSubscribe`, `rootSubscribe`, `signatureSubscribe`, `slotSubscribe`, `slotsUpdatesSubscribe`, `voteSubscribe`. `signatureSubscribe` fires once at the requested commitment and is the cheapest confirmation primitive; subscriptions are lost on disconnect with no replay, so every stream consumer needs a polling backfill (`getSignaturesForAddress` / `getSignatureStatuses`).

## Sandbox/test availability

Devnet (`https://api.devnet.solana.com`, provider devnet endpoints) and testnet; `solana-test-validator` locally. Jupiter routes do not exist on devnet, so end-to-end swap tests need mainnet with small amounts.

## Open questions / unverified

1. Official numeric JSON-RPC error-code table not located on solana.com.
2. Transaction "v1" — referenced by `maxSupportedTransactionVersion` docs; format details not fetched.
3. Confidential Transfer behaviour statements not present on the fetched page.
