package workflows_test

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/workflows"
)

// newActivityEnv registers the real activities so they can be invoked by name
// exactly as a worker would.
func newActivityEnv(t *testing.T, w *world) *testsuite.TestActivityEnvironment {
	t.Helper()
	var s testsuite.WorkflowTestSuite
	env := s.NewTestActivityEnvironment()
	if w.deps.Funding != nil {
		env.RegisterActivityWithOptions(w.deps.Funding.AdvanceDeposit, activity.RegisterOptions{Name: workflows.AdvanceDepositName})
		env.RegisterActivityWithOptions(w.deps.Funding.DescribeDeposit, activity.RegisterOptions{Name: workflows.DescribeDepositName})
		env.RegisterActivityWithOptions(w.deps.Funding.EscalateDeposit, activity.RegisterOptions{Name: workflows.EscalateDepositName})
	}
	if w.deps.Escalation != nil {
		env.RegisterActivityWithOptions(w.deps.Escalation.DescribeRecord, activity.RegisterOptions{Name: workflows.DescribeRecordName})
		env.RegisterActivityWithOptions(w.deps.Escalation.MarkInvestigating, activity.RegisterOptions{Name: workflows.MarkInvestigatingName})
		env.RegisterActivityWithOptions(w.deps.Escalation.MarkEscalated, activity.RegisterOptions{Name: workflows.MarkEscalatedName})
		env.RegisterActivityWithOptions(w.deps.Escalation.NotifyEscalation, activity.RegisterOptions{Name: workflows.NotifyEscalationName})
		env.RegisterActivityWithOptions(w.deps.Escalation.RequestContainment, activity.RegisterOptions{Name: workflows.RequestContainmentName})
	}
	return env
}

func TestNewFundingActivities_RequiresDependencies(t *testing.T) {
	t.Parallel()
	_, err := workflows.NewFundingActivities(nil, &fakeAlerter{}, nil)
	require.Error(t, err)
	_, err = workflows.NewFundingActivities(newFakeDriver(), nil, nil)
	require.Error(t, err)
}

func TestNewEscalationActivities_RequiresDependencies(t *testing.T) {
	t.Parallel()
	_, err := workflows.NewEscalationActivities(nil, &fakeAlerter{}, nil, nil)
	require.Error(t, err)
	_, err = workflows.NewEscalationActivities(newFakeRecords(), nil, nil, nil)
	require.Error(t, err)
}

// The activity advances and then re-reads, so what the workflow learns is
// what the database says and never what the step intended.
func TestActivity_AdvanceDeposit_ReportsTheStoredState(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	w.driver.script("dep-a",
		workflows.DepositState{Status: "SESSION_CREATED"},
		workflows.DepositState{Status: "AVAILABLE", Terminal: true},
	)
	env := newActivityEnv(t, w)

	val, err := env.ExecuteActivity(workflows.AdvanceDepositName, workflows.AdvanceDepositInput{DepositID: "dep-a"})
	require.NoError(t, err)
	var out workflows.DepositState
	require.NoError(t, val.Get(&out))
	assert.Equal(t, "SESSION_CREATED", out.Status)
	assert.Equal(t, "dep-a", out.DepositID)

	val, err = env.ExecuteActivity(workflows.AdvanceDepositName, workflows.AdvanceDepositInput{DepositID: "dep-a"})
	require.NoError(t, err)
	require.NoError(t, val.Get(&out))
	assert.Equal(t, "AVAILABLE", out.Status)
	assert.True(t, out.Terminal)
	assert.Equal(t, 2, w.driver.advanceCount("dep-a"))
}

