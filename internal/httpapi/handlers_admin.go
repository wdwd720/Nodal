package httpapi

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/security"
)

// minReasonLength mirrors internal/admin: every operator action names a reason
// and the reason is recorded, never inferred.
const minReasonLength = 8

// GetAdminAccounts searches accounts for operators.
func (s *Server) GetAdminAccounts(ctx context.Context, request api.GetAdminAccountsRequestObject) (api.GetAdminAccountsResponseObject, error) {
	if s.opts.Ports.Accounts == nil {
		return nil, errNotWired("accounts")
	}
	q := ""
	if request.Params.Q != nil {
		q = strings.TrimSpace(*request.Params.Q)
	}
	cursor := ""
	if request.Params.Cursor != nil {
		cursor = *request.Params.Cursor
	}
	page, err := s.opts.Ports.Accounts.Search(ctx, q, cursor,
		pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	return api.GetAdminAccounts200JSONResponse(api.AccountPage{
		Items:      toAPIAccounts(page.Items),
		NextCursor: nextCursor(page.NextCursor),
	}), nil
}

// PostAdminAccountsAccountIdStatus freezes, restricts or reactivates an
// account. There is no balance-editing endpoint and this is not one: the
// status machine of internal/accounts is the whole effect, and a frozen
// account still settles and reconciles (PART 194).
func (s *Server) PostAdminAccountsAccountIdStatus(ctx context.Context, request api.PostAdminAccountsAccountIdStatusRequestObject) (api.PostAdminAccountsAccountIdStatusResponseObject, error) {
	if s.opts.Ports.Accounts == nil {
		return nil, errNotWired("accounts")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	p, ok := security.PrincipalFrom(ctx)
	if !ok {
		return nil, errs.New(errs.CodeUnauthenticated, "authentication is required")
	}
	accountID, err := accounts.ParseAccountID(request.AccountId.String())
	if err != nil || accountID.IsZero() {
		return nil, validationError("accountId", "accountId must be a canonical UUID")
	}
	if len(strings.TrimSpace(request.Body.Reason)) < minReasonLength {
		return nil, validationError("reason", "reason must be at least 8 characters")
	}
	to := accounts.Status(request.Body.To)
	if !to.Valid() {
		return nil, validationError("to", "unknown account status")
	}
	ch := accounts.StatusChange{
		To:            to,
		ActorType:     string(p.ActorType),
		ActorID:       p.SubjectID,
		Reason:        request.Body.Reason,
		CorrelationID: observability.CorrelationID(ctx),
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.Account, commandMeta, error) {
			a, terr := s.opts.Ports.Accounts.Transition(ctx, accountID, ch)
			if terr != nil {
				return api.Account{}, commandMeta{}, terr
			}
			return toAPIAccount(a), commandMeta{
				Status: http.StatusOK, ResourceType: "account", ResourceID: a.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdminAccountsAccountIdStatus200JSONResponse(res.Value), nil
}

// GetAdminGates lists the production capability gates for this environment,
// each with the five-condition activation verdict computed by internal/gates.
func (s *Server) GetAdminGates(ctx context.Context, _ api.GetAdminGatesRequestObject) (api.GetAdminGatesResponseObject, error) {
	if s.opts.Ports.Gates == nil {
		return nil, errNotWired("capability gates")
	}
	list, err := s.opts.Ports.Gates.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]api.CapabilityGate, 0, len(list))
	for _, g := range list {
		out = append(out, toAPIGate(g))
	}
	return api.GetAdminGates200JSONResponse(out), nil
}

// GetAdminGatesCapabilityHistory lists a gate's recorded transitions. The
// rows are written by the transition functions in the same statement as the
// state change, so this is the history, not a reconstruction of it; the
// console uses it to name who moved a gate, which the gate row itself cannot
// say for a SANDBOX entry (no approval chain is written for one).
func (s *Server) GetAdminGatesCapabilityHistory(ctx context.Context, request api.GetAdminGatesCapabilityHistoryRequestObject) (api.GetAdminGatesCapabilityHistoryResponseObject, error) {
	if s.opts.Ports.Gates == nil {
		return nil, errNotWired("capability gates")
	}
	capability := gates.Capability(request.Capability)
	if !capability.Valid() {
		return nil, validationError("capability", "unknown capability")
	}
	rows, err := s.opts.Ports.Gates.History(ctx, capability)
	if err != nil {
		return nil, err
	}
	out := make([]api.CapabilityGateTransition, 0, len(rows))
	for _, t := range rows {
		out = append(out, toAPIGateTransition(t))
	}
	return api.GetAdminGatesCapabilityHistory200JSONResponse(out), nil
}

func toAPIGateTransition(t gates.Transition) api.CapabilityGateTransition {
	evidence := ""
	if len(t.EvidenceHash) > 0 {
		evidence = hex.EncodeToString(t.EvidenceHash)
	}
	return api.CapabilityGateTransition{
		TransitionId: uuid.MustParse(t.ID.String()),
		From:         api.CapabilityGateTransitionFrom(t.From),
		To:           api.CapabilityGateTransitionTo(t.To),
		ActorType:    string(t.ActorType),
		ActorId:      t.ActorID,
		Reason:       t.Reason,
		EvidenceHash: &evidence,
		OccurredAt:   t.OccurredAt.UTC(),
		Sandbox:      t.To == gates.StateSandbox,
	}
}

// PostAdminGatesCapabilityAction drives the gate state machine. Dual control,
// distinct approvers, evidence references and step-up are enforced by
// internal/gates; one environment variable can never activate a capability.
func (s *Server) PostAdminGatesCapabilityAction(ctx context.Context, request api.PostAdminGatesCapabilityActionRequestObject) (api.PostAdminGatesCapabilityActionResponseObject, error) {
	if s.opts.Ports.Gates == nil {
		return nil, errNotWired("capability gates")
	}
	capability := gates.Capability(request.Capability)
	if !capability.Valid() {
		return nil, validationError("capability", "unknown capability")
	}
	action := GateAction(request.Action)
	switch action {
	case GateActionPropose, GateActionApprove, GateActionActivate,
		GateActionSuspend, GateActionResume, GateActionRevoke,
		GateActionSandbox, GateActionUnsandbox:
	default:
		return nil, validationError("action", "unknown gate action")
	}

	var (
		proposal gates.Proposal
		note     string
	)
	if request.Body != nil {
		if len(strings.TrimSpace(request.Body.Reason)) < minReasonLength {
			return nil, validationError("reason", "reason must be at least 8 characters")
		}
		proposal.Reason = request.Body.Reason
		proposal.LegalReviewRef = deref(request.Body.LegalReviewRef)
		proposal.ProviderContractRef = deref(request.Body.ProviderContractRef)
		proposal.RiskApprovalRef = deref(request.Body.RiskApprovalRef)
		proposal.SecurityApprovalRef = deref(request.Body.SecurityApprovalRef)
		if request.Body.EvidenceHashes != nil {
			proposal.EvidenceHashes = append([]string(nil), *request.Body.EvidenceHashes...)
		}
		if request.Body.EffectiveAt != nil {
			proposal.EffectiveAt = request.Body.EffectiveAt.UTC()
		}
		if request.Body.ExpiresAt != nil {
			proposal.ExpiresAt = request.Body.ExpiresAt.UTC()
		}
		note = deref(request.Body.Note)
		if note == "" {
			note = proposal.Reason
		}
	} else if action == GateActionPropose {
		return nil, validationError("body", "a reason is required to propose a gate change")
	}

	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.CapabilityGate, commandMeta, error) {
			g, aerr := s.opts.Ports.Gates.Act(ctx, capability, action, proposal, note)
			if aerr != nil {
				return api.CapabilityGate{}, commandMeta{}, aerr
			}
			return toAPIGate(g), commandMeta{
				Status: http.StatusOK, ResourceType: "capability_gate", ResourceID: string(capability),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdminGatesCapabilityAction200JSONResponse(res.Value), nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// GetAdminKillSwitches lists every switch, active or not.
func (s *Server) GetAdminKillSwitches(ctx context.Context, _ api.GetAdminKillSwitchesRequestObject) (api.GetAdminKillSwitchesResponseObject, error) {
	if s.opts.Ports.KillSwitches == nil {
		return nil, errNotWired("kill switches")
	}
	list, err := s.opts.Ports.KillSwitches.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]api.KillSwitch, 0, len(list))
	for _, sw := range list {
		out = append(out, toAPIKillSwitch(sw))
	}
	return api.GetAdminKillSwitches200JSONResponse(out), nil
}

// PostAdminKillSwitches activates or releases a switch. Activation is the fast
// path: one operator with kill:activate and no approval, because stopping new
// risk must never wait. Release is the slow path, and internal/killswitch
// demands step-up plus an approved admin action for SEVERE switches. Nothing
// here can stop reconciliation, settlement or ledger posting.
func (s *Server) PostAdminKillSwitches(ctx context.Context, request api.PostAdminKillSwitchesRequestObject) (api.PostAdminKillSwitchesResponseObject, error) {
	if s.opts.Ports.KillSwitches == nil {
		return nil, errNotWired("kill switches")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	kind := killswitch.Kind(request.Body.Kind)
	if !kind.Valid() {
		return nil, validationError("kind", "unknown kill switch kind")
	}
	scope := killswitch.GlobalScope
	if request.Body.ScopeId != nil && *request.Body.ScopeId != "" {
		scope = *request.Body.ScopeId
	}
	normalized, err := kind.NormalizeScope(scope)
	if err != nil {
		return nil, validationError("scope_id", "scope_id is not valid for this kind")
	}
	if len(strings.TrimSpace(request.Body.Reason)) < minReasonLength {
		return nil, validationError("reason", "reason must be at least 8 characters")
	}
	var approvalID *string
	if request.Body.ApprovalId != nil {
		v := request.Body.ApprovalId.String()
		approvalID = &v
	}

	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.KillSwitch, commandMeta, error) {
			var (
				sw  killswitch.Switch
				aer error
			)
			switch request.Body.Action {
			case "activate":
				sw, aer = s.opts.Ports.KillSwitches.Activate(ctx, kind, normalized, request.Body.Reason)
			case "release":
				sw, aer = s.opts.Ports.KillSwitches.Release(ctx, kind, normalized, request.Body.Reason, approvalID)
			default:
				aer = validationError("action", "action must be activate or release")
			}
			if aer != nil {
				return api.KillSwitch{}, commandMeta{}, aer
			}
			return toAPIKillSwitch(sw), commandMeta{
				Status:       http.StatusOK,
				ResourceType: "kill_switch",
				ResourceID:   string(kind) + ":" + normalized,
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdminKillSwitches200JSONResponse(res.Value), nil
}

// GetAdminActions pages the controlled administrative actions.
func (s *Server) GetAdminActions(ctx context.Context, request api.GetAdminActionsRequestObject) (api.GetAdminActionsResponseObject, error) {
	if s.opts.Ports.AdminActions == nil {
		return nil, errNotWired("administrative actions")
	}
	status := ""
	if request.Params.Status != nil {
		status = *request.Params.Status
	}
	cursor := ""
	if request.Params.Cursor != nil {
		cursor = *request.Params.Cursor
	}
	page, err := s.opts.Ports.AdminActions.List(ctx, status, cursor,
		pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	items := make([]api.AdminAction, 0, len(page.Items))
	for _, a := range page.Items {
		items = append(items, toAPIAdminAction(a))
	}
	return api.GetAdminActions200JSONResponse(api.AdminActionPage{
		Items:      items,
		NextCursor: nextCursor(page.NextCursor),
	}), nil
}

// PostAdminActions proposes a controlled action. The kind's own propose
// permission, step-up window and expiry are enforced by internal/admin.
func (s *Server) PostAdminActions(ctx context.Context, request api.PostAdminActionsRequestObject) (api.PostAdminActionsResponseObject, error) {
	if s.opts.Ports.AdminActions == nil {
		return nil, errNotWired("administrative actions")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	kind := admin.Kind(request.Body.Kind)
	if !kind.Valid() {
		return nil, validationError("kind", "unknown administrative action kind")
	}
	if len(strings.TrimSpace(request.Body.Reason)) < minReasonLength {
		return nil, validationError("reason", "reason must be at least 8 characters")
	}
	var params json.RawMessage
	if request.Body.Params != nil {
		raw, merr := json.Marshal(*request.Body.Params)
		if merr != nil {
			return nil, validationError("params", "params must be a JSON object")
		}
		params = raw
	}
	proposal := admin.Proposal{
		Kind:          kind,
		TargetType:    request.Body.TargetType,
		TargetID:      request.Body.TargetId,
		Params:        params,
		Reason:        request.Body.Reason,
		CorrelationID: observability.CorrelationID(ctx),
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.AdminAction, commandMeta, error) {
			a, perr := s.opts.Ports.AdminActions.Propose(ctx, proposal)
			if perr != nil {
				return api.AdminAction{}, commandMeta{}, perr
			}
			return toAPIAdminAction(a), commandMeta{
				Status: http.StatusCreated, ResourceType: "admin_action", ResourceID: a.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdminActions201JSONResponse(res.Value), nil
}

// PostAdminActionsActionIdDecision approves, rejects or executes a proposed
// action. The approver must differ from the proposer and hold the kind's
// approve permission: internal/admin refuses self-approval outright.
func (s *Server) PostAdminActionsActionIdDecision(ctx context.Context, request api.PostAdminActionsActionIdDecisionRequestObject) (api.PostAdminActionsActionIdDecisionResponseObject, error) {
	if s.opts.Ports.AdminActions == nil {
		return nil, errNotWired("administrative actions")
	}
	actionID, err := admin.ParseActionID(request.ActionId.String())
	if err != nil || actionID.IsZero() {
		return nil, validationError("actionId", "actionId must be a canonical UUID")
	}
	note := ""
	if request.Body != nil && request.Body.Note != nil {
		note = *request.Body.Note
	}
	decision := string(request.Decision)
	if decision == "reject" && len(strings.TrimSpace(note)) < minReasonLength {
		return nil, validationError("note", "a rejection reason of at least 8 characters is required")
	}

	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.AdminAction, commandMeta, error) {
			var (
				a    admin.Action
				derr error
			)
			switch decision {
			case "approve":
				a, derr = s.opts.Ports.AdminActions.Approve(ctx, actionID.String(), note)
			case "reject":
				a, derr = s.opts.Ports.AdminActions.Reject(ctx, actionID.String(), note)
			case "execute":
				a, derr = s.opts.Ports.AdminActions.Execute(ctx, actionID.String())
			default:
				derr = validationError("decision", "decision must be approve, reject or execute")
			}
			if derr != nil {
				return api.AdminAction{}, commandMeta{}, derr
			}
			return toAPIAdminAction(a), commandMeta{
				Status: http.StatusOK, ResourceType: "admin_action", ResourceID: a.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdminActionsActionIdDecision200JSONResponse(res.Value), nil
}

// PostAdminInstrumentsInstrumentIdStatus changes an instrument's status.
func (s *Server) PostAdminInstrumentsInstrumentIdStatus(ctx context.Context, request api.PostAdminInstrumentsInstrumentIdStatusRequestObject) (api.PostAdminInstrumentsInstrumentIdStatusResponseObject, error) {
	if s.opts.Ports.Instruments == nil {
		return nil, errNotWired("the instrument registry")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	p, ok := security.PrincipalFrom(ctx)
	if !ok {
		return nil, errs.New(errs.CodeUnauthenticated, "authentication is required")
	}
	instrumentID, err := instruments.ParseInstrumentID(request.InstrumentId.String())
	if err != nil || instrumentID.IsZero() {
		return nil, validationError("instrumentId", "instrumentId must be a canonical UUID")
	}
	if len(strings.TrimSpace(request.Body.Reason)) < minReasonLength {
		return nil, validationError("reason", "reason must be at least 8 characters")
	}
	to := assets.Status(request.Body.To)
	if !to.Valid() {
		return nil, validationError("to", "unknown instrument status")
	}
	ch := instruments.StatusChange{
		To:            to,
		ActorType:     string(p.ActorType),
		ActorID:       p.SubjectID,
		Reason:        request.Body.Reason,
		PolicyVersion: deref(request.Body.PolicyVersion),
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.Instrument, commandMeta, error) {
			i, terr := s.opts.Ports.Instruments.Transition(ctx, instrumentID, ch)
			if terr != nil {
				return api.Instrument{}, commandMeta{}, terr
			}
			return toAPIInstrument(i), commandMeta{
				Status: http.StatusOK, ResourceType: "instrument", ResourceID: i.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdminInstrumentsInstrumentIdStatus200JSONResponse(res.Value), nil
}

// GetAdminProviders reports provider health and verification labels.
func (s *Server) GetAdminProviders(ctx context.Context, _ api.GetAdminProvidersRequestObject) (api.GetAdminProvidersResponseObject, error) {
	if s.opts.Ports.Providers == nil {
		return nil, errNotWired("provider status")
	}
	list, err := s.opts.Ports.Providers.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]api.ProviderStatus, 0, len(list))
	for _, p := range list {
		out = append(out, toAPIProvider(p))
	}
	return api.GetAdminProviders200JSONResponse(out), nil
}

// GetAdminReconciliationRecords pages reconciliation records.
func (s *Server) GetAdminReconciliationRecords(ctx context.Context, request api.GetAdminReconciliationRecordsRequestObject) (api.GetAdminReconciliationRecordsResponseObject, error) {
	if s.opts.Ports.Reconciliation == nil {
		return nil, errNotWired("reconciliation")
	}
	status := ""
	if request.Params.Status != nil {
		status = *request.Params.Status
	}
	cursor := ""
	if request.Params.Cursor != nil {
		cursor = *request.Params.Cursor
	}
	var accountID *accounts.AccountID
	if request.Params.AccountId != nil {
		a, perr := accounts.ParseAccountID(request.Params.AccountId.String())
		if perr != nil {
			return nil, validationError("account_id", "account_id must be a canonical UUID")
		}
		accountID = &a
	}
	page, err := s.opts.Ports.Reconciliation.List(ctx, status, accountID, cursor,
		pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	items := make([]api.ReconciliationRecord, 0, len(page.Items))
	for _, r := range page.Items {
		items = append(items, toAPIReconciliationRecord(r))
	}
	return api.GetAdminReconciliationRecords200JSONResponse(api.ReconciliationRecordPage{
		Items:      items,
		NextCursor: nextCursor(page.NextCursor),
	}), nil
}

// PostAdminReconciliationRecordsRecordIdResolve resolves a mismatch. A
// material resolution needs an approved admin action, and a correction posts a
// compensating journal transaction through internal/ledger. This endpoint
// never edits a balance and never builds a posting of its own: the entries are
// handed to the domain exactly as the operator supplied them.
func (s *Server) PostAdminReconciliationRecordsRecordIdResolve(ctx context.Context, request api.PostAdminReconciliationRecordsRecordIdResolveRequestObject) (api.PostAdminReconciliationRecordsRecordIdResolveResponseObject, error) {
	if s.opts.Ports.Reconciliation == nil {
		return nil, errNotWired("reconciliation")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	if len(strings.TrimSpace(request.Body.Reason)) < minReasonLength {
		return nil, validationError("reason", "reason must be at least 8 characters")
	}
	if strings.TrimSpace(request.Body.EvidenceRef) == "" {
		return nil, validationError("evidence_ref", "evidence_ref is required")
	}
	res := ReconciliationResolution{
		Reason:      request.Body.Reason,
		EvidenceRef: request.Body.EvidenceRef,
	}
	if request.Body.ApprovalId != nil {
		res.ApprovalID = request.Body.ApprovalId.String()
	}
	if c := request.Body.Compensation; c != nil {
		comp := &Compensation{ReasonCode: deref(c.ReasonCode)}
		if c.Entries != nil {
			for _, e := range *c.Entries {
				assetID, aerr := assets.ParseAssetID(e.Asset.String())
				if aerr != nil {
					return nil, validationError("compensation.entries.asset", "asset must be a canonical UUID")
				}
				qty, qerr := money.ParseQuantity(e.Quantity)
				if qerr != nil {
					return nil, validationError("compensation.entries.quantity",
						"quantity must be an exact integer base-unit string")
				}
				comp.Entries = append(comp.Entries, CompensationEntry{
					AccountCode: e.AccountCode,
					AssetID:     assetID,
					Side:        string(e.Side),
					Quantity:    qty,
				})
			}
		}
		res.Compensation = comp
	}

	out, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.ReconciliationRecord, commandMeta, error) {
			rec, rerr := s.opts.Ports.Reconciliation.Resolve(ctx, request.RecordId.String(), res)
			if rerr != nil {
				return api.ReconciliationRecord{}, commandMeta{}, rerr
			}
			return toAPIReconciliationRecord(rec), commandMeta{
				Status: http.StatusOK, ResourceType: "reconciliation_record", ResourceID: rec.ID,
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdminReconciliationRecordsRecordIdResolve200JSONResponse(out.Value), nil
}
