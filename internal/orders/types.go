package orders

import (
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/matching"
)

// Every enumeration below has a SQL twin in migration 00742 (a CHECK (col IN
// (...)) constraint), and each AllXxx function returns exactly that list, in
// the order the CHECK names it, so a parity test can pair them.

// OwnerType says whose order an order is: a customer account or a house
// (principal) book. They are different owners with different reservation
// paths and the crossing policy keys on the difference.
type OwnerType string

// Owner types.
const (
	OwnerCustomer OwnerType = "CUSTOMER"
	OwnerHouse    OwnerType = "HOUSE"
)

// AllOwnerTypes returns every owner type (book_orders.owner_type,
// book_trades.maker_owner_type / taker_owner_type,
// book_crossing_decisions.taker_owner_type).
func AllOwnerTypes() []OwnerType { return []OwnerType{OwnerCustomer, OwnerHouse} }

// Valid reports whether o is declared.
func (o OwnerType) Valid() bool { return o == OwnerCustomer || o == OwnerHouse }

// Status is the persisted state of an order. PENDING is a customer order that
// has been accepted for processing but has not reached the book yet; the
// others mirror the engine's statuses.
type Status string

// Order statuses.
const (
	StatusPending         Status = "PENDING"
	StatusOpen            Status = "OPEN"
	StatusPartiallyFilled Status = "PARTIALLY_FILLED"
	StatusFilled          Status = "FILLED"
	StatusCancelled       Status = "CANCELLED"
	StatusRejected        Status = "REJECTED"
)

// AllStatuses returns every order status (book_orders.status).
func AllStatuses() []Status {
	return []Status{StatusPending, StatusOpen, StatusPartiallyFilled, StatusFilled, StatusCancelled, StatusRejected}
}

// Valid reports whether s is declared.
func (s Status) Valid() bool {
	for _, x := range AllStatuses() {
		if x == s {
			return true
		}
	}
	return false
}

// Terminal reports whether an order in this status never changes again.
func (s Status) Terminal() bool {
	return s == StatusFilled || s == StatusCancelled || s == StatusRejected
}

// Resting reports whether an order in this status rests on the book.
func (s Status) Resting() bool { return s == StatusOpen || s == StatusPartiallyFilled }

// orderTransitions mirrors cp_book_order_transition_legal (migration 00742).
var orderTransitions = map[Status][]Status{
	StatusPending:         {StatusOpen, StatusPartiallyFilled, StatusFilled, StatusCancelled, StatusRejected},
	StatusOpen:            {StatusPartiallyFilled, StatusFilled, StatusCancelled},
	StatusPartiallyFilled: {StatusFilled, StatusCancelled},
}

