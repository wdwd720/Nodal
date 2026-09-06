package audit

import (
	"bytes"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"time"
)

// MaxSafeInteger is the largest integer magnitude CanonicalJSON accepts.
// Beyond 2^53 a JSON number cannot be round-tripped through every consumer
// exactly, so larger values (and every fractional value) must be strings.
const MaxSafeInteger = 1 << 53

// ErrNonCanonicalNumber is returned (wrapped) by CanonicalJSON for a number
// with a fractional part, an exponent, or a magnitude above MaxSafeInteger.
var ErrNonCanonicalNumber = errors.New("audit: non-canonical number: financial values must be strings")

var (
	timeType          = reflect.TypeOf(time.Time{})
	jsonMarshalerType = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	textMarshalerType = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
)

// CanonicalJSON encodes v deterministically:
//
//   - object keys are sorted bytewise at every level, whether they come from
//     a map, a struct (json tags respected) or raw JSON text;
//   - no insignificant whitespace and no HTML escaping;
//   - time.Time values (at any depth) render as RFC3339Nano in UTC;
//   - []byte renders as base64, json.RawMessage is parsed and re-encoded;
//   - numbers must be integers with |n| <= MaxSafeInteger. A float64 with a
//     fractional part, an exponent form, or a larger magnitude is rejected
//     with ErrNonCanonicalNumber: financial values must be strings
//     (money.USD and money.Quantity already render as strings). Negative
//     zero is canonicalized to 0.
//
// Values with custom json.Marshaler/encoding.TextMarshaler implementations
// (other than time.Time) are encoded by their own method, then canonicalized
// as JSON text. CanonicalJSON never panics on any input value; encoding
// failures (NaN, cycles, invalid raw JSON, unsupported types) are errors.
func CanonicalJSON(v any) ([]byte, error) {
	normalized, err := normalizeValue(v)
	if err != nil {
		return nil, err
	}
	var encoded bytes.Buffer
	enc := json.NewEncoder(&encoded)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(normalized); err != nil {
		return nil, fmt.Errorf("audit: canonical json: %w", err)
	}
	generic, err := decodeGeneric(encoded.Bytes())
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := writeCanonical(&out, generic); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// normalizeValue returns a copy of v in which every reachable time.Time has
// been converted to UTC. It recovers from reflection panics (which can only
// come from exotic values, never from JSON text) and reports them as errors.
func normalizeValue(v any) (out any, err error) {
	if v == nil {
		return nil, nil
	}
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("audit: canonical json: cannot normalize %T: %v", v, r)
		}
	}()
	rv := normalize(reflect.ValueOf(v))
	if !rv.IsValid() {
		return nil, nil
	}
	return rv.Interface(), nil
}

// normalize deep-copies v, rewriting time.Time values to UTC. Types with
// their own JSON or text marshaller are opaque and copied as-is.
func normalize(v reflect.Value) reflect.Value {
	if !v.IsValid() {
		return v
	}
	t := v.Type()
	if t == timeType {
		if !v.CanInterface() {
			return v
		}
		if tm, ok := v.Interface().(time.Time); ok {
			return reflect.ValueOf(tm.UTC())
		}
		return v
	}
	// Pointers and interfaces are followed before the marshaller check so a
	// *time.Time (whose method set includes MarshalJSON) is still normalized.
	switch t.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		p := reflect.New(t.Elem())
		p.Elem().Set(normalize(v.Elem()))
		return p
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		n := reflect.New(t).Elem()
		n.Set(normalize(v.Elem()))
		return n
	}
	if t.Implements(jsonMarshalerType) || t.Implements(textMarshalerType) {
		return v
	}
	switch t.Kind() {
	case reflect.Struct:
		c := reflect.New(t).Elem()
		c.Set(v)
		for i := range t.NumField() {
			f := c.Field(i)
			if !f.CanSet() { // unexported: encoding/json ignores it too
				continue
			}
			f.Set(normalize(v.Field(i)))
		}
		return c
	case reflect.Slice:
		if v.IsNil() || t.Elem().Kind() == reflect.Uint8 {
			return v
		}
		n := reflect.MakeSlice(t, v.Len(), v.Len())
		for i := range v.Len() {
			n.Index(i).Set(normalize(v.Index(i)))
		}
		return n
	case reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return v
		}
		n := reflect.New(t).Elem()
		for i := range v.Len() {
			n.Index(i).Set(normalize(v.Index(i)))
		}
		return n
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		n := reflect.MakeMapWithSize(t, v.Len())
		for it := v.MapRange(); it.Next(); {
			n.SetMapIndex(it.Key(), normalize(it.Value()))
		}
		return n
	}
	return v
}

// decodeGeneric parses exactly one JSON value into the generic form
// (map[string]any, []any, string, json.Number, bool, nil).
func decodeGeneric(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, fmt.Errorf("audit: canonical json: %w", err)
	}
	if _, err := dec.Token(); err == nil {
		return nil, errors.New("audit: canonical json: trailing data after value")
	}
	return generic, nil
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		buf.WriteString(strconv.FormatBool(x))
	case json.Number:
		s, err := canonicalNumber(x)
		if err != nil {
			return err
		}
		buf.WriteString(s)
	case string:
		writeString(buf, x)
	case []any:
		buf.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeString(buf, k)
			buf.WriteByte(':')
			if err := writeCanonical(buf, x[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("audit: canonical json: unexpected decoded type %T", v)
	}
	return nil
}

// writeString encodes s as a JSON string without HTML escaping.
func writeString(buf *bytes.Buffer, s string) {
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)           // encoding a string cannot fail
	buf.Truncate(buf.Len() - 1) // Encode appends a newline
}

// canonicalNumber accepts only plain integers within MaxSafeInteger and
// renders them in their shortest form ("-0" becomes "0").
func canonicalNumber(n json.Number) (string, error) {
	s := string(n)
	if !isPlainInteger(s) {
		return "", fmt.Errorf("%w: %q", ErrNonCanonicalNumber, s)
	}
	i, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return "", fmt.Errorf("%w: %q", ErrNonCanonicalNumber, s)
	}
	if i > MaxSafeInteger || i < -MaxSafeInteger {
		return "", fmt.Errorf("%w: %q exceeds 2^53", ErrNonCanonicalNumber, s)
	}
	return strconv.FormatInt(i, 10), nil
}

// isPlainInteger matches ^-?(0|[1-9][0-9]*)$.
func isPlainInteger(s string) bool {
	if s == "" {
		return false
	}
	if s[0] == '-' {
		s = s[1:]
	}
	if s == "" {
		return false
	}
	if s[0] == '0' {
		return len(s) == 1
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
