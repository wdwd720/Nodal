package verification

import (
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
)

type (
	sessionKind    struct{}
	checkKindID    struct{}
	transitionKind struct{}
)

// SessionID identifies one verification attempt.
type SessionID = id.ID[sessionKind]

// NewSessionID returns a fresh session id.
func NewSessionID() SessionID { return id.New[sessionKind]() }

// ParseSessionID parses the canonical form.
func ParseSessionID(s string) (SessionID, error) { return id.Parse[sessionKind](s) }

// CheckID identifies one recorded sub-check.
type CheckID = id.ID[checkKindID]

// NewCheckID returns a fresh check id.
func NewCheckID() CheckID { return id.New[checkKindID]() }

// TransitionID identifies one recorded state change.
type TransitionID = id.ID[transitionKind]

// NewTransitionID returns a fresh transition id.
func NewTransitionID() TransitionID { return id.New[transitionKind]() }

// Purpose is the level a session is trying to establish. A provider is told
// what a check is FOR before it runs one, and "we verified you" is meaningless
// without "for what".
type Purpose string

// Purposes.
const (
	// PurposePayoutKYC is identity verification a payout provider will accept.
	PurposePayoutKYC Purpose = "PAYOUT_KYC"
	// PurposeEnhanced is enhanced due diligence, which a provider demands for
	// higher value or higher risk.
	PurposeEnhanced Purpose = "ENHANCED"
)

var allPurposes = []Purpose{PurposePayoutKYC, PurposeEnhanced}

// AllPurposes returns every declared purpose (a copy).
func AllPurposes() []Purpose { return append([]Purpose(nil), allPurposes...) }

// Valid reports whether p is declared.
func (p Purpose) Valid() bool {
	for _, x := range allPurposes {
		if x == p {
			return true
		}
	}
	return false
}

// ParsePurpose parses the canonical uppercase form.
func ParsePurpose(in string) (Purpose, error) {
	p := Purpose(strings.ToUpper(strings.TrimSpace(in)))
	if !p.Valid() {
		return "", errs.Newf(errs.CodeValidationFailed, "unknown verification purpose %q", in)
	}
	return p, nil
}

// SessionStatus is where one attempt has got to.
//
// The union is PROVIDER_BOUNDARY §3's, derived from what Persona, Veriff,
// Sumsub and Stripe Identity each report, so no single vendor's vocabulary is
// the schema's. An adapter maps its provider's strings onto these; the state
// machine branches on these and never on a provider string.
type SessionStatus string

// Session statuses.
const (
	// SessionCreated is a row written before the provider was called, so a
	// crash between the write and the call leaves something to reconcile.
	SessionCreated SessionStatus = "CREATED"
	// SessionPendingUserAction is a hosted link issued and not yet completed.
	SessionPendingUserAction SessionStatus = "PENDING_USER_ACTION"
	// SessionProcessing is the provider deciding.
	SessionProcessing SessionStatus = "PROCESSING"
	// SessionRequiresInput is the provider asking for more.
	SessionRequiresInput SessionStatus = "REQUIRES_INPUT"
	// SessionManualReview is the provider's own human review.
	SessionManualReview SessionStatus = "MANUAL_REVIEW"
	// SessionApproved is the provider deciding in the person's favour.
	SessionApproved SessionStatus = "APPROVED"
	// SessionDeclined is the provider deciding against.
	SessionDeclined SessionStatus = "DECLINED"
	// SessionCancelled is the person or an operator abandoning it.
	SessionCancelled SessionStatus = "CANCELLED"
	// SessionExpired is a hosted link that ran out.
	SessionExpired SessionStatus = "EXPIRED"
)

var allSessionStatuses = []SessionStatus{
	SessionCreated, SessionPendingUserAction, SessionProcessing, SessionRequiresInput,
	SessionManualReview, SessionApproved, SessionDeclined, SessionCancelled, SessionExpired,
}

// AllSessionStatuses returns every declared status in declaration order (a copy).
func AllSessionStatuses() []SessionStatus {
	return append([]SessionStatus(nil), allSessionStatuses...)
}

// Valid reports whether s is declared.
func (s SessionStatus) Valid() bool {
	for _, x := range allSessionStatuses {
		if x == s {
			return true
		}
	}
	return false
}

func (s SessionStatus) String() string { return string(s) }

// ParseSessionStatus parses the canonical uppercase form.
func ParseSessionStatus(in string) (SessionStatus, error) {
	s := SessionStatus(strings.ToUpper(strings.TrimSpace(in)))
	if !s.Valid() {
		return "", errs.Newf(errs.CodeValidationFailed, "unknown verification session status %q", in)
	}
	return s, nil
}

// Open reports whether the session is still capable of producing a decision.
// The partial unique index in migration 00762 permits exactly one open session
// per person, and this is the same list.
func (s SessionStatus) Open() bool {
	switch s {
	case SessionCreated, SessionPendingUserAction, SessionProcessing, SessionRequiresInput, SessionManualReview:
		return true
	}
	return false
}

// Terminal reports whether no further transition is possible.
func (s SessionStatus) Terminal() bool {
	switch s {
	case SessionApproved, SessionDeclined, SessionCancelled, SessionExpired:
		return true
	}
	return false
}