// DescribeDeposit must not advance anything: a workflow that only wants to
// look must be able to.
func TestActivity_DescribeDeposit_DoesNotAdvance(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	w.driver.script("dep-b", workflows.DepositState{Status: "PROVIDER_PROCESSING"})
	env := newActivityEnv(t, w)

	val, err := env.ExecuteActivity(workflows.DescribeDepositName, workflows.AdvanceDepositInput{DepositID: "dep-b"})
	require.NoError(t, err)
	var out workflows.DepositState
	require.NoError(t, val.Get(&out))
	assert.Equal(t, "PROVIDER_PROCESSING", out.Status)
	assert.Zero(t, w.driver.advanceCount("dep-b"))
}

func TestActivity_RejectsMissingIdentifiers(t *testing.T) {
	t.Parallel()
	w := newWorld(t, &fakeContainment{})
	env := newActivityEnv(t, w)

	cases := []struct {
		name  string
		call  func() error
		field string
	}{
		{"advance", func() error {
			_, err := env.ExecuteActivity(workflows.AdvanceDepositName, workflows.AdvanceDepositInput{})
			return err
		}, "deposit"},
		{"describe deposit", func() error {
			_, err := env.ExecuteActivity(workflows.DescribeDepositName, workflows.AdvanceDepositInput{})
			return err
		}, "deposit"},
		{"escalate deposit", func() error {
			_, err := env.ExecuteActivity(workflows.EscalateDepositName, workflows.EscalateDepositInput{})
			return err
		}, "deposit"},
		{"describe record", func() error {
			_, err := env.ExecuteActivity(workflows.DescribeRecordName, workflows.DescribeRecordInput{})
			return err
		}, "record"},
		{"mark investigating", func() error {
			_, err := env.ExecuteActivity(workflows.MarkInvestigatingName, workflows.MarkRecordInput{})
			return err
		}, "record"},
		{"mark escalated", func() error {
			_, err := env.ExecuteActivity(workflows.MarkEscalatedName, workflows.MarkRecordInput{})
			return err
		}, "record"},
		{"notify", func() error {
			_, err := env.ExecuteActivity(workflows.NotifyEscalationName, workflows.EscalationNotice{})
			return err
		}, "record"},
		{"contain", func() error {
			_, err := env.ExecuteActivity(workflows.RequestContainmentName, workflows.ContainmentRequest{})
			return err
		}, "record"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			require.Error(t, err)
			var appErr *temporal.ApplicationError
			require.ErrorAs(t, err, &appErr)
			assert.Equal(t, string(errs.CodeValidationFailed), appErr.Type())
			assert.True(t, appErr.NonRetryable(), "a missing id will still be missing on a retry")
		})
	}
}

func TestActivity_EscalateDeposit_AlertsAnOperator(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	env := newActivityEnv(t, w)

	_, err := env.ExecuteActivity(workflows.EscalateDepositName, workflows.EscalateDepositInput{
		DepositID: "dep-c", Status: "PROVIDER_PROCESSING", StuckFor: 3 * time.Hour, CorrelationID: "corr-1",
	})
	require.NoError(t, err)
	alerts := w.alerter.all()
	require.Len(t, alerts, 1)
	assert.Equal(t, "FUNDING_DEPOSIT_STALLED", alerts[0].Kind)
	assert.Equal(t, workflows.SeverityWarning, alerts[0].Severity)
	assert.Equal(t, "deposit", alerts[0].ResourceType)
	assert.Equal(t, "corr-1", alerts[0].CorrelationID)
	assert.Contains(t, alerts[0].Detail, "PROVIDER_PROCESSING")
}

// Containment without a configured controller must fail, loudly and with a
// code that says "this build cannot do that" rather than a generic error.
func TestActivity_RequestContainment_FailsClosedWithoutAController(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	env := newActivityEnv(t, w)

	_, err := env.ExecuteActivity(workflows.RequestContainmentName, workflows.ContainmentRequest{RecordID: "rec-x"})
	require.Error(t, err)
	var appErr *temporal.ApplicationError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, string(errs.CodeUnsupported), appErr.Type())
}

