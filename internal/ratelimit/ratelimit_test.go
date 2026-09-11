package ratelimit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

var t0 = time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

func fixedClock(t *time.Time) func() time.Time { return func() time.Time { return *t } }

func TestMemoryLimiter_WindowSemantics(t *testing.T) {
	now := t0
	l, err := NewLimiter("api", NewMemoryStore(), Limit{Requests: 3, Window: time.Minute}, fixedClock(&now), false)
	require.NoError(t, err)
	for i := 1; i <= 3; i++ {
		d, err := l.Allow(context.Background(), "u1")
		require.NoError(t, err)
		assert.True(t, d.Allowed, i)
		assert.Equal(t, 3-i, d.Remaining)
	}
	d, err := l.Allow(context.Background(), "u1")
	require.NoError(t, err)
	assert.False(t, d.Allowed)
	assert.Equal(t, 0, d.Remaining)
	assert.Equal(t, t0.Add(time.Minute), d.ResetAt)

	// Other keys are independent.
	d, err = l.Allow(context.Background(), "u2")
	require.NoError(t, err)
	assert.True(t, d.Allowed)

	// Next window resets.
	now = t0.Add(time.Minute)
	d, err = l.Allow(context.Background(), "u1")
	require.NoError(t, err)
	assert.True(t, d.Allowed)
	assert.Equal(t, 2, d.Remaining)

	store, ok := l.store.(*MemoryStore)
	require.True(t, ok)
	assert.Equal(t, 1, store.Sweep(now, time.Minute), "the expired u2 window is swept")
}

func TestLimiter_Validation(t *testing.T) {
	_, err := NewLimiter("", NewMemoryStore(), Limit{Requests: 1, Window: time.Second}, func() time.Time { return t0 }, false)
	assert.Error(t, err)
	_, err = NewLimiter("x", NewMemoryStore(), Limit{Requests: 0, Window: time.Second}, func() time.Time { return t0 }, false)
	assert.Error(t, err)
}

type failingStore struct{}

func (failingStore) Incr(context.Context, string, time.Duration, time.Time) (int, time.Time, error) {
	return 0, time.Time{}, context.DeadlineExceeded
}

func TestLimiter_FailOpenVsClosed(t *testing.T) {
	open, _ := NewLimiter("api", failingStore{}, Limit{Requests: 1, Window: time.Second}, func() time.Time { return t0 }, true)
	d, err := open.Allow(context.Background(), "k")
	require.NoError(t, err)
	assert.True(t, d.Allowed)
	closed, _ := NewLimiter("api", failingStore{}, Limit{Requests: 1, Window: time.Second}, func() time.Time { return t0 }, false)
	_, err = closed.Allow(context.Background(), "k")
	assert.Error(t, err)
}

func TestMiddleware_HeadersAndProblem(t *testing.T) {
	now := time.Now().UTC()
	l, _ := NewLimiter("auth", NewMemoryStore(), Limit{Requests: 2, Window: time.Minute}, func() time.Time { return now }, false)
	h := Middleware(l, ByRemoteIP)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/auth/login", nil)
		req.RemoteAddr = "198.51.100.7:4321"
		h.ServeHTTP(rec, req)
		assert.Equal(t, 204, rec.Code)
		assert.Equal(t, "2", rec.Header().Get("RateLimit-Limit"))
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/auth/login", nil)
	req.RemoteAddr = "198.51.100.7:4321"
	req.Header.Set("X-Request-Id", "req-9")
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.NotEmpty(t, rec.Header().Get("Retry-After"))
	assert.True(t, strings.HasPrefix(rec.Header().Get("Content-Type"), "application/problem+json"))
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "RATE_LIMITED", body["code"])
	assert.Equal(t, "req-9", body["request_id"])

	// A different IP is unaffected.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/auth/login", nil)
	req.RemoteAddr = "[2001:db8::1]:443"
	h.ServeHTTP(rec, req)
	assert.Equal(t, 204, rec.Code)
}

func TestMemoryStore_Concurrent(t *testing.T) {
	store := NewMemoryStore()
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				_, _, _ = store.Incr(context.Background(), "k", time.Minute, t0)
			}
		}()
	}
	wg.Wait()
	c, _, err := store.Incr(context.Background(), "k", time.Minute, t0)
	require.NoError(t, err)
	assert.Equal(t, 3201, c)
}

// TestProp_NeverExceedsBudgetWithinWindow: for any sequence of calls inside
// one window, at most Requests are allowed.
func TestProp_NeverExceedsBudgetWithinWindow(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		limit := Limit{Requests: rapid.IntRange(1, 50).Draw(rt, "req"), Window: time.Minute}
		now := t0
		l, err := NewLimiter("p", NewMemoryStore(), limit, fixedClock(&now), false)
		if err != nil {
			rt.Fatal(err)
		}
		n := rapid.IntRange(1, 200).Draw(rt, "n")
		allowed := 0
		for i := 0; i < n; i++ {
			now = t0.Add(time.Duration(rapid.IntRange(0, 59).Draw(rt, "sec")) * time.Second)
			d, err := l.Allow(context.Background(), "k")
			if err != nil {
				rt.Fatal(err)
			}
			if d.Allowed {
				allowed++
			}
		}
		if allowed > limit.Requests {
			rt.Fatalf("allowed %d > budget %d", allowed, limit.Requests)
		}
	})
}

