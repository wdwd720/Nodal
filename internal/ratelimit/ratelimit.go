package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/nodal/controlplane/internal/errs"
)

// ParseLimit reads a budget written as "<requests>/<window>", e.g. "600/1m" or
// "50/10s". "off" (or a zero request count) disables the limiter and returns
// enabled=false. An empty spec takes def.
//
// It lives here rather than in the binary that reads the variable because two
// things need the same answer: internal/config, which refuses a deployment
// whose spec is unparseable or switched off where money is at stake, and
// cmd/api, which turns the spec into the Limit it enforces. Two parsers would
// eventually disagree, and the one that disagrees quietly is the one that
// admits more requests than anybody declared.
func ParseLimit(spec string, def Limit) (Limit, bool, error) {
	spec = strings.TrimSpace(spec)
	switch {
	case spec == "":
		return def, true, nil
	case strings.EqualFold(spec, "off"), spec == "0":
		return Limit{}, false, nil
	}
	count, window, found := strings.Cut(spec, "/")
	if !found {
		return Limit{}, false, fmt.Errorf("rate limit %q must be \"<requests>/<window>\", e.g. 600/1m", spec)
	}
	requests, err := strconv.Atoi(strings.TrimSpace(count))
	if err != nil || requests < 0 {
		return Limit{}, false, fmt.Errorf("rate limit %q has a non-numeric request count", spec)
	}
	if requests == 0 {
		return Limit{}, false, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(window))
	if err != nil || d <= 0 {
		return Limit{}, false, fmt.Errorf("rate limit %q has an invalid window", spec)
	}
	limit := Limit{Requests: requests, Window: d}
	if verr := limit.Validate(); verr != nil {
		return Limit{}, false, fmt.Errorf("rate limit %q: %w", spec, verr)
	}
	return limit, true, nil
}

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

// DefaultMaxKeys is how many distinct counters one MemoryStore holds before it
// stops counting them one at a time.
//
// Ten thousand entries is a few hundred kilobytes -- a key, a start and a
// count -- on an instance with 512 MB, and it is far more distinct callers than
// the deployments that use this backend have: config.Validate refuses the
// memory backend outside a single-process environment, and since F-166 a
// caller can no longer choose its own key by writing a header, so the
// cardinality is real source addresses.
const DefaultMaxKeys = 10_000

// MemoryStore is a process-local Store whose memory is bounded two ways:
// expired windows are dropped on access and by Sweep, and the number of
// distinct counters it holds is capped.
//
// # Why the cap exists (F-169)
//
// "Expired windows are dropped lazily on access" was true only of a key that
// COMES BACK: the drop happens when the same key is seen in a later window.
// A key seen once and never again was retained for the life of the process, and
// nothing anywhere called Sweep -- so the map grew with the number of distinct
// callers ever seen, which is not a number this process chooses.
//
// # What happens when the cap is reached
//
// Every counter is dropped and the window is marked SATURATED: for the rest of
// that window every request, whatever its key, is counted against one shared
// budget, and the next window starts clean and per-key again.
//
// It is a deliberate degradation and not a nice one, so it is worth saying
// plainly what it costs. During a saturated window callers share a budget, so a
// legitimate caller can be refused for traffic that is not theirs -- the F-88
// failure, in miniature and for at most one window. The alternatives are worse:
// evicting the oldest entries keeps the limiter exact for whoever remains and
// hands the attacker a way to evict the counter that is watching them, and
// admitting new keys without counting them removes the limit entirely at the
// moment it is being tested. A shared budget still refuses a flood, still
// recovers by itself, and cannot grow.
type MemoryStore struct {
	mu   sync.Mutex
	max  int
	data map[string]*memWindow
	// Set while a window is saturated: the instant the saturated window ends,
	// and the shared counters for it.
	satUntil  time.Time
	sat       map[satKey]int
	overflows int
}

