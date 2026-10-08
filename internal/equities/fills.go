package equities

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

type tradeRequest struct {
	OrderID        string         `json:"order_id"`
	Venue          string         `json:"venue"`
	VenueFillID    string         `json:"venue_fill_id"`
	Quantity       money.Quantity `json:"quantity"`
	Price          money.Quantity `json:"price"`
	PriceScale     int32          `json:"price_scale"`
	ExecutedAt     time.Time      `json:"executed_at"`
	DisclosureHash string         `json:"fee_disclosure_hash"`
}

type fillEvidence struct {
	Kind                 string         `json:"kind"`
	OrderID              string         `json:"order_id"`
	FillID               string         `json:"fill_id"`
	IntentID             string         `json:"intent_id"`
	AccountID            string         `json:"account_id"`
	InstrumentID         string         `json:"instrument_id"`
	Side                 Side           `json:"side"`
	OrderType            OrderType      `json:"order_type"`
	Venue                string         `json:"venue"`
	VenueFillID          string         `json:"venue_fill_id"`
	Quantity             money.Quantity `json:"quantity"`
	Price                money.Quantity `json:"price"`
	PriceScale           int32          `json:"price_scale"`
	Mark                 Mark           `json:"mark"`
	SlippageBPS          money.BPS      `json:"slippage_bps"`
	ExecutedAt           time.Time      `json:"executed_at"`
	TradeDate            string         `json:"trade_date"`
	SettlementDate       string         `json:"settlement_date"`
	RiskDecisionID       string         `json:"risk_decision_id"`
	Notional             money.Quantity `json:"notional"`
	Spread               money.Quantity `json:"spread"`
	PlatformFees         money.Quantity `json:"platform_fees"`
	VenueFees            money.Quantity `json:"venue_fees"`
	Total                money.Quantity `json:"total"`
	FeeScheduleHash      string         `json:"fee_schedule_hash"`
	FeeDisclosureHash    string         `json:"fee_disclosure_hash"`
	JournalTransactionID string         `json:"journal_transaction_id"`
}

// checkFill refuses a venue fill that contradicts the order it is for. The
// database checks the same rules again when the fill is recorded (EQ003).
func checkFill(o Order, f BrokerFill) error {
	var p []string
	if !f.Quantity.Equal(o.Quantity) {
		p = append(p, "the venue fills an order in full: fill quantity must equal the order quantity")
	}
	if f.PriceScale != o.PriceScale || !onGrid(f.Price, o.TickSize) {
		p = append(p, "the fill price must be on the order's tick grid")
	}
	bound := o.LimitPrice
	if bound == nil {
		bound = o.ProtectionPrice
	}
	if bound != nil && ((o.Side == SideBuy && f.Price.Cmp(*bound) > 0) || (o.Side == SideSell && f.Price.Cmp(*bound) < 0)) {
		p = append(p, "the fill goes through the order's limit or protection price")
	}
	if f.ExecutedAt.Before(o.CreatedAt) || !f.ExecutedAt.Before(o.ExpiresAt) {
		p = append(p, "the fill was executed outside the order's life")
	}
	if len(p) > 0 {
		return errs.New(errs.CodeValidationFailed, "equities: the venue's fill contradicts the order").
			WithField("order_id", o.ID.String()).WithField("problems", p)
	}
	return nil
}

