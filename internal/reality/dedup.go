package reality

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"unicode/utf8"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
)

// MaxDedupIDLength bounds dedup ids (they are Redpanda keys and ClickHouse
// sort keys). It equals event.MaxDedupKeyLength so outbox and stream agree.
const MaxDedupIDLength = event.MaxDedupKeyLength

// DedupID computes the canonical dedup id of a source event (PART 199).
//
//   - PROVIDER_ID: providerEventID is used verbatim and must be non-empty.
//   - COMPOSITE_HASH: sha256(source ‖ event_type ‖ canonical(identifying))
//     where identifying is the source-specific set of fields defined by the
//     normalizer; providerEventID is ignored.
func DedupID(strategy, providerEventID, source, eventType string, identifying map[string]string) (string, error) {
	switch strategy {
	case DedupProviderID:
		if providerEventID == "" {
			return "", errs.New(errs.CodeValidationFailed, "reality: PROVIDER_ID dedup requires a provider event id")
		}
		if err := validateDedupID(providerEventID); err != nil {
			return "", err
		}
		return providerEventID, nil
	case DedupCompositeHash:
		if source == "" || eventType == "" {
			return "", errs.New(errs.CodeValidationFailed, "reality: composite dedup requires source and event type")
		}
		if len(identifying) == 0 {
			return "", errs.New(errs.CodeValidationFailed, "reality: composite dedup requires identifying fields")
		}
		return CompositeHash(source, eventType, identifying), nil
	}
	return "", errs.New(errs.CodeValidationFailed, "reality: unknown dedup strategy").WithField("strategy", strategy)
}

// CompositeHash is the COMPOSITE_HASH dedup id: lower-case hex sha256 over
// source, event type and the canonical JSON (sorted keys, no HTML escaping)
// of the identifying fields, separated by 0x1f so no field can collide with
// a neighbor.
func CompositeHash(source, eventType string, identifying map[string]string) string {
	h := sha256.New()
	h.Write([]byte(source))
	h.Write([]byte{0x1f})
	h.Write([]byte(eventType))
	h.Write([]byte{0x1f})
	h.Write(canonicalFields(identifying))
	return hex.EncodeToString(h.Sum(nil))
}

// canonicalFields renders a string map deterministically.
func canonicalFields(m map[string]string) []byte {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		_ = enc.Encode(k) // encoding a string never fails
		buf.Truncate(buf.Len() - 1)
		buf.WriteByte(':')
		_ = enc.Encode(m[k])
		buf.Truncate(buf.Len() - 1)
	}
	buf.WriteByte('}')
	return buf.Bytes()
}

func validateDedupID(s string) error {
	switch {
	case s == "":
		return errs.New(errs.CodeValidationFailed, "reality: dedup id is required")
	case len(s) > MaxDedupIDLength:
		return errs.New(errs.CodeValidationFailed, "reality: dedup id too long")
	case !utf8.ValidString(s):
		return errs.New(errs.CodeValidationFailed, "reality: dedup id must be valid utf-8")
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return errs.New(errs.CodeValidationFailed, "reality: dedup id contains control characters")
		}
	}
	return nil
}

// Dedup collapses events with the same (source, event_type, dedup_id),
// keeping the first occurrence in order. It is what every consumer of the
// stream must do (PART 199) and what ClickHouse's ReplacingMergeTree does
// at merge time.
func Dedup(events []NormalizedEvent) []NormalizedEvent {
	type key struct{ source, eventType, dedup string }
	seen := make(map[key]struct{}, len(events))
	out := make([]NormalizedEvent, 0, len(events))
	for _, e := range events {
		k := key{e.Source, e.EventType, e.DedupID}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, e)
	}
	return out
}
