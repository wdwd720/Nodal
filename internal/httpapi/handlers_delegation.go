package httpapi

import (
	"context"
	"encoding/hex"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/delegation"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// The delegated agent authority routes (internal/delegation). Handlers
// translate shapes only: the delegation service decides every rule, figure
// and refusal, and its error codes reach the wire unchanged.
//
// Who reaches what is decided in three places, none of them here alone: the
// route policy (authz.go: account:read for reads, sandbox:delegate for the
// owner's commands with a step-up for every grant, revocation, destination,
// policy and decision, sandbox:operate for the operator's), the tenant scope
// below (the owner reads and writes; an operator holding account:read_any
// reads; a record read or written by its own id is scoped by the record's own
// account and answers NOT_FOUND otherwise), and the service, which checks the
// owner again by the database. No route admits an agent: an agent's requests
// are admitted in-process, never over HTTP.

const delegationFeature = "delegated agent authority"

// delegationUSDPattern is the contract's USD shape: a decimal string with
// exactly two fraction digits.
var delegationUSDPattern = regexp.MustCompile(`^-?[0-9]+\.[0-9]{2}$`)

// delegationPlanHashPattern is a plan hash as the reads render it.
var delegationPlanHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// delegationExecuteDefault is how many subjects an execution run takes when
// the request does not say.
const delegationExecuteDefault = 10

func (s *Server) delegationPort() (DelegationPort, error) {
	if s.opts.Ports.Delegation == nil {
		return nil, errNotWired(delegationFeature)
	}
	return s.opts.Ports.Delegation, nil
}

// --- the account's reads ---------------------------------------------------------

// GetAdvancedDelegationAgents lists the account's agents.
func (s *Server) GetAdvancedDelegationAgents(ctx context.Context, request api.GetAdvancedDelegationAgentsRequestObject) (api.GetAdvancedDelegationAgentsResponseObject, error) {
	port, err := s.delegationPort()
	if err != nil {
		return nil, err
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	agents, err := port.Agents(ctx, account)
	if err != nil {
		return nil, err
	}
	out := api.DelegationAgentList{Items: make([]api.DelegationAgent, 0, len(agents))}
	for _, a := range agents {
		id, err := delegationWireUUID(a.ID)
		if err != nil {
			return nil, err
		}
		strategy, err := delegationWireUUID(a.StrategyID)
		if err != nil {
			return nil, err
		}
		out.Items = append(out.Items, api.DelegationAgent{Id: id, Name: a.Name, Stage: a.Stage, State: a.State, StrategyId: strategy,
			CreatedAt: a.CreatedAt})
	}
	return api.GetAdvancedDelegationAgents200JSONResponse(out), nil
}

// GetAdvancedDelegationCapabilities pages the account's capabilities.
func (s *Server) GetAdvancedDelegationCapabilities(ctx context.Context, request api.GetAdvancedDelegationCapabilitiesRequestObject) (api.GetAdvancedDelegationCapabilitiesResponseObject, error) {
	port, err := s.delegationPort()
	if err != nil {
		return nil, err
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	page, err := port.Capabilities(ctx, account, delegationCursorParam(request.Params.Cursor),
		pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	out, err := toAPIDelegationCapabilityPage(page)
	if err != nil {
		return nil, err
	}
	return api.GetAdvancedDelegationCapabilities200JSONResponse(out), nil
}

// GetAdvancedDelegationDestinations lists the account's allow-listed
// destinations.
func (s *Server) GetAdvancedDelegationDestinations(ctx context.Context, request api.GetAdvancedDelegationDestinationsRequestObject) (api.GetAdvancedDelegationDestinationsResponseObject, error) {
	port, err := s.delegationPort()
	if err != nil {
		return nil, err
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	items, err := port.Destinations(ctx, account)
	if err != nil {
		return nil, err
	}
	out := api.DelegationDestinationList{Items: make([]api.DelegationDestination, 0, len(items))}
	for _, d := range items {
		out.Items = append(out.Items, toAPIDelegationDestination(d))
	}
	return api.GetAdvancedDelegationDestinations200JSONResponse(out), nil
}

// GetAdvancedDelegationWithdrawalPolicy returns the account's withdrawal
// policy in force and its history.
func (s *Server) GetAdvancedDelegationWithdrawalPolicy(ctx context.Context, request api.GetAdvancedDelegationWithdrawalPolicyRequestObject) (api.GetAdvancedDelegationWithdrawalPolicyResponseObject, error) {
	port, err := s.delegationPort()
	if err != nil {
		return nil, err
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	v, err := port.WithdrawalPolicy(ctx, account)
	if err != nil {
		return nil, err
	}
	out := api.DelegationWithdrawalPolicyState{AccountId: toUUID(account), ValueDomain: api.SIMULATED,
		Versions: make([]api.DelegationWithdrawalPolicy, 0, len(v.Versions))}
	for _, p := range v.Versions {
		out.Versions = append(out.Versions, toAPIDelegationPolicy(p))
	}
	if len(out.Versions) > 0 {
		current := out.Versions[0]
		out.Current = &current
	}
	return api.GetAdvancedDelegationWithdrawalPolicy200JSONResponse(out), nil
}

// GetAdvancedDelegationSwaps pages the account's signing requests.
func (s *Server) GetAdvancedDelegationSwaps(ctx context.Context, request api.GetAdvancedDelegationSwapsRequestObject) (api.GetAdvancedDelegationSwapsResponseObject, error) {
	port, err := s.delegationPort()
	if err != nil {
		return nil, err
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	var status delegation.SwapStatus
	if request.Params.Status != nil {
		if !request.Params.Status.Valid() {
			return nil, validationError("status", "status is not a swap status")
		}
		status = delegation.SwapStatus(*request.Params.Status)
	}
	page, err := port.Swaps(ctx, account, status, delegationCursorParam(request.Params.Cursor),
		pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	out := api.DelegationSwapPage{ValueDomain: api.SIMULATED, NextCursor: nextCursor(page.NextCursor),
		Items: make([]api.DelegationSwap, 0, len(page.Items))}
	for _, v := range page.Items {
		sw, err := toAPIDelegationSwap(v)
		if err != nil {
			return nil, err
		}
		out.Items = append(out.Items, sw)
	}
	return api.GetAdvancedDelegationSwaps200JSONResponse(out), nil
}

// GetAdvancedDelegationWithdrawals pages the account's withdrawals.
func (s *Server) GetAdvancedDelegationWithdrawals(ctx context.Context, request api.GetAdvancedDelegationWithdrawalsRequestObject) (api.GetAdvancedDelegationWithdrawalsResponseObject, error) {
	port, err := s.delegationPort()
	if err != nil {
		return nil, err
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	var status delegation.WithdrawalStatus
	if request.Params.Status != nil {
		if !request.Params.Status.Valid() {
			return nil, validationError("status", "status is not a withdrawal status")
		}
		status = delegation.WithdrawalStatus(*request.Params.Status)
	}
	page, err := port.Withdrawals(ctx, account, status, delegationCursorParam(request.Params.Cursor),
		pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	out := api.DelegationWithdrawalPage{ValueDomain: api.SIMULATED, NextCursor: nextCursor(page.NextCursor),
		Items: make([]api.DelegationWithdrawal, 0, len(page.Items))}
	for _, v := range page.Items {
		w, err := toAPIDelegationWithdrawal(v)
		if err != nil {
			return nil, err
		}
		out.Items = append(out.Items, w)
	}
	return api.GetAdvancedDelegationWithdrawals200JSONResponse(out), nil
}

// --- records read by their own id ------------------------------------------------------

// GetAdvancedDelegationCapabilitiesCapabilityId returns one capability with
// its uses to its account's owner (or an operator who may read any account).
// A capability of another account answers exactly as one that does not exist.
func (s *Server) GetAdvancedDelegationCapabilitiesCapabilityId(ctx context.Context, request api.GetAdvancedDelegationCapabilitiesCapabilityIdRequestObject) (api.GetAdvancedDelegationCapabilitiesCapabilityIdResponseObject, error) {
	port, err := s.delegationPort()
	if err != nil {
		return nil, err
	}
	cid, err := delegationID("capabilityId", request.CapabilityId, delegation.ParseCapabilityID)
	if err != nil {
		return nil, err
	}
	v, err := s.delegationCapabilityFor(ctx, port, cid, security.RequireAccount)
	if err != nil {
		return nil, err
	}
	out, err := toAPIDelegationCapabilityDetail(v)
	if err != nil {
		return nil, err
	}
	return api.GetAdvancedDelegationCapabilitiesCapabilityId200JSONResponse(out), nil
}

// GetAdvancedDelegationSwapsSwapId returns one signing request.
func (s *Server) GetAdvancedDelegationSwapsSwapId(ctx context.Context, request api.GetAdvancedDelegationSwapsSwapIdRequestObject) (api.GetAdvancedDelegationSwapsSwapIdResponseObject, error) {
	port, err := s.delegationPort()
	if err != nil {
		return nil, err
	}
	sid, err := delegationID("swapId", request.SwapId, delegation.ParseSwapID)
	if err != nil {
		return nil, err
	}
	v, err := s.delegationSwapFor(ctx, port, sid, security.RequireAccount)
	if err != nil {
		return nil, err
	}
	out, err := toAPIDelegationSwap(v)
	if err != nil {
		return nil, err
	}
	return api.GetAdvancedDelegationSwapsSwapId200JSONResponse(out), nil
}

// GetAdvancedDelegationWithdrawalsWithdrawalId returns one withdrawal.
func (s *Server) GetAdvancedDelegationWithdrawalsWithdrawalId(ctx context.Context, request api.GetAdvancedDelegationWithdrawalsWithdrawalIdRequestObject) (api.GetAdvancedDelegationWithdrawalsWithdrawalIdResponseObject, error) {
	port, err := s.delegationPort()
	if err != nil {
		return nil, err
	}
	wid, err := delegationID("withdrawalId", request.WithdrawalId, delegation.ParseWithdrawalID)
	if err != nil {
		return nil, err
	}
	v, err := s.delegationWithdrawalFor(ctx, port, wid, security.RequireAccount)
	if err != nil {
		return nil, err
	}
	out, err := toAPIDelegationWithdrawal(v)
	if err != nil {
		return nil, err
	}
	return api.GetAdvancedDelegationWithdrawalsWithdrawalId200JSONResponse(out), nil
}

// GetAdvancedDelegationVenues lists the registered venues and this
// deployment's co-signers.
func (s *Server) GetAdvancedDelegationVenues(ctx context.Context, _ api.GetAdvancedDelegationVenuesRequestObject) (api.GetAdvancedDelegationVenuesResponseObject, error) {
	port, err := s.delegationPort()
	if err != nil {
		return nil, err
	}
	v, err := port.Venues(ctx)
	if err != nil {
		return nil, err
	}
	out := api.DelegationVenueList{Items: make([]api.DelegationVenue, 0, len(v.Venues)),
		CoSigners: make([]api.DelegationCoSigner, 0, len(v.CoSigners))}
	for _, venue := range v.Venues {
		out.Items = append(out.Items, toAPIDelegationVenue(venue))
	}
	for _, cs := range v.CoSigners {
		out.CoSigners = append(out.CoSigners, api.DelegationCoSigner{Name: cs.Name, Chain: api.DelegationChain(cs.Chain),
			Address: cs.Address.String(), Registered: cs.Registered})
	}
	return api.GetAdvancedDelegationVenues200JSONResponse(out), nil
}

// --- the owner's commands -----------------------------------------------------------------

// PostAdvancedDelegationCapabilities grants one of the account's agents a
// capability. The Idempotency-Key is the grant's own key as well.
func (s *Server) PostAdvancedDelegationCapabilities(ctx context.Context, request api.PostAdvancedDelegationCapabilitiesRequestObject) (api.PostAdvancedDelegationCapabilitiesResponseObject, error) {
	port, err := s.delegationPort()
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	account, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	req, err := delegationGrantRequest(account, *request.Body, request.Params.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.DelegationCapability, commandMeta, error) {
			v, gerr := port.Grant(ctx, req)
			if gerr != nil {
				return api.DelegationCapability{}, commandMeta{}, gerr
			}
			out, cerr := toAPIDelegationCapability(v)
			return out, commandMeta{Status: http.StatusOK, ResourceType: "delegated_capability", ResourceID: v.Capability.ID.String()}, cerr
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedDelegationCapabilities200JSONResponse(res.Value), nil
}

// PostAdvancedDelegationCapabilitiesCapabilityIdRevoke revokes a capability
// of the caller's own account.
func (s *Server) PostAdvancedDelegationCapabilitiesCapabilityIdRevoke(ctx context.Context, request api.PostAdvancedDelegationCapabilitiesCapabilityIdRevokeRequestObject) (api.PostAdvancedDelegationCapabilitiesCapabilityIdRevokeResponseObject, error) {
	port, err := s.delegationPort()
	if err != nil {
		return nil, err
	}
	cid, err := delegationID("capabilityId", request.CapabilityId, delegation.ParseCapabilityID)
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	if _, err := s.delegationCapabilityFor(ctx, port, cid, security.RequireAccountOwner); err != nil {
		return nil, err
	}
	reason := request.Body.Reason
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.DelegationCapability, commandMeta, error) {
			v, rerr := port.Revoke(ctx, delegation.RevokeRequest{CapabilityID: cid, Reason: reason})
			if rerr != nil {
				return api.DelegationCapability{}, commandMeta{}, rerr
			}
			out, cerr := toAPIDelegationCapability(v)
			return out, commandMeta{Status: http.StatusOK, ResourceType: "delegated_capability", ResourceID: cid.String()}, cerr
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedDelegationCapabilitiesCapabilityIdRevoke200JSONResponse(res.Value), nil
}

// PostAdvancedDelegationDestinations allow-lists a withdrawal destination.
func (s *Server) PostAdvancedDelegationDestinations(ctx context.Context, request api.PostAdvancedDelegationDestinationsRequestObject) (api.PostAdvancedDelegationDestinationsResponseObject, error) {
	port, err := s.delegationPort()
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	body := *request.Body
	account, err := accountScopeWrite(ctx, body.AccountId)
	if err != nil {
		return nil, err
	}
	chain, err := delegationChain("chain", body.Chain)
	if err != nil {
		return nil, err
	}
	address, err := delegationAddress("address", body.Address)
	if err != nil {
		return nil, err
	}
	req := delegation.DestinationRequest{AccountID: account, Chain: chain, Address: address, Label: body.Label}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.DelegationDestination, commandMeta, error) {
			d, aerr := port.AddDestination(ctx, req)
			if aerr != nil {
				return api.DelegationDestination{}, commandMeta{}, aerr
			}
			return toAPIDelegationDestination(d), commandMeta{Status: http.StatusOK, ResourceType: "delegated_withdrawal_destination",
				ResourceID: d.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedDelegationDestinations200JSONResponse(res.Value), nil
}

// PostAdvancedDelegationDestinationsDestinationIdRemove removes a destination
// of the caller's own account from the allow-list.
func (s *Server) PostAdvancedDelegationDestinationsDestinationIdRemove(ctx context.Context, request api.PostAdvancedDelegationDestinationsDestinationIdRemoveRequestObject) (api.PostAdvancedDelegationDestinationsDestinationIdRemoveResponseObject, error) {
	port, err := s.delegationPort()
	if err != nil {
		return nil, err
	}
	did, err := delegationID("destinationId", request.DestinationId, delegation.ParseDestinationID)
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	notFound := errs.New(errs.CodeNotFound, "no such destination").WithField("destination_id", did.String())
	d, err := port.Destination(ctx, did)
	if errs.HasCode(err, errs.CodeNotFound) {
		return nil, notFound
	}
	if err != nil {
		return nil, err
	}
	if err := security.RequireAccountOwner(ctx, d.AccountID.String()); err != nil {
		return nil, notFound
	}
	reason := request.Body.Reason
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.DelegationDestination, commandMeta, error) {
			removed, rerr := port.RemoveDestination(ctx, did, reason)
			if rerr != nil {
				return api.DelegationDestination{}, commandMeta{}, rerr
			}
			return toAPIDelegationDestination(removed), commandMeta{Status: http.StatusOK, ResourceType: "delegated_withdrawal_destination",
				ResourceID: did.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedDelegationDestinationsDestinationIdRemove200JSONResponse(res.Value), nil
}

// PostAdvancedDelegationWithdrawalPolicy writes the account's next withdrawal
// policy version.
func (s *Server) PostAdvancedDelegationWithdrawalPolicy(ctx context.Context, request api.PostAdvancedDelegationWithdrawalPolicyRequestObject) (api.PostAdvancedDelegationWithdrawalPolicyResponseObject, error) {
	port, err := s.delegationPort()
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	account, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	policy, err := delegationPolicyRequest(account, *request.Body)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.DelegationWithdrawalPolicy, commandMeta, error) {
			p, serr := port.SetWithdrawalPolicy(ctx, policy)
			if serr != nil {
				return api.DelegationWithdrawalPolicy{}, commandMeta{}, serr
			}
			return toAPIDelegationPolicy(p), commandMeta{Status: http.StatusOK, ResourceType: "delegated_withdrawal_policy",
				ResourceID: p.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedDelegationWithdrawalPolicy200JSONResponse(res.Value), nil
}

// PostAdvancedDelegationSwapsSwapIdDecision approves or declines a swap of
// the caller's own account, bound to the plan hash the caller was shown.
func (s *Server) PostAdvancedDelegationSwapsSwapIdDecision(ctx context.Context, request api.PostAdvancedDelegationSwapsSwapIdDecisionRequestObject) (api.PostAdvancedDelegationSwapsSwapIdDecisionResponseObject, error) {
	port, err := s.delegationPort()
	if err != nil {
		return nil, err
	}
	sid, err := delegationID("swapId", request.SwapId, delegation.ParseSwapID)
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	approve, planHash, err := delegationDecision(*request.Body)
	if err != nil {
		return nil, err
	}
	if _, err := s.delegationSwapFor(ctx, port, sid, security.RequireAccountOwner); err != nil {
		return nil, err
	}
	decision := delegation.SwapDecision{SwapID: sid, PlanHash: planHash, Approve: approve, Reason: request.Body.Reason}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.DelegationSwapDecisionResult, commandMeta, error) {
			o, derr := port.DecideSwap(ctx, decision)
			if derr != nil {
				return api.DelegationSwapDecisionResult{}, commandMeta{}, derr
			}
			sw, cerr := toAPIDelegationSwap(o.Swap)
			if cerr != nil {
				return api.DelegationSwapDecisionResult{}, commandMeta{}, cerr
			}
			return api.DelegationSwapDecisionResult{Swap: sw, Execution: toAPIDelegationExecution(ctx, o.Execution),
					ValueDomain: api.SIMULATED},
				commandMeta{Status: http.StatusOK, ResourceType: "delegated_swap", ResourceID: sid.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedDelegationSwapsSwapIdDecision200JSONResponse(res.Value), nil
}

// PostAdvancedDelegationWithdrawalsWithdrawalIdDecision approves or declines
// a withdrawal of the caller's own account, bound to the plan hash shown.
func (s *Server) PostAdvancedDelegationWithdrawalsWithdrawalIdDecision(ctx context.Context, request api.PostAdvancedDelegationWithdrawalsWithdrawalIdDecisionRequestObject) (api.PostAdvancedDelegationWithdrawalsWithdrawalIdDecisionResponseObject, error) {
	port, err := s.delegationPort()
	if err != nil {
		return nil, err
	}
	wid, err := delegationID("withdrawalId", request.WithdrawalId, delegation.ParseWithdrawalID)
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	approve, planHash, err := delegationDecision(*request.Body)
	if err != nil {
		return nil, err
	}
	if _, err := s.delegationWithdrawalFor(ctx, port, wid, security.RequireAccountOwner); err != nil {
		return nil, err
	}
	decision := delegation.WithdrawalDecision{WithdrawalID: wid, PlanHash: planHash, Approve: approve, Reason: request.Body.Reason}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.DelegationWithdrawalDecisionResult, commandMeta, error) {
			o, derr := port.DecideWithdrawal(ctx, decision)
			if derr != nil {
				return api.DelegationWithdrawalDecisionResult{}, commandMeta{}, derr
			}
			w, cerr := toAPIDelegationWithdrawal(o.Withdrawal)
			if cerr != nil {
				return api.DelegationWithdrawalDecisionResult{}, commandMeta{}, cerr
			}
			return api.DelegationWithdrawalDecisionResult{Withdrawal: w, Execution: toAPIDelegationExecution(ctx, o.Execution),
					ValueDomain: api.SIMULATED},
				commandMeta{Status: http.StatusOK, ResourceType: "delegated_withdrawal", ResourceID: wid.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedDelegationWithdrawalsWithdrawalIdDecision200JSONResponse(res.Value), nil
}

// PostAdvancedDelegationWithdrawalsWithdrawalIdCancel stops a withdrawal of
// the caller's own account that has not been signed.
func (s *Server) PostAdvancedDelegationWithdrawalsWithdrawalIdCancel(ctx context.Context, request api.PostAdvancedDelegationWithdrawalsWithdrawalIdCancelRequestObject) (api.PostAdvancedDelegationWithdrawalsWithdrawalIdCancelResponseObject, error) {
	port, err := s.delegationPort()
	if err != nil {
		return nil, err
	}
	wid, err := delegationID("withdrawalId", request.WithdrawalId, delegation.ParseWithdrawalID)
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	if _, err := s.delegationWithdrawalFor(ctx, port, wid, security.RequireAccountOwner); err != nil {
		return nil, err
	}
	reason := request.Body.Reason
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.DelegationWithdrawal, commandMeta, error) {
			v, cerr := port.CancelWithdrawal(ctx, wid, reason)
			if cerr != nil {
				return api.DelegationWithdrawal{}, commandMeta{}, cerr
			}
			out, werr := toAPIDelegationWithdrawal(v)
			return out, commandMeta{Status: http.StatusOK, ResourceType: "delegated_withdrawal", ResourceID: wid.String()}, werr
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedDelegationWithdrawalsWithdrawalIdCancel200JSONResponse(res.Value), nil
}

// --- operators -------------------------------------------------------------------------------

// GetAdvancedDelegationOpsCapabilities pages capabilities across accounts.
func (s *Server) GetAdvancedDelegationOpsCapabilities(ctx context.Context, request api.GetAdvancedDelegationOpsCapabilitiesRequestObject) (api.GetAdvancedDelegationOpsCapabilitiesResponseObject, error) {
	port, err := s.delegationPort()
	if err != nil {
		return nil, err
	}
	var status delegation.CapabilityStatus
	if request.Params.Status != nil {
		if !request.Params.Status.Valid() {
			return nil, validationError("status", "status is not a capability status")
		}
		status = delegation.CapabilityStatus(*request.Params.Status)
	}
	page, err := port.AllCapabilities(ctx, status, delegationCursorParam(request.Params.Cursor),
		pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	out, err := toAPIDelegationCapabilityPage(page)
	if err != nil {
		return nil, err
	}
	return api.GetAdvancedDelegationOpsCapabilities200JSONResponse(out), nil
}

// GetAdvancedDelegationOpsPending lists every swap and withdrawal not yet at
// an end, across accounts.
func (s *Server) GetAdvancedDelegationOpsPending(ctx context.Context, request api.GetAdvancedDelegationOpsPendingRequestObject) (api.GetAdvancedDelegationOpsPendingResponseObject, error) {
	port, err := s.delegationPort()
	if err != nil {
		return nil, err
	}
	v, err := port.Pending(ctx, pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	out := api.DelegationPending{AsOf: v.AsOf, ValueDomain: api.SIMULATED,
		Swaps: make([]api.DelegationSwap, 0, len(v.Swaps)), Withdrawals: make([]api.DelegationWithdrawal, 0, len(v.Withdrawals))}
	for _, sv := range v.Swaps {
		sw, err := toAPIDelegationSwap(sv)
		if err != nil {
			return nil, err
		}
		out.Swaps = append(out.Swaps, sw)
	}
	for _, wv := range v.Withdrawals {
		w, err := toAPIDelegationWithdrawal(wv)
		if err != nil {
			return nil, err
		}
		out.Withdrawals = append(out.Withdrawals, w)
	}
	return api.GetAdvancedDelegationOpsPending200JSONResponse(out), nil
}

// PostAdvancedDelegationOpsVenues registers a venue this deployment
// co-signs for.
func (s *Server) PostAdvancedDelegationOpsVenues(ctx context.Context, request api.PostAdvancedDelegationOpsVenuesRequestObject) (api.PostAdvancedDelegationOpsVenuesResponseObject, error) {
	port, err := s.delegationPort()
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	body := *request.Body
	reg := DelegationVenueRegistration{Name: strings.TrimSpace(body.Name), SpreadBPS: money.BPS(body.SpreadBps)}
	if reg.Name == "" {
		return nil, validationError("name", "name is required")
	}
	if body.HouseAccountId != nil {
		hid, err := delegationID("house_account_id", *body.HouseAccountId, econ.ParseHouseAccountID)
		if err != nil {
			return nil, err
		}
		reg.HouseAccountID = &hid
	}
	if reg.Actor, err = delegationActorOf(ctx); err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.DelegationVenue, commandMeta, error) {
			v, rerr := port.RegisterVenue(ctx, reg)
			if rerr != nil {
				return api.DelegationVenue{}, commandMeta{}, rerr
			}
			return toAPIDelegationVenue(v), commandMeta{Status: http.StatusOK, ResourceType: "delegation_venue",
				ResourceID: v.Venue.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedDelegationOpsVenues200JSONResponse(res.Value), nil
}

// PostAdvancedDelegationOpsExecutePending executes the swaps and withdrawals
// that can be signed now, fairly across accounts, and reports each one's
// outcome.
func (s *Server) PostAdvancedDelegationOpsExecutePending(ctx context.Context, request api.PostAdvancedDelegationOpsExecutePendingRequestObject) (api.PostAdvancedDelegationOpsExecutePendingResponseObject, error) {
	port, err := s.delegationPort()
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	limit := delegationExecuteDefault
	if request.Body.Limit != nil {
		limit = *request.Body.Limit
		if limit < 1 || limit > delegationExecuteMax {
			return nil, validationError("limit", "limit must be 1 to 50")
		}
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.DelegationExecutionRun, commandMeta, error) {
			results, xerr := port.ExecutePending(ctx, limit)
			if xerr != nil {
				return api.DelegationExecutionRun{}, commandMeta{}, xerr
			}
			out := api.DelegationExecutionRun{ValueDomain: api.SIMULATED, Items: make([]api.DelegationExecutionItem, 0, len(results))}
			for _, r := range results {
				id, perr := delegationWireUUID(r.ID)
				if perr != nil {
					return api.DelegationExecutionRun{}, commandMeta{}, perr
				}
				out.Items = append(out.Items, api.DelegationExecutionItem{Kind: api.DelegationSubjectKind(r.Kind), Id: id,
					AccountId: toUUID(r.AccountID), StatusBefore: r.StatusBefore, Status: r.Status,
					Error: toAPIDelegationExecutionError(ctx, r.Err)})
			}
			return out, commandMeta{Status: http.StatusOK, ResourceType: "delegation_execution_run"}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedDelegationOpsExecutePending200JSONResponse(res.Value), nil
}

// --- scoping of records read or written by their own id ---------------------------------------

// delegationCapabilityFor reads a capability and scopes it by its own
// account with require: RequireAccount for a read (the owner, or an operator
// who may read any account), RequireAccountOwner for a write (the owner
// alone). Anything else answers exactly as an absent id does.
func (s *Server) delegationCapabilityFor(ctx context.Context, port DelegationPort, cid delegation.CapabilityID,
	require func(context.Context, string) error) (DelegationCapabilityView, error) {
	notFound := errs.New(errs.CodeNotFound, "no such capability").WithField("capability_id", cid.String())
	v, err := port.Capability(ctx, cid)
	if errs.HasCode(err, errs.CodeNotFound) {
		return DelegationCapabilityView{}, notFound
	}
	if err != nil {
		return DelegationCapabilityView{}, err
	}
	if err := require(ctx, v.Capability.Terms.AccountID.String()); err != nil {
		return DelegationCapabilityView{}, notFound
	}
	return v, nil
}

func (s *Server) delegationSwapFor(ctx context.Context, port DelegationPort, sid delegation.SwapID,
	require func(context.Context, string) error) (DelegationSwapView, error) {
	notFound := errs.New(errs.CodeNotFound, "no such swap").WithField("swap_id", sid.String())
	v, err := port.Swap(ctx, sid)
	if errs.HasCode(err, errs.CodeNotFound) {
		return DelegationSwapView{}, notFound
	}
	if err != nil {
		return DelegationSwapView{}, err
	}
	if err := require(ctx, v.Swap.AccountID.String()); err != nil {
		return DelegationSwapView{}, notFound
	}
	return v, nil
}

func (s *Server) delegationWithdrawalFor(ctx context.Context, port DelegationPort, wid delegation.WithdrawalID,
	require func(context.Context, string) error) (DelegationWithdrawalView, error) {
	notFound := errs.New(errs.CodeNotFound, "no such withdrawal").WithField("withdrawal_id", wid.String())
	v, err := port.Withdrawal(ctx, wid)
	if errs.HasCode(err, errs.CodeNotFound) {
		return DelegationWithdrawalView{}, notFound
	}
	if err != nil {
		return DelegationWithdrawalView{}, err
	}
	if err := require(ctx, v.Withdrawal.AccountID.String()); err != nil {
		return DelegationWithdrawalView{}, notFound
	}
	return v, nil
}

// --- request shapes -------------------------------------------------------------------------------

func delegationCursorParam(c *api.Cursor) string {
	if c == nil {
		return ""
	}
	return *c
}

// delegationID parses a path or body identifier; the nil UUID names nothing.
func delegationID[T interface{ IsZero() bool }](field string, raw uuid.UUID, parse func(string) (T, error)) (T, error) {
	v, err := parse(raw.String())
	if err != nil || v.IsZero() {
		var zero T
		return zero, validationError(field, field+" must be a canonical UUID")
	}
	return v, nil
}

func delegationUSD(field string, raw api.USD) (money.USD, error) {
	if !delegationUSDPattern.MatchString(raw) {
		return money.USD{}, validationError(field, field+" must be a USD decimal string with exactly two fraction digits")
	}
	u, err := money.ParseUSD(raw)
	if err != nil {
		return money.USD{}, validationError(field, field+" is not a USD amount")
	}
	return u, nil
}

func delegationChain(field string, raw api.DelegationChain) (devnet.ChainID, error) {
	if !raw.Valid() {
		return "", validationError(field, field+" must be sbx-solana or sbx-ethereum")
	}
	return devnet.ChainID(raw), nil
}

func delegationAddress(field, raw string) (devnet.PublicKey, error) {
	k, err := devnet.ParsePublicKey(strings.TrimSpace(raw))
	if err != nil || k.IsZero() {
		return devnet.PublicKey{}, validationError(field, field+" must be a simulated-chain address")
	}
	return k, nil
}

func delegationGrantRequest(account accounts.AccountID, body api.DelegationGrantRequest, key string) (delegation.GrantRequest, error) {
	if body.AgentId == uuid.Nil {
		return delegation.GrantRequest{}, validationError("agent_id", "agent_id must be a canonical UUID")
	}
	if !body.Mode.Valid() {
		return delegation.GrantRequest{}, validationError("mode", "mode is not an advanced mode")
	}
	req := delegation.GrantRequest{AccountID: account, AgentID: body.AgentId.String(), Mode: gates.Mode(body.Mode),
		ExpiresAt: body.ExpiresAt, RequiresHumanApproval: body.RequiresHumanApproval, IdempotencyKey: key}
	for _, c := range body.InstrumentClasses {
		if !c.Valid() {
			return delegation.GrantRequest{}, validationError("instrument_classes", "an instrument class is not one the contract declares")
		}
		req.InstrumentClasses = append(req.InstrumentClasses, delegation.InstrumentClass(c))
	}
	for _, a := range body.ActionClasses {
		if !a.Valid() {
			return delegation.GrantRequest{}, validationError("action_classes", "an action class is not one the contract declares")
		}
		req.ActionClasses = append(req.ActionClasses, delegation.ActionClass(a))
	}
	for _, c := range body.ChainAllowlist {
		chain, err := delegationChain("chain_allowlist", c)
		if err != nil {
			return delegation.GrantRequest{}, err
		}
		req.ChainAllowlist = append(req.ChainAllowlist, chain)
	}
	for _, d := range body.DestinationAllowlist {
		k, err := delegationAddress("destination_allowlist", d)
		if err != nil {
			return delegation.GrantRequest{}, err
		}
		req.DestinationAllowlist = append(req.DestinationAllowlist, k)
	}
	var err error
	if req.MaxNotional, err = delegationUSD("max_notional_usd", body.MaxNotionalUsd); err != nil {
		return delegation.GrantRequest{}, err
	}
	if req.MaxDailyNotional, err = delegationUSD("max_daily_notional_usd", body.MaxDailyNotionalUsd); err != nil {
		return delegation.GrantRequest{}, err
	}
	if req.MaxSingleTrade, err = delegationUSD("max_single_trade_usd", body.MaxSingleTradeUsd); err != nil {
		return delegation.GrantRequest{}, err
	}
	if body.ExpiresAt.IsZero() {
		return delegation.GrantRequest{}, validationError("expires_at", "expires_at is required")
	}
	return req, nil
}

func delegationPolicyRequest(account accounts.AccountID, body api.DelegationWithdrawalPolicyRequest) (delegation.WithdrawalPolicy, error) {
	p := delegation.WithdrawalPolicy{AccountID: account, AutoApproveEnabled: body.AutoApproveEnabled,
		AutoApproveMaxRiskScore: body.AutoApproveMaxRiskScore, MaxRiskScore: body.MaxRiskScore, VelocityMaxCount: body.VelocityMaxCount,
		StrongAuthRequired: body.StrongAuthRequired}
	var err error
	if p.AutoApproveMax, err = delegationUSD("auto_approve_max_usd", body.AutoApproveMaxUsd); err != nil {
		return delegation.WithdrawalPolicy{}, err
	}
	if p.PerWithdrawalLimit, err = delegationUSD("per_withdrawal_limit_usd", body.PerWithdrawalLimitUsd); err != nil {
		return delegation.WithdrawalPolicy{}, err
	}
	if p.DailyLimit, err = delegationUSD("daily_limit_usd", body.DailyLimitUsd); err != nil {
		return delegation.WithdrawalPolicy{}, err
	}
	for _, d := range []struct {
		field   string
		seconds int64
		dst     *time.Duration
	}{
		{"min_destination_age_seconds", body.MinDestinationAgeSeconds, &p.MinDestinationAge},
		{"min_account_age_seconds", body.MinAccountAgeSeconds, &p.MinAccountAge},
		{"cooling_period_seconds", body.CoolingPeriodSeconds, &p.CoolingPeriod},
		{"strong_auth_max_age_seconds", body.StrongAuthMaxAgeSeconds, &p.StrongAuthMaxAge},
	} {
		// The service bounds every duration; this only keeps the conversion
		// to a duration from overflowing.
		if d.seconds < 0 || d.seconds > int64(400*24*time.Hour/time.Second) {
			return delegation.WithdrawalPolicy{}, validationError(d.field, d.field+" is out of range")
		}
		*d.dst = time.Duration(d.seconds) * time.Second
	}
	return p, nil
}

func delegationDecision(body api.DelegationDecisionRequest) (bool, [32]byte, error) {
	var hash [32]byte
	if !body.Decision.Valid() {
		return false, hash, validationError("decision", "decision must be APPROVE or DECLINE")
	}
	if !delegationPlanHashPattern.MatchString(body.PlanHash) {
		return false, hash, validationError("plan_hash", "plan_hash must be the 64 lowercase hexadecimal characters of the plan shown")
	}
	raw, err := hex.DecodeString(body.PlanHash)
	if err != nil {
		return false, hash, validationError("plan_hash", "plan_hash is not hexadecimal")
	}
	copy(hash[:], raw)
	return body.Decision == api.DelegationDecisionApprove, hash, nil
}

func delegationActorOf(ctx context.Context) (delegation.Actor, error) {
	p, ok := security.PrincipalFrom(ctx)
	if !ok || p.SubjectID == "" {
		return delegation.Actor{}, errs.New(errs.CodeUnauthenticated, "authentication is required")
	}
	return delegation.Actor{Type: p.ActorType, ID: p.SubjectID}, nil
}

// --- response shapes -----------------------------------------------------------------------------

func delegationWireUUID(s string) (api.UUID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return api.UUID{}, errs.New(errs.CodeInternal, "delegation: a stored identifier is not a UUID")
	}
	return u, nil
}

func delegationWireActor(a delegation.Actor) api.DelegationActor {
	return api.DelegationActor{ActorType: string(a.Type), ActorId: a.ID}
}

func delegationWireActorPtr(a *delegation.Actor) *api.DelegationActor {
	if a == nil {
		return nil
	}
	v := delegationWireActor(*a)
	return &v
}

// delegationRemaining is a limit less its use, never below zero.
func delegationRemaining(limit, used money.USD) money.USD {
	left, err := limit.Sub(used)
	if err != nil || left.IsNegative() {
		return money.USDFromMinor(0)
	}
	return left
}

// delegationStanding is the capability's standing at the read: REVOKED when
// revoked, EXPIRED from its expiry on (expiry is decided against the clock,
// never stored), ACTIVE otherwise.
func delegationStanding(c delegation.Capability, asOf time.Time) api.DelegationCapabilityStanding {
	switch {
	case c.Status == delegation.CapabilityRevoked:
		return api.DelegationStandingRevoked
	case !asOf.Before(c.Terms.ExpiresAt):
		return api.DelegationStandingExpired
	default:
		return api.DelegationStandingActive
	}
}

func toAPIDelegationCapability(v DelegationCapabilityView) (api.DelegationCapability, error) {
	c := v.Capability
	t := c.Terms
	agent, err := delegationWireUUID(t.AgentID)
	if err != nil {
		return api.DelegationCapability{}, err
	}
	strategy, err := delegationWireUUID(t.StrategyID)
	if err != nil {
		return api.DelegationCapability{}, err
	}
	out := api.DelegationCapability{
		Id: toUUID(c.ID), AccountId: toUUID(t.AccountID), AgentId: agent, AgentName: strPtr(v.AgentName), StrategyId: strategy,
		Environment: string(t.Environment), Mode: api.AdvancedMode(t.Mode),
		InstrumentClasses: make([]api.DelegationInstrumentClass, 0, len(t.InstrumentClasses)),
		ActionClasses:     make([]api.DelegationActionClass, 0, len(t.ActionClasses)),
		MaxNotionalUsd:    t.MaxNotional.String(), MaxDailyNotionalUsd: t.MaxDailyNotional.String(),
		MaxSingleTradeUsd: t.MaxSingleTrade.String(), DestinationAllowlist: make([]string, 0, len(t.DestinationAllowlist)),
		ChainAllowlist: make([]api.DelegationChain, 0, len(t.ChainAllowlist)), GrantedAt: c.GrantedAt, ExpiresAt: t.ExpiresAt,
		RequiresHumanApproval: t.RequiresHumanApproval, Status: api.DelegationCapabilityStatus(c.Status),
		Standing: delegationStanding(c, v.AsOf), ContentHash: c.HashHex(), GrantedBy: delegationWireActor(c.GrantedBy),
		RevokedBy: delegationWireActorPtr(c.RevokedBy), RevocationReason: strPtr(c.RevocationReason), RevokedAt: timePtr(c.RevokedAt),
		Usage: api.DelegationCapabilityUsage{
			Day: v.Usage.Day.Format(time.DateOnly), Uses: v.Usage.Uses, UsedTodayUsd: v.Usage.UsedToday.String(),
			UsedTotalUsd:      v.Usage.UsedTotal.String(),
			RemainingTodayUsd: delegationRemaining(t.MaxDailyNotional, v.Usage.UsedToday).String(),
			RemainingTotalUsd: delegationRemaining(t.MaxNotional, v.Usage.UsedTotal).String(),
		},
		AsOf: v.AsOf, ValueDomain: api.SIMULATED,
	}
	for _, ic := range t.InstrumentClasses {
		out.InstrumentClasses = append(out.InstrumentClasses, api.DelegationInstrumentClass(ic))
	}
	for _, ac := range t.ActionClasses {
		out.ActionClasses = append(out.ActionClasses, api.DelegationActionClass(ac))
	}
	for _, d := range t.DestinationAllowlist {
		out.DestinationAllowlist = append(out.DestinationAllowlist, d.String())
	}
	for _, ch := range t.ChainAllowlist {
		out.ChainAllowlist = append(out.ChainAllowlist, api.DelegationChain(ch))
	}
	return out, nil
}

func toAPIDelegationCapabilityPage(page DelegationCapabilityPage) (api.DelegationCapabilityPage, error) {
	out := api.DelegationCapabilityPage{ValueDomain: api.SIMULATED, NextCursor: nextCursor(page.NextCursor),
		Items: make([]api.DelegationCapability, 0, len(page.Items))}
	for _, v := range page.Items {
		c, err := toAPIDelegationCapability(v)
		if err != nil {
			return api.DelegationCapabilityPage{}, err
		}
		out.Items = append(out.Items, c)
	}
	return out, nil
}

func toAPIDelegationCapabilityDetail(v DelegationCapabilityView) (api.DelegationCapabilityDetail, error) {
	c, err := toAPIDelegationCapability(v)
	if err != nil {
		return api.DelegationCapabilityDetail{}, err
	}
	out := api.DelegationCapabilityDetail{
		Id: c.Id, AccountId: c.AccountId, AgentId: c.AgentId, AgentName: c.AgentName, StrategyId: c.StrategyId,
		Environment: c.Environment, Mode: c.Mode, InstrumentClasses: c.InstrumentClasses, ActionClasses: c.ActionClasses,
		MaxNotionalUsd: c.MaxNotionalUsd, MaxDailyNotionalUsd: c.MaxDailyNotionalUsd, MaxSingleTradeUsd: c.MaxSingleTradeUsd,
		DestinationAllowlist: c.DestinationAllowlist, ChainAllowlist: c.ChainAllowlist, GrantedAt: c.GrantedAt,
		ExpiresAt: c.ExpiresAt, RequiresHumanApproval: c.RequiresHumanApproval, Status: c.Status, Standing: c.Standing,
		ContentHash: c.ContentHash, GrantedBy: c.GrantedBy, RevokedBy: c.RevokedBy, RevocationReason: c.RevocationReason,
		RevokedAt: c.RevokedAt, Usage: c.Usage, AsOf: c.AsOf, ValueDomain: c.ValueDomain,
		Uses: make([]api.DelegationCapabilityUse, 0, len(v.Uses)),
	}
	for _, u := range v.Uses {
		id, err := delegationWireUUID(u.ID)
		if err != nil {
			return api.DelegationCapabilityDetail{}, err
		}
		subject, err := delegationWireUUID(u.SubjectID)
		if err != nil {
			return api.DelegationCapabilityDetail{}, err
		}
		out.Uses = append(out.Uses, api.DelegationCapabilityUse{Id: id, SubjectKind: api.DelegationSubjectKind(u.SubjectKind),
			SubjectId: subject, ActionClass: api.DelegationActionClass(u.Action), NotionalUsd: u.Notional.String(), UsedAt: u.UsedAt})
	}
	return out, nil
}

func toAPIDelegationDestination(d delegation.Destination) api.DelegationDestination {
	return api.DelegationDestination{
		Id: toUUID(d.ID), AccountId: toUUID(d.AccountID), Chain: api.DelegationChain(d.Chain), Address: d.Address.String(),
		Label: d.Label, Status: api.DelegationDestinationStatus(d.Status), AddedBy: delegationWireActor(d.AddedBy),
		AddedAt: d.AddedAt, RemovedBy: delegationWireActorPtr(d.RemovedBy), RemovedAt: timePtr(d.RemovedAt),
	}
}

func toAPIDelegationPolicy(p delegation.WithdrawalPolicy) api.DelegationWithdrawalPolicy {
	return api.DelegationWithdrawalPolicy{
		Id: toUUID(p.ID), AccountId: toUUID(p.AccountID), Version: p.Version, AutoApproveEnabled: p.AutoApproveEnabled,
		AutoApproveMaxUsd: p.AutoApproveMax.String(), AutoApproveMaxRiskScore: p.AutoApproveMaxRiskScore,
		MaxRiskScore: p.MaxRiskScore, PerWithdrawalLimitUsd: p.PerWithdrawalLimit.String(), DailyLimitUsd: p.DailyLimit.String(),
		VelocityMaxCount: p.VelocityMaxCount, MinDestinationAgeSeconds: int64(p.MinDestinationAge / time.Second),
		MinAccountAgeSeconds: int64(p.MinAccountAge / time.Second), CoolingPeriodSeconds: int64(p.CoolingPeriod / time.Second),
		StrongAuthRequired: p.StrongAuthRequired, StrongAuthMaxAgeSeconds: int64(p.StrongAuthMaxAge / time.Second),
		PolicyHash: hex.EncodeToString(p.PolicyHash[:]), CreatedBy: delegationWireActor(p.CreatedBy), EffectiveAt: p.EffectiveAt,
		CreatedAt: p.CreatedAt, ValueDomain: api.SIMULATED,
	}
}

func toAPIDelegationVenue(v DelegationVenueView) api.DelegationVenue {
	return api.DelegationVenue{
		Id: toUUID(v.Venue.ID), Name: v.Venue.Name, Chain: api.DelegationChain(v.Venue.Chain), Address: v.Venue.Address.String(),
		HouseAccountId: toUUID(v.Venue.HouseAccountID), SpreadBps: api.BPS(v.Venue.SpreadBPS), CoSigner: v.CoSigner,
		CreatedBy: delegationWireActor(v.Venue.CreatedBy), CreatedAt: v.Venue.CreatedAt,
	}
}

func delegationWirePrice(p delegation.PlanPrice) api.DelegationPlanPrice {
	return api.DelegationPlanPrice{Mantissa: p.Mantissa, Scale: p.Scale, Source: p.Source, ObservedAt: p.ObservedAt.UTC(), Par: p.Par}
}

func toAPIDelegationApproval(a *DelegationApprovalView) *api.DelegationApproval {
	if a == nil {
		return nil
	}
	return &api.DelegationApproval{Decision: api.DelegationDecision(a.Decision), PlanHash: hex.EncodeToString(a.PlanHash),
		Approver: delegationWireActor(a.Approver), AuthenticatedAt: timePtr(a.AuthenticatedAt), DecidedAt: a.DecidedAt, Reason: a.Reason}
}

func toAPIDelegationInspection(d *DelegationInspectionView) (*api.DelegationInspection, error) {
	if d == nil {
		return nil, nil
	}
	id, err := delegationWireUUID(d.ID)
	if err != nil {
		return nil, err
	}
	return &api.DelegationInspection{
		Id: id, Verdict: api.DelegationVerdict(d.Verdict), ReasonCodes: append([]string{}, d.ReasonCodes...),
		SignerPublicKey: d.SignerPublicKey, CapabilityHash: hex.EncodeToString(d.CapabilityHash), PlanHash: hex.EncodeToString(d.PlanHash),
		MessageSha256: hex.EncodeToString(d.MessageSHA256), PolicyVersion: d.PolicyVersion, PolicyHash: d.PolicyHash,
		InspectorVersion: d.InspectorVersion, DecidedAt: d.DecidedAt,
	}, nil
}

func toAPIDelegationTransitions(in []DelegationTransitionView) []api.DelegationTransition {
	out := make([]api.DelegationTransition, 0, len(in))
	for _, t := range in {
		out = append(out, api.DelegationTransition{FromStatus: t.From, ToStatus: t.To, Actor: delegationWireActor(t.Actor),
			Reason: t.Reason, OccurredAt: t.At})
	}
	return out
}

func toAPIDelegationSwap(v DelegationSwapView) (api.DelegationSwap, error) {
	s := v.Swap
	agent, err := delegationWireUUID(s.AgentID)
	if err != nil {
		return api.DelegationSwap{}, err
	}
	inspection, err := toAPIDelegationInspection(v.Inspection)
	if err != nil {
		return api.DelegationSwap{}, err
	}
	p := s.Plan
	out := api.DelegationSwap{
		Id: toUUID(s.ID), CapabilityId: toUUID(s.CapabilityID), AccountId: toUUID(s.AccountID), AgentId: agent,
		RequestNonce: delegationUint(s.RequestNonce), IntentId: toUUID(s.IntentID), ReservationId: toUUID(s.ReservationID),
		Venue: api.DelegationVenueRef{Id: toUUID(v.Venue.ID), Name: v.Venue.Name, Address: v.Venue.Address},
		Mode:  api.AdvancedMode(s.Mode), ActionClass: api.DelegationActionClass(s.Action), Chain: api.DelegationChain(s.Chain),
		Wallet: s.Wallet.String(),
		PayAsset: api.DelegationAssetRef{Id: toUUID(v.PayAsset.ID), Symbol: v.PayAsset.Symbol,
			Decimals: int(v.PayAsset.Decimals)},
		ReceiveAsset: api.DelegationAssetRef{Id: toUUID(v.ReceiveAsset.ID), Symbol: v.ReceiveAsset.Symbol,
			Decimals: int(v.ReceiveAsset.Decimals)},
		AmountIn: s.AmountIn.String(), AmountOut: s.AmountOut.String(), MinAmountOut: s.MinAmountOut.String(),
		NotionalUsd: s.NotionalUSD.String(), RequestedAt: s.RequestedAt, QuoteExpiresAt: s.QuoteExpiresAt,
		RequiresApproval: s.RequiresApproval,
		Plan: api.DelegationSwapPlan{
			Version: p.Version, SwapId: p.SwapID, AccountId: p.AccountID, AgentId: p.AgentID, CapabilityId: p.CapabilityID,
			CapabilityHash: p.CapabilityHash, IntentId: p.IntentID, RequestNonce: p.RequestNonce, ActionClass: p.ActionClass,
			Mode: p.Mode, Chain: p.Chain, Wallet: p.Wallet, VenueId: p.VenueID, VenueAddress: p.VenueAddress,
			PayAssetId: p.PayAssetID, PayMint: p.PayMint, PayDecimals: p.PayDecimals, ReceiveAssetId: p.ReceiveAssetID,
			ReceiveMint: p.ReceiveMint, ReceiveDecimals: p.ReceiveDecimals, AmountIn: p.AmountIn, AmountOut: p.AmountOut,
			MinAmountOut: p.MinAmountOut, PayPrice: delegationWirePrice(p.PayPrice), ReceivePrice: delegationWirePrice(p.ReceivePrice),
			SpreadBps: p.SpreadBPS, MaxSlippageBps: p.MaxSlippageBPS, NotionalUsd: p.NotionalUSD, FeePerTx: p.FeePerTx,
			RequestedAt: p.RequestedAt.UTC(), QuoteExpiresAt: p.QuoteExpiresAt.UTC(),
		},
		PlanHash: hex.EncodeToString(s.PlanHash[:]), Status: api.DelegationSwapStatus(s.Status), SignedAt: timePtr(s.SignedAt),
		TxId: strPtr(string(s.TxID)), FailureCode: strPtr(s.FailureCode), Approval: toAPIDelegationApproval(v.Approval),
		Inspection: inspection, Transitions: toAPIDelegationTransitions(v.Transitions), CreatedAt: s.CreatedAt,
		UpdatedAt: s.UpdatedAt, ValueDomain: api.SIMULATED,
	}
	if s.EffectID != nil {
		out.EffectId = uuidPtr(*s.EffectID)
	}
	if s.JournalTransactionID != nil {
		out.JournalTransactionId = uuidPtr(*s.JournalTransactionID)
	}
	return out, nil
}

func toAPIDelegationWithdrawal(v DelegationWithdrawalView) (api.DelegationWithdrawal, error) {
	w := v.Withdrawal
	agent, err := delegationWireUUID(w.AgentID)
	if err != nil {
		return api.DelegationWithdrawal{}, err
	}
	inspection, err := toAPIDelegationInspection(v.Inspection)
	if err != nil {
		return api.DelegationWithdrawal{}, err
	}
	evaluation, err := toAPIDelegationEvaluation(w.Evaluation)
	if err != nil {
		return api.DelegationWithdrawal{}, err
	}
	p := w.Plan
	out := api.DelegationWithdrawal{
		Id: toUUID(w.ID), CapabilityId: toUUID(w.CapabilityID), AccountId: toUUID(w.AccountID), AgentId: agent,
		ProposalNonce: delegationUint(w.ProposalNonce), IntentId: toUUID(w.IntentID), ReservationId: toUUID(w.ReservationID),
		PolicyId: toUUID(w.PolicyID),
		Destination: api.DelegationDestinationRef{Id: toUUID(v.Destination.ID), Label: v.Destination.Label,
			Status: api.DelegationDestinationStatus(v.Destination.Status)},
		Environment: string(w.Environment), Mode: api.AdvancedMode(w.Mode), Chain: api.DelegationChain(w.Chain),
		Wallet: w.Wallet.String(), DestinationAddress: w.DestinationAddress.String(),
		Asset:  api.DelegationAssetRef{Id: toUUID(v.Asset.ID), Symbol: v.Asset.Symbol, Decimals: int(v.Asset.Decimals)},
		Amount: w.Amount.String(), NotionalUsd: w.NotionalUSD.String(), ProposedAt: w.ProposedAt,
		ApprovalExpiresAt: w.ApprovalExpiresAt, ExecutableAt: w.ExecutableAt, Evaluation: evaluation,
		Plan: api.DelegationWithdrawalPlan{
			Version: p.Version, WithdrawalId: p.WithdrawalID, AccountId: p.AccountID, AgentId: p.AgentID,
			CapabilityId: p.CapabilityID, CapabilityHash: p.CapabilityHash, IntentId: p.IntentID, ProposalNonce: p.ProposalNonce,
			Mode: p.Mode, Chain: p.Chain, Wallet: p.Wallet, DestinationId: p.DestinationID, DestinationAddress: p.DestinationAddress,
			AssetId: p.AssetID, Mint: p.Mint, Decimals: p.Decimals, Amount: p.Amount, NotionalUsd: p.NotionalUSD,
			Price: delegationWirePrice(p.Price), PolicyId: p.PolicyID, PolicyVersion: p.PolicyVersion, PolicyHash: p.PolicyHash,
			EvaluationOutcome: p.EvaluationOutcome, RiskScore: p.RiskScore, FeePerTx: p.FeePerTx, ProposedAt: p.ProposedAt.UTC(),
			ExecutableAt: p.ExecutableAt.UTC(), ApprovalExpiresAt: p.ApprovalExpiresAt.UTC(),
		},
		PlanHash: hex.EncodeToString(w.PlanHash[:]), Status: api.DelegationWithdrawalStatus(w.Status), SignedAt: timePtr(w.SignedAt),
		TxId: strPtr(string(w.TxID)), FailureCode: strPtr(w.FailureCode), Approval: toAPIDelegationApproval(v.Approval),
		Inspection: inspection, Transitions: toAPIDelegationTransitions(v.Transitions), CreatedAt: w.CreatedAt,
		UpdatedAt: w.UpdatedAt, ValueDomain: api.SIMULATED,
	}
	if w.EffectID != nil {
		out.EffectId = uuidPtr(*w.EffectID)
	}
	if w.JournalTransactionID != nil {
		out.JournalTransactionId = uuidPtr(*w.JournalTransactionID)
	}
	return out, nil
}

// toAPIDelegationEvaluation renders the policy's recorded evaluation. Its
// figures are decimal strings as the service stored them; the day total is in
// cents and is rendered as the sandbox dollar.
func toAPIDelegationEvaluation(e delegation.Evaluation) (api.DelegationWithdrawalEvaluation, error) {
	destinationAge, err := delegationInt64(e.DestinationAge)
	if err != nil {
		return api.DelegationWithdrawalEvaluation{}, err
	}
	accountAge, err := delegationInt64(e.AccountAge)
	if err != nil {
		return api.DelegationWithdrawalEvaluation{}, err
	}
	dayTotalMinor, err := delegationInt64(e.DayTotalBefore)
	if err != nil {
		return api.DelegationWithdrawalEvaluation{}, err
	}
	count, err := delegationInt64(e.CountLast24h)
	if err != nil {
		return api.DelegationWithdrawalEvaluation{}, err
	}
	return api.DelegationWithdrawalEvaluation{
		Outcome: api.DelegationEvaluationOutcome(e.Outcome), RiskScore: e.RiskScore, ReasonCodes: append([]string{}, e.ReasonCodes...),
		Anomalies: append([]string{}, e.Anomalies...), DestinationAgeSeconds: destinationAge, AccountAgeSeconds: accountAge,
		DayTotalBeforeUsd: money.USDFromMinor(dayTotalMinor).String(), CountLast24h: int(count), PolicyVersion: e.PolicyVersion,
		PolicyHash: e.PolicyHash, Environment: e.Environment, ComplianceClear: e.ComplianceClear,
	}, nil
}

// toAPIDelegationExecution reports what followed a decision. A failure is
// rendered as its problem would be: its stable code, the delegation reason
// when the service gave one, and a detail that never leaks an internal error.
func toAPIDelegationExecution(ctx context.Context, x DelegationExecution) api.DelegationExecution {
	return api.DelegationExecution{Attempted: x.Attempted, Error: toAPIDelegationExecutionError(ctx, x.Err)}
}

func toAPIDelegationExecutionError(ctx context.Context, err error) *api.DelegationExecutionError {
	if err == nil {
		return nil
	}
	e := classify(ctx, err)
	p := errs.ToProblem(e, "", "")
	out := &api.DelegationExecutionError{Code: string(p.Code), Detail: p.Detail}
	if r, ok := p.Fields["reason"].(string); ok && r != "" {
		out.Reason = &r
	} else if codes, ok := p.Fields["reason_codes"].([]string); ok && len(codes) > 0 {
		joined := strings.Join(codes, ",")
		out.Reason = &joined
	}
	return out
}

// delegationUint renders a request or proposal nonce as the decimal string
// the plan carries.
func delegationUint(v uint64) string { return strconv.FormatUint(v, 10) }

// delegationInt64 reads one of the evaluation's decimal-string figures.
func delegationInt64(s string) (int64, error) {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, errs.New(errs.CodeInternal, "delegation: a stored evaluation figure is not an integer")
	}
	return v, nil
}
