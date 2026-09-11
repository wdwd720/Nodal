//go:build integration

package profile_test

// Adversarial audit (goal §54), area accounts-auth. These tests DEMONSTRATE
// defects; they are expected to fail once the defects are fixed, at which point
// they should be inverted into regressions by the fixer.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/profile"
)

// F-accounts-auth-1. Migration 00758 says the cooling-off period is "enforced by
// the trigger below, not only by the service, because the whole point of a
// cooling-off period is that it survives a bug in code that is in a hurry."
//
// It does not. cp_closure_request_apply_transition compares
// `NEW.occurred_at < waits_until` -- occurred_at is a column the INSERTing role
// chooses -- instead of comparing the transaction's own clock. A caller that
// stamps the row in the future effects the closure immediately, and the wall
// clock is never consulted.
func TestAudit_ClosureCoolingOffIsMeasuredAgainstACallerSuppliedTimestamp(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	actor, _, _ := f.newUser(t)

	view, err := f.svc.RequestClosure(ctx, actor, "")
	require.NoError(t, err)
	require.NotNil(t, view.Closure)
	requestID := view.Closure.ID

	// The wall clock is nowhere near the end of the wait.
	var stillCooling bool
	require.NoError(t, f.db.Pool().QueryRow(ctx,
		`SELECT now() < cooling_off_until FROM account_closure_requests WHERE id = $1`, requestID).
		Scan(&stillCooling))
	require.True(t, stillCooling, "the fixture must still be inside the cooling-off period")

	// One INSERT, stamped whenever the caller likes.
	_, err = f.db.Pool().Exec(ctx,
		`INSERT INTO account_closure_request_transitions
		 (id, request_id, from_state, to_state, actor_type, actor_id, reason, occurred_at)
		 VALUES ($1, $2, 'PENDING', 'EFFECTED', 'OPERATOR', 'forged', 'in a hurry', now() + interval '400 days')`,
		id.New[id.Any](), requestID)
	require.NoError(t, err,
		"the database accepted an EFFECTED transition inside the cooling-off period")

	var state string
	require.NoError(t, f.db.Pool().QueryRow(ctx,
		`SELECT state FROM account_closure_requests WHERE id = $1`, requestID).Scan(&state))
	assert.Equal(t, "EFFECTED", state,
		"the cooling-off period is a service convention, not a database invariant")
}

// F-accounts-auth-2. internal/profile declares the closure edge set
// (closureTransitions: PENDING is the only origin; CANCELLED, REFUSED and
// EFFECTED are terminal) and CanCloseTransition enforces it in Go. The schema
// enforces only that a transition row DESCRIBES the change it makes (00741's
// edge binding), never that the change is legal, so the application role can
// walk a terminal request onto another state -- and decided_at/decided_reason
// are coalesced, so the row keeps the reason given for the decision it no
// longer records.
func TestAudit_TheClosureStateMachineHasNoEdgeSetInTheDatabase(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	actor, _, _ := f.newUser(t)
	op := f.operator(t)

	view, err := f.svc.RequestClosure(ctx, actor, "")
	require.NoError(t, err)
	require.NotNil(t, view.Closure)
	requestID := view.Closure.ID

	f.clk.Advance(testCoolingOff + time.Minute)
	admin, err := f.svc.Decide(ctx, op, actor.UserID, profile.DecisionEffect, "the cooling-off period has passed")
	require.NoError(t, err)
	require.NotNil(t, admin.Closure)
	require.Equal(t, profile.ClosureEffected, admin.Closure.State)
	require.True(t, admin.Closure.State.Terminal(), "EFFECTED is terminal in Go")
	require.False(t, profile.CanCloseTransition(profile.ClosureEffected, profile.ClosureCancelled),
		"Go refuses EFFECTED -> CANCELLED")

	_, err = f.db.Pool().Exec(ctx,
		`INSERT INTO account_closure_request_transitions
		 (id, request_id, from_state, to_state, actor_type, actor_id, reason, occurred_at)
		 VALUES ($1, $2, 'EFFECTED', 'CANCELLED', 'USER', 'forged', 'undo a terminal decision', now())`,
		id.New[id.Any](), requestID)
	require.NoError(t, err, "the database accepted an edge the state machine forbids")

	var state, decidedReason string
	require.NoError(t, f.db.Pool().QueryRow(ctx,
		`SELECT state, coalesce(decided_reason,'') FROM account_closure_requests WHERE id = $1`, requestID).
		Scan(&state, &decidedReason))
	assert.Equal(t, "CANCELLED", state, "a terminal request moved")
	assert.Equal(t, "the cooling-off period has passed", decidedReason,
		"and the row now says CANCELLED for the reason the EFFECT was given")
}

