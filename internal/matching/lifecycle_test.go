package matching

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
)

func TestReplaceReduceAtSamePriceKeepsPriority(t *testing.T) {
	h := newHarness(t)
	h.apply(lim("s1", "alice", SideSell, 100, 5))
	h.apply(lim("s2", "bob", SideSell, 100, 5))
	before := requireState(t, h.b, "s1", StatusOpen, 0, 5, 0)

	res := h.apply(Replace{OrderID: "s1", OwnerID: "alice", NewPriceTicks: 100, NewQtyLots: 3})
	require.Equal(t, []EventType{EventReplaced, EventBookDelta}, typesOf(res.Events))
	r := res.Events[0]
	assert.True(t, r.PriorityKept)
	assert.Equal(t, CommandReplace, r.Command)
	assert.Equal(t, []int64{100, 3, 100, 5, 3}, []int64{r.PriceTicks, r.QtyLots, r.PrevPriceTicks, r.PrevQtyLots, r.RemainingLots})
	assert.Equal(t, int64(8), res.Events[1].LevelQtyLots)
	after := requireState(t, h.b, "s1", StatusOpen, 0, 3, 0)
	assert.Equal(t, before.Priority, after.Priority)

	res = h.apply(ioc(lim("b1", "carol", SideBuy, 100, 4)))
	trades := eventsOf(res.Events, EventTrade)
	require.Len(t, trades, 2)
	assert.Equal(t, "s1", trades[0].MakerOrderID, "the reduced order kept its place at the front")
	assert.Equal(t, int64(3), trades[0].QtyLots)
	assert.Equal(t, "s2", trades[1].MakerOrderID)
}

func TestReplaceQuantityIncreaseLosesPriority(t *testing.T) {
	h := newHarness(t)
	h.apply(lim("s1", "alice", SideSell, 100, 5))
	h.apply(lim("s2", "bob", SideSell, 100, 5))
	s2 := requireState(t, h.b, "s2", StatusOpen, 0, 5, 0)

	res := h.apply(Replace{OrderID: "s1", OwnerID: "alice", NewPriceTicks: 100, NewQtyLots: 7})
	require.Equal(t, []EventType{EventReplaced, EventResting, EventBookDelta}, typesOf(res.Events))
	assert.False(t, res.Events[0].PriorityKept)
	assert.Equal(t, int64(12), res.Events[2].LevelQtyLots)
	s1 := requireState(t, h.b, "s1", StatusOpen, 0, 7, 0)
	assert.Greater(t, s1.Priority, s2.Priority)
	assert.Equal(t, res.Events[1].Seq, s1.Priority)

	res = h.apply(ioc(lim("b1", "carol", SideBuy, 100, 6)))
	trades := eventsOf(res.Events, EventTrade)
	require.Len(t, trades, 2)
	assert.Equal(t, []string{"s2", "s1"}, []string{trades[0].MakerOrderID, trades[1].MakerOrderID})
}

func TestReplacePriceChangeLosesPriority(t *testing.T) {
	h := newHarness(t)
	h.apply(lim("s1", "alice", SideSell, 100, 5))
	h.apply(lim("s2", "bob", SideSell, 101, 5))
	res := h.apply(Replace{OrderID: "s1", OwnerID: "alice", NewPriceTicks: 101, NewQtyLots: 2})
	require.Equal(t, []EventType{EventReplaced, EventBookDelta, EventResting, EventBookDelta}, typesOf(res.Events))
	assert.Equal(t, Event{Seq: res.Events[1].Seq, CommandSeq: 3, Command: CommandReplace, Type: EventBookDelta,
		Side: SideSell, PriceTicks: 100}, res.Events[1], "old level removed")
	assert.Equal(t, int64(7), res.Events[3].LevelQtyLots)
	assert.Greater(t, mustOrder(t, h.b, "s1").Priority, mustOrder(t, h.b, "s2").Priority, "re-queued behind s2")
	ask, _ := h.b.BestAsk()
	assert.Equal(t, Level{PriceTicks: 101, QtyLots: 7, Orders: 2}, ask)
	res = h.apply(ioc(lim("b1", "carol", SideBuy, 101, 1)))
	assert.Equal(t, "s2", eventsOf(res.Events, EventTrade)[0].MakerOrderID)
}

func mustOrder(t *testing.T, b *Book, id string) OrderState {
	t.Helper()
	st, ok := b.Order(id)
	require.True(t, ok)
	return st
}

