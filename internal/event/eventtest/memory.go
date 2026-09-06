package eventtest

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"sort"
	"sync"

	"github.com/nodal/controlplane/internal/event"
)

// DefaultMaxRedeliveries is how many times a failing handler is retried
// before the message is parked in Failures.
const DefaultMaxRedeliveries = 3

var (
	// ErrNotAllowed is returned by NewMemoryBus outside LOCAL/TEST/DEV.
	ErrNotAllowed = errors.New("eventtest: in-memory bus is only allowed in LOCAL, TEST or DEV")
	// ErrClosed is returned by Publish and Subscribe after Close.
	ErrClosed = errors.New("eventtest: bus is closed")
	// ErrInjectedPublishFailure is the error returned by an injected publish
	// failure (see FailPublishFor).
	ErrInjectedPublishFailure = errors.New("eventtest: injected publish failure")
)

// Allowed reports whether env permits the in-memory bus. The match is exact
// and case-sensitive.
func Allowed(env string) bool {
	switch env {
	case "LOCAL", "TEST", "DEV":
		return true
	}
	return false
}

// Failure is a message a group's handler kept rejecting.
type Failure struct {
	Group   string
	Message event.Message
	Err     error
}

// MemoryBus is an in-memory event.Bus. Delivery is synchronous: Publish
// returns after the message (and anything published from handlers during
// delivery) has been handed to every group, so tests need no waiting. A
// handler runs without any bus lock held and may publish.
type MemoryBus struct {
	env string

	mu              sync.Mutex
	closed          bool
	topics          map[string]*memTopic
	draining        bool
	seq             uint64
	published       int
	dupEvery        int
	reorder         int
	failPublish     map[string]int
	maxRedeliveries int
	failures        []Failure
	deliveries      map[string]int // "topic\x00group" -> count
}

type memTopic struct {
	log    []event.Message
	groups map[string]*memGroup
}

type memGroup struct {
	name     string
	offset   int
	handlers []*subscription
	buffer   []event.Message // ReorderWindow holding area
}

type subscription struct {
	ctx context.Context
	h   event.Handler
}

// NewMemoryBus returns a bus for env, or ErrNotAllowed.
func NewMemoryBus(env string) (*MemoryBus, error) {
	if !Allowed(env) {
		return nil, fmt.Errorf("%w: env %q", ErrNotAllowed, env)
	}
	return &MemoryBus{
		env:             env,
		topics:          map[string]*memTopic{},
		failPublish:     map[string]int{},
		maxRedeliveries: DefaultMaxRedeliveries,
		deliveries:      map[string]int{},
	}, nil
}

var _ event.Bus = (*MemoryBus)(nil)

// Env returns the environment the bus was built for.
func (b *MemoryBus) Env() string { return b.env }

// DuplicateEvery makes every nth published message be delivered twice to
// every group (n <= 0 disables).
func (b *MemoryBus) DuplicateEvery(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dupEvery = n
}

// ReorderWindow makes each group hold k messages and deliver them with the
// order of distinct keys reversed; messages of the same key keep their
// order. k <= 1 disables. Flush delivers a partially filled window.
func (b *MemoryBus) ReorderWindow(k int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if k <= 1 {
		k = 0
	}
	b.reorder = k
}

// FailPublishFor makes the next times Publish calls on topic fail with
// ErrInjectedPublishFailure.
func (b *MemoryBus) FailPublishFor(topic string, times int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if times <= 0 {
		delete(b.failPublish, topic)
		return
	}
	b.failPublish[topic] = times
}

// SetMaxRedeliveries sets how often a failing handler is retried.
func (b *MemoryBus) SetMaxRedeliveries(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n < 0 {
		n = 0
	}
	b.maxRedeliveries = n
}

// Publish appends the message to the topic log and drains deliveries.
func (b *MemoryBus) Publish(ctx context.Context, topic, key string, value []byte, headers map[string]string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return ErrClosed
	}
	if n := b.failPublish[topic]; n > 0 {
		if n == 1 {
			delete(b.failPublish, topic)
		} else {
			b.failPublish[topic] = n - 1
		}
		b.mu.Unlock()
		return fmt.Errorf("%w: topic %s", ErrInjectedPublishFailure, topic)
	}
	b.seq++
	id := headers[event.HeaderEventID]
	if id == "" {
		id = fmt.Sprintf("mem-%d", b.seq)
	}
	msg := event.Message{
		ID:      id,
		Topic:   topic,
		Key:     key,
		Value:   append([]byte(nil), value...),
		Headers: maps.Clone(headers),
	}
	t := b.topic(topic)
	t.log = append(t.log, msg)
	b.published++
	if b.dupEvery > 0 && b.published%b.dupEvery == 0 {
		t.log = append(t.log, msg)
	}
	b.mu.Unlock()
	b.drain()
	return nil
}

// Subscribe registers h as a handler of group on topic. Several handlers in
// one group split keys between them (a key always goes to the same handler
// while the membership is stable). The subscription ends when ctx is done.
// A group that subscribes after messages were published receives them all.
func (b *MemoryBus) Subscribe(ctx context.Context, topic, group string, h event.Handler) error {
	if h == nil {
		return errors.New("eventtest: nil handler")
	}
	if topic == "" || group == "" {
		return errors.New("eventtest: topic and group are required")
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return ErrClosed
	}
	t := b.topic(topic)
	g, ok := t.groups[group]
	if !ok {
		g = &memGroup{name: group}
		t.groups[group] = g
	}
	g.handlers = append(g.handlers, &subscription{ctx: ctx, h: h})
	b.mu.Unlock()
	b.drain()
	return nil
}

