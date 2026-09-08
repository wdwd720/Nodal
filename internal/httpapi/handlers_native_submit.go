package httpapi

import (
	"context"
	"net/http"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/gen/api"
)

// PostNativeAssetsAssetIdSubmit submits the creator's own DRAFT for review.
//
// It exists because the chain from "an asset was created" to "a market trades
// it" had no middle. `POST /native-assets` produced a DRAFT and nothing in any
// deployment could move it: `nativeasset.SetStatus` and `nativemarket.Create`
// were reachable only from tests, so the trading half of Domain A could not be
// started at all (F-28).
//
// This is the creator's half of that chain. The operator's half is the
// NATIVE_MARKET_LAUNCH administrative action, and they are separate because a
// content decision must not be an economic one, and an operator must not be
// able to launch a draft its creator is still editing.
func (s *Server) PostNativeAssetsAssetIdSubmit(ctx context.Context, request api.PostNativeAssetsAssetIdSubmitRequestObject) (api.PostNativeAssetsAssetIdSubmitResponseObject, error) {
	if s.opts.Ports.NativeAssets == nil {
		return nil, errNotWired("the Nodal-native economy")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	accountID, err := accountScope(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	assetID, err := assets.ParseAssetID(request.AssetId.String())
	if err != nil || assetID.IsZero() {
		return nil, validationError("assetId", "assetId must be a canonical UUID")
	}

	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.NativeAsset, commandMeta, error) {
			asset, serr := s.opts.Ports.NativeAssets.Submit(ctx, accountID, assetID)
			if serr != nil {
				return api.NativeAsset{}, commandMeta{}, serr
			}
			return toAPINativeAsset(asset), commandMeta{
				Status:       http.StatusOK,
				ResourceType: "native_asset",
				ResourceID:   asset.AssetID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostNativeAssetsAssetIdSubmit200JSONResponse(res.Value), nil
}
