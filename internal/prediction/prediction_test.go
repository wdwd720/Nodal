package prediction

import (
	"crypto/sha256"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/strategy/ir"
)

func at(offset time.Duration) time.Time {
	return time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC).Add(offset)
}

func validPrediction(t *testing.T) Prediction {
	t.Helper()
	sum := sha256.Sum256([]byte("information set"))
	return Prediction{
		ID:                   NewPredictionID(),
		AgentID:              NewPredictionID().String(),
		AgentVersion:         1,
		RunID:                NewPredictionID().String(),
		ActionName:           "enter",
		StrategyVersionID:    NewPredictionID().String(),
		AccountID:            NewPredictionID().String(),
		Mode:                 ModeShadow,
		InstrumentID:         instruments.NewInstrumentID(),
		Horizon:              time.Hour,
		Direction:            DirectionUp,
		ProbabilityDirection: prob(t, "0.62"),
		ExpectedReturnBPS:    money.BPS(120),
		DownsideProbability:  prob(t, "0.3"),
		MaxDownsideBPS:       money.BPS(200),
		Confidence:           prob(t, "0.55"),
		InformationSetHash:   sum[:],
		DecisionAvailableAt:  at(-time.Minute),
		CommittedAt:          at(0),
	}
}

func TestPredictionValidateAcceptsAWellFormedForecast(t *testing.T) {
	t.Parallel()
	require.NoError(t, validPrediction(t).Validate())
}

func TestPredictionValidateMirrorsEveryTableCheck(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*Prediction){
		"no id":                    func(p *Prediction) { p.ID = PredictionID{} },
		"no agent":                 func(p *Prediction) { p.AgentID = "" },
		"agent version below one":  func(p *Prediction) { p.AgentVersion = 0 },
		"no run":                   func(p *Prediction) { p.RunID = "" },
		"no action":                func(p *Prediction) { p.ActionName = "" },
		"no strategy version":      func(p *Prediction) { p.StrategyVersionID = "" },
		"no account":               func(p *Prediction) { p.AccountID = "" },
		"unknown mode":             func(p *Prediction) { p.Mode = "SIMULATED" },
		"no instrument":            func(p *Prediction) { p.InstrumentID = instruments.InstrumentID{} },
		"zero horizon":             func(p *Prediction) { p.Horizon = 0 },
		"negative horizon":         func(p *Prediction) { p.Horizon = -time.Hour },
		"unknown direction":        func(p *Prediction) { p.Direction = "SIDEWAYS" },
		"negative max downside":    func(p *Prediction) { p.MaxDownsideBPS = -1 },
		"short information hash":   func(p *Prediction) { p.InformationSetHash = []byte{1, 2, 3} },
		"no information hash":      func(p *Prediction) { p.InformationSetHash = nil },
		"no decision availability": func(p *Prediction) { p.DecisionAvailableAt = time.Time{} },
		"no commit time":           func(p *Prediction) { p.CommittedAt = time.Time{} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := validPrediction(t)
			mutate(&p)
			err := p.Validate()
			require.Error(t, err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}
}

// TestPredictionCannotClaimDataItDidNotHave is the point-in-time rule at the
// row level: the information set must have been available before the commit.
func TestPredictionCannotClaimDataItDidNotHave(t *testing.T) {
	t.Parallel()
	p := validPrediction(t)
	p.DecisionAvailableAt = p.CommittedAt.Add(time.Nanosecond)
	err := p.Validate()
	require.Error(t, err)
	e, ok := errs.As(err)
	require.True(t, ok)
	assert.Contains(t, e.Fields["decision_available_at"], "cannot use data it did not have")

	p.DecisionAvailableAt = p.CommittedAt
	require.NoError(t, p.Validate(), "availability exactly at the commit is lawful")
}

func TestDirectionAndProbabilityArePairedOrAbsent(t *testing.T) {
	t.Parallel()
	p := validPrediction(t)
	p.ProbabilityDirection = ir.Decimal{}
	require.Error(t, p.Validate(), "a direction without a probability says nothing")

	p = validPrediction(t)
	p.Direction = ""
	require.Error(t, p.Validate(), "a probability without a direction says nothing")

	p = validPrediction(t)
	p.Direction = ""
	p.ProbabilityDirection = ir.Decimal{}
	require.NoError(t, p.Validate(), "a prediction may decline to forecast a direction")
}

func TestProbabilitiesMustLieInTheUnitInterval(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"-0.1", "1.1", "2"} {
		d, err := ir.ParseDecimalString(bad)
		require.NoError(t, err)
		p := validPrediction(t)
		p.Confidence = d
		require.Errorf(t, p.Validate(), "confidence %s must be refused", bad)
	}
}

func TestNormalizeProbabilityWidensExactly(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"0":        "0.000000",
		"1":        "1.000000",
		"0.5":      "0.500000",
		"0.1234":   "0.123400",
		"0.123456": "0.123456",
	}
	for in, want := range cases {
		d, err := ir.ParseDecimalString(in)
		require.NoError(t, err)
		got, err := NormalizeProbability(d)
		require.NoError(t, err)
		assert.Equal(t, want, got.String(), "widening %s", in)
		assert.Equal(t, ProbabilityScale, got.Scale)
	}
}

