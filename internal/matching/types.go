package matching

import "strconv"

// Numeric bounds. They exist so that no int64 arithmetic overflows, neither
// inside the engine nor in a caller working with the values it reports:
//
//   - an order's quantity, filled, remaining and cancelled lots are each at
//     most MaxQtyLots (10^15), and a price at most MaxPriceTicks (10^15);
//   - every order satisfies PriceTicks x QtyLots <= MaxNotionalTickLots
//     (10^18), checked again on every Replace. A trade's quantity never
//     exceeds the maker's quantity and its price is the maker's price, so the
//     notional of any single trade (price x qty, in tick-lots) fits an int64
//     with ninefold headroom. Summing notionals across many trades is the
//     caller's arithmetic and needs its own bound or arbitrary precision
//     (money.Quantity);
//   - the resting quantity of one side of a book, and therefore of every
//     price level, is at most MaxRestingLotsPerSide (10^18). An order that
//     could push its side past it is rejected with BOOK_CAPACITY_EXCEEDED
//     before it touches the book;
//   - identifiers are at most MaxIDBytes bytes;
//   - sequence numbers grow by a bounded amount per command; at a billion
//     events per second an int64 sequence lasts for centuries.
const (
	// MaxPriceTicks is the largest price, in ticks, any order may carry. A
	// Book may lower it with Config.MaxPriceTicks.
	MaxPriceTicks int64 = 1_000_000_000_000_000
	// MaxQtyLots is the largest quantity, in lots, of any single order.
	MaxQtyLots int64 = 1_000_000_000_000_000
	// MaxNotionalTickLots bounds PriceTicks x QtyLots for every order (an
	// unprotected market order has PriceTicks 0 and is not limited by it).
	MaxNotionalTickLots int64 = 1_000_000_000_000_000_000
	// MaxRestingLotsPerSide bounds the total quantity resting on one side.
	MaxRestingLotsPerSide int64 = 1_000_000_000_000_000_000
	// MaxIDBytes bounds every identifier: order, client order, owner and
	// instrument.
	MaxIDBytes = 128
)

// The numeric values of every enum below are part of the snapshot format
// and of any persisted event stream: they never change.

// Side is the side of an order. The zero value is invalid.
type Side uint8

// Sides.
const (
	SideBuy  Side = 1
	SideSell Side = 2
)

func (s Side) valid() bool { return s == SideBuy || s == SideSell }

// Opposite returns the other side. It is only meaningful for a valid side.
func (s Side) Opposite() Side {
	if s == SideBuy {
		return SideSell
	}
	return SideBuy
}

// String returns the wire form: BUY or SELL.
func (s Side) String() string {
	switch s {
	case SideBuy:
		return "BUY"
	case SideSell:
		return "SELL"
	}
	return "Side(" + strconv.Itoa(int(s)) + ")"
}

// OrderType is LIMIT or MARKET. The zero value is invalid.
type OrderType uint8

// Order types.
const (
	OrderTypeLimit  OrderType = 1
	OrderTypeMarket OrderType = 2
)

func (t OrderType) valid() bool { return t == OrderTypeLimit || t == OrderTypeMarket }

// String returns the wire form: LIMIT or MARKET.
func (t OrderType) String() string {
	switch t {
	case OrderTypeLimit:
		return "LIMIT"
	case OrderTypeMarket:
		return "MARKET"
	}
	return "OrderType(" + strconv.Itoa(int(t)) + ")"
}

// TimeInForce is GTC, IOC or FOK. The zero value is invalid. A MARKET order
// must be IOC or FOK: it never rests.
type TimeInForce uint8

// Times in force.
const (
	// TIFGTC rests any unfilled remainder until it is filled or cancelled.
	TIFGTC TimeInForce = 1
	// TIFIOC fills what it can immediately and cancels the remainder.
	TIFIOC TimeInForce = 2
	// TIFFOK fills its whole quantity immediately or does not trade at all.
	TIFFOK TimeInForce = 3
)

func (t TimeInForce) valid() bool { return t >= TIFGTC && t <= TIFFOK }

// String returns the wire form: GTC, IOC or FOK.
func (t TimeInForce) String() string {
	switch t {
	case TIFGTC:
		return "GTC"
	case TIFIOC:
		return "IOC"
	case TIFFOK:
		return "FOK"
	}
	return "TimeInForce(" + strconv.Itoa(int(t)) + ")"
}

// STPMode is the self-trade prevention mode of an incoming (taker) order. A
// self-trade is a match between two orders with the same OwnerID; it never
// executes. The zero value is the default, STPCancelTaker.
type STPMode uint8

