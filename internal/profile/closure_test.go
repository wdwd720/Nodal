package profile

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/payout"
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

// The four terminal payout states this package repeats are internal/payout's.
//
// The list is copied rather than imported because internal/payout pulls in the
// ledger, the capability gates and the configuration, and this package needs
// four strings to ask "is a payout still in flight". A copy is only worth having
// if something holds it against the original, which is what this does: the
// import is in a test file, so it costs the production build nothing.
func TestClosureBlockers_TheTerminalPayoutStatesAreThePayoutPackages(t *testing.T) {
	t.Parallel()
	var want []string
	for _, s := range payout.AllStates() {
		if s.Terminal() {
			want = append(want, string(s))
		}
	}
	got := append([]string(nil), terminalPayoutStates...)
	sort.Strings(got)
	sort.Strings(want)
	require.NotEmpty(t, want, "internal/payout declares no terminal state; this comparison proves nothing")
	assert.Equal(t, want, got,
		"internal/profile's copy of the terminal payout states has drifted from internal/payout's; "+
			"a state terminal there and not here blocks a closure for a payout that is finished, "+
			"and one terminal here and not there effects a closure over a payout still in flight")
}

// The blockers read as one answer, and an empty one is clear.
func TestClosureBlockers_ClearIsTheAbsenceOfAllThree(t *testing.T) {
	t.Parallel()
	assert.True(t, ClosureBlockers{}.Clear(), "a zero value must not block")
	assert.True(t, ClosureBlockers{CreditBalance: "0"}.Clear())
	assert.Empty(t, ClosureBlockers{CreditBalance: "0"}.Reasons())

	for _, b := range []ClosureBlockers{
		{CreditBalance: "1"},
		{CreditBalance: "0", OpenPayoutRequests: 1},
		{CreditBalance: "0", OpenNativePositions: 1},
	} {
		assert.Falsef(t, b.Clear(), "%+v", b)
		assert.Lenf(t, b.Reasons(), 1, "%+v", b)
	}

	all := ClosureBlockers{CreditBalance: "250", OpenPayoutRequests: 2, OpenNativePositions: 3}
	assert.False(t, all.Clear())
	reasons := all.Reasons()
	require.Len(t, reasons, 3)
	assert.Contains(t, reasons[0], "250 Credits")
	assert.Contains(t, reasons[1], "2 payout requests")
	assert.Contains(t, reasons[2], "3 native positions")
}
