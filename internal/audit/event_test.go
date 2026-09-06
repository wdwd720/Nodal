package audit

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
)

var t0 = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

func validEvent() Event {
	return Event{
		Stream:        AccountStream("acct-1"),
		ActorType:     string(security.ActorUser),
		ActorID:       "user-1",
		Action:        "intent.created",
		ResourceType:  "intent",
		ResourceID:    "intent-1",
		BeforeHash:    []byte{1, 2},
		AfterHash:     []byte{3, 4},
		RequestID:     "req-1",
		CorrelationID: "corr-1",
		PolicyVersion: "risk-v3",
		Reason:        "customer request",
		SourceIP:      "10.0.0.1",
		Device:        "web",
		EvidenceRef:   "s3://bucket/key",
		Payload:       json.RawMessage(`{"b":"2","a":"1"}`),
		OccurredAt:    t0,
	}
}

func TestStreams(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "account:abc", AccountStream("abc"))
	assert.Equal(t, "agent:xyz", AgentStream("xyz"))
	assert.Equal(t, "admin", AdminStream)
	assert.Equal(t, "system", SystemStream)
	for _, ok := range []string{AdminStream, SystemStream, AccountStream("a"), AgentStream("b"), "account:01924e5a-7b2c-7d3e-8f4a-5b6c7d8e9f0a"} {
		assert.True(t, ValidStream(ok), ok)
	}
	for _, bad := range []string{"", "account:", "agent:", "account: x", "account:a b", "ADMIN", "other", "account:\x00", "admin:x", "account:a\tb", "account:\xff"} {
		assert.False(t, ValidStream(bad), "%q", bad)
	}
}

func TestEventValidate(t *testing.T) {
	t.Parallel()
	require.NoError(t, validEvent().Validate())

	tests := []struct {
		name  string
		mut   func(*Event)
		field string
	}{
		{"missing stream", func(e *Event) { e.Stream = "" }, "stream"},
		{"bad stream", func(e *Event) { e.Stream = "customer:1" }, "stream"},
		{"missing actor type", func(e *Event) { e.ActorType = "" }, "actor_type"},
		{"undeclared actor type", func(e *Event) { e.ActorType = "ROBOT" }, "actor_type"},
		{"lowercase actor type", func(e *Event) { e.ActorType = "user" }, "actor_type"},
		{"missing actor id", func(e *Event) { e.ActorID = "  " }, "actor_id"},
		{"missing action", func(e *Event) { e.Action = "" }, "action"},
		{"missing resource type", func(e *Event) { e.ResourceType = "" }, "resource_type"},
		{"missing resource id", func(e *Event) { e.ResourceID = "" }, "resource_id"},
		{"missing occurred_at", func(e *Event) { e.OccurredAt = time.Time{} }, "occurred_at"},
		{"invalid payload", func(e *Event) { e.Payload = json.RawMessage(`{"a":`) }, "payload"},
		{"float payload", func(e *Event) { e.Payload = json.RawMessage(`{"amount":10.5}`) }, "payload"},
		{"nul byte in payload", func(e *Event) { e.Payload = json.RawMessage("{\"a\":\"x\x00y\"}") }, "payload"},
		{"nul escape in payload", func(e *Event) { e.Payload = json.RawMessage("{\"a\":\"x\\" + "u0000y\"}") }, "payload"},
		{"nul key in payload", func(e *Event) { e.Payload = json.RawMessage("{\"k\\" + "u0000\":1}") }, "payload"},
		{"nul in reason", func(e *Event) { e.Reason = "a\x00b" }, "reason"},
		{"invalid utf8", func(e *Event) { e.Device = "\xff" }, "device"},
		{"ip with prefix", func(e *Event) { e.SourceIP = "10.0.0.0/24" }, "source_ip"},
		{"ip with zone", func(e *Event) { e.SourceIP = "fe80::1%eth0" }, "source_ip"},
		{"not an ip", func(e *Event) { e.SourceIP = "localhost" }, "source_ip"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := validEvent()
			tt.mut(&e)
			err := e.Validate()
			require.Error(t, err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
			ee, ok := errs.As(err)
			require.True(t, ok)
			assert.Equal(t, tt.field, ee.Fields["field"])
		})
	}
}

func TestEventValidate_AcceptsEveryDeclaredActorTypeIncludingAgent(t *testing.T) {
	t.Parallel()
	for _, at := range security.AllActorTypes() {
		e := validEvent()
		e.ActorType = string(at)
		require.NoError(t, e.Validate(), at)
	}
	// Minimal event: optional fields empty, payload nil.
	e := Event{Stream: SystemStream, ActorType: "SYSTEM", ActorID: "reconciler", Action: "x", ResourceType: "y", ResourceID: "z", OccurredAt: t0}
	require.NoError(t, e.Validate())
}

