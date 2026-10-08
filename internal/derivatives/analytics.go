package derivatives

import (
	"context"
	"time"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/quant"
)

// AnalyticsLabel is carried by every Analytics value. A model value says what
// the model computes from its inputs; it is not a price anyone will pay, not
// a settlement price and never posted.
const AnalyticsLabel = "REFERENCE_MODEL_VALUE: not a quote, not a settlement price, never posted"

// AnalyticsModel names the model.
const AnalyticsModel = "BLACK_SCHOLES_MERTON"

// AnalyticsDecimals is the number of decimals Analytics renders model values
// with (rounded half-even); intrinsic and time value are exact.
const AnalyticsDecimals int32 = 18

// millisPerYear is a 365-day year, the convention of quant's ThetaPerDay.
const millisPerYear int64 = 365 * 24 * 3600 * 1000

// AnalyticsInput is the model input beyond the instrument's terms.
type AnalyticsInput struct {
	// Spot is the underlying price (the index) as a mantissa at SpotScale.
	Spot      money.Quantity
	SpotScale int32
	// VolBPS is the annualised volatility, RateBPS the continuously
	// compounded rate and DividendYieldBPS the continuous yield, in basis
	// points.
	VolBPS           money.BPS
	RateBPS          money.BPS
	DividendYieldBPS money.BPS
	// Premium is the option's observed price (the mark) at the option's price
	// scale. When present the implied volatility is solved from it and the
	// time value is reported against it.
	Premium *money.Quantity
	Now     time.Time
}

// Analytics are deterministic reference values of one option at one moment.
// Decimal fields are strings so no consumer ever parses them into a float.
type Analytics struct {
	Label        string `json:"label"`
	Model        string `json:"model"`
	InstrumentID string `json:"instrument_id"`
	Right        string `json:"right"`
	Style        string `json:"style"`
	Regime       string `json:"regime"`
	Spot         string `json:"spot"`
	Strike       string `json:"strike"`
	TimeYears    string `json:"time_years"`
	Vol          string `json:"vol"`
	Price        string `json:"price"`
	Delta        string `json:"delta"`
	Gamma        string `json:"gamma"`
	Theta        string `json:"theta_per_year"`
	ThetaPerDay  string `json:"theta_per_day"`
	Vega         string `json:"vega"`
	VegaPerPoint string `json:"vega_per_vol_point"`
	// Intrinsic is the exact intrinsic value per unit at Spot.
	Intrinsic string `json:"intrinsic"`
	// TimeValue is Premium − Intrinsic, exact; empty without a premium.
	TimeValue string `json:"time_value,omitempty"`
	// ImpliedVol is solved from Premium; ImpliedVolReason says why it could
	// not be (quant's stable reason code).
	ImpliedVol       string    `json:"implied_vol,omitempty"`
	ImpliedVolReason string    `json:"implied_vol_reason,omitempty"`
	AsOf             time.Time `json:"as_of"`
}

func fixed(d quant.Dec) (string, error) {
	return d.StringFixed(AnalyticsDecimals, money.RoundHalfEven)
}

