//go:build integration

package workflows_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/nodal/controlplane/internal/workflows"
)

// Environment used by the integration tier.
const (
	envHostPort      = "CP_TEMPORAL_HOSTPORT"
	envNamespace     = "CP_TEMPORAL_NAMESPACE"
	envRecordHistory = "CP_WORKFLOW_RECORD_HISTORY"

	defaultHostPort  = "127.0.0.1:7233"
	defaultNamespace = "default"
)

func dialTemporal(t *testing.T) client.Client {
	t.Helper()
	hostPort := os.Getenv(envHostPort)
	if hostPort == "" {
		hostPort = defaultHostPort
	}
	ns := os.Getenv(envNamespace)
	if ns == "" {
		ns = defaultNamespace
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := client.DialContext(ctx, client.Options{HostPort: hostPort, Namespace: ns})
	if err != nil {
		t.Skipf("no Temporal server at %s (%v); start the local stack with `docker compose up -d --wait`", hostPort, err)
	}
	if _, err := c.CheckHealth(ctx, &client.CheckHealthRequest{}); err != nil {
		c.Close()
		t.Skipf("Temporal at %s is not healthy: %v", hostPort, err)
	}
	t.Cleanup(c.Close)
	return c
}

// TestIntegration_Workflow_RunsAgainstARealServer runs both workflows on a
// real Temporal server with a real worker, over the same Register call
// cmd/workflow-worker makes. The test environment proves the logic; this
// proves the wiring: task queue, registration names, payload codecs and the
// activity signatures the server will actually invoke.
func TestIntegration_Workflow_RunsAgainstARealServer(t *testing.T) {
	c := dialTemporal(t)
	w := newWorld(t, &fakeContainment{})
	w.driver.script("dep-live",
		workflows.DepositState{Status: "SESSION_CREATED"},
		workflows.DepositState{Status: "PROVIDER_CONFIRMED"},
		workflows.DepositState{Status: "AVAILABLE", Terminal: true, WithdrawalEligible: true},
	)
	w.records.put(workflows.RecordState{
		RecordID: "rec-live", Kind: "EXECUTION", Status: "MISMATCH", AccountID: "acct-live",
	})

	queue := "workflows-itest-" + time.Now().UTC().Format("20060102150405.000000000")
	wk := worker.New(c, queue, worker.Options{})
	require.NoError(t, workflows.Register(wk, w.deps))
	require.NoError(t, wk.Start())
	defer wk.Stop()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	t.Run("funding", func(t *testing.T) {
		run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
			ID: workflows.FundingWorkflowID("dep-live") + ":" + queue, TaskQueue: queue,
		}, workflows.FundingWorkflowName, workflows.FundingInput{
			DepositID: "dep-live", PollInterval: time.Second, ReviewAfter: time.Hour, Deadline: time.Minute,
		})
		require.NoError(t, err)
		var out workflows.FundingResult
		require.NoError(t, run.Get(ctx, &out))
		assert.Equal(t, "AVAILABLE", out.Status)
		assert.True(t, out.Terminal)
		assert.Equal(t, 3, out.Polls)
		recordHistory(t, c, run.GetID(), run.GetRunID(), "funding_workflow_history.json")
	})

	t.Run("escalation", func(t *testing.T) {
		tiers := []workflows.EscalationTier{
			{Name: "OPERATIONS", Severity: workflows.SeverityWarning, Wait: 2 * time.Second},
			{Name: "FINANCE_RISK", Severity: workflows.SeverityCritical, Wait: 2 * time.Second},
		}
		run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
			ID: workflows.EscalationWorkflowID("rec-live") + ":" + queue, TaskQueue: queue,
		}, workflows.EscalationWorkflowName, workflows.EscalationInput{
			RecordID: "rec-live", Tiers: tiers, PollInterval: time.Second,
		})
		require.NoError(t, err)
		var out workflows.EscalationResult
		require.NoError(t, run.Get(ctx, &out))
		assert.True(t, out.Escalated)
		assert.Equal(t, 2, out.TiersNotified)
		assert.Empty(t, out.ContainmentRef, "an immaterial record raises no kill switch")
		recordHistory(t, c, run.GetID(), run.GetRunID(), "escalation_workflow_history.json")
	})

	t.Run("a signal resolves an escalation on the real server", func(t *testing.T) {
		w.records.put(workflows.RecordState{RecordID: "rec-signal", Kind: "FUNDING", Status: "MISMATCH"})
		tiers := []workflows.EscalationTier{{Name: "OPERATIONS", Severity: workflows.SeverityWarning, Wait: 30 * time.Second}}
		run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
			ID: workflows.EscalationWorkflowID("rec-signal") + ":" + queue, TaskQueue: queue,
		}, workflows.EscalationWorkflowName, workflows.EscalationInput{
			RecordID: "rec-signal", Tiers: tiers, PollInterval: 2 * time.Second,
		})
		require.NoError(t, err)

		w.records.put(workflows.RecordState{
			RecordID: "rec-signal", Kind: "FUNDING", Status: "RESOLVED_MANUAL", Resolved: true,
		})
		require.NoError(t, c.SignalWorkflow(ctx, run.GetID(), run.GetRunID(), workflows.RecordResolvedSignal, nil))

		var out workflows.EscalationResult
		require.NoError(t, run.Get(ctx, &out))
		assert.True(t, out.Resolved)
		assert.False(t, out.Escalated)
	})
}

