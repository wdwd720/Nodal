package profile

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
)

func testTime() time.Time { return time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC) }

// Every edge and every non-edge, exhaustively, rather than a sample: a state
// machine tested by example is a state machine whose untested cell is the one
// that eventually fires.
func TestClosureStateMachine_EveryPair(t *testing.T) {
	t.Parallel()
	legal := map[ClosureState]map[ClosureState]bool{
		ClosurePending: {ClosureCancelled: true, ClosureRefused: true, ClosureEffected: true},
	}
	states := AllClosureStates()
	require.Len(t, states, 4)
	for _, from := range states {
		for _, to := range states {
			want := legal[from][to]
			assert.Equalf(t, want, CanCloseTransition(from, to), "%s -> %s", from, to)
		}
	}
	// Self-transitions are not edges either, including PENDING -> PENDING: a
	// request cannot be re-opened, and a second decision on a decided request
	// must fail rather than overwrite the first.
	for _, s := range states {
		assert.Falsef(t, CanCloseTransition(s, s), "%s -> %s", s, s)
	}
	// An undeclared destination is refused rather than defaulting to permitted.
	assert.False(t, CanCloseTransition(ClosurePending, ClosureState("DELETED")))
	assert.False(t, CanCloseTransition(ClosureState("DELETED"), ClosureEffected))
}

func TestClosureState_ValidAndTerminal(t *testing.T) {
	t.Parallel()
	for _, s := range AllClosureStates() {
		assert.True(t, s.Valid(), "%s", s)
	}
	assert.False(t, ClosureState("").Valid())
	assert.False(t, ClosureState("PENDING ").Valid())

	assert.False(t, ClosurePending.Terminal())
	for _, s := range []ClosureState{ClosureCancelled, ClosureRefused, ClosureEffected} {
		assert.True(t, s.Terminal(), "%s", s)
	}
}

func TestClosureRequest_EffectableOnlyAfterTheCoolingOffPeriod(t *testing.T) {
	t.Parallel()
	now := testTime()
	c := ClosureRequest{State: ClosurePending, RequestedAt: now, CoolingOffUntil: now.Add(DefaultCoolingOff)}

	assert.False(t, c.Effectable(now), "effectable the instant it was asked for")
	assert.False(t, c.Effectable(c.CoolingOffUntil.Add(-time.Nanosecond)), "effectable a nanosecond early")
	assert.True(t, c.Effectable(c.CoolingOffUntil), "not effectable at the boundary")
	assert.True(t, c.Effectable(c.CoolingOffUntil.Add(time.Hour)))

	// A decided request is never effectable again, however long ago it was.
	for _, s := range []ClosureState{ClosureCancelled, ClosureRefused, ClosureEffected} {
		decided := c
		decided.State = s
		assert.Falsef(t, decided.Effectable(c.CoolingOffUntil.Add(time.Hour)), "%s", s)
	}
}

func TestDefaultCoolingOff_IsTwoWeeks(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 14*24*time.Hour, DefaultCoolingOff)
}

func TestValidateClosureReason(t *testing.T) {
	t.Parallel()
	got, err := ValidateClosureReason("  too   expensive ")
	require.NoError(t, err)
	assert.Equal(t, "too expensive", got)

	// Optional: a person leaving does not owe an explanation.
	got, err = ValidateClosureReason("")
	require.NoError(t, err)
	assert.Equal(t, "", got)

	_, err = ValidateClosureReason(strings.Repeat("a", 501))
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

func TestClosureDecision_IsAClosedSet(t *testing.T) {
	t.Parallel()
	require.Len(t, AllClosureDecisions(), 3)
	for _, d := range AllClosureDecisions() {
		assert.True(t, d.Valid(), "%s", d)
	}
	for _, bad := range []ClosureDecision{"", "DELETE", "cancel", "EFFECT "} {
		assert.Falsef(t, bad.Valid(), "%q was accepted", bad)
	}
}
