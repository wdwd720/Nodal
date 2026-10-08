package margin

import (
	"context"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// Supply-controlled assets (migration 00763; econ.RefuseSupplyControlled).
//
// The database moves a platform stable asset or a bridged twin only in its
// controllers' kinds and five ordinary customer movements. Of the kinds this
// package posts it allows MARGIN_COLLATERAL_MOVE and nothing else:
// MARGIN_BORROW, MARGIN_INTEREST, MARGIN_REPAY (a loan's and a deficit's),
// DEFICIT_COVERAGE, LIQUIDATION and FACILITY_FUNDING are refused for such an
// asset (SU001), and so is the paper SIM_TRADE of a leveraged buy, which books
// units against no counterparty (SU002). Those refusals arrive at COMMIT, as
// INTERNAL errors, and the worst of them in the middle of risk reduction: a
// liquidation of collateral it may not sell, into a numeraire it may not
// credit, could never complete. So margin refuses such an asset at every
// entry that would lead it into one of those kinds, before anything is
// written:
//
//   - OpenAccount, as the numeraire: every loan, interest accrual,
//     repayment, deficit and liquidation's proceeds are in it;
//   - OpenAccount, as an ISOLATED account's risky asset: it is bought by the
//     paper SIM_TRADE of a leveraged buy and sold by LIQUIDATION;
//   - OpenAccount, as an asset the policy's rules name: a rule makes an asset
//     eligible as collateral and as a position, which this policy may not
//     claim for an asset no liquidation can sell;
//   - PostCollateral: collateral is what a liquidation sells. The move itself
//     is allowed (MARGIN_COLLATERAL_MOVE); the sale it exists for is not;
//   - OpenLeveragedSpot: the bought asset (the paper SIM_TRADE, then
//     LIQUIDATION);
//   - SeedFacilities: the lending pool's and the insurance fund's capital
//     (FACILITY_FUNDING, econ.RefuseFacilityFunding).
//
// ReleaseCollateral is never refused. It moves collateral back to the wallet
// (MARGIN_COLLATERAL_MOVE, which the database allows), it reduces risk, and it
// is how collateral posted before this check leaves a margin account. Borrow,
// Repay, AccrueInterest, RepayDeficit, Liquidate and CoverDeficit move only the
// account's numeraire and what the account already holds, which the entries
// above checked when they let them in; an asset supply-controlled then stays
// so, and one that was not never becomes so once value moved in it (SU003).

// whySupplyNumeraire is why a supply-controlled asset is never a numeraire.
const whySupplyNumeraire = "every loan, interest accrual, repayment, deficit coverage and liquidation of the account is booked in its numeraire " +
	"(MARGIN_BORROW, MARGIN_INTEREST, MARGIN_REPAY, DEFICIT_COVERAGE, LIQUIDATION), kinds the database refuses for such an " +
	"asset (SU001, migration 00763): the account could borrow nothing, and a liquidation of it could never complete"

// whySupplyPosition is why a supply-controlled asset is never margin
// collateral or a margin position.
const whySupplyPosition = "a liquidation sells collateral and positions in a LIQUIDATION journal, which the database refuses for such " +
	"an asset (SU001, migration 00763), and a leveraged buy of it would be a paper SIM_TRADE against no counterparty, which " +
	"it refuses too (SU002): the account would hold collateral no liquidation could ever sell"

// refuseSupplyControlledTerms refuses an account whose numeraire, ISOLATED
// asset or policy names a supply-controlled asset.
func (s *Service) refuseSupplyControlledTerms(ctx context.Context, q db.Querier, r OpenRequest) error {
	if err := econ.RefuseSupplyControlled(ctx, q, r.NumeraireAssetID, "margin: a margin account's numeraire", whySupplyNumeraire); err != nil {
		return err
	}
	if r.IsolatedAssetID != nil {
		if err := econ.RefuseSupplyControlled(ctx, q, *r.IsolatedAssetID, "margin: an ISOLATED margin account's risky asset",
			whySupplyPosition); err != nil {
			return err
		}
	}
	return s.refuseSupplyControlledPolicy(ctx, q)
}

// refuseSupplyControlledPolicy refuses a policy whose asset rules name a
// supply-controlled asset. A rule's id that is not an asset id (every asset
// id is a UUIDv7) names no asset and is not this check's business.
func (s *Service) refuseSupplyControlledPolicy(ctx context.Context, q db.Querier) error {
	ids := make([]assets.AssetID, 0, len(s.policy.AssetRules))
	for _, rule := range s.policy.AssetRules {
		if a, err := assets.ParseAssetID(rule.AssetID); err == nil {
			ids = append(ids, a)
		}
	}
	controlled, err := econ.SupplyControllers(ctx, q, ids)
	if err != nil {
		return err
	}
	for _, a := range ids {
		if controller, ok := controlled[a]; ok {
			return econ.SupplyControlledRefusal(a, controller, "margin: margin policy "+s.policy.Version+" naming it in an asset rule",
				"a rule makes an asset eligible as margin collateral and as a margin position, and "+whySupplyPosition)
		}
	}
	return nil
}

// refuseSupplyControlledFunding refuses funding the lending pool or the
// insurance fund in a supply-controlled asset, for each facility the request
// would fund.
func refuseSupplyControlledFunding(ctx context.Context, q db.Querier, asset assets.AssetID, pool, fund money.Quantity) error {
	for _, f := range []struct {
		kind ledger.FacilityKind
		amt  money.Quantity
	}{{ledger.FacilityLendingPool, pool}, {ledger.FacilityInsuranceFund, fund}} {
		if !f.amt.IsPositive() {
			continue
		}
		if err := econ.RefuseFacilityFunding(ctx, q, f.kind, asset); err != nil {
			return err
		}
	}
	return nil
}
