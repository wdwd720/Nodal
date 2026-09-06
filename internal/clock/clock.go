package clock

import "time"

// Clock supplies the current wall-clock time. Implementations must return a
// time whose Location is UTC.
//
// Clock values are for business timestamps. They carry no monotonic clock
// reading; see Elapsed and Start for duration measurement.
type Clock interface {
	Now() time.Time
}

// System returns the real wall clock, normalised to UTC.
func System() Clock {
	return systemClock{}
}

type systemClock struct{}

// Now returns time.Now in UTC. The monotonic reading is intentionally
// dropped by the UTC conversion (see package doc).
func (systemClock) Now() time.Time {
	return time.Now().UTC()
}

// Elapsed returns the duration since start using time.Since semantics: if
// start carries a monotonic clock reading (any value from time.Now or Start)
// the monotonic clock is used and wall-clock adjustments cannot skew the
// result. Values from Clock.Now have no monotonic reading, so Elapsed on them
// degrades to wall-clock arithmetic; prefer Start for in-process timing.
func Elapsed(start time.Time) time.Duration {
	return time.Since(start)
}

// Stopwatch reports the time elapsed since it was started. It is a function
// so the common form is a one-liner:
//
//	stop := clock.Start()
//	defer func() { metrics.Observe(stop()) }()
type Stopwatch func() time.Duration

// Start begins a Stopwatch on the monotonic clock. The returned function may
// be called any number of times and is safe for concurrent use.
func Start() Stopwatch {
	start := time.Now()
	return func() time.Duration {
		return time.Since(start)
	}
}

// Elapsed is the method form of calling the Stopwatch.
func (s Stopwatch) Elapsed() time.Duration {
	return s()
}
