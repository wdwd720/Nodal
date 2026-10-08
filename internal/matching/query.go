package matching

import (
	"cmp"
	"crypto/sha256"
	"slices"
)

// Level is the aggregate of one price level.
type Level struct {
	PriceTicks int64
	// QtyLots is the total remaining quantity resting at the price.
	QtyLots int64
	// Orders is the number of orders resting at the price.
	Orders int
}

// Depth is an aggregated view of the top of the book, best level first on
// each side (bids by descending price, asks by ascending price).
type Depth struct {
	Bids []Level
	Asks []Level
}

// Reader is the read-only query surface of a Book. Sequencer.Read hands one
// out under its lock.
type Reader interface {
	Config() Config
	CommandSeq() int64
	EventSeq() int64
	TradeSeq() int64
	BestBid() (Level, bool)
	BestAsk() (Level, bool)
	Depth(n int) Depth
	Order(orderID string) (OrderState, bool)
	OpenOrders(ownerID string) []OrderState
	Snapshot() []byte
	StateHash() [32]byte
}

var _ Reader = (*Book)(nil)

// Config returns the book's configuration with the effective price ceiling.
func (b *Book) Config() Config {
	return Config{Instrument: b.instrument, MaxPriceTicks: b.maxPrice}
}

// CommandSeq returns the sequence number of the last applied command (0
// before the first).
func (b *Book) CommandSeq() int64 { return b.cmdSeq }

// EventSeq returns the Seq of the last emitted event.
func (b *Book) EventSeq() int64 { return b.eventSeq }

// TradeSeq returns the TradeSeq of the last trade.
func (b *Book) TradeSeq() int64 { return b.tradeSeq }

// BestBid returns the highest bid level, if any.
func (b *Book) BestBid() (Level, bool) { return bestOf(&b.bids) }

// BestAsk returns the lowest ask level, if any.
func (b *Book) BestAsk() (Level, bool) { return bestOf(&b.asks) }

func bestOf(l *ladder) (Level, bool) {
	lvl := l.best()
	if lvl == nil {
		return Level{}, false
	}
	return Level{PriceTicks: lvl.price, QtyLots: lvl.total, Orders: lvl.count}, true
}

// Depth returns up to n aggregated levels per side, best first. The slices
// are never nil.
func (b *Book) Depth(n int) Depth {
	return Depth{Bids: b.bids.top(n), Asks: b.asks.top(n)}
}

// Order returns the state of any order whose OrderID the book has consumed:
// open, terminal or rejected.
func (b *Book) Order(orderID string) (OrderState, bool) {
	o, ok := b.orders[orderID]
	if !ok {
		return OrderState{}, false
	}
	return o.state(), true
}

// OpenOrders returns the owner's resting orders in queue-priority order,
// oldest first. The slice is never nil.
func (b *Book) OpenOrders(ownerID string) []OrderState {
	owned := b.byOwner[ownerID]
	out := make([]OrderState, 0, len(owned))
	for _, o := range owned {
		out = append(out, o.state())
	}
	// Priorities of open orders are unique, so this order is total.
	slices.SortFunc(out, func(x, y OrderState) int { return cmp.Compare(x.Priority, y.Priority) })
	return out
}

// StateHash returns the SHA-256 of Snapshot().
func (b *Book) StateHash() [32]byte { return sha256.Sum256(b.Snapshot()) }

// readView exposes only the Reader methods of a Book, so a caller of
// Sequencer.Read cannot reach Apply.
type readView struct{ b *Book }

func (r readView) Config() Config                     { return r.b.Config() }
func (r readView) CommandSeq() int64                  { return r.b.CommandSeq() }
func (r readView) EventSeq() int64                    { return r.b.EventSeq() }
func (r readView) TradeSeq() int64                    { return r.b.TradeSeq() }
func (r readView) BestBid() (Level, bool)             { return r.b.BestBid() }
func (r readView) BestAsk() (Level, bool)             { return r.b.BestAsk() }
func (r readView) Depth(n int) Depth                  { return r.b.Depth(n) }
func (r readView) Order(id string) (OrderState, bool) { return r.b.Order(id) }
func (r readView) OpenOrders(owner string) []OrderState {
	return r.b.OpenOrders(owner)
}
func (r readView) Snapshot() []byte    { return r.b.Snapshot() }
func (r readView) StateHash() [32]byte { return r.b.StateHash() }
