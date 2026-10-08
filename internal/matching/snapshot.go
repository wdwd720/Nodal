package matching

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"slices"

	"github.com/nodal/controlplane/internal/errs"
)

// Snapshot format, version 1.
//
// A snapshot is the complete state of a book -- configuration, the three
// sequence counters, and the record of every OrderID ever consumed, which
// includes the resting orders and both dedupe sets -- in one canonical
// binary encoding: equal states encode to equal bytes, and Restore accepts
// only bytes that Snapshot itself would produce.
//
//	snapshot := "NCLOB" version:u8(=1) instrument:str maxPriceTicks:uv
//	            commandSeq:uv eventSeq:uv tradeSeq:uv count:uv order{count}
//	order    := id:str status:u8 ( reason:str                  if REJECTED
//	                             | accepted                    otherwise )
//	accepted := owner:str clientOrderID:str side:u8 type:u8 tif:u8 stp:u8
//	            flags:u8 priceTicks:uv filledLots:uv remainingLots:uv
//	            cancelledLots:uv reduceOnlyLimitLots:uv priority:uv
//	            acceptedTimestamp:sv cancelReason:str
//	str      := length:uv bytes
//
// uv is an unsigned LEB128 varint (encoding/binary Uvarint) in its shortest
// form, sv a zig-zag signed varint (Varint). Orders are sorted by id,
// bytewise ascending and unique. Enum bytes are the constant values of this
// package; flags bit 0 is PostOnly and bit 1 ReduceOnly. An order's quantity
// is not stored: it is filled + remaining + cancelled. Price levels are not
// stored either: they are the open orders grouped by side and price and
// queued by priority, which is how Restore rebuilds them. The ClientOrderID
// dedupe set is the (owner, client order id) of every accepted order, and the
// OrderID dedupe set is every id present.
const (
	snapshotMagic   = "NCLOB"
	snapshotVersion = 1
	maxReasonBytes  = 64
	maxInt64        = 1<<63 - 1
)

