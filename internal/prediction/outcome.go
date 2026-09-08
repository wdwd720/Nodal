package prediction

import (
	"context"
	"errors"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/strategy/ir"
)

// ResolverVersion identifies the resolution rules. It is recorded on every
// outcome so a later change of method is visible rather than retroactive.
const ResolverVersion = "prediction-resolver/v1"

// FlatBandBPS is the band around zero that counts as FLAT. A realized move
// inside it is neither UP nor DOWN, so a prediction of FLAT is not judged
// against noise.
const FlatBandBPS = money.BPS(5)

// Outcome is one prediction_outcomes row: what actually happened, and the
// scores that follow from it. It is written once and never revised.
type Outcome struct {
	ID                     OutcomeID
	PredictionID           PredictionID
	StrategyVersionID      string
	Mode                   Mode
	HorizonEndAt           time.Time
	ResolvedAt             time.Time
	RealizedDirection      Direction
	RealizedReturnBPS      money.BPS
	RealizedMaxDrawdownBPS money.BPS
	DirectionHit           *bool
	Brier                  ir.Decimal
	LogLoss                ir.Decimal
	AbsReturnErrorBPS      money.BPS
	RegimeLabel            string
	ValuationSource        string
	PriceRefStart          string
	PriceRefEnd            string
	ResolverVersion        string
}

// UnlabelledRegime is the default regime label.
const UnlabelledRegime = "UNLABELLED"

// Validate mirrors the prediction_outcomes CHECKs.
func (o Outcome) Validate() error {
	fields := map[string]any{}
	fail := func(k, msg string) {
		if _, dup := fields[k]; !dup {
			fields[k] = msg
		}
	}
	if o.ID.IsZero() {
		fail("id", "required")
	}
	if o.PredictionID.IsZero() {
		fail("prediction_id", "required")
	}
	if o.StrategyVersionID == "" {
		fail("strategy_version_id", "required")
	}
	if !o.Mode.Valid() {
		fail("mode", "must be one of the six modes")
	}
	if o.HorizonEndAt.IsZero() {
		fail("horizon_end_at", "required")
	}
	if o.ResolvedAt.IsZero() {
		fail("resolved_at", "required")
	}
	if !o.HorizonEndAt.IsZero() && !o.ResolvedAt.IsZero() && o.ResolvedAt.Before(o.HorizonEndAt) {
		fail("resolved_at", "a prediction is never resolved before its horizon ends")
	}
	if o.RealizedDirection != "" && !o.RealizedDirection.Valid() {
		fail("realized_direction", "must be UP, DOWN or FLAT")
	}
	if o.RealizedMaxDrawdownBPS < 0 {
		fail("realized_max_drawdown_bps", "must be >= 0")
	}
	if o.AbsReturnErrorBPS < 0 {
		fail("abs_return_error_bps", "must be >= 0")
	}
	if o.ValuationSource == "" {
		fail("valuation_source", "required: an unattributed price is not evidence")
	}
	if o.PriceRefStart == "" || o.PriceRefEnd == "" {
		fail("price_ref_start", "both price references are required")
	}
	if o.ResolverVersion == "" {
		fail("resolver_version", "required")
	}
	if !isUnset(o.Brier) && !inUnitInterval(o.Brier) {
		fail("brier", "must be between 0 and 1")
	}
	if !isUnset(o.LogLoss) && o.LogLoss.Sign() < 0 {
		fail("log_loss", "must be >= 0")
	}
	if o.RegimeLabel == "" {
		fail("regime_label", "required")
	}
	if len(fields) == 0 {
		return nil
	}
	return errs.New(errs.CodeValidationFailed, "prediction: invalid outcome").WithFields(fields)
}

// PricePoint is one observed price with its provenance and knowledge time.
type PricePoint struct {
	Ref string
	// Mantissa and Scale are the exact price; there is no float here.
	Mantissa *big.Int
	Scale    int32
	Source   string
	// ObservedAt is the provider's time, ReceivedAt ours. Only ReceivedAt
	// decides what was knowable when.
	ObservedAt time.Time
	ReceivedAt time.Time
}

// Extremes are the highest and lowest prices inside a window.
type Extremes struct {
	High PricePoint
	Low  PricePoint
}

