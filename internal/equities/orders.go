package equities

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/agentauthority"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/settlement"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// OrderRequest is a customer's (or an agent's) request to trade an equity.
type OrderRequest struct {
	AccountID    accounts.AccountID
	InstrumentID instruments.InstrumentID
	Side         Side
	Type         OrderType
	// Quantity is in share base units (fractional shares at the listing's
	// decimals); it must be a whole number of lots.
	Quantity money.Quantity
	// LimitPrice is the limit per whole share in the settlement asset.
	LimitPrice *PricePoint
	// ExtendedHours lets a limit order trade in pre-market and after-hours.
	ExtendedHours  bool
	Mode           gates.Mode
	IdempotencyKey string
	ActorType      security.ActorType
	ActorID        string
	// AgentID and AgentAuthority are set exactly when an agent proposes;
	// DelegationID names the delegation it acts under, for the AuthorityReader.
	AgentID        string
	AgentAuthority agentauthority.Level
	DelegationID   string
	StrategyRef    string
	CorrelationID  string
}

// Validate checks the request's shape.
func (r OrderRequest) Validate() error {
	var p []string
	if r.AccountID.IsZero() || r.InstrumentID.IsZero() {
		p = append(p, "account and instrument required")
	}
	if !r.Side.Valid() || !r.Type.Valid() {
		p = append(p, "side must be BUY or SELL and type MARKET or LIMIT")
	}
	if !r.Quantity.IsPositive() {
		p = append(p, "quantity must be positive")
	}
	switch r.Type {
	case OrderLimit:
		if r.LimitPrice == nil || r.LimitPrice.validate("a limit price") != nil {
			p = append(p, "a limit order has a positive limit price")
		}
	case OrderMarket:
		if r.LimitPrice != nil || r.ExtendedHours {
			p = append(p, "a market order has no limit price and never trades outside regular hours")
		}
	}
	if !validOrderMode(r.Mode) {
		p = append(p, "mode must be PAPER or SANDBOX; no equity order is LIVE")
	}
	if k := strings.TrimSpace(r.IdempotencyKey); k == "" || k != r.IdempotencyKey || len(k) > 200 {
		p = append(p, "idempotency key must be 1..200 bytes without surrounding whitespace")
	}
	if len(p) > 0 {
		return errs.New(errs.CodeValidationFailed, "equities: invalid order request").WithField("problems", p)
	}
	return nil
}

// orderTerms are the kind-specific terms an equity intent carries.
type orderTerms struct {
	Side          Side        `json:"side"`
	OrderType     OrderType   `json:"order_type"`
	LimitPrice    *PricePoint `json:"limit_price,omitempty"`
	ExtendedHours bool        `json:"extended_hours"`
}

func decodeOrderTerms(raw json.RawMessage) (orderTerms, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var t orderTerms
	if err := dec.Decode(&t); err != nil {
		return orderTerms{}, errs.Wrap(err, errs.CodeValidationFailed, "equities: the intent's terms are not equity order terms")
	}
	if !t.Side.Valid() || !t.OrderType.Valid() || (t.OrderType == OrderLimit) != (t.LimitPrice != nil) ||
		(t.OrderType == OrderMarket && t.ExtendedHours) {
		return orderTerms{}, errs.New(errs.CodeValidationFailed, "equities: the intent's order terms are inconsistent")
	}
	return t, nil
}

func intentKindFor(side Side) econ.IntentKind {
	if side == SideBuy {
		return econ.KindAcquireExposure
	}
	return econ.KindReduceExposure
}

// SubmitOrderIntent records the request as an economic intent (idempotent on
// account and idempotency key) and routes it through the settlement compiler.
// The intent ends ROUTED, or REJECTED with the compiler's reasons; a replay
// returns the intent as it is. This is the only step an agent's proposal
// reaches by itself.
func (s *Service) SubmitOrderIntent(ctx context.Context, tx pgx.Tx, req OrderRequest) (econ.Intent, error) {
	if err := req.Validate(); err != nil {
		return econ.Intent{}, err
	}
	l, err := s.LoadListing(ctx, tx, req.InstrumentID)
	if err != nil {
		return econ.Intent{}, err
	}
	terms, err := json.Marshal(orderTerms{Side: req.Side, OrderType: req.Type, LimitPrice: req.LimitPrice, ExtendedHours: req.ExtendedHours})
	if err != nil {
		return econ.Intent{}, errs.Wrap(err, errs.CodeInternal, "equities: encode order terms")
	}
	qty := req.Quantity
	share := l.ShareAsset.ID
	in, existing, err := s.intents.Submit(ctx, tx, econ.Intent{
		AccountID: req.AccountID, Kind: intentKindFor(req.Side), Mode: req.Mode, Capability: gates.Equities,
		ActorType: req.ActorType, ActorID: req.ActorID, AgentID: req.AgentID, StrategyRef: req.StrategyRef,
		DelegationID: req.DelegationID, InstrumentID: req.InstrumentID.String(), AssetID: &share, Quantity: &qty, Terms: terms,
		IdempotencyKey: req.IdempotencyKey, CorrelationID: req.CorrelationID,
	})
	if err != nil || existing {
		return in, err
	}
	level, err := s.verification.Level(ctx, tx, req.AccountID)
	if err != nil {
		return econ.Intent{}, err
	}
	now := s.clk.Now().UTC()
	route, err := econ.Route(econ.RouteRequest{
		Intent:         in,
		Subject:        settlement.Subject{Type: settlement.SubjectInstrument, ID: req.InstrumentID.String(), AssetID: share.String()},
		AgentAuthority: req.AgentAuthority, Verification: level,
	}, s.router, now)
	if err != nil {
		return econ.Intent{}, err
	}
	// A ROUTED intent is accepted, priced, reserved and placed with nothing
	// asked in between, so it needs a route that may execute now: one the
	// legal router permits only once a person has confirmed it (or an
	// operator reviewed it) is permission to ask, and the order path has no
	// such step. It is refused with that step among its reasons (review 3,
	// F5).
	if !route.MayExecuteNow() {
		return s.intents.Transition(ctx, tx, in.ID, econ.StatusRejected, s.actor,
			"refused by the settlement compiler: "+strings.Join(routeRefusalReasons(route), ","), "ROUTE_REFUSED")
	}
	return s.intents.Transition(ctx, tx, in.ID, econ.StatusRouted, s.actor, "routed to the equities subsystem", "")
}

