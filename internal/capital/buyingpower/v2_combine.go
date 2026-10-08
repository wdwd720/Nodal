package buyingpower

import (
	"context"
	"fmt"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/money"
)

// CombinedMarginReader reports one owner's margin state from several
// subsystems at once -- the margin book and the derivatives book each own a
// part of what an account owes and must hold -- as the single state V2
// evaluates. It sums what each part owes and needs, and keeps the strictest
// initial margin rate, so combining can never show more buying power than
// either part would allow alone.
//
// Mode is CROSS when any part reports CROSS, else ISOLATED when any part
// does, else the first part's mode. A part that errors fails the whole read:
// a partial margin picture is not a smaller one, it is a wrong one.
type CombinedMarginReader []MarginReader

var _ MarginReader = CombinedMarginReader(nil)

// MarginState implements MarginReader.
func (c CombinedMarginReader) MarginState(ctx context.Context, q db.Querier, ownerType, ownerID string) (V2MarginState, bool, error) {
	var out V2MarginState
	found := false
	for i, r := range c {
		if r == nil {
			continue
		}
		st, ok, err := r.MarginState(ctx, q, ownerType, ownerID)
		if err != nil {
			return V2MarginState{}, false, err
		}
		if !ok {
			continue
		}
		if !found {
			out, found = st, true
			continue
		}
		if out, err = combineMargin(out, st); err != nil {
			return V2MarginState{}, false, fmt.Errorf("buyingpower: combine margin reader %d: %w", i, err)
		}
	}
	return out, found, nil
}

func combineMargin(a, b V2MarginState) (V2MarginState, error) {
	out := a
	pairs := []struct {
		dst  *money.USD
		with money.USD
	}{
		{&out.BorrowLiabilities, b.BorrowLiabilities},
		{&out.AccruedInterest, b.AccruedInterest},
		{&out.AccruedFees, b.AccruedFees},
		{&out.UnrealizedPnL, b.UnrealizedPnL},
		{&out.InitialMarginUsed, b.InitialMarginUsed},
		{&out.MaintenanceMargin, b.MaintenanceMargin},
	}
	for _, p := range pairs {
		sum, err := p.dst.Add(p.with)
		if err != nil {
			return V2MarginState{}, err
		}
		*p.dst = sum
	}
	if b.InitialMarginRateBPS > out.InitialMarginRateBPS {
		out.InitialMarginRateBPS = b.InitialMarginRateBPS
	}
	switch {
	case a.Mode == "CROSS" || b.Mode == "CROSS":
		out.Mode = "CROSS"
	case a.Mode == "ISOLATED" || b.Mode == "ISOLATED":
		out.Mode = "ISOLATED"
	}
	return out, nil
}