func TestNormalizeProbabilityRefusesTooMuchPrecision(t *testing.T) {
	t.Parallel()
	d, err := ir.ParseDecimalString("0.1234567")
	require.NoError(t, err)
	_, err = NormalizeProbability(d)
	require.Error(t, err, "scale 7 does not fit numeric(7,6); silently rounding a probability is not acceptable")
}

// TestInformationSetHashPinsExactlyWhatWasKnown.
func TestInformationSetHashPinsExactlyWhatWasKnown(t *testing.T) {
	t.Parallel()
	items := []InformationItem{
		{Dependency: "price", InvocationID: "inv-1", OutputHash: []byte{1, 2, 3}, DecisionAvailableAt: at(-2 * time.Minute)},
		{Dependency: "onchain", InvocationID: "inv-2", OutputHash: []byte{4, 5, 6}, DecisionAvailableAt: at(-time.Minute)},
	}
	h := InformationSetHash(items)
	require.Len(t, h, sha256.Size)

	// Order does not matter: the set is what was known.
	reversed := []InformationItem{items[1], items[0]}
	assert.Equal(t, h, InformationSetHash(reversed))

	// Every field is covered.
	for i := range items {
		for name, mutate := range map[string]func(*InformationItem){
			"dependency": func(it *InformationItem) { it.Dependency += "x" },
			"invocation": func(it *InformationItem) { it.InvocationID += "x" },
			"output":     func(it *InformationItem) { it.OutputHash = []byte{9, 9, 9} },
			"available":  func(it *InformationItem) { it.DecisionAvailableAt = it.DecisionAvailableAt.Add(time.Nanosecond) },
		} {
			altered := append([]InformationItem(nil), items...)
			mutate(&altered[i])
			assert.NotEqualf(t, h, InformationSetHash(altered),
				"changing the %s of item %d must change the information set hash", name, i)
		}
	}
	assert.NotEqual(t, h, InformationSetHash(items[:1]), "dropping an item must change the hash")
	assert.Len(t, InformationSetHash(nil), sha256.Size, "an empty set still hashes")
}

func TestLatestAvailableIsTheBindingInstant(t *testing.T) {
	t.Parallel()
	items := []InformationItem{
		{Dependency: "a", DecisionAvailableAt: at(-3 * time.Minute)},
		{Dependency: "b", DecisionAvailableAt: at(-time.Minute)},
		{Dependency: "c", DecisionAvailableAt: at(-2 * time.Minute)},
	}
	assert.Equal(t, at(-time.Minute), LatestAvailable(items),
		"the whole set is usable only once its slowest member is")
	assert.True(t, LatestAvailable(nil).IsZero())
}

func TestUsableAtRefusesLookAhead(t *testing.T) {
	t.Parallel()
	items := []InformationItem{
		{Dependency: "a", DecisionAvailableAt: at(-time.Minute)},
		{Dependency: "b", DecisionAvailableAt: at(time.Minute)}, // not yet knowable
	}
	assert.False(t, UsableAt(items, at(0)), "an observation from the future must not be usable")
	assert.True(t, UsableAt(items, at(time.Minute)), "usable exactly at its availability instant")
	assert.True(t, UsableAt(items[:1], at(0)))
}

func TestHorizonEnd(t *testing.T) {
	t.Parallel()
	p := validPrediction(t)
	assert.Equal(t, p.CommittedAt.Add(time.Hour), p.HorizonEnd())
}

func TestModesMirrorTheDatabaseCheck(t *testing.T) {
	t.Parallel()
	require.Len(t, Modes(), 6)
	for _, m := range Modes() {
		assert.True(t, m.Valid())
	}
	assert.False(t, Mode("DRY_RUN").Valid())
}

func TestOutcomeValidateRefusesEarlyResolution(t *testing.T) {
	t.Parallel()
	o := validOutcome(t)
	o.ResolvedAt = o.HorizonEndAt.Add(-time.Nanosecond)
	require.Error(t, o.Validate(), "a prediction is never resolved before its horizon ends")
	o.ResolvedAt = o.HorizonEndAt
	require.NoError(t, o.Validate())
}

func TestOutcomeValidateRequiresProvenance(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*Outcome){
		"no valuation source": func(o *Outcome) { o.ValuationSource = "" },
		"no start price ref":  func(o *Outcome) { o.PriceRefStart = "" },
		"no end price ref":    func(o *Outcome) { o.PriceRefEnd = "" },
		"no resolver version": func(o *Outcome) { o.ResolverVersion = "" },
		"negative drawdown":   func(o *Outcome) { o.RealizedMaxDrawdownBPS = -1 },
		"negative abs error":  func(o *Outcome) { o.AbsReturnErrorBPS = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			o := validOutcome(t)
			mutate(&o)
			require.Error(t, o.Validate())
		})
	}
}

