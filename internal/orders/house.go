package orders

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/matching"
	"github.com/nodal/controlplane/internal/money"
)

// House books.
//
// capital.Reserve reserves a customer's WALLET; a house book holds its value
// in HOUSE_INVENTORY and has no reservation totals row. This file is the
// minimal house equivalent, with the same algorithm: under the house book's
// row lock (house_accounts FOR NO KEY UPDATE serializes every reservation of that
// book), available = HOUSE_INVENTORY balance - the unconsumed remainder of the
// book's ACTIVE house reservations in that asset; a reservation is refused
// when it exceeds what is available. Consumption is by fill, a final fill or
// a cancellation closes the reservation (CONSUMED when it was used up,
// RELEASED otherwise, the unused remainder becoming available again), and
// every status change writes its transition row.

// HouseOrderRequest is an order from a house (principal) book. The treasury
// submits it on the house book's behalf; it carries no economic intent, so the
// treasury applies its own principal-trading limits before calling.
type HouseOrderRequest struct {
	InstrumentID   instruments.InstrumentID
	HouseAccountID econ.HouseAccountID
	// Mode defaults to the book's mode and must equal it.
	Mode           gates.Mode
	Side           Side
	Type           OrderType
	TimeInForce    TimeInForce
	Price          *money.Price
	Quantity       money.Quantity
	PostOnly       bool
	ReduceOnly     bool
	STP            STPMode
	ClientOrderID  string
	IdempotencyKey string
	Actor          econ.Actor
}

// CommandOutcome is the result of a command that may have reached the book.
type CommandOutcome struct {
	Order OrderView
	// CommandSeq is 0 when the command never reached the book.
	CommandSeq int64
	Trades     []Trade
	StateHash  [32]byte
	EffectID   econ.EffectID
	Evidence   econ.EvidenceRef
	// Rejection is a refusal that was recorded; nil when the command was
	// carried out.
	Rejection error
}

