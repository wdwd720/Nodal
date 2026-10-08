package orders

import (
	"context"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
)

// Supply-controlled assets (migration 00763; econ.RefuseSupplyControlled).
//
// Every journal this package posts is a SIM_TRADE, a kind the database allows
// for a platform stable asset or a bridged twin -- but only in a transaction
// that changes the asset's holdings by nothing (SU002). Whether a trade does
// depends on who its counterparty is:
//
//   - An internal-book trade (Service.Submit, the Pipeline, SubmitHouseOrder)
//     is between two booked parties: customer and customer, customer and
//     house, or house and house. What one gives the other receives, the
//     holdings do not move, and the database allows it ("a trade between two
//     books" in 00763). Nothing here refuses such an asset on the internal
//     book: a book quoted in a platform stable asset is the way it is meant
//     to trade.
//   - A paper-venue fill (PaperVenue) is against external simulated
//     liquidity whose side is never booked: a buy delivers base units from no
//     one and pays quote units to no one, a sale the reverse. A unit of a
//     supply-controlled asset delivered that way is one its controller never
//     issued, and one paid away that way leaves its holdings below its
//     controller's outstanding. The venue could only be a booked counterparty
//     with a book of its own funded in the asset -- HOUSE_FUNDING or
//     FACILITY_FUNDING, which the database refuses for it (SU001). So the
//     venue refuses an instrument whose base or quote is such an asset when
//     it resolves the instrument (PaperVenue.market), which every stage does
//     first: paperAccept before the intent is written, paperApprove before the
//     decision is recorded, ExecuteIntent -- the strategy marketplace's
//     paper-follow too -- before anything is reserved.
//
// A house order needs no check of its own. SubmitHouseOrder reserves the
// house book's HOUSE_INVENTORY of what it pays, so a house book that holds
// none of a supply-controlled asset is refused by the inventory check like
// any other (ORDER_HOUSE_INVENTORY_INSUFFICIENT); and a house book can hold
// one only by having bought it in an internal-book trade, because funding it
// is refused (treasury.Service.Fund, HOUSE_FUNDING). Its trades are then
// trades between two books, which the database allows.

// refuseSupplyControlledPaper refuses a paper-venue fill of an instrument
// whose base or quote asset is supply-controlled.
func refuseSupplyControlledPaper(ctx context.Context, q db.Querier, base, quote assets.AssetID) error {
	for _, a := range []struct {
		id   assets.AssetID
		role string
	}{{base, "base"}, {quote, "quote"}} {
		if err := econ.RefuseSupplyControlled(ctx, q, a.id, "orders: a paper-venue fill of an instrument whose "+a.role+" asset it is",
			"the venue's side of a fill is external and never booked, so the fill would create or destroy units of it against "+
				"no counterparty, a SIM_TRADE the database refuses for such an asset (SU002, migration 00763), and the venue "+
				"could hold it only through funding the database refuses too (HOUSE_FUNDING, FACILITY_FUNDING); it trades "+
				"on the internal book instead"); err != nil {
			return err
		}
	}
	return nil
}
