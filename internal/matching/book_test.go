package matching

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func requireRejected(t *testing.T, res Result, reason RejectReason) {
	t.Helper()
	got, ok := res.Rejected()
	require.True(t, ok, "expected a rejection, got %v", typesOf(res.Events))
	require.Equal(t, reason, got)
}

func requireState(t *testing.T, b *Book, id string, status OrderStatus, filled, remaining, cancelled int64) OrderState {
	t.Helper()
	st, ok := b.Order(id)
	require.True(t, ok, "order %s unknown", id)
	assert.Equal(t, status, st.Status, "status of %s", id)
	assert.Equal(t, filled, st.FilledLots, "filled of %s", id)
	assert.Equal(t, remaining, st.RemainingLots, "remaining of %s", id)
	assert.Equal(t, cancelled, st.CancelledLots, "cancelled of %s", id)
	assert.Equal(t, st.QtyLots, st.FilledLots+st.RemainingLots+st.CancelledLots, "conservation of %s", id)
	return st
}

func TestLimitOrderRestsWhenNotCrossing(t *testing.T) {
	h := newHarness(t)
	res := h.apply(NewOrder{Timestamp: 77, OrderID: "s1", ClientOrderID: "c1", OwnerID: "alice", Side: SideSell,
		Type: OrderTypeLimit, TIF: TIFGTC, PriceTicks: 101, QtyLots: 5})

	require.Equal(t, int64(1), res.CommandSeq)
	require.Equal(t, []EventType{EventAccepted, EventResting, EventBookDelta}, typesOf(res.Events))
	for i, e := range res.Events {
		assert.Equal(t, int64(i+1), e.Seq)
		assert.Equal(t, int64(1), e.CommandSeq)
		assert.Equal(t, int64(77), e.Timestamp)
		assert.Equal(t, CommandNewOrder, e.Command)
	}
	acc := res.Events[0]
	assert.Equal(t, Event{Seq: 1, CommandSeq: 1, Timestamp: 77, Command: CommandNewOrder, Type: EventAccepted,
		OrderID: "s1", ClientOrderID: "c1", OwnerID: "alice", Side: SideSell, OrderType: OrderTypeLimit,
		TimeInForce: TIFGTC, PriceTicks: 101, QtyLots: 5, RemainingLots: 5}, acc)
	rest := res.Events[1]
	assert.Equal(t, rest.Seq, rest.Priority)
	assert.Equal(t, int64(5), rest.QtyLots)
	assert.Equal(t, Event{Seq: 3, CommandSeq: 1, Timestamp: 77, Command: CommandNewOrder, Type: EventBookDelta,
		Side: SideSell, PriceTicks: 101, LevelQtyLots: 5}, res.Events[2])

	ask, ok := h.b.BestAsk()
	require.True(t, ok)
	assert.Equal(t, Level{PriceTicks: 101, QtyLots: 5, Orders: 1}, ask)
	_, ok = h.b.BestBid()
	assert.False(t, ok)
	st := requireState(t, h.b, "s1", StatusOpen, 0, 5, 0)
	assert.Equal(t, rest.Priority, st.Priority)
	assert.Equal(t, int64(77), st.AcceptedTimestamp)
}

func TestFullCrossTradesAtMakerPrice(t *testing.T) {
	h := newHarness(t)
	h.apply(lim("s1", "alice", SideSell, 100, 5))
	res := h.apply(lim("b1", "bob", SideBuy, 105, 5))

	require.Equal(t, []EventType{EventAccepted, EventTrade, EventFullFill, EventFullFill, EventBookDelta}, typesOf(res.Events))
	tr := res.Events[1]
	assert.Equal(t, int64(1), tr.TradeSeq)
	assert.Equal(t, int64(100), tr.PriceTicks, "trades execute at the maker's price")
	assert.Equal(t, int64(5), tr.QtyLots)
	assert.Equal(t, "s1", tr.MakerOrderID)
	assert.Equal(t, "alice", tr.MakerOwnerID)
	assert.Equal(t, "b1", tr.TakerOrderID)
	assert.Equal(t, "bob", tr.TakerOwnerID)
	assert.Equal(t, SideBuy, tr.AggressorSide)
	assert.Equal(t, "s1", res.Events[2].OrderID, "maker fill first")
	assert.Equal(t, "b1", res.Events[3].OrderID)
	assert.Equal(t, int64(100), res.Events[3].PriceTicks, "fill events carry the trade price")
	assert.Equal(t, int64(0), res.Events[4].LevelQtyLots)

	requireState(t, h.b, "s1", StatusFilled, 5, 0, 0)
	requireState(t, h.b, "b1", StatusFilled, 5, 0, 0)
	assert.Equal(t, Depth{Bids: []Level{}, Asks: []Level{}}, h.b.Depth(10))
}

