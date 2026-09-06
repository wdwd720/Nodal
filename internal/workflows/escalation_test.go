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

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/workflows"
)

func escalationResult(t *testing.T, env *testsuite.TestWorkflowEnvironment) workflows.EscalationResult {
	t.Helper()
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var out workflows.EscalationResult
	require.NoError(t, env.GetWorkflowResult(&out))
	return out
}

func shortTiers() []workflows.EscalationTier {
	return []workflows.EscalationTier{
		{Name: "OPERATIONS", Severity: workflows.SeverityWarning, Wait: 10 * time.Minute},
		{Name: "FINANCE_RISK", Severity: workflows.SeverityCritical, Wait: 20 * time.Minute},
	}
}

// A record already resolved when the workflow starts costs nobody a page.
func TestEscalationWorkflow_AlreadyResolvedEndsImmediately(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	w.records.put(workflows.RecordState{RecordID: "rec-done", Kind: "EXECUTION", Status: "MATCHED", Resolved: true})
	env := newEnv(t, w)

	env.ExecuteWorkflow(workflows.EscalationWorkflowName, workflows.EscalationInput{RecordID: "rec-done"})
	out := escalationResult(t, env)
	assert.True(t, out.Resolved)
	assert.False(t, out.Escalated)
	assert.Zero(t, out.TiersNotified)
	assert.Empty(t, w.alerter.all(), "nobody is notified about a matched record")
	assert.Empty(t, w.records.investigating, "a resolved record is not reopened as INVESTIGATING")
}

// The ladder runs to the end when nobody resolves the record.
func TestEscalationWorkflow_WalksTheLadderThenMarksEscalated(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	w.records.put(workflows.RecordState{
		RecordID: "rec-open", Kind: "WALLET_BALANCE", Status: "MISMATCH", AccountID: "acct-1",
	})
	env := newEnv(t, w)

	env.ExecuteWorkflow(workflows.EscalationWorkflowName, workflows.EscalationInput{
		RecordID: "rec-open", Tiers: shortTiers(), PollInterval: 2 * time.Minute,
	})

	out := escalationResult(t, env)
	assert.False(t, out.Resolved)
	assert.True(t, out.Escalated)
	assert.Equal(t, 2, out.TiersNotified)
	assert.Empty(t, out.ContainmentRef, "an immaterial record never triggers containment")

	alerts := w.alerter.all()
	require.Len(t, alerts, 2)
	assert.Equal(t, "RECONCILIATION_ESCALATION", alerts[0].Kind)
	assert.Equal(t, workflows.SeverityWarning, alerts[0].Severity)
	assert.Equal(t, workflows.SeverityCritical, alerts[1].Severity, "severity rises with the tier")
	assert.Equal(t, []string{"rec-open"}, w.records.investigating)
	assert.Equal(t, []string{"rec-open"}, w.records.escalated)
}

// Resolution at any point ends the ladder without waking the next tier.
func TestEscalationWorkflow_ResolutionStopsTheLadder(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	w.records.put(workflows.RecordState{RecordID: "rec-fix", Kind: "EXECUTION", Status: "MISMATCH"})
	env := newEnv(t, w)

	// An operator resolves it while the first tier is still waiting.
	env.RegisterDelayedCallback(func() {
		w.records.put(workflows.RecordState{
			RecordID: "rec-fix", Kind: "EXECUTION", Status: "RESOLVED_MANUAL", Resolved: true,
		})
		env.SignalWorkflow(workflows.RecordResolvedSignal, nil)
	}, 3*time.Minute)

	env.ExecuteWorkflow(workflows.EscalationWorkflowName, workflows.EscalationInput{
		RecordID: "rec-fix", Tiers: shortTiers(), PollInterval: time.Minute,
	})

	out := escalationResult(t, env)
	assert.True(t, out.Resolved)
	assert.False(t, out.Escalated)
	assert.Equal(t, 1, out.TiersNotified, "the second tier was never woken")
	assert.Equal(t, "RESOLVED_MANUAL", out.Status)
	assert.Empty(t, w.records.escalated)
}

// The record can also be resolved by polling alone, with no signal at all:
// an automatic rule closing it must be noticed.
func TestEscalationWorkflow_NoticesResolutionWithoutASignal(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	w.records.put(workflows.RecordState{RecordID: "rec-auto", Kind: "FUNDING", Status: "MISMATCH"})
	env := newEnv(t, w)

	env.RegisterDelayedCallback(func() {
		w.records.put(workflows.RecordState{
			RecordID: "rec-auto", Kind: "FUNDING", Status: "RESOLVED_AUTOMATIC", Resolved: true,
		})
	}, 3*time.Minute)

	env.ExecuteWorkflow(workflows.EscalationWorkflowName, workflows.EscalationInput{
		RecordID: "rec-auto", Tiers: shortTiers(), PollInterval: time.Minute,
	})

	out := escalationResult(t, env)
	assert.True(t, out.Resolved)
	assert.Equal(t, "RESOLVED_AUTOMATIC", out.Status)
	assert.Equal(t, 1, out.TiersNotified)
}