func TestReplaceQuantityRules(t *testing.T) {
	h := newHarness(t)
	h.apply(lim("s1", "alice", SideSell, 100, 5))
	h.apply(ioc(lim("b1", "bob", SideBuy, 100, 3)))
	requireState(t, h.b, "s1", StatusPartiallyFilled, 3, 2, 0)

	requireRejected(t, h.apply(Replace{OrderID: "s1", OwnerID: "alice", NewPriceTicks: 100, NewQtyLots: 2}), RejectReplaceQtyNotAboveFilled)
	requireRejected(t, h.apply(Replace{OrderID: "s1", OwnerID: "alice", NewPriceTicks: 100, NewQtyLots: 3}), RejectReplaceQtyNotAboveFilled)
	requireState(t, h.b, "s1", StatusPartiallyFilled, 3, 2, 0)

	res := h.apply(Replace{OrderID: "s1", OwnerID: "alice", NewPriceTicks: 100, NewQtyLots: 4})
	assert.True(t, res.Events[0].PriorityKept)
	requireState(t, h.b, "s1", StatusPartiallyFilled, 3, 1, 0)

	res = h.apply(Replace{OrderID: "s1", OwnerID: "alice", NewPriceTicks: 100, NewQtyLots: 4})
	require.Equal(t, []EventType{EventReplaced}, typesOf(res.Events), "a no-op replace changes nothing")
	assert.True(t, res.Events[0].PriorityKept)
}

func TestReplaceRejections(t *testing.T) {
	h := newHarness(t)
	h.apply(lim("s1", "alice", SideSell, 100, 5))
	h.apply(lim("f1", "alice", SideSell, 105, 1))
	h.apply(ioc(lim("x1", "bob", SideBuy, 105, 6))) // fills s1 and f1
	h.apply(lim("s2", "alice", SideSell, 110, 5))
	requireRejected(t, h.apply(lim("bad", "alice", SideSell, 0, 1)), RejectInvalidPrice)

	cases := []struct {
		name string
		cmd  Replace
		want RejectReason
	}{
		{"empty order id", Replace{OwnerID: "alice", NewPriceTicks: 1, NewQtyLots: 1}, RejectInvalidOrderID},
		{"empty owner", Replace{OrderID: "s2", NewPriceTicks: 1, NewQtyLots: 1}, RejectInvalidOwner},
		{"zero price", Replace{OrderID: "s2", OwnerID: "alice", NewQtyLots: 1}, RejectInvalidPrice},
		{"price above ceiling", Replace{OrderID: "s2", OwnerID: "alice", NewPriceTicks: MaxPriceTicks + 1, NewQtyLots: 1}, RejectInvalidPrice},
		{"zero qty", Replace{OrderID: "s2", OwnerID: "alice", NewPriceTicks: 100}, RejectInvalidQty},
		{"qty above max", Replace{OrderID: "s2", OwnerID: "alice", NewPriceTicks: 100, NewQtyLots: MaxQtyLots + 1}, RejectInvalidQty},
		{"notional", Replace{OrderID: "s2", OwnerID: "alice", NewPriceTicks: 1_000_000, NewQtyLots: 10_000_000_000_000}, RejectNotionalTooLarge},
		{"unknown order", Replace{OrderID: "nope", OwnerID: "alice", NewPriceTicks: 100, NewQtyLots: 1}, RejectUnknownOrder},
		{"rejected order", Replace{OrderID: "bad", OwnerID: "alice", NewPriceTicks: 100, NewQtyLots: 1}, RejectUnknownOrder},
		{"not owner", Replace{OrderID: "s2", OwnerID: "bob", NewPriceTicks: 100, NewQtyLots: 1}, RejectNotOwner},
		{"filled order", Replace{OrderID: "f1", OwnerID: "alice", NewPriceTicks: 100, NewQtyLots: 1}, RejectOrderNotOpen},
		{"taker that never rested", Replace{OrderID: "x1", OwnerID: "bob", NewPriceTicks: 100, NewQtyLots: 1}, RejectOrderNotOpen},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := h.b.Depth(10)
			res := h.apply(tc.cmd)
			requireRejected(t, res, tc.want)
			assert.Equal(t, CommandReplace, res.Events[0].Command)
			assert.Equal(t, tc.cmd.OrderID, res.Events[0].OrderID)
			assert.Equal(t, before, h.b.Depth(10))
		})
	}
}