func validOutcome(t *testing.T) Outcome {
	t.Helper()
	hit := true
	brier, err := Brier(prob(t, "0.62"), true)
	require.NoError(t, err)
	logLoss, err := LogLoss(prob(t, "0.62"), true, DefaultEpsilon())
	require.NoError(t, err)
	return Outcome{
		ID:                     NewOutcomeID(),
		PredictionID:           NewPredictionID(),
		StrategyVersionID:      NewPredictionID().String(),
		Mode:                   ModeShadow,
		HorizonEndAt:           at(time.Hour),
		ResolvedAt:             at(time.Hour + time.Minute),
		RealizedDirection:      DirectionUp,
		RealizedReturnBPS:      money.BPS(140),
		RealizedMaxDrawdownBPS: money.BPS(30),
		DirectionHit:           &hit,
		Brier:                  brier,
		LogLoss:                logLoss,
		AbsReturnErrorBPS:      money.BPS(20),
		RegimeLabel:            UnlabelledRegime,
		ValuationSource:        "test-oracle",
		PriceRefStart:          "price-1",
		PriceRefEnd:            "price-2",
		ResolverVersion:        ResolverVersion,
	}
}

func TestCalibrationScopeValidate(t *testing.T) {
	t.Parallel()
	base := CalibrationScope{
		StrategyVersionID: NewPredictionID().String(),
		Mode:              ModeShadow,
		WindowStart:       at(-24 * time.Hour),
		WindowEnd:         at(0),
	}
	require.NoError(t, base.Validate())
	for name, mutate := range map[string]func(*CalibrationScope){
		"no version":       func(s *CalibrationScope) { s.StrategyVersionID = "" },
		"bad mode":         func(s *CalibrationScope) { s.Mode = "NOPE" },
		"inverted window":  func(s *CalibrationScope) { s.WindowStart, s.WindowEnd = s.WindowEnd, s.WindowStart },
		"empty window":     func(s *CalibrationScope) { s.WindowEnd = s.WindowStart },
		"too many buckets": func(s *CalibrationScope) { s.Buckets = 1000 },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := base
			mutate(&s)
			require.Error(t, s.Validate())
		})
	}
}

func TestBucketBoundsPartitionTheUnitInterval(t *testing.T) {
	t.Parallel()
	for _, n := range []int{2, 4, 10, 20} {
		var prevUpper string
		for i := 0; i < n; i++ {
			lo, hi, err := bucketBounds(i, n)
			require.NoError(t, err)
			assert.Equal(t, ProbabilityScale, lo.Scale)
			assert.Equal(t, ProbabilityScale, hi.Scale)
			if i > 0 {
				assert.Equalf(t, prevUpper, lo.String(), "bucket %d of %d must start where %d ended", i, n, i-1)
			}
			prevUpper = hi.String()
		}
		assert.Equal(t, "1.000000", prevUpper, "the last bucket ends at 1")
	}
}

func TestBucketIndexPlacesProbabilities(t *testing.T) {
	t.Parallel()
	cases := map[string]int{
		"0":        0,
		"0.05":     0,
		"0.0999":   0,
		"0.1":      1,
		"0.55":     5,
		"0.999999": 9,
		"1":        9, // exactly 1 belongs to the last bucket
	}
	for in, want := range cases {
		p := prob(t, in)
		got, err := bucketIndex(mustInt(p), 10)
		require.NoError(t, err)
		assert.Equalf(t, want, got, "bucket of %s", in)
	}
}

// TestBPSColumnRefusesTruncation: money.BPS is an int64 and the schema columns
// are 32-bit integers. A silent truncation would turn an absurd number into a
// plausible one, so it is refused.
func TestBPSColumnRefusesTruncation(t *testing.T) {
	t.Parallel()
	for _, ok := range []money.BPS{0, 1, -1, 10_000, -10_000, math.MaxInt32, math.MinInt32} {
		got, err := bpsColumn("field", ok)
		require.NoErrorf(t, err, "%d fits", ok)
		assert.EqualValues(t, ok, got)
	}
	for _, bad := range []money.BPS{math.MaxInt32 + 1, math.MinInt32 - 1, math.MaxInt64, math.MinInt64} {
		_, err := bpsColumn("field", bad)
		require.Errorf(t, err, "%d must be refused rather than truncated", bad)
		assert.Equal(t, errs.CodeOverflow, errs.CodeOf(err))
	}
}

// TestDecimalScaleRefusesAnOutOfRangeScale guards the uint8 narrowing that
// carries an internal working scale onto a Decimal.
func TestDecimalScaleRefusesAnOutOfRangeScale(t *testing.T) {
	t.Parallel()
	for _, ok := range []int{0, 6, 8, ir.MaxScale} {
		got, err := decimalScale(ok)
		require.NoError(t, err)
		assert.EqualValues(t, ok, got)
	}
	for _, bad := range []int{-1, ir.MaxScale + 1, 256, 1 << 20} {
		_, err := decimalScale(bad)
		require.Errorf(t, err, "scale %d must be refused", bad)
	}
}
