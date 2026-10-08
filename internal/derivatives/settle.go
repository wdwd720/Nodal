package derivatives

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// SettleRequest settles one expired instrument.
type SettleRequest struct {
	InstrumentID instruments.InstrumentID
	// Price optionally fixes the settlement price, a mantissa at the
	// instrument's price scale on its tick grid: a future's final settlement
	// price, or the underlying's settlement price for an option. Without it
	// the price is derived from the last mark observed at or before expiry
	// (fresh within the instrument's oracle max age): a future settles at that
	// mark, an option at that mark's index aligned to the tick grid
	// half-even.
	Price         *money.Quantity
	PriceSource   string
	CorrelationID string
}

// SettlementResult is what a final settlement did.
type SettlementResult struct {
	ID     SettlementID
	Replay bool
	Kind   SettlementKind
	// Price is the settlement price (an option's: the underlying's).
	Price     money.Quantity
	Positions int
	// Paid is Σ collected from customers, Received Σ paid to them, Shortfall
	// Σ what payers could not pay.
	Paid      money.Quantity
	Received  money.Quantity
	Shortfall money.Quantity
	// Options only.
	IntrinsicPerContract money.Quantity
	Exercises            []ExerciseID
	Journals             []ledger.TransactionID
}

// SettleExpiry settles an expired dated future or option once and for all.
//
// A future: every open position closes at the settlement price; it receives
// or pays Q × V(price) − settled_value through the clearing house
// (FUTURE_SETTLEMENT), its margin is released, and it becomes SETTLED.
//
// An option is cash settled: if it expires in the money every customer long
// is exercised automatically and receives its intrinsic value, and the
// exercised contracts are assigned FIFO (open time, then id) to the writers,
// each paying the same intrinsic value per contract (OPTION_SETTLEMENT); the
// clearing house is holder or writer of record for whatever a one-sided book
// leaves over. Premiums are recognized into realized PnL, writers' margin is
// released, and every position becomes SETTLED. Out of the money, nothing is
// paid and premiums are recognized.
//
// derivative_settlements is UNIQUE per instrument: a second call -- a replay
// or a concurrent run -- posts nothing and returns the first settlement.
func (s *Service) SettleExpiry(ctx context.Context, tx pgx.Tx, r SettleRequest) (SettlementResult, error) {
	if err := s.serializeClearing(ctx, tx); err != nil {
		return SettlementResult{}, err
	}
	spec, err := loadSpec(ctx, tx, s.instruments, r.InstrumentID)
	if err != nil {
		return SettlementResult{}, err
	}
	kind := SettleFuture
	switch spec.Kind {
	case instruments.TypeFuture:
	case instruments.TypeOption:
		kind = SettleOption
	default:
		return SettlementResult{}, errs.New(errs.CodeValidationFailed, "derivatives: only a dated future or an option expires")
	}
	now := s.clk.Now().UTC()
	if !spec.ExpiredAt(now) {
		return SettlementResult{}, errs.New(errs.CodeValidationFailed, "derivatives: the contract has not expired").
			WithField("expiry", *spec.Expiry)
	}
	if existing, ok, err := settlementFor(ctx, tx, r.InstrumentID); err != nil {
		return SettlementResult{}, err
	} else if ok {
		return existing, nil
	}
	price, source, err := s.settlementPrice(ctx, tx, spec, r)
	if err != nil {
		return SettlementResult{}, err
	}
	positions, err := lockPositionsForInstrument(ctx, tx, r.InstrumentID)
	if err != nil {
		return SettlementResult{}, err
	}
	sid := newSettlementID()
	var claimed SettlementID
	err = tx.QueryRow(ctx, `INSERT INTO derivative_settlements (id, instrument_id, kind, settlement_mantissa, price_scale, price_source, positions, settled_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (instrument_id) DO NOTHING RETURNING id`,
		sid, r.InstrumentID, kind, price, spec.PriceScale, source, len(positions), now).Scan(&claimed)
	if errors.Is(err, pgx.ErrNoRows) {
		existing, _, err := settlementFor(ctx, tx, r.InstrumentID)
		return existing, err
	}
	if err != nil {
		return SettlementResult{}, dbErr("claim settlement", err)
	}
	ch, err := s.ClearingHouse(ctx, tx)
	if err != nil {
		return SettlementResult{}, err
	}
	res := SettlementResult{ID: sid, Kind: kind, Price: price, Positions: len(positions)}
	plan := exercisePlan{
		underlying: price, kind: ExerciseAutomatic, settlement: &sid, holderSource: SourceExpiry, writerSource: SourceExpiry,
		closeTo: PositionSettled, keyPrefix: "settlement:" + sid.String(), evidenceKey: "derivatives/settlement/" + sid.String(),
		now: now, correlationID: r.CorrelationID,
	}
	if kind == SettleFuture {
		return s.settleFutures(ctx, tx, ch, spec, positions, res, plan)
	}
	return s.settleOptions(ctx, tx, ch, spec, positions, res, plan)
}

