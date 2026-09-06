package redpandabus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/event"
)

// Sentinel errors.
var (
	// ErrFakeModeForbidden is returned by New for fake mode outside
	// LOCAL/TEST/DEV.
	ErrFakeModeForbidden = errors.New("redpandabus: fake mode is only allowed in LOCAL, TEST or DEV")
	// ErrClosed is returned after Close.
	ErrClosed = errors.New("redpandabus: bus is closed")
	// ErrBrokersRequired is returned when no broker is configured.
	ErrBrokersRequired = errors.New("redpandabus: at least one broker is required")
)

// Options tune the bus.
type Options struct {
	// ClientID identifies the client to the broker.
	ClientID string
	// ProduceTimeout bounds one Publish (default 15 s).
	ProduceTimeout time.Duration
	// SessionTimeout is the consumer group session timeout (default 30 s).
	SessionTimeout time.Duration
	// MaxHandlerRetries bounds retries of a failing handler before the
	// subscription gives up on the partition and reports through OnError
	// (0 = retry forever with capped backoff; a poison record then blocks
	// its partition, which is the honest at-least-once behavior).
	MaxHandlerRetries int
	// RetryBackoff is the initial handler retry backoff (default 200 ms,
	// doubling up to 10 s).
	RetryBackoff time.Duration
	// AllowAutoTopicCreation lets the broker create topics on first use
	// (LOCAL/TEST convenience; production topics are provisioned).
	AllowAutoTopicCreation bool
	// OnError receives subscription errors (fatal fetch errors, handlers
	// that exhausted their retries). Nil logs only.
	OnError func(topic, group string, err error)
	// Logger receives operational logs (discarded when nil).
	Logger *slog.Logger
}

func (o Options) withDefaults() Options {
	if o.ClientID == "" {
		o.ClientID = "controlplane"
	}
	if o.ProduceTimeout <= 0 {
		o.ProduceTimeout = 15 * time.Second
	}
	if o.SessionTimeout <= 0 {
		o.SessionTimeout = 30 * time.Second
	}
	if o.RetryBackoff <= 0 {
		o.RetryBackoff = 200 * time.Millisecond
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	return o
}

// maxBackoff caps handler retry backoff.
const maxBackoff = 10 * time.Second

// backoffFor returns the retry delay of attempt (1-based).
func backoffFor(base time.Duration, attempt int) time.Duration {
	d := base
	for i := 1; i < attempt && d < maxBackoff; i++ {
		d *= 2
	}
	if d > maxBackoff {
		d = maxBackoff
	}
	return d
}

// messageID derives event.Message.ID: the event_id header when present,
// else a stable position id.
func messageID(headers map[string]string, topic string, partition int32, offset int64) string {
	if id := headers[event.HeaderEventID]; id != "" {
		return id
	}
	return fmt.Sprintf("%s/%d/%d", topic, partition, offset)
}

// Loopback is the fake-mode event.Bus: in-process, at-least-once, with
// consumer groups that each see every message once (unless the handler
// fails, in which case the message is retried). It exists so LOCAL/TEST/DEV
// binaries in fake mode run the same code path as production without a
// broker; tests needing fault injection use event/eventtest.MemoryBus.
type Loopback struct {
	opts Options

	mu     sync.Mutex
	closed bool
	topics map[string]*loopTopic
	wg     sync.WaitGroup
}

type loopTopic struct {
	log    []event.Message
	groups map[string]*loopGroup
}

type loopGroup struct {
	offset  int
	handler event.Handler
	ctx     context.Context
	wake    chan struct{}
}

var _ event.Bus = (*Loopback)(nil)

// NewLoopback builds the loopback for env, refusing STAGING and PROD.
func NewLoopback(env config.Environment, opts Options) (*Loopback, error) {
	if !env.AllowsFakeProviders() {
		return nil, fmt.Errorf("%w: env %s", ErrFakeModeForbidden, env)
	}
	return &Loopback{opts: opts.withDefaults(), topics: map[string]*loopTopic{}}, nil
}

// Publish appends to the topic log and wakes consumers.
func (b *Loopback) Publish(ctx context.Context, topic, key string, value []byte, headers map[string]string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if topic == "" {
		return errors.New("redpandabus: topic is required")
	}
	// The double enforces the same partition rule as the real client, so a
	// producer that keys wrongly fails in the test that would otherwise have
	// blessed it rather than in production, where the symptom is reordered
	// events and not an error.
	if err := ValidatePartitionKey(topic, key, headers); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}
	t := b.topic(topic)
	offset := len(t.log)
	msg := event.Message{
		ID: messageID(headers, topic, 0, int64(offset)), Topic: topic, Key: key,
		Value: append([]byte(nil), value...), Headers: maps.Clone(headers),
	}
	t.log = append(t.log, msg)
	for _, g := range t.groups {
		select {
		case g.wake <- struct{}{}:
		default:
		}
	}
	return nil
}

