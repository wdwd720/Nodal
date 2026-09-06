package model_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/model"
	"github.com/nodal/controlplane/internal/model/modeltest"
	"github.com/nodal/controlplane/internal/money"
)

func usd(minor int64) money.USD { return money.USDFromMinor(minor) }

// TestBudget_ExhaustionStopsCallsBeforeDialing is the property that matters
// (PART 197): the refusal happens before the provider is contacted, so an
// exhausted budget costs nothing and a looping caller cannot run up a bill.
func TestBudget_ExhaustionStopsCallsBeforeDialing(t *testing.T) {
	t.Parallel()

	t.Run("call count exhausted", func(t *testing.T) {
		b := model.Budget{Kind: model.BudgetModel, MaxCalls: 2, MaxSpend: usd(10_00), CallsUsed: 2}
		err := b.CheckBeforeCall(usd(1))
		require.Error(t, err)
		assert.Equal(t, errs.CodeBudgetExhausted, errs.CodeOf(err))
		assert.True(t, b.Exhausted())
		assert.Equal(t, 0, b.RemainingCalls())
	})

	t.Run("spend exhausted", func(t *testing.T) {
		b := model.Budget{Kind: model.BudgetModel, MaxCalls: 10, MaxSpend: usd(5_00), SpendUsed: usd(5_00)}
		err := b.CheckBeforeCall(usd(1))
		require.Error(t, err)
		assert.Equal(t, errs.CodeBudgetExhausted, errs.CodeOf(err))
		assert.True(t, b.Exhausted())
		assert.Equal(t, int64(0), b.RemainingSpend().Minor())
	})

	t.Run("next call would exceed the remainder", func(t *testing.T) {
		b := model.Budget{Kind: model.BudgetModel, MaxCalls: 10, MaxSpend: usd(1_00), SpendUsed: usd(90)}
		require.NoError(t, b.CheckBeforeCall(usd(10)), "exactly the remainder is allowed")
		err := b.CheckBeforeCall(usd(11))
		require.Error(t, err, "one cent over must be refused before the call")
		assert.Equal(t, errs.CodeBudgetExhausted, errs.CodeOf(err))
	})

	t.Run("overspend is never possible by rounding", func(t *testing.T) {
		b := model.Budget{Kind: model.BudgetModel, MaxCalls: 1, MaxSpend: usd(0)}
		require.Error(t, b.CheckBeforeCall(usd(0)), "a zero allowance permits no call at all")
	})
}

func TestBudget_Consume(t *testing.T) {
	t.Parallel()
	b := model.NewBudget(model.BudgetModel, 3, usd(1_00))
	require.NoError(t, b.CheckBeforeCall(usd(10)))

	b, err := b.Consume(usd(30))
	require.NoError(t, err)
	assert.Equal(t, 1, b.CallsUsed)
	assert.Equal(t, int64(70), b.RemainingSpend().Minor())
	assert.Equal(t, 2, b.RemainingCalls())

	b, err = b.Consume(usd(70))
	require.NoError(t, err)
	assert.True(t, b.Exhausted(), "spending the remainder exhausts the budget")
	require.Error(t, b.CheckBeforeCall(usd(1)))
}

// TestBudget_DrivesARealCallSequence: a caller that honors the budget makes
// exactly the permitted number of dials and no more.
func TestBudget_DrivesARealCallSequence(t *testing.T) {
	t.Parallel()
	fake := modeltest.MustNew(config.EnvTest,
		modeltest.Turn{Body: `{"ok":true}`},
		modeltest.Turn{Body: `{"ok":true}`},
		modeltest.Turn{Body: `{"ok":true}`},
	)
	budget := model.NewBudget(model.BudgetModel, 2, usd(10_00))

	var made int
	for i := 0; i < 5; i++ {
		if err := budget.CheckBeforeCall(usd(1)); err != nil {
			break
		}
		resp, err := fake.Complete(context.Background(), baseRequest())
		require.NoError(t, err)
		made++
		budget, err = budget.Consume(resp.Usage.Cost)
		require.NoError(t, err)
	}
	assert.Equal(t, 2, made, "the loop stopped at the call cap")
	assert.Equal(t, 2, fake.Calls(), "and the provider saw exactly two dials")
}

func TestBudget_Accessors(t *testing.T) {
	t.Parallel()
	zero := model.Budget{Kind: model.BudgetData}
	assert.True(t, zero.Exhausted(), "a zero budget permits nothing")
	assert.Equal(t, 0, zero.RemainingCalls())
	assert.Equal(t, int64(0), zero.RemainingSpend().Minor())

	over := model.Budget{Kind: model.BudgetData, MaxCalls: 1, MaxSpend: usd(100), SpendUsed: usd(500)}
	assert.Equal(t, int64(0), over.RemainingSpend().Minor(), "remaining spend never goes negative")
	assert.True(t, over.Exhausted())
}
