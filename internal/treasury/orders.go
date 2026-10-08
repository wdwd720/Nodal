package treasury

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/orders"
	"github.com/nodal/controlplane/internal/risk"
)

// PlaceOrderRequest is a house order on the internal book.
type PlaceOrderRequest struct {
	HouseAccountID econ.HouseAccountID
	InstrumentID   instruments.InstrumentID
	Activity       econ.HouseStrategy
	Side           orders.Side
	Type           orders.OrderType
	TimeInForce    orders.TimeInForce
	// Price is the limit price, or a market order's protection price, which
	// the book always requires.
	Price          *money.Price
	Quantity       money.Quantity
	PostOnly       bool
	ReduceOnly     bool
	ClientOrderID  string
	IdempotencyKey string
	CorrelationID  string
	Actor          econ.Actor
}

// OrderOutcome is the result of a house order command: the recorded action,
// the order (also when the book refused it), the trades it made at once, the
// risk decision, and a recorded refusal.
type OrderOutcome struct {
	Action    Action
	Order     *orders.OrderView
	Trades    []orders.Trade
	Decision  *RiskDecision
	Rejection error
}

// PlaceOrder places a house order on the internal book after every control
// of the treasury, under the book's lock: authority, activity, capability
// (PRINCIPAL_TRADING and INTERNAL_MATCHING: a kill switch stops it), the
// policy, the treasury limits and Risk Kernel V2. Then
// orders.Service.SubmitHouseOrder applies the order book's own rules --
// HOUSE_INVENTORY reservations and the house-versus-customer crossing rule,
// which refuses by default -- and the register ingests any trade the order
// made at once. A refusal at any step is recorded and returned in
// OrderOutcome.Rejection with a nil error; the caller commits it.
func (s *Service) PlaceOrder(ctx context.Context, tx pgx.Tx, r PlaceOrderRequest) (OrderOutcome, error) {
	if err := validKey(r.IdempotencyKey); err != nil {
		return OrderOutcome{}, err
	}
	if r.Price == nil || !r.Quantity.IsPositive() || !r.Side.Valid() || !r.Type.Valid() || !r.TimeInForce.Valid() {
		return OrderOutcome{}, errs.New(errs.CodeValidationFailed,
			"treasury: a house order names a side, a type, a time in force, a price and a positive quantity")
	}
	book, err := s.lockBook(ctx, tx, r.HouseAccountID)
	if err != nil {
		return OrderOutcome{}, err
	}
	sp, err := s.currentSigningPolicy(ctx, tx, book.HouseAccountID)
	if err != nil {
		return OrderOutcome{}, err
	}
	if err := sp.Authorize(r.Actor, ActionPlaceOrder); err != nil {
		return OrderOutcome{}, err
	}
	reqHash, err := econ.RequestHash(map[string]any{
		"instrument_id": r.InstrumentID.String(), "activity": string(r.Activity), "side": string(r.Side),
		"order_type": string(r.Type), "time_in_force": string(r.TimeInForce), "price_mantissa": r.Price.Mantissa.String(),
		"price_scale": r.Price.Scale, "price_quote_asset": r.Price.QuoteAsset, "quantity": r.Quantity.String(),
		"post_only": r.PostOnly, "reduce_only": r.ReduceOnly, "client_order_id": r.ClientOrderID,
	})
	if err != nil {
		return OrderOutcome{}, err
	}
	if prior, found, err := s.actionByKey(ctx, tx, book.HouseAccountID, r.IdempotencyKey); err != nil {
		return OrderOutcome{}, err
	} else if found {
		if err := sameRequest(prior, ActionPlaceOrder, reqHash); err != nil {
			return OrderOutcome{}, err
		}
		return s.replayOrder(ctx, tx, prior)
	}
	now := s.clk.Now().UTC()
	a := Action{
		ID: id.New[actionKind](), HouseAccountID: book.HouseAccountID, Kind: ActionPlaceOrder, Activity: r.Activity, Actor: r.Actor,
		SigningPolicyVersion: sp.Version, IdempotencyKey: r.IdempotencyKey, RequestHash: reqHash, CorrelationID: r.CorrelationID,
		OccurredAt: now,
	}
	reject := func(code string, reasons []string, out OrderOutcome, detail map[string]any) (OrderOutcome, error) {
		a.Outcome, a.RejectionCode, a.ReasonCodes = OutcomeRejected, code, reasons
		if err := s.recordAction(ctx, tx, book, &a, "house order refused", detail); err != nil {
			return OrderOutcome{}, err
		}
		out.Action, out.Rejection = a, a.Rejection()
		return out, nil
	}
	if !permitted(book.Strategy, r.Activity) {
		return reject(ReasonActivityNotPermitted, []string{ReasonActivityNotPermitted}, OrderOutcome{},
			map[string]any{"strategy": string(book.Strategy)})
	}
	bk, err := s.orders.GetBook(ctx, tx, r.InstrumentID)
	if err != nil {
		return OrderOutcome{}, err
	}
	for _, c := range []gates.Capability{gates.PrincipalTrading, gates.InternalMatching} {
		if err := s.requireCapability(ctx, tx, c, bk.Mode); err != nil {
			return reject(string(errs.CodeOf(err)), []string{ReasonOf(err)}, OrderOutcome{}, map[string]any{"capability": string(c)})
		}
	}
	pv, found, err := s.currentPolicy(ctx, tx, book.HouseAccountID)
	if err != nil {
		return OrderOutcome{}, err
	}
	if !found {
		return reject(ReasonPolicyMissing, []string{ReasonPolicyMissing}, OrderOutcome{}, nil)
	}
	a.RiskPolicyVersion = pv.Version
	t := bk.Terms
	if t.QuoteAsset != book.ValuationAsset {
		return reject(ReasonQuoteNotValuation, []string{ReasonQuoteNotValuation}, OrderOutcome{},
			map[string]any{"quote_asset": t.QuoteAsset.String()})
	}
	ticks, err := t.PriceToTicks(*r.Price)
	if err != nil {
		return OrderOutcome{}, err
	}
	lots, err := t.QuantityToLots(r.Quantity)
	if err != nil {
		return OrderOutcome{}, err
	}
	if err := t.CheckNotional(ticks, lots); err != nil {
		return OrderOutcome{}, err
	}
	if _, err := s.syncLocked(ctx, tx, book); err != nil {
		return OrderOutcome{}, err
	}
	dec, err := s.orderRisk(ctx, tx, book, pv, a, r, bk, ticks, lots, now)
	if err != nil {
		return OrderOutcome{}, err
	}
	a.RiskDecisionID = &dec.ID
	if dec.Verdict == risk.Reject {
		return reject(string(errs.CodeRiskRejected), append(append([]string{}, dec.ReasonCodes...), dec.TreasuryCodes...),
			OrderOutcome{Decision: &dec}, map[string]any{"decision_hash": dec.DecisionHash, "limits": dec.Limits})
	}
	out, err := s.orders.SubmitHouseOrder(ctx, tx, orders.HouseOrderRequest{
		InstrumentID: r.InstrumentID, HouseAccountID: book.HouseAccountID, Mode: bk.Mode, Side: r.Side, Type: r.Type,
		TimeInForce: r.TimeInForce, Price: r.Price, Quantity: r.Quantity, PostOnly: r.PostOnly, ReduceOnly: r.ReduceOnly,
		ClientOrderID: r.ClientOrderID, IdempotencyKey: "treasury:" + r.IdempotencyKey, Actor: r.Actor,
	})
	if err != nil {
		return OrderOutcome{}, err
	}
	result := OrderOutcome{Decision: &dec, Trades: out.Trades}
	if !out.Order.ID.IsZero() {
		view := out.Order
		result.Order = &view
		a.OrderID = &view.ID
	}
	if out.Rejection != nil {
		reason := orders.ReasonOf(out.Rejection)
		if reason == "" {
			reason = ReasonOrderRefused
		}
		return reject(ReasonOrderRefused, []string{reason}, result, map[string]any{"order_reason": reason})
	}
	if _, err := s.syncLocked(ctx, tx, book); err != nil {
		return OrderOutcome{}, err
	}
	a.Outcome = OutcomeApplied
	if err := s.recordAction(ctx, tx, book, &a, "house order placed", map[string]any{
		"order_id": out.Order.ID.String(), "command_seq": out.CommandSeq, "trades": len(out.Trades),
		"decision_hash": dec.DecisionHash, "limits": dec.Limits, "order_status": string(out.Order.Status),
	}); err != nil {
		return OrderOutcome{}, err
	}
	result.Action = a
	return result, nil
}