// PriceReader supplies the prices an outcome is resolved from. Every method
// takes an explicit knowledge cut-off so a resolution can never read a price
// the platform had not received by then.
type PriceReader interface {
	// PriceAsOf returns the most recent price of the instrument whose
	// received_at is at or before asOf.
	PriceAsOf(ctx context.Context, q db.Querier, instrumentID instruments.InstrumentID, asOf time.Time) (PricePoint, error)
	// ExtremesBetween returns the extreme prices received in [from, to].
	ExtremesBetween(ctx context.Context, q db.Querier, instrumentID instruments.InstrumentID, from, to time.Time) (Extremes, error)
}

// ErrNoPrice is the cause of the NOT_FOUND returned when no price exists at
// or before the requested instant. Resolution then does not happen: an
// invented price would be worse than an unresolved prediction.
var ErrNoPrice = errors.New("prediction: no price at or before the requested instant")

// ErrStalePrice is the cause when a price exists but is too old to score
// against.
//
// The distinction matters more than it looks. PriceAsOf bounds its read from
// above -- `received_at <= asOf` -- so a resolution can never see the future,
// which is the property the whole package is built around and which held. It
// had no bound from below at all, and nothing compared either price's age to
// anything (F-59).
//
// So when a feed died, both reads returned the same row: the newest price that
// existed, however old. The return was then exactly zero, the realized
// direction FLAT, the drawdown zero, and Validate passed because both price
// references were non-empty. Every open prediction on that instrument was
// scored as a miss against a market nobody had observed -- durably, since
// prediction_outcomes carries forbid_mutation and the score cannot be
// corrected afterwards.
//
// Refusing leaves the prediction unresolved and retried, which is what the
// worker already does with ErrNoPrice. See F-60 for the accounting that makes
// a permanently unresolvable prediction visible rather than merely absent.
var ErrStalePrice = errors.New("prediction: the price is too old to resolve against")

// Resolver turns an elapsed prediction into an outcome. It is deliberately
// separate from the run: the run cannot score itself, and the resolver cannot
// change what was predicted.
type Resolver struct {
	clk    clock.Clock
	prices PriceReader
	eps    ir.Decimal
	band   money.BPS
	maxAge time.Duration
}

// NewResolver builds the resolver. maxPriceAge bounds how far before its
// knowledge cut-off each endpoint price may have been received.
//
// It is a required argument rather than a default inside the constructor, for
// the same reason WithFlatBand exists: the number decides whether an outcome is
// a measurement or a fabrication, and a caller should have to say it. A
// non-positive value is refused rather than corrected to something plausible.
func NewResolver(clk clock.Clock, prices PriceReader, maxPriceAge time.Duration) (*Resolver, error) {
	switch {
	case clk == nil:
		return nil, errs.New(errs.CodeValidationFailed, "prediction: resolver requires a clock")
	case prices == nil:
		return nil, errs.New(errs.CodeValidationFailed, "prediction: resolver requires a price reader")
	case maxPriceAge <= 0:
		return nil, errs.New(errs.CodeValidationFailed,
			"prediction: resolver requires a positive maximum price age; without one a dead feed scores every prediction FLAT")
	}
	return &Resolver{clk: clk, prices: prices, eps: DefaultEpsilon(), band: FlatBandBPS, maxAge: maxPriceAge}, nil
}

// WithFlatBand overrides the FLAT band. It is configuration, not a default
// buried in the code path.
func (r *Resolver) WithFlatBand(b money.BPS) *Resolver {
	if b < 0 {
		b = 0
	}
	out := *r
	out.band = b
	return &out
}

