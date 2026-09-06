package workflows_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/workflows"
)

// fakeDriver is an in-memory funding driver. Advance walks a scripted list of
// states; a deposit id that is not scripted is NOT_FOUND, which is how the
// non-retryable path is exercised.
type fakeDriver struct {
	mu       sync.Mutex
	states   map[string][]workflows.DepositState
	advances map[string]int
	describe map[string]int
	failNext map[string]error
}

func newFakeDriver() *fakeDriver {
	return &fakeDriver{
		states:   map[string][]workflows.DepositState{},
		advances: map[string]int{},
		describe: map[string]int{},
		failNext: map[string]error{},
	}
}

func (f *fakeDriver) script(depositID string, states ...workflows.DepositState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states[depositID] = states
}

func (f *fakeDriver) Advance(_ context.Context, depositID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failNext[depositID]; err != nil {
		delete(f.failNext, depositID)
		return err
	}
	if _, ok := f.states[depositID]; !ok {
		return errs.New(errs.CodeNotFound, "fake: unknown deposit")
	}
	f.advances[depositID]++
	return nil
}

func (f *fakeDriver) Describe(_ context.Context, depositID string) (workflows.DepositState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	states, ok := f.states[depositID]
	if !ok {
		return workflows.DepositState{}, errs.New(errs.CodeNotFound, "fake: unknown deposit")
	}
	f.describe[depositID]++
	i := f.advances[depositID] - 1
	if i < 0 {
		i = 0
	}
	if i >= len(states) {
		i = len(states) - 1
	}
	return states[i], nil
}

func (f *fakeDriver) advanceCount(depositID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.advances[depositID]
}

// fakeAlerter records operator alerts.
type fakeAlerter struct {
	mu     sync.Mutex
	alerts []workflows.OperatorAlert
	err    error
}

func (f *fakeAlerter) Alert(_ context.Context, a workflows.OperatorAlert) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.alerts = append(f.alerts, a)
	return nil
}

func (f *fakeAlerter) all() []workflows.OperatorAlert {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]workflows.OperatorAlert(nil), f.alerts...)
}

// fakeRecords is an in-memory reconciliation record store.
type fakeRecords struct {
	mu            sync.Mutex
	byID          map[string]workflows.RecordState
	investigating []string
	escalated     []string
}

func newFakeRecords() *fakeRecords {
	return &fakeRecords{byID: map[string]workflows.RecordState{}}
}

func (f *fakeRecords) put(s workflows.RecordState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[s.RecordID] = s
}

func (f *fakeRecords) Describe(_ context.Context, recordID string) (workflows.RecordState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.byID[recordID]
	if !ok {
		return workflows.RecordState{}, errs.New(errs.CodeNotFound, "fake: unknown record")
	}
	return s, nil
}

func (f *fakeRecords) MarkInvestigating(_ context.Context, recordID, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.byID[recordID]
	if !ok {
		return errs.New(errs.CodeNotFound, "fake: unknown record")
	}
	f.investigating = append(f.investigating, recordID)
	s.Status = "INVESTIGATING"
	f.byID[recordID] = s
	return nil
}

func (f *fakeRecords) MarkEscalated(_ context.Context, recordID, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.byID[recordID]
	if !ok {
		return errs.New(errs.CodeNotFound, "fake: unknown record")
	}
	f.escalated = append(f.escalated, recordID)
	s.Status = "ESCALATED"
	f.byID[recordID] = s
	return nil
}

// fakeContainment records containment requests and hands back a reference.
type fakeContainment struct {
	mu       sync.Mutex
	requests []workflows.ContainmentRequest
	err      error
}

func (f *fakeContainment) Contain(_ context.Context, req workflows.ContainmentRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	f.requests = append(f.requests, req)
	return "kill-switch:GLOBAL_NEW_RISK_KILL:*", nil
}

func (f *fakeContainment) all() []workflows.ContainmentRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]workflows.ContainmentRequest(nil), f.requests...)
}

// world is the set of fakes plus the real activity implementations wired over
// them, which is exactly the shape cmd/workflow-worker assembles.
type world struct {
	driver      *fakeDriver
	alerter     *fakeAlerter
	records     *fakeRecords
	containment *fakeContainment
	deps        workflows.Deps
}

func newWorld(t *testing.T, containment workflows.ContainmentController) *world {
	t.Helper()
	w := &world{driver: newFakeDriver(), alerter: &fakeAlerter{}, records: newFakeRecords()}
	if c, ok := containment.(*fakeContainment); ok {
		w.containment = c
	}
	fa, err := workflows.NewFundingActivities(w.driver, w.alerter, nil)
	require.NoError(t, err)
	ea, err := workflows.NewEscalationActivities(w.records, w.alerter, containment, nil)
	require.NoError(t, err)
	w.deps = workflows.Deps{Funding: fa, Escalation: ea}
	return w
}

// newEnv builds a test workflow environment with the real registration path:
// the same Register call cmd/workflow-worker makes, over the fakes above.
// Tests then override individual activities with env.OnActivity where they
// want to script behavior rather than drive it through a fake.
func newEnv(t *testing.T, w *world) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	var s testsuite.WorkflowTestSuite
	env := s.NewTestWorkflowEnvironment()
	// The Temporal test environment defaults to a 3s wall-clock budget for a
	// whole workflow run. Nothing here is testing how fast a workflow completes
	// — workflow time is simulated and the assertions are about ordering,
	// determinism and effects — but the budget is real wall clock, so under a
	// contended `-race` run these tests exceeded it and reported a workflow
	// error that read like a logic failure. Observed exactly that in a
	// repo-wide parallel sweep: two tests failed at ~1.95s against the 3s
	// default while passing in isolation and three times over under deliberate
	// load. A deadline that is not the property under test is made generous
	// (see D-028, the same rule applied to HTTP client deadlines).
	env.SetTestTimeout(2 * time.Minute)
	require.NoError(t, workflows.Register(env, w.deps))
	t.Cleanup(func() { env.AssertExpectations(t) })
	return env
}
