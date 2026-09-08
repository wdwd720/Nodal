//go:build integration

package gates

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/security"
)

// GT003: the evidence and window guard, observed firing.
//
// Migration 00716 describes this code as "the line that holds when the Go check
// is bypassed" -- the database's own refusal to let a high-risk capability go
// ACTIVE without legal, provider, risk and security sign-off, or on an approval
// whose window has closed. It has five raise sites.
//
// No Go code asserted on it, in production or in a test (F-58). Its siblings
// were all covered: GT001, GT002, GT004 and GT005 each have a case in
// TestIntegration_DatabaseRefusesForgedActivation driving cp_gate_transition as
// cp_app. GT003 was skipped over, and the nearest test,
// TestIntegration_ActivateRefusesExpiredWindow, asserts
// errs.CodeInvalidStateTransition from the Go layer -- which the Go check
// produces on its own, so it would pass unchanged with the SQL guard deleted.
// That is F-53's lesson: a test that passes because a different guard fired has
// not seen the guard it names.
//
// These cases bypass internal/gates entirely, which is the only way to see it.
//
// One thing the first attempt at this test got wrong, worth keeping: blanking an
// evidence column as cp_app is refused outright, because 00701 grants the
// application only UPDATE (version) on capability_gates. The privilege layer
// already covers the attacker-with-the-app-credential case that
// TestIntegration_DatabaseRefusesForgedActivation is about. GT003 covers what is
// left -- a defect in the Go layer, or anything acting as the owner -- so these
// cases drive the owning connection. A version of this test written against
// cp_app would have passed on 42501 and never reached the guard it names.

func TestIntegration_DatabaseRefusesActivationWithoutEvidence(t *testing.T) {
	f := newFixture(t, "DEV")
	// Securities in DEV: high-risk, and a (capability, environment) pair no other
	// test writes, so the gate this test mutates is its own.
	const c = Securities
	require.True(t, IsHighRisk(c), "this test is meaningless on a capability that needs no evidence")

	// Drive a legitimate proposal and approval, then strip the evidence out
	// from under it with raw SQL -- which is what an attacker holding the
	// application's credential does, and what a defect in the Go layer looks
	// like from the database's side.
	proposer, approver := f.op("risk-alice", security.RoleRisk), f.bg("bg-bob")
	_, err := f.do(t, proposer, func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Propose(ctx, tx, c, highRiskProposal("cex pilot"))
	})
	require.NoError(t, err)
	_, err = f.do(t, approver, func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Approve(ctx, tx, c, "reviewed")
	})
	require.NoError(t, err)
	g := f.get(t, c)
	require.Equal(t, StateApproved, g.State)

	for _, col := range []string{"legal_review_ref", "provider_contract_ref", "risk_approval_ref", "security_approval_ref"} {
		t.Run("missing/"+col, func(t *testing.T) {
			// Blank it, try to activate as a third principal (so no dual-control
			// or self-approval rule can be what refuses), then put it back.
			restore := blankEvidence(t, g.ID, col)
			defer restore()

			err := callGateTransition(t, g.ID, g.Version, opActivate, "bg-carol",
				`{"user_id":"bg-carol","step":"ACTIVATE"}`)
			require.Error(t, err, "a high-risk gate activated with no %s", col)
			assert.Equal(t, "GT003", db.SQLState(err), "got %v", err)
			assert.Equal(t, StateApproved, f.get(t, c).State, "the gate moved")
		})
	}

	// An empty string is not evidence either. The guard tests
	// coalesce(btrim(...), '') = '' rather than IS NULL, which is the F-50
	// lesson applied ahead of time -- a reference of "   " would otherwise
	// satisfy a NULL check and mean nothing.
	t.Run("whitespace is not a reference", func(t *testing.T) {
		restore := setEvidence(t, g.ID, "legal_review_ref", "   ")
		defer restore()
		err := callGateTransition(t, g.ID, g.Version, opActivate, "bg-carol",
			`{"user_id":"bg-carol","step":"ACTIVATE"}`)
		require.Error(t, err)
		assert.Equal(t, "GT003", db.SQLState(err), "got %v", err)
	})

	// The positive control. With every reference present, the same call by the
	// same principal is accepted -- so the refusals above are the evidence
	// guard and not something incidental to driving the function directly.
	require.NoError(t, callGateTransition(t, g.ID, g.Version, opActivate, "bg-carol",
		`{"user_id":"bg-carol","step":"ACTIVATE"}`),
		"a guard that refused a complete activation would have made every case above meaningless")
	assert.Equal(t, StateActive, f.get(t, c).State)
}

