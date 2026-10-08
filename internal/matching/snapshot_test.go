package matching

import (
	"crypto/sha256"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
)

// richBook builds a book holding every kind of record: resting orders on both
// sides, a partial fill, a cancelled order, a filled taker, a rejected order,
// a capped reduce-only order and a negative timestamp.
func richBook(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.apply(NewOrder{Timestamp: 10, OrderID: "a1", ClientOrderID: "c1", OwnerID: "alice", Side: SideSell,
		Type: OrderTypeLimit, TIF: TIFGTC, PriceTicks: 101, QtyLots: 5, STP: STPCancelBoth})
	h.apply(postOnly(lim("a2", "bob", SideSell, 102, 3)))
	h.apply(lim("b1", "carol", SideBuy, 99, 4))
	h.apply(ioc(lim("x1", "dave", SideBuy, 101, 2)))
	h.apply(Cancel{Timestamp: 20, OrderID: "b1", OwnerID: "carol"})
	h.apply(lim("bad", "dave", SideBuy, 0, 1))
	h.apply(reduceOnly(lim("r1", "erin", SideBuy, 98, 10), 4))
	h.apply(NewOrder{Timestamp: -5, OrderID: "n1", OwnerID: "frank", Side: SideBuy, Type: OrderTypeLimit,
		TIF: TIFGTC, PriceTicks: 97, QtyLots: 1})
	return h
}

func TestSnapshotRoundTripAndHash(t *testing.T) {
	h := richBook(t)
	snap := h.b.Snapshot()
	assert.Equal(t, snap, h.b.Snapshot(), "snapshots are deterministic")
	assert.Equal(t, sha256.Sum256(snap), h.b.StateHash())

	r, err := Restore(snap)
	require.NoError(t, err)
	assert.Equal(t, snap, r.Snapshot())
	assert.Equal(t, h.b.Config(), r.Config())
	assert.Equal(t, h.b.CommandSeq(), r.CommandSeq())
	assert.Equal(t, h.b.EventSeq(), r.EventSeq())
	assert.Equal(t, h.b.TradeSeq(), r.TradeSeq())
	for _, id := range []string{"a1", "a2", "b1", "x1", "bad", "r1", "n1"} {
		want, _ := h.b.Order(id)
		got, ok := r.Order(id)
		require.True(t, ok, id)
		assert.Equal(t, want, got, id)
	}

	next := []Command{
		lim("a1", "zed", SideBuy, 1, 1), // duplicate order id survives the snapshot
		NewOrder{OrderID: "z1", ClientOrderID: "c1", OwnerID: "alice", Side: SideBuy, Type: OrderTypeLimit, TIF: TIFGTC, PriceTicks: 90, QtyLots: 1},
		mkt("m1", "zed", SideBuy, 20),
		Replace{OrderID: "r1", OwnerID: "erin", NewPriceTicks: 99, NewQtyLots: 3},
		lim("s9", "zed", SideSell, 98, 2),
	}
	for _, cmd := range next {
		want := h.apply(cmd)
		got, err := r.Apply(cmd)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	reason, _ := h.results[len(h.results)-len(next)].Rejected()
	assert.Equal(t, RejectDuplicateOrderID, reason)
	reason, _ = h.results[len(h.results)-len(next)+1].Rejected()
	assert.Equal(t, RejectDuplicateClientOrderID, reason, "client order ids survive the snapshot")
	assert.Equal(t, h.b.Snapshot(), r.Snapshot())
}

// TestSnapshotMatchesDocumentedFormat encodes a small book by hand from the
// grammar documented in snapshot.go and requires byte equality, so the
// documentation and the encoder cannot drift apart.
func TestSnapshotMatchesDocumentedFormat(t *testing.T) {
	b, err := NewBook(Config{Instrument: "YES", MaxPriceTicks: 99})
	require.NoError(t, err)
	_, err = b.Apply(NewOrder{Timestamp: -3, OrderID: "o1", ClientOrderID: "k", OwnerID: "al", Side: SideBuy,
		Type: OrderTypeLimit, TIF: TIFGTC, PriceTicks: 40, QtyLots: 7, PostOnly: true, STP: STPCancelMaker})
	require.NoError(t, err)
	_, err = b.Apply(lim("o0", "al", SideBuy, 100, 1)) // rejected: above the book's ceiling
	require.NoError(t, err)

	uv := func(buf []byte, v uint64) []byte { return binary.AppendUvarint(buf, v) }
	str := func(buf []byte, s string) []byte { return append(uv(buf, uint64(len(s))), s...) }
	var want []byte
	want = append(want, "NCLOB"...)
	want = append(want, 1)
	want = str(want, "YES")
	want = uv(want, 99) // max price
	want = uv(want, 2)  // command seq
	want = uv(want, 4)  // event seq: ACCEPTED, RESTING, BOOK_DELTA, REJECTED
	want = uv(want, 0)  // trade seq
	want = uv(want, 2)  // orders, sorted by id: o0 then o1
	want = str(want, "o0")
	want = append(want, byte(StatusRejected))
	want = str(want, "INVALID_PRICE")
	want = str(want, "o1")
	want = append(want, byte(StatusOpen))
	want = str(want, "al")
	want = str(want, "k")
	want = append(want, byte(SideBuy), byte(OrderTypeLimit), byte(TIFGTC), byte(STPCancelMaker))
	flagsAt := len(want)
	want = append(want, flagPostOnly)
	want = uv(want, 40) // price
	want = uv(want, 0)  // filled
	want = uv(want, 7)  // remaining
	want = uv(want, 0)  // cancelled
	want = uv(want, 0)  // reduce-only limit
	want = uv(want, 2)  // priority: the RESTING event's seq
	want = binary.AppendVarint(want, -3)
	want = str(want, "")
	assert.Equal(t, want, b.Snapshot())

	_, err = Restore(want)
	require.NoError(t, err)
	want[flagsAt] = 1 << 2
	_, err = Restore(want)
	require.Error(t, err, "unknown flag bits")
}

func TestRestoreRejectsMalformedBytes(t *testing.T) {
	snap := richBook(t).b.Snapshot()
	requireInvalid := func(t *testing.T, data []byte, msg string) {
		t.Helper()
		b, err := Restore(data)
		require.Error(t, err, msg)
		assert.Nil(t, b)
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), msg)
	}
	for i := 0; i < len(snap); i++ {
		requireInvalid(t, snap[:i], "truncated snapshot")
	}
	requireInvalid(t, append(append([]byte{}, snap...), 0), "trailing byte")
	bad := append([]byte{}, snap...)
	bad[0] = 'X'
	requireInvalid(t, bad, "magic")
	bad = append([]byte{}, snap...)
	bad[5] = 2
	requireInvalid(t, bad, "version")

	empty := newTestBook(t).Snapshot()
	require.Equal(t, byte(0), empty[len(empty)-1], "an empty book ends with a zero order count")
	nonCanonical := append(append([]byte{}, empty[:len(empty)-1]...), 0x80, 0x00)
	requireInvalid(t, nonCanonical, "overlong varint")
	_, err := Restore(empty)
	require.NoError(t, err)
}

