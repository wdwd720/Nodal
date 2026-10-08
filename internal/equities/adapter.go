package equities

import (
	"context"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// EquitiesExecutionAdapter is the broker boundary. Every request carries
// everything the venue needs to answer it -- the order, the mark and the
// time -- so an answer is reproducible and can be recorded and replayed.
// Implementations in this build are the deterministic paper venue and the
// fixture replay; there is no live implementation.
//
// PlaceOrder and Cancel are effectful at the venue and are called outside any
// database transaction, after the order is recorded (AcceptOrder) and before
// the answer is applied (ApplyOrderReport). OrderStatus is a read and may be
// called any number of times.
type EquitiesExecutionAdapter interface {
	// Name identifies the venue; it is recorded on every order.
	Name() string
	// Mode is the provider mode the adapter was built for.
	Mode() config.ProviderMode
	// Quote returns the indicative execution price of a market order now.
	Quote(ctx context.Context, req QuoteRequest) (QuoteResponse, error)
	// PlaceOrder submits an order and returns the venue's first answer.
	PlaceOrder(ctx context.Context, req PlaceOrderRequest) (OrderReport, error)
	// OrderStatus returns the venue's current answer for an order.
	OrderStatus(ctx context.Context, req OrderStatusRequest) (OrderReport, error)
	// Cancel asks the venue to cancel an order and returns its answer, which
	// is FILLED when the cancel came too late.
	Cancel(ctx context.Context, req CancelRequest) (OrderReport, error)
}

// Mark is the reference price of one whole share in the instrument's
// settlement asset, as the price store observed it.
type Mark struct {
	Mantissa   money.Quantity `json:"mantissa"`
	Scale      int32          `json:"scale"`
	Source     string         `json:"source"`
	ObservedAt time.Time      `json:"observed_at"`
}

// Price returns the mark as a money.Price quoted in quoteAsset.
func (m Mark) Price(quoteAsset string) (money.Price, error) {
	return money.NewPrice(m.Mantissa, m.Scale, quoteAsset, m.Source, m.ObservedAt)
}

func (m Mark) validate() error {
	if !m.Mantissa.IsPositive() || m.Scale < 0 || m.Scale > money.MaxPriceScale ||
		strings.TrimSpace(m.Source) == "" || m.ObservedAt.IsZero() {
		return errs.New(errs.CodeValidationFailed, "equities: a mark is a positive price with a scale, a source and an observation time")
	}
	return nil
}

// BrokerOrder is an order as the venue sees it. Prices are mantissas at
// PriceScale on the TickSize grid of the instrument's terms.
type BrokerOrder struct {
	ClientOrderID    string          `json:"client_order_id"`
	Symbol           string          `json:"symbol"`
	ExchangeMIC      string          `json:"exchange_mic"`
	Side             Side            `json:"side"`
	Type             OrderType       `json:"order_type"`
	Quantity         money.Quantity  `json:"quantity"`
	QuantityDecimals uint8           `json:"quantity_decimals"`
	PriceScale       int32           `json:"price_scale"`
	TickSize         money.Quantity  `json:"tick_size"`
	LimitPrice       *money.Quantity `json:"limit_price,omitempty"`
	// ProtectionPrice bounds a market order: a buy never fills above it, a
	// sell never below it. It is what the order's capital reservation was
	// sized against.
	ProtectionPrice *money.Quantity `json:"protection_price,omitempty"`
	ExtendedHours   bool            `json:"extended_hours"`
	ExpiresAt       time.Time       `json:"expires_at"`
}

// Validate checks the order's shape. A venue refuses a malformed order with
// an error, never with a report.
func (o BrokerOrder) Validate() error {
	var p []string
	if strings.TrimSpace(o.ClientOrderID) == "" {
		p = append(p, "client order id required")
	}
	if strings.TrimSpace(o.Symbol) == "" || !micPattern(o.ExchangeMIC) {
		p = append(p, "symbol and a four-character exchange MIC required")
	}
	if !o.Side.Valid() || !o.Type.Valid() {
		p = append(p, "side and order type must be declared values")
	}
	if !o.Quantity.IsPositive() {
		p = append(p, "quantity must be positive")
	}
	if o.QuantityDecimals > 18 || o.PriceScale < 0 || o.PriceScale > 18 {
		p = append(p, "quantity decimals and price scale must be within 0..18")
	}
	if !o.TickSize.IsPositive() {
		p = append(p, "tick size must be positive")
	}
	switch o.Type {
	case OrderLimit:
		if o.LimitPrice == nil || !o.LimitPrice.IsPositive() || !onGrid(*o.LimitPrice, o.TickSize) {
			p = append(p, "a limit order carries a positive limit price on the tick grid")
		}
		if o.ProtectionPrice != nil {
			p = append(p, "a limit order carries no protection price; its limit is its protection")
		}
	case OrderMarket:
		if o.ProtectionPrice == nil || !o.ProtectionPrice.IsPositive() || !onGrid(*o.ProtectionPrice, o.TickSize) {
			p = append(p, "a market order carries a positive protection price on the tick grid")
		}
		if o.LimitPrice != nil || o.ExtendedHours {
			p = append(p, "a market order has no limit price and never trades outside regular hours")
		}
	}
	if o.ExpiresAt.IsZero() {
		p = append(p, "every order is a DAY order with an expiry")
	}
	if len(p) > 0 {
		return errs.New(errs.CodeValidationFailed, "equities: invalid broker order").WithField("problems", p)
	}
	return nil
}

func micPattern(s string) bool {
	if len(s) != 4 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func onGrid(price, tick money.Quantity) bool {
	_, err := price.Div(tick, money.RoundExact)
	return err == nil
}

// QuoteRequest asks for the indicative execution price of a market order.
type QuoteRequest struct {
	Symbol           string         `json:"symbol"`
	ExchangeMIC      string         `json:"exchange_mic"`
	Side             Side           `json:"side"`
	Quantity         money.Quantity `json:"quantity"`
	QuantityDecimals uint8          `json:"quantity_decimals"`
	PriceScale       int32          `json:"price_scale"`
	TickSize         money.Quantity `json:"tick_size"`
	Mark             *Mark          `json:"mark,omitempty"`
	At               time.Time      `json:"at"`
}

// QuoteResponse is the venue's indicative price: the mark moved by the
// disclosed slippage and rounded onto the tick grid against the customer.
type QuoteResponse struct {
	Broker          string         `json:"broker"`
	Symbol          string         `json:"symbol"`
	Side            Side           `json:"side"`
	Session         Session        `json:"session"`
	Mark            Mark           `json:"mark"`
	SlippageBPS     money.BPS      `json:"slippage_bps"`
	IndicativePrice money.Quantity `json:"indicative_price"`
	PriceScale      int32          `json:"price_scale"`
	At              time.Time      `json:"at"`
}

// PlaceOrderRequest submits an order. Mark is nil when no fresh mark exists.
type PlaceOrderRequest struct {
	Order BrokerOrder `json:"order"`
	Mark  *Mark       `json:"mark,omitempty"`
	At    time.Time   `json:"at"`
}

// OrderStatusRequest asks for the current answer for an order.
type OrderStatusRequest struct {
	BrokerOrderID string      `json:"broker_order_id"`
	Order         BrokerOrder `json:"order"`
	Mark          *Mark       `json:"mark,omitempty"`
	At            time.Time   `json:"at"`
}

// CancelRequest asks the venue to cancel an order.
type CancelRequest struct {
	BrokerOrderID string      `json:"broker_order_id"`
	Order         BrokerOrder `json:"order"`
	Mark          *Mark       `json:"mark,omitempty"`
	At            time.Time   `json:"at"`
}

// ReportStatus is the venue's answer for an order.
type ReportStatus string

// Report statuses.
const (
	ReportOpen      ReportStatus = "OPEN"
	ReportFilled    ReportStatus = "FILLED"
	ReportRejected  ReportStatus = "REJECTED"
	ReportCancelled ReportStatus = "CANCELLED"
	ReportExpired   ReportStatus = "EXPIRED"
)

// AllReportStatuses returns every report status.
func AllReportStatuses() []ReportStatus {
	return []ReportStatus{ReportOpen, ReportFilled, ReportRejected, ReportCancelled, ReportExpired}
}

// Valid reports whether s is declared.
func (s ReportStatus) Valid() bool {
	for _, x := range AllReportStatuses() {
		if x == s {
			return true
		}
	}
	return false
}

// Venue rejection reasons the paper venue reports.
const (
	VenueReasonSessionClosed   = "SESSION_CLOSED"
	VenueReasonNoMark          = "NO_MARK"
	VenueReasonPriceProtection = "PRICE_PROTECTION"
	VenueReasonNoBid           = "NO_BID"
	VenueReasonDayOrderExpired = "DAY_ORDER_EXPIRED"
	VenueReasonCancelled       = "CANCELLED_ON_REQUEST"
)

// BrokerFill is one execution. The paper venue fills an order in full, once.
type BrokerFill struct {
	VenueFillID string         `json:"venue_fill_id"`
	Quantity    money.Quantity `json:"quantity"`
	// Price is the execution price per whole share at PriceScale.
	Price      money.Quantity `json:"price"`
	PriceScale int32          `json:"price_scale"`
	// Mark and SlippageBPS disclose what the price was derived from.
	Mark        Mark      `json:"mark"`
	SlippageBPS money.BPS `json:"slippage_bps"`
	ExecutedAt  time.Time `json:"executed_at"`
}

// OrderReport is the venue's answer for one order.
type OrderReport struct {
	Broker        string       `json:"broker"`
	BrokerOrderID string       `json:"broker_order_id"`
	ClientOrderID string       `json:"client_order_id"`
	Status        ReportStatus `json:"status"`
	Reason        string       `json:"reason,omitempty"`
	Session       Session      `json:"session"`
	Fill          *BrokerFill  `json:"fill,omitempty"`
	At            time.Time    `json:"at"`
}

// Validate checks the report's shape: a FILLED report carries exactly one
// fill and nothing else does.
func (r OrderReport) Validate() error {
	var p []string
	if strings.TrimSpace(r.Broker) == "" || strings.TrimSpace(r.BrokerOrderID) == "" || strings.TrimSpace(r.ClientOrderID) == "" {
		p = append(p, "broker, broker order id and client order id required")
	}
	if !r.Status.Valid() {
		p = append(p, "unknown report status")
	}
	if (r.Status == ReportFilled) != (r.Fill != nil) {
		p = append(p, "a FILLED report carries a fill and no other report does")
	}
	if r.Fill != nil {
		f := r.Fill
		if strings.TrimSpace(f.VenueFillID) == "" || !f.Quantity.IsPositive() || !f.Price.IsPositive() ||
			f.PriceScale < 0 || f.PriceScale > 18 || f.ExecutedAt.IsZero() || f.SlippageBPS < 0 {
			p = append(p, "a fill has an id, a positive quantity and price, a price scale, a slippage and an execution time")
		}
		if err := f.Mark.validate(); err != nil {
			p = append(p, "a fill discloses the mark it was derived from")
		}
	}
	if r.At.IsZero() {
		p = append(p, "a report has a time")
	}
	if len(p) > 0 {
		return errs.New(errs.CodeValidationFailed, "equities: invalid order report").WithField("problems", p)
	}
	return nil
}

// Stable reasons for refusing an adapter.
const (
	// ReasonLiveExecutionUnsupported: live retail execution does not exist in
	// this build. No environment, flag or configuration changes that.
	ReasonLiveExecutionUnsupported = "EQUITIES_LIVE_EXECUTION_UNSUPPORTED"
	// ReasonProviderModeNotAllowed: fake and sandbox providers run only in
	// LOCAL, TEST and DEV.
	ReasonProviderModeNotAllowed = "EQUITIES_PROVIDER_MODE_NOT_ALLOWED"
)

// ErrLiveExecutionUnsupported is the stable refusal of provider mode live.
func ErrLiveExecutionUnsupported() error {
	return errs.New(errs.CodeUnsupported, "equities: live retail execution does not exist in this build").
		WithField("reason", ReasonLiveExecutionUnsupported)
}

// NewExecutionAdapter returns the adapter for a provider mode in an
// environment. live is refused unconditionally, before the environment is
// even looked at; fake and sandbox both run the deterministic paper venue
// (SANDBOX is "a full workflow against a deterministic provider", ADR-0020)
// and are refused outside LOCAL, TEST and DEV.
func NewExecutionAdapter(env config.Environment, mode config.ProviderMode, cfg PaperConfig) (EquitiesExecutionAdapter, error) {
	if mode == config.ProviderModeLive {
		return nil, ErrLiveExecutionUnsupported()
	}
	if !mode.IsValid() {
		return nil, errs.Newf(errs.CodeValidationFailed, "equities: unknown provider mode %q", mode)
	}
	if !env.IsValid() || !env.AllowsFakeProviders() {
		return nil, errs.Newf(errs.CodeForbidden, "equities: provider mode %s is not allowed in environment %q", mode, env).
			WithField("reason", ReasonProviderModeNotAllowed)
	}
	return NewPaperVenue(mode, cfg)
}