func TestReplaceThatCrossesTradesAsAggressor(t *testing.T) {
	h := newHarness(t)
	h.apply(lim("s1", "bob", SideSell, 100, 5))
	h.apply(lim("b1", "alice", SideBuy, 98, 4))
	res := h.apply(Replace{OrderID: "b1", OwnerID: "alice", NewPriceTicks: 100, NewQtyLots: 6})
	require.Equal(t, []EventType{
		EventReplaced, EventBookDelta, // leaves 98
		EventTrade, EventFullFill, EventPartialFill, EventBookDelta, // takes s1
		EventResting, EventBookDelta, // remainder rests at 100
	}, typesOf(res.Events))
	r := res.Events[0]
	assert.False(t, r.PriorityKept)
	assert.Equal(t, []int64{98, 4, 100, 6}, []int64{r.PrevPriceTicks, r.PrevQtyLots, r.PriceTicks, r.QtyLots})
	tr := res.Events[2]
	assert.Equal(t, "b1", tr.TakerOrderID)
	assert.Equal(t, "s1", tr.MakerOrderID)
	assert.Equal(t, SideBuy, tr.AggressorSide)
	assert.Equal(t, int64(100), tr.PriceTicks)
	assert.Equal(t, int64(5), tr.QtyLots)
	requireState(t, h.b, "b1", StatusPartiallyFilled, 5, 1, 0)
	assert.Equal(t, Depth{Bids: []Level{{PriceTicks: 100, QtyLots: 1, Orders: 1}}, Asks: []Level{}}, h.b.Depth(5))
}

func TestReplaceThatCrossesAppliesSTPAndPostOnly(t *testing.T) {
	t.Run("self-trade cancels the replaced order", func(t *testing.T) {
		h := newHarness(t)
		h.apply(lim("s1", "alice", SideSell, 100, 5))
		h.apply(lim("b1", "alice", SideBuy, 98, 4))
		res := h.apply(Replace{OrderID: "b1", OwnerID: "alice", NewPriceTicks: 100, NewQtyLots: 4})
		require.Equal(t, []EventType{EventReplaced, EventBookDelta, EventCancelled}, typesOf(res.Events))
		assert.Equal(t, CancelSTPTaker, res.Events[2].CancelReason)
		requireState(t, h.b, "b1", StatusCancelled, 0, 0, 4)
		requireState(t, h.b, "s1", StatusOpen, 0, 5, 0)
	})
	t.Run("post-only replace that would cross is rejected and the order is untouched", func(t *testing.T) {
		h := newHarness(t)
		h.apply(lim("s1", "bob", SideSell, 100, 5))
		h.apply(postOnly(lim("b1", "alice", SideBuy, 98, 4)))
		before := mustOrder(t, h.b, "b1")
		requireRejected(t, h.apply(Replace{OrderID: "b1", OwnerID: "alice", NewPriceTicks: 100, NewQtyLots: 4}), RejectPostOnlyWouldCross)
		assert.Equal(t, before, mustOrder(t, h.b, "b1"))
		h.apply(Replace{OrderID: "b1", OwnerID: "alice", NewPriceTicks: 99, NewQtyLots: 4})
		assert.Equal(t, int64(99), mustOrder(t, h.b, "b1").PriceTicks)
	})
}

func TestCancel(t *testing.T) {
	h := newHarness(t)
	h.apply(lim("s1", "alice", SideSell, 100, 5))
	h.apply(lim("s2", "alice", SideSell, 100, 5))

	res := h.apply(Cancel{Timestamp: 9, OrderID: "s1", OwnerID: "alice"})
	require.Equal(t, []EventType{EventCancelled, EventBookDelta}, typesOf(res.Events))
	c := res.Events[0]
	assert.Equal(t, CancelUser, c.CancelReason)
	assert.Equal(t, CommandCancel, c.Command)
	assert.Equal(t, int64(9), c.Timestamp)
	assert.Equal(t, int64(5), c.QtyLots)
	assert.Equal(t, int64(5), res.Events[1].LevelQtyLots)
	st := requireState(t, h.b, "s1", StatusCancelled, 0, 0, 5)
	assert.Equal(t, CancelUser, st.CancelReason)
	assert.Zero(t, st.Priority)

	requireRejected(t, h.apply(Cancel{OrderID: "s1", OwnerID: "alice"}), RejectOrderNotOpen)
	requireRejected(t, h.apply(Cancel{OrderID: "s2", OwnerID: "bob"}), RejectNotOwner)
	requireRejected(t, h.apply(Cancel{OrderID: "zz", OwnerID: "alice"}), RejectUnknownOrder)
	requireRejected(t, h.apply(Cancel{OrderID: "", OwnerID: "alice"}), RejectInvalidOrderID)
	requireRejected(t, h.apply(Cancel{OrderID: "s2", OwnerID: ""}), RejectInvalidOwner)
	requireState(t, h.b, "s2", StatusOpen, 0, 5, 0)
}

