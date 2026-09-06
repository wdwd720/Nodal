package funding

import (
	"slices"
)

// Status is the deposit state (PART 28, FINANCIAL_MODEL §4). The set is
// closed and mirrors the CHECK constraint on deposits.status.
type Status string

// Main-chain states, in order.
const (
	StatusCreated                Status = "CREATED"
	StatusSessionCreated         Status = "SESSION_CREATED"
	StatusCustomerActionRequired Status = "CUSTOMER_ACTION_REQUIRED"
	StatusProviderProcessing     Status = "PROVIDER_PROCESSING"
	StatusProviderConfirmed      Status = "PROVIDER_CONFIRMED"
	StatusSettlementObserved     Status = "SETTLEMENT_OBSERVED"
	StatusReconciled             Status = "RECONCILED"
	StatusAvailable              Status = "AVAILABLE"
)

// Side states.
const (
	StatusFailed         Status = "FAILED"
	StatusExpired        Status = "EXPIRED"
	StatusCancelled      Status = "CANCELLED"
	StatusReversed       Status = "REVERSED"
	StatusReviewRequired Status = "REVIEW_REQUIRED"
)

// mainChain lists the happy-path states in order; the index is the rank.
var mainChain = []Status{
	StatusCreated, StatusSessionCreated, StatusCustomerActionRequired, StatusProviderProcessing,
	StatusProviderConfirmed, StatusSettlementObserved, StatusReconciled, StatusAvailable,
}

var allStatuses = append(slices.Clone(mainChain),
	StatusFailed, StatusExpired, StatusCancelled, StatusReversed, StatusReviewRequired)

// timestampColumns maps every status to the deposits column stamped when
// it is entered. CREATED maps to created_at, set on insert.
var timestampColumns = map[Status]string{
	StatusCreated:                "created_at",
	StatusSessionCreated:         "session_created_at",
	StatusCustomerActionRequired: "customer_action_at",
	StatusProviderProcessing:     "provider_processing_at",
	StatusProviderConfirmed:      "provider_confirmed_at",
	StatusSettlementObserved:     "settlement_observed_at",
	StatusReconciled:             "reconciled_at",
	StatusAvailable:              "available_at",
	StatusFailed:                 "failed_at",
	StatusExpired:                "expired_at",
	StatusCancelled:              "cancelled_at",
	StatusReversed:               "reversed_at",
	StatusReviewRequired:         "review_required_at",
}

// AllStatuses returns every status in declaration order (main chain first).
func AllStatuses() []Status { return slices.Clone(allStatuses) }

// Valid reports whether s is a declared status.
func (s Status) Valid() bool { return slices.Contains(allStatuses, s) }

// Final reports whether s is terminal: no transition leaves it.
func (s Status) Final() bool {
	switch s {
	case StatusFailed, StatusExpired, StatusCancelled, StatusReversed:
		return true
	}
	return false
}

// Pending reports whether the deposit is in flight: not final and not yet
// AVAILABLE. Buying power counts these as "pending" (FINANCIAL_MODEL §6).
func (s Status) Pending() bool {
	return s.Valid() && !s.Final() && s != StatusAvailable
}

// Rank returns the position of s on the main chain and true, or 0 and false
// for a side state. Provider status updates are applied only when they
// advance the rank (webhooks are unordered; a stale status is a no-op).
func (s Status) Rank() (int, bool) {
	i := slices.Index(mainChain, s)
	if i < 0 {
		return 0, false
	}
	return i, true
}

// ProviderOwned reports whether the provider is the authority for reaching
// s. Only these states may be skipped forward; the platform-owned states
// (SETTLEMENT_OBSERVED, RECONCILED, AVAILABLE) require their own evidence.
func (s Status) ProviderOwned() bool {
	switch s {
	case StatusCustomerActionRequired, StatusProviderProcessing, StatusProviderConfirmed:
		return true
	}
	return false
}

// TimestampColumn is the deposits column stamped when s is entered; "" for
// an unknown status.
func (s Status) TimestampColumn() string { return timestampColumns[s] }

// Transitions is the explicit legal-transition table (PART 28).
//
//   - CREATED waits for the provider session; it can fail, expire or be
//     cancelled before one exists.
//   - SESSION_CREATED and CUSTOMER_ACTION_REQUIRED accept any forward
//     provider-owned state (unordered webhooks), FAILED (provider rejected),
//     EXPIRED (abandoned), CANCELLED (customer/operator) and REVIEW_REQUIRED.
//   - PROVIDER_PROCESSING: payment taken, crypto not yet delivered. It can
//     only be confirmed, rejected by the provider, or escalated.
//   - PROVIDER_CONFIRMED never fails: the provider says crypto was delivered,
//     so anything but a chain receipt is a reconciliation question.
//   - SETTLEMENT_OBSERVED reconciles or escalates.
//   - RECONCILED (posted) becomes AVAILABLE, or is REVERSED (compensating
//     postings), or escalated.
//   - AVAILABLE is reversible for the policy window, or escalated.
//   - REVIEW_REQUIRED is resolved by an operator to any platform state; the
//     repository still refuses RECONCILED/AVAILABLE without a posting and
//     REVERSED without a reversal posting.
//   - FAILED, EXPIRED, CANCELLED and REVERSED are terminal.
var Transitions = map[Status][]Status{
	StatusCreated: {StatusSessionCreated, StatusFailed, StatusExpired, StatusCancelled},
	StatusSessionCreated: {
		StatusCustomerActionRequired, StatusProviderProcessing, StatusProviderConfirmed,
		StatusFailed, StatusExpired, StatusCancelled, StatusReviewRequired,
	},
	StatusCustomerActionRequired: {
		StatusProviderProcessing, StatusProviderConfirmed,
		StatusFailed, StatusExpired, StatusCancelled, StatusReviewRequired,
	},
	StatusProviderProcessing: {StatusProviderConfirmed, StatusFailed, StatusReviewRequired},
	StatusProviderConfirmed:  {StatusSettlementObserved, StatusReviewRequired},
	StatusSettlementObserved: {StatusReconciled, StatusReviewRequired},
	StatusReconciled:         {StatusAvailable, StatusReversed, StatusReviewRequired},
	StatusAvailable:          {StatusReversed, StatusReviewRequired},
	StatusReviewRequired: {
		StatusProviderConfirmed, StatusSettlementObserved, StatusReconciled, StatusAvailable,
		StatusReversed, StatusFailed, StatusCancelled,
	},
	StatusFailed:    {},
	StatusExpired:   {},
	StatusCancelled: {},
	StatusReversed:  {},
}

// CanTransition reports whether from → to is listed in Transitions. A
// self-transition is never legal; unknown statuses are never legal.
func CanTransition(from, to Status) bool {
	if from == to || !from.Valid() || !to.Valid() {
		return false
	}
	return slices.Contains(Transitions[from], to)
}