// orderRisk evaluates a house order: the treasury limits (for a buy) and Risk
// Kernel V2, persisted.
func (s *Service) orderRisk(ctx context.Context, tx pgx.Tx, book Book, pv PolicyVersion, a Action, r PlaceOrderRequest,
	bk orders.Book, ticks, lots int64, now time.Time) (RiskDecision, error) {
	t := bk.Terms
	notional := t.Notional(ticks, lots)
	qty := t.LotsToQuantity(lots)
	riskIncreasing := r.Side == orders.SideBuy
	inst := r.InstrumentID
	snap, err := s.snapshotOf(ctx, tx, book, t.BaseAsset, &inst)
	if err != nil {
		return RiskDecision{}, err
	}
	mark, err := s.markOf(ctx, tx, t.BaseAsset, book.ValuationAsset, pv.Policy.MaxMarkAge, now)
	if err != nil {
		return RiskDecision{}, err
	}
	rep, err := s.report(ctx, tx, book, pv.Policy.MaxMarkAge, now)
	if err != nil {
		return RiskDecision{}, err
	}
	loss, err := rep.lossState()
	if err != nil {
		return RiskDecision{}, err
	}
	exposure := Exposure{Held: snap.heldBase, WorkingBuys: snap.workingBuys, InstrumentWorkingBuys: snap.instrumentBuy}
	switch {
	case snap.heldBase.IsZero():
		zero := money.Quantity{}
		exposure.HeldValue = &zero
	case mark != nil:
		v, err := money.Notional(snap.heldBase, t.BaseDecimals, *mark, t.QuoteDecimals, ExposureRounding)
		if err != nil {
			return RiskDecision{}, errs.Wrap(err, errs.CodeInternal, "treasury: held value")
		}
		exposure.HeldValue = &v
	}
	limits := EvaluateLimits(pv.Policy, LimitRequest{
		RiskIncreasing: riskIncreasing, Asset: t.BaseAsset, Quantity: qty, Notional: notional, Instrument: &inst,
		Exposure: exposure, Loss: loss,
	})
	// The balance section: what the order pays with against what it needs,
	// both in USD at the USD-pegged valuation asset.
	var availableValue, required money.Quantity
	if riskIncreasing {
		availableValue, required = snap.heldQuote.Sub(snap.reservedQuote), notional
	} else {
		free := snap.heldBase.Sub(snap.reservedBase)
		price := t.TicksToPrice(ticks, "house-order-limit", now)
		if availableValue, err = money.Notional(free.Max(money.Quantity{}), t.BaseDecimals, price, t.QuoteDecimals, money.RoundDown); err != nil {
			return RiskDecision{}, errs.Wrap(err, errs.CodeInternal, "treasury: value holdings")
		}
		required = notional
		if r.ReduceOnly {
			required = notional.Min(availableValue)
		}
	}
	availableUSD, err := usd(availableValue.Max(money.Quantity{}), t.QuoteDecimals, money.RoundDown)
	if err != nil {
		return RiskDecision{}, err
	}
	requiredUSD, err := usd(required, t.QuoteDecimals, money.RoundUp)
	if err != nil {
		return RiskDecision{}, err
	}
	notionalUSD, err := usd(notional, t.QuoteDecimals, money.RoundUp)
	if err != nil {
		return RiskDecision{}, err
	}
	kind := econ.KindTakeLiquidity
	if r.Type == orders.TypeLimit && r.TimeInForce == orders.GTC {
		kind = econ.KindProvideLiquidity
	}
	status := "ACTIVE"
	if bk.Status != orders.BookOpen {
		status = string(bk.Status)
	}
	return s.evaluateRisk(ctx, tx, riskRequest{
		book: book, policy: pv, decisionFor: a.ID, intentKind: kind, actor: r.Actor, mode: bk.Mode, instrument: &inst,
		instrumentKind: string(t.Kind),
		instrumentSnap: risk.InstrumentSnapshot{
			Status: status, SessionOpen: t.TradingCalendar == "24x7", Expired: t.Expiry != nil && !now.Before(*t.Expiry),
		},
		asset: t.BaseAsset, riskIncreasing: riskIncreasing, notionalUSD: notionalUSD,
		balance: risk.BalanceSnapshot{Available: availableUSD, Required: requiredUSD}, mark: mark, limits: limits,
	})
}

