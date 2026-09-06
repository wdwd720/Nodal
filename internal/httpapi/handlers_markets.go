package httpapi

import (
	"context"

	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/instruments"
)

// maxRegistryPage bounds the reference-data listings, which have no cursor in
// the v1 contract: an unbounded history is exactly what PART 108 forbids.
const maxRegistryPage = 500

// GetAssets lists the registered assets with chain, mint, decimals and safety
// status. Identity is (chain, mint); the symbol is display metadata.
func (s *Server) GetAssets(ctx context.Context, _ api.GetAssetsRequestObject) (api.GetAssetsResponseObject, error) {
	if s.opts.Ports.Assets == nil {
		return nil, errNotWired("the asset registry")
	}
	list, err := s.opts.Ports.Assets.List(ctx, maxRegistryPage)
	if err != nil {
		return nil, err
	}
	out := make([]api.Asset, 0, len(list))
	for _, a := range list {
		out = append(out, toAPIAsset(a))
	}
	return api.GetAssets200JSONResponse(out), nil
}

// GetInstruments lists tradable instruments with their status.
func (s *Server) GetInstruments(ctx context.Context, _ api.GetInstrumentsRequestObject) (api.GetInstrumentsResponseObject, error) {
	if s.opts.Ports.Instruments == nil {
		return nil, errNotWired("the instrument registry")
	}
	list, err := s.opts.Ports.Instruments.List(ctx, maxRegistryPage)
	if err != nil {
		return nil, err
	}
	out := make([]api.Instrument, 0, len(list))
	for _, i := range list {
		out = append(out, toAPIInstrument(i))
	}
	return api.GetInstruments200JSONResponse(out), nil
}

// GetInstrumentsInstrumentId returns an instrument with its venue listings.
func (s *Server) GetInstrumentsInstrumentId(ctx context.Context, request api.GetInstrumentsInstrumentIdRequestObject) (api.GetInstrumentsInstrumentIdResponseObject, error) {
	if s.opts.Ports.Instruments == nil {
		return nil, errNotWired("the instrument registry")
	}
	instrumentID, err := instruments.ParseInstrumentID(request.InstrumentId.String())
	if err != nil || instrumentID.IsZero() {
		return nil, validationError("instrumentId", "instrumentId must be a canonical UUID")
	}
	d, err := s.opts.Ports.Instruments.Detail(ctx, instrumentID)
	if err != nil {
		return nil, err
	}
	return api.GetInstrumentsInstrumentId200JSONResponse(toAPIInstrumentDetail(d)), nil
}
