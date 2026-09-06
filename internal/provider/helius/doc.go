// Package helius is the primary Solana data provider
// (docs/api/providers/helius.md; goal PARTS 78, 79, 107, 175, 196, 199,
// 200). It implements chain.SolanaDataProvider on top of the Helius RPC
// endpoint through internal/provider/solanarpc — so Helius and the fallback
// RPC parse transactions with the same code and disagree only when the
// chain views differ — and adds what is Helius-specific:
//
//   - authentication by api-key query parameter, resolved once from
//     config.SecretRef and never logged (the endpoint is a secret);
//   - DAS getTokenAccounts for balances (JSON-number amounts parsed as exact
//     integers, floats rejected; decimals and slot are not reported by DAS
//     and are marked unknown);
//   - StreamWalletEvents: the Enhanced WebSocket transactionSubscribe as a
//     hint source plus a periodic polling backfill over SearchWalletActivity.
//     A notification only tells the stream which signature to look at; the
//     event delivered to the sink is always an RPC observation of record,
//     archived and status-stamped. Reconnects are announced with
//     EventReconnect and followed by a backfill, because the socket has no
//     replay (helius.md, "Webhooks/streams").
//
// WebSocket transport: golang.org/x/net/websocket (already pinned through
// x/net). Client-initiated keepalive uses WebSocket ping frames as the
// documentation asks; a read timeout without any frame forces a reconnect.
//
// # What this package must never do
//
//   - Treat a stream notification, a webhook or an Enhanced/Parsed
//     Transactions payload as truth. Only RPC reads produce observations;
//     Parsed Events amounts are JSON numbers and are not used for accounting.
//   - Finalize on its own word. The agreement policy in internal/chain
//     requires the independent fallback observer for FINALIZED.
//   - Assume the socket delivered everything. Every (re)connect and every
//     poll interval backfills through getSignaturesForAddress.
//   - Send a transaction (sendTransaction / Sender belong to execution).
//   - Log or embed the api key, the WebSocket URL or a raw dial error.
//   - Parse an amount through floating point.
package helius
