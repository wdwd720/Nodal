package derivatives

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// SettleRequest settles one expired instrument by the settlement rule
// (SettlementRuleStatement). Nothing in it chooses the price.
type SettleRequest struct {
	InstrumentID instruments.InstrumentID
	// Price, when set, is the settlement price the caller confirms: a
	// mantissa at the instrument's price scale, typically read from
	// SettlementPreview. It never chooses the price -- the rule does -- and
	// if the rule's price is any other the request is refused (CONFLICT) and
	// nothing settles: an operator settles only the price it has seen.
	Price *money.Quantity
	// PriceSource, when set, is the price source the caller confirms (as
	// SettlementPreview states it, e.g. "mark:<id>"); any other refuses the
	// request in the same way.
	PriceSource   string
	CorrelationID string
}

// SettlementResult is what a final settlement did.
type SettlementResult struct {
	ID     SettlementID
	Replay bool
	Kind   SettlementKind
	// Price is the settlement price (an option's: the underlying's).
	Price money.Quantity
	// Rule is what decided the price, PriceSource where it came from
	// ("mark:<id>", "index:<id>:half_even" or "override:<id>"), MarkID the
	// mark the rule (or the override's bound) was derived from, OverrideID the
	// approved override it settled at.
	Rule        SettlementPriceRule
	PriceSource string
	MarkID      *MarkID
	OverrideID  *SettlementOverrideID
	Positions   int
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

// SettleExpiry settles an expired dated future or option once and for all,
// at the price the settlement rule derives from the recorded marks
// (SettlementRuleStatement): the latest mark observed and recorded at or
// before expiry, fresh at expiry. Without such a mark it refuses
// (STALE_MARKET_DATA) and the contract waits for an operator override
// (ProposeSettlementOverride, ApproveSettlementOverride). The caller never
// chooses the price; SettleRequest.Price only confirms it.
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
// or a concurrent run -- posts nothing and returns the first settlement. The
// database refuses a settlement row the rule or an approved override does not
// explain (DV008, DV010).
func (s *Service) SettleExpiry(ctx context.Context, tx pgx.Tx, r SettleRequest) (SettlementResult, error) {
	if err := s.serializeClearing(ctx, tx); err != nil {
		return SettlementResult{}, err
	}
	spec, kind, now, err := s.expiredContract(ctx, tx, r.InstrumentID)
	if err != nil {
		return SettlementResult{}, err
	}
	if existing, ok, err := settlementFor(ctx, tx, r.InstrumentID); err != nil {
		return SettlementResult{}, err
	} else if ok {
		if err := confirmPrice(r.Price, r.PriceSource, existing.Price, existing.PriceSource); err != nil {
			return SettlementResult{}, err
		}
		return existing, nil
	}
	quote, err := expiryQuoteFor(ctx, tx, spec)
	if err != nil {
		return SettlementResult{}, err
	}
	if !quote.hasMark {
		return SettlementResult{}, errs.New(errs.CodeStaleMarketData,
			"derivatives: no mark was observed and recorded by expiry; the contract cannot settle by the rule").
			WithField("expiry", *spec.Expiry)
	}
	if !quote.fresh {
		return SettlementResult{}, errs.New(errs.CodeStaleMarketData,
			"derivatives: the last mark at expiry is stale; the contract is not settled on a stale price by the rule -- it waits for an approved operator override").
			WithField("expiry", *spec.Expiry).WithField("reference_mark_id", quote.mark.ID.String()).
			WithField("reference_observed_at", quote.mark.ObservedAt).WithField("reference_price_mantissa", quote.price.String())
	}
	if err := confirmPrice(r.Price, r.PriceSource, quote.price, quote.source); err != nil {
		return SettlementResult{}, err
	}
	mid := quote.mark.ID
	return s.settle(ctx, tx, spec, kind, now, settlementBasis{
		rule: RuleExpiryMark, price: quote.price, source: quote.source, mark: &quote.mark, markID: &mid,
	}, r.CorrelationID)
}

// expiredContract loads (and share-locks the terms of) a dated future or an
// option that has expired.
func (s *Service) expiredContract(ctx context.Context, tx pgx.Tx, iid instruments.InstrumentID) (Spec, SettlementKind, time.Time, error) {
	spec, err := lockSpec(ctx, tx, s.instruments, iid)
	if err != nil {
		return Spec{}, "", time.Time{}, err
	}
	kind := SettleFuture
	switch spec.Kind {
	case instruments.TypeFuture:
	case instruments.TypeOption:
		kind = SettleOption
	default:
		return Spec{}, "", time.Time{}, errs.New(errs.CodeValidationFailed, "derivatives: only a dated future or an option expires")
	}
	now := s.clk.Now().UTC()
	if !spec.ExpiredAt(now) {
		return Spec{}, "", time.Time{}, errs.New(errs.CodeValidationFailed, "derivatives: the contract has not expired").
			WithField("expiry", *spec.Expiry)
	}
	return spec, kind, now, nil
}

// confirmPrice refuses a settlement whose price or source is not the one the
// caller confirmed. Unset confirmations confirm nothing.
func confirmPrice(price *money.Quantity, source string, actual money.Quantity, actualSource string) error {
	if price != nil && !price.Equal(actual) {
		return errs.New(errs.CodeConflict,
			"derivatives: the settlement price is not the one the caller confirmed; a caller confirms the price, it never chooses it").
			WithField("confirmed_price_mantissa", price.String()).WithField("settlement_price_mantissa", actual.String()).
			WithField("settlement_price_source", actualSource)
	}
	if src := strings.TrimSpace(source); src != "" && src != actualSource {
		return errs.New(errs.CodeConflict, "derivatives: the settlement price source is not the one the caller confirmed").
			WithField("confirmed_price_source", src).WithField("settlement_price_source", actualSource)
	}
	return nil
}

// settlementBasis is what a settlement price rests on.
type settlementBasis struct {
	rule   SettlementPriceRule
	price  money.Quantity
	source string
	// mark is the mark the rule settled at (nil for an override); markID the
	// mark recorded on the settlement: the rule's mark, or the reference mark
	// an override was bounded by.
	mark     *Mark
	markID   *MarkID
	override *SettlementOverride
}

// settlementBasisEvidence is the price basis as archived with a settlement:
// the rule word for word and the inputs it was applied to, or the override
// and both operators.
type settlementBasisEvidence struct {
	Rule              SettlementPriceRule `json:"rule"`
	RuleStatement     string              `json:"rule_statement"`
	PriceSource       string              `json:"price_source"`
	Expiry            time.Time           `json:"expiry"`
	OracleMaxAgeMS    int64               `json:"oracle_max_age_ms"`
	MarkID            string              `json:"mark_id,omitempty"`
	MarkMantissa      string              `json:"mark_mantissa,omitempty"`
	MarkScale         int32               `json:"mark_scale,omitempty"`
	IndexMantissa     string              `json:"index_mantissa,omitempty"`
	IndexScale        int32               `json:"index_scale,omitempty"`
	MarkObservedAt    *time.Time          `json:"mark_observed_at,omitempty"`
	MarkRecordedAt    *time.Time          `json:"mark_recorded_at,omitempty"`
	OverrideID        string              `json:"override_id,omitempty"`
	ReferenceMantissa string              `json:"reference_mantissa,omitempty"`
	MaxDeviationBPS   int64               `json:"max_deviation_bps,omitempty"`
	ProposedBy        string              `json:"proposed_by,omitempty"`
	ApprovedBy        string              `json:"approved_by,omitempty"`
	OverrideReason    string              `json:"override_reason,omitempty"`
	OverrideEvidence  string              `json:"override_evidence_ref,omitempty"`
}

func (b settlementBasis) evidence(spec Spec) settlementBasisEvidence {
	out := settlementBasisEvidence{
		Rule: b.rule, RuleStatement: SettlementRuleStatement, PriceSource: b.source,
		OracleMaxAgeMS: spec.OracleMaxAge.Milliseconds(),
	}
	if spec.Expiry != nil {
		out.Expiry = *spec.Expiry
	}
	if m := b.mark; m != nil {
		obs, rec := m.ObservedAt, m.RecordedAt
		out.MarkID, out.MarkMantissa, out.MarkScale = m.ID.String(), m.MarkMantissa.String(), m.MarkScale
		out.MarkObservedAt, out.MarkRecordedAt = &obs, &rec
		if m.HasIndex {
			out.IndexMantissa, out.IndexScale = m.IndexMantissa.String(), m.IndexScale
		}
	}
	if o := b.override; o != nil {
		out.OverrideID, out.ReferenceMantissa = o.ID.String(), o.ReferenceMantissa.String()
		out.MarkID = o.ReferenceMarkID.String()
		out.MaxDeviationBPS = int64(o.MaxDeviationBPS)
		out.ProposedBy = string(o.ProposedBy.Type) + ":" + o.ProposedBy.ID
		out.OverrideReason, out.OverrideEvidence = o.Reason, o.EvidenceRef
		if o.Decision != nil {
			out.ApprovedBy = string(o.Decision.DecidedBy.Type) + ":" + o.Decision.DecidedBy.ID
		}
	}
	return out
}

// settle claims the instrument's settlement at the basis's price and settles
// every open position at it.
func (s *Service) settle(ctx context.Context, tx pgx.Tx, spec Spec, kind SettlementKind, now time.Time, b settlementBasis, correlationID string) (SettlementResult, error) {
	positions, err := lockPositionsForInstrument(ctx, tx, spec.InstrumentID)
	if err != nil {
		return SettlementResult{}, err
	}
	sid := newSettlementID()
	var overrideID *SettlementOverrideID
	if b.override != nil {
		oid := b.override.ID
		overrideID = &oid
	}
	var claimed SettlementID
	err = tx.QueryRow(ctx, `INSERT INTO derivative_settlements (id, instrument_id, kind, settlement_mantissa, price_scale, price_source,
		price_rule, mark_id, override_id, positions, settled_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT (instrument_id) DO NOTHING RETURNING id`,
		sid, spec.InstrumentID, kind, b.price, spec.PriceScale, b.source, b.rule, b.markID, overrideID, len(positions), now).Scan(&claimed)
	if errors.Is(err, pgx.ErrNoRows) {
		existing, _, err := settlementFor(ctx, tx, spec.InstrumentID)
		return existing, err
	}
	if err != nil {
		return SettlementResult{}, dbErr("claim settlement", err)
	}
	ch, err := s.clearingHouse(ctx, tx)
	if err != nil {
		return SettlementResult{}, err
	}
	res := SettlementResult{ID: sid, Kind: kind, Price: b.price, Rule: b.rule, PriceSource: b.source, MarkID: b.markID,
		OverrideID: overrideID, Positions: len(positions)}
	basis := b.evidence(spec)
	plan := exercisePlan{
		underlying: b.price, kind: ExerciseAutomatic, settlement: &sid, holderSource: SourceExpiry, writerSource: SourceExpiry,
		closeTo: PositionSettled, keyPrefix: "settlement:" + sid.String(), evidenceKey: "derivatives/settlement/" + sid.String(),
		now: now, correlationID: correlationID, basis: &basis,
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
			Price: res.Price.String(), PriceScale: spec.PriceScale, Basis: plan.basis, Positions: res.Positions,
			PolicyVersion: s.policy.Version, Legs: pl}
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
			Price: res.Price.String(), PriceScale: spec.PriceScale, Basis: plan.basis, Positions: res.Positions,
			PolicyVersion: s.policy.Version, Legs: pl}
	}, plan.correlationID)
	if err != nil {
		return SettlementResult{}, err
	}
	res.Journals = journals(posted)
	return res, nil
}