// CanTransition reports whether an order may move from -> to.
func CanTransition(from, to Status) bool {
	for _, t := range orderTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// statusOf maps the engine's status onto the persisted one.
func statusOf(s matching.OrderStatus) Status {
	switch s {
	case matching.StatusOpen:
		return StatusOpen
	case matching.StatusPartiallyFilled:
		return StatusPartiallyFilled
	case matching.StatusFilled:
		return StatusFilled
	case matching.StatusCancelled:
		return StatusCancelled
	}
	return StatusRejected
}

// BookStatus is the state of a book. A HALTED book accepts cancels only; a
// CLOSED book accepts nothing.
type BookStatus string

// Book statuses.
const (
	BookOpen   BookStatus = "OPEN"
	BookHalted BookStatus = "HALTED"
	BookClosed BookStatus = "CLOSED"
)

// AllBookStatuses returns every book status (book_states.status).
func AllBookStatuses() []BookStatus { return []BookStatus{BookOpen, BookHalted, BookClosed} }

// Valid reports whether s is declared.
func (s BookStatus) Valid() bool { return s == BookOpen || s == BookHalted || s == BookClosed }

var bookTransitions = map[BookStatus][]BookStatus{
	BookOpen:   {BookHalted, BookClosed},
	BookHalted: {BookOpen, BookClosed},
}

// CanTransitionBook mirrors cp_book_status_transition_legal.
func CanTransitionBook(from, to BookStatus) bool {
	for _, t := range bookTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// Side is BUY or SELL.
type Side string

// Sides.
const (
	SideBuy  Side = "BUY"
	SideSell Side = "SELL"
)

// AllSides returns both sides (book_orders.side, book_trades.aggressor_side,
// paper_fills.side).
func AllSides() []Side { return []Side{SideBuy, SideSell} }

// Valid reports whether s is declared.
func (s Side) Valid() bool { return s == SideBuy || s == SideSell }

func (s Side) engine() matching.Side {
	if s == SideBuy {
		return matching.SideBuy
	}
	return matching.SideSell
}

func sideOf(s matching.Side) Side {
	if s == matching.SideBuy {
		return SideBuy
	}
	return SideSell
}

// OrderType is LIMIT or MARKET.
type OrderType string

// Order types.
const (
	TypeLimit  OrderType = "LIMIT"
	TypeMarket OrderType = "MARKET"
)

// AllOrderTypes returns both order types (book_orders.order_type).
func AllOrderTypes() []OrderType { return []OrderType{TypeLimit, TypeMarket} }

// Valid reports whether t is declared.
func (t OrderType) Valid() bool { return t == TypeLimit || t == TypeMarket }

func (t OrderType) engine() matching.OrderType {
	if t == TypeMarket {
		return matching.OrderTypeMarket
	}
	return matching.OrderTypeLimit
}

// TimeInForce is GTC, IOC or FOK.
type TimeInForce string

// Times in force.
const (
	GTC TimeInForce = "GTC"
	IOC TimeInForce = "IOC"
	FOK TimeInForce = "FOK"
)

// AllTimesInForce returns every time in force (book_orders.time_in_force).
func AllTimesInForce() []TimeInForce { return []TimeInForce{GTC, IOC, FOK} }

// Valid reports whether t is declared.
func (t TimeInForce) Valid() bool { return t == GTC || t == IOC || t == FOK }

func (t TimeInForce) engine() matching.TimeInForce {
	switch t {
	case IOC:
		return matching.TIFIOC
	case FOK:
		return matching.TIFFOK
	}
	return matching.TIFGTC
}

// STPMode is the self-trade prevention mode an order applies when it takes
// liquidity. The owner is the account (or house book): two orders of the same
// owner never trade.
type STPMode string

// Self-trade prevention modes.
const (
	STPCancelTaker STPMode = "CANCEL_TAKER"
	STPCancelMaker STPMode = "CANCEL_MAKER"
	STPCancelBoth  STPMode = "CANCEL_BOTH"
)

// AllSTPModes returns every mode (book_orders.stp_mode).
func AllSTPModes() []STPMode { return []STPMode{STPCancelTaker, STPCancelMaker, STPCancelBoth} }

// Valid reports whether m is declared.
func (m STPMode) Valid() bool {
	return m == STPCancelTaker || m == STPCancelMaker || m == STPCancelBoth
}

func (m STPMode) engine() matching.STPMode {
	switch m {
	case STPCancelMaker:
		return matching.STPCancelMaker
	case STPCancelBoth:
		return matching.STPCancelBoth
	}
	return matching.STPCancelTaker
}

// LiquidityIntent is recorded in an order's intent terms: what the order sets
// out to do. The role it actually played is recorded per fill (maker or
// taker) on book_trades.
type LiquidityIntent string

// Liquidity intents. They are the econ intent kinds of the same names, used
// as a term of an ACQUIRE_EXPOSURE / REDUCE_EXPOSURE intent.
const (
	TakeLiquidity    LiquidityIntent = "TAKE_LIQUIDITY"
	ProvideLiquidity LiquidityIntent = "PROVIDE_LIQUIDITY"
)

// AllLiquidityIntents returns both intents (book_orders.liquidity_intent).
func AllLiquidityIntents() []LiquidityIntent {
	return []LiquidityIntent{TakeLiquidity, ProvideLiquidity}
}

// liquidityIntentOf is what an order of this shape sets out to do: a market,
// IOC or FOK order takes liquidity, a GTC limit order offers it.
func liquidityIntentOf(t OrderType, tif TimeInForce) LiquidityIntent {
	if t == TypeMarket || tif != GTC {
		return TakeLiquidity
	}
	return ProvideLiquidity
}

// CommandKind names a journalled book command.
type CommandKind string

// Command kinds.
const (
	CommandNewOrder CommandKind = "NEW_ORDER"
	CommandCancel   CommandKind = "CANCEL"
	CommandReplace  CommandKind = "REPLACE"
)

// AllCommandKinds returns every command kind (book_commands.kind).
func AllCommandKinds() []CommandKind {
	return []CommandKind{CommandNewOrder, CommandCancel, CommandReplace}
}

// AllCrossingCommandKinds returns the command kinds a crossing decision can
// be about (book_crossing_decisions.command_kind): only a new order or a
// replace can trade.
func AllCrossingCommandKinds() []CommandKind { return []CommandKind{CommandNewOrder, CommandReplace} }

func commandKindOf(k matching.CommandKind) CommandKind {
	switch k {
	case matching.CommandCancel:
		return CommandCancel
	case matching.CommandReplace:
		return CommandReplace
	}
	return CommandNewOrder
}

// AllEventTypes returns every persisted engine event type
// (book_events.event_type), in the engine's numbering order.
func AllEventTypes() []string {
	out := make([]string, 0, 9)
	for t := matching.EventAccepted; t <= matching.EventBookDelta; t++ {
		out = append(out, t.String())
	}
	return out
}

// TermsSource says where a book's instrument terms came from: the canonical
// versioned terms of internal/instruments, or (for a SPOT_PAIR, which has no
// canonical terms row) terms stated when the book was opened.
type TermsSource string

// Terms sources.
const (
	TermsCanonical TermsSource = "CANONICAL"
	TermsBookSpot  TermsSource = "BOOK_SPOT"
)

// AllTermsSources returns both sources (book_states.terms_source).
func AllTermsSources() []TermsSource { return []TermsSource{TermsCanonical, TermsBookSpot} }

// AllBookModes returns the modes a book runs in (book_states.mode,
// book_orders.mode): the simulated modes INTERNAL_MATCHING implements.
func AllBookModes() []gates.Mode { return []gates.Mode{gates.ModePaper, gates.ModeSandbox} }

// AllBookInstrumentKinds returns the instrument kinds a book can be opened for
// (book_states.instrument_kind).
func AllBookInstrumentKinds() []instruments.InstrumentType {
	return []instruments.InstrumentType{instruments.TypeSpotPair, instruments.TypeEquity, instruments.TypeEventOutcome}
}

// HouseReservationStatus is the state of a house reservation.
type HouseReservationStatus string

// House reservation statuses.
const (
	HouseReservationActive   HouseReservationStatus = "ACTIVE"
	HouseReservationConsumed HouseReservationStatus = "CONSUMED"
	HouseReservationReleased HouseReservationStatus = "RELEASED"
)

// AllHouseReservationStatuses returns every status
// (book_house_reservations.status).
func AllHouseReservationStatuses() []HouseReservationStatus {
	return []HouseReservationStatus{HouseReservationActive, HouseReservationConsumed, HouseReservationReleased}
}

// Venue labels: every surface that shows a fill says it was simulated, and
// the database admits no other value.
const (
	// VenueLabelInternalBook is book_states.venue_label.
	VenueLabelInternalBook = "SIMULATED_INTERNAL_BOOK"
	// VenueLabelPaper is paper_fills.venue_label.
	VenueLabelPaper = "SIMULATED_PAPER_VENUE"
)

// Identifier kinds.
type (
	orderKind            struct{}
	bookKind             struct{}
	houseReservationKind struct{}
)

// OrderID identifies an order. For a customer order it is the id of the
// economic intent the order executes.
type OrderID = id.ID[orderKind]

// BookID identifies a book.
type BookID = id.ID[bookKind]

// HouseReservationID identifies a house reservation.
type HouseReservationID = id.ID[houseReservationKind]

// ParseOrderID parses the canonical form.
func ParseOrderID(s string) (OrderID, error) { return id.Parse[orderKind](s) }

// Stable rejection reasons of this package. They are stored on an order
// (book_orders.reject_reason), returned in the "reason" field of an error and
// never change meaning. The engine's own reasons (matching.RejectReason) are
// stored as they are.
const (
	ReasonInvalidRequest          = "ORDER_INVALID_REQUEST"
	ReasonInvalidSide             = "ORDER_INVALID_SIDE"
	ReasonInvalidType             = "ORDER_INVALID_TYPE"
	ReasonInvalidTIF              = "ORDER_INVALID_TIME_IN_FORCE"
	ReasonInvalidSTP              = "ORDER_INVALID_STP"
	ReasonMarketGTC               = "ORDER_MARKET_CANNOT_REST"
	ReasonPostOnlyShape           = "ORDER_POST_ONLY_NOT_GTC_LIMIT"
	ReasonReduceOnlyBuy           = "ORDER_REDUCE_ONLY_BUY"
	ReasonPriceRequired           = "ORDER_PRICE_REQUIRED"
	ReasonPriceNotPositive        = "ORDER_PRICE_NOT_POSITIVE"
	ReasonPriceQuoteAsset         = "ORDER_PRICE_WRONG_QUOTE_ASSET"
	ReasonPricePrecision          = "ORDER_PRICE_TOO_PRECISE"
	ReasonPriceOffTick            = "ORDER_PRICE_OFF_TICK"
	ReasonPriceOutOfRange         = "ORDER_PRICE_OUT_OF_RANGE"
	ReasonQtyNotPositive          = "ORDER_QTY_NOT_POSITIVE"
	ReasonQtyOffLot               = "ORDER_QTY_OFF_LOT"
	ReasonQtyBelowMinimum         = "ORDER_QTY_BELOW_MINIMUM"
	ReasonQtyOutOfRange           = "ORDER_QTY_OUT_OF_RANGE"
	ReasonNotionalTooLarge        = "ORDER_NOTIONAL_TOO_LARGE"
	ReasonClientOrderID           = "ORDER_CLIENT_ORDER_ID_INVALID"
	ReasonDuplicateClientOrderID  = "DUPLICATE_CLIENT_ORDER_ID"
	ReasonBookNotOpen             = "ORDER_BOOK_NOT_OPEN"
	ReasonTermsChanged            = "ORDER_INSTRUMENT_TERMS_CHANGED"
	ReasonRouteRefused            = "ORDER_ROUTE_REFUSED"
	ReasonCapability              = "ORDER_CAPABILITY_NOT_APPROVED"
	ReasonRisk                    = "ORDER_RISK_REJECTED"
	ReasonInsufficientFunds       = "ORDER_INSUFFICIENT_FUNDS"
	ReasonReduceOnlyNoPosition    = "ORDER_REDUCE_ONLY_NO_POSITION"
	ReasonReservationLost         = "ORDER_RESERVATION_NOT_ACTIVE"
	ReasonHouseCrossingForbidden  = "ORDER_HOUSE_CUSTOMER_CROSSING_FORBIDDEN"
	ReasonCancelledBeforeBook     = "ORDER_CANCELLED_BEFORE_BOOK"
	ReasonHouseInventory          = "ORDER_HOUSE_INVENTORY_INSUFFICIENT"
	ReasonHouseNotActive          = "ORDER_HOUSE_BOOK_NOT_ACTIVE"
	ReasonNotOwner                = "ORDER_NOT_OWNER"
	ReasonNotOpen                 = "ORDER_NOT_OPEN"
	ReasonScheduleNotInternalBook = "ORDER_SCHEDULE_NOT_ALLOWED_ON_INTERNAL_BOOK"
)
