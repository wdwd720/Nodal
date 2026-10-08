package matching

import (
	"strconv"
	"testing"
)

// benchEpoch is how many commands a benchmark applies to one book before
// starting a fresh one (with the timer stopped): the book keeps the record of
// every OrderID it has seen, so this bounds memory while still measuring a
// book with a deep history.
const benchEpoch = 1 << 16

// BenchmarkSubmitLimit measures one simple GTC limit order per op. Orders
// alternate sides inside a ten-tick band either side of a mid, so most rest
// and queue behind others at a handful of levels; one in eight is priced
// through the touch and trades.
func BenchmarkSubmitLimit(b *testing.B) {
	rng := splitmix64(7)
	owners := []string{"a", "b", "c", "d"}
	cmds := make([]Command, benchEpoch)
	for i := range cmds {
		side, price := SideBuy, int64(999-rng.intn(10))
		if i%2 == 1 {
			side, price = SideSell, int64(1001+rng.intn(10))
		}
		if rng.intn(8) == 0 {
			if side == SideBuy {
				price = 1005
			} else {
				price = 995
			}
		}
		cmds[i] = NewOrder{Timestamp: int64(i), OrderID: "L" + strconv.Itoa(i), OwnerID: owners[i%len(owners)],
			Side: side, Type: OrderTypeLimit, TIF: TIFGTC, PriceTicks: price, QtyLots: int64(1 + rng.intn(10))}
	}
	runBench(b, cmds)
}

// BenchmarkMatchingMix measures a realistic mix per op: limit orders near
// the touch (many crossing), market, IOC, FOK and post-only orders, cancels
// and replaces, over eight owners with every STP mode.
func BenchmarkMatchingMix(b *testing.B) {
	owners := make([]string, 8)
	for i := range owners {
		owners[i] = "owner-" + strconv.Itoa(i)
	}
	gen := newStreamGen(99, "M", owners)
	cmds := make([]Command, benchEpoch)
	for i := range cmds {
		cmds[i] = gen.nextCommand()
	}
	runBench(b, cmds)
}

func runBench(b *testing.B, cmds []Command) {
	book, err := NewBook(testCfg)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i > 0 && i%len(cmds) == 0 {
			b.StopTimer()
			book, _ = NewBook(testCfg)
			b.StartTimer()
		}
		if _, err := book.Apply(cmds[i%len(cmds)]); err != nil {
			b.Fatal(err)
		}
	}
}
