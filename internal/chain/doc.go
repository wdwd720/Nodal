// Package chain is the provider-neutral Solana chain-observation layer
// (EXECUTION.md §6, RECONCILIATION.md §3–4, POINT_IN_TIME.md §1 and §5; goal
// PARTS 78, 79, 107, 175, 196, 199, 200).
//
// It defines the contracts every observer implements (ChainObserver,
// SolanaDataProvider, RawArchive), the value types they exchange
// (TxObservation, BalanceObservation, SimulationResult, Finality), the
// AgreementPolicy that decides what two independent observers together are
// allowed to assert, and MultiObserver, which queries a primary and a
// secondary observer with per-call timeouts, samples their health, and applies
// the policy. Concrete adapters live in internal/provider/helius and
// internal/provider/solanarpc; scripted fakes and a slot-advancing chain
// simulator live in chain/chaintest.
//
// # Timestamps (PART 175)
//
// Every observation carries two platform-clock timestamps: ObservedAt (the
// instant the query was issued) and ReceivedAt (the instant the full response
// was in hand). Provider and chain clocks — BlockTime, a stream's
// ProviderPublishedAt — are stored next to them, labeled untrusted, and are
// never inputs to knowledge-time logic.
//
// # What this package must never do
//
//   - Finalize on one provider's word. FINALIZED requires both observers to
//     have found the transaction at finalized commitment with identical
//     economics; a single observer, however healthy, caps finality at
//     CONFIRMED.
//   - Choose the optimistic answer. Any difference in found-ness, slot, error,
//     fee, token deltas or lamport deltas is DISAGREED and blocks dependent
//     activity; commitment-level conflicts resolve to the lower level.
//   - Treat streams or webhooks as truth. A stream notification is a hint that
//     a signature exists; the observation of record always comes from an RPC
//     read that was archived before it was parsed.
//   - Prove absence with one observer. NOT_FOUND from a degraded (single
//     observer) query is block-dependent: recovery may not build a replacement
//     attempt on it.
//   - Use floating point. Every amount is a money.Quantity of base units.
//   - Stop observing because a provider is UNHEALTHY. Only an operator DISABLE
//     removes an observer from the query set (PART 107); an unhealthy
//     observer's answer can lower confidence but never raise it.
//   - Log or embed provider credentials. Endpoints are secrets; adapters
//     redact them from every error and log line.
package chain
