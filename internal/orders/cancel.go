package orders

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/matching"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
)

// CancelRequest cancels a customer's order.
type CancelRequest struct {
	OrderID   OrderID
	AccountID accounts.AccountID
	ActorType security.ActorType
	ActorID   string
}

// actorOf is who a customer command is recorded against: the person who
// asked, or the service when the request came from an agent (an agent never
// writes a transition row).
func (s *Service) actorOf(t security.ActorType, actorID string) econ.Actor {
	a := econ.Actor{Type: t, ID: actorID}
	if isServiceActor(a) {
		return a
	}
	return s.actor
}

// Cancel cancels a customer order. A resting order is cancelled on the book
// (serialized with every other command by the book's lock, so a cancel and an
// aggressive order racing for the same resting order have exactly one
// outcome); an order that has not reached the book yet is cancelled with its
// intent. Cancelling an order already cancelled is a no-op; a filled order
// cannot be cancelled.
func (s *Service) Cancel(ctx context.Context, tx pgx.Tx, r CancelRequest) (CommandOutcome, error) {
	o0, err := loadOrder(ctx, tx, r.OrderID, false)
	if err != nil {
		return CommandOutcome{}, err
	}
	if o0.OwnerType != OwnerCustomer || o0.AccountID == nil || *o0.AccountID != r.AccountID {
		return CommandOutcome{}, errs.New(errs.CodeForbidden, "orders: this order is not the account's").WithField("reason", ReasonNotOwner)
	}
	return s.cancelLocked(ctx, tx, o0, s.actorOf(r.ActorType, r.ActorID))
}

func (s *Service) cancelLocked(ctx context.Context, tx pgx.Tx, o0 orderRow, actor econ.Actor) (CommandOutcome, error) {
	c, cur, err := s.lockBook(ctx, tx, o0.InstrumentID)
	if err != nil {
		return CommandOutcome{}, err
	}
	o, err := loadOrder(ctx, tx, o0.ID, true)
	if err != nil {
		return CommandOutcome{}, err
	}
	switch o.Status {
	case StatusCancelled:
		return CommandOutcome{Order: viewOf(c, o)}, nil
	case StatusFilled, StatusRejected:
		return CommandOutcome{Order: viewOf(c, o), Rejection: rejection(ReasonNotOpen, "the order is "+string(o.Status))}, nil
	case StatusPending:
		return s.cancelBeforeBook(ctx, tx, c, o, actor)
	}
	eng, err := s.acquireEngine(ctx, tx, c, cur)
	if err != nil {
		return CommandOutcome{}, err
	}
	defer eng.release()
	now := s.clk.Now().UTC()
	eff, existed, err := s.effects.Begin(ctx, tx, econ.BeginRequest{
		Key: "orders:cancel:" + o.ID.String(), Subsystem: econ.SubsystemOrders, Kind: "ORDER_CANCEL", IntentID: o.IntentID,
		AccountID: customerAccount(o), Request: map[string]any{"order_id": o.ID.String(), "action": "cancel"},
		PostsLedger: false, Actor: s.actor,
	})
	if err != nil {
		return CommandOutcome{}, err
	}
	if existed {
		return CommandOutcome{}, errs.New(errs.CodeConflict, "orders: a cancel of this order was already recorded").WithField("effect_id", eff.ID.String())
	}
	res, err := s.runCommand(ctx, tx, commandSpec{
		cfg: c, cur: cur, eng: eng, kind: CommandCancel,
		cmd:     matching.Cancel{Timestamp: now.UnixNano(), OrderID: o.ID.String(), OwnerID: o.OwnerID},
		orderID: o.ID, actor: actor, now: now,
	})
	if err != nil {
		return CommandOutcome{}, err
	}
	return s.finishCommandEffect(ctx, tx, c, eff, res, o.ID)
}

func customerAccount(o orderRow) string {
	if o.OwnerType == OwnerCustomer {
		return o.OwnerID
	}
	return ""
}

