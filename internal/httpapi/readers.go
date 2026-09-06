package httpapi

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital/buyingpower"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/observability"
)

// adminApprovalVerifier is the part of internal/admin the kill-switch release
// path needs.
type adminApprovalVerifier interface {
	VerifyApproved(ctx context.Context, q db.Querier, approvalID string, kind admin.Kind, targetID string) (admin.Approval, error)
}

func adminKind(s string) admin.Kind { return admin.Kind(s) }

// The two readers below are the buying-power engine's view of the authority
// plane. They are wiring, not policy: which switch blocks which action class
// is decided by internal/killswitch's matrix, and whether a reconciliation
// record blocks new risk is a column the reconciliation engine sets. Nothing
// here interprets either.

// KillSwitchReader answers buyingpower.KillSwitchReader from the persisted
// switches through internal/killswitch.
type KillSwitchReader struct {
	ctl    *killswitch.Controller
	policy killswitch.Policy
}

// NewKillSwitchReader wraps the controller's active-switch read.
func NewKillSwitchReader(ctl *killswitch.Controller, policy killswitch.Policy) *KillSwitchReader {
	return &KillSwitchReader{ctl: ctl, policy: policy}
}

// Active reports the switches that are live for accountID, each labeled with
// whether it blocks new risk and whether it blocks withdrawal. Both labels
// come from killswitch.Blocks, the same matrix every guarded operation uses.
func (r *KillSwitchReader) Active(ctx context.Context, q db.Querier, accountID accounts.AccountID) ([]buyingpower.KillSwitchState, error) {
	active, err := r.ctl.Active(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make([]buyingpower.KillSwitchState, 0, len(active))
	for _, sw := range active {
		newRisk := killswitch.Action{Class: killswitch.NewRisk, AccountID: accountID.String()}
		withdraw := killswitch.Action{Class: killswitch.Withdraw, AccountID: accountID.String()}
		blocksNewRisk := killswitch.Blocks(sw, newRisk, r.policy)
		blocksWithdrawal := killswitch.Blocks(sw, withdraw, r.policy)
		if !blocksNewRisk && !blocksWithdrawal {
			continue
		}
		out = append(out, buyingpower.KillSwitchState{
			Kind:             string(sw.Kind),
			ScopeID:          sw.ScopeID,
			Reason:           sw.Reason,
			BlocksNewRisk:    blocksNewRisk,
			BlocksWithdrawal: blocksWithdrawal,
		})
	}
	return out, nil
}

// ReconciliationBlockReader answers buyingpower.ReconciliationBlockReader from
// the reconciliation_records table. It reports the records the reconciliation
// engine already marked as blocking; it never decides materiality.
type ReconciliationBlockReader struct{}

// NewReconciliationBlockReader returns the reader.
func NewReconciliationBlockReader() *ReconciliationBlockReader { return &ReconciliationBlockReader{} }

// Blocks returns the unresolved, blocking reconciliation records for the
// account.
func (ReconciliationBlockReader) Blocks(ctx context.Context, q db.Querier, accountID accounts.AccountID) ([]buyingpower.ReconciliationBlock, error) {
	rows, err := q.Query(ctx, `
		SELECT id::text, kind, coalesce(resolution_reason, scope_type || ':' || scope_id)
		FROM reconciliation_records
		WHERE account_id = $1
		  AND blocks_new_risk
		  AND status IN ('OPEN','MISMATCH','INVESTIGATING','ESCALATED')
		ORDER BY opened_at`, accountID)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "internal error")
	}
	defer rows.Close()
	var out []buyingpower.ReconciliationBlock
	for rows.Next() {
		var b buyingpower.ReconciliationBlock
		if err := rows.Scan(&b.RecordID, &b.Kind, &b.Detail); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "internal error")
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "internal error")
	}
	return out, nil
}

// ApprovalVerifier adapts internal/admin's approval read to the shape
// internal/killswitch expects for releasing a SEVERE switch.
type ApprovalVerifier struct {
	verify func(ctx context.Context, q db.Querier, approvalID, kind, targetID string) (killswitch.Approval, error)
}

