package treasury

import (
	"sort"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
)

// Exposure is what the house book holds and has working, read in one snapshot
// under the book's lock.
type Exposure struct {
	// Held is the HOUSE_INVENTORY balance of the asset the request may acquire.
	Held money.Quantity
	// WorkingBuys is the remaining base quantity of every working house buy of
	// that asset, on every book.
	WorkingBuys money.Quantity
	// InstrumentWorkingBuys is the remaining notional (valuation-asset base
	// units, at limit prices) of the working house buys on the request's
	// instrument.
	InstrumentWorkingBuys money.Quantity
	// HeldValue is Held at a fresh mark, rounded up; nil when Held is positive
	// and no fresh mark exists.
	HeldValue *money.Quantity
}

// LossState is what the loss measure is made of.
type LossState struct {
	// RealizedToday is the realized PnL (signed) of every position event of
	// the current UTC trading day.
	RealizedToday money.Quantity
	// OpenLosses is the sum over every open position of min(0, unrealized PnL)
	// at a fresh mark: never positive. An open gain never offsets a loss.
	OpenLosses money.Quantity
	// MissingMarks are the assets of open positions without a fresh mark,
	// whose open loss therefore cannot be known.
	MissingMarks []assets.AssetID
}

// LimitRequest is one request evaluated against a policy.
type LimitRequest struct {
	// RiskIncreasing is true for a buy of a risky asset. Treasury limits bind
	// only risk-increasing requests: a sale reduces risk and is never blocked
	// by a loss.
	RiskIncreasing bool
	Asset          assets.AssetID
	// Quantity is the base quantity the request may acquire.
	Quantity money.Quantity
	// Notional is what the request may commit, in valuation-asset base units.
	Notional money.Quantity
	// Instrument is the book instrument, nil for the paper venue.
	Instrument *instruments.InstrumentID
	Exposure   Exposure
	Loss       LossState
}

// LimitEvaluation is the outcome of EvaluateLimits: the stable reason codes,
// the circuit breakers they become in Risk Kernel V2, and every figure they
// were decided on. It is persisted with the risk decision.
type LimitEvaluation struct {
	RiskIncreasing bool            `json:"risk_increasing"`
	Codes          []string        `json:"codes"`
	Breakers       []string        `json:"circuit_breakers"`
	LossMeasure    money.Quantity  `json:"loss_measure"`
	DailyLossLimit money.Quantity  `json:"daily_loss_limit"`
	MissingMarks   []string        `json:"missing_marks"`
	InventoryAfter money.Quantity  `json:"inventory_after"`
	InventoryCap   *money.Quantity `json:"inventory_cap"`
	ExposureAfter  *money.Quantity `json:"exposure_after"`
	ExposureCap    *money.Quantity `json:"exposure_cap"`
}

// EvaluateLimits applies a policy's limits to one request. It is pure and
// deterministic, and fails closed: an asset or instrument without a limit, or
// a mark that is not fresh, is a refusal of new risk, never a pass.
//
//   - Daily loss: LossMeasure = RealizedToday + OpenLosses; a measure below
//     -DailyLossLimit stops new risk.
//   - Inventory: Held + WorkingBuys + Quantity must not exceed the asset's cap.
//   - Instrument exposure: HeldValue + InstrumentWorkingBuys + Notional must
//     not exceed the instrument's cap.
func EvaluateLimits(p Policy, r LimitRequest) LimitEvaluation {
	ev := LimitEvaluation{
		RiskIncreasing: r.RiskIncreasing, Codes: []string{}, Breakers: []string{}, MissingMarks: []string{},
		DailyLossLimit: p.DailyLossLimit, LossMeasure: r.Loss.RealizedToday.Add(r.Loss.OpenLosses),
		InventoryAfter: r.Exposure.Held.Add(r.Exposure.WorkingBuys),
	}
	for _, a := range r.Loss.MissingMarks {
		ev.MissingMarks = append(ev.MissingMarks, a.String())
	}
	sort.Strings(ev.MissingMarks)
	if !r.RiskIncreasing {
		return ev
	}
	ev.InventoryAfter = ev.InventoryAfter.Add(r.Quantity)
	add := func(code, scope string) {
		ev.Codes = append(ev.Codes, code)
		if scope == "" {
			ev.Breakers = append(ev.Breakers, code)
		} else {
			ev.Breakers = append(ev.Breakers, code+":"+scope)
		}
	}
	for _, a := range ev.MissingMarks {
		add(ReasonMarkUnavailable, a)
	}
	if ev.LossMeasure.Add(p.DailyLossLimit).IsNegative() {
		add(ReasonDailyLossLimit, "")
	}
	if limit, ok := p.InventoryCap(r.Asset); !ok {
		add(ReasonAssetNotPermitted, r.Asset.String())
	} else {
		capCopy := limit
		ev.InventoryCap = &capCopy
		if ev.InventoryAfter.Cmp(limit) > 0 {
			add(ReasonInventoryCap, r.Asset.String())
		}
	}
	if r.Instrument != nil {
		limit, ok := p.ExposureCap(*r.Instrument)
		switch {
		case !ok:
			add(ReasonInstrumentNotPermitted, r.Instrument.String())
		case r.Exposure.HeldValue == nil:
			add(ReasonMarkUnavailable, r.Asset.String())
		default:
			capCopy := limit
			ev.ExposureCap = &capCopy
			after := r.Exposure.HeldValue.Add(r.Exposure.InstrumentWorkingBuys).Add(r.Notional)
			ev.ExposureAfter = &after
			if after.Cmp(limit) > 0 {
				add(ReasonInstrumentExposure, r.Instrument.String())
			}
		}
	}
	ev.Codes = dedupeSorted(ev.Codes)
	ev.Breakers = dedupeSorted(ev.Breakers)
	return ev
}

func dedupeSorted(in []string) []string {
	sort.Strings(in)
	out := in[:0]
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}
