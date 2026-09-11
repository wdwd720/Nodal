package prediction

import (
	"context"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/strategy/ir"
)

// ComputerVersion identifies the aggregation rules, recorded on every
// snapshot so a change of method is visible rather than retroactive.
const ComputerVersion = "prediction-calibrator/v1"

// DefaultBuckets is the conventional ten-bucket partition of [0, 1].
const DefaultBuckets = 10

// AllRegimes is the label a snapshot carries when it is not split by regime.
const AllRegimes = "ALL"

// CalibrationScope selects the predictions an aggregate covers.
type CalibrationScope struct {
	StrategyVersionID string
	// AgentID is optional: empty aggregates every agent of the version.
	AgentID string
	Mode    Mode
	// WindowStart and WindowEnd bound committed_at.
	WindowStart time.Time
	WindowEnd   time.Time
	// RegimeLabel is optional: empty aggregates every regime under the label
	// ALL, and a value restricts to outcomes carrying it.
	RegimeLabel string
	// Buckets is the number of equal probability buckets; 0 means
	// DefaultBuckets.
	Buckets int
}

// Validate reports the structural reasons a scope cannot be computed.
func (s CalibrationScope) Validate() error {
	fields := map[string]any{}
	fail := func(k, msg string) {
		if _, dup := fields[k]; !dup {
			fields[k] = msg
		}
	}
	if s.StrategyVersionID == "" {
		fail("strategy_version_id", "required")
	}
	if !s.Mode.Valid() {
		fail("mode", "must be one of the six modes")
	}
	if s.WindowStart.IsZero() || s.WindowEnd.IsZero() {
		fail("window", "both bounds are required")
	}
	if !s.WindowStart.IsZero() && !s.WindowEnd.IsZero() && !s.WindowStart.Before(s.WindowEnd) {
		fail("window", "window_start must be before window_end")
	}
	if s.Buckets < 0 || s.Buckets > 100 {
		fail("buckets", "must be between 1 and 100")
	}
	if len(fields) == 0 {
		return nil
	}
	return errs.New(errs.CodeValidationFailed, "prediction: invalid calibration scope").WithFields(fields)
}

func (s CalibrationScope) buckets() int {
	if s.Buckets <= 0 {
		return DefaultBuckets
	}
	return s.Buckets
}

func (s CalibrationScope) regime() string {
	if s.RegimeLabel == "" {
		return AllRegimes
	}
	return s.RegimeLabel
}

// CalibrationRow is one calibration_snapshots row: how a probability bucket
// behaved. Predicted quality and realized return are separate columns and are
// never combined: a high return is not evidence of good calibration (PART 73).
type CalibrationRow struct {
	ID                SnapshotID
	StrategyVersionID string
	AgentID           string
	Mode              Mode
	WindowStart       time.Time
	WindowEnd         time.Time
	RegimeLabel       string
	BucketLower       ir.Decimal
	BucketUpper       ir.Decimal
	NPredictions      int
	// WindowPredictions is every prediction committed in the window and in
	// scope, resolved or not. WindowScored is how many of them carried an
	// outcome and entered the statistics.
	//
	// The difference is the sample this snapshot could not see, and until F-60
	// it was invisible: the source query inner-joins outcomes, so unresolved
	// predictions are dropped before Go sees a row, and nothing counted them.
	// NPredictions is per bucket and counts survivors, so it could not reveal
	// the loss either -- a reader could not tell 6 of 6 from 6 of 600, while a
	// promotion decision cites one of these rows by foreign key.
	WindowPredictions     int
	WindowScored          int
	MeanPredicted         ir.Decimal
	RealizedFrequency     ir.Decimal
	BrierMean             ir.Decimal
	LogLossMean           ir.Decimal
	ExpectedReturnBPSMean money.BPS
	RealizedReturnBPSMean money.BPS
	ConfidenceMean        ir.Decimal
	AbsErrorBPSMean       money.BPS
	ComputerVersion       string
	ComputedAt            time.Time
}

// Calibrator computes calibration from resolved outcomes. It never computes
// during a run and never from unresolved predictions: a bucket built from
// predictions whose horizons are still open would describe the future.
type Calibrator interface {
	Compute(ctx context.Context, q db.Querier, scope CalibrationScope, now time.Time) ([]CalibrationRow, error)
	Persist(ctx context.Context, tx pgx.Tx, rows []CalibrationRow) error
}

