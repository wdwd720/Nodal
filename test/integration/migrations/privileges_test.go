//go:build integration

package migrations_test

import (
	"context"
	"regexp"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
)

// appendOnlyTables must never be UPDATE- or DELETE-able by the application
// role (PART 19, PART 21, PART 101). Corrections are new rows. The list is
// matched as regular expressions against table names.
var appendOnlyTables = []*regexp.Regexp{
	regexp.MustCompile(`^journal_transactions$`),
	regexp.MustCompile(`^journal_entries$`),
	regexp.MustCompile(`^ledger_balances$`), // maintained only by the SECURITY DEFINER trigger
	regexp.MustCompile(`^audit_events$`),
	regexp.MustCompile(`_transitions$`),
	regexp.MustCompile(`^quotes$`),
	regexp.MustCompile(`^signing_decisions$`),
	regexp.MustCompile(`^lot_dispositions$`),
	regexp.MustCompile(`^asset_policies$`),
	regexp.MustCompile(`^asset_prices$`),
	regexp.MustCompile(`^eligibility_(policies|decisions)$`),
	regexp.MustCompile(`^risk_(policies|decisions)$`),
	regexp.MustCompile(`^capital_envelope_changes$`),
	regexp.MustCompile(`^security_events$`),
	regexp.MustCompile(`^wallet_balance_observations$`),
}

// opsHousekeeping lists the only tables the operations role may DELETE from:
// transient records whose retention is an operational policy, never financial
// history. Everything else is append-only for every role except the migration
// role.
var opsHousekeeping = map[string]bool{
	"outbox_events":    true,
	"inbox_messages":   true,
	"idempotency_keys": true,
	"sessions":         true,
	"login_attempts":   true,
	"notifications":    true,
}

// TestIntegration_ApplicationRolePrivileges asserts the least-privilege
// contract on a fully migrated schema:
//
//   - the migration runner's bookkeeping tables are invisible to cp_app;
//   - cp_app can DELETE from no table at all (cleanup is an ops-role job);
//   - cp_app cannot UPDATE append-only tables;
//   - cp_app can at least SELECT every other table (a table nobody granted is
//     a migration bug, not a feature);
//   - cp_readonly and cp_ops can SELECT everything and write nothing, EXCEPT
//     where a table is named below as an exception with its reason.
//
// That last clause used to read "everything", flatly, and F-47 is the finding
// that it contradicts migration 00010 -- which grants those two roles SELECT on
// a named list that deliberately excludes `identity_pii` and `sessions`. Both
// statements are in the tree; only the blanket ALTER DEFAULT PRIVILEGES has any
// effect, so this test was asserting one side of a contradiction as if it were
// the settled contract.
//
// It is not settled and this test no longer pretends it is. The exception list
// below records which tables are unresolved and why, so that a reader who
// arrives at the grant question is told there IS a question.
func TestIntegration_ApplicationRolePrivileges(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	admin := connect(t, migrateURL)
	require.NoError(t, migrate.Up(ctx, migrateURL))

	// Partitions are excluded. Privileges on a partitioned table are checked on
	// the parent when the parent is what the query names, which is how every
	// reader and writer in this system reaches one -- so a partition carries no
	// grants of its own and is expected to carry none. Sweeping them would
	// demand a GRANT on each, which is both unnecessary and a standing invitation
	// to grant something directly on a partition that the parent does not have
	// (00740).
	rows, err := admin.Query(ctx,
		`SELECT c.relname FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') AND NOT c.relispartition
		  ORDER BY c.relname`)
	require.NoError(t, err)
	var tables []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		tables = append(tables, n)
	}
	rows.Close()
	require.NoError(t, rows.Err())
	require.Greater(t, len(tables), 50, "expected the full schema")
	sort.Strings(tables)

	priv := func(role, table, p string) bool {
		var ok bool
		require.NoError(t, admin.QueryRow(ctx, `SELECT has_table_privilege($1, $2, $3)`, role, "public."+table, p).Scan(&ok))
		return ok
	}
	bookkeeping := map[string]bool{migrate.VersionTable: true, migrate.ChecksumTable: true}

	for _, tbl := range tables {
		if bookkeeping[tbl] {
			assert.False(t, priv("cp_app", tbl, "SELECT"), "%s: runner bookkeeping must be invisible to the app role", tbl)
			continue
		}
		// The one table nobody but the owner may read, asserted rather than
		// skipped. cp_transition_key holds the key the transition flag is
		// tagged with (00741), and a role that can read it can forge a state
		// change on seventeen audited tables. "cp_readonly reads everything" is
		// the rule below and this is the deliberate hole in it, so it is
		// checked in the strong direction here instead of being excused.
		// F-47: two deliberate statements about who may read these disagree, and
		// the disagreement is the finding. The blanket default currently wins.
		//
		// The decision is downstream of a control that does not exist yet:
		// `identity_pii`'s columns are encrypted at the application layer
		// according to SECURITY.md, and that encryption is DESIGNED rather than
		// built -- nothing in this repository writes those columns, so the table
		// is empty in every deployment. Whether SELECT on it is an exposure
		// depends on whether it holds ciphertext, which depends on the
		// unimplemented encryption. `cp_ops` additionally NEEDS SELECT on
		// `sessions` for retention, so the answer is not even the same for both.
		//
		// Asserted here as "currently readable, and that is an open question"
		// rather than "readable, as intended". The moment anything writes
		// identity_pii, TestPII_NothingWritesPersonalDataWhileTheGrantIsUnresolved
		// fails and says what has to be decided first.
		if tbl == "identity_pii" || tbl == "sessions" {
			for _, ro := range []string{"cp_readonly", "cp_ops"} {
				assert.True(t, priv(ro, tbl, "SELECT"),
					"%s: %s lost SELECT. That may be right, but F-47 is the open question and this test is not where it gets decided", tbl, ro)
				for _, p := range []string{"INSERT", "UPDATE", "TRUNCATE"} {
					assert.False(t, priv(ro, tbl, p), "%s: %s must not %s", tbl, ro, p)
				}
			}
			assert.False(t, priv("cp_app", tbl, "DELETE"), "%s: cp_app must never DELETE", tbl)
			assert.True(t, priv("cp_app", tbl, "SELECT"), "%s: cp_app has no SELECT", tbl)
			continue
		}
		if tbl == "cp_transition_key" {
			for _, r := range []string{"cp_app", "cp_readonly", "cp_ops"} {
				for _, p := range []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE"} {
					assert.False(t, priv(r, tbl, p),
						"%s: %s holds %s; a role that can read this key can forge any transition", tbl, r, p)
				}
			}
			continue
		}
		assert.False(t, priv("cp_app", tbl, "DELETE"), "%s: cp_app must never DELETE", tbl)
		assert.False(t, priv("cp_app", tbl, "TRUNCATE"), "%s: cp_app must never TRUNCATE", tbl)
		assert.True(t, priv("cp_app", tbl, "SELECT"), "%s: cp_app has no SELECT; the migration forgot its GRANT", tbl)
		for _, re := range appendOnlyTables {
			if re.MatchString(tbl) {
				assert.False(t, priv("cp_app", tbl, "UPDATE"), "%s: append-only table must not be UPDATE-able by cp_app", tbl)
				break
			}
		}
		for _, ro := range []string{"cp_readonly", "cp_ops"} {
			assert.True(t, priv(ro, tbl, "SELECT"), "%s: %s should read everything", tbl, ro)
			for _, p := range []string{"INSERT", "UPDATE", "TRUNCATE"} {
				assert.False(t, priv(ro, tbl, p), "%s: %s must not %s", tbl, ro, p)
			}
			canDelete := priv(ro, tbl, "DELETE")
			if ro == "cp_ops" && opsHousekeeping[tbl] {
				assert.True(t, canDelete, "%s: cp_ops performs retention cleanup here", tbl)
			} else {
				assert.False(t, canDelete, "%s: %s must not DELETE", tbl, ro)
			}
		}
	}
}

