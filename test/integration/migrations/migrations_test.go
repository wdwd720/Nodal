//go:build integration

// Package migrations_test exercises the migration runner against a real
// PostgreSQL (PART 140, PART 218): clean apply, status, checksum verification
// and tamper detection, idempotent re-apply, guarded rollback, and the role
// separation the migrations grant (PART 101).
package migrations_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/migrations"
)

// Exclusive lock: this suite drops and recreates schema public, so DML-only
// suites (internal/db, internal/idempotency) hold the same id shared.
const testAdvisoryLockID = 424242

var (
	appURL     = os.Getenv("CP_TEST_DATABASE_URL")
	migrateURL = os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
)

func TestMain(m *testing.M) {
	os.Exit(testMain(m))
}

func testMain(m *testing.M) int {
	if appURL == "" || migrateURL == "" {
		return m.Run()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	lockConn, err := pgx.Connect(ctx, migrateURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrations integration: connect for advisory lock:", err)
		return 1
	}
	defer func() { _ = lockConn.Close(ctx) }()
	if _, err := lockConn.Exec(ctx, "SELECT pg_advisory_lock($1)", testAdvisoryLockID); err != nil {
		fmt.Fprintln(os.Stderr, "migrations integration: advisory lock:", err)
		return 1
	}
	defer func() { _, _ = lockConn.Exec(ctx, "SELECT pg_advisory_unlock($1)", testAdvisoryLockID) }()
	return m.Run()
}

func requireEnv(t *testing.T) {
	t.Helper()
	if appURL == "" || migrateURL == "" {
		t.Skip("CP_TEST_DATABASE_URL / CP_TEST_MIGRATE_DATABASE_URL not set; skipping integration test")
	}
}

func connect(t *testing.T, url string) *pgx.Conn {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close(ctx) })
	return c
}

// resetSchema drops and recreates schema public as the migration role, then
// restores the grants and default privileges from docker/postgres/init/001_roles.sql
// (they are attached to the schema OID and vanish with DROP SCHEMA).
func resetSchema(t *testing.T, admin *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{
		"DROP SCHEMA public CASCADE",
		"CREATE SCHEMA public",
		"GRANT USAGE ON SCHEMA public TO cp_app, cp_readonly, cp_ops",
		// No default table privileges for cp_app: each migration grants explicitly (PART 101).
		"ALTER DEFAULT PRIVILEGES FOR ROLE cp_migrate IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO cp_app",
		"ALTER DEFAULT PRIVILEGES FOR ROLE cp_migrate IN SCHEMA public GRANT SELECT ON TABLES TO cp_readonly, cp_ops",
	}
	for _, s := range stmts {
		_, err := admin.Exec(ctx, s)
		require.NoError(t, err, s)
	}
}

func embeddedCount(t *testing.T) int {
	t.Helper()
	entries, err := migrations.FS.ReadDir(".")
	require.NoError(t, err)
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			n++
		}
	}
	return n
}

func tableExists(t *testing.T, c *pgx.Conn, name string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, c.QueryRow(context.Background(), "SELECT to_regclass($1) IS NOT NULL", name).Scan(&exists))
	return exists
}

