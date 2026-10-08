package httpapi

import (
	"context"
	"encoding/hex"
	"net/http"

	"github.com/google/uuid"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/transfers"
)

// Customer-to-customer transfers of simulated value and the sandbox faucet
// (ADR-0020). These handlers translate shapes and decide what a reader may see
// of a record; every financial rule is the service's.

// PostAdvancedTransfers sends simulated value from one of the caller's own
// accounts. A refusal the service records reaches the client as its stable
// code and stays readable as a REJECTED transfer.
func (s *Server) PostAdvancedTransfers(ctx context.Context, request api.PostAdvancedTransfersRequestObject) (api.PostAdvancedTransfersResponseObject, error) {
	if s.opts.Ports.Transfers == nil {
		return nil, errNotWired("advanced transfers")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	b := request.Body
	sender, err := accountScopeWrite(ctx, b.AccountId)
	if err != nil {
		return nil, err
	}
	amount, qty, err := transferAmountOrQuantity(b.Amount, b.Quantity)
	if err != nil {
		return nil, err
	}
	cmd := TransferSend{
		SenderAccountID: sender, RecipientHandle: deref(b.RecipientHandle), Amount: amount, Quantity: qty,
		Mode: gates.Mode(b.Mode), Memo: deref(b.Memo),
		IdempotencyKey: request.Params.IdempotencyKey, CorrelationID: observability.CorrelationID(ctx),
	}
	if b.RecipientAccountId != nil {
		if cmd.RecipientAccountID, err = accounts.ParseAccountID(b.RecipientAccountId.String()); err != nil || cmd.RecipientAccountID.IsZero() {
			return nil, validationError("recipient_account_id", "recipient_account_id must be a canonical UUID")
		}
	}
	if b.AssetId != nil {
		if cmd.AssetID, err = assets.ParseAssetID(b.AssetId.String()); err != nil || cmd.AssetID.IsZero() {
			return nil, validationError("asset_id", "asset_id must be a canonical UUID")
		}
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.AdvancedTransfer, commandMeta, error) {
			sent, serr := s.opts.Ports.Transfers.Send(ctx, cmd)
			if serr != nil {
				return api.AdvancedTransfer{}, commandMeta{}, serr
			}
			status := http.StatusCreated
			if sent.Replayed {
				// The service had already decided this key: nothing new exists.
				status = http.StatusOK
			}
			return toAPIAdvancedTransfer(sent.View, transferAudienceOf(ctx, sent.View.Transfer)), commandMeta{
				Status: status, ResourceType: "internal_transfer", ResourceID: sent.View.Transfer.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	if res.Replayed || res.Meta.Status == http.StatusOK {
		return api.PostAdvancedTransfers200JSONResponse(res.Value), nil
	}
	return api.PostAdvancedTransfers201JSONResponse(res.Value), nil
}

// GetAdvancedTransfers pages the transfers an account sent or received.
func (s *Server) GetAdvancedTransfers(ctx context.Context, request api.GetAdvancedTransfersRequestObject) (api.GetAdvancedTransfersResponseObject, error) {
	if s.opts.Ports.Transfers == nil {
		return nil, errNotWired("advanced transfers")
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	items, next, err := s.opts.Ports.Transfers.List(ctx, account, deref(request.Params.Cursor),
		pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	page := api.AdvancedTransferPage{
		Items: make([]api.AdvancedTransfer, 0, len(items)), NextCursor: nextCursor(next), ValueDomain: api.SIMULATED,
	}
	for _, v := range items {
		aud := transferAudienceOf(ctx, v.Transfer)
		if !aud.visible {
			// A REJECTED attempt listed under its recipient is the sender's
			// record. The cursor still advances past it.
			continue
		}
		page.Items = append(page.Items, toAPIAdvancedTransfer(v, aud))
	}
	return api.GetAdvancedTransfers200JSONResponse(page), nil
}

// GetAdvancedTransfersTransferId returns one transfer to one of its parties
// or an operator who may read any account. Anyone else, and a recipient asking
// for an attempt that never reached them, is told it does not exist.
func (s *Server) GetAdvancedTransfersTransferId(ctx context.Context, request api.GetAdvancedTransfersTransferIdRequestObject) (api.GetAdvancedTransfersTransferIdResponseObject, error) {
	if s.opts.Ports.Transfers == nil {
		return nil, errNotWired("advanced transfers")
	}
	tid, err := transfers.ParseTransferID(request.TransferId.String())
	if err != nil || tid.IsZero() {
		return nil, validationError("transferId", "transferId must be a canonical UUID")
	}
	v, err := s.opts.Ports.Transfers.Get(ctx, tid)
	if err != nil {
		return nil, err
	}
	aud := transferAudienceOf(ctx, v.Transfer)
	if !aud.visible {
		// NOT_FOUND, never FORBIDDEN, and the very answer an absent id gets: a
		// distinguishable refusal would confirm the id exists (F-41).
		return nil, transferNotFound(tid)
	}
	return api.GetAdvancedTransfersTransferId200JSONResponse(toAPIAdvancedTransfer(v, aud)), nil
}

// --- reversal under dual control ------------------------------------------------
//
// A COMPLETED transfer is reversed by two distinct operators (review finding
// F3). These handlers carry each step to the transfer service, which decides
// who may take it; the single-operator reversal no longer exists anywhere.

const transferReversalsFeature = "transfer reversals"

// PostAdvancedTransfersTransferIdReversalProposals records one operator's
// proposal to reverse a COMPLETED transfer. Nothing moves.
func (s *Server) PostAdvancedTransfersTransferIdReversalProposals(ctx context.Context, request api.PostAdvancedTransfersTransferIdReversalProposalsRequestObject) (api.PostAdvancedTransfersTransferIdReversalProposalsResponseObject, error) {
	port := s.opts.Ports.TransferReversals
	if port == nil {
		return nil, errNotWired(transferReversalsFeature)
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	tid, err := transfers.ParseTransferID(request.TransferId.String())
	if err != nil || tid.IsZero() {
		return nil, validationError("transferId", "transferId must be a canonical UUID")
	}
	cmd := TransferReversalPropose{
		TransferID: tid, ReasonCode: request.Body.ReasonCode, Reason: request.Body.Reason,
		CorrelationID: observability.CorrelationID(ctx),
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.AdvancedTransferReversalProposal, commandMeta, error) {
			out, perr := port.Propose(ctx, cmd)
			if perr != nil {
				return api.AdvancedTransferReversalProposal{}, commandMeta{}, perr
			}
			v, cerr := toAPITransferReversalProposal(ctx, out.View)
			if cerr != nil {
				return api.AdvancedTransferReversalProposal{}, commandMeta{}, cerr
			}
			status := http.StatusCreated
			if out.Replayed {
				// The same proposal was already pending: nothing new exists.
				status = http.StatusOK
			}
			return v, commandMeta{Status: status, ResourceType: "transfer_reversal_proposal", ResourceID: out.View.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	if res.Replayed || res.Meta.Status == http.StatusOK {
		return api.PostAdvancedTransfersTransferIdReversalProposals200JSONResponse(res.Value), nil
	}
	return api.PostAdvancedTransfersTransferIdReversalProposals201JSONResponse(res.Value), nil
}

// GetAdvancedTransfersReversalProposals pages reversal proposals for
// operators, newest first.
func (s *Server) GetAdvancedTransfersReversalProposals(ctx context.Context, request api.GetAdvancedTransfersReversalProposalsRequestObject) (api.GetAdvancedTransfersReversalProposalsResponseObject, error) {
	port := s.opts.Ports.TransferReversals
	if port == nil {
		return nil, errNotWired(transferReversalsFeature)
	}
	status := ""
	if request.Params.Status != nil {
		switch st := *request.Params.Status; st {
		case api.TransferReversalPending, api.TransferReversalApproved, api.TransferReversalRejected:
			status = string(st)
		default:
			return nil, validationError("status", "status must be PENDING, APPROVED or REJECTED")
		}
	}
	var transfer *transfers.TransferID
	if request.Params.TransferId != nil {
		tid, err := transfers.ParseTransferID(request.Params.TransferId.String())
		if err != nil || tid.IsZero() {
			return nil, validationError("transfer_id", "transfer_id must be a canonical UUID")
		}
		transfer = &tid
	}
	page, err := port.List(ctx, status, transfer, deref(request.Params.Cursor),
		pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	out := api.AdvancedTransferReversalProposalPage{
		Items:       make([]api.AdvancedTransferReversalProposal, 0, len(page.Items)),
		NextCursor:  nextCursor(page.NextCursor),
		ValueDomain: api.SIMULATED,
	}
	for _, v := range page.Items {
		item, cerr := toAPITransferReversalProposal(ctx, v)
		if errs.HasCode(cerr, errs.CodeNotFound) {
			// A transfer this principal may not read is not listed; the
			// cursor still advances past it.
			continue
		}
		if cerr != nil {
			return nil, cerr
		}
		out.Items = append(out.Items, item)
	}
	return api.GetAdvancedTransfersReversalProposals200JSONResponse(out), nil
}

// GetAdvancedTransfersReversalProposalsProposalId returns one proposal with
// its transfer.
func (s *Server) GetAdvancedTransfersReversalProposalsProposalId(ctx context.Context, request api.GetAdvancedTransfersReversalProposalsProposalIdRequestObject) (api.GetAdvancedTransfersReversalProposalsProposalIdResponseObject, error) {
	port := s.opts.Ports.TransferReversals
	if port == nil {
		return nil, errNotWired(transferReversalsFeature)
	}
	pid := request.ProposalId
	if pid == uuid.Nil {
		return nil, validationError("proposalId", "proposalId must be a canonical UUID")
	}
	v, err := port.Get(ctx, pid)
	if errs.HasCode(err, errs.CodeNotFound) {
		return nil, transferReversalNotFound(pid)
	}
	if err != nil {
		return nil, err
	}
	out, err := toAPITransferReversalProposal(ctx, v)
	if errs.HasCode(err, errs.CodeNotFound) {
		return nil, transferReversalNotFound(pid)
	}
	if err != nil {
		return nil, err
	}
	return api.GetAdvancedTransfersReversalProposalsProposalId200JSONResponse(out), nil
}

// PostAdvancedTransfersReversalProposalsProposalIdApprove is the second,
// distinct operator's approval, which executes the reversal in the same
// transaction.
func (s *Server) PostAdvancedTransfersReversalProposalsProposalIdApprove(ctx context.Context, request api.PostAdvancedTransfersReversalProposalsProposalIdApproveRequestObject) (api.PostAdvancedTransfersReversalProposalsProposalIdApproveResponseObject, error) {
	port := s.opts.Ports.TransferReversals
	if port == nil {
		return nil, errNotWired(transferReversalsFeature)
	}
	if request.ProposalId == uuid.Nil {
		return nil, validationError("proposalId", "proposalId must be a canonical UUID")
	}
	cmd := TransferReversalDecide{ProposalID: request.ProposalId, CorrelationID: observability.CorrelationID(ctx)}
	if request.Body != nil {
		cmd.Reason = deref(request.Body.Reason)
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.AdvancedTransferReversalDecisionResult, commandMeta, error) {
			return transferReversalDecision(ctx, port.Approve, cmd)
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedTransfersReversalProposalsProposalIdApprove200JSONResponse(res.Value), nil
}

// PostAdvancedTransfersReversalProposalsProposalIdReject rejects a proposal:
// it moves nothing and can never be executed afterwards.
func (s *Server) PostAdvancedTransfersReversalProposalsProposalIdReject(ctx context.Context, request api.PostAdvancedTransfersReversalProposalsProposalIdRejectRequestObject) (api.PostAdvancedTransfersReversalProposalsProposalIdRejectResponseObject, error) {
	port := s.opts.Ports.TransferReversals
	if port == nil {
		return nil, errNotWired(transferReversalsFeature)
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	if request.ProposalId == uuid.Nil {
		return nil, validationError("proposalId", "proposalId must be a canonical UUID")
	}
	cmd := TransferReversalDecide{
		ProposalID: request.ProposalId, Reason: deref(request.Body.Reason), CorrelationID: observability.CorrelationID(ctx),
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.AdvancedTransferReversalDecisionResult, commandMeta, error) {
			return transferReversalDecision(ctx, port.Reject, cmd)
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedTransfersReversalProposalsProposalIdReject200JSONResponse(res.Value), nil
}

// transferReversalDecision runs one decision through the port and renders it.
func transferReversalDecision(ctx context.Context, decide func(context.Context, TransferReversalDecide) (TransferReversalOutcome, error),
	cmd TransferReversalDecide) (api.AdvancedTransferReversalDecisionResult, commandMeta, error) {
	out, err := decide(ctx, cmd)
	if err != nil {
		return api.AdvancedTransferReversalDecisionResult{}, commandMeta{}, err
	}
	v, err := toAPITransferReversalProposal(ctx, out.View)
	if err != nil {
		return api.AdvancedTransferReversalDecisionResult{}, commandMeta{}, err
	}
	return api.AdvancedTransferReversalDecisionResult{Proposal: v, Replayed: out.Replayed, ValueDomain: api.SIMULATED},
		commandMeta{Status: http.StatusOK, ResourceType: "transfer_reversal_proposal", ResourceID: out.View.ID.String()}, nil
}

// transferReversalNotFound is the one answer for a proposal that does not
// exist and one this principal may not read.
func transferReversalNotFound(pid uuid.UUID) error {
	return errs.New(errs.CodeNotFound, "reversal proposal not found").WithField("proposal_id", pid.String())
}

// toAPITransferReversalProposal renders a proposal. The transfer is shown
// as the principal in ctx may see it; a principal who may not see it is
// told the proposal does not exist (NOT_FOUND).
func toAPITransferReversalProposal(ctx context.Context, v TransferReversalProposalView) (api.AdvancedTransferReversalProposal, error) {
	status := api.AdvancedTransferReversalStatus(v.Status)
	switch status {
	case api.TransferReversalPending, api.TransferReversalApproved, api.TransferReversalRejected:
	default:
		return api.AdvancedTransferReversalProposal{}, errs.Newf(errs.CodeInternal, "a reversal proposal reported the unknown status %q", v.Status)
	}
	aud := transferAudienceOf(ctx, v.Transfer.Transfer)
	if !aud.visible || v.Transfer.Transfer.ID != v.TransferID {
		return api.AdvancedTransferReversalProposal{}, transferReversalNotFound(v.ID)
	}
	return api.AdvancedTransferReversalProposal{
		Id: v.ID, TransferId: toUUID(v.TransferID), Seq: v.Seq, Status: status, ReasonCode: v.ReasonCode, Reason: v.Reason,
		ProposedBy: v.ProposedBy, ProposedAt: v.ProposedAt.UTC(), DecidedBy: strPtr(v.DecidedBy), DecidedAt: timePtr(v.DecidedAt),
		DecisionReason: strPtr(v.DecisionReason), Transfer: toAPIAdvancedTransfer(v.Transfer, aud), ValueDomain: api.SIMULATED,
	}, nil
}

// PostAdvancedTransfersHandles registers the handle other customers pay one of
// the caller's accounts by. No route resolves a handle to an account id.
func (s *Server) PostAdvancedTransfersHandles(ctx context.Context, request api.PostAdvancedTransfersHandlesRequestObject) (api.PostAdvancedTransfersHandlesResponseObject, error) {
	if s.opts.Ports.Transfers == nil {
		return nil, errNotWired("advanced transfers")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	account, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	handle := request.Body.Handle
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.AdvancedTransferHandle, commandMeta, error) {
			h, herr := s.opts.Ports.Transfers.RegisterHandle(ctx, account, handle)
			if herr != nil {
				return api.AdvancedTransferHandle{}, commandMeta{}, herr
			}
			hid, perr := uuid.Parse(h.ID)
			if perr != nil {
				return api.AdvancedTransferHandle{}, commandMeta{},
					errs.Wrap(perr, errs.CodeInternal, "the handle's id is not a UUID")
			}
			return api.AdvancedTransferHandle{
				Id: hid, AccountId: toUUID(h.AccountID), Handle: h.Handle, CreatedAt: h.CreatedAt,
			}, commandMeta{
				Status: http.StatusCreated, ResourceType: "transfer_handle", ResourceID: h.ID,
			}, nil
		})
	if err != nil {
		return nil, err
	}
	if res.Replayed {
		return api.PostAdvancedTransfersHandles200JSONResponse(res.Value), nil
	}
	return api.PostAdvancedTransfersHandles201JSONResponse(res.Value), nil
}

// PostAdvancedTransfersRecipientRules records one of the caller's rules about
// one recipient. A recipient named by handle is resolved on the server and the
// rule is echoed back the way it was named.
func (s *Server) PostAdvancedTransfersRecipientRules(ctx context.Context, request api.PostAdvancedTransfersRecipientRulesRequestObject) (api.PostAdvancedTransfersRecipientRulesResponseObject, error) {
	if s.opts.Ports.Transfers == nil {
		return nil, errNotWired("advanced transfers")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	b := request.Body
	sender, err := accountScopeWrite(ctx, b.AccountId)
	if err != nil {
		return nil, err
	}
	cmd := TransferRecipientRule{
		SenderAccountID: sender, RecipientHandle: deref(b.RecipientHandle),
		Rule: transfers.RuleKind(b.Rule), Reason: deref(b.Reason),
	}
	if b.RecipientAccountId != nil {
		if cmd.RecipientAccountID, err = accounts.ParseAccountID(b.RecipientAccountId.String()); err != nil || cmd.RecipientAccountID.IsZero() {
			return nil, validationError("recipient_account_id", "recipient_account_id must be a canonical UUID")
		}
	}
	if (cmd.RecipientHandle == "") == cmd.RecipientAccountID.IsZero() {
		return nil, validationError("recipient", "name the recipient by exactly one of recipient_handle or recipient_account_id")
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.AdvancedTransferRecipientRule, commandMeta, error) {
			if rerr := s.opts.Ports.Transfers.SetRecipientRule(ctx, cmd); rerr != nil {
				return api.AdvancedTransferRecipientRule{}, commandMeta{}, rerr
			}
			out := api.AdvancedTransferRecipientRule{
				AccountId: toUUID(sender), Rule: b.Rule, Reason: strPtr(cmd.Reason),
				RecipientAccountId: uuidPtr(cmd.RecipientAccountID),
			}
			if cmd.RecipientHandle != "" {
				// The service accepted it, so it normalizes: echo the handle the
				// rule is actually about.
				if h, herr := transfers.NormalizeHandle(cmd.RecipientHandle); herr == nil {
					out.RecipientHandle = &h
				}
			}
			return out, commandMeta{Status: http.StatusOK, ResourceType: "transfer_recipient_rule", ResourceID: sender.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedTransfersRecipientRules200JSONResponse(res.Value), nil
}

// PostAdvancedTransfersSenderMode records whether one of the caller's accounts
// pays anyone or only allowlisted recipients.
func (s *Server) PostAdvancedTransfersSenderMode(ctx context.Context, request api.PostAdvancedTransfersSenderModeRequestObject) (api.PostAdvancedTransfersSenderModeResponseObject, error) {
	if s.opts.Ports.Transfers == nil {
		return nil, errNotWired("advanced transfers")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	account, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	mode := request.Body.Mode
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.AdvancedTransferSenderModeSetting, commandMeta, error) {
			if merr := s.opts.Ports.Transfers.SetSenderMode(ctx, account, transfers.SenderMode(mode)); merr != nil {
				return api.AdvancedTransferSenderModeSetting{}, commandMeta{}, merr
			}
			return api.AdvancedTransferSenderModeSetting{AccountId: toUUID(account), Mode: mode},
				commandMeta{Status: http.StatusOK, ResourceType: "transfer_sender_mode", ResourceID: account.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedTransfersSenderMode200JSONResponse(res.Value), nil
}

// GetAdvancedTransfersReconciliation reconciles the transfer register, its
// effects and the ledger. It reads only.
func (s *Server) GetAdvancedTransfersReconciliation(ctx context.Context, request api.GetAdvancedTransfersReconciliationRequestObject) (api.GetAdvancedTransfersReconciliationResponseObject, error) {
	if s.opts.Ports.Transfers == nil {
		return nil, errNotWired("advanced transfers")
	}
	var scope *assets.AssetID
	if request.Params.AssetId != nil {
		aid, err := assets.ParseAssetID(request.Params.AssetId.String())
		if err != nil || aid.IsZero() {
			return nil, validationError("asset_id", "asset_id must be a canonical UUID")
		}
		scope = &aid
	}
	rep, err := s.opts.Ports.Transfers.Reconcile(ctx, scope)
	if err != nil {
		return nil, err
	}
	out := api.AdvancedTransferReconciliation{
		CheckedAt: rep.CheckedAt, Transfers: rep.Transfers, Ok: rep.OK(), ValueDomain: api.SIMULATED,
		Assets:        make([]api.AdvancedTransferAssetIdentity, 0, len(rep.Assets)),
		Discrepancies: make([]api.AdvancedTransferDiscrepancy, 0, len(rep.Discrepancies)),
	}
	for _, a := range rep.Assets {
		aid, perr := uuid.Parse(a.AssetID)
		if perr != nil {
			return nil, errs.Wrap(perr, errs.CodeInternal, "a reconciled asset id is not a UUID")
		}
		out.Assets = append(out.Assets, api.AdvancedTransferAssetIdentity{
			AssetId: aid, TransferOut: a.TransferOut.String(), TransferIn: a.TransferIn.String(),
			RegisterCompleted: a.RegisterCompleted.String(), WalletDelta: a.WalletDelta.String(),
		})
	}
	for _, d := range rep.Discrepancies {
		out.Discrepancies = append(out.Discrepancies, api.AdvancedTransferDiscrepancy{
			Code: d.Code, Detail: d.Detail, TransferId: parseUUIDText(d.TransferID),
			AccountId: parseUUIDText(d.AccountID), AssetId: parseUUIDText(d.AssetID),
		})
	}
	return api.GetAdvancedTransfersReconciliation200JSONResponse(out), nil
}

// PostAdvancedTransfersFaucetGrants asks the sandbox faucet for sandbox
// dollars into one of the caller's own accounts.
func (s *Server) PostAdvancedTransfersFaucetGrants(ctx context.Context, request api.PostAdvancedTransfersFaucetGrantsRequestObject) (api.PostAdvancedTransfersFaucetGrantsResponseObject, error) {
	if s.opts.Ports.SandboxFaucet == nil {
		return nil, errNotWired("the sandbox faucet")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	account, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	amount, qty, err := transferAmountOrQuantity(request.Body.Amount, request.Body.Quantity)
	if err != nil {
		return nil, err
	}
	cmd := FaucetRequest{AccountID: account, Amount: amount, Quantity: qty, IdempotencyKey: request.Params.IdempotencyKey}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.AdvancedFaucetGrant, commandMeta, error) {
			v, gerr := s.opts.Ports.SandboxFaucet.Grant(ctx, cmd)
			if gerr != nil {
				return api.AdvancedFaucetGrant{}, commandMeta{}, gerr
			}
			status := http.StatusCreated
			if v.Grant.Replayed {
				status = http.StatusOK
			}
			return toAPIAdvancedFaucetGrant(v), commandMeta{
				Status: status, ResourceType: "sandbox_funding", ResourceID: v.Grant.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	if res.Replayed || res.Meta.Status == http.StatusOK {
		return api.PostAdvancedTransfersFaucetGrants200JSONResponse(res.Value), nil
	}
	return api.PostAdvancedTransfersFaucetGrants201JSONResponse(res.Value), nil
}

// GetAdvancedTransfersFaucetAllowance reports what the faucet would still
// grant an account in the current window.
func (s *Server) GetAdvancedTransfersFaucetAllowance(ctx context.Context, request api.GetAdvancedTransfersFaucetAllowanceRequestObject) (api.GetAdvancedTransfersFaucetAllowanceResponseObject, error) {
	if s.opts.Ports.SandboxFaucet == nil {
		return nil, errNotWired("the sandbox faucet")
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	v, err := s.opts.Ports.SandboxFaucet.Allowance(ctx, account)
	if err != nil {
		return nil, err
	}
	return api.GetAdvancedTransfersFaucetAllowance200JSONResponse(api.AdvancedFaucetAllowance{
		AccountId: toUUID(v.AccountID), AssetId: toUUID(v.AssetID), AssetSymbol: v.AssetSymbol,
		AssetDecimals: int(v.AssetDecimals), Cap: v.Cap.String(), Used: v.Used.String(), Remaining: v.Remaining.String(),
		WindowMs: v.Policy.WindowMS, PolicyVersion: v.Policy.Version, PolicyHash: v.PolicyHash,
		Environment: v.Environment, ValueDomain: api.SIMULATED,
	}), nil
}

// transferAmountOrQuantity reads the two spellings of one value: exactly one of a plain
// decimal amount (converted by the adapter against the asset's decimals, never
// rounded) and a base-unit quantity.
func transferAmountOrQuantity(amount *string, quantity *api.Quantity) (string, *money.Quantity, error) {
	if (amount == nil) == (quantity == nil) {
		return "", nil, validationError("amount",
			"give the value as exactly one of amount (a plain decimal in whole units of the asset) or quantity (exact base units)")
	}
	if amount != nil {
		return *amount, nil, nil
	}
	q, err := money.ParseQuantity(*quantity)
	if err != nil {
		return "", nil, validationError("quantity", "quantity must be an exact integer base-unit string")
	}
	return "", &q, nil
}

// transferNotFound is the one answer for a transfer a principal may not see
// and a transfer that does not exist; it matches the service's own.
func transferNotFound(tid transfers.TransferID) error {
	return errs.New(errs.CodeNotFound, "transfer not found").WithField("transfer_id", tid.String())
}

// transferAudience is how much of one transfer one principal may see.
type transferAudience struct {
	// visible is false when the principal is told the transfer does not exist.
	visible bool
	// recipientAccount shows the recipient's account id.
	recipientAccount bool
	// decision shows the sender's decision record -- rejection codes, the risk
	// decision, the decision evidence -- and the operator's reversal reason.
	decision bool
}

// transferOperatorAudience sees everything.
var transferOperatorAudience = transferAudience{visible: true, recipientAccount: true, decision: true}

// transferAudienceOf decides what the principal in ctx may see of t:
//
//   - the sender sees their whole record, except the account id behind a
//     handle they paid (unless that account is theirs too): resolution of a
//     handle stays on the server;
//   - an operator holding account:read_any sees everything;
//   - the recipient sees a transfer once it moved value (COMPLETED or
//     REVERSED), without the sender's decision record or the reversal reason.
//     A REJECTED attempt is the sender's: showing it to the recipient would
//     disclose why the sender was refused (a compliance block included) and
//     would carry the memo of a transfer that never happened;
//   - anyone else sees nothing.
func transferAudienceOf(ctx context.Context, t transfers.Transfer) transferAudience {
	p, ok := security.PrincipalFrom(ctx)
	if !ok {
		return transferAudience{}
	}
	sender, recipient := t.SenderAccountID.String(), t.RecipientAccountID.String()
	switch {
	case p.OwnsAccount(sender):
		return transferAudience{
			visible: true, decision: true,
			recipientAccount: t.RecipientHandle == "" || p.OwnsAccount(recipient),
		}
	case securityRequireAccount(ctx, sender) == nil:
		// Not the owner, so this is the account:read_any override.
		return transferOperatorAudience
	case p.OwnsAccount(recipient) && (t.Status == transfers.StatusCompleted || t.Status == transfers.StatusReversed):
		return transferAudience{visible: true, recipientAccount: true}
	}
	return transferAudience{}
}

func toAPIAdvancedTransfer(v TransferView, aud transferAudience) api.AdvancedTransfer {
	t := v.Transfer
	out := api.AdvancedTransfer{
		Id: toUUID(t.ID), IntentId: toUUID(t.IntentID), Status: api.AdvancedTransferStatus(t.Status),
		SenderAccountId: toUUID(t.SenderAccountID), RecipientHandle: strPtr(t.RecipientHandle),
		AssetId: toUUID(t.AssetID), AssetSymbol: v.AssetSymbol, AssetDecimals: int(v.AssetDecimals),
		Quantity: t.Quantity.String(), NotionalUsd: t.NotionalUSD.String(), Mode: api.AdvancedMode(t.Mode),
		Memo: strPtr(t.Memo), ReasonCodes: []string{}, PolicyVersion: t.PolicyVersion, PolicyHash: t.PolicyHash,
		DecidedAt: timePtr(t.DecidedAt), ReversedAt: timePtr(t.ReversedAt),
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt, ValueDomain: api.SIMULATED,
	}
	if t.EffectID != nil {
		out.EffectId = uuidPtr(*t.EffectID)
	}
	if t.JournalTransactionID != nil {
		out.JournalTransactionId = uuidPtr(*t.JournalTransactionID)
	}
	if t.ReversalJournalTransactionID != nil {
		out.ReversalJournalTransactionId = uuidPtr(*t.ReversalJournalTransactionID)
	}
	if aud.recipientAccount {
		out.RecipientAccountId = uuidPtr(t.RecipientAccountID)
	}
	if aud.decision {
		out.RejectionCode = strPtr(t.RejectionCode)
		out.ReasonCodes = append(out.ReasonCodes, t.ReasonCodes...)
		if t.RiskDecisionID != nil {
			out.RiskDecisionId = uuidPtr(*t.RiskDecisionID)
		}
		out.EvidenceRef = strPtr(t.EvidenceRef)
		if len(t.EvidenceSHA256) > 0 {
			h := hex.EncodeToString(t.EvidenceSHA256)
			out.EvidenceSha256 = &h
		}
		out.ReversalReasonCode = strPtr(t.ReversalReasonCode)
		if t.ReversalProposalID != nil {
			out.ReversalProposalId = uuidPtr(*t.ReversalProposalID)
		}
	}
	return out
}

func toAPIAdvancedFaucetGrant(v FaucetGrantView) api.AdvancedFaucetGrant {
	g := v.Grant
	out := api.AdvancedFaucetGrant{
		Id: toUUID(g.ID), AccountId: toUUID(g.AccountID), AssetId: toUUID(g.AssetID), AssetSymbol: v.AssetSymbol,
		AssetDecimals: int(v.AssetDecimals), Quantity: g.Quantity.String(), PolicyVersion: g.PolicyVersion,
		PolicyHash: g.PolicyHash, Environment: string(g.Environment), EffectId: toUUID(g.EffectID),
		JournalTransactionId: toUUID(g.JournalTransactionID), EvidenceRef: strPtr(g.EvidenceRef),
		GrantedAt: g.GrantedAt, ValueDomain: api.SIMULATED,
	}
	if len(g.EvidenceSHA256) > 0 {
		h := hex.EncodeToString(g.EvidenceSHA256)
		out.EvidenceSha256 = &h
	}
	return out
}
