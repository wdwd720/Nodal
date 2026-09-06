// Package ledger is the append-only, per-asset double-entry ledger of the
// control plane: the internal accounting truth (FINANCIAL_MODEL §1) for what
// every customer, and the platform itself, is entitled to, in exact asset
// base units (PARTS 19-21).
//
// # Model
//
// A LedgerAccount is (owner_type, owner_id, code, asset_id) and holds exactly
// one asset. A Posting describes a JournalTransaction: a kind, an idempotency
// key, the financial event it records, and at least two entries. Every asset
// inside a transaction balances independently:
//
//	∀ posted transaction T, ∀ asset A in T:  Σ debit(T,A) == Σ credit(T,A)
//
// Balances are kept in normal-side terms (a DEBIT-normal account grows with
// debits) by the database trigger of migration 00101, inside the posting
// transaction; the application never writes ledger_balances. USD figures on
// entries are valuation metadata and are never balanced.
//
// # Posting
//
// Service.Post validates a Posting (balanced per asset, positive quantities,
// known kind and codes, non-empty idempotency key, platform owner id,
// reversal_of resolvable), resolves ledger accounts lazily, orders entries by
// ledger_account_id so balance-row locks are taken in one global order, and
// inserts the header and entries. It is idempotent: the same idempotency key
// with the same content hash returns the existing transaction with
// PostResult.Existing set; the same key with different content fails with
// INVALID_IDEMPOTENCY_REUSE.
//
// Balanced-per-asset is enforced a second time by a deferred constraint
// trigger evaluated at COMMIT. Callers therefore run Post inside db.InTx and
// treat a commit error as a posting failure; MapError turns the SQLSTATEs
// LG001-LG005 into LEDGER_* codes. Service.PostInTx wraps that pattern for a
// single posting, including the deadlock/serialization retry of db.InTx.
//
// # What this package must never do
//
//   - Update or delete a posted journal row. Corrections are new transactions
//     with reversal_of set (kind COMPENSATION or CORRECTION) and a reason code.
//     The database triggers and role grants enforce the same rule.
//   - Balance one asset against another. Each asset balances alone; USD
//     valuations attached to entries are metadata, not balances.
//   - Expose a generic mutation. The only writes are Post (a balanced,
//     idempotent journal transaction) and the lazy creation of ledger
//     accounts; there is no balance edit, no entry edit, no delete.
//   - Use binary floating point for any quantity. All arithmetic is
//     money.Quantity; the nofloat test scans the sources.
//   - Accept a posting from an AGENT principal: agents propose, deterministic
//     services post.
//   - Hold connections, configuration or mutable state at package level.
package ledger
