package treasury

import (
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// Rounding modes of the position register, named once.
const (
	// CostRounding rounds the cost a partial disposal releases: up, so the
	// realized gain of a disposal is never overstated. A full disposal
	// releases exactly the remaining cost, so nothing leaks over a round trip.
	CostRounding = money.RoundUp
	// MarketValueRounding values a holding at a mark for PnL: down, so an
	// unrealized gain is never overstated.
	MarketValueRounding = money.RoundDown
	// ExposureRounding values a holding at a mark for an exposure limit: up,
	// so a limit is never passed by rounding.
	ExposureRounding = money.RoundUp
	// FundingCostRounding values non-cash funding at its mark: up, so an
	// unrealized gain is never created by rounding.
	FundingCostRounding = money.RoundUp
)

// Position is the house book's holding of one non-cash asset: its quantity
// in base units of the asset and its remaining cost in base units of the
// valuation asset. A zero quantity always carries zero cost.
type Position struct {
	Quantity money.Quantity
	Cost     money.Quantity
}

// Movement is one position event's effect, exactly as the register stores it
// (and the database re-checks it, TY005).
type Movement struct {
	Direction   Direction
	Quantity    money.Quantity
	Value       money.Quantity
	Fees        money.Quantity
	Before      Position
	After       Position
	CostRemoved money.Quantity
	// Realized is the signed realized PnL of the event: for an increase,
	// minus the fees; for a decrease, the proceeds less the cost released
	// and the fees.
	Realized money.Quantity
}

func (p Position) validate() error {
	if p.Quantity.IsNegative() || p.Cost.IsNegative() || (p.Quantity.IsZero() && !p.Cost.IsZero()) {
		return errs.New(errs.CodeInternal, "treasury: the position register holds an impossible position").
			WithField("quantity", p.Quantity.String()).WithField("cost", p.Cost.String())
	}
	return nil
}

func checkMovement(qty, value, fees money.Quantity) error {
	if !qty.IsPositive() || value.IsNegative() || fees.IsNegative() {
		return errs.New(errs.CodeValidationFailed, "treasury: a position event moves a positive quantity at a non-negative value and fee")
	}
	return nil
}

// Increase acquires qty at a cost of value; fees are expensed, not
// capitalized.
func (p Position) Increase(qty, value, fees money.Quantity) (Movement, error) {
	if err := p.validate(); err != nil {
		return Movement{}, err
	}
	if err := checkMovement(qty, value, fees); err != nil {
		return Movement{}, err
	}
	after := Position{Quantity: p.Quantity.Add(qty), Cost: p.Cost.Add(value)}
	return Movement{
		Direction: Increase, Quantity: qty, Value: value, Fees: fees, Before: p, After: after,
		Realized: fees.Neg(),
	}, nil
}

// Decrease disposes of qty for proceeds, releasing cost at average cost:
// ceil(Cost x qty / Quantity), or the whole remaining cost when the position
// is closed. A disposal larger than the position is refused.
func (p Position) Decrease(qty, proceeds, fees money.Quantity) (Movement, error) {
	if err := p.validate(); err != nil {
		return Movement{}, err
	}
	if err := checkMovement(qty, proceeds, fees); err != nil {
		return Movement{}, err
	}
	if qty.Cmp(p.Quantity) > 0 {
		return Movement{}, errs.New(errs.CodeConflict, "treasury: a disposal exceeds the recorded position").
			WithField("reason", ReasonInsufficientInventory).
			WithField("position", p.Quantity.String()).WithField("disposal", qty.String())
	}
	removed := p.Cost
	if qty.Cmp(p.Quantity) < 0 {
		r, err := p.Cost.MulDiv(qty, p.Quantity, CostRounding)
		if err != nil {
			return Movement{}, errs.Wrap(err, errs.CodeInternal, "treasury: average cost")
		}
		removed = r
	}
	after := Position{Quantity: p.Quantity.Sub(qty), Cost: p.Cost.Sub(removed)}
	return Movement{
		Direction: Decrease, Quantity: qty, Value: proceeds, Fees: fees, Before: p, After: after,
		CostRemoved: removed, Realized: proceeds.Sub(removed).Sub(fees),
	}, nil
}

// Unrealized is the position's unrealized PnL at a market value.
func (p Position) Unrealized(marketValue money.Quantity) money.Quantity {
	return marketValue.Sub(p.Cost)
}
