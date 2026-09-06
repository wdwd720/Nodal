// Package notification records customer-facing notifications (PART 193):
// funding available/failed/reversed, trade filled/failed, agent paused, risk
// limit hit, security session events.
//
// A notification is written in the same transaction as the domain event that
// caused it (so it can never describe something that did not happen) and is
// delivered asynchronously through a NotificationProvider. Delivery is
// best-effort and idempotent; the row is the record of truth for "what the
// customer was told", never for financial state.
//
// This package must never: compute or restate balances, run outside the
// causing transaction, or expose one user's notifications to another.
package notification
