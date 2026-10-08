package capital

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/event"
)

// TestOutboxEmitter_FailsClosed: an unknown payload or an unregistered topic is
// an error before any SQL, never a silent drop.
func TestOutboxEmitter_FailsClosed(t *testing.T) {
	e := OutboxEmitter{Outbox: event.NewOutbox(clock.System()), Clock: clock.System()}
	err := e.Emit(context.Background(), nil, TopicReservationCreated, struct{}{})
	require.ErrorIs(t, err, ErrUnknownEventPayload)
	err = e.Emit(context.Background(), nil, "not.a.topic", ReservationEvent{ReservationID: "r"})
	require.Error(t, err)
	err = e.Emit(context.Background(), nil, TopicReservationCreated, ReservationEvent{})
	require.Error(t, err, "no aggregate id")
	require.Error(t, OutboxEmitter{}.Emit(context.Background(), nil, TopicReservationCreated, ReservationEvent{ReservationID: "r"}))
}