func TestIntegration_Migrations(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	admin := connect(t, migrateURL)
	total := embeddedCount(t)
	require.GreaterOrEqual(t, total, 3)

	// Whatever happens below, leave the database fully migrated for other suites.
	t.Cleanup(func() { require.NoError(t, migrate.Up(context.Background(), migrateURL)) })

	t.Run("clean schema: everything pending, verify passes vacuously", func(t *testing.T) {
		resetSchema(t, admin)
		rows, err := migrate.Status(ctx, migrateURL)
		require.NoError(t, err)
		require.Len(t, rows, total)
		for _, r := range rows {
			assert.False(t, r.Applied, r.Name)
			assert.False(t, r.Orphaned, r.Name)
		}
		v, err := migrate.Version(ctx, migrateURL)
		require.NoError(t, err)
		assert.Equal(t, int64(0), v)
		require.NoError(t, migrate.Verify(ctx, migrateURL))
	})

	t.Run("up applies all migrations and records checksums atomically", func(t *testing.T) {
		require.NoError(t, migrate.Up(ctx, migrateURL))
		rows, err := migrate.Status(ctx, migrateURL)
		require.NoError(t, err)
		require.Len(t, rows, total)
		for _, r := range rows {
			assert.True(t, r.Applied, r.Name)
			assert.False(t, r.AppliedAt.IsZero(), r.Name)
			assert.Len(t, r.SHA256, 64)
		}
		v, err := migrate.Version(ctx, migrateURL)
		require.NoError(t, err)
		assert.Equal(t, rows[len(rows)-1].Version, v)

		var n int
		require.NoError(t, admin.QueryRow(ctx, "SELECT count(*) FROM "+migrate.ChecksumTable).Scan(&n))
		assert.Equal(t, total, n, "one checksum per applied migration")
		for _, tbl := range []string{"outbox_events", "inbox_messages", "idempotency_keys", migrate.VersionTable, migrate.ChecksumTable} {
			assert.True(t, tableExists(t, admin, tbl), tbl)
		}
		require.NoError(t, migrate.Verify(ctx, migrateURL))
	})

	t.Run("re-running up is a no-op", func(t *testing.T) {
		before, err := migrate.Status(ctx, migrateURL)
		require.NoError(t, err)
		var sumsBefore string
		require.NoError(t, admin.QueryRow(ctx, "SELECT string_agg(version||':'||sha256||':'||applied_at::text, ',' ORDER BY version) FROM "+migrate.ChecksumTable).Scan(&sumsBefore))

		require.NoError(t, migrate.Up(ctx, migrateURL))

		after, err := migrate.Status(ctx, migrateURL)
		require.NoError(t, err)
		assert.Equal(t, before, after)
		var sumsAfter string
		require.NoError(t, admin.QueryRow(ctx, "SELECT string_agg(version||':'||sha256||':'||applied_at::text, ',' ORDER BY version) FROM "+migrate.ChecksumTable).Scan(&sumsAfter))
		assert.Equal(t, sumsBefore, sumsAfter, "checksum rows untouched")
	})

	t.Run("verify detects a tampered checksum and a missing one", func(t *testing.T) {
		var original string
		require.NoError(t, admin.QueryRow(ctx, "SELECT sha256 FROM "+migrate.ChecksumTable+" WHERE version = 2").Scan(&original))

		_, err := admin.Exec(ctx, "UPDATE "+migrate.ChecksumTable+" SET sha256 = repeat('0', 64) WHERE version = 2")
		require.NoError(t, err)
		err = migrate.Verify(ctx, migrateURL)
		require.Error(t, err)
		assert.ErrorIs(t, err, migrate.ErrChecksumMismatch)
		assert.NotErrorIs(t, err, migrate.ErrMissingChecksum)
		assert.Contains(t, err.Error(), "version 2")

		_, err = admin.Exec(ctx, "DELETE FROM "+migrate.ChecksumTable+" WHERE version = 2")
		require.NoError(t, err)
		err = migrate.Verify(ctx, migrateURL)
		require.Error(t, err)
		assert.ErrorIs(t, err, migrate.ErrMissingChecksum)

		// A version applied by hand that the binary does not ship is reported too.
		_, err = admin.Exec(ctx, "INSERT INTO "+migrate.VersionTable+" (version_id, is_applied) VALUES (99999, true)")
		require.NoError(t, err)
		err = migrate.Verify(ctx, migrateURL)
		require.Error(t, err)
		assert.ErrorIs(t, err, migrate.ErrUnknownMigration)
		rows, err := migrate.Status(ctx, migrateURL)
		require.NoError(t, err)
		require.Len(t, rows, total+1)
		assert.True(t, rows[len(rows)-1].Orphaned)
		assert.Equal(t, int64(99999), rows[len(rows)-1].Version)

		// Restore.
		_, err = admin.Exec(ctx, "DELETE FROM "+migrate.VersionTable+" WHERE version_id = 99999")
		require.NoError(t, err)
		_, err = admin.Exec(ctx, "INSERT INTO "+migrate.ChecksumTable+" (version, sha256) VALUES (2, $1)", original)
		require.NoError(t, err)
		require.NoError(t, migrate.Verify(ctx, migrateURL))
	})

	t.Run("down-to guard: allowed below the protected version while none is applied", func(t *testing.T) {
		current, err := migrate.Version(ctx, migrateURL)
		require.NoError(t, err)
		if current >= migrate.ProtectedVersion {
			// Once a ledger migration ships, this branch becomes the live check.
			err := migrate.DownTo(ctx, migrateURL, migrate.ProtectedVersion-1)
			require.ErrorIs(t, err, migrate.ErrProtectedVersion)
			err = migrate.DownTo(ctx, migrateURL, 0)
			require.ErrorIs(t, err, migrate.ErrProtectedVersion)
			return
		}
		// No protected migration applied: rolling back to protected-1 is a no-op.
		require.NoError(t, migrate.DownTo(ctx, migrateURL, migrate.ProtectedVersion-1))
		v, err := migrate.Version(ctx, migrateURL)
		require.NoError(t, err)
		assert.Equal(t, current, v)
		// Refuses negative targets outright.
		require.Error(t, migrate.DownTo(ctx, migrateURL, -1))
	})

	t.Run("down-to rolls back foundation tables and their checksums, up restores", func(t *testing.T) {
		// Rollback below the protected version is only legal while no protected
		// migration is applied, so exercise the foundation Down migrations on a
		// database migrated only up to ProtectedVersion-1. Everything at or above
		// ProtectedVersion is history and is covered by the guard test above.
		resetSchema(t, admin)
		require.NoError(t, migrate.UpTo(ctx, migrateURL, migrate.ProtectedVersion-1))
		v0, err := migrate.Version(ctx, migrateURL)
		require.NoError(t, err)
		require.Less(t, v0, migrate.ProtectedVersion)
		require.NoError(t, migrate.DownTo(ctx, migrateURL, 1))
		v, err := migrate.Version(ctx, migrateURL)
		require.NoError(t, err)
		assert.Equal(t, int64(1), v)
		assert.False(t, tableExists(t, admin, "idempotency_keys"))
		assert.False(t, tableExists(t, admin, "outbox_events"))
		assert.False(t, tableExists(t, admin, "inbox_messages"))
		var n int
		require.NoError(t, admin.QueryRow(ctx, "SELECT count(*) FROM "+migrate.ChecksumTable).Scan(&n))
		assert.Equal(t, 1, n, "checksums for rolled-back versions are removed")
		require.NoError(t, migrate.Verify(ctx, migrateURL))

		require.NoError(t, migrate.DownTo(ctx, migrateURL, 0))
		v, err = migrate.Version(ctx, migrateURL)
		require.NoError(t, err)
		assert.Equal(t, int64(0), v)
		require.NoError(t, admin.QueryRow(ctx, "SELECT count(*) FROM pg_proc WHERE proname IN ('set_updated_at','forbid_mutation')").Scan(&n))
		assert.Equal(t, 0, n, "00001 down drops the helper functions")
		assert.True(t, tableExists(t, admin, migrate.ChecksumTable), "runner bookkeeping survives a full rollback")

		require.NoError(t, migrate.UpTo(ctx, migrateURL, 2))
		v, err = migrate.Version(ctx, migrateURL)
		require.NoError(t, err)
		assert.Equal(t, int64(2), v)
		assert.True(t, tableExists(t, admin, "outbox_events"))
		assert.False(t, tableExists(t, admin, "idempotency_keys"))

		require.NoError(t, migrate.Up(ctx, migrateURL))
		require.NoError(t, migrate.Verify(ctx, migrateURL))
		rows, err := migrate.Status(ctx, migrateURL)
		require.NoError(t, err)
		require.Len(t, rows, total)
		for _, r := range rows {
			assert.True(t, r.Applied, r.Name)
		}
	})

	t.Run("role separation: cp_app has DML without DELETE and no DDL", func(t *testing.T) {
		app := connect(t, appURL)
		for _, tbl := range []string{"outbox_events", "inbox_messages", "idempotency_keys"} {
			var sel, ins, upd, del bool
			require.NoError(t, app.QueryRow(ctx, `SELECT has_table_privilege('cp_app', $1, 'SELECT'), has_table_privilege('cp_app', $1, 'INSERT'),
				has_table_privilege('cp_app', $1, 'UPDATE'), has_table_privilege('cp_app', $1, 'DELETE')`, tbl).Scan(&sel, &ins, &upd, &del))
			assert.True(t, sel && ins && upd, "%s: SELECT/INSERT/UPDATE", tbl)
			assert.False(t, del, "%s: DELETE must be revoked from cp_app", tbl)

			_, err := app.Exec(ctx, "DELETE FROM "+tbl+" WHERE false")
			require.Error(t, err, tbl)
			assert.True(t, db.IsInsufficientPrivilege(err), "%s: SQLSTATE %q", tbl, db.SQLState(err))

			var ro, ops bool
			require.NoError(t, app.QueryRow(ctx, `SELECT has_table_privilege('cp_readonly', $1, 'SELECT'), has_table_privilege('cp_ops', $1, 'DELETE')`, tbl).Scan(&ro, &ops))
			assert.True(t, ro, "%s: cp_readonly SELECT", tbl)
			assert.True(t, ops, "%s: cp_ops DELETE (cleanup job)", tbl)
		}

		_, err := app.Exec(ctx, "CREATE TABLE should_not_exist (id int)")
		require.Error(t, err)
		assert.True(t, db.IsInsufficientPrivilege(err), "cp_app must not modify the schema (PART 101); SQLSTATE %q", db.SQLState(err))
		_, err = app.Exec(ctx, "DROP TABLE idempotency_keys")
		require.Error(t, err)
		assert.True(t, db.IsInsufficientPrivilege(err), "SQLSTATE %q", db.SQLState(err))
		// Privilege checks fire at plan time, so WHERE false probes without mutating.
		for _, tbl := range []string{migrate.VersionTable, migrate.ChecksumTable} {
			for _, stmt := range []string{
				"UPDATE " + tbl + " SET version_id = 0 WHERE false",
				"DELETE FROM " + tbl + " WHERE false",
				"INSERT INTO " + tbl + " SELECT * FROM " + tbl + " WHERE false",
			} {
				if tbl == migrate.ChecksumTable && strings.HasPrefix(stmt, "UPDATE") {
					stmt = "UPDATE " + tbl + " SET sha256 = repeat('0', 64) WHERE false"
				}
				_, err = app.Exec(ctx, stmt)
				require.Error(t, err, "cp_app must not rewrite migration bookkeeping: %s", stmt)
				assert.True(t, db.IsInsufficientPrivilege(err), "%s: SQLSTATE %q", stmt, db.SQLState(err))
			}
			var ro bool
			require.NoError(t, app.QueryRow(ctx, "SELECT has_table_privilege('cp_readonly', $1, 'SELECT')", tbl).Scan(&ro))
			assert.True(t, ro, "%s: cp_readonly can still audit bookkeeping", tbl)
		}

		// The DML that is granted actually works through the app pool.
		pool, err := db.Open(ctx, db.Config{URL: appURL, AppName: "migrations-itest", MaxConns: 2})
		require.NoError(t, err)
		defer pool.Close()
		require.NoError(t, pool.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO inbox_messages (source, message_id, schema_version, status) VALUES ('itest', 'm1', 1, 'RECEIVED')
				ON CONFLICT (source, message_id) DO UPDATE SET status = 'PROCESSED', processed_at = now()`)
			return err
		}))
		_, err = pool.Exec(ctx, `INSERT INTO inbox_messages (source, message_id, schema_version, status) VALUES ('itest', 'm2', 1, 'BOGUS')`)
		require.Error(t, err)
		assert.True(t, db.IsCheckViolation(err), "status CHECK; SQLSTATE %q", db.SQLState(err))
	})

	t.Run("outbox dedup index and helper triggers behave", func(t *testing.T) {
		insert := `INSERT INTO outbox_events (id, topic, partition_key, event_type, schema_version, source, occurred_at, dedup_key, payload)
			VALUES (gen_random_uuid(), 'intents', 'acct-1', 'IntentCreated', 1, 'itest', now(), $1, '{}'::jsonb)`
		_, err := admin.Exec(ctx, insert, "dk-1")
		require.NoError(t, err)
		_, err = admin.Exec(ctx, insert, "dk-1")
		require.Error(t, err)
		assert.True(t, db.IsUniqueViolation(err), "SQLSTATE %q", db.SQLState(err))
		assert.Equal(t, "outbox_events_topic_dedup_key_uidx", db.ConstraintName(err))
		_, err = admin.Exec(ctx, insert, nil)
		require.NoError(t, err)
		_, err = admin.Exec(ctx, insert, nil)
		require.NoError(t, err, "NULL dedup keys never collide")

		_, err = admin.Exec(ctx, `CREATE TEMP TABLE posted (id int PRIMARY KEY, updated_at timestamptz NOT NULL DEFAULT '2000-01-01');
			CREATE TRIGGER posted_forbid BEFORE UPDATE OR DELETE ON posted FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
			INSERT INTO posted (id) VALUES (1)`)
		require.NoError(t, err)
		_, err = admin.Exec(ctx, "UPDATE posted SET id = 2 WHERE id = 1")
		require.Error(t, err)
		assert.True(t, db.IsImmutableRow(err), "%v", err)
		_, err = admin.Exec(ctx, "DELETE FROM posted WHERE id = 1")
		require.Error(t, err)
		assert.True(t, db.IsImmutableRow(err), "%v", err)

		_, err = admin.Exec(ctx, `CREATE TEMP TABLE mutable (id int PRIMARY KEY, v int, updated_at timestamptz NOT NULL DEFAULT '2000-01-01');
			CREATE TRIGGER mutable_touch BEFORE UPDATE ON mutable FOR EACH ROW EXECUTE FUNCTION set_updated_at();
			INSERT INTO mutable (id, v) VALUES (1, 1)`)
		require.NoError(t, err)
		var updatedAt time.Time
		require.NoError(t, admin.QueryRow(ctx, "UPDATE mutable SET v = 2 WHERE id = 1 RETURNING updated_at").Scan(&updatedAt))
		assert.WithinDuration(t, time.Now(), updatedAt, time.Minute, "set_updated_at stamped now()")

		_, err = admin.Exec(ctx, "DELETE FROM outbox_events WHERE source = 'itest'")
		require.NoError(t, err)
	})
}

// Every globally-unique idempotency key is claimed by something that checks
// whose it is (F-106).
//
// A key that is UNIQUE across the whole table, rather than per account, means a
// caller who guesses or reuses another account's key collides with their row.
// The HTTP boundary's own idempotency record is keyed by (actor, endpoint,
// key), so that collision is not caught there -- it passes the boundary and
// arrives in the domain, which then decides whether to hand the row over.
//
// Four packages compared the account and refused with INVALID_IDEMPOTENCY_REUSE.
// Three returned the row: a payout, a marketplace order and a market fill, each
// rendered to a stranger along with the caller's own request being discarded.
//
// This is the list, and it exists because the defect was silent: nothing said
// which tables were in this shape. A new table with a global key fails here
// until somebody writes down how its owner is checked.
func TestIntegration_EveryGlobalIdempotencyKeyIsScopedByItsOwner(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	require.NoError(t, migrate.Up(ctx, migrateURL))
	app := connect(t, appURL)

	// table -> where the account comparison lives.
	declared := map[string]string{
		"asset_reservations":       "internal/capital.replayReservation compares AccountID, AssetID, Quantity, USD and envelope",
		"credit_fundings":          "internal/credit.OpenFunding compares AccountID and CreditQuantity",
		"deposits":                 "internal/funding: Repository.Create and Service.Start both compare AccountID",
		"internal_commerce_orders": "internal/commerce.Purchase compares BuyerAccountID (F-106)",
		"native_market_fills":      "internal/nativemarket.Execute compares the fill's account_id (F-106)",
		"payout_requests":          "internal/payout.Create compares AccountID (F-106)",
		"withdrawals":              "internal/withdrawal.Create compares AccountID",
		// Not account-scoped by design. The ledger's key is the caller's own
		// namespaced string and a collision is refused by content hash, not by
		// owner; a plan step's key is derived by the planner, never by a client.
		"journal_transactions": "internal/ledger compares the content hash; the key is server-derived",
		"execution_plan_steps": "server-derived by the planner; no client supplies it",
	}

	rows, err := app.Query(ctx, `
		SELECT c.relname, array_to_string(array_agg(a.attname ORDER BY k.ord), ',')
		  FROM pg_index x
		  JOIN pg_class c ON c.oid = x.indrelid
		  JOIN LATERAL unnest(x.indkey) WITH ORDINALITY AS k(attnum, ord) ON true
		  JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = k.attnum
		 WHERE x.indisunique AND c.relnamespace = 'public'::regnamespace
		 GROUP BY c.relname, x.indexrelid
		HAVING array_to_string(array_agg(a.attname ORDER BY k.ord), ',') LIKE '%idempotency_key%'
		 ORDER BY 1`)
	require.NoError(t, err)
	defer rows.Close()

	var undeclared, scoped []string
	for rows.Next() {
		var table, cols string
		require.NoError(t, rows.Scan(&table, &cols))
		if strings.Contains(cols, "account_id") {
			// Scoped in the schema itself, which is the strongest form: the
			// collision cannot happen at all. trade_intents does this.
			scoped = append(scoped, table)
			continue
		}
		if _, ok := declared[table]; !ok {
			undeclared = append(undeclared, table+" ("+cols+")")
		}
	}
	require.NoError(t, rows.Err())

	assert.Empty(t, undeclared,
		"these tables have a globally unique idempotency key and nothing here says how its owner is "+
			"checked, so a caller reusing another account's key may be handed their row:\n  %s",
		strings.Join(undeclared, "\n  "))
	assert.NotEmpty(t, scoped,
		"no table scopes its idempotency key by account in the schema; trade_intents did, and losing "+
			"that would mean the strongest form of this control is gone")
}
