package orders

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/agentauthority"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/matching"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/settlement"
)

// A customer order is the execution of one economic intent, and the intent's
// status machine moves one step per transaction: the database binds every
// status change to a transition row describing exactly that change, and a
// row whose status changed twice in one transaction cannot satisfy that
// (AU001). The submission is therefore a sequence of stages, each its own
// transaction, each idempotent and resumable, each recording its refusal
// rather than throwing it away:
//
//	Accept   RECEIVED -> ROUTED         intent recorded, order PENDING, settlement compiler
//	Approve  ROUTED -> RISK_APPROVED    capability mode (econ.Authorizer), Risk Kernel V2
//	Reserve  RISK_APPROVED -> RESERVED  capital reservation (quote for a buy, base for a sell)
//	Execute  RESERVED -> EXECUTING      book lock, crossing policy, effect, engine, trades
//	Complete EXECUTING -> COMPLETED     once the order is filled or cancelled
//
// Any stage may end the intent REJECTED with a stable code (and the order with
// it). A resting order's intent stays EXECUTING until the order is filled or
// cancelled, which completes it in that command's transaction. Pipeline runs
// the stages; a crashed submission is resumed with Pipeline.Resume, and
// CompleteTerminal sweeps intents whose orders finished.

// RiskContext carries the risk sections an intent needs that only the caller
// can resolve: delegated authority for an agent, the strategy's approval for
// a strategy-originated order, the sandbox's verdict for sandbox-originated
// code. They become part of the intent's terms, so they are content-hashed.
type RiskContext struct {
	Authority *risk.AuthoritySnapshot `json:"authority,omitempty"`
	Strategy  *risk.StrategySnapshot  `json:"strategy,omitempty"`
	Sandbox   *risk.SandboxSnapshot   `json:"sandbox,omitempty"`
}

// OrderRequest is a customer order on an internal book.
type OrderRequest struct {
	InstrumentID instruments.InstrumentID
	AccountID    accounts.AccountID
	// Mode defaults to the book's mode and must equal it.
	Mode        gates.Mode
	Side        Side
	Type        OrderType
	TimeInForce TimeInForce
	// Price is the limit price, or a market order's protection price, which
	// this book requires: a market buy without one could not be reserved.
	Price *money.Price
	// Quantity is in base units of the base asset.
	Quantity   money.Quantity
	PostOnly   bool
	ReduceOnly bool
	// STP defaults to CANCEL_TAKER.
	STP            STPMode
	ClientOrderID  string
	IdempotencyKey string
	ActorType      security.ActorType
	ActorID        string
	AgentID        string
	AgentAuthority agentauthority.Level
	StrategyRef    string
	DelegationID   string
	CorrelationID  string
	Jurisdiction   string
	Risk           RiskContext
}

// orderShape is the part of an order that does not depend on the book.
type orderShape struct {
	Side          Side
	Type          OrderType
	TIF           TimeInForce
	STP           STPMode
	PostOnly      bool
	ReduceOnly    bool
	ClientOrderID string
}

func (o orderShape) withDefaults() orderShape {
	if o.STP == "" {
		o.STP = STPCancelTaker
	}
	return o
}

func (o orderShape) validate() error {
	switch {
	case !o.Side.Valid():
		return rejection(ReasonInvalidSide, "side must be BUY or SELL")
	case !o.Type.Valid():
		return rejection(ReasonInvalidType, "order type must be LIMIT or MARKET")
	case !o.TIF.Valid():
		return rejection(ReasonInvalidTIF, "time in force must be GTC, IOC or FOK")
	case o.STP != "" && !o.STP.Valid():
		return rejection(ReasonInvalidSTP, "unknown self-trade prevention mode")
	case o.Type == TypeMarket && o.TIF == GTC:
		return rejection(ReasonMarketGTC, "a market order never rests: use IOC or FOK")
	case o.PostOnly && (o.Type != TypeLimit || o.TIF != GTC):
		return rejection(ReasonPostOnlyShape, "post-only is a GTC limit order")
	case o.ReduceOnly && o.Side == SideBuy:
		return rejection(ReasonReduceOnlyBuy, "spot has no short position: reduce-only is a sell capped at holdings")
	}
	if o.ClientOrderID != "" {
		if len(o.ClientOrderID) > matching.MaxIDBytes || strings.TrimSpace(o.ClientOrderID) != o.ClientOrderID {
			return rejection(ReasonClientOrderID, "client order id must be 1..128 bytes without surrounding whitespace")
		}
		for _, r := range o.ClientOrderID {
			if unicode.IsControl(r) {
				return rejection(ReasonClientOrderID, "client order id must not contain control characters")
			}
		}
	}
	return nil
}