// sessionTransitions is the explicit legal transition table.
var sessionTransitions = map[SessionStatus][]SessionStatus{
	// Written before the provider was called. Either the call produced a link,
	// or it did not and the row is cancelled or ages out -- "ages out" being
	// Service.ExpireOverdueSessions, which since F-170 exists and runs: this
	// edge was unreachable, and the row it describes holds the only open
	// session its owner is allowed (00762). It cannot jump to a decision: a
	// session nobody was ever sent to cannot have been decided.
	SessionCreated: {SessionPendingUserAction, SessionCancelled, SessionExpired},

	// A link is out. The person completes it (PROCESSING), the provider asks
	// for more up front (REQUIRES_INPUT), or it is abandoned or ages out.
	SessionPendingUserAction: {SessionProcessing, SessionRequiresInput, SessionCancelled, SessionExpired},

	// The provider decides, asks for more, or escalates to its own reviewer.
	SessionProcessing: {SessionApproved, SessionDeclined, SessionRequiresInput, SessionManualReview, SessionExpired},

	// More was asked. Supplying it reissues a link or goes straight back.
	SessionRequiresInput: {SessionPendingUserAction, SessionProcessing, SessionDeclined, SessionCancelled, SessionExpired},

	// A human at the provider decides. There is no route back to PROCESSING:
	// once a person is looking at it, an automated verdict must not overtake
	// them.
	SessionManualReview: {SessionApproved, SessionDeclined, SessionExpired},

	SessionApproved:  {},
	SessionDeclined:  {},
	SessionCancelled: {},
	SessionExpired:   {},
}

// CanTransitionSession reports whether from → to is legal.
func CanTransitionSession(from, to SessionStatus) bool {
	for _, t := range sessionTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// SessionEdges returns every legal session edge as a flat from,to sequence, in
// declaration order. Migration 00806 populates
// `verification_session_status_edges` from it and the apply trigger consults
// it; the enum parity suite holds the two identical.
func SessionEdges() []string {
	out := make([]string, 0, 32)
	for _, from := range allSessionStatuses {
		for _, to := range sessionTransitions[from] {
			out = append(out, string(from), string(to))
		}
	}
	return out
}

// SessionTransitionsFrom returns the legal destinations of a status (a copy).
func SessionTransitionsFrom(s SessionStatus) []SessionStatus {
	return append([]SessionStatus(nil), sessionTransitions[s]...)
}

// SessionPath is the shortest legal sequence of statuses from `from` to `to`,
// excluding `from` and including `to`. It returns nil when there is none.
//
// It exists because a provider does not report every step. A person completes a
// hosted flow and the provider decides before Nodal next asks, so a poll that
// last saw PENDING_USER_ACTION is answered APPROVED -- a jump the machine has
// no single edge for. Refusing that would be a state machine that cannot ingest
// reality; writing one transition row per inferred step keeps the edges
// meaningful AND keeps the trail honest, because each row says it was inferred.
//
// The path is the shortest one, and the table has no ambiguous shortest paths
// today (TestSessionPath_IsUnambiguous asserts it), so "the shortest" is also
// "the only sensible" one.
func SessionPath(from, to SessionStatus) []SessionStatus {
	if from == to {
		return nil
	}
	prev := map[SessionStatus]SessionStatus{}
	seen := map[SessionStatus]bool{from: true}
	queue := []SessionStatus{from}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range sessionTransitions[cur] {
			if seen[next] {
				continue
			}
			seen[next] = true
			prev[next] = cur
			if next == to {
				return reverseWalk(prev, from, to)
			}
			queue = append(queue, next)
		}
	}
	return nil
}

// Path is the same shortest-path walk over the verification state machine, for
// the same reason: a provider result implies a standing that may be more than
// one edge away from where the profile currently stands.
func Path(from, to State) []State {
	if from == to {
		return nil
	}
	prev := map[State]State{}
	seen := map[State]bool{from: true}
	queue := []State{from}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range stateTransitions[cur] {
			if seen[next] {
				continue
			}
			seen[next] = true
			prev[next] = cur
			if next == to {
				return reverseWalkState(prev, from, to)
			}
			queue = append(queue, next)
		}
	}
	return nil
}

func reverseWalk(prev map[SessionStatus]SessionStatus, from, to SessionStatus) []SessionStatus {
	var out []SessionStatus
	for at := to; at != from; at = prev[at] {
		out = append([]SessionStatus{at}, out...)
	}
	return out
}

func reverseWalkState(prev map[State]State, from, to State) []State {
	var out []State
	for at := to; at != from; at = prev[at] {
		out = append([]State{at}, out...)
	}
	return out
}

// Session mirrors a verification_sessions row.
//
// It carries no hosted URL. Those links are single-use credentials for
// resuming somebody's identity check; one is handed to the browser that asked
// for it and is never written down.
type Session struct {
	ID                  SessionID
	UserID              accounts.UserID
	Purpose             Purpose
	Provider            string
	ProviderRef         string
	Status              SessionStatus
	JurisdictionCountry string
	JurisdictionRegion  string
	RulesVersion        string
	Environment         string
	Sandbox             bool
	FailureReason       string
	ExpiresAt           *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
}