// routeRefusalReasons are why a route may not execute now, sorted: the
// compiler's refusals, and for a route it permits only after a person's
// confirmation or an operator's review, that step (REQUIRES_USER_CONFIRMATION,
// REQUIRES_MANUAL_REVIEW).
func routeRefusalReasons(route settlement.Route) []string {
	reasons := append([]string{}, route.Reasons...)
	if route.Permitted && route.RequiredConfirmation != settlement.ConfirmNone {
		reasons = append(reasons, "REQUIRES_"+string(route.RequiredConfirmation))
	}
	sort.Strings(reasons)
	return reasons
}

// AcceptOrder turns a ROUTED equity intent into an order: it authorizes the
// capability, validates the order against the instrument (lots, limit price,
// pending corporate actions), prices it (the mark, the venue's indicative
// price and the fee schedule), evaluates the risk kernel with every required
// section and records the decision, and reserves the capital the order will
// draw on. The order is ACCEPTED, or REJECTED with a stable code; either way
// it is recorded and the intent moves one step (RISK_APPROVED or REJECTED).
// A refusal is not an error: the caller commits it and returns
// Order.Refusal(). Replaying an intent returns its order.
func (s *Service) AcceptOrder(ctx context.Context, tx pgx.Tx, intentID econ.IntentID) (Order, error) {
	if _, err := tx.Exec(ctx, `SELECT 1 FROM economic_intents WHERE id = $1 FOR UPDATE`, intentID); err != nil {
		return Order{}, storeErr("lock intent", err)
	}
	if o, ok, err := orderByIntent(ctx, tx, intentID); err != nil || ok {
		return o, err
	}
	in, err := s.intents.Get(ctx, tx, intentID)
	if err != nil {
		return Order{}, err
	}
	if in.Status != econ.StatusRouted {
		return Order{}, errs.Newf(errs.CodeInvalidStateTransition, "equities: intent %s is %s; only a ROUTED intent is accepted", in.ID, in.Status).
			WithField("intent_id", in.ID.String())
	}
	if in.Capability != gates.Equities || in.Quantity == nil || in.InstrumentID == "" {
		return Order{}, errs.New(errs.CodeValidationFailed, "equities: the intent is not an equity order")
	}
	terms, err := decodeOrderTerms(in.Terms)
	if err != nil {
		return Order{}, err
	}
	if in.Kind != intentKindFor(terms.Side) {
		return Order{}, errs.New(errs.CodeValidationFailed, "equities: the intent kind disagrees with its side")
	}
	iid, err := instruments.ParseInstrumentID(in.InstrumentID)
	if err != nil {
		return Order{}, errs.Wrap(err, errs.CodeValidationFailed, "equities: intent instrument id")
	}
	if err := lockInstrument(ctx, tx, iid, false); err != nil {
		return Order{}, err
	}
	l, err := s.LoadListing(ctx, tx, iid)
	if err != nil {
		return Order{}, err
	}
	t := l.Terms.Terms
	cal, err := s.Calendar(ctx, tx, t.TradingCalendar)
	if err != nil {
		return Order{}, err
	}
	now := s.clk.Now().UTC()
	session, eligible, expires := cal.OrderWindow(now, terms.OrderType, terms.ExtendedHours)
	o := Order{
		ID: NewOrderID(), AccountID: in.AccountID, InstrumentID: iid, IntentID: in.ID, ShareAssetID: l.ShareAsset.ID,
		CashAssetID: l.CashAsset.ID, Symbol: t.Symbol, ExchangeMIC: t.ExchangeMIC, TermsVersion: l.Terms.Version,
		Side: terms.Side, Type: terms.OrderType, Mode: in.Mode, Quantity: *in.Quantity, QuantityDecimals: t.QuantityDecimals,
		PriceScale: t.PriceScale, TickSize: t.TickSize, ExtendedHours: terms.ExtendedHours, Session: session,
		TradeDate: cal.TradeDate(now), ExpiresAt: expires, Broker: s.adapter.Name(), ActorType: in.ActorType,
		ActorID: in.ActorID, CorrelationID: in.CorrelationID, CreatedAt: now,
	}
	// The limit is resolved first so that every refusal of a valid limit
	// order records its limit; an invalid one is refused below.
	limitValid := true
	if o.Type == OrderLimit {
		limit, err := rescalePrice(terms.LimitPrice.Mantissa, terms.LimitPrice.Scale, t.PriceScale)
		if err == nil && limit.IsPositive() && onGrid(limit, t.TickSize) {
			o.LimitPrice = &limit
		} else {
			limitValid = false
		}
	}

	// 1. The capability, in this mode, in this deployment.
	if err := s.auth.Require(ctx, tx, gates.Equities, in.Mode); err != nil {
		code := RejectCapabilityNotApproved
		switch errs.CodeOf(err) {
		case errs.CodeCapabilityNotApproved:
		case errs.CodeKillSwitchActive:
			code = RejectKillSwitch
		default:
			return Order{}, err
		}
		return s.reject(ctx, tx, in, o, code, fieldStrings(err, "reasons"))
	}
	// 2. The order's shape against the instrument.
	if code, ok := checkQuantity(o.Quantity, t); !ok {
		return s.reject(ctx, tx, in, o, code, nil)
	}
	if !limitValid {
		return s.reject(ctx, tx, in, o, RejectLimitPriceInvalid, nil)
	}
	if blocking, err := s.blockingAction(ctx, tx, iid, o.Side, o.TradeDate); err != nil {
		return Order{}, err
	} else if blocking != nil {
		return s.reject(ctx, tx, in, o, RejectCorporateActionPending, []string{string(blocking.Type) + ":" + blocking.ID.String()})
	}
	if split, err := splitAppliedSince(ctx, tx, iid, in.CreatedAt); err != nil {
		return Order{}, err
	} else if split != nil {
		return s.reject(ctx, tx, in, o, RejectIntentPredatesSplit, []string{string(split.Type) + ":" + split.ID.String()})
	}
	// 3. Price discovery: what the reservation is sized against.
	mark, err := s.markFor(ctx, tx, l, now)
	if err != nil {
		return Order{}, err
	}
	var reference money.Quantity
	switch o.Type {
	case OrderMarket:
		if mark == nil {
			return s.reject(ctx, tx, in, o, RejectNoMark, nil)
		}
		q, err := s.adapter.Quote(ctx, QuoteRequest{Symbol: o.Symbol, ExchangeMIC: o.ExchangeMIC, Side: o.Side, Quantity: o.Quantity,
			QuantityDecimals: o.QuantityDecimals, PriceScale: o.PriceScale, TickSize: o.TickSize, Mark: mark, At: now})
		if err != nil {
			return Order{}, err
		}
		protection, err := protectionPrice(q.IndicativePrice, o.Side, s.collar, o.PriceScale, o.TickSize)
		if err != nil {
			return Order{}, err
		}
		o.ProtectionPrice, reference = &protection, protection
	case OrderLimit:
		reference = *o.LimitPrice
	}
	quote, err := s.priceOrder(o, l, reference, in.StrategyRef != "", now)
	if err != nil {
		if errs.CodeOf(err) == errs.CodeValidationFailed {
			return s.reject(ctx, tx, in, o, RejectNotionalTooSmall, nil)
		}
		return Order{}, err
	}
	if _, err := ChargesOf(quote); err != nil {
		if errs.CodeOf(err) == errs.CodeValidationFailed {
			return s.reject(ctx, tx, in, o, RejectNotionalTooSmall, nil)
		}
		return Order{}, err
	}
	notional, err := money.QuoteQuantityToUSD(quote.NotionalAtReference, l.CashAsset.Decimals, money.RoundDown)
	if err != nil {
		return Order{}, errs.Wrap(err, errs.CodeOverflow, "equities: order notional")
	}
	if notional.Cmp(s.minNotional) < 0 {
		return s.reject(ctx, tx, in, o, RejectNotionalTooSmall, nil)
	}
	reserveAsset, reserveQty := l.ShareAsset.ID, o.Quantity
	exposure := quote.NotionalAtReference
	if o.Side == SideBuy {
		reserveAsset, reserveQty, exposure = l.CashAsset.ID, quote.WorstCaseTotal, quote.WorstCaseTotal
	}
	riskNotional, err := money.QuoteQuantityToUSD(exposure, l.CashAsset.Decimals, money.RoundUp)
	if err != nil {
		return Order{}, errs.Wrap(err, errs.CodeOverflow, "equities: risk notional")
	}
	// 4. Risk, with every required section, recorded either way.
	decision, decisionID, err := s.evaluateRisk(ctx, tx, in, l, o.Side, eligible, mark, riskNotional, now)
	if err != nil {
		return Order{}, err
	}
	o.RiskDecisionID = &decisionID
	if decision.Verdict != risk.Allow {
		return s.reject(ctx, tx, in, o, RejectRisk, decision.ReasonCodes)
	}
	// 5. Reserve what the order will draw on, under the account's totals lock.
	ttl := expires.Sub(now) + time.Hour
	if ttl > capital.MaxReservationTTL {
		ttl = capital.MaxReservationTTL
	}
	// The reservation names the economic intent it serves (00733).
	res, err := s.capital.Reserve(ctx, tx, capital.ReserveRequest{
		AccountID: o.AccountID.String(), AssetID: reserveAsset, Quantity: reserveQty, EconomicIntentID: in.ID.String(),
		ActorType: s.actor.Type, ActorID: s.actor.ID, IdempotencyKey: "equities:order:" + in.ID.String(), TTL: ttl,
		Reason: "equity " + strings.ToLower(string(o.Side)) + " order " + o.ID.String() + " (economic intent " + in.ID.String() + ")",
	})
	if err != nil {
		if errs.CodeOf(err) == errs.CodeInsufficientBuyingPower {
			return s.reject(ctx, tx, in, o, RejectInsufficientBuyingPower, nil)
		}
		return Order{}, err
	}
	o.ReservationID, o.ReservedQuantity, o.Status = &res.ID, &reserveQty, OrderAccepted
	if o, err = insertOrder(ctx, tx, o); err != nil {
		return Order{}, err
	}
	if err := s.capital.LockForOrder(ctx, tx, res.ID, o.ID.String()); err != nil {
		return Order{}, err
	}
	// 6. Claim the venue submission: the external step between this
	// transaction and the one that applies the venue's answer.
	if _, _, err := s.effects.Begin(ctx, tx, econ.BeginRequest{
		Key: submitKey(o.ID), Subsystem: econ.SubsystemEquities, Kind: "ORDER_SUBMISSION", IntentID: &in.ID,
		AccountID: o.AccountID.String(), Request: submissionRequest(o), PostsLedger: false, Actor: s.actor,
		CorrelationID: o.CorrelationID,
	}); err != nil {
		return Order{}, err
	}
	if _, err := s.intents.Transition(ctx, tx, in.ID, econ.StatusRiskApproved, s.actor, "equity order accepted and reserved", ""); err != nil {
		return Order{}, err
	}
	if err := s.recordOrder(ctx, tx, o, "accepted"); err != nil {
		return Order{}, err
	}
	return o, nil
}