// Snapshot returns the canonical encoding of the book's full state.
func (b *Book) Snapshot() []byte {
	ids := make([]string, 0, len(b.orders))
	for id := range b.orders {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	buf := make([]byte, 0, 64+len(b.instrument)+48*len(ids))
	buf = append(buf, snapshotMagic...)
	buf = append(buf, snapshotVersion)
	buf = appendStr(buf, b.instrument)
	buf = binary.AppendUvarint(buf, uint64(b.maxPrice))
	buf = binary.AppendUvarint(buf, uint64(b.cmdSeq))
	buf = binary.AppendUvarint(buf, uint64(b.eventSeq))
	buf = binary.AppendUvarint(buf, uint64(b.tradeSeq))
	buf = binary.AppendUvarint(buf, uint64(len(ids)))
	for _, id := range ids {
		buf = appendOrder(buf, b.orders[id])
	}
	return buf
}

func appendStr(buf []byte, s string) []byte {
	buf = binary.AppendUvarint(buf, uint64(len(s)))
	return append(buf, s...)
}

func appendOrder(buf []byte, o *order) []byte {
	buf = appendStr(buf, o.id)
	buf = append(buf, byte(o.status))
	if o.status == StatusRejected {
		return appendStr(buf, string(o.rejectReason))
	}
	buf = appendStr(buf, o.owner)
	buf = appendStr(buf, o.clientID)
	buf = append(buf, byte(o.side), byte(o.typ), byte(o.tif), byte(o.stp), o.flags())
	buf = binary.AppendUvarint(buf, uint64(o.price))
	buf = binary.AppendUvarint(buf, uint64(o.filled))
	buf = binary.AppendUvarint(buf, uint64(o.remaining))
	buf = binary.AppendUvarint(buf, uint64(o.cancelled))
	buf = binary.AppendUvarint(buf, uint64(o.reduceOnlyLimit))
	buf = binary.AppendUvarint(buf, uint64(o.priority))
	buf = binary.AppendVarint(buf, o.acceptedTS)
	return appendStr(buf, string(o.cancelReason))
}

// Restore rebuilds a book from a snapshot. It never panics: it rejects with
// VALIDATION_FAILED anything that is truncated, has trailing bytes, is not
// in canonical form, or describes a state that violates any book invariant
// (a crossed book, a quantity that is not conserved, an impossible status,
// a duplicate client order id, and so on). Applying the journal entries
// after the snapshot's CommandSeq to the restored book reproduces exactly
// what applying the whole journal to a new book produces.
func Restore(snapshot []byte) (*Book, error) {
	d := decoder{buf: snapshot}
	if len(snapshot) < len(snapshotMagic)+1 || string(snapshot[:len(snapshotMagic)]) != snapshotMagic {
		return nil, malformed("missing magic", 0)
	}
	d.pos = len(snapshotMagic)
	if v := d.u8(); v != snapshotVersion {
		return nil, malformed("unsupported version", len(snapshotMagic))
	}
	instrument := d.str(MaxIDBytes)
	maxPrice := d.uv(MaxPriceTicks)
	cmdSeq := d.uv(maxInt64)
	eventSeq := d.uv(maxInt64)
	tradeSeq := d.uv(maxInt64)
	count := d.uv(maxInt64)
	if d.err != nil {
		return nil, d.err
	}
	if !validID(instrument) || maxPrice < 1 {
		return nil, malformed("invalid configuration", d.pos)
	}
	// Every order record takes at least three bytes.
	if count > int64(len(snapshot)-d.pos)/3 {
		return nil, malformed("order count exceeds input", d.pos)
	}
	b := newBook(instrument, maxPrice)
	b.cmdSeq, b.eventSeq, b.tradeSeq = cmdSeq, eventSeq, tradeSeq

	var open []*order
	prevID := ""
	for i := int64(0); i < count; i++ {
		start := d.pos
		o := d.order(eventSeq)
		if d.err != nil {
			return nil, d.err
		}
		if i > 0 && o.id <= prevID {
			return nil, malformed("orders not sorted by id or not unique", start)
		}
		prevID = o.id
		if msg := checkRecord(o, maxPrice); msg != "" {
			return nil, malformed("order "+o.id+": "+msg, start)
		}
		b.orders[o.id] = o
		if o.status == StatusRejected {
			continue
		}
		if o.clientID != "" {
			key := clientKey{o.owner, o.clientID}
			if _, dup := b.clientIDs[key]; dup {
				return nil, malformed("duplicate client order id", start)
			}
			b.clientIDs[key] = struct{}{}
		}
		if o.status.IsOpen() {
			open = append(open, o)
		}
	}
	if d.pos != len(snapshot) {
		return nil, malformed("trailing bytes", d.pos)
	}

	slices.SortFunc(open, func(x, y *order) int { return cmp.Compare(x.priority, y.priority) })
	for _, o := range open {
		l := b.side(o.side)
		if o.remaining > MaxRestingLotsPerSide-l.total {
			return nil, malformed("resting quantity exceeds capacity", len(snapshot))
		}
		lvl := l.levelAt(o.price)
		lvl.push(o)
		lvl.total += o.remaining
		l.total += o.remaining
		owned := b.byOwner[o.owner]
		if owned == nil {
			owned = make(map[string]*order)
			b.byOwner[o.owner] = owned
		}
		owned[o.id] = o
	}
	if err := b.verify(); err != nil {
		return nil, errs.Wrap(err, errs.CodeValidationFailed, "matching: snapshot violates a book invariant")
	}
	if !bytes.Equal(b.Snapshot(), snapshot) {
		return nil, errs.New(errs.CodeValidationFailed, "matching: snapshot is not in canonical form")
	}
	return b, nil
}

// decoder reads the snapshot grammar. After the first error every read
// returns a zero value and the error sticks.
type decoder struct {
	buf []byte
	pos int
	err error
}

func malformed(what string, offset int) error {
	return errs.New(errs.CodeValidationFailed, "matching: malformed snapshot: "+what).WithField("offset", offset)
}

func (d *decoder) fail(what string) {
	if d.err == nil {
		d.err = malformed(what, d.pos)
	}
}

func (d *decoder) u8() byte {
	if d.err != nil {
		return 0
	}
	if d.pos >= len(d.buf) {
		d.fail("truncated")
		return 0
	}
	v := d.buf[d.pos]
	d.pos++
	return v
}

// uv reads an unsigned varint no greater than limit.
func (d *decoder) uv(limit int64) int64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Uvarint(d.buf[d.pos:])
	if n <= 0 {
		d.fail("bad varint")
		return 0
	}
	if v > uint64(limit) {
		d.fail("value out of range")
		return 0
	}
	d.pos += n
	return int64(v)
}

func (d *decoder) sv() int64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Varint(d.buf[d.pos:])
	if n <= 0 {
		d.fail("bad varint")
		return 0
	}
	d.pos += n
	return v
}

func (d *decoder) str(limit int) string {
	n := d.uv(int64(limit))
	if d.err != nil {
		return ""
	}
	if n > int64(len(d.buf)-d.pos) {
		d.fail("truncated string")
		return ""
	}
	s := string(d.buf[d.pos : d.pos+int(n)])
	d.pos += int(n)
	return s
}

// order decodes one order record. Semantic checks are checkRecord's.
func (d *decoder) order(eventSeq int64) *order {
	o := &order{id: d.str(MaxIDBytes), status: OrderStatus(d.u8())}
	if o.status == StatusRejected {
		o.rejectReason = RejectReason(d.str(maxReasonBytes))
		return o
	}
	o.owner = d.str(MaxIDBytes)
	o.clientID = d.str(MaxIDBytes)
	o.side = Side(d.u8())
	o.typ = OrderType(d.u8())
	o.tif = TimeInForce(d.u8())
	o.stp = STPMode(d.u8())
	flags := d.u8()
	if flags&^(flagPostOnly|flagReduceOnly) != 0 {
		d.fail("unknown flags")
	}
	o.postOnly = flags&flagPostOnly != 0
	o.reduceOnly = flags&flagReduceOnly != 0
	o.price = d.uv(MaxPriceTicks)
	o.filled = d.uv(MaxQtyLots)
	o.remaining = d.uv(MaxQtyLots)
	o.cancelled = d.uv(MaxQtyLots)
	o.reduceOnlyLimit = d.uv(MaxQtyLots)
	o.priority = d.uv(eventSeq)
	o.acceptedTS = d.sv()
	o.cancelReason = CancelReason(d.str(maxReasonBytes))
	o.qty = o.filled + o.remaining + o.cancelled
	return o
}