func (s *Service) replayOrder(ctx context.Context, tx pgx.Tx, prior Action) (OrderOutcome, error) {
	out := OrderOutcome{Action: prior, Rejection: prior.Rejection()}
	if prior.OrderID != nil {
		view, err := s.orders.Order(ctx, tx, *prior.OrderID)
		if err != nil {
			return OrderOutcome{}, err
		}
		out.Order = &view
		trades, err := s.orders.OrderTrades(ctx, tx, *prior.OrderID)
		if err != nil {
			return OrderOutcome{}, err
		}
		out.Trades = trades
	}
	if prior.RiskDecisionID != nil {
		d, err := s.Decision(ctx, tx, *prior.RiskDecisionID)
		if err != nil {
			return OrderOutcome{}, err
		}
		out.Decision = &d
	}
	return out, nil
}

// QuoteRequest is a two-sided quote: a post-only GTC bid and ask.
type QuoteRequest struct {
	HouseAccountID econ.HouseAccountID
	InstrumentID   instruments.InstrumentID
	// Activity is MARKET_MAKING or LIQUIDITY_PROVISION.
	Activity       econ.HouseStrategy
	BidPrice       *money.Price
	BidQuantity    money.Quantity
	AskPrice       *money.Price
	AskQuantity    money.Quantity
	IdempotencyKey string
	CorrelationID  string
	Actor          econ.Actor
}