// reject records a refused order and rejects its intent.
func (s *Service) reject(ctx context.Context, tx pgx.Tx, in econ.Intent, o Order, code RejectionCode, reasons []string) (Order, error) {
	sorted := append([]string{}, reasons...)
	sort.Strings(sorted)
	o.Status, o.RejectionCode, o.RejectionReasons = OrderRejected, code, sorted
	o.ReservationID, o.ReservedQuantity = nil, nil
	if o.Type == OrderLimit {
		o.ProtectionPrice = nil
	}
	out, err := insertOrder(ctx, tx, o)
	if err != nil {
		return Order{}, err
	}
	if _, err := s.intents.Transition(ctx, tx, in.ID, econ.StatusRejected, s.actor, "equity order refused: "+string(code), string(code)); err != nil {
		return Order{}, err
	}
	if err := s.recordOrder(ctx, tx, out, "rejected"); err != nil {
		return Order{}, err
	}
	return out, nil
}

func checkQuantity(q money.Quantity, t instruments.Terms) (RejectionCode, bool) {
	if _, err := q.Div(t.LotSize, money.RoundExact); err != nil {
		return RejectNotLotMultiple, false
	}
	if q.Cmp(t.MinQuantity) < 0 {
		return RejectBelowMinimum, false
	}
	return "", true
}

