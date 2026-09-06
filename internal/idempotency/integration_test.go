//go:build integration

package idempotency

import (
	"context"
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
)

// Shared with internal/db and test/integration/migrations: schema-mutating
// suites hold pg_advisory_lock(424242) exclusively; this DML-only suite holds
// it shared.
const testAdvisoryLockID = 424242

var (
	testAppURL     = os.Getenv("CP_TEST_DATABASE_URL")
	testMigrateURL = os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	testDB         *db.DB
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
		fmt.Fprintln(os.Stderr, "idempotency integration: connect for advisory lock:", err)
		return 1
	}
	defer func() { _ = lockConn.Close(ctx) }()
	if _, err := lockConn.Exec(ctx, "SELECT pg_advisory_lock_shared($1)", testAdvisoryLockID); err != nil {
		fmt.Fprintln(os.Stderr, "idempotency integration: advisory lock:", err)
		return 1
	}
	defer func() { _, _ = lockConn.Exec(ctx, "SELECT pg_advisory_unlock_shared($1)", testAdvisoryLockID) }()
	// Schema is a precondition; Up is a no-op when already applied, and the
	// migrations suite (exclusive lock) never runs concurrently with us.
	if err := migrate.Up(ctx, testMigrateURL); err != nil {
		fmt.Fprintln(os.Stderr, "idempotency integration: migrate up:", err)
		return 1
	}
	testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "idempotency-itest", MaxConns: 10})
	if err != nil {
		fmt.Fprintln(os.Stderr, "idempotency integration: open pool:", err)
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

// fakeClock is a settable clock; cp_app cannot delete rows, so every test uses
// a fresh actor id instead of cleaning up.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newFixture(t *testing.T) (*Store, *fakeClock, string) {
	t.Helper()
	requireEnv(t)
	clk := &fakeClock{t: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)}
	return NewStore(clk.Now), clk, "actor-" + uuid.NewString()
}

func begin(t *testing.T, s *Store, actor, endpoint, key, hash string, ttl time.Duration) (Begun, error) {
	t.Helper()
	var out Begun
	err := testDB.InTx(context.Background(), db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.Begin(ctx, tx, actor, endpoint, key, hash, ttl)
		return err
	})
	return out, err
}

func TestIntegration_AcquireCompleteReplay(t *testing.T) {
	s, clk, actor := newFixture(t)
	ctx := context.Background()
	const endpoint, key = "POST /v1/intents", "idem-1"
	hash := HashRequest("POST", "/v1/intents", []byte(`{"amount":"10.00"}`))

	got, err := begin(t, s, actor, endpoint, key, hash, time.Hour)
	require.NoError(t, err)
	acq, ok := got.(Acquired)
	require.True(t, ok, "%T", got)
	assert.Equal(t, clk.Now().Add(time.Hour), acq.ExpiresAt)

	rec, found, err := s.Get(ctx, testDB, actor, endpoint, key)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, StatusInProgress, rec.Status)
	assert.Equal(t, hash, rec.RequestHash)
	assert.Nil(t, rec.CompletedAt)

	// Second Begin for the same request while in progress.
	got, err = begin(t, s, actor, endpoint, key, hash, time.Hour)
	require.NoError(t, err)
	ip, ok := got.(InProgress)
	require.True(t, ok, "%T", got)
	assert.Equal(t, clk.Now(), ip.StartedAt)
	assert.Equal(t, clk.Now().Add(time.Hour), ip.ExpiresAt)

	clk.Advance(2 * time.Second)
	body := []byte(`{"intent_id":"abc","state":"ACCEPTED"}`)
	require.NoError(t, testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return s.Complete(ctx, tx, actor, endpoint, key, 201, "intent", "abc", body)
	}))

	got, err = begin(t, s, actor, endpoint, key, hash, time.Hour)
	require.NoError(t, err)
	rp, ok := got.(Replay)
	require.True(t, ok, "%T", got)
	assert.Equal(t, 201, rp.ResponseStatus)
	assert.Equal(t, "intent", rp.ResourceType)
	assert.Equal(t, "abc", rp.ResourceID)
	assert.JSONEq(t, string(body), string(rp.ResponseBody))
	assert.Equal(t, clk.Now(), rp.CompletedAt)

	// Completing twice is an error, not a silent overwrite.
	err = testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return s.Complete(ctx, tx, actor, endpoint, key, 200, "intent", "other", nil)
	})
	require.ErrorIs(t, err, ErrNotInProgress)

	// A replay result is stable even after a "different" replay attempt.
	got, err = begin(t, s, actor, endpoint, key, hash, time.Hour)
	require.NoError(t, err)
	rp, ok = got.(Replay)
	require.True(t, ok, "%T", got)
	assert.Equal(t, "abc", rp.ResourceID)
}

