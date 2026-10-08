package quant

import (
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// Implied-volatility solver parameters. They are fixed so that the same
// inputs always take the same path to the same digits.
const (
	// IVVolMinBPS is the lower end of the search interval: 1 bp = 0.0001.
	IVVolMinBPS money.BPS = 1
	// IVVolMaxBPS is the upper end of the search interval: 50_000 bps = 5.0.
	IVVolMaxBPS money.BPS = 50_000
	// IVToleranceDigits: the solver stops when a Newton step or the
	// half-width of the bracketing interval is at most 10^-28 in volatility.
	IVToleranceDigits = 28
	// IVMaxIterations bounds the solver. The safeguards below at least halve
	// the bracket every two iterations, so 10^-28 is reached from
	// [0.0001, 5] in about 2·log2(5·10^28) ≈ 192 iterations in the worst
	// case; typical inputs need fewer than ten.
	IVMaxIterations = 256
)

// ImpliedVolInput is BSMInput with the observed option price in place of
// the volatility.
type ImpliedVolInput struct {
	Kind          OptionKind
	Spot          Dec
	Strike        Dec
	TimeYears     Dec
	Rate          Dec
	DividendYield Dec
	// Price is the target option price, in the units of Spot and Strike.
	Price Dec
}

// ImpliedVolResult is the solution of BlackScholesPrice(σ) = Price.
type ImpliedVolResult struct {
	// Vol is the implied volatility, in [IVVolMin, IVVolMax].
	Vol Dec
	// Iterations is the number of solver iterations, each one evaluation of
	// the price and vega. The two evaluations at the interval ends and the
	// final residual evaluation are not counted; 0 means an end of the
	// interval matched the price exactly.
	Iterations int
	// Residual is BlackScholesPrice at Vol minus the target price.
	Residual Dec
}

// ImpliedVol solves BlackScholesPrice(σ) = in.Price for σ in
// [IVVolMinBPS, IVVolMaxBPS] (0.0001 to 5.0).
//
// A price outside the model's range fails with VALIDATION_FAILED and
// Fields["reason"] set to one of:
//
//   - ReasonPriceBelowIntrinsic: below max(S·e^{−qT} − K·e^{−rT}, 0) for a
//     call (mirrored for a put): no volatility can produce it;
//   - ReasonPriceAboveUpperBound: above S·e^{−qT} (call) or K·e^{−rT} (put);
//   - ReasonPriceBelowVolMin / ReasonPriceAboveVolMax: attainable only with
//     a volatility outside the search interval.
//
// Method: a safeguarded Newton iteration (Numerical Recipes' rtsafe)
// started at the Manaster–Koehler point σ* = √(2|ln(F/K)|/T), from which
// Newton converges monotonically, or at the Brenner–Subrahmanyam estimate
// when the forward is at the money. Every step keeps a bracket [lo, hi]
// with model(lo) < Price < model(hi); a Newton step that leaves the bracket
// or fails to halve the step before last is replaced by bisection, so the
// search always terminates. It stops when the step or the bracket's
// half-width is at most 10^-IVToleranceDigits. All arithmetic is
// exact-integer Dec, so the
// result is reproducible bit for bit.
//
// Where vega is tiny (deep in or out of the money, or T → 0) many
// volatilities give the same 40-digit price and the returned one is only
// one of them; the round trip through BlackScholesPrice is still exact to
// the tolerance, the volatility itself is not well determined.
func ImpliedVol(in ImpliedVolInput) (ImpliedVolResult, error) {
	core, err := newBSMCore(in.Kind, in.Spot, in.Strike, in.TimeYears, in.Rate, in.DividendYield)
	if err != nil {
		return ImpliedVolResult{}, err
	}
	target := in.Price
	if target.Cmp(core.lower) < 0 {
		return ImpliedVolResult{}, noSolution(ReasonPriceBelowIntrinsic,
			"price is below the discounted intrinsic lower bound", target).
			WithField("lower_bound", core.lower.String())
	}
	if target.Cmp(core.upper) > 0 {
		return ImpliedVolResult{}, noSolution(ReasonPriceAboveUpperBound,
			"price is above the no-arbitrage upper bound", target).
			WithField("upper_bound", core.upper.String())
	}

	vmin, vmax := DecFromBPS(IVVolMinBPS), DecFromBPS(IVVolMaxBPS)
	lo, hi := vmin, vmax
	pLo := core.price(core.at(lo, false))
	switch target.Cmp(pLo) {
	case -1:
		return ImpliedVolResult{}, noSolution(ReasonPriceBelowVolMin,
			"price requires a volatility below 0.0001", target).
			WithField("price_at_vol_min", pLo.String())
	case 0:
		return ImpliedVolResult{Vol: lo, Residual: Dec{}}, nil
	}
	pHi := core.price(core.at(hi, false))
	switch target.Cmp(pHi) {
	case 1:
		return ImpliedVolResult{}, noSolution(ReasonPriceAboveVolMax,
			"price requires a volatility above 5", target).
			WithField("price_at_vol_max", pHi.String())
	case 0:
		return ImpliedVolResult{Vol: hi, Residual: Dec{}}, nil
	}

	tol := decPow10(-IVToleranceDigits)
	sigma := core.ivStart(target, lo, hi)
	dx := hi.Sub(lo)
	dxOld := dx
	for it := 1; it <= IVMaxIterations; it++ {
		p := core.at(sigma, true)
		f := core.price(p).Sub(target)
		if f.IsZero() {
			return ImpliedVolResult{Vol: sigma, Iterations: it, Residual: f}, nil
		}
		if f.Sign() < 0 {
			lo = sigma
		} else {
			hi = sigma
		}
		if vega := core.vega(p); vega.Sign() > 0 {
			step := f.quo(vega)
			next := sigma.Sub(step)
			if step.Abs().Cmp(tol) <= 0 {
				return core.ivResult(next.Max(vmin).Min(vmax), it, target), nil
			}
			if next.Cmp(lo) > 0 && next.Cmp(hi) < 0 && step.Abs().Add(step.Abs()).Cmp(dxOld.Abs()) <= 0 {
				dxOld, dx = dx, step
				sigma = next
				continue
			}
		}
		dxOld = dx
		dx = hi.Sub(lo).quo(core.two)
		sigma = lo.Add(dx)
		if dx.Cmp(tol) <= 0 {
			return core.ivResult(sigma.Max(vmin).Min(vmax), it, target), nil
		}
	}
	// Unreachable: bisection alone reaches the tolerance well within
	// IVMaxIterations.
	return ImpliedVolResult{}, errs.New(errs.CodeInternal, "quant: implied volatility did not converge")
}

// ivStart picks the first iterate: the Manaster–Koehler point when it lies
// strictly inside (lo, hi), else the Brenner–Subrahmanyam at-the-money
// estimate Price·√(2π)/(S·e^{−qT}·√T), else the midpoint.
func (c *bsmCore) ivStart(target, lo, hi Dec) Dec {
	inside := func(s Dec) bool { return s.Cmp(lo) > 0 && s.Cmp(hi) < 0 }
	if !c.m.IsZero() {
		if s := sqrtDec(c.m.Abs().Add(c.m.Abs()).quo(c.t)); inside(s) {
			return s
		}
	}
	if den := c.sq.Mul(c.sqrtT).Mul(NormPDF(Dec{})); den.Sign() > 0 {
		if s := target.quo(den); inside(s) {
			return s
		}
	}
	return lo.Add(hi.Sub(lo).quo(c.two))
}

// ivResult evaluates the residual at the accepted volatility.
func (c *bsmCore) ivResult(vol Dec, it int, target Dec) ImpliedVolResult {
	return ImpliedVolResult{Vol: vol, Iterations: it, Residual: c.price(c.at(vol, false)).Sub(target)}
}

func noSolution(reason, detail string, price Dec) *errs.Error {
	return errInvalid("implied volatility: "+detail).
		WithField("reason", reason).
		WithField("price", price.String())
}