// protectionPrice moves the indicative price by the collar against the
// customer and rounds it onto the tick grid against the customer.
func protectionPrice(indicative money.Quantity, side Side, collar money.BPS, priceScale int32, tick money.Quantity) (money.Quantity, error) {
	m := Mark{Mantissa: indicative, Scale: priceScale}
	if side == SideBuy {
		return adjustedPrice(m, true, collar, priceScale, tick, money.RoundCeil)
	}
	p, err := adjustedPrice(m, false, collar, priceScale, tick, money.RoundFloor)
	if err != nil {
		return money.Quantity{}, err
	}
	if !p.IsPositive() {
		return tick, nil
	}
	return p, nil
}

func feeSide(s Side) fees.Side {
	if s == SideBuy {
		return fees.SideBuy
	}
	return fees.SideSell
}

// priceOrder prices qty of the order at a reference price (mantissa at the
// instrument's price scale) under the settlement asset's fee schedule.
func (s *Service) priceOrder(o Order, l Listing, reference money.Quantity, strategy bool, at time.Time) (fees.Quote, error) {
	sch, ok := s.schedules[l.CashAsset.ID]
	if !ok {
		return fees.Quote{}, errs.New(errs.CodeInternal, "equities: no fee schedule covers this settlement asset")
	}
	price, err := money.NewPrice(reference, o.PriceScale, l.CashAsset.ID.String(), "equities:"+o.Broker, at)
	if err != nil {
		return fees.Quote{}, errs.Wrap(err, errs.CodeValidationFailed, "equities: reference price")
	}
	return sch.Quote(fees.QuoteInput{Side: feeSide(o.Side), BaseQty: o.Quantity, BaseDecimals: o.QuantityDecimals,
		QuoteDecimals: l.CashAsset.Decimals, ReferencePrice: price, StrategyOriginated: strategy})
}

