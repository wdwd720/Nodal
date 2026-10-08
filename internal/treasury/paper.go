package treasury

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/orders"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// The house paper venue: external simulated liquidity for the house book.
//
// A paper fill executes at once, in full, against a fresh reference mark of
// the asset in the valuation asset, through the fee schedule the risk policy
// defines (a disclosed spread and an explicit venue fee). The quote and its
// disclosure are verified (fees.VerifyQuote, fees.VerifyDisclosure) before
// anything is posted. The posting is a SIM_TRADE of the house book alone --
// the venue's side is external and not booked -- with the spread and the fee
// on HOUSE_FEES, and the whole schedule and disclosure stored with the fill,
// so every unit the house paid is recomputable from the record.

// PaperRequest is an immediate fill of the house book on the paper venue:
// a buy or sell of Asset against the book's valuation asset.
type PaperRequest struct {
	HouseAccountID econ.HouseAccountID
	Activity       econ.HouseStrategy
	Asset          assets.AssetID
	Side           orders.Side
	// Quantity is in base units of Asset.
	Quantity money.Quantity
	// Mode is PAPER (default) or SANDBOX.
	Mode           gates.Mode
	IdempotencyKey string
	CorrelationID  string
	Actor          econ.Actor
}

// PaperFill is a stored house paper-venue fill.
type PaperFill struct {
	ID                   PaperFillID
	HouseAccountID       econ.HouseAccountID
	Activity             econ.HouseStrategy
	VenueLabel           string
	Side                 orders.Side
	BaseAsset            assets.AssetID
	QuoteAsset           assets.AssetID
	BaseQuantity         money.Quantity
	ReferencePrice       money.Price
	NotionalAtReference  money.Quantity
	SpreadBPS            money.BPS
	SpreadAmount         money.Quantity
	FeeAmount            money.Quantity
	TotalQuote           money.Quantity
	Schedule             []byte
	ScheduleHash         string
	Disclosure           fees.Disclosure
	DisclosureBytes      []byte
	DisclosureHash       string
	EffectID             econ.EffectID
	JournalTransactionID ledger.TransactionID
	EvidenceRef          string
	EvidenceSHA256       []byte
	FilledAt             time.Time
}

// PaperOutcome is the result of a paper request: the recorded action, the
// fill when it was made, the risk decision, and a recorded refusal.
type PaperOutcome struct {
	Action    Action
	Fill      *PaperFill
	Decision  *RiskDecision
	Rejection error
}

// Hedge fills on the paper venue under the HEDGING activity.
func (s *Service) Hedge(ctx context.Context, tx pgx.Tx, r PaperRequest) (PaperOutcome, error) {
	r.Activity = econ.HouseHedging
	return s.ExecutePaper(ctx, tx, r)
}

// ConvertRequest converts between the valuation asset and another sandbox
// asset at a mark with a disclosed spread. Quantity is the amount of the
// non-valuation asset bought (when To is it) or sold (when From is it).
type ConvertRequest struct {
	HouseAccountID econ.HouseAccountID
	From           assets.AssetID
	To             assets.AssetID
	Quantity       money.Quantity
	Mode           gates.Mode
	IdempotencyKey string
	CorrelationID  string
	Actor          econ.Actor
}

// Convert is a treasury conversion: a paper fill under TREASURY_CONVERSION.
func (s *Service) Convert(ctx context.Context, tx pgx.Tx, r ConvertRequest) (PaperOutcome, error) {
	book, err := s.GetBook(ctx, tx, r.HouseAccountID)
	if err != nil {
		return PaperOutcome{}, err
	}
	p := PaperRequest{
		HouseAccountID: r.HouseAccountID, Activity: econ.HouseTreasuryConversion, Quantity: r.Quantity, Mode: r.Mode,
		IdempotencyKey: r.IdempotencyKey, CorrelationID: r.CorrelationID, Actor: r.Actor,
	}
	switch {
	case r.From == book.ValuationAsset && r.To != book.ValuationAsset:
		p.Asset, p.Side = r.To, orders.SideBuy
	case r.To == book.ValuationAsset && r.From != book.ValuationAsset:
		p.Asset, p.Side = r.From, orders.SideSell
	default:
		return PaperOutcome{}, rejection(errs.CodeValidationFailed, ReasonValuationAsset,
			"a conversion is between the book's valuation asset and one other asset")
	}
	return s.ExecutePaper(ctx, tx, p)
}

