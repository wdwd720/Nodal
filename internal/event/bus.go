package event

import (
	"context"
	"time"
)

// Standard message header keys set by the relay on every published message.
// Producer-supplied Envelope.Headers are copied first; these keys always win.
const (
	HeaderEventID       = "event_id"
	HeaderEventType     = "event_type"
	HeaderSchemaVersion = "schema_version"
	HeaderSource        = "source"
	HeaderAggregateType = "aggregate_type"
	HeaderAggregateID   = "aggregate_id"
	HeaderCorrelationID = "correlation_id"
	HeaderCausationID   = "causation_id"
	HeaderOccurredAt    = "occurred_at"
	HeaderRecordedAt    = "recorded_at"
	HeaderContentType   = "content_type"

	// ContentTypeJSON is the value of HeaderContentType for canonical
	// envelopes.
	ContentTypeJSON = "application/json"
)

// Message is what a Bus delivers to a Handler. ID is the event id (header
// event_id) and is what consumers dedup on through Inbox.Process. Value is
// the canonical envelope JSON; Envelope decodes it.
type Message struct {
	ID      string
	Topic   string
	Key     string
	Value   []byte
	Headers map[string]string
}

// Envelope decodes and validates Value.
func (m Message) Envelope() (Envelope, error) { return Decode(m.Value) }

// Handler consumes one message. Returning an error signals the bus to
// redeliver (at-least-once); handlers must therefore be idempotent, which
// Inbox.Process guarantees when every side effect runs inside it.
type Handler func(ctx context.Context, m Message) error

// Bus is the transport abstraction (CONVENTIONS.md). Production wires
// Redpanda; LOCAL/TEST/DEV may use eventtest.MemoryBus. Publish must not
// return before the transport has acknowledged the message, because the
// relay marks the outbox row published when Publish returns nil.
type Bus interface {
	Publish(ctx context.Context, topic, key string, value []byte, headers map[string]string) error
	Subscribe(ctx context.Context, topic, group string, h Handler) error
	Close(ctx context.Context) error
}

// Observer receives metrics hooks. Implementations must be safe for
// concurrent use and must not block; internal/observability wires the real
// instruments so this package never imports it.
type Observer interface {
	// OnPublished is called after the bus acknowledged an event. attempts is
	// the total number of publish attempts including the successful one;
	// lag is now minus recorded_at.
	OnPublished(topic string, attempts int, lag time.Duration)
	// OnPublishFailed is called when the bus rejected an event. attempts
	// counts the failed attempt.
	OnPublishFailed(topic string, attempts int, err error)
	// OnDuplicate is called when Inbox.Process sees an already processed
	// message for source.
	OnDuplicate(source string)
}

// NopObserver is the Observer used when none is configured.
type NopObserver struct{}

func (NopObserver) OnPublished(string, int, time.Duration) {}
func (NopObserver) OnPublishFailed(string, int, error)     {}
func (NopObserver) OnDuplicate(string)                     {}

var _ Observer = NopObserver{}