func TestPartialCrossRestsRemainder(t *testing.T) {
	h := newHarness(t)
	h.apply(lim("s1", "alice", SideSell, 100, 3))
	res := h.apply(lim("b1", "bob", SideBuy, 100, 5))

	require.Equal(t, []EventType{EventAccepted, EventTrade, EventFullFill, EventPartialFill, EventBookDelta,
		EventResting, EventBookDelta}, typesOf(res.Events))
	assert.Equal(t, int64(3), res.Events[3].FilledLots)
	assert.Equal(t, int64(2), res.Events[3].RemainingLots)
	assert.Equal(t, int64(2), res.Events[5].QtyLots)
	assert.Equal(t, Event{Seq: res.Events[6].Seq, CommandSeq: 2, Command: CommandNewOrder, Type: EventBookDelta,
		Side: SideBuy, PriceTicks: 100, LevelQtyLots: 2}, res.Events[6])
	requireState(t, h.b, "b1", StatusPartiallyFilled, 3, 2, 0)
	bid, _ := h.b.BestBid()
	assert.Equal(t, Level{PriceTicks: 100, QtyLots: 2, Orders: 1}, bid)
}

func TestPriceTimePriority(t *testing.T) {
	h := newHarness(t)
	h.apply(lim("s1", "alice", SideSell, 101, 2))
	h.apply(lim("s2", "bob", SideSell, 100, 2))
	h.apply(lim("s3", "carol", SideSell, 100, 2))
	h.apply(lim("s4", "alice", SideSell, 102, 2))
	res := h.apply(lim("b1", "dave", SideBuy, 101, 5))

	trades := eventsOf(res.Events, EventTrade)
	require.Len(t, trades, 3)
	makers := []string{trades[0].MakerOrderID, trades[1].MakerOrderID, trades[2].MakerOrderID}
	prices := []int64{trades[0].PriceTicks, trades[1].PriceTicks, trades[2].PriceTicks}
	assert.Equal(t, []string{"s2", "s3", "s1"}, makers, "best price first, then time")
	assert.Equal(t, []int64{100, 100, 101}, prices)
	assert.Equal(t, []int64{1, 2, 3}, []int64{trades[0].TradeSeq, trades[1].TradeSeq, trades[2].TradeSeq})
	requireState(t, h.b, "s1", StatusPartiallyFilled, 1, 1, 0)
	requireState(t, h.b, "b1", StatusFilled, 5, 0, 0)
	assert.Equal(t, []Level{{PriceTicks: 101, QtyLots: 1, Orders: 1}, {PriceTicks: 102, QtyLots: 2, Orders: 1}}, h.b.Depth(5).Asks)
}

func TestSweepStopsAtLimitAndRests(t *testing.T) {
	h := newHarness(t)
	h.apply(lim("s1", "alice", SideSell, 100, 2))
	h.apply(lim("s2", "alice", SideSell, 101, 2))
	h.apply(lim("s3", "alice", SideSell, 102, 5))
	res := h.apply(lim("b1", "bob", SideBuy, 101, 10))
	assert.Len(t, eventsOf(res.Events, EventTrade), 2)
	requireState(t, h.b, "b1", StatusPartiallyFilled, 4, 6, 0)
	assert.Equal(t, Depth{
		Bids: []Level{{PriceTicks: 101, QtyLots: 6, Orders: 1}},
		Asks: []Level{{PriceTicks: 102, QtyLots: 5, Orders: 1}},
	}, h.b.Depth(10))
}

