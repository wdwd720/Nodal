package derivatives

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// holderLot is one exercise: contracts of a holder's long position, or (house)
// the clearing house exercising the net long it carries for a one-sided book.
type holderLot struct {
	pos   Position
	house bool
	qty   money.Quantity
}

// exercisePlan is everything one exercise-and-assignment run needs.
type exercisePlan struct {
	holders []holderLot
	// writers are the OPEN short positions in FIFO order (opened_at, id),
	// row-locked.
	writers []Position
	// underlying is the underlying's settlement price, on the tick grid.
	underlying    money.Quantity
	kind          ExerciseKind
	intent        *econ.IntentID
	settlement    *SettlementID
	holderSource  FillSource
	writerSource  FillSource
	closeTo       PositionStatus
	keyPrefix     string
	evidenceKey   string
	now           time.Time
	correlationID string
}

// exerciseResult is what an exercise run did.
type exerciseResult struct {
	exercises       []ExerciseID
	holderFills     []FillRecord
	holderPositions []Position
	journals        []ledger.TransactionID
	// receipts is what customer holders received; payments what was
	// collected from assigned customer writers, shortfall what they could
	// not pay (absorbed by the clearing house).
	receipts  money.Quantity
	payments  money.Quantity
	shortfall money.Quantity
}

// exerciseEvidence is the archived document of an exercise run.
type exerciseEvidence struct {
	InstrumentID         string       `json:"instrument_id"`
	Kind                 ExerciseKind `json:"kind"`
	Underlying           string       `json:"underlying_settlement_mantissa"`
	IntrinsicPrice       string       `json:"intrinsic_price_mantissa"`
	IntrinsicPerContract string       `json:"intrinsic_per_contract"`
	Exercises            []string     `json:"exercise_ids"`
	Allocations          []Allocation `json:"allocations"`
	PolicyVersion        string       `json:"policy_version"`
	Legs                 []postedLeg  `json:"legs"`
}