func (s *Service) settleFutures(ctx context.Context, tx pgx.Tx, ch econ.Facility, spec Spec, positions []Position, res SettlementResult, plan exercisePlan) (SettlementResult, error) {
	ref := ledger.FinancialEventReference{Type: "derivative_settlement", ID: res.ID.String()}
	var payers, receivers []leg
	for _, p := range positions {
		red, _, err := s.reducePosition(ctx, tx, ch, spec, p, p.Quantity.Neg(), res.Price, plan, SourceExpiry, FlowFutureSettlement,
			"position:"+p.ID.String(), ref)
		if err != nil {
			return SettlementResult{}, err
		}
		if red.payer() {
			res.Paid, res.Shortfall = res.Paid.Add(red.pay.collected()), res.Shortfall.Add(red.pay.Shortfall)
			payers = append(payers, red.legs...)
		} else {
			res.Received = res.Received.Add(red.cash)
			receivers = append(receivers, red.legs...)
		}
	}
	posted, err := s.executeLegs(ctx, tx, ch, append(payers, receivers...), plan.evidenceKey, func(pl []postedLeg) any {
		return settlementEvidence{SettlementID: res.ID.String(), InstrumentID: spec.InstrumentID.String(), Kind: res.Kind,
			Price: res.Price.String(), PriceScale: spec.PriceScale, Positions: res.Positions, PolicyVersion: s.policy.Version, Legs: pl}
	}, plan.correlationID)
	if err != nil {
		return SettlementResult{}, err
	}
	res.Journals = journals(posted)
	return res, nil
}

func (s *Service) settleOptions(ctx context.Context, tx pgx.Tx, ch econ.Facility, spec Spec, positions []Position, res SettlementResult, plan exercisePlan) (SettlementResult, error) {
	per, err := IntrinsicPerContract(spec, res.Price)
	if err != nil {
		return SettlementResult{}, err
	}
	res.IntrinsicPerContract = per
	if per.IsPositive() {
		// In the money: every long is exercised, the clearing house exercises
		// whatever net long it carries, and every exercised contract is
		// assigned FIFO.
		longs, shorts := money.Quantity{}, money.Quantity{}
		for _, p := range positions {
			if p.Quantity.IsPositive() {
				plan.holders = append(plan.holders, holderLot{pos: p, qty: p.Quantity})
				longs = longs.Add(p.Quantity)
			} else {
				plan.writers = append(plan.writers, p)
				shorts = shorts.Add(p.Quantity.Abs())
			}
		}
		if shorts.Cmp(longs) > 0 {
			plan.holders = append(plan.holders, holderLot{house: true, qty: shorts.Sub(longs)})
		}
		ex, err := s.exercise(ctx, tx, ch, spec, plan)
		if err != nil {
			return SettlementResult{}, err
		}
		res.Exercises, res.Journals = ex.exercises, ex.journals
		res.Received, res.Paid, res.Shortfall = ex.receipts, ex.payments, ex.shortfall
		return res, nil
	}
	// Out of the money: every position closes at zero; only premiums are
	// recognized and writers' margin released.
	ref := ledger.FinancialEventReference{Type: "derivative_settlement", ID: res.ID.String()}
	var legs []leg
	for _, p := range positions {
		red, _, err := s.reducePosition(ctx, tx, ch, spec, p, p.Quantity.Neg(), money.Quantity{}, plan, SourceExpiry,
			FlowOptionExercise, "position:"+p.ID.String(), ref)
		if err != nil {
			return SettlementResult{}, err
		}
		legs = append(legs, red.legs...)
	}
	posted, err := s.executeLegs(ctx, tx, ch, legs, plan.evidenceKey, func(pl []postedLeg) any {
		return settlementEvidence{SettlementID: res.ID.String(), InstrumentID: spec.InstrumentID.String(), Kind: res.Kind,
			Price: res.Price.String(), PriceScale: spec.PriceScale, Positions: res.Positions, PolicyVersion: s.policy.Version, Legs: pl}
	}, plan.correlationID)
	if err != nil {
		return SettlementResult{}, err
	}
	res.Journals = journals(posted)
	return res, nil
}

