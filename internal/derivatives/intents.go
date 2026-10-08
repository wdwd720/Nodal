package derivatives

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/agentauthority"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/settlement"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// IntentRequest is what every intent-driven derivatives operation carries:
// who asks, for which account and contract, in which simulated mode, under
// which idempotency key.
type IntentRequest struct {
	AccountID      accounts.AccountID
	InstrumentID   instruments.InstrumentID
	Mode           gates.Mode
	IdempotencyKey string
	ActorType      security.ActorType
	ActorID        string
	AgentID        string
	AgentAuthority agentauthority.Level
	Jurisdiction   string
	Verification   valuedomain.VerificationLevel
	CorrelationID  string
}

func (r IntentRequest) validate() error {
	var problems []string
	if r.AccountID.IsZero() || r.InstrumentID.IsZero() {
		problems = append(problems, "account and instrument required")
	}
	if !r.Mode.Simulated() {
		problems = append(problems, "mode must be PAPER or SANDBOX: derivatives move only simulated value")
	}
	if k := strings.TrimSpace(r.IdempotencyKey); k == "" || k != r.IdempotencyKey {
		problems = append(problems, "idempotency key required")
	}
	if !r.ActorType.Valid() || strings.TrimSpace(r.ActorID) == "" {
		problems = append(problems, "actor required")
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "derivatives: invalid request").WithField("problems", problems)
	}
	return nil
}

// OpenRequest opens or adds to a position.
type OpenRequest struct {
	IntentRequest
	Side Side
	// Quantity is the number of contracts, positive and a whole number of
	// lots.
	Quantity money.Quantity
}

// CloseRequest reduces or closes a position.
type CloseRequest struct {
	IntentRequest
	// Quantity is the number of contracts to close; nil closes the position.
	Quantity *money.Quantity
}

// ExerciseRequest exercises held American options.
type ExerciseRequest struct {
	IntentRequest
	// Quantity is the number of contracts to exercise; nil exercises all.
	Quantity *money.Quantity
}

// begin records the intent (idempotently), routes it through the settlement
// compiler and authorizes the capability in the requested mode. It returns
// the intent in ROUTED to proceed with, or an outcome to return as is: a
// replay of an earlier attempt or a refusal.
func (s *Service) begin(ctx context.Context, tx pgx.Tx, r IntentRequest, kind econ.IntentKind, spec Spec, qty *money.Quantity, terms map[string]string) (econ.Intent, *Outcome, error) {
	if err := s.serializeClearing(ctx, tx); err != nil {
		return econ.Intent{}, nil, err
	}
	body, err := json.Marshal(terms)
	if err != nil {
		return econ.Intent{}, nil, errs.Wrap(err, errs.CodeInternal, "derivatives: encode intent terms")
	}
	asset := spec.SettlementAssetID
	in, existing, err := s.intents.Submit(ctx, tx, econ.Intent{
		AccountID: r.AccountID, Kind: kind, Mode: r.Mode, Capability: gates.Derivatives,
		ActorType: r.ActorType, ActorID: r.ActorID, AgentID: r.AgentID, InstrumentID: r.InstrumentID.String(),
		AssetID: &asset, Quantity: qty, Terms: body, IdempotencyKey: r.IdempotencyKey, CorrelationID: r.CorrelationID,
	})
	if err != nil {
		return econ.Intent{}, nil, err
	}
	if existing {
		out, err := s.replayOutcome(ctx, tx, in)
		return econ.Intent{}, &out, err
	}
	route, err := econ.Route(econ.RouteRequest{
		Intent:         in,
		Subject:        settlement.Subject{Type: settlement.SubjectInstrument, ID: r.InstrumentID.String(), AssetID: asset.String()},
		AgentAuthority: r.AgentAuthority, Jurisdiction: r.Jurisdiction, Verification: r.Verification,
	}, s.router, s.clk.Now().UTC())
	if err != nil {
		return econ.Intent{}, nil, err
	}
	if !route.MayExecuteNow() {
		out, err := s.refuse(ctx, tx, in, errs.CodeForbidden, "the settlement compiler did not route this intent", route.Reasons, nil)
		return econ.Intent{}, &out, err
	}
	if in, err = s.advance(ctx, tx, in, econ.StatusRouted, "routed to "+route.Product); err != nil {
		return econ.Intent{}, nil, err
	}
	if err := s.authorizer.Require(ctx, tx, gates.Derivatives, r.Mode); err != nil {
		ee, ok := errs.As(err)
		if !ok {
			return econ.Intent{}, nil, err
		}
		reasons, _ := ee.Fields["reasons"].([]string)
		out, rerr := s.refuse(ctx, tx, in, ee.Code, "the derivatives capability may not be exercised in this mode here", reasons, nil)
		return econ.Intent{}, &out, rerr
	}
	return in, nil, nil
}

