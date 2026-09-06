// Package solanarpc is the fallback / canonical Solana JSON-RPC
// ChainObserver (docs/api/providers/solana-rpc.md; goal PARTS 78, 79, 107,
// 175, 196). It speaks plain JSON-RPC 2.0 over HTTP to any Solana RPC vendor
// and is also the transport the Helius adapter builds on, so both observers
// parse transactions with exactly the same code and disagree only when the
// chain views differ.
//
// Behavior fixed by the documentation:
//
//   - every getTransaction sends maxSupportedTransactionVersion: 0 and
//     encoding "json", and resolves lookup-table addresses from
//     meta.loadedAddresses;
//   - token deltas come from uiTokenAmount.amount strings and lamport deltas
//     from preBalances/postBalances, both as exact money.Quantity values;
//     any float, exponent or fractional token is rejected as
//     VALIDATION_FAILED;
//   - reads are SAFE_RETRY: transport failures, 429 and 5xx are retried with
//     full jitter within the caller's context; nothing else is retried;
//   - every response body is archived through chain.RawArchive before it is
//     parsed, and the observation carries the archive reference;
//   - every attempt is a provider.Tracker sample (PART 79);
//   - the endpoint (which may carry a vendor key) is a secret: it never
//     appears in errors, logs or archive metadata.
//
// # What this package must never do
//
//   - Send a transaction. sendTransaction belongs to the execution adapter;
//     Simulate has no chain effect and is the only write-shaped call here.
//   - Parse an amount through floating point or trust uiAmount.
//   - Report a commitment it did not verify: GetTransaction pairs the read
//     with getSignatureStatuses and reports the weaker of the two when the
//     status is missing.
//   - Treat a null result as failure or as proof of absence; it is "not known
//     to this node" and the agreement policy decides what that means.
//   - Retry after a malformed response or a request-side RPC error.
//   - Log the endpoint, the API key or a raw URL error.
package solanarpc
