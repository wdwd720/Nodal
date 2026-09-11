package profile

import (
	"strconv"
	"strings"
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

// ClosureEdges returns the legal edges, flattened to from, to, from, to... in
// declaration order.
//
// It is the Go half of the pair test/integration/enums holds against
// account_closure_request_transitions_edge_check (00798), and it is derived from
// closureTransitions rather than typed out, so an edge added in one language and
// not the other fails a test rather than a write.
func ClosureEdges() []string {
	var out []string
	for _, from := range AllClosureStates() {
		for _, to := range closureTransitions[from] {
			out = append(out, string(from), string(to))
		}
	}
	return out
}

// A user's status. The column has held these three since 00010; migration 00798
// binds the edges between them and this is the Go list it is held against.
const (
	// UserActive is the only status a user is created in.
	UserActive = "ACTIVE"
	// UserSuspended can still sign in and read, and can do nothing that moves
	// value. It is reversible.
	UserSuspended = "SUSPENDED"
	// UserClosed is terminal: identity.Complete refuses the login, and
	// reopening is a new relationship rather than an undo.
	UserClosed = "CLOSED"
)

// userStatusTransitions is the edge set 00757's header describes in words.
// CLOSED has no edge out of it: reopening a closed account is a decision about
// whether a person who asked to leave may come back, and it arrives with a
// retention question attached (ADR-0020).
var userStatusTransitions = map[string][]string{
	UserActive:    {UserSuspended, UserClosed},
	UserSuspended: {UserActive, UserClosed},
	UserClosed:    {},
}

// AllUserStatuses returns every declared user status, in declaration order.
func AllUserStatuses() []string { return []string{UserActive, UserSuspended, UserClosed} }

// CanUserStatusTransition reports whether from -> to is legal.
func CanUserStatusTransition(from, to string) bool {
	for _, t := range userStatusTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// UserStatusEdges returns the legal edges, flattened to from, to, from, to...
// in declaration order. It is the Go half of the pair test/integration/enums
// holds against user_status_transitions_edge_check (00798).
func UserStatusEdges() []string {
	var out []string
	for _, from := range AllUserStatuses() {
		for _, to := range userStatusTransitions[from] {
			out = append(out, from, to)
		}
	}
	return out
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

// ClosureBlockers is what a person's accounts still hold at the moment an
// operator is asked to close them.
//
// EFFECT is the one irreversible action on the support surface: it writes
// users.status = CLOSED, closes every account the person owns and revokes every
// session, after which identity.Complete refuses the login. Whatever the account
// still holds is then unreachable by the person who owns it.
//
// Migration 00758 and this file both say REFUSED exists for "an unsettled
// payout, an open dispute, or a balance to deal with first", and until F-179 the
// operator was given none of those three facts and nothing consulted them. These
// are the three, read together so that the surface the operator decides from and
// the check that refuses the decision cannot disagree.
//
// It is not a veto on leaving. A blocker means the request is REFUSED with a
// reason the person is shown -- take the balance out, let the payout settle,
// close the position -- and they ask again. It means "not like this", never "no".
type ClosureBlockers struct {
	// CreditBalance is the gross Credit balance across every account the person
	// owns, in base units, as a decimal string. Gross and not spendable: a
	// disputed or frozen lot is still value that belongs to them.
	CreditBalance string
	// OpenPayoutRequests is how many payout requests are not in a terminal
	// state. Each one is either holding value out of the balance or waiting on
	// a provider that will answer after the account is gone.
	OpenPayoutRequests int
	// OpenNativePositions is how many native-asset positions still hold a
	// non-zero quantity.
	OpenNativePositions int
}

// Clear reports that nothing financial stands in the way of effecting.
func (b ClosureBlockers) Clear() bool {
	return !b.hasBalance() && b.OpenPayoutRequests == 0 && b.OpenNativePositions == 0
}

func (b ClosureBlockers) hasBalance() bool {
	s := strings.TrimSpace(b.CreditBalance)
	return s != "" && s != "0"
}

// Reasons returns one sentence per blocker, in a fixed order, written for the
// operator who is deciding and for the person who will read the refusal.
func (b ClosureBlockers) Reasons() []string {
	var out []string
	if b.hasBalance() {
		out = append(out, "this person still holds "+strings.TrimSpace(b.CreditBalance)+
			" Credits; closing the account would put them out of reach")
	}
	if b.OpenPayoutRequests == 1 {
		out = append(out, "one payout request has not reached a terminal state")
	} else if b.OpenPayoutRequests > 1 {
		out = append(out, strconv.Itoa(b.OpenPayoutRequests)+" payout requests have not reached a terminal state")
	}
	if b.OpenNativePositions == 1 {
		out = append(out, "one native position is still open")
	} else if b.OpenNativePositions > 1 {
		out = append(out, strconv.Itoa(b.OpenNativePositions)+" native positions are still open")
	}
	return out
}