// PGCalibrator is the PostgreSQL calibrator.
type PGCalibrator struct {
	clk clock.Clock
}

var _ Calibrator = (*PGCalibrator)(nil)

// NewCalibrator builds the calibrator.
func NewCalibrator(clk clock.Clock) (*PGCalibrator, error) {
	if clk == nil {
		return nil, errs.New(errs.CodeValidationFailed, "prediction: calibrator requires a clock")
	}
	return &PGCalibrator{clk: clk}, nil
}

// calibrationSourceSQL bounds BOTH clocks.
//
// The window is on p.committed_at, which is when the prediction was made. The
// outcome's resolved_at is when this platform learned how it turned out, and
// nothing bounded it -- so recomputing a snapshot "as of" an earlier date
// folded in every outcome resolved since, and the same scope recomputed a month
// later produced a different answer (F-120).
//
// That is the reproducibility path that matters: calibration_snapshots rows are
// cited as CALIBRATION_SNAPSHOT promotion evidence, and evidence that changes
// when you look at it again is not evidence. The `now` argument existed and was
// used only to stamp ComputedAt.
const calibrationSourceSQL = `
SELECT p.probability_direction::text, p.confidence::text, p.expected_return_bps,
       o.direction_hit, o.brier::text, o.log_loss::text, o.realized_return_bps, o.abs_return_error_bps
  FROM predictions p
  JOIN prediction_outcomes o ON o.prediction_id = p.id
 WHERE p.strategy_version_id = $1
   AND p.mode = $2
   AND p.committed_at >= $3
   AND p.committed_at < $4
   AND o.resolved_at <= $7
   AND p.direction IS NOT NULL
   AND ($5::uuid IS NULL OR p.agent_id = $5::uuid)
   AND ($6::text IS NULL OR o.regime_label = $6::text)`

// calibrationPopulationSQL counts the predictions the window holds, on the same
// scope as the source query above but WITHOUT the outcome join. It is the
// denominator the statistics are silent about.
//
// The regime filter is deliberately absent here. A regime is a property of the
// outcome, so an unresolved prediction has none, and filtering on it would
// re-introduce exactly the exclusion this count exists to expose.
const calibrationPopulationSQL = `
SELECT count(*)
  FROM predictions p
 WHERE p.strategy_version_id = $1
   AND p.mode = $2
   AND p.committed_at >= $3
   AND p.committed_at < $4
   AND p.direction IS NOT NULL
   AND ($5::uuid IS NULL OR p.agent_id = $5::uuid)`

// sample is one resolved prediction in the aggregation.
type sample struct {
	probability *big.Int // scale 6
	confidence  *big.Int // scale 6
	expectedBPS int64
	hit         bool
	hasHit      bool
	brier       *big.Int // scale 8
	logLoss     *big.Int // scale 8
	realizedBPS int64
	absErrorBPS int64
}

