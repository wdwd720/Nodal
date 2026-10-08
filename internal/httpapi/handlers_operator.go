package httpapi

import (
	"context"
	"math"
	"net/http"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/observability"
)

// The operator console routes (/advanced/ops). Handlers translate shapes
// only: what a mark may be, and every figure the overview reports, is the
// operator adapter's to decide from the database and the devnet host.

const operatorFeature = "the operator console"

// GetAdvancedOpsOverview is the console's one read.
func (s *Server) GetAdvancedOpsOverview(ctx context.Context, _ api.GetAdvancedOpsOverviewRequestObject) (api.GetAdvancedOpsOverviewResponseObject, error) {
	port := s.opts.Ports.Operator
	if port == nil {
		return nil, errNotWired(operatorFeature)
	}
	v, err := port.Overview(ctx)
	if err != nil {
		return nil, err
	}
	return api.GetAdvancedOpsOverview200JSONResponse(toAPIOpsOverview(v)), nil
}

// GetAdvancedOpsMarks lists the latest reference marks of an asset in the
// sandbox dollar.
func (s *Server) GetAdvancedOpsMarks(ctx context.Context, request api.GetAdvancedOpsMarksRequestObject) (api.GetAdvancedOpsMarksResponseObject, error) {
	port := s.opts.Ports.Operator
	if port == nil {
		return nil, errNotWired(operatorFeature)
	}
	asset, err := assets.ParseAssetID(request.Params.AssetId.String())
	if err != nil || asset.IsZero() {
		return nil, validationError("asset_id", "asset_id must be a canonical UUID")
	}
	v, err := port.Marks(ctx, asset, pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	out := api.AdvancedReferenceMarkList{
		AssetId: toUUID(v.Asset.ID), AssetSymbol: v.Asset.Symbol, AssetKind: string(v.Asset.Kind),
		QuoteAssetId: toUUID(v.Quote.ID), QuoteAssetSymbol: v.Quote.Symbol, MaxAgeMs: v.MaxAge.Milliseconds(),
		AsOf: v.AsOf.UTC(), Items: make([]api.AdvancedReferenceMark, 0, len(v.Items)), ValueDomain: api.SIMULATED,
	}
	for _, m := range v.Items {
		item, cerr := toAPIReferenceMark(m)
		if cerr != nil {
			return nil, cerr
		}
		out.Items = append(out.Items, item)
	}
	return api.GetAdvancedOpsMarks200JSONResponse(out), nil
}

// PostAdvancedOpsMarks records one sandbox reference mark with its audit
// event.
func (s *Server) PostAdvancedOpsMarks(ctx context.Context, request api.PostAdvancedOpsMarksRequestObject) (api.PostAdvancedOpsMarksResponseObject, error) {
	port := s.opts.Ports.Operator
	if port == nil {
		return nil, errNotWired(operatorFeature)
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	b := request.Body
	asset, err := assets.ParseAssetID(b.AssetId.String())
	if err != nil || asset.IsZero() {
		return nil, validationError("asset_id", "asset_id must be a canonical UUID")
	}
	if b.Price == "" {
		return nil, validationError("price", "price is required")
	}
	if b.Source == "" {
		return nil, validationError("source", "source is required")
	}
	actor, err := marginActor(ctx)
	if err != nil {
		return nil, err
	}
	cmd := ReferenceMarkCommand{
		AssetID: asset, Price: b.Price, Source: b.Source, Actor: actor, CorrelationID: observability.CorrelationID(ctx),
	}
	if b.QuoteAssetId != nil {
		quote, perr := assets.ParseAssetID(b.QuoteAssetId.String())
		if perr != nil || quote.IsZero() {
			return nil, validationError("quote_asset_id", "quote_asset_id must be a canonical UUID")
		}
		cmd.QuoteAssetID = &quote
	}
	if b.ObservedAt != nil {
		at := b.ObservedAt.UTC()
		cmd.ObservedAt = &at
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.AdvancedReferenceMarkRecorded, commandMeta, error) {
			v, rerr := port.RecordMark(ctx, cmd)
			if rerr != nil {
				return api.AdvancedReferenceMarkRecorded{}, commandMeta{}, rerr
			}
			mark, cerr := toAPIReferenceMark(v.Mark)
			if cerr != nil {
				return api.AdvancedReferenceMarkRecorded{}, commandMeta{}, cerr
			}
			status := http.StatusCreated
			if v.Duplicate {
				// The same observation was already recorded: nothing new exists.
				status = http.StatusOK
			}
			return api.AdvancedReferenceMarkRecorded{
				Mark: mark, AssetSymbol: v.AssetSymbol, QuoteAssetSymbol: v.QuoteSymbol, Duplicate: v.Duplicate,
				MaxAgeMs: v.MaxAge.Milliseconds(), ValueDomain: api.SIMULATED,
			}, commandMeta{Status: status, ResourceType: "asset_price", ResourceID: v.Mark.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	if res.Replayed || res.Meta.Status == http.StatusOK {
		return api.PostAdvancedOpsMarks200JSONResponse(res.Value), nil
	}
	return api.PostAdvancedOpsMarks201JSONResponse(res.Value), nil
}

// GetAdvancedOpsChains reports the tip of each simulated chain.
func (s *Server) GetAdvancedOpsChains(ctx context.Context, _ api.GetAdvancedOpsChainsRequestObject) (api.GetAdvancedOpsChainsResponseObject, error) {
	port := s.opts.Ports.Operator
	if port == nil {
		return nil, errNotWired(operatorFeature)
	}
	v, err := port.Chains(ctx)
	if err != nil {
		return nil, err
	}
	out := api.AdvancedOpsChains{
		Network: v.Network, ObservedAt: v.ObservedAt.UTC(), Items: make([]api.AdvancedOpsChainTip, 0, len(v.Items)),
		ValueDomain: api.SIMULATED,
	}
	for _, c := range v.Items {
		slot, serr := opsInt64(c.Slot)
		height, herr := opsInt64(c.Height)
		finalized, ferr := opsInt64(c.FinalizedHeight)
		if serr != nil || herr != nil || ferr != nil {
			return nil, errs.New(errs.CodeInternal, "a simulated chain reported a height beyond the API's range")
		}
		out.Items = append(out.Items, api.AdvancedOpsChainTip{
			ChainId: c.ChainID, Slot: slot, Height: height, Hash: c.Hash, Final: c.Final, FinalizedHeight: finalized,
			LastBlockProducedAt: timePtr(c.LastBlockProducedAt),
		})
	}
	return api.GetAdvancedOpsChains200JSONResponse(out), nil
}

// --- response shapes ----------------------------------------------------------

func opsInt64(v uint64) (int64, error) {
	if v > math.MaxInt64 {
		return 0, errs.New(errs.CodeOverflow, "value exceeds int64")
	}
	return int64(v), nil // #nosec G115 -- bounded by the check above
}

func toAPIReferenceMark(m ReferenceMark) (api.AdvancedReferenceMark, error) {
	if m.Scale < 0 || m.Scale > money.MaxPriceScale || m.Mantissa.IsNegative() {
		return api.AdvancedReferenceMark{}, errs.New(errs.CodeInternal, "a stored price observation is out of range")
	}
	return api.AdvancedReferenceMark{
		Id: toUUID(m.ID), AssetId: toUUID(m.AssetID), QuoteAssetId: toUUID(m.QuoteAssetID),
		Price:    m.Mantissa.ToDecimalString(uint8(m.Scale)), // #nosec G115 -- 0..38 by the check above
		Mantissa: m.Mantissa.String(), Scale: int(m.Scale), Source: m.Source, ObservedAt: m.ObservedAt.UTC(),
		ReceivedAt: m.ReceivedAt.UTC(), AgeMs: m.Age.Milliseconds(), Stale: m.Stale, Future: m.Future,
	}, nil
}

func toAPIOpsRecord(r *OpsReconciliationRecord) *api.AdvancedOpsReconciliationRecord {
	if r == nil {
		return nil
	}
	return &api.AdvancedOpsReconciliationRecord{
		Id: r.ID, Kind: r.Kind, Status: r.Status, Material: r.Material, BlocksNewRisk: r.BlocksNewRisk,
		OpenedAt: r.OpenedAt.UTC(), ResolvedAt: timePtr(r.ResolvedAt),
	}
}

func toAPIOpsOverview(v OpsOverview) api.AdvancedOpsOverview {
	out := api.AdvancedOpsOverview{
		GeneratedAt: v.GeneratedAt.UTC(), Environment: v.Environment, MaxMarkAgeMs: v.MaxMarkAge.Milliseconds(),
		KillSwitches:   make([]api.AdvancedOpsKillSwitch, 0, len(v.KillSwitches)),
		StableAssets:   make([]api.AdvancedOpsStableAsset, 0, len(v.StableAssets)),
		BridgeRoutes:   make([]api.AdvancedOpsBridgeRoute, 0, len(v.BridgeRoutes)),
		Reconciliation: make([]api.AdvancedOpsReconciliationKind, 0, len(v.Reconciliation)),
		DualControl: api.AdvancedOpsDualControl{
			TransferReversalsPending: v.DualControl.TransferReversalsPending,
			MarginCasesInReview:      v.DualControl.MarginCasesInReview,
			MarginResolutionsPending: v.DualControl.MarginResolutionsPending,
		},
		Oracle:           make([]api.AdvancedOpsOracleAsset, 0, len(v.Oracle)),
		ChainsConfigured: v.ChainsConfigured,
		ValueDomain:      api.SIMULATED,
	}
	for _, k := range v.KillSwitches {
		out.KillSwitches = append(out.KillSwitches, api.AdvancedOpsKillSwitch{
			Kind: string(k.Kind), ScopeId: k.ScopeID, Severity: string(k.Severity), Reason: strPtr(k.Reason),
			ActivatedBy: strPtr(k.ActivatedBy), ActivatedAt: timePtr(k.ActivatedAt),
		})
	}
	for _, a := range v.StableAssets {
		item := api.AdvancedOpsStableAsset{
			Id: a.ID, AssetId: a.AssetID, Name: a.Name, ChainId: a.ChainID, Status: a.Status, LatestSupplyReconciliation: toAPIOpsRecord(a.LatestSupplyRecord),
		}
		if a.LatestAlert != nil {
			item.LatestAlert = &api.AdvancedOpsPegAlert{
				Kind: a.LatestAlert.Kind, Severity: a.LatestAlert.Severity, BreakerTripped: a.LatestAlert.BreakerTripped,
				ObservedAt: a.LatestAlert.ObservedAt.UTC(),
			}
		}
		out.StableAssets = append(out.StableAssets, item)
	}
	for _, r := range v.BridgeRoutes {
		item := api.AdvancedOpsBridgeRoute{
			Id: r.ID, Name: r.Name, SourceChain: r.SourceChain, DestinationChain: r.DestinationChain, Status: r.Status,
			OpenReconciliation: toAPIOpsRecord(r.OpenReconciliation),
		}
		out.BridgeRoutes = append(out.BridgeRoutes, item)
	}
	out.BridgeLegsInReview = make([]api.AdvancedOpsBridgeLeg, 0, len(v.BridgeLegsInReview))
	for _, l := range v.BridgeLegsInReview {
		out.BridgeLegsInReview = append(out.BridgeLegsInReview, api.AdvancedOpsBridgeLeg{
			Id: l.ID, RouteId: l.RouteID, RouteName: l.RouteName, AccountId: l.AccountID, Quantity: l.Quantity.String(),
			Decimals: int(l.Decimals), SourceSymbol: l.SourceSymbol, Released: l.Released, RefundRequested: l.RefundRequested,
			EnteredReviewAt: timePtr(l.EnteredReviewAt), ReviewReason: strPtr(l.ReviewReason),
		})
	}
	for _, k := range v.Reconciliation {
		item := api.AdvancedOpsReconciliationKind{
			Kind: k.Kind, Unresolved: k.Unresolved, Material: k.Material, BlocksNewRisk: k.BlocksNewRisk,
			OldestOpenedAt: timePtr(k.OldestOpened),
		}
		if k.OldestAge != nil {
			age := k.OldestAge.Milliseconds()
			item.OldestAgeMs = &age
		}
		out.Reconciliation = append(out.Reconciliation, item)
	}
	for _, o := range v.Oracle {
		item := api.AdvancedOpsOracleAsset{
			AssetId: toUUID(o.AssetID), Symbol: o.Symbol, ObservedAt: timePtr(o.ObservedAt), Source: strPtr(o.Source), Stale: o.Stale,
		}
		if o.Age != nil {
			age := o.Age.Milliseconds()
			item.AgeMs = &age
		}
		out.Oracle = append(out.Oracle, item)
	}
	return out
}