func validKey(k string) error {
	if strings.TrimSpace(k) == "" || strings.TrimSpace(k) != k || len(k) > 256 {
		return rejection(ReasonInvalidRequest, "idempotency key must be 1..256 bytes without surrounding whitespace")
	}
	return nil
}

// convert turns a price and a quantity into engine units, exactly or not at all.
func convert(t BookTerms, price *money.Price, qty money.Quantity) (int64, int64, error) {
	if price == nil {
		return 0, 0, rejection(ReasonPriceRequired, "a limit price, or a market order's protection price, is required")
	}
	ticks, err := t.PriceToTicks(*price)
	if err != nil {
		return 0, 0, err
	}
	lots, err := t.QuantityToLots(qty)
	if err != nil {
		return 0, 0, err
	}
	if err := t.CheckNotional(ticks, lots); err != nil {
		return 0, 0, err
	}
	return ticks, lots, nil
}

// checkTermsVersion refuses new risk on a book whose instrument's canonical
// terms moved on since the book was opened: its conversions would no longer
// be the instrument's.
func (s *Service) checkTermsVersion(ctx context.Context, q db.Querier, c *bookConfig) error {
	if c.TermsSource != TermsCanonical {
		return nil
	}
	v, err := s.instr.CurrentTerms(ctx, q, c.InstrumentID)
	if err != nil {
		return err
	}
	if v.Version != c.TermsVersion {
		return rejection(ReasonTermsChanged, "the instrument's terms changed since the book was opened")
	}
	return nil
}

