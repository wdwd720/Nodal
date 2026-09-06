// Package accounts holds customer, canary, and platform accounts and their
// explicit status state machine (ACTIVE, RESTRICTED, FROZEN, CLOSED).
//
// A frozen account accepts no new risk and no withdrawals unless compliance
// or an operator permits, while settlement, reconciliation, and ledger
// posting for existing activity continue (PART 194). Every status change is
// recorded with actor and reason and audited.
//
// This package must never: expose a balance-editing operation, derive
// ownership from client-supplied headers, or let an AGENT actor change
// account status.
package accounts