// Self-trade prevention modes.
const (
	// STPCancelTaker cancels the incoming order's remaining quantity and
	// leaves the resting order untouched. Matching stops.
	STPCancelTaker STPMode = 0
	// STPCancelMaker cancels the resting order and keeps matching the
	// incoming order against the orders behind it.
	STPCancelMaker STPMode = 1
	// STPCancelBoth cancels the resting order and the incoming order's
	// remaining quantity. Matching stops.
	STPCancelBoth STPMode = 2
)

func (m STPMode) valid() bool { return m <= STPCancelBoth }

// String returns the wire form: CANCEL_TAKER, CANCEL_MAKER or CANCEL_BOTH.
func (m STPMode) String() string {
	switch m {
	case STPCancelTaker:
		return "CANCEL_TAKER"
	case STPCancelMaker:
		return "CANCEL_MAKER"
	case STPCancelBoth:
		return "CANCEL_BOTH"
	}
	return "STPMode(" + strconv.Itoa(int(m)) + ")"
}

// OrderStatus is the lifecycle state of an order between commands. OPEN and
// PARTIALLY_FILLED orders rest on the book; the others are terminal.
type OrderStatus uint8

// Order statuses.
const (
	StatusOpen            OrderStatus = 1
	StatusPartiallyFilled OrderStatus = 2
	StatusFilled          OrderStatus = 3
	StatusCancelled       OrderStatus = 4
	StatusRejected        OrderStatus = 5
)

func (s OrderStatus) valid() bool { return s >= StatusOpen && s <= StatusRejected }

// IsOpen reports whether an order in this status rests on the book.
func (s OrderStatus) IsOpen() bool { return s == StatusOpen || s == StatusPartiallyFilled }

// String returns the wire form.
func (s OrderStatus) String() string {
	switch s {
	case StatusOpen:
		return "OPEN"
	case StatusPartiallyFilled:
		return "PARTIALLY_FILLED"
	case StatusFilled:
		return "FILLED"
	case StatusCancelled:
		return "CANCELLED"
	case StatusRejected:
		return "REJECTED"
	}
	return "OrderStatus(" + strconv.Itoa(int(s)) + ")"
}

// RejectReason is the stable, machine-readable reason of a REJECTED event.
// A rejected command changes nothing except the sequence counters and, for
// a NewOrder with a well-formed, unseen OrderID, the set of consumed order
// IDs. Values never change meaning and are never removed.
type RejectReason string

// Reject reasons for NewOrder.
const (
	RejectInvalidOrderID       RejectReason = "INVALID_ORDER_ID"
	RejectInvalidOwner         RejectReason = "INVALID_OWNER"
	RejectInvalidClientOrderID RejectReason = "INVALID_CLIENT_ORDER_ID"
	RejectInvalidSide          RejectReason = "INVALID_SIDE"
	RejectInvalidOrderType     RejectReason = "INVALID_ORDER_TYPE"
	// RejectInvalidTIF also covers a MARKET order with GTC.
	RejectInvalidTIF RejectReason = "INVALID_TIF"
	RejectInvalidSTP RejectReason = "INVALID_STP"
	// RejectInvalidPrice: a LIMIT price outside [1, max] or a MARKET
	// protection price outside [0, max], max being the book's ceiling.
	RejectInvalidPrice RejectReason = "INVALID_PRICE"
	// RejectInvalidQty: a quantity outside [1, MaxQtyLots].
	RejectInvalidQty RejectReason = "INVALID_QTY"
	// RejectNotionalTooLarge: PriceTicks x QtyLots exceeds MaxNotionalTickLots.
	RejectNotionalTooLarge RejectReason = "NOTIONAL_TOO_LARGE"
	// RejectInvalidPostOnly: PostOnly on anything but a GTC LIMIT order.
	RejectInvalidPostOnly RejectReason = "INVALID_POST_ONLY"
	// RejectInvalidReduceOnly: ReduceOnlyLimitLots outside [0, MaxQtyLots],
	// or non-zero without ReduceOnly.
	RejectInvalidReduceOnly RejectReason = "INVALID_REDUCE_ONLY"
	// RejectDuplicateOrderID: the OrderID was carried by an earlier NewOrder,
	// whatever happened to it.
	RejectDuplicateOrderID RejectReason = "DUPLICATE_ORDER_ID"
	// RejectDuplicateClientOrderID: the owner already used this non-empty
	// ClientOrderID on an accepted order.
	RejectDuplicateClientOrderID RejectReason = "DUPLICATE_CLIENT_ORDER_ID"
	// RejectReduceOnlyNoPosition: ReduceOnly with ReduceOnlyLimitLots 0.
	RejectReduceOnlyNoPosition RejectReason = "REDUCE_ONLY_NO_POSITION"
	// RejectPostOnlyWouldCross: a post-only order (or the Replace of one)
	// would match on entry.
	RejectPostOnlyWouldCross RejectReason = "POST_ONLY_WOULD_CROSS"
	// RejectMarketNoLiquidity: a MARKET order found no opposite order within
	// its protection price on entry.
	RejectMarketNoLiquidity RejectReason = "MARKET_NO_LIQUIDITY"
	// RejectBookCapacityExceeded: resting the order (or the Replace) could
	// push its side past MaxRestingLotsPerSide.
	RejectBookCapacityExceeded RejectReason = "BOOK_CAPACITY_EXCEEDED"
)