// Resolve computes the outcome of p. It refuses to resolve before the horizon
// has elapsed, and it reads the closing price with a knowledge cut-off of the
// horizon end, so the score can never use information from after the window
// it is meant to judge.
func (r *Resolver) Resolve(ctx context.Context, q db.Querier, p Prediction) (Outcome, error) {
	now := r.clk.Now()
	end := p.HorizonEnd()
	if now.Before(end) {
		return Outcome{}, errs.Newf(errs.CodeConflict,
			"prediction: %s cannot be resolved before its horizon ends at %s", p.ID, end.Format(time.RFC3339Nano)).
			WithField("horizon_end_at", end.Format(time.RFC3339Nano))
	}
	start, err := r.prices.PriceAsOf(ctx, q, p.InstrumentID, p.CommittedAt)
	if err != nil {
		return Outcome{}, err
	}
	finish, err := r.prices.PriceAsOf(ctx, q, p.InstrumentID, end)
	if err != nil {
		return Outcome{}, err
	}
	if err := r.usable(p, start, finish, end); err != nil {
		return Outcome{}, err
	}
	returnBPS, err := returnInBPS(start, finish)
	if err != nil {
		return Outcome{}, err
	}
	ext, err := r.prices.ExtremesBetween(ctx, q, p.InstrumentID, p.CommittedAt, end)
	if err != nil {
		return Outcome{}, err
	}
	drawdown, err := drawdownBPS(start, ext, p.Direction)
	if err != nil {
		return Outcome{}, err
	}

	realized := DirectionFlat
	switch {
	case returnBPS > r.band:
		realized = DirectionUp
	case returnBPS < -r.band:
		realized = DirectionDown
	}

	o := Outcome{
		ID:                     NewOutcomeID(),
		PredictionID:           p.ID,
		StrategyVersionID:      p.StrategyVersionID,
		Mode:                   p.Mode,
		HorizonEndAt:           end,
		ResolvedAt:             now,
		RealizedDirection:      realized,
		RealizedReturnBPS:      returnBPS,
		RealizedMaxDrawdownBPS: drawdown,
		AbsReturnErrorBPS:      absBPS(returnBPS - p.ExpectedReturnBPS),
		RegimeLabel:            UnlabelledRegime,
		ValuationSource:        finish.Source,
		PriceRefStart:          start.Ref,
		PriceRefEnd:            finish.Ref,
		ResolverVersion:        ResolverVersion,
	}
	if p.Direction != "" {
		hit := realized == p.Direction
		o.DirectionHit = &hit
		brier, err := Brier(p.ProbabilityDirection, hit)
		if err != nil {
			return Outcome{}, err
		}
		logLoss, err := LogLoss(p.ProbabilityDirection, hit, r.eps)
		if err != nil {
			return Outcome{}, err
		}
		o.Brier = brier
		o.LogLoss = logLoss
	}
	if err := o.Validate(); err != nil {
		return Outcome{}, err
	}
	return o, nil
}

// returnInBPS is (finish - start) / start in basis points, exactly. Prices
// are big.Int mantissas with scales; the division rounds half-even once, at
// the end, and never touches a float.
// usable refuses a pair of prices that cannot measure the window they are
// meant to measure. Two rules, and only the second has a number in it.
//
// The first is unconditional: the two endpoints must be different observations.
// If PriceAsOf returned the same row for the start and the finish, then no
// price arrived between the commitment and the horizon, the computed return is
// zero by construction rather than by measurement, and FLAT would be a
// statement about our data rather than about the market.
//
// The second bounds each price's age against its own cut-off. ReceivedAt is the
// timestamp used, per PricePoint: only what we had received decides what was
// knowable when, and a provider timestamp we learned about late is not evidence
// we held it.
func (r *Resolver) usable(p Prediction, start, finish PricePoint, end time.Time) error {
	if start.Ref == finish.Ref {
		return errs.Wrap(ErrStalePrice, errs.CodeStaleMarketData,
			"prediction: the window contains no price movement to score -- both endpoints resolve to one observation").
			WithField("prediction_id", p.ID.String()).
			WithField("instrument_id", p.InstrumentID.String()).
			WithField("price_ref", start.Ref).
			WithField("price_received_at", start.ReceivedAt.UTC().Format(time.RFC3339Nano)).
			WithField("committed_at", p.CommittedAt.UTC().Format(time.RFC3339Nano)).
			WithField("horizon_end_at", end.UTC().Format(time.RFC3339Nano))
	}
	for _, e := range []struct {
		name  string
		point PricePoint
		asOf  time.Time
	}{
		{"start", start, p.CommittedAt},
		{"end", finish, end},
	} {
		if age := e.asOf.Sub(e.point.ReceivedAt); age > r.maxAge {
			return errs.Wrap(ErrStalePrice, errs.CodeStaleMarketData,
				"prediction: the "+e.name+" price is older than the resolver allows").
				WithField("prediction_id", p.ID.String()).
				WithField("instrument_id", p.InstrumentID.String()).
				WithField("price_ref", e.point.Ref).
				WithField("price_age", age.String()).
				WithField("max_price_age", r.maxAge.String()).
				WithField("as_of", e.asOf.UTC().Format(time.RFC3339Nano))
		}
	}
	return nil
}