func clientOrderIDTaken(ctx context.Context, q db.Querier, ownerID, clientID string) (bool, error) {
	if clientID == "" {
		return false, nil
	}
	var taken bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM book_orders WHERE owner_id = $1::uuid AND client_order_id = $2
		AND status <> 'REJECTED')`, ownerID, clientID).Scan(&taken); err != nil {
		return false, dbErr("check client order id", err)
	}
	return taken, nil
}

// orderTerms are the typed terms of an order intent. Prices and quantities
// are decimal strings; the engine units are recorded beside them.
type orderTerms struct {
	Venue           string          `json:"venue"`
	VenueLabel      string          `json:"venue_label"`
	BookID          string          `json:"book_id"`
	Side            Side            `json:"side"`
	OrderType       OrderType       `json:"order_type"`
	TimeInForce     TimeInForce     `json:"time_in_force"`
	Price           string          `json:"price"`
	PriceTicks      int64           `json:"price_ticks"`
	Quantity        string          `json:"quantity"`
	QtyLots         int64           `json:"qty_lots"`
	PostOnly        bool            `json:"post_only"`
	ReduceOnly      bool            `json:"reduce_only"`
	STP             STPMode         `json:"stp"`
	ClientOrderID   string          `json:"client_order_id"`
	LiquidityIntent LiquidityIntent `json:"liquidity_intent"`
	Risk            *RiskContext    `json:"risk_context,omitempty"`
}

func decodeOrderTerms(raw json.RawMessage) (orderTerms, error) {
	var t orderTerms
	if err := json.Unmarshal(raw, &t); err != nil {
		return orderTerms{}, errs.Wrap(err, errs.CodeInternal, "orders: intent terms are not order terms")
	}
	return t, nil
}

// StageOutcome is the state after one stage.
type StageOutcome struct {
	Order  OrderView
	Intent econ.Intent
	// CommandSeq, Trades and StateHash are set by the Execute stage when the
	// order reached the book.
	CommandSeq int64
	Trades     []Trade
	StateHash  [32]byte
	// Rejection is a refusal that this or an earlier stage recorded. The
	// transaction must still commit: the refusal is the stage's result.
	Rejection error
}

// Accept records the intent and the PENDING order and routes the intent
// through the settlement compiler. It is idempotent on (account, idempotency
// key): a replay returns the order the key created.
func (s *Service) Accept(ctx context.Context, tx pgx.Tx, r OrderRequest) (StageOutcome, error) {
	if r.AccountID.IsZero() {
		return StageOutcome{}, rejection(ReasonInvalidRequest, "account required")
	}
	if err := validKey(r.IdempotencyKey); err != nil {
		return StageOutcome{}, err
	}
	shape := orderShape{Side: r.Side, Type: r.Type, TIF: r.TimeInForce, STP: r.STP, PostOnly: r.PostOnly, ReduceOnly: r.ReduceOnly, ClientOrderID: r.ClientOrderID}
	if err := shape.validate(); err != nil {
		return StageOutcome{}, err
	}
	shape = shape.withDefaults()
	c, cur, err := s.readBook(ctx, tx, r.InstrumentID, false)
	if err != nil {
		return StageOutcome{}, err
	}
	mode := r.Mode
	if mode == "" {
		mode = c.Mode
	}
	if mode != c.Mode {
		return StageOutcome{}, errs.Newf(errs.CodeValidationFailed, "orders: this book runs in %s", c.Mode).WithField("reason", ReasonInvalidRequest)
	}
	if cur.Status != BookOpen {
		return StageOutcome{}, rejection(ReasonBookNotOpen, "book is "+string(cur.Status))
	}
	ticks, lots, err := convert(c.Terms, r.Price, r.Quantity)
	if err != nil {
		return StageOutcome{}, err
	}
	if err := s.checkTermsVersion(ctx, tx, c); err != nil {
		return StageOutcome{}, err
	}
	terms := orderTerms{
		Venue: "INTERNAL_BOOK", VenueLabel: VenueLabelInternalBook, BookID: c.ID.String(), Side: shape.Side,
		OrderType: shape.Type, TimeInForce: shape.TIF, Price: c.Terms.PriceString(ticks), PriceTicks: ticks,
		Quantity: c.Terms.LotsToQuantity(lots).String(), QtyLots: lots, PostOnly: shape.PostOnly, ReduceOnly: shape.ReduceOnly,
		STP: shape.STP, ClientOrderID: shape.ClientOrderID, LiquidityIntent: liquidityIntentOf(shape.Type, shape.TIF),
	}
	if r.Risk.Authority != nil || r.Risk.Strategy != nil || r.Risk.Sandbox != nil {
		rc := r.Risk
		terms.Risk = &rc
	}
	rawTerms, err := json.Marshal(terms)
	if err != nil {
		return StageOutcome{}, errs.Wrap(err, errs.CodeInternal, "orders: encode intent terms")
	}
	kind := econ.KindAcquireExposure
	if shape.Side == SideSell {
		kind = econ.KindReduceExposure
	}
	base := c.Terms.BaseAsset
	qty := c.Terms.LotsToQuantity(lots)
	in, existed, err := s.intents.Submit(ctx, tx, econ.Intent{
		AccountID: r.AccountID, Kind: kind, Mode: mode, Capability: gates.InternalMatching,
		ActorType: r.ActorType, ActorID: r.ActorID, AgentID: r.AgentID, StrategyRef: r.StrategyRef,
		DelegationID: r.DelegationID, InstrumentID: c.InstrumentID.String(), AssetID: &base, Quantity: &qty,
		Terms: rawTerms, IdempotencyKey: r.IdempotencyKey, CorrelationID: r.CorrelationID,
	})
	if err != nil {
		return StageOutcome{}, err
	}
	oid := OrderID(in.ID)
	if existed {
		o, err := loadOrder(ctx, tx, oid, false)
		if err != nil {
			return StageOutcome{}, err
		}
		return s.outcome(c, o, in), nil
	}
	account, iid := r.AccountID, in.ID
	o := orderRow{
		ID: oid, BookID: c.ID, InstrumentID: c.InstrumentID, OwnerType: OwnerCustomer, OwnerID: r.AccountID.String(),
		AccountID: &account, IntentID: &iid, IdempotencyKey: r.IdempotencyKey, RequestHash: in.ContentHash,
		ClientOrderID: shape.ClientOrderID, Side: shape.Side, Type: shape.Type, TIF: shape.TIF, STP: shape.STP,
		Liquidity: terms.LiquidityIntent, PostOnly: shape.PostOnly, ReduceOnly: shape.ReduceOnly,
		StrategyOriginated: r.StrategyRef != "", Mode: mode, PriceTicks: ticks, QtyLots: lots, RemainingLots: lots,
		Status: StatusPending,
	}
	level, err := s.verify.Level(ctx, tx, r.AccountID)
	if err != nil {
		return StageOutcome{}, err
	}
	route, err := econ.Route(econ.RouteRequest{
		Intent:         in,
		Subject:        settlement.Subject{Type: settlement.SubjectInstrument, ID: c.InstrumentID.String(), AssetID: base.String()},
		AgentAuthority: r.AgentAuthority, Jurisdiction: r.Jurisdiction, Verification: level,
	}, s.router, s.clk.Now().UTC())
	if err != nil {
		return StageOutcome{}, err
	}
	if !route.MayExecuteNow() {
		reasons := strings.Join(route.Reasons, ",")
		if reasons == "" {
			reasons = "confirmation required"
		}
		return s.recordAcceptRejection(ctx, tx, c, o, in, ReasonRouteRefused,
			rejection(ReasonRouteRefused, "the settlement compiler refused the order: "+reasons).WithField("route_reasons", route.Reasons))
	}
	if taken, err := clientOrderIDTaken(ctx, tx, o.OwnerID, o.ClientOrderID); err != nil {
		return StageOutcome{}, err
	} else if taken {
		return s.recordAcceptRejection(ctx, tx, c, o, in, ReasonDuplicateClientOrderID,
			rejection(ReasonDuplicateClientOrderID, "this client order id is already used by the account"))
	}
	// A concurrent submission can take the client order id between the check
	// and the insert; the unique index decides, inside a savepoint so the
	// refusal can still be recorded.
	sp, err := tx.Begin(ctx)
	if err != nil {
		return StageOutcome{}, dbErr("savepoint", err)
	}
	if err := insertOrder(ctx, sp, o); err != nil {
		_ = sp.Rollback(ctx)
		if db.IsUniqueViolation(err) && db.ConstraintName(err) == "book_orders_owner_client_order_id" {
			return s.recordAcceptRejection(ctx, tx, c, o, in, ReasonDuplicateClientOrderID,
				rejection(ReasonDuplicateClientOrderID, "this client order id is already used by the account"))
		}
		return StageOutcome{}, err
	}
	if err := sp.Commit(ctx); err != nil {
		return StageOutcome{}, dbErr("savepoint", err)
	}
	in, err = s.intents.Transition(ctx, tx, in.ID, econ.StatusRouted, s.actor, "routed: "+route.Product, "")
	if err != nil {
		return StageOutcome{}, err
	}
	return s.outcome(c, o, in), nil
}

// recordAcceptRejection records an order refused at acceptance: the order is
// born REJECTED and the intent moves RECEIVED -> REJECTED.
func (s *Service) recordAcceptRejection(ctx context.Context, tx pgx.Tx, c *bookConfig, o orderRow, in econ.Intent, reason string, rej error) (StageOutcome, error) {
	o.Status, o.RejectReason = StatusRejected, reason
	o.RemainingLots = 0
	if err := insertOrder(ctx, tx, o); err != nil {
		return StageOutcome{}, err
	}
	in, err := s.intents.Transition(ctx, tx, in.ID, econ.StatusRejected, s.actor, "rejected: "+reason, reason)
	if err != nil {
		return StageOutcome{}, err
	}
	out := s.outcome(c, o, in)
	out.Rejection = rej
	return out, nil
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// stageStart locks an order and then its intent for a stage. Every path in
// this package that holds both takes them in this order (book, order,
// intent), as runCommand does when it completes a maker's intent, so a stage
// and a command that fills the same order cannot deadlock.
func (s *Service) stageStart(ctx context.Context, tx pgx.Tx, oid OrderID) (*bookConfig, bookCursor, orderRow, econ.Intent, error) {
	o, err := loadOrder(ctx, tx, oid, true)
	if err != nil {
		return nil, bookCursor{}, orderRow{}, econ.Intent{}, err
	}
	if o.OwnerType != OwnerCustomer || o.IntentID == nil {
		return nil, bookCursor{}, orderRow{}, econ.Intent{}, errs.New(errs.CodeValidationFailed, "orders: only a customer order has stages")
	}
	c, cur, err := s.readBookByID(ctx, tx, o.BookID, false)
	if err != nil {
		return nil, bookCursor{}, orderRow{}, econ.Intent{}, err
	}
	in, err := lockIntent(ctx, tx, s.intents, *o.IntentID)
	if err != nil {
		return nil, bookCursor{}, orderRow{}, econ.Intent{}, err
	}
	return c, cur, o, in, nil
}

// Submit carries a customer order through every stage -- Accept, Approve,
// Reserve, Execute, Complete -- atomically, in the caller's transaction: the
// order and every refusal are committed together or not at all. It takes the
// book's row lock first, so every lock it takes afterwards is one a command
// holding the book may also want, in the same order. A refusal is returned in
// StageOutcome.Rejection with a nil error: the caller commits it. Pipeline
// runs the same stages one transaction each, which keeps the book lock only
// for Execute.
func (s *Service) Submit(ctx context.Context, tx pgx.Tx, r OrderRequest) (StageOutcome, error) {
	if _, _, err := s.lockBook(ctx, tx, r.InstrumentID); err != nil {
		return StageOutcome{}, err
	}
	out, err := s.Accept(ctx, tx, r)
	if err != nil || out.Rejection != nil {
		return out, err
	}
	oid := out.Order.ID
	var executed StageOutcome
	for i, stage := range []func(context.Context, pgx.Tx, OrderID) (StageOutcome, error){s.Approve, s.Reserve, s.Execute, s.Complete} {
		if out, err = stage(ctx, tx, oid); err != nil {
			return StageOutcome{}, err
		}
		if i == 2 {
			executed = out
		}
		if out.Rejection != nil {
			return out, nil
		}
	}
	out.CommandSeq, out.Trades, out.StateHash = executed.CommandSeq, executed.Trades, executed.StateHash
	return out, nil
}

// settled reports the outcome of a stage that has nothing left to do because
// the intent is already past it.
func (s *Service) settled(c *bookConfig, o orderRow, in econ.Intent) StageOutcome {
	out := s.outcome(c, o, in)
	switch in.Status {
	case econ.StatusRejected:
		out.Rejection = rejection(reasonOr(o.RejectReason, in.RejectionCode), "the order was rejected: "+reasonOr(o.RejectReason, in.RejectionCode))
	case econ.StatusCancelled:
		out.Rejection = rejection(ReasonCancelledBeforeBook, "the order was cancelled before it reached the book")
	}
	return out
}

// rejectPending ends a PENDING order and its intent with a stable reason,
// handing back any reservation.
func (s *Service) rejectPending(ctx context.Context, tx pgx.Tx, c *bookConfig, o orderRow, in econ.Intent, reason, intentCode string, rej error) (StageOutcome, error) {
	after := o
	after.Status, after.RejectReason = StatusRejected, reason
	after.RemainingLots = 0
	if err := updateOrder(ctx, tx, o, after, nil, s.actor, "rejected: "+reason, s.clk.Now().UTC()); err != nil {
		return StageOutcome{}, err
	}
	if err := s.release(ctx, tx, o, "order rejected: "+reason); err != nil {
		return StageOutcome{}, err
	}
	in, err := s.intents.Transition(ctx, tx, in.ID, econ.StatusRejected, s.actor, "rejected: "+reason, intentCode)
	if err != nil {
		return StageOutcome{}, err
	}
	out := s.outcome(c, after, in)
	out.Rejection = rej
	return out, nil
}

// Approve authorizes the capability in the order's mode and evaluates Risk
// Kernel V2 with every section the intent requires, persisting the decision.
func (s *Service) Approve(ctx context.Context, tx pgx.Tx, oid OrderID) (StageOutcome, error) {
	c, cur, o, in, err := s.stageStart(ctx, tx, oid)
	if err != nil {
		return StageOutcome{}, err
	}
	if in.Status != econ.StatusRouted {
		return s.settled(c, o, in), nil
	}
	if err := s.auth.Require(ctx, tx, gates.InternalMatching, o.Mode); err != nil {
		code := errs.CodeOf(err)
		if code == errs.CodeCapabilityNotApproved || code == errs.CodeKillSwitchActive {
			return s.rejectPending(ctx, tx, c, o, in, ReasonCapability, string(code), err)
		}
		return StageOutcome{}, err
	}
	if cur.Status != BookOpen {
		return s.rejectPending(ctx, tx, c, o, in, ReasonBookNotOpen, ReasonBookNotOpen, rejection(ReasonBookNotOpen, "book is "+string(cur.Status)))
	}
	terms, err := decodeOrderTerms(in.Terms)
	if err != nil {
		return StageOutcome{}, err
	}
	riskIncreasing := o.Side == SideBuy
	input, err := s.riskInput(ctx, tx, c, in, o, terms.Risk, o.PriceTicks, o.QtyLots, riskIncreasing, money.Quantity{})
	if err != nil {
		return StageOutcome{}, err
	}
	d := risk.EvaluateAdvanced(s.policy, input)
	if _, err := s.risk.RecordAdvancedDecision(ctx, tx, input, d, in.CorrelationID); err != nil {
		return StageOutcome{}, dbErr("record risk decision", err)
	}
	if d.Verdict != risk.Allow {
		return s.rejectPending(ctx, tx, c, o, in, riskReason(d.ReasonCodes), string(errs.CodeRiskRejected), d.RejectionError())
	}
	in, err = s.intents.Transition(ctx, tx, in.ID, econ.StatusRiskApproved, s.actor, "risk approved: "+d.Hash, "")
	if err != nil {
		return StageOutcome{}, err
	}
	return s.outcome(c, o, in), nil
}

// riskReason renders a rejection's risk codes as the order's stable reason,
// within the column's bound.
func riskReason(codes []string) string {
	out := ""
	for _, c := range codes {
		next := c
		if out != "" {
			next = out + "," + c
		}
		if len(next) > 128 {
			break
		}
		out = next
	}
	if out == "" {
		return ReasonRisk
	}
	return out
}

// Reserve reserves what the order can spend: for a buy, quantity x limit (or
// protection) price plus the most its fees can be; for a sell, the base
// quantity, which for a reduce-only sell is capped at the seller's available
// holdings.
func (s *Service) Reserve(ctx context.Context, tx pgx.Tx, oid OrderID) (StageOutcome, error) {
	c, _, o, in, err := s.stageStart(ctx, tx, oid)
	if err != nil {
		return StageOutcome{}, err
	}
	if in.Status != econ.StatusRiskApproved {
		return s.settled(c, o, in), nil
	}
	t := c.Terms
	asset, amount := t.QuoteAsset, money.Quantity{}
	switch o.Side {
	case SideBuy:
		notional := t.Notional(o.PriceTicks, o.QtyLots)
		bound, err := FeeBound(c.MakerSchedule, c.TakerSchedule, notional, o.StrategyOriginated)
		if err != nil {
			return StageOutcome{}, errs.Wrap(err, errs.CodeInternal, "orders: fee bound")
		}
		amount = notional.Add(bound)
	case SideSell:
		asset, amount = t.BaseAsset, t.LotsToQuantity(o.QtyLots)
		if o.ReduceOnly {
			avail, err := s.capital.Availability(ctx, tx, o.OwnerID, t.BaseAsset, s.clk.Now())
			if err != nil {
				return StageOutcome{}, err
			}
			capLots, err := avail.Available.Div(t.LotSize, money.RoundDown)
			if err != nil {
				return StageOutcome{}, errs.Wrap(err, errs.CodeInternal, "orders: reduce-only cap")
			}
			limit := min(clampLots(capLots), o.QtyLots)
			if limit < 1 {
				return s.rejectPending(ctx, tx, c, o, in, ReasonReduceOnlyNoPosition, ReasonReduceOnlyNoPosition,
					rejection(ReasonReduceOnlyNoPosition, "the account holds none of the base asset to reduce"))
			}
			amount = t.LotsToQuantity(limit)
		}
	}
	// The reservation names the economic intent it serves (00733).
	res, err := s.capital.Reserve(ctx, tx, capital.ReserveRequest{
		AccountID: o.OwnerID, AssetID: asset, Quantity: amount, EconomicIntentID: in.ID.String(),
		ActorType: s.actor.Type, ActorID: s.actor.ID, IdempotencyKey: "orders:reserve:" + o.ID.String(),
		TTL: s.resTTL, Reason: "internal-book order " + o.ID.String() + " (economic intent " + in.ID.String() + ")",
	})
	if err != nil {
		switch errs.CodeOf(err) {
		case errs.CodeInsufficientBuyingPower, errs.CodeAccountFrozen, errs.CodeAssetRestricted:
			return s.rejectPending(ctx, tx, c, o, in, ReasonInsufficientFunds, string(errs.CodeOf(err)), err)
		}
		return StageOutcome{}, err
	}
	after := o
	after.ReservationID = res.ID.String()
	if err := updateOrder(ctx, tx, o, after, nil, s.actor, "reserved", s.clk.Now().UTC()); err != nil {
		return StageOutcome{}, err
	}
	in, err = s.intents.Transition(ctx, tx, in.ID, econ.StatusReserved, s.actor, "reserved "+amount.String()+" of "+asset.String(), "")
	if err != nil {
		return StageOutcome{}, err
	}
	return s.outcome(c, after, in), nil
}

// Execute takes the book's lock and puts the order on the book: the crossing
// policy, the effect claim, the engine, and every trade the order makes.
func (s *Service) Execute(ctx context.Context, tx pgx.Tx, oid OrderID) (StageOutcome, error) {
	o0, err := loadOrder(ctx, tx, oid, false)
	if err != nil {
		return StageOutcome{}, err
	}
	// The book's lock first: every command, including a cancel of this very
	// order, serializes on it.
	c, cur, err := s.lockBook(ctx, tx, o0.InstrumentID)
	if err != nil {
		return StageOutcome{}, err
	}
	_, _, o, in, err := s.stageStart(ctx, tx, oid)
	if err != nil {
		return StageOutcome{}, err
	}
	if in.Status != econ.StatusReserved {
		return s.settled(c, o, in), nil
	}
	if o.Status != StatusPending {
		return StageOutcome{}, errs.New(errs.CodeInternal, "orders: a reserved intent's order is not pending").WithField("order_id", o.ID.String())
	}
	if cur.Status != BookOpen {
		return s.rejectPending(ctx, tx, c, o, in, ReasonBookNotOpen, ReasonBookNotOpen, rejection(ReasonBookNotOpen, "book is "+string(cur.Status)))
	}
	if err := s.checkTermsVersion(ctx, tx, c); err != nil {
		if ReasonOf(err) == ReasonTermsChanged {
			return s.rejectPending(ctx, tx, c, o, in, ReasonTermsChanged, ReasonTermsChanged, err)
		}
		return StageOutcome{}, err
	}
	rid, err := capital.ParseReservationID(o.ReservationID)
	if err != nil {
		return StageOutcome{}, errs.New(errs.CodeInternal, "orders: a reserved order names no reservation")
	}
	reservation, err := s.capital.Get(ctx, tx, rid)
	if err != nil {
		return StageOutcome{}, err
	}
	if reservation.Status != capital.ReservationActive {
		return s.rejectPending(ctx, tx, c, o, in, ReasonReservationLost, ReasonReservationLost,
			rejection(ReasonReservationLost, "the order's reservation is "+string(reservation.Status)))
	}
	if err := s.capital.LockForOrder(ctx, tx, rid, o.ID.String()); err != nil {
		return StageOutcome{}, err
	}
	reduceLimit := int64(0)
	if o.ReduceOnly {
		lots, err := reservation.Quantity.Div(c.Terms.LotSize, money.RoundExact)
		if err != nil {
			return StageOutcome{}, errs.Wrap(err, errs.CodeInternal, "orders: a reduce-only reservation is not whole lots")
		}
		reduceLimit = clampLots(lots)
	}
	eng, err := s.acquireEngine(ctx, tx, c, cur)
	if err != nil {
		return StageOutcome{}, err
	}
	defer eng.release()
	now := s.clk.Now().UTC()
	cmd := matching.NewOrder{
		Timestamp: now.UnixNano(), OrderID: o.ID.String(), ClientOrderID: o.ClientOrderID, OwnerID: o.OwnerID,
		Side: o.Side.engine(), Type: o.Type.engine(), TIF: o.TIF.engine(), STP: o.STP.engine(), PriceTicks: o.PriceTicks,
		QtyLots: o.QtyLots, PostOnly: o.PostOnly, ReduceOnly: o.ReduceOnly, ReduceOnlyLimitLots: reduceLimit,
	}
	iid := in.ID
	eff, existed, err := s.effects.Begin(ctx, tx, econ.BeginRequest{
		Key: "orders:submit:" + o.ID.String(), Subsystem: econ.SubsystemOrders, Kind: "ORDER_SUBMIT", IntentID: &iid,
		AccountID: o.OwnerID, Request: map[string]any{"order_id": o.ID.String(), "book_id": c.ID.String(),
			"owner_type": string(OwnerCustomer), "owner_id": o.OwnerID, "request_hash": hexOf(o.RequestHash)},
		PostsLedger: false, Actor: s.actor, CorrelationID: in.CorrelationID,
	})
	if err != nil {
		return StageOutcome{}, err
	}
	if existed {
		return StageOutcome{}, errs.New(errs.CodeConflict, "orders: this order's submission effect was already claimed")
	}
	decision, err := s.checkCrossing(ctx, tx, c, eng, CommandNewOrder, cmd, o.ID, OwnerCustomer, o.Side, o.PriceTicks, false)
	if err != nil {
		return StageOutcome{}, err
	}
	if !decision.Allowed {
		if _, err := s.effects.Fail(ctx, tx, eff.ID, ReasonHouseCrossingForbidden, s.actor, "house-versus-customer crossing is forbidden"); err != nil {
			return StageOutcome{}, err
		}
		return s.rejectPending(ctx, tx, c, o, in, ReasonHouseCrossingForbidden, ReasonHouseCrossingForbidden,
			rejection(ReasonHouseCrossingForbidden, "the order would trade against a house book"))
	}
	res, err := s.runCommand(ctx, tx, commandSpec{
		cfg: c, cur: cur, eng: eng, kind: CommandNewOrder, cmd: cmd, orderID: o.ID, actor: s.actor, now: now,
		stageTaker: &o.ID, crossingRuleVersion: decision.ruleVersionFor(),
	})
	if err != nil {
		return StageOutcome{}, err
	}
	co, err := s.finishCommandEffect(ctx, tx, c, eff, res, o.ID)
	if err != nil {
		return StageOutcome{}, err
	}
	if res.Rejected {
		in, err = s.intents.Transition(ctx, tx, in.ID, econ.StatusRejected, s.actor, "the book rejected the order: "+res.RejectReason, res.RejectReason)
	} else {
		in, err = s.intents.Transition(ctx, tx, in.ID, econ.StatusExecuting, s.actor, "on the book at command "+itoa(res.Seq), "")
	}
	if err != nil {
		return StageOutcome{}, err
	}
	return StageOutcome{Order: co.Order, Intent: in, CommandSeq: res.Seq, Trades: res.Trades, StateHash: res.StateHash, Rejection: co.Rejection}, nil
}

// Complete moves the intent of a filled or cancelled order to COMPLETED. An
// order still resting leaves its intent EXECUTING; whichever command finishes
// it completes it.
func (s *Service) Complete(ctx context.Context, tx pgx.Tx, oid OrderID) (StageOutcome, error) {
	c, _, o, in, err := s.stageStart(ctx, tx, oid)
	if err != nil {
		return StageOutcome{}, err
	}
	if in.Status == econ.StatusExecuting && o.Status.Terminal() && o.Status != StatusRejected {
		in, err = s.intents.Transition(ctx, tx, in.ID, econ.StatusCompleted, s.actor, "order "+string(o.Status), "")
		if err != nil {
			return StageOutcome{}, err
		}
	}
	return s.settled(c, o, in), nil
}

// CompleteTerminal completes up to limit EXECUTING intents whose orders are
// already filled or cancelled: the sweep that finishes submissions a crash
// interrupted between Execute and Complete. It returns how many it moved.
func (s *Service) CompleteTerminal(ctx context.Context, tx pgx.Tx, limit int) (int, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := tx.Query(ctx, `SELECT o.id, o.status FROM book_orders o JOIN economic_intents i ON i.id = o.intent_id
		WHERE i.status = 'EXECUTING' AND o.status IN ('FILLED','CANCELLED')
		ORDER BY o.updated_at, o.id LIMIT $1 FOR UPDATE OF i SKIP LOCKED`, limit)
	if err != nil {
		return 0, dbErr("find finished orders", err)
	}
	type pair struct {
		id     econ.IntentID
		status Status
	}
	var todo []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.id, &p.status); err != nil {
			rows.Close()
			return 0, dbErr("scan finished orders", err)
		}
		todo = append(todo, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, dbErr("find finished orders", err)
	}
	for _, p := range todo {
		if _, err := s.intents.Transition(ctx, tx, p.id, econ.StatusCompleted, s.actor, "order "+string(p.status), ""); err != nil {
			return 0, err
		}
	}
	return len(todo), nil
}
