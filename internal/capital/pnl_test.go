package capital

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

func usd(minor int64) money.USD { return money.USDFromMinor(minor) }

func TestNextUTCMidnight(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	cases := []struct {
		name string
		in   time.Time
		want time.Time
	}{
		{"mid-day", time.Date(2026, 9, 5, 12, 30, 0, 0, time.UTC), time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)},
		{"exactly midnight moves to the next day", time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)},
		{"one nanosecond before midnight", time.Date(2026, 9, 5, 23, 59, 59, 999_999_999, time.UTC), time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)},
		{"month end", time.Date(2026, 2, 28, 10, 0, 0, 0, time.UTC), time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
		{"non-UTC input is normalised", time.Date(2026, 9, 5, 21, 0, 0, 0, ny), time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)}, // 01:00 UTC on the 6th → midnight of the 7th
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nextUTCMidnight(tc.in)
			assert.True(t, got.Equal(tc.want), "got %s want %s", got, tc.want)
			assert.Equal(t, time.UTC, got.Location())
		})
	}
}

func baseEnvelope() Envelope {
	return Envelope{
		ID:               NewEnvelopeID(),
		Status:           EnvelopeActive,
		Allocation:       usd(1_000_000),
		Available:        usd(1_000_000),
		MaxDailyLoss:     usd(50_000),
		MaxDrawdown:      usd(100_000),
		DailyLossResetAt: time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC),
	}
}

func TestApplyRealizedPnL_Arithmetic(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

	t.Run("profit touches only realized_pnl", func(t *testing.T) {
		e, out, err := applyRealizedPnL(baseEnvelope(), usd(10_000), now)
		require.NoError(t, err)
		assert.Equal(t, usd(10_000), e.RealizedPnL)
		assert.True(t, e.RealizedLoss.IsZero())
		assert.True(t, e.DailyLoss.IsZero())
		assert.True(t, e.CurrentDrawdown.IsZero())
		assert.Equal(t, EnvelopeActive, e.Status)
		assert.Equal(t, pnlOutcome{}, out)
	})

	t.Run("loss increments loss, daily loss and drawdown", func(t *testing.T) {
		e, out, err := applyRealizedPnL(baseEnvelope(), usd(-12_345), now)
		require.NoError(t, err)
		assert.Equal(t, usd(-12_345), e.RealizedPnL)
		assert.Equal(t, usd(12_345), e.RealizedLoss)
		assert.Equal(t, usd(12_345), e.DailyLoss)
		assert.Equal(t, usd(12_345), e.CurrentDrawdown)
		assert.Equal(t, EnvelopeActive, e.Status)
		assert.Empty(t, out.LimitHit)
	})

	t.Run("drawdown is measured from the running peak", func(t *testing.T) {
		e := baseEnvelope()
		var err error
		e, _, err = applyRealizedPnL(e, usd(30_000), now)
		require.NoError(t, err)
		e, _, err = applyRealizedPnL(e, usd(-20_000), now)
		require.NoError(t, err)
		assert.Equal(t, usd(10_000), e.RealizedPnL)
		assert.Equal(t, usd(20_000), e.CurrentDrawdown, "peak 300.00 − pnl 100.00")
		assert.Equal(t, usd(20_000), e.RealizedLoss)
		assert.Equal(t, usd(20_000), e.DailyLoss)
		// Recovering above the old peak clears the drawdown and sets a new peak.
		e, _, err = applyRealizedPnL(e, usd(25_000), now)
		require.NoError(t, err)
		assert.Equal(t, usd(35_000), e.RealizedPnL)
		assert.True(t, e.CurrentDrawdown.IsZero())
		e, _, err = applyRealizedPnL(e, usd(-1_000), now)
		require.NoError(t, err)
		assert.Equal(t, usd(1_000), e.CurrentDrawdown, "peak is now 350.00")
	})

	t.Run("daily loss resets at the reset instant and schedules the next UTC midnight", func(t *testing.T) {
		e := baseEnvelope()
		e.DailyLoss = usd(40_000)
		e.RealizedLoss = usd(40_000)
		e.RealizedPnL = usd(-40_000)
		e.CurrentDrawdown = usd(40_000)
		// Not yet due: the counter accumulates.
		got, out, err := applyRealizedPnL(e, usd(-5_000), e.DailyLossResetAt.Add(-time.Second))
		require.NoError(t, err)
		assert.False(t, out.DailyReset)
		assert.Equal(t, usd(45_000), got.DailyLoss)
		// Exactly at the reset instant: counter reset first, then applied.
		at := e.DailyLossResetAt.Add(36 * time.Hour) // 12:00 on the 7th
		got, out, err = applyRealizedPnL(e, usd(-5_000), at)
		require.NoError(t, err)
		assert.True(t, out.DailyReset)
		assert.Equal(t, usd(5_000), got.DailyLoss)
		assert.Equal(t, usd(45_000), got.RealizedLoss, "cumulative loss never resets")
		assert.True(t, got.DailyLossResetAt.Equal(time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)), got.DailyLossResetAt)
	})
}