// NewApprovalVerifier wraps an admin service.
func NewApprovalVerifier(v adminApprovalVerifier) *ApprovalVerifier {
	return &ApprovalVerifier{
		verify: func(ctx context.Context, q db.Querier, approvalID, kind, targetID string) (killswitch.Approval, error) {
			a, err := v.VerifyApproved(ctx, q, approvalID, adminKind(kind), targetID)
			if err != nil {
				return killswitch.Approval{}, err
			}
			out := killswitch.Approval{
				ID:         a.ActionID.String(),
				Kind:       string(a.Kind),
				TargetID:   a.TargetID,
				ProposedBy: a.ProposedBy,
				ExpiresAt:  a.ExpiresAt,
			}
			if a.ApprovedBy != nil {
				out.ApprovedBy = *a.ApprovedBy
			}
			if a.ApprovedAt != nil {
				out.ApprovedAt = *a.ApprovedAt
			}
			return out, nil
		},
	}
}

// VerifyApproved implements killswitch.ApprovalVerifier.
func (a *ApprovalVerifier) VerifyApproved(ctx context.Context, q db.Querier, approvalID, kind, targetID string) (killswitch.Approval, error) {
	return a.verify(ctx, q, approvalID, kind, targetID)
}

// AuditAppender adapts the control plane's audit writer (internal/audit) to
// the narrower appenders internal/gates and internal/killswitch declare. It is
// a shape change only: the stream, actor, action, resource, reason and payload
// pass through untouched, and the request and correlation ids are taken from
// the context so an operator action can be traced back to its HTTP request.
type AuditAppender struct{ w audit.Writer }

// NewAuditAppender wraps an audit writer.
func NewAuditAppender(w audit.Writer) *AuditAppender { return &AuditAppender{w: w} }

// AppendGate implements gates.AuditAppender.
func (a *AuditAppender) AppendGate(ctx context.Context, tx pgx.Tx, e gates.AuditEvent) error {
	_, err := a.w.Append(ctx, tx, audit.Event{
		Stream:        e.Stream,
		ActorType:     e.ActorType,
		ActorID:       e.ActorID,
		Action:        e.Action,
		ResourceType:  e.ResourceType,
		ResourceID:    e.ResourceID,
		Reason:        e.Reason,
		EvidenceRef:   e.EvidenceRef,
		PolicyVersion: e.PolicyVersion,
		Payload:       e.Payload,
		OccurredAt:    e.OccurredAt,
		RequestID:     observability.RequestID(ctx),
		CorrelationID: observability.CorrelationID(ctx),
	})
	return err
}

// AppendKillSwitch implements killswitch.AuditAppender.
func (a *AuditAppender) AppendKillSwitch(ctx context.Context, tx pgx.Tx, e killswitch.AuditEvent) error {
	_, err := a.w.Append(ctx, tx, audit.Event{
		Stream:        e.Stream,
		ActorType:     e.ActorType,
		ActorID:       e.ActorID,
		Action:        e.Action,
		ResourceType:  e.ResourceType,
		ResourceID:    e.ResourceID,
		Reason:        e.Reason,
		EvidenceRef:   e.EvidenceRef,
		PolicyVersion: e.PolicyVersion,
		Payload:       e.Payload,
		OccurredAt:    e.OccurredAt,
		RequestID:     observability.RequestID(ctx),
		CorrelationID: observability.CorrelationID(ctx),
	})
	return err
}

// GateAudit returns the gates.AuditAppender view of a.
func (a *AuditAppender) GateAudit() gates.AuditAppender { return gateAudit{a} }

// KillSwitchAudit returns the killswitch.AuditAppender view of a.
func (a *AuditAppender) KillSwitchAudit() killswitch.AuditAppender { return killSwitchAudit{a} }

type gateAudit struct{ a *AuditAppender }

func (g gateAudit) Append(ctx context.Context, tx pgx.Tx, e gates.AuditEvent) error {
	return g.a.AppendGate(ctx, tx, e)
}

type killSwitchAudit struct{ a *AuditAppender }

func (k killSwitchAudit) Append(ctx context.Context, tx pgx.Tx, e killswitch.AuditEvent) error {
	return k.a.AppendKillSwitch(ctx, tx, e)
}
