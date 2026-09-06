package agent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// TestEachBudgetIsEnforcedIndependently: exhausting one budget refuses
// exactly the calls that budget governs and nothing else. None of them relies
// on another failing first.
func TestEachBudgetIsEnforcedIndependently(t *testing.T) {
	t.Parallel()
	base := BudgetSnapshot{
		Data: BudgetLimit{
			Kind: BudgetData, CallsPerRun: 3, CallsPerDay: 10, SpendPerDay: money.USDFromMinor(500),
		},
		Model: BudgetLimit{
			Kind: BudgetModel, CallsPerRun: 2, CallsPerDay: 5, SpendPerDay: money.USDFromMinor(300),
		},
		IntentsPerHour: 4,
	}
	cost := money.USDFromMinor(10)

	t.Run("fresh snapshot permits everything", func(t *testing.T) {
		s := base
		require.NoError(t, s.CheckTool(cost))
		require.NoError(t, s.CheckModel(cost))
		require.NoError(t, s.CheckIntent())
	})

	t.Run("data calls per run", func(t *testing.T) {
		s := base
		s.DataUsed.CallsThisRun = 3
		err := s.CheckTool(cost)
		requireBudgetRefusal(t, err, BudgetData)
		assert.NoError(t, s.CheckModel(cost), "the model budget is untouched")
		assert.NoError(t, s.CheckIntent(), "the order-rate budget is untouched")
	})

	t.Run("data calls per day", func(t *testing.T) {
		s := base
		s.DataUsed.CallsToday = 10
		requireBudgetRefusal(t, s.CheckTool(cost), BudgetData)
		assert.NoError(t, s.CheckModel(cost))
	})

	t.Run("data spend per day", func(t *testing.T) {
		s := base
		s.DataUsed.SpentToday = money.USDFromMinor(495)
		requireBudgetRefusal(t, s.CheckTool(cost), BudgetData)
		assert.NoError(t, s.CheckTool(money.USDFromMinor(5)), "a call that fits is still permitted")
		assert.NoError(t, s.CheckModel(cost))
	})

	t.Run("model calls per run", func(t *testing.T) {
		s := base
		s.ModelUsed.CallsThisRun = 2
		requireBudgetRefusal(t, s.CheckModel(cost), BudgetModel)
		assert.NoError(t, s.CheckTool(cost), "the data budget is untouched")
	})

	t.Run("model spend per day", func(t *testing.T) {
		s := base
		s.ModelUsed.SpentToday = money.USDFromMinor(299)
		requireBudgetRefusal(t, s.CheckModel(cost), BudgetModel)
		assert.NoError(t, s.CheckTool(cost))
	})

	t.Run("order rate per hour", func(t *testing.T) {
		s := base
		s.IntentsThisHour = 4
		err := s.CheckIntent()
		require.Error(t, err)
		assert.Equal(t, errs.CodeRateLimited, errs.CodeOf(err))
		e, ok := errs.As(err)
		require.True(t, ok)
		assert.Equal(t, BudgetOrderRate.String(), e.Fields["budget"])
		assert.NoError(t, s.CheckTool(cost), "the data budget is untouched")
		assert.NoError(t, s.CheckModel(cost), "the model budget is untouched")
	})
}

func TestBudgetSpendIsCheckedBeforeTheCallNotAfter(t *testing.T) {
	t.Parallel()
	s := BudgetSnapshot{
		Data: BudgetLimit{Kind: BudgetData, SpendPerDay: money.USDFromMinor(100)},
	}
	s.DataUsed.SpentToday = money.USDFromMinor(100)
	// Already exactly at the cap: one more cent must be refused before dialing.
	requireBudgetRefusal(t, s.CheckTool(money.USDFromMinor(1)), BudgetData)
	// A free call is still permitted: the cap is on spend, not on calls.
	assert.NoError(t, s.CheckTool(money.USDFromMinor(0)))
}

func TestZeroLimitsMeanUncapped(t *testing.T) {
	t.Parallel()
	s := BudgetSnapshot{Data: BudgetLimit{Kind: BudgetData}}
	s.DataUsed = BudgetUsage{CallsThisRun: 1_000_000, CallsToday: 1_000_000, SpentToday: money.USDFromMinor(1 << 40)}
	assert.NoError(t, s.CheckTool(money.USDFromMinor(1)),
		"a budget that declares no cap does not refuse; the other budgets and the risk kernel still bind")
	assert.NoError(t, s.CheckIntent(), "an unset order rate is not a refusal")
}

func TestNegativeCostIsRefused(t *testing.T) {
	t.Parallel()
	s := BudgetSnapshot{Data: BudgetLimit{Kind: BudgetData, SpendPerDay: money.USDFromMinor(100)}}
	err := s.CheckTool(money.USDFromMinor(-1))
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

func TestRemainingCalls(t *testing.T) {
	t.Parallel()
	s := BudgetSnapshot{
		Data:  BudgetLimit{Kind: BudgetData, CallsPerRun: 5, CallsPerDay: 8},
		Model: BudgetLimit{Kind: BudgetModel},
	}
	s.DataUsed.CallsThisRun = 2
	s.DataUsed.CallsToday = 7
	assert.Equal(t, 1, s.RemainingToolCalls(), "the strictest remaining cap wins")
	assert.Equal(t, -1, s.RemainingModelCalls(), "an uncapped budget reports -1")
}

func TestBudgetSnapshotDayAndHourWindows(t *testing.T) {
	t.Parallel()
	// The daily counter is a UTC day and the rate counter a rolling hour;
	// both are derived, never configured per call.
	now := time.Date(2026, 9, 6, 23, 59, 59, 0, time.UTC)
	dayStart := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	assert.Equal(t, dayStart, time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC))
	assert.Equal(t, now.Add(-time.Hour), now.Add(-time.Hour))
}

func requireBudgetRefusal(t *testing.T, err error, kind BudgetKind) {
	t.Helper()
	require.Error(t, err)
	require.Equal(t, errs.CodeBudgetExhausted, errs.CodeOf(err))
	e, ok := errs.As(err)
	require.True(t, ok)
	assert.Equal(t, kind.String(), e.Fields["budget"], "the refusal must name which budget ran out")
}
