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
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// The operator override of a final settlement price.
//
// The settlement rule (SettlementRuleStatement) settles a contract at the
// price it derives from the latest mark observed and recorded by expiry, and
// only when that mark is fresh. When it is stale -- the feed stalled before
// expiry -- or the operators judge the reference print wrong, the contract can
// still be settled, but only at a price that:
//
//   - one OPERATOR proposed, with a reason and evidence (a URI and its
//     SHA-256), after expiry and before settlement;
//   - lies on the tick grid within the policy's SettlementOverrideMaxBPS of
//     the rule's reference price -- the price the rule derives from that same
//     latest mark at expiry, fresh or not (the bound is stored with the
//     proposal and checked by the database as well);
//   - a second, different OPERATOR approved. Approval settles the contract at
//     that price in the same transaction.
//
// The database binds all of it: the proposal's reference is the rule's, its
// price within its bound (DV009); an approval by the proposer, or any decision
// once the contract has settled, is refused; and a settlement whose price rule
// is OPERATOR_OVERRIDE must name an override of the same instrument and price
// approved by a second operator (DV010).

// SettlementOverride is one proposed operator override of a final settlement
// price, with its decision once there is one.
type SettlementOverride struct {
	ID           SettlementOverrideID
	InstrumentID instruments.InstrumentID
	// PriceMantissa is the proposed settlement price at PriceScale (an
	// option's: the underlying's settlement price).
	PriceMantissa money.Quantity
	PriceScale    int32
	// ReferenceMarkID and ReferenceMantissa are the rule's reference at
	// expiry the proposal is bounded by; MaxDeviationBPS is the bound.
	ReferenceMarkID   MarkID
	ReferenceMantissa money.Quantity
	MaxDeviationBPS   money.BPS
	Reason            string
	EvidenceRef       string
	EvidenceSHA256    []byte
	ProposedBy        econ.Actor
	ProposedAt        time.Time
	Status            OverrideStatus
	// Decision is nil while the proposal is PROPOSED.
	Decision *OverrideDecisionRecord
	// SettlementID is the settlement an approved override settled.
	SettlementID *SettlementID
}

// OverrideDecisionRecord is a second operator's decision on a proposal.
type OverrideDecisionRecord struct {
	Decision  OverrideDecision
	Reason    string
	DecidedBy econ.Actor
	DecidedAt time.Time
}

// DeviationBPS is how far the proposed price is from the reference, in basis
// points of the reference, rounded up (for display; the bound is checked
// exactly).
func (o SettlementOverride) DeviationBPS() money.BPS {
	return deviationBPS(o.PriceMantissa, o.ReferenceMantissa)
}

// ProposeOverrideRequest proposes an override of an expired contract's final
// settlement price.
type ProposeOverrideRequest struct {
	InstrumentID instruments.InstrumentID
	// Price is the proposed settlement price: a mantissa at the instrument's
	// price scale, on its tick grid (an option's: the underlying's price).
	Price money.Quantity
	// Reason says why the rule's price is not the right one, in words.
	Reason string
	// EvidenceRef and EvidenceSHA256 name the document the proposal rests on (a URI or archive reference, and its digest).
	EvidenceRef    string
	EvidenceSHA256 []byte
	// Actor is the proposing OPERATOR.
	Actor         econ.Actor
	CorrelationID string
}

// OverrideDecisionRequest approves or rejects a proposal.
type OverrideDecisionRequest struct {
	OverrideID SettlementOverrideID
	Reason     string
	// Actor is the deciding OPERATOR; an approval must come from an operator
	// other than the proposer.
	Actor econ.Actor
	// Price, when set, is the override price the approver confirms; any other
	// refuses the approval (CONFLICT).
	Price         *money.Quantity
	CorrelationID string
}

func requireOperator(a econ.Actor, what string) error {
	if a.Type != security.ActorOperator {
		return errs.New(errs.CodeForbidden, "derivatives: only an operator "+what+" a settlement override").
			WithField("actor_type", string(a.Type))
	}
	if id := strings.TrimSpace(a.ID); id == "" || id != a.ID || len(id) > 200 {
		return errs.New(errs.CodeValidationFailed, "derivatives: the operator is named (1..200 characters)")
	}
	return nil
}

func requireReason(reason string) error {
	if r := strings.TrimSpace(reason); r == "" || r != reason || len(r) > 1000 {
		return errs.New(errs.CodeValidationFailed, "derivatives: a settlement override states its reason (1..1000 characters, trimmed)")
	}
	return nil
}

