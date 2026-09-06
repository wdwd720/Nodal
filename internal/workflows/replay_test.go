package workflows_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/worker"

	"github.com/nodal/controlplane/internal/workflows"
)

// TestReplay_RecordedHistories replays histories captured from a real
// Temporal server against the current workflow code.
//
// This is the test that catches the failure mode nobody notices until it is
// expensive. A workflow in flight is resumed by replaying its history through
// today's code; if the code emits a different sequence of commands than the
// history records, Temporal reports a non-determinism error and the execution
// is stuck — after the deploy, on somebody's money, with no way to roll
// forward. A change that would do that fails here instead, before it ships.
//
// The histories in testdata are recorded by
// TestIntegration_Workflow_ReplayFromServer (build tag integration) against
// the local Temporal server with CP_WORKFLOW_RECORD_HISTORY=1. They are
// checked in so this tier needs no server.
func TestReplay_RecordedHistories(t *testing.T) {
	t.Parallel()
	histories, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	require.NoError(t, err)
	require.NotEmpty(t, histories,
		"no recorded histories in testdata; record them with `CP_WORKFLOW_RECORD_HISTORY=1 go test -tags=integration -run TestIntegration_Workflow_ReplayFromServer ./internal/workflows/`")

	for _, path := range histories {
		t.Run(filepath.Base(path), func(t *testing.T) {
			t.Parallel()
			info, err := os.Stat(path)
			require.NoError(t, err)
			require.Positive(t, info.Size(), "the recorded history is empty")

			r := worker.NewWorkflowReplayer()
			workflows.RegisterWorkflows(r)
			require.NoError(t, r.ReplayWorkflowHistoryFromJSONFile(nil, path),
				"the workflow no longer replays its own recorded history: a determinism regression")
		})
	}
}

// TestReplay_RegistrationIsStable pins the names a history refers to. A
// recorded history names its workflow and every activity by string; renaming
// one is not a refactor, it is a migration.
func TestReplay_RegistrationIsStable(t *testing.T) {
	t.Parallel()
	require.Equal(t, []string{
		"controlplane.funding.deposit.v1",
		"controlplane.reconciliation.escalation.v1",
	}, workflows.WorkflowNames())
	require.Equal(t, []string{
		"controlplane.funding.AdvanceDeposit",
		"controlplane.funding.DescribeDeposit",
		"controlplane.funding.EscalateDeposit",
		"controlplane.reconciliation.DescribeRecord",
		"controlplane.reconciliation.MarkInvestigating",
		"controlplane.reconciliation.MarkEscalated",
		"controlplane.reconciliation.NotifyEscalation",
		"controlplane.reconciliation.RequestContainment",
	}, workflows.ActivityNames())
}