// Close flushes reorder windows and rejects further use.
func (b *MemoryBus) Close(ctx context.Context) error {
	b.Flush()
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	return nil
}

// Flush delivers every partially filled reorder window.
func (b *MemoryBus) Flush() {
	b.mu.Lock()
	type pending struct {
		g     *memGroup
		batch []event.Message
	}
	var work []pending
	for _, t := range b.topics {
		for _, g := range sortedGroups(t.groups) {
			if len(g.buffer) > 0 {
				work = append(work, pending{g, reorderBatch(g.buffer)})
				g.buffer = nil
			}
		}
	}
	b.mu.Unlock()
	for _, w := range work {
		b.mu.Lock()
		handlers := liveHandlers(w.g)
		b.mu.Unlock()
		b.deliver(w.g, handlers, w.batch)
	}
	b.drain()
}

// Log returns a copy of everything published on topic (duplicates included).
func (b *MemoryBus) Log(topic string) []event.Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.topics[topic]
	if !ok {
		return nil
	}
	return append([]event.Message(nil), t.log...)
}

// Delivered returns how many messages were handed to group's handlers on
// topic (successful or not, redeliveries counted once per message).
func (b *MemoryBus) Delivered(topic, group string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.deliveries[topic+"\x00"+group]
}

// Failures returns the messages whose handlers kept failing.
func (b *MemoryBus) Failures() []Failure {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Failure(nil), b.failures...)
}

func (b *MemoryBus) topic(name string) *memTopic {
	t, ok := b.topics[name]
	if !ok {
		t = &memTopic{groups: map[string]*memGroup{}}
		b.topics[name] = t
	}
	return t
}

// drain delivers pending messages until every group with a live handler has
// consumed its topic log. Only one goroutine drains at a time; others return
// immediately because the drainer will pick their messages up.
func (b *MemoryBus) drain() {
	b.mu.Lock()
	if b.draining {
		b.mu.Unlock()
		return
	}
	b.draining = true
	for {
		g, msg, ok := b.next()
		if !ok {
			b.draining = false
			b.mu.Unlock()
			return
		}
		var batch []event.Message
		if b.reorder > 0 {
			g.buffer = append(g.buffer, msg)
			if len(g.buffer) < b.reorder {
				continue
			}
			batch = reorderBatch(g.buffer)
			g.buffer = nil
		} else {
			batch = []event.Message{msg}
		}
		handlers := liveHandlers(g)
		b.mu.Unlock()
		b.deliver(g, handlers, batch)
		b.mu.Lock()
	}
}

// next finds the oldest undelivered message of any group with a live
// handler and advances that group's offset. Topics and groups are visited
// in name order so delivery is deterministic. Must be called with mu held.
func (b *MemoryBus) next() (*memGroup, event.Message, bool) {
	names := make([]string, 0, len(b.topics))
	for name := range b.topics {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t := b.topics[name]
		for _, g := range sortedGroups(t.groups) {
			if g.offset >= len(t.log) || len(liveHandlers(g)) == 0 {
				continue
			}
			msg := t.log[g.offset]
			g.offset++
			b.deliveries[name+"\x00"+g.name]++
			return g, msg, true
		}
	}
	return nil, event.Message{}, false
}

func sortedGroups(groups map[string]*memGroup) []*memGroup {
	out := make([]*memGroup, 0, len(groups))
	for _, g := range groups {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// liveHandlers drops subscriptions whose ctx ended. Must be called with mu
// held.
func liveHandlers(g *memGroup) []*subscription {
	live := g.handlers[:0]
	for _, s := range g.handlers {
		if s.ctx.Err() == nil {
			live = append(live, s)
		}
	}
	for i := len(live); i < len(g.handlers); i++ {
		g.handlers[i] = nil
	}
	g.handlers = live
	return append([]*subscription(nil), live...)
}

// deliver hands each message of batch to the handler owning its key,
// retrying a failing handler up to maxRedeliveries times.
func (b *MemoryBus) deliver(g *memGroup, handlers []*subscription, batch []event.Message) {
	if len(handlers) == 0 {
		return
	}
	b.mu.Lock()
	retries := b.maxRedeliveries
	b.mu.Unlock()
	for _, msg := range batch {
		s := handlers[partitionOf(msg.Key, len(handlers))]
		var err error
		for attempt := 0; attempt <= retries; attempt++ {
			m := msg
			m.Value = append([]byte(nil), msg.Value...)
			m.Headers = maps.Clone(msg.Headers)
			if err = s.h(s.ctx, m); err == nil {
				break
			}
		}
		if err != nil {
			b.mu.Lock()
			b.failures = append(b.failures, Failure{Group: g.name, Message: msg, Err: err})
			b.mu.Unlock()
		}
	}
}

func partitionOf(key string, n int) int {
	if n <= 1 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return int(h.Sum32() % uint32(n)) //nolint:gosec // G115: n is a small positive partition count
}

// reorderBatch reverses the order of distinct keys while keeping the order
// of messages sharing a key.
func reorderBatch(in []event.Message) []event.Message {
	var order []string
	byKey := map[string][]event.Message{}
	for _, m := range in {
		if _, seen := byKey[m.Key]; !seen {
			order = append(order, m.Key)
		}
		byKey[m.Key] = append(byKey[m.Key], m)
	}
	out := make([]event.Message, 0, len(in))
	for i := len(order) - 1; i >= 0; i-- {
		out = append(out, byKey[order[i]]...)
	}
	return out
}