// gateDecisionColumns are the capability_gates columns that gates.Evaluate
// reads to decide whether a live-money capability is active. Migration 00701
// puts every one of them out of cp_app's UPDATE reach, so the five-condition
// rule is a database invariant rather than an application convention.
var gateDecisionColumns = []string{
	"state", "approval_version", "approvers", "proposed_by_user_id", "evidence_hashes",
	"legal_review_ref", "provider_contract_ref", "risk_approval_ref", "security_approval_ref",
	"effective_at", "expires_at", "revoked_at", "revoke_reason",
}

// TestIntegration_CapabilityGateStateAuthority pins the privilege half of
// migration 00701: state moves only through cp_gate_transition, which cp_app
// can call but cannot own, redefine or bypass. A later migration that
// re-granted UPDATE on capability_gates (or INSERT on its history) would put
// back the one-transaction forgery 00701 closed, and fails here.
func TestIntegration_CapabilityGateStateAuthority(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	admin := connect(t, migrateURL)
	require.NoError(t, migrate.Up(ctx, migrateURL))

	colPriv := func(table, col, p string) bool {
		var ok bool
		require.NoError(t, admin.QueryRow(ctx,
			`SELECT has_column_privilege('cp_app', $1, $2, $3)`, "public."+table, col, p).Scan(&ok))
		return ok
	}
	for _, col := range gateDecisionColumns {
		assert.False(t, colPriv("capability_gates", col, "UPDATE"),
			"cp_app must not UPDATE capability_gates.%s: it decides whether live money moves", col)
	}
	// The single exception: `version` is the optimistic-concurrency counter,
	// granted only because PostgreSQL requires UPDATE for SELECT ... FOR UPDATE.
	assert.True(t, colPriv("capability_gates", "version", "UPDATE"),
		"cp_app needs UPDATE (version) to take a row lock on a gate")

	tablePriv := func(table, p string) bool {
		var ok bool
		require.NoError(t, admin.QueryRow(ctx,
			`SELECT has_table_privilege('cp_app', $1, $2)`, "public."+table, p).Scan(&ok))
		return ok
	}
	assert.False(t, tablePriv("capability_gate_transitions", "INSERT"),
		"gate history is written only by cp_gate_transition, in the same statement as the state change")
	assert.True(t, tablePriv("capability_gates", "INSERT"), "gates are bootstrapped DISABLED by the application")
	assert.True(t, tablePriv("capability_gates", "SELECT"), "the checker reads a gate on every live-money call")

	var owner string
	var secDef bool
	require.NoError(t, admin.QueryRow(ctx, `SELECT pg_get_userbyid(p.proowner), p.prosecdef
		FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = 'public' AND p.proname = 'cp_gate_transition'`).Scan(&owner, &secDef))
	assert.Equal(t, "cp_migrate", owner, "the gate authority must be owned by the migration role")
	assert.True(t, secDef, "cp_gate_transition must be SECURITY DEFINER")

	var appMayExecute, publicMayExecute, appIsMigrate bool
	require.NoError(t, admin.QueryRow(ctx, `SELECT
		has_function_privilege('cp_app', p.oid, 'EXECUTE'),
		has_function_privilege('public', p.oid, 'EXECUTE'),
		pg_has_role('cp_app', 'cp_migrate', 'USAGE')
		FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = 'public' AND p.proname = 'cp_gate_transition'`).
		Scan(&appMayExecute, &publicMayExecute, &appIsMigrate))
	assert.True(t, appMayExecute, "the application drives gate transitions through the function")
	assert.False(t, publicMayExecute, "EXECUTE must not be left with PUBLIC")
	assert.False(t, appIsMigrate, "cp_app must not be able to become the function's owner")

	// A gate can only be born DISABLED (PART 244), whatever role inserts it.
	_, err := admin.Exec(ctx, `INSERT INTO capability_gates (id, capability, environment, state, effective_at)
		VALUES (gen_random_uuid(), 'WITHDRAWALS', 'PROD', 'ACTIVE', now())`)
	require.Error(t, err, "even the migration role may not create a gate already ACTIVE")
	assert.Equal(t, "GT005", db.SQLState(err), "got %v", err)
}

