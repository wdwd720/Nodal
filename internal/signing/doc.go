// Package signing is the bounded signing boundary (goal PARTS 34, 44, 95,
// 96, 97, 152, 190; EXECUTION.md §3): the only component that may ask a
// wallet.SigningProvider to sign, and it does so only after an independent
// inspection of the transaction against persisted, approved state.
//
// # Responsibility
//
//   - Service.Sign receives identifiers (attempt, plan, intent, risk
//     decision, wallet) plus the exact unsigned bytes and their hash. It
//     re-loads the execution attempt, approved plan, quote, trade intent,
//     asset reservation, risk decision, wallet and assets from Postgres,
//     validates that they link to each other and are in signable states,
//     rebuilds inspect.Expectations from those rows only (caller-supplied
//     hashes and expectations are compared, never trusted), recomputes the
//     transaction hash, runs inspect.Inspect, and records a signing_decisions
//     row together with an audit event in one transaction. Only an APPROVED
//     decision reaches the provider; the provider is called under the
//     attempt's row lock with idempotency key = attempt id, and the signed
//     bytes are persisted in signing_results. A repeated Sign for the same
//     attempt returns the recorded decision (Replayed) and never signs
//     twice. Rejections emit a security_events row (kind signing_rejection).
//   - Chain facts the inspector needs but must not fetch itself (current
//     block height, lookup-table contents, the persisted simulation result)
//     come from injected sources; in STAGING/PROD every source is mandatory
//     and caller-supplied lookup tables are ignored.
//   - New fails closed: STAGING/PROD/DEV refuse to start unless the wallet
//     provider's capability probe reports delegated signing VERIFIED
//     (PART 96), simulation is required, and a lookup-table source exists.
//   - Server adapts Service to the generated gRPC contract
//     (proto/controlplane/signing/v1); NewInProcessClient satisfies the
//     generated client interface without a network so the executor is
//     written against the future process boundary today.
//
// # What this package must never do
//
//   - Sign without inspection, or sign after a REJECTED decision.
//   - Trust anything in the request beyond identifiers and the bytes to be
//     signed; expectations are rebuilt from persisted rows.
//   - Log transaction bytes, signed bytes, provider secrets or keys. Logs
//     carry ids, hashes and reason codes only.
//   - Update or delete a decision (the table is immutable) — outcomes are
//     new rows.
//   - Be imported by cmd/agent-worker, internal/agent or internal/strategy
//     (depguard, lintfin and TestAgentBoundaryNeverImportsSigning).
//   - Import internal/execution or internal/settlement: the boundary reads
//     their tables through its own repository so it cannot inherit their
//     in-memory state.
package signing
