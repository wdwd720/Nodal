package orders

import (
	"bytes"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/matching"
)

func sampleCommands() []matching.Command {
	return []matching.Command{
		matching.NewOrder{Timestamp: 1_790_000_000_123_456_789, OrderID: "o-1", ClientOrderID: "c-1", OwnerID: "acct-a",
			Side: matching.SideBuy, Type: matching.OrderTypeLimit, TIF: matching.TIFGTC, STP: matching.STPCancelMaker,
			PriceTicks: matching.MaxPriceTicks, QtyLots: 1000, PostOnly: true},
		matching.NewOrder{Timestamp: -5, OrderID: "o-2", OwnerID: "acct-b", Side: matching.SideSell, Type: matching.OrderTypeMarket,
			TIF: matching.TIFFOK, QtyLots: matching.MaxQtyLots, ReduceOnly: true, ReduceOnlyLimitLots: 7},
		matching.Cancel{Timestamp: 9, OrderID: "o-1", OwnerID: "acct-a"},
		matching.Replace{Timestamp: 10, OrderID: "o-1", OwnerID: "acct-a", NewPriceTicks: 5, NewQtyLots: 6},
	}
}

func TestCommandCodec_RoundTripsEveryKindExactly(t *testing.T) {
	for _, c := range sampleCommands() {
		b, err := EncodeCommand(c)
		require.NoError(t, err)
		got, err := DecodeCommand(b)
		require.NoError(t, err)
		assert.Equal(t, c, got)
	}
}

func TestCommandCodec_RefusesEverythingButTheCanonicalBytes(t *testing.T) {
	b, err := EncodeCommand(matching.Cancel{Timestamp: 9, OrderID: "o-1", OwnerID: "acct-a"})
	require.NoError(t, err)
	for name, bad := range map[string][]byte{
		"leading space":            append([]byte(" "), b...),
		"trailing bytes":           append(append([]byte{}, b...), '\n'),
		"an unknown field":         append(append([]byte{}, b[:len(b)-1]...), []byte(`,"x":1}`)...),
		"a cancel carrying a side": bytes.Replace(b, []byte(`"side":0`), []byte(`"side":1`), 1),
		"an unknown kind":          bytes.Replace(b, []byte(`"kind":2`), []byte(`"kind":7`), 1),
		"empty":                    nil,
	} {
		_, err := DecodeCommand(bad)
		require.Error(t, err, name)
	}
	_, err = EncodeCommand(nil)
	require.Error(t, err)
}

func TestEventCodec_RoundTripsEngineEvents(t *testing.T) {
	b, err := matching.NewBook(matching.Config{Instrument: "codec"})
	require.NoError(t, err)
	for _, c := range sampleCommands() {
		res, err := b.Apply(c)
		require.NoError(t, err)
		for _, e := range res.Events {
			enc, err := EncodeEvent(e)
			require.NoError(t, err)
			got, err := DecodeEvent(enc)
			require.NoError(t, err)
			assert.Equal(t, e, got)
		}
	}
}

// FuzzDecodeCommand: arbitrary bytes never panic the decoder, and anything it
// accepts re-encodes to exactly the same bytes.
func FuzzDecodeCommand(f *testing.F) {
	for _, c := range sampleCommands() {
		b, err := EncodeCommand(c)
		require.NoError(f, err)
		f.Add(b)
	}
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"kind":1,"price_ticks":1e3}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		c, err := DecodeCommand(b)
		if err != nil {
			return
		}
		again, err := EncodeCommand(c)
		require.NoError(t, err)
		require.Equal(t, b, again)
	})
}