func (s *Service) applyFill(ctx context.Context, tx pgx.Tx, o Order, r OrderReport, now time.Time) (Order, error) {
	vf := *r.Fill
	vf.ExecutedAt = vf.ExecutedAt.UTC()
	if err := checkFill(o, vf); err != nil {
		return Order{}, err
	}
	if o.ReservationID == nil || o.ReservedQuantity == nil {
		return Order{}, errs.New(errs.CodeInternal, "equities: an accepted order has no reservation")
	}
	if err := lockInstrument(ctx, tx, o.InstrumentID, false); err != nil {
		return Order{}, err
	}
	l, err := s.LoadListing(ctx, tx, o.InstrumentID)
	if err != nil {
		return Order{}, err
	}
	cal, err := s.Calendar(ctx, tx, l.Terms.Terms.TradingCalendar)
	if err != nil {
		return Order{}, err
	}
	tradeDate := cal.TradeDate(vf.ExecutedAt)
	settleDate, err := cal.SettlementDate(tradeDate)
	if err != nil {
		return Order{}, err
	}
	in, err := s.intents.Get(ctx, tx, o.IntentID)
	if err != nil {
		return Order{}, err
	}

	// Price the execution under the schedule and prove the quote before
	// anything is posted.
	sch, ok := s.schedules[l.CashAsset.ID]
	if !ok {
		return Order{}, errs.New(errs.CodeInternal, "equities: no fee schedule covers this settlement asset")
	}
	ref, err := money.NewPrice(vf.Price, vf.PriceScale, l.CashAsset.ID.String(), "venue:"+o.Broker, vf.ExecutedAt)
	if err != nil {
		return Order{}, errs.Wrap(err, errs.CodeValidationFailed, "equities: fill price")
	}
	qin := fees.QuoteInput{Side: feeSide(o.Side), BaseQty: vf.Quantity, BaseDecimals: o.QuantityDecimals,
		QuoteDecimals: l.CashAsset.Decimals, ReferencePrice: ref, StrategyOriginated: in.StrategyRef != ""}
	quote, err := sch.Quote(qin)
	if err != nil {
		return Order{}, err
	}
	if err := fees.VerifyQuote(sch, qin, quote); err != nil {
		return Order{}, errs.Wrap(err, errs.CodeInternal, "equities: the fee quote does not verify; nothing is posted")
	}
	charges, err := ChargesOf(quote)
	if err != nil {
		return Order{}, err
	}
	consume := vf.Quantity
	if o.Side == SideBuy {
		consume = charges.Total
	}
	if consume.Cmp(*o.ReservedQuantity) > 0 {
		return Order{}, errs.New(errs.CodeInsufficientBuyingPower,
			"equities: the fill costs more than the order reserved; it is not booked and needs reconciliation").
			WithField("order_id", o.ID.String()).WithField("reserved", o.ReservedQuantity.String()).WithField("cost", consume.String())
	}
	disclosure := quote.Disclosure()
	discBytes, err := disclosure.Canonical()
	if err != nil {
		return Order{}, err
	}
	discHash, err := disclosure.Hash()
	if err != nil {
		return Order{}, err
	}
	schedBytes, err := EncodeSchedule(sch)
	if err != nil {
		return Order{}, err
	}
	entries, err := TradeEntries(o.AccountID, o.Side, l.ShareAsset.ID, l.CashAsset.ID, vf.Quantity, charges)
	if err != nil {
		return Order{}, err
	}

	// Consume, claim, post, evidence, apply. The reservation is consumed
	// first so that every path takes the account's capital lock before its
	// audit stream lock, as AcceptOrder does.
	if _, err := s.capital.ConsumeFinal(ctx, tx, *o.ReservationID, consume, 0, o.ID.String()); err != nil {
		return Order{}, err
	}
	fillID := id.New[fillKind]()
	effect, replay, err := s.effects.Begin(ctx, tx, econ.BeginRequest{
		Key: tradeKey(o.ID), Subsystem: econ.SubsystemEquities, Kind: "EQUITY_TRADE", IntentID: &o.IntentID,
		AccountID: o.AccountID.String(), PostsLedger: true, Actor: s.actor, CorrelationID: o.CorrelationID,
		Request: tradeRequest{OrderID: o.ID.String(), Venue: o.Broker, VenueFillID: vf.VenueFillID, Quantity: vf.Quantity,
			Price: vf.Price, PriceScale: vf.PriceScale, ExecutedAt: vf.ExecutedAt, DisclosureHash: discHash},
	})
	if err != nil {
		return Order{}, err
	}
	if replay {
		return Order{}, errs.Newf(errs.CodeReconciliationRequired,
			"equities: the trade effect of order %s exists (%s) but the order is %s; reconcile, do not retry", o.ID, effect.Status, o.Status).
			WithField("effect_id", effect.ID.String())
	}
	posted, err := s.ledger.Post(ctx, tx, ledger.Posting{
		Kind: ledger.KindSimTrade, IdempotencyKey: tradeKey(o.ID),
		Reference:   ledger.FinancialEventReference{Type: "equity_fill", ID: fillID.String()},
		EffectiveAt: vf.ExecutedAt, CorrelationID: o.CorrelationID,
		Description: "equity " + strings.ToLower(string(o.Side)) + " " + o.Symbol,
		Entries:     entries,
		Metadata: map[string]any{"order_id": o.ID.String(), "fill_id": fillID.String(), "venue_fill_id": vf.VenueFillID,
			"fee_disclosure_hash": discHash},
	})
	if err != nil {
		return Order{}, err
	}
	ev, err := econ.RecordEvidence(ctx, tx, econ.Evidence{Key: "equities/fill/" + o.ID.String(), Document: fillEvidence{
		Kind: "equities.fill.v1", OrderID: o.ID.String(), FillID: fillID.String(), IntentID: o.IntentID.String(),
		AccountID: o.AccountID.String(), InstrumentID: o.InstrumentID.String(), Side: o.Side, OrderType: o.Type,
		Venue: o.Broker, VenueFillID: vf.VenueFillID, Quantity: vf.Quantity, Price: vf.Price, PriceScale: vf.PriceScale,
		Mark: vf.Mark, SlippageBPS: vf.SlippageBPS, ExecutedAt: vf.ExecutedAt, TradeDate: tradeDate.String(),
		SettlementDate: settleDate.String(), RiskDecisionID: decisionString(o), Notional: charges.Notional,
		Spread: charges.Spread, PlatformFees: charges.PlatformFees, VenueFees: charges.VenueFees, Total: charges.Total,
		FeeScheduleHash: quote.ScheduleHash, FeeDisclosureHash: discHash, JournalTransactionID: posted.TransactionID.String(),
	}})
	if err != nil {
		return Order{}, err
	}
	if _, err := s.effects.Apply(ctx, tx, effect.ID, econ.ApplyRequest{
		JournalTransactionID: &posted.TransactionID, ReservationID: o.ReservationID.String(), ExternalRef: vf.VenueFillID,
		Evidence: &ev, Actor: s.actor, Reason: "equity fill posted",
	}); err != nil {
		return Order{}, err
	}
	fill, err := insertFill(ctx, tx, Fill{
		ID: fillID, OrderID: o.ID, AccountID: o.AccountID, InstrumentID: o.InstrumentID, Side: o.Side, Quantity: vf.Quantity,
		Price: vf.Price, PriceScale: vf.PriceScale, Mark: vf.Mark, SlippageBPS: vf.SlippageBPS, Venue: o.Broker,
		VenueFillID: vf.VenueFillID, ExecutedAt: vf.ExecutedAt, TradeDate: tradeDate, SettlementDate: settleDate,
		Charges: charges, FeeScheduleVersion: sch.Version, FeeScheduleHash: quote.ScheduleHash, FeeSchedule: schedBytes,
		FeeDisclosure: discBytes, FeeDisclosureHash: discHash, JournalTransactionID: posted.TransactionID, EffectID: effect.ID,
		CreatedAt: now,
	})
	if err != nil {
		return Order{}, err
	}
	deliver, deliverQty := l.ShareAsset.ID, vf.Quantity
	if o.Side == SideSell {
		deliver, deliverQty = l.CashAsset.ID, charges.Total
	}
	if _, err := insertSettlement(ctx, tx, Settlement{
		ID: id.New[settlementKind](), FillID: fill.ID, OrderID: o.ID, AccountID: o.AccountID, InstrumentID: o.InstrumentID,
		AssetID: deliver, Quantity: deliverQty, TradeDate: tradeDate, DueDate: settleDate, CreatedAt: now,
	}); err != nil {
		return Order{}, err
	}
	out, err := transitionOrder(ctx, tx, o, orderTransition{to: OrderFilled, actor: s.actor, reason: "filled at the venue",
		brokerOrderID: r.BrokerOrderID}, now)
	if err != nil {
		return Order{}, err
	}
	if o.Status == OrderAccepted {
		if err := s.settleSubmission(ctx, tx, o, r, ""); err != nil {
			return Order{}, err
		}
		if _, err := s.intents.Transition(ctx, tx, o.IntentID, econ.StatusExecuting, s.actor, "filled; settles "+settleDate.String(), ""); err != nil {
			return Order{}, err
		}
	}
	return out, s.recordOrder(ctx, tx, out, "filled")
}

