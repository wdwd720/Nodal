package matching

import "strconv"

// EventType identifies the kind of an Event.
type EventType uint8

// Event types.
const (
	// EventAccepted: a NewOrder passed every check and became an order.
	EventAccepted EventType = 1
	// EventRejected: a command was refused; it is the command's only event.
	EventRejected EventType = 2
	// EventResting: an order joined the back of a price level's queue.
	EventResting EventType = 3
	// EventTrade: one match between a resting maker and an incoming taker.
	EventTrade EventType = 4
	// EventPartialFill: an order traded and still has remaining quantity.
	EventPartialFill EventType = 5
	// EventFullFill: an order traded and has no remaining quantity.
	EventFullFill EventType = 6
	// EventCancelled: quantity of an order was cancelled.
	EventCancelled EventType = 7
	// EventReplaced: a Replace changed an open order's price or quantity.
	EventReplaced EventType = 8
	// EventBookDelta: the aggregate resting quantity at one price changed.
	EventBookDelta EventType = 9
)

// String returns the wire form, e.g. PARTIAL_FILL.
func (t EventType) String() string {
	switch t {
	case EventAccepted:
		return "ACCEPTED"
	case EventRejected:
		return "REJECTED"
	case EventResting:
		return "RESTING"
	case EventTrade:
		return "TRADE"
	case EventPartialFill:
		return "PARTIAL_FILL"
	case EventFullFill:
		return "FULL_FILL"
	case EventCancelled:
		return "CANCELLED"
	case EventReplaced:
		return "REPLACED"
	case EventBookDelta:
		return "BOOK_DELTA"
	}
	return "EventType(" + strconv.Itoa(int(t)) + ")"
}

// Event is one output of the engine. It is a flat, comparable value: the
// fields a type does not use are zero, so two event streams are identical
// exactly when they are equal element by element.
//
// Every event carries Seq (the engine's event sequence: 1-based, gap-free,
// strictly increasing across the life of the book, never reused), and the
// CommandSeq, Command kind and Timestamp of the command that produced it.
//
// Per type, the other fields used are:
//
//   - ACCEPTED: the order's identity (OrderID, ClientOrderID, OwnerID), Side,
//     OrderType, TimeInForce, STP, PostOnly, ReduceOnly, PriceTicks, QtyLots
//     (the order quantity) and RemainingLots (equal to QtyLots).
//   - REJECTED: RejectReason and the command's fields as submitted: for a
//     NewOrder its identity, Side, OrderType, TimeInForce, STP, PostOnly,
//     ReduceOnly, PriceTicks and QtyLots; for a Cancel OrderID and OwnerID;
//     for a Replace OrderID, OwnerID, PriceTicks (NewPriceTicks) and QtyLots
//     (NewQtyLots).
//   - RESTING: the order's identity, Side, PriceTicks, QtyLots (the quantity
//     now resting), FilledLots, RemainingLots and Priority, the order's queue
//     priority, which is the Seq of this very event.
//   - TRADE: TradeSeq (1-based, gap-free), PriceTicks (always the maker's
//     resting price), QtyLots, MakerOrderID, MakerOwnerID, TakerOrderID,
//     TakerOwnerID and AggressorSide (the taker's side).
//   - PARTIAL_FILL and FULL_FILL: emitted for the maker and then the taker
//     after each TRADE: the order's identity, Side, PriceTicks (the trade
//     price), QtyLots (this fill), FilledLots (cumulative), RemainingLots and
//     the TradeSeq that caused it.
//   - CANCELLED: the order's identity, Side, PriceTicks (the order's price),
//     QtyLots (the quantity cancelled now), FilledLots, RemainingLots (zero
//     unless CancelReason is REDUCE_ONLY_CAP) and CancelReason.
//   - REPLACED: the order's identity, Side, PriceTicks and QtyLots (new),
//     PrevPriceTicks and PrevQtyLots (before), FilledLots, RemainingLots and
//     PriorityKept.
//   - BOOK_DELTA: Side, PriceTicks and LevelQtyLots, the new aggregate
//     resting quantity at that price; 0 means the level was removed.
//
// An order's quantity always equals FilledLots + RemainingLots + the sum of
// the QtyLots of its CANCELLED events. A Replace redefines it: its new
// quantity is NewQtyLots plus whatever was already cancelled (only a
// reduce-only cap can cancel part of an order that stays open).
type Event struct {
	Seq        int64
	CommandSeq int64
	Timestamp  int64
	Command    CommandKind
	Type       EventType

	OrderID       string
	ClientOrderID string
	OwnerID       string

	Side        Side
	OrderType   OrderType
	TimeInForce TimeInForce
	STP         STPMode
	PostOnly    bool
	ReduceOnly  bool

	PriceTicks    int64
	QtyLots       int64
	FilledLots    int64
	RemainingLots int64

	RejectReason RejectReason
	CancelReason CancelReason

	PrevPriceTicks int64
	PrevQtyLots    int64
	PriorityKept   bool

	Priority int64

	TradeSeq      int64
	MakerOrderID  string
	MakerOwnerID  string
	TakerOrderID  string
	TakerOwnerID  string
	AggressorSide Side

	LevelQtyLots int64
}

// Result is the outcome of one applied command: its sequence number and the
// events it produced, in order. A command always produces at least one
// event; a rejected command produces exactly one, of type REJECTED.
type Result struct {
	CommandSeq int64
	Events     []Event
}

// Rejected reports whether the command was rejected, and why.
func (r Result) Rejected() (RejectReason, bool) {
	if len(r.Events) == 1 && r.Events[0].Type == EventRejected {
		return r.Events[0].RejectReason, true
	}
	return "", false
}
