//go:build integration

package gates

import (
	"context"
	"testing"
	"time"

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
//
// It starts from REVOKED, which is the documented source that used to produce a
// row that could never be active: cp_gate_sandbox wrote the state and left
// `revoked_at`, and evaluateSandbox refused on it before it looked at anything
// else, so "sandbox-activated" and "refused with gate is revoked" were true of
// the same gate at the same time (F-160). Migration 00791 clears the revoke with
// the rest of the row.
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
	// An operator pulls the capability during an incident, and later the tier
	// needs it back to rehearse with.
	_, err := f.do(t, f.bg("sbx-revoker"), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Revoke(ctx, tx, c, "an operator pulled this capability during an incident")
	})
	require.NoError(t, err)
	require.Equal(t, StateRevoked, f.get(t, c).State)
	require.NotNil(t, f.get(t, c).RevokedAt, "precondition: the revoke is recorded on the row")

	// An Admin not built for a sandbox tier refuses before any query.
	_, err = f.do(t, f.op("sbx-1", security.RoleAdmin), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
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
	assert.Nil(t, g.RevokedAt, "entering SANDBOX clears the revoke it was entered from; a row that kept it could never be active")
	assert.Empty(t, g.RevokeReason)
	assert.Empty(t, g.Approvers, "a sandbox gate carries no approval chain")
	assert.Equal(t, 0, g.ApprovalVersion)
	assert.Empty(t, g.LegalReviewRef+g.ProviderContractRef+g.RiskApprovalRef+g.SecurityApprovalRef)

	// History was written by the function, in the same statement.
	added := transitionsSince(t, g.ID, baseline)
	require.Len(t, added, 1)
	assert.Equal(t, StateRevoked, added[0].From)
	assert.Equal(t, StateSandbox, added[0].To)
	assert.Equal(t, security.ActorOperator, added[0].ActorType)
	blank := Gate{}.EvidenceDigest()
	assert.Equal(t, blank[:], added[0].EvidenceHash,
		"a sandbox transition attests the digest of no evidence, because no evidence was produced for it")

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

// A gate that reached ACTIVE by the real ceremony and then expired carries an
// approval version: three chain entries, a proposer, four evidence references
// and the digest they attested. Sandboxing it must leave none of that on the
// row, because every document that describes a SANDBOX row -- migration 00755's
// header, ADR-0023 §1, the package doc and the operator console's "none, and
// none is expected" -- says it carries no approval, and the API returns the
// columns verbatim beside `state: SANDBOX, active: true` (F-161).
func TestIntegration_SandboxFromAFinishedCeremonyCarriesNoneOfIt(t *testing.T) {
	f := newFixture(t, "STAGING")
	const c = CreditPurchase // high risk: all four evidence references required
	f.reset(t, c)
	if g := f.get(t, c); g.State == StateSandbox {
		_, err := f.do(t, f.op("sbx-reset", security.RoleAdmin), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
			return f.admin.WithSandbox(true).Unsandbox(ctx, tx, c, "test reset")
		})
		require.NoError(t, err)
	}

	// The real ceremony, rehearsed on the tier that exists to rehearse it:
	// three distinct principals and four evidence references.
	prop := highRiskProposal("a real ceremony on the sandbox tier, as a rehearsal should be")
	prop.EffectiveAt = f.clk.Now()
	prop.ExpiresAt = f.clk.Now().Add(time.Hour)
	_, err := f.do(t, f.op("cer-proposer", security.RoleAdmin), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Propose(ctx, tx, c, prop)
	})
	require.NoError(t, err)
	f.clk.Advance(time.Minute)
	_, err = f.do(t, f.bg("cer-approver"), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Approve(ctx, tx, c, "second principal")
	})
	require.NoError(t, err)
	f.clk.Advance(time.Minute)
	active, err := f.do(t, f.bg("cer-activator"), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Activate(ctx, tx, c, "third principal")
	})
	require.NoError(t, err)
	require.Equal(t, StateActive, active.State)
	require.NotEmpty(t, active.Approvers, "precondition: the ceremony recorded a chain")
	require.Len(t, active.MissingEvidence(), 0, "precondition: the ceremony recorded every evidence reference")

	// It expires, which is a documented source of SANDBOX.
	f.clk.Advance(2 * time.Hour)
	require.NoError(t, f.inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		_, eerr := f.admin.ExpireDue(ctx, tx, f.clk.Now())
		return eerr
	}))
	require.Equal(t, StateExpired, f.get(t, c).State)

	baseline := transitionsOf(t, f.get(t, c).ID)
	g, err := f.do(t, f.op("sbx-operator", security.RoleAdmin), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.WithSandbox(true).Sandbox(ctx, tx, c, "sandbox tier: rehearse the purchase path")
	})
	require.NoError(t, err)
	require.Equal(t, StateSandbox, g.State)

	// The row the API returns, and the row the console describes.
	assert.Empty(t, g.Approvers, "a SANDBOX row carries no approval chain, whatever it was sandboxed from")
	assert.Empty(t, g.DistinctApprovers())
	assert.Empty(t, g.ProposedBy, "and names no proposer")
	assert.Zero(t, g.ApprovalVersion, "and is not on an approval version")
	assert.Empty(t, g.EvidenceHashes)
	assert.Len(t, g.MissingEvidence(), 4, "and records no evidence reference: %+v", g.MissingEvidence())
	assert.Equal(t, Gate{}.EvidenceDigestHex(), g.EvidenceDigestHex(),
		"so its evidence digest is the digest of nothing")
	assert.Nil(t, g.RevokedAt)

	// And the transition that produced it attests the same nothing, rather than
	// the digest of the approval that reached ACTIVE.
	added := transitionsSince(t, g.ID, baseline)
	require.Len(t, added, 1)
	assert.Equal(t, StateExpired, added[0].From)
	blank := Gate{}.EvidenceDigest()
	assert.Equal(t, blank[:], added[0].EvidenceHash)

	// It is active for a sandbox-tier checker, which is the point of the row.
	v, err := f.checker.WithSandbox(true).IsActive(context.Background(), testDB, c)
	require.NoError(t, err)
	assert.True(t, v.Active)
	assert.True(t, v.Sandbox)
	assert.Zero(t, v.ApprovalVersion)
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
