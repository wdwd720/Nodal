package db

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

func pgErr(code string) error {
	return &pgconn.PgError{Severity: "ERROR", Code: code, Message: "synthetic " + code}
}

func TestShouldRetry(t *testing.T) {
	t.Parallel()
	serialization := pgErr(SQLStateSerializationFailure)
	deadlock := pgErr(SQLStateDeadlockDetected)
	unique := pgErr(SQLStateUniqueViolation)
	plain := errors.New("boom")

	tests := []struct {
		name       string
		err        error
		attempt    int
		maxRetries int
		want       bool
	}{
		{"nil error never retries", nil, 0, 5, false},
		{"serialization within budget", serialization, 0, 5, true},
		{"serialization wrapped within budget", fmt.Errorf("repo: %w", serialization), 2, 5, true},
		{"serialization last allowed attempt", serialization, 4, 5, true},
		{"serialization budget exhausted", serialization, 5, 5, false},
		{"serialization zero budget", serialization, 0, 0, false},
		{"serialization negative budget", serialization, 0, -1, false},
		{"negative attempt", serialization, -1, 5, false},
		{"deadlock within budget", deadlock, 1, 3, true},
		{"deadlock exhausted", deadlock, 3, 3, false},
		{"unique violation never", unique, 0, 5, false},
		{"plain error never", plain, 0, 5, false},
		{"plain error wrapping nothing pg", fmt.Errorf("x: %w", plain), 0, 5, false},
		{"context canceled never", context.Canceled, 0, 5, false},
		{"serialization joined with cancellation never", errors.Join(serialization, context.Canceled), 0, 5, false},
		{"deadline exceeded never", fmt.Errorf("q: %w", context.DeadlineExceeded), 0, 5, false},
		{"statement timeout never", pgErr(SQLStateQueryCanceled), 0, 5, false},
		{"lock timeout never", pgErr(SQLStateLockNotAvailable), 0, 5, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, shouldRetry(tt.err, tt.attempt, tt.maxRetries))
		})
	}
}

func TestBackoffDelay_Bounds(t *testing.T) {
	t.Parallel()
	// attempt 0: d=5ms → [2.5ms, 5ms)
	assert.Equal(t, 2500*time.Microsecond, backoffDelay(0, 0))
	assert.Less(t, backoffDelay(0, 0.999), 5*time.Millisecond)
	// attempt 3: d=40ms → [20ms, 40ms)
	assert.Equal(t, 20*time.Millisecond, backoffDelay(3, 0))
	// large attempts are capped at 500ms → [250ms, 500ms)
	assert.Equal(t, 250*time.Millisecond, backoffDelay(10, 0))
	assert.Equal(t, 250*time.Millisecond, backoffDelay(1000, 0))
	assert.Less(t, backoffDelay(1000, 1.5), 500*time.Millisecond)
	// negative attempt and r are clamped
	assert.Equal(t, 2500*time.Microsecond, backoffDelay(-4, -1))
}

func TestProp_BackoffDelay(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		attempt := rapid.IntRange(0, 64).Draw(rt, "attempt")
		r := rapid.Float64Range(0, 0.9999999).Draw(rt, "r")
		d := backoffDelay(attempt, r)
		if d < retryBaseDelay/2 || d >= retryMaxDelay {
			rt.Fatalf("delay %v out of [%v, %v)", d, retryBaseDelay/2, retryMaxDelay)
		}
		// monotone non-decreasing in attempt for a fixed r
		if attempt > 0 && backoffDelay(attempt-1, r) > d {
			rt.Fatalf("delay decreased from attempt %d to %d", attempt-1, attempt)
		}
		// jitter never exceeds the un-jittered delay for the attempt
		if backoffDelay(attempt, 0) > d {
			rt.Fatalf("jitter reduced delay below floor")
		}
	})
}

type recordingSleep struct {
	delays []time.Duration
	err    error
}

func (r *recordingSleep) sleep(_ context.Context, d time.Duration) error {
	r.delays = append(r.delays, d)
	return r.err
}

