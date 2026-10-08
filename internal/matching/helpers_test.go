package matching

import (
	"bytes"
	"fmt"
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

var testCfg = Config{Instrument: "BTC-USD"}

func newTestBook(t testing.TB) *Book {
	t.Helper()
	b, err := NewBook(testCfg)
	require.NoError(t, err)
	return b
}

// harness applies commands to a book, journals them, keeps every result and
// checks the structural invariants after every command.
type harness struct {
	t       require.TestingT
	b       *Book
	j       Journal
	results []Result
	ts      int64
}

func newHarness(t require.TestingT) *harness {
	b, err := NewBook(testCfg)
	require.NoError(t, err)
	return &harness{t: t, b: b, results: []Result{}}
}

func (h *harness) apply(cmd Command) Result {
	res, err := h.b.Apply(cmd)
	require.NoError(h.t, err)
	require.NoError(h.t, h.j.Append(JournalEntry{Seq: res.CommandSeq, Command: cmd}))
	require.NoError(h.t, h.b.verify())
	h.results = append(h.results, res)
	return res
}

// Command constructors.

func lim(id, owner string, side Side, price, qty int64) NewOrder {
	return NewOrder{OrderID: id, OwnerID: owner, Side: side, Type: OrderTypeLimit, TIF: TIFGTC, PriceTicks: price, QtyLots: qty}
}

func ioc(o NewOrder) NewOrder { o.TIF = TIFIOC; return o }
func fok(o NewOrder) NewOrder { o.TIF = TIFFOK; return o }

func stp(o NewOrder, m STPMode) NewOrder { o.STP = m; return o }

func postOnly(o NewOrder) NewOrder { o.PostOnly = true; return o }

func reduceOnly(o NewOrder, limit int64) NewOrder {
	o.ReduceOnly = true
	o.ReduceOnlyLimitLots = limit
	return o
}

func mkt(id, owner string, side Side, qty int64) NewOrder {
	return NewOrder{OrderID: id, OwnerID: owner, Side: side, Type: OrderTypeMarket, TIF: TIFIOC, QtyLots: qty}
}

func typesOf(evs []Event) []EventType {
	out := make([]EventType, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}

func eventsOf(evs []Event, t EventType) []Event {
	var out []Event
	for _, e := range evs {
		if e.Type == t {
			out = append(out, e)
		}
	}
	return out
}

// shadow is an independent oracle: it rebuilds every order's quantities and
// the aggregated book from the event stream alone, checks each event against
// the engine's documented contract, and cross-checks the result against the
// engine's own state after every command.
type shadow struct {
	orders     map[string]*shadowOrder
	levels     map[shadowLevel]int64
	lastCmd    int64
	lastSeq    int64
	lastTrade  int64
	traded     int64 // sum of TRADE quantities
	filledBuy  int64 // sum of fill quantities on buy orders
	filledSell int64 // sum of fill quantities on sell orders
}

type shadowOrder struct {
	side      Side
	owner     string
	typ       OrderType
	tif       TimeInForce
	price     int64
	qty       int64
	filled    int64
	remaining int64
	cancelled int64
	resting   bool
	closed    bool
	postOnly  bool
	// roLimit is a reduce-only order's ReduceOnlyLimitLots, taken from the
	// command (the events do not carry it); 0 when not reduce-only.
	roLimit int64
}

type shadowLevel struct {
	side  Side
	price int64
}

func newShadow() *shadow {
	return &shadow{orders: make(map[string]*shadowOrder), levels: make(map[shadowLevel]int64)}
}

func (s *shadow) observe(cmd Command, res Result, b *Book) error {
	if res.CommandSeq != s.lastCmd+1 {
		return fmt.Errorf("command seq %d after %d", res.CommandSeq, s.lastCmd)
	}
	s.lastCmd = res.CommandSeq
	if len(res.Events) == 0 {
		return fmt.Errorf("command %d produced no events", res.CommandSeq)
	}
	if _, rejected := res.Rejected(); !rejected {
		for _, e := range res.Events {
			if e.Type == EventRejected {
				return fmt.Errorf("REJECTED event mixed with other events")
			}
		}
	}
	touched := map[string]bool{}
	tradesByTaker := map[string]int{}
	for i, e := range res.Events {
		if e.Seq != s.lastSeq+1 {
			return fmt.Errorf("event seq %d after %d", e.Seq, s.lastSeq)
		}
		s.lastSeq = e.Seq
		if e.CommandSeq != res.CommandSeq || e.Command != cmd.Kind() {
			return fmt.Errorf("event %d carries the wrong command", e.Seq)
		}
		if e.OrderID != "" {
			touched[e.OrderID] = true
		}
		o := s.orders[e.OrderID]
		switch e.Type {
		case EventRejected:
			continue
		case EventAccepted:
			if o != nil {
				return fmt.Errorf("order %s accepted twice", e.OrderID)
			}
			if e.QtyLots < 1 || e.RemainingLots != e.QtyLots || e.FilledLots != 0 {
				return fmt.Errorf("ACCEPTED %s quantities", e.OrderID)
			}
			no, ok := cmd.(NewOrder)
			if !ok || no.OrderID != e.OrderID || e.PostOnly != no.PostOnly || e.ReduceOnly != no.ReduceOnly {
				return fmt.Errorf("ACCEPTED %s does not describe its command", e.OrderID)
			}
			so := &shadowOrder{side: e.Side, owner: e.OwnerID, typ: e.OrderType, tif: e.TimeInForce,
				price: e.PriceTicks, qty: e.QtyLots, remaining: e.QtyLots, postOnly: e.PostOnly}
			if no.ReduceOnly {
				so.roLimit = no.ReduceOnlyLimitLots
			}
			s.orders[e.OrderID] = so
			continue
		case EventTrade:
			if e.TradeSeq != s.lastTrade+1 {
				return fmt.Errorf("trade seq %d after %d", e.TradeSeq, s.lastTrade)
			}
			s.lastTrade = e.TradeSeq
			m, t := s.orders[e.MakerOrderID], s.orders[e.TakerOrderID]
			switch {
			case m == nil || t == nil:
				return fmt.Errorf("trade %d between unknown orders", e.TradeSeq)
			case !m.resting || t.resting:
				return fmt.Errorf("trade %d: maker must rest and taker must not", e.TradeSeq)
			case m.side == t.side || e.AggressorSide != t.side:
				return fmt.Errorf("trade %d sides", e.TradeSeq)
			case m.owner == t.owner || e.MakerOwnerID != m.owner || e.TakerOwnerID != t.owner:
				return fmt.Errorf("trade %d is a self-trade or misattributed", e.TradeSeq)
			case e.PriceTicks != m.price:
				return fmt.Errorf("trade %d not at the maker's price", e.TradeSeq)
			case !acceptable(t.side, t.price, e.PriceTicks):
				return fmt.Errorf("trade %d violates the taker's limit", e.TradeSeq)
			case e.QtyLots < 1 || e.QtyLots > m.remaining || e.QtyLots > t.remaining:
				return fmt.Errorf("trade %d quantity", e.TradeSeq)
			case t.postOnly:
				return fmt.Errorf("trade %d: post-only order %s took liquidity", e.TradeSeq, e.TakerOrderID)
			}
			if i+3 >= len(res.Events) {
				return fmt.Errorf("trade %d not followed by fills and a delta", e.TradeSeq)
			}
			mf, tf, d := res.Events[i+1], res.Events[i+2], res.Events[i+3]
			if mf.OrderID != e.MakerOrderID || tf.OrderID != e.TakerOrderID || mf.TradeSeq != e.TradeSeq ||
				tf.TradeSeq != e.TradeSeq || mf.QtyLots != e.QtyLots || tf.QtyLots != e.QtyLots ||
				d.Type != EventBookDelta || d.Side != m.side || d.PriceTicks != e.PriceTicks {
				return fmt.Errorf("trade %d follow-up events malformed", e.TradeSeq)
			}
			s.traded += e.QtyLots
			tradesByTaker[e.TakerOrderID]++
			continue
		case EventBookDelta:
			if e.LevelQtyLots < 0 {
				return fmt.Errorf("negative level")
			}
			k := shadowLevel{e.Side, e.PriceTicks}
			if e.LevelQtyLots == 0 {
				delete(s.levels, k)
			} else {
				s.levels[k] = e.LevelQtyLots
			}
			continue
		}
		if o == nil {
			return fmt.Errorf("%s event for unknown order %s", e.Type, e.OrderID)
		}
		if o.closed {
			return fmt.Errorf("%s event for closed order %s", e.Type, e.OrderID)
		}
		switch e.Type {
		case EventResting:
			if o.resting || e.Priority != e.Seq || e.QtyLots != o.remaining || e.PriceTicks != o.price ||
				o.typ != OrderTypeLimit || o.tif != TIFGTC {
				return fmt.Errorf("RESTING %s inconsistent", e.OrderID)
			}
			o.resting = true
		case EventPartialFill, EventFullFill:
			o.filled += e.QtyLots
			o.remaining -= e.QtyLots
			if o.roLimit > 0 && o.filled > o.roLimit {
				return fmt.Errorf("reduce-only %s filled %d above its limit %d", e.OrderID, o.filled, o.roLimit)
			}
			if o.side == SideBuy {
				s.filledBuy += e.QtyLots
			} else {
				s.filledSell += e.QtyLots
			}
			if (e.Type == EventFullFill) != (o.remaining == 0) {
				return fmt.Errorf("%s for %s with %d remaining", e.Type, e.OrderID, o.remaining)
			}
			if o.remaining == 0 {
				o.closed, o.resting = true, false
			}
		case EventCancelled:
			if e.QtyLots < 1 || e.QtyLots > o.remaining {
				return fmt.Errorf("CANCELLED %s quantity", e.OrderID)
			}
			o.remaining -= e.QtyLots
			o.cancelled += e.QtyLots
			if (e.CancelReason == CancelReduceOnlyCap) != (o.remaining > 0) {
				return fmt.Errorf("CANCELLED %s: only a reduce-only cap leaves the order open", e.OrderID)
			}
			if o.remaining == 0 {
				o.closed, o.resting = true, false
			}
		case EventReplaced:
			if !o.resting {
				return fmt.Errorf("REPLACED %s which does not rest", e.OrderID)
			}
			if e.PrevPriceTicks != o.price || e.PrevQtyLots != o.qty {
				return fmt.Errorf("REPLACED %s previous values", e.OrderID)
			}
			if keeps := e.PriceTicks == o.price && e.RemainingLots <= o.remaining; e.PriorityKept != keeps {
				return fmt.Errorf("REPLACED %s: priority kept %v, the rule says %v", e.OrderID, e.PriorityKept, keeps)
			}
			o.price, o.qty, o.remaining = e.PriceTicks, e.QtyLots, e.RemainingLots
			o.resting = e.PriorityKept
		default:
			return fmt.Errorf("unexpected event type %s", e.Type)
		}
		if e.FilledLots != o.filled || e.RemainingLots != o.remaining {
			return fmt.Errorf("%s %s reports %d/%d, model %d/%d", e.Type, e.OrderID,
				e.FilledLots, e.RemainingLots, o.filled, o.remaining)
		}
		if o.filled+o.remaining+o.cancelled != o.qty || o.filled > o.qty || o.remaining < 0 {
			return fmt.Errorf("order %s not conserved: %d+%d+%d != %d", e.OrderID, o.filled, o.remaining, o.cancelled, o.qty)
		}
	}

	// FOK is all or nothing: an accepted FOK either fills everything left
	// after its reduce-only cap, or is cancelled (FOK) with zero fills.
	if no, ok := cmd.(NewOrder); ok && no.TIF == TIFFOK && res.Events[0].Type == EventAccepted {
		o := s.orders[no.OrderID]
		killed, capped := false, int64(0)
		for _, e := range res.Events {
			if e.Type != EventCancelled || e.OrderID != no.OrderID {
				continue
			}
			switch e.CancelReason {
			case CancelFOK:
				killed = true
			case CancelReduceOnlyCap:
				capped += e.QtyLots
			default:
				return fmt.Errorf("FOK %s cancelled for %s", no.OrderID, e.CancelReason)
			}
		}
		switch {
		case !o.closed:
			return fmt.Errorf("FOK %s left open", no.OrderID)
		case killed && (o.filled != 0 || tradesByTaker[no.OrderID] != 0):
			return fmt.Errorf("FOK %s killed after trading", no.OrderID)
		case !killed && o.filled != o.qty-capped:
			return fmt.Errorf("FOK %s partially filled", no.OrderID)
		}
	}

	// Both sides of every trade are accounted for.
	if s.filledBuy != s.traded || s.filledSell != s.traded {
		return fmt.Errorf("traded %d, buy fills %d, sell fills %d", s.traded, s.filledBuy, s.filledSell)
	}

	// The engine agrees with the model on every order the command touched.
	for id := range touched {
		o := s.orders[id]
		st, ok := b.Order(id)
		if o == nil {
			if ok && st.Status != StatusRejected {
				return fmt.Errorf("order %s exists in the book but was never accepted", id)
			}
			continue
		}
		if !ok || st.QtyLots != o.qty || st.FilledLots != o.filled || st.RemainingLots != o.remaining ||
			st.CancelledLots != o.cancelled || st.PriceTicks != o.price || st.Status.IsOpen() != o.resting {
			return fmt.Errorf("order %s: book %+v, model %+v", id, st, *o)
		}
	}

	// The aggregated book rebuilt from BOOK_DELTA equals the engine's.
	depth := b.Depth(1 << 30)
	n := 0
	for side, levels := range map[Side][]Level{SideBuy: depth.Bids, SideSell: depth.Asks} {
		for _, l := range levels {
			n++
			if s.levels[shadowLevel{side, l.PriceTicks}] != l.QtyLots {
				return fmt.Errorf("level %s %d: book %d, deltas %d", side, l.PriceTicks, l.QtyLots,
					s.levels[shadowLevel{side, l.PriceTicks}])
			}
		}
	}
	if n != len(s.levels) {
		return fmt.Errorf("book has %d levels, deltas describe %d", n, len(s.levels))
	}

	// Never crossed.
	if bid, ok := b.BestBid(); ok {
		if ask, ok := b.BestAsk(); ok && bid.PriceTicks >= ask.PriceTicks {
			return fmt.Errorf("crossed book: bid %d ask %d", bid.PriceTicks, ask.PriceTicks)
		}
	}
	return nil
}

// checkAll verifies every order the model knows against the book: used at
// the end of long runs where checking only touched orders is not enough.
func (s *shadow) checkAll(b *Book) error {
	for id, o := range s.orders {
		st, ok := b.Order(id)
		if !ok || st.QtyLots != o.qty || st.FilledLots != o.filled || st.RemainingLots != o.remaining ||
			st.CancelledLots != o.cancelled {
			return fmt.Errorf("order %s: book %+v, model %+v", id, st, *o)
		}
		if st.FilledLots+st.RemainingLots+st.CancelledLots != st.QtyLots || st.FilledLots > st.QtyLots {
			return fmt.Errorf("order %s not conserved in the book", id)
		}
	}
	return nil
}

// requireSameResults compares two result streams command by command and
// reports only the first difference. Long streams must not go through a
// whole-value require.Equal: rendering and diffing tens of thousands of
// results on failure takes longer than the test timeout.
func requireSameResults(t require.TestingT, want, got []Result, msg string) {
	require.Equal(t, len(want), len(got), "%s: number of results", msg)
	for i := range want {
		if want[i].CommandSeq != got[i].CommandSeq || !slices.Equal(want[i].Events, got[i].Events) {
			require.Equal(t, want[i], got[i], "%s: first difference at result %d", msg, i)
		}
	}
}

// requireSameBytes compares two snapshots and reports the first differing
// offset instead of a hex-dump diff.
func requireSameBytes(t require.TestingT, want, got []byte, msg string) {
	if bytes.Equal(want, got) {
		return
	}
	i := 0
	for i < len(want) && i < len(got) && want[i] == got[i] {
		i++
	}
	require.FailNow(t, fmt.Sprintf("%s: first difference at byte %d (lengths %d and %d)", msg, i, len(want), len(got)))
}

// requireReplayIdentical replays entries into a new book and requires the
// identical results and snapshot.
func requireReplayIdentical(t require.TestingT, entries []JournalEntry, results []Result, snapshot []byte) {
	rb, rres, err := Replay(testCfg, entries)
	require.NoError(t, err)
	requireSameResults(t, results, rres, "replayed events")
	requireSameBytes(t, snapshot, rb.Snapshot(), "replayed snapshot")
}

// requireRestoreMidpoint snapshots a replay of entries[:k], restores it,
// replays the rest and requires the same results and final snapshot.
func requireRestoreMidpoint(t require.TestingT, entries []JournalEntry, results []Result, snapshot []byte, k int) {
	mid, _, err := Replay(testCfg, entries[:k])
	require.NoError(t, err)
	restored, err := Restore(mid.Snapshot())
	require.NoError(t, err)
	require.Equal(t, mid.StateHash(), restored.StateHash())
	rest, err := restored.Replay(entries[k:])
	require.NoError(t, err)
	requireSameResults(t, results[k:], rest, "events after restore")
	requireSameBytes(t, snapshot, restored.Snapshot(), "state after restore")
}

// splitmix64 is a tiny deterministic PRNG for synthetic streams.
type splitmix64 uint64

func (s *splitmix64) next() uint64 {
	*s += 0x9e3779b97f4a7c15
	z := uint64(*s)
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// intn returns a value in [0, n).
func (s *splitmix64) intn(n int) int { return int(s.next() % uint64(n)) }

// streamGen produces a deterministic synthetic command stream around a mid
// price: mostly limit orders near the touch (many of which cross), plus
// markets, IOC, FOK, post-only, cancels and replaces of known orders.
type streamGen struct {
	rng    splitmix64
	prefix string
	owners []string
	n      int
	live   []liveRef // candidate targets for cancel and replace
}

type liveRef struct{ id, owner string }

func newStreamGen(seed uint64, prefix string, owners []string) *streamGen {
	return &streamGen{rng: splitmix64(seed), prefix: prefix, owners: owners}
}

func (g *streamGen) nextCommand() Command {
	g.n++
	ts := int64(g.n)
	r := g.rng.intn(100)
	if len(g.live) > 0 && r >= 80 {
		i := g.rng.intn(len(g.live))
		ref := g.live[i]
		if r < 92 {
			g.live[i] = g.live[len(g.live)-1]
			g.live = g.live[:len(g.live)-1]
			return Cancel{Timestamp: ts, OrderID: ref.id, OwnerID: ref.owner}
		}
		return Replace{Timestamp: ts, OrderID: ref.id, OwnerID: ref.owner,
			NewPriceTicks: int64(990 + g.rng.intn(21)), NewQtyLots: int64(1 + g.rng.intn(20))}
	}
	id := g.prefix + strconv.Itoa(g.n)
	owner := g.owners[g.rng.intn(len(g.owners))]
	side := SideBuy
	if g.rng.intn(2) == 1 {
		side = SideSell
	}
	o := NewOrder{Timestamp: ts, OrderID: id, OwnerID: owner, Side: side, Type: OrderTypeLimit, TIF: TIFGTC,
		PriceTicks: int64(990 + g.rng.intn(21)), QtyLots: int64(1 + g.rng.intn(20)), STP: STPMode(g.rng.intn(3))}
	switch k := g.rng.intn(100); {
	case k < 6:
		o.Type, o.TIF, o.PriceTicks = OrderTypeMarket, TIFIOC, 0
	case k < 14:
		o.TIF = TIFIOC
	case k < 18:
		o.TIF = TIFFOK
	case k < 26:
		o.PostOnly = true
	}
	if o.TIF == TIFGTC {
		g.live = append(g.live, liveRef{id, owner})
		if len(g.live) > 4096 {
			g.live = g.live[1:]
		}
	}
	return o
}
