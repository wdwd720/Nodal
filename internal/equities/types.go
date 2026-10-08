package equities

import (
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
)

type (
	orderKind       struct{}
	fillKind        struct{}
	settlementKind  struct{}
	actionKind      struct{}
	applicationKind struct{}
)

// Identifiers. All are UUIDv7 minted in Go.
type (
	OrderID           = id.ID[orderKind]
	FillID            = id.ID[fillKind]
	SettlementID      = id.ID[settlementKind]
	CorporateActionID = id.ID[actionKind]
	ApplicationID     = id.ID[applicationKind]
)

// NewOrderID returns a fresh order id.
func NewOrderID() OrderID { return id.New[orderKind]() }

// ParseOrderID parses the canonical form.
func ParseOrderID(s string) (OrderID, error) { return id.Parse[orderKind](s) }

// ParseCorporateActionID parses the canonical form.
func ParseCorporateActionID(s string) (CorporateActionID, error) { return id.Parse[actionKind](s) }

// Side is the direction of an equity order.
type Side string

// Sides.
const (
	SideBuy  Side = "BUY"
	SideSell Side = "SELL"
)

// AllSides returns every side; it is the Go twin of equity_orders_side_check.
func AllSides() []Side { return []Side{SideBuy, SideSell} }

// Valid reports whether s is declared.
func (s Side) Valid() bool { return s == SideBuy || s == SideSell }

// OrderType is MARKET or LIMIT.
type OrderType string

// Order types.
const (
	OrderMarket OrderType = "MARKET"
	OrderLimit  OrderType = "LIMIT"
)

// AllOrderTypes returns every order type (equity_orders_order_type_check).
func AllOrderTypes() []OrderType { return []OrderType{OrderMarket, OrderLimit} }

// Valid reports whether t is declared.
func (t OrderType) Valid() bool { return t == OrderMarket || t == OrderLimit }

// OrderStatus is the status of an equity order.
type OrderStatus string

// Order statuses. ACCEPTED means recorded, reserved and not yet answered by
// the venue; OPEN means resting at the venue.
const (
	OrderAccepted  OrderStatus = "ACCEPTED"
	OrderRejected  OrderStatus = "REJECTED"
	OrderOpen      OrderStatus = "OPEN"
	OrderFilled    OrderStatus = "FILLED"
	OrderCancelled OrderStatus = "CANCELLED"
	OrderExpired   OrderStatus = "EXPIRED"
)

// AllOrderStatuses returns every order status (equity_orders_status_check).
func AllOrderStatuses() []OrderStatus {
	return []OrderStatus{OrderAccepted, OrderRejected, OrderOpen, OrderFilled, OrderCancelled, OrderExpired}
}

// Valid reports whether s is declared.
func (s OrderStatus) Valid() bool {
	for _, x := range AllOrderStatuses() {
		if x == s {
			return true
		}
	}
	return false
}

// Terminal reports whether no further transition is legal.
func (s OrderStatus) Terminal() bool {
	return s == OrderRejected || s == OrderFilled || s == OrderCancelled || s == OrderExpired
}

// orderTransitions mirrors cp_equity_order_transition_legal (migration
// 00743); a parity test compares the two.
var orderTransitions = map[OrderStatus][]OrderStatus{
	OrderAccepted: {OrderOpen, OrderFilled, OrderRejected, OrderCancelled, OrderExpired},
	OrderOpen:     {OrderFilled, OrderCancelled, OrderExpired},
}

