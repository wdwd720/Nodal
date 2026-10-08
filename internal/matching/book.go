package matching

import (
	"fmt"

	"github.com/nodal/controlplane/internal/errs"
)

// Config configures a Book.
type Config struct {
	// Instrument identifies the book. It is opaque to the engine, recorded
	// in every snapshot, and must be 1..MaxIDBytes bytes.
	Instrument string
	// MaxPriceTicks optionally lowers the price ceiling of this book, for
	// example to 99 for a binary market quoted in cents. Zero means the
	// package MaxPriceTicks; otherwise it must be in [1, MaxPriceTicks].
	MaxPriceTicks int64
}

// Book is the central limit order book of one instrument. It is a
// deterministic state machine: Apply is its only mutator, and the events and
// state it produces depend only on the sequence of commands applied.
//
// A Book is not safe for concurrent use; put a Sequencer in front of it to
// accept commands from many goroutines.
type Book struct {
	instrument string
	maxPrice   int64

	bids ladder
	asks ladder

	orders    map[string]*order            // every OrderID ever consumed
	clientIDs map[clientKey]struct{}       // (owner, client order id) of accepted orders
	byOwner   map[string]map[string]*order // open orders per owner

	cmdSeq   int64
	eventSeq int64
	tradeSeq int64

	cur cmdContext // the command being applied
	out []Event
}

type clientKey struct{ owner, clientID string }

type cmdContext struct {
	seq  int64
	ts   int64
	kind CommandKind
}

// NewBook returns an empty book.
func NewBook(cfg Config) (*Book, error) {
	if !validID(cfg.Instrument) {
		return nil, errs.New(errs.CodeValidationFailed, "matching: instrument must be 1 to 128 bytes").
			WithField("instrument_bytes", len(cfg.Instrument))
	}
	maxPrice := cfg.MaxPriceTicks
	if maxPrice == 0 {
		maxPrice = MaxPriceTicks
	}
	if maxPrice < 1 || maxPrice > MaxPriceTicks {
		return nil, errs.New(errs.CodeValidationFailed, "matching: max price ticks must be in [1, MaxPriceTicks]").
			WithField("max_price_ticks", cfg.MaxPriceTicks)
	}
	return newBook(cfg.Instrument, maxPrice), nil
}

func newBook(instrument string, maxPrice int64) *Book {
	return &Book{
		instrument: instrument,
		maxPrice:   maxPrice,
		bids:       newLadder(SideBuy),
		asks:       newLadder(SideSell),
		orders:     make(map[string]*order),
		clientIDs:  make(map[clientKey]struct{}),
		byOwner:    make(map[string]map[string]*order),
	}
}

// Apply applies one command and returns its sequence number and events.
//
// A business outcome, including every rejection, is reported as events with
// a nil error, and the command counts: it consumes a CommandSeq and belongs
// in the journal. Apply returns an error only for a value that is not a
// command -- nil, a pointer, or a foreign type -- and then changes nothing.
func (b *Book) Apply(cmd Command) (Result, error) {
	switch c := cmd.(type) {
	case NewOrder:
		b.begin(c.Timestamp, CommandNewOrder)
		b.newOrder(c)
	case Cancel:
		b.begin(c.Timestamp, CommandCancel)
		b.cancel(c)
	case Replace:
		b.begin(c.Timestamp, CommandReplace)
		b.replace(c)
	case nil:
		return Result{}, errs.New(errs.CodeValidationFailed, "matching: nil command")
	default:
		return Result{}, errs.New(errs.CodeValidationFailed,
			"matching: unsupported command; pass a NewOrder, Cancel or Replace value").
			WithField("type", fmt.Sprintf("%T", cmd))
	}
	res := Result{CommandSeq: b.cur.seq, Events: b.out}
	b.out = nil
	return res, nil
}

func (b *Book) begin(ts int64, kind CommandKind) {
	b.cmdSeq++
	b.cur = cmdContext{seq: b.cmdSeq, ts: ts, kind: kind}
	b.out = make([]Event, 0, 6)
}

func (b *Book) emit(e Event) {
	b.eventSeq++
	e.Seq = b.eventSeq
	e.CommandSeq = b.cur.seq
	e.Timestamp = b.cur.ts
	e.Command = b.cur.kind
	b.out = append(b.out, e)
}

func (b *Book) side(s Side) *ladder {
	if s == SideBuy {
		return &b.bids
	}
	return &b.asks
}

// crosses reports whether a taker on side with limit (0 = none) would find
// an acceptable opposite order.
func (b *Book) crosses(side Side, limit int64) bool {
	best := b.side(side.Opposite()).best()
	return best != nil && acceptable(side, limit, best.price)
}

