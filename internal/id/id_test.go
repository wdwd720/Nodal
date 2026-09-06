package id_test

import (
	"bytes"
	"database/sql"
	"database/sql/driver"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/id"
)

// Domain kind pattern as documented on id.Kind.
type (
	orderKind   struct{}
	accountKind struct{}
)

type (
	OrderID   = id.ID[orderKind]
	AccountID = id.ID[accountKind]
)

// Compile-time interface assertions.
var (
	_ fmt.Stringer             = OrderID{}
	_ encoding.TextMarshaler   = OrderID{}
	_ encoding.TextUnmarshaler = (*OrderID)(nil)
	_ json.Marshaler           = OrderID{}
	_ json.Unmarshaler         = (*OrderID)(nil)
	_ driver.Valuer            = OrderID{}
	_ sql.Scanner              = (*OrderID)(nil)
	_ slog.LogValuer           = OrderID{}
)

const (
	// A fixed v7 UUID: ts 0x018f_1234_5678 ms, rand_a 0xabc, variant 10.
	fixedV7 = "018f1234-5678-7abc-9def-0123456789ab"
	nilUUID = "00000000-0000-0000-0000-000000000000"
)

var fixedBytes = [16]byte{
	0x01, 0x8f, 0x12, 0x34, 0x56, 0x78,
	0x7a, 0xbc,
	0x9d, 0xef,
	0x01, 0x23, 0x45, 0x67, 0x89, 0xab,
}

func TestString_Format(t *testing.T) {
	t.Parallel()
	i, err := id.Bytes16[orderKind](fixedBytes)
	require.NoError(t, err)
	require.Equal(t, fixedV7, i.String())
	require.Equal(t, fixedBytes, i.Bytes())
	require.Len(t, i.String(), 36)
	require.Equal(t, strings.ToLower(i.String()), i.String(), "canonical form is lowercase")
	require.Equal(t, nilUUID, OrderID{}.String())
}

func TestParse_AcceptsCanonical(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"lowercase": fixedV7,
		"uppercase": strings.ToUpper(fixedV7),
		"mixed":     "018F1234-5678-7abc-9DEF-0123456789ab",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := id.Parse[orderKind](in)
			require.NoError(t, err)
			require.Equal(t, fixedBytes, got.Bytes())
			require.Equal(t, fixedV7, got.String(), "String normalises to lowercase")
		})
	}
}

func TestParse_NilUUIDIsZero(t *testing.T) {
	t.Parallel()
	got, err := id.Parse[orderKind](nilUUID)
	require.NoError(t, err)
	require.True(t, got.IsZero())
	require.Equal(t, OrderID{}, got)
	require.True(t, got.Time().IsZero())
}

func TestParse_Rejects(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want error
	}{
		{"empty", "", id.ErrFormat},
		{"braces", "{" + fixedV7 + "}", id.ErrFormat},
		{"urn", "urn:uuid:" + fixedV7, id.ErrFormat},
		{"hex32", strings.ReplaceAll(fixedV7, "-", ""), id.ErrFormat},
		{"too short", fixedV7[:35], id.ErrFormat},
		{"too long", fixedV7 + "a", id.ErrFormat},
		{"leading space", " " + fixedV7[1:], id.ErrFormat},
		{"trailing newline", fixedV7[:35] + "\n", id.ErrFormat},
		{"hyphen misplaced", "018f12345-678-7abc-9def-0123456789ab", id.ErrFormat},
		{"all hyphens", strings.Repeat("-", 36), id.ErrFormat},
		{"non-hex g", "018f1234-5678-7abc-9def-0123456789ag", id.ErrFormat},
		{"unicode digit", "018f1234-5678-7abc-9def-012345678٩ab", id.ErrFormat},
		{"multibyte 36 bytes", "018f1234-5678-7abc-9def-01234567éab", id.ErrFormat},
		{"version 4", "018f1234-5678-4abc-9def-0123456789ab", id.ErrVersion},
		{"version 1", "018f1234-5678-1abc-9def-0123456789ab", id.ErrVersion},
		{"version 6", "018f1234-5678-6abc-9def-0123456789ab", id.ErrVersion},
		{"version 8", "018f1234-5678-8abc-9def-0123456789ab", id.ErrVersion},
		{"max uuid", "ffffffff-ffff-ffff-ffff-ffffffffffff", id.ErrVersion},
		{"variant ncs", "018f1234-5678-7abc-7def-0123456789ab", id.ErrVersion},
		{"variant microsoft", "018f1234-5678-7abc-cdef-0123456789ab", id.ErrVersion},
		{"variant future", "018f1234-5678-7abc-edef-0123456789ab", id.ErrVersion},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := id.Parse[orderKind](tc.in)
			require.ErrorIs(t, err, tc.want)
			require.True(t, got.IsZero(), "failed parse yields zero ID")

			var u OrderID
			require.Error(t, u.UnmarshalText([]byte(tc.in)))
			require.True(t, u.IsZero())
		})
	}
}

