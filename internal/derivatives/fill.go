package derivatives

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
)

// FillDirection constrains what a fill may do to the position it lands on.
type FillDirection int

// Fill directions.
const (
	// FillOpens must open a position or add to it.
	FillOpens FillDirection = iota + 1
	// FillReduces must reduce a position, never past zero.
	FillReduces
	// FillEither may do either (a matched book fill); it may still never take
	// a position through zero.
	FillEither
)

// Fill is a trade to apply to a position: a paper fill priced by this
// package, or a fill matched in an internal order book. Both go through
// ApplyFill, the only path that changes a position by trading.
type Fill struct {
	// Key is deterministic per fill ("intent:<intent id>" for a paper fill,
	// the match id and side for a book fill). Applying the same key twice is a
	// replay: nothing is written the second time.
	Key          string
	AccountID    accounts.AccountID
	InstrumentID instruments.InstrumentID
	Side         FillSide
	// Quantity is the number of contracts, positive.
	Quantity money.Quantity
	// Price is the trade price at the instrument's price scale, on its tick
	// grid.
	Price  money.Quantity
	Source FillSource
	// MarkID is the mark the fill's initial margin is computed at (and, for a
	// paper fill, the mark it was priced from).
	MarkID      MarkID
	SlippageBPS money.BPS
	Direction   FillDirection
	IntentID    *econ.IntentID
	// DecisionID is the recorded Risk V2 decision that allowed the trade. The
	// database refuses a trade without one.
	DecisionID    risk.DecisionID
	OccurredAt    time.Time
	CorrelationID string
}

func (f Fill) validate() error {
	var problems []string
	if k := strings.TrimSpace(f.Key); k == "" || k != f.Key || len(k) > 200 {
		problems = append(problems, "key must be 1..200 bytes without surrounding whitespace")
	}
	if f.AccountID.IsZero() || f.InstrumentID.IsZero() {
		problems = append(problems, "account and instrument required")
	}
	if !f.Side.Valid() {
		problems = append(problems, "side must be BUY or SELL")
	}
	if !f.Quantity.IsPositive() {
		problems = append(problems, "quantity must be positive")
	}
	if f.Price.IsNegative() {
		problems = append(problems, "price must not be negative")
	}
	if !f.Source.isTrade() {
		problems = append(problems, "a fill is a PAPER or BOOK trade")
	}
	if f.MarkID.IsZero() {
		problems = append(problems, "the mark the fill is margined at is required")
	}
	if f.DecisionID.IsZero() {
		problems = append(problems, "a trade carries the risk decision that allowed it")
	}
	if f.Direction < FillOpens || f.Direction > FillEither {
		problems = append(problems, "direction required")
	}
	if f.OccurredAt.IsZero() {
		problems = append(problems, "occurred_at required")
	}
	if f.SlippageBPS < 0 {
		problems = append(problems, "slippage must not be negative")
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "derivatives: invalid fill").WithField("problems", problems)
	}
	return nil
}

// FillResult is what ApplyFill did.
type FillResult struct {
	Fill     FillRecord
	Position Position
	Replay   bool
	// InitialMargin is the margin an opening fill posted.
	InitialMargin money.Quantity
	// PremiumPaid is the premium an opening long option fill paid.
	PremiumPaid money.Quantity
	Journals    []ledger.TransactionID
}

