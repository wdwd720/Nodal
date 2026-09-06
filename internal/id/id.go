package id

import (
	"bytes"
	"crypto/rand"
	"database/sql/driver"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// Kind is the constraint of the phantom type parameter of ID. It is an alias
// of any: the parameter carries no runtime data and exists only to make IDs of
// different aggregates distinct types.
//
// Declare one unexported empty struct per aggregate in the package that owns
// the aggregate, and export an alias plus a constructor:
//
//	type orderKind struct{}
//	type OrderID = id.ID[orderKind]
//	func NewOrderID() OrderID { return id.New[orderKind]() }
//
// Keeping the kind unexported means only the owning package can mint the
// type, while the alias lets every other package name it.
type Kind = any

// Any is the kind for genuinely untyped contexts: generic infrastructure such
// as outbox/inbox envelopes, audit rows and tracing, where the aggregate type
// is carried separately as data. It must never be used as a domain identity;
// use Untyped to erase a typed ID and Bytes16 to restore one.
type Any struct{}

// ID is a UUID version 7 identifier typed by the phantom kind K.
//
// The zero value is the "absent" identifier: IsZero reports true, String
// renders the Nil UUID, and Value maps it to SQL NULL. IDs are comparable
// with == and ordered by Compare.
type ID[K Kind] struct {
	b [16]byte
}

var (
	// ErrFormat is returned by Parse and friends when the input is not the
	// canonical 36-character xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx form.
	ErrFormat = errors.New("id: not in canonical 36-character uuid form")
	// ErrVersion is returned when a well-formed UUID is not an RFC 9562
	// version 7 UUID with the RFC variant (and is not the Nil UUID).
	ErrVersion = errors.New("id: not an rfc 9562 version 7 uuid")
)

// maxUnixMilli is the largest timestamp representable in the 48-bit
// unix_ts_ms field (year 10889).
const maxUnixMilli = 1<<48 - 1

// New returns a fresh UUIDv7 from the wall clock and crypto/rand.
//
// IDs created by New within one process are strictly increasing in byte
// order even when created in the same millisecond: the 12-bit rand_a field
// carries a sub-millisecond sequence and the timestamp is bumped when that
// sequence would otherwise repeat.
//
// New panics only if the platform CSPRNG is unavailable, which crypto/rand
// itself treats as an unrecoverable condition.
func New[K Kind]() ID[K] {
	u, err := uuid.NewV7()
	if err != nil {
		panic(fmt.Sprintf("id: generating uuidv7: %v", err))
	}
	return ID[K]{b: [16]byte(u)}
}

// NewAt returns a UUIDv7 whose timestamp field is t truncated to
// milliseconds, with all remaining bits drawn from crypto/rand.
//
// It is intended for deterministic tests and for back-filling records with a
// known creation time. Unlike New it does not participate in the process-wide
// monotonic sequence: two IDs built with the same millisecond are ordered
// randomly relative to each other.
//
// NewAt panics if t is before the Unix epoch or beyond the 48-bit millisecond
// range, because such a time cannot be encoded and indicates a programming
// error rather than external input.
func NewAt[K Kind](t time.Time) ID[K] {
	ms := t.UnixMilli()
	if t.IsZero() || ms < 0 || ms > maxUnixMilli {
		panic(fmt.Sprintf("id: NewAt: %s is outside the uuidv7 timestamp range", t.UTC().Format(time.RFC3339Nano)))
	}
	var b [16]byte
	if _, err := rand.Read(b[6:]); err != nil {
		panic(fmt.Sprintf("id: crypto/rand: %v", err))
	}
	// ms is non-negative and below 2^48 (checked above), so the low six
	// bytes of its big-endian encoding are exactly the 48-bit timestamp.
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(ms))
	copy(b[:6], ts[2:])
	b[6] = 0x70 | (b[6] & 0x0f) // version 7
	b[8] = 0x80 | (b[8] & 0x3f) // RFC variant (10xx)
	return ID[K]{b: b}
}

