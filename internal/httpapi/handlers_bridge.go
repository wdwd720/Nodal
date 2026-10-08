package httpapi

import (
	"context"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/bridge"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/security"
)

// The cross-chain capital routes (internal/bridge) and the simulated chain
// host's routes (internal/devnethost). Handlers translate shapes only: the
// bridge service and the host decide every figure, status and refusal, and
// their error codes reach the wire unchanged. A leg is the account owner's
// own command (accountScopeWrite) under sandbox:bridge; operator commands
// need sandbox:operate, and those that open, pause or resume a route, drive
// a leg, produce blocks or inject a fault a recent step-up as well (authz.go).

// bridgeNotWired is the answer of every route where no sandbox chain is
// configured.
func bridgeNotWired() *errs.Error {
	return errNotWired("the bridge between the simulated chains (no sandbox chain is configured: CP_SANDBOX_DEVNET_NETWORK)")
}

// --- reads -------------------------------------------------------------------------

// GetAdvancedBridgeRoutes lists the routes.
func (s *Server) GetAdvancedBridgeRoutes(ctx context.Context, request api.GetAdvancedBridgeRoutesRequestObject) (api.GetAdvancedBridgeRoutesResponseObject, error) {
	if s.opts.Ports.Bridge == nil {
		return nil, bridgeNotWired()
	}
	views, err := s.opts.Ports.Bridge.Routes(ctx, pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	out := api.BridgeRouteList{ValueDomain: api.SIMULATED, Items: make([]api.BridgeRoute, 0, len(views))}
	for _, v := range views {
		item, cerr := toAPIBridgeRoute(v)
		if cerr != nil {
			return nil, cerr
		}
		out.Items = append(out.Items, item)
	}
	return api.GetAdvancedBridgeRoutes200JSONResponse(out), nil
}

// GetAdvancedBridgeRoutesBridgeRouteId returns one route.
func (s *Server) GetAdvancedBridgeRoutesBridgeRouteId(ctx context.Context, request api.GetAdvancedBridgeRoutesBridgeRouteIdRequestObject) (api.GetAdvancedBridgeRoutesBridgeRouteIdResponseObject, error) {
	if s.opts.Ports.Bridge == nil {
		return nil, bridgeNotWired()
	}
	rid, err := bridgeRouteIDParam(request.BridgeRouteId.String())
	if err != nil {
		return nil, err
	}
	v, err := s.opts.Ports.Bridge.Route(ctx, rid)
	if err != nil {
		return nil, err
	}
	out, err := toAPIBridgeRoute(v)
	if err != nil {
		return nil, err
	}
	return api.GetAdvancedBridgeRoutesBridgeRouteId200JSONResponse(out), nil
}

// GetAdvancedBridgeLegs pages an account's legs.
func (s *Server) GetAdvancedBridgeLegs(ctx context.Context, request api.GetAdvancedBridgeLegsRequestObject) (api.GetAdvancedBridgeLegsResponseObject, error) {
	if s.opts.Ports.Bridge == nil {
		return nil, bridgeNotWired()
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	cursor := ""
	if request.Params.Cursor != nil {
		cursor = *request.Params.Cursor
	}
	page, err := s.opts.Ports.Bridge.Legs(ctx, account, cursor, pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	out := api.BridgeLegPage{ValueDomain: api.SIMULATED, Items: make([]api.BridgeLeg, 0, len(page.Items)), NextCursor: nextCursor(page.NextCursor)}
	for _, v := range page.Items {
		out.Items = append(out.Items, toAPIBridgeLeg(v))
	}
	return api.GetAdvancedBridgeLegs200JSONResponse(out), nil
}

// GetAdvancedBridgeLegsBridgeLegId returns one leg to its account's owner (or
// an operator who may read any account). A leg of another account answers
// exactly as one that does not exist.
func (s *Server) GetAdvancedBridgeLegsBridgeLegId(ctx context.Context, request api.GetAdvancedBridgeLegsBridgeLegIdRequestObject) (api.GetAdvancedBridgeLegsBridgeLegIdResponseObject, error) {
	if s.opts.Ports.Bridge == nil {
		return nil, bridgeNotWired()
	}
	lid, err := bridgeLegIDParam(request.BridgeLegId.String())
	if err != nil {
		return nil, err
	}
	notFound := errs.New(errs.CodeNotFound, "no such bridge leg").WithField("leg_id", lid.String())
	v, err := s.opts.Ports.Bridge.Leg(ctx, lid)
	if errs.HasCode(err, errs.CodeNotFound) {
		return nil, notFound
	}
	if err != nil {
		return nil, err
	}
	if err := security.RequireAccount(ctx, v.Leg.AccountID.String()); err != nil {
		return nil, notFound
	}
	out := api.BridgeLegDetail{
		ValueDomain: api.SIMULATED, Leg: toAPIBridgeLeg(v.BridgeLegView),
		Attempts: make([]api.BridgeLegAttempt, 0, len(v.Attempts)), Transitions: make([]api.BridgeLegTransition, 0, len(v.Transitions)),
	}
	if v.Lock != nil {
		lock := toAPIBridgeChainTx(*v.Lock)
		out.Lock = &lock
	}
	for _, at := range v.Attempts {
		if at.BlockhashHeight > uint64(bridgeMaxInt64) {
			return nil, errs.New(errs.CodeInternal, "a release attempt's blockhash height is out of range")
		}
		out.Attempts = append(out.Attempts, api.BridgeLegAttempt{
			Number: at.Number, Kind: api.BridgeAttemptKind(at.Kind), Chain: string(at.Chain), TxId: string(at.TxID),
			BlockhashHeight: int64(at.BlockhashHeight), CreatedAt: at.CreatedAt.UTC(), OnChain: toAPIBridgeChainTx(at.OnChain),
		})
	}
	for _, t := range v.Transitions {
		out.Transitions = append(out.Transitions, api.BridgeLegTransition{
			FromStatus: t.From, ToStatus: api.BridgeLegStatus(t.To), ActorType: t.ActorType, ActorId: t.ActorID, Reason: t.Reason,
			OccurredAt: t.OccurredAt.UTC(),
		})
	}
	return api.GetAdvancedBridgeLegsBridgeLegId200JSONResponse(out), nil
}

// GetAdvancedBridgeHoldings returns an account's simulated holdings by chain,
// its value in flight between chains and the haircut that value counts at.
func (s *Server) GetAdvancedBridgeHoldings(ctx context.Context, request api.GetAdvancedBridgeHoldingsRequestObject) (api.GetAdvancedBridgeHoldingsResponseObject, error) {
	if s.opts.Ports.Bridge == nil {
		return nil, bridgeNotWired()
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	v, err := s.opts.Ports.Bridge.Holdings(ctx, account)
	if err != nil {
		return nil, err
	}
	out, err := toAPIBridgeHoldings(v)
	if err != nil {
		return nil, err
	}
	return api.GetAdvancedBridgeHoldings200JSONResponse(out), nil
}

// GetAdvancedBridgeChains reads both simulated chains for display.
func (s *Server) GetAdvancedBridgeChains(ctx context.Context, _ api.GetAdvancedBridgeChainsRequestObject) (api.GetAdvancedBridgeChainsResponseObject, error) {
	if s.opts.Ports.Bridge == nil {
		return nil, bridgeNotWired()
	}
	v, err := s.opts.Ports.Bridge.Chains(ctx)
	if err != nil {
		return nil, err
	}
	out := api.BridgeChains{
		ValueDomain: api.SIMULATED, Network: v.Network, FaultInjectionAllowed: v.FaultInjectionAllowed,
		Chains: make([]api.BridgeChain, 0, len(v.Chains)),
	}
	for _, c := range v.Chains {
		item, cerr := toAPIBridgeChain(c)
		if cerr != nil {
			return nil, cerr
		}
		out.Chains = append(out.Chains, item)
	}
	return api.GetAdvancedBridgeChains200JSONResponse(out), nil
}

// --- the customer's command ------------------------------------------------------------

// PostAdvancedBridgeLegs moves units of a route's source asset across for
// the caller's own account. 200 when the leg is final; 202 when it is
// recorded and not final, including a lock whose outcome is unknown.
func (s *Server) PostAdvancedBridgeLegs(ctx context.Context, request api.PostAdvancedBridgeLegsRequestObject) (api.PostAdvancedBridgeLegsResponseObject, error) {
	if s.opts.Ports.Bridge == nil {
		return nil, bridgeNotWired()
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	account, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	var rid bridge.RouteID
	if err := rid.UnmarshalText([]byte(request.Body.RouteId.String())); err != nil || rid.IsZero() {
		return nil, validationError("route_id", "route_id must be a canonical UUID")
	}
	qty, err := money.ParseQuantity(request.Body.Quantity)
	if err != nil {
		return nil, validationError("quantity", "quantity must be an integer string of the source asset's base units")
	}
	if !request.Body.Mode.Valid() {
		return nil, validationError("mode", "mode must be SANDBOX or DEVNET")
	}
	actor, err := stableActor(ctx)
	if err != nil {
		return nil, err
	}
	cmd := BridgeLegCommand{
		RouteID: rid, AccountID: account, Quantity: qty, Mode: gates.Mode(request.Body.Mode),
		IdempotencyKey: request.Params.IdempotencyKey, Actor: actor, CorrelationID: observability.CorrelationID(ctx),
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.BridgeLegResult, commandMeta, error) {
			o, berr := s.opts.Ports.Bridge.Bridge(ctx, cmd)
			if berr != nil {
				return api.BridgeLegResult{}, commandMeta{}, berr
			}
			return toAPIBridgeLegResult(o), commandMeta{Status: stableCommandStatus(o.Settled), ResourceType: "bridge_leg", ResourceID: o.View.Leg.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	if res.Meta.Status == http.StatusAccepted {
		return api.PostAdvancedBridgeLegs202JSONResponse(res.Value), nil
	}
	return api.PostAdvancedBridgeLegs200JSONResponse(res.Value), nil
}

// --- the operator's commands -------------------------------------------------------------

// PostAdvancedBridgeRoutes opens a route.
func (s *Server) PostAdvancedBridgeRoutes(ctx context.Context, request api.PostAdvancedBridgeRoutesRequestObject) (api.PostAdvancedBridgeRoutesResponseObject, error) {
	if s.opts.Ports.Bridge == nil {
		return nil, bridgeNotWired()
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	b := *request.Body
	src, err := assets.ParseAssetID(b.SourceAssetId.String())
	if err != nil || src.IsZero() {
		return nil, validationError("source_asset_id", "source_asset_id must be a canonical UUID")
	}
	if !b.DestinationChain.Valid() {
		return nil, validationError("destination_chain", "destination_chain must be sbx-solana or sbx-ethereum")
	}
	price, err := money.PriceFromDecimalString(b.UnitPrice, "USD", "ROUTE_UNIT_PRICE", s.clk.Now())
	if err != nil {
		return nil, validationError("unit_price", "unit_price must be an exact decimal")
	}
	actor, err := stableActor(ctx)
	if err != nil {
		return nil, err
	}
	def := bridge.RouteDefinition{
		Name: b.Name, SourceAssetID: src, DestinationChain: devnet.ChainID(b.DestinationChain), DestinationSymbol: b.DestinationSymbol,
		UnitPrice: price, MaxReleaseAttempts: b.MaxReleaseAttempts,
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.BridgeRoute, commandMeta, error) {
			v, cerr := s.opts.Ports.Bridge.CreateRoute(ctx, def, actor)
			if cerr != nil {
				return api.BridgeRoute{}, commandMeta{}, cerr
			}
			out, rerr := toAPIBridgeRoute(v)
			if rerr != nil {
				return api.BridgeRoute{}, commandMeta{}, rerr
			}
			return out, commandMeta{Status: http.StatusOK, ResourceType: "bridge_route", ResourceID: v.Route.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedBridgeRoutes200JSONResponse(res.Value), nil
}

// PostAdvancedBridgeRoutesBridgeRouteIdPause pauses a route.
func (s *Server) PostAdvancedBridgeRoutesBridgeRouteIdPause(ctx context.Context, request api.PostAdvancedBridgeRoutesBridgeRouteIdPauseRequestObject) (api.PostAdvancedBridgeRoutesBridgeRouteIdPauseResponseObject, error) {
	out, err := s.bridgeSetRouteStatus(ctx, request.BridgeRouteId.String(), request.Body, request.Params.IdempotencyKey, bridge.RoutePaused)
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedBridgeRoutesBridgeRouteIdPause200JSONResponse(out), nil
}

// PostAdvancedBridgeRoutesBridgeRouteIdResume resumes a paused route; the
// service accepts it from an operator only.
func (s *Server) PostAdvancedBridgeRoutesBridgeRouteIdResume(ctx context.Context, request api.PostAdvancedBridgeRoutesBridgeRouteIdResumeRequestObject) (api.PostAdvancedBridgeRoutesBridgeRouteIdResumeResponseObject, error) {
	out, err := s.bridgeSetRouteStatus(ctx, request.BridgeRouteId.String(), request.Body, request.Params.IdempotencyKey, bridge.RouteActive)
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedBridgeRoutesBridgeRouteIdResume200JSONResponse(out), nil
}

func (s *Server) bridgeSetRouteStatus(ctx context.Context, rawID string, body *api.BridgeReasonRequest, key string, to bridge.RouteStatus) (api.BridgeRoute, error) {
	if s.opts.Ports.Bridge == nil {
		return api.BridgeRoute{}, bridgeNotWired()
	}
	rid, err := bridgeRouteIDParam(rawID)
	if err != nil {
		return api.BridgeRoute{}, err
	}
	if body == nil {
		return api.BridgeRoute{}, validationError("body", "a request body is required")
	}
	actor, err := stableActor(ctx)
	if err != nil {
		return api.BridgeRoute{}, err
	}
	reason := body.Reason
	res, err := runCommand(ctx, s, key, func(ctx context.Context) (api.BridgeRoute, commandMeta, error) {
		v, serr := s.opts.Ports.Bridge.SetRouteStatus(ctx, rid, to, actor, reason)
		if serr != nil {
			return api.BridgeRoute{}, commandMeta{}, serr
		}
		out, cerr := toAPIBridgeRoute(v)
		if cerr != nil {
			return api.BridgeRoute{}, commandMeta{}, cerr
		}
		return out, commandMeta{Status: http.StatusOK, ResourceType: "bridge_route", ResourceID: rid.String()}, nil
	})
	if err != nil {
		return api.BridgeRoute{}, err
	}
	return res.Value, nil
}

// PostAdvancedBridgeRoutesBridgeRouteIdReconciliations reconciles a route's
// books with both chains. It moves no value; a mismatch pauses the route.
func (s *Server) PostAdvancedBridgeRoutesBridgeRouteIdReconciliations(ctx context.Context, request api.PostAdvancedBridgeRoutesBridgeRouteIdReconciliationsRequestObject) (api.PostAdvancedBridgeRoutesBridgeRouteIdReconciliationsResponseObject, error) {
	if s.opts.Ports.Bridge == nil {
		return nil, bridgeNotWired()
	}
	rid, err := bridgeRouteIDParam(request.BridgeRouteId.String())
	if err != nil {
		return nil, err
	}
	actor, err := stableActor(ctx)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.BridgeRouteReconciliation, commandMeta, error) {
			r, rerr := s.opts.Ports.Bridge.ReconcileRoute(ctx, rid, actor)
			if rerr != nil {
				return api.BridgeRouteReconciliation{}, commandMeta{}, rerr
			}
			meta := commandMeta{Status: http.StatusOK, ResourceType: "bridge_route", ResourceID: rid.String()}
			if r.RecordID != "" {
				meta.ResourceType, meta.ResourceID = "reconciliation_record", r.RecordID
			}
			return toAPIBridgeRouteReconciliation(r), meta, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedBridgeRoutesBridgeRouteIdReconciliations200JSONResponse(res.Value), nil
}

// PostAdvancedBridgeLegsBridgeLegIdReconcile drives a leg from both chains by
// transaction id and reconciles it, never resubmitting anything.
func (s *Server) PostAdvancedBridgeLegsBridgeLegIdReconcile(ctx context.Context, request api.PostAdvancedBridgeLegsBridgeLegIdReconcileRequestObject) (api.PostAdvancedBridgeLegsBridgeLegIdReconcileResponseObject, error) {
	if s.opts.Ports.Bridge == nil {
		return nil, bridgeNotWired()
	}
	lid, err := bridgeLegIDParam(request.BridgeLegId.String())
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.BridgeLegResult, commandMeta, error) {
			o, rerr := s.opts.Ports.Bridge.ReconcileLeg(ctx, lid)
			if rerr != nil {
				return api.BridgeLegResult{}, commandMeta{}, rerr
			}
			return toAPIBridgeLegResult(o), commandMeta{Status: stableCommandStatus(o.Settled), ResourceType: "bridge_leg", ResourceID: lid.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	if res.Meta.Status == http.StatusAccepted {
		return api.PostAdvancedBridgeLegsBridgeLegIdReconcile202JSONResponse(res.Value), nil
	}
	return api.PostAdvancedBridgeLegsBridgeLegIdReconcile200JSONResponse(res.Value), nil
}

// --- a leg in manual review ------------------------------------------------------

// bridgeReviewCommand reads what every way out of a leg's MANUAL_REVIEW
// needs: the leg, the operator and a reason, which the route requires even
// where the service would not.
func (s *Server) bridgeReviewCommand(ctx context.Context, rawID string, body *api.BridgeReasonRequest) (bridge.LegID, econ.Actor, string, error) {
	lid, err := bridgeLegIDParam(rawID)
	if err != nil {
		return bridge.LegID{}, econ.Actor{}, "", err
	}
	if body == nil {
		return bridge.LegID{}, econ.Actor{}, "", validationError("body", "a request body is required")
	}
	if strings.TrimSpace(body.Reason) == "" || len(body.Reason) > 1000 {
		return bridge.LegID{}, econ.Actor{}, "", validationError("reason", "a reason of 1..1000 bytes is required")
	}
	actor, err := stableActor(ctx)
	if err != nil {
		return bridge.LegID{}, econ.Actor{}, "", err
	}
	return lid, actor, body.Reason, nil
}

// PostAdvancedBridgeLegsBridgeLegIdResumeRelay has a leg in manual review
// relayed again, with a fresh round of release attempts.
func (s *Server) PostAdvancedBridgeLegsBridgeLegIdResumeRelay(ctx context.Context, request api.PostAdvancedBridgeLegsBridgeLegIdResumeRelayRequestObject) (api.PostAdvancedBridgeLegsBridgeLegIdResumeRelayResponseObject, error) {
	if s.opts.Ports.Bridge == nil {
		return nil, bridgeNotWired()
	}
	lid, actor, reason, err := s.bridgeReviewCommand(ctx, request.BridgeLegId.String(), request.Body)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey, func(ctx context.Context) (api.BridgeLeg, commandMeta, error) {
		v, rerr := s.opts.Ports.Bridge.ResumeRelay(ctx, lid, actor, reason)
		if rerr != nil {
			return api.BridgeLeg{}, commandMeta{}, rerr
		}
		return toAPIBridgeLeg(v), commandMeta{Status: http.StatusOK, ResourceType: "bridge_leg", ResourceID: lid.String()}, nil
	})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedBridgeLegsBridgeLegIdResumeRelay200JSONResponse(res.Value), nil
}

// PostAdvancedBridgeLegsBridgeLegIdResolveReview re-checks a released leg in
// manual review against both chains and resolves it as reconciled when they
// agree.
func (s *Server) PostAdvancedBridgeLegsBridgeLegIdResolveReview(ctx context.Context, request api.PostAdvancedBridgeLegsBridgeLegIdResolveReviewRequestObject) (api.PostAdvancedBridgeLegsBridgeLegIdResolveReviewResponseObject, error) {
	if s.opts.Ports.Bridge == nil {
		return nil, bridgeNotWired()
	}
	lid, actor, reason, err := s.bridgeReviewCommand(ctx, request.BridgeLegId.String(), request.Body)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey, func(ctx context.Context) (api.BridgeLeg, commandMeta, error) {
		v, rerr := s.opts.Ports.Bridge.ResolveReview(ctx, lid, actor, reason)
		if rerr != nil {
			return api.BridgeLeg{}, commandMeta{}, rerr
		}
		return toAPIBridgeLeg(v), commandMeta{Status: http.StatusOK, ResourceType: "bridge_leg", ResourceID: lid.String()}, nil
	})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedBridgeLegsBridgeLegIdResolveReview200JSONResponse(res.Value), nil
}

// PostAdvancedBridgeLegsBridgeLegIdRefund refunds a leg in manual review on
// its source chain; 202 while the refund is not final yet.
func (s *Server) PostAdvancedBridgeLegsBridgeLegIdRefund(ctx context.Context, request api.PostAdvancedBridgeLegsBridgeLegIdRefundRequestObject) (api.PostAdvancedBridgeLegsBridgeLegIdRefundResponseObject, error) {
	if s.opts.Ports.Bridge == nil {
		return nil, bridgeNotWired()
	}
	lid, actor, reason, err := s.bridgeReviewCommand(ctx, request.BridgeLegId.String(), request.Body)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey, func(ctx context.Context) (api.BridgeLegResult, commandMeta, error) {
		o, rerr := s.opts.Ports.Bridge.Refund(ctx, lid, actor, reason)
		if rerr != nil {
			return api.BridgeLegResult{}, commandMeta{}, rerr
		}
		return toAPIBridgeLegResult(o), commandMeta{Status: stableCommandStatus(o.Settled), ResourceType: "bridge_leg", ResourceID: lid.String()}, nil
	})
	if err != nil {
		return nil, err
	}
	if res.Meta.Status == http.StatusAccepted {
		return api.PostAdvancedBridgeLegsBridgeLegIdRefund202JSONResponse(res.Value), nil
	}
	return api.PostAdvancedBridgeLegsBridgeLegIdRefund200JSONResponse(res.Value), nil
}

// PostAdvancedBridgeChainsBridgeChainIdBlocks produces blocks on a chain.
func (s *Server) PostAdvancedBridgeChainsBridgeChainIdBlocks(ctx context.Context, request api.PostAdvancedBridgeChainsBridgeChainIdBlocksRequestObject) (api.PostAdvancedBridgeChainsBridgeChainIdBlocksResponseObject, error) {
	if s.opts.Ports.Bridge == nil {
		return nil, bridgeNotWired()
	}
	chain, err := bridgeChainParam(request.BridgeChainId)
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	if request.Body.Count < 1 || request.Body.Count > devnethost.MaxBlocksPerCall {
		return nil, validationError("count", "count must be between 1 and 1000")
	}
	n := uint64(request.Body.Count)
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.BridgeBlocksResult, commandMeta, error) {
			v, perr := s.opts.Ports.Bridge.ProduceBlocks(ctx, chain, n)
			if perr != nil {
				return api.BridgeBlocksResult{}, commandMeta{}, perr
			}
			c, cerr := toAPIBridgeChain(v)
			if cerr != nil {
				return api.BridgeBlocksResult{}, commandMeta{}, cerr
			}
			out := api.BridgeBlocksResult{ValueDomain: api.SIMULATED, Produced: request.Body.Count, Chain: c}
			return out, commandMeta{Status: http.StatusOK, ResourceType: "devnet_chain", ResourceID: string(chain)}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedBridgeChainsBridgeChainIdBlocks200JSONResponse(res.Value), nil
}

// PostAdvancedBridgeChainsBridgeChainIdFaults arms a submission fault. Fault
// injection exists only where the environment allows fake providers (LOCAL,
// TEST and DEV); everywhere else it is refused here, before the host -- which
// refuses it as well -- is reached.
func (s *Server) PostAdvancedBridgeChainsBridgeChainIdFaults(ctx context.Context, request api.PostAdvancedBridgeChainsBridgeChainIdFaultsRequestObject) (api.PostAdvancedBridgeChainsBridgeChainIdFaultsResponseObject, error) {
	if s.opts.Ports.Bridge == nil {
		return nil, bridgeNotWired()
	}
	if !s.opts.Env.AllowsFakeProviders() {
		return nil, errs.New(errs.CodeForbidden, "fault injection is available only in LOCAL, TEST and DEV").
			WithField("reason", devnethost.ReasonFaultInjectionForbidden)
	}
	chain, err := bridgeChainParam(request.BridgeChainId)
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	fault, err := bridgeFaultOf(*request.Body)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.BridgeFaultResult, commandMeta, error) {
			v, ferr := s.opts.Ports.Bridge.InjectFault(ctx, chain, fault)
			if ferr != nil {
				return api.BridgeFaultResult{}, commandMeta{}, ferr
			}
			c, cerr := toAPIBridgeChain(v)
			if cerr != nil {
				return api.BridgeFaultResult{}, commandMeta{}, cerr
			}
			out := api.BridgeFaultResult{ValueDomain: api.SIMULATED, Kind: request.Body.Kind, Count: request.Body.Count, Chain: c}
			return out, commandMeta{Status: http.StatusOK, ResourceType: "devnet_chain", ResourceID: string(chain)}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedBridgeChainsBridgeChainIdFaults200JSONResponse(res.Value), nil
}

// --- request helpers -------------------------------------------------------------------------

// bridgeMaxInt64 bounds the chain figures rendered as JSON integers.
const bridgeMaxInt64 = 1<<63 - 1

func bridgeRouteIDParam(raw string) (bridge.RouteID, error) {
	var rid bridge.RouteID
	if err := rid.UnmarshalText([]byte(raw)); err != nil || rid.IsZero() {
		return bridge.RouteID{}, validationError("bridgeRouteId", "bridgeRouteId must be a canonical UUID")
	}
	return rid, nil
}

func bridgeLegIDParam(raw string) (bridge.LegID, error) {
	var lid bridge.LegID
	if err := lid.UnmarshalText([]byte(raw)); err != nil || lid.IsZero() {
		return bridge.LegID{}, validationError("bridgeLegId", "bridgeLegId must be a canonical UUID")
	}
	return lid, nil
}

func bridgeChainParam(c api.BridgeChainId) (devnet.ChainID, error) {
	if !c.Valid() {
		return "", validationError("bridgeChainId", "bridgeChainId must be sbx-solana or sbx-ethereum")
	}
	return devnet.ChainID(c), nil
}

// bridgeFaultOf translates a fault request: DROP_NEXT names how many
// submissions to drop, DELAY_INCLUSION how many blocks to hold each one (0
// restores immediate inclusion), and the other kinds take no count.
func bridgeFaultOf(b api.BridgeFaultRequest) (BridgeFault, error) {
	if !b.Kind.Valid() {
		return BridgeFault{}, validationError("kind", "kind is not a fault the chain host knows")
	}
	f := BridgeFault{Kind: BridgeFaultKind(b.Kind)}
	switch f.Kind {
	case BridgeFaultDropNext, BridgeFaultDelayInclusion:
		if b.Count == nil || *b.Count < 0 || *b.Count > devnethost.MaxFaultCount {
			return BridgeFault{}, validationError("count", string(f.Kind)+" needs a count between 0 and 1000")
		}
		if f.Kind == BridgeFaultDropNext && *b.Count == 0 {
			return BridgeFault{}, validationError("count", "DROP_NEXT needs at least one submission to drop")
		}
		f.Count = uint64(*b.Count)
	default:
		if b.Count != nil {
			return BridgeFault{}, validationError("count", string(f.Kind)+" takes no count")
		}
	}
	return f, nil
}

// --- rendering -------------------------------------------------------------------------------

func toAPIBridgeRoute(v BridgeRouteView) (api.BridgeRoute, error) {
	r := v.Route
	if r.UnitPrice.Scale < 0 || r.UnitPrice.Scale > money.MaxPriceScale {
		return api.BridgeRoute{}, errs.New(errs.CodeInternal, "a stored route unit price has a scale out of range")
	}
	return api.BridgeRoute{
		ValueDomain: api.SIMULATED, Id: toUUID(r.ID), Name: r.Name, Status: api.BridgeRouteStatus(r.Status), Network: r.Network,
		FacilityId: toUUID(r.FacilityID), SourceChain: string(r.SourceChain), SourceAssetId: toUUID(r.SourceAssetID),
		SourceSymbol: v.Source.Symbol, SourceMint: r.SourceMint.String(), DestinationChain: string(r.DestinationChain),
		DestinationAssetId: toUUID(r.DestinationAssetID), DestinationSymbol: v.Destination.Symbol,
		DestinationMint: r.DestinationMint.String(), Decimals: int(r.Decimals), Attester: r.Attester.String(),
		UnitPrice: api.BridgePrice{
			Mantissa: r.UnitPrice.Mantissa.String(), Scale: int(r.UnitPrice.Scale),
			Decimal: r.UnitPrice.Mantissa.ToDecimalString(uint8(r.UnitPrice.Scale)), Quote: r.UnitPrice.QuoteAsset,
		},
		MaxReleaseAttempts: r.MaxReleaseAttempts, CreatedAt: r.CreatedAt.UTC(),
	}, nil
}

func bridgeEffectStatus(st *econ.EffectStatus) *api.BridgeEffectStatus {
	if st == nil {
		return nil
	}
	v := api.BridgeEffectStatus(*st)
	return &v
}

func toAPIBridgeLeg(v BridgeLegView) api.BridgeLeg {
	l := v.Leg
	r := v.Route.Route
	out := api.BridgeLeg{
		ValueDomain: api.SIMULATED, Id: toUUID(l.ID), RouteId: toUUID(l.RouteID), AccountId: toUUID(l.AccountID),
		IntentId: toUUID(l.IntentID), Mode: api.BridgeLegMode(l.Mode), Quantity: l.Quantity.String(), Decimals: int(r.Decimals),
		SourceChain: string(r.SourceChain), SourceAssetId: toUUID(r.SourceAssetID), SourceSymbol: v.Route.Source.Symbol,
		DestinationChain: string(r.DestinationChain), DestinationAssetId: toUUID(r.DestinationAssetID),
		DestinationSymbol: v.Route.Destination.Symbol, SourceOwner: l.SourceOwner.String(), DestinationOwner: l.DestinationOwner.String(),
		Nonce: strconv.FormatUint(l.Nonce, 10), PlanHash: hex.EncodeToString(l.PlanHash[:]), Status: api.BridgeLegStatus(l.Status),
		Holding: api.BridgeLegHolding(l.Holding()), FailureCode: strPtr(l.FailureCode), LockTxId: strPtr(string(l.LockTxID)),
		SourceEffectStatus: bridgeEffectStatus(v.Effects.Source), DestinationEffectStatus: bridgeEffectStatus(v.Effects.Destination),
		RefundEffectStatus: bridgeEffectStatus(v.Effects.Refund), Reconciliation: toAPIBridgeReconciliation(v.Reconciliation),
		CreatedAt: l.CreatedAt.UTC(), UpdatedAt: l.UpdatedAt.UTC(),
	}
	if l.SourceJournal != nil {
		out.SourceJournalTransactionId = uuidPtr(*l.SourceJournal)
	}
	if l.DestinationJournal != nil {
		out.DestinationJournalTransactionId = uuidPtr(*l.DestinationJournal)
	}
	if l.RefundJournal != nil {
		out.RefundJournalTransactionId = uuidPtr(*l.RefundJournal)
	}
	return out
}

func toAPIBridgeReconciliation(r BridgeLegReconciliation) api.BridgeReconciliation {
	switch {
	case r.Awaiting:
		return api.BridgeReconciliation{State: api.BridgeReconciliationAwaiting, UnknownSince: timePtr(r.UnknownSince)}
	case r.UnknownSince != nil:
		return api.BridgeReconciliation{State: api.BridgeReconciliationResolved, UnknownSince: timePtr(r.UnknownSince),
			ResolvedAt: timePtr(r.ResolvedAt)}
	default:
		return api.BridgeReconciliation{State: api.BridgeReconciliationNotRequired}
	}
}

func toAPIBridgeLegResult(o BridgeLegOutcome) api.BridgeLegResult {
	return api.BridgeLegResult{ValueDomain: api.SIMULATED, Replay: o.Replay, Settled: o.Settled, Leg: toAPIBridgeLeg(o.View)}
}

func toAPIBridgeChainTx(t BridgeChainTxView) api.BridgeChainTx {
	out := api.BridgeChainTx{
		Chain: string(t.Chain), TxId: string(t.TxID), State: api.BridgeTxState(t.Status.State.String()), Final: t.Status.Final,
	}
	switch t.Status.State {
	case devnet.TxConfirmed, devnet.TxFinalized, devnet.TxFailed:
		if t.Status.Height <= uint64(bridgeMaxInt64) {
			h := int64(t.Status.Height)
			out.Height = &h
		}
	}
	if t.Status.State == devnet.TxFailed {
		out.Error = strPtr(string(t.Status.Err))
	}
	return out
}

func toAPIBridgeChain(v BridgeChainView) (api.BridgeChain, error) {
	for _, n := range []uint64{v.Tip.Slot, v.Tip.Height, v.FinalizedHeight, v.Params.BlockhashWindow, v.Params.FinalityDepth} {
		if n > uint64(bridgeMaxInt64) {
			return api.BridgeChain{}, errs.New(errs.CodeInternal, "a chain figure is out of range")
		}
	}
	out := api.BridgeChain{
		Id: api.BridgeChainId(v.Params.ID), TipSlot: int64(v.Tip.Slot), TipHeight: int64(v.Tip.Height), TipHash: v.Tip.Hash.String(),
		FinalizedHeight: int64(v.FinalizedHeight), BlockhashWindow: int64(v.Params.BlockhashWindow),
		FinalityDepth: int64(v.Params.FinalityDepth), FeePerTx: devnet.QuantityOf(v.Params.FeePerTx).String(),
	}
	if !v.Params.BridgeAuthority.IsZero() {
		out.BridgeAuthority = strPtr(v.Params.BridgeAuthority.String())
	}
	return out, nil
}

func toAPIBridgeRouteReconciliation(r bridge.RouteReconciliation) api.BridgeRouteReconciliation {
	return api.BridgeRouteReconciliation{
		ValueDomain: api.SIMULATED, RouteId: toUUID(r.RouteID), Locked: r.Locked.String(), Outstanding: r.Outstanding.String(),
		InFlight: r.InFlight.String(), Refunded: r.Refunded.String(), Escrow: r.Escrow.String(),
		DestinationSupply: r.DestinationSupply.String(), LandedUnpostedLocks: r.LandedUnpostedLocks.String(),
		LandedUnpostedReleases: r.LandedUnpostedReleases.String(), ExpectedEscrow: r.ExpectedEscrow.String(),
		ExpectedSupply: r.ExpectedSupply.String(), BooksConserve: r.BooksConserve, EscrowMatches: r.EscrowMatches,
		SupplyMatches: r.SupplyMatches, Matched: r.Matched(), RecordId: parseUUIDText(r.RecordID), BreakerTripped: r.BreakerTripped,
	}
}

func toAPIBridgeHoldings(v BridgeHoldingsView) (api.BridgeHoldings, error) {
	cc := v.CrossChain
	out := api.BridgeHoldings{
		ValueDomain: api.SIMULATED, AccountId: toUUID(v.AccountID), AsOf: v.AsOf.UTC(),
		Chains: make([]api.BridgeChainHoldings, 0, len(v.Chains)), InFlight: make([]api.BridgeInFlightHolding, 0, len(cc.Holdings)),
		Legs: make([]api.BridgeInFlightLeg, 0, len(cc.Legs)), PendingLocksUsd: cc.PendingLocks.String(),
		Policy: api.BridgeHaircutPolicy{Version: cc.Policy.Version, InFlightBps: int(cc.Policy.InFlightBPS), ObservedBps: int(cc.Policy.ObservedBPS)},
	}
	for _, c := range v.Chains {
		item := api.BridgeChainHoldings{Chain: c.Chain, Holdings: make([]api.BridgeHolding, 0, len(c.Holdings))}
		for _, h := range c.Holdings {
			item.Holdings = append(item.Holdings, api.BridgeHolding{
				AssetId: toUUID(h.AssetID), Symbol: h.Label.Symbol, Decimals: int(h.Label.Decimals), Quantity: h.Quantity.String(),
			})
		}
		out.Chains = append(out.Chains, item)
	}
	for _, h := range cc.Holdings {
		aid, err := assets.ParseAssetID(h.AssetID)
		if err != nil {
			return api.BridgeHoldings{}, errs.Wrap(err, errs.CodeInternal, "an in-flight holding names an asset id that does not parse")
		}
		l, err := stableLabelOf(v.Labels, aid)
		if err != nil {
			return api.BridgeHoldings{}, err
		}
		out.InFlight = append(out.InFlight, api.BridgeInFlightHolding{
			AssetId: toUUID(aid), Symbol: h.Symbol, Decimals: int(l.Decimals), Quantity: h.Quantity.String(),
			MarkedUsd: h.MarkedUSD.String(), FactorBps: int(h.FactorBPS), Reason: h.Reason,
		})
	}
	for _, leg := range cc.Legs {
		l, err := stableLabelOf(v.Labels, leg.AssetID)
		if err != nil {
			return api.BridgeHoldings{}, err
		}
		out.Legs = append(out.Legs, api.BridgeInFlightLeg{
			LegId: toUUID(leg.LegID), RouteId: toUUID(leg.RouteID), AssetId: toUUID(leg.AssetID), Symbol: l.Symbol,
			Decimals: int(l.Decimals), Status: api.BridgeLegStatus(leg.Status), Holding: api.BridgeLegHolding(leg.Holding),
			Quantity: leg.Quantity.String(), FactorBps: int(leg.FactorBPS), Reason: leg.Reason,
		})
	}
	return out, nil
}
