// Package reconciliation compares internal accounting truth with external
// truth and turns every difference into a first-class record with an explicit
// lifecycle (PARTS 21, 48-52, 163, 195, 196; docs/architecture/RECONCILIATION.md).
//
// It owns three comparison modes — EVENT_DRIVEN (after an execution or funding
// action), PERIODIC (provider/account windows) and FULL (complete balance
// comparison) — the PART 51 record state machine, the PART 48
// unknown-submission recovery path, the PART 21 internal consistency
// verifiers, and the PART 195 financial repair path.
//
// What this package must never do:
//
//   - Stop for a kill switch. Kill switches halt new risk; reconciliation,
//     settlement, observation and ledger posting are never blocked (PART 52).
//     Nothing here calls killswitch.Checker for permission; the
//     killswitch.Reconcile action class exists precisely to prove that.
//   - Let an agent resolve anything. Every resolution path refuses
//     security.ActorAgent before any query, and the database refuses it again
//     (reconciliation_records.resolved_by_actor_type CHECK).
//   - Edit a balance or overwrite a position. Financial repair is a
//     compensating journal transaction referenced from the record; there is no
//     balance-edit path in this system (PART 195).
//   - Resolve a material mismatch without operator, reason, evidence and an
//     approved admin_actions row (PART 51).
//   - Submit anything. Recovery queries providers and the chain; it never
//     retries a money movement blindly (PART 46).
//
// Exactly-once is the whole point of the recovery path: the same external fill
// discovered any number of times persists one fill (UNIQUE (venue,
// external_fill_id)), one position change (fills.position_applied_at set once)
// and one set of ledger entries (ledger idempotency key "fill:<id>").
package reconciliation
