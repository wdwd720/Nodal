//go:build integration

package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/errs"
)

// Shared with internal/db and test/integration/migrations: schema-mutating
// suites hold pg_advisory_lock(424242) exclusively; this suite holds it
// shared. Run against an isolated database (scripts/testdb -name audit).
const testAdvisoryLockID = 424242

var (
	testAppURL     = os.Getenv("CP_TEST_DATABASE_URL")
	testMigrateURL = os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	testDB         *db.DB
)

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	if testAppURL == "" || testMigrateURL == "" {
		return m.Run()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	lockConn, err := pgx.Connect(ctx, testAppURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "audit integration: connect for advisory lock:", err)
		return 1
	}
	defer func() { _ = lockConn.Close(ctx) }()
	if _, err := lockConn.Exec(ctx, "SELECT pg_advisory_lock_shared($1)", testAdvisoryLockID); err != nil {
		fmt.Fprintln(os.Stderr, "audit integration: advisory lock:", err)
		return 1
	}
	defer func() { _, _ = lockConn.Exec(ctx, "SELECT pg_advisory_unlock_shared($1)", testAdvisoryLockID) }()
	if err := migrate.Up(ctx, testMigrateURL); err != nil {
		fmt.Fprintln(os.Stderr, "audit integration: migrate up:", err)
		return 1
	}
	testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "audit-itest", MaxConns: 32})
	if err != nil {
		fmt.Fprintln(os.Stderr, "audit integration: open pool:", err)
		return 1
	}
	defer testDB.Close()
	return m.Run()
}

func requireEnv(t *testing.T) {
	t.Helper()
	if testDB == nil {
		t.Skip("CP_TEST_DATABASE_URL / CP_TEST_MIGRATE_DATABASE_URL not set; skipping integration test")
	}
}

func migrateConn(t *testing.T) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), testMigrateURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func freshStream() string { return AccountStream(uuid.NewString()) }

func appendOne(t *testing.T, w Writer, e Event) Appended {
	t.Helper()
	var out Appended
	require.NoError(t, testDB.InTx(context.Background(), db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = w.Append(ctx, tx, e)
		return err
	}))
	return out
}

func sampleEvent(stream string, i int) Event {
	e := validEvent()
	e.Stream = stream
	e.ResourceID = fmt.Sprintf("res-%d", i)
	e.Payload = json.RawMessage(fmt.Sprintf(`{"z":"%d","a":{"y":true,"b":[1,2]}}`, i))
	e.OccurredAt = t0.Add(time.Duration(i) * time.Second).Add(123456789 * time.Nanosecond)
	return e
}

func TestIntegration_AppendLinksAndVerifies(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	w := NewWriterWithBuildVersion("itest-build")
	v := NewVerifier()
	stream := freshStream()

	var prev []byte
	for i := 1; i <= 5; i++ {
		app := appendOne(t, w, sampleEvent(stream, i))
		assert.Equal(t, stream, app.Stream)
		assert.Equal(t, int64(i), app.StreamSeq)
		assert.Len(t, app.ContentHash, 32)
		assert.Equal(t, prev, app.PrevHash)
		assert.False(t, app.ID.IsZero())
		// The stored hash equals the exported HashEvent for the same inputs.
		want, err := HashEvent(sampleEvent(stream, i), int64(i), prev, "itest-build")
		require.NoError(t, err)
		assert.Equal(t, want, app.ContentHash)
		prev = app.ContentHash
	}

	rep, err := v.VerifyStream(ctx, testDB, stream)
	require.NoError(t, err)
	assert.True(t, rep.OK, rep.Reason)
	assert.Equal(t, 5, rep.Events)
	assert.Nil(t, rep.BrokenAt)

	// Stored columns are the normalized forms.
	var (
		payload, hostIP, build string
		occurred               time.Time
		prevHash               []byte
	)
	require.NoError(t, testDB.QueryRow(ctx,
		`SELECT payload::text, host(source_ip), build_version, occurred_at, prev_hash FROM audit_events WHERE stream = $1 AND stream_seq = 3`, stream).
		Scan(&payload, &hostIP, &build, &occurred, &prevHash))
	assert.JSONEq(t, `{"a":{"b":[1,2],"y":true},"z":"3"}`, payload)
	assert.Equal(t, "10.0.0.1", hostIP)
	assert.Equal(t, "itest-build", build)
	assert.Equal(t, t0.Add(3*time.Second).Add(123456*time.Microsecond), occurred.UTC(), "microsecond precision")
	assert.NotEmpty(t, prevHash)

	// An empty stream verifies trivially; VerifyAll covers every stream.
	empty, err := v.VerifyStream(ctx, testDB, freshStream())
	require.NoError(t, err)
	assert.True(t, empty.OK)
	assert.Equal(t, 0, empty.Events)

	reports, err := v.VerifyAll(ctx, testDB, 3)
	require.NoError(t, err)
	found := false
	for i, r := range reports {
		if i > 0 {
			assert.Less(t, reports[i-1].Stream, r.Stream, "stream order")
		}
		if r.Stream == stream {
			found = true
			assert.True(t, r.OK)
			assert.Equal(t, 5, r.Events)
		}
	}
	assert.True(t, found)
}