func decisionString(o Order) string {
	if o.RiskDecisionID == nil {
		return ""
	}
	return o.RiskDecisionID.String()
}

// FillForOrder returns an order's fill, if it filled.
func (s *Service) FillForOrder(ctx context.Context, q db.Querier, oid OrderID) (Fill, bool, error) {
	return fillByOrder(ctx, q, oid)
}

// SettlementForFill returns a fill's settlement.
func (s *Service) SettlementForFill(ctx context.Context, q db.Querier, fid FillID) (Settlement, bool, error) {
	return settlementByFill(ctx, q, fid)
}

// VerifyFill re-derives everything a fill charged from what is stored: the
// schedule decodes and hashes to the hash the fill and its disclosure name,
// the disclosure is canonical and fees.VerifyDisclosure accepts it, its
// spread and fees are the ones recorded on the fill, and the journal
// transaction books exactly those amounts to FEES_SPREAD, SPREAD_REVENUE,
// FEES_PLATFORM and PLATFORM_FEE_REVENUE. It returns the verified disclosure.
func (s *Service) VerifyFill(ctx context.Context, q db.Querier, oid OrderID) (fees.Disclosure, error) {
	f, ok, err := fillByOrder(ctx, q, oid)
	if err != nil {
		return fees.Disclosure{}, err
	}
	if !ok {
		return fees.Disclosure{}, errs.New(errs.CodeNotFound, "equities: the order has no fill")
	}
	sch, err := DecodeSchedule(f.FeeSchedule)
	if err != nil {
		return fees.Disclosure{}, err
	}
	hash, err := sch.Hash()
	if err != nil {
		return fees.Disclosure{}, err
	}
	d, err := DecodeDisclosure(f.FeeDisclosure)
	if err != nil {
		return fees.Disclosure{}, err
	}
	dHash, err := d.Hash()
	if err != nil {
		return fees.Disclosure{}, err
	}
	var p []string
	if hash != f.FeeScheduleHash || hash != d.ScheduleHash || sch.Version != f.FeeScheduleVersion {
		p = append(p, "the stored schedule is not the one the fill and its disclosure name")
	}
	if dHash != f.FeeDisclosureHash {
		p = append(p, "the stored disclosure does not hash to the recorded hash")
	}
	if err := fees.VerifyDisclosure(sch, d); err != nil {
		p = append(p, "fees.VerifyDisclosure refuses the stored disclosure: "+err.Error())
	}
	platform := money.QuantityFromInt64(0)
	venue := money.QuantityFromInt64(0)
	for _, l := range d.Fees.Lines {
		switch l.Component {
		case fees.ComponentVenuePassThrough:
			venue = venue.Add(l.Charged)
		default:
			platform = platform.Add(l.Charged)
		}
	}
	platform = platform.Add(d.Fees.MinimumTopUp)
	if !d.Spread.Amount.Equal(f.Charges.Spread) || !platform.Equal(f.Charges.PlatformFees) || !venue.Equal(f.Charges.VenueFees) ||
		!d.NotionalAtReference.Equal(f.Charges.Notional) {
		p = append(p, "the fill's recorded charges are not the disclosure's")
	}
	tx, err := s.ledger.Transaction(ctx, q, f.JournalTransactionID)
	if err != nil {
		return fees.Disclosure{}, err
	}
	sums := map[ledger.Code]money.Quantity{}
	for _, e := range tx.Entries {
		if e.Account.AssetID != d.QuoteAsset {
			continue
		}
		sums[e.Account.Code] = sums[e.Account.Code].Add(e.Quantity)
	}
	if !sums[ledger.CodeFeesSpread].Equal(d.Spread.Amount) || !sums[ledger.CodeSpreadRevenue].Equal(d.Spread.Amount) {
		p = append(p, "the journal's spread is not the disclosed spread")
	}
	if !sums[ledger.CodeFeesPlatform].Equal(platform) || !sums[ledger.CodePlatformFeeRevenue].Equal(platform) {
		p = append(p, "the journal's platform fees are not the disclosed fees")
	}
	if len(p) > 0 {
		return fees.Disclosure{}, errs.New(errs.CodeConflict, "equities: the fill does not verify against its stored disclosure").
			WithField("order_id", oid.String()).WithField("problems", p)
	}
	return d, nil
}

