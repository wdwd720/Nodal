package workflows

import (
	"context"
	"log/slog"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/observability"
)

// codeValidationFailed is spelled out here so workflow code (which must not
// depend on anything mutable) can name it without importing more than it
// needs.
const codeValidationFailed = errs.CodeValidationFailed

// RecordState is what a workflow may know about a reconciliation record. It
// carries no amounts: the difference between expected and observed is
// financial truth and stays in Postgres (PART 116).
type RecordState struct {
	RecordID string `json:"record_id"`
	Kind     string `json:"kind"`
	Status   string `json:"status"`
	// Resolved is true for MATCHED, RESOLVED_AUTOMATIC and RESOLVED_MANUAL.
	Resolved bool `json:"resolved"`
	// Material means the mismatch is large enough to require approval to
	// resolve (PART 51).
	Material bool `json:"material"`
	// BlocksNewRisk means policy says no new risk may be taken while this
	// record is open.
	BlocksNewRisk bool   `json:"blocks_new_risk"`
	AccountID     string `json:"account_id,omitempty"`
}

// DescribeRecordInput names the record to read.
type DescribeRecordInput struct {
	RecordID      string `json:"record_id"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

// MarkRecordInput moves a record to a workflow-driven status.
type MarkRecordInput struct {
	RecordID      string `json:"record_id"`
	Reason        string `json:"reason"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

// EscalationNotice is one rung's notification.
type EscalationNotice struct {
	RecordID      string `json:"record_id"`
	Tier          string `json:"tier"`
	Severity      string `json:"severity"`
	Status        string `json:"status"`
	Kind          string `json:"kind"`
	Material      bool   `json:"material"`
	BlocksNewRisk bool   `json:"blocks_new_risk"`
	AccountID     string `json:"account_id,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

// ContainmentRequest asks for new risk to be stopped while a material
// mismatch is unresolved.
type ContainmentRequest struct {
	RecordID      string `json:"record_id"`
	AccountID     string `json:"account_id,omitempty"`
	Kind          string `json:"kind"`
	Reason        string `json:"reason"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

// ReconciliationRecords is the record persistence the escalation workflow
// needs. internal/reconciliation's repository satisfies it through an adapter
// in cmd/workflow-worker; this package deliberately does not import that
// package, so the two can be written and changed independently.
//
// Every method must be idempotent: Temporal retries, and marking a record
// INVESTIGATING twice must leave one record in one state, not two
// transitions' worth of noise.
type ReconciliationRecords interface {
	Describe(ctx context.Context, recordID string) (RecordState, error)
	MarkInvestigating(ctx context.Context, recordID, reason string) error
	MarkEscalated(ctx context.Context, recordID, reason string) error
}

// ContainmentController raises the operational stop that a material,
// risk-blocking mismatch justifies, and returns a reference to what it
// raised.
//
// It stops NEW RISK. It must never stop observation, settlement,
// reconciliation, ledger posting or cancellation: those are the operations
// that find out what actually happened, and an incident is exactly when they
// matter most (PART 52). internal/killswitch's action-class matrix is what
// enforces that; the implementation simply chooses a NEW_RISK-class switch.
type ContainmentController interface {
	Contain(ctx context.Context, req ContainmentRequest) (string, error)
}

// EscalationActivities are the escalation workflow's I/O.
type EscalationActivities struct {
	records     ReconciliationRecords
	alert       OperatorAlerter
	containment ContainmentController
	log         *slog.Logger
}

// NewEscalationActivities wires the escalation activities. Containment may be
// nil, in which case a containment request fails loudly rather than silently
// doing nothing: an operator must know that the stop did not happen.
func NewEscalationActivities(r ReconciliationRecords, a OperatorAlerter, c ContainmentController, log *slog.Logger) (*EscalationActivities, error) {
	if r == nil || a == nil {
		return nil, errs.New(errs.CodeInternal, "workflows: escalation activities need a record store and an alerter")
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &EscalationActivities{records: r, alert: a, containment: c, log: log}, nil
}

// DescribeRecord reads one reconciliation record.
func (a *EscalationActivities) DescribeRecord(ctx context.Context, in DescribeRecordInput) (RecordState, error) {
	if in.RecordID == "" {
		return RecordState{}, ActivityError(errs.New(errs.CodeValidationFailed, "workflows: record id is required"))
	}
	ctx = observability.WithCorrelationID(ctx, in.CorrelationID)
	st, err := a.records.Describe(ctx, in.RecordID)
	if err != nil {
		return RecordState{}, ActivityError(err)
	}
	st.RecordID = in.RecordID
	return st, nil
}

// MarkInvestigating records that the escalation is under way.
func (a *EscalationActivities) MarkInvestigating(ctx context.Context, in MarkRecordInput) error {
	if in.RecordID == "" {
		return ActivityError(errs.New(errs.CodeValidationFailed, "workflows: record id is required"))
	}
	ctx = observability.WithCorrelationID(ctx, in.CorrelationID)
	return ActivityError(a.records.MarkInvestigating(ctx, in.RecordID, in.Reason))
}

// MarkEscalated records that the ladder ran out.
func (a *EscalationActivities) MarkEscalated(ctx context.Context, in MarkRecordInput) error {
	if in.RecordID == "" {
		return ActivityError(errs.New(errs.CodeValidationFailed, "workflows: record id is required"))
	}
	ctx = observability.WithCorrelationID(ctx, in.CorrelationID)
	return ActivityError(a.records.MarkEscalated(ctx, in.RecordID, in.Reason))
}

// NotifyEscalation tells one tier about the record.
func (a *EscalationActivities) NotifyEscalation(ctx context.Context, n EscalationNotice) error {
	if n.RecordID == "" {
		return ActivityError(errs.New(errs.CodeValidationFailed, "workflows: record id is required"))
	}
	ctx = observability.WithCorrelationID(ctx, n.CorrelationID)
	a.log.WarnContext(ctx, "workflows: reconciliation escalation",
		"record_id", n.RecordID, "tier", n.Tier, "severity", n.Severity, "status", n.Status, "material", n.Material)
	return ActivityError(a.alert.Alert(ctx, OperatorAlert{
		Kind:          "RECONCILIATION_ESCALATION",
		Severity:      n.Severity,
		Subject:       "reconciliation record " + n.Status + " is unresolved",
		ResourceType:  "reconciliation_record",
		ResourceID:    n.RecordID,
		Detail:        "escalation tier " + n.Tier + " for a " + n.Kind + " record",
		CorrelationID: n.CorrelationID,
		Fields: map[string]string{
			"tier": n.Tier, "status": n.Status, "kind": n.Kind,
			"material": boolText(n.Material), "blocks_new_risk": boolText(n.BlocksNewRisk),
			"account_id": n.AccountID,
		},
	}))
}

// RequestContainment stops new risk while a material mismatch is unresolved
// and returns the reference of what was raised.
func (a *EscalationActivities) RequestContainment(ctx context.Context, req ContainmentRequest) (string, error) {
	if req.RecordID == "" {
		return "", ActivityError(errs.New(errs.CodeValidationFailed, "workflows: record id is required"))
	}
	ctx = observability.WithCorrelationID(ctx, req.CorrelationID)
	if a.containment == nil {
		// Failing here is deliberate. A silent no-op would leave an operator
		// believing trading was stopped when it was not.
		return "", ActivityError(errs.New(errs.CodeUnsupported,
			"workflows: containment was requested but no containment controller is configured"))
	}
	ref, err := a.containment.Contain(ctx, req)
	if err != nil {
		return "", ActivityError(err)
	}
	a.log.WarnContext(ctx, "workflows: containment raised for an unresolved material mismatch",
		"record_id", req.RecordID, "account_id", req.AccountID, "containment_ref", ref)
	return ref, nil
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
