package delegation

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
)

// Supply-controlled assets (migration 00763; econ.RefuseSupplyControlled).
//
// This package posts two journals: a settled swap's SIM_TRADE between the
// customer and the venue's house book, and a settled withdrawal's
// SANDBOX_WITHDRAWAL. For a platform stable asset or a bridged twin the
// database allows the first -- both sides are booked, so the asset's holdings
// do not move -- and refuses the second outright (SU001): a unit that leaves
// the books would leave the asset's holdings below its controller's
// outstanding. It leaves only through its controller (a redemption, a bridge).
//
//   - ProposeWithdrawal refuses such an asset before anything is written. A
//     withdrawal proposed anyway would be signed and executed on the chain
//     and then fail at its posting, after the units had left.
//   - RequestSwap refuses a swap that receives such an asset at a venue
//     whose house book holds none of it to spare, before anything is
//     written. A venue's house book is never a treasury book (DG008), and it
//     gets inventory either through HOUSE_FUNDING, which the database refuses
//     for such an asset, or by being paid it in earlier swaps. Holding none,
//     it could deliver only after a funding that can never be booked. A venue
//     that holds some may deliver it: the swap is an allowed SIM_TRADE, and
//     the inventory check of the admission (DELEGATION_VENUE_INVENTORY)
//     still decides whether it holds enough. Paying such an asset to a venue
//     is never refused.

// refuseSupplyControlledWithdrawal refuses a withdrawal of a
// supply-controlled asset.
func refuseSupplyControlledWithdrawal(ctx context.Context, q db.Querier, asset assets.AssetID) error {
	return econ.RefuseSupplyControlled(ctx, q, asset, "delegation: a withdrawal of it (SANDBOX_WITHDRAWAL)",
		"the database refuses a withdrawal journal for such an asset (SU001, migration 00763): a unit leaving the books "+
			"would leave its holdings below what its controller issued, so it leaves only through its controller "+
			"(a redemption, a bridge)")
}

// refuseUnfundableDelivery refuses a swap that would receive a
// supply-controlled asset from a venue whose house book holds none of it to
// spare. A request whose intent is already recorded (intentKey) is a replay:
// it is answered as it was the first time, however the venue's inventory has
// moved since, so it is never refused here.
func (s *Service) refuseUnfundableDelivery(ctx context.Context, tx pgx.Tx, account accounts.AccountID, intentKey string, v Venue,
	recv assets.AssetID) error {
	controller, err := econ.SupplyController(ctx, tx, recv)
	if err != nil || controller == "" {
		return err
	}
	var recorded bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM economic_intents WHERE account_id = $1 AND idempotency_key = $2)`,
		account, intentKey).Scan(&recorded); err != nil {
		return dbErr("look up the request's intent", err)
	}
	if recorded {
		return nil
	}
	spare, active, err := s.venueSpare(ctx, tx, v, recv)
	if err != nil || !active || spare.IsPositive() {
		return err
	}
	return econ.SupplyControlledRefusal(recv, controller, "delegation: a swap receiving it from venue "+v.Name,
		"the venue's house book holds none of it to spare, and it could get more only through a HOUSE_FUNDING the database "+
			"refuses for such an asset (SU001, migration 00763) or by being paid it in other swaps")
}
