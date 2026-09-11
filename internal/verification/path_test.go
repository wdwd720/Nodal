package verification

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A provider does not report every step. The shortest-path walk is what lets a
// jump be ingested without inventing an edge, and these are the properties that
// make the walk safe to trust.

func TestSessionPath_WalksOnlyLegalEdges(t *testing.T) {
	t.Parallel()
	for _, from := range AllSessionStatuses() {
		for _, to := range AllSessionStatuses() {
			path := SessionPath(from, to)
			if from == to {
				assert.Nilf(t, path, "%s -> %s", from, to)
				continue
			}
			if path == nil {
				continue
			}
			at := from
			for _, step := range path {
				assert.Truef(t, CanTransitionSession(at, step),
					"the walk %s -> %s used the illegal edge %s -> %s", from, to, at, step)
				at = step
			}
			assert.Equalf(t, to, at, "the walk %s -> %s ended at %s", from, to, at)
		}
	}
}

func TestSessionPath_TheJumpsARealProviderProduces(t *testing.T) {
	t.Parallel()
	// The one that made this necessary: a person completes a hosted flow and
	// the provider decides before the next poll.
	assert.Equal(t, []SessionStatus{SessionProcessing, SessionApproved},
		SessionPath(SessionPendingUserAction, SessionApproved))
	assert.Equal(t, []SessionStatus{SessionProcessing, SessionDeclined},
		SessionPath(SessionPendingUserAction, SessionDeclined))
	assert.Equal(t, []SessionStatus{SessionPendingUserAction, SessionProcessing, SessionApproved},
		SessionPath(SessionCreated, SessionApproved))

	// And the ones that must stay unreachable: nothing walks out of a terminal
	// status, whatever the provider says next.
	for _, from := range []SessionStatus{SessionApproved, SessionDeclined, SessionCancelled, SessionExpired} {
		for _, to := range AllSessionStatuses() {
			if from == to {
				continue
			}
			assert.Nilf(t, SessionPath(from, to), "%s must not walk to %s", from, to)
		}
	}
}

func TestStatePath_WalksOnlyLegalEdges(t *testing.T) {
	t.Parallel()
	for _, from := range AllStates() {
		for _, to := range AllStates() {
			path := Path(from, to)
			if from == to {
				assert.Nilf(t, path, "%s -> %s", from, to)
				continue
			}
			if path == nil {
				continue
			}
			at := from
			for _, step := range path {
				assert.Truef(t, CanTransition(at, step),
					"the walk %s -> %s used the illegal edge %s -> %s", from, to, at, step)
				at = step
			}
			assert.Equalf(t, to, at, "the walk %s -> %s ended at %s", from, to, at)
		}
	}
}

// The walk never shortcuts the property the machine exists to enforce: every
// route to VERIFIED goes through a provider decision (PENDING), a lifted
// restriction (RESTRICTED) or an operator restoring standing (SUSPENDED).
func TestStatePath_NeverReachesVerifiedWithoutADecision(t *testing.T) {
	t.Parallel()
	for _, from := range AllStates() {
		if from == StateVerified {
			continue
		}
		path := Path(from, StateVerified)
		require.NotNilf(t, path, "%s must be able to become verified eventually", from)
		penultimate := from
		if len(path) > 1 {
			penultimate = path[len(path)-2]
		}
		assert.Containsf(t, []State{StatePending, StateRestricted, StateSuspended}, penultimate,
			"the walk from %s reached VERIFIED via %s", from, penultimate)
	}
}
