package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/funding"
	"github.com/nodal/controlplane/internal/workflows"
)

func TestRun_Usage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no arguments", nil, exitUsage},
		{"unknown command", []string{"frobnicate"}, exitUsage},
		{"run with arguments", []string{"run", "extra"}, exitUsage},
		{"check with arguments", []string{"check", "extra"}, exitUsage},
		{"help", []string{"help"}, exitOK},
		{"long help", []string{"--help"}, exitOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out, errOut bytes.Buffer
			code := run(tc.args, func(string) (string, bool) { return "", false }, &out, &errOut)
			assert.Equal(t, tc.want, code)
			if tc.want == exitOK {
				assert.Contains(t, out.String(), "usage: workflow-worker")
			}
		})
	}
}

// A command that cannot resolve its configuration must fail before it touches
// anything, not halfway through.
func TestRun_FailsClosedOnBadConfiguration(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	code := run([]string{"check"}, func(k string) (string, bool) {
		if k == "CP_ENV" {
			return "NOT_AN_ENVIRONMENT", true
		}
		return "", false
	}, &out, &errOut)
	assert.Equal(t, exitFailure, code)
	assert.Contains(t, errOut.String(), "workflow-worker:")
}

func TestIntVar(t *testing.T) {
	t.Parallel()
	lookup := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}
	n, err := intVar(lookup(nil), envMaxActivities, 7)
	require.NoError(t, err)
	assert.Equal(t, 7, n)

	n, err = intVar(lookup(map[string]string{envMaxActivities: " 12 "}), envMaxActivities, 7)
	require.NoError(t, err)
	assert.Equal(t, 12, n)

	for _, bad := range []string{"0", "-1", "many"} {
		_, err := intVar(lookup(map[string]string{envMaxActivities: bad}), envMaxActivities, 7)
		assert.Error(t, err, bad)
	}
}

func TestDurationVar(t *testing.T) {
	t.Parallel()
	lookup := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}
	d, err := durationVar(lookup(nil), envDrainTimeout, time.Second)
	require.NoError(t, err)
	assert.Equal(t, time.Second, d)

	d, err = durationVar(lookup(map[string]string{envDrainTimeout: "90s"}), envDrainTimeout, time.Second)
	require.NoError(t, err)
	assert.Equal(t, 90*time.Second, d)

	for _, bad := range []string{"0s", "-5s", "soon"} {
		_, err := durationVar(lookup(map[string]string{envDrainTimeout: bad}), envDrainTimeout, time.Second)
		assert.Error(t, err, bad)
	}
}

// stubLifecycle and stubDeposits let the adapter be tested without a database.
type stubLifecycle struct {
	calls []funding.DepositID
	err   error
}

func (s *stubLifecycle) Advance(_ context.Context, id funding.DepositID) error {
	s.calls = append(s.calls, id)
	return s.err
}

type stubDeposits struct {
	dep funding.Deposit
	err error
}

func (s stubDeposits) Get(context.Context, funding.DepositID) (funding.Deposit, error) {
	return s.dep, s.err
}

func TestFundingDriver_MapsStatusWithoutAmounts(t *testing.T) {
	t.Parallel()
	id := funding.NewDepositID()
	cases := []struct {
		status   funding.Status
		terminal bool
	}{
		{funding.StatusCreated, false},
		{funding.StatusSessionCreated, false},
		{funding.StatusProviderProcessing, false},
		{funding.StatusReconciled, false},
		{funding.StatusAvailable, true},
		{funding.StatusFailed, true},
		{funding.StatusExpired, true},
		{funding.StatusCancelled, true},
		{funding.StatusReversed, true},
		{funding.StatusReviewRequired, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			t.Parallel()
			d, err := newFundingDriver(&stubLifecycle{}, stubDeposits{
				dep: funding.Deposit{ID: id, Status: tc.status, WithdrawalEligible: tc.status == funding.StatusAvailable},
			})
			require.NoError(t, err)
			state, err := d.Describe(t.Context(), id.String())
			require.NoError(t, err)
			assert.Equal(t, string(tc.status), state.Status)
			assert.Equal(t, tc.terminal, state.Terminal,
				"REVIEW_REQUIRED is not terminal: an operator can still move it, so the workflow keeps watching")
			assert.Equal(t, id.String(), state.DepositID)
		})
	}
}

