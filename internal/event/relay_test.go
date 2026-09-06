package event

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/clock"
)

type nopBus struct{}

func (nopBus) Publish(context.Context, string, string, []byte, map[string]string) error { return nil }

func (nopBus) Subscribe(context.Context, string, string, Handler) error { return nil }

func (nopBus) Close(context.Context) error { return nil }

func TestNewRelay_Defaults(t *testing.T) {
	t.Parallel()
	r := NewRelay(nil, nopBus{}, nil, RelayOptions{})
	o := r.Options()
	assert.Equal(t, DefaultBatchSize, o.BatchSize)
	assert.Equal(t, DefaultPollInterval, o.PollInterval)
	assert.Equal(t, DefaultMaxInterval, o.MaxInterval)
	assert.Equal(t, DefaultJitterPercent, o.JitterPercent)
	assert.Equal(t, DefaultRetryBackoffBase, o.RetryBackoffBase)
	assert.Equal(t, DefaultRetryBackoffMax, o.RetryBackoffMax)
	assert.Equal(t, DefaultRunTimeout, o.RunTimeout)
	assert.NotNil(t, o.Clock)
	assert.NotNil(t, o.Observer)
	assert.NotNil(t, r.log)

	r = NewRelay(nil, nopBus{}, nil, RelayOptions{
		PollInterval: time.Second, MaxInterval: time.Millisecond, JitterPercent: 500,
		RetryBackoffBase: time.Minute, RetryBackoffMax: time.Second, Clock: clock.NewFake(fixedTime),
	})
	o = r.Options()
	assert.Equal(t, time.Second, o.MaxInterval, "MaxInterval is raised to PollInterval")
	assert.Equal(t, 100, o.JitterPercent)
	assert.Equal(t, time.Minute, o.RetryBackoffMax, "RetryBackoffMax is raised to base")
}

func TestRelay_FailureDelayGrowsToCap(t *testing.T) {
	t.Parallel()
	r := NewRelay(nil, nopBus{}, nil, RelayOptions{PollInterval: 100 * time.Millisecond, MaxInterval: time.Second})
	assert.Equal(t, 100*time.Millisecond, r.failureDelay(0))
	assert.Equal(t, 100*time.Millisecond, r.failureDelay(1))
	assert.Equal(t, 200*time.Millisecond, r.failureDelay(2))
	assert.Equal(t, 400*time.Millisecond, r.failureDelay(3))
	assert.Equal(t, 800*time.Millisecond, r.failureDelay(4))
	assert.Equal(t, time.Second, r.failureDelay(5))
	assert.Equal(t, time.Second, r.failureDelay(50))
}

func TestRelay_JitterStaysWithinBounds(t *testing.T) {
	t.Parallel()
	r := NewRelay(nil, nopBus{}, nil, RelayOptions{PollInterval: time.Second, JitterPercent: 20})
	for i := 0; i < 500; i++ {
		d := r.jitter(time.Second)
		assert.GreaterOrEqual(t, d, 800*time.Millisecond)
		assert.LessOrEqual(t, d, 1200*time.Millisecond)
		z := r.jitter(0)
		assert.GreaterOrEqual(t, z, time.Duration(0))
		assert.LessOrEqual(t, z, 200*time.Millisecond)
	}
	r = NewRelay(nil, nopBus{}, nil, RelayOptions{PollInterval: time.Second, JitterPercent: 100})
	for i := 0; i < 100; i++ {
		assert.LessOrEqual(t, r.jitter(time.Second), 2*time.Second)
	}
}

func TestPublishHeaders(t *testing.T) {
	t.Parallel()
	e := validEnvelope(TopicKillSwitchChanged, "ks-1")
	e.RecordedAt = fixedTime.Add(time.Millisecond)
	e.Headers = map[string]string{"x-trace": "abc", HeaderEventID: "spoofed"}
	h := PublishHeaders(e)
	assert.Equal(t, e.ID, h[HeaderEventID], "standard headers win over producer headers")
	assert.Equal(t, "killswitch.changed", h[HeaderEventType])
	assert.Equal(t, "1", h[HeaderSchemaVersion])
	assert.Equal(t, "event-test", h[HeaderSource])
	assert.Equal(t, "kill_switch", h[HeaderAggregateType])
	assert.Equal(t, "ks-1", h[HeaderAggregateID])
	assert.Equal(t, "corr-1", h[HeaderCorrelationID])
	assert.Equal(t, "cause-1", h[HeaderCausationID])
	assert.Equal(t, "2026-09-05T12:00:00.123456789Z", h[HeaderOccurredAt])
	assert.Equal(t, "2026-09-05T12:00:00.124456789Z", h[HeaderRecordedAt])
	assert.Equal(t, ContentTypeJSON, h[HeaderContentType])
	assert.Equal(t, "abc", h["x-trace"])

	e.CorrelationID, e.CausationID, e.RecordedAt = "", "", time.Time{}
	h = PublishHeaders(e)
	_, ok := h[HeaderCorrelationID]
	assert.False(t, ok)
	_, ok = h[HeaderRecordedAt]
	assert.False(t, ok)
}

func TestTruncateError(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "", truncateError(nil))
	assert.Equal(t, "boom", truncateError(errors.New("boom")))
	long := errors.New(strings.Repeat("x", maxLastError+100))
	assert.Len(t, truncateError(long), maxLastError)
}

func TestMessage_Envelope(t *testing.T) {
	t.Parallel()
	e := validEnvelope(TopicAgentRunCompleted, "run-1")
	c, err := e.CanonicalBytes()
	require.NoError(t, err)
	got, err := Message{Value: c}.Envelope()
	require.NoError(t, err)
	assert.Equal(t, e.ID, got.ID)
	_, err = Message{Value: []byte("nope")}.Envelope()
	assert.Error(t, err)
}