// Subscribe starts a consumer goroutine for (topic, group) and returns
// immediately; delivery continues until ctx is done or Close.
func (b *Loopback) Subscribe(ctx context.Context, topic, group string, h event.Handler) error {
	if h == nil || topic == "" || group == "" {
		return errors.New("redpandabus: topic, group and handler are required")
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return ErrClosed
	}
	t := b.topic(topic)
	if _, dup := t.groups[group]; dup {
		b.mu.Unlock()
		return fmt.Errorf("redpandabus: group %q already subscribed to %q", group, topic)
	}
	g := &loopGroup{handler: h, ctx: ctx, wake: make(chan struct{}, 1)}
	t.groups[group] = g
	b.wg.Add(1)
	b.mu.Unlock()
	go b.consume(ctx, topic, group, t, g)
	return nil
}

func (b *Loopback) consume(ctx context.Context, topic, group string, t *loopTopic, g *loopGroup) {
	defer b.wg.Done()
	defer func() {
		b.mu.Lock()
		delete(t.groups, group)
		b.mu.Unlock()
	}()
	for {
		b.mu.Lock()
		closed := b.closed
		var msg event.Message
		have := g.offset < len(t.log)
		if have {
			msg = t.log[g.offset]
		}
		b.mu.Unlock()
		if closed {
			return
		}
		if !have {
			select {
			case <-ctx.Done():
				return
			case <-g.wake:
				continue
			}
		}
		if !b.deliver(ctx, topic, group, msg, g.handler) {
			return
		}
		b.mu.Lock()
		g.offset++ // "commit" after the handler succeeded
		b.mu.Unlock()
	}
}

// deliver retries the handler until it succeeds, ctx ends, or retries are
// exhausted. It reports whether the subscription may continue.
func (b *Loopback) deliver(ctx context.Context, topic, group string, msg event.Message, h event.Handler) bool {
	for attempt := 1; ; attempt++ {
		m := msg
		m.Value = append([]byte(nil), msg.Value...)
		m.Headers = maps.Clone(msg.Headers)
		err := h(ctx, m)
		if err == nil {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		b.opts.Logger.WarnContext(ctx, "handler failed; redelivering", slog.String("topic", topic), slog.String("group", group), slog.Int("attempt", attempt))
		if b.opts.MaxHandlerRetries > 0 && attempt > b.opts.MaxHandlerRetries {
			if b.opts.OnError != nil {
				b.opts.OnError(topic, group, fmt.Errorf("redpandabus: handler exhausted retries on %s: %w", msg.ID, err))
			}
			return false
		}
		timer := time.NewTimer(backoffFor(b.opts.RetryBackoff, attempt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

// Close stops every consumer and rejects further use.
func (b *Loopback) Close(ctx context.Context) error {
	b.mu.Lock()
	b.closed = true
	for _, t := range b.topics {
		for _, g := range t.groups {
			select {
			case g.wake <- struct{}{}:
			default:
			}
		}
	}
	b.mu.Unlock()
	done := make(chan struct{})
	go func() { b.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Log returns a copy of the topic log (tests).
func (b *Loopback) Log(topic string) []event.Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.topics[topic]
	if !ok {
		return nil
	}
	return append([]event.Message(nil), t.log...)
}

func (b *Loopback) topic(name string) *loopTopic {
	t, ok := b.topics[name]
	if !ok {
		t = &loopTopic{groups: map[string]*loopGroup{}}
		b.topics[name] = t
	}
	return t
}
