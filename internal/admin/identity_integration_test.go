//go:build integration

package admin

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/security"
)

// An approval authorises what it was proposed to authorise (F-64).
//
// Three columns decide what dual control means, and nothing protected any of
// them: `requires_dual` (whether a second signature is needed), `kind` (what is
// authorised) and `target_id` (what it is authorised against). All three sat on
// a table `cp_app` held table-wide UPDATE on, with no CHECK, no trigger, and no
// coverage by `params_hash` -- which hashes `params` alone.
//
// Two consequences, both reachable by anything holding the application's
// database credential:
//
//   - `VerifyApproved` read `requires_dual` from the ROW while `Approve` and
//     `executable` read it from the code's `KindSpec`. It was the only consumer
//     that trusted the column, and it is the gate `killswitch`, `agent` and
//     `reconciliation` call before acting.
//   - An approval could be repointed after both signatures. Two principals sign
//     for kill switch A; one UPDATE makes the same approval verify for B.
//
// Migration 00723 freezes the identity columns and narrows the grant;
// `VerifyApproved` now derives dual control from the spec. This drives both
// halves, because either alone would leave the other standing.

func openOwner(t *testing.T) *db.DB {
	t.Helper()
	url := os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	if url == "" {
		t.Skip("CP_TEST_MIGRATE_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d, err := db.Open(ctx, db.Config{URL: url, AppName: "admin-itest-owner", MaxConns: 2})
	require.NoError(t, err)
	t.Cleanup(d.Close)
	return d
}

// approvedAction drives a dual-control action to APPROVED through the real
// propose/approve path and returns it.
func (f *fixture) approvedAction(t *testing.T) Action {
	t.Helper()
	proposer, approver := f.newUser(), f.newUser()
	a, err := f.propose(f.operator(proposer, security.RoleOperations), Proposal{
		Kind: KindKillSwitchRelease, TargetType: "kill_switch",
		TargetID: "GLOBAL_NEW_RISK_KILL:*", Reason: "an incident that ended",
	})
	require.NoError(t, err)
	a, err = f.approve(f.breakGlass(approver), a.ID.String(), "reviewed")
	require.NoError(t, err)
	require.Equal(t, StatusApproved, a.Status)
	return a
}

func TestIntegration_AnApprovalCannotBeRepointedAtAnotherTarget(t *testing.T) {
	f := newFixture(t)
	owner := openOwner(t)
	ctx := context.Background()
	a := f.approvedAction(t)

	// It verifies for what it was approved for.
	_, err := f.svc.VerifyApproved(ctx, testDB, a.ID.String(), KindKillSwitchRelease, "GLOBAL_NEW_RISK_KILL:*")
	require.NoError(t, err, "the approval must verify for its own target, or every refusal below is meaningless")

	// Repointing it is refused, as the owner -- so this is the trigger and not
	// the narrowed grant, which is checked separately.
	for _, set := range []string{
		`target_id = 'ACCOUNT_FREEZE:some-other-account'`,
		`kind = 'LEDGER_CORRECTION'`,
		`target_type = 'ledger_account'`,
		`expires_at = expires_at + interval '30 days'`,
		`proposed_by_user_id = approved_by_user_id`,
	} {
		err := owner.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
			func(ctx context.Context, tx pgx.Tx) error {
				_, e := tx.Exec(ctx, `UPDATE admin_actions SET `+set+` WHERE id = $1`, a.ID)
				return e
			})
		require.Errorf(t, err, "an approved action was changed: %s", set)
		assert.Equal(t, "AD001", db.SQLState(err), "got %v", err)
		assert.Contains(t, err.Error(), "ADMIN_ACTION_IMMUTABLE")
	}

	// And the application role cannot reach those columns at all.
	for _, set := range []string{
		`target_id = 'somewhere-else'`,
		`kind = 'LEDGER_CORRECTION'`,
		`requires_dual = false`,
		`params_hash = '\x00'::bytea`,
	} {
		err := testDB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
			func(ctx context.Context, tx pgx.Tx) error {
				_, e := tx.Exec(ctx, `UPDATE admin_actions SET `+set+` WHERE id = $1`, a.ID)
				return e
			})
		require.Errorf(t, err, "cp_app changed %s", set)
		assert.Equal(t, db.SQLStateInsufficientPrivilege, db.SQLState(err), "got %v", err)
	}

	// Nothing moved.
	_, err = f.svc.VerifyApproved(ctx, testDB, a.ID.String(), KindKillSwitchRelease, "GLOBAL_NEW_RISK_KILL:*")
	require.NoError(t, err)
	_, err = f.svc.VerifyApproved(ctx, testDB, a.ID.String(), KindKillSwitchRelease, "ACCOUNT_FREEZE:some-other-account")
	require.Error(t, err, "an approval verified for a target nobody approved")
}

