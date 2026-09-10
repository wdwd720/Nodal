//go:build integration

package migrations_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/id"
)

// 00603's header said "the flag can only be set by inserting an immutable
// transition row." F-42 recorded that this was false and stayed open for four
// sessions while three candidate fixes were tried and rejected. 00741 makes it
// true by tagging the flag with a key the application cannot read, salted with
// the top-level transaction id.
//
// These tests are the two forgeries and the three things that must not have
// broken. Every one was run against the schema BEFORE 00741 and observed doing
// the wrong thing.

func seedAccount(ctx context.Context, t *testing.T, app *pgx.Conn) string {
	t.Helper()
	userID, accountID := id.New[id.Any](), id.New[id.Any]()
	_, err := app.Exec(ctx, `INSERT INTO users (id, idp_issuer, idp_subject, status) VALUES ($1, 'tag-test', $2, 'ACTIVE')`,
		userID, "sub-"+userID.String())
	require.NoError(t, err)
	_, err = app.Exec(ctx, `INSERT INTO accounts (id, owner_user_id, kind, status) VALUES ($1, $2, 'CUSTOMER', 'ACTIVE')`,
		accountID, userID)
	require.NoError(t, err)
	return accountID.String()
}

func settingName(prefix, label, entityID string) string {
	out := make([]byte, 0, len(entityID))
	for i := 0; i < len(entityID); i++ {
		if entityID[i] == '-' {
			out = append(out, '_')
			continue
		}
		out = append(out, entityID[i])
	}
	return prefix + label + ".x" + string(out)
}

// Forgery one, as F-42 records it: set the GUC by hand and change state with no
// transition row behind it. Before 00741 this committed.
func TestIntegration_TheTransitionFlagCannotBeSetByHand(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	require.NoError(t, migrate.Up(ctx, migrateURL))
	app := connect(t, appURL)
	acct := seedAccount(ctx, t, app)

	tx, err := app.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	// Both forms, because the entity is bound by both: the edge form (00731)
	// and the destination-only form (00603).
	_, err = tx.Exec(ctx, `SELECT set_config($1, $2, true)`,
		settingName("cp.edge.", "accounts", acct), "ACTIVE>FROZEN")
	require.NoError(t, err, "setting a GUC is allowed; it is what the flag being a GUC means")
	_, err = tx.Exec(ctx, `SELECT set_config($1, $2, true)`,
		settingName("cp.transition.", "accounts", acct), "FROZEN")
	require.NoError(t, err)

	_, err = tx.Exec(ctx, `UPDATE accounts SET status = 'FROZEN' WHERE id = $1`, acct)
	require.NoError(t, err, "the UPDATE itself is not what refuses; the deferred trigger is")

	err = tx.Commit(ctx)
	require.Error(t, err, "an account froze with no transition row: the flag is still forgeable")
	assert.Contains(t, err.Error(), "AU001")

	var status string
	require.NoError(t, app.QueryRow(ctx, `SELECT status FROM accounts WHERE id = $1`, acct).Scan(&status))
	assert.Equal(t, "ACTIVE", status)
}

// Forgery two, which F-42 does not record and which is the reason a fix that
// only hardened the flag's VALUE would have been bypassed in three lines:
// cp_app holds TEMPORARY on the database, so it could attach the real setter to
// a table of its own and mint a flag for any entity and any edge.
func TestIntegration_TheApplicationCannotMintATransitionFlag(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	require.NoError(t, migrate.Up(ctx, migrateURL))
	app := connect(t, appURL)
	owner := connect(t, migrateURL)

	// The key is the whole basis of the control. Nothing but the owner reads it.
	for _, role := range []string{"cp_app", "cp_readonly", "cp_ops"} {
		var may bool
		require.NoError(t, owner.QueryRow(ctx,
			`SELECT has_table_privilege($1, 'cp_transition_key', 'SELECT')`, role).Scan(&may))
		assert.False(t, may, "%s can read cp_transition_key, so it can compute a tag", role)
	}

	// And nothing but the owner may run the functions that use it. PostgreSQL
	// checks EXECUTE when a trigger is CREATED, not when it fires, so revoking
	// this leaves every existing trigger working -- which the last test here
	// proves rather than assumes.
	for _, fn := range []string{
		"cp_transition_tag(text, text)",
		"cp_flag_transition()",
		"cp_flag_transition_edge()",
		"cp_require_transition()",
		"cp_require_transition_edge()",
		"cp_require_transition_on_insert()",
	} {
		var may bool
		require.NoError(t, owner.QueryRow(ctx,
			`SELECT has_function_privilege('cp_app', $1, 'EXECUTE')`, fn).Scan(&may))
		assert.False(t, may, "cp_app can execute %s, so it can mint a flag from a table of its own", fn)
	}

	// The route itself, end to end.
	acct := seedAccount(ctx, t, app)
	_, err := app.Exec(ctx, `CREATE TEMP TABLE forge (account_id uuid, from_status text, to_status text)`)
	require.NoError(t, err, "cp_app holds TEMPORARY; that is not what this closes")
	_, err = app.Exec(ctx, `CREATE TRIGGER forge_flag AFTER INSERT ON forge FOR EACH ROW
		EXECUTE FUNCTION cp_flag_transition_edge('account_id','from_status','to_status','accounts')`)
	require.Error(t, err, "cp_app attached the real flag setter to a table of its own")
	assert.Contains(t, err.Error(), "permission denied")
	_ = acct
}

