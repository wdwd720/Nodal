package event

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
)

// eventKind is the phantom kind of EventID. Envelope carries the string form
// so the contract in CONVENTIONS.md stays free of generics.
type eventKind struct{}

// EventID identifies one Envelope. It is a UUIDv7 so ids sort by creation
// time, which the relay uses as the tie-breaker within a recorded_at.
type EventID = id.ID[eventKind]

// NewEventID mints a fresh EventID.
func NewEventID() EventID { return id.New[eventKind]() }

// ParseEventID parses the canonical 36-character form.
func ParseEventID(s string) (EventID, error) { return id.Parse[eventKind](s) }

// Size limits enforced by Validate. They protect the outbox, the bus and
// consumers from unbounded input; they are not tunable per deployment.
const (
	MaxTypeLength      = 128
	MaxSourceLength    = 128
	MaxAggregateLength = 256
	MaxReferenceLength = 256
	MaxDedupKeyLength  = 512
	MaxHeaders         = 64
	MaxHeaderKeyLength = 128
	MaxHeaderValLength = 1024
	MaxPayloadBytes    = 1 << 20
)

// typePattern is the grammar of event types and topics.
var typePattern = regexp.MustCompile(`^[a-z0-9_.]+$`)

// ErrNULByte is returned when a payload, header or identifier contains U+0000,
// which jsonb cannot store.
var ErrNULByte = errors.New("event: NUL byte is not allowed")