func TestActivity_RequestContainment_ReturnsTheReference(t *testing.T) {
	t.Parallel()
	containment := &fakeContainment{}
	w := newWorld(t, containment)
	env := newActivityEnv(t, w)

	val, err := env.ExecuteActivity(workflows.RequestContainmentName, workflows.ContainmentRequest{
		RecordID: "rec-y", AccountID: "acct-y", Kind: "WALLET_BALANCE", Reason: "unresolved",
	})
	require.NoError(t, err)
	var ref string
	require.NoError(t, val.Get(&ref))
	assert.Equal(t, "kill-switch:GLOBAL_NEW_RISK_KILL:*", ref)
	require.Len(t, containment.all(), 1)
}

// TestActivityError_MapsCodesToRetryability is the contract between the
// domain errors and Temporal's retry policy: a code that will give the same
// answer next time must never be retried, and everything else must be.
func TestActivityError_MapsCodesToRetryability(t *testing.T) {
	t.Parallel()
	assert.NoError(t, workflows.ActivityError(nil))

	final := []errs.Code{
		errs.CodeValidationFailed, errs.CodeNotFound, errs.CodeForbidden,
		errs.CodeUnauthenticated, errs.CodeInvalidStateTransition, errs.CodeUnsupported,
	}
	for _, code := range final {
		err := workflows.ActivityError(errs.New(code, "detail"))
		var appErr *temporal.ApplicationError
		require.ErrorAs(t, err, &appErr, "%s", code)
		assert.Equal(t, string(code), appErr.Type())
		assert.True(t, appErr.NonRetryable(), "%s must not be retried", code)
	}

	retryable := []errs.Code{errs.CodeInternal, errs.CodeProviderUnavailable, errs.CodeConflict, errs.CodeRateLimited}
	for _, code := range retryable {
		err := workflows.ActivityError(errs.New(code, "detail"))
		var appErr *temporal.ApplicationError
		require.ErrorAs(t, err, &appErr, "%s", code)
		assert.Equal(t, string(code), appErr.Type())
		assert.False(t, appErr.NonRetryable(), "%s must be retried", code)
	}

	// A plain error is passed through untouched, so nothing is silently
	// reclassified as retryable or not.
	plain := errors.New("boom")
	assert.Equal(t, plain, workflows.ActivityError(plain))
}

// The detail carried into a Temporal history must be the client-safe detail,
// never the wrapped cause: histories live outside Postgres and are visible in
// the Temporal UI.
func TestActivityError_CarriesNoWrappedCause(t *testing.T) {
	t.Parallel()
	cause := errors.New("dial tcp 10.0.0.5:5432: connection refused")
	err := workflows.ActivityError(errs.Wrap(cause, errs.CodeInternal, "workflows: could not read the deposit"))
	var appErr *temporal.ApplicationError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, "workflows: could not read the deposit", appErr.Message())
	assert.NotContains(t, appErr.Error(), "10.0.0.5")
}

func TestTaskQueue(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "control-plane", workflows.TaskQueue(""))
	assert.Equal(t, "cp-local-control-plane", workflows.TaskQueue("cp-local"))
}

func TestWorkflowIDsAreDeterministic(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "funding-deposit:abc", workflows.FundingWorkflowID("abc"))
	assert.Equal(t, workflows.FundingWorkflowID("abc"), workflows.FundingWorkflowID("abc"))
	assert.Equal(t, "reconciliation-escalation:xyz", workflows.EscalationWorkflowID("xyz"))
	assert.NotEqual(t, workflows.FundingWorkflowID("abc"), workflows.EscalationWorkflowID("abc"))
}

func TestRegister_RequiresSomethingToRegister(t *testing.T) {
	t.Parallel()
	require.Error(t, workflows.Register(nil, workflows.Deps{}))

	w := newWorld(t, nil)
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	require.Error(t, workflows.Register(env, workflows.Deps{}), "a worker with no activities registers nothing")
	require.NoError(t, workflows.Register(env, w.deps))
}