func TestIntegration_DifferentHashIsDeterministicConflict(t *testing.T) {
	s, _, actor := newFixture(t)
	const endpoint, key = "POST /v1/intents", "idem-conflict"
	h1 := HashRequest("POST", "/v1/intents", []byte(`{"amount":"10.00"}`))
	h2 := HashRequest("POST", "/v1/intents", []byte(`{"amount":"99.00"}`))

	got, err := begin(t, s, actor, endpoint, key, h1, time.Hour)
	require.NoError(t, err)
	require.IsType(t, Acquired{}, got)

	// In progress with a different request.
	_, err = begin(t, s, actor, endpoint, key, h2, time.Hour)
	require.ErrorIs(t, err, ErrKeyReuseConflict)

	// Completed with a different request: still a conflict, never a replay.
	require.NoError(t, testDB.InTx(context.Background(), db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return s.Complete(ctx, tx, actor, endpoint, key, 201, "intent", "x", nil)
	}))
	_, err = begin(t, s, actor, endpoint, key, h2, time.Hour)
	require.ErrorIs(t, err, ErrKeyReuseConflict)
	got, err = begin(t, s, actor, endpoint, key, h1, time.Hour)
	require.NoError(t, err)
	require.IsType(t, Replay{}, got)

	// The same key on another endpoint or for another actor is independent.
	got, err = begin(t, s, actor, "POST /v1/orders", key, h2, time.Hour)
	require.NoError(t, err)
	require.IsType(t, Acquired{}, got)
	got, err = begin(t, s, "actor-"+uuid.NewString(), endpoint, key, h2, time.Hour)
	require.NoError(t, err)
	require.IsType(t, Acquired{}, got)
}

func TestIntegration_ConcurrentBegin_ExactlyOneAcquires(t *testing.T) {
	s, _, actor := newFixture(t)
	const endpoint, key, n = "POST /v1/intents", "idem-race", 50
	hash := HashRequest("POST", "/v1/intents", []byte(`{"amount":"1.00"}`))

	results := make([]Begun, n)
	errs := make([]error, n)
	var start sync.WaitGroup
	start.Add(1)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start.Wait()
			results[i], errs[i] = begin(t, s, actor, endpoint, key, hash, time.Hour)
		}(i)
	}
	start.Done()
	wg.Wait()

	acquired, inProgress := 0, 0
	for i := 0; i < n; i++ {
		require.NoError(t, errs[i], "goroutine %d", i)
		switch results[i].(type) {
		case Acquired:
			acquired++
		case InProgress:
			inProgress++
		default:
			t.Fatalf("goroutine %d: unexpected %T", i, results[i])
		}
	}
	assert.Equal(t, 1, acquired)
	assert.Equal(t, n-1, inProgress)
}

func TestIntegration_FailedKeyIsReacquirable(t *testing.T) {
	s, clk, actor := newFixture(t)
	ctx := context.Background()
	const endpoint, key = "POST /v1/intents", "idem-fail"
	hash := HashRequest("POST", "/v1/intents", []byte(`{"amount":"1.00"}`))

	got, err := begin(t, s, actor, endpoint, key, hash, time.Hour)
	require.NoError(t, err)
	require.IsType(t, Acquired{}, got)

	require.NoError(t, testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return s.Fail(ctx, tx, actor, endpoint, key, 500, []byte(`{"code":"INTERNAL"}`))
	}))
	rec, _, err := s.Get(ctx, testDB, actor, endpoint, key)
	require.NoError(t, err)
	assert.Equal(t, StatusFailed, rec.Status)
	require.NotNil(t, rec.ResponseStatus)
	assert.Equal(t, 500, *rec.ResponseStatus)

	// Fail twice is rejected.
	err = testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return s.Fail(ctx, tx, actor, endpoint, key, 500, nil)
	})
	require.ErrorIs(t, err, ErrNotInProgress)

	// Same request retried after failure: re-acquired with a fresh window.
	clk.Advance(time.Minute)
	got, err = begin(t, s, actor, endpoint, key, hash, time.Hour)
	require.NoError(t, err)
	require.IsType(t, Acquired{}, got)
	rec, _, err = s.Get(ctx, testDB, actor, endpoint, key)
	require.NoError(t, err)
	assert.Equal(t, StatusInProgress, rec.Status)
	assert.Nil(t, rec.ResponseStatus)
	assert.Nil(t, rec.ResponseBody)
	assert.Equal(t, clk.Now(), rec.CreatedAt)
	assert.Equal(t, clk.Now().Add(time.Hour), rec.ExpiresAt)

	// But a failed key with a different request is still a conflict.
	other := HashRequest("POST", "/v1/intents", []byte(`{"amount":"2.00"}`))
	_, err = begin(t, s, actor, endpoint, key, other, time.Hour)
	require.ErrorIs(t, err, ErrKeyReuseConflict)
}

