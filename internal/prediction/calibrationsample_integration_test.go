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
// resolveAt is resolveIt with an explicit knowledge time, for the tests that
// care when the platform LEARNED an outcome rather than when it happened.
func (f *fixture) resolveAt(t *testing.T, l *PGLedger, p Prediction, hit bool, resolvedAt time.Time) {
	t.Helper()
	f.resolveWith(t, l, p, hit, resolvedAt)
}

// resolveIt resolves a prediction a minute after its horizon closes, and moves
// the fixture clock there.
//
// The clock move is the part that matters. Outcome.Validate refuses a
// resolution before the horizon ends, so a fixture that commits a prediction
// and resolves it two seconds later was stamping resolved_at an HOUR in the
// future -- recording an outcome the platform could not yet have had. That went
// unnoticed while nothing compared resolved_at to anything; F-120's bound on
// knowledge time is what surfaced it.
func (f *fixture) resolveIt(t *testing.T, l *PGLedger, p Prediction, hit bool) {
	t.Helper()
	at := p.HorizonEnd().Add(time.Minute)
	if now := f.clk.Now(); now.Before(at) {
		f.clk.Advance(at.Sub(now))
	}
	f.resolveWith(t, l, p, hit, at)
}

func (f *fixture) resolveWith(t *testing.T, l *PGLedger, p Prediction, hit bool, resolvedAt time.Time) {
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
		HorizonEndAt: p.HorizonEnd(), ResolvedAt: resolvedAt,
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
		// The window is a day rather than an hour because resolveIt now moves the
		// clock to the resolution instant: a prediction with a one-hour horizon
		// cannot be resolved two seconds after it is committed, and the fixture
		// used to stamp resolved_at an hour in the future to pretend otherwise.
		// F-120's bound on knowledge time is what surfaced that.
		WindowStart: f.clk.Now().Add(-24 * time.Hour), WindowEnd: f.clk.Now().Add(time.Hour),
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
		// The window is a day rather than an hour because resolveIt now moves the
		// clock to the resolution instant: a prediction with a one-hour horizon
		// cannot be resolved two seconds after it is committed, and the fixture
		// used to stamp resolved_at an hour in the future to pretend otherwise.
		// F-120's bound on knowledge time is what surfaced that.
		WindowStart: f.clk.Now().Add(-24 * time.Hour), WindowEnd: f.clk.Now().Add(time.Hour),
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

// A calibration snapshot is reproducible (F-120).
//
// The window is on p.committed_at -- when the prediction was made. The
// outcome's resolved_at is when this platform learned how it turned out, and
// nothing bounded it. So recomputing a snapshot "as of" an earlier date folded
// in every outcome resolved since, and the same scope recomputed later gave a
// different answer.
//
// That is the reproducibility path that matters: calibration_snapshots rows are
// cited as CALIBRATION_SNAPSHOT promotion evidence, and evidence that changes
// when you look at it again is not evidence. Compute already took `now`, and
// used it only to stamp ComputedAt.
func TestIntegration_ACalibrationSnapshotIsReproducible(t *testing.T) {
	f := newFixture(t)
	l := f.ledger(t)
	ctx := context.Background()

	// Two predictions committed inside the window.
	var committed []Prediction
	for i := range 2 {
		runID := newUUID()
		f.newRun(t, runID)
		draft := f.draft(t, fmt.Sprintf("repro-%d", i), time.Hour, "0.7")
		draft.RunID = runID
		committed = append(committed, f.commit(t, draft))
		f.clk.Advance(time.Second)
	}

	// The first is resolved promptly, a minute after its horizon closes.
	f.resolveAt(t, l, committed[0], true, committed[0].HorizonEnd().Add(time.Minute))
	f.clk.Advance(2 * time.Hour) // past both horizons and the first resolution

	// The instant the snapshot is taken. One outcome is known.
	asOf := f.clk.Now()
	scope := CalibrationScope{
		StrategyVersionID: f.versionID, AgentID: f.agentID, Mode: ModeShadow,
		WindowStart: asOf.Add(-24 * time.Hour), WindowEnd: asOf.Add(time.Hour),
	}
	c, err := NewCalibrator(f.clk)
	require.NoError(t, err)
	scoredIn := func(rows []CalibrationRow) int {
		n := 0
		for _, r := range rows {
			n += r.NPredictions
		}
		return n
	}

	first, err := c.Compute(ctx, testDB, scope, asOf)
	require.NoError(t, err)
	require.Equal(t, 1, scoredIn(first), "the fixture must have exactly one known outcome at asOf")

	// A month passes and the second prediction is finally resolved. The window
	// is unchanged; only what the platform has since LEARNED has changed.
	f.clk.Advance(30 * 24 * time.Hour)
	f.resolveAt(t, l, committed[1], true, f.clk.Now())

	again, err := c.Compute(ctx, testDB, scope, asOf)
	require.NoError(t, err)
	assert.Equal(t, 1, scoredIn(again),
		"the same scope recomputed as of the same instant folded in an outcome resolved after it")

	// The control: asked as of NOW the later outcome is included, so the bound
	// is on knowledge time and the row is not simply invisible.
	current, err := c.Compute(ctx, testDB, scope, f.clk.Now())
	require.NoError(t, err)
	assert.Equal(t, 2, scoredIn(current))
}
