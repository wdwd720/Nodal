//go:build integration

package migrations_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/id"
)

// AUDIT (platform-hardening) — F-platform-4.
//
// Migration 00761 states the rule it is enforcing: "an application statement
// could move a profile from UNVERIFIED to VERIFIED with no recorded edge, no
// actor and no provider reference, and the trail a regulator would read was
// whatever the last writer said." It fixes that for `identity_state` with a
// transitions table, an edge binding, a SECURITY DEFINER trigger that writes
// the column, and — the part that actually enforces it — `REVOKE UPDATE ON
// compliance_profiles FROM cp_app`.
//
// The very next statement grants `UPDATE (… sanctions_state …)` back.
//
// `sanctions_state` is not an attribute. `internal/eligibility/evaluate.go`
// reads it (`contains(sanctionsStates, in.SanctionsState)`) as one of the
// allowlists that decides whether a payout may proceed, and
// `verification.sanctionsStateFrom` derives it from the provider's sanctions
// and PEP checks. It is a screening DECISION with exactly the same properties
// 00761 gives as the reason `identity_state` had to move: a single UPDATE
// changes it, and nothing in the database records who, when, on what evidence,
// or from which value.
//
// This test drives both columns as cp_app, in one transaction, and shows that
// the one 00761 protected is refused and the one beside it is not.
func TestAudit_SanctionsStateIsWritableWithNoTransitionRow(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	require.NoError(t, migrate.Up(ctx, migrateURL))
	app := connect(t, appURL)

	userID := id.New[id.Any]()
	_, err := app.Exec(ctx,
		`INSERT INTO users (id, idp_issuer, idp_subject, status) VALUES ($1,'audit-platform',$2,'ACTIVE')`,
		userID, "sub-"+userID.String())
	require.NoError(t, err)
	_, err = app.Exec(ctx,
		`INSERT INTO compliance_profiles (user_id, identity_state, sanctions_state, age_verified)
		 VALUES ($1,'UNVERIFIED','HIT',false)`, userID)
	require.NoError(t, err, "a profile is born UNVERIFIED (00761) and this one is born screened HIT")

	// The control: the state 00761 protected cannot be written at all.
	_, err = app.Exec(ctx,
		`UPDATE compliance_profiles SET identity_state = 'VERIFIED' WHERE user_id = $1`, userID)
	require.Error(t, err, "cp_app can still write compliance_profiles.identity_state")
	assert.Contains(t, err.Error(), "permission denied")

	// The finding: the sanctions screen beside it can. One statement, no
	// transition row, no actor, no provider reference, no recorded edge.
	_, err = app.Exec(ctx,
		`UPDATE compliance_profiles SET sanctions_state = 'CLEAR' WHERE user_id = $1`, userID)
	assert.Error(t, err,
		"cp_app cleared a sanctions HIT with a bare UPDATE; the screening decision that gates a "+
			"payout has none of the binding 00761 gave the verification state next to it")

	var state string
	require.NoError(t, app.QueryRow(ctx,
		`SELECT sanctions_state FROM compliance_profiles WHERE user_id = $1`, userID).Scan(&state))
	assert.Equal(t, "HIT", state, "the screening outcome was rewritten")

	var transitions int
	require.NoError(t, app.QueryRow(ctx,
		`SELECT count(*) FROM compliance_profile_transitions WHERE user_id = $1`, userID).Scan(&transitions))
	assert.NotZero(t, transitions,
		"the sanctions state changed and the transitions table records nothing about it")
}