// OpenPosition opens a position or adds to one with a paper fill: the mark
// moved against the customer by the policy's disclosed slippage, on the tick
// grid. It is the economic operation of ADVANCED_FINANCIAL_MODEL §6 end to
// end: intent, route, mode authorization, Risk V2 (instrument, oracle, margin
// after the trade; a stale or divergent mark refuses it), reservation of the
// initial margin and premium, the fill, its postings, evidence and the
// effects -- exactly once per idempotency key. A refusal is a *Refusal (see
// its documentation: the transaction holds the record of it).
func (s *Service) OpenPosition(ctx context.Context, tx pgx.Tx, r OpenRequest) (Outcome, error) {
	if err := r.IntentRequest.validate(); err != nil {
		return Outcome{}, err
	}
	if !r.Side.Valid() {
		return Outcome{}, errs.New(errs.CodeValidationFailed, "derivatives: side must be LONG or SHORT")
	}
	spec, err := loadSpec(ctx, tx, s.instruments, r.InstrumentID)
	if err != nil {
		return Outcome{}, err
	}
	if err := spec.CheckQuantity(r.Quantity); err != nil {
		return Outcome{}, err
	}
	qty := r.Quantity
	in, early, err := s.begin(ctx, tx, r.IntentRequest, econ.KindOpenPosition, spec, &qty, map[string]string{"side": string(r.Side)})
	if early != nil || err != nil {
		return derefOutcome(early), err
	}
	ch, err := s.ClearingHouse(ctx, tx)
	if err != nil {
		return Outcome{}, err
	}
	now := s.clk.Now().UTC()
	side := r.Side.fillSide()
	d := qty
	if side == Sell {
		d = d.Neg()
	}
	pos, found, err := lockOpenPosition(ctx, tx, r.AccountID, r.InstrumentID)
	if err != nil {
		return Outcome{}, err
	}
	if found && pos.Quantity.Sign() != d.Sign() {
		return s.refuse(ctx, tx, in, errs.CodeConflict, "an open position on the other side must be closed before opening this one", nil, nil)
	}
	mark, hasMark, err := LatestMark(ctx, tx, r.InstrumentID)
	if err != nil {
		return Outcome{}, err
	}
	tr := tradeRisk{spec: spec, mark: mark, hasMark: hasMark, riskIncreasing: true}
	var price, im, premium money.Quantity
	if hasMark {
		if price, err = paperPrice(spec, mark.MarkMantissa, side, s.policy.PaperSlippageBPS); err != nil {
			return Outcome{}, err
		}
		im, err = initialMarginForFill(s.policy, spec, side, qty, markInputOf(mark), now)
		switch {
		case err == nil:
			tr.margin, err = s.marginSnapshot(ctx, tx, r.AccountID, spec, d, price, mark, now)
			switch {
			case err == nil:
			case errs.CodeOf(err) == errs.CodeStaleMarketData:
				// The book cannot be valued (a stale mark or price of
				// another position): the kernel refuses the new risk as an
				// unavailable oracle, and the refusal is recorded.
				tr.unvaluable = true
			default:
				return Outcome{}, err
			}
			req, err := RequirementFor(s.policy, spec, d, markInputOf(mark), now)
			if err != nil {
				return Outcome{}, err
			}
			tr.notional = req.Notional
		case errs.CodeOf(err) == errs.CodeStaleMarketData:
			// No index to margin against: the oracle section reports the
			// mark unusable and the kernel refuses.
			im = money.Quantity{}
		default:
			return Outcome{}, err
		}
		if spec.IsOption() && side == Buy {
			if premium, err = spec.Value(qty, price); err != nil {
				return Outcome{}, err
			}
		}
	}
	decision, did, err := s.evaluate(ctx, tx, ch, in, r.IntentRequest, tr, now)
	if err != nil {
		return Outcome{}, err
	}
	if decision.Verdict != risk.Allow {
		out, rerr := s.refuse(ctx, tx, in, errs.CodeRiskRejected, "the risk kernel refused this order", decision.ReasonCodes, &did)
		out.Decision = &decision
		return out, rerr
	}
	if in, err = s.advance(ctx, tx, in, econ.StatusRiskApproved, "risk approved: "+decision.Hash); err != nil {
		return Outcome{}, err
	}
	reserve := im.Add(premium)
	var reservation *capital.Reservation
	if reserve.IsPositive() {
		res, err := s.capital.Reserve(ctx, tx, capital.ReserveRequest{
			AccountID: r.AccountID.String(), AssetID: spec.SettlementAssetID, Quantity: reserve,
			EconomicIntentID: in.ID.String(),
			ActorType:        s.actor.Type, ActorID: s.actor.ID, IdempotencyKey: "derivatives:reserve:" + in.ID.String(),
			TTL: s.reservationTTL, Reason: "derivative initial margin and premium",
		})
		if errs.CodeOf(err) == errs.CodeInsufficientBuyingPower {
			out, rerr := s.refuse(ctx, tx, in, errs.CodeInsufficientBuyingPower, "the margin and premium of this order exceed what is available",
				[]string{"INSUFFICIENT_BUYING_POWER"}, &did)
			out.Decision = &decision
			return out, rerr
		}
		if err != nil {
			return Outcome{}, err
		}
		reservation = &res
		if in, err = s.advance(ctx, tx, in, econ.StatusReserved, "margin and premium reserved"); err != nil {
			return Outcome{}, err
		}
	}
	if in, err = s.advance(ctx, tx, in, econ.StatusExecuting, "paper fill"); err != nil {
		return Outcome{}, err
	}
	iid := in.ID
	fr, err := s.ApplyFill(ctx, tx, Fill{
		Key: "intent:" + in.ID.String(), AccountID: r.AccountID, InstrumentID: r.InstrumentID, Side: side, Quantity: qty,
		Price: price, Source: SourcePaper, MarkID: mark.ID, SlippageBPS: s.policy.PaperSlippageBPS, Direction: FillOpens,
		IntentID: &iid, DecisionID: did, OccurredAt: now, CorrelationID: r.CorrelationID,
	})
	if err != nil {
		return Outcome{}, err
	}
	if reservation != nil {
		used := fr.InitialMargin.Add(fr.PremiumPaid)
		if !used.Equal(reserve) {
			return Outcome{}, errs.New(errs.CodeInternal, "derivatives: the fill posted a different margin and premium than was reserved").
				WithField("reserved", reserve.String()).WithField("posted", used.String())
		}
		if _, err := s.capital.Consume(ctx, tx, reservation.ID, used, 0, ""); err != nil {
			return Outcome{}, err
		}
	}
	if in, err = s.advance(ctx, tx, in, econ.StatusCompleted, "position opened"); err != nil {
		return Outcome{}, err
	}
	return Outcome{Intent: in, DecisionID: &did, Decision: &decision, Fill: &fr.Fill, Position: &fr.Position, Journals: fr.Journals}, nil
}