// QuoteOutcome is both legs of a two-sided quote. Each leg is its own house
// action: one can be refused (a bid over the inventory cap) while the other
// rests.
type QuoteOutcome struct {
	Bid OrderOutcome
	Ask OrderOutcome
}

// QuoteTwoSided places both sides of a market maker's quote on the internal
// book, the bid evaluated first so the ask is evaluated against it. A quote
// whose bid is not strictly below its ask is refused.
func (s *Service) QuoteTwoSided(ctx context.Context, tx pgx.Tx, r QuoteRequest) (QuoteOutcome, error) {
	if r.Activity != econ.HouseMarketMaking && r.Activity != econ.HouseLiquidityProvision {
		return QuoteOutcome{}, errs.New(errs.CodeValidationFailed, "treasury: a two-sided quote is market making or liquidity provision")
	}
	if r.BidPrice == nil || r.AskPrice == nil {
		return QuoteOutcome{}, errs.New(errs.CodeValidationFailed, "treasury: a quote has a bid and an ask price")
	}
	if c, ok := comparePrices(*r.BidPrice, *r.AskPrice); !ok || c >= 0 {
		return QuoteOutcome{}, errs.New(errs.CodeValidationFailed, "treasury: a quote's bid must be strictly below its ask, in one quote asset")
	}
	leg := func(side orders.Side, price *money.Price, qty money.Quantity, suffix string) PlaceOrderRequest {
		return PlaceOrderRequest{
			HouseAccountID: r.HouseAccountID, InstrumentID: r.InstrumentID, Activity: r.Activity, Side: side,
			Type: orders.TypeLimit, TimeInForce: orders.GTC, Price: price, Quantity: qty, PostOnly: true,
			IdempotencyKey: r.IdempotencyKey + "/" + suffix, CorrelationID: r.CorrelationID, Actor: r.Actor,
		}
	}
	bid, err := s.PlaceOrder(ctx, tx, leg(orders.SideBuy, r.BidPrice, r.BidQuantity, "bid"))
	if err != nil {
		return QuoteOutcome{}, err
	}
	ask, err := s.PlaceOrder(ctx, tx, leg(orders.SideSell, r.AskPrice, r.AskQuantity, "ask"))
	if err != nil {
		return QuoteOutcome{}, err
	}
	return QuoteOutcome{Bid: bid, Ask: ask}, nil
}

