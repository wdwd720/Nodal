package agents

import (
	"context"
	"time"

	"github.com/nodal/controlplane/internal/agent"
)

// EventKind is something that happened to an agent that another domain may
// want to tell somebody about.
//
// This package deliberately imports neither internal/notifications nor
// internal/activity. It publishes; what a notification says, whether one is
// sent at all, and which activity feed it lands in are those packages'
// decisions, and an agent management service that reached into them would make
// "was the user told" a property of the lifecycle rather than of the product.
type EventKind string

// Agent lifecycle events. The set is closed and every one of them corresponds
// to a row this package wrote.
const (
	// EventAgentCreated: an agent exists, with a grant behind it. It is not
	// enabled and nothing evaluates it.
	EventAgentCreated EventKind = "AGENT_CREATED"
	// EventAgentEnabled: the owner turned it on. On a deployment with no
	// evaluator this changes what is PERMITTED, not what is running, and the
	// event carries the runtime state so a notification cannot imply otherwise.
	EventAgentEnabled EventKind = "AGENT_ENABLED"
	// EventAgentPaused: an open agent_pauses row exists. Raised for an owner
	// pause and for an operator pause; ActorType tells them apart.
	EventAgentPaused EventKind = "AGENT_PAUSED"
	// EventAgentResumed: the pause was closed by a person.
	EventAgentResumed EventKind = "AGENT_RESUMED"
	// EventAgentDisabled: authority was revoked. Terminal.
	EventAgentDisabled EventKind = "AGENT_DISABLED"
	// EventAgentArchived: the grant was archived and the agent leaves the
	// default list. No lifecycle state changed.
	EventAgentArchived EventKind = "AGENT_ARCHIVED"
)

var allEventKinds = []EventKind{
	EventAgentCreated, EventAgentEnabled, EventAgentPaused,
	EventAgentResumed, EventAgentDisabled, EventAgentArchived,
}

// EventKinds returns every declared kind, so a consumer can assert it handles
// all of them rather than discovering a new one in production.
func EventKinds() []EventKind { return append([]EventKind(nil), allEventKinds...) }

// Valid reports whether k is declared.
func (k EventKind) Valid() bool {
	for _, v := range allEventKinds {
		if v == k {
			return true
		}
	}
	return false
}

// String renders the kind.
func (k EventKind) String() string { return string(k) }

// Event is one thing that happened to one agent.
//
// It carries the agent's runtime state at the moment of the event on purpose.
// A notification that says "your agent is now running" when no evaluator is
// deployed would be the overstatement goal §17 forbids, and the only way to
// stop a downstream consumer writing that sentence is to hand it the truth.
type Event struct {
	Kind      EventKind
	AgentID   agent.AgentID
	AccountID string
	AgentName string
	// State is the agent's lifecycle state after the event.
	State agent.State
	// Runtime is what is actually evaluating the agent, which on a deployment
	// with no agent worker is NOT_DEPLOYED regardless of State.
	Runtime RuntimeStatus
	// ActorType is USER for an owner action and OPERATOR for an admin one.
	ActorType string
	ActorID   string
	// Reason is the human-supplied reason, where the action has one.
	Reason        string
	CorrelationID string
	OccurredAt    time.Time
}

// Publisher receives agent events. It is satisfied by whatever the composition
// root wires: an activity writer, a notification producer, a stream fan-out, or
// nothing at all.
//
// A publisher's error is logged and swallowed by the caller, never returned:
// an agent that could not be paused because a notification failed would be a
// control defeated by a mailbox.
type Publisher interface {
	Publish(ctx context.Context, e Event) error
}

// PublisherFunc adapts a function to Publisher.
type PublisherFunc func(ctx context.Context, e Event) error

// Publish calls f.
func (f PublisherFunc) Publish(ctx context.Context, e Event) error { return f(ctx, e) }
