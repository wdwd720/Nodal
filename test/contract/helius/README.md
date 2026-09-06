# Helius contract fixtures

Recorded from the shapes in `docs/api/providers/helius.md` (fetched 2026-09-05; overall status PARTIAL). No fixture contains live data. The replay server (`replay_test.go`) serves JSON-RPC bodies verbatim with the `id` rewritten to echo the request, checks the `api-key` query parameter on every HTTP and WebSocket request, and plays the WebSocket fixtures over a real `golang.org/x/net/websocket` handshake.

Everything Helius passes through unchanged (getTransaction, getSignatureStatuses, getSignaturesForAddress, getTokenAccountsByOwner, getBalance, getBlockHeight, getLatestBlockhash, isBlockhashValid, simulateTransaction) is covered by `test/contract/solanarpc`; the Helius adapter is built on the same client, which is the point: both observers parse with identical code and differ only in what the chain shows them.

| Fixture | Surface | Status | Documented fields | Assumed / notes |
|---|---|---|---|---|
| `getTokenAccounts_page1.json` | DAS `getTokenAccounts` (named params `{owner, page, limit, options.showZeroBalance}`) | documented | `total`, `limit`, `page`, `cursor`, `token_accounts[].{address, mint, owner, amount (JSON integer), delegated_amount, frozen}` | `amount` is a JSON **number**: values above 2^53 lose precision in float parsers, so the third account carries 2^64+1 to prove exact parsing. `decimals`, `token_extensions` and a slot are not in the schema: the adapter marks `DecimalsKnown=false`, `Slot=0`. `cursor: null` shape assumed. |
| `getTokenAccounts_float_amount.json` | DAS | **assumed malformed** | — | `amount: 1.0` must be rejected as `VALIDATION_FAILED`; it is never rounded. |
| `getTransaction_v0_swap.json` | RPC pass-through | documented | as in `test/contract/solanarpc` | served with `?api-key=` |
| `getSignatureStatuses_finalized.json`, `getSignaturesForAddress_*.json`, `getTokenAccountsByOwner_empty.json`, `getBalance.json` | RPC pass-through | documented | — | backfill inputs for the stream tests |
| `transactionSubscribe_ack.json` | Enhanced WebSocket | **assumed** | the request shape `[filter, options]` and the `transactionNotification` method name are documented; the acknowledgement is standard JSON-RPC (`result` = subscription id) | — |
| `transactionSubscribe_error.json` | Enhanced WebSocket | **assumed** | plan gating is documented ("Developer/Business/Professional"); the error body is not | the stream must keep polling |
| `transactionNotification.json` | Enhanced WebSocket | **assumed** | `transactionNotification` method name; `commitment`, `encoding`, `transactionDetails` options | the `params.result` payload shape is not on the fetched pages. The adapter reads only `signature` (falling back to `transaction.signatures[0]` / `transaction.transaction.signatures[0]`) and `slot`, then observes the signature through RPC. Any other shape is an unparseable hint that triggers a backfill. |
| `error_401.json` | any | documented (status) | HTTP 401 for a bad/missing key | body shape assumed; never retried; the key never appears in errors or logs |
| `error_429.json` | any | documented (status) | HTTP 429, no documented headers | body shape assumed; retried within the Retry-After cap |

Webhooks are deliberately absent from this adapter: Helius retries a delivery three times one second apart and then "the event is permanently lost", so a webhook can only ever be a hint. The stream tests assert the same rule for WebSocket notifications: the event delivered to the sink is always an RPC observation.
