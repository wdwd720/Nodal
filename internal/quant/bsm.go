package quant

import (
	"github.com/nodal/controlplane/internal/money"
)

// OptionKind is the right a European option conveys. The zero value is not
// a valid kind, so an unset kind is rejected rather than defaulted.
type OptionKind string

const (
	// Call is the right to buy the underlying at the strike at expiry.
	Call OptionKind = "CALL"
	// Put is the right to sell the underlying at the strike at expiry.
	Put OptionKind = "PUT"
)

// Valid reports whether k is Call or Put.
func (k OptionKind) Valid() bool { return k == Call || k == Put }

// Regime records which formula produced a BSMResult.
type Regime string

const (
	// RegimeAnalytic: the closed-form Black–Scholes–Merton formulas.
	RegimeAnalytic Regime = "ANALYTIC"
	// RegimeIntrinsicLimit: σ√T < 10^-TotalVolatilityFloorDigits, where the
	// option is valued at its σ√T → 0 limit, the discounted forward
	// intrinsic value. See BlackScholes.
	RegimeIntrinsicLimit Regime = "INTRINSIC_LIMIT"
)

// Input domain of BlackScholes and ImpliedVol. Inputs outside it fail with
// VALIDATION_FAILED naming the field; the limits keep every intermediate
// (in particular e^{−rT} and e^{−qT}) inside the documented domains of the
// transcendental functions.
const (
	// PriceMagnitudeDigits bounds Spot and Strike to [10^-18, 10^18].
	PriceMagnitudeDigits = 18
	// MaxTimeYears bounds TimeYears to (0, 100].
	MaxTimeYears = 100
	// MaxVolBPS bounds Vol to (0, 10] (100_000 bps = 1000% a year).
	MaxVolBPS money.BPS = 100_000
	// MaxAbsRateBPS bounds |Rate| and |DividendYield| to 1 (100% a year,
	// continuously compounded), so |rT| and |qT| never exceed 100.
	MaxAbsRateBPS money.BPS = 10_000
	// TotalVolatilityFloorDigits sets the intrinsic-limit threshold
	// σ√T < 10^-16. Below it the analytic formula would divide by a total
	// volatility carrying fewer than 24 significant digits at Scale 40,
	// while the time value it represents is below 0.4·10^-16·S.
	TotalVolatilityFloorDigits = 16
)

// BSMInput is a European option under Black–Scholes–Merton dynamics with a
// continuous dividend (or convenience / foreign) yield. Spot and Strike are
// in the same quote units per one unit of underlying; rates and volatility
// are annualised decimals (0.05 = 5%); time is in years.
type BSMInput struct {
	Kind          OptionKind
	Spot          Dec // S > 0
	Strike        Dec // K > 0
	TimeYears     Dec // T in (0, 100]
	Vol           Dec // σ in (0, 10], annualised
	Rate          Dec // r, continuously compounded risk-free rate
	DividendYield Dec // q, continuous dividend yield
}

// BSMResult holds a price and its sensitivities. Every value is a
// deterministic reference value at Scale digits, not a market price.
type BSMResult struct {
	// Price is the model value, projected onto the no-arbitrage interval
	// [discounted forward intrinsic, S·e^{−qT} (call) or K·e^{−rT} (put)].
	// The projection only ever moves a value by its rounding error.
	Price Dec
	// Delta is ∂V/∂S: in [0, e^{−qT}] for a call, [−e^{−qT}, 0] for a put.
	Delta Dec
	// Gamma is ∂²V/∂S², the same for calls and puts, never negative.
	Gamma Dec
	// Vega is ∂V/∂σ per 1.00 (100 volatility points); see VegaPerVolPoint.
	Vega Dec
	// Theta is ∂V/∂t per year (the negative of ∂V/∂T); see ThetaPerDay.
	Theta Dec
	// Rho is ∂V/∂r per 1.00 (100%) change in the rate.
	Rho Dec
	// Regime records which formula applied.
	Regime Regime
}

// VegaPerVolPoint returns Vega per one volatility point (σ + 0.01), i.e.
// Vega/100 rounded half-even to Scale digits.
func (r BSMResult) VegaPerVolPoint() Dec { return r.Vega.quo(DecFromInt64(100)) }

// ThetaPerDay returns Theta per calendar day using a 365-day year, i.e.
// Theta/365 rounded half-even to Scale digits.
func (r BSMResult) ThetaPerDay() Dec { return r.Theta.quo(DecFromInt64(365)) }

