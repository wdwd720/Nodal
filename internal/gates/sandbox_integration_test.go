//go:build integration

package gates

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
)

// The sandbox tier, against the real function and the real triggers: a gate
// enters SANDBOX by one operator on a sandbox tier, is active there and
// nowhere else, carries no approval, leaves to DISABLED, and can never do any
// of it in PROD.
func TestIntegration_SandboxGate(t *testing.T) {
	f := newFixture(t, "STAGING")
	const c = NativeMarketTrading
	f.reset(t, c)
	// A previous run may have left it SANDBOX; put it back to DISABLED so
	// the whole path is exercised.
	if g := f.get(t, c); g.State == StateSandbox {
		_, err := f.do(t, f.op("sbx-reset", security.RoleAdmin), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
			return f.admin.WithSandbox(true).Unsandbox(ctx, tx, c, "test reset")
		})
		require.NoError(t, err)
	}

	// An Admin not built for a sandbox tier refuses before any query.
	_, err := f.do(t, f.op("sbx-1", security.RoleAdmin), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Sandbox(ctx, tx, c, "rehearsal")
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))

	sbx := f.admin.WithSandbox(true)
	baseline := transitionsOf(t, f.get(t, c).ID)
	g, err := f.do(t, f.op("sbx-1", security.RoleAdmin), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return sbx.Sandbox(ctx, tx, c, "rehearsal of the native market on the sandbox tier")
	})
	require.NoError(t, err)
	assert.Equal(t, StateSandbox, g.State)
	assert.NotNil(t, g.EffectiveAt)
	assert.Empty(t, g.Approvers, "a sandbox gate carries no approval chain")
	assert.Equal(t, 0, g.ApprovalVersion)
	assert.Empty(t, g.LegalReviewRef+g.ProviderContractRef+g.RiskApprovalRef+g.SecurityApprovalRef)

	// History was written by the function, in the same statement.
	added := transitionsSince(t, g.ID, baseline)
	require.Len(t, added, 1)
	assert.Equal(t, StateDisabled, added[0].From)
	assert.Equal(t, StateSandbox, added[0].To)
	assert.Equal(t, security.ActorOperator, added[0].ActorType)

	// Active for a sandbox-tier checker, inactive with a reason for any other.
	v, err := f.checker.WithSandbox(true).IsActive(context.Background(), testDB, c)
	require.NoError(t, err)
	assert.True(t, v.Active)
	assert.True(t, v.Sandbox)
	v = f.verdict(t, c)
	assert.False(t, v.Active, "the fixture's checker is not a sandbox tier")
	assert.Equal(t, ReasonSandboxNotAllowedHere, v.Reason)

	// Not a step towards ACTIVE: it cannot be proposed or approved from here.
	_, err = f.do(t, f.op("sbx-2", security.RoleAdmin), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Propose(ctx, tx, c, Proposal{Reason: "trying to promote a sandbox gate"})
	})
	require.Error(t, err, "SANDBOX -> PENDING_APPROVAL must be refused")

	// And it leaves to DISABLED, where the real ceremony starts.
	g, err = f.do(t, f.op("sbx-1", security.RoleAdmin), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return sbx.Unsandbox(ctx, tx, c, "rehearsal over")
	})
	require.NoError(t, err)
	assert.Equal(t, StateDisabled, g.State)
	assert.Nil(t, g.EffectiveAt)
}

func TestIntegration_SandboxGateCanNeverExistInProd(t *testing.T) {
	f := newFixture(t, "PROD")
	const c = CreditPurchase
	f.reset(t, c)
	_, err := f.do(t, f.op("sbx-prod", security.RoleAdmin), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.WithSandbox(true).Sandbox(ctx, tx, c, "this must be refused")
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "GATE_SANDBOX_NEVER_IN_PROD")
	assert.Equal(t, StateDisabled, f.get(t, c).State, "the refusal moved nothing")

	// And the boot-time path is refused before it reaches the database.
	err = f.inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := BootstrapSandbox(ctx, tx, "PROD", []Capability{c}, f.clk, f.audit)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
}

// The blueprint's sandbox gates are activated once and left alone after.
func TestIntegration_BootstrapSandboxIsIdempotentAndNeverMovesAnApproval(t *testing.T) {
	f := newFixture(t, "STAGING")
	caps := []Capability{Marketplace, NativeAssetCreation}
	for _, c := range caps {
		f.reset(t, c)
		if g := f.get(t, c); g.State == StateSandbox {
			_, err := f.do(t, f.op("sbx-reset", security.RoleAdmin), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
				return f.admin.WithSandbox(true).Unsandbox(ctx, tx, c, "test reset")
			})
			require.NoError(t, err)
		}
	}
	var moved []Gate
	require.NoError(t, f.inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		moved, err = BootstrapSandbox(ctx, tx, "STAGING", caps, f.clk, f.audit)
		return err
	}))
	require.Len(t, moved, 2)
	for _, g := range moved {
		assert.Equal(t, StateSandbox, g.State)
	}
	audits := f.audit.count()

	require.NoError(t, f.inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		moved, err = BootstrapSandbox(ctx, tx, "STAGING", caps, f.clk, f.audit)
		return err
	}))
	assert.Empty(t, moved, "a second boot moves nothing")
	assert.Equal(t, audits, f.audit.count(), "and writes no audit event")

	// A gate that is part of a real ceremony is refused, not moved.
	const real = LiveFunding
	f.reset(t, real)
	if g := f.get(t, real); g.State == StateSandbox {
		_, err := f.do(t, f.op("sbx-reset", security.RoleAdmin), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
			return f.admin.WithSandbox(true).Unsandbox(ctx, tx, real, "test reset")
		})
		require.NoError(t, err)
	}
	_, err := f.do(t, f.op("proposer-1", security.RoleAdmin), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Propose(ctx, tx, real, highRiskProposal("a real proposal"))
	})
	require.NoError(t, err)
	err = f.inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := BootstrapSandbox(ctx, tx, "STAGING", []Capability{real}, f.clk, f.audit)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))
	assert.Equal(t, StatePendingApproval, f.get(t, real).State, "the real proposal is untouched")
}