// ProposeSettlementOverride records one operator's proposal to settle an
// expired, unsettled contract at a given price, within the policy's bound of
// the rule's reference price, with a reason and evidence. Nothing settles
// until a second operator approves it (ApproveSettlementOverride).
func (s *Service) ProposeSettlementOverride(ctx context.Context, tx pgx.Tx, r ProposeOverrideRequest) (SettlementOverride, error) {
	if err := requireOperator(r.Actor, "proposes"); err != nil {
		return SettlementOverride{}, err
	}
	if err := requireReason(r.Reason); err != nil {
		return SettlementOverride{}, err
	}
	if u := strings.TrimSpace(r.EvidenceRef); u == "" || u != r.EvidenceRef || len(u) > 500 {
		return SettlementOverride{}, errs.New(errs.CodeValidationFailed, "derivatives: an override names the evidence it rests on (a reference of 1..500 characters)")
	}
	if len(r.EvidenceSHA256) != 32 {
		return SettlementOverride{}, errs.New(errs.CodeValidationFailed, "derivatives: an override's evidence carries its SHA-256 digest (32 bytes)")
	}
	if err := s.serializeClearing(ctx, tx); err != nil {
		return SettlementOverride{}, err
	}
	spec, _, now, err := s.expiredContract(ctx, tx, r.InstrumentID)
	if err != nil {
		return SettlementOverride{}, err
	}
	if _, settled, err := settlementFor(ctx, tx, r.InstrumentID); err != nil {
		return SettlementOverride{}, err
	} else if settled {
		return SettlementOverride{}, errs.New(errs.CodeConflict, "derivatives: the contract has been settled; there is nothing to override").
			WithField("instrument_id", r.InstrumentID.String())
	}
	if !spec.OnTick(r.Price) {
		return SettlementOverride{}, errs.New(errs.CodeValidationFailed, "derivatives: an override price must lie on the tick grid").
			WithField("price_mantissa", r.Price.String()).WithField("tick_size", spec.Tick.String())
	}
	quote, err := expiryQuoteFor(ctx, tx, spec)
	if err != nil {
		return SettlementOverride{}, err
	}
	if !quote.hasMark {
		return SettlementOverride{}, errs.New(errs.CodeStaleMarketData,
			"derivatives: no mark was observed and recorded by expiry, so there is no reference price to bound an override by").
			WithField("instrument_id", r.InstrumentID.String())
	}
	bound := s.policy.SettlementOverrideMaxBPS
	if !withinBound(r.Price, quote.price, bound) {
		lo, hi, err := overrideBounds(spec, quote.price, bound)
		if err != nil {
			return SettlementOverride{}, err
		}
		return SettlementOverride{}, errs.New(errs.CodeValidationFailed,
			"derivatives: an override price must lie within the policy's bound of the rule's reference price").
			WithField("price_mantissa", r.Price.String()).WithField("reference_mantissa", quote.price.String()).
			WithField("max_deviation_bps", int64(bound)).WithField("lowest_price_mantissa", lo.String()).
			WithField("highest_price_mantissa", hi.String())
	}
	o := SettlementOverride{
		ID: newSettlementOverrideID(), InstrumentID: r.InstrumentID, PriceMantissa: r.Price, PriceScale: spec.PriceScale,
		ReferenceMarkID: quote.mark.ID, ReferenceMantissa: quote.price, MaxDeviationBPS: bound, Reason: r.Reason,
		EvidenceRef: r.EvidenceRef, EvidenceSHA256: append([]byte(nil), r.EvidenceSHA256...), ProposedBy: r.Actor,
		ProposedAt: now, Status: OverrideProposed,
	}
	if _, err := tx.Exec(ctx, `INSERT INTO derivative_settlement_overrides
		(id, instrument_id, price_mantissa, price_scale, reference_mark_id, reference_mantissa, max_deviation_bps, reason,
		 evidence_ref, evidence_sha256, proposed_by_actor_type, proposed_by_actor_id, proposed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		o.ID, o.InstrumentID, o.PriceMantissa, o.PriceScale, o.ReferenceMarkID, o.ReferenceMantissa, int64(o.MaxDeviationBPS),
		o.Reason, o.EvidenceRef, o.EvidenceSHA256, o.ProposedBy.Type, o.ProposedBy.ID, o.ProposedAt); err != nil {
		return SettlementOverride{}, dbErr("record settlement override", err)
	}
	if _, err := econ.RecordEvidence(ctx, tx, econ.Evidence{Key: "derivatives/settlement-override/" + o.ID.String(),
		Document: overrideEvidence{
			OverrideID: o.ID.String(), InstrumentID: o.InstrumentID.String(), Price: o.PriceMantissa.String(), PriceScale: o.PriceScale,
			ReferenceMarkID: o.ReferenceMarkID.String(), ReferenceMantissa: o.ReferenceMantissa.String(),
			ReferenceObservedAt: quote.mark.ObservedAt, ReferenceRecordedAt: quote.mark.RecordedAt, ReferenceFresh: quote.fresh,
			MaxDeviationBPS: int64(bound), DeviationBPS: int64(o.DeviationBPS()), Reason: o.Reason, EvidenceRef: o.EvidenceRef,
			EvidenceSHA256: o.EvidenceSHA256, ProposedBy: string(o.ProposedBy.Type) + ":" + o.ProposedBy.ID, ProposedAt: now,
			RuleStatement: SettlementRuleStatement, PolicyVersion: s.policy.Version, PolicyHash: s.policy.Hash(),
		}}); err != nil {
		return SettlementOverride{}, err
	}
	return o, nil
}

// overrideEvidence is the archived document of a proposal.
type overrideEvidence struct {
	OverrideID          string    `json:"override_id"`
	InstrumentID        string    `json:"instrument_id"`
	Price               string    `json:"price_mantissa"`
	PriceScale          int32     `json:"price_scale"`
	ReferenceMarkID     string    `json:"reference_mark_id"`
	ReferenceMantissa   string    `json:"reference_mantissa"`
	ReferenceObservedAt time.Time `json:"reference_observed_at"`
	ReferenceRecordedAt time.Time `json:"reference_recorded_at"`
	ReferenceFresh      bool      `json:"reference_fresh"`
	MaxDeviationBPS     int64     `json:"max_deviation_bps"`
	DeviationBPS        int64     `json:"deviation_bps"`
	Reason              string    `json:"reason"`
	EvidenceRef         string    `json:"evidence_ref"`
	EvidenceSHA256      []byte    `json:"evidence_sha256"`
	ProposedBy          string    `json:"proposed_by"`
	ProposedAt          time.Time `json:"proposed_at"`
	RuleStatement       string    `json:"rule_statement"`
	PolicyVersion       string    `json:"policy_version"`
	PolicyHash          string    `json:"policy_hash"`
}

// ApproveSettlementOverride is the second operator's approval of a proposal,
// and the settlement at its price, in one transaction. The approver must not
// be the proposer; the contract must still be unsettled and its reference at
// expiry still the one the proposal was bounded by.
func (s *Service) ApproveSettlementOverride(ctx context.Context, tx pgx.Tx, r OverrideDecisionRequest) (SettlementOverride, SettlementResult, error) {
	if err := requireOperator(r.Actor, "approves"); err != nil {
		return SettlementOverride{}, SettlementResult{}, err
	}
	if err := requireReason(r.Reason); err != nil {
		return SettlementOverride{}, SettlementResult{}, err
	}
	if err := s.serializeClearing(ctx, tx); err != nil {
		return SettlementOverride{}, SettlementResult{}, err
	}
	o, err := getSettlementOverride(ctx, tx, r.OverrideID)
	if err != nil {
		return SettlementOverride{}, SettlementResult{}, err
	}
	if err := undecided(o); err != nil {
		return SettlementOverride{}, SettlementResult{}, err
	}
	if r.Actor.ID == o.ProposedBy.ID {
		return SettlementOverride{}, SettlementResult{}, errs.New(errs.CodeForbidden,
			"derivatives: a settlement override is approved by a second operator, never by the one who proposed it").
			WithField("override_id", o.ID.String())
	}
	spec, kind, now, err := s.expiredContract(ctx, tx, o.InstrumentID)
	if err != nil {
		return SettlementOverride{}, SettlementResult{}, err
	}
	if _, settled, err := settlementFor(ctx, tx, o.InstrumentID); err != nil {
		return SettlementOverride{}, SettlementResult{}, err
	} else if settled {
		return SettlementOverride{}, SettlementResult{}, errs.New(errs.CodeConflict, "derivatives: the contract has been settled; the proposal can no longer be approved").
			WithField("override_id", o.ID.String())
	}
	if err := confirmPrice(r.Price, "", o.PriceMantissa, ""); err != nil {
		return SettlementOverride{}, SettlementResult{}, err
	}
	quote, err := expiryQuoteFor(ctx, tx, spec)
	if err != nil {
		return SettlementOverride{}, SettlementResult{}, err
	}
	if !quote.hasMark || quote.mark.ID != o.ReferenceMarkID || !quote.price.Equal(o.ReferenceMantissa) {
		return SettlementOverride{}, SettlementResult{}, errs.New(errs.CodeConflict,
			"derivatives: the rule's reference at expiry is no longer the one this proposal was bounded by").
			WithField("override_id", o.ID.String())
	}
	if err := insertOverrideDecision(ctx, tx, o.ID, DecisionApproved, r.Reason, r.Actor, now); err != nil {
		return SettlementOverride{}, SettlementResult{}, err
	}
	o.Status, o.Decision = OverrideApproved, &OverrideDecisionRecord{Decision: DecisionApproved, Reason: r.Reason, DecidedBy: r.Actor, DecidedAt: now}
	mid := o.ReferenceMarkID
	res, err := s.settle(ctx, tx, spec, kind, now, settlementBasis{
		rule: RuleOperatorOverride, price: o.PriceMantissa, source: "override:" + o.ID.String(), markID: &mid, override: &o,
	}, r.CorrelationID)
	if err != nil {
		return SettlementOverride{}, SettlementResult{}, err
	}
	if res.Replay {
		// A concurrent settlement won the claim: this approval did not settle
		// anything and must not stand as if it had.
		return SettlementOverride{}, SettlementResult{}, errs.New(errs.CodeConflict, "derivatives: the contract was settled concurrently").
			WithField("override_id", o.ID.String())
	}
	sid := res.ID
	o.SettlementID = &sid
	return o, res, nil
}

// RejectSettlementOverride records an operator's rejection of a proposal (the
// proposer may withdraw its own). A rejected proposal settles nothing.
func (s *Service) RejectSettlementOverride(ctx context.Context, tx pgx.Tx, r OverrideDecisionRequest) (SettlementOverride, error) {
	if err := requireOperator(r.Actor, "rejects"); err != nil {
		return SettlementOverride{}, err
	}
	if err := requireReason(r.Reason); err != nil {
		return SettlementOverride{}, err
	}
	if err := s.serializeClearing(ctx, tx); err != nil {
		return SettlementOverride{}, err
	}
	o, err := getSettlementOverride(ctx, tx, r.OverrideID)
	if err != nil {
		return SettlementOverride{}, err
	}
	if err := undecided(o); err != nil {
		return SettlementOverride{}, err
	}
	if _, settled, err := settlementFor(ctx, tx, o.InstrumentID); err != nil {
		return SettlementOverride{}, err
	} else if settled {
		return SettlementOverride{}, errs.New(errs.CodeConflict, "derivatives: the contract has been settled; there is nothing left to decide").
			WithField("override_id", o.ID.String())
	}
	now := s.clk.Now().UTC()
	if err := insertOverrideDecision(ctx, tx, o.ID, DecisionRejected, r.Reason, r.Actor, now); err != nil {
		return SettlementOverride{}, err
	}
	o.Status, o.Decision = OverrideRejected, &OverrideDecisionRecord{Decision: DecisionRejected, Reason: r.Reason, DecidedBy: r.Actor, DecidedAt: now}
	return o, nil
}

func undecided(o SettlementOverride) error {
	if o.Decision != nil {
		return errs.Newf(errs.CodeConflict, "derivatives: this proposal was already %s", strings.ToLower(string(o.Decision.Decision))).
			WithField("override_id", o.ID.String())
	}
	return nil
}

func insertOverrideDecision(ctx context.Context, tx pgx.Tx, oid SettlementOverrideID, d OverrideDecision, reason string, by econ.Actor, at time.Time) error {
	if _, err := tx.Exec(ctx, `INSERT INTO derivative_settlement_override_decisions
		(id, override_id, decision, reason, decided_by_actor_type, decided_by_actor_id, decided_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		id.New[id.Any](), oid, d, reason, by.Type, by.ID, at.UTC()); err != nil {
		if db.IsUniqueViolation(err) {
			return errs.Wrap(err, errs.CodeConflict, "derivatives: this proposal was decided concurrently").WithField("override_id", oid.String())
		}
		return dbErr("record settlement override decision", err)
	}
	return nil
}

const overrideColumns = `o.id, o.instrument_id, o.price_mantissa, o.price_scale, o.reference_mark_id, o.reference_mantissa,
	o.max_deviation_bps, o.reason, o.evidence_ref, o.evidence_sha256, o.proposed_by_actor_type, o.proposed_by_actor_id,
	o.proposed_at, d.decision, d.reason, d.decided_by_actor_type, d.decided_by_actor_id, d.decided_at, s.id`

const overrideFrom = ` FROM derivative_settlement_overrides o
	LEFT JOIN derivative_settlement_override_decisions d ON d.override_id = o.id
	LEFT JOIN derivative_settlements s ON s.override_id = o.id`

func scanOverride(row pgx.Row) (SettlementOverride, error) {
	var o SettlementOverride
	var bps int64
	var decision, reason, byType, byID *string
	var decidedAt *time.Time
	if err := row.Scan(&o.ID, &o.InstrumentID, &o.PriceMantissa, &o.PriceScale, &o.ReferenceMarkID, &o.ReferenceMantissa, &bps,
		&o.Reason, &o.EvidenceRef, &o.EvidenceSHA256, &o.ProposedBy.Type, &o.ProposedBy.ID, &o.ProposedAt,
		&decision, &reason, &byType, &byID, &decidedAt, &o.SettlementID); err != nil {
		return SettlementOverride{}, err
	}
	o.MaxDeviationBPS = money.BPS(bps)
	o.ProposedAt = o.ProposedAt.UTC()
	o.Status = OverrideProposed
	if decision != nil && reason != nil && byType != nil && byID != nil && decidedAt != nil {
		o.Decision = &OverrideDecisionRecord{Decision: OverrideDecision(*decision), Reason: *reason,
			DecidedBy: econ.Actor{Type: security.ActorType(*byType), ID: *byID}, DecidedAt: decidedAt.UTC()}
		o.Status = OverrideApproved
		if o.Decision.Decision == DecisionRejected {
			o.Status = OverrideRejected
		}
	}
	return o, nil
}

// getSettlementOverride reads one proposal. Deciding one needs no row lock:
// every decision is taken under the clearing house's lock, and the database
// allows one decision per proposal (UNIQUE) besides.
func getSettlementOverride(ctx context.Context, q db.Querier, oid SettlementOverrideID) (SettlementOverride, error) {
	o, err := scanOverride(q.QueryRow(ctx, `SELECT `+overrideColumns+overrideFrom+` WHERE o.id = $1`, oid))
	if errors.Is(err, pgx.ErrNoRows) {
		return SettlementOverride{}, errs.New(errs.CodeNotFound, "derivatives: settlement override not found").WithField("override_id", oid.String())
	}
	if err != nil {
		return SettlementOverride{}, dbErr("read settlement override", err)
	}
	return o, nil
}

// GetSettlementOverride returns one proposal with its decision.
func GetSettlementOverride(ctx context.Context, q db.Querier, oid SettlementOverrideID) (SettlementOverride, error) {
	return getSettlementOverride(ctx, q, oid)
}

// SettlementOverrides lists an instrument's override proposals with their
// decisions, newest first.
func SettlementOverrides(ctx context.Context, q db.Querier, iid instruments.InstrumentID) ([]SettlementOverride, error) {
	rows, err := q.Query(ctx, `SELECT `+overrideColumns+overrideFrom+` WHERE o.instrument_id = $1
		ORDER BY o.proposed_at DESC, o.id DESC LIMIT 200`, iid)
	if err != nil {
		return nil, dbErr("list settlement overrides", err)
	}
	defer rows.Close()
	out := []SettlementOverride{}
	for rows.Next() {
		o, err := scanOverride(rows)
		if err != nil {
			return nil, dbErr("scan settlement override", err)
		}
		out = append(out, o)
	}
	return out, dbErr("list settlement overrides", rows.Err())
}

// SettlementPreview is what the settlement rule says about a dated contract
// now: before expiry, nothing; after it, the rule's reference mark and price,
// whether the rule settles at it, and the range an override may propose.
type SettlementPreview struct {
	InstrumentID instruments.InstrumentID
	Kind         SettlementKind
	Expiry       time.Time
	Expired      bool
	Settled      bool
	PriceScale   int32
	// HasReference is false before expiry and when no mark was observed and
	// recorded by expiry.
	HasReference   bool
	ReferenceMark  Mark
	ReferencePrice money.Quantity
	PriceSource    string
	// Settleable: the reference is fresh at expiry, so SettleExpiry settles
	// at ReferencePrice.
	Settleable bool
	// OverrideMinPrice and OverrideMaxPrice bound, on the tick grid, what an
	// override may propose: within MaxDeviationBPS of ReferencePrice.
	OverrideMinPrice money.Quantity
	OverrideMaxPrice money.Quantity
	MaxDeviationBPS  money.BPS
	RuleStatement    string
}

// SettlementPreview applies the settlement rule to a dated contract's
// recorded marks without settling anything.
func (s *Service) SettlementPreview(ctx context.Context, q db.Querier, iid instruments.InstrumentID) (SettlementPreview, error) {
	spec, err := loadSpec(ctx, q, s.instruments, iid)
	if err != nil {
		return SettlementPreview{}, err
	}
	kind := SettleFuture
	switch spec.Kind {
	case instruments.TypeFuture:
	case instruments.TypeOption:
		kind = SettleOption
	default:
		return SettlementPreview{}, errs.New(errs.CodeValidationFailed, "derivatives: only a dated future or an option has a final settlement")
	}
	out := SettlementPreview{InstrumentID: iid, Kind: kind, Expiry: *spec.Expiry, PriceScale: spec.PriceScale,
		MaxDeviationBPS: s.policy.SettlementOverrideMaxBPS, RuleStatement: SettlementRuleStatement}
	if _, settled, err := settlementFor(ctx, q, iid); err != nil {
		return SettlementPreview{}, err
	} else if settled {
		out.Settled = true
	}
	out.Expired = spec.ExpiredAt(s.clk.Now().UTC())
	if !out.Expired {
		return out, nil
	}
	quote, err := expiryQuoteFor(ctx, q, spec)
	if err != nil || !quote.hasMark {
		return out, err
	}
	out.HasReference, out.ReferenceMark, out.ReferencePrice, out.PriceSource = true, quote.mark, quote.price, quote.source
	out.Settleable = quote.fresh
	if out.OverrideMinPrice, out.OverrideMaxPrice, err = overrideBounds(spec, quote.price, out.MaxDeviationBPS); err != nil {
		return SettlementPreview{}, err
	}
	return out, nil
}

// withinBound reports whether |price − reference| × 10 000 ≤ bps ×
// reference: exactly the CHECK derivative_settlement_overrides_bounded.
func withinBound(price, reference money.Quantity, bps money.BPS) bool {
	if price.IsNegative() || reference.IsNegative() || bps < 0 {
		return false
	}
	diff := price.Sub(reference).Abs()
	return diff.Mul(money.QuantityFromInt64(int64(money.OneHundredPercent))).Cmp(reference.Mul(money.QuantityFromInt64(int64(bps)))) <= 0
}

// overrideBounds returns the lowest and the highest price on the tick grid
// within bps of an on-grid reference.
func overrideBounds(spec Spec, reference money.Quantity, bps money.BPS) (lo, hi money.Quantity, err error) {
	delta, err := reference.MulDiv(money.QuantityFromInt64(int64(bps)), money.QuantityFromInt64(int64(money.OneHundredPercent)), money.RoundFloor)
	if err != nil {
		return money.Quantity{}, money.Quantity{}, errs.Wrap(err, errs.CodeInternal, "derivatives: override bound")
	}
	steps, err := delta.Div(spec.Tick, money.RoundFloor)
	if err != nil {
		return money.Quantity{}, money.Quantity{}, errs.Wrap(err, errs.CodeInternal, "derivatives: override bound")
	}
	span := steps.Mul(spec.Tick)
	return reference.Sub(span).Max(money.Quantity{}), reference.Add(span), nil
}

// deviationBPS is |price − reference| / reference in basis points, rounded
// up; a zero reference deviates by zero only from itself.
func deviationBPS(price, reference money.Quantity) money.BPS {
	diff := price.Sub(reference).Abs()
	if diff.IsZero() {
		return 0
	}
	if !reference.IsPositive() {
		return maxDivergenceBPS
	}
	v, err := diff.MulDiv(money.QuantityFromInt64(int64(money.OneHundredPercent)), reference, money.RoundUp)
	if err != nil || v.Cmp(money.QuantityFromInt64(int64(maxDivergenceBPS))) > 0 {
		return maxDivergenceBPS
	}
	b, _ := v.Int64()
	return money.BPS(b)
}
