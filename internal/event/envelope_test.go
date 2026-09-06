package event

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/errs"
)

func TestEnvelope_Validate(t *testing.T) {
	t.Parallel()
	base := validEnvelope(TopicOrderTransitioned, "order-1")
	require.NoError(t, base.Validate())

	tests := []struct {
		name   string
		mutate func(e *Envelope)
		field  string
	}{
		{"missing id", func(e *Envelope) { e.ID = "" }, "id"},
		{"nil id", func(e *Envelope) { e.ID = "00000000-0000-0000-0000-000000000000" }, "id"},
		{"non-v7 id", func(e *Envelope) { e.ID = "123e4567-e89b-12d3-a456-426614174000" }, "id"},
		{"missing type", func(e *Envelope) { e.Type = "" }, "type"},
		{"uppercase type", func(e *Envelope) { e.Type = "Order.Transitioned" }, "type"},
		{"type with dash", func(e *Envelope) { e.Type = "order-transitioned" }, "type"},
		{"type too long", func(e *Envelope) { e.Type = strings.Repeat("a", MaxTypeLength+1) }, "type"},
		{"schema version zero", func(e *Envelope) { e.SchemaVersion = 0 }, "schema_version"},
		{"schema version negative", func(e *Envelope) { e.SchemaVersion = -1 }, "schema_version"},
		{"missing source", func(e *Envelope) { e.Source = "" }, "source"},
		{"control char in source", func(e *Envelope) { e.Source = "a\x01b" }, "source"},
		{"missing aggregate type", func(e *Envelope) { e.AggregateType = "" }, "aggregate_type"},
		{"missing aggregate id", func(e *Envelope) { e.AggregateID = "" }, "aggregate_id"},
		{"NUL in correlation id", func(e *Envelope) { e.CorrelationID = "x\x00y" }, "correlation_id"},
		{"dedup key too long", func(e *Envelope) { e.DedupKey = strings.Repeat("k", MaxDedupKeyLength+1) }, "dedup_key"},
		{"occurred_at zero", func(e *Envelope) { e.OccurredAt = time.Time{} }, "occurred_at"},
		{"occurred_at not UTC", func(e *Envelope) { e.OccurredAt = fixedTime.In(time.FixedZone("X", 3600)) }, "occurred_at"},
		{"recorded_at not UTC", func(e *Envelope) { e.RecordedAt = fixedTime.In(time.FixedZone("X", -3600)) }, "recorded_at"},
		{"empty header key", func(e *Envelope) { e.Headers = map[string]string{"": "v"} }, "headers"},
		{"NUL header value", func(e *Envelope) { e.Headers = map[string]string{"k": "a\x00"} }, "headers"},
		{"empty payload", func(e *Envelope) { e.Payload = nil }, "payload"},
		{"invalid payload", func(e *Envelope) { e.Payload = json.RawMessage(`{"a":`) }, "payload"},
		{"payload not object", func(e *Envelope) { e.Payload = json.RawMessage(`[1,2]`) }, "payload"},
		{"payload null", func(e *Envelope) { e.Payload = json.RawMessage(`null`) }, "payload"},
		{"payload trailing data", func(e *Envelope) { e.Payload = json.RawMessage(`{} {}`) }, "payload"},
		{"payload NUL escape", func(e *Envelope) { e.Payload = json.RawMessage(`{"a":"` + nulEscape + `"}`) }, "payload"},
		{"payload too large", func(e *Envelope) {
			e.Payload = json.RawMessage(`{"a":"` + strings.Repeat("x", MaxPayloadBytes) + `"}`)
		}, "payload"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := base
			e.Headers = map[string]string{"x-trace": "abc"}
			tc.mutate(&e)
			err := e.Validate()
			require.Error(t, err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
			ee, ok := errs.As(err)
			require.True(t, ok)
			assert.Contains(t, ee.Fields, tc.field, "fields: %v", ee.Fields)
		})
	}
}

func TestEnvelope_Validate_OptionalFieldsMayBeEmpty(t *testing.T) {
	t.Parallel()
	e := validEnvelope(TopicIntentTransitioned, "intent-1")
	e.CorrelationID, e.CausationID, e.DedupKey = "", "", ""
	e.Headers = nil
	e.RecordedAt = time.Time{}
	require.NoError(t, e.Validate())
}

func TestEnvelope_CanonicalBytes_StableUnderFieldReordering(t *testing.T) {
	t.Parallel()
	a := validEnvelope(TopicFillObserved, "order-9")
	a.Payload = json.RawMessage(`{"b":2,"a":{"y":[1,2,{"q":null}],"x":"1.50"},"c":true}`)
	b := a
	b.Payload = json.RawMessage(` { "c" : true , "a" : { "x" : "1.50" , "y" : [ 1 , 2 , { "q" : null } ] } , "b" : 2 } `)

	ca, err := a.CanonicalBytes()
	require.NoError(t, err)
	cb, err := b.CanonicalBytes()
	require.NoError(t, err)
	assert.Equal(t, string(ca), string(cb))
	assert.True(t, json.Valid(ca))
	assert.Contains(t, string(ca), `"payload":{"a":{"x":"1.50","y":[1,2,{"q":null}]},"b":2,"c":true}`)

	ha, err := a.Hash()
	require.NoError(t, err)
	hb, err := b.Hash()
	require.NoError(t, err)
	assert.Equal(t, ha, hb)
	assert.Len(t, ha, 64)

	// Top-level keys are sorted and the first key is aggregate_id.
	assert.True(t, bytes.HasPrefix(ca, []byte(`{"aggregate_id":"order-9","aggregate_type":"order",`)), string(ca))
}