func TestParse_ErrorDoesNotEchoInput(t *testing.T) {
	t.Parallel()
	in := "SECRETSECRET-SECRET-SECRET-SECRETSECRETSECRET"
	_, err := id.Parse[orderKind](in)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "SECRET")
}

func TestMustParse(t *testing.T) {
	t.Parallel()
	require.Equal(t, fixedBytes, id.MustParse[orderKind](fixedV7).Bytes())
	require.PanicsWithValue(t,
		`id: MustParse("018f1234-5678-4abc-9def-0123456789ab"): id: not an rfc 9562 version 7 uuid: version 4`,
		func() { id.MustParse[orderKind]("018f1234-5678-4abc-9def-0123456789ab") })
}

func TestParseAny(t *testing.T) {
	t.Parallel()
	v4 := "018f1234-5678-4abc-9def-0123456789ab"
	got, err := id.ParseAny(v4)
	require.NoError(t, err)
	require.Equal(t, v4, got.String())

	ms := "018f1234-5678-7abc-cdef-0123456789ab" // non-RFC variant
	_, err = id.ParseAny(ms)
	require.NoError(t, err)

	_, err = id.ParseAny("{" + v4 + "}")
	require.ErrorIs(t, err, id.ErrFormat)
	_, err = id.ParseAny(strings.ReplaceAll(v4, "-", ""))
	require.ErrorIs(t, err, id.ErrFormat)

	// An untyped external reference cannot be laundered into a domain ID
	// unless it really is a v7.
	_, err = id.Bytes16[orderKind](got.Bytes())
	require.ErrorIs(t, err, id.ErrVersion)
}

func TestBytes16(t *testing.T) {
	t.Parallel()
	got, err := id.Bytes16[orderKind](fixedBytes)
	require.NoError(t, err)
	require.Equal(t, fixedV7, got.String())

	zero, err := id.Bytes16[orderKind]([16]byte{})
	require.NoError(t, err)
	require.True(t, zero.IsZero())

	bad := fixedBytes
	bad[6] = 0x4a
	_, err = id.Bytes16[orderKind](bad)
	require.ErrorIs(t, err, id.ErrVersion)

	bad = fixedBytes
	bad[8] = 0x3d
	_, err = id.Bytes16[orderKind](bad)
	require.ErrorIs(t, err, id.ErrVersion)
}

func TestNew_IsVersion7(t *testing.T) {
	t.Parallel()
	before := time.Now().Truncate(time.Millisecond)
	i := id.New[orderKind]()
	after := time.Now()

	require.False(t, i.IsZero())
	b := i.Bytes()
	require.Equal(t, byte(7), b[6]>>4, "version nibble")
	require.Equal(t, byte(0x80), b[8]&0xc0, "RFC variant")

	// Cross-check with google/uuid.
	u := uuid.UUID(b)
	require.Equal(t, uuid.Version(7), u.Version())
	require.Equal(t, uuid.RFC4122, u.Variant())

	ts := i.Time()
	require.Equal(t, time.UTC, ts.Location())
	require.False(t, ts.Before(before), "timestamp %s before %s", ts, before)
	// google/uuid may advance the timestamp slightly to keep monotonicity
	// under bursts; allow a small slack.
	require.False(t, ts.After(after.Add(100*time.Millisecond)), "timestamp %s after %s", ts, after)

	back, err := id.Parse[orderKind](i.String())
	require.NoError(t, err)
	require.Equal(t, i, back)
}

