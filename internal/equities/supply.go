package equities

import (
	"context"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
)

// Supply-controlled assets (migration 00763; econ.RefuseSupplyControlled).
//
// A listing books two assets: its settlement (cash) asset and its share
// asset. A platform stable asset is a USD-pegged SIMULATED asset, so it passes
// checkCashAsset; but nothing equities posts may move it:
//
//   - the trade-date SIM_TRADE is a fill at an external venue that is never
//     booked, so it moves the customer's cash and shares against no
//     counterparty: the asset's holdings would change by what no controller
//     issued (SU002);
//   - EQUITY_SETTLEMENT moves the cash a sale delivered, and the shares a
//     purchase delivered, out of UNSETTLED_RECEIVABLE (SU001);
//   - CORPORATE_ACTION pays dividends and cash in lieu in the cash asset,
//     and adjusts the shares (SU001).
//
// So a supply-controlled cash asset is refused where a listing is made
// (ListEquity, before its share asset is created), and a listing that names
// one in either role -- one made around ListEquity, through
// instruments.Repository.CreateAdvanced -- is refused where an order or a
// corporate action begins (SubmitOrderIntent, AnnounceCorporateAction), before
// anything is written. Every fill, settlement and application follows from
// one of those, and a listing's settlement asset never changes
// (instruments.Repository.AmendTerms refuses it).

// whySupplyListing is why a listing never books a supply-controlled asset.
const whySupplyListing = "an equity trade is a paper fill at an external venue that is never booked, so its trade-date SIM_TRADE " +
	"would change the asset's holdings by what its controller never issued (SU002), and its settlement and corporate " +
	"actions (EQUITY_SETTLEMENT, CORPORATE_ACTION: dividends, cash in lieu, splits) are kinds the database refuses for " +
	"such an asset (SU001, migration 00763)"

// refuseSupplyControlledCash refuses a supply-controlled settlement asset for
// a new listing.
func refuseSupplyControlledCash(ctx context.Context, q db.Querier, cash assets.AssetID) error {
	return econ.RefuseSupplyControlled(ctx, q, cash, "equities: an equity listing's settlement (cash) asset", whySupplyListing)
}

// refuseSupplyControlledListing refuses an order or a corporate action of a
// listing that books a supply-controlled asset, as its cash or as its
// shares.
func refuseSupplyControlledListing(ctx context.Context, q db.Querier, l Listing, what string) error {
	controlled, err := econ.SupplyControllers(ctx, q, []assets.AssetID{l.CashAsset.ID, l.ShareAsset.ID})
	if err != nil {
		return err
	}
	for _, a := range []struct {
		id   assets.AssetID
		role string
	}{{l.CashAsset.ID, "settlement (cash) asset"}, {l.ShareAsset.ID, "share asset"}} {
		if controller, ok := controlled[a.id]; ok {
			return econ.SupplyControlledRefusal(a.id, controller,
				"equities: "+what+" of "+l.Terms.Terms.Symbol+", whose "+a.role+" it is,", whySupplyListing)
		}
	}
	return nil
}
