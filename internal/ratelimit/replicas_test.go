package ratelimit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file are about a rate limit under more than one replica,
// which is the only condition under which the choice of store matters. Under
// one process every store behaves the same and the question does not arise.

// replica is one API task: its own Limiter, over whichever store that task
// would have built.
func replica(t *testing.T, store Store, now func() time.Time) *Limiter {
	t.Helper()
	l, err := NewLimiter("general", store, Limit{Requests: 10, Window: time.Minute}, now, false)
	require.NoError(t, err)
	return l
}

// TestReplicas_ProcessLocalCountersMultiplyTheBudget is the defect, stated as a
// measurement rather than as an opinion.
//
// Three API tasks each keeping their own counters do not enforce a limit of 10.
// They enforce 10 each, so the caller gets 30 -- and the deployment's
// autoscaling maximum decides how much more than the configured number is
// actually allowed. This is what config.Validate refuses in STAGING and PROD.
func TestReplicas_ProcessLocalCountersMultiplyTheBudget(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	clk := fixedClock(&now)
	ctx := context.Background()

	// Three tasks, three MemoryStores, because a MemoryStore is per process by
	// construction -- there is no way for one task to see another's.
	var tasks []*Limiter
	for range 3 {
		tasks = append(tasks, replica(t, NewMemoryStore(), clk))
	}

	allowed := 0
	for i := range 60 {
		d, err := tasks[i%len(tasks)].Allow(ctx, "subject-1")
		require.NoError(t, err)
		if d.Allowed {
			allowed++
		}
	}
	assert.Equal(t, 30, allowed,
		"three process-local stores admit three times the configured limit; this is the reason for the redis backend")
}

// TestReplicas_OneSharedStoreEnforcesOneBudget is the fix, measured the same
// way. The store is shared; the limiters are not, because each replica builds
// its own.
func TestReplicas_OneSharedStoreEnforcesOneBudget(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	clk := fixedClock(&now)
	ctx := context.Background()

	shared := NewMemoryStore() // stands in for Redis: one set of counters, many readers.
	var tasks []*Limiter
	for range 3 {
		tasks = append(tasks, replica(t, shared, clk))
	}

	allowed := 0
	for i := range 60 {
		d, err := tasks[i%len(tasks)].Allow(ctx, "subject-1")
		require.NoError(t, err)
		if d.Allowed {
			allowed++
		}
	}
	assert.Equal(t, 10, allowed, "one budget, however many replicas spend it")

	// And the budget is per subject, not global: a second caller is unaffected
	// by the first having exhausted theirs.
	d, err := tasks[0].Allow(ctx, "subject-2")
	require.NoError(t, err)
	assert.True(t, d.Allowed)
}

// countingStore records how often it was asked, so a test can prove that a
// limiter kept using it rather than quietly switching to something else.
type countingStore struct {
	calls atomic.Int64
	err   error
}

func (c *countingStore) Incr(context.Context, string, time.Duration, time.Time) (int, time.Time, error) {
	c.calls.Add(1)
	if c.err != nil {
		return 0, time.Time{}, c.err
	}
	return 1, time.Now().Add(time.Minute), nil
}

// TestReplicas_ASharedStoreOutageDoesNotBecomeAProcessLocalLimit.
//
// The failure mode worth naming is not "Redis went down". It is "Redis went
// down and the limiter carried on looking like it worked", because a limiter
// that has silently become per-process reports the same numbers as one that
// has not. There is no fallback path in Limiter at all -- the store it was
// given is the store it uses -- and this test holds that open: every request
// still goes to the failed store, and each one is refused.
func TestReplicas_ASharedStoreOutageDoesNotBecomeAProcessLocalLimit(t *testing.T) {
	t.Parallel()
	down := &countingStore{err: errors.New("dial tcp: connection refused")}
	l, err := NewLimiter("general", down, Limit{Requests: 10, Window: time.Minute}, time.Now, false)
	require.NoError(t, err)

	for range 25 {
		d, err := l.Allow(context.Background(), "subject-1")
		require.Error(t, err, "a failed shared store must not answer as if it had counted")
		assert.False(t, d.Allowed)
	}
	assert.Equal(t, int64(25), down.calls.Load(),
		"every request went to the shared store; nothing fell back to memory")

	// And the transport says so, rather than serving the request. 500 rather
	// than 429: the limit is not exceeded, it is unknown.
	rec := httptest.NewRecorder()
	served := false
	Middleware(l, ByRemoteIP)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		served = true
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/quotes", nil))
	assert.False(t, served, "the handler must not run when the limiter cannot decide")
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// TestReplicas_FailOpenIsStillAvailableForTheProcessLocalStore: the two
// backends differ in what a store error means, and both meanings have to keep
// working. A MemoryStore cannot fail, so fail-open there is a statement about
// an impossible case; the flag is still honoured, and the test says which
// behaviour goes with which choice.
func TestReplicas_FailOpenIsStillAvailableForTheProcessLocalStore(t *testing.T) {
	t.Parallel()
	down := &countingStore{err: errors.New("unreachable")}

	open, err := NewLimiter("general", down, Limit{Requests: 10, Window: time.Minute}, time.Now, true)
	require.NoError(t, err)
	d, err := open.Allow(context.Background(), "subject-1")
	require.NoError(t, err)
	assert.True(t, d.Allowed, "fail-open admits the request")

	closed, err := NewLimiter("general", down, Limit{Requests: 10, Window: time.Minute}, time.Now, false)
	require.NoError(t, err)
	_, err = closed.Allow(context.Background(), "subject-1")
	require.Error(t, err, "fail-closed refuses to guess")
}
