package treasury

import (
	"context"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
)

// Supply-controlled assets (migration 00763; econ.RefuseSupplyControlled).
//
// A house book holds value only in its own HOUSE accounts, and it gets value
// in two ways: HOUSE_FUNDING, and trades. For a platform stable asset or a
// bridged twin the database refuses HOUSE_FUNDING outright (SU001), and it
// allows a trade (SIM_TRADE) only when the trade moves the asset's holdings by
// nothing (SU002): a trade between two booked parties does, a fill at the
// house paper venue -- whose side is external and never booked -- never does.
//
//   - Fund refuses such an asset (HOUSE_FUNDING), before the book is locked.
//   - ExecutePaper -- and so Hedge, Convert and Rebalance -- refuses a paper
//     fill whose asset or whose quote, the book's valuation asset, is such an
//     asset, before the book is locked: a paper buy would create its units, a
//     paper sale destroy them.
//
// A house book may still come to hold such an asset, and trade it, on the
// internal book: PlaceOrder and QuoteTwoSided reach orders.SubmitHouseOrder,
// whose trades are SIM_TRADEs between the house book and its counterparty,
// both booked, which the database allows ("a house book's principal trade").
// So neither they nor Register (a book valued in a stable asset trades on
// books quoted in it) refuse it.

// refuseSupplyControlledFunding refuses funding a house book in a
// supply-controlled asset.
func refuseSupplyControlledFunding(ctx context.Context, q db.Querier, asset assets.AssetID) error {
	return econ.RefuseSupplyControlled(ctx, q, asset, "treasury: funding a house book in it (HOUSE_FUNDING)",
		"a house book's units of it would be units no issuance backs, and the database refuses a HOUSE_FUNDING journal for "+
			"such an asset (SU001, migration 00763); a house book holds it only by trading for it on the internal book")
}

// refuseSupplyControlledPaper refuses a house paper fill of asset against the
// book's valuation asset when either is supply-controlled. It reads the book
// without locking it, so that the refusal comes before anything is written.
func (s *Service) refuseSupplyControlledPaper(ctx context.Context, q db.Querier, house econ.HouseAccountID, asset assets.AssetID) error {
	book, err := s.GetBook(ctx, q, house)
	if err != nil {
		return err
	}
	controlled, err := econ.SupplyControllers(ctx, q, []assets.AssetID{asset, book.ValuationAsset})
	if err != nil {
		return err
	}
	for _, a := range []struct {
		id   assets.AssetID
		role string
	}{{asset, "the asset"}, {book.ValuationAsset, "the quote (the book's valuation asset)"}} {
		if controller, ok := controlled[a.id]; ok {
			return econ.SupplyControlledRefusal(a.id, controller, "treasury: a house paper-venue fill in which it is "+a.role,
				"the paper venue's side of a fill is external and never booked, so the fill would create or destroy units of it "+
					"against no counterparty, a SIM_TRADE the database refuses for such an asset (SU002, migration 00763); the "+
					"house book trades it on the internal book instead")
		}
	}
	return nil
}