// RebalanceRequest moves the house book's holding of Asset to Target on the
// paper venue.
type RebalanceRequest struct {
	HouseAccountID econ.HouseAccountID
	Asset          assets.AssetID
	Target         money.Quantity
	Mode           gates.Mode
	IdempotencyKey string
	CorrelationID  string
	Actor          econ.Actor
}

// Rebalance computes the trade that takes the book's holding of an asset to a
// target and fills it on the paper venue under INVENTORY_REBALANCING: a sale
// of the excess, or a buy of the shortfall (which the limits bind). A holding
// already at its target needs no fill and records nothing (NoOp).
func (s *Service) Rebalance(ctx context.Context, tx pgx.Tx, r RebalanceRequest) (PaperOutcome, bool, error) {
	if r.Target.IsNegative() {
		return PaperOutcome{}, false, errs.New(errs.CodeValidationFailed, "treasury: a rebalancing target must not be negative")
	}
	book, err := s.lockBook(ctx, tx, r.HouseAccountID)
	if err != nil {
		return PaperOutcome{}, false, err
	}
	if prior, found, err := s.actionByKey(ctx, tx, book.HouseAccountID, r.IdempotencyKey); err != nil {
		return PaperOutcome{}, false, err
	} else if found {
		out, err := s.replayPaper(ctx, tx, prior)
		return out, false, err
	}
	if _, err := s.syncLocked(ctx, tx, book); err != nil {
		return PaperOutcome{}, false, err
	}
	held, err := s.ledger.Balance(ctx, tx, book.account(ledger.CodeHouseInventory, r.Asset))
	if err != nil {
		return PaperOutcome{}, false, err
	}
	p := PaperRequest{
		HouseAccountID: r.HouseAccountID, Activity: econ.HouseInventoryRebalancing, Asset: r.Asset, Mode: r.Mode,
		IdempotencyKey: r.IdempotencyKey, CorrelationID: r.CorrelationID, Actor: r.Actor,
	}
	switch held.Cmp(r.Target) {
	case 0:
		return PaperOutcome{}, true, nil
	case 1:
		p.Side, p.Quantity = orders.SideSell, held.Sub(r.Target)
	default:
		p.Side, p.Quantity = orders.SideBuy, r.Target.Sub(held)
	}
	out, err := s.ExecutePaper(ctx, tx, p)
	return out, false, err
}