// TestParseLimit covers the "<requests>/<window>" form, the disable forms and
// everything that must be refused rather than silently defaulted.
//
// It lives here rather than in cmd/api because the parser does: internal/config
// refuses a deployment whose spec is unparseable or switched off where money is
// at stake, and cmd/api turns the same spec into the budget it enforces. One
// parser, one test.
func TestParseLimit(t *testing.T) {
	t.Parallel()
	def := Limit{Requests: 600, Window: time.Minute}
	cases := []struct {
		spec    string
		limit   Limit
		enabled bool
		wantErr bool
	}{
		{"", def, true, false},
		{"   ", def, true, false},
		{"600/1m", Limit{Requests: 600, Window: time.Minute}, true, false},
		{"50/10s", Limit{Requests: 50, Window: 10 * time.Second}, true, false},
		{" 5000 / 1h ", Limit{Requests: 5000, Window: time.Hour}, true, false},
		{"off", Limit{}, false, false},
		{"OFF", Limit{}, false, false},
		{"0", Limit{}, false, false},
		{"0/1m", Limit{}, false, false},
		{"600", Limit{}, false, true},
		{"abc/1m", Limit{}, false, true},
		{"-1/1m", Limit{}, false, true},
		{"600/nonsense", Limit{}, false, true},
		{"600/0s", Limit{}, false, true},
		{"600/-1m", Limit{}, false, true},
	}
	for _, tc := range cases {
		t.Run("spec="+tc.spec, func(t *testing.T) {
			t.Parallel()
			limit, enabled, err := ParseLimit(tc.spec, def)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.enabled, enabled)
			assert.Equal(t, tc.limit, limit)
		})
	}
}

// The store holds a bounded number of counters (F-169).
//
// "Bounded memory: expired windows are dropped lazily on access and by Sweep"
// held for neither half of a key that is never revisited: the lazy drop
// replaces the entry for a key that COMES BACK, and nothing in the process
// called Sweep. So the map retained one entry per distinct (limiter, key) pair
// for the life of the process, which on a 512 MB instance is an allocation the
// callers choose the size of.
func TestMemoryStore_StopsGrowingAtItsCap(t *testing.T) {
	t.Parallel()
	now := t0
	store := NewMemoryStoreWithMax(100)

	for i := 0; i < 10_000; i++ {
		_, _, err := store.Incr(context.Background(), fmt.Sprintf("auth:ip:198.51.100.%d", i), time.Minute, now)
		require.NoError(t, err)
	}

	stats := store.Stats(now)
	assert.LessOrEqual(t, stats.Keys, 100, "the store held %d counters for a cap of 100", stats.Keys)
	assert.Equal(t, 1, stats.Overflows)
	assert.True(t, stats.Saturated, "the window that ran out of counters is still in force")

	// A saturated window still counts, and counts everybody together: the
	// limit is degraded, never absent.
	first, _, err := store.Incr(context.Background(), "auth:ip:203.0.113.1", time.Minute, now)
	require.NoError(t, err)
	second, _, err := store.Incr(context.Background(), "auth:ip:203.0.113.2", time.Minute, now)
	require.NoError(t, err)
	assert.Greater(t, second, first, "a saturated window admitted a caller without counting it")

	// The next window starts clean and per-key again.
	next := now.Add(time.Minute)
	a, _, err := store.Incr(context.Background(), "auth:ip:203.0.113.1", time.Minute, next)
	require.NoError(t, err)
	b, _, err := store.Incr(context.Background(), "auth:ip:203.0.113.2", time.Minute, next)
	require.NoError(t, err)
	assert.Equal(t, 1, a)
	assert.Equal(t, 1, b, "two callers shared a counter after the saturated window ended")
	assert.False(t, store.Stats(next).Saturated)
}

// TestMemoryStore_SweepUsesEachEntrysOwnWindow.
//
// One store serves every limiter, and the limits do not share a window. A
// sweeper ticking at the LARGEST window must not remove a shorter window that
// is still open, and must not keep one that has ended -- so each entry is swept
// by the window it was written with, and the argument is only a fallback.
func TestMemoryStore_SweepUsesEachEntrysOwnWindow(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := context.Background()

	_, _, err := store.Incr(ctx, "auth:ip:a", time.Minute, t0)
	require.NoError(t, err)
	_, _, err = store.Incr(ctx, "slow:ip:a", 10*time.Minute, t0)
	require.NoError(t, err)

	// Two minutes on: the minute window is dead and the ten-minute one is open,
	// swept with the largest window as the argument.
	assert.Equal(t, 1, store.Sweep(t0.Add(2*time.Minute), 10*time.Minute),
		"the ended one-minute window survived a ten-minute sweep")
	assert.Equal(t, 1, store.Stats(t0.Add(2*time.Minute)).Keys)

	// And the open one is still counting where it left off.
	n, _, err := store.Incr(ctx, "slow:ip:a", 10*time.Minute, t0.Add(2*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 2, n, "an open window was swept away and restarted")

	assert.Equal(t, 1, store.Sweep(t0.Add(11*time.Minute), 10*time.Minute))
	assert.Zero(t, store.Stats(t0.Add(11*time.Minute)).Keys)
}