// FuzzDecodeEvent: arbitrary bytes never panic the event decoder.
func FuzzDecodeEvent(f *testing.F) {
	b, err := EncodeEvent(matching.Event{Seq: 1, CommandSeq: 1, Type: matching.EventTrade, TradeSeq: 1, PriceTicks: 5, QtyLots: 2})
	require.NoError(f, err)
	f.Add(b)
	f.Add([]byte(`{"seq":"x"}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		e, err := DecodeEvent(b)
		if err != nil {
			return
		}
		again, err := EncodeEvent(e)
		require.NoError(t, err)
		require.Equal(t, b, again)
	})
}

// streamGen draws a random but well-formed command stream over a few owners
// around a mid price: limit, IOC, FOK, post-only, reduce-only and market
// orders, cancels and replaces of orders it has seen.
type streamGen struct {
	owners []string
	ids    []string
	n      int
}

func (g *streamGen) next(rt *rapid.T) matching.Command {
	g.n++
	owner := rapid.SampledFrom(g.owners).Draw(rt, "owner")
	if len(g.ids) > 0 && rapid.IntRange(0, 9).Draw(rt, "cancel or replace") < 3 {
		target := rapid.SampledFrom(g.ids).Draw(rt, "target")
		if rapid.Bool().Draw(rt, "cancel") {
			return matching.Cancel{Timestamp: int64(g.n), OrderID: target, OwnerID: owner}
		}
		return matching.Replace{Timestamp: int64(g.n), OrderID: target, OwnerID: owner,
			NewPriceTicks: rapid.Int64Range(95, 105).Draw(rt, "new price"), NewQtyLots: rapid.Int64Range(1, 12).Draw(rt, "new qty")}
	}
	id := "o-" + strconv.Itoa(g.n)
	g.ids = append(g.ids, id)
	c := matching.NewOrder{Timestamp: int64(g.n), OrderID: id, OwnerID: owner,
		Side: rapid.SampledFrom([]matching.Side{matching.SideBuy, matching.SideSell}).Draw(rt, "side"),
		Type: matching.OrderTypeLimit, TIF: matching.TIFGTC, PriceTicks: rapid.Int64Range(95, 105).Draw(rt, "price"),
		QtyLots: rapid.Int64Range(1, 10).Draw(rt, "qty"),
		STP:     rapid.SampledFrom([]matching.STPMode{matching.STPCancelTaker, matching.STPCancelMaker, matching.STPCancelBoth}).Draw(rt, "stp")}
	switch rapid.IntRange(0, 7).Draw(rt, "shape") {
	case 0:
		c.TIF = matching.TIFIOC
	case 1:
		c.TIF = matching.TIFFOK
	case 2:
		c.PostOnly = true
	case 3:
		c.Type, c.TIF = matching.OrderTypeMarket, rapid.SampledFrom([]matching.TimeInForce{matching.TIFIOC, matching.TIFFOK}).Draw(rt, "market tif")
	case 4:
		c.ReduceOnly, c.ReduceOnlyLimitLots = true, rapid.Int64Range(1, 10).Draw(rt, "reduce limit")
	}
	return c
}

// TestProp_JournalThroughTheCodecReplaysByteIdenticallyAndNeverCrosses: a
// random command stream, journalled through the canonical codec and replayed
// on a fresh book, reproduces every event and every snapshot byte for byte;
// a snapshot taken mid-stream plus the rest of the journal reproduces the
// same final state; and after every single command the resting book is not
// crossed.
func TestProp_JournalThroughTheCodecReplaysByteIdenticallyAndNeverCrosses(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		cfg := matching.Config{Instrument: "prop"}
		live, err := matching.NewBook(cfg)
		require.NoError(rt, err)
		g := &streamGen{owners: []string{"a", "b", "c"}}
		n := rapid.IntRange(1, 120).Draw(rt, "commands")
		mid := rapid.IntRange(0, n).Draw(rt, "snapshot at")
		var journal [][]byte
		var results []matching.Result
		var midSnap []byte
		for i := 0; i < n; i++ {
			cmd := g.next(rt)
			res, err := live.Apply(cmd)
			require.NoError(rt, err)
			b, err := EncodeCommand(cmd)
			require.NoError(rt, err)
			journal = append(journal, b)
			results = append(results, res)
			if bid, ok := live.BestBid(); ok {
				if ask, ok := live.BestAsk(); ok && bid.PriceTicks >= ask.PriceTicks {
					rt.Fatalf("crossed book after command %d: bid %d ask %d", i+1, bid.PriceTicks, ask.PriceTicks)
				}
			}
			if i+1 == mid {
				midSnap = live.Snapshot()
			}
		}
		replica, err := matching.NewBook(cfg)
		require.NoError(rt, err)
		for i, b := range journal {
			cmd, err := DecodeCommand(b)
			require.NoError(rt, err)
			res, err := replica.Apply(cmd)
			require.NoError(rt, err)
			require.Equal(rt, results[i], res, "command %d", i+1)
		}
		require.Equal(rt, live.Snapshot(), replica.Snapshot())
		restored, err := matching.NewBook(cfg)
		require.NoError(rt, err)
		from := 0
		if midSnap != nil {
			restored, err = matching.Restore(midSnap)
			require.NoError(rt, err)
			from = mid
		}
		for _, b := range journal[from:] {
			cmd, err := DecodeCommand(b)
			require.NoError(rt, err)
			_, err = restored.Apply(cmd)
			require.NoError(rt, err)
		}
		require.Equal(rt, live.StateHash(), restored.StateHash())
	})
}