// A material, risk-blocking mismatch that nobody resolves ends in containment
// — and containment stops NEW RISK only.
func TestEscalationWorkflow_MaterialUnresolvedMismatchTriggersContainment(t *testing.T) {
	t.Parallel()
	containment := &fakeContainment{}
	w := newWorld(t, containment)
	w.records.put(workflows.RecordState{
		RecordID: "rec-bad", Kind: "POSITION_LEDGER", Status: "MISMATCH",
		Material: true, BlocksNewRisk: true, AccountID: "acct-7",
	})
	env := newEnv(t, w)

	env.ExecuteWorkflow(workflows.EscalationWorkflowName, workflows.EscalationInput{
		RecordID: "rec-bad", Tiers: shortTiers(), PollInterval: 5 * time.Minute,
	})

	out := escalationResult(t, env)
	assert.True(t, out.Escalated)
	assert.Equal(t, "kill-switch:GLOBAL_NEW_RISK_KILL:*", out.ContainmentRef)
	reqs := containment.all()
	require.Len(t, reqs, 1)
	assert.Equal(t, "rec-bad", reqs[0].RecordID)
	assert.Equal(t, "acct-7", reqs[0].AccountID)
}

// Kill switches halt trading. They never halt reconciliation. After
// containment is raised, the workflow must still be reading the record.
func TestEscalationWorkflow_ContainmentDoesNotStopReconciliation(t *testing.T) {
	t.Parallel()
	containment := &fakeContainment{}
	w := newWorld(t, containment)
	w.records.put(workflows.RecordState{
		RecordID: "rec-contained", Kind: "WALLET_BALANCE", Status: "MISMATCH",
		Material: true, BlocksNewRisk: true, AccountID: "acct-9",
	})
	env := newEnv(t, w)

	var describesBefore int
	env.OnActivity(workflows.DescribeRecordName, mock.Anything, mock.Anything).
		Return(func(_ context.Context, in workflows.DescribeRecordInput) (workflows.RecordState, error) {
			describesBefore++
			return workflows.RecordState{
				RecordID: in.RecordID, Kind: "WALLET_BALANCE", Status: "MISMATCH",
				Material: true, BlocksNewRisk: true, AccountID: "acct-9",
			}, nil
		})
	env.OnActivity(workflows.RequestContainmentName, mock.Anything, mock.Anything).
		Return(func(ctx context.Context, req workflows.ContainmentRequest) (string, error) {
			return containment.Contain(ctx, req)
		})

	env.ExecuteWorkflow(workflows.EscalationWorkflowName, workflows.EscalationInput{
		RecordID: "rec-contained", Tiers: shortTiers(), PollInterval: 5 * time.Minute,
	})
	out := escalationResult(t, env)
	require.True(t, out.Escalated)
	require.NotEmpty(t, out.ContainmentRef)
	assert.Positive(t, describesBefore, "the record was read while the ladder ran")

	// Now the crucial half: with containment in force, a fresh escalation of
	// the same record still runs. Reconciliation is never gated on a switch.
	env2 := newEnv(t, w)
	env2.OnActivity(workflows.DescribeRecordName, mock.Anything, mock.Anything).
		Return(workflows.RecordState{
			RecordID: "rec-contained", Kind: "WALLET_BALANCE", Status: "ESCALATED",
			Material: true, BlocksNewRisk: true, AccountID: "acct-9",
		}, nil)
	env2.OnActivity(workflows.RequestContainmentName, mock.Anything, mock.Anything).
		Return("kill-switch:GLOBAL_NEW_RISK_KILL:*", nil)
	env2.ExecuteWorkflow(workflows.EscalationWorkflowName, workflows.EscalationInput{
		RecordID: "rec-contained", Tiers: shortTiers()[:1], PollInterval: 5 * time.Minute,
	})
	out2 := escalationResult(t, env2)
	assert.True(t, out2.Escalated, "reconciliation keeps working while trading is halted")
	assert.Equal(t, 1, out2.TiersNotified)
}

// A containment request with no controller configured must fail loudly. A
// silent no-op would tell an operator that trading stopped when it did not.
func TestEscalationWorkflow_ContainmentWithoutAControllerFails(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil) // no containment controller
	w.records.put(workflows.RecordState{
		RecordID: "rec-nocontain", Kind: "LEDGER_INTERNAL", Status: "MISMATCH",
		Material: true, BlocksNewRisk: true,
	})
	env := newEnv(t, w)

	env.ExecuteWorkflow(workflows.EscalationWorkflowName, workflows.EscalationInput{
		RecordID: "rec-nocontain", Tiers: shortTiers()[:1], PollInterval: 5 * time.Minute,
	})
	require.True(t, env.IsWorkflowCompleted())
	err := env.GetWorkflowError()
	require.Error(t, err)
	var appErr *temporal.ApplicationError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, string(errs.CodeUnsupported), appErr.Type())

	// The record was still marked ESCALATED before containment was attempted,
	// so the failure does not hide the escalation itself.
	assert.Equal(t, []string{"rec-nocontain"}, w.records.escalated)
}

func TestEscalationWorkflow_RefusesAnEmptyRecordID(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	env := newEnv(t, w)
	env.ExecuteWorkflow(workflows.EscalationWorkflowName, workflows.EscalationInput{})
	require.True(t, env.IsWorkflowCompleted())
	err := env.GetWorkflowError()
	require.Error(t, err)
	var appErr *temporal.ApplicationError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, string(errs.CodeValidationFailed), appErr.Type())
	assert.True(t, appErr.NonRetryable())
}

func TestEscalationWorkflow_DefaultTiersAreStableAndCopied(t *testing.T) {
	t.Parallel()
	a := workflows.DefaultEscalationTiers()
	b := workflows.DefaultEscalationTiers()
	assert.Equal(t, a, b)
	a[0].Name = "MUTATED"
	assert.NotEqual(t, a[0].Name, workflows.DefaultEscalationTiers()[0].Name,
		"the defaults are a fresh slice: no caller can change what a replay sees")
	assert.Len(t, b, 3)
	for i := 1; i < len(b); i++ {
		assert.Greater(t, b[i].Wait, b[i-1].Wait, "each rung waits longer than the last")
	}
}
