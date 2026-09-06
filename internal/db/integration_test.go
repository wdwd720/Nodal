//go:build integration

package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Integration tests share the controlplane_test database with other packages.
// Schema-mutating suites (test/integration/migrations) take pg_advisory_lock
// 424242 exclusively; DML-only suites hold it shared so they never observe a
// half-migrated schema.
const testAdvisoryLockID = 424242

const scratchTable = "db_itest_counters"

var (
	testAppURL     = os.Getenv("CP_TEST_DATABASE_URL")
	testMigrateURL = os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
)

func TestMain(m *testing.M) {
	os.Exit(testMain(m))
}

func testMain(m *testing.M) int {
	if testAppURL == "" || testMigrateURL == "" {
		return m.Run()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	lockConn, err := pgx.Connect(ctx, testAppURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "db integration: connect for advisory lock:", err)
		return 1
	}
	defer func() { _ = lockConn.Close(ctx) }()
	if _, err := lockConn.Exec(ctx, "SELECT pg_advisory_lock_shared($1)", testAdvisoryLockID); err != nil {
		fmt.Fprintln(os.Stderr, "db integration: advisory lock:", err)
		return 1
	}
	defer func() { _, _ = lockConn.Exec(ctx, "SELECT pg_advisory_unlock_shared($1)", testAdvisoryLockID) }()

	admin, err := pgx.Connect(ctx, testMigrateURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "db integration: connect as migrate role:", err)
		return 1
	}
	defer func() { _ = admin.Close(ctx) }()
	setup := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %[1]s (id int PRIMARY KEY, n int NOT NULL DEFAULT 0);
