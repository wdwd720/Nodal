package event

import (
	"encoding/json"
	"time"
)

// nul is a NUL character; spelled as an expression so no source file
// carries a NUL byte.
var nul = string(rune(0))

// backslash is spelled as an expression for the same reason.
var backslash = string(rune(92))

// nulEscape is the JSON escape sequence for NUL.
var nulEscape = backslash + "u0000"

// fixedTime is the deterministic instant used by unit tests.
var fixedTime = time.Date(2026, 9, 5, 12, 0, 0, 123456789, time.UTC)

// validEnvelope returns a fully populated valid envelope on the given topic.
func validEnvelope(topic Topic, aggregateID string) Envelope {
	sp, ok := Lookup(topic)
	if !ok {
		panic("validEnvelope: unknown topic " + string(topic))
	}
	return Envelope{
		ID:            NewEventID().String(),
		Type:          string(topic),
		SchemaVersion: sp.SchemaVersion,
		Source:        "event-test",
		AggregateType: sp.AggregateType,
		AggregateID:   aggregateID,
		CorrelationID: "corr-1",
		CausationID:   "cause-1",
		OccurredAt:    fixedTime,
		Headers:       map[string]string{"x-trace": "abc"},
		Payload:       json.RawMessage(`{"amount":"10.00","n":1,"nested":{"z":true,"a":[1,"<b>&"]}}`),
	}
}
