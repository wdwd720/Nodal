package stream

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/security"
)

// Type is the client-facing event type (matches the OpenAPI StreamEvent enum).
type Type string

// Client-facing event types.
const (
	TypeBuyingPowerChanged  Type = "buying_power.changed"
	TypeOrderTransitioned   Type = "order.transitioned"
	TypeIntentTransitioned  Type = "intent.transitioned"
	TypeDepositTransitioned Type = "deposit.transitioned"
	TypeAgentState          Type = "agent.state"
	TypeResync              Type = "resync"
)

// topicTypes maps bus topics to client types; topics not listed are not streamed.
var topicTypes = map[event.Topic]Type{
	event.TopicLedgerTransactionPosted:      TypeBuyingPowerChanged,
	event.TopicCapitalReservationCreated:    TypeBuyingPowerChanged,
	event.TopicCapitalReservationConsumed:   TypeBuyingPowerChanged,
	event.TopicCapitalReservationReleased:   TypeBuyingPowerChanged,
	event.TopicCapitalReservationExpired:    TypeBuyingPowerChanged,
	event.TopicCapitalEnvelopeChanged:       TypeAgentState,
	event.TopicFundingDepositTransitioned:   TypeDepositTransitioned,
	event.TopicIntentTransitioned:           TypeIntentTransitioned,
	event.TopicOrderTransitioned:            TypeOrderTransitioned,
	event.TopicExecutionAttemptTransitioned: TypeOrderTransitioned,
	event.TopicFillObserved:                 TypeOrderTransitioned,
	event.TopicAgentRunCompleted:            TypeAgentState,
}

// StreamedTopics lists the bus topics the hub subscribes to.
func StreamedTopics() []event.Topic {
	out := make([]event.Topic, 0, len(topicTypes))
	for t := range topicTypes {
		out = append(out, t)
	}
	return out
}

