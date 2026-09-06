package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/nodal/controlplane/internal/errs"
)

// Limit is a fixed-window budget: at most Requests per Window.
type Limit struct {
	Requests int
	Window   time.Duration
}

// Validate checks the limit.
func (l Limit) Validate() error {
	if l.Requests <= 0 || l.Window <= 0 {
		return errors.New("ratelimit: requests and window must be positive")
	}
	return nil
}

// Decision is the outcome of one Allow call.
type Decision struct {
	Allowed   bool
	Remaining int
	ResetAt   time.Time
}

// Store counts requests per key within fixed windows.
type Store interface {
	// Incr increments the counter for key in the window containing now and
	// returns the new count and when the window resets.
	Incr(ctx context.Context, key string, window time.Duration, now time.Time) (count int, resetAt time.Time, err error)
}

// Limiter applies a Limit through a Store.
type Limiter struct {
	store Store
	limit Limit
	name  string
	now   func() time.Time
	// failOpen makes store errors count as allowed (transport protection is
	// best-effort; money paths have their own deterministic limits).
	failOpen bool
}

// NewLimiter returns a Limiter.
func NewLimiter(name string, store Store, limit Limit, now func() time.Time, failOpen bool) (*Limiter, error) {
	if name == "" || store == nil || now == nil {
		return nil, errors.New("ratelimit: name, store and clock required")
	}
	if err := limit.Validate(); err != nil {
		return nil, err
	}
	return &Limiter{store: store, limit: limit, name: name, now: now, failOpen: failOpen}, nil
}

// Allow records one request for key and decides.
func (l *Limiter) Allow(ctx context.Context, key string) (Decision, error) {
	now := l.now()
	count, resetAt, err := l.store.Incr(ctx, l.name+":"+key, l.limit.Window, now)
	if err != nil {
		if l.failOpen {
			return Decision{Allowed: true, Remaining: l.limit.Requests, ResetAt: now.Add(l.limit.Window)}, nil
		}
		return Decision{}, fmt.Errorf("ratelimit: %w", err)
	}
	remaining := l.limit.Requests - count
	if remaining < 0 {
		remaining = 0
	}
	return Decision{Allowed: count <= l.limit.Requests, Remaining: remaining, ResetAt: resetAt}, nil
}

// windowStart aligns now to the window boundary.
func windowStart(now time.Time, window time.Duration) time.Time {
	return now.Truncate(window)
}

// MemoryStore is a process-local Store with bounded memory: expired windows
// are dropped lazily on access and by Sweep.
type MemoryStore struct {
	mu   sync.Mutex
	data map[string]*memWindow
}

type memWindow struct {
	start time.Time
	count int
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore { return &MemoryStore{data: map[string]*memWindow{}} }

// Incr implements Store.
func (m *MemoryStore) Incr(_ context.Context, key string, window time.Duration, now time.Time) (int, time.Time, error) {
	start := windowStart(now, window)
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.data[key]
	if !ok || !w.start.Equal(start) {
		w = &memWindow{start: start}
		m.data[key] = w
	}
	w.count++
	return w.count, start.Add(window), nil
}

// Sweep removes windows that ended before now.
func (m *MemoryStore) Sweep(now time.Time, window time.Duration) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k, w := range m.data {
		if !w.start.Add(window).After(now) {
			delete(m.data, k)
			n++
		}
	}
	return n
}

// RedisStore is a Store over a shared Redis; counters expire with their window.
type RedisStore struct {
	client redis.Cmdable
	prefix string
}

// NewRedisStore returns a RedisStore.
func NewRedisStore(client redis.Cmdable, prefix string) *RedisStore {
	if prefix == "" {
		prefix = "cp:rl:"
	}
	return &RedisStore{client: client, prefix: prefix}
}

// incrScript increments a window key and sets its expiry exactly once, atomically.
var incrScript = redis.NewScript(`
local c = redis.call('INCR', KEYS[1])
if c == 1 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
return c
`)

// Incr implements Store.
func (r *RedisStore) Incr(ctx context.Context, key string, window time.Duration, now time.Time) (int, time.Time, error) {
	start := windowStart(now, window)
	resetAt := start.Add(window)
	redisKey := r.prefix + key + ":" + strconv.FormatInt(start.UnixMilli(), 10)
	ttl := resetAt.Sub(now) + time.Second
	res, err := incrScript.Run(ctx, r.client, []string{redisKey}, ttl.Milliseconds()).Int64()
	if err != nil {
		return 0, time.Time{}, err
	}
	return int(res), resetAt, nil
}

// KeyFunc derives the limiter key from a request (subject id, IP, ...).
type KeyFunc func(r *http.Request) string

// Middleware enforces the limiter on every request, adding RateLimit headers
// and answering 429 problem+json with Retry-After when exhausted.
func Middleware(l *Limiter, keyOf KeyFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := keyOf(r)
			if key == "" {
				next.ServeHTTP(w, r)
				return
			}
			d, err := l.Allow(r.Context(), key)
			if err != nil {
				errs.WriteProblem(w, errs.ToProblem(errs.Wrap(err, errs.CodeInternal, "rate limiter unavailable"), r.URL.Path, requestID(r)))
				return
			}
			w.Header().Set("RateLimit-Limit", strconv.Itoa(l.limit.Requests))
			w.Header().Set("RateLimit-Remaining", strconv.Itoa(d.Remaining))
			w.Header().Set("RateLimit-Reset", strconv.FormatInt(int64(time.Until(d.ResetAt).Seconds())+1, 10))
			if !d.Allowed {
				retry := time.Until(d.ResetAt)
				if retry < time.Second {
					retry = time.Second
				}
				w.Header().Set("Retry-After", strconv.FormatInt(int64(retry.Seconds()), 10))
				e := errs.New(errs.CodeRateLimited, "too many requests").WithRetryAfter(retry)
				errs.WriteProblem(w, errs.ToProblem(e, r.URL.Path, requestID(r)))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func requestID(r *http.Request) string { return r.Header.Get("X-Request-Id") }

// ByRemoteIP keys by the client IP (after trusted proxy handling upstream).
func ByRemoteIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := lastColon(host); i > 0 {
		host = host[:i]
	}
	return "ip:" + host
}

func lastColon(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ':' {
			return i
		}
	}
	return -1
}
