package event

import (
	"errors"
	"fmt"
	"sort"
)

// Topic names a domain event stream. Topic names follow the event type
// grammar ^[a-z0-9_.]+$ and are stable public identifiers: renaming one is a
// schema migration.
type Topic string

// Registered topics. The schema version and partition rule of each live in
// the registry below; topics_test.go asserts every constant is registered.
const (
	TopicLedgerTransactionPosted          Topic = "ledger.transaction.posted"
	TopicCapitalReservationCreated        Topic = "capital.reservation.created"
	TopicCapitalReservationConsumed       Topic = "capital.reservation.consumed"
	TopicCapitalReservationReleased       Topic = "capital.reservation.released"
	TopicCapitalReservationExpired        Topic = "capital.reservation.expired"
	TopicCapitalReservationLocked         Topic = "capital.reservation.locked"
	TopicCapitalHoldPlaced                Topic = "capital.hold.placed"
	TopicCapitalHoldReleased              Topic = "capital.hold.released"
	TopicCapitalEnvelopeChanged           Topic = "capital.envelope.changed"
	TopicCapitalEnvelopeCreated           Topic = "capital.envelope.created"
	TopicCapitalEnvelopeUpdated           Topic = "capital.envelope.updated"
	TopicCapitalEnvelopeStatusChanged     Topic = "capital.envelope.status_changed"
	TopicCapitalEnvelopePnLApplied        Topic = "capital.envelope.pnl_applied"
	TopicCapitalEnvelopeExhausted         Topic = "capital.envelope.exhausted"
	TopicCapitalEnvelopeUndeployed        Topic = "capital.envelope.undeployed"
	TopicFundingDepositTransitioned       Topic = "funding.deposit.transitioned"
	TopicIntentTransitioned               Topic = "intent.transitioned"
	TopicOrderTransitioned                Topic = "order.transitioned"
	TopicExecutionAttemptTransitioned     Topic = "execution.attempt.transitioned"
	TopicFillObserved                     Topic = "fill.observed"
	TopicReconciliationRecordTransitioned Topic = "reconciliation.record.transitioned"
	TopicGateTransitioned                 Topic = "gate.transitioned"
	TopicKillSwitchChanged                Topic = "killswitch.changed"
	TopicAuditEventAppended               Topic = "audit.event.appended"
	TopicSecurityEvent                    Topic = "security.event"
	TopicAgentRunCompleted                Topic = "agent.run.completed"
	TopicPredictionCommitted              Topic = "prediction.committed"
)

// Aggregate types used as partition keys. Declared here so producers and the
// registry agree on the spelling.
const (
	AggregateLedgerTransaction    = "ledger_transaction"
	AggregateReservation          = "reservation"
	AggregateCapitalEnvelope      = "capital_envelope"
	AggregateWithdrawalHold       = "withdrawal_hold"
	AggregateDeposit              = "deposit"
	AggregateIntent               = "intent"
	AggregateOrder                = "order"
	AggregateExecutionAttempt     = "execution_attempt"
	AggregateReconciliationRecord = "reconciliation_record"
	AggregateCapabilityGate       = "capability_gate"
	AggregateKillSwitch           = "kill_switch"
	AggregateAuditEvent           = "audit_event"
	AggregatePrincipal            = "principal"
	AggregateAgentRun             = "agent_run"
	AggregatePrediction           = "prediction"
)

// ErrUnknownTopic is the cause of the VALIDATION_FAILED error returned when
// a topic is not registered.
var ErrUnknownTopic = errors.New("event: unknown topic")

// KeyFunc derives the partition key of an envelope. It returns an error when
// the envelope cannot be keyed (wrong aggregate type, empty aggregate id).
type KeyFunc func(Envelope) (string, error)

// Spec describes a registered topic.
type Spec struct {
	// Topic is the registered name.
	Topic Topic
	// SchemaVersion is the version producers in this binary must emit.
	SchemaVersion int
	// AggregateType is the aggregate whose id keys the topic.
	AggregateType string
	// EventTypes lists the envelope types accepted on the topic. Empty means
	// exactly the topic name.
	EventTypes []string
	// Key is the partition-key rule.
	Key KeyFunc
}

// Accepts reports whether an envelope type is allowed on the topic.
func (s Spec) Accepts(eventType string) bool {
	if len(s.EventTypes) == 0 {
		return eventType == string(s.Topic)
	}
	for _, t := range s.EventTypes {
		if t == eventType {
			return true
		}
	}
	return false
}

