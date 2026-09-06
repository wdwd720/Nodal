package withdrawal

import "slices"

// Status is the withdrawal state (migration 00103 CHECK constraint).
type Status string

// Statuses.
const (
	StatusRequested         Status = "REQUESTED"
	StatusStepUpVerified    Status = "STEP_UP_VERIFIED"
	StatusApprovalPending   Status = "APPROVAL_PENDING"
	StatusApproved          Status = "APPROVED"
	StatusRejected          Status = "REJECTED"
	StatusSubmitted         Status = "SUBMITTED"
	StatusSubmissionUnknown Status = "SUBMISSION_UNKNOWN"
	StatusSettled           Status = "SETTLED"
	StatusFailed            Status = "FAILED"
	StatusCancelled         Status = "CANCELLED"
)

var allStatuses = []Status{
	StatusRequested, StatusStepUpVerified, StatusApprovalPending, StatusApproved, StatusRejected,
	StatusSubmitted, StatusSubmissionUnknown, StatusSettled, StatusFailed, StatusCancelled,
}

// AllStatuses returns every status in declaration order.
func AllStatuses() []Status { return slices.Clone(allStatuses) }

// Valid reports whether s is declared.
func (s Status) Valid() bool { return slices.Contains(allStatuses, s) }

// Final reports whether s is terminal.
func (s Status) Final() bool {
	switch s {
	case StatusRejected, StatusSettled, StatusFailed, StatusCancelled:
		return true
	}
	return false
}

// Active reports whether the withdrawal still counts against velocity
// limits: requested, in approval, or in flight (SETTLED counts too; only
// rejected, failed and cancelled requests free their quota).
func (s Status) Active() bool {
	return s.Valid() && s != StatusRejected && s != StatusFailed && s != StatusCancelled
}

// Transitions is the explicit legal-transition table. SUBMISSION_UNKNOWN
// never goes back to SUBMITTED: reconciliation decides SETTLED or FAILED.
var Transitions = map[Status][]Status{
	StatusRequested:         {StatusStepUpVerified, StatusRejected, StatusCancelled},
	StatusStepUpVerified:    {StatusApprovalPending, StatusRejected, StatusCancelled},
	StatusApprovalPending:   {StatusApproved, StatusRejected, StatusCancelled},
	StatusApproved:          {StatusSubmitted, StatusFailed, StatusCancelled},
	StatusSubmitted:         {StatusSettled, StatusFailed, StatusSubmissionUnknown},
	StatusSubmissionUnknown: {StatusSettled, StatusFailed},
	StatusRejected:          {},
	StatusSettled:           {},
	StatusFailed:            {},
	StatusCancelled:         {},
}

// CanTransition reports whether from → to is legal.
func CanTransition(from, to Status) bool {
	if from == to || !from.Valid() || !to.Valid() {
		return false
	}
	return slices.Contains(Transitions[from], to)
}