// cancelBeforeBook ends a customer order that never reached the book: the
// order is CANCELLED with nothing filled, its reservation (if any) released,
// and its intent CANCELLED.
func (s *Service) cancelBeforeBook(ctx context.Context, tx pgx.Tx, c *bookConfig, o orderRow, actor econ.Actor) (CommandOutcome, error) {
	if o.IntentID == nil {
		return CommandOutcome{}, errs.New(errs.CodeInternal, "orders: a pending order without an intent")
	}
	in, err := lockIntent(ctx, tx, s.intents, *o.IntentID)
	if err != nil {
		return CommandOutcome{}, err
	}
	switch in.Status {
	case econ.StatusReceived, econ.StatusRouted, econ.StatusRiskApproved, econ.StatusReserved:
	default:
		return CommandOutcome{}, errs.Newf(errs.CodeInternal, "orders: a pending order's intent is %s", in.Status)
	}
	after := o
	after.Status, after.CancelReason = StatusCancelled, ReasonCancelledBeforeBook
	after.CancelledLots, after.RemainingLots = o.QtyLots, 0
	if err := updateOrder(ctx, tx, o, after, nil, actor, "cancelled before reaching the book", s.clk.Now().UTC()); err != nil {
		return CommandOutcome{}, err
	}
	if err := s.release(ctx, tx, o, "order cancelled before reaching the book"); err != nil {
		return CommandOutcome{}, err
	}
	if _, err := s.intents.Transition(ctx, tx, in.ID, econ.StatusCancelled, actor, "cancelled before reaching the book", ""); err != nil {
		return CommandOutcome{}, err
	}
	return CommandOutcome{Order: viewOf(c, after)}, nil
}

// ReplaceRequest changes a resting customer order's price and quantity.
// NewQuantity is the new TOTAL quantity in base units, fills included.
type ReplaceRequest struct {
	OrderID        OrderID
	AccountID      accounts.AccountID
	NewPrice       money.Price
	NewQuantity    money.Quantity
	IdempotencyKey string
	ActorType      security.ActorType
	ActorID        string
}