// The application role holds column grants where the columns are money (F-109).
//
// Every audited entity binds its STATE change to a transition row. Nothing binds
// the other columns, and the binding cannot: a constraint trigger declared
// AFTER UPDATE OF status fires only when status is in the statement's SET list.
// So a bare rewrite of an amount or a destination fired nothing at all, and one
// smuggled alongside a lawful state move committed with AU001 satisfied and the
// trail recording a move that did happen while the payload changed underneath.
//
// The remedy is privilege rather than detection, which is why it works, and it
// is the treatment 00604, 00701, 00719, 00720, 00723 and 00730 already apply.
// This asserts the four tables 00733 converted, by column, so that a later
// migration widening one back to table-wide UPDATE fails here.
func TestIntegration_MoneyColumnsAreOutOfTheApplicationsReach(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	require.NoError(t, migrate.Up(ctx, migrateURL))
	admin := connect(t, migrateURL)

	granted := func(table string) []string {
		var cols []string
		rows, err := admin.Query(ctx,
			`SELECT column_name FROM information_schema.column_privileges
			  WHERE grantee = 'cp_app' AND privilege_type = 'UPDATE' AND table_name = $1
			  ORDER BY column_name`, table)
		require.NoError(t, err)
		defer rows.Close()
		for rows.Next() {
			var c string
			require.NoError(t, rows.Scan(&c))
			cols = append(cols, c)
		}
		require.NoError(t, rows.Err())
		return cols
	}

	for table, want := range map[string][]string{
		// The amount, the destination and the approval are written once.
		"withdrawals": {"status", "step_up_verified_at"},
		// What an asset IS decides how reconciliation values it: a stablecoin
		// pegged to USD is marked at face value, scaled by its own decimals,
		// with no status or risk-class check. That is the materiality test.
		"assets": {"status"},
		// A live instrument's settlement asset is not repointable.
		"instruments": {"status"},
		// The account, the requested quantity, the destination and both
		// idempotency keys are not the application's to change.
		"payout_requests": {
			"failure_reason", "policy_hash", "policy_version", "provider",
			"provider_idempotency_key", "provider_reference", "provider_status",
			"reserved_at", "reserved_quantity", "settled_at", "settled_quantity",
			"state", "submitted_at", "verification_level",
		},
	} {
		assert.Equal(t, want, granted(table),
			"%s: cp_app's UPDATE grant is not the column set 00733 established; a table-wide "+
				"grant here means an amount or a destination can be rewritten with no transition row", table)
	}

	// The negative control. If the query above stopped matching anything it
	// would report an empty set for every table and pass by comparing nothing.
	assert.NotEmpty(t, granted("payout_requests"), "the privilege query returned nothing; it is not looking at the catalogue")
}