// Compute aggregates resolved outcomes into per-bucket calibration rows. It
// returns one row per non-empty bucket; an empty bucket is omitted rather
// than reported as zero, because "no evidence" and "perfectly calibrated"
// are not the same statement.
func (c *PGCalibrator) Compute(ctx context.Context, q db.Querier, scope CalibrationScope, now time.Time) ([]CalibrationRow, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	var agentFilter, regimeFilter *string
	if scope.AgentID != "" {
		v := scope.AgentID
		agentFilter = &v
	}
	if scope.RegimeLabel != "" {
		v := scope.RegimeLabel
		regimeFilter = &v
	}
	rows, err := q.Query(ctx, calibrationSourceSQL, scope.StrategyVersionID, string(scope.Mode),
		scope.WindowStart.UTC(), scope.WindowEnd.UTC(), agentFilter, regimeFilter, now.UTC())
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "prediction: read calibration source")
	}
	defer rows.Close()

	n := scope.buckets()
	buckets := make([][]sample, n)
	for rows.Next() {
		var (
			probStr, confStr    string
			expectedBPS         int32
			hit                 *bool
			brierStr, logStr    *string
			realizedBPS, absBPS int32
		)
		if err := rows.Scan(&probStr, &confStr, &expectedBPS, &hit, &brierStr, &logStr, &realizedBPS, &absBPS); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "prediction: scan calibration source")
		}
		s, err := newSample(probStr, confStr, expectedBPS, hit, brierStr, logStr, realizedBPS, absBPS)
		if err != nil {
			return nil, err
		}
		idx, err := bucketIndex(s.probability, n)
		if err != nil {
			return nil, err
		}
		buckets[idx] = append(buckets[idx], s)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "prediction: iterate calibration source")
	}

	scored := 0
	for _, b := range buckets {
		scored += len(b)
	}
	var population int
	if err := q.QueryRow(ctx, calibrationPopulationSQL, scope.StrategyVersionID, string(scope.Mode),
		scope.WindowStart.UTC(), scope.WindowEnd.UTC(), agentFilter).Scan(&population); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "prediction: count calibration population")
	}
	if population < scored {
		// The two queries disagree about a window neither of them writes to.
		// Refusing is right: a snapshot claiming to have scored more
		// predictions than the window held would be worse than none, and the
		// database CHECK would refuse the row anyway.
		return nil, errs.Newf(errs.CodeInternal,
			"prediction: scored %d predictions in a window holding %d", scored, population)
	}

	out := make([]CalibrationRow, 0, n)
	for i, bucket := range buckets {
		if len(bucket) == 0 {
			continue
		}
		row, err := aggregate(scope, i, n, bucket, now)
		if err != nil {
			return nil, err
		}
		row.WindowPredictions = population
		row.WindowScored = scored
		out = append(out, row)
	}
	return out, nil
}

func newSample(probStr, confStr string, expectedBPS int32, hit *bool, brierStr, logStr *string, realizedBPS, absBPS int32) (sample, error) {
	prob, err := decimalMantissaAt(probStr, ProbabilityScale)
	if err != nil {
		return sample{}, err
	}
	conf, err := decimalMantissaAt(confStr, ProbabilityScale)
	if err != nil {
		return sample{}, err
	}
	s := sample{
		probability: prob, confidence: conf,
		expectedBPS: int64(expectedBPS), realizedBPS: int64(realizedBPS), absErrorBPS: int64(absBPS),
	}
	if hit != nil {
		s.hit, s.hasHit = *hit, true
	}
	if brierStr != nil {
		v, err := decimalMantissaAt(*brierStr, ScoreScale)
		if err != nil {
			return sample{}, err
		}
		s.brier = v
	}
	if logStr != nil {
		v, err := decimalMantissaAt(*logStr, ScoreScale)
		if err != nil {
			return sample{}, err
		}
		s.logLoss = v
	}
	return s, nil
}

// decimalMantissaAt parses a decimal string and returns its mantissa at the
// requested scale, refusing anything that would need rounding.
func decimalMantissaAt(s string, scale uint8) (*big.Int, error) {
	d, err := ir.ParseDecimalString(s)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "prediction: decode decimal "+s)
	}
	if d.Scale > scale {
		trimmed, terr := d.Rescale(scale, money.RoundHalfEven)
		if terr != nil {
			return nil, errs.Wrap(terr, errs.CodeInternal, "prediction: rescale decimal")
		}
		d = trimmed
	}
	v, err := scaleTo(mustInt(d), int(d.Scale), int(scale))
	if err != nil {
		return nil, err
	}
	return v, nil
}

func mustInt(d ir.Decimal) *big.Int {
	v, err := d.Int()
	if err != nil {
		return big.NewInt(0)
	}
	return v
}

// bucketIndex places a scale-6 probability into one of n equal buckets over
// [0, 1]. A probability of exactly 1 belongs to the last bucket.
func bucketIndex(prob *big.Int, n int) (int, error) {
	if prob == nil {
		return 0, errs.New(errs.CodeInternal, "prediction: missing probability")
	}
	unit := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(ProbabilityScale)), nil)
	if prob.Sign() < 0 || prob.Cmp(unit) > 0 {
		return 0, errs.New(errs.CodeInternal, "prediction: probability outside [0, 1]")
	}
	scaled := new(big.Int).Mul(prob, big.NewInt(int64(n)))
	idx := new(big.Int).Quo(scaled, unit)
	i := int(idx.Int64())
	if i >= n {
		i = n - 1
	}
	return i, nil
}

