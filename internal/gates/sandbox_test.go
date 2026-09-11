package gates

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A SANDBOX row is active on a sandbox tier and nowhere else, and the flag
// that makes it so changes nothing for any other state.
func TestEvaluateWith_SandboxRowIsActiveOnlyOnASandboxTier(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	effective := now.Add(-time.Minute)
	row := func(mut func(*Gate)) *Gate {
		g := &Gate{Capability: NativeMarketTrading, Environment: "STAGING", State: StateSandbox, EffectiveAt: &effective}
		if mut != nil {
			mut(g)
		}
		return g
	}

	v := EvaluateWith(row(nil), true, true, now)
	assert.True(t, v.Active, "a SANDBOX row on a sandbox tier is active")
	assert.True(t, v.Sandbox, "and says so")
	assert.Empty(t, v.Reason)

	v = EvaluateWith(row(nil), true, false, now)
	assert.False(t, v.Active, "the same row anywhere else is inactive")
	assert.Equal(t, ReasonSandboxNotAllowedHere, v.Reason)
	assert.False(t, v.Sandbox)

	v = Evaluate(row(nil), true, now)
	assert.False(t, v.Active, "Evaluate is EvaluateWith(sandbox=false): the old callers see no change")
	assert.Equal(t, ReasonSandboxNotAllowedHere, v.Reason)

	v = EvaluateWith(row(nil), false, true, now)
	assert.Equal(t, ReasonConfigDisabled, v.Reason, "configuration is still condition 1")

	// A revoke is a STATE. Revoking a gate moves it to REVOKED, which is
	// refused by the state check above; `revoked_at` on a SANDBOX row is
	// residue of an earlier life, which cp_gate_sandbox clears on the way in
	// (migration 00791). Reading the residue as a refusal is what made every
	// gate sandbox-activated out of REVOKED -- a documented source -- report
	// success and stay permanently inert (F-160).
	revoked := now.Add(-time.Second)
	v = EvaluateWith(row(func(g *Gate) { g.RevokedAt = &revoked }), true, true, now)
	assert.True(t, v.Active, "a SANDBOX row is judged by the sandbox conditions, not by a stale revoke")
	assert.True(t, v.Sandbox)
	v = EvaluateWith(&Gate{
		Capability: NativeMarketTrading, Environment: "STAGING", State: StateRevoked,
		EffectiveAt: &effective, RevokedAt: &revoked,
	}, true, true, now)
	assert.False(t, v.Active, "and a gate that IS revoked is in REVOKED, where no sandbox tier makes it active")
	assert.Equal(t, ReasonStateNotActive, v.Reason)

	v = EvaluateWith(row(func(g *Gate) { g.EffectiveAt = nil }), true, true, now)
	assert.Equal(t, ReasonNoEffectiveAt, v.Reason)
	future := now.Add(time.Hour)
	v = EvaluateWith(row(func(g *Gate) { g.EffectiveAt = &future }), true, true, now)
	assert.Equal(t, ReasonNotYetEffective, v.Reason)
	past := now.Add(-time.Second)
	v = EvaluateWith(row(func(g *Gate) { g.ExpiresAt = &past }), true, true, now)
	assert.Equal(t, ReasonExpired, v.Reason)

	// The flag does not touch any other state: a DISABLED row stays inactive
	// for the same reason, and an ACTIVE row is judged by the five
	// conditions exactly as before.
	disabled := &Gate{Capability: NativeMarketTrading, Environment: "STAGING", State: StateDisabled}
	assert.Equal(t, ReasonStateNotActive, EvaluateWith(disabled, true, true, now).Reason)
	active := &Gate{Capability: NativeMarketTrading, Environment: "STAGING", State: StateActive, EffectiveAt: &effective}
	got := EvaluateWith(active, true, true, now)
	assert.False(t, got.Active, "an ACTIVE row with no approval chain is inactive, sandbox tier or not")
	assert.Equal(t, Evaluate(active, true, now).Reason, got.Reason, "for exactly the reason it always was")
	assert.NotEmpty(t, got.Reason)
	assert.False(t, got.Sandbox)
}

// SANDBOX is reachable only from the states a fresh ceremony starts from, and
// leaves only to them: it is never a step towards ACTIVE.
func TestSandboxIsNotOnThePathToActive(t *testing.T) {
	t.Parallel()
	for _, from := range []GateState{StateDisabled, StateRevoked, StateExpired} {
		assert.True(t, CanTransition(from, StateSandbox), "%s -> SANDBOX", from)
	}
	for _, from := range []GateState{StatePendingApproval, StateApproved, StateActive, StateSuspended} {
		assert.False(t, CanTransition(from, StateSandbox), "%s -> SANDBOX must be illegal: it would move a real approval's state", from)
	}
	for _, to := range []GateState{StatePendingApproval, StateApproved, StateActive, StateSuspended, StateExpired} {
		assert.False(t, CanTransition(StateSandbox, to), "SANDBOX -> %s must be illegal", to)
	}
	assert.True(t, CanTransition(StateSandbox, StateDisabled))
	assert.True(t, CanTransition(StateSandbox, StateRevoked))
	require.True(t, StateSandbox.Valid())
}

// An Admin not built for a sandbox tier refuses the operation before any
// query, whatever the principal holds.
func TestSandboxOpIsRefusedOutsideASandboxTier(t *testing.T) {
	t.Parallel()
	c, err := NewChecker("STAGING", func(Capability) bool { return true }, fakeClock{})
	require.NoError(t, err)
	assert.False(t, c.SandboxAllowed())
	assert.True(t, c.WithSandbox(true).SandboxAllowed())
	assert.False(t, c.SandboxAllowed(), "WithSandbox returns a copy; the original is unchanged")
}

type fakeClock struct{}

func (fakeClock) Now() time.Time { return time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC) }