func TestHashEvent(t *testing.T) {
	t.Parallel()
	e := validEvent()
	h1, err := HashEvent(e, 1, nil, "build-a")
	require.NoError(t, err)
	require.Len(t, h1, 32)

	// Deterministic.
	h1b, err := HashEvent(e, 1, nil, "build-a")
	require.NoError(t, err)
	assert.Equal(t, h1, h1b)

	// Payload key order, whitespace and time zone/precision do not matter.
	e2 := e
	e2.Payload = json.RawMessage(` { "a" : "1" , "b" : "2" } `)
	e2.OccurredAt = t0.In(time.FixedZone("X", 7200)).Add(500 * time.Nanosecond) // sub-microsecond is dropped
	h2, err := HashEvent(e2, 1, nil, "build-a")
	require.NoError(t, err)
	assert.Equal(t, h1, h2)

	// IP text form is canonicalized.
	e3 := e
	e3.SourceIP = "010.0.0.1"
	_, err = HashEvent(e3, 1, nil, "build-a")
	require.Error(t, err, "non-canonical dotted quad is rejected by netip")
	e3.SourceIP = "0:0:0:0:0:0:0:1"
	e4 := e
	e4.SourceIP = "::1"
	h3, err := HashEvent(e3, 1, nil, "build-a")
	require.NoError(t, err)
	h4, err := HashEvent(e4, 1, nil, "build-a")
	require.NoError(t, err)
	assert.Equal(t, h3, h4)

	// Every chain field and the build version are bound into the hash.
	hSeq, err := HashEvent(e, 2, nil, "build-a")
	require.NoError(t, err)
	hPrev, err := HashEvent(e, 1, []byte{9}, "build-a")
	require.NoError(t, err)
	hBuild, err := HashEvent(e, 1, nil, "build-b")
	require.NoError(t, err)
	hMicro, err := HashEvent(func() Event { x := e; x.OccurredAt = t0.Add(time.Microsecond); return x }(), 1, nil, "build-a")
	require.NoError(t, err)
	hReason, err := HashEvent(func() Event { x := e; x.Reason = "other"; return x }(), 1, nil, "build-a")
	require.NoError(t, err)
	hStream, err := HashEvent(func() Event { x := e; x.Stream = AccountStream("acct-2"); return x }(), 1, nil, "build-a")
	require.NoError(t, err)
	seen := map[string]string{}
	for name, h := range map[string][]byte{"base": h1, "seq": hSeq, "prev": hPrev, "build": hBuild, "micro": hMicro, "reason": hReason, "stream": hStream} {
		if prev, dup := seen[string(h)]; dup {
			t.Fatalf("%s and %s hash identically", prev, name)
		}
		seen[string(h)] = name
	}
	// Empty prev and nil prev are the same (first row of a stream).
	hEmptyPrev, err := HashEvent(e, 1, []byte{}, "build-a")
	require.NoError(t, err)
	assert.Equal(t, h1, hEmptyPrev)

	// Invalid events never hash.
	bad := e
	bad.Action = ""
	_, err = HashEvent(bad, 1, nil, "build-a")
	require.Error(t, err)
}

func TestHashRecordShape(t *testing.T) {
	t.Parallel()
	rec, err := validEvent().record("b1")
	require.NoError(t, err)
	rec.StreamSeq = 3
	rec.PrevHash = []byte{0xaa}
	b, err := CanonicalJSON(rec)
	require.NoError(t, err)
	s := string(b)
	// Column names as keys, sorted, chain fields present, no id/recorded_at/content_hash.
	for _, key := range []string{`"stream":"account:acct-1"`, `"stream_seq":3`, `"prev_hash":"qg=="`, `"build_version":"b1"`, `"occurred_at":"2026-09-05T12:00:00Z"`, `"payload":{"a":"1","b":"2"}`, `"source_ip":"10.0.0.1"`} {
		assert.Contains(t, s, key)
	}
	for _, absent := range []string{`"id"`, `"recorded_at"`, `"content_hash"`} {
		assert.NotContains(t, s, absent)
	}
	assert.True(t, strings.HasPrefix(s, `{"action":"intent.created","actor_id":"user-1","actor_type":"USER"`), s)

	// Optional fields are null, not empty strings, so SQL NULL round-trips.
	minimal := Event{Stream: SystemStream, ActorType: "SYSTEM", ActorID: "a", Action: "b", ResourceType: "c", ResourceID: "d", OccurredAt: t0}
	rec, err = minimal.record("")
	require.NoError(t, err)
	b, err = CanonicalJSON(rec)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"reason":null`)
	assert.Contains(t, string(b), `"build_version":null`)
	assert.Contains(t, string(b), `"before_hash":null`)
	assert.Contains(t, string(b), `"payload":{}`)
}