func TestCancelPartiallyFilledOrder(t *testing.T) {
	h := newHarness(t)
	h.apply(lim("s1", "alice", SideSell, 100, 5))
	h.apply(ioc(lim("b1", "bob", SideBuy, 100, 2)))
	requireState(t, h.b, "s1", StatusPartiallyFilled, 2, 3, 0)

	res := h.apply(Cancel{OrderID: "s1", OwnerID: "alice"})
	c := res.Events[0]
	assert.Equal(t, []int64{3, 2, 0}, []int64{c.QtyLots, c.FilledLots, c.RemainingLots})
	st := requireState(t, h.b, "s1", StatusCancelled, 2, 0, 3)
	assert.Equal(t, int64(5), st.QtyLots)
	assert.Equal(t, int64(0), res.Events[1].LevelQtyLots)

	h.apply(lim("s2", "alice", SideSell, 100, 1))
	h.apply(ioc(lim("b2", "bob", SideBuy, 100, 1)))
	requireRejected(t, h.apply(Cancel{OrderID: "s2", OwnerID: "alice"}), RejectOrderNotOpen)
}

func TestReduceOnly(t *testing.T) {
	t.Run("quantity above the limit is cancelled on entry and fills never exceed it", func(t *testing.T) {
		h := newHarness(t)
		h.apply(lim("s1", "bob", SideSell, 100, 3))
		res := h.apply(reduceOnly(lim("b1", "alice", SideBuy, 100, 10), 6))
		require.Equal(t, []EventType{EventAccepted, EventCancelled,
			EventTrade, EventFullFill, EventPartialFill, EventBookDelta,
			EventResting, EventBookDelta}, typesOf(res.Events))
		assert.True(t, res.Events[0].ReduceOnly)
		assert.Equal(t, int64(10), res.Events[0].QtyLots)
		c := res.Events[1]
		assert.Equal(t, CancelReduceOnlyCap, c.CancelReason)
		assert.Equal(t, []int64{4, 0, 6}, []int64{c.QtyLots, c.FilledLots, c.RemainingLots})
		st := requireState(t, h.b, "b1", StatusPartiallyFilled, 3, 3, 4)
		assert.Equal(t, int64(10), st.QtyLots)
		assert.Empty(t, st.CancelReason, "a cap does not close the order")

		h.apply(lim("s2", "carol", SideSell, 100, 10))
		requireState(t, h.b, "b1", StatusFilled, 6, 0, 4)
		requireState(t, h.b, "s2", StatusPartiallyFilled, 3, 7, 0)
	})
	t.Run("IOC fills up to the limit", func(t *testing.T) {
		h := newHarness(t)
		h.apply(lim("s1", "bob", SideSell, 100, 10))
		res := h.apply(reduceOnly(ioc(lim("b1", "alice", SideBuy, 100, 10)), 4))
		assert.Equal(t, []EventType{EventAccepted, EventCancelled, EventTrade, EventPartialFill, EventFullFill,
			EventBookDelta}, typesOf(res.Events))
		requireState(t, h.b, "b1", StatusFilled, 4, 0, 6)
	})
	t.Run("limit at or above qty does not cap", func(t *testing.T) {
		h := newHarness(t)
		res := h.apply(reduceOnly(lim("b1", "alice", SideBuy, 100, 5), 5))
		assert.Equal(t, []EventType{EventAccepted, EventResting, EventBookDelta}, typesOf(res.Events))
	})
	t.Run("FOK checks the capped quantity", func(t *testing.T) {
		h := newHarness(t)
		h.apply(lim("s1", "bob", SideSell, 100, 3))
		res := h.apply(reduceOnly(fok(lim("b1", "alice", SideBuy, 100, 10)), 4))
		require.Equal(t, []EventType{EventAccepted, EventCancelled, EventCancelled}, typesOf(res.Events))
		assert.Equal(t, CancelReduceOnlyCap, res.Events[1].CancelReason)
		assert.Equal(t, CancelFOK, res.Events[2].CancelReason)
		requireState(t, h.b, "b1", StatusCancelled, 0, 0, 10)
		res = h.apply(reduceOnly(fok(lim("b2", "alice", SideBuy, 100, 10)), 3))
		requireState(t, h.b, "b2", StatusFilled, 3, 0, 7)
		assert.Len(t, eventsOf(res.Events, EventTrade), 1)
	})
	t.Run("no position, bad limits and replaces above the limit are rejected", func(t *testing.T) {
		h := newHarness(t)
		requireRejected(t, h.apply(reduceOnly(lim("b1", "alice", SideBuy, 100, 5), 0)), RejectReduceOnlyNoPosition)
		requireRejected(t, h.apply(reduceOnly(lim("b2", "alice", SideBuy, 100, 5), -1)), RejectInvalidReduceOnly)
		requireRejected(t, h.apply(reduceOnly(lim("b3", "alice", SideBuy, 100, 5), MaxQtyLots+1)), RejectInvalidReduceOnly)
		o := lim("b4", "alice", SideBuy, 100, 5)
		o.ReduceOnlyLimitLots = 3
		requireRejected(t, h.apply(o), RejectInvalidReduceOnly)

		h.apply(reduceOnly(lim("b5", "alice", SideBuy, 100, 10), 6))
		requireRejected(t, h.apply(Replace{OrderID: "b5", OwnerID: "alice", NewPriceTicks: 100, NewQtyLots: 7}), RejectReduceOnlyLimitExceeded)
		h.apply(Replace{OrderID: "b5", OwnerID: "alice", NewPriceTicks: 99, NewQtyLots: 5})
		st := requireState(t, h.b, "b5", StatusOpen, 0, 5, 4)
		assert.Equal(t, int64(9), st.QtyLots, "quantity = new working quantity + what the cap cancelled")
	})
}

