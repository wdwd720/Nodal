// Package sandboxfund is the sandbox faucet (ADR-0020,
// docs/architecture/ADVANCED_FINANCIAL_MODEL.md §2 "Sandbox funding", §5
// SANDBOX_FUNDING; subsystem SANDBOX_FUNDING; migration 00744): it grants
// SIMULATED value to customer accounts so a deployment can demonstrate the
// advanced capabilities, and it provides the standard sandbox assets other
// packages and the demo build on (EnsureSandboxDollar: SBX-USD, 6 decimals,
// SIMULATED, chain nodal-sandbox, a USD-pegged unit valued at par).
//
// # Responsibilities
//
//   - Fund posts SANDBOX_FUNDING (customer Dr WALLET / Cr CAPITAL) with an
//     economic effect keyed on (account, idempotency key), a write-once
//     evidence document and a grant record, in the caller's transaction.
//   - A versioned, hashed Policy caps what one account may be granted of one
//     asset in a rolling window (24 hours by default), read from the grants
//     table under a per-account transaction lock, so concurrent grants can
//     never exceed it together.
//
// # Where the faucet runs (the environment gate)
//
// The faucet is deliberately NOT a capability gate: it moves no value
// between parties, and there is no live version of it that a gate could one
// day activate. It is gated on the deployment environment instead:
//
//   - LOCAL, TEST, DEV: always on, so a fresh deployment can fund its own
//     demonstration accounts.
//   - STAGING, PROD: off unless deployment configuration passes Enabled =
//     true, because handing play money to the users of a deployed system is a
//     product decision someone owns. Even then the asset must be SIMULATED.
//   - any other environment: off.
//
// # What this package must never do
//
//   - Grant anything but a SIMULATED asset. Fund refuses it, the ledger
//     refuses a non-simulated account in a SANDBOX_FUNDING journal (SM001),
//     and the grant table refuses it again (SF001).
//   - Grant a prediction-market outcome share: a share exists only as its
//     market issues it. Fund refuses it by name (RefusalPredictionShare), and
//     the ledger refuses any SANDBOX_FUNDING journal on a share (PM008).
//   - Grant an asset whose supply a controller owns: a platform stable asset
//     exists only as its issuer mints it against reserve, a bridged twin only
//     as its bridge releases it against a lock. Fund refuses both by name
//     (RefusalSupplyControlled, with the controller), and the ledger refuses
//     any journal of a kind their controllers and customer movements do not
//     use, and any that creates or destroys a unit outside an issuance
//     (SU001, SU002, migration 00763).
//   - Grant beyond the policy cap, or grant twice for one idempotency key.
//   - Accept an agent or an operator as the requester, or let a customer
//     fund an account they do not own.
//   - Record a grant that does not match the journal and the APPLIED effect
//     that made it (SF002), or edit a grant afterwards.
//   - Create a second asset for a standard symbol, or silently reuse one
//     whose decimals or peg differ from the standard.
//   - Hold mutable state at package level, read the wall clock or use
//     floating point.
package sandboxfund