// ExecutePaper fills a house paper request: authority, capability, the
// treasury limits and Risk Kernel V2 under the book's lock, then an
// exactly-once effect posting the house book's SIM_TRADE with its disclosed
// spread and fee.
func (s *Service) ExecutePaper(ctx context.Context, tx pgx.Tx, r PaperRequest) (PaperOutcome, error) {
	if err := validKey(r.IdempotencyKey); err != nil {
		return PaperOutcome{}, err
	}
	if !r.Side.Valid() || !r.Quantity.IsPositive() || r.Asset.IsZero() {
		return PaperOutcome{}, errs.New(errs.CodeValidationFailed, "treasury: a paper fill names an asset, a side and a positive quantity")
	}
	mode := r.Mode
	if mode == "" {
		mode = gates.ModePaper
	}
	if mode != gates.ModePaper && mode != gates.ModeSandbox {
		return PaperOutcome{}, errs.Newf(errs.CodeValidationFailed, "treasury: the house paper venue runs in PAPER or SANDBOX, not %q", mode)
	}
	book, err := s.lockBook(ctx, tx, r.HouseAccountID)
	if err != nil {
		return PaperOutcome{}, err
	}
	sp, err := s.currentSigningPolicy(ctx, tx, book.HouseAccountID)
	if err != nil {
		return PaperOutcome{}, err
	}
	if err := sp.Authorize(r.Actor, ActionPaperFill); err != nil {
		return PaperOutcome{}, err
	}
	reqHash, err := econ.RequestHash(map[string]any{
		"activity": string(r.Activity), "asset": r.Asset.String(), "side": string(r.Side), "quantity": r.Quantity.String(),
		"mode": string(mode),
	})
	if err != nil {
		return PaperOutcome{}, err
	}
	if prior, found, err := s.actionByKey(ctx, tx, book.HouseAccountID, r.IdempotencyKey); err != nil {
		return PaperOutcome{}, err
	} else if found {
		if err := sameRequest(prior, ActionPaperFill, reqHash); err != nil {
			return PaperOutcome{}, err
		}
		return s.replayPaper(ctx, tx, prior)
	}
	now := s.clk.Now().UTC()
	a := Action{
		ID: id.New[actionKind](), HouseAccountID: book.HouseAccountID, Kind: ActionPaperFill, Activity: r.Activity,
		Actor: r.Actor, SigningPolicyVersion: sp.Version, IdempotencyKey: r.IdempotencyKey, RequestHash: reqHash,
		CorrelationID: r.CorrelationID, OccurredAt: now,
	}
	reject := func(code string, reasons []string, decision *RiskDecision, detail map[string]any) (PaperOutcome, error) {
		a.Outcome, a.RejectionCode, a.ReasonCodes = OutcomeRejected, code, reasons
		if err := s.recordAction(ctx, tx, book, &a, "paper fill refused", detail); err != nil {
			return PaperOutcome{}, err
		}
		return PaperOutcome{Action: a, Decision: decision, Rejection: a.Rejection()}, nil
	}
	if !permitted(book.Strategy, r.Activity) {
		return reject(ReasonActivityNotPermitted, []string{ReasonActivityNotPermitted}, nil,
			map[string]any{"strategy": string(book.Strategy)})
	}
	if err := s.requireCapability(ctx, tx, gates.PrincipalTrading, mode); err != nil {
		return reject(string(errs.CodeOf(err)), []string{ReasonOf(err)}, nil, map[string]any{"capability": string(gates.PrincipalTrading)})
	}
	pv, found, err := s.currentPolicy(ctx, tx, book.HouseAccountID)
	if err != nil {
		return PaperOutcome{}, err
	}
	if !found {
		return reject(ReasonPolicyMissing, []string{ReasonPolicyMissing}, nil, nil)
	}
	a.RiskPolicyVersion = pv.Version
	if r.Asset == book.ValuationAsset {
		return PaperOutcome{}, rejection(errs.CodeValidationFailed, ReasonValuationAsset,
			"the valuation asset is cash; a paper fill buys or sells another asset for it")
	}
	base, err := s.assets.Get(ctx, tx, r.Asset)
	if err != nil {
		return PaperOutcome{}, err
	}
	if base.Kind != assets.KindSimulated || base.ValueDomain != valuedomain.Simulated {
		return PaperOutcome{}, errs.New(errs.CodeForbidden, "treasury: the house paper venue moves only SIMULATED assets")
	}
	quote, err := s.assets.Get(ctx, tx, book.ValuationAsset)
	if err != nil {
		return PaperOutcome{}, err
	}
	if _, err := s.syncLocked(ctx, tx, book); err != nil {
		return PaperOutcome{}, err
	}
	mark, err := s.markOf(ctx, tx, r.Asset, book.ValuationAsset, pv.Policy.MaxMarkAge, now)
	if err != nil {
		return PaperOutcome{}, err
	}
	if mark == nil {
		return reject(ReasonMarkUnavailable, []string{ReasonMarkUnavailable}, nil, map[string]any{"asset": r.Asset.String()})
	}
	schedule := pv.paperSchedule(quote.Decimals)
	input := fees.QuoteInput{
		Side: fees.Side(r.Side), BaseQty: r.Quantity, BaseDecimals: base.Decimals, QuoteDecimals: quote.Decimals,
		ReferencePrice: *mark,
	}
	q, err := schedule.Quote(input)
	if err != nil {
		return PaperOutcome{}, err
	}
	if err := fees.VerifyQuote(schedule, input, q); err != nil {
		return PaperOutcome{}, errs.Wrap(err, errs.CodeInternal, "treasury: a paper quote does not verify")
	}
	disclosure := q.Disclosure()
	if err := fees.VerifyDisclosure(schedule, disclosure); err != nil {
		return PaperOutcome{}, errs.Wrap(err, errs.CodeInternal, "treasury: a paper disclosure does not verify")
	}
	if !q.Network.Amount.IsZero() || !q.Fees.Fixed.Charged.IsZero() || !q.Fees.Strategy.Charged.IsZero() ||
		!q.Fees.Percentage.Charged.IsZero() || !q.Fees.MinimumTopUp.IsZero() {
		return PaperOutcome{}, errs.New(errs.CodeInternal, "treasury: the house paper schedule charged a component it does not define")
	}
	// Serialize with the order book's reservations of this house book, then
	// check what is free to pay with.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM house_accounts WHERE id = $1 FOR NO KEY UPDATE`, book.HouseAccountID); err != nil {
		return PaperOutcome{}, dbErr("lock house book", err)
	}
	snap, err := s.snapshotOf(ctx, tx, book, r.Asset, nil)
	if err != nil {
		return PaperOutcome{}, err
	}
	pos, err := positionOf(ctx, tx, book.HouseAccountID, r.Asset)
	if err != nil {
		return PaperOutcome{}, err
	}
	riskIncreasing := r.Side == orders.SideBuy
	total := q.Total()
	var available, required money.Quantity
	if riskIncreasing {
		available, required = snap.heldQuote.Sub(snap.reservedQuote), total
	} else {
		// A sale needs the inventory free of book reservations AND recorded
		// in the position register (the register is what its PnL comes from).
		available = snap.heldBase.Sub(snap.reservedBase).Min(pos.Quantity)
		required = r.Quantity
	}
	if available.Cmp(required) < 0 {
		return reject(ReasonInsufficientInventory, []string{ReasonInsufficientInventory}, nil, map[string]any{
			"available": available.String(), "required": required.String(),
		})
	}
	rep, err := s.report(ctx, tx, book, pv.Policy.MaxMarkAge, now)
	if err != nil {
		return PaperOutcome{}, err
	}
	loss, err := rep.lossState()
	if err != nil {
		return PaperOutcome{}, err
	}
	heldValue, err := money.Notional(snap.heldBase, base.Decimals, *mark, quote.Decimals, ExposureRounding)
	if err != nil {
		return PaperOutcome{}, errs.Wrap(err, errs.CodeInternal, "treasury: held value")
	}
	limits := EvaluateLimits(pv.Policy, LimitRequest{
		RiskIncreasing: riskIncreasing, Asset: r.Asset, Quantity: r.Quantity, Notional: total,
		Exposure: Exposure{Held: snap.heldBase, WorkingBuys: snap.workingBuys, HeldValue: &heldValue}, Loss: loss,
	})
	notionalUSD, err := usd(q.NotionalAtReference, quote.Decimals, money.RoundUp)
	if err != nil {
		return PaperOutcome{}, err
	}
	availableValue := available
	requiredValue := required
	if !riskIncreasing {
		if availableValue, err = money.Notional(available, base.Decimals, *mark, quote.Decimals, money.RoundDown); err != nil {
			return PaperOutcome{}, errs.Wrap(err, errs.CodeInternal, "treasury: available value")
		}
		requiredValue = q.NotionalAtReference
	}
	availableUSD, err := usd(availableValue, quote.Decimals, money.RoundDown)
	if err != nil {
		return PaperOutcome{}, err
	}
	requiredUSD, err := usd(requiredValue, quote.Decimals, money.RoundUp)
	if err != nil {
		return PaperOutcome{}, err
	}
	dec, err := s.evaluateRisk(ctx, tx, riskRequest{
		book: book, policy: pv, decisionFor: a.ID, intentKind: econ.KindTakeLiquidity, actor: r.Actor, mode: mode,
		instrumentKind: "PAPER_VENUE", instrumentSnap: risk.InstrumentSnapshot{Status: "ACTIVE", SessionOpen: true},
		asset: r.Asset, riskIncreasing: riskIncreasing, notionalUSD: notionalUSD,
		balance: risk.BalanceSnapshot{Available: availableUSD, Required: requiredUSD}, mark: mark, limits: limits,
	})
	if err != nil {
		return PaperOutcome{}, err
	}
	a.RiskDecisionID = &dec.ID
	if dec.Verdict == risk.Reject {
		return reject(string(errs.CodeRiskRejected), append(append([]string{}, dec.ReasonCodes...), dec.TreasuryCodes...), &dec,
			map[string]any{"decision_hash": dec.DecisionHash, "limits": limits})
	}
	fill, err := s.postPaperFill(ctx, tx, book, r, mode, base, quote, *mark, schedule, q, disclosure, now)
	if err != nil {
		return PaperOutcome{}, err
	}
	var m Movement
	if riskIncreasing {
		m, err = pos.Increase(r.Quantity, q.NotionalAtReference, fill.SpreadAmount.Add(fill.FeeAmount))
	} else {
		m, err = pos.Decrease(r.Quantity, q.NotionalAtReference, fill.SpreadAmount.Add(fill.FeeAmount))
	}
	if err != nil {
		return PaperOutcome{}, err
	}
	fid := fill.ID
	if _, err := insertEvent(ctx, tx, book.HouseAccountID, EventPaperFill, r.Asset, m,
		eventSource{paperFill: &fid, journal: fill.JournalTransactionID}, now); err != nil {
		return PaperOutcome{}, err
	}
	a.Outcome, a.PaperFillID, a.EffectID = OutcomeApplied, &fid, &fill.EffectID
	if err := s.recordAction(ctx, tx, book, &a, "paper fill", map[string]any{
		"paper_fill_id": fid.String(), "disclosure_hash": fill.DisclosureHash, "schedule_hash": fill.ScheduleHash,
		"spread": fill.SpreadAmount.String(), "fee": fill.FeeAmount.String(), "total": fill.TotalQuote.String(),
		"decision_hash": dec.DecisionHash, "realized_pnl": m.Realized.String(),
	}); err != nil {
		return PaperOutcome{}, err
	}
	return PaperOutcome{Action: a, Fill: &fill, Decision: &dec}, nil
}

