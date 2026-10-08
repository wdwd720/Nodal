package equities

import (
	"context"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// MaxPaperSlippageBPS bounds the paper venue's disclosed slippage (10%).
const MaxPaperSlippageBPS money.BPS = 1_000

// PaperConfig configures the paper venue.
type PaperConfig struct {
	// Name identifies the venue on every order and report.
	Name string
	// Calendar is the venue's own trading calendar: it decides which session
	// an order arrives in, exactly as an exchange's clock would.
	Calendar *Calendar
	// SlippageBPS is the disclosed slippage applied to the mark for every
	// market execution and every crossing limit order.
	SlippageBPS money.BPS
}

// PaperVenue is a deterministic, stateless simulated broker. An answer is a
// pure function of the request: the order, the mark and the time it carries.
//
//   - A market order fills in full at the mark moved by the disclosed
//     slippage against the customer, rounded onto the tick grid against the
//     customer, unless that price is beyond the order's protection price
//     (REJECTED, PRICE_PROTECTION) or there is no mark (REJECTED, NO_MARK).
//   - A limit order fills in full only when the mark crosses its limit (a
//     buy: mark <= limit; a sell: mark >= limit), at the slipped price capped
//     by the limit; otherwise it rests (OPEN).
//   - Session rules: an order outside the session it may trade in is
//     REJECTED (SESSION_CLOSED) when placed; an order at or after its expiry
//     is EXPIRED.
//   - Cancel returns CANCELLED for a resting order, and the order's fill when
//     it crosses at that instant.
type PaperVenue struct {
	name     string
	mode     config.ProviderMode
	cal      *Calendar
	slippage money.BPS
}

var _ EquitiesExecutionAdapter = (*PaperVenue)(nil)

// NewPaperVenue builds the paper venue for a fake or sandbox provider mode.
// Live is refused: there is no live venue behind this type.
func NewPaperVenue(mode config.ProviderMode, cfg PaperConfig) (*PaperVenue, error) {
	switch mode {
	case config.ProviderModeLive:
		return nil, ErrLiveExecutionUnsupported()
	case config.ProviderModeFake, config.ProviderModeSandbox:
	default:
		return nil, errs.Newf(errs.CodeValidationFailed, "equities: unknown provider mode %q", mode)
	}
	if strings.TrimSpace(cfg.Name) == "" || cfg.Calendar == nil {
		return nil, errs.New(errs.CodeValidationFailed, "equities: the paper venue needs a name and a calendar")
	}
	if cfg.SlippageBPS < 0 || cfg.SlippageBPS > MaxPaperSlippageBPS {
		return nil, errs.Newf(errs.CodeValidationFailed, "equities: paper slippage must be within 0..%d bps", MaxPaperSlippageBPS)
	}
	return &PaperVenue{name: cfg.Name, mode: mode, cal: cfg.Calendar, slippage: cfg.SlippageBPS}, nil
}

// Name implements EquitiesExecutionAdapter.
func (p *PaperVenue) Name() string { return p.name }

// Mode implements EquitiesExecutionAdapter.
func (p *PaperVenue) Mode() config.ProviderMode { return p.mode }

// SlippageBPS returns the disclosed slippage.
func (p *PaperVenue) SlippageBPS() money.BPS { return p.slippage }

func (p *PaperVenue) brokerOrderID(clientOrderID string) string { return "paper-" + clientOrderID }

// Quote implements EquitiesExecutionAdapter.
func (p *PaperVenue) Quote(ctx context.Context, req QuoteRequest) (QuoteResponse, error) {
	if err := ctx.Err(); err != nil {
		return QuoteResponse{}, err
	}
	if !req.Side.Valid() || !req.Quantity.IsPositive() || !req.TickSize.IsPositive() ||
		req.PriceScale < 0 || req.PriceScale > 18 || req.At.IsZero() {
		return QuoteResponse{}, errs.New(errs.CodeValidationFailed, "equities: invalid quote request")
	}
	if req.Mark == nil {
		return QuoteResponse{}, errs.New(errs.CodeStaleMarketData, "equities: the paper venue quotes only against a fresh mark").
			WithField("reason", VenueReasonNoMark)
	}
	if err := req.Mark.validate(); err != nil {
		return QuoteResponse{}, err
	}
	price, err := slippedPrice(*req.Mark, req.Side, p.slippage, req.PriceScale, req.TickSize)
	if err != nil {
		return QuoteResponse{}, errs.Wrap(err, errs.CodeValidationFailed, "equities: price the quote")
	}
	return QuoteResponse{
		Broker: p.name, Symbol: req.Symbol, Side: req.Side, Session: p.cal.SessionAt(req.At), Mark: *req.Mark,
		SlippageBPS: p.slippage, IndicativePrice: price, PriceScale: req.PriceScale, At: req.At.UTC(),
	}, nil
}

// PlaceOrder implements EquitiesExecutionAdapter.
func (p *PaperVenue) PlaceOrder(ctx context.Context, req PlaceOrderRequest) (OrderReport, error) {
	if err := ctx.Err(); err != nil {
		return OrderReport{}, err
	}
	return p.evaluate(req.Order, req.Mark, req.At, true)
}

// OrderStatus implements EquitiesExecutionAdapter.
func (p *PaperVenue) OrderStatus(ctx context.Context, req OrderStatusRequest) (OrderReport, error) {
	if err := ctx.Err(); err != nil {
		return OrderReport{}, err
	}
	if err := p.checkBrokerOrderID(req.BrokerOrderID, req.Order); err != nil {
		return OrderReport{}, err
	}
	return p.evaluate(req.Order, req.Mark, req.At, false)
}

// Cancel implements EquitiesExecutionAdapter.
func (p *PaperVenue) Cancel(ctx context.Context, req CancelRequest) (OrderReport, error) {
	if err := ctx.Err(); err != nil {
		return OrderReport{}, err
	}
	if err := p.checkBrokerOrderID(req.BrokerOrderID, req.Order); err != nil {
		return OrderReport{}, err
	}
	r, err := p.evaluate(req.Order, req.Mark, req.At, false)
	if err != nil {
		return OrderReport{}, err
	}
	if r.Status != ReportOpen {
		// Filled at this instant, or already expired: the cancel is too late
		// and the venue says what happened instead.
		return r, nil
	}
	r.Status, r.Reason = ReportCancelled, VenueReasonCancelled
	return r, nil
}

// checkBrokerOrderID accepts the venue's own id for the order, or none for an
// order whose first answer was never received: it is found by its client
// order id, as a real venue finds it.
func (p *PaperVenue) checkBrokerOrderID(brokerOrderID string, o BrokerOrder) error {
	if brokerOrderID != "" && brokerOrderID != p.brokerOrderID(o.ClientOrderID) {
		return errs.New(errs.CodeNotFound, "equities: the paper venue knows no such order").
			WithField("broker_order_id", brokerOrderID)
	}
	return nil
}

// evaluate is the venue's whole decision procedure.
func (p *PaperVenue) evaluate(o BrokerOrder, mark *Mark, at time.Time, placing bool) (OrderReport, error) {
	if err := o.Validate(); err != nil {
		return OrderReport{}, err
	}
	if at.IsZero() {
		return OrderReport{}, errs.New(errs.CodeValidationFailed, "equities: a venue request has a time")
	}
	if mark != nil {
		if err := mark.validate(); err != nil {
			return OrderReport{}, err
		}
	}
	at = at.UTC()
	session, eligible, _ := p.cal.OrderWindow(at, o.Type, o.ExtendedHours)
	r := OrderReport{Broker: p.name, BrokerOrderID: p.brokerOrderID(o.ClientOrderID), ClientOrderID: o.ClientOrderID,
		Session: session, At: at}
	reject := func(reason string) (OrderReport, error) {
		r.Status, r.Reason = ReportRejected, reason
		return r, nil
	}
	if !at.Before(o.ExpiresAt) {
		r.Status, r.Reason = ReportExpired, VenueReasonDayOrderExpired
		return r, nil
	}
	if !eligible {
		if placing {
			return reject(VenueReasonSessionClosed)
		}
		r.Status = ReportOpen
		return r, nil
	}
	if mark == nil {
		if o.Type == OrderMarket {
			return reject(VenueReasonNoMark)
		}
		r.Status = ReportOpen
		return r, nil
	}
	slipped, err := slippedPrice(*mark, o.Side, p.slippage, o.PriceScale, o.TickSize)
	if err != nil {
		return OrderReport{}, errs.Wrap(err, errs.CodeValidationFailed, "equities: price the execution")
	}
	if !slipped.IsPositive() {
		return reject(VenueReasonNoBid)
	}
	var price money.Quantity
	switch o.Type {
	case OrderMarket:
		protection := *o.ProtectionPrice
		if (o.Side == SideBuy && slipped.Cmp(protection) > 0) || (o.Side == SideSell && slipped.Cmp(protection) < 0) {
			return reject(VenueReasonPriceProtection)
		}
		price = slipped
	case OrderLimit:
		limit := *o.LimitPrice
		c := compareMark(*mark, limit, o.PriceScale)
		if (o.Side == SideBuy && c > 0) || (o.Side == SideSell && c < 0) {
			r.Status = ReportOpen
			return r, nil
		}
		price = slipped
		if o.Side == SideBuy {
			price = price.Min(limit)
		} else {
			price = price.Max(limit)
		}
	}
	r.Status = ReportFilled
	r.Fill = &BrokerFill{
		VenueFillID: "paper-fill-" + o.ClientOrderID, Quantity: o.Quantity, Price: price, PriceScale: o.PriceScale,
		Mark: *mark, SlippageBPS: p.slippage, ExecutedAt: at,
	}
	return r, nil
}