func TestApplyRealizedPnL_Limits(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

	t.Run("daily loss at the limit exhausts an ACTIVE envelope", func(t *testing.T) {
		e := baseEnvelope()
		e.MaxDailyLoss = usd(10_000)
		got, out, err := applyRealizedPnL(e, usd(-10_000), now)
		require.NoError(t, err)
		assert.Equal(t, EnvelopeExhausted, got.Status)
		assert.Equal(t, limitDailyLoss, out.LimitHit)
		assert.True(t, out.Exhausted)
	})

	t.Run("one cent below the limit stays ACTIVE", func(t *testing.T) {
		e := baseEnvelope()
		e.MaxDailyLoss = usd(10_000)
		got, out, err := applyRealizedPnL(e, usd(-9_999), now)
		require.NoError(t, err)
		assert.Equal(t, EnvelopeActive, got.Status)
		assert.Empty(t, out.LimitHit)
	})

	t.Run("drawdown at the limit exhausts", func(t *testing.T) {
		e := baseEnvelope()
		e.MaxDailyLoss = usd(1_000_000)
		e.MaxDrawdown = usd(15_000)
		var err error
		e, _, err = applyRealizedPnL(e, usd(10_000), now)
		require.NoError(t, err)
		got, out, err := applyRealizedPnL(e, usd(-15_000), now)
		require.NoError(t, err)
		assert.Equal(t, usd(15_000), got.CurrentDrawdown)
		assert.Equal(t, EnvelopeExhausted, got.Status)
		assert.Equal(t, limitDrawdown, out.LimitHit)
	})

	t.Run("zero limit is zero tolerance, never unlimited", func(t *testing.T) {
		e := baseEnvelope()
		e.MaxDailyLoss = usd(0)
		got, out, err := applyRealizedPnL(e, usd(100), now)
		require.NoError(t, err, "profit with a zero limit is fine")
		assert.Equal(t, EnvelopeActive, got.Status)
		assert.Empty(t, out.LimitHit)
		got, out, err = applyRealizedPnL(e, usd(-1), now)
		require.NoError(t, err)
		assert.Equal(t, EnvelopeExhausted, got.Status)
		assert.Equal(t, limitDailyLoss, out.LimitHit)
	})

	t.Run("a PAUSED envelope records the breach but does not change status", func(t *testing.T) {
		e := baseEnvelope()
		e.Status = EnvelopePaused
		e.MaxDailyLoss = usd(100)
		got, out, err := applyRealizedPnL(e, usd(-100), now)
		require.NoError(t, err)
		assert.Equal(t, EnvelopePaused, got.Status)
		assert.Equal(t, limitDailyLoss, out.LimitHit)
		assert.False(t, out.Exhausted)
	})

	t.Run("overflow surfaces as OVERFLOW, never wraps", func(t *testing.T) {
		e := baseEnvelope()
		e.RealizedPnL = money.MaxUSD()
		_, _, err := applyRealizedPnL(e, usd(1), now)
		require.Error(t, err)
		assert.Equal(t, errs.CodeOverflow, errs.CodeOf(err))
		e = baseEnvelope()
		_, _, err = applyRealizedPnL(e, money.MinUSD(), now)
		require.Error(t, err, "negating MinUSD overflows")
		assert.Equal(t, errs.CodeOverflow, errs.CodeOf(err))
	})
}

func TestLimitBreached(t *testing.T) {
	cases := []struct {
		name        string
		daily, maxD int64
		dd, maxDD   int64
		want        string
	}{
		{name: "fresh", daily: 0, maxD: 0, dd: 0, maxDD: 0, want: ""},
		{name: "daily below", daily: 99, maxD: 100, want: ""},
		{name: "daily at", daily: 100, maxD: 100, want: limitDailyLoss},
		{name: "daily above", daily: 101, maxD: 100, want: limitDailyLoss},
		{name: "drawdown at", dd: 50, maxDD: 50, maxD: 1000, want: limitDrawdown},
		{name: "daily wins when both", daily: 100, maxD: 100, dd: 50, maxDD: 50, want: limitDailyLoss},
		{name: "zero tolerance drawdown", dd: 1, maxDD: 0, maxD: 1000, want: limitDrawdown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := Envelope{DailyLoss: usd(tc.daily), MaxDailyLoss: usd(tc.maxD), CurrentDrawdown: usd(tc.dd), MaxDrawdown: usd(tc.maxDD)}
			assert.Equal(t, tc.want, limitBreached(e))
		})
	}
}

func TestMoneyErr(t *testing.T) {
	assert.NoError(t, moneyErr(nil))
	assert.Equal(t, errs.CodeOverflow, errs.CodeOf(moneyErr(money.ErrOverflow)))
	assert.Equal(t, errs.CodePrecisionLoss, errs.CodeOf(moneyErr(money.ErrPrecisionLoss)))
	assert.Equal(t, errs.CodeInternal, errs.CodeOf(moneyErr(money.ErrDivisionByZero)))
}