// postPaperFill claims the fill's effect, posts the house book's SIM_TRADE,
// archives the evidence, applies the effect and stores the fill (whose row the
// database checks against the journal and the effect, TY004).
func (s *Service) postPaperFill(ctx context.Context, tx pgx.Tx, book Book, r PaperRequest, mode gates.Mode, base, quote assets.Asset,
	mark money.Price, schedule fees.Schedule, q fees.Quote, d fees.Disclosure, now time.Time) (PaperFill, error) {
	scheduleBytes, err := schedule.Canonical()
	if err != nil {
		return PaperFill{}, err
	}
	scheduleHash, err := schedule.Hash()
	if err != nil {
		return PaperFill{}, err
	}
	discBytes, err := d.Canonical()
	if err != nil {
		return PaperFill{}, err
	}
	discHash, err := d.Hash()
	if err != nil {
		return PaperFill{}, err
	}
	f := PaperFill{
		ID: id.New[paperFillKind](), HouseAccountID: book.HouseAccountID, Activity: r.Activity, VenueLabel: VenueLabelPaper,
		Side: r.Side, BaseAsset: base.ID, QuoteAsset: quote.ID, BaseQuantity: r.Quantity, ReferencePrice: mark,
		NotionalAtReference: q.NotionalAtReference, SpreadBPS: q.Spread.BPS, SpreadAmount: q.Spread.Amount,
		FeeAmount: q.Fees.Total, TotalQuote: q.Total(), Schedule: scheduleBytes, ScheduleHash: scheduleHash, Disclosure: d,
		DisclosureBytes: discBytes, DisclosureHash: discHash, FilledAt: now,
	}
	key := "treasury:paper:" + book.HouseAccountID.String() + ":" + r.IdempotencyKey
	eff, existed, err := s.effects.Begin(ctx, tx, econ.BeginRequest{
		Key: key, Subsystem: econ.SubsystemTreasury, Kind: "HOUSE_PAPER_FILL",
		Request: map[string]any{"house_account_id": book.HouseAccountID.String(), "disclosure_hash": discHash,
			"schedule_hash": scheduleHash, "mode": string(mode)},
		PostsLedger: true, Actor: r.Actor, CorrelationID: r.CorrelationID,
	})
	if err != nil {
		return PaperFill{}, err
	}
	if existed {
		return PaperFill{}, errs.New(errs.CodeConflict, "treasury: this paper fill's effect was already claimed").
			WithField("effect_id", eff.ID.String())
	}
	posted, err := s.ledger.Post(postingContext(ctx, r.Actor), tx, ledger.Posting{
		Kind: ledger.KindSimTrade, IdempotencyKey: key,
		Reference:   ledger.FinancialEventReference{Type: "treasury_paper_fill", ID: f.ID.String()},
		EffectiveAt: now, Description: "simulated house paper-venue fill at a reference mark with a disclosed spread",
		CorrelationID: r.CorrelationID, Entries: paperEntries(book, f),
		Metadata: map[string]any{
			"venue_label": VenueLabelPaper, "ledger_segment": LedgerSegment, "house_account_id": book.HouseAccountID.String(),
			"activity": string(r.Activity), "disclosure_hash": discHash, "schedule_hash": scheduleHash,
			"spread": f.SpreadAmount.String(), "fee": f.FeeAmount.String(),
		},
	})
	if err != nil {
		return PaperFill{}, err
	}
	f.JournalTransactionID = posted.TransactionID
	var discDoc any
	if err := json.Unmarshal(discBytes, &discDoc); err != nil {
		return PaperFill{}, errs.Wrap(err, errs.CodeInternal, "treasury: re-read disclosure")
	}
	ev, err := econ.RecordEvidence(ctx, tx, econ.Evidence{
		Key: "treasury/" + book.HouseAccountID.String() + "/paper/" + f.ID.String(),
		Document: map[string]any{
			"kind": "treasury.paper_fill.v1", "venue_label": VenueLabelPaper, "house_account_id": book.HouseAccountID.String(),
			"activity": string(r.Activity), "side": string(r.Side), "schedule_hash": scheduleHash, "disclosure_hash": discHash,
			"disclosure": discDoc, "journal_transaction_id": posted.TransactionID.String(), "audit_tags": auditTags(book),
		},
	})
	if err != nil {
		return PaperFill{}, err
	}
	f.EvidenceRef, f.EvidenceSHA256 = ev.URI, ev.SHA256
	applied, err := s.effects.Apply(ctx, tx, eff.ID, econ.ApplyRequest{
		JournalTransactionID: &posted.TransactionID, Evidence: &ev, Actor: r.Actor, Reason: "house paper fill posted",
	})
	if err != nil {
		return PaperFill{}, err
	}
	f.EffectID = applied.ID
	if _, err := tx.Exec(ctx, `INSERT INTO treasury_paper_fills (id, house_account_id, activity, side, base_asset_id,
		quote_asset_id, base_quantity, reference_mantissa, reference_scale, mark_observed_at, notional_at_reference, spread_bps,
		spread_amount, fee_amount, total_quote, schedule, schedule_hash, disclosure, disclosure_hash, idempotency_key, effect_id,
		journal_transaction_id, evidence_ref, evidence_sha256, filled_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7::numeric,$8::numeric,$9,$10,$11::numeric,$12,$13::numeric,$14::numeric,$15::numeric,$16,$17,
		        $18,$19,$20,$21,$22,$23,$24,$25)`,
		f.ID, book.HouseAccountID, r.Activity, r.Side, base.ID, quote.ID, f.BaseQuantity.String(), mark.Mantissa.String(),
		mark.Scale, mark.At.UTC(), f.NotionalAtReference.String(), int(f.SpreadBPS), f.SpreadAmount.String(),
		f.FeeAmount.String(), f.TotalQuote.String(), scheduleBytes, scheduleHash, discBytes, discHash, r.IdempotencyKey,
		applied.ID, posted.TransactionID, ev.URI, ev.SHA256, now); err != nil {
		return PaperFill{}, dbErr("record paper fill", err)
	}
	return f, nil
}

