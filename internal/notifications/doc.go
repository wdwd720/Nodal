// Package notifications is the product notification centre: the record of what
// a person was told, and the only writer of the `notifications` table on the
// product path (product goal §36).
//
// # A notification is a fact of the transaction that caused it
//
// Producer.Emit takes the caller's pgx.Tx and writes inside it. A notification
// therefore exists if and only if the state change it describes committed;
// there is no window in which a person has been told about a purchase that
// rolled back, and none in which a purchase committed and the telling was lost
// to a crash between two transactions. It is idempotent on
// (user, kind, ref, occurrence), collapsed into the table's dedup_key and its
// unique index, so a replayed domain event, a retried webhook and a follower
// pass that re-reads its own window all produce one row.
//
// # It is not the audit trail and it is not financial truth
//
// internal/audit records what the platform did; this records what the customer
// was shown. internal/alert pages an operator; nothing here reaches an
// operator. The ledger holds balances; a notification quotes identifiers and
// state names and the reader refetches canonical REST state.
//
// # There is one delivery channel and the schema says so
//
// No e-mail, SMS or push provider exists in this codebase. Preferences carry
// channel IN_APP only (migration 00782) and a kind the user has switched off
// produces no row at all, except for the kinds Kind.Suppressible reports false
// for -- security, account restriction, a reversed purchase, a failed payout
// and SYSTEM -- which a person may not switch off and still be treated as
// informed.
//
// # This package must never
//
// Compute or restate a balance, run outside the causing transaction, write a
// ledger row, page an operator, or return one user's notifications to another.
package notifications