// newOrder processes a NewOrder. Event order: ACCEPTED; CANCELLED
// (REDUCE_ONLY_CAP) if capped; then either CANCELLED (FOK), or the matching
// events followed by RESTING + BOOK_DELTA or a CANCELLED for the remainder.
func (b *Book) newOrder(c NewOrder) {
	if reason := b.screenNew(&c); reason != "" {
		if reason != RejectInvalidOrderID && reason != RejectDuplicateOrderID {
			b.orders[c.OrderID] = &order{id: c.OrderID, status: StatusRejected, rejectReason: reason}
		}
		b.emit(newOrderRejected(c, reason))
		return
	}
	o := &order{
		id:              c.OrderID,
		clientID:        c.ClientOrderID,
		owner:           c.OwnerID,
		side:            c.Side,
		typ:             c.Type,
		tif:             c.TIF,
		stp:             c.STP,
		postOnly:        c.PostOnly,
		reduceOnly:      c.ReduceOnly,
		status:          StatusOpen,
		price:           c.PriceTicks,
		qty:             c.QtyLots,
		remaining:       c.QtyLots,
		reduceOnlyLimit: c.ReduceOnlyLimitLots,
		acceptedTS:      c.Timestamp,
	}
	b.orders[o.id] = o
	if o.clientID != "" {
		b.clientIDs[clientKey{o.owner, o.clientID}] = struct{}{}
	}
	b.emit(o.acceptedEvent())

	if o.reduceOnly && o.remaining > o.reduceOnlyLimit {
		excess := o.remaining - o.reduceOnlyLimit
		o.remaining -= excess
		o.cancelled += excess
		b.emit(o.cancelEvent(excess, CancelReduceOnlyCap))
	}
	if o.tif == TIFFOK && b.fillable(o) < o.remaining {
		b.cancelTaker(o, CancelFOK)
		return
	}
	b.match(o)
	if o.remaining == 0 {
		return
	}
	switch {
	case o.typ == OrderTypeMarket:
		b.cancelTaker(o, CancelMarketRemainder)
	case o.tif == TIFIOC:
		b.cancelTaker(o, CancelIOCRemainder)
	case o.tif == TIFFOK:
		// Unreachable: a FOK that passed fillable fills completely. Kept so
		// that no FOK remainder can ever rest.
		b.cancelTaker(o, CancelFOK)
	default:
		b.rest(o)
	}
}

// screenNew returns the reason to reject c, or "" to accept it. Checks run
// in a fixed order: identity, shape, dedupe, then book-dependent checks.
func (b *Book) screenNew(c *NewOrder) RejectReason {
	if !validID(c.OrderID) {
		return RejectInvalidOrderID
	}
	if _, seen := b.orders[c.OrderID]; seen {
		return RejectDuplicateOrderID
	}
	switch {
	case !validID(c.OwnerID):
		return RejectInvalidOwner
	case len(c.ClientOrderID) > MaxIDBytes:
		return RejectInvalidClientOrderID
	case !c.Side.valid():
		return RejectInvalidSide
	case !c.Type.valid():
		return RejectInvalidOrderType
	case !c.TIF.valid() || (c.Type == OrderTypeMarket && c.TIF == TIFGTC):
		return RejectInvalidTIF
	case !c.STP.valid():
		return RejectInvalidSTP
	}
	minPrice := int64(1)
	if c.Type == OrderTypeMarket {
		minPrice = 0
	}
	switch {
	case c.PriceTicks < minPrice || c.PriceTicks > b.maxPrice:
		return RejectInvalidPrice
	case c.QtyLots < 1 || c.QtyLots > MaxQtyLots:
		return RejectInvalidQty
	case c.PriceTicks > 0 && c.QtyLots > MaxNotionalTickLots/c.PriceTicks:
		return RejectNotionalTooLarge
	case c.PostOnly && (c.Type != OrderTypeLimit || c.TIF != TIFGTC):
		return RejectInvalidPostOnly
	case c.ReduceOnly && (c.ReduceOnlyLimitLots < 0 || c.ReduceOnlyLimitLots > MaxQtyLots):
		return RejectInvalidReduceOnly
	case !c.ReduceOnly && c.ReduceOnlyLimitLots != 0:
		return RejectInvalidReduceOnly
	}
	if c.ClientOrderID != "" {
		if _, dup := b.clientIDs[clientKey{c.OwnerID, c.ClientOrderID}]; dup {
			return RejectDuplicateClientOrderID
		}
	}
	if c.ReduceOnly && c.ReduceOnlyLimitLots == 0 {
		return RejectReduceOnlyNoPosition
	}
	if c.PostOnly && b.crosses(c.Side, c.PriceTicks) {
		return RejectPostOnlyWouldCross
	}
	if c.Type == OrderTypeMarket && !b.crosses(c.Side, c.PriceTicks) {
		return RejectMarketNoLiquidity
	}
	if c.Type == OrderTypeLimit && c.TIF == TIFGTC {
		restable := c.QtyLots
		if c.ReduceOnly {
			restable = min(restable, c.ReduceOnlyLimitLots)
		}
		if restable > MaxRestingLotsPerSide-b.side(c.Side).total {
			return RejectBookCapacityExceeded
		}
	}
	return ""
}

