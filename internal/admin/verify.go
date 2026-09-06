package admin

import (
	"context"
	"encoding/json"
	"time"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
)

// Approval is the verified view of an action returned by VerifyApproved.
type Approval struct {
	ActionID     ActionID
	Kind         Kind
	TargetType   string
	TargetID     string
	Status       Status
	Params       json.RawMessage
	ParamsHash   []byte
	RequiresDual bool
	ProposedBy   string
	ApprovedBy   *string
	ApprovedAt   *time.Time
	ExecutedAt   *time.Time
	ExpiresAt    time.Time
}

// ApprovalVerifier is the dependency other packages take to confirm an
// approval id before acting on it.
type ApprovalVerifier interface {
	VerifyApproved(ctx context.Context, q db.Querier, approvalID string, kind Kind, targetID string) (Approval, error)
}

// Verification failures. They are *errs.Error values, so errors.Is works
// and the codes reach the API boundary unchanged.
var (
	ErrApprovalNotFound   = errs.New(errs.CodeNotFound, "admin: approval not found")
	ErrApprovalMismatch   = errs.New(errs.CodeForbidden, "admin: approval does not cover this kind and target")
	ErrApprovalNotActive  = errs.New(errs.CodeForbidden, "admin: action is neither approved nor executed")
	ErrApprovalExpired    = errs.New(errs.CodeForbidden, "admin: approval has expired")
	ErrApprovalIncomplete = errs.New(errs.CodeForbidden, "admin: dual-control action lacks a distinct approver")
)

var _ ApprovalVerifier = (*Service)(nil)

// VerifyApproved returns the approval only if approvalID names an action of
// exactly kind and targetID whose status is APPROVED or EXECUTED, that has
// not expired, and (for dual-control kinds) that was approved by someone
// other than its proposer. It needs no principal but refuses agents.
//
// Callers that execute through Actions.Execute should verify inside the
// callback, where the action is still APPROVED.
func (s *Service) VerifyApproved(ctx context.Context, q db.Querier, approvalID string, kind Kind, targetID string) (Approval, error) {
	if err := refuseAgent(ctx); err != nil {
		return Approval{}, err
	}
	aid, err := ParseActionID(approvalID)
	if err != nil {
		return Approval{}, ErrApprovalNotFound
	}
	a, err := getAction(ctx, q, aid)
	if err != nil {
		if errs.CodeOf(err) == errs.CodeNotFound {
			return Approval{}, ErrApprovalNotFound
		}
		return Approval{}, err
	}
	if a.Kind != kind || a.TargetID != targetID || targetID == "" {
		return Approval{}, ErrApprovalMismatch
	}
	if a.Status != StatusApproved && a.Status != StatusExecuted {
		return Approval{}, ErrApprovalNotActive.WithField("status", string(a.Status))
	}
	if a.Expired(s.now()) {
		return Approval{}, ErrApprovalExpired
	}
	if a.RequiresDual && (a.ApprovedBy == nil || *a.ApprovedBy == a.ProposedBy) {
		return Approval{}, ErrApprovalIncomplete
	}
	return Approval{
		ActionID: a.ID, Kind: a.Kind, TargetType: a.TargetType, TargetID: a.TargetID, Status: a.Status,
		Params: a.Params, ParamsHash: a.ParamsHash, RequiresDual: a.RequiresDual,
		ProposedBy: a.ProposedBy, ApprovedBy: a.ApprovedBy, ApprovedAt: a.ApprovedAt, ExecutedAt: a.ExecutedAt,
		ExpiresAt: a.ExpiresAt,
	}, nil
}
