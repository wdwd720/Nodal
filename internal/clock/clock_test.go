package clock_test

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/clock"
)

func TestSystem_ReturnsUTC(t *testing.T) {
	t.Parallel()
	clk := clock.System()
	before := time.Now()
	now := clk.Now()
	after := time.Now()

	require.Equal(t, time.UTC, now.Location())
	require.Equal(t, "UTC", now.Location().String())
	require.False(t, now.Before(before), "System().Now() %s before time.Now() %s", now, before)
	require.False(t, now.After(after), "System().Now() %s after time.Now() %s", now, after)

	// The RFC3339 rendering must carry the Z designator, never an offset.
	require.Regexp(t, `Z$`, now.Format(time.RFC3339Nano))
}

func TestSystem_IsStateless(t *testing.T) {
	t.Parallel()
	require.Equal(t, clock.System(), clock.System())
	a := clock.System().Now()
	b := clock.System().Now()
	require.False(t, b.Before(a))
}

func TestFake_NowSetAdvance(t *testing.T) {
	t.Parallel()
	loc := time.FixedZone("plus5", 5*3600)
	start := time.Date(2026, 9, 5, 14, 30, 0, 0, loc)

	f := clock.NewFake(start)
	got := f.Now()
	require.Equal(t, time.UTC, got.Location(), "NewFake normalises to UTC")
	require.True(t, got.Equal(start))
	require.Equal(t, "2026-09-05T09:30:00Z", got.Format(time.RFC3339Nano))

	require.Equal(t, got, f.Now(), "does not move on its own")

	next := f.Advance(90 * time.Second)
	require.Equal(t, start.Add(90*time.Second).UTC(), next)
	require.Equal(t, next, f.Now())
	require.Equal(t, time.UTC, next.Location())

	back := f.Advance(-time.Minute)
	require.Equal(t, start.Add(30*time.Second).UTC(), back)

	later := time.Date(2027, 1, 1, 0, 0, 0, 0, loc)
	f.Set(later)
	require.Equal(t, later.UTC(), f.Now())
	require.Equal(t, time.UTC, f.Now().Location())

	f.Set(start)
	require.True(t, f.Now().Equal(start), "moving backwards is allowed")
}

func TestFake_ZeroValue(t *testing.T) {
	t.Parallel()
	var f clock.Fake
	require.True(t, f.Now().IsZero())
	require.Equal(t, time.UTC, f.Now().Location())
	f.Advance(time.Second)
	require.Equal(t, time.Time{}.Add(time.Second), f.Now())
}

func TestFake_NoMonotonicReading(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(time.Now())
	// Round(0) strips a monotonic reading; equality with == therefore proves
	// there was none to strip.
	now := f.Now()
	require.True(t, now == now.Round(0)) //nolint:staticcheck // QF1009: == is the point; Equal would ignore a monotonic reading
	sys := clock.System().Now()
	require.True(t, sys == sys.Round(0), "System().Now() must not carry a monotonic reading") //nolint:staticcheck // QF1009: == is the point; Equal would ignore a monotonic reading
}

func TestFake_ConcurrentAdvanceAndRead(t *testing.T) {
	t.Parallel()
	const writers, readers, steps = 8, 4, 1000
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f := clock.NewFake(start)

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s := 0; s < steps; s++ {
				f.Advance(time.Millisecond)
			}
		}()
	}
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			prev := f.Now()
			for s := 0; s < steps; s++ {
				cur := f.Now()
				if cur.Before(prev) {
					t.Errorf("fake time went backwards under concurrent Advance: %s -> %s", prev, cur)
				}
				if cur.Location() != time.UTC {
					t.Errorf("non-UTC time %s", cur)
				}
				prev = cur
			}
		}()
	}
	wg.Wait()
	require.Equal(t, start.Add(writers*steps*time.Millisecond), f.Now())
}

func TestFake_ConcurrentSet(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(time.Unix(0, 0))
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			f.Set(time.Unix(int64(i), 0))
			_ = f.Now()
		}(i)
	}
	wg.Wait()
	sec := f.Now().Unix()
	require.GreaterOrEqual(t, sec, int64(0))
	require.Less(t, sec, int64(16))
}

func TestElapsed(t *testing.T) {
	t.Parallel()
	start := time.Now()
	time.Sleep(2 * time.Millisecond)
	d := clock.Elapsed(start)
	require.GreaterOrEqual(t, d, 2*time.Millisecond)
	require.Less(t, d, 10*time.Second)

	// A start in the future (wall clock) yields a negative duration; Elapsed
	// never panics or clamps silently.
	require.Negative(t, clock.Elapsed(time.Now().Add(time.Hour)))
}

func TestStopwatch(t *testing.T) {
	t.Parallel()
	stop := clock.Start()
	first := stop()
	require.GreaterOrEqual(t, first, time.Duration(0))

	time.Sleep(2 * time.Millisecond)
	second := stop()
	require.GreaterOrEqual(t, second, 2*time.Millisecond)
	require.GreaterOrEqual(t, second, first, "stopwatch is monotonic and reusable")
	require.GreaterOrEqual(t, stop.Elapsed(), second)
	require.GreaterOrEqual(t, clock.Stopwatch(stop).Elapsed(), second)
}

func TestStopwatch_ConcurrentReads(t *testing.T) {
	t.Parallel()
	stop := clock.Start()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < 100; k++ {
				if stop() < 0 {
					t.Error("negative elapsed")
				}
			}
		}()
	}
	wg.Wait()
}