func TestIntegration_ValidationHappensBeforeAnyWrite(t *testing.T) {
	requireEnv(t)
	w := NewWriter()
	stream := freshStream()
	bad := sampleEvent(stream, 1)
	bad.ActorType = "AGENT_X"
	err := testDB.InTx(context.Background(), db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.Append(ctx, tx, bad)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	// A rolled-back append leaves no trace; the next append is seq 1.
	sentinel := errors.New("caller aborted")
	err = testDB.InTx(context.Background(), db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := w.Append(ctx, tx, sampleEvent(stream, 1)); err != nil {
			return err
		}
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)
	app := appendOne(t, w, sampleEvent(stream, 2))
	assert.Equal(t, int64(1), app.StreamSeq)
	assert.Nil(t, app.PrevHash)
	assert.Equal(t, "dev", func() string {
		var b string
		require.NoError(t, testDB.QueryRow(context.Background(), `SELECT build_version FROM audit_events WHERE id = $1`, app.ID).Scan(&b))
		return b
	}(), "config.BuildVersion default")
}

func TestIntegration_TamperIsDetectedAtExactSeq(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	w := NewWriter()
	v := NewVerifier()
	admin := migrateConn(t)

	// Tampering requires the table owner and disabling the immutability
	// trigger: something cp_app can never do (see the privilege test).
	tamper := func(t *testing.T, sql string, args ...any) {
		t.Helper()
		_, err := admin.Exec(ctx, `ALTER TABLE audit_events DISABLE TRIGGER audit_events_immutable`)
		require.NoError(t, err)
		defer func() {
			_, err := admin.Exec(ctx, `ALTER TABLE audit_events ENABLE TRIGGER audit_events_immutable`)
			require.NoError(t, err)
		}()
		_, err = admin.Exec(ctx, sql, args...)
		require.NoError(t, err)
	}

	t.Run("content changed", func(t *testing.T) {
		stream := freshStream()
		for i := 1; i <= 5; i++ {
			appendOne(t, w, sampleEvent(stream, i))
		}
		tamper(t, `UPDATE audit_events SET reason = 'edited after the fact' WHERE stream = $1 AND stream_seq = 3`, stream)
		rep, err := v.VerifyStream(ctx, testDB, stream)
		require.NoError(t, err)
		assert.False(t, rep.OK)
		require.NotNil(t, rep.BrokenAt)
		assert.Equal(t, int64(3), *rep.BrokenAt)
		assert.Contains(t, rep.Reason, "content_hash")
		assert.Equal(t, 3, rep.Events, "stops at the first broken row")
	})

	t.Run("payload changed", func(t *testing.T) {
		stream := freshStream()
		for i := 1; i <= 4; i++ {
			appendOne(t, w, sampleEvent(stream, i))
		}
		tamper(t, `UPDATE audit_events SET payload = '{"a":{"b":[1,2],"y":true},"z":"999"}'::jsonb WHERE stream = $1 AND stream_seq = 4`, stream)
		rep, err := v.VerifyStream(ctx, testDB, stream)
		require.NoError(t, err)
		require.NotNil(t, rep.BrokenAt)
		assert.Equal(t, int64(4), *rep.BrokenAt)
		assert.Contains(t, rep.Reason, "content_hash")
	})

	t.Run("link changed", func(t *testing.T) {
		stream := freshStream()
		for i := 1; i <= 5; i++ {
			appendOne(t, w, sampleEvent(stream, i))
		}
		tamper(t, `UPDATE audit_events SET prev_hash = decode('00', 'hex') WHERE stream = $1 AND stream_seq = 5`, stream)
		rep, err := v.VerifyStream(ctx, testDB, stream)
		require.NoError(t, err)
		require.NotNil(t, rep.BrokenAt)
		assert.Equal(t, int64(5), *rep.BrokenAt)
		assert.Contains(t, rep.Reason, "prev_hash")
	})

	t.Run("row deleted", func(t *testing.T) {
		stream := freshStream()
		for i := 1; i <= 5; i++ {
			appendOne(t, w, sampleEvent(stream, i))
		}
		tamper(t, `DELETE FROM audit_events WHERE stream = $1 AND stream_seq = 2`, stream)
		rep, err := v.VerifyStream(ctx, testDB, stream)
		require.NoError(t, err)
		require.NotNil(t, rep.BrokenAt)
		assert.Equal(t, int64(3), *rep.BrokenAt, "the gap is reported at the first surviving row after it")
		assert.Contains(t, rep.Reason, "sequence gap")
	})

	t.Run("re-hashed row with a broken link", func(t *testing.T) {
		// An attacker who recomputes content_hash for an edited row still
		// breaks the next row's prev_hash link.
		stream := freshStream()
		for i := 1; i <= 3; i++ {
			appendOne(t, w, sampleEvent(stream, i))
		}
		edited := sampleEvent(stream, 2)
		edited.Reason = "edited"
		var prev []byte
		require.NoError(t, testDB.QueryRow(ctx, `SELECT content_hash FROM audit_events WHERE stream = $1 AND stream_seq = 1`, stream).Scan(&prev))
		newHash, err := HashEvent(edited, 2, prev, "dev")
		require.NoError(t, err)
		tamper(t, `UPDATE audit_events SET reason = 'edited', content_hash = $2 WHERE stream = $1 AND stream_seq = 2`, stream, newHash)
		rep, err := v.VerifyStream(ctx, testDB, stream)
		require.NoError(t, err)
		require.NotNil(t, rep.BrokenAt)
		assert.Equal(t, int64(3), *rep.BrokenAt)
		assert.Contains(t, rep.Reason, "prev_hash")
	})
}

func TestIntegration_RowsAreImmutable(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	w := NewWriter()
	stream := freshStream()
	appendOne(t, w, sampleEvent(stream, 1))

	// cp_app has no UPDATE/DELETE privilege at all.
	_, err := testDB.Exec(ctx, `UPDATE audit_events SET reason = 'x' WHERE stream = $1`, stream)
	require.Error(t, err)
	assert.True(t, db.IsInsufficientPrivilege(err), "SQLSTATE %q", db.SQLState(err))
	_, err = testDB.Exec(ctx, `DELETE FROM audit_events WHERE stream = $1`, stream)
	require.Error(t, err)
	assert.True(t, db.IsInsufficientPrivilege(err), "SQLSTATE %q", db.SQLState(err))

	// Even the owner is stopped by the forbid_mutation trigger.
	admin := migrateConn(t)
	_, err = admin.Exec(ctx, `UPDATE audit_events SET reason = 'x' WHERE stream = $1`, stream)
	require.Error(t, err)
	assert.True(t, db.IsImmutableRow(err), "SQLSTATE %q: %v", db.SQLState(err), err)
	_, err = admin.Exec(ctx, `DELETE FROM audit_events WHERE stream = $1`, stream)
	require.Error(t, err)
	assert.True(t, db.IsImmutableRow(err), "SQLSTATE %q: %v", db.SQLState(err), err)

	var n int
	require.NoError(t, testDB.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE stream = $1 AND reason = 'x'`, stream).Scan(&n))
	assert.Equal(t, 0, n)
}

func TestIntegration_ConcurrentAppendsSameStream(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	w := NewWriter()
	stream := freshStream()
	const n = 100

	var start sync.WaitGroup
	start.Add(1)
	var wg sync.WaitGroup
	results := make([]Appended, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start.Wait()
			errs[i] = testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				results[i], err = w.Append(ctx, tx, sampleEvent(stream, i))
				return err
			})
		}(i)
	}
	start.Done()
	wg.Wait()

	seen := map[int64]bool{}
	for i := 0; i < n; i++ {
		require.NoError(t, errs[i], "goroutine %d", i)
		assert.False(t, seen[results[i].StreamSeq], "duplicate seq %d", results[i].StreamSeq)
		seen[results[i].StreamSeq] = true
	}
	for s := int64(1); s <= n; s++ {
		assert.True(t, seen[s], "missing seq %d", s)
	}
	rep, err := NewVerifier().VerifyStream(ctx, testDB, stream)
	require.NoError(t, err)
	assert.True(t, rep.OK, rep.Reason)
	assert.Equal(t, n, rep.Events)
}

func TestIntegration_ConcurrentAppendsAcrossStreams(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	w := NewWriter()
	const streams, perStream = 10, 20
	names := make([]string, streams)
	for i := range names {
		if i%2 == 0 {
			names[i] = AccountStream(uuid.NewString())
		} else {
			names[i] = AgentStream(uuid.NewString())
		}
	}

	var start sync.WaitGroup
	start.Add(1)
	var wg sync.WaitGroup
	errs := make([]error, streams*perStream)
	for s := 0; s < streams; s++ {
		for i := 0; i < perStream; i++ {
			wg.Add(1)
			go func(s, i int) {
				defer wg.Done()
				start.Wait()
				errs[s*perStream+i] = testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
					_, err := w.Append(ctx, tx, sampleEvent(names[s], i))
					return err
				})
			}(s, i)
		}
	}
	start.Done()
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "append %d", i)
	}
	v := NewVerifier()
	for _, name := range names {
		rep, err := v.VerifyStream(ctx, testDB, name)
		require.NoError(t, err)
		assert.True(t, rep.OK, "%s: %s", name, rep.Reason)
		assert.Equal(t, perStream, rep.Events, name)
	}
}

func TestIntegration_SerializableAppendsRetryToContiguity(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	w := NewWriter()
	stream := freshStream()
	const n = 6

	var start sync.WaitGroup
	start.Add(1)
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start.Wait()
			errs[i] = testDB.Serializable(ctx, func(ctx context.Context, tx pgx.Tx) error {
				_, err := w.Append(ctx, tx, sampleEvent(stream, i))
				return err
			})
		}(i)
	}
	start.Done()
	wg.Wait()

	committed := 0
	for i, err := range errs {
		if err == nil {
			committed++
			continue
		}
		// Under SERIALIZABLE the snapshot predates the advisory lock, so a
		// lost race is a serialization failure that db.Serializable retries;
		// exhausting the retry budget is the only acceptable failure.
		assert.True(t, db.IsSerializationFailure(err), "goroutine %d: SQLSTATE %q: %v", i, db.SQLState(err), err)
	}
	assert.GreaterOrEqual(t, committed, 1)
	rep, err := NewVerifier().VerifyStream(ctx, testDB, stream)
	require.NoError(t, err)
	assert.True(t, rep.OK, rep.Reason)
	assert.Equal(t, committed, rep.Events, "every committed append is in the chain, contiguous")
}
