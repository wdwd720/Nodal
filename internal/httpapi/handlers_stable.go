package httpapi

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/stableasset"
)

// The sandbox stable asset routes (internal/stableasset). Handlers translate
// shapes only: the stable asset service decides every figure, every status
// and every refusal, and its error codes reach the wire unchanged. Customer
// commands are the account owner's (accountScopeWrite) under sandbox:stable;
// operator commands need sandbox:operate, and those that register, pause,
// resume or move value a recent step-up as well (authz.go).

// stableNotWired is the answer of every route where no sandbox chain is
// configured: the stable asset runs on that chain, so without it there is
// nothing to answer with.
func stableNotWired() *errs.Error {
	return errNotWired("the sandbox stable asset (no sandbox chain is configured: CP_SANDBOX_DEVNET_NETWORK)")
}

// stableBatchLimitMax is the most requests one operator pass may settle.
const stableBatchLimitMax = 500

// --- reference reads --------------------------------------------------------------

// GetAdvancedStableAssets lists the registered stable assets.
func (s *Server) GetAdvancedStableAssets(ctx context.Context, request api.GetAdvancedStableAssetsRequestObject) (api.GetAdvancedStableAssetsResponseObject, error) {
	if s.opts.Ports.Stable == nil {
		return nil, stableNotWired()
	}
	views, err := s.opts.Ports.Stable.Assets(ctx, pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	out := api.StableAssetList{ValueDomain: api.SIMULATED, Items: make([]api.StableAsset, 0, len(views))}
	for _, v := range views {
		item, cerr := toAPIStableAsset(v)
		if cerr != nil {
			return nil, cerr
		}
		out.Items = append(out.Items, item)
	}
	return api.GetAdvancedStableAssets200JSONResponse(out), nil
}

// GetAdvancedStableAssetsStableAssetId returns one stable asset with its
// status history and newest peg-monitor alerts.
func (s *Server) GetAdvancedStableAssetsStableAssetId(ctx context.Context, request api.GetAdvancedStableAssetsStableAssetIdRequestObject) (api.GetAdvancedStableAssetsStableAssetIdResponseObject, error) {
	if s.opts.Ports.Stable == nil {
		return nil, stableNotWired()
	}
	sid, err := stableAssetIDParam(request.StableAssetId.String())
	if err != nil {
		return nil, err
	}
	v, err := s.opts.Ports.Stable.Asset(ctx, sid)
	if err != nil {
		return nil, err
	}
	out, err := toAPIStableAssetDetail(v)
	if err != nil {
		return nil, err
	}
	return api.GetAdvancedStableAssetsStableAssetId200JSONResponse(out), nil
}

// GetAdvancedStableAssetsStableAssetIdReserve returns the issuer's reserve
// ledger of one stable asset.
func (s *Server) GetAdvancedStableAssetsStableAssetIdReserve(ctx context.Context, request api.GetAdvancedStableAssetsStableAssetIdReserveRequestObject) (api.GetAdvancedStableAssetsStableAssetIdReserveResponseObject, error) {
	if s.opts.Ports.Stable == nil {
		return nil, stableNotWired()
	}
	sid, err := stableAssetIDParam(request.StableAssetId.String())
	if err != nil {
		return nil, err
	}
	v, err := s.opts.Ports.Stable.Reserve(ctx, sid)
	if err != nil {
		return nil, err
	}
	out, err := toAPIStableReserve(v.Asset, v.Ledger, v.Labels)
	if err != nil {
		return nil, err
	}
	return api.GetAdvancedStableAssetsStableAssetIdReserve200JSONResponse(out), nil
}

// GetAdvancedStableAssetsStableAssetIdSnapshots returns the newest part of an
// asset's signed snapshot chain and the verification of the whole chain.
func (s *Server) GetAdvancedStableAssetsStableAssetIdSnapshots(ctx context.Context, request api.GetAdvancedStableAssetsStableAssetIdSnapshotsRequestObject) (api.GetAdvancedStableAssetsStableAssetIdSnapshotsResponseObject, error) {
	if s.opts.Ports.Stable == nil {
		return nil, stableNotWired()
	}
	sid, err := stableAssetIDParam(request.StableAssetId.String())
	if err != nil {
		return nil, err
	}
	v, err := s.opts.Ports.Stable.Snapshots(ctx, sid, pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	out := api.StableSnapshotChain{
		ValueDomain: api.SIMULATED, StableAssetId: toUUID(v.StableAssetID), Total: v.Total,
		Verification: toAPIStableSnapshotVerification(v.Verification), Items: make([]api.StableSnapshot, 0, len(v.Items)),
	}
	for i, sn := range v.Items {
		item, cerr := toAPIStableSnapshot(sn, v.Verification.VerifiedAt(v.Offset+i))
		if cerr != nil {
			return nil, cerr
		}
		out.Items = append(out.Items, item)
	}
	return api.GetAdvancedStableAssetsStableAssetIdSnapshots200JSONResponse(out), nil
}

// --- account reads -------------------------------------------------------------------

// GetAdvancedStableAssetsHoldings returns an account's stable asset holdings.
func (s *Server) GetAdvancedStableAssetsHoldings(ctx context.Context, request api.GetAdvancedStableAssetsHoldingsRequestObject) (api.GetAdvancedStableAssetsHoldingsResponseObject, error) {
	if s.opts.Ports.Stable == nil {
		return nil, stableNotWired()
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	v, err := s.opts.Ports.Stable.Holdings(ctx, account)
	if err != nil {
		return nil, err
	}
	return api.GetAdvancedStableAssetsHoldings200JSONResponse(toAPIStableHoldings(v)), nil
}

// GetAdvancedStableAssetsIssuances pages an account's mint requests.
func (s *Server) GetAdvancedStableAssetsIssuances(ctx context.Context, request api.GetAdvancedStableAssetsIssuancesRequestObject) (api.GetAdvancedStableAssetsIssuancesResponseObject, error) {
	if s.opts.Ports.Stable == nil {
		return nil, stableNotWired()
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	filter, err := stableAssetFilter(request.Params.StableAssetId)
	if err != nil {
		return nil, err
	}
	page, err := s.opts.Ports.Stable.Issuances(ctx, account, filter, stableCursorParam(request.Params.Cursor),
		pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	out := api.StableIssuancePage{ValueDomain: api.SIMULATED, Items: make([]api.StableIssuance, 0, len(page.Items)), NextCursor: nextCursor(page.NextCursor)}
	for _, v := range page.Items {
		item, cerr := toAPIStableIssuance(v)
		if cerr != nil {
			return nil, cerr
		}
		out.Items = append(out.Items, item)
	}
	return api.GetAdvancedStableAssetsIssuances200JSONResponse(out), nil
}

// GetAdvancedStableAssetsIssuancesStableIssuanceId returns one mint request
// to its account's owner (or an operator who may read any account). A
// request of another account answers exactly as one that does not exist.
func (s *Server) GetAdvancedStableAssetsIssuancesStableIssuanceId(ctx context.Context, request api.GetAdvancedStableAssetsIssuancesStableIssuanceIdRequestObject) (api.GetAdvancedStableAssetsIssuancesStableIssuanceIdResponseObject, error) {
	if s.opts.Ports.Stable == nil {
		return nil, stableNotWired()
	}
	var rid stableasset.MintRequestID
	if err := rid.UnmarshalText([]byte(request.StableIssuanceId.String())); err != nil || rid.IsZero() {
		return nil, validationError("stableIssuanceId", "stableIssuanceId must be a canonical UUID")
	}
	// One answer for "absent" and "not yours", built here, so the refusal
	// cannot tell a stranger the id exists (F-41).
	notFound := errs.New(errs.CodeNotFound, "no such issuance request").WithField("request_id", rid.String())
	v, err := s.opts.Ports.Stable.Issuance(ctx, rid)
	if errs.HasCode(err, errs.CodeNotFound) {
		return nil, notFound
	}
	if err != nil {
		return nil, err
	}
	if err := security.RequireAccount(ctx, v.Record.AccountID.String()); err != nil {
		return nil, notFound
	}
	out, err := toAPIStableIssuance(v)
	if err != nil {
		return nil, err
	}
	return api.GetAdvancedStableAssetsIssuancesStableIssuanceId200JSONResponse(out), nil
}

// GetAdvancedStableAssetsRedemptions pages an account's burn requests.
func (s *Server) GetAdvancedStableAssetsRedemptions(ctx context.Context, request api.GetAdvancedStableAssetsRedemptionsRequestObject) (api.GetAdvancedStableAssetsRedemptionsResponseObject, error) {
	if s.opts.Ports.Stable == nil {
		return nil, stableNotWired()
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	filter, err := stableAssetFilter(request.Params.StableAssetId)
	if err != nil {
		return nil, err
	}
	page, err := s.opts.Ports.Stable.Redemptions(ctx, account, filter, stableCursorParam(request.Params.Cursor),
		pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	out := api.StableRedemptionPage{ValueDomain: api.SIMULATED, Items: make([]api.StableRedemption, 0, len(page.Items)), NextCursor: nextCursor(page.NextCursor)}
	for _, v := range page.Items {
		item, cerr := toAPIStableRedemption(v)
		if cerr != nil {
			return nil, cerr
		}
		out.Items = append(out.Items, item)
	}
	return api.GetAdvancedStableAssetsRedemptions200JSONResponse(out), nil
}

// GetAdvancedStableAssetsRedemptionsStableRedemptionId returns one burn
// request to its account's owner (or an operator who may read any account);
// another account's answers NOT_FOUND.
func (s *Server) GetAdvancedStableAssetsRedemptionsStableRedemptionId(ctx context.Context, request api.GetAdvancedStableAssetsRedemptionsStableRedemptionIdRequestObject) (api.GetAdvancedStableAssetsRedemptionsStableRedemptionIdResponseObject, error) {
	if s.opts.Ports.Stable == nil {
		return nil, stableNotWired()
	}
	var rid stableasset.BurnRequestID
	if err := rid.UnmarshalText([]byte(request.StableRedemptionId.String())); err != nil || rid.IsZero() {
		return nil, validationError("stableRedemptionId", "stableRedemptionId must be a canonical UUID")
	}
	notFound := errs.New(errs.CodeNotFound, "no such redemption request").WithField("request_id", rid.String())
	v, err := s.opts.Ports.Stable.Redemption(ctx, rid)
	if errs.HasCode(err, errs.CodeNotFound) {
		return nil, notFound
	}
	if err != nil {
		return nil, err
	}
	if err := security.RequireAccount(ctx, v.Record.AccountID.String()); err != nil {
		return nil, notFound
	}
	out, err := toAPIStableRedemption(v)
	if err != nil {
		return nil, err
	}
	return api.GetAdvancedStableAssetsRedemptionsStableRedemptionId200JSONResponse(out), nil
}

// --- the customer's commands ----------------------------------------------------------

// PostAdvancedStableAssetsStableAssetIdIssuances mints for the caller's own
// account. 200 when the request is final; 202 when it is recorded and not
// final, including a submission whose outcome is unknown.
func (s *Server) PostAdvancedStableAssetsStableAssetIdIssuances(ctx context.Context, request api.PostAdvancedStableAssetsStableAssetIdIssuancesRequestObject) (api.PostAdvancedStableAssetsStableAssetIdIssuancesResponseObject, error) {
	if s.opts.Ports.Stable == nil {
		return nil, stableNotWired()
	}
	sid, err := stableAssetIDParam(request.StableAssetId.String())
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
	qty, err := money.ParseQuantity(request.Body.Quantity)
	if err != nil {
		return nil, validationError("quantity", "quantity must be an integer string of the stable asset's base units")
	}
	reserve, err := assets.ParseAssetID(request.Body.ReserveAssetId.String())
	if err != nil || reserve.IsZero() {
		return nil, validationError("reserve_asset_id", "reserve_asset_id must be a canonical UUID")
	}
	mode, err := stableModeParam(request.Body.Mode)
	if err != nil {
		return nil, err
	}
	actor, err := stableActor(ctx)
	if err != nil {
		return nil, err
	}
	cmd := StableIssueCommand{
		StableAssetID: sid, AccountID: account, Quantity: qty, ReserveAssetID: reserve, Mode: mode,
		IdempotencyKey: request.Params.IdempotencyKey, Actor: actor, CorrelationID: observability.CorrelationID(ctx),
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.StableIssuanceResult, commandMeta, error) {
			o, ierr := s.opts.Ports.Stable.Issue(ctx, cmd)
			if ierr != nil {
				return api.StableIssuanceResult{}, commandMeta{}, ierr
			}
			item, cerr := toAPIStableIssuance(o.View)
			if cerr != nil {
				return api.StableIssuanceResult{}, commandMeta{}, cerr
			}
			out := api.StableIssuanceResult{ValueDomain: api.SIMULATED, Replay: o.Replay, Settled: o.Settled, Issuance: item}
			return out, commandMeta{Status: stableCommandStatus(o.Settled), ResourceType: "stable_mint_request", ResourceID: o.View.Record.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	if res.Meta.Status == http.StatusAccepted {
		return api.PostAdvancedStableAssetsStableAssetIdIssuances202JSONResponse(res.Value), nil
	}
	return api.PostAdvancedStableAssetsStableAssetIdIssuances200JSONResponse(res.Value), nil
}

// PostAdvancedStableAssetsStableAssetIdRedemptions burns for the caller's own
// account, 200 when final and 202 when recorded and not final.
func (s *Server) PostAdvancedStableAssetsStableAssetIdRedemptions(ctx context.Context, request api.PostAdvancedStableAssetsStableAssetIdRedemptionsRequestObject) (api.PostAdvancedStableAssetsStableAssetIdRedemptionsResponseObject, error) {
	if s.opts.Ports.Stable == nil {
		return nil, stableNotWired()
	}
	sid, err := stableAssetIDParam(request.StableAssetId.String())
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
	qty, err := money.ParseQuantity(request.Body.Quantity)
	if err != nil {
		return nil, validationError("quantity", "quantity must be an integer string of the stable asset's base units")
	}
	mode, err := stableModeParam(request.Body.Mode)
	if err != nil {
		return nil, err
	}
	actor, err := stableActor(ctx)
	if err != nil {
		return nil, err
	}
	cmd := StableRedeemCommand{
		StableAssetID: sid, AccountID: account, Quantity: qty, Mode: mode,
		IdempotencyKey: request.Params.IdempotencyKey, Actor: actor, CorrelationID: observability.CorrelationID(ctx),
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.StableRedemptionResult, commandMeta, error) {
			o, rerr := s.opts.Ports.Stable.Redeem(ctx, cmd)
			if rerr != nil {
				return api.StableRedemptionResult{}, commandMeta{}, rerr
			}
			item, cerr := toAPIStableRedemption(o.View)
			if cerr != nil {
				return api.StableRedemptionResult{}, commandMeta{}, cerr
			}
			out := api.StableRedemptionResult{ValueDomain: api.SIMULATED, Replay: o.Replay, Settled: o.Settled, Redemption: item}
			return out, commandMeta{Status: stableCommandStatus(o.Settled), ResourceType: "stable_burn_request", ResourceID: o.View.Record.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	if res.Meta.Status == http.StatusAccepted {
		return api.PostAdvancedStableAssetsStableAssetIdRedemptions202JSONResponse(res.Value), nil
	}
	return api.PostAdvancedStableAssetsStableAssetIdRedemptions200JSONResponse(res.Value), nil
}

// --- the operator's commands ------------------------------------------------------------

// PostAdvancedStableAssets registers a stable asset pegged in the sandbox
// dollar. The peg's quote asset is never the caller's choice.
func (s *Server) PostAdvancedStableAssets(ctx context.Context, request api.PostAdvancedStableAssetsRequestObject) (api.PostAdvancedStableAssetsResponseObject, error) {
	if s.opts.Ports.Stable == nil {
		return nil, stableNotWired()
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	def, err := stableDefinition(*request.Body, s.clk.Now())
	if err != nil {
		return nil, err
	}
	actor, err := stableActor(ctx)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.StableAssetDetail, commandMeta, error) {
			v, rerr := s.opts.Ports.Stable.Register(ctx, def, actor)
			if rerr != nil {
				return api.StableAssetDetail{}, commandMeta{}, rerr
			}
			out, cerr := toAPIStableAssetDetail(v)
			if cerr != nil {
				return api.StableAssetDetail{}, commandMeta{}, cerr
			}
			return out, commandMeta{Status: http.StatusOK, ResourceType: "stable_asset", ResourceID: v.Asset.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedStableAssets200JSONResponse(res.Value), nil
}

// PostAdvancedStableAssetsStableAssetIdPause pauses a stable asset.
func (s *Server) PostAdvancedStableAssetsStableAssetIdPause(ctx context.Context, request api.PostAdvancedStableAssetsStableAssetIdPauseRequestObject) (api.PostAdvancedStableAssetsStableAssetIdPauseResponseObject, error) {
	out, err := s.stableSetStatus(ctx, request.StableAssetId.String(), request.Body, request.Params.IdempotencyKey, stableasset.StatusPaused)
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedStableAssetsStableAssetIdPause200JSONResponse(out), nil
}

// PostAdvancedStableAssetsStableAssetIdResume resumes a paused stable asset;
// the service accepts it from an operator only.
func (s *Server) PostAdvancedStableAssetsStableAssetIdResume(ctx context.Context, request api.PostAdvancedStableAssetsStableAssetIdResumeRequestObject) (api.PostAdvancedStableAssetsStableAssetIdResumeResponseObject, error) {
	out, err := s.stableSetStatus(ctx, request.StableAssetId.String(), request.Body, request.Params.IdempotencyKey, stableasset.StatusActive)
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedStableAssetsStableAssetIdResume200JSONResponse(out), nil
}

func (s *Server) stableSetStatus(ctx context.Context, rawID string, body *api.StableReasonRequest, key string, to stableasset.Status) (api.StableAssetDetail, error) {
	if s.opts.Ports.Stable == nil {
		return api.StableAssetDetail{}, stableNotWired()
	}
	sid, err := stableAssetIDParam(rawID)
	if err != nil {
		return api.StableAssetDetail{}, err
	}
	if body == nil {
		return api.StableAssetDetail{}, validationError("body", "a request body is required")
	}
	actor, err := stableActor(ctx)
	if err != nil {
		return api.StableAssetDetail{}, err
	}
	reason := body.Reason
	res, err := runCommand(ctx, s, key, func(ctx context.Context) (api.StableAssetDetail, commandMeta, error) {
		v, serr := s.opts.Ports.Stable.SetStatus(ctx, sid, to, actor, reason)
		if serr != nil {
			return api.StableAssetDetail{}, commandMeta{}, serr
		}
		out, cerr := toAPIStableAssetDetail(v)
		if cerr != nil {
			return api.StableAssetDetail{}, commandMeta{}, cerr
		}
		return out, commandMeta{Status: http.StatusOK, ResourceType: "stable_asset", ResourceID: sid.String()}, nil
	})
	if err != nil {
		return api.StableAssetDetail{}, err
	}
	return res.Value, nil
}

// PostAdvancedStableAssetsStableAssetIdMonitorRuns runs the peg monitor. It
// moves no value; its circuit breaker may pause the asset.
func (s *Server) PostAdvancedStableAssetsStableAssetIdMonitorRuns(ctx context.Context, request api.PostAdvancedStableAssetsStableAssetIdMonitorRunsRequestObject) (api.PostAdvancedStableAssetsStableAssetIdMonitorRunsResponseObject, error) {
	if s.opts.Ports.Stable == nil {
		return nil, stableNotWired()
	}
	sid, err := stableAssetIDParam(request.StableAssetId.String())
	if err != nil {
		return nil, err
	}
	actor, err := stableActor(ctx)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.StableMonitorReport, commandMeta, error) {
			v, merr := s.opts.Ports.Stable.RunMonitor(ctx, sid, actor)
			if merr != nil {
				return api.StableMonitorReport{}, commandMeta{}, merr
			}
			out, cerr := toAPIStableMonitor(v)
			if cerr != nil {
				return api.StableMonitorReport{}, commandMeta{}, cerr
			}
			return out, commandMeta{Status: http.StatusOK, ResourceType: "stable_asset", ResourceID: sid.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedStableAssetsStableAssetIdMonitorRuns200JSONResponse(res.Value), nil
}

// PostAdvancedStableAssetsStableAssetIdReconciliations reconciles the asset's
// supply. It moves no value; a mismatch trips the circuit breaker.
func (s *Server) PostAdvancedStableAssetsStableAssetIdReconciliations(ctx context.Context, request api.PostAdvancedStableAssetsStableAssetIdReconciliationsRequestObject) (api.PostAdvancedStableAssetsStableAssetIdReconciliationsResponseObject, error) {
	if s.opts.Ports.Stable == nil {
		return nil, stableNotWired()
	}
	sid, err := stableAssetIDParam(request.StableAssetId.String())
	if err != nil {
		return nil, err
	}
	actor, err := stableActor(ctx)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.StableSupplyReconciliation, commandMeta, error) {
			r, rerr := s.opts.Ports.Stable.ReconcileSupply(ctx, sid, actor)
			if rerr != nil {
				return api.StableSupplyReconciliation{}, commandMeta{}, rerr
			}
			meta := commandMeta{Status: http.StatusOK, ResourceType: "stable_asset", ResourceID: sid.String()}
			if r.RecordID != "" {
				meta.ResourceType, meta.ResourceID = "reconciliation_record", r.RecordID
			}
			return toAPIStableSupply(r), meta, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedStableAssetsStableAssetIdReconciliations200JSONResponse(res.Value), nil
}

// PostAdvancedStableAssetsStableAssetIdSnapshots records the next signed
// reserve snapshot. It moves no value.
func (s *Server) PostAdvancedStableAssetsStableAssetIdSnapshots(ctx context.Context, request api.PostAdvancedStableAssetsStableAssetIdSnapshotsRequestObject) (api.PostAdvancedStableAssetsStableAssetIdSnapshotsResponseObject, error) {
	if s.opts.Ports.Stable == nil {
		return nil, stableNotWired()
	}
	sid, err := stableAssetIDParam(request.StableAssetId.String())
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.StableSnapshotResult, commandMeta, error) {
			v, terr := s.opts.Ports.Stable.TakeSnapshot(ctx, sid)
			if terr != nil {
				return api.StableSnapshotResult{}, commandMeta{}, terr
			}
			sn, cerr := toAPIStableSnapshot(v.Snapshot, v.Verification.VerifiedAt(v.Index))
			if cerr != nil {
				return api.StableSnapshotResult{}, commandMeta{}, cerr
			}
			out := api.StableSnapshotResult{ValueDomain: api.SIMULATED, Snapshot: sn, Verification: toAPIStableSnapshotVerification(v.Verification)}
			return out, commandMeta{Status: http.StatusOK, ResourceType: "stable_reserve_snapshot", ResourceID: hex.EncodeToString(v.Snapshot.Hash[:])}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedStableAssetsStableAssetIdSnapshots200JSONResponse(res.Value), nil
}

// PostAdvancedStableAssetsStableAssetIdRedemptionQueueRun pays out the
// asset's queued redemptions.
func (s *Server) PostAdvancedStableAssetsStableAssetIdRedemptionQueueRun(ctx context.Context, request api.PostAdvancedStableAssetsStableAssetIdRedemptionQueueRunRequestObject) (api.PostAdvancedStableAssetsStableAssetIdRedemptionQueueRunResponseObject, error) {
	if s.opts.Ports.Stable == nil {
		return nil, stableNotWired()
	}
	sid, err := stableAssetIDParam(request.StableAssetId.String())
	if err != nil {
		return nil, err
	}
	limit, err := stableBatchLimit(request.Body)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.StableSettleReport, commandMeta, error) {
			r, perr := s.opts.Ports.Stable.ProcessRedemptions(ctx, sid, limit)
			if perr != nil {
				return api.StableSettleReport{}, commandMeta{}, perr
			}
			return toAPIStableSettleReport(r), commandMeta{Status: http.StatusOK, ResourceType: "stable_asset", ResourceID: sid.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedStableAssetsStableAssetIdRedemptionQueueRun200JSONResponse(res.Value), nil
}

// PostAdvancedStableAssetsOpsResolveUnknown settles every request whose
// submission outcome is unknown from the chain by transaction id.
func (s *Server) PostAdvancedStableAssetsOpsResolveUnknown(ctx context.Context, request api.PostAdvancedStableAssetsOpsResolveUnknownRequestObject) (api.PostAdvancedStableAssetsOpsResolveUnknownResponseObject, error) {
	if s.opts.Ports.Stable == nil {
		return nil, stableNotWired()
	}
	limit, err := stableBatchLimit(request.Body)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.StableSettleReport, commandMeta, error) {
			r, rerr := s.opts.Ports.Stable.ResolveUnknown(ctx, limit)
			if rerr != nil {
				return api.StableSettleReport{}, commandMeta{}, rerr
			}
			return toAPIStableSettleReport(r), commandMeta{Status: http.StatusOK, ResourceType: "stable_unknown_submissions"}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedStableAssetsOpsResolveUnknown200JSONResponse(res.Value), nil
}

// --- request helpers ----------------------------------------------------------------------

func stableAssetIDParam(raw string) (stableasset.StableAssetID, error) {
	sid, err := stableasset.ParseStableAssetID(raw)
	if err != nil || sid.IsZero() {
		return stableasset.StableAssetID{}, validationError("stableAssetId", "stableAssetId must be a canonical UUID")
	}
	return sid, nil
}

func stableAssetFilter(u *api.UUID) (*stableasset.StableAssetID, error) {
	if u == nil {
		return nil, nil
	}
	sid, err := stableasset.ParseStableAssetID(u.String())
	if err != nil || sid.IsZero() {
		return nil, validationError("stable_asset_id", "stable_asset_id must be a canonical UUID")
	}
	return &sid, nil
}

func stableCursorParam(c *api.Cursor) string {
	if c == nil {
		return ""
	}
	return *c
}

// stableModeParam accepts the two modes a request may run in; the service
// decides whether the mode may run in this deployment.
func stableModeParam(m api.StableRequestMode) (gates.Mode, error) {
	if !m.Valid() {
		return "", validationError("mode", "mode must be SANDBOX or DEVNET")
	}
	return gates.Mode(m), nil
}

func stableActor(ctx context.Context) (econ.Actor, error) {
	p, ok := security.PrincipalFrom(ctx)
	if !ok || p.SubjectID == "" {
		return econ.Actor{}, errs.New(errs.CodeUnauthenticated, "authentication is required")
	}
	return econ.Actor{Type: p.ActorType, ID: p.SubjectID}, nil
}

// stableCommandStatus is 200 for a request that is final and 202 for one that
// is recorded and not final: in flight, or with an unknown outcome.
func stableCommandStatus(settled bool) int {
	if settled {
		return http.StatusOK
	}
	return http.StatusAccepted
}

// stableBatchLimit reads an operator pass's optional limit; zero lets the
// service use its default.
func stableBatchLimit(body *api.StableBatchRequest) (int, error) {
	if body == nil || body.Limit == nil {
		return 0, nil
	}
	if *body.Limit < 1 || *body.Limit > stableBatchLimitMax {
		return 0, validationError("limit", "limit must be between 1 and 500")
	}
	return *body.Limit, nil
}

// stableDefinition translates a registration request. The peg is quoted in
// the sandbox dollar; every rule about the definition is the service's.
func stableDefinition(b api.StableAssetDefinition, now time.Time) (stableasset.Definition, error) {
	if b.Decimals < 0 || b.Decimals > math.MaxUint8 {
		return stableasset.Definition{}, validationError("decimals", "decimals must be between 0 and 18")
	}
	issuanceCap, err := money.ParseQuantity(b.IssuanceCap)
	if err != nil {
		return stableasset.Definition{}, validationError("issuance_cap", "issuance_cap must be an integer string of base units")
	}
	if !b.RedemptionMode.Valid() {
		return stableasset.Definition{}, validationError("redemption_mode", "redemption_mode must be INSTANT or QUEUED")
	}
	peg, err := money.PriceFromDecimalString(b.Peg, econ.SandboxDollarID().String(), "TARGET_PEG", now)
	if err != nil {
		return stableasset.Definition{}, validationError("peg", "peg must be an exact decimal such as one whole sandbox dollar written out")
	}
	if b.MaxPriceAgeMs < 1 || b.MaxPriceAgeMs > math.MaxInt64/int64(time.Millisecond) {
		return stableasset.Definition{}, validationError("max_price_age_ms", "max_price_age_ms must be a positive number of milliseconds a duration can hold")
	}
	def := stableasset.Definition{
		Name: b.Name, Symbol: b.Symbol, PegCurrency: b.PegCurrency, Decimals: uint8(b.Decimals), IssuanceCap: issuanceCap,
		RedemptionMode: stableasset.RedemptionMode(b.RedemptionMode), PegQuoteAssetID: econ.SandboxDollarID(),
		PegMantissa: peg.Mantissa, PegScale: peg.Scale, OracleSource: b.OracleSource,
		MaxPriceAge:           time.Duration(b.MaxPriceAgeMs) * time.Millisecond,
		PegDeviationThreshold: money.BPS(b.PegDeviationThresholdBps), Attestable: b.Attestable,
	}
	for i, r := range b.Reserves {
		aid, perr := assets.ParseAssetID(r.AssetId.String())
		if perr != nil || aid.IsZero() {
			return stableasset.Definition{}, validationError("reserves", "reserves["+strconv.Itoa(i)+"].asset_id must be a canonical UUID")
		}
		if !r.Kind.Valid() {
			return stableasset.Definition{}, validationError("reserves", "reserves["+strconv.Itoa(i)+"].kind is not a reserve kind")
		}
		def.Reserves = append(def.Reserves, stableasset.ReserveSpec{
			AssetID: aid, Kind: stableasset.ReserveKind(r.Kind), FactorBPS: money.BPS(r.FactorBps), AcceptedForMint: r.AcceptedForMint,
		})
	}
	return def, nil
}

// --- rendering ---------------------------------------------------------------------------

func stableLabelOf(labels map[assets.AssetID]StableAssetLabel, aid assets.AssetID) (StableAssetLabel, error) {
	l, ok := labels[aid]
	if !ok {
		return StableAssetLabel{}, errs.New(errs.CodeInternal, "an asset the response names was not resolved").WithField("asset_id", aid.String())
	}
	return l, nil
}

func toAPIStableActor(a econ.Actor) api.StableActor {
	return api.StableActor{ActorType: string(a.Type), ActorId: a.ID}
}

// toAPIStablePrice renders a price exactly: its mantissa and scale, and the
// same value written as a decimal.
func toAPIStablePrice(p money.Price) (api.StablePrice, error) {
	if p.Scale < 0 || p.Scale > money.MaxPriceScale {
		return api.StablePrice{}, errs.New(errs.CodeInternal, "a stored price has a scale out of range")
	}
	return api.StablePrice{
		Mantissa: p.Mantissa.String(), Scale: int(p.Scale), Decimal: p.Mantissa.ToDecimalString(uint8(p.Scale)),
		Quote: p.QuoteAsset, Source: p.Source, ObservedAt: p.At.UTC(),
	}, nil
}

// toAPIStablePriceDoc renders a price as a plan or a reserve line recorded it.
func toAPIStablePriceDoc(d stableasset.PriceDoc) (api.StablePrice, error) {
	mantissa, err := money.ParseQuantity(d.Mantissa)
	if err != nil {
		return api.StablePrice{}, errs.Wrap(err, errs.CodeInternal, "a recorded price mantissa is not an integer")
	}
	return toAPIStablePrice(money.Price{Mantissa: mantissa, Scale: d.Scale, QuoteAsset: d.Quote, Source: d.Source, At: d.At})
}

func toAPIStableAsset(v StableAssetView) (api.StableAsset, error) {
	a := v.Asset
	peg, err := toAPIStablePrice(a.Peg)
	if err != nil {
		return api.StableAsset{}, err
	}
	out := api.StableAsset{
		ValueDomain: api.SIMULATED, Id: toUUID(a.ID), Name: a.Name, Symbol: v.Label.Symbol, PegCurrency: v.PegCurrency,
		AssetId: toUUID(a.AssetID), FacilityId: toUUID(a.FacilityID), Network: a.Network, Chain: string(a.Chain),
		Mint: a.Mint.String(), Decimals: int(a.Decimals), Status: api.StableAssetStatus(a.Status), IssuanceCap: a.IssuanceCap.String(),
		MintAuthorityMode: string(a.MintAuthorityMode), MintAuthority: a.MintAuthority.String(),
		BurnAuthorityMode: string(a.BurnAuthorityMode), RedemptionMode: api.StableRedemptionMode(a.RedemptionMode), Peg: peg,
		OracleSource: a.OracleSource, MaxPriceAgeMs: a.MaxPriceAge.Milliseconds(),
		PegDeviationThresholdBps: int(a.PegDeviationThreshold), Attestable: a.Attestable,
		Reserves: make([]api.StableReserveAsset, 0, len(a.Reserves)), CreatedBy: toAPIStableActor(a.CreatedBy),
		CreatedAt: a.CreatedAt.UTC(), UpdatedAt: a.UpdatedAt.UTC(),
	}
	for _, r := range a.Reserves {
		l, lerr := stableLabelOf(v.Reserves, r.AssetID)
		if lerr != nil {
			return api.StableAsset{}, lerr
		}
		out.Reserves = append(out.Reserves, api.StableReserveAsset{
			AssetId: toUUID(r.AssetID), Symbol: l.Symbol, Decimals: int(r.Decimals), Kind: api.StableReserveKind(r.Kind),
			FactorBps: int(r.FactorBPS), AcceptedForMint: r.AcceptedForMint,
		})
	}
	return out, nil
}

func toAPIStableAssetDetail(v StableAssetDetailView) (api.StableAssetDetail, error) {
	asset, err := toAPIStableAsset(v.StableAssetView)
	if err != nil {
		return api.StableAssetDetail{}, err
	}
	out := api.StableAssetDetail{
		ValueDomain: api.SIMULATED, Asset: asset,
		Transitions: make([]api.StableAssetTransition, 0, len(v.Transitions)), Alerts: make([]api.StableAlert, 0, len(v.Alerts)),
	}
	for _, t := range v.Transitions {
		out.Transitions = append(out.Transitions, api.StableAssetTransition{
			FromStatus: t.From, ToStatus: api.StableAssetStatus(t.To), Actor: toAPIStableActor(t.Actor), Reason: t.Reason,
			OccurredAt: t.OccurredAt.UTC(),
		})
	}
	for _, al := range v.Alerts {
		observed := map[string]interface{}{}
		if len(al.Observed) > 0 {
			if jerr := json.Unmarshal(al.Observed, &observed); jerr != nil {
				return api.StableAssetDetail{}, errs.Wrap(jerr, errs.CodeInternal, "a recorded peg alert is not a JSON object")
			}
		}
		out.Alerts = append(out.Alerts, api.StableAlert{
			Id: toUUID(al.ID), Kind: api.StableAlertKind(al.Kind), Severity: api.StableAlertSeverity(al.Severity),
			BreakerTripped: al.BreakerTripped, Observed: observed, ObservedAt: al.ObservedAt.UTC(),
		})
	}
	return out, nil
}

func toAPIStableReserveHolding(h stableasset.ReserveHolding, labels map[assets.AssetID]StableAssetLabel) (api.StableReserveHolding, error) {
	l, err := stableLabelOf(labels, h.AssetID)
	if err != nil {
		return api.StableReserveHolding{}, err
	}
	out := api.StableReserveHolding{
		AssetId: toUUID(h.AssetID), Symbol: l.Symbol, Decimals: int(h.Decimals), Kind: api.StableReserveKind(h.Kind),
		Quantity: h.Quantity.String(), FactorBps: int(h.FactorBPS), Eligible: h.Eligible.String(),
	}
	if h.Price != nil {
		p, perr := toAPIStablePriceDoc(*h.Price)
		if perr != nil {
			return api.StableReserveHolding{}, perr
		}
		out.Price = &p
	}
	return out, nil
}

func toAPIStableReserveHoldings(hs []stableasset.ReserveHolding, labels map[assets.AssetID]StableAssetLabel) ([]api.StableReserveHolding, error) {
	out := make([]api.StableReserveHolding, 0, len(hs))
	for _, h := range hs {
		item, err := toAPIStableReserveHolding(h, labels)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

func toAPIStableReserve(asset StableAssetView, rl stableasset.ReserveLedger, labels map[assets.AssetID]StableAssetLabel) (api.StableReserve, error) {
	peg, err := toAPIStablePrice(asset.Asset.Peg)
	if err != nil {
		return api.StableReserve{}, err
	}
	out := api.StableReserve{
		ValueDomain: api.SIMULATED, StableAssetId: toUUID(rl.StableAssetID), Symbol: asset.Label.Symbol,
		Decimals: int(asset.Asset.Decimals), AsOf: rl.AsOf.UTC(), Peg: peg, IssuedSupply: rl.IssuedSupply.String(),
		PendingMint: rl.PendingMint.String(), PendingBurn: rl.PendingBurn.String(), SupplyValue: rl.SupplyValue.String(),
		EligibleReserve: rl.EligibleReserve.String(), CoverageBps: int64(rl.CoverageBPS), Covered: rl.Covered(),
		UnverifiedReserveAssetIds: make([]api.UUID, 0, len(rl.Unverified)),
	}
	if out.CashEquivalentReserve, err = toAPIStableReserveHoldings(rl.CashEquivalentReserve, labels); err != nil {
		return api.StableReserve{}, err
	}
	if out.TokenizedReserve, err = toAPIStableReserveHoldings(rl.TokenizedReserve, labels); err != nil {
		return api.StableReserve{}, err
	}
	if out.Receivable, err = toAPIStableReserveHoldings(rl.Receivable, labels); err != nil {
		return api.StableReserve{}, err
	}
	if out.PendingRedemption, err = toAPIStableReserveHoldings(rl.PendingRedemption, labels); err != nil {
		return api.StableReserve{}, err
	}
	for _, aid := range rl.Unverified {
		out.UnverifiedReserveAssetIds = append(out.UnverifiedReserveAssetIds, toUUID(aid))
	}
	return out, nil
}

func toAPIStableSnapshotPrice(d stableasset.PriceDoc) api.StableSnapshotPrice {
	return api.StableSnapshotPrice{Mantissa: d.Mantissa, Scale: int(d.Scale), Quote: d.Quote, Source: d.Source, At: d.At.UTC()}
}

// toAPIStableSnapshot renders a snapshot exactly as it was signed: its
// canonical bytes, its hash and signature in hex, and its body field for
// field. verified is what the chain's verification says of it.
func toAPIStableSnapshot(sn stableasset.SignedSnapshot, verified bool) (api.StableSnapshot, error) {
	seq, err := strconv.ParseInt(sn.Snapshot.Seq, 10, 64)
	if err != nil {
		return api.StableSnapshot{}, errs.Wrap(err, errs.CodeInternal, "a stored snapshot's sequence number is not an integer")
	}
	b := sn.Snapshot
	body := api.StableSnapshotBody{
		Version: b.Version, Disclaimer: b.Disclaimer, StableAssetId: b.StableAssetID, AssetId: b.AssetID, Chain: b.Chain, Mint: b.Mint,
		Seq: b.Seq, TakenAt: b.TakenAt.UTC(), PrevHash: b.PrevHash, LedgerSupply: b.LedgerSupply, ChainSupply: b.ChainSupply,
		PendingMint: b.PendingMint, PendingBurn: b.PendingBurn, Peg: toAPIStableSnapshotPrice(b.Peg),
		Reserves: make([]api.StableSnapshotReserve, 0, len(b.Reserves)), Composition: make([]api.StableSnapshotComposition, 0, len(b.Composition)),
		SupplyValue: b.SupplyValue, EligibleReserve: b.EligibleReserve, CoverageBps: b.CoverageBPS,
	}
	for _, r := range b.Reserves {
		item := api.StableSnapshotReserve{
			AssetId: r.AssetID, Kind: r.Kind, Balance: r.Balance, Decimals: r.Decimals, FactorBps: r.FactorBPS, Eligible: r.Eligible,
		}
		if r.Price != nil {
			p := toAPIStableSnapshotPrice(*r.Price)
			item.Price = &p
		}
		body.Reserves = append(body.Reserves, item)
	}
	for _, c := range b.Composition {
		body.Composition = append(body.Composition, api.StableSnapshotComposition{Kind: c.Kind, Eligible: c.Eligible, ShareBps: c.ShareBPS})
	}
	return api.StableSnapshot{
		Seq: seq, TakenAt: b.TakenAt.UTC(), Hash: hex.EncodeToString(sn.Hash[:]), PrevHash: b.PrevHash,
		Signature: hex.EncodeToString(sn.Signature), SignerPublicKey: hex.EncodeToString(sn.PublicKey), EvidenceRef: sn.EvidenceRef,
		Verified: verified, CanonicalBody: string(sn.Body), Body: body,
	}, nil
}

func toAPIStableSnapshotVerification(v StableSnapshotVerification) api.StableSnapshotVerification {
	out := api.StableSnapshotVerification{
		Verified: v.Verified(), Checked: v.Checked, PublicKey: hex.EncodeToString(v.PublicKey), Disclaimer: stableasset.SnapshotDisclaimer,
	}
	if !v.Verified() {
		index := v.FailedIndex
		reason := api.StableSnapshotFailure(v.Reason)
		out.FailedIndex, out.Reason = &index, &reason
	}
	return out
}

func toAPIStableReconciliation(r StableRequestReconciliation) api.StableReconciliation {
	out := api.StableReconciliation{EffectStatus: api.StableEffectStatus(r.EffectStatus), UnknownSince: timePtr(r.UnknownSince)}
	switch {
	case r.EffectStatus == econ.EffectUnknown:
		out.State = api.StableReconciliationAwaiting
	case r.UnknownSince != nil:
		out.State = api.StableReconciliationResolved
		out.ResolvedAt = timePtr(r.ResolvedAt)
	default:
		out.State = api.StableReconciliationNotRequired
	}
	return out
}

func stableFinalSlot(slot *int64) *int64 {
	if slot == nil {
		return nil
	}
	v := *slot
	return &v
}

func toAPIStableIssuance(v StableIssuanceView) (api.StableIssuance, error) {
	r := v.Record
	price, err := toAPIStablePriceDoc(r.Plan.ReservePrice)
	if err != nil {
		return api.StableIssuance{}, err
	}
	out := api.StableIssuance{
		ValueDomain: api.SIMULATED, Id: toUUID(r.ID), StableAssetId: toUUID(r.StableAssetID), Symbol: v.Stable.Symbol,
		Decimals: int(v.Stable.Decimals), AccountId: toUUID(r.AccountID), IntentId: toUUID(r.IntentID), EffectId: toUUID(r.EffectID),
		Mode: api.StableRequestMode(r.Mode), Quantity: r.Quantity.String(), ReserveAssetId: toUUID(r.ReserveAssetID),
		ReserveSymbol: v.Reserve.Symbol, ReserveDecimals: int(v.Reserve.Decimals), ReserveQuantity: r.ReserveQuantity.String(),
		ReservePrice: price, Recipient: r.Recipient.String(), Chain: string(r.Chain), TxId: string(r.TxID),
		PlanHash: hex.EncodeToString(r.PlanHash[:]), Status: api.StableIssuanceStatus(r.Status), FailureCode: strPtr(r.FailureCode),
		Reconciliation: toAPIStableReconciliation(v.Reconciliation), FinalSlot: stableFinalSlot(r.FinalSlot),
		CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC(),
	}
	if r.JournalTransactionID != nil {
		out.JournalTransactionId = uuidPtr(*r.JournalTransactionID)
	}
	return out, nil
}

func toAPIStableRedemption(v StableRedemptionView) (api.StableRedemption, error) {
	r := v.Record
	mode := api.StableRedemptionMode(r.Plan.RedemptionMode)
	if !mode.Valid() {
		return api.StableRedemption{}, errs.New(errs.CodeInternal, "a burn plan names no redemption mode").WithField("request_id", r.ID.String())
	}
	out := api.StableRedemption{
		ValueDomain: api.SIMULATED, Id: toUUID(r.ID), StableAssetId: toUUID(r.StableAssetID), Symbol: v.Stable.Symbol,
		Decimals: int(v.Stable.Decimals), AccountId: toUUID(r.AccountID), IntentId: toUUID(r.IntentID), EffectId: toUUID(r.EffectID),
		Mode: api.StableRequestMode(r.Mode), Quantity: r.Quantity.String(), PayoutAssetId: toUUID(r.PayoutAssetID),
		PayoutSymbol: v.Payout.Symbol, PayoutDecimals: int(v.Payout.Decimals), PayoutQuantity: r.PayoutQuantity.String(),
		RedemptionMode: mode, Holder: r.Holder.String(), Chain: string(r.Chain), TxId: string(r.TxID),
		PlanHash: hex.EncodeToString(r.PlanHash[:]), Status: api.StableRedemptionStatus(r.Status), FailureCode: strPtr(r.FailureCode),
		Reconciliation: toAPIStableReconciliation(v.Reconciliation), FinalSlot: stableFinalSlot(r.FinalSlot),
		CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC(),
	}
	if r.JournalTransactionID != nil {
		out.JournalTransactionId = uuidPtr(*r.JournalTransactionID)
	}
	return out, nil
}

func toAPIStableHoldings(v StableHoldingsView) api.StableHoldings {
	out := api.StableHoldings{
		ValueDomain: api.SIMULATED, AccountId: toUUID(v.AccountID), HolderAddress: v.HolderAddress, AsOf: v.AsOf.UTC(),
		Items: make([]api.StableHolding, 0, len(v.Items)), PaymentAssets: make([]api.StablePaymentAsset, 0, len(v.Payment)),
	}
	for _, h := range v.Items {
		out.Items = append(out.Items, api.StableHolding{
			StableAssetId: toUUID(h.Asset.ID), AssetId: toUUID(h.Asset.AssetID), Symbol: h.Label.Symbol, Decimals: int(h.Label.Decimals),
			Chain: string(h.Asset.Chain), Status: api.StableAssetStatus(h.Asset.Status), Wallet: h.Availability.WalletBalance.String(),
			Reserved: h.Availability.Reserved.String(), Held: h.Availability.Held.String(), Available: h.Availability.Available.String(),
			ChainBalance: h.ChainBalance.String(),
		})
	}
	for _, p := range v.Payment {
		out.PaymentAssets = append(out.PaymentAssets, api.StablePaymentAsset{
			AssetId: toUUID(p.AssetID), Symbol: p.Label.Symbol, Decimals: int(p.Label.Decimals),
			Wallet: p.Availability.WalletBalance.String(), Reserved: p.Availability.Reserved.String(),
			Held: p.Availability.Held.String(), Available: p.Availability.Available.String(),
		})
	}
	return out
}

func toAPIStableSupply(r stableasset.SupplyReconciliation) api.StableSupplyReconciliation {
	out := api.StableSupplyReconciliation{
		ValueDomain: api.SIMULATED, StableAssetId: toUUID(r.StableAssetID), LedgerOutstanding: r.LedgerOutstanding.String(),
		CustomerWalletSum: r.CustomerWalletSum.String(), HeldElsewhere: r.HeldElsewhere.String(), ChainSupply: r.ChainSupply.String(),
		LandedUnpostedMints: r.LandedUnpostedMints.String(), LandedUnpostedBurns: r.LandedUnpostedBurns.String(),
		ExpectedChainSupply: r.ExpectedChainSupply.String(), SupplyValue: r.SupplyValue.String(),
		EligibleReserve: r.EligibleReserve.String(), CoverageBps: int64(r.CoverageBPS), SupplyMatches: r.SupplyMatches,
		WalletsMatch: r.WalletsMatch, Covered: r.Covered, Matched: r.Matched(), BreakerTripped: r.BreakerTripped,
	}
	out.RecordId = parseUUIDText(r.RecordID)
	return out
}

func toAPIStableSettleReport(r stableasset.SettleReport) api.StableSettleReport {
	return api.StableSettleReport{
		ValueDomain: api.SIMULATED, Examined: r.Examined, Issued: r.Issued, Redeemed: r.Redeemed, Queued: r.Queued,
		Failed: r.Failed, Waiting: r.Waiting,
	}
}

func toAPIStableMonitor(v StableMonitorView) (api.StableMonitorReport, error) {
	r := v.Report
	reserve, err := toAPIStableReserve(v.Asset, r.Reserve, v.Labels)
	if err != nil {
		return api.StableMonitorReport{}, err
	}
	out := api.StableMonitorReport{
		ValueDomain: api.SIMULATED, StableAssetId: toUUID(r.StableAssetID), ObservedAt: r.ObservedAt.UTC(), PriceStale: r.PriceStale,
		DeviationBps: int64(r.DeviationBPS), Alerts: make([]api.StableAlertKind, 0, len(r.Alerts)), BreakerTripped: r.BreakerTripped,
		Status: api.StableAssetStatus(r.Status), Reserve: reserve, Supply: toAPIStableSupply(r.Supply),
	}
	for _, k := range r.Alerts {
		out.Alerts = append(out.Alerts, api.StableAlertKind(k))
	}
	if r.MarketPrice != nil {
		p, perr := toAPIStablePriceDoc(*r.MarketPrice)
		if perr != nil {
			return api.StableMonitorReport{}, perr
		}
		out.MarketPrice = &p
	}
	return out, nil
}