// Parse decodes the canonical 36-character form (hex digits in either case,
// hyphens at positions 8, 13, 18 and 23). It rejects braces, urn:uuid:
// prefixes, 32-character hex, surrounding whitespace and any UUID version
// other than 7. The Nil UUID decodes to the zero ID without error.
//
// Errors wrap ErrFormat or ErrVersion and never echo the input.
func Parse[K Kind](s string) (ID[K], error) {
	b, err := parseCanonical(s)
	if err != nil {
		return ID[K]{}, err
	}
	if err := checkV7(b); err != nil {
		return ID[K]{}, err
	}
	return ID[K]{b: b}, nil
}

// MustParse is Parse for compile-time constants and test fixtures. It panics
// on invalid input and must not be used on external data.
func MustParse[K Kind](s string) ID[K] {
	i, err := Parse[K](s)
	if err != nil {
		panic(fmt.Sprintf("id: MustParse(%q): %v", s, err))
	}
	return i
}

// ParseAny decodes the canonical 36-character form of a UUID of any version
// or variant, for external references (provider identifiers that happen to be
// UUIDs). The result is deliberately untyped; a domain-typed ID must always be
// a version 7 UUID and is obtained with Parse or Bytes16.
func ParseAny(s string) (ID[Any], error) {
	b, err := parseCanonical(s)
	if err != nil {
		return ID[Any]{}, err
	}
	return ID[Any]{b: b}, nil
}

// Bytes16 builds an ID from its raw 16-byte form, as stored in a binary uuid
// column or produced by Bytes. It applies the same version check as Parse.
func Bytes16[K Kind](b [16]byte) (ID[K], error) {
	if err := checkV7(b); err != nil {
		return ID[K]{}, err
	}
	return ID[K]{b: b}, nil
}

// Compare returns -1, 0 or +1 according to the byte order of a and b, which
// for version 7 UUIDs is creation order (millisecond precision, then the
// monotonic sequence for IDs minted by New in the same process).
func Compare[K Kind](a, b ID[K]) int {
	return bytes.Compare(a.b[:], b.b[:])
}

// String returns the canonical lowercase 36-character form. The zero ID
// renders as the Nil UUID 00000000-0000-0000-0000-000000000000.
func (i ID[K]) String() string {
	var buf [36]byte
	i.encode(&buf)
	return string(buf[:])
}

// IsZero reports whether i is the zero (absent) identifier. It also lets the
// encoding/json `omitzero` option drop unset IDs.
func (i ID[K]) IsZero() bool {
	return i.b == [16]byte{}
}

// Time returns the creation time encoded in the UUID, in UTC with millisecond
// precision. The zero ID returns the zero time.Time.
func (i ID[K]) Time() time.Time {
	if i.IsZero() {
		return time.Time{}
	}
	ms := int64(i.b[0])<<40 | int64(i.b[1])<<32 | int64(i.b[2])<<24 |
		int64(i.b[3])<<16 | int64(i.b[4])<<8 | int64(i.b[5])
	return time.UnixMilli(ms).UTC()
}

// Bytes returns the raw 16-byte big-endian form.
func (i ID[K]) Bytes() [16]byte {
	return i.b
}

// Untyped erases the kind for use in generic infrastructure (see Any).
func (i ID[K]) Untyped() ID[Any] {
	return ID[Any](i)
}

// LogValue renders the ID as a string attribute for log/slog.
func (i ID[K]) LogValue() slog.Value {
	return slog.StringValue(i.String())
}

// MarshalText implements encoding.TextMarshaler with the canonical form.
func (i ID[K]) MarshalText() ([]byte, error) {
	var buf [36]byte
	i.encode(&buf)
	return buf[:], nil
}

// UnmarshalText implements encoding.TextUnmarshaler with Parse semantics.
// On error the receiver is left unchanged.
func (i *ID[K]) UnmarshalText(text []byte) error {
	p, err := Parse[K](string(text))
	if err != nil {
		return err
	}
	*i = p
	return nil
}