// comparePrices compares two prices exactly; ok is false when they are not
// quoted in the same asset.
func comparePrices(a, b money.Price) (int, bool) {
	if a.QuoteAsset != b.QuoteAsset {
		return 0, false
	}
	am, bm := a.Mantissa, b.Mantissa
	ten := money.QuantityFromInt64(10)
	for s := a.Scale; s < b.Scale; s++ {
		am = am.Mul(ten)
	}
	for s := b.Scale; s < a.Scale; s++ {
		bm = bm.Mul(ten)
	}
	return am.Cmp(bm), true
}

// CancelRequest cancels a house order.
type CancelRequest struct {
	HouseAccountID econ.HouseAccountID
	OrderID        orders.OrderID
	IdempotencyKey string
	CorrelationID  string
	Actor          econ.Actor
}

// CancelOrder cancels a resting house order. A cancel is never new risk: no
// capability, limit or kill switch stops it, and an operator may always cancel.
func (s *Service) CancelOrder(ctx context.Context, tx pgx.Tx, r CancelRequest) (OrderOutcome, error) {
	if err := validKey(r.IdempotencyKey); err != nil {
		return OrderOutcome{}, err
	}
	book, err := s.lockBook(ctx, tx, r.HouseAccountID)
	if err != nil {
		return OrderOutcome{}, err
	}
	sp, err := s.currentSigningPolicy(ctx, tx, book.HouseAccountID)
	if err != nil {
		return OrderOutcome{}, err
	}
	if err := sp.Authorize(r.Actor, ActionCancelOrder); err != nil {
		return OrderOutcome{}, err
	}
	reqHash, err := econ.RequestHash(map[string]any{"cancel_order_id": r.OrderID.String()})
	if err != nil {
		return OrderOutcome{}, err
	}
	if prior, found, err := s.actionByKey(ctx, tx, book.HouseAccountID, r.IdempotencyKey); err != nil {
		return OrderOutcome{}, err
	} else if found {
		if err := sameRequest(prior, ActionCancelOrder, reqHash); err != nil {
			return OrderOutcome{}, err
		}
		return s.replayOrder(ctx, tx, prior)
	}
	out, err := s.orders.CancelHouseOrder(ctx, tx, book.HouseAccountID, r.OrderID, r.Actor)
	if err != nil {
		return OrderOutcome{}, err
	}
	oid := r.OrderID
	a := Action{
		ID: id.New[actionKind](), HouseAccountID: book.HouseAccountID, Kind: ActionCancelOrder, Actor: r.Actor,
		SigningPolicyVersion: sp.Version, IdempotencyKey: r.IdempotencyKey, RequestHash: reqHash, OrderID: &oid,
		CorrelationID: r.CorrelationID, OccurredAt: s.clk.Now().UTC(), Outcome: OutcomeApplied,
	}
	view := out.Order
	result := OrderOutcome{Order: &view}
	if out.Rejection != nil {
		reason := orders.ReasonOf(out.Rejection)
		if reason == "" {
			reason = ReasonOrderRefused
		}
		a.Outcome, a.RejectionCode, a.ReasonCodes = OutcomeRejected, ReasonOrderRefused, []string{reason}
	}
	if err := s.recordAction(ctx, tx, book, &a, "house order cancelled", map[string]any{"order_id": oid.String(),
		"order_status": string(out.Order.Status)}); err != nil {
		return OrderOutcome{}, err
	}
	result.Action, result.Rejection = a, a.Rejection()
	return result, nil
}
