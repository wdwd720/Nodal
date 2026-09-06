package execution

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// EventSource is the outbox source of every event this package emits.
const EventSource = "execution"

// Audit actions written by this package.
const (
	AuditOrderCreated      = "order.created"
	AuditOrderTransition   = "order.transition"
	AuditFillRecorded      = "fill.recorded"
	AuditFillPosted        = "fill.posted"
	AuditFillPositionApply = "fill.position_applied"
	AuditAttemptCreated    = "attempt.created"
	AuditAttemptTransition = "attempt.transition"
)

// EventEmitter appends envelopes to the transactional outbox inside the
// caller's transaction. *event.Outbox satisfies it.
type EventEmitter interface {
	Enqueue(ctx context.Context, tx pgx.Tx, topic string, events ...event.Envelope) error
}

// OrderTransitionedEvent is the payload of order.transitioned.
type OrderTransitionedEvent struct {
	OrderID              string         `json:"order_id"`
	IntentID             string         `json:"intent_id"`
	PlanID               string         `json:"plan_id"`
	AccountID            string         `json:"account_id"`
	TransitionID         string         `json:"transition_id"`
	From                 OrderStatus    `json:"from"`
	To                   OrderStatus    `json:"to"`
	Reason               string         `json:"reason,omitempty"`
	RejectionCode        string         `json:"rejection_code,omitempty"`
	EvidenceRef          string         `json:"evidence_ref,omitempty"`
	FilledInputQuantity  money.Quantity `json:"filled_input_quantity"`
	FilledOutputQuantity money.Quantity `json:"filled_output_quantity"`
	OccurredAt           time.Time      `json:"occurred_at"`
}

// AttemptTransitionedEvent is the payload of execution.attempt.transitioned.
type AttemptTransitionedEvent struct {
	AttemptID   string        `json:"attempt_id"`
	OrderID     string        `json:"order_id"`
	PlanID      string        `json:"plan_id"`
	AttemptNo   int32         `json:"attempt_no"`
	From        AttemptStatus `json:"from"`
	To          AttemptStatus `json:"to"`
	TxSignature string        `json:"tx_signature,omitempty"`
	Finality    FinalityLevel `json:"finality,omitempty"`
	Reason      string        `json:"reason,omitempty"`
	OccurredAt  time.Time     `json:"occurred_at"`
}

// FillObservedEvent is the payload of fill.observed. It is keyed by the
// order so consumers see fills in order with the order's transitions.
type FillObservedEvent struct {
	FillID         string         `json:"fill_id"`
	OrderID        string         `json:"order_id"`
	AttemptID      string         `json:"attempt_id,omitempty"`
	AccountID      string         `json:"account_id"`
	Venue          string         `json:"venue"`
	ExternalFillID string         `json:"external_fill_id"`
	TxSignature    string         `json:"tx_signature,omitempty"`
	InputAssetID   string         `json:"input_asset_id"`
	InputQuantity  money.Quantity `json:"input_quantity"`
	OutputAssetID  string         `json:"output_asset_id"`
	OutputQuantity money.Quantity `json:"output_quantity"`
	Source         FillSource     `json:"source"`
	Finality       FinalityLevel  `json:"finality"`
	OrderStatus    OrderStatus    `json:"order_status"`
	ObservedAt     time.Time      `json:"observed_at"`
	OccurredAt     time.Time      `json:"occurred_at"`
}

// envelope builds a validated outbox envelope on a registered topic.
func envelope(topic event.Topic, aggregateType, aggregateID, correlationID, causationID string, occurredAt time.Time, payload any) (event.Envelope, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return event.Envelope{}, errs.Wrap(err, errs.CodeInternal, "execution: encode event payload")
	}
	return event.Envelope{
		ID:            event.NewEventID().String(),
		Type:          string(topic),
		SchemaVersion: topic.Version(),
		Source:        EventSource,
		AggregateType: aggregateType,
		AggregateID:   aggregateID,
		CorrelationID: correlationID,
		CausationID:   causationID,
		OccurredAt:    occurredAt.UTC(),
		Payload:       body,
	}, nil
}

// auditActor normalises the actor of an audit or transition record: SYSTEM
// / "execution" when the caller named none.
func auditActor(actorType, actorID string) (string, string) {
	if actorType == "" {
		actorType = string(security.ActorSystem)
	}
	if actorID == "" {
		actorID = EventSource
	}
	return actorType, actorID
}

// appendAudit writes one audit event on the account stream.
func appendAudit(ctx context.Context, w audit.Writer, tx pgx.Tx, accountID, actorType, actorID, action, resourceType, resourceID, reason, evidenceRef, correlationID, requestID string, payload any, at time.Time) error {
	if w == nil {
		return errs.New(errs.CodeInternal, "execution: audit writer is not configured")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "execution: encode audit payload")
	}
	actorType, actorID = auditActor(actorType, actorID)
	_, err = w.Append(ctx, tx, audit.Event{
		Stream: audit.AccountStream(accountID), ActorType: actorType, ActorID: actorID,
		Action: action, ResourceType: resourceType, ResourceID: resourceID,
		Reason: reason, EvidenceRef: evidenceRef, CorrelationID: correlationID, RequestID: requestID,
		Payload: body, OccurredAt: at.UTC(),
	})
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "execution: append audit event")
	}
	return nil
}
