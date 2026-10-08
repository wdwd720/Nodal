// Package econ is the shared economic kernel of the advanced financial
// capabilities (ADR-0020,
// docs/architecture/ADVANCED_FINANCIAL_MODEL.md).
//
// Every advanced subsystem -- stable assets, margin, derivatives, equities,
// orders, transfers, treasury, fees, the strategy marketplace, the code
// sandbox, delegated agent authority, withdrawals, bridges and prediction
// markets -- reaches the database through the same five things defined here,
// so that "every economic operation is exactly once, typed, authorized,
// recorded and evidenced" is one implementation rather than fifteen.
//
// # Responsibilities
//
//   - IntentKind and Intent are the small typed family every advanced economic
//     operation is expressed in (AcquireExposure, TransferValue, Borrow, ...).
//     IntentStore persists them idempotently on (account, idempotency key) with
//     a canonical content hash, and moves them through an explicit status
//     machine whose every step writes a transition row, an outbox event and an
//     audit event in the caller's transaction.
//   - EffectStore is the exactly-once register. Every mutation that produces
//     an economic effect begins by claiming the effect's deterministic key; a
//     replay with the same request returns the first outcome and a replay with
//     a different request is refused. An effect that posts to the ledger is not
//     APPLIED until it names its journal transaction (enforced in SQL), an
//     effect whose outcome is unknown is UNKNOWN and is resolved by
//     reconciliation, never by a blind retry, and a reversal is a compensating
//     journal transaction recorded as COMPENSATED, never an edit.
//   - Authorizer decides whether a capability may be exercised in a mode in this
//     deployment (gates.DecideMode over the gate Checker and the kill switches).
//     LIVE is refused for every advanced capability in this build.
//   - Facilities and house books are the segregated owners the ledger's
//     FACILITY and HOUSE accounts belong to.
//   - Evidence writes a canonical, hashed evidence document into the
//     write-once evidence archive inside the caller's transaction.
//
// # What this package must never do
//
//   - Move value itself. It records intents and effects and links them to the
//     journal transactions other packages post; it never posts.
//   - Accept a LIVE intent: Mode must be PAPER, SANDBOX or DEVNET, and the
//     database refuses anything else.
//   - Let an effect change identity, skip a state, or become APPLIED without
//     its journal transaction.
//   - Retry an effect whose outcome is UNKNOWN. Only a resolution that has
//     observed the outcome may move it to APPLIED or FAILED.
//   - Hold connections, configuration or mutable state at package level, read
//     the wall clock implicitly, or use floating point.
package econ
