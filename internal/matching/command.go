package matching

import "strconv"

// CommandKind identifies the kind of a Command.
type CommandKind uint8

// Command kinds.
const (
	CommandNewOrder CommandKind = 1
	CommandCancel   CommandKind = 2
	CommandReplace  CommandKind = 3
)

// String returns the wire form: NEW_ORDER, CANCEL or REPLACE.
func (k CommandKind) String() string {
	switch k {
	case CommandNewOrder:
		return "NEW_ORDER"
	case CommandCancel:
		return "CANCEL"
	case CommandReplace:
		return "REPLACE"
	}
	return "CommandKind(" + strconv.Itoa(int(k)) + ")"
}

// Command is one input to a Book: NewOrder, Cancel or Replace, always
// passed by value. Commands are plain values, so a journaled command can
// never change after it was applied. Every command carries a caller-supplied
// Timestamp (Unix nanoseconds) that the engine copies onto its events and
// otherwise ignores: the engine never reads a clock, and a timestamp does not
// influence matching.
type Command interface {
	// Kind returns the command's kind.
	Kind() CommandKind
	isCommand()
}

// NewOrder submits an order.
type NewOrder struct {
	// Timestamp is the caller's time for this command, in Unix nanoseconds.
	Timestamp int64
	// OrderID is the order's engine-wide identity, 1..MaxIDBytes bytes. It
	// is consumed by the first NewOrder that carries it, whatever the
	// outcome, so a retried submission can never create a second order.
	OrderID string
	// ClientOrderID is the owner's own reference, 0..MaxIDBytes bytes.
	// When non-empty it must be unique per owner among accepted orders.
	ClientOrderID string
	// OwnerID identifies the account, 1..MaxIDBytes bytes. It scopes Cancel
	// and Replace and defines self-trades.
	OwnerID string
	Side    Side
	Type    OrderType
	TIF     TimeInForce
	// PriceTicks is the limit price of a LIMIT order (>= 1). For a MARKET
	// order it is an optional protection price: the order never trades
	// beyond it; 0 means no protection.
	PriceTicks int64
	// QtyLots is the order quantity, in [1, MaxQtyLots].
	QtyLots int64
	// PostOnly orders (GTC LIMIT only) are rejected if they would match on
	// entry.
	PostOnly bool
	// ReduceOnly orders never fill more than ReduceOnlyLimitLots, which the
	// caller computed from its position; any quantity above it is cancelled
	// on entry (REDUCE_ONLY_CAP) and a limit of 0 is a rejection
	// (REDUCE_ONLY_NO_POSITION). The engine knows nothing of positions: if
	// the position changes while the order rests, the caller cancels or
	// replaces it.
	ReduceOnly bool
	// ReduceOnlyLimitLots must be 0 unless ReduceOnly is set.
	ReduceOnlyLimitLots int64
	// STP is the self-trade prevention mode applied when this order takes
	// liquidity. The zero value is STPCancelTaker.
	STP STPMode
}

// Kind implements Command.
func (NewOrder) Kind() CommandKind { return CommandNewOrder }
func (NewOrder) isCommand()        {}

// Cancel cancels the whole remaining quantity of an open order.
type Cancel struct {
	Timestamp int64
	OrderID   string
	// OwnerID must be the order's owner.
	OwnerID string
}

// Kind implements Command.
func (Cancel) Kind() CommandKind { return CommandCancel }
func (Cancel) isCommand()        {}

// Replace changes the price and quantity of an open order, keeping its
// OrderID. NewQtyLots is the new total order quantity including what has
// already been filled, so the new remaining quantity is NewQtyLots minus the
// filled quantity, which must leave something to work.
//
// Reducing the quantity at an unchanged price keeps the order's place in the
// queue. Any price change or quantity increase loses it: the order leaves its
// level and re-enters at the new price with a fresh priority, exactly as a
// new GTC limit order under the same OrderID would -- if the new price
// crosses the opposite side the order trades as the aggressor first, and a
// post-only order is rejected instead.
type Replace struct {
	Timestamp     int64
	OrderID       string
	OwnerID       string
	NewPriceTicks int64
	NewQtyLots    int64
}

// Kind implements Command.
func (Replace) Kind() CommandKind { return CommandReplace }
func (Replace) isCommand()        {}