// ComputeAnalytics prices an option with Black–Scholes–Merton and returns
// its Greeks, its implied volatility from the premium, and its exact intrinsic
// and time value. It is pure and labels everything it returns as a reference
// value.
func ComputeAnalytics(s Spec, in AnalyticsInput) (Analytics, error) {
	if !s.IsOption() {
		return Analytics{}, errs.New(errs.CodeValidationFailed, "derivatives: analytics are an option's")
	}
	if s.Expiry == nil || !in.Now.Before(*s.Expiry) {
		return Analytics{}, errs.New(errs.CodeValidationFailed, "derivatives: an expired option has no model value")
	}
	if !in.Spot.IsPositive() || in.SpotScale < 0 || in.SpotScale > money.MaxPriceScale {
		return Analytics{}, errs.New(errs.CodeValidationFailed, "derivatives: analytics need a positive spot")
	}
	spot, err := quant.DecFromQuantity(in.Spot, uint8(in.SpotScale), money.RoundExact) // #nosec G115 -- checked above
	if err != nil {
		return Analytics{}, err
	}
	strike, err := quant.DecFromQuantity(s.Strike, uint8(s.PriceScale), money.RoundExact) // #nosec G115 -- 0..18
	if err != nil {
		return Analytics{}, err
	}
	years, err := quant.DecFromInt64(s.Expiry.Sub(in.Now).Milliseconds()).Quo(quant.DecFromInt64(millisPerYear), money.RoundHalfEven)
	if err != nil {
		return Analytics{}, err
	}
	kind := quant.Call
	if s.Right == instruments.Put {
		kind = quant.Put
	}
	vol := quant.DecFromBPS(in.VolBPS)
	rate, div := quant.DecFromBPS(in.RateBPS), quant.DecFromBPS(in.DividendYieldBPS)
	res, err := quant.BlackScholes(quant.BSMInput{
		Kind: kind, Spot: spot, Strike: strike, TimeYears: years, Vol: vol, Rate: rate, DividendYield: div,
	})
	if err != nil {
		return Analytics{}, err
	}
	a := Analytics{
		Label: AnalyticsLabel, Model: AnalyticsModel, InstrumentID: s.InstrumentID.String(),
		Right: string(s.Right), Style: string(s.Style), Regime: string(res.Regime), AsOf: in.Now.UTC(),
		Spot: spot.String(), Strike: strike.String(),
	}
	for _, f := range []struct {
		dst *string
		v   quant.Dec
	}{
		{&a.TimeYears, years}, {&a.Vol, vol}, {&a.Price, res.Price}, {&a.Delta, res.Delta}, {&a.Gamma, res.Gamma},
		{&a.Theta, res.Theta}, {&a.ThetaPerDay, res.ThetaPerDay()}, {&a.Vega, res.Vega}, {&a.VegaPerPoint, res.VegaPerVolPoint()},
	} {
		if *f.dst, err = fixed(f.v); err != nil {
			return Analytics{}, err
		}
	}

	// Intrinsic and time value are exact integers at a common scale.
	intrinsic, _, _, c := intrinsicAt(s, in.Spot, in.SpotScale)
	a.Intrinsic = intrinsic.ToDecimalString(uint8(c)) // #nosec G115 -- c is in [0, 38]
	if in.Premium == nil {
		return a, nil
	}
	if in.Premium.IsNegative() {
		return Analytics{}, errs.New(errs.CodeValidationFailed, "derivatives: a premium cannot be negative")
	}
	prem := commonScale(*in.Premium, s.PriceScale, c)
	a.TimeValue = prem.Sub(intrinsic).ToDecimalString(uint8(c))                               // #nosec G115 -- c is in [0, 38]
	premDec, err := quant.DecFromQuantity(*in.Premium, uint8(s.PriceScale), money.RoundExact) // #nosec G115 -- 0..18
	if err != nil {
		return Analytics{}, err
	}
	iv, err := quant.ImpliedVol(quant.ImpliedVolInput{
		Kind: kind, Spot: spot, Strike: strike, TimeYears: years, Rate: rate, DividendYield: div, Price: premDec,
	})
	switch {
	case err == nil:
		if a.ImpliedVol, err = fixed(iv.Vol); err != nil {
			return Analytics{}, err
		}
	case ivReason(err) != "":
		a.ImpliedVolReason = ivReason(err)
	default:
		return Analytics{}, err
	}
	return a, nil
}

// AnalyticsRequest asks for an option's reference analytics at its latest
// mark.
type AnalyticsRequest struct {
	InstrumentID     instruments.InstrumentID
	VolBPS           money.BPS
	RateBPS          money.BPS
	DividendYieldBPS money.BPS
}

// Analytics computes an option's reference analytics at its latest mark: the
// mark's index is the spot and the mark itself the premium the implied
// volatility is solved from. It is read-only. Nothing it returns is a
// settlement price, and nothing it returns is posted.
func (s *Service) Analytics(ctx context.Context, q db.Querier, r AnalyticsRequest) (Analytics, error) {
	spec, err := loadSpec(ctx, q, s.instruments, r.InstrumentID)
	if err != nil {
		return Analytics{}, err
	}
	m, ok, err := LatestMark(ctx, q, r.InstrumentID)
	if err != nil {
		return Analytics{}, err
	}
	if !ok || !m.HasIndex {
		return Analytics{}, errs.New(errs.CodeStaleMarketData, "derivatives: analytics need a mark with an index")
	}
	prem := m.MarkMantissa
	return ComputeAnalytics(spec, AnalyticsInput{
		Spot: m.IndexMantissa, SpotScale: m.IndexScale, VolBPS: r.VolBPS, RateBPS: r.RateBPS,
		DividendYieldBPS: r.DividendYieldBPS, Premium: &prem, Now: s.clk.Now().UTC(),
	})
}

// ivReason returns quant's stable reason code when ImpliedVol found no
// solution, and "" for any other error.
func ivReason(err error) string {
	ee, ok := errs.As(err)
	if !ok || ee.Code != errs.CodeValidationFailed {
		return ""
	}
	r, _ := ee.Fields["reason"].(string)
	return r
}