func TestRestoreRejectsInconsistentStates(t *testing.T) {
	cases := map[string]func(b *Book){
		"crossed book":                 func(b *Book) { b.orders["r1"].price = 105 },
		"fills on an OPEN order":       func(b *Book) { b.orders["a2"].filled = 1 },
		"duplicate client order id":    func(b *Book) { b.orders["a2"].owner, b.orders["a2"].clientID = "alice", "c1" },
		"duplicate priority":           func(b *Book) { b.orders["a2"].priority = b.orders["r1"].priority },
		"priority after last event":    func(b *Book) { b.orders["a2"].priority = b.eventSeq + 1 },
		"priority on a closed order":   func(b *Book) { b.orders["b1"].priority = 3 },
		"IOC order resting":            func(b *Book) { b.orders["a1"].tif = TIFIOC },
		"market GTC":                   func(b *Book) { b.orders["a2"].typ = OrderTypeMarket },
		"unconsumable reject reason":   func(b *Book) { b.orders["bad"].rejectReason = RejectDuplicateOrderID },
		"unknown reject reason":        func(b *Book) { b.orders["bad"].rejectReason = "NOPE" },
		"unknown status":               func(b *Book) { b.orders["a2"].status = 9 },
		"reduce-only above its limit":  func(b *Book) { b.orders["r1"].reduceOnlyLimit = 1 },
		"unknown cancel reason":        func(b *Book) { b.orders["b1"].cancelReason = "NOPE" },
		"cap as a closing reason":      func(b *Book) { b.orders["b1"].cancelReason = CancelReduceOnlyCap },
		"notional above the bound":     func(b *Book) { b.orders["a2"].price, b.orders["a2"].remaining = MaxPriceTicks, 2000 },
		"unknown side":                 func(b *Book) { b.orders["a2"].side = 3 },
		"unknown stp":                  func(b *Book) { b.orders["a2"].stp = 7 },
		"partial cancel without cap":   func(b *Book) { b.orders["a2"].cancelled = 1 },
		"zero price ceiling":           func(b *Book) { b.maxPrice = 0 },
		"price above the ceiling":      func(b *Book) { b.maxPrice = 100 },
		"fewer events than commands":   func(b *Book) { b.eventSeq = b.cmdSeq - 1 },
		"more trades than events":      func(b *Book) { b.tradeSeq = b.eventSeq + 1 },
		"empty owner":                  func(b *Book) { b.orders["a2"].owner = "" },
		"filled status with remaining": func(b *Book) { b.orders["a2"].status = StatusFilled },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h := richBook(t)
			mutate(h.b)
			_, err := Restore(h.b.Snapshot())
			require.Error(t, err)
			t.Log(err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}
}

// FuzzRestore feeds arbitrary bytes to Restore: it must never panic, and
// whatever it accepts must be a valid book that re-encodes to the same bytes.
func FuzzRestore(f *testing.F) {
	h := newHarness(f)
	for _, cmd := range []Command{
		lim("a", "x", SideSell, 101, 5), lim("b", "y", SideBuy, 99, 5), ioc(lim("c", "y", SideBuy, 101, 2)),
		lim("d", "z", SideBuy, 0, 1), reduceOnly(lim("e", "z", SideBuy, 98, 9), 3), Cancel{OrderID: "b", OwnerID: "y"},
	} {
		h.apply(cmd)
	}
	f.Add(h.b.Snapshot())
	empty, err := NewBook(testCfg)
	require.NoError(f, err)
	f.Add(empty.Snapshot())
	f.Add([]byte("NCLOB"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		b, err := Restore(data)
		if err != nil {
			require.Nil(t, b)
			require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
			return
		}
		require.NoError(t, b.verify())
		require.Equal(t, data, b.Snapshot())
	})
}
