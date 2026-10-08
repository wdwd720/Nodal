package sandboxrt

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"unicode/utf8"
)

// Record types. There are exactly two, and neither can sign, transfer,
// withdraw or name a destination.
const (
	RecordIntent = "intent"
	RecordLog    = "log"
)

// RecordTypes returns every record type the protocol has.
func RecordTypes() []string { return []string{RecordIntent, RecordLog} }

// intentKeys and logKeys are the exact key sets of each record type. A record
// with any other key is malformed, so an extra "destination", "signature" or
// "to_address" field cannot ride along on an otherwise valid record.
var (
	intentRequiredKeys = []string{"type", "seq", "kind", "instrument_id", "side", "quantity"}
	intentOptionalKeys = []string{"limit_price"}
	logRequiredKeys    = []string{"type", "seq", "event"}
	logOptionalKeys    = []string{"fields"}
)

// IntentRecord is a structurally valid intent record. Its field values are
// still untrusted: ValidateProposal decides whether it is a proposal.
type IntentRecord struct {
	Seq          int
	Kind         string
	InstrumentID string
	Side         string
	Quantity     string
	LimitPrice   string
}

// LogRecord is a structurally valid log record.
type LogRecord struct {
	Seq    int               `json:"seq"`
	Event  string            `json:"event"`
	Fields map[string]string `json:"fields"`
}

// ParsedOutput is the structural reading of a run's stdout.
type ParsedOutput struct {
	Intents []IntentRecord
	Logs    []LogRecord
	// Violations are run-level: when there is any, the run is
	// INVALID_OUTPUT and nothing in Intents or Logs may be used.
	Violations []Violation
}

// ParseOutput reads stdout as newline-delimited JSON records. Every line must
// be exactly one JSON object (valid UTF-8, no duplicate keys, no trailing
// data) of type "intent" or "log" with exactly that type's keys, and seq must
// count 1, 2, 3... The output must end with a newline. ParseOutput never
// panics, whatever the bytes.
func ParseOutput(out []byte, maxRecords int) ParsedOutput {
	var p ParsedOutput
	if len(out) == 0 {
		return p
	}
	body := out
	if out[len(out)-1] != '\n' {
		p.Violations = append(p.Violations, Violation{Code: ViolationOutputTruncated, Detail: "output does not end with a newline"})
	} else {
		body = out[:len(out)-1]
	}
	for i, line := range bytes.Split(body, []byte{'\n'}) {
		seq := i + 1
		if seq > maxRecords {
			p.Violations = append(p.Violations, Violation{Code: ViolationOutputTooManyRecords, Seq: seq,
				Detail: fmt.Sprintf("more than %d records", maxRecords)})
			break
		}
		if v := p.parseLine(line, seq); v != nil {
			p.Violations = append(p.Violations, *v)
		}
	}
	return p
}

func (p *ParsedOutput) parseLine(line []byte, seq int) *Violation {
	fields, err := decodeObject(line)
	if err != nil {
		code := ViolationOutputNotJSON
		if errors.Is(err, errDuplicateKey) {
			code = ViolationOutputRecordMalformed
		}
		return &Violation{Code: code, Seq: seq, Detail: truncate(err.Error(), 200)}
	}
	typ, ok := stringField(fields, "type")
	if !ok {
		return &Violation{Code: ViolationOutputRecordMalformed, Seq: seq, Detail: "type must be a string"}
	}
	switch typ {
	case RecordIntent:
		if v := checkKeys(fields, intentRequiredKeys, intentOptionalKeys, seq); v != nil {
			return v
		}
		rec := IntentRecord{Seq: seq}
		var okAll = true
		for _, f := range []struct {
			key string
			dst *string
		}{{"kind", &rec.Kind}, {"instrument_id", &rec.InstrumentID}, {"side", &rec.Side}, {"quantity", &rec.Quantity}} {
			v, ok := stringField(fields, f.key)
			okAll = okAll && ok
			*f.dst = v
		}
		if _, present := fields["limit_price"]; present {
			v, ok := stringField(fields, "limit_price")
			okAll = okAll && ok
			rec.LimitPrice = v
		}
		if !okAll {
			return &Violation{Code: ViolationOutputRecordMalformed, Seq: seq, Detail: "intent fields must be strings"}
		}
		if v := checkSeq(fields, seq); v != nil {
			return v
		}
		p.Intents = append(p.Intents, rec)
	case RecordLog:
		if v := checkKeys(fields, logRequiredKeys, logOptionalKeys, seq); v != nil {
			return v
		}
		event, ok := stringField(fields, "event")
		if !ok {
			return &Violation{Code: ViolationOutputRecordMalformed, Seq: seq, Detail: "event must be a string"}
		}
		rec := LogRecord{Seq: seq, Event: event, Fields: map[string]string{}}
		if raw, present := fields["fields"]; present {
			sub, err := decodeObject(raw)
			if err != nil {
				return &Violation{Code: ViolationOutputRecordMalformed, Seq: seq, Detail: "fields must be an object"}
			}
			for k := range sub {
				v, ok := stringField(sub, k)
				if !ok {
					return &Violation{Code: ViolationOutputRecordMalformed, Seq: seq, Detail: "log field values must be strings"}
				}
				rec.Fields[k] = v
			}
		}
		if v := checkSeq(fields, seq); v != nil {
			return v
		}
		p.Logs = append(p.Logs, rec)
	default:
		return &Violation{Code: ViolationOutputRecordType, Seq: seq, Detail: "unknown record type " + strconv.Quote(truncate(typ, 64))}
	}
	return nil
}

var errDuplicateKey = errors.New("duplicate key")

// decodeObject decodes exactly one JSON object, refusing invalid UTF-8,
// duplicate keys and anything after the closing brace.
func decodeObject(b []byte) (map[string]json.RawMessage, error) {
	if !utf8.Valid(b) {
		return nil, errors.New("not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("not a JSON object")
	}
	out := map[string]json.RawMessage{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := kt.(string)
		if !ok {
			return nil, errors.New("object key is not a string")
		}
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("%w %q", errDuplicateKey, truncate(key, 64))
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		out[key] = raw
	}
	end, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := end.(json.Delim); !ok || d != '}' {
		return nil, errors.New("object not closed")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after the object")
	}
	return out, nil
}

func stringField(fields map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := fields[key]
	if !ok || len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

func checkKeys(fields map[string]json.RawMessage, required, optional []string, seq int) *Violation {
	for _, k := range required {
		if _, ok := fields[k]; !ok {
			return &Violation{Code: ViolationOutputRecordMalformed, Seq: seq, Detail: "missing " + k}
		}
	}
	// Sorted, so the reported field -- and therefore the stored violation --
	// is the same on every run of the same output.
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !containsString(required, k) && !containsString(optional, k) {
			return &Violation{Code: ViolationOutputRecordMalformed, Seq: seq, Detail: "unexpected field " + strconv.Quote(truncate(k, 64))}
		}
	}
	return nil
}

// checkSeq requires the record's seq to be the JSON integer equal to its
// position, so records cannot be reordered, skipped or replayed.
func checkSeq(fields map[string]json.RawMessage, seq int) *Violation {
	raw := fields["seq"]
	n, err := strconv.Atoi(string(raw))
	if err != nil || n != seq || strconv.Itoa(n) != string(raw) {
		return &Violation{Code: ViolationOutputRecordMalformed, Seq: seq, Detail: "seq must be the integer " + strconv.Itoa(seq)}
	}
	return nil
}
