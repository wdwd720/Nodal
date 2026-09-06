// Package clock is the single source of wall-clock time for the control
// plane, and the only place production code may read it.
//
// # Responsibility
//
//   - Clock is the injectable interface every component takes instead of
//     calling time.Now directly. System returns the real clock; Fake is the
//     test double. Both always return UTC (goal PART 16).
//   - Elapsed and Start/Stopwatch measure durations with the monotonic clock
//     reading that time.Now attaches, so they are immune to wall-clock jumps.
//
// # Semantics worth knowing
//
// Values returned by Clock.Now carry no monotonic reading: converting to UTC
// strips it, and a Fake has none to begin with. Use them for business
// timestamps (occurred_at, received_at, ...) that are persisted and compared
// as instants. For measuring how long something took inside a process, use
// Start or Elapsed with a time.Now-derived start, never the difference of two
// Clock.Now values.
//
// # Never
//
// This package must never read configuration, touch I/O, or offer a way to
// change the system clock. Business code must never call time.Now directly
// for anything that is persisted or compared; inject a Clock.
package clock