// Replace changes a resting order. Everything that could refuse it is decided
// before anything moves: the risk kernel (for a replace that adds risk), the
// engine's own rules and the crossing policy (both through a dry run on a
// copy of the book), and the reservation the new order needs (resized inside
// a savepoint). Only then is the command applied, and it may trade. It is
// idempotent on (order, idempotency key).
func (s *Service) Replace(ctx context.Context, tx pgx.Tx, r ReplaceRequest) (CommandOutcome, error) {
	if err := validKey(r.IdempotencyKey); err != nil {
		return CommandOutcome{}, err
	}
	o0, err := loadOrder(ctx, tx, r.OrderID, false)
	if err != nil {
		return CommandOutcome{}, err
	}
	if o0.OwnerType != OwnerCustomer || o0.AccountID == nil || *o0.AccountID != r.AccountID {
		return CommandOutcome{}, errs.New(errs.CodeForbidden, "orders: this order is not the account's").WithField("reason", ReasonNotOwner)
	}
	actor := s.actorOf(r.ActorType, r.ActorID)
	c, cur, err := s.lockBook(ctx, tx, o0.InstrumentID)
	if err != nil {
		return CommandOutcome{}, err
	}
	o, err := loadOrder(ctx, tx, o0.ID, true)
	if err != nil {
		return CommandOutcome{}, err
	}
	ticks, lots, err := convert(c.Terms, &r.NewPrice, r.NewQuantity)
	if err != nil {
		return CommandOutcome{}, err
	}
	in, err := lockIntent(ctx, tx, s.intents, *o.IntentID)
	if err != nil {
		return CommandOutcome{}, err
	}
	key := "orders:replace:" + o.ID.String() + ":" + r.IdempotencyKey
	eff, existed, err := s.effects.Begin(ctx, tx, econ.BeginRequest{
		Key: key, Subsystem: econ.SubsystemOrders, Kind: "ORDER_REPLACE", IntentID: o.IntentID, AccountID: o.OwnerID,
		Request:     map[string]any{"order_id": o.ID.String(), "new_price_ticks": ticks, "new_qty_lots": lots},
		PostsLedger: false, Actor: s.actor, CorrelationID: in.CorrelationID,
	})
	if err != nil {
		return CommandOutcome{}, err
	}
	if existed {
		out := CommandOutcome{Order: viewOf(c, o), EffectID: eff.ID}
		switch eff.Status {
		case econ.EffectApplied:
			return out, nil
		case econ.EffectFailed:
			out.Rejection = rejection(eff.FailureCode, "this replace was refused: "+eff.FailureCode)
			return out, nil
		}
		return CommandOutcome{}, errs.New(errs.CodeConflict, "orders: this replace is already in progress")
	}
	refuse := func(reason string, rej error) (CommandOutcome, error) {
		failed, err := s.effects.Fail(ctx, tx, eff.ID, reason, s.actor, "replace refused")
		if err != nil {
			return CommandOutcome{}, err
		}
		return CommandOutcome{Order: viewOf(c, o), EffectID: failed.ID, Rejection: rej}, nil
	}
	if !o.Status.Resting() {
		return refuse(ReasonNotOpen, rejection(ReasonNotOpen, "the order is "+string(o.Status)))
	}
	if cur.Status != BookOpen {
		return refuse(ReasonBookNotOpen, rejection(ReasonBookNotOpen, "book is "+string(cur.Status)))
	}
	if err := s.checkTermsVersion(ctx, tx, c); err != nil {
		if ReasonOf(err) == ReasonTermsChanged {
			return refuse(ReasonTermsChanged, err)
		}
		return CommandOutcome{}, err
	}
	newRemaining := lots - o.FilledLots
	if newRemaining < 1 {
		reason := string(matching.RejectReplaceQtyNotAboveFilled)
		return refuse(reason, rejection(reason, "the new quantity does not exceed what is already filled"))
	}
	terms, err := decodeOrderTerms(in.Terms)
	if err != nil {
		return CommandOutcome{}, err
	}
	riskIncreasing := o.Side == SideBuy && (ticks > o.PriceTicks || newRemaining > o.RemainingLots)
	ownReserved := money.Quantity{}
	if rid, err := capital.ParseReservationID(o.ReservationID); err == nil {
		r, err := s.capital.Get(ctx, tx, rid)
		if err != nil {
			return CommandOutcome{}, err
		}
		if r.Status == capital.ReservationActive {
			ownReserved = r.Remaining()
		}
	}
	input, err := s.riskInput(ctx, tx, c, in, o, terms.Risk, ticks, newRemaining, riskIncreasing, ownReserved)
	if err != nil {
		return CommandOutcome{}, err
	}
	d := risk.EvaluateAdvanced(s.policy, input)
	if _, err := s.risk.RecordAdvancedDecision(ctx, tx, input, d, in.CorrelationID); err != nil {
		return CommandOutcome{}, dbErr("record risk decision", err)
	}
	if d.Verdict != risk.Allow {
		return refuse(string(errs.CodeRiskRejected), d.RejectionError())
	}
	eng, err := s.acquireEngine(ctx, tx, c, cur)
	if err != nil {
		return CommandOutcome{}, err
	}
	defer eng.release()
	now := s.clk.Now().UTC()
	cmd := matching.Replace{Timestamp: now.UnixNano(), OrderID: o.ID.String(), OwnerID: o.OwnerID, NewPriceTicks: ticks, NewQtyLots: lots}
	decision, err := s.checkCrossing(ctx, tx, c, eng, CommandReplace, cmd, o.ID, OwnerCustomer, o.Side, ticks, true)
	if err != nil {
		return CommandOutcome{}, err
	}
	if reason, rejected := decision.DryRun.Rejected(); rejected {
		return refuse(string(reason), rejection(string(reason), "the book would reject this replace: "+string(reason)))
	}
	if !decision.Allowed {
		return refuse(ReasonHouseCrossingForbidden, rejection(ReasonHouseCrossingForbidden, "the replace would trade against a house book"))
	}
	resized, err := s.resizeReservation(ctx, tx, c, o, in.ID, eff.ID, ticks, newRemaining)
	if err != nil {
		return CommandOutcome{}, err
	}
	if resized.refused != nil {
		return refuse(ReasonInsufficientFunds, resized.refused)
	}
	if resized.row.ReservationID != o.ReservationID {
		if err := updateOrder(ctx, tx, o, resized.row, nil, actor, "reservation resized for a replace", now); err != nil {
			return CommandOutcome{}, err
		}
	}
	res, err := s.runCommand(ctx, tx, commandSpec{
		cfg: c, cur: cur, eng: eng, kind: CommandReplace, cmd: cmd, orderID: o.ID, actor: actor, now: now,
		crossingRuleVersion: decision.ruleVersionFor(),
	})
	if err != nil {
		return CommandOutcome{}, err
	}
	return s.finishCommandEffect(ctx, tx, c, eff, res, o.ID)
}