// A tag is bound to the transaction that produced it. Without that, a caller
// could transition an entity legitimately, keep the string, and replay it later
// to change state again with no audit row -- which is a forgery, and is why a
// keyed tag on its own is not enough.
func TestIntegration_ATransitionFlagDoesNotOutliveItsTransaction(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	require.NoError(t, migrate.Up(ctx, migrateURL))
	app := connect(t, appURL)
	acct := seedAccount(ctx, t, app)
	setting := settingName("cp.edge.", "accounts", acct)

	// T1: go ACTIVE -> FROZEN legitimately, keeping the flag it produced.
	var captured string
	tx, err := app.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `INSERT INTO account_status_transitions (id, account_id, from_status, to_status, reason, actor_type, actor_id)
		VALUES ($1, $2, 'ACTIVE', 'FROZEN', 'legit', 'SYSTEM', 'p')`, id.New[id.Any](), acct)
	require.NoError(t, err)
	require.NoError(t, tx.QueryRow(ctx, `SELECT current_setting($1, true)`, setting).Scan(&captured))
	_, err = tx.Exec(ctx, `UPDATE accounts SET status = 'FROZEN' WHERE id = $1`, acct)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))
	require.NotEmpty(t, captured, "the setter wrote nothing to capture")

	// T2: back to ACTIVE legitimately, so the same edge is available again.
	tx, err = app.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `INSERT INTO account_status_transitions (id, account_id, from_status, to_status, reason, actor_type, actor_id)
		VALUES ($1, $2, 'FROZEN', 'ACTIVE', 'legit', 'SYSTEM', 'p')`, id.New[id.Any](), acct)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `UPDATE accounts SET status = 'ACTIVE' WHERE id = $1`, acct)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	// T3: replay T1's flag for exactly the edge it described, with no row.
	tx, err = app.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT set_config($1, $2, true)`, setting, captured)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `UPDATE accounts SET status = 'FROZEN' WHERE id = $1`, acct)
	require.NoError(t, err)
	err = tx.Commit(ctx)
	require.Error(t, err, "a flag from an earlier transaction licensed a state change in this one")
	assert.Contains(t, err.Error(), "AU001")
}

// The regression guard, and the reason three earlier fixes were rejected. The
// admin executor runs every action inside a SAVEPOINT, and F-42 records that
// `xmin = pg_current_xact_id()` is false for a row written in one. 00741 uses
// pg_current_xact_id() itself, which is the TOP-LEVEL id and is the same in
// every subtransaction -- so a legitimate transition must still commit whether
// the row and the update share a savepoint, sit in different ones, or use none.
func TestIntegration_ALegitimateTransitionSurvivesSavepoints(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	require.NoError(t, migrate.Up(ctx, migrateURL))
	app := connect(t, appURL)

	for _, tc := range []struct {
		name  string
		split bool
	}{
		{name: "row and update in one savepoint", split: false},
		{name: "row and update in different savepoints", split: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acct := seedAccount(ctx, t, app)
			tx, err := app.Begin(ctx)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback(ctx) }()

			_, err = tx.Exec(ctx, `SAVEPOINT a`)
			require.NoError(t, err)
			_, err = tx.Exec(ctx, `INSERT INTO account_status_transitions (id, account_id, from_status, to_status, reason, actor_type, actor_id)
				VALUES ($1, $2, 'ACTIVE', 'FROZEN', 'savepoint', 'SYSTEM', 'p')`, id.New[id.Any](), acct)
			require.NoError(t, err)
			if tc.split {
				_, err = tx.Exec(ctx, `RELEASE SAVEPOINT a`)
				require.NoError(t, err)
				_, err = tx.Exec(ctx, `SAVEPOINT b`)
				require.NoError(t, err)
			}
			_, err = tx.Exec(ctx, `UPDATE accounts SET status = 'FROZEN' WHERE id = $1`, acct)
			require.NoError(t, err)
			_, err = tx.Exec(ctx, `RELEASE SAVEPOINT `+map[bool]string{true: "b", false: "a"}[tc.split])
			require.NoError(t, err)

			require.NoError(t, tx.Commit(ctx),
				"a legitimate transition was refused; the tag does not survive a subtransaction")

			var status string
			require.NoError(t, app.QueryRow(ctx, `SELECT status FROM accounts WHERE id = $1`, acct).Scan(&status))
			assert.Equal(t, "FROZEN", status)
		})
	}
}