// TestIntegration_Workflow_ReplayFromServer runs a workflow, downloads its
// history from the server and replays it. Unlike the checked-in replay test
// this uses a history the server produced in this very run, so it also proves
// the recording pipeline that keeps testdata honest.
func TestIntegration_Workflow_ReplayFromServer(t *testing.T) {
	c := dialTemporal(t)
	w := newWorld(t, &fakeContainment{})
	w.driver.script("dep-replay",
		workflows.DepositState{Status: "PROVIDER_PROCESSING"},
		workflows.DepositState{Status: "PROVIDER_CONFIRMED"},
		workflows.DepositState{Status: "SETTLEMENT_OBSERVED"},
		workflows.DepositState{Status: "AVAILABLE", Terminal: true},
	)
	w.records.put(workflows.RecordState{
		RecordID: "rec-replay", Kind: "WALLET_BALANCE", Status: "MISMATCH",
		Material: true, BlocksNewRisk: true, AccountID: "acct-replay",
	})

	queue := "workflows-replay-" + time.Now().UTC().Format("20060102150405.000000000")
	wk := worker.New(c, queue, worker.Options{})
	require.NoError(t, workflows.Register(wk, w.deps))
	require.NoError(t, wk.Start())
	defer wk.Stop()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	cases := []struct {
		name     string
		file     string
		workflow string
		id       string
		input    any
	}{
		{
			name: "funding", file: "funding_workflow_history.json",
			workflow: workflows.FundingWorkflowName, id: workflows.FundingWorkflowID("dep-replay") + ":" + queue,
			input: workflows.FundingInput{
				DepositID: "dep-replay", PollInterval: time.Second, ReviewAfter: 2 * time.Second, Deadline: time.Minute,
			},
		},
		{
			name: "escalation", file: "escalation_workflow_history.json",
			workflow: workflows.EscalationWorkflowName, id: workflows.EscalationWorkflowID("rec-replay") + ":" + queue,
			input: workflows.EscalationInput{
				RecordID: "rec-replay", PollInterval: time.Second,
				Tiers: []workflows.EscalationTier{
					{Name: "OPERATIONS", Severity: workflows.SeverityWarning, Wait: 2 * time.Second},
					{Name: "INCIDENT", Severity: workflows.SeverityCritical, Wait: 2 * time.Second},
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: tc.id, TaskQueue: queue}, tc.workflow, tc.input)
			require.NoError(t, err)
			require.NoError(t, run.Get(ctx, nil))

			hist := fetchHistory(t, c, run.GetID(), run.GetRunID())
			require.NotEmpty(t, hist.Events)

			r := worker.NewWorkflowReplayer()
			workflows.RegisterWorkflows(r)
			require.NoError(t, r.ReplayWorkflowHistory(nil, hist),
				"the workflow does not replay the history the server just recorded")

			recordHistory(t, c, run.GetID(), run.GetRunID(), tc.file)
		})
	}
}

// fetchHistory downloads a completed execution's history.
func fetchHistory(t *testing.T, c client.Client, workflowID, runID string) *history.History {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	iter := c.GetWorkflowHistory(ctx, workflowID, runID, false, enums.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	var h history.History
	for iter.HasNext() {
		ev, err := iter.Next()
		require.NoError(t, err)
		h.Events = append(h.Events, ev)
	}
	return &h
}

// recordHistory writes the execution's history to testdata when
// CP_WORKFLOW_RECORD_HISTORY is set, so the unit-tier replay test has a
// checked-in history to replay. It is opt-in because a test that rewrote its
// own expectations on every run would never fail.
func recordHistory(t *testing.T, c client.Client, workflowID, runID, name string) {
	t.Helper()
	if os.Getenv(envRecordHistory) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	iter := c.GetWorkflowHistory(ctx, workflowID, runID, false, enums.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	var h history.History
	for iter.HasNext() {
		ev, err := iter.Next()
		require.NoError(t, err)
		h.Events = append(h.Events, ev)
	}
	body, err := protojson.MarshalOptions{Indent: "  "}.Marshal(&h)
	require.NoError(t, err)
	// Round-trip through encoding/json so the file is stably formatted:
	// protojson deliberately randomizes whitespace between releases.
	var pretty any
	require.NoError(t, json.Unmarshal(body, &pretty))
	stable, err := json.MarshalIndent(pretty, "", "  ")
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll("testdata", 0o755))
	path := filepath.Join("testdata", name)
	require.NoError(t, os.WriteFile(path, append(stable, '\n'), 0o600))
	t.Logf("recorded %d history events to %s", len(h.Events), path)
}
