package matching

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

var propOwners = []string{"alice", "bob", "carol"}

// propGen draws commands that make crossing, self-trades and races against
// resting orders frequent: three owners, a five-tick band, small quantities,
// every order type and flag, cancels and replaces aimed mostly at orders
// that rest, and a sprinkling of duplicates and invalid values.
type propGen struct {
	n       int
	ids     []string
	ownerOf map[string]string
}

func newPropGen() *propGen { return &propGen{ownerOf: make(map[string]string)} }

func (g *propGen) draw(t *rapid.T, b *Book) Command {
	g.n++
	ts := int64(g.n) * 1000
	kind := rapid.IntRange(0, 99).Draw(t, "kind")
	if len(g.ids) > 0 && kind >= 64 {
		id := g.target(t, b)
		owner := g.ownerOf[id]
		switch rapid.IntRange(0, 19).Draw(t, "targetVariant") {
		case 0:
			id = "ghost"
		case 1:
			owner = rapid.SampledFrom(propOwners).Draw(t, "anyOwner")
		}
		if kind < 80 {
			return Cancel{Timestamp: ts, OrderID: id, OwnerID: owner}
		}
		return Replace{Timestamp: ts, OrderID: id, OwnerID: owner,
			NewPriceTicks: rapid.Int64Range(98, 102).Draw(t, "newPrice"),
			NewQtyLots:    rapid.Int64Range(1, 12).Draw(t, "newQty")}
	}

	id := "o" + strconv.Itoa(g.n)
	if len(g.ids) > 0 && rapid.IntRange(0, 29).Draw(t, "dupID") == 0 {
		id = g.ids[rapid.IntRange(0, len(g.ids)-1).Draw(t, "dupOf")]
	}
	o := NewOrder{
		Timestamp:  ts,
		OrderID:    id,
		OwnerID:    rapid.SampledFrom(propOwners).Draw(t, "owner"),
		Side:       rapid.SampledFrom([]Side{SideBuy, SideSell}).Draw(t, "side"),
		Type:       OrderTypeLimit,
		TIF:        TIFGTC,
		PriceTicks: rapid.Int64Range(98, 102).Draw(t, "price"),
		QtyLots:    rapid.Int64Range(1, 10).Draw(t, "qty"),
		STP:        STPMode(rapid.IntRange(0, 2).Draw(t, "stp")),
	}
	if rapid.IntRange(0, 3).Draw(t, "hasClientID") == 0 {
		o.ClientOrderID = "c" + strconv.Itoa(rapid.IntRange(0, 9).Draw(t, "clientID"))
	}
	switch v := rapid.IntRange(0, 99).Draw(t, "variant"); {
	case v < 12:
		o.TIF = TIFIOC
	case v < 22:
		o.TIF = TIFFOK
	case v < 34:
		o.Type = OrderTypeMarket
		o.TIF = rapid.SampledFrom([]TimeInForce{TIFIOC, TIFIOC, TIFFOK}).Draw(t, "marketTIF")
		o.PriceTicks = 0
		if rapid.Bool().Draw(t, "protected") {
			o.PriceTicks = rapid.Int64Range(98, 102).Draw(t, "protection")
		}
	case v < 44:
		o.PostOnly = true
	case v < 54:
		o.ReduceOnly = true
		o.ReduceOnlyLimitLots = rapid.Int64Range(0, 10).Draw(t, "reduceOnlyLimit")
		o.TIF = rapid.SampledFrom([]TimeInForce{TIFGTC, TIFIOC, TIFFOK}).Draw(t, "reduceOnlyTIF")
	case v < 57:
		switch rapid.IntRange(0, 3).Draw(t, "invalid") {
		case 0:
			o.QtyLots = 0
		case 1:
			o.PriceTicks = 0
		case 2:
			o.Side = 0
		case 3:
			o.OwnerID = ""
		}
	}
	if _, seen := g.ownerOf[id]; !seen {
		g.ids = append(g.ids, id)
		g.ownerOf[id] = o.OwnerID
	}
	return o
}

// target picks a cancel/replace target: usually a resting order, otherwise
// any id ever submitted (closed, rejected or never accepted).
func (g *propGen) target(t *rapid.T, b *Book) string {
	var open []string
	for _, owner := range propOwners {
		for _, st := range b.OpenOrders(owner) {
			open = append(open, st.OrderID)
		}
	}
	if len(open) > 0 && rapid.IntRange(0, 3).Draw(t, "targetOpen") > 0 {
		return open[rapid.IntRange(0, len(open)-1).Draw(t, "openTarget")]
	}
	return g.ids[rapid.IntRange(0, len(g.ids)-1).Draw(t, "anyTarget")]
}

