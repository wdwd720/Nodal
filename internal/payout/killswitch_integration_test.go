//go:build integration

package payout_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/payout"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// The emergency controls, against the real switches, the real transitions table
// and the real conversion-request path (F-163, D-092).
//
// POLICY_AUTHORITY §2 declares the blocking matrix as a property of every
// guarded operation, and puts the WITHDRAW class under WITHDRAWALS_DISABLE,
// GLOBAL_NEW_RISK_KILL and ACCOUNT_FREEZE. The conversion request is the
// withdrawal surface this product actually reaches -- the sandbox legal policy
// is the only one that permits PAYOUT at all -- and internal/payout imported no
// kill switch, so an operator could activate all three and every part of it kept
// working: a request could be created, value reserved out of the spendable
// balance, and the whole thing handed to a provider.
//
// Each test below activates one switch, proves the refusal names it, releases
// it, and proves the path works again. A refusal that survived the release
// would be a different defect with the same symptom.

// killFixture is a payout fixture plus the controller that activates switches.
type killFixture struct {
	*fixture
	ctl *killswitch.Controller
	ver *fakeReleaseVerifier
}

type fakeReleaseVerifier struct {
	approvals map[string]killswitch.Approval
}

func (f *fakeReleaseVerifier) VerifyApproved(_ context.Context, _ db.Querier, approvalID, kind, targetID string) (killswitch.Approval, error) {
	a, ok := f.approvals[approvalID]
	if !ok || a.Kind != kind || a.TargetID != targetID {
		return killswitch.Approval{}, errs.New(errs.CodeForbidden, "no such approval")
	}
	return a, nil
}

type nopKillAudit struct{}

func (nopKillAudit) Append(context.Context, pgx.Tx, killswitch.AuditEvent) error { return nil }

func newKillFixture(t *testing.T) *killFixture {
	t.Helper()
	f := newFixture(t)
	ver := &fakeReleaseVerifier{approvals: map[string]killswitch.Approval{}}
	ctl, err := killswitch.NewController(f.clk, nopKillAudit{}, ver)
	require.NoError(t, err)
	return &killFixture{fixture: f, ctl: ctl, ver: ver}
}

// operator is a principal that may activate and release switches, strongly
// authenticated at the fixture's current time.
func (k *killFixture) operator(sub string) security.Principal {
	p := security.Principal{
		SubjectID: sub, ActorType: security.ActorOperator,
		Roles:    []security.Role{security.RoleOperations, security.RoleBreakGlass},
		AuthTime: k.clk.Now(), AMR: []string{"mfa"},
	}
	until := k.clk.Now().Add(time.Hour)
	p.BreakGlassUntil = &until
	return p
}

// activate turns a switch on and registers its release cleanup, so a global
// switch can never leak into the next test in this package.
func (k *killFixture) activate(t *testing.T, kind killswitch.Kind, scope, reason string) {
	t.Helper()
	require.NoError(t, testDB.InTx(k.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := k.ctl.Activate(security.WithPrincipal(ctx, k.operator("ops-kill")), tx, kind, scope, reason)
			return err
		}))
	t.Cleanup(func() {
		if s, err := killswitch.Get(k.ctx, testDB, kind, scope); err == nil && s.Active {
			k.release(t, kind, scope)
		}
	})
}

// release turns it off again. A SEVERE switch needs a verified dual-controlled
// approval, which is what the fake verifier stands in for: the approval
// machinery is internal/admin's and has its own tests.
func (k *killFixture) release(t *testing.T, kind killswitch.Kind, scope string) {
	t.Helper()
	var approvalID *string
	if kind.Severity() == killswitch.SeveritySevere {
		id := uuid.NewString()
		k.ver.approvals[id] = killswitch.Approval{
			ID: id, Kind: killswitch.ApprovalKindRelease, TargetID: killswitch.ReleaseTargetID(kind, scope),
			ProposedBy: "ops-kill", ApprovedBy: "ops-other",
			ApprovedAt: k.clk.Now(), ExpiresAt: k.clk.Now().Add(time.Hour),
		}
		approvalID = &id
	}
	require.NoError(t, testDB.InTx(k.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := k.ctl.Release(security.WithPrincipal(ctx, k.operator("ops-kill")), tx,
				kind, scope, "the incident is over", approvalID)
			return err
		}))
}

