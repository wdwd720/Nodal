//go:build integration

package migrations_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db/migrate"
)

// security_events became RANGE-partitioned by month in 00740, so that F-105's
// remaining half -- a security trail nothing could prune, on a 500 MB ceiling
// that halts every financial action -- has a remedy that does not weaken the
// thing the table is for.
//
// These tests exist because the remedy has two halves that pull in opposite
// directions. Partitioning must make months droppable; immutability must stay
// exactly as strong as it was. A change that achieved the first and quietly
// gave up the second would look like a success from inside the schema.

// The property that must NOT have changed. Rows are immutable to every role
// including the table owner, which is stronger than a privilege and is the
// reason ADR-0020 forbids a DELETE-based retention scheme outright.
func TestIntegration_SecurityEventsIsPartitionedAndStillImmutable(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	require.NoError(t, migrate.Up(ctx, migrateURL))
	owner := connect(t, migrateURL)
	app := connect(t, appURL)

	var relkind string
	require.NoError(t, owner.QueryRow(ctx,
		`SELECT relkind FROM pg_class WHERE relname = 'security_events'`).Scan(&relkind))
	require.Equal(t, "p", relkind, "security_events is not partitioned; 00740 did not take")

	var partitions int
	require.NoError(t, owner.QueryRow(ctx,
		`SELECT count(*) FROM pg_inherits WHERE inhparent = 'public.security_events'::regclass`).Scan(&partitions))
	assert.Greater(t, partitions, 1, "a partitioned table with one partition has runway for one month")

	var hasDefault bool
	require.NoError(t, owner.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = 'security_events_default')`).Scan(&hasDefault))
	assert.True(t, hasDefault,
		"without a default partition, a skewed clock makes an INSERT of a SECURITY EVENT fail; that write is the worst one to drop")

	// The application writes them and never removes one.
	_, err := app.Exec(ctx,
		`INSERT INTO security_events (id, kind, severity, detail) VALUES (gen_random_uuid(), 'probe', 'INFO', '{}')`)
	require.NoError(t, err, "cp_app must still be able to record a security event")

	_, err = app.Exec(ctx, `DELETE FROM security_events WHERE kind = 'probe'`)
	require.Error(t, err, "cp_app can delete a security event")
	_, err = app.Exec(ctx, `UPDATE security_events SET severity = 'INFO' WHERE kind = 'probe'`)
	require.Error(t, err, "cp_app can rewrite a security event")

	// The one that matters: the OWNER is refused too. This is the trigger, not
	// a grant, and it is what makes the trail evidence rather than a log.
	_, err = owner.Exec(ctx, `DELETE FROM security_events WHERE kind = 'probe'`)
	require.Error(t, err, "the table owner can delete a security event; the immutability trigger did not survive partitioning")
	assert.Contains(t, err.Error(), "immutable row",
		"the refusal came from something other than forbid_mutation, which means the guard being relied on is not the one that fired")

	_, err = owner.Exec(ctx, `UPDATE security_events SET severity = 'INFO' WHERE kind = 'probe'`)
	require.Error(t, err, "the table owner can rewrite a security event")
}

// A row trigger on a partitioned parent is cloned to partitions created later.
// That is a PostgreSQL behaviour this design leans on entirely: without it, the
// first partition the retention pass creates would be a mutable hole in an
// otherwise immutable table, and nothing would say so.
func TestIntegration_APartitionMadeLaterIsAsImmutableAsOneMadeNow(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	require.NoError(t, migrate.Up(ctx, migrateURL))
	owner := connect(t, migrateURL)
	tag := fmt.Sprintf("%d", time.Now().UnixNano())

	_, err := owner.Exec(ctx, `SELECT cp_security_events_ensure_partitions(36)`)
	require.NoError(t, err)

	// Three years out is well past anything 00740 created up front.
	future := time.Now().UTC().AddDate(0, 30, 0)
	name := fmt.Sprintf("security_events_%04d_%02d", future.Year(), int(future.Month()))
	var exists bool
	require.NoError(t, owner.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = $1)`, name).Scan(&exists))
	require.True(t, exists, "ensure_partitions did not create %s", name)

	stamp := time.Date(future.Year(), future.Month(), 15, 0, 0, 0, 0, time.UTC)
	_, err = owner.Exec(ctx,
		`INSERT INTO security_events (id, kind, severity, detail, occurred_at)
		 VALUES (gen_random_uuid(), 'future_probe_' || $2, 'INFO', '{}', $1)`, stamp, tag)
	require.NoError(t, err)

	// regclass::text omits the schema when public is on the search path, so the
	// name is compared bare. Getting this wrong in the other direction is how an
	// assertion becomes vacuous: NotEqual against a prefixed name would have
	// passed no matter which partition the row reached.
	var landed string
	require.NoError(t, owner.QueryRow(ctx,
		`SELECT tableoid::regclass::text FROM security_events WHERE kind = 'future_probe_' || $1`, tag).Scan(&landed))
	assert.Equal(t, name, landed,
		"the row did not land in the partition made for it; if it went to the default, retention can never drop it")

	_, err = owner.Exec(ctx, `DELETE FROM security_events WHERE kind = 'future_probe_' || $1`, tag)
	require.Error(t, err, "a partition created by the retention function is mutable; the trigger was not cloned")
	assert.Contains(t, err.Error(), "immutable row")
}

