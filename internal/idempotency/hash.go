package idempotency

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"sort"
	"strconv"
	"strings"
)

// HashRequest returns the hex SHA-256 of the canonical request form:
//
//	UPPER(method) "\n" path "\n" CanonicalJSON(body)
//
// so that retries differing only in JSON key order or whitespace hash the same,
// while any semantic difference (values, numbers, array order) hashes
// differently. Non-JSON bodies are hashed byte-for-byte.
func HashRequest(method, path string, body []byte) string {
	h := sha256.New()
	_, _ = io.WriteString(h, strings.ToUpper(strings.TrimSpace(method)))
	h.Write([]byte{'\n'})
	_, _ = io.WriteString(h, path)
	h.Write([]byte{'\n'})
	h.Write(CanonicalJSON(body))
	return hex.EncodeToString(h.Sum(nil))
}

// CanonicalJSON returns body with object keys sorted recursively, no
// insignificant whitespace, numbers kept verbatim and strings encoded without
// HTML escaping. A body that is empty or only whitespace canonicalises to
// nothing; a body that is not exactly one valid JSON value is returned
// unchanged so hashing never fails.
func CanonicalJSON(body []byte) []byte {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return body
	}
	if _, err := dec.Token(); err != io.EOF {
		return body // trailing garbage after the first value
	}
	var buf bytes.Buffer
	if err := writeCanonical(&buf, v); err != nil {
		return body
	}
	return buf.Bytes()
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		buf.WriteString(strconv.FormatBool(x))
	case json.Number:
		buf.WriteString(x.String())
	case string:
		enc := json.NewEncoder(buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(x); err != nil {
			return err
		}
		buf.Truncate(buf.Len() - 1) // Encode appends a newline
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
			if err := writeCanonical(buf, k); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := writeCanonical(buf, x[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return err
		}
		buf.Write(b)
	}
	return nil
}
