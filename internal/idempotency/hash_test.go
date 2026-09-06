package idempotency

import (
	"encoding/json"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

func TestCanonicalJSON(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"whitespace only", " \n\t ", ""},
		{"sorted keys", `{"b":1,"a":2}`, `{"a":2,"b":1}`},
		{"nested sort and compaction", `{ "z": {"y": [1, 2, {"c": null, "b": true}]}, "a": "x" }`, `{"a":"x","z":{"y":[1,2,{"b":true,"c":null}]}}`},
		{"numbers verbatim", `{"n": 1.10, "m": 1e3, "k": -0}`, `{"k":-0,"m":1e3,"n":1.10}`},
		{"array order preserved", `[3,1,2]`, `[3,1,2]`},
		{"no html escaping", `{"s":"<a&b>"}`, `{"s":"<a&b>"}`},
		{"unicode kept", `{"s":"héllo ✓"}`, `{"s":"héllo ✓"}`},
		{"scalar string", `"x"`, `"x"`},
		{"scalar true", ` true `, `true`},
		{"invalid json returned unchanged", `{"a":`, `{"a":`},
		{"trailing garbage returned unchanged", `{"a":1} x`, `{"a":1} x`},
		{"two values returned unchanged", `{"a":1}{"b":2}`, `{"a":1}{"b":2}`},
		{"plain text unchanged", `hello`, `hello`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := string(CanonicalJSON([]byte(tt.in)))
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestHashRequest(t *testing.T) {
	t.Parallel()
	a := HashRequest("POST", "/v1/intents", []byte(`{"amount":"10.00","asset":"USDC"}`))
	b := HashRequest("post", "/v1/intents", []byte("{ \"asset\": \"USDC\",\n \"amount\": \"10.00\" }"))
	assert.Equal(t, a, b, "method case, key order and whitespace do not matter")
	assert.Len(t, a, 64)

	assert.NotEqual(t, a, HashRequest("POST", "/v1/intents", []byte(`{"amount":"10.01","asset":"USDC"}`)), "value change")
	assert.NotEqual(t, a, HashRequest("POST", "/v1/orders", []byte(`{"amount":"10.00","asset":"USDC"}`)), "path change")
	assert.NotEqual(t, a, HashRequest("PUT", "/v1/intents", []byte(`{"amount":"10.00","asset":"USDC"}`)), "method change")
	assert.NotEqual(t, HashRequest("POST", "/x", []byte(`{"n":1}`)), HashRequest("POST", "/x", []byte(`{"n":1.0}`)), "numeric text is significant")
	assert.NotEqual(t, HashRequest("POST", "/x", []byte(`[1,2]`)), HashRequest("POST", "/x", []byte(`[2,1]`)), "array order is significant")

	empty := HashRequest("DELETE", "/v1/x", nil)
	assert.Equal(t, empty, HashRequest("DELETE", "/v1/x", []byte("  \n")))
	assert.NotEqual(t, empty, HashRequest("DELETE", "/v1/x", []byte("{}")))

	// Non-JSON bodies still hash deterministically.
	assert.Equal(t, HashRequest("POST", "/x", []byte("not json")), HashRequest("POST", "/x", []byte("not json")))
	assert.NotEqual(t, HashRequest("POST", "/x", []byte("not json")), HashRequest("POST", "/x", []byte("not json!")))

	// Path/method separators cannot be confused.
	assert.NotEqual(t, HashRequest("POST", "/a\n/b", nil), HashRequest("POST\n/a", "/b", nil))
}

// jsonValue generates arbitrary JSON values as Go values.
func jsonValue(depth int) *rapid.Generator[any] {
	return rapid.Custom(func(t *rapid.T) any {
		choice := rapid.IntRange(0, 5).Draw(t, "kind")
		if depth <= 0 && choice >= 4 {
			choice = rapid.IntRange(0, 3).Draw(t, "leafkind")
		}
		switch choice {
		case 0:
			return nil
		case 1:
			return rapid.Bool().Draw(t, "bool")
		case 2:
			return json.Number(rapid.SampledFrom([]string{"0", "1", "-1", "1.10", "1e3", "12345678901234567890", "0.000001"}).Draw(t, "num"))
		case 3:
			return rapid.String().Draw(t, "str")
		case 4:
			n := rapid.IntRange(0, 4).Draw(t, "len")
			arr := make([]any, n)
			for i := range arr {
				arr[i] = jsonValue(depth-1).Draw(t, "elem")
			}
			return arr
		default:
			n := rapid.IntRange(0, 4).Draw(t, "keys")
			m := map[string]any{}
			for i := 0; i < n; i++ {
				k := rapid.StringMatching(`[a-z_]{1,6}`).Draw(t, "key")
				m[k] = jsonValue(depth-1).Draw(t, "val")
			}
			return m
		}
	})
}

// shuffledJSON renders v with object keys in random order and random whitespace.
func shuffledJSON(rng *rand.Rand, v any) string {
	pad := func() string {
		if rng.IntN(3) == 0 {
			return " "
		}
		return ""
	}
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		if x {
			return "true"
		}
		return "false"
	case json.Number:
		return x.String()
	case string:
		b, _ := json.Marshal(x)
		return string(b)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = shuffledJSON(rng, e)
		}
		return "[" + pad() + strings.Join(parts, ","+pad()) + pad() + "]"
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		rng.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
		parts := make([]string, len(keys))
		for i, k := range keys {
			kb, _ := json.Marshal(k)
			parts[i] = string(kb) + pad() + ":" + pad() + shuffledJSON(rng, x[k])
		}
		return "{" + pad() + strings.Join(parts, ","+pad()) + pad() + "}"
	}
	panic("unreachable")
}

func TestProp_CanonicalJSON_IndependentOfKeyOrderAndWhitespace(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		v := jsonValue(3).Draw(rt, "value")
		seed := rapid.Uint64().Draw(rt, "seed")
		rng1 := rand.New(rand.NewPCG(seed, 1))
		rng2 := rand.New(rand.NewPCG(seed, 2))
		a := shuffledJSON(rng1, v)
		b := shuffledJSON(rng2, v)
		ca := CanonicalJSON([]byte(a))
		cb := CanonicalJSON([]byte(b))
		if string(ca) != string(cb) {
			rt.Fatalf("canonical forms differ:\n%s\n%s", ca, cb)
		}
		if !json.Valid(ca) {
			rt.Fatalf("canonical form is not valid JSON: %s", ca)
		}
		// Idempotent.
		if string(CanonicalJSON(ca)) != string(ca) {
			rt.Fatalf("canonicalisation is not idempotent")
		}
		if HashRequest("POST", "/p", []byte(a)) != HashRequest("POST", "/p", []byte(b)) {
			rt.Fatalf("hashes differ for equivalent bodies")
		}
	})
}

func FuzzCanonicalJSON(f *testing.F) {
	for _, s := range []string{"", "{}", "[]", `{"a":1}`, `{"b":{"a":[1,2,3]}}`, `"x"`, "nope", `{"a":`, "\x00", `{"a":1}{"b":2}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		out := CanonicalJSON(body)
		if json.Valid(body) && len(strings.TrimSpace(string(body))) > 0 {
			require.True(t, json.Valid(out), "valid input must canonicalise to valid JSON")
			require.Equal(t, string(out), string(CanonicalJSON(out)), "idempotent")
		}
		_ = HashRequest("POST", "/fuzz", body)
	})
}
