package matching

import (
	"slices"

	"github.com/nodal/controlplane/internal/errs"
)

// verify checks every structural invariant of the book in O(N log N) and
// returns the first violation found, scanning in a deterministic order.
// Restore runs it on every decoded snapshot; the tests run it after every
// command. The live engine does not need it: each command preserves the
// invariants by construction.
//
// Invariants: the configuration and sequence counters are in range; each
// side's levels are strictly ordered, indexed and non-empty; each level's
// FIFO is correctly linked, holds only open orders of its side and price in
// strictly increasing priority, and its count and total match its orders;
// each side's total matches its levels and stays within
// MaxRestingLotsPerSide; the book is not crossed; open-order priorities are
// unique and no later than the last event; every record is internally
// consistent (checkRecord); an order rests exactly when its status is open;
// and the client-order-id set and the owner index hold exactly the accepted
// and the open orders respectively.
func (b *Book) verify() error {
	if !validID(b.instrument) || b.maxPrice < 1 || b.maxPrice > MaxPriceTicks {
		return violation("configuration out of range")
	}
	if b.cmdSeq < 0 || b.eventSeq < b.cmdSeq || b.tradeSeq < 0 || b.tradeSeq > b.eventSeq {
		return violation("sequence counters out of range")
	}
	var priorities []int64
	open := 0
	for _, l := range [...]*ladder{&b.bids, &b.asks} {
		n, err := b.verifySide(l, &priorities)
		if err != nil {
			return err
		}
		open += n
	}
	if bid, ask := b.bids.best(), b.asks.best(); bid != nil && ask != nil && bid.price >= ask.price {
		return violation("book is crossed")
	}
	slices.Sort(priorities)
	for i := 1; i < len(priorities); i++ {
		if priorities[i] == priorities[i-1] {
			return violation("two resting orders share a priority")
		}
	}

	ids := make([]string, 0, len(b.orders))
	for id := range b.orders {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	clients, openRecords := 0, 0
	for _, id := range ids {
		o := b.orders[id]
		if o == nil || o.id != id {
			return violation("order index does not match its records")
		}
		if msg := checkRecord(o, b.maxPrice); msg != "" {
			return violation("order " + id + ": " + msg)
		}
		if o.status == StatusRejected {
			continue
		}
		if o.status.IsOpen() != (o.lvl != nil) {
			return violation("order " + id + ": rests exactly when open")
		}
		if o.status.IsOpen() {
			openRecords++
		}
		if o.clientID != "" {
			clients++
			if _, ok := b.clientIDs[clientKey{o.owner, o.clientID}]; !ok {
				return violation("order " + id + ": client order id not indexed")
			}
		}
	}
	if clients != len(b.clientIDs) {
		return violation("client order id index does not match accepted orders")
	}
	if openRecords != open {
		return violation("open orders missing from the book")
	}
	owned := 0
	for _, m := range b.byOwner {
		if len(m) == 0 {
			return violation("empty owner index entry")
		}
		owned += len(m)
	}
	if owned != open {
		return violation("owner index does not match open orders")
	}
	return nil
}

// verifySide checks one ladder and returns its number of resting orders.
func (b *Book) verifySide(l *ladder, priorities *[]int64) (int, error) {
	side := l.side.String()
	if len(l.byPrice) != len(l.levels) {
		return 0, violation(side + ": level index size differs from level count")
	}
	var sideTotal int64
	open := 0
	for i, lvl := range l.levels {
		if i > 0 && l.key(l.levels[i-1].price) >= l.key(lvl.price) {
			return 0, violation(side + ": levels not in strict price order")
		}
		if l.byPrice[lvl.price] != lvl {
			return 0, violation(side + ": level index mismatch")
		}
		if lvl.price < 1 || lvl.price > b.maxPrice {
			return 0, violation(side + ": level price out of range")
		}
		var sum, last int64
		count := 0
		var prev *order
		for o := lvl.head; o != nil; o = o.next {
			if count == len(b.orders) {
				return 0, violation(side + ": level queue does not terminate")
			}
			switch {
			case o.prev != prev || o.lvl != lvl:
				return 0, violation(side + ": broken queue links")
			case o.side != l.side || o.price != lvl.price:
				return 0, violation(side + ": order queued at the wrong side or price")
			case !o.status.IsOpen() || o.remaining < 1:
				return 0, violation(side + ": closed order in the queue")
			case o.priority <= last || o.priority > b.eventSeq:
				return 0, violation(side + ": queue not in strictly increasing priority (FIFO)")
			case b.orders[o.id] != o:
				return 0, violation(side + ": queued order not in the order index")
			case b.byOwner[o.owner][o.id] != o:
				return 0, violation(side + ": queued order not in the owner index")
			case o.remaining > MaxRestingLotsPerSide-sum:
				return 0, violation(side + ": level total exceeds capacity")
			}
			sum += o.remaining
			last = o.priority
			*priorities = append(*priorities, o.priority)
			prev = o
			count++
		}
		if count == 0 || prev != lvl.tail || count != lvl.count || sum != lvl.total {
			return 0, violation(side + ": level aggregate differs from its orders")
		}
		if sum > MaxRestingLotsPerSide-sideTotal {
			return 0, violation(side + ": side total exceeds capacity")
		}
		sideTotal += sum
		open += count
	}
	if sideTotal != l.total {
		return 0, violation(side + ": side total differs from its levels")
	}
	return open, nil
}

// checkRecord validates one order record in isolation and returns a
// description of the first violation, or "".
func checkRecord(o *order, maxPrice int64) string {
	if !validID(o.id) {
		return "order id length"
	}
	if o.status == StatusRejected {
		if !o.rejectReason.known() || o.rejectReason == RejectInvalidOrderID || o.rejectReason == RejectDuplicateOrderID {
			return "reject reason"
		}
		if *o != (order{id: o.id, status: o.status, rejectReason: o.rejectReason}) {
			return "rejected record carries order data"
		}
		return ""
	}
	switch {
	case !o.status.valid():
		return "status"
	case !validID(o.owner):
		return "owner length"
	case len(o.clientID) > MaxIDBytes:
		return "client order id length"
	case !o.side.valid() || !o.typ.valid() || !o.tif.valid() || !o.stp.valid():
		return "enum value"
	case o.typ == OrderTypeMarket && o.tif == TIFGTC:
		return "market order with GTC"
	case o.rejectReason != "":
		return "reject reason on an accepted order"
	}
	minPrice := int64(1)
	if o.typ == OrderTypeMarket {
		minPrice = 0
	}
	if o.price < minPrice || o.price > maxPrice {
		return "price out of range"
	}
	for _, q := range [...]int64{o.filled, o.remaining, o.cancelled} {
		if q < 0 || q > MaxQtyLots {
			return "quantity out of range"
		}
	}
	if o.qty != o.filled+o.remaining+o.cancelled {
		return "quantity not conserved"
	}
	if o.qty < 1 || o.qty > MaxQtyLots {
		return "order quantity out of range"
	}
	working := o.filled + o.remaining
	if o.price > 0 && working > MaxNotionalTickLots/o.price {
		return "notional out of range"
	}
	if o.postOnly && (o.typ != OrderTypeLimit || o.tif != TIFGTC) {
		return "post-only on a non-GTC or non-limit order"
	}
	if o.reduceOnly {
		if o.reduceOnlyLimit < 1 || o.reduceOnlyLimit > MaxQtyLots || working > o.reduceOnlyLimit {
			return "reduce-only limit"
		}
	} else if o.reduceOnlyLimit != 0 {
		return "reduce-only limit without reduce-only"
	} else if o.cancelled > 0 && o.status != StatusCancelled {
		return "partial cancellation without reduce-only"
	}
	switch o.status {
	case StatusOpen:
		if o.remaining == 0 || o.filled != 0 {
			return "OPEN status with fills or nothing remaining"
		}
	case StatusPartiallyFilled:
		if o.remaining == 0 || o.filled == 0 {
			return "PARTIALLY_FILLED status inconsistent"
		}
	case StatusFilled:
		if o.remaining != 0 || o.filled == 0 {
			return "FILLED status inconsistent"
		}
	case StatusCancelled:
		if o.remaining != 0 || o.cancelled == 0 {
			return "CANCELLED status inconsistent"
		}
	}
	if o.status.IsOpen() {
		if o.typ != OrderTypeLimit || o.tif != TIFGTC || o.priority < 1 {
			return "resting order is not a prioritised GTC limit order"
		}
	} else if o.priority != 0 {
		return "priority on a closed order"
	}
	if o.status == StatusCancelled {
		if !o.cancelReason.known() || o.cancelReason == CancelReduceOnlyCap {
			return "cancel reason"
		}
	} else if o.cancelReason != "" {
		return "cancel reason on an order that is not cancelled"
	}
	return ""
}

func violation(what string) error {
	return errs.New(errs.CodeInternal, "matching: invariant violated: "+what)
}