func TestFundingDriver_RejectsAMalformedID(t *testing.T) {
	t.Parallel()
	d, err := newFundingDriver(&stubLifecycle{}, stubDeposits{})
	require.NoError(t, err)
	_, err = d.Describe(t.Context(), "not-a-uuid")
	require.Error(t, err)
	assert.True(t, errs.HasCode(err, errs.CodeValidationFailed), "%v", err)
	assert.Error(t, d.Advance(t.Context(), "not-a-uuid"))
}

func TestFundingDriver_AdvanceReachesTheLifecycle(t *testing.T) {
	t.Parallel()
	id := funding.NewDepositID()
	life := &stubLifecycle{}
	d, err := newFundingDriver(life, stubDeposits{dep: funding.Deposit{ID: id, Status: funding.StatusCreated}})
	require.NoError(t, err)
	require.NoError(t, d.Advance(t.Context(), id.String()))
	require.Len(t, life.calls, 1)
	assert.Equal(t, id, life.calls[0])
}

func TestNewFundingDriver_RequiresBothHalves(t *testing.T) {
	t.Parallel()
	_, err := newFundingDriver(nil, stubDeposits{})
	require.Error(t, err)
	_, err = newFundingDriver(&stubLifecycle{}, nil)
	require.Error(t, err)
}

// The reconciliation store is not wired in this build. Every operation must
// fail with UNSUPPORTED rather than quietly succeeding, because a workflow
// that reported an escalation it never recorded would be worse than no
// workflow at all.
func TestUnwiredReconciliation_FailsClosed(t *testing.T) {
	t.Parallel()
	var r workflows.ReconciliationRecords = unwiredRecords{}
	_, err := r.Describe(t.Context(), "rec-1")
	require.Error(t, err)
	assert.True(t, errs.HasCode(err, errs.CodeUnsupported), "%v", err)
	assert.True(t, errs.HasCode(r.MarkInvestigating(t.Context(), "rec-1", "why"), errs.CodeUnsupported))
	assert.True(t, errs.HasCode(r.MarkEscalated(t.Context(), "rec-1", "why"), errs.CodeUnsupported))

	records, containment := bindReconciliation(nil)
	assert.NotNil(t, records)
	assert.Nil(t, containment, "a nil containment controller makes internal/workflows refuse the request loudly")
}

func TestLogAlerter_UsesTheAlertSeverity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		severity string
		want     string
	}{
		{workflows.SeverityCritical, "level=ERROR"},
		{workflows.SeverityWarning, "level=WARN"},
		{workflows.SeverityInfo, "level=INFO"},
	} {
		t.Run(tc.severity, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			a := newLogAlerter(textLogger(&buf))
			require.NoError(t, a.Alert(t.Context(), workflows.OperatorAlert{
				Kind: "K", Severity: tc.severity, Subject: "s", ResourceType: "deposit", ResourceID: "d1",
				Fields: map[string]string{"status": "PROVIDER_PROCESSING"},
			}))
			out := buf.String()
			assert.Contains(t, out, tc.want)
			assert.Contains(t, out, "resource_id=d1")
			assert.Contains(t, out, "field.status=PROVIDER_PROCESSING")
		})
	}
}

func TestLogAlerter_TolerantOfANilLogger(t *testing.T) {
	t.Parallel()
	require.NoError(t, newLogAlerter(nil).Alert(t.Context(), workflows.OperatorAlert{Kind: "K"}))
}

// textLogger builds a text slog logger writing into buf.
func textLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// The task queue must be derived from configuration, never left empty: a
// worker listening on "" would silently receive nothing.
func TestTaskQueueNeverEmpty(t *testing.T) {
	t.Parallel()
	assert.NotEmpty(t, workflows.TaskQueue(""))
	assert.True(t, strings.HasSuffix(workflows.TaskQueue("cp-prod"), "control-plane"))
}