// Event is one client-facing stream event.
type Event struct {
	ID         uint64          `json:"-"`
	Type       Type            `json:"type"`
	OccurredAt time.Time       `json:"occurred_at"`
	ResourceID string          `json:"resource_id,omitempty"`
	AccountID  string          `json:"account_id,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
}

// visibleTo reports whether p may receive e.
func (e Event) visibleTo(p security.Principal) bool {
	if e.Type == TypeResync {
		return true
	}
	if p.ActorType == security.ActorOperator && p.Has(security.PermAccountReadAny, time.Now()) {
		return true
	}
	if e.AccountID == "" {
		return false
	}
	for _, a := range p.AccountIDs {
		if a == e.AccountID {
			return true
		}
	}
	return false
}

// Hub fans bus events out to subscribers with a bounded replay buffer.
type Hub struct {
	mu       sync.Mutex
	next     uint64
	buffer   []Event // ring of the last capacity events, oldest first
	capacity int
	subs     map[*Subscriber]struct{}
	log      *slog.Logger
}

type Subscriber struct {
	principal security.Principal
	ch        chan Event
}

// NewHub returns a hub retaining up to capacity events for resume.
func NewHub(capacity int, log *slog.Logger) *Hub {
	if capacity <= 0 {
		capacity = 4096
	}
	if log == nil {
		log = slog.Default()
	}
	return &Hub{capacity: capacity, subs: map[*Subscriber]struct{}{}, log: log}
}

// Attach subscribes the hub to every streamed topic on the bus. group must be
// unique per process so each replica receives every event.
func (h *Hub) Attach(ctx context.Context, bus event.Bus, group string) error {
	for _, topic := range StreamedTopics() {
		if err := bus.Subscribe(ctx, string(topic), group, h.handle); err != nil {
			return err
		}
	}
	return nil
}

// handle converts a bus message into a stream event and publishes it.
func (h *Hub) handle(_ context.Context, m event.Message) error {
	env, err := event.Decode(m.Value)
	if err != nil {
		h.log.Warn("stream: undecodable event dropped", "topic", m.Topic, "err", err)
		return nil // never block the consumer on a bad message; the outbox row stays published
	}
	typ, ok := topicTypes[event.Topic(env.Type)]
	if !ok {
		if typ, ok = topicTypes[event.Topic(m.Topic)]; !ok {
			return nil
		}
	}
	h.Publish(Event{
		Type:       typ,
		OccurredAt: env.OccurredAt,
		ResourceID: env.AggregateID,
		AccountID:  accountIDOf(env),
		Data:       summarize(env),
	})
	return nil
}

// accountIDOf extracts the account scope from the payload or headers.
func accountIDOf(env event.Envelope) string {
	if v := env.Headers["account_id"]; v != "" {
		return v
	}
	if env.AggregateType == "account" {
		return env.AggregateID
	}
	var probe struct {
		AccountID string `json:"account_id"`
	}
	if len(env.Payload) > 0 && json.Unmarshal(env.Payload, &probe) == nil {
		return probe.AccountID
	}
	return ""
}

// summarize keeps only identifiers and state names from the payload; the
// client refetches full resources over REST.
func summarize(env event.Envelope) json.RawMessage {
	var m map[string]any
	if len(env.Payload) == 0 || json.Unmarshal(env.Payload, &m) != nil {
		return nil
	}
	keep := map[string]any{}
	for _, k := range []string{"id", "intent_id", "order_id", "deposit_id", "agent_id", "status", "from", "to", "from_status", "to_status", "state", "kind", "reason_code", "correlation_id"} {
		if v, ok := m[k]; ok {
			keep[k] = v
		}
	}
	keep["event_id"] = env.ID
	keep["event_type"] = env.Type
	b, err := json.Marshal(keep)
	if err != nil {
		return nil
	}
	return b
}

// Publish assigns the next id, records e in the replay buffer, and delivers
// it to every subscriber that may see it. Slow subscribers are dropped after
// a short wait so one stalled client cannot back-pressure the bus consumer.
func (h *Hub) Publish(e Event) Event {
	h.mu.Lock()
	h.next++
	e.ID = h.next
	if len(h.buffer) == h.capacity {
		copy(h.buffer, h.buffer[1:])
		h.buffer[len(h.buffer)-1] = e
	} else {
		h.buffer = append(h.buffer, e)
	}
	subs := make([]*Subscriber, 0, len(h.subs))
	for s := range h.subs {
		subs = append(subs, s)
	}
	h.mu.Unlock()
	for _, s := range subs {
		if !e.visibleTo(s.principal) {
			continue
		}
		select {
		case s.ch <- e:
		case <-time.After(50 * time.Millisecond):
			h.Unsubscribe(s)
		}
	}
	return e
}

// Subscribe registers a subscriber. Events with id > afterID still in the
// buffer are replayed first; if afterID predates the buffer, a resync event
// is delivered first and replay starts from the oldest retained event.
func (h *Hub) Subscribe(p security.Principal, afterID uint64, bufferSize int) (*Subscriber, []Event) {
	if bufferSize <= 0 {
		bufferSize = 256
	}
	s := &Subscriber{principal: p, ch: make(chan Event, bufferSize)}
	h.mu.Lock()
	defer h.mu.Unlock()
	var replay []Event
	if afterID > 0 {
		oldest := uint64(0)
		if len(h.buffer) > 0 {
			oldest = h.buffer[0].ID
		}
		if len(h.buffer) == 0 && afterID < h.next || afterID+1 < oldest {
			replay = append(replay, Event{ID: afterID, Type: TypeResync, OccurredAt: time.Now().UTC()})
		}
		for _, e := range h.buffer {
			if e.ID > afterID && e.visibleTo(p) {
				replay = append(replay, e)
			}
		}
	}
	h.subs[s] = struct{}{}
	return s, replay
}

// Unsubscribe removes a subscriber and closes its channel.
func (h *Hub) Unsubscribe(s *Subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[s]; ok {
		delete(h.subs, s)
		close(s.ch)
	}
}

// Events returns the subscriber's channel.
func (s *Subscriber) Events() <-chan Event { return s.ch }

// ParseLastEventID parses the SSE resume cursor.
func ParseLastEventID(s string) (uint64, error) {
	if s == "" {
		return 0, nil
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, errors.New("stream: invalid Last-Event-ID")
	}
	return v, nil
}