// MarshalJSON encodes the ID as a JSON string in canonical form.
func (i ID[K]) MarshalJSON() ([]byte, error) {
	var buf [38]byte
	buf[0] = '"'
	i.encode((*[36]byte)(buf[1:37]))
	buf[37] = '"'
	return buf[:], nil
}

// UnmarshalJSON decodes a JSON string with Parse semantics. Following the
// encoding/json convention a JSON null leaves the receiver unchanged. Any
// other non-string token is an ErrFormat error.
func (i *ID[K]) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("%w: expected a json string", ErrFormat)
	}
	return i.UnmarshalText([]byte(s))
}

// Value implements driver.Valuer. Non-zero IDs are written in canonical
// string form, which Postgres casts to uuid. The zero ID is written as SQL
// NULL so that an unset identifier fails a NOT NULL constraint instead of
// silently persisting the Nil UUID, and maps naturally onto nullable
// reference columns.
func (i ID[K]) Value() (driver.Value, error) {
	if i.IsZero() {
		return nil, nil
	}
	return i.String(), nil
}

// Scan implements sql.Scanner. It accepts NULL (zero ID), a string or a
// 36-byte []byte in canonical form, a raw 16-byte []byte, a [16]byte or a
// uuid.UUID. The version check of Parse applies; on error the receiver is
// left unchanged.
func (i *ID[K]) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*i = ID[K]{}
		return nil
	case string:
		return i.scanText(v)
	case []byte:
		if len(v) == 16 {
			return i.scanRaw([16]byte(v))
		}
		return i.scanText(string(v))
	case [16]byte:
		return i.scanRaw(v)
	case uuid.UUID:
		return i.scanRaw([16]byte(v))
	default:
		return fmt.Errorf("id: cannot scan %T into ID", src)
	}
}

func (i *ID[K]) scanText(s string) error {
	p, err := Parse[K](s)
	if err != nil {
		return fmt.Errorf("id: scan: %w", err)
	}
	*i = p
	return nil
}

func (i *ID[K]) scanRaw(b [16]byte) error {
	p, err := Bytes16[K](b)
	if err != nil {
		return fmt.Errorf("id: scan: %w", err)
	}
	*i = p
	return nil
}

// encode writes the canonical lowercase form into buf.
func (i ID[K]) encode(buf *[36]byte) {
	hex.Encode(buf[0:8], i.b[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], i.b[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], i.b[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], i.b[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], i.b[10:16])
}

// hexOffsets are the positions of the 16 hex byte pairs in the canonical
// form; the gaps are the four hyphens.
var hexOffsets = [16]int{0, 2, 4, 6, 9, 11, 14, 16, 19, 21, 24, 26, 28, 30, 32, 34}

// parseCanonical decodes exactly the xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
// form. It never panics and never includes the input in its error.
func parseCanonical(s string) ([16]byte, error) {
	var b [16]byte
	if len(s) != 36 {
		return b, fmt.Errorf("%w: length %d", ErrFormat, len(s))
	}
	if s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return b, fmt.Errorf("%w: hyphens must be at positions 8, 13, 18 and 23", ErrFormat)
	}
	for i, off := range hexOffsets {
		hi, ok1 := unhex(s[off])
		lo, ok2 := unhex(s[off+1])
		if !ok1 || !ok2 {
			return b, fmt.Errorf("%w: non-hex character at position %d", ErrFormat, off)
		}
		b[i] = hi<<4 | lo
	}
	return b, nil
}

// checkV7 accepts the Nil UUID (zero ID) or a version 7 UUID with the RFC
// variant, and rejects everything else with ErrVersion.
func checkV7(b [16]byte) error {
	if b == [16]byte{} {
		return nil
	}
	if v := b[6] >> 4; v != 7 {
		return fmt.Errorf("%w: version %d", ErrVersion, v)
	}
	if b[8]&0xc0 != 0x80 {
		return fmt.Errorf("%w: variant bits %02b", ErrVersion, b[8]>>6)
	}
	return nil
}

func unhex(c byte) (byte, bool) {
	switch {
	case '0' <= c && c <= '9':
		return c - '0', true
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10, true
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}