// resized is the outcome of resizeReservation: the order with its new
// reservation, or the refusal that left the old one untouched.
type resized struct {
	row     orderRow
	refused error
}

// resizeReservation makes the order's reservation exactly what the replaced
// order can spend: the old one is released and a new one reserved, inside a
// savepoint, so a shortfall leaves the old reservation untouched.
func (s *Service) resizeReservation(ctx context.Context, tx pgx.Tx, c *bookConfig, o orderRow, intentID econ.IntentID, effectID econ.EffectID,
	ticks, remaining int64) (resized, error) {
	t := c.Terms
	asset, need := t.QuoteAsset, money.Quantity{}
	if o.Side == SideBuy {
		notional := t.Notional(ticks, remaining)
		bound, err := FeeBound(c.MakerSchedule, c.TakerSchedule, notional, o.StrategyOriginated)
		if err != nil {
			return resized{}, err
		}
		need = notional.Add(bound)
	} else {
		asset, need = t.BaseAsset, t.LotsToQuantity(remaining)
	}
	rid, err := capital.ParseReservationID(o.ReservationID)
	if err != nil {
		return resized{}, errs.New(errs.CodeInternal, "orders: a resting order names no reservation")
	}
	cur, err := s.capital.Get(ctx, tx, rid)
	if err != nil {
		return resized{}, err
	}
	if cur.Status == capital.ReservationActive && cur.Remaining().Equal(need) {
		return resized{row: o}, nil
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return resized{}, dbErr("savepoint", err)
	}
	if cur.Status == capital.ReservationActive {
		if _, err := s.capital.Release(ctx, sp, rid, "order replaced"); err != nil {
			_ = sp.Rollback(ctx)
			return resized{}, err
		}
	}
	// Serves economic intent intentID; ReserveRequest.IntentID names a V1
	// trade intent, so it stays empty.
	next, err := s.capital.Reserve(ctx, sp, capital.ReserveRequest{
		AccountID: o.OwnerID, AssetID: asset, Quantity: need,
		ActorType: s.actor.Type, ActorID: s.actor.ID, IdempotencyKey: "orders:reserve:" + o.ID.String() + ":replace:" + effectID.String(),
		TTL: s.resTTL, Reason: "internal-book order " + o.ID.String() + " replaced (economic intent " + intentID.String() + ")",
	})
	if err != nil {
		_ = sp.Rollback(ctx)
		switch errs.CodeOf(err) {
		case errs.CodeInsufficientBuyingPower, errs.CodeAccountFrozen, errs.CodeAssetRestricted:
			return resized{row: o, refused: err}, nil
		}
		return resized{}, err
	}
	if err := s.capital.LockForOrder(ctx, sp, next.ID, o.ID.String()); err != nil {
		_ = sp.Rollback(ctx)
		return resized{}, err
	}
	if err := sp.Commit(ctx); err != nil {
		return resized{}, dbErr("savepoint", err)
	}
	after := o
	after.ReservationID = next.ID.String()
	return resized{row: after}, nil
}

// replayOutcome is the outcome of a request that was already carried out.
func replayOutcome(c *bookConfig, o orderRow) CommandOutcome {
	out := CommandOutcome{Order: viewOf(c, o)}
	if o.Status == StatusRejected {
		out.Rejection = rejection(o.RejectReason, "the order was rejected: "+o.RejectReason)
	}
	return out
}
