package eventtest

import (
	"context"
	"maps"
	"strconv"
	"sync"

	"github.com/nodal/controlplane/internal/event"
)

// Recorder is an event.Bus that only remembers what was published. It never
// delivers; Subscribe records the group so tests can assert wiring.
type Recorder struct {
	mu       sync.Mutex
	msgs     []event.Message
	err      error
	failNext int
	subs     map[string][]string // topic -> groups
	closed   bool
	seq      uint64
}

// NewRecorder returns an empty Recorder.
func NewRecorder() *Recorder {
	return &Recorder{subs: map[string][]string{}}
}

var _ event.Bus = (*Recorder)(nil)

// Publish records the message, or returns the configured error.
func (r *Recorder) Publish(ctx context.Context, topic, key string, value []byte, headers map[string]string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	if r.failNext > 0 {
		r.failNext--
		err := r.err
		if r.failNext == 0 {
			r.err = nil
		}
		return err
	}
	if r.err != nil {
		return r.err
	}
	r.seq++
	id := headers[event.HeaderEventID]
	if id == "" {
		id = "rec-" + strconv.FormatUint(r.seq, 10)
	}
	r.msgs = append(r.msgs, event.Message{
		ID:      id,
		Topic:   topic,
		Key:     key,
		Value:   append([]byte(nil), value...),
		Headers: maps.Clone(headers),
	})
	return nil
}

// Subscribe records the group; nothing is delivered.
func (r *Recorder) Subscribe(ctx context.Context, topic, group string, h event.Handler) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	r.subs[topic] = append(r.subs[topic], group)
	return nil
}

// Close rejects further use.
func (r *Recorder) Close(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return nil
}

// SetPublishError makes every Publish fail with err until cleared with nil.
func (r *Recorder) SetPublishError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
	r.failNext = 0
}

// FailNext makes the next n Publish calls fail with err, then succeed.
func (r *Recorder) FailNext(n int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 || err == nil {
		r.err = nil
		r.failNext = 0
		return
	}
	r.err = err
	r.failNext = n
}

// Messages returns a copy of everything recorded, in publish order.
func (r *Recorder) Messages() []event.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]event.Message(nil), r.msgs...)
}

// ByTopic returns the recorded messages of one topic.
func (r *Recorder) ByTopic(topic string) []event.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []event.Message
	for _, m := range r.msgs {
		if m.Topic == topic {
			out = append(out, m)
		}
	}
	return out
}

// Groups returns the groups subscribed on topic.
func (r *Recorder) Groups(topic string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.subs[topic]...)
}

// Reset forgets recorded messages (subscriptions and errors are kept).
func (r *Recorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = nil
}