// blockingAction returns an effective, unapplied corporate action that must
// stop this order: a split, reverse split or symbol change stops every order;
// a delisting that has begun stops buys, and one past its final date stops
// everything.
func (s *Service) blockingAction(ctx context.Context, q db.Querier, iid instruments.InstrumentID, side Side, today Date) (*CorporateAction, error) {
	actions, err := openActions(ctx, q, iid)
	if err != nil {
		return nil, err
	}
	for i := range actions {
		a := actions[i]
		switch {
		case a.Type.blocksTrading() && !a.EffectiveDate.After(today):
			return &a, nil
		case a.Type == ActionDelisting && a.Status == ActionAnnounced && !a.EffectiveDate.After(today) && side == SideBuy:
			return &a, nil
		case a.Type == ActionDelisting && a.Status == ActionApplying && !a.FinalDate.After(today):
			return &a, nil
		}
	}
	return nil, nil
}

func (s *Service) evaluateRisk(ctx context.Context, tx pgx.Tx, in econ.Intent, l Listing, side Side, sessionOpen bool,
	mark *Mark, notional money.USD, now time.Time) (risk.AdvancedDecision, risk.DecisionID, error) {
	acct, err := s.accounts.Get(ctx, tx, in.AccountID)
	if err != nil {
		return risk.AdvancedDecision{}, risk.DecisionID{}, err
	}
	level, err := s.verification.Level(ctx, tx, in.AccountID)
	if err != nil {
		return risk.AdvancedDecision{}, risk.DecisionID{}, err
	}
	snap, err := s.kills.Snapshot(ctx, tx)
	if err != nil {
		return risk.AdvancedDecision{}, risk.DecisionID{}, err
	}
	switches := make([]risk.KillSwitch, 0, len(snap.Switches))
	for _, sw := range snap.Switches {
		if sw.Active {
			switches = append(switches, risk.KillSwitch{Kind: string(sw.Kind), Scope: sw.ScopeID})
		}
	}
	oracle := &risk.OracleSnapshot{Available: false}
	if mark != nil {
		oracle = &risk.OracleSnapshot{Available: true, ObservedAt: mark.ObservedAt}
	}
	input := risk.AdvancedInput{
		Stage: risk.StagePreTrade,
		Intent: risk.AdvancedIntent{
			ID: in.ID.String(), Kind: string(in.Kind), AccountID: in.AccountID.String(), ActorType: string(in.ActorType),
			AgentID: in.AgentID, StrategyRef: in.StrategyRef, Capability: string(gates.Equities), Mode: string(in.Mode),
			InstrumentID: l.Instrument.ID.String(), InstrumentKind: string(l.Instrument.Type), NotionalUSD: notional,
			RiskIncreasing: side == SideBuy,
		},
		Identity:     &risk.IdentitySnapshot{Verified: level.AtLeast(valuedomain.VerificationNodalIdentity)},
		Account:      &risk.AdvancedAccountSnapshot{Status: string(acct.Status)},
		Instrument:   &risk.InstrumentSnapshot{Status: string(l.Instrument.Status), SessionOpen: sessionOpen},
		Oracle:       oracle,
		KillSwitches: switches,
		Now:          now,
	}
	// The authority and strategy sections come from their registers when the
	// composition root wires them; without one the section stays missing and
	// the kernel refuses the order.
	subject := RiskSubject{Intent: in, NotionalUSD: notional, At: now}
	if in.ActorType == security.ActorAgent && s.authority != nil {
		a, err := s.authority.Authority(ctx, tx, subject)
		if err != nil {
			return risk.AdvancedDecision{}, risk.DecisionID{}, err
		}
		input.Authority = &a
	}
	if in.StrategyRef != "" && s.strategies != nil {
		st, err := s.strategies.Strategy(ctx, tx, subject)
		if err != nil {
			return risk.AdvancedDecision{}, risk.DecisionID{}, err
		}
		input.Strategy = &st
	}
	d := risk.EvaluateAdvanced(s.riskPolicy, input)
	did, err := s.riskStore.RecordAdvancedDecision(ctx, tx, input, d, in.CorrelationID)
	if err != nil {
		return risk.AdvancedDecision{}, risk.DecisionID{}, storeErr("record risk decision", err)
	}
	return d, did, nil
}

func fieldStrings(err error, field string) []string {
	ee, ok := errs.As(err)
	if !ok {
		return nil
	}
	v, _ := ee.Fields[field].([]string)
	return v
}

func submitKey(oid OrderID) string { return "equities:submit:" + oid.String() }
func tradeKey(oid OrderID) string  { return "equities:trade:" + oid.String() }

type submission struct {
	OrderID         string          `json:"order_id"`
	Broker          string          `json:"broker"`
	Symbol          string          `json:"symbol"`
	ExchangeMIC     string          `json:"exchange_mic"`
	Side            Side            `json:"side"`
	Type            OrderType       `json:"order_type"`
	Quantity        money.Quantity  `json:"quantity"`
	LimitPrice      *money.Quantity `json:"limit_price,omitempty"`
	ProtectionPrice *money.Quantity `json:"protection_price,omitempty"`
	PriceScale      int32           `json:"price_scale"`
	ExtendedHours   bool            `json:"extended_hours"`
	ExpiresAt       time.Time       `json:"expires_at"`
}