func TestDuplicateIdentifiers(t *testing.T) {
	h := newHarness(t)
	h.apply(NewOrder{OrderID: "o1", ClientOrderID: "c1", OwnerID: "alice", Side: SideBuy, Type: OrderTypeLimit, TIF: TIFGTC, PriceTicks: 100, QtyLots: 1})

	t.Run("order id of an accepted order", func(t *testing.T) {
		before := mustOrder(t, h.b, "o1")
		requireRejected(t, h.apply(lim("o1", "bob", SideSell, 200, 1)), RejectDuplicateOrderID)
		assert.Equal(t, before, mustOrder(t, h.b, "o1"), "a duplicate does not touch the original")
	})
	t.Run("order id of a closed order", func(t *testing.T) {
		h.apply(Cancel{OrderID: "o1", OwnerID: "alice"})
		requireRejected(t, h.apply(lim("o1", "alice", SideBuy, 100, 1)), RejectDuplicateOrderID)
	})
	t.Run("order id of a rejected order", func(t *testing.T) {
		requireRejected(t, h.apply(lim("r1", "alice", SideBuy, 0, 1)), RejectInvalidPrice)
		requireRejected(t, h.apply(lim("r1", "alice", SideBuy, 100, 1)), RejectDuplicateOrderID)
	})
	t.Run("an invalid order id is not consumed", func(t *testing.T) {
		long := strings.Repeat("x", MaxIDBytes+1)
		requireRejected(t, h.apply(lim(long, "alice", SideBuy, 100, 1)), RejectInvalidOrderID)
		_, ok := h.b.Order(long)
		assert.False(t, ok)
	})
	t.Run("client order id is unique per owner among accepted orders, forever", func(t *testing.T) {
		dup := NewOrder{OrderID: "o2", ClientOrderID: "c1", OwnerID: "alice", Side: SideBuy, Type: OrderTypeLimit, TIF: TIFGTC, PriceTicks: 100, QtyLots: 1}
		requireRejected(t, h.apply(dup), RejectDuplicateClientOrderID) // o1 is cancelled, its client id is retained
		other := dup
		other.OrderID, other.OwnerID = "o3", "bob"
		h.apply(other) // same client id, different owner
		assert.Equal(t, StatusOpen, mustOrder(t, h.b, "o3").Status)
	})
	t.Run("a rejected order does not consume its client order id", func(t *testing.T) {
		bad := NewOrder{OrderID: "o4", ClientOrderID: "c9", OwnerID: "alice", Side: SideBuy, Type: OrderTypeLimit, TIF: TIFGTC, QtyLots: 1}
		requireRejected(t, h.apply(bad), RejectInvalidPrice)
		bad.OrderID, bad.PriceTicks = "o5", 100
		h.apply(bad)
		assert.Equal(t, "c9", mustOrder(t, h.b, "o5").ClientOrderID)
	})
	t.Run("empty client order ids are never deduplicated", func(t *testing.T) {
		h.apply(lim("o6", "alice", SideBuy, 90, 1))
		h.apply(lim("o7", "alice", SideBuy, 90, 1))
		assert.Equal(t, StatusOpen, mustOrder(t, h.b, "o7").Status)
	})
}