type settlementRequest struct {
	SettlementID string         `json:"settlement_id"`
	FillID       string         `json:"fill_id"`
	AssetID      string         `json:"asset_id"`
	Quantity     money.Quantity `json:"quantity"`
	DueDate      string         `json:"due_date"`
}

// SettleDue posts EQUITY_SETTLEMENT for every unsettled settlement due on or
// before today's New York trade date, up to limit, and completes the order's
// intent. Each settlement settles exactly once: an econ effect under a
// deterministic key, rows taken FOR UPDATE SKIP LOCKED, and a database guard
// that refuses any change to a settled row.
func (s *Service) SettleDue(ctx context.Context, tx pgx.Tx, limit int) ([]Settlement, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	cal, err := s.Calendar(ctx, tx, CalendarXNYS)
	if err != nil {
		return nil, err
	}
	now := s.clk.Now().UTC()
	today := cal.TradeDate(now)
	rows, err := tx.Query(ctx, `SELECT `+settlementColumns+` FROM equity_settlements
		WHERE settled_at IS NULL AND due_date <= $1 ORDER BY due_date, id LIMIT $2 FOR UPDATE SKIP LOCKED`, today.Time(), limit)
	if err != nil {
		return nil, storeErr("select due settlements", err)
	}
	var due []Settlement
	for rows.Next() {
		st, err := scanSettlement(rows)
		if err != nil {
			rows.Close()
			return nil, storeErr("scan settlement", err)
		}
		due = append(due, st)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, storeErr("select due settlements", err)
	}
	out := make([]Settlement, 0, len(due))
	for _, st := range due {
		settled, err := s.settleOne(ctx, tx, st, now)
		if err != nil {
			return nil, err
		}
		out = append(out, settled)
	}
	return out, nil
}

