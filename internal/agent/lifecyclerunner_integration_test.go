//go:build integration

package agent

import (
	"context"
	"regexp"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/security"
)

// Revoking an agent stops the runs it already has open (F-73).
//
// PGDispatcher.dispatchAgent asks Runnable() before opening anything, so a
// REVOKED, FAILED or SUPERSEDED agent gets no NEW runs. Runner.prepare loaded
// the agent row and took `stage` and `risk_policy_version` from it while
// discarding the state, and ListOpenRuns selects on the run's status with no
// predicate on the agent -- so every run already open ran through to its
// intent.
//
// Pause reaches an in-flight run at five separate points, which is the right
// shape and is what made the gap easy to miss: the lifecycle control an
// operator reaches for in an incident is the terminal one, and that was the one
// that did not reach.
func TestIntegration_ARevokedAgentStopsARunAlreadyOpen(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state State
		drive func(t *testing.T, f *runnerFixture)
	}{
		{"revoked", StateRevoked, func(t *testing.T, f *runnerFixture) {
			require.NoError(t, inTx(ctxAs(f.operator(security.RoleOperations, security.RoleRisk)), t,
				func(ctx context.Context, tx pgx.Tx) error {
					_, err := f.lifecycle.Revoke(ctx, tx, f.agent.ID, "revoked mid-incident")
					return err
				}))
		}},
		{"failed", StateFailed, func(t *testing.T, f *runnerFixture) {
			require.NoError(t, inTx(ctxAs(f.operator()), t, func(ctx context.Context, tx pgx.Tx) error {
				_, err := f.lifecycle.Fail(ctx, tx, f.agent.ID, "evaluator is unrecoverable")
				return err
			}))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunnerFixture(t, nil)
			f.evaluator.decisions = []Decision{predictionDecision(t, mustInstrument(t, f.instrumentID))}
			tc.drive(t, f)

			a, err := f.store.Get(context.Background(), testDB, f.agent.ID)
			require.NoError(t, err)
			require.Equal(t, tc.state, a.State, "the fixture did not reach the state this case is about")

			res, err := f.runner.Run(context.Background(), f.run.ID)
			require.NoError(t, err)
			assert.Equal(t, SkipAgentNotRunnable, res.SkipReason)
			assert.Equal(t, RunSkipped, res.Run.Status)
			assert.Contains(t, res.Run.Error, string(tc.state),
				"the recorded reason must name the state, or an operator cannot tell the three apart")
			assert.EqualValues(t, 0, f.adapter.dials.Load(), "a stopped agent makes no provider call")
			assert.Zero(t, f.evaluator.calls, "a stopped agent is not evaluated")
			// The whole point: no prediction and no intent came out of a run
			// whose agent had been terminated.
			assertCounts(t, f, 1, 0, 0)
		})
	}
}

// TestIntegration_ARunnableAgentStillRuns is the control. A check placed one
// condition too wide would pass every case above and stop the platform's agents
// altogether, which is the more expensive failure of the two.
func TestIntegration_ARunnableAgentStillRuns(t *testing.T) {
	f := newRunnerFixture(t, nil)
	f.evaluator.decisions = []Decision{predictionDecision(t, mustInstrument(t, f.instrumentID))}
	res, err := f.runner.Run(context.Background(), f.run.ID)
	require.NoError(t, err)
	assert.NotEqual(t, SkipAgentNotRunnable, res.SkipReason, "a SHADOW agent was refused as not runnable")
	assert.Positive(t, f.evaluator.calls)
	assertCounts(t, f, 1, 1, 0)
}

// TestIntegration_TheReasonListsMatchTheDatabase: allSkipReasons and
// pauseReasons are each a list written twice, once in Go and once in a CHECK
// constraint. The unit tests named ...MirrorTheDatabaseCheck do not read the
// database at all -- they assert a hardcoded length and then that every member
// of the list is a member of the list -- so they force a human to bump a number
// and cannot detect the divergence their names claim to cover. This is that
// check.
//
// The failure it prevents arrives at the worst moment: a reason declared in Go
// and missing from the CHECK is a run or a pause that cannot be recorded at
// all, and the constraint violation lands exactly when something has already
// gone wrong enough to need recording.
func TestIntegration_TheReasonListsMatchTheDatabase(t *testing.T) {
	requireEnv(t)
	skips := make([]string, 0, len(allSkipReasons))
	for _, r := range SkipReasons() {
		skips = append(skips, string(r))
	}
	pauses := make([]string, 0, len(PauseReasons()))
	for _, r := range PauseReasons() {
		pauses = append(pauses, string(r))
	}
	for _, tc := range []struct {
		table, constraint string
		want              []string
	}{
		{"agent_runs", "agent_runs_skip_reason_check", skips},
		{"agent_pauses", "agent_pauses_reason_code_check", pauses},
	} {
		t.Run(tc.constraint, func(t *testing.T) {
			var def string
			require.NoError(t, testDB.QueryRow(context.Background(),
				`SELECT pg_get_constraintdef(oid) FROM pg_constraint
					WHERE conrelid = $1::regclass AND conname = $2`, tc.table, tc.constraint).Scan(&def))

			found := regexp.MustCompile(`'([A-Z_]+)'`).FindAllStringSubmatch(def, -1)
			require.NotEmpty(t, found, "no literals in %q -- the constraint is not the one this test means", def)
			got := make([]string, 0, len(found))
			for _, m := range found {
				got = append(got, m[1])
			}
			want := append([]string(nil), tc.want...)
			sort.Strings(got)
			sort.Strings(want)
			assert.Equal(t, want, got, "the Go list and %s have diverged", tc.constraint)
		})
	}
}