func TestNewOrderValidation(t *testing.T) {
	base := func() NewOrder { return lim("v", "alice", SideBuy, 100, 5) }
	long := strings.Repeat("x", MaxIDBytes+1)
	cases := []struct {
		name string
		mod  func(*NewOrder)
		want RejectReason
	}{
		{"empty order id", func(o *NewOrder) { o.OrderID = "" }, RejectInvalidOrderID},
		{"long order id", func(o *NewOrder) { o.OrderID = long }, RejectInvalidOrderID},
		{"empty owner", func(o *NewOrder) { o.OwnerID = "" }, RejectInvalidOwner},
		{"long owner", func(o *NewOrder) { o.OwnerID = long }, RejectInvalidOwner},
		{"long client order id", func(o *NewOrder) { o.ClientOrderID = long }, RejectInvalidClientOrderID},
		{"zero side", func(o *NewOrder) { o.Side = 0 }, RejectInvalidSide},
		{"unknown side", func(o *NewOrder) { o.Side = 3 }, RejectInvalidSide},
		{"zero type", func(o *NewOrder) { o.Type = 0 }, RejectInvalidOrderType},
		{"unknown type", func(o *NewOrder) { o.Type = 9 }, RejectInvalidOrderType},
		{"zero tif", func(o *NewOrder) { o.TIF = 0 }, RejectInvalidTIF},
		{"unknown tif", func(o *NewOrder) { o.TIF = 4 }, RejectInvalidTIF},
		{"market GTC", func(o *NewOrder) { o.Type, o.PriceTicks = OrderTypeMarket, 0 }, RejectInvalidTIF},
		{"unknown stp", func(o *NewOrder) { o.STP = 3 }, RejectInvalidSTP},
		{"zero price", func(o *NewOrder) { o.PriceTicks = 0 }, RejectInvalidPrice},
		{"negative price", func(o *NewOrder) { o.PriceTicks = -1 }, RejectInvalidPrice},
		{"price above max", func(o *NewOrder) { o.PriceTicks = MaxPriceTicks + 1 }, RejectInvalidPrice},
		{"negative market protection", func(o *NewOrder) { o.Type, o.TIF, o.PriceTicks = OrderTypeMarket, TIFIOC, -1 }, RejectInvalidPrice},
		{"zero qty", func(o *NewOrder) { o.QtyLots = 0 }, RejectInvalidQty},
		{"negative qty", func(o *NewOrder) { o.QtyLots = -5 }, RejectInvalidQty},
		{"qty above max", func(o *NewOrder) { o.QtyLots = MaxQtyLots + 1 }, RejectInvalidQty},
		{"notional above max", func(o *NewOrder) { o.PriceTicks, o.QtyLots = 1_000_000, 10_000_000_000_000 }, RejectNotionalTooLarge},
		{"post-only IOC", func(o *NewOrder) { o.PostOnly, o.TIF = true, TIFIOC }, RejectInvalidPostOnly},
		{"post-only FOK", func(o *NewOrder) { o.PostOnly, o.TIF = true, TIFFOK }, RejectInvalidPostOnly},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			o := base()
			tc.mod(&o)
			res := h.apply(o)
			requireRejected(t, res, tc.want)
			e := res.Events[0]
			assert.Equal(t, o.OrderID, e.OrderID, "the rejection echoes the command")
			assert.Equal(t, o.QtyLots, e.QtyLots)
			assert.Equal(t, o.PriceTicks, e.PriceTicks)
			st, ok := h.b.Order(o.OrderID)
			if tc.want == RejectInvalidOrderID {
				assert.False(t, ok, "an invalid order id is not recorded")
				return
			}
			require.True(t, ok)
			assert.Equal(t, OrderState{OrderID: o.OrderID, Status: StatusRejected, RejectReason: tc.want}, st)
		})
	}
	t.Run("notional exactly at the bound is accepted", func(t *testing.T) {
		h := newHarness(t)
		res := h.apply(lim("n1", "alice", SideBuy, 1_000, MaxQtyLots))
		assert.Equal(t, EventAccepted, res.Events[0].Type)
	})
	t.Run("per-book price ceiling", func(t *testing.T) {
		b, err := NewBook(Config{Instrument: "WILL-IT-RAIN", MaxPriceTicks: 99})
		require.NoError(t, err)
		res, err := b.Apply(lim("p1", "alice", SideBuy, 100, 1))
		require.NoError(t, err)
		requireRejected(t, res, RejectInvalidPrice)
		res, err = b.Apply(lim("p2", "alice", SideBuy, 99, 1))
		require.NoError(t, err)
		assert.Equal(t, EventAccepted, res.Events[0].Type)
		assert.Equal(t, Config{Instrument: "WILL-IT-RAIN", MaxPriceTicks: 99}, b.Config())
	})
}

