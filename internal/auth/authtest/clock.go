package authtest

import (
	"sync"
	"time"
)

// Clock is a settable clock. Its Now method value satisfies the
// func() time.Time parameters used across the auth and security packages.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// NewClock returns a clock frozen at t (converted to UTC).
func NewClock(t time.Time) *Clock { return &Clock{t: t.UTC()} }

// Now returns the current fake time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Set moves the clock to t.
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t.UTC()
}

// Advance moves the clock forward (or backward) by d.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}
