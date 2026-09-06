package positions

import (
	"errors"
	"fmt"
	"time"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// BasisRounding is the rounding mode of every proportional allocation
// (basis, proceeds, fees). Because allocations are computed as differences
// of cumulative rounded shares, the choice of mode changes at most which
// piece carries a cent of remainder, never the total.
const BasisRounding = money.RoundHalfEven

// openLot is the in-memory view of an OPEN position_lots row used by the
// pure allocator.
type openLot struct {
	ID         LotID
	Original   money.Quantity
	Open       money.Quantity
	CostBasis  money.USD // for Original
	AcquiredAt time.Time
}

// consumed returns Original − Open.
func (l openLot) consumed() money.Quantity { return l.Original.Sub(l.Open) }

// allocation is one lot's share of a disposal.
type allocation struct {
	Lot       openLot
	Take      money.Quantity
	OpenAfter money.Quantity
	Basis     money.USD
	Proceeds  money.USD
	Fees      money.USD
}

// pnl returns Proceeds − Fees − Basis.
func (a allocation) pnl() (money.USD, error) {
	net, err := a.Proceeds.Sub(a.Fees)
	if err != nil {
		return money.USD{}, mapMoney(err)
	}
	out, err := net.Sub(a.Basis)
	if err != nil {
		return money.USD{}, mapMoney(err)
	}
	return out, nil
}

// cumulativeShare returns round(total × part / whole) in BasisRounding.
// part must lie in [0, whole] and whole must be positive; violations are
// programming errors and are reported as INTERNAL.
func cumulativeShare(total money.USD, part, whole money.Quantity) (money.USD, error) {
	if !whole.IsPositive() || part.IsNegative() || part.Cmp(whole) > 0 {
		return money.USD{}, errs.Newf(errs.CodeInternal, "positions: share %s of %s is outside [0, whole]", part, whole)
	}
	q, err := money.QuantityFromInt64(total.Minor()).MulDiv(part, whole, BasisRounding)
	if err != nil {
		return money.USD{}, mapMoney(err)
	}
	minor, err := q.Int64()
	if err != nil {
		return money.USD{}, mapMoney(err)
	}
	return money.USDFromMinor(minor), nil
}

// incrementalShare returns cumulativeShare(after) − cumulativeShare(before):
// the exact amount attributable to the units between before and after.
func incrementalShare(total money.USD, before, after, whole money.Quantity) (money.USD, error) {
	hi, err := cumulativeShare(total, after, whole)
	if err != nil {
		return money.USD{}, err
	}
	lo, err := cumulativeShare(total, before, whole)
	if err != nil {
		return money.USD{}, err
	}
	out, err := hi.Sub(lo)
	if err != nil {
		return money.USD{}, mapMoney(err)
	}
	return out, nil
}

// remainingBasis is the basis still carried by the open part of a lot:
// CostBasis − cumulativeShare(CostBasis, consumed, Original). It is exactly
// what future dispositions of the lot will be allocated in total.
func remainingBasis(l openLot) (money.USD, error) {
	used, err := cumulativeShare(l.CostBasis, l.consumed(), l.Original)
	if err != nil {
		return money.USD{}, err
	}
	out, err := l.CostBasis.Sub(used)
	if err != nil {
		return money.USD{}, mapMoney(err)
	}
	return out, nil
}

// allocateFIFO consumes qty from lots in the order given (the caller orders
// them acquired_at, id) and spreads proceeds and fees over the pieces by
// cumulative proportional rounding. It never mutates lots. An insufficient
// open quantity is VALIDATION_FAILED.
func allocateFIFO(lots []openLot, qty money.Quantity, proceeds, fees money.USD) ([]allocation, error) {
	if !qty.IsPositive() {
		return nil, errs.New(errs.CodeValidationFailed, "disposal quantity must be positive")
	}
	if proceeds.IsNegative() || fees.IsNegative() {
		return nil, errs.New(errs.CodeValidationFailed, "proceeds and fees must not be negative")
	}
	var available money.Quantity
	for _, l := range lots {
		if l.Open.IsNegative() || l.Open.Cmp(l.Original) > 0 || !l.Original.IsPositive() {
			return nil, errs.Newf(errs.CodeInternal, "positions: lot %s has inconsistent quantities", l.ID)
		}
		available = available.Add(l.Open)
	}
	if available.Cmp(qty) < 0 {
		return nil, errs.New(errs.CodeValidationFailed, "insufficient open quantity to dispose").
			WithField("requested", qty.String()).
			WithField("open", available.String())
	}

	var out []allocation
	remaining := qty
	var done money.Quantity
	for _, l := range lots {
		if remaining.IsZero() {
			break
		}
		if !l.Open.IsPositive() {
			continue
		}
		take := l.Open.Min(remaining)
		before := l.consumed()
		after := before.Add(take)
		basis, err := incrementalShare(l.CostBasis, before, after, l.Original)
		if err != nil {
			return nil, err
		}
		doneAfter := done.Add(take)
		proc, err := incrementalShare(proceeds, done, doneAfter, qty)
		if err != nil {
			return nil, err
		}
		fee, err := incrementalShare(fees, done, doneAfter, qty)
		if err != nil {
			return nil, err
		}
		out = append(out, allocation{
			Lot:       l,
			Take:      take,
			OpenAfter: l.Open.Sub(take),
			Basis:     basis,
			Proceeds:  proc,
			Fees:      fee,
		})
		remaining = remaining.Sub(take)
		done = doneAfter
	}
	if !remaining.IsZero() {
		// Unreachable: availability was checked above.
		return nil, errs.New(errs.CodeInternal, "positions: allocation did not consume the full quantity")
	}
	return out, nil
}

// mapMoney translates money sentinels to stable codes.
func mapMoney(err error) error {
	switch {
	case errors.Is(err, money.ErrOverflow):
		return errs.Wrap(err, errs.CodeOverflow, "positions: overflow")
	case errors.Is(err, money.ErrPrecisionLoss):
		return errs.Wrap(err, errs.CodePrecisionLoss, "positions: precision loss")
	}
	return errs.Wrap(err, errs.CodeInternal, fmt.Sprintf("positions: %v", err))
}