func TestIntegration_ExpiredRowsAreReacquired(t *testing.T) {
	s, clk, actor := newFixture(t)
	ctx := context.Background()
	const endpoint = "POST /v1/intents"
	h1 := HashRequest("POST", "/v1/intents", []byte(`{"amount":"1.00"}`))
	h2 := HashRequest("POST", "/v1/intents", []byte(`{"amount":"2.00"}`))

	// Expired IN_PROGRESS (a crashed handler that never completed).
	got, err := begin(t, s, actor, endpoint, "exp-inprogress", h1, time.Minute)
	require.NoError(t, err)
	require.IsType(t, Acquired{}, got)
	clk.Advance(30 * time.Second)
	got, err = begin(t, s, actor, endpoint, "exp-inprogress", h1, time.Minute)
	require.NoError(t, err)
	require.IsType(t, InProgress{}, got, "not yet expired")
	clk.Advance(30 * time.Second) // exactly at expires_at: expired
	got, err = begin(t, s, actor, endpoint, "exp-inprogress", h1, time.Minute)
	require.NoError(t, err)
	acq, ok := got.(Acquired)
	require.True(t, ok, "%T", got)
	assert.Equal(t, clk.Now().Add(time.Minute), acq.ExpiresAt)

	// Expired rows behave as deleted: even a different hash re-acquires.
	got, err = begin(t, s, actor, endpoint, "exp-otherhash", h1, time.Minute)
	require.NoError(t, err)
	require.IsType(t, Acquired{}, got)
	clk.Advance(2 * time.Minute)
	got, err = begin(t, s, actor, endpoint, "exp-otherhash", h2, time.Minute)
	require.NoError(t, err)
	require.IsType(t, Acquired{}, got)
	rec, _, err := s.Get(ctx, testDB, actor, endpoint, "exp-otherhash")
	require.NoError(t, err)
	assert.Equal(t, h2, rec.RequestHash)

	// Expired COMPLETED is not replayed (cleanup would have removed it).
	got, err = begin(t, s, actor, endpoint, "exp-completed", h1, time.Minute)
	require.NoError(t, err)
	require.IsType(t, Acquired{}, got)
	require.NoError(t, testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return s.Complete(ctx, tx, actor, endpoint, "exp-completed", 201, "intent", "i1", []byte(`{"ok":true}`))
	}))
	got, err = begin(t, s, actor, endpoint, "exp-completed", h1, time.Minute)
	require.NoError(t, err)
	require.IsType(t, Replay{}, got)
	clk.Advance(2 * time.Minute)
	got, err = begin(t, s, actor, endpoint, "exp-completed", h1, time.Minute)
	require.NoError(t, err)
	require.IsType(t, Acquired{}, got)
	rec, _, err = s.Get(ctx, testDB, actor, endpoint, "exp-completed")
	require.NoError(t, err)
	assert.Equal(t, StatusInProgress, rec.Status)
	assert.Nil(t, rec.ResponseBody, "stale result was cleared")
}

func TestIntegration_BeginRollsBackWithCallerTransaction(t *testing.T) {
	s, _, actor := newFixture(t)
	ctx := context.Background()
	const endpoint, key = "POST /v1/intents", "idem-rollback"
	hash := HashRequest("POST", "/v1/intents", nil)

	sentinel := fmt.Errorf("command failed before commit")
	err := testDB.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		got, err := s.Begin(ctx, tx, actor, endpoint, key, hash, time.Hour)
		if err != nil {
			return err
		}
		if _, ok := got.(Acquired); !ok {
			return fmt.Errorf("expected Acquired, got %T", got)
		}
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)

	_, found, err := s.Get(ctx, testDB, actor, endpoint, key)
	require.NoError(t, err)
	assert.False(t, found, "the claim disappeared with the rolled-back transaction")

	// Serializable transactions work too (the FOR UPDATE re-read path included).
	require.NoError(t, testDB.Serializable(ctx, func(ctx context.Context, tx pgx.Tx) error {
		got, err := s.Begin(ctx, tx, actor, endpoint, key, hash, time.Hour)
		if err != nil {
			return err
		}
		if _, ok := got.(Acquired); !ok {
			return fmt.Errorf("expected Acquired, got %T", got)
		}
		return s.Complete(ctx, tx, actor, endpoint, key, 200, "", "", nil)
	}))
	require.NoError(t, testDB.Serializable(ctx, func(ctx context.Context, tx pgx.Tx) error {
		got, err := s.Begin(ctx, tx, actor, endpoint, key, hash, time.Hour)
		if err != nil {
			return err
		}
		rp, ok := got.(Replay)
		if !ok {
			return fmt.Errorf("expected Replay, got %T", got)
		}
		assert.Equal(t, 200, rp.ResponseStatus)
		assert.Nil(t, rp.ResponseBody)
		assert.Empty(t, rp.ResourceType)
		return nil
	}))
}

func TestIntegration_AppRoleCannotDeleteIdempotencyKeys(t *testing.T) {
	requireEnv(t)
	_, err := testDB.Exec(context.Background(), "DELETE FROM idempotency_keys WHERE actor_id = $1", "nobody")
	require.Error(t, err)
	assert.True(t, db.IsInsufficientPrivilege(err), "SQLSTATE %q", db.SQLState(err))
}