// TestVerifyApprovedReadsTheSpecNotTheRow: the Go half of F-64, asserted
// against this package's own source.
//
// `VerifyApproved` read `a.RequiresDual` from the row while `Approve` and
// `executable` read `spec.RequiresDual` from the code table. It is the only
// consumer that trusted the column, and it is the gate `killswitch`, `agent` and
// `reconciliation` call before acting.
//
// The behavioural version of this test is not written, deliberately. Reaching
// the branch means an APPROVED dual-control row with no approver, which needs a
// hand-written `admin_action_transitions` row to satisfy AU001 -- and that is
// exactly the forgery F-42 records, performed by a test. The package's own
// `TestIntegration_AdminStreamVerifiesAfterEverything` caught the first attempt
// at it: one transition, no audit event, invariant broken. A fixture that has to
// commit the exploit to reach the code is a fixture that should not exist.
//
// So the schema half is driven for real by
// TestIntegration_AnApprovalCannotBeRepointedAtAnotherTarget, and this asserts
// the source property that made the divergence possible.
func TestVerifyApprovedReadsTheSpecNotTheRow(t *testing.T) {
	body, err := os.ReadFile("verify.go")
	require.NoError(t, err)
	src := string(body)

	require.Contains(t, src, "spec, ok := Spec(a.Kind)",
		"VerifyApproved no longer derives the kind spec; F-64 has come back")
	require.Contains(t, src, "spec.RequiresDual &&",
		"VerifyApproved no longer gates dual control on the spec")
	assert.NotContains(t, src, "a.RequiresDual &&",
		"VerifyApproved is gating on the stored requires_dual column again (F-64). "+
			"Approve and executable both use the KindSpec; a row is not authority for whether a row needs two signatures.")

	// The positive control: the column is still read for reporting, so a check
	// that simply banned the identifier would pass on a file that had dropped
	// the field entirely.
	assert.Contains(t, src, "RequiresDual: a.RequiresDual",
		"the Approval no longer reports what the row recorded; this check is guarding a field that moved")
}

// TestIntegration_TheLifecycleStillMoves is the positive control for the grant.
// A column list one entry short would pass every refusal above and break
// propose/approve/execute -- and the failure would look like actions never
// completing rather than like a permission error anybody was watching for.
func TestIntegration_TheLifecycleStillMoves(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.approvedAction(t)

	done, err := f.execute(f.breakGlass(f.newUser()), a.ID.String(),
		func(context.Context, pgx.Tx, json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"released":true}`), nil
		})
	require.NoError(t, err, "an approved action could not be executed under the narrowed grant")
	require.Equal(t, StatusExecuted, done.Status)

	var updatedMoved bool
	require.NoError(t, testDB.QueryRow(ctx,
		`SELECT updated_at > proposed_at FROM admin_actions WHERE id = $1`, a.ID).Scan(&updatedMoved))
	assert.True(t, updatedMoved, "set_updated_at did not fire; the column grant is too narrow after all")
}