// CanTransitionOrder reports whether from -> to is legal.
func CanTransitionOrder(from, to OrderStatus) bool {
	for _, t := range orderTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// AllOrderModes returns the modes an equity order can be placed in
// (equity_orders_mode_check). LIVE is absent, as it is everywhere in this
// build.
func AllOrderModes() []gates.Mode { return []gates.Mode{gates.ModePaper, gates.ModeSandbox} }

func validOrderMode(m gates.Mode) bool { return m == gates.ModePaper || m == gates.ModeSandbox }

// Session is a trading session of the calendar.
type Session string

// Sessions.
const (
	SessionClosed     Session = "CLOSED"
	SessionPreMarket  Session = "PRE_MARKET"
	SessionRegular    Session = "REGULAR"
	SessionAfterHours Session = "AFTER_HOURS"
)

// AllSessions returns every session (equity_orders_session_check).
func AllSessions() []Session {
	return []Session{SessionClosed, SessionPreMarket, SessionRegular, SessionAfterHours}
}

// RejectionCode is the stable, machine-readable reason an order was refused.
type RejectionCode string

// Rejection codes.
const (
	// RejectCapabilityNotApproved: the Authorizer refused EQUITIES in this
	// mode and deployment.
	RejectCapabilityNotApproved RejectionCode = "CAPABILITY_NOT_APPROVED"
	// RejectKillSwitch: a CAPABILITY_DISABLE or global kill switch refused it.
	RejectKillSwitch RejectionCode = "KILL_SWITCH_ACTIVE"
	// RejectRisk: the risk kernel refused it; the reasons are on the order.
	RejectRisk RejectionCode = "RISK_REJECTED"
	// RejectInsufficientBuyingPower: the capital reservation failed.
	RejectInsufficientBuyingPower RejectionCode = "INSUFFICIENT_BUYING_POWER"
	// RejectBelowMinimum: the quantity is below the instrument's minimum.
	RejectBelowMinimum RejectionCode = "EQUITIES_QUANTITY_BELOW_MINIMUM"
	// RejectNotLotMultiple: the quantity is not a whole number of lots.
	RejectNotLotMultiple RejectionCode = "EQUITIES_QUANTITY_NOT_LOT_MULTIPLE"
	// RejectLimitPriceInvalid: the limit price is not a positive price on
	// the instrument's tick grid in its settlement asset.
	RejectLimitPriceInvalid RejectionCode = "EQUITIES_LIMIT_PRICE_INVALID"
	// RejectNoMark: there is no fresh mark to price or fill the order.
	RejectNoMark RejectionCode = "EQUITIES_NO_MARK"
	// RejectNotionalTooSmall: the order is worth less than the minimum
	// notional, or too little to carry its spread and fees.
	RejectNotionalTooSmall RejectionCode = "EQUITIES_NOTIONAL_TOO_SMALL"
	// RejectCorporateActionPending: a split, reverse split, symbol change or
	// delisting is effective and not yet applied.
	RejectCorporateActionPending RejectionCode = "EQUITIES_CORPORATE_ACTION_PENDING"
	// RejectIntentPredatesSplit: a split or reverse split was applied after
	// the intent was recorded, so its quantity and limit are in units the
	// instrument no longer trades in.
	RejectIntentPredatesSplit RejectionCode = "EQUITIES_INTENT_PREDATES_SPLIT"
	// RejectVenue: the venue refused the order.
	RejectVenue RejectionCode = "VENUE_REJECTED"
)

// AllRejectionCodes returns every rejection code
// (equity_orders_rejection_code_check).
func AllRejectionCodes() []RejectionCode {
	return []RejectionCode{
		RejectCapabilityNotApproved, RejectKillSwitch, RejectRisk, RejectInsufficientBuyingPower,
		RejectBelowMinimum, RejectNotLotMultiple, RejectLimitPriceInvalid, RejectNoMark,
		RejectNotionalTooSmall, RejectCorporateActionPending, RejectIntentPredatesSplit, RejectVenue,
	}
}

// errorCode is the errs.Code a refusal with this rejection code renders as.
func (c RejectionCode) errorCode() errs.Code {
	switch c {
	case RejectCapabilityNotApproved:
		return errs.CodeCapabilityNotApproved
	case RejectKillSwitch:
		return errs.CodeKillSwitchActive
	case RejectRisk:
		return errs.CodeRiskRejected
	case RejectInsufficientBuyingPower:
		return errs.CodeInsufficientBuyingPower
	case RejectNoMark:
		return errs.CodeStaleMarketData
	case RejectCorporateActionPending:
		return errs.CodeAssetRestricted
	case RejectIntentPredatesSplit, RejectVenue:
		return errs.CodeConflict
	}
	return errs.CodeValidationFailed
}

// CorporateActionType is the kind of a corporate action.
type CorporateActionType string

// Corporate action types.
const (
	ActionSplit        CorporateActionType = "SPLIT"
	ActionReverseSplit CorporateActionType = "REVERSE_SPLIT"
	ActionCashDividend CorporateActionType = "CASH_DIVIDEND"
	ActionSymbolChange CorporateActionType = "SYMBOL_CHANGE"
	ActionDelisting    CorporateActionType = "DELISTING"
)

// AllCorporateActionTypes returns every type
// (equity_corporate_actions_action_type_check).
func AllCorporateActionTypes() []CorporateActionType {
	return []CorporateActionType{ActionSplit, ActionReverseSplit, ActionCashDividend, ActionSymbolChange, ActionDelisting}
}

// Valid reports whether t is declared.
func (t CorporateActionType) Valid() bool {
	for _, x := range AllCorporateActionTypes() {
		if x == t {
			return true
		}
	}
	return false
}

// blocksTrading reports whether an effective, unapplied action of this type
// must stop new orders: positions and symbols are about to change under
// them. A dividend does not change what is traded.
func (t CorporateActionType) blocksTrading() bool {
	return t == ActionSplit || t == ActionReverseSplit || t == ActionSymbolChange
}

// CorporateActionStatus is the status of a corporate action.
type CorporateActionStatus string

// Corporate action statuses. APPLYING is used only by a delisting, between
// the instrument becoming DELISTING and becoming DELISTED.
const (
	ActionAnnounced CorporateActionStatus = "ANNOUNCED"
	ActionApplying  CorporateActionStatus = "APPLYING"
	ActionApplied   CorporateActionStatus = "APPLIED"
	ActionCancelled CorporateActionStatus = "CANCELLED"
)

// AllCorporateActionStatuses returns every status
// (equity_corporate_actions_status_check).
func AllCorporateActionStatuses() []CorporateActionStatus {
	return []CorporateActionStatus{ActionAnnounced, ActionApplying, ActionApplied, ActionCancelled}
}

// actionTransitions mirrors cp_equity_action_transition_legal (00743).
var actionTransitions = map[CorporateActionStatus][]CorporateActionStatus{
	ActionAnnounced: {ActionApplying, ActionApplied, ActionCancelled},
	ActionApplying:  {ActionApplied},
}

// CanTransitionAction reports whether from -> to is legal.
func CanTransitionAction(from, to CorporateActionStatus) bool {
	for _, t := range actionTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// CalendarXNYS is the US equity market calendar.
const CalendarXNYS = "XNYS"

// AllCalendarCodes returns every calendar this build implements
// (equity_calendar_holidays_calendar_code_check).
func AllCalendarCodes() []string { return []string{CalendarXNYS} }