func submissionRequest(o Order) submission {
	return submission{OrderID: o.ID.String(), Broker: o.Broker, Symbol: o.Symbol, ExchangeMIC: o.ExchangeMIC, Side: o.Side,
		Type: o.Type, Quantity: o.Quantity, LimitPrice: o.LimitPrice, ProtectionPrice: o.ProtectionPrice,
		PriceScale: o.PriceScale, ExtendedHours: o.ExtendedHours, ExpiresAt: o.ExpiresAt.UTC()}
}

// Order returns one order.
func (s *Service) Order(ctx context.Context, q db.Querier, oid OrderID) (Order, error) {
	return getOrder(ctx, q, oid, false)
}

// OrderForIntent returns the order an intent became, if any.
func (s *Service) OrderForIntent(ctx context.Context, q db.Querier, iid econ.IntentID) (Order, bool, error) {
	return orderByIntent(ctx, q, iid)
}

// OpenOrders returns orders the venue still owes an answer for (ACCEPTED and
// OPEN), oldest first: the worklist of a venue poller.
func (s *Service) OpenOrders(ctx context.Context, q db.Querier, limit int) ([]Order, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := q.Query(ctx, `SELECT `+orderColumns+` FROM equity_orders WHERE status IN ('ACCEPTED','OPEN')
		ORDER BY created_at, id LIMIT $1`, limit)
	if err != nil {
		return nil, storeErr("list open orders", err)
	}
	defer rows.Close()
	var out []Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, storeErr("scan order", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// venueContext loads what a venue request needs for an order still owed an
// answer.
func (s *Service) venueContext(ctx context.Context, q db.Querier, oid OrderID) (Order, *Mark, time.Time, error) {
	o, err := getOrder(ctx, q, oid, false)
	if err != nil {
		return Order{}, nil, time.Time{}, err
	}
	if o.Status != OrderAccepted && o.Status != OrderOpen {
		return Order{}, nil, time.Time{}, errs.Newf(errs.CodeInvalidStateTransition, "equities: order %s is %s; the venue owes it nothing", o.ID, o.Status)
	}
	l, err := s.LoadListing(ctx, q, o.InstrumentID)
	if err != nil {
		return Order{}, nil, time.Time{}, err
	}
	now := s.clk.Now().UTC()
	blocked, err := s.executionBlocked(ctx, q, l, o)
	if err != nil {
		return Order{}, nil, time.Time{}, err
	}
	if blocked {
		// The venue is shown no mark, so nothing executes: a resting limit
		// order keeps resting (and can still be cancelled), and a market
		// order is refused.
		return o, nil, now, nil
	}
	mark, err := s.markFor(ctx, q, l, now)
	if err != nil {
		return Order{}, nil, time.Time{}, err
	}
	return o, mark, now, nil
}

// executionBlocked reports whether an order already at the venue may not
// execute now. It mirrors what AcceptOrder would refuse for a new order: an
// instrument that does not trade (HALTED, RESTRICTED, DELISTED), a buy on an
// instrument that only closes (CLOSE_ONLY, DELISTING), and the kill-switch
// matrix for the order's class -- a buy is NEW_RISK, a sell REDUCE_RISK --
// on its account, instrument and capability.
func (s *Service) executionBlocked(ctx context.Context, q db.Querier, l Listing, o Order) (bool, error) {
	switch l.Instrument.Status {
	case assets.StatusActive:
	case assets.StatusCloseOnly, assets.StatusDelisting:
		if o.Side == SideBuy {
			return true, nil
		}
	default:
		return true, nil
	}
	snap, err := s.kills.Snapshot(ctx, q)
	if err != nil {
		return false, err
	}
	class := killswitch.ReduceRisk
	if o.Side == SideBuy {
		class = killswitch.NewRisk
	}
	_, blocked := snap.Blocking(killswitch.Action{Class: class, AccountID: o.AccountID.String(),
		InstrumentID: l.Instrument.ID.String(), Capability: string(gates.Equities)}, killswitch.Policy{})
	return blocked, nil
}

// PlaceAtVenue submits an ACCEPTED order to the venue with the current mark.
// It writes nothing; the caller applies the answer with ApplyOrderReport in a
// transaction of its own, or MarkSubmissionUnknown when there is none. Only a
// PENDING submission is sent: one whose outcome is UNKNOWN is never sent
// again, and is resolved with PollVenue.
func (s *Service) PlaceAtVenue(ctx context.Context, q db.Querier, oid OrderID) (OrderReport, error) {
	o, mark, now, err := s.venueContext(ctx, q, oid)
	if err != nil {
		return OrderReport{}, err
	}
	if o.Status != OrderAccepted {
		return OrderReport{}, errs.Newf(errs.CodeInvalidStateTransition, "equities: order %s is %s and was placed already", o.ID, o.Status)
	}
	e, ok, err := s.effects.GetByKey(ctx, q, submitKey(o.ID))
	if err != nil {
		return OrderReport{}, err
	}
	if !ok {
		return OrderReport{}, errs.New(errs.CodeInternal, "equities: an accepted order has no submission effect")
	}
	if e.Status != econ.EffectPending {
		return OrderReport{}, errs.Newf(errs.CodeReconciliationRequired,
			"equities: the submission of order %s is %s; it is resolved by polling the venue, never sent again", o.ID, e.Status).
			WithField("order_id", o.ID.String()).WithField("effect_id", e.ID.String())
	}
	return s.adapter.PlaceOrder(ctx, PlaceOrderRequest{Order: o.BrokerOrder(), Mark: mark, At: now})
}

// PollVenue asks the venue for its current answer for an ACCEPTED or OPEN
// order: a resting limit order fills when the mark crosses, and every order
// expires at the end of its day. It writes nothing.
func (s *Service) PollVenue(ctx context.Context, q db.Querier, oid OrderID) (OrderReport, error) {
	o, mark, now, err := s.venueContext(ctx, q, oid)
	if err != nil {
		return OrderReport{}, err
	}
	return s.adapter.OrderStatus(ctx, OrderStatusRequest{BrokerOrderID: o.BrokerOrderID, Order: o.BrokerOrder(), Mark: mark, At: now})
}

// CancelAtVenue asks the venue to cancel an ACCEPTED or OPEN order. It writes
// nothing; the answer -- CANCELLED, or the fill that beat the cancel -- is
// applied with ApplyOrderReport.
func (s *Service) CancelAtVenue(ctx context.Context, q db.Querier, oid OrderID) (OrderReport, error) {
	o, mark, now, err := s.venueContext(ctx, q, oid)
	if err != nil {
		return OrderReport{}, err
	}
	return s.adapter.Cancel(ctx, CancelRequest{BrokerOrderID: o.BrokerOrderID, Order: o.BrokerOrder(), Mark: mark, At: now})
}

// MarkSubmissionUnknown records that the venue's answer to a submission is
// not known (a timeout, a lost connection). The submission effect becomes
// UNKNOWN and is never retried: the order is resolved by polling the venue
// and applying what it says.
func (s *Service) MarkSubmissionUnknown(ctx context.Context, tx pgx.Tx, oid OrderID, reason string) error {
	o, err := getOrder(ctx, tx, oid, true)
	if err != nil {
		return err
	}
	if o.Status != OrderAccepted {
		return errs.Newf(errs.CodeInvalidStateTransition, "equities: order %s is %s; its submission outcome is known", o.ID, o.Status)
	}
	e, ok, err := s.effects.GetByKey(ctx, tx, submitKey(o.ID))
	if err != nil {
		return err
	}
	if !ok {
		return errs.New(errs.CodeInternal, "equities: an accepted order has no submission effect")
	}
	_, err = s.effects.MarkUnknown(ctx, tx, e.ID, "", s.actor, reason)
	return err
}

// ApplyOrderReport applies the venue's answer for an order, exactly once.
//
//   - OPEN: the order rests at the venue.
//   - FILLED: the trade is priced under the fee schedule, the quote is
//     re-verified with fees.VerifyQuote, the SIM_TRADE posting is made, the
//     reservation is consumed, the fill (with its full disclosure and the
//     schedule that produced it) and its T+1 settlement are recorded.
//   - REJECTED, CANCELLED, EXPIRED: the reservation is released.
//
// An answer for a terminal order is a replay when it agrees with what was
// recorded (the same fill) and a CONFLICT when it does not; nothing is
// applied twice.
func (s *Service) ApplyOrderReport(ctx context.Context, tx pgx.Tx, oid OrderID, r OrderReport) (Order, error) {
	if err := r.Validate(); err != nil {
		return Order{}, err
	}
	o, err := getOrder(ctx, tx, oid, true)
	if err != nil {
		return Order{}, err
	}
	if r.ClientOrderID != o.ID.String() || r.Broker != o.Broker {
		return Order{}, errs.New(errs.CodeValidationFailed, "equities: the report is for another order or venue").
			WithField("order_id", o.ID.String())
	}
	if o.BrokerOrderID != "" && r.BrokerOrderID != o.BrokerOrderID {
		return Order{}, errs.New(errs.CodeConflict, "equities: the report names a different broker order").
			WithField("order_id", o.ID.String())
	}
	if o.Status.Terminal() {
		return s.replayReport(ctx, tx, o, r)
	}
	now := s.clk.Now().UTC()
	switch r.Status {
	case ReportOpen:
		if o.Status == OrderOpen {
			return o, nil
		}
		return s.applyOpen(ctx, tx, o, r, now)
	case ReportFilled:
		return s.applyFill(ctx, tx, o, r, now)
	case ReportRejected:
		if o.Status != OrderAccepted {
			return Order{}, errs.New(errs.CodeConflict, "equities: a venue cannot reject an order it already accepted").
				WithField("order_id", o.ID.String())
		}
		return s.applyClose(ctx, tx, o, r, OrderRejected, now)
	case ReportCancelled:
		return s.applyClose(ctx, tx, o, r, OrderCancelled, now)
	case ReportExpired:
		return s.applyClose(ctx, tx, o, r, OrderExpired, now)
	}
	return Order{}, errs.New(errs.CodeValidationFailed, "equities: unknown report status")
}

func reportStatusOf(s OrderStatus) ReportStatus {
	switch s {
	case OrderOpen:
		return ReportOpen
	case OrderFilled:
		return ReportFilled
	case OrderRejected:
		return ReportRejected
	case OrderCancelled:
		return ReportCancelled
	case OrderExpired:
		return ReportExpired
	}
	return ""
}

func (s *Service) replayReport(ctx context.Context, tx pgx.Tx, o Order, r OrderReport) (Order, error) {
	conflict := errs.Newf(errs.CodeConflict, "equities: order %s is %s and the venue's answer %s contradicts it", o.ID, o.Status, r.Status).
		WithField("order_id", o.ID.String())
	if reportStatusOf(o.Status) != r.Status {
		return Order{}, conflict
	}
	if o.Status != OrderFilled {
		return o, nil
	}
	f, ok, err := fillByOrder(ctx, tx, o.ID)
	if err != nil {
		return Order{}, err
	}
	if !ok || f.VenueFillID != r.Fill.VenueFillID || !f.Price.Equal(r.Fill.Price) || !f.Quantity.Equal(r.Fill.Quantity) {
		return Order{}, conflict
	}
	return o, nil
}

// settleSubmission records the venue's definitive answer on the submission
// effect of an order that was ACCEPTED: APPLIED when the venue took the
// order (OPEN or FILLED), FAILED when it did not.
func (s *Service) settleSubmission(ctx context.Context, tx pgx.Tx, o Order, r OrderReport, failure string) error {
	e, ok, err := s.effects.GetByKey(ctx, tx, submitKey(o.ID))
	if err != nil {
		return err
	}
	if !ok {
		return errs.New(errs.CodeInternal, "equities: an accepted order has no submission effect")
	}
	if e.Status != econ.EffectPending && e.Status != econ.EffectUnknown {
		return nil
	}
	if failure != "" {
		_, err = s.effects.Fail(ctx, tx, e.ID, failure, s.actor, "the venue answered "+string(r.Status))
		return err
	}
	_, err = s.effects.Apply(ctx, tx, e.ID, econ.ApplyRequest{ExternalRef: r.BrokerOrderID, Actor: s.actor,
		Reason: "the venue answered " + string(r.Status)})
	return err
}

func (s *Service) applyOpen(ctx context.Context, tx pgx.Tx, o Order, r OrderReport, now time.Time) (Order, error) {
	out, err := transitionOrder(ctx, tx, o, orderTransition{to: OrderOpen, actor: s.actor, reason: "resting at the venue",
		brokerOrderID: r.BrokerOrderID}, now)
	if err != nil {
		return Order{}, err
	}
	if err := s.settleSubmission(ctx, tx, o, r, ""); err != nil {
		return Order{}, err
	}
	if _, err := s.intents.Transition(ctx, tx, o.IntentID, econ.StatusExecuting, s.actor, "resting at the venue", ""); err != nil {
		return Order{}, err
	}
	return out, s.recordOrder(ctx, tx, out, "open")
}

func (s *Service) applyClose(ctx context.Context, tx pgx.Tx, o Order, r OrderReport, to OrderStatus, now time.Time) (Order, error) {
	if o.ReservationID != nil {
		if _, err := s.capital.Release(ctx, tx, *o.ReservationID, "equity order "+strings.ToLower(string(to))); err != nil {
			return Order{}, err
		}
	}
	t := orderTransition{to: to, actor: s.actor, reason: "venue answered " + string(r.Status), brokerOrderID: r.BrokerOrderID}
	if to == OrderRejected {
		t.rejectionCode, t.reasons = RejectVenue, nonEmpty(r.Reason)
	}
	out, err := transitionOrder(ctx, tx, o, t, now)
	if err != nil {
		return Order{}, err
	}
	if o.Status == OrderAccepted {
		if err := s.settleSubmission(ctx, tx, o, r, string(r.Status)); err != nil {
			return Order{}, err
		}
		intentTo, code := econ.StatusCancelled, ""
		if to == OrderRejected {
			intentTo, code = econ.StatusRejected, string(RejectVenue)
		}
		if _, err := s.intents.Transition(ctx, tx, o.IntentID, intentTo, s.actor, "equity order "+strings.ToLower(string(to)), code); err != nil {
			return Order{}, err
		}
	} else {
		if _, err := s.intents.Transition(ctx, tx, o.IntentID, econ.StatusFailed, s.actor,
			"equity order "+strings.ToLower(string(to))+" before it filled", ""); err != nil {
			return Order{}, err
		}
	}
	return out, s.recordOrder(ctx, tx, out, strings.ToLower(string(to)))
}

func nonEmpty(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return []string{s}
}

func (s *Service) recordOrder(ctx context.Context, tx pgx.Tx, o Order, what string) error {
	return s.record(ctx, tx, audit.AccountStream(o.AccountID.String()), "equities.order."+what, "equity_order", o.ID.String(),
		"equity order "+what, map[string]string{
			"order_id": o.ID.String(), "intent_id": o.IntentID.String(), "status": string(o.Status),
			"side": string(o.Side), "order_type": string(o.Type), "quantity": o.Quantity.String(),
			"rejection_code": string(o.RejectionCode),
		})
}