// settlementEvidence is the archived document of a final settlement.
type settlementEvidence struct {
	SettlementID  string                   `json:"settlement_id"`
	InstrumentID  string                   `json:"instrument_id"`
	Kind          SettlementKind           `json:"kind"`
	Price         string                   `json:"settlement_mantissa"`
	PriceScale    int32                    `json:"price_scale"`
	Basis         *settlementBasisEvidence `json:"price_basis"`
	Positions     int                      `json:"positions"`
	PolicyVersion string                   `json:"policy_version"`
	Legs          []postedLeg              `json:"legs"`
}

// expiryQuote is what the settlement rule says about an expired contract.
type expiryQuote struct {
	// mark is the rule's mark (expiryMark), whether fresh or not.
	mark    Mark
	hasMark bool
	// price is the price the rule derives from mark, and source where it
	// came from.
	price  money.Quantity
	source string
	// fresh: the mark is fresh at expiry, so the rule settles at price.
	fresh bool
}

// expiryQuoteFor applies the settlement rule (SettlementRuleStatement) to an
// expired contract's recorded marks.
func expiryQuoteFor(ctx context.Context, q db.Querier, spec Spec) (expiryQuote, error) {
	if spec.Expiry == nil {
		return expiryQuote{}, errs.New(errs.CodeValidationFailed, "derivatives: only a dated future or an option expires")
	}
	m, ok, err := expiryMark(ctx, q, spec)
	if err != nil || !ok {
		return expiryQuote{}, err
	}
	if !markPriceable(spec, m) {
		// Recorded under terms the contract no longer has: it prices nothing.
		return expiryQuote{}, nil
	}
	out := expiryQuote{mark: m, hasMark: true, fresh: markFresh(spec, m, *spec.Expiry)}
	if spec.IsLinear() {
		out.price, out.source = m.MarkMantissa, "mark:"+m.ID.String()
		return out, nil
	}
	if out.price, err = alignIndex(spec, m.IndexMantissa, m.IndexScale, money.RoundHalfEven); err != nil {
		return expiryQuote{}, err
	}
	out.source = "index:" + m.ID.String() + ":half_even"
	return out, nil
}

