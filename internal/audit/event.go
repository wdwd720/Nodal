package audit

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/security"
)

type eventKind struct{}

// EventID identifies one audit_events row.
type EventID = id.ID[eventKind]

// NewEventID returns a fresh audit event id.
func NewEventID() EventID { return id.New[eventKind]() }

// ParseEventID parses the canonical form.
func ParseEventID(s string) (EventID, error) { return id.Parse[eventKind](s) }

// Stream names (POLICY_AUTHORITY §6). Every event belongs to exactly one
// stream and streams are hash-chained independently.
const (
	// AdminStream carries gate, kill-switch and admin-action transitions and
	// break-glass use.
	AdminStream = "admin"
	// SystemStream carries background work with no account or agent scope.
	SystemStream = "system"

	accountStreamPrefix = "account:"
	agentStreamPrefix   = "agent:"
)

// AccountStream names the stream of one account.
func AccountStream(accountID string) string { return accountStreamPrefix + accountID }

// AgentStream names the stream of one agent.
func AgentStream(agentID string) string { return agentStreamPrefix + agentID }

// ValidStream reports whether s is one of the four declared stream shapes
// with a non-empty scope identifier.
func ValidStream(s string) bool {
	switch {
	case s == AdminStream, s == SystemStream:
		return true
	case strings.HasPrefix(s, accountStreamPrefix):
		return isPlainToken(s[len(accountStreamPrefix):])
	case strings.HasPrefix(s, agentStreamPrefix):
		return isPlainToken(s[len(agentStreamPrefix):])
	}
	return false
}

