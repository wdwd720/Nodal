package clock

import (
	"sync"
	"time"
)

// Fake is a Clock under test control. It only moves when told to, so tests
// are deterministic. All methods are safe for concurrent use; times are
// normalised to UTC on the way in and never carry a monotonic reading.
//
// The zero Fake reports the zero time; use NewFake to start at a known
// instant.
type Fake struct {
	mu  sync.Mutex
	now time.Time
}

// Compile-time check that *Fake satisfies Clock.
var _ Clock = (*Fake)(nil)

// NewFake returns a Fake that reports t (in UTC) until moved.
func NewFake(t time.Time) *Fake {
	return &Fake{now: t.UTC()}
}

// Now returns the current fake time in UTC.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Set moves the clock to t (in UTC). Moving backwards is allowed so tests can
// simulate wall-clock corrections.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = t.UTC()
}

// Advance moves the clock by d, which may be negative, and returns the new
// time. Concurrent Advance calls are serialized, so the total displacement is
// the sum of all d.
func (f *Fake) Advance(d time.Duration) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	return f.now
}
