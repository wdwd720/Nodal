// Package capital owns the internal accounting truth for capital control:
// asset reservations (PART 22), the per-(account, asset) reservation totals
// row that serializes them, withdrawal holds (PART 27) and capital envelopes
// (PART 24). It is the only place a reservation is created, consumed,
// released or expired, and the only place an envelope's authority fields
// change.
//
// # Reservation algorithm (FINANCIAL_MODEL §3)
//
// Reserve runs inside the caller's PostgreSQL transaction:
//
//  1. upsert then SELECT ... FOR UPDATE the asset_reservation_totals row for
//     (account, asset) — this lock serializes every reservation on that pair;
//  2. available = ledger WALLET balance − totals.reserved − Σ active
//     withdrawal holds; require available ≥ requested (exact money.Quantity
//     arithmetic) else INSUFFICIENT_BUYING_POWER;
//  3. when an envelope is named: SELECT ... FOR UPDATE the envelope, require
//     it to be ACTIVE inside its effective window with available_usd ≥
//     usd_minor, then move available → reserved;
//  4. insert the reservation (idempotent on idempotency_key), bump totals,
//     append the outbox event through the injected Emitter.
//
// Read committed plus those two row locks is rigorously equivalent to
// SERIALIZABLE for this access pattern; the torture test runs both.
//
// Lifecycle: ACTIVE → CONSUMED | RELEASED | EXPIRED, nothing else. Consume is
// legal only from ACTIVE, so a released reservation can never support
// execution. Expiry never touches a reservation whose locked_by_order_id is
// set.
//
// # Authority
//
// Envelope authority fields (allocation, limits, allowlists, policy version,
// status, validity window) change only through EnvelopeService, which
// requires a non-agent security.Principal scoped to the account and writes a
// capital_envelope_changes audit row for every change. The reserve / consume
// / release path is the only agent-reachable path and cannot touch authority
// fields. ApplyRealizedPnL may flip an ACTIVE envelope to EXHAUSTED when a
// loss limit is hit: that is a limit enforcement driven by ledger facts, not
// an authority change, and it is recorded with a SYSTEM actor.
//
// # This package must never
//
//   - reserve, consume, release or expire outside a PostgreSQL transaction:
//     every mutating method takes the caller's pgx.Tx and the outbox row is
//     written in that same transaction;
//   - use Redis, process memory, Temporal state or any cache as reservation
//     or envelope truth (PART 22: no Redis lock, no process mutex, no
//     optimistic local cache);
//   - let an AGENT actor create an envelope or change any authority field,
//     or let an envelope's available + reserved + deployed exceed its
//     allocation (no agent can increase its own capital);
//   - represent a quantity or a USD amount with floating point; everything
//     is money.Quantity / money.USD with checked arithmetic;
//   - import internal/ledger: WALLET balances are read directly from
//     ledger_balances / ledger_accounts, never written;
//   - compute or cache buying power: buyingpower_types.go only declares the
//     output contract for the engine that lives elsewhere.
package capital