// ClosePosition reduces or closes a position with a paper fill. Closing is
// never risk-increasing: a stale mark does not prevent it (it is priced at
// the last mark there is), and no margin is reserved; the released margin
// returns to the wallet and any loss is collected as an obligation.
func (s *Service) ClosePosition(ctx context.Context, tx pgx.Tx, r CloseRequest) (Outcome, error) {
	if err := r.IntentRequest.validate(); err != nil {
		return Outcome{}, err
	}
	spec, err := loadSpec(ctx, tx, s.instruments, r.InstrumentID)
	if err != nil {
		return Outcome{}, err
	}
	if r.Quantity != nil {
		if err := spec.CheckQuantity(*r.Quantity); err != nil {
			return Outcome{}, err
		}
	}
	in, early, err := s.begin(ctx, tx, r.IntentRequest, econ.KindClosePosition, spec, r.Quantity, map[string]string{})
	if early != nil || err != nil {
		return derefOutcome(early), err
	}
	ch, err := s.ClearingHouse(ctx, tx)
	if err != nil {
		return Outcome{}, err
	}
	now := s.clk.Now().UTC()
	pos, found, err := lockOpenPosition(ctx, tx, r.AccountID, r.InstrumentID)
	if err != nil {
		return Outcome{}, err
	}
	if !found {
		return s.refuse(ctx, tx, in, errs.CodeNotFound, "there is no open position to close", nil, nil)
	}
	if spec.ExpiredAt(now) {
		return s.refuse(ctx, tx, in, errs.CodeInvalidStateTransition, "the contract has expired and settles; it can no longer be traded", nil, nil)
	}
	n := pos.Quantity.Abs()
	if r.Quantity != nil {
		if r.Quantity.Cmp(n) > 0 {
			return s.refuse(ctx, tx, in, errs.CodeValidationFailed, "cannot close more contracts than the position holds", nil, nil)
		}
		n = *r.Quantity
	}
	mark, hasMark, err := LatestMark(ctx, tx, r.InstrumentID)
	if err != nil {
		return Outcome{}, err
	}
	if !hasMark {
		return s.refuse(ctx, tx, in, errs.CodeStaleMarketData, "the contract has never been marked; a paper close needs a price", nil, nil)
	}
	side := Sell
	d := n.Neg()
	if pos.Quantity.IsNegative() {
		side, d = Buy, n
	}
	price, err := paperPrice(spec, mark.MarkMantissa, side, s.policy.PaperSlippageBPS)
	if err != nil {
		return Outcome{}, err
	}
	tr := tradeRisk{spec: spec, mark: mark, hasMark: true}
	if req, err := RequirementFor(s.policy, spec, d, markInputOf(mark), now); err == nil {
		tr.notional = req.Notional
	}
	// Closing is never blocked by the book's valuation: the margin section is
	// supplied when the book can be valued and is not required otherwise.
	if snap, err := s.marginSnapshot(ctx, tx, r.AccountID, spec, d, price, mark, now); err == nil {
		tr.margin = snap
	}
	decision, did, err := s.evaluate(ctx, tx, ch, in, r.IntentRequest, tr, now)
	if err != nil {
		return Outcome{}, err
	}
	if decision.Verdict != risk.Allow {
		out, rerr := s.refuse(ctx, tx, in, errs.CodeRiskRejected, "the risk kernel refused this close", decision.ReasonCodes, &did)
		out.Decision = &decision
		return out, rerr
	}
	if in, err = s.advance(ctx, tx, in, econ.StatusRiskApproved, "risk approved: "+decision.Hash); err != nil {
		return Outcome{}, err
	}
	if in, err = s.advance(ctx, tx, in, econ.StatusExecuting, "paper fill"); err != nil {
		return Outcome{}, err
	}
	iid := in.ID
	fr, err := s.ApplyFill(ctx, tx, Fill{
		Key: "intent:" + in.ID.String(), AccountID: r.AccountID, InstrumentID: r.InstrumentID, Side: side, Quantity: n,
		Price: price, Source: SourcePaper, MarkID: mark.ID, SlippageBPS: s.policy.PaperSlippageBPS, Direction: FillReduces,
		IntentID: &iid, DecisionID: did, OccurredAt: now, CorrelationID: r.CorrelationID,
	})
	if err != nil {
		return Outcome{}, err
	}
	if in, err = s.advance(ctx, tx, in, econ.StatusCompleted, "position reduced"); err != nil {
		return Outcome{}, err
	}
	return Outcome{Intent: in, DecisionID: &did, Decision: &decision, Fill: &fr.Fill, Position: &fr.Position, Journals: fr.Journals}, nil
}

