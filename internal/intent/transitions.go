package intent

// Transitions is the explicit intent state machine (SETTLEMENT_COMPILER.md
// §2). Every legal edge is listed; anything absent is illegal and fails with
// INVALID_STATE_TRANSITION. Terminal statuses have an empty list.
//
// Side states are reachable from every non-terminal status: REJECTED (a
// policy said no), EXPIRED (the deadline passed), CANCELLED (the submitter
// withdrew, or — once EXECUTING — the venue confirmed the cancel, PART 227),
// FAILED (an internal or provider failure that concluded the intent).
// NO_VALID_PLAN is reachable once capital is RESERVED, because the planner
// runs after reservation, and again from PLANNED when a re-plan finds no
// route (replanning supersedes the previous plan, PART 39).
var Transitions = map[Status][]Status{
	StatusReceived:           {StatusEligibilityChecked, StatusRejected, StatusExpired, StatusCancelled, StatusFailed},
	StatusEligibilityChecked: {StatusRiskChecked, StatusRejected, StatusExpired, StatusCancelled, StatusFailed},
	StatusRiskChecked:        {StatusReserved, StatusRejected, StatusExpired, StatusCancelled, StatusFailed},
	StatusReserved:           {StatusPlanned, StatusNoValidPlan, StatusRejected, StatusExpired, StatusCancelled, StatusFailed},
	StatusPlanned:            {StatusExecuting, StatusNoValidPlan, StatusRejected, StatusExpired, StatusCancelled, StatusFailed},
	StatusExecuting:          {StatusCompleted, StatusExpired, StatusCancelled, StatusFailed},
	StatusCompleted:          {},
	StatusRejected:           {},
	StatusExpired:            {},
	StatusCancelled:          {},
	StatusFailed:             {},
	StatusNoValidPlan:        {},
}

// CanTransition reports whether from → to is listed in Transitions. Unknown
// statuses on either side are never legal.
func CanTransition(from, to Status) bool {
	if !from.Valid() || !to.Valid() {
		return false
	}
	for _, x := range Transitions[from] {
		if x == to {
			return true
		}
	}
	return false
}

// RequiresRejectionCode reports whether a transition into to must carry a
// stable machine-readable rejection code (REJECTED and NO_VALID_PLAN). Other
// terminal statuses may carry one; non-terminal statuses must not.
func RequiresRejectionCode(to Status) bool {
	return to == StatusRejected || to == StatusNoValidPlan
}