// requireLevelsFIFOAndAggregates walks both sides independently of verify:
// queue priorities strictly increase within each level, each level's
// aggregate equals the sum of its orders, and the book is not crossed.
func requireLevelsFIFOAndAggregates(t require.TestingT, b *Book) {
	for _, l := range []*ladder{&b.bids, &b.asks} {
		for _, lvl := range l.levels {
			var sum, last int64
			for o := lvl.head; o != nil; o = o.next {
				require.Greater(t, o.priority, last, "FIFO violated at %s %d", l.side, lvl.price)
				last = o.priority
				sum += o.remaining
			}
			require.Equal(t, sum, lvl.total, "level aggregate at %s %d", l.side, lvl.price)
		}
	}
	if bid, ask := b.bids.best(), b.asks.best(); bid != nil && ask != nil {
		require.Less(t, bid.price, ask.price, "crossed book")
	}
}

// TestProp_InvariantsAndReplay applies random command sequences and, after
// EVERY command, checks the structural invariants, the event-derived model
// (per-order conservation, no overfill, both sides of every trade, FOK
// atomicity, depth from deltas), FIFO and aggregates, and that a from-scratch
// replay of the journal so far reproduces byte-identical snapshots and the
// identical event stream. At the end it snapshots at a random midpoint,
// restores, replays the rest and requires the same outcome.
func TestProp_InvariantsAndReplay(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		h := newHarness(rt)
		model := newShadow()
		gen := newPropGen()
		n := rapid.IntRange(1, 90).Draw(rt, "commands")
		for i := 0; i < n; i++ {
			cmd := gen.draw(rt, h.b)
			res := h.apply(cmd) // includes verify()
			require.NoError(rt, model.observe(cmd, res, h.b))
			requireLevelsFIFOAndAggregates(rt, h.b)
			requireReplayIdentical(rt, h.j.Entries(), h.results, h.b.Snapshot())
		}
		require.NoError(rt, model.checkAll(h.b))
		k := rapid.IntRange(0, n).Draw(rt, "midpoint")
		requireRestoreMidpoint(rt, h.j.Entries(), h.results, h.b.Snapshot(), k)
	})
}

// TestProp_LongRunsReplayIncrementally explores deeper books: a replica fed
// the journal entry by entry must match the live book's events and snapshot
// after every command, and several random snapshots must restore and resume
// to the same final state.
func TestProp_LongRunsReplayIncrementally(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		h := newHarness(rt)
		model := newShadow()
		gen := newPropGen()
		replica, err := NewBook(testCfg)
		require.NoError(rt, err)
		n := rapid.IntRange(100, 400).Draw(rt, "commands")
		for i := 0; i < n; i++ {
			cmd := gen.draw(rt, h.b)
			res := h.apply(cmd)
			require.NoError(rt, model.observe(cmd, res, h.b))
			got, err := replica.Replay(h.j.Since(replica.CommandSeq()))
			require.NoError(rt, err)
			require.Equal(rt, []Result{res}, got)
			requireSameBytes(rt, h.b.Snapshot(), replica.Snapshot(), "replica snapshot")
		}
		require.NoError(rt, model.checkAll(h.b))
		requireLevelsFIFOAndAggregates(rt, h.b)
		snapshot := h.b.Snapshot()
		requireReplayIdentical(rt, h.j.Entries(), h.results, snapshot)
		for i := 0; i < 3; i++ {
			k := rapid.IntRange(0, n).Draw(rt, "midpoint")
			requireRestoreMidpoint(rt, h.j.Entries(), h.results, snapshot, k)
		}
	})
}

// TestProp_SnapshotRestoreIsExact checks that a restored book is
// indistinguishable from the original: same snapshot and hash, same queries,
// and the same events and state for any continuation.
func TestProp_SnapshotRestoreIsExact(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		h := newHarness(rt)
		gen := newPropGen()
		for n := rapid.IntRange(0, 120).Draw(rt, "prefix"); n > 0; n-- {
			h.apply(gen.draw(rt, h.b))
		}
		restored, err := Restore(h.b.Snapshot())
		require.NoError(rt, err)
		requireSameBytes(rt, h.b.Snapshot(), restored.Snapshot(), "restored snapshot")
		require.Equal(rt, h.b.StateHash(), restored.StateHash())
		require.Equal(rt, h.b.Depth(100), restored.Depth(100))
		for _, owner := range propOwners {
			require.Equal(rt, h.b.OpenOrders(owner), restored.OpenOrders(owner))
		}
		for _, id := range gen.ids {
			want, okWant := h.b.Order(id)
			got, okGot := restored.Order(id)
			require.Equal(rt, okWant, okGot)
			require.Equal(rt, want, got)
		}
		for n := rapid.IntRange(1, 60).Draw(rt, "continuation"); n > 0; n-- {
			cmd := gen.draw(rt, h.b)
			want := h.apply(cmd)
			got, err := restored.Apply(cmd)
			require.NoError(rt, err)
			require.Equal(rt, want, got)
		}
		requireSameBytes(rt, h.b.Snapshot(), restored.Snapshot(), "restored snapshot")
		require.NoError(rt, restored.verify())
	})
}
