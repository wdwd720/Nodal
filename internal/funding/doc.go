// Package funding owns the deposit lifecycle (goal PARTS 27–31, 162): the
// state machine of FINANCIAL_MODEL §4, the postings of §2.2 (FUNDING_SETTLED
// on reconciliation; FUNDING_REVERSAL and FUNDING_REVERSAL_DEFICIT on a
// provider reversal), the availability and reversibility policy flags, and
// the provider-neutral contracts a fiat-to-crypto onramp adapter implements.
//
// A deposit moves CREATED → SESSION_CREATED → CUSTOMER_ACTION_REQUIRED →
// PROVIDER_PROCESSING → PROVIDER_CONFIRMED → SETTLEMENT_OBSERVED →
// RECONCILED → AVAILABLE, with side states FAILED, EXPIRED, CANCELLED,
// REVERSED and REVIEW_REQUIRED. Provider-owned states may be skipped forward
// (webhook delivery is unordered); platform-owned states never are: only an
// observed chain receipt reaches SETTLEMENT_OBSERVED, only a posted
// FUNDING_SETTLED journal transaction reaches RECONCILED, and only
// RECONCILED reaches AVAILABLE. Every transition is a deposit_transitions
// row, an outbox event and an audit event in the same database transaction;
// migration 00603 rejects a status change without its transition row.
//
// Three truths stay distinct (PART 11): the provider's session status, the
// chain receipt, and the ledger. "Funds can trade" (buying_power_eligible)
// never implies "funds can withdraw" (withdrawal_eligible), which is set
// only after reversible_until has passed with fraud_state NONE or CLEARED.
//
// This package must never:
//   - credit a customer on a redirect "success", a provider HTTP 200, or a
//     provider status alone: money exists in the ledger only after a chain
//     receipt has been observed and reconciled against the provider;
//   - process a provider event outside the inbox transaction the webhook
//     pipeline opens (ApplyProviderEvent takes the caller's tx and is never
//     reached from an unpersisted request);
//   - let a reversal drive WALLET negative: a shortfall is posted to the
//     credit-normal DEFICIT account, the account is frozen and an alert row
//     is written, never a silently negative balance;
//   - change a deposit status without a transition row, or accept a
//     transition the table does not list;
//   - be reached by an AGENT principal (Start requires funding:create,
//     which agents never hold; ledger posting refuses agents outright);
//   - import a provider SDK or provider types: adapters live under
//     internal/provider and speak only the contracts declared here;
//   - persist or log a provider client secret beyond the response that
//     hands it to the customer.
package funding