// SubmitHouseOrder places a house book's order in the caller's transaction.
// It is idempotent on (house book, idempotency key).
func (s *Service) SubmitHouseOrder(ctx context.Context, tx pgx.Tx, r HouseOrderRequest) (CommandOutcome, error) {
	if !isServiceActor(r.Actor) {
		return CommandOutcome{}, errs.New(errs.CodeForbidden, "orders: a house order is placed by a service or a person, never by an agent")
	}
	if err := validKey(r.IdempotencyKey); err != nil {
		return CommandOutcome{}, err
	}
	shape := orderShape{Side: r.Side, Type: r.Type, TIF: r.TimeInForce, STP: r.STP, PostOnly: r.PostOnly, ReduceOnly: r.ReduceOnly, ClientOrderID: r.ClientOrderID}
	if err := shape.validate(); err != nil {
		return CommandOutcome{}, err
	}
	shape = shape.withDefaults()
	house, err := s.registry.HouseAccount(ctx, tx, r.HouseAccountID)
	if err != nil {
		return CommandOutcome{}, err
	}
	if house.Status != econ.HouseActive {
		return CommandOutcome{}, rejection(ReasonHouseNotActive, "house book is "+string(house.Status))
	}
	c, cur, err := s.lockBook(ctx, tx, r.InstrumentID)
	if err != nil {
		return CommandOutcome{}, err
	}
	mode := r.Mode
	if mode == "" {
		mode = c.Mode
	}
	if mode != c.Mode {
		return CommandOutcome{}, errs.Newf(errs.CodeValidationFailed, "orders: this book runs in %s", c.Mode)
	}
	for _, capability := range []gates.Capability{gates.InternalMatching, gates.PrincipalTrading} {
		if err := s.auth.Require(ctx, tx, capability, mode); err != nil {
			return CommandOutcome{}, err
		}
	}
	if cur.Status != BookOpen {
		return CommandOutcome{}, rejection(ReasonBookNotOpen, "book is "+string(cur.Status))
	}
	if err := s.checkTermsVersion(ctx, tx, c); err != nil {
		return CommandOutcome{}, err
	}
	ticks, lots, err := convert(c.Terms, r.Price, r.Quantity)
	if err != nil {
		return CommandOutcome{}, err
	}
	reqHash, err := econ.RequestHash(map[string]any{
		"instrument_id": r.InstrumentID.String(), "house_account_id": r.HouseAccountID.String(), "mode": string(mode),
		"side": string(shape.Side), "order_type": string(shape.Type), "time_in_force": string(shape.TIF),
		"stp": string(shape.STP), "post_only": shape.PostOnly, "reduce_only": shape.ReduceOnly,
		"client_order_id": shape.ClientOrderID, "price_ticks": ticks, "qty_lots": lots,
	})
	if err != nil {
		return CommandOutcome{}, err
	}
	if prior, found, err := orderByKey(ctx, tx, house.ID.String(), r.IdempotencyKey); err != nil {
		return CommandOutcome{}, err
	} else if found {
		if string(prior.RequestHash) != string(reqHash) {
			return CommandOutcome{}, errs.New(errs.CodeInvalidIdempotencyReuse, "orders: this idempotency key was used for a different house order").
				WithField("order_id", prior.ID.String())
		}
		return replayOutcome(c, prior), nil
	}
	hid := house.ID
	o := orderRow{
		ID: id.New[orderKind](), BookID: c.ID, InstrumentID: c.InstrumentID, OwnerType: OwnerHouse, OwnerID: house.ID.String(),
		HouseID: &hid, IdempotencyKey: r.IdempotencyKey, RequestHash: reqHash, ClientOrderID: shape.ClientOrderID,
		Side: shape.Side, Type: shape.Type, TIF: shape.TIF, STP: shape.STP, Liquidity: liquidityIntentOf(shape.Type, shape.TIF),
		PostOnly: shape.PostOnly, ReduceOnly: shape.ReduceOnly, Mode: mode, PriceTicks: ticks, QtyLots: lots,
		RemainingLots: lots, Status: StatusPending,
	}
	if dup, err := clientOrderIDTaken(ctx, tx, o.OwnerID, o.ClientOrderID); err != nil {
		return CommandOutcome{}, err
	} else if dup {
		return s.rejectHouseOrder(ctx, tx, c, o, ReasonDuplicateClientOrderID, "this client order id is already used by the house book")
	}
	// Serialize this house book's reservations, then size this one.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM house_accounts WHERE id = $1`+bookLock, house.ID); err != nil {
		return CommandOutcome{}, dbErr("lock house book", err)
	}
	asset, amount := c.Terms.QuoteAsset, c.Terms.Notional(ticks, lots)
	if shape.Side == SideSell {
		asset, amount = c.Terms.BaseAsset, c.Terms.LotsToQuantity(lots)
	}
	available, err := s.houseAvailable(ctx, tx, house.ID.String(), asset)
	if err != nil {
		return CommandOutcome{}, err
	}
	reduceLimit := int64(0)
	if shape.ReduceOnly {
		capLots, err := available.Div(c.Terms.LotSize, money.RoundDown)
		if err != nil {
			return CommandOutcome{}, errs.Wrap(err, errs.CodeInternal, "orders: house reduce-only cap")
		}
		reduceLimit = min(clampLots(capLots), lots)
		if reduceLimit < 1 {
			return s.rejectHouseOrder(ctx, tx, c, o, ReasonReduceOnlyNoPosition, "the house book holds none of the base asset")
		}
		amount = c.Terms.LotsToQuantity(reduceLimit)
	}
	if available.Cmp(amount) < 0 {
		return s.rejectHouseOrder(ctx, tx, c, o, ReasonHouseInventory, "the house book's inventory does not cover the order")
	}
	eng, err := s.acquireEngine(ctx, tx, c, cur)
	if err != nil {
		return CommandOutcome{}, err
	}
	defer eng.release()
	now := s.clk.Now().UTC()
	cmd := matching.NewOrder{
		Timestamp: now.UnixNano(), OrderID: o.ID.String(), ClientOrderID: o.ClientOrderID, OwnerID: o.OwnerID,
		Side: o.Side.engine(), Type: o.Type.engine(), TIF: o.TIF.engine(), STP: o.STP.engine(), PriceTicks: ticks,
		QtyLots: lots, PostOnly: o.PostOnly, ReduceOnly: o.ReduceOnly, ReduceOnlyLimitLots: reduceLimit,
	}
	eff, existed, err := s.effects.Begin(ctx, tx, econ.BeginRequest{
		Key: "orders:submit:" + o.ID.String(), Subsystem: econ.SubsystemOrders, Kind: "ORDER_SUBMIT",
		Request: map[string]any{"order_id": o.ID.String(), "book_id": c.ID.String(), "owner_type": string(OwnerHouse),
			"owner_id": o.OwnerID, "request_hash": hexOf(reqHash)},
		PostsLedger: false, Actor: s.actor,
	})
	if err != nil {
		return CommandOutcome{}, err
	}
	if existed {
		return CommandOutcome{}, errs.New(errs.CodeConflict, "orders: this house order's effect was already claimed")
	}
	decision, err := s.checkCrossing(ctx, tx, c, eng, CommandNewOrder, cmd, o.ID, OwnerHouse, o.Side, ticks, false)
	if err != nil {
		return CommandOutcome{}, err
	}
	if !decision.Allowed {
		if _, err := s.effects.Fail(ctx, tx, eff.ID, ReasonHouseCrossingForbidden, s.actor, "house-versus-customer crossing is forbidden"); err != nil {
			return CommandOutcome{}, err
		}
		out, err := s.rejectHouseOrder(ctx, tx, c, o, ReasonHouseCrossingForbidden, "the order would trade a house book against a customer")
		out.EffectID, out.Evidence = eff.ID, decision.Evidence
		return out, err
	}
	rid, err := s.reserveHouse(ctx, tx, house.ID, asset, o.ID, amount)
	if err != nil {
		return CommandOutcome{}, err
	}
	o.ReservationID = rid.String()
	birth := o
	res, err := s.runCommand(ctx, tx, commandSpec{
		cfg: c, cur: cur, eng: eng, kind: CommandNewOrder, cmd: cmd, orderID: o.ID, actor: r.Actor, now: now,
		birth: &birth, crossingRuleVersion: decision.ruleVersionFor(),
	})
	if err != nil {
		return CommandOutcome{}, err
	}
	return s.finishCommandEffect(ctx, tx, c, eff, res, o.ID)
}