// Reject reasons for Cancel and Replace (with the ID and quantity reasons
// above).
const (
	// RejectUnknownOrder: no order with this OrderID was ever accepted.
	RejectUnknownOrder RejectReason = "UNKNOWN_ORDER"
	// RejectNotOwner: the order belongs to a different OwnerID.
	RejectNotOwner RejectReason = "NOT_OWNER"
	// RejectOrderNotOpen: the order is already filled or cancelled.
	RejectOrderNotOpen RejectReason = "ORDER_NOT_OPEN"
	// RejectReplaceQtyNotAboveFilled: a Replace's NewQtyLots is at or below
	// the quantity already filled, which would leave nothing to work.
	RejectReplaceQtyNotAboveFilled RejectReason = "REPLACE_QTY_NOT_ABOVE_FILLED"
	// RejectReduceOnlyLimitExceeded: a Replace would raise a reduce-only
	// order's quantity above its ReduceOnlyLimitLots.
	RejectReduceOnlyLimitExceeded RejectReason = "REDUCE_ONLY_LIMIT_EXCEEDED"
)

// known reports whether r is one of the reasons above.
func (r RejectReason) known() bool {
	switch r {
	case RejectInvalidOrderID, RejectInvalidOwner, RejectInvalidClientOrderID,
		RejectInvalidSide, RejectInvalidOrderType, RejectInvalidTIF, RejectInvalidSTP,
		RejectInvalidPrice, RejectInvalidQty, RejectNotionalTooLarge,
		RejectInvalidPostOnly, RejectInvalidReduceOnly,
		RejectDuplicateOrderID, RejectDuplicateClientOrderID,
		RejectReduceOnlyNoPosition, RejectPostOnlyWouldCross,
		RejectMarketNoLiquidity, RejectBookCapacityExceeded,
		RejectUnknownOrder, RejectNotOwner, RejectOrderNotOpen,
		RejectReplaceQtyNotAboveFilled, RejectReduceOnlyLimitExceeded:
		return true
	}
	return false
}

// CancelReason is the stable, machine-readable reason of a CANCELLED event.
type CancelReason string

// Cancel reasons.
const (
	// CancelUser: a Cancel command from the order's owner.
	CancelUser CancelReason = "USER"
	// CancelIOCRemainder: the unfilled remainder of an IOC limit order.
	CancelIOCRemainder CancelReason = "IOC_REMAINDER"
	// CancelMarketRemainder: the unfilled remainder of a MARKET order (book
	// exhausted, or protection price reached).
	CancelMarketRemainder CancelReason = "MARKET_REMAINDER"
	// CancelFOK: a FOK order whose whole quantity was not fillable on
	// entry. It is cancelled with zero fills and nothing else changes.
	CancelFOK CancelReason = "FOK"
	// CancelSTPTaker: the incoming order's remainder, by self-trade
	// prevention.
	CancelSTPTaker CancelReason = "STP_TAKER"
	// CancelSTPMaker: a resting order, by the self-trade prevention mode of
	// an incoming order from the same owner.
	CancelSTPMaker CancelReason = "STP_MAKER"
	// CancelReduceOnlyCap: the part of a reduce-only order above its
	// ReduceOnlyLimitLots, cancelled on entry. It is the only cancellation
	// that leaves the order open.
	CancelReduceOnlyCap CancelReason = "REDUCE_ONLY_CAP"
)

// known reports whether r is one of the reasons above.
func (r CancelReason) known() bool {
	switch r {
	case CancelUser, CancelIOCRemainder, CancelMarketRemainder, CancelFOK,
		CancelSTPTaker, CancelSTPMaker, CancelReduceOnlyCap:
		return true
	}
	return false
}

// validID reports whether s is a well-formed required identifier.
func validID(s string) bool { return len(s) >= 1 && len(s) <= MaxIDBytes }
