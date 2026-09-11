package profile

import (
	"time"

	"github.com/nodal/controlplane/internal/errs"
)

// ClosureState is the state of a request to close an account.
type ClosureState string

// The closure states. Migration 00758 holds the same list in a CHECK and
// test/integration/enums keeps them identical.
const (
	// ClosurePending is the only state a request is born in. The cooling-off
	// period is running and the user may cancel.
	ClosurePending ClosureState = "PENDING"
	// ClosureCancelled is the user (or an operator, on the user's behalf)
	// stopping the request.
	ClosureCancelled ClosureState = "CANCELLED"
	// ClosureRefused is an operator declining to close, with a reason: an
	// unsettled payout, an open dispute, a balance to deal with first.
	ClosureRefused ClosureState = "REFUSED"
	// ClosureEffected is the account closed: users.status CLOSED, every account
	// owned CLOSED, every session revoked. It is terminal and irreversible.
	ClosureEffected ClosureState = "EFFECTED"
)

// AllClosureStates returns every declared state, in declaration order.
func AllClosureStates() []ClosureState {
	return []ClosureState{ClosurePending, ClosureCancelled, ClosureRefused, ClosureEffected}
}

// Valid reports whether s is declared.
func (s ClosureState) Valid() bool {
	for _, v := range AllClosureStates() {
		if v == s {
			return true
		}
	}
	return false
}

// Terminal reports whether the request is finished.
func (s ClosureState) Terminal() bool { return s != ClosurePending }

// closureTransitions is the edge set. PENDING is the only origin, because a
// decided request is decided; a user who changes their mind after a refusal or
// a cancellation makes a NEW request, which restarts the cooling-off period.
// That is the point: the wait is not something you can bank.
var closureTransitions = map[ClosureState][]ClosureState{
	ClosurePending:   {ClosureCancelled, ClosureRefused, ClosureEffected},
	ClosureCancelled: {},
	ClosureRefused:   {},
	ClosureEffected:  {},
}

// CanCloseTransition reports whether from -> to is legal.
func CanCloseTransition(from, to ClosureState) bool {
	for _, t := range closureTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// DefaultCoolingOff is how long a closure request waits before it can be
// effected.
//
// Fourteen days, and the number is a judgement rather than a derivation, so it
// is worth saying what it is trading off. Too short and a hijacked session can
// end an account before its owner reads the notification; too long and a person
// who wants to leave is kept. Fourteen days is longer than any plausible gap
// between a compromise and its owner noticing a login they did not make, and is
// the period consumer products conventionally use for the same purpose.
const DefaultCoolingOff = 14 * 24 * time.Hour

// ClosureRequest is a user's request to close their account.
type ClosureRequest struct {
	ID              string
	UserID          string
	State           ClosureState
	RequestedReason string
	RequestedAt     time.Time
	CoolingOffUntil time.Time
	SessionID       string
	DecidedAt       *time.Time
	DecidedReason   string
}

// Effectable reports whether the cooling-off period has passed.
func (c ClosureRequest) Effectable(now time.Time) bool {
	return c.State == ClosurePending && !now.Before(c.CoolingOffUntil)
}

// ValidateClosureReason bounds what a user may write on the way out. It is
// optional: a person leaving does not owe an explanation.
func ValidateClosureReason(in string) (string, error) {
	r := NormalizeDisplayName(in)
	if len([]rune(r)) > 500 {
		return "", errs.New(errs.CodeValidationFailed, "a closure reason may be at most 500 characters").
			WithField("field", "reason")
	}
	return r, nil
}