// ApplyFill applies one trade to the account's position on the instrument:
// it opens, adds to or reduces the position, posts the trade's cash flows
// through the clearing house (premium, margin, realized PnL), registers them,
// archives the evidence and applies every effect -- exactly once per fill key.
//
// It enforces the invariants of a trade, not the decision to make it: the
// caller (OpenPosition, ClosePosition, or the order book) evaluated Risk V2
// and names the decision. A fill that would take a position through zero is
// refused; trading ends at expiry.
func (s *Service) ApplyFill(ctx context.Context, tx pgx.Tx, f Fill) (FillResult, error) {
	if err := f.validate(); err != nil {
		return FillResult{}, err
	}
	if err := s.serializeClearing(ctx, tx); err != nil {
		return FillResult{}, err
	}
	if existing, ok, err := fillByKey(ctx, tx, f.Key); err != nil {
		return FillResult{}, err
	} else if ok {
		return s.replayFill(ctx, tx, f, existing)
	}
	spec, err := loadSpec(ctx, tx, s.instruments, f.InstrumentID)
	if err != nil {
		return FillResult{}, err
	}
	now := s.clk.Now().UTC()
	if f.OccurredAt.After(now) {
		return FillResult{}, errs.New(errs.CodeValidationFailed, "derivatives: a fill cannot occur in the future").
			WithField("occurred_at", f.OccurredAt).WithField("now", now)
	}
	// Trading ends at expiry by the fill's own time and by this service's
	// clock: a fill matched just before expiry but applied after it would
	// open a position the final settlement already passed over.
	if spec.ExpiredAt(f.OccurredAt) || spec.ExpiredAt(now) {
		return FillResult{}, errs.New(errs.CodeInvalidStateTransition, "derivatives: trading in this contract ended at expiry; it settles").
			WithField("instrument_id", f.InstrumentID.String())
	}
	if _, settled, err := settlementFor(ctx, tx, f.InstrumentID); err != nil {
		return FillResult{}, err
	} else if settled {
		return FillResult{}, errs.New(errs.CodeInvalidStateTransition, "derivatives: this contract has been finally settled").
			WithField("instrument_id", f.InstrumentID.String())
	}
	if err := spec.CheckQuantity(f.Quantity); err != nil {
		return FillResult{}, err
	}
	if !spec.OnTick(f.Price) {
		return FillResult{}, errs.New(errs.CodeValidationFailed, "derivatives: a trade price must lie on the tick grid").
			WithField("price_mantissa", f.Price.String())
	}
	mark, err := markByID(ctx, tx, f.InstrumentID, f.MarkID)
	if err != nil {
		return FillResult{}, err
	}
	ch, err := s.ClearingHouse(ctx, tx)
	if err != nil {
		return FillResult{}, err
	}
	d := f.Quantity
	if f.Side == Sell {
		d = d.Neg()
	}
	// A concurrent transaction may commit the first OPEN position on this
	// (account, instrument) between our read and our insert; the second pass
	// then finds it locked and applies the fill to it.
	for attempt := 0; attempt < 2; attempt++ {
		pos, found, err := lockOpenPosition(ctx, tx, f.AccountID, f.InstrumentID)
		if err != nil {
			return FillResult{}, err
		}
		if !found {
			if f.Direction == FillReduces {
				return FillResult{}, errs.New(errs.CodeNotFound, "derivatives: there is no open position to reduce").
					WithField("instrument_id", f.InstrumentID.String())
			}
			pos = Position{
				ID: newPositionID(), AccountID: f.AccountID, InstrumentID: f.InstrumentID,
				SettlementAssetID: spec.SettlementAssetID, Status: PositionOpen, OpenedAt: f.OccurredAt.UTC(), Version: 1,
			}
		}
		res, done, err := s.applyToPosition(ctx, tx, ch, spec, mark, f, pos, !found, d)
		if err != nil {
			return FillResult{}, err
		}
		if done {
			return res, nil
		}
	}
	return FillResult{}, errs.New(errs.CodeConflict, "derivatives: the position changed concurrently; retry the fill").
		WithField("fill_key", f.Key)
}

