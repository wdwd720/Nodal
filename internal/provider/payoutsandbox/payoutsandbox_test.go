package payoutsandbox

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/payout"
)

var _ payout.Provider = (*Provider)(nil)

// The provider refuses PROD on its own, before any registry has a say.
func TestNew_RefusesProd(t *testing.T) {
	t.Parallel()
	_, err := New(config.EnvProd, nil)
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	for _, env := range []config.Environment{config.EnvStaging, config.EnvDev, config.EnvTest, config.EnvLocal} {
		p, err := New(env, nil)
		require.NoError(t, err, env)
		assert.Equal(t, Name, p.Name())
		caps := p.Capabilities()
		assert.Equal(t, payout.AvailabilitySandbox, caps.Availability)
		assert.True(t, caps.RequiresKYC)
	}
}

// A submission is ACCEPTED, the same key is the same payout, a lookup settles
// it only once SettleAfter has passed, and a key never seen is UNKNOWN.
func TestSubmitAndLookup_SettleLaterMoveNothing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	p, err := New(config.EnvStaging, clock)
	require.NoError(t, err)
	ctx := context.Background()

	first, err := p.Submit(ctx, payout.SubmitRequest{IdempotencyKey: "k-1"})
	require.NoError(t, err)
	assert.Equal(t, payout.ProviderAccepted, first.Status)
	assert.Equal(t, "sandbox-k-1", first.ProviderReference)
	assert.Nil(t, first.SettledAt)

	again, err := p.Submit(ctx, payout.SubmitRequest{IdempotencyKey: "k-1"})
	require.NoError(t, err)
	assert.Equal(t, first, again, "the same idempotency key is the same payout")

	pending, err := p.Lookup(ctx, "k-1")
	require.NoError(t, err)
	assert.Equal(t, payout.ProviderAccepted, pending.Status, "not settled before SettleAfter")

	now = now.Add(SettleAfter - time.Millisecond)
	pending, err = p.Lookup(ctx, "k-1")
	require.NoError(t, err)
	assert.Equal(t, payout.ProviderAccepted, pending.Status)

	now = now.Add(time.Millisecond)
	settled, err := p.Lookup(ctx, "k-1")
	require.NoError(t, err)
	assert.Equal(t, payout.ProviderSettled, settled.Status)
	require.NotNil(t, settled.SettledAt)
	assert.Equal(t, time.Date(2026, 9, 10, 12, 0, 10, 0, time.UTC), settled.SettledAt.UTC())
	assert.Equal(t, "sandbox-k-1", settled.ProviderReference)

	// A later submission under the settled key still reports settled.
	late, err := p.Submit(ctx, payout.SubmitRequest{IdempotencyKey: "k-1"})
	require.NoError(t, err)
	assert.Equal(t, payout.ProviderSettled, late.Status)

	unknown, err := p.Lookup(ctx, "never-submitted")
	require.NoError(t, err)
	assert.Equal(t, payout.ProviderUnknown, unknown.Status, "forgetfulness is UNKNOWN, never FAILED")
	assert.NotEmpty(t, unknown.FailureReason)
}