func TestSellTakerMatchesBidsBestFirst(t *testing.T) {
	h := newHarness(t)
	h.apply(lim("b1", "alice", SideBuy, 99, 2))
	h.apply(lim("b2", "alice", SideBuy, 101, 2))
	h.apply(lim("b3", "alice", SideBuy, 100, 2))
	res := h.apply(ioc(lim("s1", "bob", SideSell, 100, 10)))
	trades := eventsOf(res.Events, EventTrade)
	require.Len(t, trades, 2)
	assert.Equal(t, []int64{101, 100}, []int64{trades[0].PriceTicks, trades[1].PriceTicks})
	assert.Equal(t, SideSell, trades[0].AggressorSide)
	last := res.Events[len(res.Events)-1]
	assert.Equal(t, EventCancelled, last.Type)
	assert.Equal(t, CancelIOCRemainder, last.CancelReason)
	assert.Equal(t, int64(6), last.QtyLots)
}

func TestIOC(t *testing.T) {
	t.Run("partial fill cancels the remainder", func(t *testing.T) {
		h := newHarness(t)
		h.apply(lim("s1", "alice", SideSell, 100, 3))
		res := h.apply(ioc(lim("b1", "bob", SideBuy, 100, 5)))
		require.Equal(t, []EventType{EventAccepted, EventTrade, EventFullFill, EventPartialFill, EventBookDelta,
			EventCancelled}, typesOf(res.Events))
		c := res.Events[5]
		assert.Equal(t, CancelIOCRemainder, c.CancelReason)
		assert.Equal(t, int64(2), c.QtyLots)
		assert.Equal(t, int64(3), c.FilledLots)
		assert.Equal(t, int64(0), c.RemainingLots)
		st := requireState(t, h.b, "b1", StatusCancelled, 3, 0, 2)
		assert.Equal(t, CancelIOCRemainder, st.CancelReason)
		_, ok := h.b.BestBid()
		assert.False(t, ok, "IOC never rests")
	})
	t.Run("nothing crosses: accepted and cancelled with zero fills", func(t *testing.T) {
		h := newHarness(t)
		h.apply(lim("s1", "alice", SideSell, 100, 3))
		res := h.apply(ioc(lim("b1", "bob", SideBuy, 99, 5)))
		require.Equal(t, []EventType{EventAccepted, EventCancelled}, typesOf(res.Events))
		assert.Equal(t, CancelIOCRemainder, res.Events[1].CancelReason)
		requireState(t, h.b, "b1", StatusCancelled, 0, 0, 5)
	})
}

func TestFOK(t *testing.T) {
	setup := func(t *testing.T) *harness {
		h := newHarness(t)
		h.apply(lim("s1", "alice", SideSell, 100, 3))
		h.apply(lim("s2", "bob", SideSell, 101, 3))
		return h
	}
	t.Run("insufficient liquidity: cancelled with zero fills, book untouched", func(t *testing.T) {
		h := setup(t)
		before := h.b.Depth(10)
		res := h.apply(fok(lim("b1", "carol", SideBuy, 101, 7)))
		require.Equal(t, []EventType{EventAccepted, EventCancelled}, typesOf(res.Events))
		assert.Equal(t, CancelFOK, res.Events[1].CancelReason)
		assert.Equal(t, int64(7), res.Events[1].QtyLots)
		requireState(t, h.b, "b1", StatusCancelled, 0, 0, 7)
		assert.Equal(t, before, h.b.Depth(10))
		requireState(t, h.b, "s1", StatusOpen, 0, 3, 0)
	})
	t.Run("limit price excludes liquidity", func(t *testing.T) {
		h := setup(t)
		res := h.apply(fok(lim("b1", "carol", SideBuy, 100, 6)))
		require.Equal(t, []EventType{EventAccepted, EventCancelled}, typesOf(res.Events))
	})
	t.Run("sufficient liquidity: executes completely across levels", func(t *testing.T) {
		h := setup(t)
		res := h.apply(fok(lim("b1", "carol", SideBuy, 101, 6)))
		assert.Len(t, eventsOf(res.Events, EventTrade), 2)
		assert.Empty(t, eventsOf(res.Events, EventCancelled))
		requireState(t, h.b, "b1", StatusFilled, 6, 0, 0)
	})
}