// exercise settles exercised contracts in cash and assigns them to writers
// FIFO. Each holder closes its exercised contracts at the intrinsic price
// (receiving exactly qty × intrinsic per contract) and each assigned writer
// closes its assigned contracts at the same price (paying exactly the same per
// contract), so customer receipts equal customer payments whenever no
// contracts fall to the clearing house. Writers are posted before holders so
// the clearing house collects before it pays.
func (s *Service) exercise(ctx context.Context, tx pgx.Tx, ch econ.Facility, spec Spec, p exercisePlan) (exerciseResult, error) {
	ip, err := IntrinsicPrice(spec, p.underlying)
	if err != nil {
		return exerciseResult{}, err
	}
	per, err := spec.Value(money.QuantityFromInt64(1), ip)
	if err != nil {
		return exerciseResult{}, err
	}
	if !per.IsPositive() {
		return exerciseResult{}, errs.New(errs.CodeValidationFailed, "derivatives: an out-of-the-money option is not exercised")
	}
	exercised := make([]money.Quantity, len(p.holders))
	for i, h := range p.holders {
		exercised[i] = h.qty
	}
	writerQty := make([]money.Quantity, len(p.writers))
	for i, w := range p.writers {
		writerQty[i] = w.Quantity.Abs()
	}
	allocs, err := AssignFIFO(exercised, writerQty)
	if err != nil {
		return exerciseResult{}, err
	}
	res := exerciseResult{}
	for _, h := range p.holders {
		eid := newExerciseID()
		var pos, acct any
		if !h.house {
			pos, acct = h.pos.ID, h.pos.AccountID
		}
		if _, err := tx.Exec(ctx, `INSERT INTO option_exercises
			(id, instrument_id, position_id, account_id, kind, quantity, settlement_mantissa, intrinsic_per_contract,
			 intent_id, settlement_id, occurred_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			eid, spec.InstrumentID, pos, acct, p.kind, h.qty, p.underlying, per, p.intent, p.settlement, p.now); err != nil {
			return exerciseResult{}, dbErr("record exercise", err)
		}
		res.exercises = append(res.exercises, eid)
	}
	assigned := make([]money.Quantity, len(p.writers))
	seq := make([]int, len(p.holders))
	for _, a := range allocs {
		amount := a.Quantity.Mul(per)
		var short, acct any
		if a.Writer != HouseWriter {
			w := p.writers[a.Writer]
			short, acct = w.ID, w.AccountID
			assigned[a.Writer] = assigned[a.Writer].Add(a.Quantity)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO option_assignments
			(id, exercise_id, seq, short_position_id, account_id, quantity, amount) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			newAssignmentID(), res.exercises[a.Exercise], seq[a.Exercise], short, acct, a.Quantity, amount); err != nil {
			return exerciseResult{}, dbErr("record assignment", err)
		}
		seq[a.Exercise]++
	}
	ref := ledger.FinancialEventReference{Type: "option_exercise", ID: p.keyPrefix}
	var legs []leg
	// Writers first: the clearing house collects before it pays.
	for wi, w := range p.writers {
		if !assigned[wi].IsPositive() {
			continue
		}
		red, _, err := s.reducePosition(ctx, tx, ch, spec, w, assigned[wi], ip, p, p.writerSource, FlowOptionAssignment, "assign:"+w.ID.String(), ref)
		if err != nil {
			return exerciseResult{}, err
		}
		res.payments = res.payments.Add(red.pay.collected())
		res.shortfall = res.shortfall.Add(red.pay.Shortfall)
		legs = append(legs, red.legs...)
	}
	for _, h := range p.holders {
		if h.house {
			continue
		}
		red, fill, err := s.reducePosition(ctx, tx, ch, spec, h.pos, h.qty.Neg(), ip, p, p.holderSource, FlowOptionExercise, "exercise:"+h.pos.ID.String(), ref)
		if err != nil {
			return exerciseResult{}, err
		}
		res.receipts = res.receipts.Add(red.cash)
		res.holderFills = append(res.holderFills, fill)
		res.holderPositions = append(res.holderPositions, red.after)
		legs = append(legs, red.legs...)
	}
	ids := make([]string, 0, len(res.exercises))
	for _, e := range res.exercises {
		ids = append(ids, e.String())
	}
	posted, err := s.executeLegs(ctx, tx, ch, legs, p.evidenceKey, func(pl []postedLeg) any {
		return exerciseEvidence{
			InstrumentID: spec.InstrumentID.String(), Kind: p.kind, Underlying: p.underlying.String(), IntrinsicPrice: ip.String(),
			IntrinsicPerContract: per.String(), Exercises: ids, Allocations: allocs, PolicyVersion: s.policy.Version, Legs: pl,
		}
	}, p.correlationID)
	if err != nil {
		return exerciseResult{}, err
	}
	res.journals = journals(posted)
	return res, nil
}

// reducePosition closes d (signed, against the position) contracts of pos at
// price as a non-trade fill (exercise, assignment or expiry): it plans the
// legs, persists the position (closing it when flat) and records the fill.
// The legs are returned for the caller to execute with the rest of its run.
func (s *Service) reducePosition(ctx context.Context, tx pgx.Tx, ch econ.Facility, spec Spec, pos Position, d, price money.Quantity,
	p exercisePlan, source FillSource, kind CashFlowKind, legKey string, ref ledger.FinancialEventReference) (reduction, FillRecord, error) {
	eff, err := applyFill(spec, pos.state(), d, price)
	if err != nil {
		return reduction{}, FillRecord{}, err
	}
	if eff.Increase {
		return reduction{}, FillRecord{}, errs.New(errs.CodeInternal, "derivatives: an exercise or settlement can only reduce a position")
	}
	journal, effect := ledger.KindOptionSettlement, EffectOptionSettlement
	if spec.IsLinear() {
		journal, effect = ledger.KindFutureSettlement, EffectFutureSettlement
	}
	red, err := s.planReduction(ctx, tx, ch, spec, pos, eff, reductionKeys{
		prefix: "deriv:" + p.keyPrefix + ":" + legKey, reference: ref, intent: p.intent,
		cashKind: kind, cashJournal: journal, cashEffect: effect,
	})
	if err != nil {
		return reduction{}, FillRecord{}, err
	}
	saved, _, err := persistPosition(ctx, tx, red.after, false, p.closeTo, s.actor, string(source)+" "+p.keyPrefix, p.now)
	if err != nil {
		return reduction{}, FillRecord{}, err
	}
	red.after = saved
	side := Sell
	if d.IsPositive() {
		side = Buy
	}
	fill := FillRecord{
		ID: newFillID(), Key: p.keyPrefix + ":" + legKey, PositionID: pos.ID, AccountID: pos.AccountID,
		InstrumentID: pos.InstrumentID, Source: source, Side: side, Quantity: d, PriceMantissa: price, PriceScale: spec.PriceScale,
		Value: eff.Value, RealizedPnL: eff.RealizedPnL, CashSettled: eff.Cash, OccurredAt: p.now,
	}
	if p.intent != nil {
		fill.IntentID = p.intent.String()
	}
	if err := insertFill(ctx, tx, fill); err != nil {
		return reduction{}, FillRecord{}, err
	}
	return red, fill, nil
}