// isPlainToken accepts a non-empty identifier without whitespace or control
// characters.
func isPlainToken(s string) bool {
	if s == "" || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r <= ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

// Event is one audit record as supplied by a writer (PART 89). Stream,
// ActorType, ActorID, Action, ResourceType, ResourceID and OccurredAt are
// required; everything else is optional and stored as NULL when empty.
//
// ActorType must be one of the declared security.ActorType values. AGENT
// events are accepted: they are evidence of what an agent did, not authority.
type Event struct {
	Stream       string
	ActorType    string
	ActorID      string
	Action       string
	ResourceType string
	ResourceID   string

	BeforeHash []byte
	AfterHash  []byte

	RequestID     string
	CorrelationID string
	PolicyVersion string
	Reason        string

	SourceIP    string // an IP address (no prefix, no zone); empty when not lawful/relevant
	Device      string
	EvidenceRef string

	Payload    json.RawMessage // JSON value; nil means {}
	OccurredAt time.Time
}

// Appended describes the row Append inserted.
type Appended struct {
	ID          EventID
	Stream      string
	StreamSeq   int64
	ContentHash []byte
	PrevHash    []byte // nil for the first event of a stream
}

// Validate reports the first reason e cannot be appended. The error carries
// errs.CodeValidationFailed with the offending field.
func (e Event) Validate() error {
	_, err := e.record("")
	return err
}

// HashEvent computes the content hash e would receive at stream_seq seq
// following prevHash, stamped with buildVersion. It is what Append stores
// and what the Verifier recomputes; it is exported so external tooling and
// test doubles can reproduce the chain exactly.
func HashEvent(e Event, seq int64, prevHash []byte, buildVersion string) ([]byte, error) {
	rec, err := e.record(buildVersion)
	if err != nil {
		return nil, err
	}
	rec.StreamSeq = seq
	rec.PrevHash = nilIfEmpty(prevHash)
	return rec.hash()
}

// record validates e and produces the persisted/hashed projection without
// the chain fields (StreamSeq, PrevHash), which Append fills under the lock.
func (e Event) record(buildVersion string) (hashRecord, error) {
	invalid := func(field, detail string) (hashRecord, error) {
		return hashRecord{}, errs.Newf(errs.CodeValidationFailed, "audit: %s %s", field, detail).WithField("field", field)
	}
	if !ValidStream(e.Stream) {
		return invalid("stream", "must be admin, system, account:<id> or agent:<id>")
	}
	if !security.ActorType(e.ActorType).Valid() {
		return invalid("actor_type", "is not a declared actor type")
	}
	required := []struct{ name, value string }{
		{"actor_id", e.ActorID}, {"action", e.Action}, {"resource_type", e.ResourceType}, {"resource_id", e.ResourceID},
	}
	for _, f := range required {
		if strings.TrimSpace(f.value) == "" {
			return invalid(f.name, "is required")
		}
	}
	if e.OccurredAt.IsZero() {
		return invalid("occurred_at", "is required")
	}
	texts := []struct{ name, value string }{
		{"stream", e.Stream},
		{"actor_id", e.ActorID},
		{"action", e.Action},
		{"resource_type", e.ResourceType},
		{"resource_id", e.ResourceID},
		{"request_id", e.RequestID},
		{"correlation_id", e.CorrelationID},
		{"policy_version", e.PolicyVersion},
		{"reason", e.Reason},
		{"device", e.Device},
		{"evidence_ref", e.EvidenceRef},
		{"build_version", buildVersion},
	}
	for _, f := range texts {
		if !validText(f.value) {
			return invalid(f.name, "must be valid UTF-8 without NUL")
		}
	}
	payload := e.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	canonical, err := CanonicalJSON(payload)
	if err != nil {
		return hashRecord{}, errs.Wrap(err, errs.CodeValidationFailed, "audit: payload is not canonical JSON").WithField("field", "payload")
	}
	if payloadContainsNUL(canonical) {
		return invalid("payload", "must not contain NUL (jsonb cannot store it)")
	}
	var sourceIP *string
	if e.SourceIP != "" {
		addr, err := netip.ParseAddr(e.SourceIP)
		if err != nil || addr.Zone() != "" {
			return invalid("source_ip", "must be a plain IP address")
		}
		sourceIP = optional(addr.String())
	}
	return hashRecord{
		Stream:        e.Stream,
		ActorType:     e.ActorType,
		ActorID:       e.ActorID,
		Action:        e.Action,
		ResourceType:  e.ResourceType,
		ResourceID:    e.ResourceID,
		BeforeHash:    nilIfEmpty(e.BeforeHash),
		AfterHash:     nilIfEmpty(e.AfterHash),
		RequestID:     optional(e.RequestID),
		CorrelationID: optional(e.CorrelationID),
		PolicyVersion: optional(e.PolicyVersion),
		Reason:        optional(e.Reason),
		SourceIP:      sourceIP,
		Device:        optional(e.Device),
		EvidenceRef:   optional(e.EvidenceRef),
		BuildVersion:  optional(buildVersion),
		Payload:       canonical,
		OccurredAt:    e.OccurredAt.UTC().Truncate(time.Microsecond),
	}, nil
}

// hashRecord is the canonical projection of an audit_events row: every
// persisted column except id, recorded_at and content_hash. Its JSON keys are
// the column names, so an external verifier can rebuild it from SQL alone.
//
// occurred_at is truncated to microseconds (the timestamptz precision) and
// source_ip is the canonical textual address, so the hash computed before
// the insert equals the hash recomputed from the row.
type hashRecord struct {
	Stream        string          `json:"stream"`
	StreamSeq     int64           `json:"stream_seq"`
	ActorType     string          `json:"actor_type"`
	ActorID       string          `json:"actor_id"`
	Action        string          `json:"action"`
	ResourceType  string          `json:"resource_type"`
	ResourceID    string          `json:"resource_id"`
	BeforeHash    []byte          `json:"before_hash"`
	AfterHash     []byte          `json:"after_hash"`
	RequestID     *string         `json:"request_id"`
	CorrelationID *string         `json:"correlation_id"`
	PolicyVersion *string         `json:"policy_version"`
	Reason        *string         `json:"reason"`
	SourceIP      *string         `json:"source_ip"`
	Device        *string         `json:"device"`
	EvidenceRef   *string         `json:"evidence_ref"`
	BuildVersion  *string         `json:"build_version"`
	Payload       json.RawMessage `json:"payload"`
	OccurredAt    time.Time       `json:"occurred_at"`
	PrevHash      []byte          `json:"prev_hash"`
}

// hash returns sha256(CanonicalJSON(r)).
func (r hashRecord) hash() ([]byte, error) {
	b, err := CanonicalJSON(r)
	if err != nil {
		return nil, fmt.Errorf("audit: hash record: %w", err)
	}
	sum := sha256.Sum256(b)
	return sum[:], nil
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nilIfEmpty(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

func validText(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

// payloadContainsNUL reports whether any string (key or value) in the
// canonical JSON text contains U+0000, which PostgreSQL jsonb rejects.
func payloadContainsNUL(canonical []byte) bool {
	generic, err := decodeGeneric(canonical)
	if err != nil {
		return true // cannot happen for canonical output; fail closed
	}
	var walk func(v any) bool
	walk = func(v any) bool {
		switch x := v.(type) {
		case string:
			return strings.ContainsRune(x, 0)
		case []any:
			for _, e := range x {
				if walk(e) {
					return true
				}
			}
		case map[string]any:
			for k, e := range x {
				if strings.ContainsRune(k, 0) || walk(e) {
					return true
				}
			}
		}
		return false
	}
	return walk(generic)
}
