package matching

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// byteReader yields the input's bytes and then zeros.
type byteReader struct {
	data []byte
	pos  int
}

func (r *byteReader) more() bool { return r.pos < len(r.data) }

func (r *byteReader) next() byte {
	if r.pos >= len(r.data) {
		return 0
	}
	b := r.data[r.pos]
	r.pos++
	return b
}

// fuzzCommands decodes arbitrary bytes into at most 256 commands. Every input
// maps to some sequence. Most opcodes build well-formed commands over three
// owners and a seven-tick band, so crossing, self-trades and races against
// resting orders are frequent; one opcode passes raw bytes through enums,
// prices, quantities and ids so malformed and duplicate commands appear too.
func fuzzCommands(data []byte) []Command {
	owners := [...]string{"alice", "bob", "carol"}
	r := byteReader{data: data}
	var cmds []Command
	var ids []string
	ownerOf := map[string]string{}
	remember := func(id, owner string) {
		if _, ok := ownerOf[id]; !ok && id != "" {
			ids = append(ids, id)
			ownerOf[id] = owner
		}
	}
	target := func() (string, string) {
		if len(ids) == 0 {
			return "ghost", "alice"
		}
		id := ids[int(r.next())%len(ids)]
		owner := ownerOf[id]
		if r.next()%8 == 0 {
			owner = owners[int(r.next())%len(owners)]
		}
		return id, owner
	}
	for r.more() && len(cmds) < 256 {
		op := r.next()
		ts := int64(len(cmds))
		id := "f" + strconv.Itoa(len(cmds))
		owner := owners[int(r.next())%len(owners)]
		switch op % 10 {
		case 0, 1, 2, 3, 4:
			flags := r.next()
			o := NewOrder{Timestamp: ts, OrderID: id, OwnerID: owner, Side: Side(1 + flags&1),
				Type: OrderTypeLimit, TIF: TimeInForce(1 + (flags>>1)%3), STP: STPMode((flags >> 3) % 3),
				PriceTicks: int64(97 + r.next()%7), QtyLots: int64(1 + r.next()%9)}
			o.PostOnly = flags&0x20 != 0 && o.TIF == TIFGTC
			if flags&0x40 != 0 {
				o.ClientOrderID = "k" + strconv.Itoa(int(r.next()%4))
			}
			if flags&0x80 != 0 {
				o.ReduceOnly = true
				o.ReduceOnlyLimitLots = int64(r.next() % 8)
			}
			if op%10 == 4 {
				o.Type, o.TIF, o.PostOnly = OrderTypeMarket, TimeInForce(2+flags%2), false
				o.PriceTicks = 0
				if flags&0x04 != 0 {
					o.PriceTicks = int64(97 + r.next()%7)
				}
			}
			remember(id, owner)
			cmds = append(cmds, o)
		case 5, 6:
			tid, towner := target()
			cmds = append(cmds, Cancel{Timestamp: ts, OrderID: tid, OwnerID: towner})
		case 7, 8:
			tid, towner := target()
			cmds = append(cmds, Replace{Timestamp: ts, OrderID: tid, OwnerID: towner,
				NewPriceTicks: int64(97 + r.next()%7), NewQtyLots: int64(1 + r.next()%11)})
		case 9:
			switch r.next() % 4 {
			case 0:
				if len(ids) > 0 {
					id = ids[int(r.next())%len(ids)]
				}
			case 1:
				id = ""
			}
			o := NewOrder{Timestamp: ts, OrderID: id, OwnerID: owner, Side: Side(r.next() % 4),
				Type: OrderType(r.next() % 3), TIF: TimeInForce(r.next() % 5), STP: STPMode(r.next() % 4),
				PriceTicks: 97 + int64(int8(r.next())), QtyLots: int64(int8(r.next())),
				PostOnly: r.next()%5 == 0, ClientOrderID: "k" + strconv.Itoa(int(r.next()%4))}
			if rb := r.next(); rb%3 == 0 {
				o.ReduceOnly = true
				o.ReduceOnlyLimitLots = int64(int8(rb)) % 9
			}
			remember(id, owner)
			cmds = append(cmds, o)
		}
	}
	return cmds
}

// FuzzBook asserts, for any input: no panic, every invariant after every
// command, the event-derived model, replay determinism and snapshot/restore
// equivalence at a midpoint chosen by the input.
func FuzzBook(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0, 0, 0, 3, 5, 1, 1, 1, 6, 5, 9, 2, 0, 3, 0, 3})
	f.Add([]byte{4, 1, 0x05, 2, 7, 0, 0, 0, 1, 4, 0, 2, 0, 8, 0, 1, 3, 9, 9, 7, 1, 2, 2, 2, 2, 9, 9, 1, 2, 5, 5})
	f.Add([]byte("crossing, self-trades and replaces"))
	rng := splitmix64(2026)
	for i := 0; i < 4; i++ {
		seed := make([]byte, 64+64*i)
		for j := range seed {
			seed[j] = byte(rng.next())
		}
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		cmds := fuzzCommands(data)
		h := newHarness(t)
		model := newShadow()
		for _, cmd := range cmds {
			res := h.apply(cmd) // verify() after every command
			require.NoError(t, model.observe(cmd, res, h.b))
		}
		require.NoError(t, model.checkAll(h.b))
		requireLevelsFIFOAndAggregates(t, h.b)
		snapshot := h.b.Snapshot()
		requireReplayIdentical(t, h.j.Entries(), h.results, snapshot)
		k := 0
		if len(data) > 0 {
			k = int(data[len(data)-1]) % (len(cmds) + 1)
		}
		requireRestoreMidpoint(t, h.j.Entries(), h.results, snapshot, k)
	})
}