// AggregateKey is the standard partition rule: the envelope must be about
// the given aggregate type and its aggregate id becomes the key, so every
// event of one aggregate lands on one partition in order.
func AggregateKey(aggregateType string) KeyFunc {
	return func(e Envelope) (string, error) {
		if e.AggregateType != aggregateType {
			return "", fmt.Errorf("event: aggregate_type %q is not %q", e.AggregateType, aggregateType)
		}
		if e.AggregateID == "" {
			return "", errors.New("event: aggregate_id is required for the partition key")
		}
		return e.AggregateID, nil
	}
}

func spec(t Topic, version int, aggregate string) Spec {
	return Spec{Topic: t, SchemaVersion: version, AggregateType: aggregate, Key: AggregateKey(aggregate)}
}

// registry is immutable after package initialisation.
var registry = func() map[Topic]Spec {
	specs := []Spec{
		spec(TopicLedgerTransactionPosted, 1, AggregateLedgerTransaction),
		spec(TopicCapitalReservationCreated, 1, AggregateReservation),
		spec(TopicCapitalReservationConsumed, 1, AggregateReservation),
		spec(TopicCapitalReservationReleased, 1, AggregateReservation),
		spec(TopicCapitalReservationExpired, 1, AggregateReservation),
		spec(TopicCapitalReservationLocked, 1, AggregateReservation),
		// Withdrawal holds are their own aggregate: a hold is keyed by hold
		// id, not by the reservation or account it constrains.
		spec(TopicCapitalHoldPlaced, 1, AggregateWithdrawalHold),
		spec(TopicCapitalHoldReleased, 1, AggregateWithdrawalHold),
		spec(TopicCapitalEnvelopeChanged, 1, AggregateCapitalEnvelope),
		spec(TopicCapitalEnvelopeCreated, 1, AggregateCapitalEnvelope),
		spec(TopicCapitalEnvelopeUpdated, 1, AggregateCapitalEnvelope),
		spec(TopicCapitalEnvelopeStatusChanged, 1, AggregateCapitalEnvelope),
		spec(TopicCapitalEnvelopePnLApplied, 1, AggregateCapitalEnvelope),
		spec(TopicCapitalEnvelopeExhausted, 1, AggregateCapitalEnvelope),
		spec(TopicCapitalEnvelopeUndeployed, 1, AggregateCapitalEnvelope),
		spec(TopicFundingDepositTransitioned, 1, AggregateDeposit),
		spec(TopicIntentTransitioned, 1, AggregateIntent),
		spec(TopicOrderTransitioned, 1, AggregateOrder),
		spec(TopicExecutionAttemptTransitioned, 1, AggregateExecutionAttempt),
		// Fills are keyed by their order so they interleave in order with
		// the order's own transitions on the consumer side.
		spec(TopicFillObserved, 1, AggregateOrder),
		spec(TopicReconciliationRecordTransitioned, 1, AggregateReconciliationRecord),
		spec(TopicGateTransitioned, 1, AggregateCapabilityGate),
		spec(TopicKillSwitchChanged, 1, AggregateKillSwitch),
		spec(TopicAuditEventAppended, 1, AggregateAuditEvent),
		spec(TopicSecurityEvent, 1, AggregatePrincipal),
		spec(TopicAgentRunCompleted, 1, AggregateAgentRun),
		spec(TopicPredictionCommitted, 1, AggregatePrediction),
	}
	m := make(map[Topic]Spec, len(specs))
	for _, s := range specs {
		if _, dup := m[s.Topic]; dup {
			panic("event: duplicate topic registration " + string(s.Topic))
		}
		m[s.Topic] = s
	}
	return m
}()

// Lookup returns the Spec of a registered topic.
func Lookup(topic Topic) (Spec, bool) {
	s, ok := registry[topic]
	return s, ok
}

// Topics returns every registered Spec sorted by name. The slice is a copy.
func Topics() []Spec {
	out := make([]Spec, 0, len(registry))
	for _, s := range registry {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Topic < out[j].Topic })
	return out
}

// String returns the topic name.
func (t Topic) String() string { return string(t) }

// Spec returns the topic's registration.
func (t Topic) Spec() (Spec, bool) { return Lookup(t) }

// Version returns the registered schema version, or 0 for an unknown topic.
func (t Topic) Version() int {
	if s, ok := registry[t]; ok {
		return s.SchemaVersion
	}
	return 0
}