func TestFOKRespectsSelfTradePrevention(t *testing.T) {
	setup := func(t *testing.T) *harness {
		h := newHarness(t)
		h.apply(lim("s1", "bob", SideSell, 100, 2))
		h.apply(lim("s2", "alice", SideSell, 100, 5))
		h.apply(lim("s3", "carol", SideSell, 101, 5))
		return h
	}
	t.Run("CANCEL_TAKER counts only liquidity ahead of the self order", func(t *testing.T) {
		h := setup(t)
		res := h.apply(fok(lim("b1", "alice", SideBuy, 101, 4)))
		require.Equal(t, []EventType{EventAccepted, EventCancelled}, typesOf(res.Events))
		assert.Equal(t, CancelFOK, res.Events[1].CancelReason)
		requireState(t, h.b, "s2", StatusOpen, 0, 5, 0)

		res = h.apply(fok(lim("b2", "alice", SideBuy, 101, 2)))
		assert.Len(t, eventsOf(res.Events, EventTrade), 1)
		requireState(t, h.b, "b2", StatusFilled, 2, 0, 0)
		requireState(t, h.b, "s2", StatusOpen, 0, 5, 0)
	})
	t.Run("CANCEL_BOTH insufficient leaves the self order untouched", func(t *testing.T) {
		h := setup(t)
		res := h.apply(stp(fok(lim("b1", "alice", SideBuy, 101, 4)), STPCancelBoth))
		require.Equal(t, []EventType{EventAccepted, EventCancelled}, typesOf(res.Events))
		requireState(t, h.b, "s2", StatusOpen, 0, 5, 0)
	})
	t.Run("CANCEL_MAKER skips the self order and cancels it while executing", func(t *testing.T) {
		h := setup(t)
		res := h.apply(stp(fok(lim("b1", "alice", SideBuy, 101, 4)), STPCancelMaker))
		require.Equal(t, []EventType{
			EventAccepted,
			EventTrade, EventFullFill, EventPartialFill, EventBookDelta, // s1
			EventCancelled, EventBookDelta, // s2 by STP
			EventTrade, EventPartialFill, EventFullFill, EventBookDelta, // s3
		}, typesOf(res.Events))
		assert.Equal(t, CancelSTPMaker, res.Events[5].CancelReason)
		assert.Equal(t, "s2", res.Events[5].OrderID)
		requireState(t, h.b, "b1", StatusFilled, 4, 0, 0)
		requireState(t, h.b, "s2", StatusCancelled, 0, 0, 5)
	})
	t.Run("CANCEL_MAKER insufficient has no side effects", func(t *testing.T) {
		h := setup(t)
		res := h.apply(stp(fok(lim("b1", "alice", SideBuy, 101, 8)), STPCancelMaker))
		require.Equal(t, []EventType{EventAccepted, EventCancelled}, typesOf(res.Events))
		requireState(t, h.b, "s2", StatusOpen, 0, 5, 0)
	})
}

func TestPostOnly(t *testing.T) {
	h := newHarness(t)
	h.apply(lim("s1", "alice", SideSell, 100, 3))

	res := h.apply(postOnly(lim("b1", "bob", SideBuy, 100, 2)))
	requireRejected(t, res, RejectPostOnlyWouldCross)
	st, ok := h.b.Order("b1")
	require.True(t, ok)
	assert.Equal(t, OrderState{OrderID: "b1", Status: StatusRejected, RejectReason: RejectPostOnlyWouldCross}, st)

	res = h.apply(postOnly(lim("b2", "alice", SideBuy, 100, 2)))
	requireRejected(t, res, RejectPostOnlyWouldCross) // crossing only the owner's own order still crosses

	res = h.apply(postOnly(lim("b3", "bob", SideBuy, 99, 2)))
	require.Equal(t, []EventType{EventAccepted, EventResting, EventBookDelta}, typesOf(res.Events))
	assert.True(t, res.Events[0].PostOnly)

	requireRejected(t, h.apply(postOnly(ioc(lim("b4", "bob", SideBuy, 90, 2)))), RejectInvalidPostOnly)
	requireRejected(t, h.apply(postOnly(mkt("b5", "bob", SideBuy, 2))), RejectInvalidPostOnly)
	requireState(t, h.b, "s1", StatusOpen, 0, 3, 0)
}

