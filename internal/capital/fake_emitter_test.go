package capital

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5"
)

// recordedEvent is one Emit call observed by recordingEmitter.
type recordedEvent struct {
	Topic   string
	Payload any
}

// recordingEmitter is the test double for Emitter. It is safe for
// concurrent use (the torture test emits from 100 goroutines) and records
// every call, including calls from transactions that later roll back, so
// counts are exact only in sequential tests.
type recordingEmitter struct {
	mu     sync.Mutex
	events []recordedEvent
	fail   error
}

func (e *recordingEmitter) Emit(_ context.Context, _ pgx.Tx, topic string, payload any) error {
	if e.fail != nil {
		return e.fail
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, recordedEvent{Topic: topic, Payload: payload})
	return nil
}