func TestNew_MonotonicWithinProcess(t *testing.T) {
	t.Parallel()
	const n = 10_000
	prev := id.New[orderKind]()
	seen := make(map[OrderID]struct{}, n)
	seen[prev] = struct{}{}
	for k := 1; k < n; k++ {
		cur := id.New[orderKind]()
		cb, pb := cur.Bytes(), prev.Bytes()
		require.Equal(t, 1, bytes.Compare(cb[:], pb[:]), "id %d not strictly greater than its predecessor: %s <= %s", k, cur, prev)
		require.Equal(t, 1, id.Compare(cur, prev))
		require.Less(t, prev.String(), cur.String(), "string order must match byte order")
		_, dup := seen[cur]
		require.False(t, dup, "duplicate id %s", cur)
		seen[cur] = struct{}{}
		prev = cur
	}
}

func TestNew_ConcurrentUnique(t *testing.T) {
	t.Parallel()
	const workers, per = 16, 500
	var (
		mu  sync.Mutex
		all = make(map[OrderID]struct{}, workers*per)
		wg  sync.WaitGroup
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]OrderID, 0, per)
			for k := 0; k < per; k++ {
				local = append(local, id.New[orderKind]())
			}
			// Each goroutine's own sequence is strictly increasing.
			for k := 1; k < len(local); k++ {
				if id.Compare(local[k-1], local[k]) >= 0 {
					t.Errorf("goroutine sequence not increasing: %s >= %s", local[k-1], local[k])
				}
			}
			mu.Lock()
			defer mu.Unlock()
			for _, i := range local {
				all[i] = struct{}{}
			}
		}()
	}
	wg.Wait()
	require.Len(t, all, workers*per)
}

func TestNewAt(t *testing.T) {
	t.Parallel()
	loc := time.FixedZone("plus5", 5*3600)
	in := time.Date(2026, 9, 5, 14, 30, 45, 123_456_789, loc)

	i := id.NewAt[orderKind](in)
	require.False(t, i.IsZero())
	b := i.Bytes()
	require.Equal(t, byte(7), b[6]>>4)
	require.Equal(t, byte(0x80), b[8]&0xc0)

	got := i.Time()
	require.Equal(t, time.UTC, got.Location())
	require.True(t, got.Equal(in.Truncate(time.Millisecond)), "want %s got %s", in.Truncate(time.Millisecond), got)
	require.Equal(t, "2026-09-05T09:30:45.123Z", got.Format(time.RFC3339Nano))

	// Two IDs at the same instant differ (random tail) but share the prefix.
	j := id.NewAt[orderKind](in)
	require.NotEqual(t, i, j)
	require.Equal(t, i.String()[:13], j.String()[:13])

	// Epoch is representable.
	e := id.NewAt[orderKind](time.UnixMilli(0))
	require.False(t, e.IsZero())
	require.True(t, e.Time().Equal(time.UnixMilli(0)))
}

func TestNewAt_Ordering(t *testing.T) {
	t.Parallel()
	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Millisecond)
	a := id.NewAt[orderKind](t1)
	b := id.NewAt[orderKind](t2)
	require.Equal(t, -1, id.Compare(a, b))
	require.Less(t, a.String(), b.String())
}

func TestNewAt_PanicsOutOfRange(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() { id.NewAt[orderKind](time.Time{}) })
	require.Panics(t, func() { id.NewAt[orderKind](time.UnixMilli(-1)) })
	require.Panics(t, func() { id.NewAt[orderKind](time.UnixMilli(1 << 48)) })
	require.NotPanics(t, func() { id.NewAt[orderKind](time.UnixMilli(1<<48 - 1)) })
}

