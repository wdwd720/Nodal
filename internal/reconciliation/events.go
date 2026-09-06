package reconciliation

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/security"
)

// EventSource is the outbox source of every event this package emits.
const EventSource = "reconciliation"

// Audit actions written by this package. They all land on the account stream
// when the record is account-scoped and on the system stream otherwise.
const (
	AuditRecordOpened       = "reconciliation.record.opened"
	AuditRecordTransitioned = "reconciliation.record.transitioned"
	AuditRepairPosted       = "reconciliation.repair.posted"
	AuditRecoveryAdopted    = "reconciliation.recovery.adopted"
	AuditRecoveryAbsent     = "reconciliation.recovery.proven_absent"
)

// RecordTransitionedEvent is the payload of reconciliation.record.transitioned.
type RecordTransitionedEvent struct {
	RecordID      string    `json:"record_id"`
	Kind          Kind      `json:"kind"`
	Mode          Mode      `json:"mode"`
	ScopeType     string    `json:"scope_type"`
	ScopeID       string    `json:"scope_id"`
	AccountID     string    `json:"account_id,omitempty"`
	AssetID       string    `json:"asset_id,omitempty"`
	From          Status    `json:"from"`
	To            Status    `json:"to"`
	Material      bool      `json:"material"`
	BlocksNewRisk bool      `json:"blocks_new_risk"`
	ActorType     string    `json:"actor_type"`
	ActorID       string    `json:"actor_id"`
	Reason        string    `json:"reason,omitempty"`
	EvidenceRef   string    `json:"evidence_ref,omitempty"`
	OccurredAt    time.Time `json:"occurred_at"`
}

// announce writes the outbox envelope and the audit event for one status
// change, inside the caller's transaction.
func (r *Repository) announce(ctx context.Context, tx pgx.Tx, rec Record, from, to Status, actor Actor, reason, evidenceRef string, now time.Time) error {
	payload := RecordTransitionedEvent{
		RecordID: rec.ID.String(), Kind: rec.Kind, Mode: rec.Mode, ScopeType: rec.ScopeType, ScopeID: rec.ScopeID,
		From: from, To: to, Material: rec.Material, BlocksNewRisk: rec.BlocksNewRisk,
		ActorType: string(actor.Type), ActorID: actor.ID, Reason: reason, EvidenceRef: evidenceRef, OccurredAt: now,
	}
	if !rec.AccountID.IsZero() {
		payload.AccountID = rec.AccountID.String()
	}
	if !rec.AssetID.IsZero() {
		payload.AssetID = rec.AssetID.String()
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "reconciliation: encode event payload")
	}
	env := event.Envelope{
		ID:            event.NewEventID().String(),
		Type:          string(event.TopicReconciliationRecordTransitioned),
		SchemaVersion: event.TopicReconciliationRecordTransitioned.Version(),
		Source:        EventSource,
		AggregateType: event.AggregateReconciliationRecord,
		AggregateID:   rec.ID.String(),
		CorrelationID: rec.CorrelationID,
		OccurredAt:    now.UTC(),
		Payload:       body,
	}
	if err := r.emit.Enqueue(ctx, tx, string(event.TopicReconciliationRecordTransitioned), env); err != nil {
		return dbErr("enqueue reconciliation event", err)
	}
	action := AuditRecordTransitioned
	if from == StatusNone {
		action = AuditRecordOpened
	}
	auditPayload := map[string]any{
		"record_id": rec.ID.String(), "kind": string(rec.Kind), "mode": string(rec.Mode),
		"scope_type": rec.ScopeType, "scope_id": rec.ScopeID, "from": string(from), "to": string(to),
		"material": rec.Material, "blocks_new_risk": rec.BlocksNewRisk,
		"expected": rec.Expected, "observed": rec.Observed, "difference": rec.Difference,
		"approval_id": rec.ApprovalID, "compensating_journal_transaction_id": rec.CompensatingJournalTxID,
	}
	return r.appendAudit(ctx, tx, rec, actor, action, reason, evidenceRef, auditPayload, now)
}

// appendAudit writes one audit event about a record. Account-scoped records
// go on the account's stream so an account's full history is one chain;
// everything else goes on the system stream.
func (r *Repository) appendAudit(ctx context.Context, tx pgx.Tx, rec Record, actor Actor, action, reason, evidenceRef string, payload any, at time.Time) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "reconciliation: encode audit payload")
	}
	stream := audit.SystemStream
	if !rec.AccountID.IsZero() {
		stream = audit.AccountStream(rec.AccountID.String())
	}
	actor = actor.normalize()
	if _, err := r.audit.Append(ctx, tx, audit.Event{
		Stream:        stream,
		ActorType:     string(actor.Type),
		ActorID:       actor.ID,
		Action:        action,
		ResourceType:  "reconciliation_record",
		ResourceID:    rec.ID.String(),
		Reason:        reason,
		EvidenceRef:   evidenceRef,
		CorrelationID: rec.CorrelationID,
		Payload:       body,
		OccurredAt:    at.UTC(),
	}); err != nil {
		return errs.Wrap(err, errs.CodeInternal, "reconciliation: append audit event")
	}
	return nil
}

// AppendEvidence writes an extra audit event about a record without changing
// its status: the recovery trail (what was queried, what came back, what was
// adopted) that PART 49 step 13 requires.
func (r *Repository) AppendEvidence(ctx context.Context, tx pgx.Tx, rec Record, actor Actor, action, reason, evidenceRef string, payload any) error {
	if err := r.check(); err != nil {
		return err
	}
	return r.appendAudit(ctx, tx, rec, actor, action, reason, evidenceRef, payload, r.clk.Now())
}

// operatorActor builds an Actor from the principal in ctx. It refuses agents
// and unauthenticated callers, so no resolution path can be reached without a
// named human or service.
func operatorActor(ctx context.Context) (Actor, error) {
	p, ok := security.PrincipalFrom(ctx)
	if !ok {
		return Actor{}, errs.New(errs.CodeUnauthenticated, "reconciliation: no principal in context")
	}
	a := Actor{Type: p.ActorType, ID: p.SubjectID}
	if err := a.normalize().validate(); err != nil {
		return Actor{}, err
	}
	return a.normalize(), nil
}