func TestApplyRejectsNonCommandsWithoutSideEffects(t *testing.T) {
	h := newHarness(t)
	h.apply(lim("s1", "alice", SideSell, 100, 5))
	snap := h.b.Snapshot()
	for name, cmd := range map[string]Command{
		"nil":             nil,
		"pointer":         &NewOrder{OrderID: "x", OwnerID: "a", Side: SideBuy, Type: OrderTypeLimit, TIF: TIFGTC, PriceTicks: 1, QtyLots: 1},
		"nil pointer":     (*Cancel)(nil),
		"pointer replace": &Replace{OrderID: "s1", OwnerID: "alice", NewPriceTicks: 100, NewQtyLots: 1},
	} {
		res, err := h.b.Apply(cmd)
		require.Error(t, err, name)
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), name)
		assert.Equal(t, Result{}, res, name)
		assert.Equal(t, snap, h.b.Snapshot(), "%s changed the book", name)
	}
	assert.Equal(t, int64(1), h.b.CommandSeq())
}

func TestNewBookValidation(t *testing.T) {
	for name, cfg := range map[string]Config{
		"empty instrument":    {},
		"long instrument":     {Instrument: strings.Repeat("i", MaxIDBytes+1)},
		"negative max price":  {Instrument: "X", MaxPriceTicks: -1},
		"max price above cap": {Instrument: "X", MaxPriceTicks: MaxPriceTicks + 1},
	} {
		_, err := NewBook(cfg)
		require.Error(t, err, name)
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), name)
	}
	b, err := NewBook(Config{Instrument: "X"})
	require.NoError(t, err)
	assert.Equal(t, MaxPriceTicks, b.Config().MaxPriceTicks)
}

func TestQueries(t *testing.T) {
	h := newHarness(t)
	h.apply(lim("b1", "alice", SideBuy, 98, 1))
	h.apply(lim("b2", "bob", SideBuy, 99, 2))
	h.apply(lim("b3", "alice", SideBuy, 99, 3))
	h.apply(lim("a1", "alice", SideSell, 101, 4))
	h.apply(lim("a2", "bob", SideSell, 102, 5))
	h.apply(lim("a3", "alice", SideSell, 103, 6))

	bid, ok := h.b.BestBid()
	require.True(t, ok)
	assert.Equal(t, Level{PriceTicks: 99, QtyLots: 5, Orders: 2}, bid)
	ask, ok := h.b.BestAsk()
	require.True(t, ok)
	assert.Equal(t, Level{PriceTicks: 101, QtyLots: 4, Orders: 1}, ask)

	assert.Equal(t, Depth{
		Bids: []Level{{PriceTicks: 99, QtyLots: 5, Orders: 2}, {PriceTicks: 98, QtyLots: 1, Orders: 1}},
		Asks: []Level{{PriceTicks: 101, QtyLots: 4, Orders: 1}, {PriceTicks: 102, QtyLots: 5, Orders: 1}, {PriceTicks: 103, QtyLots: 6, Orders: 1}},
	}, h.b.Depth(10))
	assert.Equal(t, Depth{
		Bids: []Level{{PriceTicks: 99, QtyLots: 5, Orders: 2}},
		Asks: []Level{{PriceTicks: 101, QtyLots: 4, Orders: 1}},
	}, h.b.Depth(1))
	assert.Equal(t, Depth{Bids: []Level{}, Asks: []Level{}}, h.b.Depth(0))
	assert.Equal(t, Depth{Bids: []Level{}, Asks: []Level{}}, h.b.Depth(-3))

	ids := func(states []OrderState) []string {
		out := make([]string, len(states))
		for i, s := range states {
			out[i] = s.OrderID
		}
		return out
	}
	assert.Equal(t, []string{"b1", "b3", "a1", "a3"}, ids(h.b.OpenOrders("alice")))
	h.apply(Replace{OrderID: "b1", OwnerID: "alice", NewPriceTicks: 97, NewQtyLots: 1})
	assert.Equal(t, []string{"b3", "a1", "a3", "b1"}, ids(h.b.OpenOrders("alice")), "priority order, oldest first")
	assert.Equal(t, []OrderState{}, h.b.OpenOrders("nobody"))
	_, ok = h.b.Order("missing")
	assert.False(t, ok)

	h.apply(Cancel{OrderID: "a2", OwnerID: "bob"})
	assert.Equal(t, []string{"b2"}, ids(h.b.OpenOrders("bob")))
}