func TestTime_ExtractsMilliseconds(t *testing.T) {
	t.Parallel()
	i := id.MustParse[orderKind](fixedV7)
	require.Equal(t, time.UnixMilli(0x018f12345678).UTC(), i.Time())
	require.Equal(t, time.UTC, i.Time().Location())
}

func TestJSON_RoundTrip(t *testing.T) {
	t.Parallel()
	type doc struct {
		Order   OrderID   `json:"order"`
		Account AccountID `json:"account,omitzero"`
		Parent  *OrderID  `json:"parent,omitempty"`
	}
	order := id.MustParse[orderKind](fixedV7)
	acct := id.NewAt[accountKind](time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	in := doc{Order: order, Account: acct, Parent: &order}

	raw, err := json.Marshal(in)
	require.NoError(t, err)
	require.JSONEq(t, fmt.Sprintf(`{"order":%q,"account":%q,"parent":%q}`, fixedV7, acct.String(), fixedV7), string(raw))

	var out doc
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Equal(t, in.Order, out.Order)
	require.Equal(t, in.Account, out.Account)
	require.NotNil(t, out.Parent)
	require.Equal(t, order, *out.Parent)

	// Zero ID with omitzero is dropped; without it the Nil UUID is emitted.
	raw, err = json.Marshal(doc{Order: order})
	require.NoError(t, err)
	require.JSONEq(t, fmt.Sprintf(`{"order":%q}`, fixedV7), string(raw))

	raw, err = json.Marshal(OrderID{})
	require.NoError(t, err)
	require.Equal(t, `"`+nilUUID+`"`, string(raw))

	// Standalone value.
	raw, err = json.Marshal(order)
	require.NoError(t, err)
	require.Equal(t, `"`+fixedV7+`"`, string(raw))
	var single OrderID
	require.NoError(t, json.Unmarshal(raw, &single))
	require.Equal(t, order, single)
}

func TestJSON_Unmarshal_Rejects(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"number":          `12345`,
		"object":          `{"id":"` + fixedV7 + `"}`,
		"array":           `["` + fixedV7 + `"]`,
		"bool":            `true`,
		"empty string":    `""`,
		"v4 string":       `"018f1234-5678-4abc-9def-0123456789ab"`,
		"braced":          `"{` + fixedV7 + `}"`,
		"escaped garbage": `"018f1234-5678-7abc-9def-0123456789éb"`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var out OrderID
			err := json.Unmarshal([]byte(raw), &out)
			require.Error(t, err)
			require.True(t, out.IsZero())
		})
	}
}

func TestJSON_NullLeavesValueUnchanged(t *testing.T) {
	t.Parallel()
	order := id.MustParse[orderKind](fixedV7)
	out := order
	require.NoError(t, json.Unmarshal([]byte(`null`), &out))
	require.Equal(t, order, out)

	type doc struct {
		Order OrderID `json:"order"`
	}
	var d doc
	require.NoError(t, json.Unmarshal([]byte(`{"order":null}`), &d))
	require.True(t, d.Order.IsZero())
}

func TestJSON_UnicodeEscapedStringIsAccepted(t *testing.T) {
	t.Parallel()
	// A JSON string may spell ASCII with \u escapes; json decoding happens
	// before the ID grammar is applied.
	backslash := string(rune(0x5c))
	raw := `"` + backslash + `u0030` + fixedV7[1:] + `"`
	require.Contains(t, raw, backslash+"u0030", "input must carry a literal JSON escape")
	require.Len(t, raw, 36+2+5, "escape is six bytes for one character")
	var out OrderID
	require.NoError(t, json.Unmarshal([]byte(raw), &out))
	require.Equal(t, fixedV7, out.String())
}

func TestText_MapKeys(t *testing.T) {
	t.Parallel()
	order := id.MustParse[orderKind](fixedV7)
	m := map[OrderID]int{order: 1}
	raw, err := json.Marshal(m)
	require.NoError(t, err)
	require.Equal(t, `{"`+fixedV7+`":1}`, string(raw))

	var back map[OrderID]int
	require.NoError(t, json.Unmarshal(raw, &back))
	require.Equal(t, m, back)

	text, err := order.MarshalText()
	require.NoError(t, err)
	require.Equal(t, fixedV7, string(text))
}

