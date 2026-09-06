package event

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/errs"
)

// These tests exercise everything Enqueue does before it touches the
// transaction; a nil tx proves no SQL ran.

func TestOutbox_Enqueue_NoEventsIsNoop(t *testing.T) {
	t.Parallel()
	o := NewOutbox(nil)
	require.NoError(t, o.Enqueue(context.Background(), nil, "nope"))
}

func TestOutbox_Enqueue_UnknownTopic(t *testing.T) {
	t.Parallel()
	o := NewOutbox(clock.NewFake(fixedTime))
	err := o.Enqueue(context.Background(), nil, "nope.topic", validEnvelope(TopicOrderTransitioned, "o-1"))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnknownTopic)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

func TestOutbox_Enqueue_ValidationBeforeSQL(t *testing.T) {
	t.Parallel()
	o := NewOutbox(clock.NewFake(fixedTime))
	ctx := context.Background()
	topic := TopicOrderTransitioned.String()

	tests := []struct {
		name   string
		mutate func(e *Envelope)
		field  string
	}{
		{"invalid envelope", func(e *Envelope) { e.Payload = json.RawMessage(`nope`) }, "payload"},
		{"type not on topic", func(e *Envelope) { e.Type = "intent.transitioned" }, "type"},
		{"schema version mismatch", func(e *Envelope) { e.SchemaVersion = 99 }, "schema_version"},
		{"wrong aggregate", func(e *Envelope) { e.AggregateType = "intent" }, "topic"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			good := validEnvelope(TopicOrderTransitioned, "o-1")
			bad := validEnvelope(TopicOrderTransitioned, "o-2")
			tc.mutate(&bad)
			err := o.Enqueue(ctx, nil, topic, good, bad)
			require.Error(t, err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
			ee, ok := errs.As(err)
			require.True(t, ok)
			assert.Contains(t, ee.Fields, tc.field, "%v", ee.Fields)
			assert.Equal(t, 1, ee.Fields["index"])
		})
	}
}

func TestOutbox_Enqueue_ValidEnvelopeReachesTransaction(t *testing.T) {
	t.Parallel()
	o := NewOutbox(clock.NewFake(fixedTime))
	err := o.Enqueue(context.Background(), nil, TopicOrderTransitioned.String(), validEnvelope(TopicOrderTransitioned, "o-1"))
	require.Error(t, err)
	assert.Equal(t, errs.CodeInternal, errs.CodeOf(err), "nil tx is the only thing left to fail on")
}

func TestOutbox_Prepare_StampsRecordedAtAndCanonicalisesPayload(t *testing.T) {
	t.Parallel()
	o := NewOutbox(clock.NewFake(fixedTime))
	sp, _ := Lookup(TopicFillObserved)
	ev := validEnvelope(TopicFillObserved, "order-7")
	ev.Payload = json.RawMessage(`{ "b" : 1 , "a" : "<x>" }`)
	ev.Headers = map[string]string{"z": "1", "a": "2"}
	row, err := o.prepare(sp, ev)
	require.NoError(t, err)
	assert.Equal(t, fixedTime, row.recordedAt)
	assert.Equal(t, "order-7", row.partitionKey)
	assert.Equal(t, `{"a":"<x>","b":1}`, string(row.payload))
	assert.Equal(t, `{"a":"2","z":"1"}`, string(row.headers))
	assert.Equal(t, ev.ID, row.id)
	assert.Nil(t, nullable(""))
	assert.Equal(t, "x", *nullable("x"))

	ev.RecordedAt = fixedTime.Add(-time.Hour)
	row, err = o.prepare(sp, ev)
	require.NoError(t, err)
	assert.Equal(t, fixedTime.Add(-time.Hour), row.recordedAt, "explicit recorded_at is kept")
}