func (s *Service) settleOne(ctx context.Context, tx pgx.Tx, st Settlement, now time.Time) (Settlement, error) {
	if err := lockInstrument(ctx, tx, st.InstrumentID, false); err != nil {
		return Settlement{}, err
	}
	o, err := getOrder(ctx, tx, st.OrderID, false)
	if err != nil {
		return Settlement{}, err
	}
	key := "equities:settlement:" + st.FillID.String()
	effect, replay, err := s.effects.Begin(ctx, tx, econ.BeginRequest{
		Key: key, Subsystem: econ.SubsystemEquities, Kind: "EQUITY_SETTLEMENT", IntentID: &o.IntentID,
		AccountID: st.AccountID.String(), PostsLedger: true, Actor: s.actor, CorrelationID: o.CorrelationID,
		Request: settlementRequest{SettlementID: st.ID.String(), FillID: st.FillID.String(), AssetID: st.AssetID.String(),
			Quantity: st.Quantity, DueDate: st.DueDate.String()},
	})
	if err != nil {
		return Settlement{}, err
	}
	if replay {
		return Settlement{}, errs.Newf(errs.CodeReconciliationRequired,
			"equities: settlement %s has an effect (%s) but is not settled; reconcile, do not retry", st.ID, effect.Status).
			WithField("effect_id", effect.ID.String())
	}
	entries, err := SettlementEntries(st.AccountID, st.AssetID, st.Quantity)
	if err != nil {
		return Settlement{}, err
	}
	posted, err := s.ledger.Post(ctx, tx, ledger.Posting{
		Kind: ledger.KindEquitySettlement, IdempotencyKey: key,
		Reference:   ledger.FinancialEventReference{Type: "equity_settlement", ID: st.ID.String()},
		EffectiveAt: now, CorrelationID: o.CorrelationID, Description: "equity settlement " + st.DueDate.String(),
		Entries:  entries,
		Metadata: map[string]any{"order_id": st.OrderID.String(), "fill_id": st.FillID.String(), "due_date": st.DueDate.String()},
	})
	if err != nil {
		return Settlement{}, err
	}
	ev, err := econ.RecordEvidence(ctx, tx, econ.Evidence{Key: "equities/settlement/" + st.FillID.String(), Document: map[string]any{
		"kind": "equities.settlement.v1", "settlement_id": st.ID.String(), "fill_id": st.FillID.String(),
		"order_id": st.OrderID.String(), "account_id": st.AccountID.String(), "asset_id": st.AssetID.String(),
		"quantity": st.Quantity.String(), "trade_date": st.TradeDate.String(), "due_date": st.DueDate.String(),
		"journal_transaction_id": posted.TransactionID.String(),
	}})
	if err != nil {
		return Settlement{}, err
	}
	if _, err := s.effects.Apply(ctx, tx, effect.ID, econ.ApplyRequest{JournalTransactionID: &posted.TransactionID,
		Evidence: &ev, Actor: s.actor, Reason: "settled"}); err != nil {
		return Settlement{}, err
	}
	out, err := scanSettlement(tx.QueryRow(ctx, `UPDATE equity_settlements SET settled_at = $2, journal_transaction_id = $3
		WHERE id = $1 AND settled_at IS NULL RETURNING `+settlementColumns, st.ID, now, posted.TransactionID))
	if err != nil {
		return Settlement{}, storeErr("settle", err)
	}
	if _, err := s.intents.Transition(ctx, tx, o.IntentID, econ.StatusCompleted, s.actor, "settled "+st.DueDate.String(), ""); err != nil {
		return Settlement{}, err
	}
	return out, s.record(ctx, tx, audit.AccountStream(st.AccountID.String()), "equities.settlement.settled", "equity_settlement",
		st.ID.String(), "settled", map[string]string{"fill_id": st.FillID.String(), "quantity": st.Quantity.String(),
			"due_date": st.DueDate.String(), "journal_transaction_id": posted.TransactionID.String()})
}
