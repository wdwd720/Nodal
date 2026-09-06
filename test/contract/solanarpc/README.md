# Solana JSON-RPC contract fixtures

Recorded from the shapes in `docs/api/providers/solana-rpc.md` (verified against solana.com on 2026-09-05). No fixture contains live data; identifiers are well-formed base58 placeholders. The replay server (`replay_test.go`) serves each body verbatim except that the JSON-RPC `id` is rewritten to echo the request id, exactly as a node does.

Every fixture is labeled below as **documented** (field and type read from the official page) or **assumed** (shape inferred from third-party references or chosen to exercise a rejection path). Assumed fields never carry money semantics; the adapter treats every deviation from a documented type as `VALIDATION_FAILED`.

| Fixture | Method | Status | Documented fields | Assumed / notes |
|---|---|---|---|---|
| `getTransaction_v0_swap.json` | getTransaction (v0, `maxSupportedTransactionVersion: 0`, `encoding: json`) | documented | `slot`, `blockTime`, `version` (number 0), `transaction.signatures`, `transaction.message.accountKeys`, `meta.err`, `meta.fee`, `meta.preBalances/postBalances` (u64 indexed to account keys), `meta.preTokenBalances/postTokenBalances[].{accountIndex, mint, owner, programId, uiTokenAmount{amount (string), decimals, uiAmount, uiAmountString}}`, `meta.loadedAddresses{writable, readonly}`, `meta.logMessages`, `meta.computeUnitsConsumed` | `addressTableLookups` and `instructions` content is illustrative; the adapter ignores them (the inspector decodes instructions). `uiAmount` floats are present as documented and never read. |
| `getTransaction_null.json` | getTransaction | documented | `result: null` = not found by this node (never proof of absence) | — |
| `getTransaction_failed.json` | getTransaction | documented | `meta.err` object form `{"InstructionError":[index, error]}`; a failed transaction still pays `fee` and is still "found" | `status` (deprecated field) present as documented; ignored |
| `getTransaction_float_amount.json` | getTransaction | **assumed malformed** | — | `uiTokenAmount.amount` is a JSON float instead of the documented string. Must be rejected. |
| `getSignatureStatuses_confirmed.json` | getSignatureStatuses (`searchTransactionHistory: true`) | documented | `context.slot`, `value[].{slot, confirmations (usize), err, confirmationStatus}` | `status` deprecated field present; ignored |
| `getSignatureStatuses_finalized.json` | getSignatureStatuses | documented | `confirmations: null` = finalized (rooted) | — |
| `getSignatureStatuses_unknown.json` | getSignatureStatuses | documented | `value: [null]` = unknown / not seen, never "failed" | — |
| `getSignaturesForAddress_page1.json`, `_page2.json`, `_empty.json` | getSignaturesForAddress | documented | items `{signature, slot, err, memo, blockTime|null, confirmationStatus}` ordered newest to oldest; page with `before` = last signature | page 2 is deliberately older than the test's `since` to exercise the stop rule (blockTime is the chain clock; untrusted, ordering only) |
| `getTokenAccountsByOwner_usdc.json`, `_empty.json` | getTokenAccountsByOwner (`jsonParsed`) | documented | `context.slot`, `value[].{pubkey, account{lamports, owner, executable, rentEpoch, space, data{program, parsed{type, info{mint, owner, state, isNative, tokenAmount{amount (string), decimals, uiAmount, uiAmountString}}}}}}` | `rentEpoch` uses the u64 max sentinel seen on mainnet; parsed as a number and ignored |
| `getBalance.json` | getBalance | documented | `context.slot`, `value` (u64 lamports) | — |
| `getBlockHeight.json` | getBlockHeight | documented | bare u64 block height (not a slot) | — |
| `getLatestBlockhash.json` | getLatestBlockhash | documented | `value.{blockhash, lastValidBlockHeight (u64 block height)}` | — |
| `isBlockhashValid.json` | isBlockhashValid | documented | `value` bool | — |
| `simulateTransaction_ok.json`, `_failed.json` | simulateTransaction (`encoding: base64`) | documented | `value.{err, logs[], accounts|null, unitsConsumed, returnData, innerInstructions}` | inner instruction items use the compiled `{programIdIndex, accounts, data, stackHeight}` shape; the parsed `{programId}` shape is also accepted. Index resolution needs the caller's resolved account keys; unresolved indexes are reported as `unresolved:<n>` so an inspector fails closed. |
| `error_429.json` | any | **assumed** | HTTP 429 is documented for Helius; the body shape and the `Retry-After` header are not documented anywhere | the adapter classifies by HTTP status only; the body is archived, never parsed for decisions |
| `error_503.json` | any | **assumed** | HTTP 5xx retry semantics (SAFE_RETRY reads) | non-JSON-RPC body; archived verbatim |
| `rpc_error_invalid_params.json` | any | **assumed** | JSON-RPC 2.0 error envelope is documented; numeric code `-32602` is unverified against solana.com | request-side codes are never retried |
| `rpc_error_node_unhealthy.json` | any | **assumed** | numeric code `-32005` and `data.numSlotsBehind` come from third-party references only | treated as retryable; classification does not depend on `data` |

## What the contract tests assert

- Exact token deltas from `uiTokenAmount.amount` strings and lamport deltas from `preBalances`/`postBalances`, with Σ lamport deltas = −fee.
- Lookup-table addresses (`loadedAddresses`) are appended to the static keys in the documented order, so `accountIndex` 4 resolves to the first writable loaded address.
- `maxSupportedTransactionVersion: 0` and `encoding: json` on every getTransaction request.
- Commitment reported from getSignatureStatuses: confirmed vs finalized vs unknown.
- Malformed numbers (`float_amount`) are rejected, archived, and never retried.
- Timeouts and 5xx become `PROVIDER_UNAVAILABLE`, 429 becomes `RATE_LIMITED` (retried within the Retry-After cap), every attempt is a health sample, every body is archived.
- Paging of getSignaturesForAddress with `before`, stopping at the `since` bound.