// F-accounts-auth-3. Migration 00757: "CLOSED is terminal: there is no edge out
// of it, here or in Go. ... Nothing in the product needs it today, so the schema
// does not quietly permit it." The schema permits it: cp_user_apply_status_
// transition writes whatever to_status the row names, and the edge binding only
// requires the row to describe the change, not to be a legal one.
func TestAudit_AClosedUserCanBeReopenedByTheApplicationRole(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	actor, user, _ := f.newUser(t)
	op := f.operator(t)

	_, err := f.svc.RequestClosure(ctx, actor, "")
	require.NoError(t, err)
	f.clk.Advance(testCoolingOff + time.Minute)
	admin, err := f.svc.Decide(ctx, op, actor.UserID, profile.DecisionEffect, "the cooling-off period has passed")
	require.NoError(t, err)
	require.Equal(t, "CLOSED", admin.UserStatus)

	_, err = f.db.Pool().Exec(ctx,
		`INSERT INTO user_status_transitions
		 (id, user_id, from_status, to_status, actor_type, actor_id, reason, occurred_at)
		 VALUES ($1, $2, 'CLOSED', 'ACTIVE', 'OPERATOR', 'forged', 'reopen a closed account', now())`,
		id.New[id.Any](), user.ID)
	require.NoError(t, err, "the database accepted CLOSED -> ACTIVE")

	var status string
	require.NoError(t, f.db.Pool().QueryRow(ctx,
		`SELECT status FROM users WHERE id = $1`, user.ID).Scan(&status))
	assert.Equal(t, "ACTIVE", status, "a terminal user status moved")
}

// F-accounts-auth-8 (service half). Decide(EFFECT) closes the account without
// consulting anything financial. The nearest thing to a check is the
// cooling-off period; the three conditions 00758 and closure.go name as the
// reason REFUSED exists -- an unsettled payout, an open dispute, a balance to
// deal with first -- are enforced nowhere and reported nowhere.
func TestAudit_EffectingAClosureChecksNothingFinancial(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	actor, user, acct := f.newUser(t)
	op := f.operator(t)

	_, err := f.svc.RequestClosure(ctx, actor, "")
	require.NoError(t, err)

	// What the operator is shown before deciding.
	before, err := f.svc.AdminUser(ctx, actor.UserID)
	require.NoError(t, err)
	require.NotNil(t, before.Closure)
	// There is no field on it that could carry a balance, an open payout or an
	// open position: the decision is made without them.
	assert.Len(t, before.Accounts, 1)

	f.clk.Advance(testCoolingOff + time.Minute)
	admin, err := f.svc.Decide(ctx, op, actor.UserID, profile.DecisionEffect, "the cooling-off period has passed")
	require.NoError(t, err, "nothing refused the closure")
	assert.Equal(t, "CLOSED", admin.UserStatus)

	var userStatus, acctStatus string
	require.NoError(t, f.db.Pool().QueryRow(ctx, `SELECT status FROM users WHERE id = $1`, user.ID).Scan(&userStatus))
	require.NoError(t, f.db.Pool().QueryRow(ctx, `SELECT status FROM accounts WHERE id = $1`, acct.ID).Scan(&acctStatus))
	assert.Equal(t, "CLOSED", userStatus)
	assert.Equal(t, "CLOSED", acctStatus)
	// identity.Complete refuses a user whose status is not ACTIVE, so whatever
	// this account still holds is now unreachable by the person who owns it.
}

// F-accounts-auth-9. operator_roles is the only source of operator authority in
// the system (ADR-0022, ADR-0024) and cp_app holds blanket UPDATE on it from
// 00010 -- the one authority-bearing table that never got the treatment 00744
// gave accounts, 00757 gave users and 00758 gave account_closure_requests.
// Nothing in Go ever updates it, so the grant serves nothing, and with it a
// revocation does not stay revoked and a role can be rewritten in place while
// granted_by, granted_at and reason keep describing the grant that was made.
func TestAudit_TheOperatorDirectoryIsRewritableByTheApplicationRole(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, user, _ := f.newUser(t)

	_, err := f.db.Pool().Exec(ctx,
		`INSERT INTO operator_roles (user_id, role, reason) VALUES ($1, 'SUPPORT_READ_ONLY', 'support desk')`, user.ID)
	require.NoError(t, err)

	// An operator revokes it.
	_, err = f.db.Pool().Exec(ctx,
		`UPDATE operator_roles SET revoked_at = now() WHERE user_id = $1`, user.ID)
	require.NoError(t, err)

	// And the application role puts it back, and promotes it, in one statement.
	_, err = f.db.Pool().Exec(ctx,
		`UPDATE operator_roles SET revoked_at = NULL, role = 'ADMIN', expires_at = NULL WHERE user_id = $1`, user.ID)
	require.NoError(t, err, "ADR-0024 says a revoked grant stays revoked")

	var role, reason string
	var revoked *time.Time
	var grantedBy *string
	require.NoError(t, f.db.Pool().QueryRow(ctx,
		`SELECT role, coalesce(reason,''), revoked_at, granted_by::text FROM operator_roles WHERE user_id = $1`, user.ID).
		Scan(&role, &reason, &revoked, &grantedBy))
	assert.Equal(t, "ADMIN", role, "a SUPPORT_READ_ONLY row became ADMIN in place")
	assert.Nil(t, revoked, "a revocation was undone")
	assert.Equal(t, "support desk", reason,
		"and the provenance columns still describe the grant that was actually made")

	// There is no transition table for this, so nothing records that it happened.
	var transitionTables int
	require.NoError(t, f.db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables
		  WHERE table_schema = 'public' AND table_name LIKE 'operator_role%transitions'`).Scan(&transitionTables))
	assert.Zero(t, transitionTables, "operator_roles has no transition table")
}