func TestEnvelope_CanonicalBytes_NoHTMLEscapingAndUTCTimes(t *testing.T) {
	t.Parallel()
	e := validEnvelope(TopicSecurityEvent, "principal-1")
	e.Payload = json.RawMessage(`{"html":"<b>&'\"</b>","u":"é "}`)
	e.OccurredAt = time.Date(2026, 9, 5, 7, 0, 0, 5, time.FixedZone("X", 5*3600)).UTC()
	c, err := e.CanonicalBytes()
	require.NoError(t, err)
	s := string(c)
	assert.Contains(t, s, `"html":"<b>&'\"</b>"`)
	assert.NotContains(t, s, backslash+"u003c", "no html escaping")
	assert.NotContains(t, s, backslash+"u0026")
	assert.Contains(t, s, `"occurred_at":"2026-09-05T02:00:00.000000005Z"`)
	assert.Contains(t, s, `"é`)
	assert.NotContains(t, s, `"recorded_at"`, "zero recorded_at is omitted")
	assert.Contains(t, s, `"headers":{"x-trace":"abc"}`)
}

func TestEnvelope_CanonicalBytes_RejectsNUL(t *testing.T) {
	t.Parallel()
	e := validEnvelope(TopicSecurityEvent, "principal-1")
	e.Payload = json.RawMessage(`{"a` + nulEscape + `b":1}`)
	_, err := e.CanonicalBytes()
	require.ErrorIs(t, err, ErrNULByte)
	assert.Error(t, e.Validate())
	e.Payload = json.RawMessage(`{"a":"` + nul + `"}`)
	_, err = e.CanonicalBytes()
	require.Error(t, err, "a raw NUL is not even valid json")
}

func TestDecode_RoundTrip(t *testing.T) {
	t.Parallel()
	e := validEnvelope(TopicLedgerTransactionPosted, "ltx-1")
	e.RecordedAt = fixedTime.Add(time.Second)
	e.DedupKey = "dedup-1"
	c, err := e.CanonicalBytes()
	require.NoError(t, err)

	got, err := Decode(c)
	require.NoError(t, err)
	gotC, err := got.CanonicalBytes()
	require.NoError(t, err)
	assert.Equal(t, string(c), string(gotC))
	assert.Equal(t, e.ID, got.ID)
	assert.Equal(t, e.Type, got.Type)
	assert.Equal(t, e.SchemaVersion, got.SchemaVersion)
	assert.Equal(t, e.AggregateID, got.AggregateID)
	assert.Equal(t, e.DedupKey, got.DedupKey)
	assert.True(t, e.OccurredAt.Equal(got.OccurredAt))
	assert.True(t, e.RecordedAt.Equal(got.RecordedAt))
	assert.Equal(t, time.UTC, got.OccurredAt.Location())
	assert.Equal(t, e.Headers, got.Headers)
}