GRANT SELECT, INSERT, UPDATE, DELETE ON %[1]s TO cp_app`, scratchTable)
	if _, err := admin.Exec(ctx, setup); err != nil {
		fmt.Fprintln(os.Stderr, "db integration: create scratch table:", err)
		return 1
	}
	defer func() { _, _ = admin.Exec(ctx, "DROP TABLE IF EXISTS "+scratchTable) }()

	return m.Run()
}

func requireEnv(t *testing.T) {
	t.Helper()
	if testAppURL == "" || testMigrateURL == "" {
		t.Skip("CP_TEST_DATABASE_URL / CP_TEST_MIGRATE_DATABASE_URL not set; skipping integration test")
	}
}

func openTest(t *testing.T, cfg Config) *DB {
	t.Helper()
	requireEnv(t)
	if cfg.URL == "" {
		cfg.URL = testAppURL
	}
	if cfg.AppName == "" {
		cfg.AppName = "db-itest"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d, err := Open(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(d.Close)
	return d
}

// resetRow makes sure the counter row id exists with n = 0.
func resetRow(t *testing.T, d *DB, id int) {
	t.Helper()
	_, err := d.Exec(context.Background(),
		"INSERT INTO "+scratchTable+" (id, n) VALUES ($1, 0) ON CONFLICT (id) DO UPDATE SET n = 0", id)
	require.NoError(t, err)
}

func readN(t *testing.T, d *DB, id int) int {
	t.Helper()
	var n int
	require.NoError(t, d.QueryRow(context.Background(), "SELECT n FROM "+scratchTable+" WHERE id = $1", id).Scan(&n))
	return n
}

func TestIntegration_Open_AppliesSessionSettings(t *testing.T) {
	d := openTest(t, Config{StatementTimeout: 7 * time.Second, LockTimeout: 3 * time.Second, MaxConns: 2})
	ctx := context.Background()

	var appName, stmt, lock, tz string
	require.NoError(t, d.QueryRow(ctx, "SELECT current_setting('application_name'), current_setting('statement_timeout'), current_setting('lock_timeout'), current_setting('TimeZone')").
		Scan(&appName, &stmt, &lock, &tz))
	assert.Equal(t, "db-itest", appName)
	assert.Equal(t, "7s", stmt)
	assert.Equal(t, "3s", lock)
	assert.Equal(t, "UTC", tz)
	require.NoError(t, d.Ping(ctx))
	assert.NotNil(t, d.Pool())
}

func TestIntegration_Open_StatementTimeoutIsEnforced(t *testing.T) {
	d := openTest(t, Config{StatementTimeout: 300 * time.Millisecond, MaxConns: 1})
	_, err := d.Exec(context.Background(), "SELECT pg_sleep(2)")
	require.Error(t, err)
	assert.True(t, IsStatementTimeout(err), "got SQLSTATE %q", SQLState(err))
}

func TestIntegration_InTx_CommitAndRollback(t *testing.T) {
	d := openTest(t, Config{})
	ctx := context.Background()
	const id = 1
	resetRow(t, d, id)

	require.NoError(t, d.InTx(ctx, TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE "+scratchTable+" SET n = n + 5 WHERE id = $1", id)
		return err
	}))
	assert.Equal(t, 5, readN(t, d, id))

	boom := errors.New("business rule violated")
	err := d.InTx(ctx, TxOptions{MaxRetries: 5}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "UPDATE "+scratchTable+" SET n = n + 100 WHERE id = $1", id); err != nil {
			return err
		}
		return boom
	})
	require.ErrorIs(t, err, boom)
	assert.Same(t, boom, err, "fn errors come back unwrapped")
	assert.Equal(t, 5, readN(t, d, id), "rolled back")

	// Read-only transactions refuse writes.
	err = d.InTx(ctx, TxOptions{ReadOnly: true}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE "+scratchTable+" SET n = 0 WHERE id = $1", id)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "25006", SQLState(err), "read_only_sql_transaction")
	assert.Equal(t, 5, readN(t, d, id))
}

func TestIntegration_InTx_PanicRollsBackAndRepanics(t *testing.T) {
	d := openTest(t, Config{MaxConns: 2})
	ctx := context.Background()
	const id = 2
	resetRow(t, d, id)

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_ = d.InTx(ctx, TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "UPDATE "+scratchTable+" SET n = 99 WHERE id = $1", id); err != nil {
				return err
			}
			panic("handler exploded")
		})
	}()
	require.Equal(t, "handler exploded", recovered)
	assert.Equal(t, 0, readN(t, d, id), "panic rolled the transaction back")

	// The pool is still healthy: both connections usable.
	for range 4 {
		require.NoError(t, d.Ping(ctx))
	}
}

func TestIntegration_InTx_NonRetryableErrorRunsOnce(t *testing.T) {
	d := openTest(t, Config{})
	ctx := context.Background()
	const id = 3
	resetRow(t, d, id)

	calls := 0
	err := d.InTx(ctx, TxOptions{MaxRetries: 5}, func(ctx context.Context, tx pgx.Tx) error {
		calls++
		_, err := tx.Exec(ctx, "INSERT INTO "+scratchTable+" (id, n) VALUES ($1, 0)", id) // duplicate PK
		return err
	})
	require.Error(t, err)
	assert.True(t, IsUniqueViolation(err))
	assert.Equal(t, 1, calls, "unique violations are never retried")
}

func TestIntegration_Serializable_ConcurrentWritersRetryAndBothCommit(t *testing.T) {
	d := openTest(t, Config{MaxConns: 4})
	ctx := context.Background()
	const id = 4
	resetRow(t, d, id)

	var attempts atomic.Int32
	var barrier sync.WaitGroup
	barrier.Add(2)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			first := true
			errs[i] = d.Serializable(ctx, func(ctx context.Context, tx pgx.Tx) error {
				attempts.Add(1)
				var n int
				if err := tx.QueryRow(ctx, "SELECT n FROM "+scratchTable+" WHERE id = $1", id).Scan(&n); err != nil {
					return err
				}
				if first {
					// Both transactions read the same snapshot before either writes.
					first = false
					barrier.Done()
					barrier.Wait()
				}
				_, err := tx.Exec(ctx, "UPDATE "+scratchTable+" SET n = $2 WHERE id = $1", id, n+1)
				return err
			})
		}(i)
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	assert.Equal(t, 2, readN(t, d, id), "no lost update")
	assert.GreaterOrEqual(t, attempts.Load(), int32(3), "at least one transaction had to retry")
}

func TestIntegration_InTx_DeadlockIsRetried(t *testing.T) {
	// lock_timeout must exceed deadlock_timeout (1s default) so Postgres reports 40P01 rather than 55P03.
	d := openTest(t, Config{MaxConns: 4, LockTimeout: 10 * time.Second})
	ctx := context.Background()
	const a, b = 5, 6
	resetRow(t, d, a)
	resetRow(t, d, b)

	var attempts atomic.Int32
	var barrier sync.WaitGroup
	barrier.Add(2)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	order := [][2]int{{a, b}, {b, a}}
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			first := true
			errs[i] = d.InTx(ctx, TxOptions{MaxRetries: 3}, func(ctx context.Context, tx pgx.Tx) error {
				attempts.Add(1)
				if _, err := tx.Exec(ctx, "UPDATE "+scratchTable+" SET n = n + 1 WHERE id = $1", order[i][0]); err != nil {
					return err
				}
				if first {
					first = false
					barrier.Done()
					barrier.Wait()
				}
				_, err := tx.Exec(ctx, "UPDATE "+scratchTable+" SET n = n + 1 WHERE id = $1", order[i][1])
				return err
			})
		}(i)
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	assert.Equal(t, 2, readN(t, d, a))
	assert.Equal(t, 2, readN(t, d, b))
	assert.GreaterOrEqual(t, attempts.Load(), int32(3), "the deadlock victim retried")
}

func TestIntegration_InTx_RetriesExhausted(t *testing.T) {
	d := openTest(t, Config{MaxConns: 3})
	ctx := context.Background()
	const id = 7
	resetRow(t, d, id)

	// Hold a row lock in a long-lived serializable transaction that updates the
	// row, so every other serializable writer fails with 40001 after waiting.
	holder, err := d.Pool().BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	require.NoError(t, err)
	defer holder.Rollback(ctx) //nolint:errcheck
	_, err = holder.Exec(ctx, "UPDATE "+scratchTable+" SET n = n + 1 WHERE id = $1", id)
	require.NoError(t, err)

	calls := 0
	err = d.InTx(ctx, TxOptions{Isolation: pgx.Serializable, MaxRetries: 2}, func(ctx context.Context, tx pgx.Tx) error {
		calls++
		var n int
		if err := tx.QueryRow(ctx, "SELECT n FROM "+scratchTable+" WHERE id = $1", id).Scan(&n); err != nil {
			return err
		}
		if calls == 1 {
			// Let the holder commit after our snapshot is taken; the UPDATE then
			// blocks on the row and reports a serialization failure.
			require.NoError(t, holder.Commit(ctx))
		}
		// After the holder committed, every subsequent attempt conflicts with
		// this ever-present writer to exhaust the budget deterministically.
		if calls > 1 {
			return &serializationInjector{}
		}
		_, err := tx.Exec(ctx, "UPDATE "+scratchTable+" SET n = $2 WHERE id = $1", id, n+10)
		return err
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRetriesExhausted)
	assert.True(t, IsSerializationFailure(err), "SQLSTATE %q", SQLState(err))
	assert.Equal(t, 3, calls, "first attempt plus MaxRetries")
	assert.Equal(t, 1, readN(t, d, id), "only the holder's write survived")
}

// serializationInjector is a synthetic 40001 used to keep a retry loop
// failing without depending on scheduler timing.
type serializationInjector struct{}

func (*serializationInjector) Error() string { return "injected serialization failure" }
func (*serializationInjector) Unwrap() error { return pgErr(SQLStateSerializationFailure) }
