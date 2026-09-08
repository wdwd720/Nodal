package withdrawal_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/withdrawal"
)

// What the production velocity policy actually permits, stated as a test
// (F-68).
//
// `cmd/api` wires a zero `VelocityPolicy` under a comment that read "the bounds
// stay at zero (no rolling allowance)". A zero policy bounds nothing:
// `MaxPerRequest` is not positive so the per-request branch is skipped, and both
// window limits are zero so `Check` returns nil before looking at anything.
//
// The gate is what refuses a withdrawal today, several checks earlier, and it is
// the only control here. This test exists so that fact is recorded rather than
// believed, and so that whoever approves the WITHDRAWALS capability has to
// confront it: the day the gate opens, this test is the one that says the
// velocity policy is still a no-op.

func TestUnboundedVelocityBoundsNothing(t *testing.T) {
	p := withdrawal.UnboundedVelocity()
	require.NoError(t, p.Validate(), "the zero policy must remain constructible; the gate is the control")
	assert.True(t, p.PermitsEverything())

	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	huge, err := money.ParseQuantity("340282366920938463463374607431768211455") // 2^128 - 1
	require.NoError(t, err)
	assert.NoError(t, p.Check(huge, nil, now),
		"the wired policy refused something; if that is now true, cmd/api's comment and this test both need rewriting")
}

// TestARealPolicyActuallyBounds is the positive control. Without it the test
// above could pass against a `Check` that had stopped refusing anything at all,
// and would then be asserting that a broken bound is an absent one.
func TestARealPolicyActuallyBounds(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	ten, err := money.ParseQuantity("10")
	require.NoError(t, err)
	eleven, err := money.ParseQuantity("11")
	require.NoError(t, err)

	p := withdrawal.VelocityPolicy{MaxPerRequest: ten, Window: time.Hour}
	require.NoError(t, p.Validate())
	assert.False(t, p.PermitsEverything())

	require.NoError(t, p.Check(ten, nil, now), "a request at the limit is allowed")
	err = p.Check(eleven, nil, now)
	require.Error(t, err, "a request above the per-request limit was allowed")
	assert.Contains(t, err.Error(), "per-request limit")
}
