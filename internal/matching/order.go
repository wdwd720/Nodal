package matching

// order is the engine's record of one OrderID. Every OrderID ever consumed
// keeps its record for the life of the book: open orders are also linked into
// their price level, terminal ones are kept for Order queries and for the
// OrderID and ClientOrderID dedupe rules. A rejected NewOrder keeps only its
// id, status and reject reason.
//
// Quantity conservation, at all times: qty == filled + remaining + cancelled.
type order struct {
	id       string
	clientID string
	owner    string

	side       Side
	typ        OrderType
	tif        TimeInForce
	stp        STPMode
	postOnly   bool
	reduceOnly bool
	status     OrderStatus

	price           int64 // limit price, or a market order's protection (0 = none)
	qty             int64
	filled          int64
	remaining       int64
	cancelled       int64
	reduceOnlyLimit int64
	priority        int64 // Seq of the RESTING event that queued it; 0 when not resting
	acceptedTS      int64

	rejectReason RejectReason
	cancelReason CancelReason // the reason that closed a CANCELLED order

	// Intrusive FIFO links; lvl is nil unless the order rests.
	prev, next *order
	lvl        *level
}

const (
	flagPostOnly   = 1 << 0
	flagReduceOnly = 1 << 1
)

func (o *order) flags() byte {
	var f byte
	if o.postOnly {
		f |= flagPostOnly
	}
	if o.reduceOnly {
		f |= flagReduceOnly
	}
	return f
}

// applyFill moves q lots from remaining to filled.
func (o *order) applyFill(q int64) {
	o.filled += q
	o.remaining -= q
	if o.remaining == 0 {
		o.status = StatusFilled
	} else {
		o.status = StatusPartiallyFilled
	}
}

// closeCancelled cancels the whole remaining quantity and closes the order.
func (o *order) closeCancelled(reason CancelReason) {
	o.cancelled += o.remaining
	o.remaining = 0
	o.status = StatusCancelled
	o.cancelReason = reason
}

// event returns an event of type t carrying the order's identity and
// current quantities.
func (o *order) event(t EventType) Event {
	return Event{
		Type:          t,
		OrderID:       o.id,
		ClientOrderID: o.clientID,
		OwnerID:       o.owner,
		Side:          o.side,
		PriceTicks:    o.price,
		FilledLots:    o.filled,
		RemainingLots: o.remaining,
	}
}

func (o *order) acceptedEvent() Event {
	e := o.event(EventAccepted)
	e.OrderType = o.typ
	e.TimeInForce = o.tif
	e.STP = o.stp
	e.PostOnly = o.postOnly
	e.ReduceOnly = o.reduceOnly
	e.QtyLots = o.qty
	return e
}

func (o *order) restingEvent() Event {
	e := o.event(EventResting)
	e.QtyLots = o.remaining
	e.Priority = o.priority
	return e
}

func (o *order) fillEvent(price, q, tradeSeq int64) Event {
	t := EventPartialFill
	if o.remaining == 0 {
		t = EventFullFill
	}
	e := o.event(t)
	e.PriceTicks = price
	e.QtyLots = q
	e.TradeSeq = tradeSeq
	return e
}

func (o *order) cancelEvent(q int64, reason CancelReason) Event {
	e := o.event(EventCancelled)
	e.QtyLots = q
	e.CancelReason = reason
	return e
}

func (o *order) replacedEvent(prevPrice, prevQty int64, kept bool) Event {
	e := o.event(EventReplaced)
	e.QtyLots = o.qty
	e.PrevPriceTicks = prevPrice
	e.PrevQtyLots = prevQty
	e.PriorityKept = kept
	return e
}

func bookDelta(side Side, price, levelQty int64) Event {
	return Event{Type: EventBookDelta, Side: side, PriceTicks: price, LevelQtyLots: levelQty}
}

func newOrderRejected(c NewOrder, reason RejectReason) Event {
	return Event{
		Type:          EventRejected,
		OrderID:       c.OrderID,
		ClientOrderID: c.ClientOrderID,
		OwnerID:       c.OwnerID,
		Side:          c.Side,
		OrderType:     c.Type,
		TimeInForce:   c.TIF,
		STP:           c.STP,
		PostOnly:      c.PostOnly,
		ReduceOnly:    c.ReduceOnly,
		PriceTicks:    c.PriceTicks,
		QtyLots:       c.QtyLots,
		RejectReason:  reason,
	}
}

// OrderState is a copy of the engine's record of one order.
type OrderState struct {
	OrderID       string
	ClientOrderID string
	OwnerID       string

	Side                Side
	Type                OrderType
	TIF                 TimeInForce
	STP                 STPMode
	PostOnly            bool
	ReduceOnly          bool
	ReduceOnlyLimitLots int64

	// PriceTicks is the limit price, or a MARKET order's protection price.
	PriceTicks int64
	// QtyLots == FilledLots + RemainingLots + CancelledLots.
	QtyLots       int64
	FilledLots    int64
	RemainingLots int64
	CancelledLots int64

	Status OrderStatus
	// Priority is the queue priority of a resting order (the Seq of its
	// RESTING event; lower is earlier); 0 when the order does not rest.
	Priority int64
	// AcceptedTimestamp is the Timestamp of the NewOrder that created it.
	AcceptedTimestamp int64

	// RejectReason is set for REJECTED orders, which keep nothing else but
	// their OrderID.
	RejectReason RejectReason
	// CancelReason is the reason that closed a CANCELLED order.
	CancelReason CancelReason
}

func (o *order) state() OrderState {
	return OrderState{
		OrderID:             o.id,
		ClientOrderID:       o.clientID,
		OwnerID:             o.owner,
		Side:                o.side,
		Type:                o.typ,
		TIF:                 o.tif,
		STP:                 o.stp,
		PostOnly:            o.postOnly,
		ReduceOnly:          o.reduceOnly,
		ReduceOnlyLimitLots: o.reduceOnlyLimit,
		PriceTicks:          o.price,
		QtyLots:             o.qty,
		FilledLots:          o.filled,
		RemainingLots:       o.remaining,
		CancelledLots:       o.cancelled,
		Status:              o.status,
		Priority:            o.priority,
		AcceptedTimestamp:   o.acceptedTS,
		RejectReason:        o.rejectReason,
		CancelReason:        o.cancelReason,
	}
}
