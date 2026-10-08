package derivatives

import (
	"context"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
)

// Supply-controlled assets (migration 00763; econ.RefuseSupplyControlled).
//
// Every value this package books is in an instrument's settlement asset:
// positions are contracts, never ledger assets, so neither the underlying nor
// a quote asset the terms may name is ever booked here. Of the kinds it
// posts, the database allows DERIVATIVE_MARGIN_MOVE for a platform stable
// asset or a bridged twin, and refuses every other (SU001): OPTION_PREMIUM,
// VARIATION_MARGIN, FUNDING_PAYMENT, FUTURE_SETTLEMENT, OPTION_SETTLEMENT and
// FACILITY_FUNDING. A position settled in such an asset could post its
// initial margin and then never pay a premium, a variation margin, a funding
// payment or its final settlement -- nor close at a loss.
//
// So the settlement asset is the one this package refuses, at the two entries
// that would lead it into a refused kind, before anything is written:
//
//   - OpenPosition, the one way a position comes into being outside tests:
//     every later flow -- ClosePosition, ExerciseOption, RunVariationMargin,
//     RunFunding, SettleExpiry and its overrides -- acts on positions that
//     exist, so none of them can reach such an asset once no position on it
//     can be opened. The settlement asset is also the margin asset
//     (DERIVATIVE_MARGIN_MOVE posts it), so refusing it here refuses posting
//     derivative margin in it too: the move would be allowed, the flows it
//     secures would not. An instrument's settlement asset never changes
//     (instruments.Repository.AmendTerms refuses it), and an asset
//     supply-controlled once stays so.
//   - FundClearingHouse: FACILITY_FUNDING (econ.RefuseFacilityFunding).
//
// The underlying and a quote asset are not refused: a contract on a stable
// asset's peg, settled in the sandbox dollar, moves no unit of it.

// refuseSupplyControlledSettlement refuses a position on an instrument whose
// settlement asset is supply-controlled.
func refuseSupplyControlledSettlement(ctx context.Context, q db.Querier, spec Spec) error {
	return econ.RefuseSupplyControlled(ctx, q, spec.SettlementAssetID,
		"derivatives: a position in "+spec.Symbol+", which settles in it,",
		"its initial margin could be posted (DERIVATIVE_MARGIN_MOVE), but its premium, variation margin, funding, final "+
			"settlement and exercise would be OPTION_PREMIUM, VARIATION_MARGIN, FUNDING_PAYMENT, FUTURE_SETTLEMENT and "+
			"OPTION_SETTLEMENT journals in it, which the database refuses for such an asset (SU001, migration 00763): the "+
			"position could never pay or be paid, settle, or close at a loss")
}