// settlementEvidence is the archived document of a final settlement.
type settlementEvidence struct {
	SettlementID  string         `json:"settlement_id"`
	InstrumentID  string         `json:"instrument_id"`
	Kind          SettlementKind `json:"kind"`
	Price         string         `json:"settlement_mantissa"`
	PriceScale    int32          `json:"price_scale"`
	Positions     int            `json:"positions"`
	PolicyVersion string         `json:"policy_version"`
	Legs          []postedLeg    `json:"legs"`
}

// settlementPrice resolves the price an instrument settles at.
func (s *Service) settlementPrice(ctx context.Context, tx pgx.Tx, spec Spec, r SettleRequest) (money.Quantity, string, error) {
	if r.Price != nil {
		if !spec.OnTick(*r.Price) {
			return money.Quantity{}, "", errs.New(errs.CodeValidationFailed, "derivatives: a settlement price must lie on the tick grid").
				WithField("price_mantissa", r.Price.String())
		}
		src := strings.TrimSpace(r.PriceSource)
		if src == "" {
			return money.Quantity{}, "", errs.New(errs.CodeValidationFailed, "derivatives: a given settlement price names its source")
		}
		return *r.Price, src, nil
	}
	m, ok, err := markAt(ctx, tx, spec.InstrumentID, *spec.Expiry, spec.OracleMaxAge)
	if err != nil {
		return money.Quantity{}, "", err
	}
	if !ok {
		return money.Quantity{}, "", errs.New(errs.CodeStaleMarketData, "derivatives: no fresh mark at expiry; the contract is not settled on a stale price").
			WithField("expiry", *spec.Expiry)
	}
	if spec.IsLinear() {
		return m.MarkMantissa, "mark:" + m.ID.String(), nil
	}
	if !m.HasIndex {
		return money.Quantity{}, "", errs.New(errs.CodeStaleMarketData, "derivatives: the expiry mark carries no index to settle the option at")
	}
	p, err := alignIndex(spec, m.IndexMantissa, m.IndexScale, money.RoundHalfEven)
	if err != nil {
		return money.Quantity{}, "", err
	}
	return p, "index:" + m.ID.String() + ":half_even", nil
}

// settlementFor returns an instrument's settlement, if it has one.
func settlementFor(ctx context.Context, q pgx.Tx, iid instruments.InstrumentID) (SettlementResult, bool, error) {
	var r SettlementResult
	err := q.QueryRow(ctx, `SELECT id, kind, settlement_mantissa, positions FROM derivative_settlements WHERE instrument_id = $1`, iid).
		Scan(&r.ID, &r.Kind, &r.Price, &r.Positions)
	if errors.Is(err, pgx.ErrNoRows) {
		return SettlementResult{}, false, nil
	}
	if err != nil {
		return SettlementResult{}, false, dbErr("read settlement", err)
	}
	r.Replay = true
	return r, true, nil
}
