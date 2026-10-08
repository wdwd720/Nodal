package margin

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/money"
)

func TestInterestFor_IsExactACT365RoundedUp(t *testing.T) {
	day := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	// 1,000,000 units at 8% for a full day: 1,000,000 × 0.08 / 365 = 219.178... → 220.
	got, err := InterestFor(money.QuantityFromInt64(1_000_000), 800, day, day.Add(24*time.Hour))
	require.NoError(t, err)
	requireQ(t, money.QuantityFromInt64(220), got, "one day")
	// 365 days at 10% is exactly 10%.
	got, err = InterestFor(money.QuantityFromInt64(1_000_000), 1_000, day, day.Add(365*24*time.Hour))
	require.NoError(t, err)
	requireQ(t, money.QuantityFromInt64(100_000), got, "one year")
	// A positive principal, rate and period always accrues at least one unit.
	got, err = InterestFor(money.QuantityFromInt64(1), 1, day, day.Add(time.Nanosecond))
	require.NoError(t, err)
	requireQ(t, money.QuantityFromInt64(1), got, "the lender's rounding")
	for name, args := range map[string][3]int64{"no time": {1_000, 800, 0}, "no principal": {0, 800, 1}, "no rate": {1_000, 0, 1}} {
		got, err := InterestFor(money.QuantityFromInt64(args[0]), money.BPS(args[1]), day, day.Add(time.Duration(args[2])*time.Hour))
		require.NoError(t, err)
		assert.True(t, got.IsZero(), name)
	}
}

func TestDayPeriods_SplitAtMidnightAndStub(t *testing.T) {
	open := time.Date(2026, 10, 7, 15, 30, 0, 0, time.UTC)
	until := time.Date(2026, 10, 10, 6, 0, 0, 0, time.UTC)
	ps := DayPeriods(open, until, false, 100)
	require.Len(t, ps, 3, "the opening stub and two full days; the running day is not complete")
	assert.Equal(t, open, ps[0].Start)
	assert.Equal(t, time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC), ps[0].End)
	assert.Equal(t, time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC), ps[2].End)
	for i := 1; i < len(ps); i++ {
		assert.Equal(t, ps[i-1].End, ps[i].Start, "contiguous")
	}
	withStub := DayPeriods(open, until, true, 100)
	require.Len(t, withStub, 4)
	assert.Equal(t, until, withStub[3].End)
	assert.Len(t, DayPeriods(open, until, true, 2), 2, "bounded")
	assert.Empty(t, DayPeriods(until, until, true, 10), "an empty interval accrues nothing")
	// Microsecond truncation: what the database stores is what comes back.
	ns := open.Add(999 * time.Nanosecond)
	assert.Equal(t, open, DayPeriods(ns, until, true, 1)[0].Start)
}