func TestSQL_Value(t *testing.T) {
	t.Parallel()
	order := id.MustParse[orderKind](fixedV7)
	v, err := order.Value()
	require.NoError(t, err)
	require.Equal(t, fixedV7, v)

	v, err = OrderID{}.Value()
	require.NoError(t, err)
	require.Nil(t, v, "zero ID is SQL NULL")
}

func TestSQL_Scan(t *testing.T) {
	t.Parallel()
	want := id.MustParse[orderKind](fixedV7)
	raw := want.Bytes()

	cases := []struct {
		name string
		src  any
	}{
		{"string", fixedV7},
		{"string upper", strings.ToUpper(fixedV7)},
		{"bytes 36", []byte(fixedV7)},
		{"bytes 16", raw[:]},
		{"array 16", raw},
		{"uuid.UUID", uuid.UUID(raw)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got OrderID
			require.NoError(t, got.Scan(tc.src))
			require.Equal(t, want, got)
		})
	}

	t.Run("nil", func(t *testing.T) {
		t.Parallel()
		got := want
		require.NoError(t, got.Scan(nil))
		require.True(t, got.IsZero())
	})

	bad := []struct {
		name string
		src  any
		want error
	}{
		{"int", 42, nil},
		{"v4 string", "018f1234-5678-4abc-9def-0123456789ab", id.ErrVersion},
		{"braced string", "{" + fixedV7 + "}", id.ErrFormat},
		{"bytes 15", raw[:15], id.ErrFormat},
		{"bytes 17", append(append([]byte{}, raw[:]...), 0), id.ErrFormat},
		{"bytes 32 hex", []byte(strings.ReplaceAll(fixedV7, "-", "")), id.ErrFormat},
		{"raw v4", func() []byte { b := raw; b[6] = 0x4a; return b[:] }(), id.ErrVersion},
	}
	for _, tc := range bad {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			t.Parallel()
			got := want
			err := got.Scan(tc.src)
			require.Error(t, err)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
			}
			require.Equal(t, want, got, "receiver unchanged on error")
		})
	}
}

func TestCompare(t *testing.T) {
	t.Parallel()
	a := id.MustParse[orderKind]("018f1234-5678-7abc-9def-000000000001")
	b := id.MustParse[orderKind]("018f1234-5678-7abc-9def-000000000002")
	require.Equal(t, -1, id.Compare(a, b))
	require.Equal(t, 1, id.Compare(b, a))
	require.Equal(t, 0, id.Compare(a, a))
	require.Equal(t, -1, id.Compare(OrderID{}, a), "zero sorts first")
}

func TestUntyped(t *testing.T) {
	t.Parallel()
	order := id.MustParse[orderKind](fixedV7)
	u := order.Untyped()
	require.Equal(t, order.Bytes(), u.Bytes())
	require.Equal(t, order.String(), u.String())

	back, err := id.Bytes16[orderKind](u.Bytes())
	require.NoError(t, err)
	require.Equal(t, order, back)
}

func TestLogValue(t *testing.T) {
	t.Parallel()
	order := id.MustParse[orderKind](fixedV7)
	require.Equal(t, slog.KindString, order.LogValue().Kind())
	require.Equal(t, fixedV7, order.LogValue().String())
}

func TestErrors_AreWrapped(t *testing.T) {
	t.Parallel()
	_, err := id.Parse[orderKind]("x")
	require.True(t, errors.Is(err, id.ErrFormat))
	_, err = id.Parse[orderKind]("018f1234-5678-4abc-9def-0123456789ab")
	require.True(t, errors.Is(err, id.ErrVersion))
}