// applyToPosition applies the fill to pos. done is false when pos was new and
// a concurrent transaction created the open position first.
func (s *Service) applyToPosition(ctx context.Context, tx pgx.Tx, ch econ.Facility, spec Spec, mark Mark, f Fill,
	pos Position, isNew bool, d money.Quantity) (FillResult, bool, error) {
	eff, err := applyFill(spec, pos.state(), d, f.Price)
	if err != nil {
		return FillResult{}, false, err
	}
	switch {
	case f.Direction == FillOpens && !eff.Increase:
		return FillResult{}, false, errs.New(errs.CodeConflict,
			"derivatives: an opening fill would reduce the open position on the other side; close it first").
			WithField("position_id", pos.ID.String())
	case f.Direction == FillReduces && eff.Increase:
		return FillResult{}, false, errs.New(errs.CodeConflict, "derivatives: a reducing fill would add to the position").
			WithField("position_id", pos.ID.String())
	}
	fillID := newFillID()
	mid := mark.ID
	rec := FillRecord{
		ID: fillID, Key: f.Key, PositionID: pos.ID, AccountID: f.AccountID, InstrumentID: f.InstrumentID, Source: f.Source,
		Side: f.Side, Quantity: d, PriceMantissa: f.Price, PriceScale: spec.PriceScale, Value: eff.Value,
		RealizedPnL: eff.RealizedPnL, CashSettled: eff.Cash, MarkID: &mid, SlippageBPS: f.SlippageBPS,
		RiskDecisionID: f.DecisionID.String(), OccurredAt: f.OccurredAt.UTC(),
	}
	if f.IntentID != nil {
		rec.IntentID = f.IntentID.String()
	}
	ref := ledger.FinancialEventReference{Type: "derivative_fill", ID: fillID.String()}
	prefix := "deriv:fill:" + f.Key
	var legs []leg
	var im, premiumPaid money.Quantity
	after := pos
	if eff.Increase {
		if im, err = initialMarginForFill(s.policy, spec, f.Side, f.Quantity, markInputOf(mark), f.OccurredAt); err != nil {
			return FillResult{}, false, err
		}
		after.Quantity, after.CostValue, after.SettledValue = eff.After.Quantity, eff.After.CostValue, eff.After.SettledValue
		after.MarginPosted = eff.After.MarginPosted.Add(im)
		acct := f.AccountID
		if spec.IsOption() {
			premium := eff.Value.Abs()
			var entries []ledger.Entry
			if f.Side == Buy {
				entries, premiumPaid = premiumPaidEntries(acct, ch, spec.SettlementAssetID, premium), premium
			} else {
				entries = premiumReceivedEntries(acct, ch, spec.SettlementAssetID, premium)
			}
			legs = append(legs, leg{
				name: "option premium", key: prefix + ":premium", effectKind: EffectPremium, kind: ledger.KindOptionPremium,
				account: &acct, intent: f.IntentID, reference: ref, entries: entries,
				flow: s.flowFor(pos, spec, FlowOptionPremium, eff.Cash, money.Quantity{}, prefix+":premium", ref),
			})
		}
		legs = append(legs, leg{
			name: "initial margin", key: prefix + ":margin-post", effectKind: EffectMarginPost, kind: ledger.KindDerivativeMarginMove,
			account: &acct, intent: f.IntentID, reference: ref, entries: marginMoveEntries(acct, spec.SettlementAssetID, im, true),
		})
	} else {
		red, err := s.planReduction(ctx, tx, ch, spec, pos, eff, reductionKeys{
			prefix: prefix, reference: ref, intent: f.IntentID, cashKind: tradeCashKind(spec), cashJournal: tradeJournal(spec),
			cashEffect: tradeEffect(spec),
		})
		if err != nil {
			return FillResult{}, false, err
		}
		after, legs = red.after, red.legs
	}
	saved, ok, err := persistPosition(ctx, tx, after, isNew, PositionClosed, s.actor, "closed by fill "+f.Key, f.OccurredAt)
	if err != nil {
		return FillResult{}, false, err
	}
	if !ok {
		return FillResult{}, false, nil
	}
	if err := insertFill(ctx, tx, rec); err != nil {
		return FillResult{}, false, err
	}
	posted, err := s.executeLegs(ctx, tx, ch, legs, "derivatives/fill/"+fillID.String(), func(p []postedLeg) any {
		return fillEvidence{
			Fill: rec, Before: pos.state(), After: saved.state(), MarkID: mark.ID.String(), Mark: mark.MarkMantissa.String(),
			MarkObservedAt: mark.ObservedAt, InitialMargin: im.String(), PolicyVersion: s.policy.Version, PolicyHash: s.policy.Hash(),
			Legs: p,
		}
	}, f.CorrelationID)
	if err != nil {
		return FillResult{}, false, err
	}
	return FillResult{Fill: rec, Position: saved, InitialMargin: im, PremiumPaid: premiumPaid, Journals: journals(posted)}, true, nil
}

// fillEvidence is the archived document of one fill.
type fillEvidence struct {
	Fill           FillRecord    `json:"fill"`
	Before         positionState `json:"position_before"`
	After          positionState `json:"position_after"`
	MarkID         string        `json:"mark_id"`
	Mark           string        `json:"mark_mantissa"`
	MarkObservedAt time.Time     `json:"mark_observed_at"`
	InitialMargin  string        `json:"initial_margin_posted"`
	PolicyVersion  string        `json:"policy_version"`
	PolicyHash     string        `json:"policy_hash"`
	Legs           []postedLeg   `json:"legs"`
}

func (s *Service) replayFill(ctx context.Context, tx pgx.Tx, f Fill, existing FillRecord) (FillResult, error) {
	d := f.Quantity
	if f.Side == Sell {
		d = d.Neg()
	}
	if existing.AccountID != f.AccountID || existing.InstrumentID != f.InstrumentID || existing.Source != f.Source ||
		!existing.Quantity.Equal(d) || !existing.PriceMantissa.Equal(f.Price) {
		return FillResult{}, errs.New(errs.CodeInvalidIdempotencyReuse, "derivatives: this fill key was already used for a different fill").
			WithField("fill_key", f.Key)
	}
	p, err := GetPosition(ctx, tx, existing.PositionID)
	if err != nil {
		return FillResult{}, err
	}
	return FillResult{Fill: existing, Position: p, Replay: true}, nil
}