// aggregate builds one bucket's row. Every mean is an exact integer division
// rounded half-even once, at the reporting scale.
func aggregate(scope CalibrationScope, idx, n int, bucket []sample, now time.Time) (CalibrationRow, error) {
	count := int64(len(bucket))
	sumProb := new(big.Int)
	sumConf := new(big.Int)
	sumBrier := new(big.Int)
	sumLog := new(big.Int)
	var (
		hits         int64
		hitCount     int64
		brierCount   int64
		logCount     int64
		sumExpected  int64
		sumRealized  int64
		sumAbsErrBPS int64
	)
	for _, s := range bucket {
		sumProb.Add(sumProb, s.probability)
		sumConf.Add(sumConf, s.confidence)
		if s.hasHit {
			hitCount++
			if s.hit {
				hits++
			}
		}
		if s.brier != nil {
			sumBrier.Add(sumBrier, s.brier)
			brierCount++
		}
		if s.logLoss != nil {
			sumLog.Add(sumLog, s.logLoss)
			logCount++
		}
		sumExpected += s.expectedBPS
		sumRealized += s.realizedBPS
		sumAbsErrBPS += s.absErrorBPS
	}

	meanPredicted, err := meanAt(sumProb, count, int(ProbabilityScale), int(ScoreScale))
	if err != nil {
		return CalibrationRow{}, err
	}
	meanConfidence, err := meanAt(sumConf, count, int(ProbabilityScale), int(ScoreScale))
	if err != nil {
		return CalibrationRow{}, err
	}
	var realizedFreq, brierMean, logMean ir.Decimal
	if hitCount > 0 {
		unit := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(ScoreScale)), nil)
		realizedFreq, err = meanAt(new(big.Int).Mul(big.NewInt(hits), unit), hitCount, int(ScoreScale), int(ScoreScale))
		if err != nil {
			return CalibrationRow{}, err
		}
	}
	if brierCount > 0 {
		brierMean, err = meanAt(sumBrier, brierCount, int(ScoreScale), int(ScoreScale))
		if err != nil {
			return CalibrationRow{}, err
		}
	}
	if logCount > 0 {
		logMean, err = meanAt(sumLog, logCount, int(ScoreScale), int(ScoreScale))
		if err != nil {
			return CalibrationRow{}, err
		}
	}

	lower, upper, err := bucketBounds(idx, n)
	if err != nil {
		return CalibrationRow{}, err
	}
	return CalibrationRow{
		ID:                    NewSnapshotID(),
		StrategyVersionID:     scope.StrategyVersionID,
		AgentID:               scope.AgentID,
		Mode:                  scope.Mode,
		WindowStart:           scope.WindowStart.UTC(),
		WindowEnd:             scope.WindowEnd.UTC(),
		RegimeLabel:           scope.regime(),
		BucketLower:           lower,
		BucketUpper:           upper,
		NPredictions:          len(bucket),
		MeanPredicted:         meanPredicted,
		RealizedFrequency:     realizedFreq,
		BrierMean:             brierMean,
		LogLossMean:           logMean,
		ExpectedReturnBPSMean: money.BPS(divInt(sumExpected, count)),
		RealizedReturnBPSMean: money.BPS(divInt(sumRealized, count)),
		ConfidenceMean:        meanConfidence,
		AbsErrorBPSMean:       money.BPS(divInt(sumAbsErrBPS, count)),
		ComputerVersion:       ComputerVersion,
		ComputedAt:            now.UTC(),
	}, nil
}