// BlackScholes prices a European option and its Greeks.
//
// With F = S·e^{(r−q)T}, d1 = ln(F/K)/(σ√T) + σ√T/2 and d2 = d1 − σ√T:
//
//	call = S·e^{−qT}·N(d1) − K·e^{−rT}·N(d2)
//	put  = K·e^{−rT}·N(−d2) − S·e^{−qT}·N(−d1)
//	Delta = e^{−qT}·N(d1) (call), −e^{−qT}·N(−d1) (put)
//	Gamma = e^{−qT}·φ(d1)/(S·σ√T)
//	Vega  = S·e^{−qT}·φ(d1)·√T
//	Theta = −S·e^{−qT}·φ(d1)·σ/(2√T) − r·K·e^{−rT}·N(d2) + q·S·e^{−qT}·N(d1) (call)
//	        −S·e^{−qT}·φ(d1)·σ/(2√T) + r·K·e^{−rT}·N(−d2) − q·S·e^{−qT}·N(−d1) (put)
//	Rho   = K·T·e^{−rT}·N(d2) (call), −K·T·e^{−rT}·N(−d2) (put)
//
// Every primitive (exp, ln, √, N, φ) is correctly rounded at 40 digits and
// every product rounds half-even, so outputs carry absolute errors of a few
// units of 10^-40, times max(1, S, K), amplified by at most 1/(σ√T) through
// d1 (and for Gamma through its own division by σ√T). Against an
// independent 320-bit oracle, randomised tests (spot 1 to 10^4, strike 0.5
// to 2 times spot, one day to 30 years, σ 1% to 300%) observe deviations
// below 10^-34 on every output, and the textbook golden cases agree to
// 10^-37. Deep in- and out-of-the-money inputs are stable: N saturates to
// exactly 0 or 1 (|d| >= 14) and the price lands on its no-arbitrage bound.
//
// Limit regime: when σ√T < 10^-16 (T → 0 or σ → 0) the option is valued at
// the σ√T → 0 limit, deterministically and without dividing by σ√T:
// Price = max(S·e^{−qT} − K·e^{−rT}, 0) for a call (mirrored for a put),
// which is the intrinsic value as T → 0 and the discounted forward
// intrinsic value as σ → 0. N(d1) and N(d2) are taken as 1, 0, or exactly
// 1/2 when S·e^{−qT} = K·e^{−rT}; Gamma and Vega are reported as 0 and
// Theta has no diffusion term. Regime is then RegimeIntrinsicLimit.
func BlackScholes(in BSMInput) (BSMResult, error) {
	if err := validateVol(in.Vol); err != nil {
		return BSMResult{}, err
	}
	core, err := newBSMCore(in.Kind, in.Spot, in.Strike, in.TimeYears, in.Rate, in.DividendYield)
	if err != nil {
		return BSMResult{}, err
	}
	return core.greeks(core.at(in.Vol, true)), nil
}

// BlackScholesPrice is BlackScholes without the Greeks; it skips φ(d1) and
// returns exactly BlackScholes(in).Price.
func BlackScholesPrice(in BSMInput) (Dec, error) {
	if err := validateVol(in.Vol); err != nil {
		return Dec{}, err
	}
	core, err := newBSMCore(in.Kind, in.Spot, in.Strike, in.TimeYears, in.Rate, in.DividendYield)
	if err != nil {
		return Dec{}, err
	}
	return core.price(core.at(in.Vol, false)), nil
}

// validateVol checks σ in (0, 10].
func validateVol(vol Dec) error {
	if vol.Sign() <= 0 || vol.Cmp(DecFromBPS(MaxVolBPS)) > 0 {
		return errField("vol", "volatility must be in (0, 10]").WithField("vol", vol.String())
	}
	return nil
}

// validateMarket checks every input except the volatility.
func validateMarket(kind OptionKind, spot, strike, t, r, q Dec) error {
	if !kind.Valid() {
		return errField("kind", "option kind must be CALL or PUT").WithField("kind", string(kind))
	}
	lo, hi := decPow10(-PriceMagnitudeDigits), decPow10(PriceMagnitudeDigits)
	if spot.Cmp(lo) < 0 || spot.Cmp(hi) > 0 {
		return errField("spot", "spot must be in [1e-18, 1e18]").WithField("spot", spot.String())
	}
	if strike.Cmp(lo) < 0 || strike.Cmp(hi) > 0 {
		return errField("strike", "strike must be in [1e-18, 1e18]").WithField("strike", strike.String())
	}
	if t.Sign() <= 0 || t.Cmp(DecFromInt64(MaxTimeYears)) > 0 {
		return errField("time_years", "time to expiry must be in (0, 100] years").WithField("time_years", t.String())
	}
	maxRate := DecFromBPS(MaxAbsRateBPS)
	if r.Abs().Cmp(maxRate) > 0 {
		return errField("rate", "rate must be in [-1, 1]").WithField("rate", r.String())
	}
	if q.Abs().Cmp(maxRate) > 0 {
		return errField("dividend_yield", "dividend yield must be in [-1, 1]").WithField("dividend_yield", q.String())
	}
	return nil
}

// bsmCore holds everything that does not depend on the volatility, so the
// implied-volatility solver computes it once.
type bsmCore struct {
	kind          OptionKind
	spot, t, r, q Dec
	sqrtT         Dec
	dfq           Dec // e^{−qT}
	sq            Dec // S·e^{−qT}
	kr            Dec // K·e^{−rT}
	m             Dec // ln(S/K) + (r − q)T = ln(F/K)
	lower, upper  Dec // no-arbitrage bounds on the price
	intrinsicN    Dec // N(d1) = N(d2) in the limit regime: 1, 1/2 or 0
	totalVolFloor Dec // 10^-TotalVolatilityFloorDigits
	one, two      Dec
}