// flowFor builds the register row of a cash flow (nil when nothing is due).
func (s *Service) flowFor(pos Position, spec Spec, kind CashFlowKind, amount, shortfall money.Quantity, key string, ref ledger.FinancialEventReference) *CashFlow {
	if amount.IsZero() && shortfall.IsZero() {
		return nil
	}
	return &CashFlow{
		Key: key, PositionID: pos.ID, AccountID: pos.AccountID, InstrumentID: pos.InstrumentID,
		SettlementAssetID: spec.SettlementAssetID, Kind: kind, Amount: amount, Shortfall: shortfall,
		ReferenceType: ref.Type, ReferenceID: ref.ID,
	}
}

func tradeCashKind(s Spec) CashFlowKind {
	if s.IsOption() {
		return FlowOptionPremium
	}
	return FlowRealized
}

func tradeJournal(s Spec) ledger.Kind {
	if s.IsOption() {
		return ledger.KindOptionPremium
	}
	return ledger.KindVariationMargin
}

func tradeEffect(s Spec) string {
	if s.IsOption() {
		return EffectPremium
	}
	return EffectRealized
}

// reductionKeys names the legs of a reduction.
type reductionKeys struct {
	prefix      string
	reference   ledger.FinancialEventReference
	intent      *econ.IntentID
	cashKind    CashFlowKind
	cashJournal ledger.Kind
	cashEffect  string
}

// reduction is the plan for closing contracts of one position.
type reduction struct {
	after Position
	pay   payment
	// cash is the signed cash due (> 0 to the customer).
	cash money.Quantity
	legs []leg
}

// payer reports whether the reduction collects from the customer.
func (r reduction) payer() bool { return r.cash.IsNegative() }

// planReduction plans the legs of closing eff.Closed contracts of pos: the
// released share of margin first, then the cash. A payment is an obligation:
// it is taken from the wallet (which now holds the released margin), then from
// the margin the position keeps; what neither covers is a shortfall the
// clearing house absorbs and both the register and the position record. An
// option's cash leg also recognizes the closed contracts' premium.
func (s *Service) planReduction(ctx context.Context, tx pgx.Tx, ch econ.Facility, spec Spec, pos Position, eff fillEffect, k reductionKeys) (reduction, error) {
	acct := pos.AccountID
	asset := spec.SettlementAssetID
	after := pos
	after.Quantity, after.CostValue, after.SettledValue = eff.After.Quantity, eff.After.CostValue, eff.After.SettledValue
	after.MarginPosted = eff.After.MarginPosted
	after.RealizedPnL = pos.RealizedPnL.Add(eff.RealizedPnL)
	r := reduction{cash: eff.Cash}
	if eff.MarginRelease.IsPositive() {
		r.legs = append(r.legs, leg{
			name: "margin release", key: k.prefix + ":margin-release", effectKind: EffectMarginRelease,
			kind: ledger.KindDerivativeMarginMove, account: &acct, intent: k.intent, reference: k.reference,
			entries: marginMoveEntries(acct, asset, eff.MarginRelease, false),
		})
	}
	var amount money.Quantity
	if eff.Cash.IsNegative() {
		wallet, err := s.ledger.Balance(ctx, tx, ledger.CustomerAccount(acct, ledger.CodeWallet, asset))
		if err != nil {
			return reduction{}, err
		}
		r.pay = splitPayment(eff.Cash.Neg(), wallet.Add(eff.MarginRelease), after.MarginPosted)
		after.MarginPosted = after.MarginPosted.Sub(r.pay.FromMargin)
		after.Shortfall = pos.Shortfall.Add(r.pay.Shortfall)
		amount = r.pay.collected().Neg()
	} else {
		amount = eff.Cash
	}
	entries := cashEntries(acct, ch, asset, eff.Cash, r.pay)
	if spec.IsOption() {
		entries = append(entries, recognizePremiumEntries(acct, asset, eff.AllocatedCost)...)
	}
	if len(entries) > 0 || !r.pay.Shortfall.IsZero() {
		r.legs = append(r.legs, leg{
			name: strings.ToLower(string(k.cashKind)), key: k.prefix + ":cash", effectKind: k.cashEffect, kind: k.cashJournal,
			account: &acct, intent: k.intent, reference: k.reference, entries: entries,
			flow: s.flowFor(pos, spec, k.cashKind, amount, r.pay.Shortfall, k.prefix+":cash", k.reference),
		})
	}
	r.after = after
	return r, nil
}