func TestSequenceNumbersAreGapFree(t *testing.T) {
	h := newHarness(t)
	h.apply(lim("s1", "alice", SideSell, 100, 2))
	h.apply(lim("s2", "alice", SideSell, 101, 2))
	h.apply(Cancel{OrderID: "nope", OwnerID: "x"})
	h.apply(lim("b1", "bob", SideBuy, 101, 3))
	h.apply(mkt("m1", "carol", SideBuy, 5))

	var seq, trade int64
	for i, res := range h.results {
		assert.Equal(t, int64(i+1), res.CommandSeq)
		for _, e := range res.Events {
			seq++
			assert.Equal(t, seq, e.Seq)
			if e.Type == EventTrade {
				trade++
				assert.Equal(t, trade, e.TradeSeq)
			}
		}
	}
	assert.Equal(t, seq, h.b.EventSeq())
	assert.Equal(t, trade, h.b.TradeSeq())
	assert.Equal(t, int64(3), trade)
	assert.Equal(t, int64(5), h.b.CommandSeq())
}

func TestBookCapacity(t *testing.T) {
	// 1000 orders of MaxQtyLots at price 1 fill one side to its capacity of
	// 10^18 lots minus 10.
	h := newHarness(t)
	for i := 0; i < 999; i++ {
		h.apply(lim("f"+strconv.Itoa(i), "alice", SideBuy, 1, MaxQtyLots))
	}
	h.apply(lim("last", "alice", SideBuy, 1, MaxQtyLots-10))
	requireRejected(t, h.apply(lim("over", "bob", SideBuy, 1, 11)), RejectBookCapacityExceeded)
	requireRejected(t, h.apply(reduceOnly(lim("over2", "bob", SideBuy, 1, 50), 11)), RejectBookCapacityExceeded)
	h.apply(reduceOnly(lim("ro", "bob", SideBuy, 1, 50), 4)) // only the capped part could rest
	h.apply(lim("fits", "bob", SideBuy, 1, 6))
	bid, _ := h.b.BestBid()
	assert.Equal(t, MaxRestingLotsPerSide, bid.QtyLots)

	h.apply(ioc(lim("ioc", "bob", SideBuy, 1, 5))) // an order that cannot rest is not limited
	requireRejected(t, h.apply(Replace{OrderID: "fits", OwnerID: "bob", NewPriceTicks: 2, NewQtyLots: 7}), RejectBookCapacityExceeded)
	h.apply(Replace{OrderID: "fits", OwnerID: "bob", NewPriceTicks: 2, NewQtyLots: 6})
	assert.Equal(t, int64(2), mustOrder(t, h.b, "fits").PriceTicks)
}

func TestEnumStrings(t *testing.T) {
	assert.Equal(t, "BUY", SideBuy.String())
	assert.Equal(t, "SELL", SideSell.String())
	assert.Equal(t, "Side(7)", Side(7).String())
	assert.Equal(t, SideSell, SideBuy.Opposite())
	assert.Equal(t, "MARKET", OrderTypeMarket.String())
	assert.Equal(t, "FOK", TIFFOK.String())
	assert.Equal(t, "CANCEL_BOTH", STPCancelBoth.String())
	assert.Equal(t, "PARTIALLY_FILLED", StatusPartiallyFilled.String())
	assert.Equal(t, "PARTIAL_FILL", EventPartialFill.String())
	assert.Equal(t, "BOOK_DELTA", EventBookDelta.String())
	assert.Equal(t, "REPLACE", CommandReplace.String())
	assert.Equal(t, "EventType(0)", EventType(0).String())
}