// TestIntegration_DatabaseRefusesActivationOutsideTheWindow covers the other
// two GT003 conditions: an approval whose expiry has passed, and a gate that
// has been revoked. In both the row still says APPROVED, and only the clock or
// a later revocation makes it unusable.
//
// Each case gets its own gate. The first version of this test activated one
// gate as the positive control and then pushed its state back to APPROVED to
// reuse it, which the AU001 audit binding refuses: a state change needs a
// transition row in the same transaction. That is F-53's second mistake made
// again -- and it is a good refusal, because a test that edits a gate's state
// by hand is not testing the path a gate actually takes.
func TestIntegration_DatabaseRefusesActivationOutsideTheWindow(t *testing.T) {
	f := newFixture(t, "DEV")

	// The positive control first: a gate inside its window activates. Without
	// this the refusals below could be caused by anything.
	live := f.approved(t, CrossChain, 30*time.Minute)
	require.NoError(t, callGateTransition(t, live.ID, live.Version, opActivate, "bg-carol",
		`{"user_id":"bg-carol","step":"ACTIVATE"}`),
		"a gate inside its approval window must still activate")
	require.Equal(t, StateActive, f.get(t, CrossChain).State)

	// An approval whose window has closed. Only expires_at moves, so no state
	// change happens outside cp_gate_transition and the audit binding has
	// nothing to object to.
	expired := f.approved(t, PredictionMarkets, 30*time.Minute)
	setColumn(t, expired.ID, "expires_at", "now() - interval '1 hour'")
	expired = f.get(t, PredictionMarkets)
	err := callGateTransition(t, expired.ID, expired.Version, opActivate, "bg-carol",
		`{"user_id":"bg-carol","step":"ACTIVATE"}`)
	require.Error(t, err, "an expired approval was activated")
	assert.Equal(t, "GT003", db.SQLState(err), "got %v", err)
	assert.Equal(t, StateApproved, f.get(t, PredictionMarkets).State)

	// A revoked gate, whatever its state column says.
	revoked := f.approved(t, Marketplace, time.Hour)
	setColumn(t, revoked.ID, "revoked_at", "now()")
	revoked = f.get(t, Marketplace)
	err = callGateTransition(t, revoked.ID, revoked.Version, opActivate, "bg-carol",
		`{"user_id":"bg-carol","step":"ACTIVATE"}`)
	require.Error(t, err, "a revoked gate was activated")
	assert.Equal(t, "GT003", db.SQLState(err), "got %v", err)
	assert.Equal(t, StateApproved, f.get(t, Marketplace).State)
}

// approved drives a capability to APPROVED through the real proposal and
// approval path, with an approval window of the given length.
func (f *fixture) approved(t *testing.T, c Capability, window time.Duration) Gate {
	t.Helper()
	p := highRiskProposal("window test")
	p.ExpiresAt = f.clk.Now().Add(window)
	_, err := f.do(t, f.op("risk-alice", security.RoleRisk), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Propose(ctx, tx, c, p)
	})
	require.NoError(t, err)
	_, err = f.do(t, f.bg("bg-bob"), func(ctx context.Context, tx pgx.Tx) (Gate, error) {
		return f.admin.Approve(ctx, tx, c, "reviewed")
	})
	require.NoError(t, err)
	g := f.get(t, c)
	require.Equal(t, StateApproved, g.State)
	return g
}

// setColumn writes one non-state column as the owner. The value is SQL rather
// than a parameter because these are all clock expressions; it never comes from
// outside this file.
func setColumn(t *testing.T, id GateID, column, valueSQL string) {
	t.Helper()
	// #nosec G202 -- column and valueSQL are literals in this file.
	_, err := testOwnerDB.Exec(context.Background(),
		`UPDATE capability_gates SET `+column+` = `+valueSQL+` WHERE id = $1`, id)
	require.NoError(t, err)
}

// blankEvidence sets one evidence column to NULL and returns a function
// restoring it, so a subtest cannot leave the row broken for the next one.
func blankEvidence(t *testing.T, id GateID, column string) func() {
	t.Helper()
	return setEvidenceValue(t, id, column, nil)
}

func setEvidence(t *testing.T, id GateID, column, value string) func() {
	t.Helper()
	return setEvidenceValue(t, id, column, &value)
}

func setEvidenceValue(t *testing.T, id GateID, column string, value *string) func() {
	t.Helper()
	ctx := context.Background()
	var was *string
	// #nosec G202 -- column comes from a literal list in this file, never input.
	require.NoError(t, testOwnerDB.QueryRow(ctx,
		`SELECT `+column+` FROM capability_gates WHERE id = $1`, id).Scan(&was))
	// #nosec G202 -- as above.
	_, err := testOwnerDB.Exec(ctx, `UPDATE capability_gates SET `+column+` = $2 WHERE id = $1`, id, value)
	require.NoError(t, err)
	return func() {
		// #nosec G202 -- as above.
		_, err := testOwnerDB.Exec(ctx, `UPDATE capability_gates SET `+column+` = $2 WHERE id = $1`, id, was)
		require.NoError(t, err)
	}
}