// fillable returns how much of t's remaining quantity could execute right
// now, walking the opposite side exactly as match would: in price-time
// priority, within t's limit, skipping orders that STPCancelMaker would
// cancel and stopping at the first self-trade under the other modes. It
// stops counting once the remaining quantity is covered.
func (b *Book) fillable(t *order) int64 {
	opp := b.side(t.side.Opposite())
	var got int64
	for i := len(opp.levels) - 1; i >= 0; i-- {
		lvl := opp.levels[i]
		if !acceptable(t.side, t.price, lvl.price) {
			break
		}
		for m := lvl.head; m != nil; m = m.next {
			if m.owner == t.owner {
				if t.stp == STPCancelMaker {
					continue
				}
				return got
			}
			got += m.remaining
			if got >= t.remaining {
				return got
			}
		}
	}
	return got
}

// match executes t against the opposite side in price-time priority while
// t has remaining quantity and the best opposite price is within t's limit
// (t.price; 0 means none). Self-trade prevention may cancel t.
func (b *Book) match(t *order) {
	opp := b.side(t.side.Opposite())
	for t.remaining > 0 {
		lvl := opp.best()
		if lvl == nil || !acceptable(t.side, t.price, lvl.price) {
			return
		}
		m := lvl.head
		if m.owner == t.owner {
			switch t.stp {
			case STPCancelMaker:
				b.cancelResting(m, CancelSTPMaker)
				continue
			case STPCancelBoth:
				b.cancelResting(m, CancelSTPMaker)
			}
			b.cancelTaker(t, CancelSTPTaker)
			return
		}
		b.fill(t, m)
	}
}

// fill executes one trade between taker t and the maker m at the head of
// the best level. Events: TRADE, maker fill, taker fill, BOOK_DELTA.
func (b *Book) fill(t, m *order) {
	q := min(t.remaining, m.remaining)
	price := m.price
	b.tradeSeq++
	trade := b.tradeSeq
	b.emit(Event{
		Type:          EventTrade,
		TradeSeq:      trade,
		PriceTicks:    price,
		QtyLots:       q,
		MakerOrderID:  m.id,
		MakerOwnerID:  m.owner,
		TakerOrderID:  t.id,
		TakerOwnerID:  t.owner,
		AggressorSide: t.side,
	})
	lvl := m.lvl
	lvl.total -= q
	b.side(m.side).total -= q
	m.applyFill(q)
	if m.remaining == 0 {
		b.unlink(m)
	}
	b.emit(m.fillEvent(price, q, trade))
	t.applyFill(q)
	b.emit(t.fillEvent(price, q, trade))
	b.emit(bookDelta(m.side, price, lvl.total))
}

// rest queues o at the back of its price level. Events: RESTING, BOOK_DELTA.
func (b *Book) rest(o *order) {
	l := b.side(o.side)
	lvl := l.levelAt(o.price)
	o.priority = b.eventSeq + 1 // the Seq of the RESTING event below
	lvl.push(o)
	lvl.total += o.remaining
	l.total += o.remaining
	owned := b.byOwner[o.owner]
	if owned == nil {
		owned = make(map[string]*order)
		b.byOwner[o.owner] = owned
	}
	owned[o.id] = o
	b.emit(o.restingEvent())
	b.emit(bookDelta(o.side, o.price, lvl.total))
}

// unlink takes a resting order off its level (dropping the level when it
// empties) and out of the owner index. Totals are the caller's business.
func (b *Book) unlink(o *order) {
	lvl := o.lvl
	lvl.remove(o)
	if lvl.count == 0 {
		b.side(o.side).drop(lvl)
	}
	o.lvl = nil
	o.priority = 0
	owned := b.byOwner[o.owner]
	delete(owned, o.id)
	if len(owned) == 0 {
		delete(b.byOwner, o.owner)
	}
}

// cancelResting cancels a resting order. Events: CANCELLED, BOOK_DELTA.
func (b *Book) cancelResting(o *order, reason CancelReason) {
	lvl := o.lvl
	q := o.remaining
	lvl.total -= q
	b.side(o.side).total -= q
	b.unlink(o)
	o.closeCancelled(reason)
	b.emit(o.cancelEvent(q, reason))
	b.emit(bookDelta(o.side, o.price, lvl.total))
}