type memWindow struct {
	start  time.Time
	window time.Duration
	count  int
}

// satKey identifies one shared counter while the store is saturated. There is
// one per (window start, window length) in flight, which is one per configured
// limit -- four, in this binary -- and not one per caller.
type satKey struct {
	start  time.Time
	window time.Duration
}

// NewMemoryStore returns an empty MemoryStore holding at most DefaultMaxKeys
// counters.
func NewMemoryStore() *MemoryStore { return NewMemoryStoreWithMax(DefaultMaxKeys) }

// NewMemoryStoreWithMax returns an empty MemoryStore holding at most max
// counters. A max below one takes DefaultMaxKeys, so a caller cannot configure
// a store that saturates on its first request.
func NewMemoryStoreWithMax(max int) *MemoryStore {
	if max < 1 {
		max = DefaultMaxKeys
	}
	return &MemoryStore{max: max, data: map[string]*memWindow{}}
}

// Incr implements Store.
func (m *MemoryStore) Incr(_ context.Context, key string, window time.Duration, now time.Time) (int, time.Time, error) {
	start := windowStart(now, window)
	resetAt := start.Add(window)
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.saturated(now) {
		k := satKey{start: start, window: window}
		m.sat[k]++
		return m.sat[k], resetAt, nil
	}

	if w, ok := m.data[key]; ok {
		if w.start.Equal(start) {
			w.count++
			return w.count, resetAt, nil
		}
		// The same key in a later window: its old window is dropped here,
		// which is the lazy half of the bound and only ever helps a key that
		// comes back.
		w.start, w.window, w.count = start, window, 1
		return 1, resetAt, nil
	}

	if len(m.data) >= m.max {
		// One window has more distinct keys than this store will hold. Drop
		// them and count the rest of the window together; see the type's
		// comment for why that is the least bad of the three options.
		m.data = make(map[string]*memWindow)
		m.satUntil = resetAt
		m.sat = map[satKey]int{{start: start, window: window}: 1}
		m.overflows++
		return 1, resetAt, nil
	}

	m.data[key] = &memWindow{start: start, window: window, count: 1}
	return 1, resetAt, nil
}

// saturated reports whether the store is counting into shared buckets at now,
// and clears the saturation when its window has ended.
func (m *MemoryStore) saturated(now time.Time) bool {
	if m.satUntil.IsZero() {
		return false
	}
	if now.Before(m.satUntil) {
		return true
	}
	m.satUntil, m.sat = time.Time{}, nil
	return false
}

// Sweep removes windows that ended at or before now and returns how many it
// removed. `window` is a fallback for an entry written before the store
// recorded each window's own length; an entry that knows its own window is
// swept by that, so a sweeper ticking at the LARGEST configured window never
// removes a shorter window that is still open, and never keeps one that is not.
func (m *MemoryStore) Sweep(now time.Time, window time.Duration) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k, w := range m.data {
		d := w.window
		if d <= 0 {
			d = window
		}
		if !w.start.Add(d).After(now) {
			delete(m.data, k)
			n++
		}
	}
	m.saturated(now)
	return n
}

// MemoryStats is what one MemoryStore is holding, for the sweeper's log. A
// store that is saturating is a deployment whose caller cardinality has passed
// what a process-local limiter can count exactly, and the operator wants to
// know that from a log line rather than from a support ticket.
type MemoryStats struct {
	// Keys is how many counters are held right now.
	Keys int
	// Max is the cap.
	Max int
	// Saturated says whether a shared budget is in force at this instant.
	Saturated bool
	// Overflows is how many windows have saturated since the process started.
	Overflows int
}

// Stats reports what the store is holding at now.
func (m *MemoryStore) Stats(now time.Time) MemoryStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return MemoryStats{
		Keys:      len(m.data),
		Max:       m.max,
		Saturated: !m.satUntil.IsZero() && now.Before(m.satUntil),
		Overflows: m.overflows,
	}
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