func returnInBPS(start, finish PricePoint) (money.BPS, error) {
	if start.Mantissa == nil || finish.Mantissa == nil {
		return 0, errs.New(errs.CodeStaleMarketData, "prediction: missing price mantissa")
	}
	if start.Mantissa.Sign() <= 0 {
		return 0, errs.New(errs.CodeStaleMarketData, "prediction: non-positive starting price")
	}
	// Bring both onto the higher scale.
	scale := start.Scale
	if finish.Scale > scale {
		scale = finish.Scale
	}
	s, err := widen(start.Mantissa, start.Scale, scale)
	if err != nil {
		return 0, err
	}
	f, err := widen(finish.Mantissa, finish.Scale, scale)
	if err != nil {
		return 0, err
	}
	diff := new(big.Int).Sub(f, s)
	num := new(big.Int).Mul(diff, big.NewInt(int64(money.OneHundredPercent)))
	q, err := divRound(num, s, money.RoundHalfEven)
	if err != nil {
		return 0, err
	}
	if !q.IsInt64() {
		return 0, errs.New(errs.CodeOverflow, "prediction: realized return does not fit in basis points")
	}
	return money.BPS(q.Int64()), nil
}

// drawdownBPS is the worst adverse excursion against the predicted direction,
// as a non-negative bps figure. For an UP prediction it is the deepest drop
// below the entry price; for DOWN, the highest rise above it; for FLAT or no
// direction, the larger of the two.
func drawdownBPS(start PricePoint, ext Extremes, dir Direction) (money.BPS, error) {
	worst := money.BPS(0)
	consider := func(p PricePoint, invert bool) error {
		if p.Mantissa == nil {
			return nil
		}
		bps, err := returnInBPS(start, p)
		if err != nil {
			return err
		}
		if invert {
			bps = -bps
		}
		if bps < 0 {
			bps = -bps
		} else {
			return nil
		}
		if bps > worst {
			worst = bps
		}
		return nil
	}
	switch dir {
	case DirectionUp:
		if err := consider(ext.Low, false); err != nil {
			return 0, err
		}
	case DirectionDown:
		if err := consider(ext.High, true); err != nil {
			return 0, err
		}
	default:
		if err := consider(ext.Low, false); err != nil {
			return 0, err
		}
		if err := consider(ext.High, true); err != nil {
			return 0, err
		}
	}
	return worst, nil
}

func widen(v *big.Int, from, to int32) (*big.Int, error) {
	if from == to {
		return new(big.Int).Set(v), nil
	}
	if from > to {
		return nil, errs.New(errs.CodePrecisionLoss, "prediction: price scale narrowing would round")
	}
	mul := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(to-from)), nil)
	return new(big.Int).Mul(v, mul), nil
}

func absBPS(b money.BPS) money.BPS {
	if b < 0 {
		return -b
	}
	return b
}

const priceAsOfSQL = `
SELECT p.id::text, p.mantissa::text, p.scale, p.source, p.observed_at, p.received_at
  FROM asset_prices p
  JOIN instruments i ON i.base_asset_id = p.asset_id AND i.quote_asset_id = p.quote_asset_id
 WHERE i.id = $1 AND p.received_at <= $2
 ORDER BY p.received_at DESC, p.observed_at DESC
 LIMIT 1`

const extremesSQL = `
SELECT
    (SELECT p.id::text || '|' || p.mantissa::text || '|' || p.scale::text || '|' || p.source
       FROM asset_prices p JOIN instruments i ON i.base_asset_id = p.asset_id AND i.quote_asset_id = p.quote_asset_id
      WHERE i.id = $1 AND p.received_at >= $2 AND p.received_at <= $3
      ORDER BY (p.mantissa / power(10, p.scale)) DESC, p.received_at LIMIT 1),
    (SELECT p.id::text || '|' || p.mantissa::text || '|' || p.scale::text || '|' || p.source
       FROM asset_prices p JOIN instruments i ON i.base_asset_id = p.asset_id AND i.quote_asset_id = p.quote_asset_id
      WHERE i.id = $1 AND p.received_at >= $2 AND p.received_at <= $3
      ORDER BY (p.mantissa / power(10, p.scale)) ASC, p.received_at LIMIT 1)`