// cancelTaker cancels the remainder of an order that is not resting.
func (b *Book) cancelTaker(o *order, reason CancelReason) {
	q := o.remaining
	o.closeCancelled(reason)
	b.emit(o.cancelEvent(q, reason))
}

// lookupOpen resolves the target of a Cancel or Replace.
func (b *Book) lookupOpen(id, owner string) (*order, RejectReason) {
	if !validID(id) {
		return nil, RejectInvalidOrderID
	}
	if !validID(owner) {
		return nil, RejectInvalidOwner
	}
	o, ok := b.orders[id]
	if !ok || o.status == StatusRejected {
		return nil, RejectUnknownOrder
	}
	if o.owner != owner {
		return nil, RejectNotOwner
	}
	if o.lvl == nil {
		return nil, RejectOrderNotOpen
	}
	return o, ""
}

// cancel processes a Cancel. Events: CANCELLED (USER), BOOK_DELTA.
func (b *Book) cancel(c Cancel) {
	o, reason := b.lookupOpen(c.OrderID, c.OwnerID)
	if reason != "" {
		b.emit(Event{Type: EventRejected, OrderID: c.OrderID, OwnerID: c.OwnerID, RejectReason: reason})
		return
	}
	b.cancelResting(o, CancelUser)
}

// replace processes a Replace.
//
// Keeping priority (same price, quantity not increased): REPLACED, then
// BOOK_DELTA if the quantity changed. Losing priority: REPLACED, BOOK_DELTA
// for the old level if the price changed, the matching events if the new
// price crosses, then RESTING + BOOK_DELTA for whatever remains.
func (b *Book) replace(c Replace) {
	o, reason := b.screenReplace(c)
	if reason != "" {
		b.emit(Event{
			Type:         EventRejected,
			OrderID:      c.OrderID,
			OwnerID:      c.OwnerID,
			PriceTicks:   c.NewPriceTicks,
			QtyLots:      c.NewQtyLots,
			RejectReason: reason,
		})
		return
	}
	prevPrice, prevQty := o.price, o.qty
	newRemaining := c.NewQtyLots - o.filled
	l := b.side(o.side)
	lvl := o.lvl

	if c.NewPriceTicks == o.price && c.NewQtyLots <= o.filled+o.remaining {
		cut := o.remaining - newRemaining
		o.remaining = newRemaining
		o.qty -= cut
		lvl.total -= cut
		l.total -= cut
		b.emit(o.replacedEvent(prevPrice, prevQty, true))
		if cut > 0 {
			b.emit(bookDelta(o.side, o.price, lvl.total))
		}
		return
	}

	lvl.total -= o.remaining
	l.total -= o.remaining
	b.unlink(o)
	o.qty += newRemaining - o.remaining
	o.remaining = newRemaining
	o.price = c.NewPriceTicks
	b.emit(o.replacedEvent(prevPrice, prevQty, false))
	if prevPrice != o.price {
		b.emit(bookDelta(o.side, prevPrice, lvl.total))
	}
	b.match(o)
	if o.remaining > 0 {
		b.rest(o)
	}
}

// screenReplace returns the order to replace, or the reason to reject.
func (b *Book) screenReplace(c Replace) (*order, RejectReason) {
	switch {
	case !validID(c.OrderID):
		return nil, RejectInvalidOrderID
	case !validID(c.OwnerID):
		return nil, RejectInvalidOwner
	case c.NewPriceTicks < 1 || c.NewPriceTicks > b.maxPrice:
		return nil, RejectInvalidPrice
	case c.NewQtyLots < 1 || c.NewQtyLots > MaxQtyLots:
		return nil, RejectInvalidQty
	case c.NewQtyLots > MaxNotionalTickLots/c.NewPriceTicks:
		return nil, RejectNotionalTooLarge
	}
	o, reason := b.lookupOpen(c.OrderID, c.OwnerID)
	if reason != "" {
		return nil, reason
	}
	if c.NewQtyLots <= o.filled {
		return nil, RejectReplaceQtyNotAboveFilled
	}
	if o.reduceOnly && c.NewQtyLots > o.reduceOnlyLimit {
		return nil, RejectReduceOnlyLimitExceeded
	}
	if c.NewPriceTicks == o.price && c.NewQtyLots <= o.filled+o.remaining {
		return o, ""
	}
	if o.postOnly && b.crosses(o.side, c.NewPriceTicks) {
		return nil, RejectPostOnlyWouldCross
	}
	if growth := c.NewQtyLots - o.filled - o.remaining; growth > MaxRestingLotsPerSide-b.side(o.side).total {
		return nil, RejectBookCapacityExceeded
	}
	return o, ""
}
