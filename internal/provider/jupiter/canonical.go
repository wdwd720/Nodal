package jupiter

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"unicode"
)

// canonicalJSON re-encodes raw JSON text deterministically: object keys
// sorted bytewise at every level, no insignificant whitespace, no HTML
// escaping, and every number token preserved verbatim (route plans carry
// informational non-integer numbers such as usdValue, which are never
// interpreted, only hashed). It never panics on any input; malformed JSON
// is an error.
func canonicalJSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, fmt.Errorf("jupiter: canonical json: %w", err)
	}
	rest, _ := io.ReadAll(dec.Buffered())
	if len(bytes.TrimSpace(rest)) != 0 || dec.InputOffset() < int64(len(bytes.TrimRightFunc(raw, unicode.IsSpace))) {
		return nil, errors.New("jupiter: canonical json: trailing data after value")
	}
	var out bytes.Buffer
	if err := writeCanonical(&out, generic); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if x {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case json.Number:
		buf.WriteString(string(x))
	case string:
		writeJSONString(buf, x)
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
			writeJSONString(buf, k)
			buf.WriteByte(':')
			if err := writeCanonical(buf, x[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("jupiter: canonical json: unexpected decoded type %T", v)
	}
	return nil
}

func writeJSONString(buf *bytes.Buffer, s string) {
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)           // encoding a string cannot fail
	buf.Truncate(buf.Len() - 1) // Encode appends a newline
}

// sha256Of returns the SHA-256 of b as a fresh slice.
func sha256Of(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// routeSummary canonicalizes a raw routePlan and returns the canonical
// bytes and their hash. An absent route plan yields "[]" so that the hash
// is always defined.
func routeSummary(raw json.RawMessage) (json.RawMessage, []byte, error) {
	if isNull(raw) {
		raw = json.RawMessage("[]")
	}
	canon, err := canonicalJSON(raw)
	if err != nil {
		return nil, nil, err
	}
	return json.RawMessage(canon), sha256Of(canon), nil
}