// Envelope is the canonical domain event (CONVENTIONS.md, internal/event).
//
// Times are UTC with nanosecond precision. OccurredAt is when the fact became
// true in the domain; RecordedAt is when the outbox row was written (filled
// by Outbox.Enqueue when zero). Payload is the event body as JSON and is
// never interpreted by this package. DedupKey is optional producer-side
// de-duplication within a topic (PART 199): the outbox enforces it with a
// unique index.
type Envelope struct {
	ID            string            `json:"id"`
	Type          string            `json:"type"`
	SchemaVersion int               `json:"schema_version"`
	Source        string            `json:"source"`
	AggregateType string            `json:"aggregate_type"`
	AggregateID   string            `json:"aggregate_id"`
	CorrelationID string            `json:"correlation_id,omitempty"`
	CausationID   string            `json:"causation_id,omitempty"`
	OccurredAt    time.Time         `json:"occurred_at"`
	RecordedAt    time.Time         `json:"recorded_at,omitzero"`
	DedupKey      string            `json:"dedup_key,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
	Payload       json.RawMessage   `json:"payload"`
}

// Validate checks the envelope contract and returns a VALIDATION_FAILED
// *errs.Error whose Fields map each offending field to a message. It never
// panics on any input.
func (e Envelope) Validate() error {
	fields := map[string]any{}
	fail := func(k, msg string) { fields[k] = msg }

	if e.ID == "" {
		fail("id", "required")
	} else if eid, err := ParseEventID(e.ID); err != nil || eid.IsZero() {
		fail("id", "must be a canonical uuidv7")
	}
	switch {
	case e.Type == "":
		fail("type", "required")
	case len(e.Type) > MaxTypeLength:
		fail("type", "too long")
	case !typePattern.MatchString(e.Type):
		fail("type", "must match ^[a-z0-9_.]+$")
	}
	if e.SchemaVersion < 1 {
		fail("schema_version", "must be >= 1")
	}
	checkText(fail, "source", e.Source, true, MaxSourceLength)
	checkText(fail, "aggregate_type", e.AggregateType, true, MaxAggregateLength)
	checkText(fail, "aggregate_id", e.AggregateID, true, MaxAggregateLength)
	checkText(fail, "correlation_id", e.CorrelationID, false, MaxReferenceLength)
	checkText(fail, "causation_id", e.CausationID, false, MaxReferenceLength)
	checkText(fail, "dedup_key", e.DedupKey, false, MaxDedupKeyLength)
	checkTime(fail, "occurred_at", e.OccurredAt, true)
	checkTime(fail, "recorded_at", e.RecordedAt, false)

	if len(e.Headers) > MaxHeaders {
		fail("headers", "too many headers")
	}
	for k, v := range e.Headers {
		switch {
		case k == "" || len(k) > MaxHeaderKeyLength || !isClean(k):
			fail("headers", "invalid header key")
		case len(v) > MaxHeaderValLength || !isClean(v):
			fail("headers", "invalid header value")
		}
	}

	switch {
	case len(e.Payload) == 0:
		fail("payload", "required")
	case len(e.Payload) > MaxPayloadBytes:
		fail("payload", "too large")
	default:
		if _, err := canonicalPayload(e.Payload); err != nil {
			fail("payload", err.Error())
		}
	}

	if len(fields) == 0 {
		return nil
	}
	return errs.New(errs.CodeValidationFailed, "event: invalid envelope").WithFields(fields)
}

func checkText(fail func(k, msg string), k, v string, required bool, maxLen int) {
	switch {
	case v == "":
		if required {
			fail(k, "required")
		}
	case len(v) > maxLen:
		fail(k, "too long")
	case !isClean(v):
		fail(k, "must be valid utf-8 without control characters")
	}
}

func checkTime(fail func(k, msg string), k string, t time.Time, required bool) {
	switch {
	case t.IsZero():
		if required {
			fail(k, "required")
		}
	case t.Location() != time.UTC:
		fail(k, "must be UTC")
	}
}

// isClean reports valid UTF-8 without control characters.
func isClean(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// CanonicalBytes returns the deterministic JSON form of the envelope: object
// keys sorted, no insignificant whitespace, HTML characters unescaped, times
// as RFC3339Nano UTC, and the payload canonicalised recursively. Optional
// string fields are omitted when empty; headers are always present. The
// result is what the relay publishes and what Hash covers.
func (e Envelope) CanonicalBytes() ([]byte, error) {
	payload, err := decodePayload(e.Payload)
	if err != nil {
		return nil, err
	}
	obj := map[string]any{
		"id":             e.ID,
		"type":           e.Type,
		"schema_version": e.SchemaVersion,
		"source":         e.Source,
		"aggregate_type": e.AggregateType,
		"aggregate_id":   e.AggregateID,
		"occurred_at":    formatTime(e.OccurredAt),
		"headers":        headersOrEmpty(e.Headers),
		"payload":        payload,
	}
	if e.CorrelationID != "" {
		obj["correlation_id"] = e.CorrelationID
	}
	if e.CausationID != "" {
		obj["causation_id"] = e.CausationID
	}
	if e.DedupKey != "" {
		obj["dedup_key"] = e.DedupKey
	}
	if !e.RecordedAt.IsZero() {
		obj["recorded_at"] = formatTime(e.RecordedAt)
	}
	var buf bytes.Buffer
	if err := writeCanonical(&buf, obj); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// CanonicalPayload returns the canonical JSON of Payload alone. It is what
// Outbox stores in outbox_events.payload.
func (e Envelope) CanonicalPayload() ([]byte, error) {
	return canonicalPayload(e.Payload)
}

// Hash returns the lowercase hex SHA-256 of CanonicalBytes.
func (e Envelope) Hash() (string, error) {
	b, err := e.CanonicalBytes()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Decode parses the JSON produced by CanonicalBytes (or any JSON object with
// the same keys), normalises times to UTC and validates the result. It never
// panics on any input.
func Decode(value []byte) (Envelope, error) {
	var e Envelope
	if len(value) > MaxPayloadBytes+64*1024 {
		return Envelope{}, errs.New(errs.CodeValidationFailed, "event: message too large")
	}
	dec := json.NewDecoder(bytes.NewReader(value))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		return Envelope{}, errs.Wrap(err, errs.CodeValidationFailed, "event: malformed envelope json")
	}
	if dec.More() {
		return Envelope{}, errs.New(errs.CodeValidationFailed, "event: trailing data after envelope")
	}
	if !e.OccurredAt.IsZero() {
		e.OccurredAt = e.OccurredAt.UTC()
	}
	if !e.RecordedAt.IsZero() {
		e.RecordedAt = e.RecordedAt.UTC()
	}
	if len(e.Headers) == 0 {
		e.Headers = nil
	}
	if err := e.Validate(); err != nil {
		return Envelope{}, err
	}
	return e, nil
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func headersOrEmpty(h map[string]string) map[string]string {
	if h == nil {
		return map[string]string{}
	}
	return h
}

// decodePayload parses the payload into the generic form used by the
// canonical writer, preserving number text and rejecting non-objects and
// NUL bytes.
func decodePayload(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, errors.New("event: payload is empty")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("event: payload is not valid json: %w", err)
	}
	if dec.More() {
		return nil, errors.New("event: payload has trailing data")
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, errors.New("event: payload must be a json object")
	}
	return v, nil
}

func canonicalPayload(raw json.RawMessage) ([]byte, error) {
	v, err := decodePayload(raw)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := writeCanonical(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeCanonical writes v as canonical JSON. Supported values are the ones
// encoding/json produces with UseNumber plus the scalar Go types the
// envelope itself uses.
func writeCanonical(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		buf.WriteString(strconv.FormatBool(x))
	case json.Number:
		s := string(x)
		if !isJSONNumber(s) {
			return fmt.Errorf("event: invalid json number %q", s)
		}
		buf.WriteString(s)
	case int:
		buf.WriteString(strconv.Itoa(x))
	case string:
		return writeJSONString(buf, x)
	case map[string]string:
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
			if err := writeJSONString(buf, k); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := writeJSONString(buf, x[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
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
			if err := writeJSONString(buf, k); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := writeCanonical(buf, x[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case []any:
		buf.WriteByte('[')
		for i, el := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, el); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	default:
		return fmt.Errorf("event: unsupported canonical value %T", v)
	}
	return nil
}

// writeJSONString encodes s as a JSON string without HTML escaping. NUL is
// rejected because jsonb cannot store it; invalid UTF-8 is replaced with
// U+FFFD exactly as encoding/json does.
func writeJSONString(buf *bytes.Buffer, s string) error {
	if strings.IndexByte(s, 0) >= 0 {
		return ErrNULByte
	}
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return err
	}
	buf.Truncate(buf.Len() - 1) // Encode appends a newline
	return nil
}

// isJSONNumber validates the JSON number grammar for a json.Number that may
// have been constructed by hand.
func isJSONNumber(s string) bool {
	if s == "" {
		return false
	}
	i := 0
	if s[i] == '-' {
		i++
	}
	if i >= len(s) {
		return false
	}
	switch {
	case s[i] == '0':
		i++
	case s[i] >= '1' && s[i] <= '9':
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	default:
		return false
	}
	if i < len(s) && s[i] == '.' {
		i++
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == start {
			return false
		}
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == start {
			return false
		}
	}
	return i == len(s)
}
