package devnet

import (
	"encoding/binary"
)

// Wire limits and versions.
const (
	// WireVersion is the only transaction and message version this package
	// encodes or accepts.
	WireVersion = 1
	// MaxTransactionSize caps an encoded transaction, signatures included.
	// Decode rejects anything longer before reading it.
	MaxTransactionSize = 4096
	// MaxInstructions caps the instructions in one message.
	MaxInstructions = 16
	// MaxSignatures caps the signature list on the wire. The semantic limit
	// is the number of required signers, at most 1 + MaxInstructions.
	MaxSignatures = 32
	// MaxMemoSize caps a MEMO's payload; an empty memo is rejected.
	MaxMemoSize = 256
)

const (
	txMagic  = "ndvt"
	msgMagic = "ndvm"
	// txOverhead is the framing a transaction adds around its message:
	// magic, version, u16 message length and u8 signature count.
	txOverhead = len(txMagic) + 1 + 2 + 1
	// maxMessageSize is the largest message that still fits a transaction.
	maxMessageSize = MaxTransactionSize - txOverhead
)

// writer appends the canonical encoding of fixed-width values.
type writer struct{ buf []byte }

func (w *writer) u8(v uint8)            { w.buf = append(w.buf, v) }
func (w *writer) u16(v uint16)          { w.buf = binary.LittleEndian.AppendUint16(w.buf, v) }
func (w *writer) u64(v uint64)          { w.buf = binary.LittleEndian.AppendUint64(w.buf, v) }
func (w *writer) raw(b []byte)          { w.buf = append(w.buf, b...) }
func (w *writer) str(s string)          { w.buf = append(w.buf, s...) }
func (w *writer) key(k PublicKey)       { w.buf = append(w.buf, k[:]...) }
func (w *writer) hash(h Hash)           { w.buf = append(w.buf, h[:]...) }
func (w *writer) signature(s Signature) { w.buf = append(w.buf, s[:]...) }

// chainID writes a validated chain id as u8 length || bytes.
func (w *writer) chainID(id ChainID) error {
	if err := id.Validate(); err != nil {
		return err
	}
	w.u8(uint8(len(id))) // #nosec G115 -- Validate bounds len(id) to MaxChainIDLen (32)
	w.str(string(id))
	return nil
}

// count writes n, which the caller has bounded to [0, 255], as a u8.
func (w *writer) count(n int) {
	w.u8(uint8(n)) // #nosec G115 -- every caller bounds n to MaxInstructions or MaxSignatures, both below 256
}

// length writes a len() result as a u64. Hash preimages only.
func (w *writer) length(n int) {
	w.u64(uint64(n)) // #nosec G115 -- n is a len() result, never negative
}

// label writes s as u64 length || bytes, which is unambiguous for any
// length. Hash preimages only; the wire format uses chainID.
func (w *writer) label(s string) {
	w.length(len(s))
	w.str(s)
}

// reader consumes a canonical encoding. The first failure is sticky: every
// later read returns zero values, and finish reports the first problem.
type reader struct {
	buf []byte
	off int
	err error
}

// fail records the first structural problem.
func (r *reader) fail(detail string) {
	if r.err == nil {
		r.err = malformed(detail)
	}
}

// take returns the next n bytes, aliasing the input, or false (with the
// failure recorded) when fewer than n remain.
func (r *reader) take(n int, what string) ([]byte, bool) {
	if r.err != nil {
		return nil, false
	}
	if n < 0 || len(r.buf)-r.off < n {
		r.fail("truncated " + what)
		return nil, false
	}
	b := r.buf[r.off : r.off+n]
	r.off += n
	return b, true
}

func (r *reader) u8(what string) uint8 {
	b, ok := r.take(1, what)
	if !ok {
		return 0
	}
	return b[0]
}

func (r *reader) u16(what string) uint16 {
	b, ok := r.take(2, what)
	if !ok {
		return 0
	}
	return binary.LittleEndian.Uint16(b)
}

func (r *reader) u64(what string) uint64 {
	b, ok := r.take(8, what)
	if !ok {
		return 0
	}
	return binary.LittleEndian.Uint64(b)
}

func (r *reader) key(what string) PublicKey {
	var k PublicKey
	if b, ok := r.take(PublicKeySize, what); ok {
		copy(k[:], b)
	}
	return k
}

func (r *reader) hash(what string) Hash {
	var h Hash
	if b, ok := r.take(HashSize, what); ok {
		copy(h[:], b)
	}
	return h
}

func (r *reader) signature(what string) Signature {
	var s Signature
	if b, ok := r.take(SignatureSize, what); ok {
		copy(s[:], b)
	}
	return s
}

// magic consumes and checks a fixed prefix.
func (r *reader) magic(want, what string) {
	b, ok := r.take(len(want), what)
	if ok && string(b) != want {
		r.fail("unknown " + what)
	}
}

// chainID reads a u8-length-prefixed chain id and validates it.
func (r *reader) chainID(what string) ChainID {
	n := int(r.u8(what))
	b, ok := r.take(n, what)
	if !ok {
		return ""
	}
	id := ChainID(b) // the conversion copies, so the id never aliases the input
	if id.Validate() != nil {
		r.fail("invalid " + what)
		return ""
	}
	return id
}

// finish reports the first failure, or trailing bytes after a complete value.
func (r *reader) finish(what string) error {
	if r.err == nil && r.off != len(r.buf) {
		r.fail("trailing bytes after " + what)
	}
	return r.err
}
