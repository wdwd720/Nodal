package audit

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/id"
)

type orderedA struct {
	Zeta  string            `json:"zeta"`
	Alpha int               `json:"alpha"`
	When  time.Time         `json:"when"`
	Inner map[string]any    `json:"inner"`
	Raw   json.RawMessage   `json:"raw"`
	Blob  []byte            `json:"blob"`
	Ptr   *time.Time        `json:"ptr"`
	Nest  []orderedInnerA   `json:"nest"`
	Opt   map[string]string `json:"opt,omitempty"`
}

type orderedB struct {
	Nest  []orderedInnerB   `json:"nest"`
	Ptr   *time.Time        `json:"ptr"`
	Blob  []byte            `json:"blob"`
	Raw   json.RawMessage   `json:"raw"`
	Inner map[string]any    `json:"inner"`
	When  time.Time         `json:"when"`
	Alpha int               `json:"alpha"`
	Zeta  string            `json:"zeta"`
	Opt   map[string]string `json:"opt,omitempty"`
}

type orderedInnerA struct {
	B string `json:"b"`
	A int64  `json:"a"`
}

type orderedInnerB struct {
	A int64  `json:"a"`
	B string `json:"b"`
}

func TestCanonicalJSON_Table(t *testing.T) {
	t.Parallel()
	est := time.FixedZone("EST", -5*3600)
	when := time.Date(2026, 9, 5, 7, 0, 0, 123456789, est) // 12:00:00.123456789Z
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"nil", nil, `null`},
		{"bool", true, `true`},
		{"string no html escape", "<a&b> \"q\" \\", `"<a&b> \"q\" \\"`},
		{"int", 42, `42`},
		{"negative zero float", math.Copysign(0, -1), `0`},
		{"integral float", 1.0, `1`},
		{"map sorted", map[string]any{"b": 1, "a": 2, "aa": 3}, `{"a":2,"aa":3,"b":1}`},
		{"nested sorted", map[string]any{"z": map[string]any{"y": []any{1, map[string]any{"c": nil, "b": true}}}, "a": "x"}, `{"a":"x","z":{"y":[1,{"b":true,"c":null}]}}`},
		{"raw message reordered", json.RawMessage(`{ "b" : 1 , "a" : [ 2 , 3 ] }`), `{"a":[2,3],"b":1}`},
		{"raw message negative zero", json.RawMessage(`{"n":-0}`), `{"n":0}`},
		{"time utc nanos", when, `"2026-09-05T12:00:00.123456789Z"`},
		{"time in map", map[string]any{"t": when}, `{"t":"2026-09-05T12:00:00.123456789Z"}`},
		{"time pointer", &when, `"2026-09-05T12:00:00.123456789Z"`},
		{"bytes base64", []byte{0xde, 0xad, 0xbe, 0xef}, `"3q2+7w=="`},
		{"nil bytes", []byte(nil), `null`},
		{"id marshals as string", id.MustParse[id.Any]("01924e5a-7b2c-7d3e-8f4a-5b6c7d8e9f0a"), `"01924e5a-7b2c-7d3e-8f4a-5b6c7d8e9f0a"`},
		{"unicode kept", "héllo ✓", `"héllo ✓"`},
		{"array order preserved", []int{3, 1, 2}, `[3,1,2]`},
		{"max safe integer", int64(MaxSafeInteger), `9007199254740992`},
		{"min safe integer", int64(-MaxSafeInteger), `-9007199254740992`},
		{"empty struct", struct{}{}, `{}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := CanonicalJSON(tt.in)
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(got))
		})
	}
}

func TestCanonicalJSON_RejectsNonCanonicalNumbers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   any
	}{
		{"fraction", 1.5},
		{"fraction in map", map[string]any{"amount": 10.25}},
		{"tiny", 1e-7},
		{"huge float", 1e21},
		{"above 2^53", int64(MaxSafeInteger + 1)},
		{"below -2^53", int64(-MaxSafeInteger - 1)},
		{"uint64 max", uint64(math.MaxUint64)},
		{"raw fraction", json.RawMessage(`{"a":1.0}`)},
		{"raw exponent", json.RawMessage(`[1e3]`)},
		{"raw leading zero", json.RawMessage(`[01]`)},
		{"raw big", json.RawMessage(`123456789012345678901234567890`)},
		{"json.Number fraction", json.Number("2.50")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := CanonicalJSON(tt.in)
			require.Error(t, err)
			if tt.name != "raw leading zero" {
				assert.True(t, errors.Is(err, ErrNonCanonicalNumber), "got %v", err)
			}
		})
	}
}

func TestCanonicalJSON_Errors(t *testing.T) {
	t.Parallel()
	_, err := CanonicalJSON(math.NaN())
	require.Error(t, err)
	_, err = CanonicalJSON(json.RawMessage(`{"a":`))
	require.Error(t, err)
	_, err = CanonicalJSON(json.RawMessage(`{"a":1} {"b":2}`))
	require.Error(t, err)
	_, err = CanonicalJSON(make(chan int))
	require.Error(t, err)
	_, err = CanonicalJSON(func() {})
	require.Error(t, err)
}

func TestCanonicalJSON_StructFieldOrderIrrelevant(t *testing.T) {
	t.Parallel()
	loc := time.FixedZone("X", 3*3600)
	when := time.Date(2026, 1, 2, 3, 4, 5, 6, loc)
	ptr := time.Date(2025, 12, 31, 23, 59, 59, 0, loc)
	a := orderedA{
		Zeta: "z", Alpha: 7, When: when, Inner: map[string]any{"k2": "v", "k1": []any{"x", 1}},
		Raw: json.RawMessage(`{"q":2,"p":1}`), Blob: []byte("hi"), Ptr: &ptr,
		Nest: []orderedInnerA{{B: "b", A: 1}, {B: "c", A: 2}},
	}
	b := orderedB{
		Nest: []orderedInnerB{{A: 1, B: "b"}, {A: 2, B: "c"}}, Ptr: &ptr, Blob: []byte("hi"),
		Raw: json.RawMessage(`{"p":1,"q":2}`), Inner: map[string]any{"k1": []any{"x", 1}, "k2": "v"},
		When: when.UTC(), Alpha: 7, Zeta: "z",
	}
	ca, err := CanonicalJSON(a)
	require.NoError(t, err)
	cb, err := CanonicalJSON(b)
	require.NoError(t, err)
	assert.Equal(t, string(ca), string(cb))
	assert.Equal(t, `{"alpha":7,"blob":"aGk=","inner":{"k1":["x",1],"k2":"v"},"nest":[{"a":1,"b":"b"},{"a":2,"b":"c"}],"ptr":"2025-12-31T20:59:59Z","raw":{"p":1,"q":2},"when":"2026-01-02T00:04:05.000000006Z","zeta":"z"}`, string(ca))

	// The input is not mutated (deep copy), and pointer targets are untouched.
	assert.Equal(t, loc, ptr.Location())
	assert.Equal(t, loc, a.When.Location())
}

func TestCanonicalJSON_Idempotent(t *testing.T) {
	t.Parallel()
	in := json.RawMessage(`{"z":[{"b":null,"a":"<&"}],"a":"x","m":{"y":1,"x":-0}}`)
	once, err := CanonicalJSON(in)
	require.NoError(t, err)
	twice, err := CanonicalJSON(json.RawMessage(once))
	require.NoError(t, err)
	assert.Equal(t, string(once), string(twice))
	assert.Equal(t, `{"a":"x","m":{"x":0,"y":1},"z":[{"a":"<&","b":null}]}`, string(once))
}

// genValue produces a random JSON-compatible value tree with integers inside
// the canonical range.
func genValue(depth int) *rapid.Generator[any] {
	return rapid.Custom(func(t *rapid.T) any {
		kind := rapid.IntRange(0, 5).Draw(t, "kind")
		if depth <= 0 && kind >= 4 {
			kind = rapid.IntRange(0, 3).Draw(t, "leaf")
		}
		switch kind {
		case 0:
			return nil
		case 1:
			return rapid.Bool().Draw(t, "bool")
		case 2:
			return rapid.Int64Range(-MaxSafeInteger, MaxSafeInteger).Draw(t, "int")
		case 3:
			return rapid.String().Draw(t, "string")
		case 4:
			n := rapid.IntRange(0, 4).Draw(t, "len")
			out := make([]any, n)
			for i := range out {
				out[i] = genValue(depth-1).Draw(t, "elem")
			}
			return out
		default:
			n := rapid.IntRange(0, 5).Draw(t, "size")
			out := make(map[string]any, n)
			for i := 0; i < n; i++ {
				out[rapid.String().Draw(t, "key")] = genValue(depth-1).Draw(t, "value")
			}
			return out
		}
	})
}

// writeShuffled serializes v as JSON with object keys in a random order.
func writeShuffled(buf *bytes.Buffer, v any, rng *rand.Rand) {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		buf.WriteString(strconv.FormatBool(x))
	case int64:
		buf.WriteString(strconv.FormatInt(x, 10))
	case string:
		b, _ := json.Marshal(x)
		buf.Write(b)
	case []any:
		buf.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				buf.WriteString(" , ")
			}
			writeShuffled(buf, e, rng)
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		rng.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
		buf.WriteString("{ ")
		for i, k := range keys {
			if i > 0 {
				buf.WriteString(" ,\n ")
			}
			b, _ := json.Marshal(k)
			buf.Write(b)
			buf.WriteString(" : ")
			writeShuffled(buf, x[k], rng)
		}
		buf.WriteString(" }")
	}
}

func TestProp_CanonicalJSON_KeyOrderAndWhitespaceIrrelevant(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		v := genValue(3).Draw(rt, "value")
		seed1 := rapid.Uint64().Draw(rt, "seed1")
		seed2 := rapid.Uint64().Draw(rt, "seed2")
		var b1, b2 bytes.Buffer
		writeShuffled(&b1, v, rand.New(rand.NewPCG(seed1, 1)))
		writeShuffled(&b2, v, rand.New(rand.NewPCG(seed2, 2)))

		fromValue, err := CanonicalJSON(v)
		require.NoError(rt, err)
		fromText1, err := CanonicalJSON(json.RawMessage(b1.Bytes()))
		require.NoError(rt, err)
		fromText2, err := CanonicalJSON(json.RawMessage(b2.Bytes()))
		require.NoError(rt, err)
		assert.Equal(rt, string(fromValue), string(fromText1))
		assert.Equal(rt, string(fromValue), string(fromText2))

		again, err := CanonicalJSON(json.RawMessage(fromValue))
		require.NoError(rt, err)
		assert.Equal(rt, string(fromValue), string(again), "idempotent")
		assert.True(rt, json.Valid(fromValue))
	})
}

func FuzzCanonicalJSON(f *testing.F) {
	seeds := []string{`{}`, `[]`, `null`, `{"a":1,"b":[true,null,"x"]}`, `1.5`, `-0`, "\"\x00\"", `{"a":`, `[1e3]`, `{"b":{"a":1},"a":{"b":2}}`, "\xff\xfe", `9007199254740993`}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		out, err := CanonicalJSON(json.RawMessage(data))
		if err == nil {
			if !json.Valid(out) {
				t.Fatalf("canonical output is not valid JSON: %q", out)
			}
			again, err := CanonicalJSON(json.RawMessage(out))
			if err != nil || !bytes.Equal(out, again) {
				t.Fatalf("not idempotent: %q -> %q (%v)", out, again, err)
			}
			if strings.ContainsAny(string(out), "\n\t") {
				t.Fatalf("whitespace in canonical output: %q", out)
			}
		}
		// A string value must always canonicalize, even invalid UTF-8.
		if _, err := CanonicalJSON(string(data)); err != nil {
			t.Fatalf("string input failed: %v", err)
		}
		_, _ = CanonicalJSON(map[string]any{"k": string(data), "raw": json.RawMessage(data)})
	})
}