// meanAt divides a sum at scale from by count and reports it at scale to.
func meanAt(sum *big.Int, count int64, from, to int) (ir.Decimal, error) {
	if count <= 0 {
		return ir.Decimal{}, errs.New(errs.CodeInternal, "prediction: mean of an empty bucket")
	}
	widened, err := scaleTo(sum, from, maxInt(from, to)+2)
	if err != nil {
		return ir.Decimal{}, err
	}
	q, err := divRound(widened, big.NewInt(count), money.RoundHalfEven)
	if err != nil {
		return ir.Decimal{}, err
	}
	return rescaleInt(q, maxInt(from, to)+2, to, money.RoundHalfEven)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// divInt is integer division rounded half away from zero, for bps means.
func divInt(sum, count int64) int64 {
	if count == 0 {
		return 0
	}
	q := sum / count
	r := sum % count
	if r == 0 {
		return q
	}
	if (r < 0) != (count < 0) {
		r = -r
	}
	if 2*r >= count {
		if (sum < 0) != (count < 0) {
			return q - 1
		}
		return q + 1
	}
	return q
}

// bucketBounds returns the [lower, upper) bounds of bucket idx of n, as
// scale-6 decimals matching the numeric(7,6) columns.
func bucketBounds(idx, n int) (lower, upper ir.Decimal, err error) {
	unit := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(ProbabilityScale)), nil)
	lo := new(big.Int).Quo(new(big.Int).Mul(big.NewInt(int64(idx)), unit), big.NewInt(int64(n)))
	hi := new(big.Int).Quo(new(big.Int).Mul(big.NewInt(int64(idx+1)), unit), big.NewInt(int64(n)))
	if idx+1 == n {
		hi = unit
	}
	l := ir.Decimal{Mantissa: lo.String(), Scale: ProbabilityScale}
	u := ir.Decimal{Mantissa: hi.String(), Scale: ProbabilityScale}
	if verr := l.Validate(); verr != nil {
		return ir.Decimal{}, ir.Decimal{}, errs.Wrap(verr, errs.CodeInternal, "prediction: bucket lower bound")
	}
	if verr := u.Validate(); verr != nil {
		return ir.Decimal{}, ir.Decimal{}, errs.Wrap(verr, errs.CodeInternal, "prediction: bucket upper bound")
	}
	return l, u, nil
}

const insertCalibrationSQL = `
INSERT INTO calibration_snapshots (
    id, strategy_version_id, agent_id, mode, window_start, window_end, regime_label,
    bucket_lower, bucket_upper, n_predictions, window_predictions, window_scored,
    mean_predicted, realized_frequency,
    brier_mean, log_loss_mean, expected_return_bps_mean, realized_return_bps_mean,
    confidence_mean, abs_error_bps_mean, computer_version, computed_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7,
    $8::numeric, $9::numeric, $10, $11, $12,
    $13::numeric, $14::numeric,
    $15::numeric, $16::numeric, $17, $18,
    $19::numeric, $20, $21, $22
)`

// Persist appends the snapshot rows. calibration_snapshots is append-only:
// a recomputation writes new rows with a new computed_at rather than
// overwriting what an earlier promotion relied on.
func (c *PGCalibrator) Persist(ctx context.Context, tx pgx.Tx, rows []CalibrationRow) error {
	if tx == nil {
		return errs.New(errs.CodeInternal, "prediction: Persist requires a transaction")
	}
	for _, r := range rows {
		if r.NPredictions < 0 {
			return errs.New(errs.CodeValidationFailed, "prediction: negative bucket count")
		}
		// A row that does not state its sample is refused here rather than at
		// the database, so the message names the snapshot rather than the
		// constraint. Compute always sets both; a caller assembling a row by
		// hand has to as well.
		if r.WindowScored < 0 || r.WindowPredictions < r.WindowScored {
			return errs.Newf(errs.CodeValidationFailed,
				"prediction: snapshot claims %d scored of %d in the window", r.WindowScored, r.WindowPredictions)
		}
		expected, err := bpsColumn("expected_return_bps_mean", r.ExpectedReturnBPSMean)
		if err != nil {
			return err
		}
		realized, err := bpsColumn("realized_return_bps_mean", r.RealizedReturnBPSMean)
		if err != nil {
			return err
		}
		absError, err := bpsColumn("abs_error_bps_mean", r.AbsErrorBPSMean)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, insertCalibrationSQL,
			r.ID, r.StrategyVersionID, nullUUID(r.AgentID), string(r.Mode), r.WindowStart, r.WindowEnd, r.RegimeLabel,
			r.BucketLower.String(), r.BucketUpper.String(), r.NPredictions,
			r.WindowPredictions, r.WindowScored,
			decimalOrNil(r.MeanPredicted), decimalOrNil(r.RealizedFrequency),
			decimalOrNil(r.BrierMean), decimalOrNil(r.LogLossMean),
			expected, realized,
			decimalOrNil(r.ConfidenceMean), absError,
			r.ComputerVersion, r.ComputedAt)
		if err != nil {
			return errs.Wrap(err, errs.CodeInternal, "prediction: persist calibration snapshot")
		}
	}
	return nil
}

func decimalOrNil(d ir.Decimal) *string {
	if isUnset(d) {
		return nil
	}
	s := d.String()
	return &s
}
