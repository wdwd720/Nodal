//go:build integration

package prediction

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// A calibration snapshot states the sample it was computed from (F-60).
//
// `calibrationSourceSQL` inner-joins outcomes, so every prediction in the window
// without one is dropped before Go sees a row. `PGCalibrator` holds a clock and
// nothing else -- no logger, no counter, no metric -- and nothing anywhere
// compared the window's prediction count against the outcomes found.
// `NPredictions` counts survivors, per bucket, so it could not reveal the loss
// either: a reader could not tell six of six from six of six hundred.
//
// That is load-bearing rather than untidy.
// `agent_lifecycle_transitions.calibration_snapshot_id` is a foreign key onto
// this table, so a promotion decision cites one of these rows -- and a strategy
// whose losing predictions happen not to resolve would look beautifully
// calibrated on a sample it chose.
//
// The existing test pinned the silence rather than questioning it:
// `TestCalibrationIgnoresUnresolvedPredictions` builds one open prediction and
// asserts err == nil with zero rows. It is a test *for* the drop. The mixed
// case -- some resolved, some not -- had no coverage at all, and the only
// aggregation test writes an outcome for all six of its predictions.

// resolveIt writes a real outcome for p through the ledger, the way the
// aggregation test does -- an inserted row rather than a fabricated one, so the
// join this test is about is the join production uses.
func (f *fixture) resolveIt(t *testing.T, l *PGLedger, p Prediction, hit bool) {
	t.Helper()
	brier, err := Brier(p.ProbabilityDirection, hit)
	require.NoError(t, err)
	logLoss, err := LogLoss(p.ProbabilityDirection, hit, DefaultEpsilon())
	require.NoError(t, err)
	realized := money.BPS(100)
	if !hit {
		realized = money.BPS(-100)
	}
	o := Outcome{
		ID: NewOutcomeID(), PredictionID: p.ID, StrategyVersionID: f.versionID, Mode: ModeShadow,
		HorizonEndAt: p.HorizonEnd(), ResolvedAt: p.HorizonEnd().Add(time.Minute),
		RealizedDirection: DirectionUp, RealizedReturnBPS: realized, RealizedMaxDrawdownBPS: money.BPS(10),
		DirectionHit: &hit, Brier: brier, LogLoss: logLoss,
		AbsReturnErrorBPS: money.BPS(20), RegimeLabel: UnlabelledRegime,
		ValuationSource: "itest-oracle", PriceRefStart: "a", PriceRefEnd: "b",
		ResolverVersion: ResolverVersion,
	}
	require.NoError(t, testDB.InTx(context.Background(), db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 0},
		func(ctx context.Context, tx pgx.Tx) error { return l.RecordOutcome(ctx, tx, o) }))
}

func TestIntegration_CalibrationStatesWhatItCouldNotScore(t *testing.T) {
	f := newFixture(t)
	l := f.ledger(t)
	ctx := context.Background()

	// Six predictions in the window. Two get outcomes; four do not, which is
	// what a dead feed or an unresolvable horizon leaves behind.
	const total, resolved = 6, 2
	for i := range total {
		runID := newUUID()
		f.newRun(t, runID)
		draft := f.draft(t, fmt.Sprintf("enter-%d", i), time.Hour, "0.7")
		draft.RunID = runID
		p := f.commit(t, draft)
		if i < resolved {
			f.resolveIt(t, l, p, true)
		}
		f.clk.Advance(time.Second)
	}

	c, err := NewCalibrator(f.clk)
	require.NoError(t, err)
	rows, err := c.Compute(ctx, testDB, CalibrationScope{
		StrategyVersionID: f.versionID, AgentID: f.agentID, Mode: ModeShadow,
		WindowStart: f.clk.Now().Add(-time.Hour), WindowEnd: f.clk.Now().Add(time.Hour),
	}, f.clk.Now())
	require.NoError(t, err)
	require.NotEmpty(t, rows, "two resolved predictions must produce a bucket")

	scored := 0
	for _, r := range rows {
		assert.Equal(t, total, r.WindowPredictions,
			"the snapshot must state the window's population, not its survivors")
		assert.Equal(t, resolved, r.WindowScored)
		scored += r.NPredictions
	}
	assert.Equal(t, resolved, scored, "the buckets must partition exactly the scored predictions")

	// And it survives to the row a promotion decision reads.
	require.NoError(t, testDB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error { return c.Persist(ctx, tx, rows) }))

	var gotPopulation, gotScored int
	require.NoError(t, testDB.QueryRow(ctx,
		`SELECT window_predictions, window_scored FROM calibration_snapshots WHERE id = $1`,
		rows[0].ID).Scan(&gotPopulation, &gotScored))
	assert.Equal(t, total, gotPopulation)
	assert.Equal(t, resolved, gotScored)
}

// TestIntegration_CalibrationSampleIsNotVacuous is the positive signal the
// assertions above need. A `WindowPredictions` that silently equalled
// `WindowScored` -- because the population query drifted to carry the outcome
// join, say -- would agree with every snapshot and report nothing. So one case
// must show the two numbers differing and another must show them equal when
// they genuinely are.
func TestIntegration_CalibrationSampleIsNotVacuous(t *testing.T) {
	f := newFixture(t)
	l := f.ledger(t)
	ctx := context.Background()

	for i := range 3 {
		runID := newUUID()
		f.newRun(t, runID)
		draft := f.draft(t, fmt.Sprintf("enter-%d", i), time.Hour, "0.7")
		draft.RunID = runID
		f.resolveIt(t, l, f.commit(t, draft), true)
		f.clk.Advance(time.Second)
	}

	c, err := NewCalibrator(f.clk)
	require.NoError(t, err)
	rows, err := c.Compute(ctx, testDB, CalibrationScope{
		StrategyVersionID: f.versionID, AgentID: f.agentID, Mode: ModeShadow,
		WindowStart: f.clk.Now().Add(-time.Hour), WindowEnd: f.clk.Now().Add(time.Hour),
	}, f.clk.Now())
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	for _, r := range rows {
		assert.Equal(t, 3, r.WindowPredictions)
		assert.Equal(t, 3, r.WindowScored, "nothing was dropped, and the snapshot should say so")
	}
}

// TestPersistRefusesASnapshotThatDoesNotStateItsSample: a row assembled by hand
// cannot skip the accounting. The database CHECK would refuse it too, but the
// error here names the snapshot rather than the constraint.
func TestPersistRefusesASnapshotThatDoesNotStateItsSample(t *testing.T) {
	f := newFixture(t)
	c, err := NewCalibrator(f.clk)
	require.NoError(t, err)

	err = testDB.InTx(context.Background(), db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			return c.Persist(ctx, tx, []CalibrationRow{{
				NPredictions: 4, WindowScored: 4, WindowPredictions: 1,
			}})
		})
	require.Error(t, err, "a snapshot claiming to have scored more than the window held was accepted")
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	assert.Contains(t, err.Error(), "claims 4 scored of 1")
}