func TestMarketOrders(t *testing.T) {
	setup := func(t *testing.T) *harness {
		h := newHarness(t)
		h.apply(lim("s1", "alice", SideSell, 100, 2))
		h.apply(lim("s2", "alice", SideSell, 101, 2))
		h.apply(lim("s3", "alice", SideSell, 102, 2))
		return h
	}
	t.Run("empty book is rejected and consumes the order id", func(t *testing.T) {
		h := newHarness(t)
		requireRejected(t, h.apply(mkt("m1", "bob", SideBuy, 5)), RejectMarketNoLiquidity)
		requireRejected(t, h.apply(mkt("m1", "bob", SideBuy, 5)), RejectDuplicateOrderID)
		h.apply(lim("b1", "carol", SideBuy, 100, 2))
		requireRejected(t, h.apply(mkt("m2", "bob", SideBuy, 5)), RejectMarketNoLiquidity) // only bids exist
	})
	t.Run("sweeps levels at each maker's price", func(t *testing.T) {
		h := setup(t)
		res := h.apply(mkt("m1", "bob", SideBuy, 5))
		trades := eventsOf(res.Events, EventTrade)
		require.Len(t, trades, 3)
		assert.Equal(t, []int64{100, 101, 102}, []int64{trades[0].PriceTicks, trades[1].PriceTicks, trades[2].PriceTicks})
		assert.Equal(t, []int64{2, 2, 1}, []int64{trades[0].QtyLots, trades[1].QtyLots, trades[2].QtyLots})
		requireState(t, h.b, "m1", StatusFilled, 5, 0, 0)
	})
	t.Run("remainder is cancelled when the book runs out", func(t *testing.T) {
		h := setup(t)
		res := h.apply(mkt("m1", "bob", SideBuy, 10))
		last := res.Events[len(res.Events)-1]
		assert.Equal(t, EventCancelled, last.Type)
		assert.Equal(t, CancelMarketRemainder, last.CancelReason)
		assert.Equal(t, int64(4), last.QtyLots)
		requireState(t, h.b, "m1", StatusCancelled, 6, 0, 4)
		_, ok := h.b.BestBid()
		assert.False(t, ok, "market orders never rest")
	})
	t.Run("protection price bounds the sweep", func(t *testing.T) {
		h := setup(t)
		m := mkt("m1", "bob", SideBuy, 5)
		m.PriceTicks = 101
		res := h.apply(m)
		assert.Len(t, eventsOf(res.Events, EventTrade), 2)
		requireState(t, h.b, "m1", StatusCancelled, 4, 0, 1)
		ask, _ := h.b.BestAsk()
		assert.Equal(t, int64(102), ask.PriceTicks)
	})
	t.Run("protection price excluding all liquidity is rejected", func(t *testing.T) {
		h := setup(t)
		m := mkt("m1", "bob", SideBuy, 5)
		m.PriceTicks = 99
		requireRejected(t, h.apply(m), RejectMarketNoLiquidity)
	})
	t.Run("market sell", func(t *testing.T) {
		h := newHarness(t)
		h.apply(lim("b1", "alice", SideBuy, 99, 2))
		h.apply(lim("b2", "alice", SideBuy, 98, 2))
		m := mkt("m1", "bob", SideSell, 3)
		m.PriceTicks = 98
		res := h.apply(m)
		trades := eventsOf(res.Events, EventTrade)
		require.Len(t, trades, 2)
		assert.Equal(t, []int64{99, 98}, []int64{trades[0].PriceTicks, trades[1].PriceTicks})
	})
	t.Run("market FOK", func(t *testing.T) {
		h := setup(t)
		m := mkt("m1", "bob", SideBuy, 7)
		m.TIF = TIFFOK
		res := h.apply(m)
		require.Equal(t, []EventType{EventAccepted, EventCancelled}, typesOf(res.Events))
		assert.Equal(t, CancelFOK, res.Events[1].CancelReason)
		m.OrderID, m.QtyLots = "m2", 6
		res = h.apply(m)
		assert.Len(t, eventsOf(res.Events, EventTrade), 3)
		requireState(t, h.b, "m2", StatusFilled, 6, 0, 0)
	})
	t.Run("market GTC is invalid", func(t *testing.T) {
		h := setup(t)
		m := mkt("m1", "bob", SideBuy, 1)
		m.TIF = TIFGTC
		requireRejected(t, h.apply(m), RejectInvalidTIF)
	})
}