// expiryMark is the mark the settlement rule reads: of the instrument's marks
// observed at or before expiry AND recorded at or before expiry (an option's:
// those carrying an index), the one observed last; ties by recording time,
// then id. A mark recorded after expiry is never one of them, whatever moment
// it claims. cp_derivative_expiry_mark (migration 00755) is the same query.
func expiryMark(ctx context.Context, q db.Querier, spec Spec) (Mark, bool, error) {
	m, err := scanMark(q.QueryRow(ctx, `SELECT `+markColumns+` FROM derivative_marks WHERE instrument_id = $1
		AND observed_at <= $2 AND recorded_at <= $2 AND ($3 = false OR index_mantissa IS NOT NULL)
		ORDER BY observed_at DESC, recorded_at DESC, id DESC LIMIT 1`, spec.InstrumentID, spec.Expiry.UTC(), spec.IsOption()))
	if errors.Is(err, pgx.ErrNoRows) {
		return Mark{}, false, nil
	}
	if err != nil {
		return Mark{}, false, dbErr("expiry mark", err)
	}
	return m, true, nil
}

// settlementFor returns an instrument's settlement, if it has one.
func settlementFor(ctx context.Context, q db.Querier, iid instruments.InstrumentID) (SettlementResult, bool, error) {
	rec, ok, err := GetSettlement(ctx, q, iid)
	if err != nil || !ok {
		return SettlementResult{}, ok, err
	}
	return SettlementResult{
		ID: rec.ID, Replay: true, Kind: rec.Kind, Price: rec.PriceMantissa, Rule: rec.Rule, PriceSource: rec.PriceSource,
		MarkID: rec.MarkID, OverrideID: rec.OverrideID, Positions: rec.Positions,
	}, true, nil
}
