package matching

// level is one price level: an intrusive doubly-linked FIFO of resting
// orders, oldest (best priority) at head. total is the sum of the orders'
// remaining quantity; list operations never touch it, callers do.
type level struct {
	price      int64
	total      int64
	count      int
	head, tail *order
}

// push appends o at the back of the queue.
func (lvl *level) push(o *order) {
	o.lvl = lvl
	o.prev = lvl.tail
	o.next = nil
	if lvl.tail != nil {
		lvl.tail.next = o
	} else {
		lvl.head = o
	}
	lvl.tail = o
	lvl.count++
}

// remove unlinks o in O(1).
func (lvl *level) remove(o *order) {
	if o.prev != nil {
		o.prev.next = o.next
	} else {
		lvl.head = o.next
	}
	if o.next != nil {
		o.next.prev = o.prev
	} else {
		lvl.tail = o.prev
	}
	o.prev, o.next = nil, nil
	lvl.count--
}

// ladder is one side of the book: price levels sorted from worst to best,
// so the best level is the last element and the common operations -- reading
// the touch, consuming it, and adding a level near it -- touch the end of the
// slice. byPrice gives O(1) access to a level; locating a level's position
// is a binary search, O(log L) for L levels.
type ladder struct {
	side    Side
	levels  []*level
	byPrice map[int64]*level
	total   int64 // resting quantity of the whole side
}

func newLadder(side Side) ladder {
	return ladder{side: side, byPrice: make(map[int64]*level)}
}

// key maps a price to an ascending rank: higher is better for this side.
func (l *ladder) key(price int64) int64 {
	if l.side == SideBuy {
		return price
	}
	return -price
}

// best returns the best level, or nil when the side is empty.
func (l *ladder) best() *level {
	if n := len(l.levels); n > 0 {
		return l.levels[n-1]
	}
	return nil
}

// search returns the index of the first level whose key is >= key(price).
func (l *ladder) search(price int64) int {
	k := l.key(price)
	lo, hi := 0, len(l.levels)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if l.key(l.levels[mid].price) < k {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// levelAt returns the level at price, creating it if needed.
func (l *ladder) levelAt(price int64) *level {
	if lvl, ok := l.byPrice[price]; ok {
		return lvl
	}
	lvl := &level{price: price}
	i := len(l.levels)
	if i > 0 && l.key(l.levels[i-1].price) > l.key(price) {
		i = l.search(price)
	}
	l.levels = append(l.levels, nil)
	copy(l.levels[i+1:], l.levels[i:])
	l.levels[i] = lvl
	l.byPrice[price] = lvl
	return lvl
}

// drop removes an empty level.
func (l *ladder) drop(lvl *level) {
	n := len(l.levels)
	i := n - 1
	if i < 0 || l.levels[i] != lvl {
		i = l.search(lvl.price)
		if i >= n || l.levels[i] != lvl {
			return
		}
	}
	copy(l.levels[i:], l.levels[i+1:])
	l.levels[n-1] = nil
	l.levels = l.levels[:n-1]
	delete(l.byPrice, lvl.price)
}

// top returns up to n levels, best first.
func (l *ladder) top(n int) []Level {
	k := min(max(n, 0), len(l.levels))
	out := make([]Level, 0, k)
	for i := len(l.levels) - 1; len(out) < k; i-- {
		lvl := l.levels[i]
		out = append(out, Level{PriceTicks: lvl.price, QtyLots: lvl.total, Orders: lvl.count})
	}
	return out
}

// acceptable reports whether a taker on side with limit (0 = no limit) may
// trade at a maker's price.
func acceptable(side Side, limit, price int64) bool {
	if limit == 0 {
		return true
	}
	if side == SideBuy {
		return price <= limit
	}
	return price >= limit
}
