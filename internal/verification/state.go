package verification

import (
	"strings"

	"github.com/nodal/controlplane/internal/errs"
)

// State is the financial verification state of a person (goal §20). It is the
// value of compliance_profiles.identity_state, and migration 00761 holds the
// two lists identical.
type State string

// Verification states.
//
// §20's canonical list is NOT_STARTED, REQUIRED, STARTED, PENDING,
// NEEDS_INFORMATION, VERIFIED, REJECTED, RESTRICTED, EXPIRED, SUSPENDED.
// UNVERIFIED is NOT_STARTED under the name the schema already used, for the
// reason recorded in D-057: rows carry it, eligibility policy documents
// allowlist it by name, and renaming a value to match a document breaks the
// documents somebody already wrote.
const (
	// StateUnverified is a person nothing has been asked of. §20's NOT_STARTED.
	StateUnverified State = "UNVERIFIED"
	// StateRequired is a person who has asked for something that needs
	// verification and has not begun one. It is what turns "no" into "not
	// yet, and here is the step".
	StateRequired State = "REQUIRED"
	// StateStarted is a live provider session the person has not finished.
	StateStarted State = "STARTED"
	// StatePending is the provider deciding. Nothing is required of the person.
	StatePending State = "PENDING"
	// StateNeedsInformation is the provider asking for something more.
	StateNeedsInformation State = "NEEDS_INFORMATION"
	// StateVerified is a provider decision in the person's favour, within its
	// validity window.
	StateVerified State = "VERIFIED"
	// StateRejected is a provider decision against the person.
	StateRejected State = "REJECTED"
	// StateExpired is a decision that has aged out of its validity window. It
	// is not a rejection and must not be shown as one.
	StateExpired State = "EXPIRED"
	// StateRestricted is verified, with something — a jurisdiction, a sanctions
	// review, an operator restriction — limiting what it permits.
	StateRestricted State = "RESTRICTED"
	// StateSuspended is an operator having stopped everything pending review.
	StateSuspended State = "SUSPENDED"
)

var allStates = []State{
	StateUnverified, StateRequired, StateStarted, StatePending, StateNeedsInformation,
	StateVerified, StateRejected, StateExpired, StateRestricted, StateSuspended,
}

// AllStates returns every declared state in declaration order (a copy).
func AllStates() []State { return append([]State(nil), allStates...) }

// Valid reports whether s is declared.
func (s State) Valid() bool {
	for _, x := range allStates {
		if x == s {
			return true
		}
	}
	return false
}

func (s State) String() string { return string(s) }

// ParseState parses the canonical uppercase form.
func ParseState(in string) (State, error) {
	s := State(strings.ToUpper(strings.TrimSpace(in)))
	if !s.Valid() {
		return "", errs.Newf(errs.CodeValidationFailed, "unknown verification state %q", in)
	}
	return s, nil
}

// stateTransitions is the explicit legal transition table. Every edge below
// exists because something in the real journey produces it; an edge that is
// absent is absent on purpose.
var stateTransitions = map[State][]State{
	// Nothing has been asked. The person can be told it is needed, can begin
	// unprompted, or can be stopped by an operator before either.
	StateUnverified: {StateRequired, StateStarted, StateSuspended},

	// The requirement is on record. The only way forward is to begin.
	StateRequired: {StateStarted, StateSuspended},

	// A session exists. The provider may take it away to decide, may ask for
	// more, may refuse outright, or the person may abandon it — in which case
	// the REQUIREMENT survives, which is why abandonment lands on REQUIRED and
	// not back on UNVERIFIED. Losing the requirement would mean the product
	// forgets why it asked.
	StateStarted: {StatePending, StateNeedsInformation, StateRejected, StateRequired, StateSuspended},

	// The provider is deciding. It can decide either way, ask for more, or
	// decide in the person's favour with a limitation attached.
	StatePending: {StateVerified, StateRejected, StateNeedsInformation, StateRestricted, StateSuspended},

	// The provider asked for something. Supplying it resumes the session
	// (STARTED, because a resume issues a fresh hosted link) or goes straight
	// back to the provider (PENDING) when the provider took it without a new
	// interaction.
	StateNeedsInformation: {StateStarted, StatePending, StateRejected, StateSuspended},

	// A decision ages out; a later screening can restrict or reverse it. There
	// is no edge back to PENDING: re-verifying starts a new session, and a
	// verified person whose session is in flight is still verified until it
	// concludes.
	StateVerified: {StateExpired, StateRestricted, StateRejected, StateSuspended},

	// A restriction can be lifted, hardened into a rejection, aged out, or
	// escalated to a suspension.
	StateRestricted: {StateVerified, StateRejected, StateExpired, StateSuspended},

	// Expiry is not failure. Re-verification is the way out, and it starts the
	// same way a first verification does.
	StateExpired: {StateRequired, StateStarted, StateSuspended},

	// A rejection is not permanent and must not be modelled as though it were:
	// every hosted provider in PROVIDER_BOUNDARY §5 permits a re-attempt, and a
	// person whose document photograph was unreadable is not a person the
	// platform has decided about. What it cannot do is become VERIFIED without
	// going through a session again.
	StateRejected: {StateRequired, StateSuspended},

	// An operator resolves a suspension by restoring standing or ending it.
	StateSuspended: {StateVerified, StateRestricted, StateRejected, StateRequired},
}

// CanTransition reports whether from → to is legal.
//
// Note what is absent. Nothing reaches VERIFIED except from PENDING, from
// RESTRICTED (a limitation lifted) or from SUSPENDED (an operator restoring
// what was already decided). In particular UNVERIFIED → VERIFIED does not
// exist: there is no edge by which a person becomes verified without a provider
// session having decided so, which is the property that makes the state machine
// worth having.
func CanTransition(from, to State) bool {
	for _, t := range stateTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// TransitionsFrom returns the legal destinations of a state (a copy), for the
// benefit of an operator tool that has to offer them.
func TransitionsFrom(s State) []State {
	return append([]State(nil), stateTransitions[s]...)
}

// Settled reports whether the state is one where nothing is expected to happen
// without somebody acting. It is the question a "what should I do next" screen
// asks, and it is deliberately not called Terminal: a compliance standing is
// never terminal, because a sanctions list can change tomorrow.
func (s State) Settled() bool {
	switch s {
	case StatePending, StateStarted:
		return false
	}
	return true
}

// AwaitingPerson reports whether the next move is the person's.
func (s State) AwaitingPerson() bool {
	switch s {
	case StateRequired, StateStarted, StateNeedsInformation, StateExpired, StateUnverified:
		return true
	}
	return false
}