// Retention drops whole months and only whole months, and it refuses to be
// pointed at recent history. The floor exists because the caller of this
// function is a web service holding an operations credential -- exactly the
// principal an attacker would want -- and a retention control that accepts
// "one day" is an erase-the-evidence control with a retention control's name.
func TestIntegration_SecurityEventRetentionIsBoundedAndDropsWholeMonths(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	require.NoError(t, migrate.Up(ctx, migrateURL))
	owner := connect(t, migrateURL)

	// Every marker is unique to this run. The table is append-only and the
	// partitions this test creates outlive it, so counting rows by a fixed
	// literal would pass the first time and fail the second -- which is a test
	// that reports on the database's history rather than on this run.
	tag := fmt.Sprintf("%d", time.Now().UnixNano())

	for _, days := range []int{-1, 0, 1, 30, 89} {
		_, err := owner.Exec(ctx, `SELECT cp_security_events_drop_expired($1)`, days)
		require.Error(t, err, "a %d day retention window was accepted on a security audit trail", days)
		assert.Contains(t, err.Error(), "at least 90 days")
	}

	// Two months old enough to go, and one row that is not in any of them.
	old1 := time.Now().UTC().AddDate(0, -14, 0)
	old2 := time.Now().UTC().AddDate(0, -13, 0)
	for _, m := range []time.Time{old1, old2} {
		start := time.Date(m.Year(), m.Month(), 1, 0, 0, 0, 0, time.UTC)
		name := fmt.Sprintf("security_events_%04d_%02d", start.Year(), int(start.Month()))
		_, err := owner.Exec(ctx, fmt.Sprintf(
			`CREATE TABLE %s PARTITION OF security_events FOR VALUES FROM ('%s') TO ('%s')`,
			name, start.Format(time.DateOnly), start.AddDate(0, 1, 0).Format(time.DateOnly),
		))
		require.NoError(t, err)
		_, err = owner.Exec(ctx,
			`INSERT INTO security_events (id, kind, severity, detail, occurred_at)
			 VALUES (gen_random_uuid(), 'aged_' || $2, 'INFO', '{}', $1)`, start.AddDate(0, 0, 14), tag)
		require.NoError(t, err)
	}
	// A row with a wildly out-of-range timestamp, which is what the default
	// partition is for. Its age is unknown by construction, so retention must
	// not judge it.
	_, err := owner.Exec(ctx,
		`INSERT INTO security_events (id, kind, severity, detail, occurred_at)
		 VALUES (gen_random_uuid(), 'skewed_' || $1, 'INFO', '{}', '2400-01-01T00:00:00Z')`, tag)
	require.NoError(t, err)

	_, err = owner.Exec(ctx,
		`INSERT INTO security_events (id, kind, severity, detail) VALUES (gen_random_uuid(), 'recent_' || $1, 'INFO', '{}')`, tag)
	require.NoError(t, err)

	rows, err := owner.Query(ctx, `SELECT partition_name FROM cp_security_events_drop_expired(90)`)
	require.NoError(t, err)
	var dropped []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		dropped = append(dropped, n)
	}
	rows.Close()
	require.NoError(t, rows.Err())
	// At least the two this run made. A shared database may hold older months
	// from an earlier run, and dropping those is correct behaviour, not a
	// failure -- what matters is that both of this run's months went and nothing
	// inside the window did.
	assert.GreaterOrEqual(t, len(dropped), 2, "expected at least the two aged months to go, got %v", dropped)

	var aged, recent, skewed int
	require.NoError(t, owner.QueryRow(ctx, `SELECT count(*) FROM security_events WHERE kind = 'aged_' || $1`, tag).Scan(&aged))
	require.NoError(t, owner.QueryRow(ctx, `SELECT count(*) FROM security_events WHERE kind = 'recent_' || $1`, tag).Scan(&recent))
	require.NoError(t, owner.QueryRow(ctx, `SELECT count(*) FROM security_events WHERE kind = 'skewed_' || $1`, tag).Scan(&skewed))
	assert.Zero(t, aged, "the aged months were reported dropped but their rows are still readable")
	assert.Equal(t, 1, recent, "retention took a row inside the window")
	assert.Equal(t, 1, skewed, "retention dropped the default partition, whose rows are of unknown age by definition")

	var defaultStillThere bool
	require.NoError(t, owner.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = 'security_events_default')`).Scan(&defaultStillThere))
	assert.True(t, defaultStillThere, "the default partition was dropped")
}

// The functions are SECURITY DEFINER, so who may EXECUTE them is the whole
// access control. ADR-0020 rejected granting cp_ops a general DELETE for
// exactly this reason; granting it general DDL instead would have been the same
// mistake in a different verb.
func TestIntegration_OnlyTheOperationsRoleMayManagePartitions(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	require.NoError(t, migrate.Up(ctx, migrateURL))
	owner := connect(t, migrateURL)

	fns := []string{
		"cp_security_events_ensure_partitions(integer)",
		"cp_security_events_drop_expired(integer)",
	}
	for _, fn := range fns {
		var opsMay, appMay, roMay, publicMay bool
		require.NoError(t, owner.QueryRow(ctx,
			`SELECT has_function_privilege('cp_ops', $1, 'EXECUTE'),
			        has_function_privilege('cp_app', $1, 'EXECUTE'),
			        has_function_privilege('cp_readonly', $1, 'EXECUTE'),
			        has_function_privilege('public', $1, 'EXECUTE')`,
			fn).Scan(&opsMay, &appMay, &roMay, &publicMay))

		assert.True(t, opsMay, "%s: cp_ops cannot execute it, so the retention pass cannot run", fn)
		assert.False(t, appMay, "%s: cp_app can execute it; an attacker with the application credential can drop a month of the security trail", fn)
		assert.False(t, roMay, "%s: cp_readonly can execute it", fn)
		assert.False(t, publicMay, "%s: PUBLIC can execute it", fn)
	}

	// A SECURITY DEFINER function that resolves unqualified names through the
	// caller's search_path is a privilege escalation waiting for a schema.
	for _, fn := range []string{"cp_security_events_ensure_partitions", "cp_security_events_drop_expired"} {
		var cfg []string
		require.NoError(t, owner.QueryRow(ctx,
			`SELECT coalesce(proconfig, ARRAY[]::text[]) FROM pg_proc WHERE proname = $1`, fn).Scan(&cfg))
		assert.Contains(t, cfg, "search_path=public, pg_temp",
			"%s is SECURITY DEFINER with no pinned search_path", fn)
	}

	// And cp_ops still holds no DDL of its own: the function is the boundary,
	// not a shortcut around a grant that was given anyway.
	var opsOwnsAny bool
	require.NoError(t, owner.QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM pg_class c JOIN pg_roles r ON r.oid = c.relowner
		    WHERE r.rolname = 'cp_ops' AND c.relkind IN ('r','p'))`).Scan(&opsOwnsAny))
	assert.False(t, opsOwnsAny, "cp_ops owns a table, which is DDL power the function design exists to avoid")
}