func TestRunWithRetry(t *testing.T) {
	t.Parallel()

	t.Run("succeeds first time without sleeping", func(t *testing.T) {
		t.Parallel()
		rs := &recordingSleep{}
		calls := 0
		err := runWithRetry(context.Background(), 5, rs.sleep, func(context.Context) error { calls++; return nil })
		require.NoError(t, err)
		assert.Equal(t, 1, calls)
		assert.Empty(t, rs.delays)
	})

	t.Run("retries serialization failures then succeeds", func(t *testing.T) {
		t.Parallel()
		rs := &recordingSleep{}
		calls := 0
		err := runWithRetry(context.Background(), 5, rs.sleep, func(context.Context) error {
			calls++
			if calls < 3 {
				return fmt.Errorf("wrapped: %w", pgErr(SQLStateSerializationFailure))
			}
			return nil
		})
		require.NoError(t, err)
		assert.Equal(t, 3, calls)
		require.Len(t, rs.delays, 2)
		assert.GreaterOrEqual(t, rs.delays[0], 2500*time.Microsecond)
		assert.Less(t, rs.delays[0], 5*time.Millisecond)
		assert.GreaterOrEqual(t, rs.delays[1], 5*time.Millisecond)
		assert.Less(t, rs.delays[1], 10*time.Millisecond)
	})

	t.Run("retries deadlocks", func(t *testing.T) {
		t.Parallel()
		rs := &recordingSleep{}
		calls := 0
		err := runWithRetry(context.Background(), 2, rs.sleep, func(context.Context) error {
			calls++
			if calls == 1 {
				return pgErr(SQLStateDeadlockDetected)
			}
			return nil
		})
		require.NoError(t, err)
		assert.Equal(t, 2, calls)
	})

	t.Run("never retries a non-retryable error", func(t *testing.T) {
		t.Parallel()
		rs := &recordingSleep{}
		calls := 0
		boom := errors.New("boom")
		err := runWithRetry(context.Background(), 5, rs.sleep, func(context.Context) error { calls++; return boom })
		require.ErrorIs(t, err, boom)
		assert.Same(t, boom, err, "non-retryable errors are returned unwrapped")
		assert.Equal(t, 1, calls)
		assert.Empty(t, rs.delays)
	})

	t.Run("never retries a unique violation", func(t *testing.T) {
		t.Parallel()
		rs := &recordingSleep{}
		calls := 0
		err := runWithRetry(context.Background(), 5, rs.sleep, func(context.Context) error {
			calls++
			return pgErr(SQLStateUniqueViolation)
		})
		require.Error(t, err)
		assert.True(t, IsUniqueViolation(err))
		assert.False(t, errors.Is(err, ErrRetriesExhausted))
		assert.Equal(t, 1, calls)
	})

	t.Run("zero budget runs once and returns the raw error", func(t *testing.T) {
		t.Parallel()
		rs := &recordingSleep{}
		calls := 0
		err := runWithRetry(context.Background(), 0, rs.sleep, func(context.Context) error {
			calls++
			return pgErr(SQLStateSerializationFailure)
		})
		require.Error(t, err)
		assert.True(t, IsSerializationFailure(err))
		assert.False(t, errors.Is(err, ErrRetriesExhausted), "no retries were possible, so nothing was exhausted")
		assert.Equal(t, 1, calls)
		assert.Empty(t, rs.delays)
	})

	t.Run("exhausts the budget and reports it", func(t *testing.T) {
		t.Parallel()
		rs := &recordingSleep{}
		calls := 0
		err := runWithRetry(context.Background(), 3, rs.sleep, func(context.Context) error {
			calls++
			return pgErr(SQLStateSerializationFailure)
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrRetriesExhausted)
		assert.True(t, IsSerializationFailure(err), "underlying SQLSTATE stays visible")
		assert.True(t, IsRetryable(err))
		assert.Equal(t, 4, calls, "first attempt plus three retries")
		assert.Len(t, rs.delays, 3)
	})

	t.Run("cancelled context stops before the next attempt", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		err := runWithRetry(ctx, 5, sleepWithContext, func(context.Context) error {
			calls++
			cancel()
			return pgErr(SQLStateSerializationFailure)
		})
		require.ErrorIs(t, err, context.Canceled)
		assert.True(t, IsSerializationFailure(err), "last error is attached")
		assert.Equal(t, 1, calls)
	})

	t.Run("already cancelled context does not run op", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		err := runWithRetry(ctx, 5, sleepWithContext, func(context.Context) error { calls++; return nil })
		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, 0, calls)
	})
}

func TestSQLStateHelpers(t *testing.T) {
	t.Parallel()
	assert.True(t, IsUniqueViolation(fmt.Errorf("x: %w", pgErr(SQLStateUniqueViolation))))
	assert.True(t, IsForeignKeyViolation(pgErr(SQLStateForeignKeyViolation)))
	assert.True(t, IsCheckViolation(pgErr(SQLStateCheckViolation)))
	assert.True(t, IsSerializationFailure(pgErr(SQLStateSerializationFailure)))
	assert.True(t, IsDeadlock(pgErr(SQLStateDeadlockDetected)))
	assert.True(t, IsInsufficientPrivilege(pgErr(SQLStateInsufficientPrivilege)))
	assert.True(t, IsStatementTimeout(pgErr(SQLStateQueryCanceled)))
	assert.True(t, IsLockTimeout(pgErr(SQLStateLockNotAvailable)))
	assert.False(t, IsUniqueViolation(errors.New("nope")))
	assert.False(t, IsUniqueViolation(nil))
	assert.Equal(t, "", SQLState(errors.New("nope")))
	assert.Equal(t, "", ConstraintName(nil))
	assert.Equal(t, "outbox_events_pkey", ConstraintName(&pgconn.PgError{Code: SQLStateUniqueViolation, ConstraintName: "outbox_events_pkey"}))
	assert.True(t, IsImmutableRow(&pgconn.PgError{Code: SQLStateRaiseException, Message: "immutable row: UPDATE on public.journal_entries is forbidden"}))
	assert.False(t, IsImmutableRow(&pgconn.PgError{Code: SQLStateRaiseException, Message: "something else"}))
	assert.False(t, IsImmutableRow(errors.New("immutable row")))
}
