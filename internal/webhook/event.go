package webhook

import (
	"errors"
	"time"
)

// Identity is what every inbound provider event must expose after
// verification: enough to deduplicate, to time-bound replay, and to file the
// evidence row. Providers fill it from the verified payload, never from
// unauthenticated headers.
type Identity struct {
	// Provider is the adapter name (e.g. "stripe"); it is the provider_events
	// and inbox source key.
	Provider string
	// EventID is the provider's own event identifier; unique per provider.
	EventID string
	// EventType is the provider's event type string.
	EventType string
	// PublishedAt is when the provider created the event (UTC). Zero when the
	// provider does not say. It is evidence, not a replay bound: providers
	// redeliver old events for days.
	PublishedAt time.Time
	// SignedAt is the timestamp bound into the signature of this delivery
	// (UTC). Zero when the scheme carries none. It is the replay bound.
	SignedAt time.Time
	// Livemode reports whether the provider marked the event as live.
	Livemode bool
}

// Event is implemented by every provider-specific verified event type so the
// pipeline can be generic over the payload while still filing evidence.
type Event interface {
	WebhookIdentity() Identity
}

// Sentinel causes adapters wrap (with errs.Wrap so errors.Is still matches)
// to tell the pipeline how a request failed verification.
var (
	// ErrSignatureInvalid: the signature header is missing, malformed, uses
	// no supported scheme, or does not verify.
	ErrSignatureInvalid = errors.New("webhook: signature invalid")
	// ErrTimestampOutOfTolerance: the signature verified but its timestamp
	// is outside the replay tolerance.
	ErrTimestampOutOfTolerance = errors.New("webhook: signature timestamp outside tolerance")
	// ErrMalformed: the signature verified but the body is not the expected
	// document (bad JSON, wrong object, missing required field).
	ErrMalformed = errors.New("webhook: malformed payload")
)

// Disposition is what a Dispatcher reports for a verified, deduplicated
// event.
type Disposition string

// Dispositions.
const (
	// Applied: the event changed domain state (or was a legitimate no-op the
	// domain chose to acknowledge as processed).
	Applied Disposition = "PROCESSED"
	// Ignored: the event type or subject is not one this deployment acts on;
	// it is retained as evidence and never retried.
	Ignored Disposition = "IGNORED"
)
