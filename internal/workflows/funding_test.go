package workflows_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/workflows"
)

func fundingResult(t *testing.T, env *testsuite.TestWorkflowEnvironment) workflows.FundingResult {
	t.Helper()
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var out workflows.FundingResult
	require.NoError(t, env.GetWorkflowResult(&out))
	return out
}

// The plainest case, driven end to end through the real activities over the
// fake funding driver: the workflow keeps advancing the deposit until the
// database says it is terminal.
func TestFundingWorkflow_RunsUntilTheDepositIsTerminal(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	w.driver.script("dep-1",
		workflows.DepositState{Status: "SESSION_CREATED"},
		workflows.DepositState{Status: "PROVIDER_CONFIRMED"},
		workflows.DepositState{Status: "SETTLEMENT_OBSERVED"},
		workflows.DepositState{Status: "AVAILABLE", Terminal: true, WithdrawalEligible: true},
	)
	env := newEnv(t, w)

	env.ExecuteWorkflow(workflows.FundingWorkflowName, workflows.FundingInput{
		DepositID: "dep-1", PollInterval: time.Minute,
	})

	out := fundingResult(t, env)
	assert.Equal(t, "AVAILABLE", out.Status)
	assert.True(t, out.Terminal)
	assert.Equal(t, 4, out.Polls)
	assert.Equal(t, 4, w.driver.advanceCount("dep-1"), "one Advance per poll, no more")
	assert.False(t, out.Escalated)
	assert.False(t, out.TimedOut)
	assert.Empty(t, w.alerter.all(), "a deposit that moves never wakes an operator")
}

// A deposit that does not move must reach an operator exactly once, and the
// workflow must keep driving it afterwards rather than giving up.
func TestFundingWorkflow_EscalatesAStalledDepositOnce(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	env := newEnv(t, w)

	var polls int
	env.OnActivity(workflows.AdvanceDepositName, mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ workflows.AdvanceDepositInput) (workflows.DepositState, error) {
			polls++
			if polls >= 12 {
				return workflows.DepositState{Status: "AVAILABLE", Terminal: true}, nil
			}
			return workflows.DepositState{Status: "PROVIDER_PROCESSING"}, nil
		})

	env.ExecuteWorkflow(workflows.FundingWorkflowName, workflows.FundingInput{
		DepositID: "dep-stuck", PollInterval: time.Minute, ReviewAfter: 5 * time.Minute,
	})

	out := fundingResult(t, env)
	assert.True(t, out.Terminal, "the workflow kept driving the deposit after escalating")
	assert.True(t, out.Escalated)
	alerts := w.alerter.all()
	require.Len(t, alerts, 1, "an operator is told once, not once per poll")
	assert.Equal(t, "FUNDING_DEPOSIT_STALLED", alerts[0].Kind)
	assert.Equal(t, "dep-stuck", alerts[0].ResourceID)
	assert.Equal(t, workflows.SeverityWarning, alerts[0].Severity)
}

// A provider webhook wakes the workflow before its next scheduled poll.
func TestFundingWorkflow_SignalWakesItEarly(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	w.driver.script("dep-signal",
		workflows.DepositState{Status: "CUSTOMER_ACTION_REQUIRED"},
		workflows.DepositState{Status: "AVAILABLE", Terminal: true},
	)
	env := newEnv(t, w)

	// The poll interval is an hour; the signal arrives after a second. If the
	// signal were ignored the workflow would still be waiting.
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(workflows.DepositUpdatedSignal, nil)
	}, time.Second)

	env.ExecuteWorkflow(workflows.FundingWorkflowName, workflows.FundingInput{
		DepositID: "dep-signal", PollInterval: time.Hour,
	})
	out := fundingResult(t, env)
	assert.True(t, out.Terminal)
	assert.Equal(t, 2, out.Polls)
}