// paperEntries is the house book's side of a paper fill: a SIM_TRADE with
// the HOUSE_* codes at the reference notional, and the spread and the venue
// fee each on HOUSE_FEES -- every entry an account of this house book.
//
//	BUY:  quote Cr HOUSE_INVENTORY total / Dr HOUSE_TRADING_OUTFLOW notional, Dr HOUSE_FEES spread, Dr HOUSE_FEES fee
//	      base  Dr HOUSE_INVENTORY qty   / Cr HOUSE_TRADING_INFLOW qty
//	SELL: base  Cr HOUSE_INVENTORY qty   / Dr HOUSE_TRADING_OUTFLOW qty
//	      quote Dr HOUSE_INVENTORY total, Dr HOUSE_FEES spread, Dr HOUSE_FEES fee / Cr HOUSE_TRADING_INFLOW notional
func paperEntries(book Book, f PaperFill) []ledger.Entry {
	inv := func(a assets.AssetID) ledger.AccountRef { return book.account(ledger.CodeHouseInventory, a) }
	var out []ledger.Entry
	if f.Side == orders.SideBuy {
		out = append(out,
			ledger.Entry{Account: inv(f.QuoteAsset), Side: ledger.Credit, Quantity: f.TotalQuote},
			ledger.Entry{Account: book.account(ledger.CodeHouseTradingOutflow, f.QuoteAsset), Side: ledger.Debit, Quantity: f.NotionalAtReference},
			ledger.Entry{Account: inv(f.BaseAsset), Side: ledger.Debit, Quantity: f.BaseQuantity},
			ledger.Entry{Account: book.account(ledger.CodeHouseTradingInflow, f.BaseAsset), Side: ledger.Credit, Quantity: f.BaseQuantity})
	} else {
		out = append(out,
			ledger.Entry{Account: inv(f.BaseAsset), Side: ledger.Credit, Quantity: f.BaseQuantity},
			ledger.Entry{Account: book.account(ledger.CodeHouseTradingOutflow, f.BaseAsset), Side: ledger.Debit, Quantity: f.BaseQuantity},
			ledger.Entry{Account: book.account(ledger.CodeHouseTradingInflow, f.QuoteAsset), Side: ledger.Credit, Quantity: f.NotionalAtReference})
		if f.TotalQuote.IsPositive() {
			out = append(out, ledger.Entry{Account: inv(f.QuoteAsset), Side: ledger.Debit, Quantity: f.TotalQuote})
		}
	}
	for _, c := range []money.Quantity{f.SpreadAmount, f.FeeAmount} {
		if c.IsPositive() {
			out = append(out, ledger.Entry{Account: book.account(ledger.CodeHouseFees, f.QuoteAsset), Side: ledger.Debit, Quantity: c})
		}
	}
	// A BUY whose notional is zero (dust at a tiny mark) has nothing to
	// move out of HOUSE_TRADING_OUTFLOW; a zero entry is not an entry.
	clean := out[:0]
	for _, e := range out {
		if e.Quantity.IsPositive() {
			clean = append(clean, e)
		}
	}
	return clean
}