// Creating runway is not destructive and runs on every pass, so it has to be
// safe to call repeatedly and has to refuse an argument that would create
// thousands of partitions.
func TestIntegration_EnsurePartitionsIsIdempotentAndBounded(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	require.NoError(t, migrate.Up(ctx, migrateURL))
	owner := connect(t, migrateURL)
	tag := fmt.Sprintf("%d", time.Now().UnixNano())

	for _, months := range []int{-1, 0, 61, 1000} {
		_, err := owner.Exec(ctx, `SELECT cp_security_events_ensure_partitions($1)`, months)
		require.Error(t, err, "months_ahead=%d was accepted", months)
		assert.Contains(t, err.Error(), "between 1 and 60")
	}

	var first, second int
	require.NoError(t, owner.QueryRow(ctx, `SELECT cp_security_events_ensure_partitions(24)`).Scan(&first))
	require.NoError(t, owner.QueryRow(ctx, `SELECT cp_security_events_ensure_partitions(24)`).Scan(&second))
	assert.Zero(t, second, "the second identical call created %d more partitions; the pass runs hourly", second)

	// Whatever it created, the table must still cover the current month.
	var landsIn string
	_, err := owner.Exec(ctx,
		`INSERT INTO security_events (id, kind, severity, detail) VALUES (gen_random_uuid(), 'now_probe_' || $1, 'INFO', '{}')`, tag)
	require.NoError(t, err)
	require.NoError(t, owner.QueryRow(ctx,
		`SELECT tableoid::regclass::text FROM security_events WHERE kind = 'now_probe_' || $1`, tag).Scan(&landsIn))
	assert.NotEqual(t, "security_events_default", landsIn,
		"a row written now landed in the default partition, which retention can never drop")
}