// TestIntegration_EveryWithdrawSwitchStopsAConversionRequest is the matrix of
// POLICY_AUTHORITY §2, applied to the path that had none of it.
func TestIntegration_EveryWithdrawSwitchStopsAConversionRequest(t *testing.T) {
	f := newKillFixture(t)
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 1_000)

	for _, tc := range []struct {
		kind  killswitch.Kind
		scope string
		why   string
	}{
		{killswitch.WithdrawalsDisable, killswitch.GlobalScope, "withdrawals are stopped while the provider is investigated"},
		{killswitch.GlobalNewRiskKill, killswitch.GlobalScope, "everything stops"},
		{killswitch.AccountFreeze, "", "this account is under review"},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			scope := tc.scope
			if tc.kind == killswitch.AccountFreeze {
				scope = f.account.String()
			}
			spendable := f.balance(ledger.CodeCreditBalance).String()
			reserved := f.balance(ledger.CodePayoutReserved).String()
			f.activate(t, tc.kind, scope, tc.why)

			_, _, err := f.create(100, f.input())
			require.Error(t, err, "%s must stop a conversion request", tc.kind)
			assert.Equal(t, errs.CodeKillSwitchActive, errs.CodeOf(err))
			assert.Contains(t, err.Error(), string(tc.kind),
				"the refusal names the switch an operator activated, not a downstream symptom")

			// Nothing moved: the refusal is before the reservation, so the
			// user's spendable balance is exactly what it was.
			assert.Equal(t, spendable, f.balance(ledger.CodeCreditBalance).String())
			assert.Equal(t, reserved, f.balance(ledger.CodePayoutReserved).String())

			// And the release reopens it. Without this the test would pass
			// against a path that refuses a conversion request always, which
			// is a different defect with the same symptom.
			f.release(t, tc.kind, scope)
			req, dec, err := f.create(100, f.input())
			require.NoError(t, err, "releasing the switch reopens the path")
			require.True(t, dec.Sufficient())
			require.Equal(t, "100", req.ReservedQuantity.String())
		})
	}
}

// TestIntegration_AFrozenAccountCannotConvert: the account's own status, read in
// the same transaction as the kill switches. internal/withdrawal has always
// made this check at the same boundary; the conversion request made neither.
func TestIntegration_AFrozenAccountCannotConvert(t *testing.T) {
	f := newKillFixture(t)
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 1_000)

	repo := accounts.NewRepository()
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := repo.Transition(ctx, tx, f.account, accounts.StatusChange{
				To: accounts.StatusFrozen, ActorType: "OPERATOR", ActorID: "ops-kill",
				Reason: "under review",
			}, f.clk.Now())
			return err
		}))

	_, _, err := f.create(100, f.input())
	require.Error(t, err)
	assert.Equal(t, errs.CodeAccountFrozen, errs.CodeOf(err))
	assert.Equal(t, "1000", f.balance(ledger.CodeCreditBalance).String(), "nothing was reserved")

	// And unfreezing restores it, so the refusal is the status and not a
	// permanent state of the account.
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, terr := repo.Transition(ctx, tx, f.account, accounts.StatusChange{
				To: accounts.StatusActive, ActorType: "OPERATOR", ActorID: "ops-kill",
				Reason: "review closed",
			}, f.clk.Now())
			return terr
		}))
	_, dec, err := f.create(100, f.input())
	require.NoError(t, err)
	assert.True(t, dec.Sufficient())
}

// TestIntegration_AKillSwitchStopsASubmissionMidFlight is the second half of the
// guard: the request already exists and the value is already reserved, and the
// sweep is about to hand it to a provider under a committed idempotency key.
// After that only reconciliation can answer what happened, so this is the last
// point at which an operator can still stop value leaving.
func TestIntegration_AKillSwitchStopsASubmissionMidFlight(t *testing.T) {
	f := newKillFixture(t)
	f.issue(valuedomain.OriginCreatorEarning, valuedomain.FinalitySettled, 1_000)

	req, dec, err := f.create(400, f.input())
	require.NoError(t, err)
	require.True(t, dec.Sufficient())
	require.Equal(t, payout.StateVerified, req.State)

	f.activate(t, killswitch.WithdrawalsDisable, killswitch.GlobalScope, "stop paying out while the provider is investigated")
	_, err = f.svc.Submit(f.ctx, testDB, req.ID, "sandbox")
	require.Error(t, err)
	assert.Equal(t, errs.CodeKillSwitchActive, errs.CodeOf(err))
	current, gerr := f.svc.Get(f.ctx, testDB, req.ID)
	require.NoError(t, gerr)
	assert.Equal(t, payout.StateVerified, current.State,
		"the request stays where it was, with the value still reserved")
	assert.Equal(t, "400", f.balance(ledger.CodePayoutReserved).String())
	assert.Zero(t, f.provider.Submits(), "and the provider was never called")

	f.release(t, killswitch.WithdrawalsDisable, killswitch.GlobalScope)
	submitted, err := f.svc.Submit(f.ctx, testDB, req.ID, "sandbox")
	require.NoError(t, err, "the next sweep after the release submits it")
	assert.NotEmpty(t, submitted.ProviderIdempotencyKey)
}
