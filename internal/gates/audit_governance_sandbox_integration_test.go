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

// Adversarial audit (goal §54), area governance-sandbox-tier. Each test FAILS
// against the current tree and reproduces one defect from the audit report,
// against the real cp_gate_sandbox, the real triggers and the real privileges.

// auditSandboxReset drives c to DISABLED with no revoke recorded, whatever a
// previous run left behind. It is deliberately not fixture.reset: that helper
// returns early on REVOKED and EXPIRED, which are precisely the states these
// tests are about.
func auditSandboxReset(t *testing.T, f *fixture, c Capability) {
	t.Helper()
	for i := 0; i < 6; i++ {
		g := f.get(t, c)
		switch g.State {
		case StateDisabled:
			if g.RevokedAt == nil {
				return
			}
			// A DISABLED row still carrying a revoke: only a proposal clears
			// it, and the ceremony below puts it back.
			_, err := f.do(t, f.op("audit-reset-a", security.RoleAdmin), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
				return f.admin.Propose(ctx, tx, c, highRiskProposal("audit fixture: clear the stale revoke"))
			})
			require.NoError(t, err)
		case StateSandbox:
			_, err := f.do(t, f.op("audit-reset-a", security.RoleAdmin), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
				return f.admin.WithSandbox(true).Unsandbox(ctx, tx, c, "audit fixture reset")
			})
			require.NoError(t, err)
		case StatePendingApproval, StateApproved, StateActive, StateSuspended, StateExpired, StateRevoked:
			// Everything else goes through SANDBOX to reach DISABLED, which is
			// the only edge into it.
			if !CanTransition(g.State, StateSandbox) {
				_, err := f.do(t, f.bg("audit-reset-b"), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
					return f.admin.Revoke(ctx, tx, c, "audit fixture reset")
				})
				require.NoError(t, err)
				continue
			}
			_, err := f.do(t, f.op("audit-reset-a", security.RoleAdmin), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
				return f.admin.WithSandbox(true).Sandbox(ctx, tx, c, "audit fixture reset")
			})
			require.NoError(t, err)
		}
	}
	t.Fatalf("audit fixture: %s/%s would not settle on a clean DISABLED row", c, f.env)
}

// F-gov-1. REVOKED is a documented source of SANDBOX and cp_gate_sandbox does
// not clear revoked_at, so the move reports success and produces a gate that
// can never be active -- including through BootstrapSandbox, which cmd/api
// runs at boot and which logs the gate as "sandbox-activated".
func TestIntegration_AUDIT_SandboxFromRevokedKeepsTheRevoke(t *testing.T) {
	f := newFixture(t, "STAGING")
	const c = PayoutSettle
	auditSandboxReset(t, f, c)

	_, err := f.do(t, f.bg("audit-revoker"), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Revoke(ctx, tx, c, "an operator pulled this capability during an incident")
	})
	require.NoError(t, err)
	require.Equal(t, StateRevoked, f.get(t, c).State)

	// The boot path. cmd/api calls exactly this for every capability named in
	// CP_API_SANDBOX_GATES, and logs each returned gate with
	// "capability sandbox-activated at boot".
	var moved []Gate
	require.NoError(t, f.inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		var berr error
		moved, berr = BootstrapSandbox(ctx, tx, f.env, []Capability{c}, f.clk, f.audit)
		return berr
	}))
	require.Len(t, moved, 1, "BootstrapSandbox reports the gate as moved")
	require.Equal(t, StateSandbox, moved[0].State)

	g := f.get(t, c)
	assert.Nil(t, g.RevokedAt,
		"cp_gate_sandbox must clear revoked_at when it writes a SANDBOX row: it writes effective_at and "+
			"expires_at and leaves the revoke, so the row it produces is refused by evaluateSandbox "+
			"before any sandbox condition is consulted")

	v, err := f.checker.WithSandbox(true).IsActive(context.Background(), testDB, c)
	require.NoError(t, err)
	assert.True(t, v.Active,
		"the deployment sandbox-activated %s at boot and logged that it did; the checker reports inactive "+
			"with reason %q, so the rehearsal this tier exists for is refused and nothing said so", c, v.Reason)
}

