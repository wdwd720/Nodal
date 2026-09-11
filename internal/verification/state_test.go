package verification

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every edge of the verification state machine, and every edge that is absent.
//
// The table below is the whole machine written out a second time, deliberately:
// comparing the implementation against itself would pass however wrong it is,
// so this is an independent statement of what the edges are meant to be. When
// the two disagree, one of them is wrong and a person has to decide which.
func TestStateMachine_EveryEdgeIsTheOneIntended(t *testing.T) {
	t.Parallel()
	want := map[State][]State{
		StateUnverified:       {StateRequired, StateStarted, StateSuspended},
		StateRequired:         {StateStarted, StateSuspended},
		StateStarted:          {StatePending, StateNeedsInformation, StateRejected, StateRequired, StateSuspended},
		StatePending:          {StateVerified, StateRejected, StateNeedsInformation, StateRestricted, StateSuspended},
		StateNeedsInformation: {StateStarted, StatePending, StateRejected, StateSuspended},
		StateVerified:         {StateExpired, StateRestricted, StateRejected, StateSuspended},
		StateRestricted:       {StateVerified, StateRejected, StateExpired, StateSuspended},
		StateExpired:          {StateRequired, StateStarted, StateSuspended},
		StateRejected:         {StateRequired, StateSuspended},
		StateSuspended:        {StateVerified, StateRestricted, StateRejected, StateRequired},
	}
	require.Len(t, want, len(AllStates()), "a state was added and this table was not updated")

	for _, from := range AllStates() {
		legal := map[State]bool{}
		for _, to := range want[from] {
			legal[to] = true
		}
		for _, to := range AllStates() {
			assert.Equalf(t, legal[to], CanTransition(from, to), "%s -> %s", from, to)
		}
		assert.ElementsMatch(t, want[from], TransitionsFrom(from), "TransitionsFrom(%s)", from)
	}
}

// The one edge whose absence is the point: nobody becomes verified without a
// provider session having decided so. VERIFIED is reachable only from PENDING,
// from RESTRICTED (a limitation lifted) and from SUSPENDED (an operator
// restoring what was already decided) — and never from UNVERIFIED, REQUIRED,
// STARTED, NEEDS_INFORMATION, EXPIRED or REJECTED.
func TestStateMachine_NobodyBecomesVerifiedWithoutADecision(t *testing.T) {
	t.Parallel()
	var sources []State
	for _, from := range AllStates() {
		if CanTransition(from, StateVerified) {
			sources = append(sources, from)
		}
	}
	assert.ElementsMatch(t, []State{StatePending, StateRestricted, StateSuspended}, sources,
		"something can reach VERIFIED without a provider decision")

	for _, from := range []State{StateUnverified, StateRequired, StateStarted, StateNeedsInformation, StateExpired, StateRejected} {
		assert.Falsef(t, CanTransition(from, StateVerified), "%s must not reach VERIFIED directly", from)
	}
}

func TestStateMachine_ParsingAndClassification(t *testing.T) {
	t.Parallel()
	for _, s := range AllStates() {
		parsed, err := ParseState(" " + string(s) + " ")
		require.NoError(t, err, s)
		assert.Equal(t, s, parsed)
		assert.True(t, s.Valid())
	}
	_, err := ParseState("MAYBE")
	assert.Error(t, err)
	assert.False(t, State("MAYBE").Valid())
	assert.Empty(t, TransitionsFrom("MAYBE"), "an unknown state licenses nothing")

	// Settled is "nothing happens without somebody acting"; AwaitingPerson is
	// "the next move is the person's". PENDING is neither.
	assert.False(t, StatePending.Settled())
	assert.False(t, StateStarted.Settled())
	assert.True(t, StateVerified.Settled())
	assert.True(t, StateUnverified.AwaitingPerson())
	assert.True(t, StateNeedsInformation.AwaitingPerson())
	assert.False(t, StatePending.AwaitingPerson())
	assert.False(t, StateVerified.AwaitingPerson())
}

// Every edge of the session machine, and the two absences that matter: a
// session nobody was sent to cannot have been decided, and a session a human
// reviewer is looking at cannot be overtaken by an automated verdict.
func TestSessionMachine_EveryEdgeIsTheOneIntended(t *testing.T) {
	t.Parallel()
	want := map[SessionStatus][]SessionStatus{
		SessionCreated:           {SessionPendingUserAction, SessionCancelled, SessionExpired},
		SessionPendingUserAction: {SessionProcessing, SessionRequiresInput, SessionCancelled, SessionExpired},
		SessionProcessing:        {SessionApproved, SessionDeclined, SessionRequiresInput, SessionManualReview, SessionExpired},
		SessionRequiresInput:     {SessionPendingUserAction, SessionProcessing, SessionDeclined, SessionCancelled, SessionExpired},
		SessionManualReview:      {SessionApproved, SessionDeclined, SessionExpired},
		SessionApproved:          {},
		SessionDeclined:          {},
		SessionCancelled:         {},
		SessionExpired:           {},
	}
	require.Len(t, want, len(AllSessionStatuses()))

	for _, from := range AllSessionStatuses() {
		legal := map[SessionStatus]bool{}
		for _, to := range want[from] {
			legal[to] = true
		}
		for _, to := range AllSessionStatuses() {
			assert.Equalf(t, legal[to], CanTransitionSession(from, to), "%s -> %s", from, to)
		}
		assert.ElementsMatch(t, want[from], SessionTransitionsFrom(from), "SessionTransitionsFrom(%s)", from)
	}

	assert.False(t, CanTransitionSession(SessionCreated, SessionApproved),
		"a session nobody was sent to cannot have been decided")
	assert.False(t, CanTransitionSession(SessionManualReview, SessionProcessing),
		"an automated verdict must not overtake a human reviewer")

	for _, s := range []SessionStatus{SessionApproved, SessionDeclined, SessionCancelled, SessionExpired} {
		assert.True(t, s.Terminal(), s)
		assert.False(t, s.Open(), s)
		assert.Empty(t, SessionTransitionsFrom(s), "%s is terminal and licenses nothing", s)
	}
	// Open is the same list as the partial unique index in migration 00762.
	var open []SessionStatus
	for _, s := range AllSessionStatuses() {
		if s.Open() {
			open = append(open, s)
		}
	}
	assert.ElementsMatch(t, []SessionStatus{
		SessionCreated, SessionPendingUserAction, SessionProcessing, SessionRequiresInput, SessionManualReview,
	}, open)
}

func TestSessionMachine_ParsingFailsClosed(t *testing.T) {
	t.Parallel()
	for _, s := range AllSessionStatuses() {
		parsed, err := ParseSessionStatus(" " + string(s) + " ")
		require.NoError(t, err, s)
		assert.Equal(t, s, parsed)
	}
	_, err := ParseSessionStatus("APPROVED_MAYBE")
	assert.Error(t, err)
	assert.False(t, SessionStatus("").Valid())
	assert.False(t, SessionStatus("").Open(), "the zero status is not open")
	assert.False(t, SessionStatus("").Terminal(), "the zero status is not terminal either; it is nothing")
}

func TestPurposeParsing(t *testing.T) {
	t.Parallel()
	for _, p := range AllPurposes() {
		parsed, err := ParsePurpose(string(p))
		require.NoError(t, err)
		assert.Equal(t, p, parsed)
	}
	_, err := ParsePurpose("SOMETHING")
	assert.Error(t, err)
}
