package execution

import (
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// OrderStatus is the PART 47 order state.
type OrderStatus string

// Order statuses. The happy path runs top to bottom; the side states are
// listed after SETTLED.
const (
	OrderCreated                OrderStatus = "CREATED"
	OrderValidated              OrderStatus = "VALIDATED"
	OrderCapitalReserved        OrderStatus = "CAPITAL_RESERVED"
	OrderPlanned                OrderStatus = "PLANNED"
	OrderSubmitting             OrderStatus = "SUBMITTING"
	OrderSubmitted              OrderStatus = "SUBMITTED"
	OrderAcknowledged           OrderStatus = "ACKNOWLEDGED"
	OrderPartiallyFilled        OrderStatus = "PARTIALLY_FILLED"
	OrderFilled                 OrderStatus = "FILLED"
	OrderSettling               OrderStatus = "SETTLING"
	OrderSettled                OrderStatus = "SETTLED"
	OrderRejected               OrderStatus = "REJECTED"
	OrderExpired                OrderStatus = "EXPIRED"
	OrderCancelRequested        OrderStatus = "CANCEL_REQUESTED"
	OrderCancelled              OrderStatus = "CANCELLED"
	OrderSubmissionUnknown      OrderStatus = "SUBMISSION_UNKNOWN"
	OrderReconciliationRequired OrderStatus = "RECONCILIATION_REQUIRED"
	OrderFailedFinal            OrderStatus = "FAILED_FINAL"
)

// AllOrderStatuses returns every status in declaration order.
func AllOrderStatuses() []OrderStatus {
	return []OrderStatus{
		OrderCreated, OrderValidated, OrderCapitalReserved, OrderPlanned, OrderSubmitting, OrderSubmitted,
		OrderAcknowledged, OrderPartiallyFilled, OrderFilled, OrderSettling, OrderSettled,
		OrderRejected, OrderExpired, OrderCancelRequested, OrderCancelled, OrderSubmissionUnknown,
		OrderReconciliationRequired, OrderFailedFinal,
	}
}

// Valid reports whether s is a declared status.
func (s OrderStatus) Valid() bool {
	_, ok := OrderTransitions[s]
	return ok
}

// Terminal reports whether no transition leaves s. The set matches the
// partial index orders_open_idx of migration 00202.
func (s OrderStatus) Terminal() bool {
	switch s {
	case OrderSettled, OrderRejected, OrderExpired, OrderCancelled, OrderFailedFinal:
		return true
	}
	return false
}

// Submitted reports whether the order has (or may have) reached the venue,
// i.e. whether an external effect may exist.
func (s OrderStatus) Submitted() bool {
	switch s {
	case OrderSubmitted, OrderAcknowledged, OrderPartiallyFilled, OrderFilled, OrderSettling, OrderSettled,
		OrderSubmissionUnknown, OrderReconciliationRequired:
		return true
	case OrderCancelRequested:
		return true
	}
	return false
}

// AcceptsFill reports whether a fill may be recorded against an order in
// this status. A fill is external truth, so every post-submission state
// that is not terminal accepts one — including CANCEL_REQUESTED (the fill
// wins, PART 227), SUBMISSION_UNKNOWN (an adopted execution) and
// RECONCILIATION_REQUIRED (reconciliation discovered it).
func (s OrderStatus) AcceptsFill() bool {
	switch s {
	case OrderSubmitted, OrderAcknowledged, OrderPartiallyFilled, OrderCancelRequested,
		OrderSubmissionUnknown, OrderReconciliationRequired:
		return true
	}
	return false
}

// OrderTransitions is the explicit PART 47 transition table. A status maps
// to the set of statuses it may move to; terminal statuses map to an empty
// set. It is the single source of truth for CanTransition and for
// Repository.Transition.
var OrderTransitions = map[OrderStatus][]OrderStatus{
	OrderCreated:         {OrderValidated, OrderRejected, OrderExpired},
	OrderValidated:       {OrderCapitalReserved, OrderRejected, OrderExpired},
	OrderCapitalReserved: {OrderPlanned, OrderRejected, OrderExpired},
	OrderPlanned:         {OrderSubmitting, OrderRejected, OrderExpired, OrderCancelRequested},
	OrderSubmitting:      {OrderSubmitted, OrderSubmissionUnknown, OrderRejected, OrderExpired, OrderCancelRequested},
	OrderSubmitted: {
		OrderAcknowledged, OrderPartiallyFilled, OrderFilled, OrderExpired, OrderFailedFinal,
		OrderSubmissionUnknown, OrderReconciliationRequired, OrderCancelRequested,
	},
	OrderAcknowledged: {
		OrderPartiallyFilled, OrderFilled, OrderExpired, OrderFailedFinal, OrderReconciliationRequired, OrderCancelRequested,
	},
	OrderPartiallyFilled: {
		OrderFilled, OrderSettling, OrderExpired, OrderFailedFinal, OrderReconciliationRequired, OrderCancelRequested,
	},
	OrderFilled:   {OrderSettling, OrderReconciliationRequired},
	OrderSettling: {OrderSettled, OrderReconciliationRequired},
	OrderSettled:  {},
	OrderRejected: {},
	OrderExpired:  {},
	OrderCancelRequested: {
		OrderCancelled, OrderPartiallyFilled, OrderFilled, OrderExpired, OrderSubmissionUnknown, OrderReconciliationRequired,
	},
	OrderCancelled: {},
	OrderSubmissionUnknown: {
		OrderSubmitting, OrderSubmitted, OrderAcknowledged, OrderPartiallyFilled, OrderFilled,
		OrderExpired, OrderFailedFinal, OrderReconciliationRequired,
	},
	OrderReconciliationRequired: {
		OrderSubmitted, OrderAcknowledged, OrderPartiallyFilled, OrderFilled, OrderSettling,
		OrderExpired, OrderFailedFinal, OrderCancelled,
	},
	OrderFailedFinal: {},
}

// CanTransition reports whether from → to is in the table.
func CanTransition(from, to OrderStatus) bool {
	for _, s := range OrderTransitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// checkTransition returns INVALID_STATE_TRANSITION with from/to fields.
func checkTransition(orderID OrderID, from, to OrderStatus) error {
	if CanTransition(from, to) {
		return nil
	}
	return errs.Newf(errs.CodeInvalidStateTransition, "order is %s; %s -> %s is not allowed", from, from, to).
		WithField("order_id", orderID.String()).WithField("from", string(from)).WithField("to", string(to))
}

// Mode is the performance mode stored on every order (PART 159).
type Mode string

// Modes.
const (
	ModeBacktest Mode = "BACKTEST"
	ModePaper    Mode = "PAPER"
	ModeShadow   Mode = "SHADOW"
	ModeCanary   Mode = "CANARY"
	ModeLimited  Mode = "LIMITED"
	ModeLive     Mode = "LIVE"
)

// Valid reports whether m is a declared mode.
func (m Mode) Valid() bool {
	switch m {
	case ModeBacktest, ModePaper, ModeShadow, ModeCanary, ModeLimited, ModeLive:
		return true
	}
	return false
}

// Order mirrors one orders row (migration 00202).
type Order struct {
	ID                   OrderID
	IntentID             string // uuid text
	PlanID               string // uuid text
	AccountID            accounts.AccountID
	InstrumentID         string // uuid text
	VenueListingID       string // uuid text
	Side                 Side
	Mode                 Mode
	Status               OrderStatus
	InputAssetID         assets.AssetID
	InputQuantity        money.Quantity
	OutputAssetID        assets.AssetID
	MinOutputQuantity    money.Quantity
	FilledInputQuantity  money.Quantity
	FilledOutputQuantity money.Quantity
	ReservationID        string // uuid text
	QuoteID              string // uuid text
	RejectionCode        string
	CorrelationID        string
	TerminalAt           *time.Time
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// Validate checks the structural rules of a new order.
func (o Order) Validate() error {
	problems := map[string]any{}
	if o.ID.IsZero() {
		problems["id"] = "required"
	}
	for k, v := range map[string]string{
		"intent_id": o.IntentID, "plan_id": o.PlanID, "instrument_id": o.InstrumentID,
		"venue_listing_id": o.VenueListingID, "reservation_id": o.ReservationID, "quote_id": o.QuoteID,
	} {
		if v == "" {
			problems[k] = "required"
		}
	}
	if o.AccountID.IsZero() {
		problems["account_id"] = "required"
	}
	if !o.Side.Valid() {
		problems["side"] = "must be BUY or SELL"
	}
	if !o.Mode.Valid() {
		problems["mode"] = "unknown mode"
	}
	if o.InputAssetID.IsZero() || o.OutputAssetID.IsZero() {
		problems["assets"] = "input and output assets are required"
	}
	if o.InputAssetID == o.OutputAssetID && !o.InputAssetID.IsZero() {
		problems["assets"] = "input and output assets must differ"
	}
	if !o.InputQuantity.IsPositive() {
		problems["input_quantity"] = "must be positive"
	}
	if o.MinOutputQuantity.IsNegative() {
		problems["min_output_quantity"] = "must not be negative"
	}
	if o.CorrelationID == "" {
		problems["correlation_id"] = "required"
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "execution: invalid order").WithFields(problems)
	}
	return nil
}

// Remaining returns the unfilled input quantity.
func (o Order) Remaining() money.Quantity { return o.InputQuantity.Sub(o.FilledInputQuantity) }

// NextStatusAfterFill is the pure PART 228 rule: given the current status
// and the cumulative filled input after a fill, it returns the status the
// order moves to (which may equal the current one for a further partial fill
// on an already PARTIALLY_FILLED order). It fails with
// INVALID_STATE_TRANSITION when the status does not accept fills, and with
// RECONCILIATION_REQUIRED when the cumulative fill exceeds the order.
func NextStatusAfterFill(current OrderStatus, filledInput, inputQuantity money.Quantity) (OrderStatus, error) {
	if !current.AcceptsFill() {
		return "", errs.Newf(errs.CodeInvalidStateTransition, "order is %s and does not accept fills", current).
			WithField("from", string(current))
	}
	switch c := filledInput.Cmp(inputQuantity); {
	case c > 0:
		return "", errs.New(errs.CodeReconciliationRequired, "cumulative fills exceed the order input quantity").
			WithField("filled_input_quantity", filledInput.String()).WithField("input_quantity", inputQuantity.String())
	case c == 0:
		return OrderFilled, nil
	default:
		return OrderPartiallyFilled, nil
	}
}

// TransitionEvidence is what a transition records: who caused it and why.
// ActorType defaults to SYSTEM and ActorID to "execution" when empty.
type TransitionEvidence struct {
	ActorType     string
	ActorID       string
	Reason        string
	EvidenceRef   string
	RejectionCode string
	RequestID     string
	// CausationID is the event that caused the transition (a fill id, an
	// attempt id); it is copied onto the outbox envelope.
	CausationID string
}

// Transition is one order_transitions row.
type Transition struct {
	ID          TransitionID
	OrderID     OrderID
	From        OrderStatus
	To          OrderStatus
	ActorType   string
	ActorID     string
	Reason      string
	EvidenceRef string
	OccurredAt  time.Time
}