func TestDecode_Rejects(t *testing.T) {
	t.Parallel()
	for name, in := range map[string]string{
		"empty":         "",
		"array":         "[]",
		"null":          "null",
		"unknown field": `{"id":"x","bogus":1}`,
		"trailing":      `{} {}`,
		"bad time":      `{"id":"x","occurred_at":"yesterday"}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Decode([]byte(in))
			require.Error(t, err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}
}

func TestDecode_NormalisesOffsetTimesToUTC(t *testing.T) {
	t.Parallel()
	e := validEnvelope(TopicGateTransitioned, "gate-1")
	c, err := e.CanonicalBytes()
	require.NoError(t, err)
	shifted := bytes.Replace(c, []byte(`"occurred_at":"2026-09-05T12:00:00.123456789Z"`), []byte(`"occurred_at":"2026-09-05T14:00:00.123456789+02:00"`), 1)
	require.NotEqual(t, string(c), string(shifted))
	got, err := Decode(shifted)
	require.NoError(t, err)
	assert.Equal(t, time.UTC, got.OccurredAt.Location())
	assert.True(t, got.OccurredAt.Equal(e.OccurredAt))
}

// jsonValue generates JSON values in the shape encoding/json produces with
// UseNumber, without NUL (jsonb cannot store it).
func jsonValue(depth int) *rapid.Generator[any] {
	return rapid.Custom(func(t *rapid.T) any {
		maxKind := 5
		if depth <= 0 {
			maxKind = 3
		}
		switch rapid.IntRange(0, maxKind).Draw(t, "kind") {
		case 0:
			return nil
		case 1:
			return rapid.Bool().Draw(t, "bool")
		case 2:
			if rapid.Bool().Draw(t, "float") {
				return json.Number(strconv.FormatInt(rapid.Int64().Draw(t, "int"), 10) + "." + strconv.Itoa(rapid.IntRange(0, 999).Draw(t, "frac")))
			}
			return json.Number(strconv.FormatInt(rapid.Int64().Draw(t, "int"), 10))
		case 3:
			return cleanString().Draw(t, "string")
		case 4:
			return rapid.SliceOfN(jsonValue(depth-1), 0, 4).Draw(t, "array")
		default:
			return rapid.MapOfN(cleanString(), jsonValue(depth-1), 0, 4).Draw(t, "object")
		}
	})
}

func cleanString() *rapid.Generator[string] {
	return rapid.String().Filter(func(s string) bool { return !strings.ContainsRune(s, 0) })
}

// writeShuffled writes v as JSON with object keys in a random order.
func writeShuffled(t *rapid.T, buf *bytes.Buffer, v any) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		keys = rapid.Permutation(keys).Draw(t, "order")
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteString(" , ")
			}
			kb, _ := json.Marshal(k)
			buf.Write(kb)
			buf.WriteString(" : ")
			writeShuffled(t, buf, x[k])
		}
		buf.WriteByte('}')
	case []any:
		buf.WriteByte('[')
		for i, el := range x {
			if i > 0 {
				buf.WriteString(", ")
			}
			writeShuffled(t, buf, el)
		}
		buf.WriteByte(']')
	default:
		b, err := json.Marshal(x)
		if err != nil {
			t.Fatalf("marshal %T: %v", x, err)
		}
		buf.Write(b)
	}
}

func TestProp_CanonicalBytesStableUnderReordering(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		obj := rapid.MapOfN(cleanString(), jsonValue(3), 0, 6).Draw(rt, "payload")
		var a, b bytes.Buffer
		writeShuffled(rt, &a, obj)
		writeShuffled(rt, &b, obj)

		e := validEnvelope(TopicAuditEventAppended, "audit-1")
		e.Payload = a.Bytes()
		ca, err := e.CanonicalBytes()
		if err != nil {
			rt.Fatalf("canonical a: %v", err)
		}
		e.Payload = b.Bytes()
		cb, err := e.CanonicalBytes()
		if err != nil {
			rt.Fatalf("canonical b: %v", err)
		}
		if !bytes.Equal(ca, cb) {
			rt.Fatalf("canonical bytes differ:\n%s\n%s", ca, cb)
		}
		if !json.Valid(ca) {
			rt.Fatalf("canonical bytes are not valid json: %s", ca)
		}
		// Canonicalisation is idempotent through Decode.
		d, err := Decode(ca)
		if err != nil {
			rt.Fatalf("decode canonical: %v", err)
		}
		cd, err := d.CanonicalBytes()
		if err != nil {
			rt.Fatalf("re-canonicalise: %v", err)
		}
		if !bytes.Equal(ca, cd) {
			rt.Fatalf("canonicalisation not idempotent:\n%s\n%s", ca, cd)
		}
	})
}

func FuzzEnvelopeJSON(f *testing.F) {
	e := validEnvelope(TopicPredictionCommitted, "pred-1")
	c, err := e.CanonicalBytes()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(c)
	f.Add([]byte(`{}`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`null`))
	f.Add([]byte(`{"id":"` + e.ID + `","type":"a.b","schema_version":1,"source":"s","aggregate_type":"t","aggregate_id":"i","occurred_at":"2026-01-01T00:00:00Z","payload":{"k":"` + nulEscape + `"}}`))
	f.Add([]byte(`{"payload":{"n":1e999999999}}`))
	f.Add([]byte("\xff\xfe"))
	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := Decode(data)
		if err != nil {
			// Also exercise the validator and canonicaliser on the raw
			// struct decode path with lenient parsing.
			var raw Envelope
			if json.Unmarshal(data, &raw) == nil {
				_ = raw.Validate()
				_, _ = raw.CanonicalBytes()
				_, _ = raw.Hash()
			}
			return
		}
		if err := got.Validate(); err != nil {
			t.Fatalf("decoded envelope does not validate: %v", err)
		}
		c1, err := got.CanonicalBytes()
		if err != nil {
			t.Fatalf("canonical: %v", err)
		}
		again, err := Decode(c1)
		if err != nil {
			t.Fatalf("decode canonical: %v\n%s", err, c1)
		}
		c2, err := again.CanonicalBytes()
		if err != nil {
			t.Fatalf("re-canonical: %v", err)
		}
		if !bytes.Equal(c1, c2) {
			t.Fatalf("canonical bytes unstable:\n%s\n%s", c1, c2)
		}
	})
}

func TestIsJSONNumber(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"0", "-0", "12", "1.5", "1e5", "1E+5", "-1.25e-3"} {
		assert.True(t, isJSONNumber(ok), ok)
	}
	for _, bad := range []string{"", "-", "01", "1.", ".5", "1e", "1e+", "+1", "0x1", "1 ", "NaN"} {
		assert.False(t, isJSONNumber(bad), bad)
	}
}
