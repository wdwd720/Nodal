package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/money"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newTestEmitter returns an emitter whose log and drop counter can be
// inspected.
func newTestEmitter(t *testing.T) (outboxEmitter, *syncBuffer, *sdkmetric.ManualReader) {
	t.Helper()
	var buf syncBuffer
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	counter, err := newDroppedEventCounter(provider.Meter("test"))
	require.NoError(t, err)
	return outboxEmitter{
		outbox:  event.NewOutbox(clock.NewFake(time.Now().UTC())),
		clk:     clock.NewFake(time.Now().UTC()),
		log:     slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})),
		dropped: counter,
	}, &buf, reader
}

// droppedCount returns the value of outbox_events_dropped for the given topic
// and reason, and -1 when no such data point was recorded.
func droppedCount(t *testing.T, reader *sdkmetric.ManualReader, topic, reason string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "outbox_events_dropped" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "outbox_events_dropped must be a counter")
			for _, dp := range sum.DataPoints {
				gotTopic, _ := dp.Attributes.Value("topic")
				gotReason, _ := dp.Attributes.Value("reason")
				if gotTopic.AsString() == topic && gotReason.AsString() == reason {
					return dp.Value
				}
			}
		}
	}
	return -1
}

// TestOutboxEmitterDropsAreLoud: an event that cannot be published does not
// fail the financial transaction — a transport gap must never block money —
// but it is recorded at WARN and counted, because a dropped event means the
// ledger and its consumers have silently diverged.
func TestOutboxEmitterDropsAreLoud(t *testing.T) {
	t.Parallel()

	t.Run("unregistered topic", func(t *testing.T) {
		t.Parallel()
		e, log, reader := newTestEmitter(t)
		payload := capital.ReservationEvent{ReservationID: capital.NewReservationID().String()}

		// A nil transaction proves the drop happens before any write.
		require.NoError(t, e.Emit(context.Background(), nil, "capital.something.invented", payload))

		out := log.String()
		assert.Contains(t, out, `"level":"WARN"`)
		assert.Contains(t, out, "outbox event dropped")
		assert.Contains(t, out, `"topic":"capital.something.invented"`)
		assert.Contains(t, out, `"reason":"`+dropUnregisteredTopic+`"`)
		assert.Equal(t, int64(1),
			droppedCount(t, reader, "capital.something.invented", dropUnregisteredTopic))
	})

	t.Run("missing aggregate id", func(t *testing.T) {
		t.Parallel()
		e, log, reader := newTestEmitter(t)
		topic := string(event.TopicCapitalReservationCreated)

		require.NoError(t, e.Emit(context.Background(), nil, topic, capital.ReservationEvent{}))

		out := log.String()
		assert.Contains(t, out, `"level":"WARN"`)
		assert.Contains(t, out, `"reason":"`+dropMissingAggregate+`"`)
		assert.Equal(t, int64(1), droppedCount(t, reader, topic, dropMissingAggregate))
	})

	t.Run("a drop is counted per topic", func(t *testing.T) {
		t.Parallel()
		e, _, reader := newTestEmitter(t)
		for range 3 {
			require.NoError(t, e.Emit(context.Background(), nil, "capital.gone", capital.ReservationEvent{ReservationID: "r-1"}))
		}
		assert.Equal(t, int64(3), droppedCount(t, reader, "capital.gone", dropUnregisteredTopic))
	})
}

// TestOutboxEmitterFailsClosedOnAnUnknownPayload: a payload type this binary
// cannot key is a code gap, not a deployment gap. It fails the caller's
// transaction so it surfaces in a test or a canary instead of quietly costing
// an event in production.
func TestOutboxEmitterFailsClosedOnAnUnknownPayload(t *testing.T) {
	t.Parallel()
	e, log, reader := newTestEmitter(t)

	type futurePayload struct{ Thing string }
	err := e.Emit(context.Background(), nil, string(event.TopicCapitalReservationCreated),
		futurePayload{Thing: "added later"})

	require.Error(t, err)
	assert.ErrorIs(t, err, errUnknownEventPayload)
	assert.Contains(t, err.Error(), "futurePayload")
	assert.Contains(t, log.String(), `"level":"ERROR"`)
	assert.Equal(t, int64(-1),
		droppedCount(t, reader, string(event.TopicCapitalReservationCreated), dropUnregisteredTopic),
		"a fail-closed payload is not a drop")
}

// TestCapitalAggregateIDCoversEveryPayloadThePackageEmits: every capital event
// type must be keyable. A new one added to internal/capital fails here rather
// than silently losing its events.
func TestCapitalAggregateIDCoversEveryPayloadThePackageEmits(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		payload any
		want    string
	}{
		{"reservation", capital.ReservationEvent{ReservationID: "r-1"}, "r-1"},
		{"envelope", capital.EnvelopeEvent{EnvelopeID: "e-1"}, "e-1"},
		{"hold", capital.HoldEvent{HoldID: "h-1"}, "h-1"},
	}
	for _, tc := range cases {
		got, err := capitalAggregateID(tc.payload)
		require.NoError(t, err, tc.name)
		assert.Equal(t, tc.want, got, tc.name)
	}
	_, err := capitalAggregateID(struct{ X int }{1})
	assert.ErrorIs(t, err, errUnknownEventPayload)
}

// TestEveryCapitalTopicIsRegistered: the emitter's drop path exists for a
// deployment whose registry lags the domain, but in this build every topic
// internal/capital emits must be publishable.
func TestEveryCapitalTopicIsRegistered(t *testing.T) {
	t.Parallel()
	topics := []string{
		capital.TopicReservationCreated, capital.TopicReservationConsumed,
		capital.TopicReservationReleased, capital.TopicReservationExpired,
		capital.TopicReservationLocked,
		capital.TopicHoldPlaced, capital.TopicHoldReleased,
		capital.TopicEnvelopeCreated, capital.TopicEnvelopeUpdated,
		capital.TopicEnvelopeStatusChanged, capital.TopicEnvelopePnLApplied,
		capital.TopicEnvelopeExhausted, capital.TopicEnvelopeUndeployed,
	}
	for _, topic := range topics {
		spec, ok := event.Lookup(event.Topic(topic))
		assert.True(t, ok, "topic %q is not registered, so its events would be dropped", topic)
		if ok {
			assert.NotEmpty(t, spec.AggregateType, topic)
		}
	}
}

// TestDropLogNeverCarriesAPayload: the drop line names the topic, the reason
// and the aggregate id. It must not serialize the event body, which can carry
// amounts and identifiers that do not belong in an operational log line.
func TestDropLogNeverCarriesAPayload(t *testing.T) {
	t.Parallel()
	e, log, _ := newTestEmitter(t)
	usd, err := money.ParseUSD("1234.56")
	require.NoError(t, err)
	payload := capital.ReservationEvent{
		ReservationID: "r-9", AccountID: "secret-account-id", USD: usd, Reason: "secret-reason",
	}
	require.NoError(t, e.Emit(context.Background(), nil, "capital.not.registered", payload))

	out := log.String()
	assert.Contains(t, out, `"aggregate_id":"r-9"`)
	for _, leaked := range []string{"secret-account-id", "secret-reason", "1234.56"} {
		assert.False(t, strings.Contains(out, leaked), "the drop log leaked %q", leaked)
	}
}