func TestSelfTradePrevention(t *testing.T) {
	setup := func(t *testing.T) *harness {
		h := newHarness(t)
		h.apply(lim("s1", "bob", SideSell, 100, 2))
		h.apply(lim("s2", "alice", SideSell, 100, 3))
		h.apply(lim("s3", "carol", SideSell, 101, 4))
		return h
	}
	t.Run("CANCEL_TAKER (default) cancels the incoming remainder, keeps the maker", func(t *testing.T) {
		h := setup(t)
		res := h.apply(lim("b1", "alice", SideBuy, 101, 6))
		require.Equal(t, []EventType{EventAccepted, EventTrade, EventFullFill, EventPartialFill, EventBookDelta,
			EventCancelled}, typesOf(res.Events))
		assert.Equal(t, CancelSTPTaker, res.Events[5].CancelReason)
		assert.Equal(t, int64(4), res.Events[5].QtyLots)
		requireState(t, h.b, "b1", StatusCancelled, 2, 0, 4)
		requireState(t, h.b, "s2", StatusOpen, 0, 3, 0)
		requireState(t, h.b, "s3", StatusOpen, 0, 4, 0)
		_, ok := h.b.BestBid()
		assert.False(t, ok, "an STP-cancelled GTC order does not rest")
	})
	t.Run("CANCEL_MAKER cancels the resting order and keeps matching", func(t *testing.T) {
		h := setup(t)
		res := h.apply(stp(lim("b1", "alice", SideBuy, 101, 6), STPCancelMaker))
		require.Equal(t, []EventType{EventAccepted,
			EventTrade, EventFullFill, EventPartialFill, EventBookDelta,
			EventCancelled, EventBookDelta,
			EventTrade, EventFullFill, EventFullFill, EventBookDelta}, typesOf(res.Events))
		assert.Equal(t, CancelSTPMaker, res.Events[5].CancelReason)
		requireState(t, h.b, "b1", StatusFilled, 6, 0, 0)
		requireState(t, h.b, "s2", StatusCancelled, 0, 0, 3)
	})
	t.Run("CANCEL_BOTH cancels both", func(t *testing.T) {
		h := setup(t)
		res := h.apply(stp(lim("b1", "alice", SideBuy, 101, 6), STPCancelBoth))
		require.Equal(t, []EventType{EventAccepted,
			EventTrade, EventFullFill, EventPartialFill, EventBookDelta,
			EventCancelled, EventBookDelta, EventCancelled}, typesOf(res.Events))
		assert.Equal(t, "s2", res.Events[5].OrderID)
		assert.Equal(t, CancelSTPMaker, res.Events[5].CancelReason)
		assert.Equal(t, "b1", res.Events[7].OrderID)
		assert.Equal(t, CancelSTPTaker, res.Events[7].CancelReason)
		requireState(t, h.b, "b1", StatusCancelled, 2, 0, 4)
		requireState(t, h.b, "s2", StatusCancelled, 0, 0, 3)
		requireState(t, h.b, "s3", StatusOpen, 0, 4, 0)
	})
	t.Run("a market order with only self liquidity is accepted then STP-cancelled", func(t *testing.T) {
		h := newHarness(t)
		h.apply(lim("s1", "alice", SideSell, 100, 2))
		res := h.apply(mkt("m1", "alice", SideBuy, 2))
		require.Equal(t, []EventType{EventAccepted, EventCancelled}, typesOf(res.Events))
		assert.Equal(t, CancelSTPTaker, res.Events[1].CancelReason)
	})
}
