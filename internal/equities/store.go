package equities

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
)

// Order is an equity_orders row.
type Order struct {
	ID               OrderID
	AccountID        accounts.AccountID
	InstrumentID     instruments.InstrumentID
	IntentID         econ.IntentID
	ShareAssetID     assets.AssetID
	CashAssetID      assets.AssetID
	Symbol           string
	ExchangeMIC      string
	TermsVersion     int
	Side             Side
	Type             OrderType
	Mode             gates.Mode
	Quantity         money.Quantity
	QuantityDecimals uint8
	PriceScale       int32
	TickSize         money.Quantity
	LimitPrice       *money.Quantity
	ProtectionPrice  *money.Quantity
	ExtendedHours    bool
	Session          Session
	TradeDate        Date
	ExpiresAt        time.Time
	Status           OrderStatus
	RejectionCode    RejectionCode
	RejectionReasons []string
	ReservationID    *capital.ReservationID
	ReservedQuantity *money.Quantity
	RiskDecisionID   *risk.DecisionID
	Broker           string
	BrokerOrderID    string
	ActorType        security.ActorType
	ActorID          string
	CorrelationID    string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Refusal renders a REJECTED order's refusal as the stable error a caller
// returns; nil for every other status.
func (o Order) Refusal() error {
	if o.Status != OrderRejected {
		return nil
	}
	reasons := o.RejectionReasons
	if reasons == nil {
		reasons = []string{}
	}
	return errs.Newf(o.RejectionCode.errorCode(), "equities: order %s was refused: %s", o.ID, o.RejectionCode).
		WithField("order_id", o.ID.String()).
		WithField("rejection_code", string(o.RejectionCode)).
		WithField("reason_codes", reasons)
}

// BrokerOrder returns the order as the venue sees it.
func (o Order) BrokerOrder() BrokerOrder {
	return BrokerOrder{
		ClientOrderID: o.ID.String(), Symbol: o.Symbol, ExchangeMIC: o.ExchangeMIC, Side: o.Side, Type: o.Type,
		Quantity: o.Quantity, QuantityDecimals: o.QuantityDecimals, PriceScale: o.PriceScale, TickSize: o.TickSize,
		LimitPrice: o.LimitPrice, ProtectionPrice: o.ProtectionPrice, ExtendedHours: o.ExtendedHours, ExpiresAt: o.ExpiresAt.UTC(),
	}
}

const orderColumns = `id, account_id, instrument_id, intent_id, share_asset_id, cash_asset_id, symbol, exchange_mic, terms_version,
	side, order_type, mode, quantity::text, quantity_decimals, price_scale, tick_size::text, coalesce(limit_price::text, ''),
	coalesce(protection_price::text, ''), extended_hours, session, trade_date, expires_at, status, coalesce(rejection_code, ''),
	rejection_reasons, reservation_id, coalesce(reserved_quantity::text, ''), risk_decision_id, broker, coalesce(broker_order_id, ''),
	actor_type, actor_id, coalesce(correlation_id, ''), created_at, updated_at`

func scanOrder(row pgx.Row) (Order, error) {
	var (
		o                                           Order
		qty, tick, limit, protection, reserved      string
		decimals                                    int16
		tradeDate                                   time.Time
		reservation                                 capital.ReservationID
		decision                                    risk.DecisionID
		termsVersion                                int32
		priceScale                                  int32
		side, typ, mode, session, status, rejection string
	)
	if err := row.Scan(&o.ID, &o.AccountID, &o.InstrumentID, &o.IntentID, &o.ShareAssetID, &o.CashAssetID, &o.Symbol, &o.ExchangeMIC,
		&termsVersion, &side, &typ, &mode, &qty, &decimals, &priceScale, &tick, &limit, &protection, &o.ExtendedHours, &session,
		&tradeDate, &o.ExpiresAt, &status, &rejection, &o.RejectionReasons, &reservation, &reserved, &decision, &o.Broker,
		&o.BrokerOrderID, &o.ActorType, &o.ActorID, &o.CorrelationID, &o.CreatedAt, &o.UpdatedAt); err != nil {
		return Order{}, err
	}
	if decimals < 0 || decimals > 18 {
		return Order{}, fmt.Errorf("equities: stored quantity decimals %d out of range", decimals)
	}
	o.QuantityDecimals = uint8(decimals) // #nosec G115 -- bounded by the check above and by the column CHECK
	o.TermsVersion, o.PriceScale = int(termsVersion), priceScale
	o.Side, o.Type, o.Mode, o.Session = Side(side), OrderType(typ), gates.Mode(mode), Session(session)
	o.Status, o.RejectionCode = OrderStatus(status), RejectionCode(rejection)
	o.TradeDate = DateOf(tradeDate)
	o.ExpiresAt, o.CreatedAt, o.UpdatedAt = o.ExpiresAt.UTC(), o.CreatedAt.UTC(), o.UpdatedAt.UTC()
	var err error
	if o.Quantity, err = money.ScanQuantity(qty); err != nil {
		return Order{}, err
	}
	if o.TickSize, err = money.ScanQuantity(tick); err != nil {
		return Order{}, err
	}
	if o.LimitPrice, err = optionalQuantity(limit); err != nil {
		return Order{}, err
	}
	if o.ProtectionPrice, err = optionalQuantity(protection); err != nil {
		return Order{}, err
	}
	if o.ReservedQuantity, err = optionalQuantity(reserved); err != nil {
		return Order{}, err
	}
	if !reservation.IsZero() {
		o.ReservationID = &reservation
	}
	if !decision.IsZero() {
		o.RiskDecisionID = &decision
	}
	if o.RejectionReasons == nil {
		o.RejectionReasons = []string{}
	}
	return o, nil
}

func optionalQuantity(s string) (*money.Quantity, error) {
	if s == "" {
		return nil, nil
	}
	q, err := money.ScanQuantity(s)
	if err != nil {
		return nil, err
	}
	return &q, nil
}

func quantityArg(q *money.Quantity) any {
	if q == nil {
		return nil
	}
	return q.String()
}

func insertOrder(ctx context.Context, tx pgx.Tx, o Order) (Order, error) {
	var reservation any
	if o.ReservationID != nil {
		reservation = *o.ReservationID
	}
	var decision any
	if o.RiskDecisionID != nil {
		decision = *o.RiskDecisionID
	}
	reasons := o.RejectionReasons
	if reasons == nil {
		reasons = []string{}
	}
	row := tx.QueryRow(ctx, `INSERT INTO equity_orders (id, account_id, instrument_id, intent_id, share_asset_id, cash_asset_id,
		symbol, exchange_mic, terms_version, side, order_type, mode, quantity, quantity_decimals, price_scale, tick_size,
		limit_price, protection_price, extended_hours, session, trade_date, expires_at, status, rejection_code, rejection_reasons,
		reservation_id, reserved_quantity, risk_decision_id, broker, actor_type, actor_id, correlation_id, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13::numeric,$14,$15,$16::numeric,$17::numeric,$18::numeric,$19,$20,$21,$22,
		        $23,NULLIF($24,''),$25,$26,$27::numeric,$28,$29,$30,$31,NULLIF($32,''),$33,$33)
		RETURNING `+orderColumns,
		o.ID, o.AccountID, o.InstrumentID, o.IntentID, o.ShareAssetID, o.CashAssetID, o.Symbol, o.ExchangeMIC, o.TermsVersion,
		string(o.Side), string(o.Type), string(o.Mode), o.Quantity.String(), int16(o.QuantityDecimals), o.PriceScale,
		o.TickSize.String(), quantityArg(o.LimitPrice), quantityArg(o.ProtectionPrice), o.ExtendedHours, string(o.Session),
		o.TradeDate.Time(), o.ExpiresAt.UTC(), string(o.Status), string(o.RejectionCode), reasons, reservation,
		quantityArg(o.ReservedQuantity), decision, o.Broker, string(o.ActorType), o.ActorID, o.CorrelationID, o.CreatedAt.UTC())
	out, err := scanOrder(row)
	if err != nil {
		return Order{}, storeErr("insert order", err)
	}
	return out, nil
}

func getOrder(ctx context.Context, q db.Querier, oid OrderID, forUpdate bool) (Order, error) {
	sql := `SELECT ` + orderColumns + ` FROM equity_orders WHERE id = $1`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	o, err := scanOrder(q.QueryRow(ctx, sql, oid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, errs.New(errs.CodeNotFound, "equity order not found").WithField("order_id", oid.String())
	}
	if err != nil {
		return Order{}, storeErr("load order", err)
	}
	return o, nil
}

func orderByIntent(ctx context.Context, q db.Querier, iid econ.IntentID) (Order, bool, error) {
	o, err := scanOrder(q.QueryRow(ctx, `SELECT `+orderColumns+` FROM equity_orders WHERE intent_id = $1`, iid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, false, nil
	}
	if err != nil {
		return Order{}, false, storeErr("load order by intent", err)
	}
	return o, true, nil
}

// orderTransition is one status change of an order.
type orderTransition struct {
	to            OrderStatus
	actor         econ.Actor
	reason        string
	rejectionCode RejectionCode
	reasons       []string
	brokerOrderID string
}

// transitionOrder writes the transition row and the status change together;
// the database binds the two (AU001) and refuses an illegal edge (EQ001).
func transitionOrder(ctx context.Context, tx pgx.Tx, o Order, t orderTransition, now time.Time) (Order, error) {
	if !CanTransitionOrder(o.Status, t.to) {
		return Order{}, errs.Newf(errs.CodeInvalidStateTransition, "equity order %s cannot move %s -> %s", o.ID, o.Status, t.to).
			WithField("order_id", o.ID.String())
	}
	if _, err := tx.Exec(ctx, `INSERT INTO equity_order_transitions (id, order_id, from_status, to_status, actor_type, actor_id,
		reason, correlation_id, occurred_at) VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),$9)`,
		id.New[id.Any](), o.ID, string(o.Status), string(t.to), string(t.actor.Type), t.actor.ID, t.reason, o.CorrelationID, now.UTC()); err != nil {
		return Order{}, storeErr("record order transition", err)
	}
	var rejection any
	reasons := o.RejectionReasons
	if t.to == OrderRejected {
		rejection = string(t.rejectionCode)
		reasons = t.reasons
		if reasons == nil {
			reasons = []string{}
		}
	}
	out, err := scanOrder(tx.QueryRow(ctx, `UPDATE equity_orders
		SET status = $2, rejection_code = coalesce($3, rejection_code), rejection_reasons = $4,
		    broker_order_id = coalesce(NULLIF($5, ''), broker_order_id)
		WHERE id = $1 RETURNING `+orderColumns, o.ID, string(t.to), rejection, reasons, t.brokerOrderID))
	if err != nil {
		return Order{}, storeErr("update order", err)
	}
	return out, nil
}

// Fill is an equity_fills row.
type Fill struct {
	ID                   FillID
	OrderID              OrderID
	AccountID            accounts.AccountID
	InstrumentID         instruments.InstrumentID
	Side                 Side
	Quantity             money.Quantity
	Price                money.Quantity
	PriceScale           int32
	Mark                 Mark
	SlippageBPS          money.BPS
	Venue                string
	VenueFillID          string
	ExecutedAt           time.Time
	TradeDate            Date
	SettlementDate       Date
	Charges              TradeCharges
	FeeScheduleVersion   string
	FeeScheduleHash      string
	FeeSchedule          []byte
	FeeDisclosure        []byte
	FeeDisclosureHash    string
	JournalTransactionID ledger.TransactionID
	EffectID             econ.EffectID
	CreatedAt            time.Time
}

const fillColumns = `id, order_id, account_id, instrument_id, side, quantity::text, price::text, price_scale, mark_mantissa::text,
	mark_scale, mark_source, mark_observed_at, slippage_bps, venue, venue_fill_id, executed_at, trade_date, settlement_date,
	notional::text, spread_amount::text, platform_fees::text, venue_fees::text, total::text, fee_schedule_version, fee_schedule_hash,
	fee_schedule, fee_disclosure, fee_disclosure_hash, journal_transaction_id, effect_id, created_at`

func scanFill(row pgx.Row) (Fill, error) {
	var (
		f                                                                Fill
		side, qty, price, mark, notional, spread, platform, venue, total string
		schedule, disclosure                                             string
		tradeDate, settleDate                                            time.Time
		slippage                                                         int64
	)
	if err := row.Scan(&f.ID, &f.OrderID, &f.AccountID, &f.InstrumentID, &side, &qty, &price, &f.PriceScale, &mark,
		&f.Mark.Scale, &f.Mark.Source, &f.Mark.ObservedAt, &slippage, &f.Venue, &f.VenueFillID, &f.ExecutedAt, &tradeDate,
		&settleDate, &notional, &spread, &platform, &venue, &total, &f.FeeScheduleVersion, &f.FeeScheduleHash, &schedule,
		&disclosure, &f.FeeDisclosureHash, &f.JournalTransactionID, &f.EffectID, &f.CreatedAt); err != nil {
		return Fill{}, err
	}
	f.Side, f.SlippageBPS = Side(side), money.BPS(slippage)
	f.FeeSchedule, f.FeeDisclosure = []byte(schedule), []byte(disclosure)
	f.TradeDate, f.SettlementDate = DateOf(tradeDate), DateOf(settleDate)
	f.ExecutedAt, f.CreatedAt, f.Mark.ObservedAt = f.ExecutedAt.UTC(), f.CreatedAt.UTC(), f.Mark.ObservedAt.UTC()
	for _, p := range []struct {
		dst *money.Quantity
		src string
	}{{&f.Quantity, qty}, {&f.Price, price}, {&f.Mark.Mantissa, mark}, {&f.Charges.Notional, notional},
		{&f.Charges.Spread, spread}, {&f.Charges.PlatformFees, platform}, {&f.Charges.VenueFees, venue}, {&f.Charges.Total, total}} {
		v, err := money.ScanQuantity(p.src)
		if err != nil {
			return Fill{}, err
		}
		*p.dst = v
	}
	return f, nil
}

func insertFill(ctx context.Context, tx pgx.Tx, f Fill) (Fill, error) {
	out, err := scanFill(tx.QueryRow(ctx, `INSERT INTO equity_fills (id, order_id, account_id, instrument_id, side, quantity, price,
		price_scale, mark_mantissa, mark_scale, mark_source, mark_observed_at, slippage_bps, venue, venue_fill_id, executed_at,
		trade_date, settlement_date, notional, spread_amount, platform_fees, venue_fees, total, fee_schedule_version,
		fee_schedule_hash, fee_schedule, fee_disclosure, fee_disclosure_hash, journal_transaction_id, effect_id, created_at)
		VALUES ($1,$2,$3,$4,$5,$6::numeric,$7::numeric,$8,$9::numeric,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19::numeric,$20::numeric,
		        $21::numeric,$22::numeric,$23::numeric,$24,$25,$26,$27,$28,$29,$30,$31)
		RETURNING `+fillColumns,
		f.ID, f.OrderID, f.AccountID, f.InstrumentID, string(f.Side), f.Quantity.String(), f.Price.String(), f.PriceScale,
		f.Mark.Mantissa.String(), f.Mark.Scale, f.Mark.Source, f.Mark.ObservedAt.UTC(), int64(f.SlippageBPS), f.Venue, f.VenueFillID,
		f.ExecutedAt.UTC(), f.TradeDate.Time(), f.SettlementDate.Time(), f.Charges.Notional.String(), f.Charges.Spread.String(),
		f.Charges.PlatformFees.String(), f.Charges.VenueFees.String(), f.Charges.Total.String(), f.FeeScheduleVersion,
		f.FeeScheduleHash, string(f.FeeSchedule), string(f.FeeDisclosure), f.FeeDisclosureHash, f.JournalTransactionID, f.EffectID,
		f.CreatedAt.UTC()))
	if err != nil {
		return Fill{}, storeErr("insert fill", err)
	}
	return out, nil
}

func fillByOrder(ctx context.Context, q db.Querier, oid OrderID) (Fill, bool, error) {
	f, err := scanFill(q.QueryRow(ctx, `SELECT `+fillColumns+` FROM equity_fills WHERE order_id = $1`, oid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Fill{}, false, nil
	}
	if err != nil {
		return Fill{}, false, storeErr("load fill", err)
	}
	return f, true, nil
}

// Settlement is an equity_settlements row.
type Settlement struct {
	ID                   SettlementID
	FillID               FillID
	OrderID              OrderID
	AccountID            accounts.AccountID
	InstrumentID         instruments.InstrumentID
	AssetID              assets.AssetID
	Quantity             money.Quantity
	TradeDate            Date
	DueDate              Date
	SettledAt            *time.Time
	JournalTransactionID *ledger.TransactionID
	CreatedAt            time.Time
}

// Settled reports whether the settlement has been posted.
func (s Settlement) Settled() bool { return s.SettledAt != nil }

const settlementColumns = `id, fill_id, order_id, account_id, instrument_id, asset_id, quantity::text, trade_date, due_date,
	settled_at, journal_transaction_id, created_at`

func scanSettlement(row pgx.Row) (Settlement, error) {
	var (
		s          Settlement
		qty        string
		trade, due time.Time
		journal    ledger.TransactionID
		settledAt  *time.Time
		createdAt  time.Time
	)
	if err := row.Scan(&s.ID, &s.FillID, &s.OrderID, &s.AccountID, &s.InstrumentID, &s.AssetID, &qty, &trade, &due,
		&settledAt, &journal, &createdAt); err != nil {
		return Settlement{}, err
	}
	q, err := money.ScanQuantity(qty)
	if err != nil {
		return Settlement{}, err
	}
	s.Quantity, s.TradeDate, s.DueDate, s.CreatedAt = q, DateOf(trade), DateOf(due), createdAt.UTC()
	if settledAt != nil {
		t := settledAt.UTC()
		s.SettledAt = &t
	}
	if !journal.IsZero() {
		s.JournalTransactionID = &journal
	}
	return s, nil
}

func insertSettlement(ctx context.Context, tx pgx.Tx, s Settlement) (Settlement, error) {
	out, err := scanSettlement(tx.QueryRow(ctx, `INSERT INTO equity_settlements (id, fill_id, order_id, account_id, instrument_id,
		asset_id, quantity, trade_date, due_date, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7::numeric,$8,$9,$10,$10) RETURNING `+settlementColumns,
		s.ID, s.FillID, s.OrderID, s.AccountID, s.InstrumentID, s.AssetID, s.Quantity.String(), s.TradeDate.Time(), s.DueDate.Time(),
		s.CreatedAt.UTC()))
	if err != nil {
		return Settlement{}, storeErr("insert settlement", err)
	}
	return out, nil
}

func settlementByFill(ctx context.Context, q db.Querier, fid FillID) (Settlement, bool, error) {
	s, err := scanSettlement(q.QueryRow(ctx, `SELECT `+settlementColumns+` FROM equity_settlements WHERE fill_id = $1`, fid))
	if errors.Is(err, pgx.ErrNoRows) {
		return Settlement{}, false, nil
	}
	if err != nil {
		return Settlement{}, false, storeErr("load settlement", err)
	}
	return s, true, nil
}

// Holidays returns the closures of a calendar, in date order.
func Holidays(ctx context.Context, q db.Querier, code string) ([]Holiday, error) {
	rows, err := q.Query(ctx, `SELECT holiday_date, name FROM equity_calendar_holidays WHERE calendar_code = $1 ORDER BY holiday_date`, code)
	if err != nil {
		return nil, storeErr("load holidays", err)
	}
	defer rows.Close()
	var out []Holiday
	for rows.Next() {
		var d time.Time
		var name string
		if err := rows.Scan(&d, &name); err != nil {
			return nil, storeErr("scan holiday", err)
		}
		out = append(out, Holiday{Date: DateOf(d), Name: name})
	}
	return out, rows.Err()
}

// storeErr keeps typed errors and retryable failures as they are and wraps
// the rest, naming this package's SQLSTATEs.
func storeErr(op string, err error) error {
	if err == nil {
		return nil
	}
	if db.IsRetryable(err) {
		return err
	}
	if _, ok := errs.As(err); ok {
		return err
	}
	switch db.SQLState(err) {
	case "EQ001", "EQ005":
		return errs.Wrap(err, errs.CodeInvalidStateTransition, "equities: "+op)
	case "EQ002", "EQ004":
		return errs.Wrap(err, errs.CodeConflict, "equities: "+op+": the record is frozen")
	case "EQ003":
		return errs.Wrap(err, errs.CodeValidationFailed, "equities: "+op+": the fill contradicts its order")
	}
	if db.IsUniqueViolation(err) {
		return errs.Wrap(err, errs.CodeConflict, "equities: "+op+": already recorded")
	}
	if mapped := ledger.MapError(err); mapped != nil {
		if ee, ok := errs.As(mapped); ok && ee.Code != errs.CodeInternal {
			return ee
		}
	}
	return errs.Wrap(err, errs.CodeInternal, "equities: "+strings.TrimSpace(op))
}