// finishCommandEffect applies (or, for a command the engine rejected, fails)
// the command's effect with its evidence and returns the outcome.
func (s *Service) finishCommandEffect(ctx context.Context, tx pgx.Tx, c *bookConfig, eff econ.Effect, res commandResult, oid OrderID) (CommandOutcome, error) {
	var err error
	if res.Rejected {
		eff, err = s.effects.Fail(ctx, tx, eff.ID, res.RejectReason, s.actor, "the book rejected the command")
	} else {
		eff, err = s.effects.Apply(ctx, tx, eff.ID, econ.ApplyRequest{Evidence: &res.Evidence, Actor: s.actor, Reason: "command applied to the book"})
	}
	if err != nil {
		return CommandOutcome{}, err
	}
	o, ok := res.Orders[oid.String()]
	if !ok {
		loaded, err := loadOrder(ctx, tx, oid, false)
		if err != nil {
			return CommandOutcome{}, err
		}
		o = loaded
	}
	out := CommandOutcome{Order: viewOf(c, o), CommandSeq: res.Seq, Trades: res.Trades, StateHash: res.StateHash, EffectID: eff.ID, Evidence: res.Evidence}
	if res.Rejected {
		out.Rejection = rejection(res.RejectReason, "the book rejected the command: "+res.RejectReason)
	}
	return out, nil
}

// rejectHouseOrder records a house order refused before it reached the book.
func (s *Service) rejectHouseOrder(ctx context.Context, tx pgx.Tx, c *bookConfig, o orderRow, reason, detail string) (CommandOutcome, error) {
	o.Status, o.RejectReason = StatusRejected, reason
	o.RemainingLots, o.FilledLots, o.CancelledLots, o.ReservationID = 0, 0, 0, ""
	if err := insertOrder(ctx, tx, o); err != nil {
		return CommandOutcome{}, err
	}
	return CommandOutcome{Order: viewOf(c, o), Rejection: rejection(reason, detail)}, nil
}

// CancelHouseOrder cancels a house book's resting order.
func (s *Service) CancelHouseOrder(ctx context.Context, tx pgx.Tx, houseID econ.HouseAccountID, oid OrderID, actor econ.Actor) (CommandOutcome, error) {
	if !isServiceActor(actor) {
		return CommandOutcome{}, errs.New(errs.CodeForbidden, "orders: a house order is cancelled by a service or a person")
	}
	o0, err := loadOrder(ctx, tx, oid, false)
	if err != nil {
		return CommandOutcome{}, err
	}
	if o0.OwnerType != OwnerHouse || o0.HouseID == nil || *o0.HouseID != houseID {
		return CommandOutcome{}, errs.New(errs.CodeForbidden, "orders: this order is not the house book's").WithField("reason", ReasonNotOwner)
	}
	return s.cancelLocked(ctx, tx, o0, actor)
}