const paperColumns = `id, house_account_id, activity, side, base_asset_id, quote_asset_id, base_quantity::text, reference_mantissa::text,
	reference_scale, mark_observed_at, notional_at_reference::text, spread_bps, spread_amount::text, fee_amount::text,
	total_quote::text, schedule, schedule_hash, disclosure, disclosure_hash, effect_id, journal_transaction_id, evidence_ref,
	evidence_sha256, filled_at`

// Fill returns a stored paper fill.
func (s *Service) Fill(ctx context.Context, q db.Querier, fid PaperFillID) (PaperFill, error) {
	var f PaperFill
	var v [6]string
	var spread int
	err := q.QueryRow(ctx, `SELECT `+paperColumns+` FROM treasury_paper_fills WHERE id = $1`, fid).Scan(&f.ID, &f.HouseAccountID,
		&f.Activity, &f.Side, &f.BaseAsset, &f.QuoteAsset, &v[0], &v[1], &f.ReferencePrice.Scale, &f.ReferencePrice.At, &v[2],
		&spread, &v[3], &v[4], &v[5], &f.Schedule, &f.ScheduleHash, &f.DisclosureBytes, &f.DisclosureHash, &f.EffectID,
		&f.JournalTransactionID, &f.EvidenceRef, &f.EvidenceSHA256, &f.FilledAt)
	if err != nil {
		return PaperFill{}, notFound(err, "paper fill", fid.String())
	}
	for i, dst := range []*money.Quantity{&f.BaseQuantity, &f.ReferencePrice.Mantissa, &f.NotionalAtReference, &f.SpreadAmount,
		&f.FeeAmount, &f.TotalQuote} {
		x, err := money.ScanQuantity(v[i])
		if err != nil {
			return PaperFill{}, errs.Wrap(err, errs.CodeInternal, "treasury: stored paper fill amounts")
		}
		*dst = x
	}
	f.SpreadBPS = money.BPS(spread)
	f.VenueLabel = VenueLabelPaper
	f.ReferencePrice.QuoteAsset = f.QuoteAsset.String()
	f.ReferencePrice.At = f.ReferencePrice.At.UTC()
	f.FilledAt = f.FilledAt.UTC()
	if f.Disclosure, err = decodeDisclosure(f.DisclosureBytes, f.DisclosureHash); err != nil {
		return PaperFill{}, err
	}
	return f, nil
}