// PGPriceReader reads asset_prices for the instrument's (base, quote) pair.
// Every query filters on received_at, our own knowledge time, never on the
// provider's observed_at: a provider clock cannot make the platform appear to
// have known something earlier than it did.
type PGPriceReader struct{}

var _ PriceReader = PGPriceReader{}

// NewPriceReader returns the PostgreSQL price reader.
func NewPriceReader() PGPriceReader { return PGPriceReader{} }

// PriceAsOf returns the newest price received at or before asOf.
func (PGPriceReader) PriceAsOf(ctx context.Context, q db.Querier, instrumentID instruments.InstrumentID, asOf time.Time) (PricePoint, error) {
	var (
		p        PricePoint
		mantissa string
	)
	err := q.QueryRow(ctx, priceAsOfSQL, instrumentID, asOf.UTC()).
		Scan(&p.Ref, &mantissa, &p.Scale, &p.Source, &p.ObservedAt, &p.ReceivedAt)
	switch {
	case isNoRows(err):
		return PricePoint{}, errs.Wrap(ErrNoPrice, errs.CodeStaleMarketData,
			"prediction: no price for the instrument at or before the requested instant").
			WithField("instrument_id", instrumentID.String()).
			WithField("as_of", asOf.UTC().Format(time.RFC3339Nano))
	case err != nil:
		return PricePoint{}, errs.Wrap(err, errs.CodeInternal, "prediction: read price")
	}
	v, ok := new(big.Int).SetString(mantissa, 10)
	if !ok {
		return PricePoint{}, errs.New(errs.CodeInternal, "prediction: price mantissa is not an integer")
	}
	p.Mantissa = v
	return p, nil
}

// ExtremesBetween returns the highest and lowest prices received in the
// window. A window with no prices returns zero-valued points, which the
// drawdown calculation treats as "no excursion observed" rather than as zero.
func (PGPriceReader) ExtremesBetween(ctx context.Context, q db.Querier, instrumentID instruments.InstrumentID, from, to time.Time) (Extremes, error) {
	var high, low *string
	err := q.QueryRow(ctx, extremesSQL, instrumentID, from.UTC(), to.UTC()).Scan(&high, &low)
	if err != nil && !isNoRows(err) {
		return Extremes{}, errs.Wrap(err, errs.CodeInternal, "prediction: read price extremes")
	}
	out := Extremes{}
	if high != nil {
		p, perr := parsePackedPrice(*high)
		if perr != nil {
			return Extremes{}, perr
		}
		out.High = p
	}
	if low != nil {
		p, perr := parsePackedPrice(*low)
		if perr != nil {
			return Extremes{}, perr
		}
		out.Low = p
	}
	return out, nil
}

// parsePackedPrice decodes "id|mantissa|scale|source".
func parsePackedPrice(s string) (PricePoint, error) {
	parts := splitN(s, '|', 4)
	if len(parts) != 4 {
		return PricePoint{}, errs.New(errs.CodeInternal, "prediction: malformed packed price")
	}
	v, ok := new(big.Int).SetString(parts[1], 10)
	if !ok {
		return PricePoint{}, errs.New(errs.CodeInternal, "prediction: price mantissa is not an integer")
	}
	scale, err := parseInt32(parts[2])
	if err != nil {
		return PricePoint{}, err
	}
	return PricePoint{Ref: parts[0], Mantissa: v, Scale: scale, Source: parts[3]}, nil
}

func splitN(s string, sep byte, n int) []string {
	out := make([]string, 0, n)
	start := 0
	for i := 0; i < len(s) && len(out) < n-1; i++ {
		if s[i] == sep {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func parseInt32(s string) (int32, error) {
	if s == "" {
		return 0, errs.New(errs.CodeInternal, "prediction: empty scale")
	}
	var v int32
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, errs.New(errs.CodeInternal, "prediction: non-numeric scale")
		}
		v = v*10 + int32(c-'0')
		if v > 38 {
			return 0, errs.New(errs.CodeInternal, "prediction: scale out of range")
		}
	}
	return v, nil
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
