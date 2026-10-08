package derivatives

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// collection is the part of a customer's obligation taken from its WALLET.
// It is reserved through capital.Reserve -- which recomputes availability
// under the account's reservation-totals lock -- before it is posted, and the
// reservation is consumed for exactly that amount right after the posting, in
// the same transaction. So a loss, a variation margin or a funding payment
// takes only funds that are not reserved or held for anything else, and the
// cash flow that took them names the reservation (the database checks it,
// DV011).
type collection struct {
	reservation capital.Reservation
	amount      money.Quantity
}

// reservationID is the reservation a collection consumed, or nil.
func (c *collection) reservationID() *capital.ReservationID {
	if c == nil {
		return nil
	}
	id := c.reservation.ID
	return &id
}

// taken is what the collection takes from the wallet (zero for none).
func (c *collection) taken() money.Quantity {
	if c == nil {
		return money.Quantity{}
	}
	return c.amount
}

// availableFunds is the customer's WALLET balance in asset less what is
// reserved and held on it (capital.Availability, not floored): the most a
// collection may take. It is negative only when something else has already
// broken the reservation invariant, and then nothing is taken.
func (s *Service) availableFunds(ctx context.Context, q db.Querier, account accounts.AccountID, asset assets.AssetID, now time.Time) (money.Quantity, error) {
	a, err := s.capital.Availability(ctx, q, account.String(), asset, now)
	if err != nil {
		return money.Quantity{}, err
	}
	return a.WalletBalance.Sub(a.Reserved).Sub(a.Held), nil
}

// reserveCollection reserves up to want of the customer's available wallet
// funds for one leg's collection (key is the leg's effect key) and returns
// the collection, or nil when nothing is available. What it cannot reserve
// the caller takes from the position's own margin and records as a shortfall:
// never from funds reserved or held for something else.
//
// A frozen account or a restricted asset has no available funds for this
// purpose: its wallet is not touched, so the obligation falls to the
// position's margin and the shortfall, exactly like an empty wallet.
func (s *Service) reserveCollection(ctx context.Context, tx pgx.Tx, account accounts.AccountID, asset assets.AssetID,
	want money.Quantity, key string, intent *econ.IntentID, reason string) (*collection, error) {
	if !want.IsPositive() {
		return nil, nil
	}
	now := s.clk.Now().UTC()
	// The availability snapshot is not locked. If another transaction
	// reserves or holds these funds before Reserve takes the account's totals
	// lock, Reserve refuses; it still holds that lock, so the second reading
	// is stable for the rest of this transaction.
	for attempt := 0; attempt < 2; attempt++ {
		free, err := s.availableFunds(ctx, tx, account, asset, now)
		if err != nil {
			return nil, err
		}
		amount := want.Min(free.Max(money.Quantity{}))
		if !amount.IsPositive() {
			return nil, nil
		}
		req := capital.ReserveRequest{
			AccountID: account.String(), AssetID: asset, Quantity: amount,
			ActorType: s.actor.Type, ActorID: s.actor.ID, IdempotencyKey: "derivatives:collect:" + key,
			TTL: s.reservationTTL, Reason: reason,
		}
		if intent != nil {
			req.EconomicIntentID = intent.String()
		}
		res, err := s.capital.Reserve(ctx, tx, req)
		switch errs.CodeOf(err) {
		case "":
			return &collection{reservation: res, amount: amount}, nil
		case errs.CodeAccountFrozen, errs.CodeAssetRestricted:
			return nil, nil
		case errs.CodeInsufficientBuyingPower:
			continue
		default:
			return nil, err
		}
	}
	return nil, errs.New(errs.CodeConflict, "derivatives: the account's available funds changed while they were being collected; retry").
		WithField("account_id", account.String())
}

// consumeCollection consumes a collection's reservation for exactly what was
// taken, after the leg that took it posted.
func (s *Service) consumeCollection(ctx context.Context, tx pgx.Tx, c *collection) error {
	if c == nil {
		return nil
	}
	_, err := s.capital.ConsumeFinal(ctx, tx, c.reservation.ID, c.amount, 0, "")
	return err
}

// releaseCollection hands a collection's reservation back unused: its leg
// had already been applied by an earlier attempt and posted nothing now.
func (s *Service) releaseCollection(ctx context.Context, tx pgx.Tx, c *collection) error {
	if c == nil || c.reservation.Status != capital.ReservationActive {
		return nil
	}
	_, err := s.capital.Release(ctx, tx, c.reservation.ID, "derivatives: the collection's leg had already been applied")
	return err
}
