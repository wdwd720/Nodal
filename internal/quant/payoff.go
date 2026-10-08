package quant

import (
	"strconv"

	"github.com/nodal/controlplane/internal/money"
)

// The helpers in this file are exact integer arithmetic on money.Quantity
// and need no Dec. Spot, strike and results are amounts of the same quote
// asset in its base units (for example micro-USDC) per one unit of the
// underlying; the caller scales by contract size and position.

// IntrinsicCall returns the exercise value of a call, max(spot − strike, 0),
// exactly.
func IntrinsicCall(spot, strike money.Quantity) money.Quantity {
	return spot.Sub(strike).Max(money.Quantity{})
}

// IntrinsicPut returns the exercise value of a put, max(strike − spot, 0),
// exactly.
func IntrinsicPut(spot, strike money.Quantity) money.Quantity {
	return strike.Sub(spot).Max(money.Quantity{})
}

// ScenarioGrid returns base × (1 + shock) for every shock, in order, each
// computed exactly as base × (10_000 + shock) / 10_000 and rounded once in
// mode. A shock of −10_000 bps (−100%) yields 0; shocks below that yield
// negative values, which ScenarioGridChecked rejects instead.
//
// Like money.Quantity.MulBPS, and because its signature has no error
// return, ScenarioGrid panics on the two programmer errors it cannot
// report: an invalid mode, and RoundExact (whose outcome would depend on
// the data). Neither depends on the values involved, so ScenarioGrid never
// panics on data. Use ScenarioGridChecked for RoundExact or untrusted input.
func ScenarioGrid(base money.Quantity, shocksBPS []money.BPS, mode money.RoundingMode) []money.Quantity {
	if !mode.Valid() {
		panic("quant: ScenarioGrid: invalid rounding mode " + mode.String())
	}
	if mode == money.RoundExact {
		panic("quant: ScenarioGrid does not support RoundExact; use ScenarioGridChecked")
	}
	out := make([]money.Quantity, len(shocksBPS))
	for i, s := range shocksBPS {
		v, err := shocked(base, s, mode)
		if err != nil {
			// Unreachable: the mode is valid and inexact and the divisor
			// is 10_000.
			panic("quant: ScenarioGrid: " + err.Error())
		}
		out[i] = v
	}
	return out
}

// ScenarioGridChecked is ScenarioGrid for untrusted input. It fails with
// VALIDATION_FAILED for an invalid mode, a negative base or any shock below
// −10_000 bps (a price cannot fall below zero), and with PRECISION_LOSS
// under RoundExact when a shocked value is not a whole number of base
// units.
func ScenarioGridChecked(base money.Quantity, shocksBPS []money.BPS, mode money.RoundingMode) ([]money.Quantity, error) {
	if err := checkMode(mode); err != nil {
		return nil, err
	}
	if base.IsNegative() {
		return nil, errField("base", "scenario base price must not be negative")
	}
	out := make([]money.Quantity, len(shocksBPS))
	for i, s := range shocksBPS {
		if s < -money.OneHundredPercent {
			return nil, errField("shocks_bps", "shock below -100% at index "+strconv.Itoa(i)).WithField("index", i)
		}
		v, err := shocked(base, s, mode)
		if err != nil {
			return nil, mapMoneyError(err, "scenario grid")
		}
		out[i] = v
	}
	return out, nil
}

// shocked returns base × (10_000 + shock) / 10_000 rounded in mode. The
// factor is formed in a big integer, so no shock can overflow.
func shocked(base money.Quantity, shock money.BPS, mode money.RoundingMode) (money.Quantity, error) {
	hundred := money.QuantityFromInt64(int64(money.OneHundredPercent))
	factor := money.QuantityFromInt64(int64(shock)).Add(hundred)
	return base.MulDiv(factor, hundred, mode)
}