func newBSMCore(kind OptionKind, spot, strike, t, r, q Dec) (*bsmCore, error) {
	if err := validateMarket(kind, spot, strike, t, r, q); err != nil {
		return nil, err
	}
	c := &bsmCore{kind: kind, spot: spot, t: t, r: r, q: q}
	c.one = DecFromInt64(1)
	c.two = DecFromInt64(2)
	c.totalVolFloor = decPow10(-TotalVolatilityFloorDigits)
	c.sqrtT = sqrtDec(t)
	// |qT|, |rT| <= 100 by validation, inside Exp's domain.
	c.dfq = expDec(q.Mul(t).Neg())
	c.sq = spot.Mul(c.dfq)
	c.kr = strike.Mul(expDec(r.Mul(t).Neg()))
	c.m = lnRatio(spot, strike).Add(r.Sub(q).Mul(t))
	fwdIntrinsic := c.sq.Sub(c.kr) // > 0: call in the money at the forward
	switch fwdIntrinsic.Sign() {
	case 1:
		c.intrinsicN = c.one
	case 0:
		c.intrinsicN = c.one.quo(c.two)
	default:
		c.intrinsicN = Dec{}
	}
	if kind == Call {
		c.lower, c.upper = fwdIntrinsic.Max(Dec{}), c.sq
	} else {
		c.lower, c.upper = fwdIntrinsic.Neg().Max(Dec{}), c.kr
	}
	return c, nil
}

// bsmPoint is the volatility-dependent part of an evaluation.
type bsmPoint struct {
	vol, sd Dec // σ and σ√T
	n1, n2  Dec // N(d1), N(d2)
	pdf     Dec // φ(d1); 0 in the limit regime or when not requested
	limit   bool
}

// at evaluates N(d1), N(d2) and, when needPDF, φ(d1) at volatility vol.
func (c *bsmCore) at(vol Dec, needPDF bool) bsmPoint {
	sd := vol.Mul(c.sqrtT)
	if sd.Cmp(c.totalVolFloor) < 0 {
		return bsmPoint{vol: vol, sd: sd, n1: c.intrinsicN, n2: c.intrinsicN, limit: true}
	}
	ratio := c.m.quo(sd)
	half := sd.quo(c.two)
	d1, d2 := ratio.Add(half), ratio.Sub(half)
	p := bsmPoint{vol: vol, sd: sd, n1: NormCDF(d1), n2: NormCDF(d2)}
	if needPDF {
		p.pdf = NormPDF(d1)
	}
	return p
}

// price returns the option value at p, projected onto [lower, upper]. The
// put uses N(−d) = 1 − N(d), which holds exactly for NormCDF.
func (c *bsmCore) price(p bsmPoint) Dec {
	var v Dec
	if c.kind == Call {
		v = c.sq.Mul(p.n1).Sub(c.kr.Mul(p.n2))
	} else {
		v = c.kr.Mul(c.one.Sub(p.n2)).Sub(c.sq.Mul(c.one.Sub(p.n1)))
	}
	return v.Max(c.lower).Min(c.upper)
}

// vega returns S·e^{−qT}·φ(d1)·√T at p (0 in the limit regime).
func (c *bsmCore) vega(p bsmPoint) Dec { return c.sq.Mul(p.pdf).Mul(c.sqrtT) }

// greeks assembles the full result at p (which must carry φ(d1)).
func (c *bsmCore) greeks(p bsmPoint) BSMResult {
	res := BSMResult{Price: c.price(p), Regime: RegimeAnalytic}
	var diffusion Dec // S·e^{−qT}·φ(d1)·σ/(2√T)
	if p.limit {
		res.Regime = RegimeIntrinsicLimit
	} else {
		res.Gamma = c.dfq.Mul(p.pdf).quo(c.spot).quo(p.sd)
		res.Vega = c.vega(p)
		diffusion = c.sq.Mul(p.pdf).Mul(p.vol).quo(c.sqrtT.Add(c.sqrtT))
	}
	if c.kind == Call {
		res.Delta = c.dfq.Mul(p.n1)
		res.Theta = diffusion.Neg().Sub(c.r.Mul(c.kr.Mul(p.n2))).Add(c.q.Mul(c.sq.Mul(p.n1)))
		res.Rho = c.t.Mul(c.kr.Mul(p.n2))
	} else {
		nm1, nm2 := c.one.Sub(p.n1), c.one.Sub(p.n2) // N(−d1), N(−d2)
		res.Delta = c.dfq.Mul(nm1).Neg()
		res.Theta = diffusion.Neg().Add(c.r.Mul(c.kr.Mul(nm2))).Sub(c.q.Mul(c.sq.Mul(nm1)))
		res.Rho = c.t.Mul(c.kr.Mul(nm2)).Neg()
	}
	return res
}
