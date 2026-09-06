//go:build integration

package migrations_test

import (
	"context"
	"regexp"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
//   - cp_readonly and cp_ops can SELECT everything and write nothing.
func TestIntegration_ApplicationRolePrivileges(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	admin := connect(t, migrateURL)
	require.NoError(t, migrate.Up(ctx, migrateURL))

	rows, err := admin.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname = 'public' ORDER BY tablename`)
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