// ExerciseOption exercises held American options before or at expiry. The
// exercise settles in cash at the underlying's fresh index (aligned to the
// tick grid, half-even): the holder receives the intrinsic value from the
// clearing house, and the exercised contracts are assigned to writers FIFO by
// position open time, then id -- each assigned writer pays the same intrinsic
// value per contract; contracts no writer is left to take are assigned to the
// clearing house. A European option is exercised automatically at expiry
// (SettleExpiry), never by its holder.
func (s *Service) ExerciseOption(ctx context.Context, tx pgx.Tx, r ExerciseRequest) (Outcome, error) {
	if err := r.IntentRequest.validate(); err != nil {
		return Outcome{}, err
	}
	spec, err := loadSpec(ctx, tx, s.instruments, r.InstrumentID)
	if err != nil {
		return Outcome{}, err
	}
	if !spec.IsOption() {
		return Outcome{}, errs.New(errs.CodeValidationFailed, "derivatives: only an option is exercised")
	}
	if r.Quantity != nil {
		if err := spec.CheckQuantity(*r.Quantity); err != nil {
			return Outcome{}, err
		}
	}
	in, early, err := s.begin(ctx, tx, r.IntentRequest, econ.KindExerciseOption, spec, r.Quantity, map[string]string{})
	if early != nil || err != nil {
		return derefOutcome(early), err
	}
	ch, err := s.ClearingHouse(ctx, tx)
	if err != nil {
		return Outcome{}, err
	}
	now := s.clk.Now().UTC()
	if spec.Style != instruments.American {
		return s.refuse(ctx, tx, in, errs.CodeUnsupported, "a European option is exercised automatically at expiry, not by its holder", nil, nil)
	}
	if spec.Expiry != nil && now.After(*spec.Expiry) {
		return s.refuse(ctx, tx, in, errs.CodeInvalidStateTransition, "the option has expired; it settles", nil, nil)
	}
	// Every position of the instrument is locked in the one global order
	// (opened_at, id) shared with the runs, so an exercise and a settlement
	// never wait on each other in opposite orders.
	all, err := lockPositionsForInstrument(ctx, tx, r.InstrumentID)
	if err != nil {
		return Outcome{}, err
	}
	var pos Position
	found := false
	var writers []Position
	for _, p := range all {
		if p.AccountID == r.AccountID {
			pos, found = p, true
		}
		if p.Quantity.IsNegative() {
			writers = append(writers, p)
		}
	}
	if !found || !pos.Quantity.IsPositive() {
		return s.refuse(ctx, tx, in, errs.CodeValidationFailed, "only the holder of a long position exercises", nil, nil)
	}
	n := pos.Quantity
	if r.Quantity != nil {
		if r.Quantity.Cmp(n) > 0 {
			return s.refuse(ctx, tx, in, errs.CodeValidationFailed, "cannot exercise more contracts than are held", nil, nil)
		}
		n = *r.Quantity
	}
	mark, hasMark, err := LatestMark(ctx, tx, r.InstrumentID)
	if err != nil {
		return Outcome{}, err
	}
	if !hasMark || !markUsable(spec, mark, now) {
		return s.refuse(ctx, tx, in, errs.CodeStaleMarketData, "an exercise settles at a fresh index and there is none", nil, nil)
	}
	underlying, err := alignIndex(spec, mark.IndexMantissa, mark.IndexScale, money.RoundHalfEven)
	if err != nil {
		return Outcome{}, err
	}
	per, err := IntrinsicPerContract(spec, underlying)
	if err != nil {
		return Outcome{}, err
	}
	if !per.IsPositive() {
		return s.refuse(ctx, tx, in, errs.CodeValidationFailed, "the option is out of the money; exercising it would pay nothing", nil, nil)
	}
	tr := tradeRisk{spec: spec, mark: mark, hasMark: true}
	if req, err := RequirementFor(s.policy, spec, n, markInputOf(mark), now); err == nil {
		tr.notional = req.Notional
	}
	ip, err := IntrinsicPrice(spec, underlying)
	if err != nil {
		return Outcome{}, err
	}
	if snap, err := s.marginSnapshot(ctx, tx, r.AccountID, spec, n.Neg(), ip, mark, now); err == nil {
		tr.margin = snap
	}
	decision, did, err := s.evaluate(ctx, tx, ch, in, r.IntentRequest, tr, now)
	if err != nil {
		return Outcome{}, err
	}
	if decision.Verdict != risk.Allow {
		out, rerr := s.refuse(ctx, tx, in, errs.CodeRiskRejected, "the risk kernel refused this exercise", decision.ReasonCodes, &did)
		out.Decision = &decision
		return out, rerr
	}
	if in, err = s.advance(ctx, tx, in, econ.StatusRiskApproved, "risk approved: "+decision.Hash); err != nil {
		return Outcome{}, err
	}
	if in, err = s.advance(ctx, tx, in, econ.StatusExecuting, "exercise"); err != nil {
		return Outcome{}, err
	}
	iid := in.ID
	res, err := s.exercise(ctx, tx, ch, spec, exercisePlan{
		holders: []holderLot{{pos: pos, qty: n}}, writers: writers, underlying: underlying, kind: ExerciseHolder,
		intent: &iid, holderSource: SourceExercise, writerSource: SourceAssignment, closeTo: PositionClosed,
		keyPrefix: "exercise:" + in.ID.String(), evidenceKey: "derivatives/exercise/" + in.ID.String(),
		now: now, correlationID: r.CorrelationID,
	})
	if err != nil {
		return Outcome{}, err
	}
	if in, err = s.advance(ctx, tx, in, econ.StatusCompleted, "exercised"); err != nil {
		return Outcome{}, err
	}
	out := Outcome{Intent: in, DecisionID: &did, Decision: &decision, Journals: res.journals}
	if len(res.holderFills) > 0 {
		f, p := res.holderFills[0], res.holderPositions[0]
		out.Fill, out.Position = &f, &p
	}
	if len(res.exercises) > 0 {
		e := res.exercises[0]
		out.ExerciseID = &e
	}
	return out, nil
}

// alignIndex expresses an index (mantissa at scale) as a price on the
// instrument's tick grid, rounding once in mode.
func alignIndex(s Spec, mantissa money.Quantity, scale int32, mode money.RoundingMode) (money.Quantity, error) {
	if !mantissa.IsPositive() || scale < 0 || scale > money.MaxPriceScale {
		return money.Quantity{}, errs.New(errs.CodeValidationFailed, "derivatives: an index must be positive")
	}
	// ticks = mantissa × 10^priceScale / (10^scale × tick)
	num := commonScale(mantissa, 0, s.PriceScale)
	den := commonScale(s.Tick, 0, scale)
	ticks, err := num.Div(den, mode)
	if err != nil {
		return money.Quantity{}, errs.Wrap(err, errs.CodeInternal, "derivatives: align an index")
	}
	return ticks.Mul(s.Tick), nil
}

func derefOutcome(o *Outcome) Outcome {
	if o == nil {
		return Outcome{}
	}
	return *o
}