// Several signals arriving together must not turn into several extra polls.
func TestFundingWorkflow_BurstOfSignalsIsCollapsed(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	env := newEnv(t, w)

	var polls int
	env.OnActivity(workflows.AdvanceDepositName, mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ workflows.AdvanceDepositInput) (workflows.DepositState, error) {
			polls++
			if polls >= 2 {
				return workflows.DepositState{Status: "FAILED", Terminal: true}, nil
			}
			return workflows.DepositState{Status: "PROVIDER_PROCESSING"}, nil
		})
	env.RegisterDelayedCallback(func() {
		for range 5 {
			env.SignalWorkflow(workflows.DepositUpdatedSignal, nil)
		}
	}, time.Second)

	env.ExecuteWorkflow(workflows.FundingWorkflowName, workflows.FundingInput{
		DepositID: "dep-burst", PollInterval: time.Hour,
	})
	out := fundingResult(t, env)
	assert.Equal(t, 2, out.Polls, "five signals produced one extra poll, not five")
	assert.Equal(t, "FAILED", out.Status)
	assert.True(t, out.Terminal, "FAILED is a terminal deposit status and is reported, not judged")
}

// The deadline stops the workflow without pronouncing on the deposit.
func TestFundingWorkflow_DeadlineStopsWithoutAVerdict(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	env := newEnv(t, w)

	env.OnActivity(workflows.AdvanceDepositName, mock.Anything, mock.Anything).
		Return(workflows.DepositState{Status: "PROVIDER_PROCESSING"}, nil)

	env.ExecuteWorkflow(workflows.FundingWorkflowName, workflows.FundingInput{
		DepositID: "dep-slow", PollInterval: time.Minute, ReviewAfter: time.Hour, Deadline: 10 * time.Minute,
	})

	out := fundingResult(t, env)
	assert.True(t, out.TimedOut)
	assert.False(t, out.Terminal, "a deadline is not a terminal deposit status")
	assert.Equal(t, "PROVIDER_PROCESSING", out.Status, "the workflow reports what the row said, nothing more")
}

// A long-lived deposit must not grow an unbounded history.
func TestFundingWorkflow_ContinuesAsNewBeforeTheHistoryGrows(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	env := newEnv(t, w)

	env.OnActivity(workflows.AdvanceDepositName, mock.Anything, mock.Anything).
		Return(workflows.DepositState{Status: "PROVIDER_PROCESSING"}, nil)

	env.ExecuteWorkflow(workflows.FundingWorkflowName, workflows.FundingInput{
		DepositID: "dep-long", PollInterval: time.Second, ReviewAfter: 24 * time.Hour, Deadline: 24 * time.Hour,
	})
	require.True(t, env.IsWorkflowCompleted())
	err := env.GetWorkflowError()
	require.Error(t, err)
	var canErr *workflow.ContinueAsNewError
	require.ErrorAs(t, err, &canErr, "the workflow continued as new rather than growing its history")
}

func TestFundingWorkflow_RefusesAnEmptyDepositID(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	env := newEnv(t, w)
	env.ExecuteWorkflow(workflows.FundingWorkflowName, workflows.FundingInput{})
	require.True(t, env.IsWorkflowCompleted())
	err := env.GetWorkflowError()
	require.Error(t, err)
	var appErr *temporal.ApplicationError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, string(errs.CodeValidationFailed), appErr.Type())
	assert.True(t, appErr.NonRetryable(), "a missing id will still be missing on a retry")
}

// An unknown deposit is NOT_FOUND, which the retry policy must treat as
// final: retrying a lookup for a row that does not exist only wastes time.
func TestFundingWorkflow_UnknownDepositIsNotRetried(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	env := newEnv(t, w)

	env.ExecuteWorkflow(workflows.FundingWorkflowName, workflows.FundingInput{DepositID: "dep-missing"})
	require.True(t, env.IsWorkflowCompleted())
	err := env.GetWorkflowError()
	require.Error(t, err)
	var appErr *temporal.ApplicationError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, string(errs.CodeNotFound), appErr.Type())
	assert.True(t, appErr.NonRetryable())
	assert.Equal(t, 0, w.driver.advanceCount("dep-missing"))
}
