package capital

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/event"
)

// ErrUnknownEventPayload is returned for a payload this package does not emit.
var ErrUnknownEventPayload = errors.New("capital: unknown event payload")

// OutboxEmitter is the Emitter that writes capital events to the
// transactional outbox in the caller's transaction. It is the same mapping
// cmd/api wires (with logging and a drop counter around it), made reusable for
// the workers, the scripts and the advanced subsystems' tests, so none of them
// needs a stub emitter that silently discards what a downstream consumer
// relies on.
//
// It fails closed: an unrecognized payload type or an unregistered topic is an
// error, never a silent drop.
type OutboxEmitter struct {
	Outbox *event.Outbox
	Clock  clock.Clock
}

var _ Emitter = OutboxEmitter{}

// Emit implements Emitter.
func (e OutboxEmitter) Emit(ctx context.Context, tx pgx.Tx, topic string, payload any) error {
	if e.Outbox == nil || e.Clock == nil {
		return errors.New("capital: OutboxEmitter needs an outbox and a clock")
	}
	var aggregate string
	switch p := payload.(type) {
	case ReservationEvent:
		aggregate = p.ReservationID
	case EnvelopeEvent:
		aggregate = p.EnvelopeID
	case HoldEvent:
		aggregate = p.HoldID
	default:
		return fmt.Errorf("%w: topic %s, payload %T", ErrUnknownEventPayload, topic, payload)
	}
	spec, ok := event.Lookup(event.Topic(topic))
	if !ok {
		return fmt.Errorf("capital: topic %q is not registered", topic)
	}
	if aggregate == "" {
		return fmt.Errorf("capital: %s event without an aggregate id", topic)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return e.Outbox.Enqueue(ctx, tx, topic, event.Envelope{
		ID: event.NewEventID().String(), Type: topic, SchemaVersion: spec.SchemaVersion, Source: "capital",
		AggregateType: spec.AggregateType, AggregateID: aggregate, OccurredAt: e.Clock.Now().UTC(), Payload: body,
	})
}