// houseAvailable is HOUSE_INVENTORY minus the unconsumed remainder of the
// house book's active reservations in the asset. The caller holds the house
// book's row lock.
func (s *Service) houseAvailable(ctx context.Context, tx pgx.Tx, houseID string, asset assets.AssetID) (money.Quantity, error) {
	bal, err := s.ledger.Balance(ctx, tx, ledger.AccountRef{OwnerType: ledger.OwnerHouse, OwnerID: houseID, Code: ledger.CodeHouseInventory, AssetID: asset})
	if err != nil {
		return money.Quantity{}, err
	}
	var text string
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum(quantity - consumed_quantity), 0)::text FROM book_house_reservations
		WHERE house_account_id = $1 AND asset_id = $2 AND status = 'ACTIVE'`, houseID, asset).Scan(&text); err != nil {
		return money.Quantity{}, dbErr("house reservations", err)
	}
	reserved, err := money.ScanQuantity(text)
	if err != nil {
		return money.Quantity{}, errs.Wrap(err, errs.CodeInternal, "orders: house reservations")
	}
	return bal.Sub(reserved), nil
}

func (s *Service) reserveHouse(ctx context.Context, tx pgx.Tx, house econ.HouseAccountID, asset assets.AssetID, oid OrderID, amount money.Quantity) (HouseReservationID, error) {
	rid := id.New[houseReservationKind]()
	if _, err := tx.Exec(ctx, `INSERT INTO book_house_reservations (id, house_account_id, asset_id, order_id, quantity, status)
		VALUES ($1,$2,$3,$4,$5::numeric,'ACTIVE')`, rid, house, asset, oid, amount.String()); err != nil {
		return HouseReservationID{}, dbErr("reserve house inventory", err)
	}
	return rid, nil
}

type houseReservation struct {
	id       HouseReservationID
	quantity money.Quantity
	consumed money.Quantity
	status   HouseReservationStatus
}

func lockHouseReservation(ctx context.Context, tx pgx.Tx, rid string) (houseReservation, error) {
	var r houseReservation
	var qty, consumed string
	err := tx.QueryRow(ctx, `SELECT id, quantity::text, consumed_quantity::text, status FROM book_house_reservations WHERE id = $1`+bookLock, rid).
		Scan(&r.id, &qty, &consumed, &r.status)
	if errors.Is(err, pgx.ErrNoRows) {
		return houseReservation{}, errs.New(errs.CodeInternal, "orders: a house order's reservation is missing")
	}
	if err != nil {
		return houseReservation{}, dbErr("lock house reservation", err)
	}
	if r.quantity, err = money.ScanQuantity(qty); err != nil {
		return houseReservation{}, errs.Wrap(err, errs.CodeInternal, "orders: house reservation quantity")
	}
	if r.consumed, err = money.ScanQuantity(consumed); err != nil {
		return houseReservation{}, errs.Wrap(err, errs.CodeInternal, "orders: house reservation consumption")
	}
	return r, nil
}

// consumeHouse records a fill against a house reservation; a final fill
// closes it.
func (s *Service) consumeHouse(ctx context.Context, tx pgx.Tx, o orderRow, amount money.Quantity, final bool) error {
	r, err := lockHouseReservation(ctx, tx, o.ReservationID)
	if err != nil {
		return err
	}
	if r.status != HouseReservationActive {
		return errs.New(errs.CodeInternal, "orders: a house order traded against a closed reservation")
	}
	consumed := r.consumed.Add(amount)
	if consumed.Cmp(r.quantity) > 0 {
		return errs.New(errs.CodeInternal, "orders: a house fill exceeds its reservation")
	}
	to := HouseReservationActive
	if final || consumed.Equal(r.quantity) {
		to = HouseReservationReleased
		if consumed.Equal(r.quantity) {
			to = HouseReservationConsumed
		}
	}
	return s.setHouseReservation(ctx, tx, r, consumed, to, "order filled")
}

// releaseHouse closes a house reservation whose order no longer needs it.
func (s *Service) releaseHouse(ctx context.Context, tx pgx.Tx, o orderRow, reason string) error {
	r, err := lockHouseReservation(ctx, tx, o.ReservationID)
	if err != nil {
		return err
	}
	if r.status != HouseReservationActive {
		return nil
	}
	return s.setHouseReservation(ctx, tx, r, r.consumed, HouseReservationReleased, reason)
}

func (s *Service) setHouseReservation(ctx context.Context, tx pgx.Tx, r houseReservation, consumed money.Quantity, to HouseReservationStatus, reason string) error {
	if to != r.status {
		if _, err := tx.Exec(ctx, `INSERT INTO book_house_reservation_transitions (id, reservation_id, from_status, to_status,
			reason, actor_type, actor_id, occurred_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			id.New[id.Any](), r.id, r.status, to, reasonOr(strings.TrimSpace(reason), string(to)), s.actor.Type, s.actor.ID,
			s.clk.Now().UTC()); err != nil {
			return dbErr("record house reservation transition", err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE book_house_reservations SET consumed_quantity = $2::numeric, status = $3 WHERE id = $1`,
		r.id, consumed.String(), to); err != nil {
		return dbErr("update house reservation", err)
	}
	return nil
}

// clampLots converts a lot count that may exceed the engine's range into one
// that does not.
func clampLots(q money.Quantity) int64 {
	n, err := q.Int64()
	if err != nil || n > matching.MaxQtyLots {
		return matching.MaxQtyLots
	}
	return n
}