// TestProp_RoundTrip: any 16 bytes forced into the v7 layout survive
// String -> Parse and Bytes -> Bytes16 unchanged, in both hex cases.
func TestProp_RoundTrip(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		raw := rapid.SliceOfN(rapid.Byte(), 16, 16).Draw(rt, "raw")
		var b [16]byte
		copy(b[:], raw)
		b[6] = 0x70 | (b[6] & 0x0f)
		b[8] = 0x80 | (b[8] & 0x3f)

		i, err := id.Bytes16[orderKind](b)
		require.NoError(rt, err)
		require.Equal(rt, b, i.Bytes())

		s := i.String()
		require.Len(rt, s, 36)
		for _, in := range []string{s, strings.ToUpper(s)} {
			back, err := id.Parse[orderKind](in)
			require.NoError(rt, err)
			require.Equal(rt, i, back)
		}

		raw2, err := json.Marshal(i)
		require.NoError(rt, err)
		var back OrderID
		require.NoError(rt, json.Unmarshal(raw2, &back))
		require.Equal(rt, i, back)

		var scanned OrderID
		require.NoError(rt, scanned.Scan(s))
		require.Equal(rt, i, scanned)
	})
}

// TestProp_TimeOrdering: NewAt preserves the order of distinct milliseconds
// and Time() recovers the truncated instant.
func TestProp_TimeOrdering(t *testing.T) {
	t.Parallel()
	const maxMs = 1<<48 - 1
	rapid.Check(t, func(rt *rapid.T) {
		ms1 := rapid.Int64Range(0, maxMs).Draw(rt, "ms1")
		ms2 := rapid.Int64Range(0, maxMs).Draw(rt, "ms2")
		sub := rapid.Int64Range(0, int64(time.Millisecond)-1).Draw(rt, "sub")

		a := id.NewAt[orderKind](time.UnixMilli(ms1).Add(time.Duration(sub)))
		b := id.NewAt[orderKind](time.UnixMilli(ms2))
		require.Equal(rt, ms1, a.Time().UnixMilli())
		require.Equal(rt, ms2, b.Time().UnixMilli())

		switch {
		case ms1 < ms2:
			require.Equal(rt, -1, id.Compare(a, b))
			require.Less(rt, a.String(), b.String())
		case ms1 > ms2:
			require.Equal(rt, 1, id.Compare(a, b))
			require.Greater(rt, a.String(), b.String())
		default:
			require.Equal(rt, a.String()[:13], b.String()[:13])
		}
	})
}

// FuzzParse: Parse and ParseAny never panic; a successful Parse round-trips
// through String and equals the lowercase input.
func FuzzParse(f *testing.F) {
	seeds := []string{
		"", fixedV7, strings.ToUpper(fixedV7), nilUUID,
		"018f1234-5678-4abc-9def-0123456789ab",
		"{" + fixedV7 + "}", "urn:uuid:" + fixedV7,
		strings.ReplaceAll(fixedV7, "-", ""),
		"ffffffff-ffff-ffff-ffff-ffffffffffff",
		"018f1234-5678-7abc-9def-0123456789a\xff",
		"٩٩٩٩٩٩٩٩-٩٩٩٩",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got, err := id.Parse[orderKind](s)
		if err != nil {
			if !got.IsZero() {
				t.Fatalf("Parse(%q) returned error and non-zero id", s)
			}
			if !errors.Is(err, id.ErrFormat) && !errors.Is(err, id.ErrVersion) {
				t.Fatalf("Parse(%q) error %v is not ErrFormat/ErrVersion", s, err)
			}
		} else {
			if want := strings.ToLower(s); got.String() != want {
				t.Fatalf("Parse(%q).String() = %q, want %q", s, got.String(), want)
			}
			back, err := id.Parse[orderKind](got.String())
			if err != nil || back != got {
				t.Fatalf("round trip of %q failed: %v", s, err)
			}
			text, _ := got.MarshalText()
			var viaText OrderID
			if err := viaText.UnmarshalText(text); err != nil || viaText != got {
				t.Fatalf("text round trip of %q failed: %v", s, err)
			}
		}

		anyID, err := id.ParseAny(s)
		if err == nil && anyID.String() != strings.ToLower(s) {
			t.Fatalf("ParseAny(%q).String() = %q", s, anyID.String())
		}

		var scanned OrderID
		_ = scanned.Scan(s)
		_ = scanned.Scan([]byte(s))
	})
}