// decodeDisclosure decodes a stored paper fill's disclosure and proves it is
// the canonical form its recorded hash names, so the disclosure returned is
// the one the fill was priced and hashed with.
func decodeDisclosure(body []byte, hash string) (fees.Disclosure, error) {
	var d fees.Disclosure
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return fees.Disclosure{}, errs.Wrap(err, errs.CodeInternal, "treasury: stored disclosure is unreadable")
	}
	again, err := d.Canonical()
	if err != nil {
		return fees.Disclosure{}, err
	}
	if !bytes.Equal(again, body) || hashHex(again) != hash {
		return fees.Disclosure{}, errs.New(errs.CodeInternal, "treasury: stored disclosure is not the canonical form its hash names")
	}
	return d, nil
}

func (s *Service) replayPaper(ctx context.Context, tx pgx.Tx, prior Action) (PaperOutcome, error) {
	out := PaperOutcome{Action: prior, Rejection: prior.Rejection()}
	if prior.Kind != ActionPaperFill {
		return PaperOutcome{}, errs.New(errs.CodeInvalidIdempotencyReuse, "treasury: this idempotency key was used for a different house action")
	}
	if prior.PaperFillID != nil {
		f, err := s.Fill(ctx, tx, *prior.PaperFillID)
		if err != nil {
			return PaperOutcome{}, err
		}
		out.Fill = &f
	}
	if prior.RiskDecisionID != nil {
		d, err := s.Decision(ctx, tx, *prior.RiskDecisionID)
		if err != nil {
			return PaperOutcome{}, err
		}
		out.Decision = &d
	}
	return out, nil
}