// F-gov-2. Migration 00755's header, ADR-0023, the package doc and the
// operator console all state that a SANDBOX gate carries no approval chain and
// no evidence. That is true only of a gate sandboxed from a freshly
// bootstrapped DISABLED row. cp_gate_sandbox deliberately does not touch those
// columns, so a gate sandboxed from EXPIRED or REVOKED -- both documented
// sources -- keeps the whole approval version that reached ACTIVE: three chain
// entries, a proposer and all four evidence references. GET /v1/admin/gates
// returns every one of them alongside `state: SANDBOX, active: true`.
func TestIntegration_AUDIT_SandboxFromExpiredCarriesTheApprovalItClaimsNotToHave(t *testing.T) {
	f := newFixture(t, "STAGING")
	const c = CreditPurchase // high risk: all four evidence references required
	auditSandboxReset(t, f, c)

	prop := highRiskProposal("a real ceremony on the sandbox tier, as a rehearsal should be")
	prop.EffectiveAt = f.clk.Now()
	prop.ExpiresAt = f.clk.Now().Add(time.Hour)
	_, err := f.do(t, f.op("audit-proposer", security.RoleAdmin), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Propose(ctx, tx, c, prop)
	})
	require.NoError(t, err)
	f.clk.Advance(time.Minute)
	_, err = f.do(t, f.bg("audit-approver"), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Approve(ctx, tx, c, "second principal")
	})
	require.NoError(t, err)
	f.clk.Advance(time.Minute)
	_, err = f.do(t, f.bg("audit-activator"), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Activate(ctx, tx, c, "third principal")
	})
	require.NoError(t, err)

	f.clk.Advance(2 * time.Hour)
	require.NoError(t, f.inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		_, eerr := f.admin.ExpireDue(ctx, tx, f.clk.Now())
		return eerr
	}))
	require.Equal(t, StateExpired, f.get(t, c).State)

	g, err := f.do(t, f.op("audit-sandboxer", security.RoleAdmin), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.WithSandbox(true).Sandbox(ctx, tx, c, "sandbox tier: rehearse the purchase path")
	})
	require.NoError(t, err)
	require.Equal(t, StateSandbox, g.State)

	v, err := f.checker.WithSandbox(true).IsActive(context.Background(), testDB, c)
	require.NoError(t, err)
	require.True(t, v.Active, "precondition: the sandbox row is active for a sandbox-tier checker")
	require.True(t, v.Sandbox)

	// Everything below is returned verbatim by GET /v1/admin/gates for a row
	// the same response marks active and sandbox.
	assert.Empty(t, g.Approvers,
		"a SANDBOX gate carries no approval chain (00755, ADR-0023, sandbox.go, apps/admin gates.ts "+
			"renders \"none, and none is expected\"); this one carries %d entries naming %v",
		len(g.Approvers), g.DistinctApprovers())
	assert.Empty(t, g.ProposedBy, "and names no proposer")
	assert.Zero(t, g.ApprovalVersion, "and is not on an approval version")
	assert.Len(t, g.MissingEvidence(), 4,
		"the console tells the operator a sandbox gate records no evidence; this one records "+
			"legal=%q provider=%q risk=%q security=%q",
		g.LegalReviewRef, g.ProviderContractRef, g.RiskApprovalRef, g.SecurityApprovalRef)
	assert.Equal(t, Gate{}.EvidenceDigestHex(), g.EvidenceDigestHex(),
		"and the sandbox transition and its audit event attest the evidence digest of the real approval")
}

// F-gov-3. `sandboxGatesAtBoot` turns a gate that is part of a real ceremony
// into a fatal startup error, and cmd/api's wire step returns it -- so running
// the dual-control ceremony on a sandbox tier for any capability named in
// CP_API_SANDBOX_GATES stops the deployment booting until somebody edits the
// blueprint. The only in-machine escape, a revoke, lands on F-gov-1's inert
// gate.
func TestIntegration_AUDIT_ARealCeremonyOnASandboxCapabilityBlocksTheNextBoot(t *testing.T) {
	f := newFixture(t, "STAGING")
	const c = Marketplace
	auditSandboxReset(t, f, c)

	// An operator rehearses step one of the real ceremony, which is what a
	// STAGING tier is for.
	_, err := f.do(t, f.op("audit-proposer", security.RoleAdmin), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Propose(ctx, tx, c, highRiskProposal("rehearsing the real activation ceremony"))
	})
	require.NoError(t, err)
	require.Equal(t, StatePendingApproval, f.get(t, c).State)

	// The next restart. cmd/api runs this before the server is built and
	// returns the error, so the process exits.
	berr := f.inTx(t, func(ctx context.Context, tx pgx.Tx) error {
		_, e := BootstrapSandbox(ctx, tx, f.env, []Capability{c}, f.clk, f.audit)
		return e
	})
	assert.NoError(t, berr,
		"CP_API_SANDBOX_GATES names %s, an operator proposed it, and the boot-time sandbox step now "+
			"refuses with %v; cmd/api returns that error from wire, so a deployment cannot restart "+
			"until the blueprint is edited. Leaving a real ceremony alone must not mean refusing to boot",
		c, errs.CodeOf(berr))
}
