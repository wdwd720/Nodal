package httpapi

import (
	"context"

	"github.com/google/uuid"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/nativemarket"
)

// Handlers for market discovery, the asset detail screen, the chart and the
// tape (product goal §§12-14, §35, §47).
//
// Every one of them is a read, and every number they return is an exact integer
// string with its scale beside it. Nothing here formats a price: a client that
// must be told the scale can apply it, and a server that guessed one would be
// wrong the moment a creator chose a different one (F-44).

// GetNativeMarkets returns one page of the markets list.
func (s *Server) GetNativeMarkets(ctx context.Context, request api.GetNativeMarketsRequestObject) (api.GetNativeMarketsResponseObject, error) {
	if s.opts.Ports.MarketData == nil {
		return nil, errNotWired("the Nodal-native economy")
	}
	r := nativemarket.ListRequest{Sort: nativemarket.SortNewest}
	if request.Params.Sort != nil {
		r.Sort = nativemarket.SortBy(*request.Params.Sort)
	}
	if request.Params.Status != nil {
		for _, st := range *request.Params.Status {
			r.Statuses = append(r.Statuses, nativemarket.Status(st))
		}
	}
	// No creator filter. This route is unauthenticated (D-080) and an
	// identifier on it is both readable and enumerable, so the list carries no
	// identity and answers no question about one (D-110). The creator is on the
	// summary read, which is behind a session.
	if request.Params.Q != nil {
		r.Query = *request.Params.Q
	}
	if request.Params.Cursor != nil {
		r.Cursor = *request.Params.Cursor
	}
	if request.Params.Limit != nil {
		r.Limit = *request.Params.Limit
	}
	page, err := s.opts.Ports.MarketData.List(ctx, r)
	if err != nil {
		return nil, err
	}
	markets := make([]api.NativeMarketSummary, 0, len(page.Markets))
	for _, m := range page.Markets {
		markets = append(markets, toAPIMarketSummary(m))
	}
	return api.GetNativeMarkets200JSONResponse(api.NativeMarketPage{
		Markets:    markets,
		Sort:       api.NativeMarketPageSort(r.Sort),
		Stable:     page.Stable,
		NextCursor: nextCursor(page.NextCursor),
	}), nil
}

// GetNativeMarketsMarketIdSummary returns the asset detail / trading screen's
// read, including the limits that would refuse a trade.
func (s *Server) GetNativeMarketsMarketIdSummary(ctx context.Context, request api.GetNativeMarketsMarketIdSummaryRequestObject) (api.GetNativeMarketsMarketIdSummaryResponseObject, error) {
	if s.opts.Ports.MarketData == nil {
		return nil, errNotWired("the Nodal-native economy")
	}
	marketID, err := nativemarket.ParseMarketID(request.MarketId.String())
	if err != nil {
		return nil, validationError("marketId", "marketId must be a canonical UUID")
	}
	// An account is optional here and must be one the caller owns: it marks the
	// caller's own row in the holder list and changes nothing else (D-111).
	var caller accounts.AccountID
	if request.Params.AccountId != nil {
		caller, err = accountScope(ctx, *request.Params.AccountId)
		if err != nil {
			return nil, err
		}
	}
	view, err := s.opts.Ports.MarketData.Detail(ctx, marketID, caller)
	if err != nil {
		return nil, err
	}
	holders := toAPIHolders(view.Holders)
	out := api.NativeMarketDetail{
		Market:        toAPIMarketSummary(view.Summary),
		LimitsInForce: toAPIMarketLimits(view.Limits),
		TopHolders:    &holders,
	}
	// The creator, on the gated read and not on the summary the public list
	// serves (D-110). A market whose creator this projection did not carry
	// renders no field at all rather than the nil UUID, which would read as an
	// account that exists.
	if !view.Summary.CreatorAccountID.IsZero() {
		out.CreatorAccountId = ptr(uuid.MustParse(view.Summary.CreatorAccountID.String()))
	}
	return api.GetNativeMarketsMarketIdSummary200JSONResponse(out), nil
}

// GetNativeMarketsMarketIdCandles returns OHLCV over a bounded window.
func (s *Server) GetNativeMarketsMarketIdCandles(ctx context.Context, request api.GetNativeMarketsMarketIdCandlesRequestObject) (api.GetNativeMarketsMarketIdCandlesResponseObject, error) {
	if s.opts.Ports.MarketData == nil {
		return nil, errNotWired("the Nodal-native economy")
	}
	marketID, err := nativemarket.ParseMarketID(request.MarketId.String())
	if err != nil {
		return nil, validationError("marketId", "marketId must be a canonical UUID")
	}
	view, err := s.opts.Ports.MarketData.Candles(ctx, nativemarket.CandleRequest{
		MarketID: marketID,
		Interval: nativemarket.Interval(request.Params.Interval),
		From:     request.Params.From,
		To:       request.Params.To,
	})
	if err != nil {
		return nil, err
	}
	candles := make([]api.NativeCandle, 0, len(view.Candles))
	for _, c := range view.Candles {
		candles = append(candles, api.NativeCandle{
			OpenTime:     c.OpenTime,
			Open:         c.Open.String(),
			High:         c.High.String(),
			Low:          c.Low.String(),
			Close:        c.Close.String(),
			CreditVolume: c.CreditVolume.String(),
			AssetVolume:  c.AssetVolume.String(),
			Trades:       c.Trades,
		})
	}
	return api.GetNativeMarketsMarketIdCandles200JSONResponse(api.NativeCandlePage{
		MarketId:      uuid.MustParse(view.MarketID.String()),
		Interval:      api.NativeCandlePageInterval(view.Interval),
		From:          view.From,
		To:            view.To,
		PriceScale:    view.PriceScale,
		AssetDecimals: view.AssetDecimals,
		Candles:       candles,
	}), nil
}

// GetNativeMarketsMarketIdTrades returns the public tape.
func (s *Server) GetNativeMarketsMarketIdTrades(ctx context.Context, request api.GetNativeMarketsMarketIdTradesRequestObject) (api.GetNativeMarketsMarketIdTradesResponseObject, error) {
	if s.opts.Ports.MarketData == nil {
		return nil, errNotWired("the Nodal-native economy")
	}
	marketID, err := nativemarket.ParseMarketID(request.MarketId.String())
	if err != nil {
		return nil, validationError("marketId", "marketId must be a canonical UUID")
	}
	limit := 0
	if request.Params.Limit != nil {
		limit = *request.Params.Limit
	}
	view, err := s.opts.Ports.MarketData.Trades(ctx, marketID, limit)
	if err != nil {
		return nil, err
	}
	trades := make([]api.NativeTradePrint, 0, len(view.Prints))
	for _, p := range view.Prints {
		trades = append(trades, api.NativeTradePrint{
			Seq:             p.Seq,
			Side:            api.NativeTradePrintSide(p.Side),
			EffectivePrice:  p.EffectivePrice.String(),
			SpotPriceBefore: ptr(p.SpotBefore.String()),
			SpotPriceAfter:  p.SpotAfter.String(),
			CreditVolume:    p.CreditVolume.String(),
			AssetVolume:     p.AssetVolume.String(),
			PrintedAt:       p.PrintedAt,
		})
	}
	return api.GetNativeMarketsMarketIdTrades200JSONResponse(api.NativeTradePage{
		MarketId:      uuid.MustParse(view.MarketID.String()),
		PriceScale:    view.PriceScale,
		AssetDecimals: view.AssetDecimals,
		Trades:        trades,
	}), nil
}

// --- rendering ---------------------------------------------------------------

// toAPIHolders renders the concentration without naming anybody.
//
// There is no account id on the wire at all -- not even the caller's own, which
// they sent -- because a field that is sometimes an identity is a field a client
// will eventually render as one. `is_you` carries the whole of what the caller
// is entitled to learn from this list about a person (D-111).
func toAPIHolders(hs []nativemarket.Holding) []api.NativeAssetHolder {
	out := make([]api.NativeAssetHolder, 0, len(hs))
	for _, h := range hs {
		row := api.NativeAssetHolder{
			Rank:     h.Rank,
			Quantity: h.Quantity.String(),
			ShareBps: int(h.ShareBPS),
		}
		if h.IsYou {
			row.IsYou = ptr(true)
		}
		out = append(out, row)
	}
	return out
}

func toAPIMarketSummary(m nativemarket.MarketSummary) api.NativeMarketSummary {
	out := api.NativeMarketSummary{
		MarketId:             uuid.MustParse(m.MarketID.String()),
		AssetId:              uuid.MustParse(m.AssetID.String()),
		CreditAssetId:        uuid.MustParse(m.CreditAssetID.String()),
		Name:                 m.Name,
		Symbol:               m.Symbol,
		MarketStatus:         api.NativeMarketSummaryMarketStatus(m.MarketStatus),
		AssetStatus:          api.NativeMarketSummaryAssetStatus(m.AssetStatus),
		LastPrice:            m.LastPrice.String(),
		PriceScale:           m.PriceScale,
		CreditVolume24h:      m.CreditVolume24h.String(),
		Trades24h:            m.Trades24h,
		VirtualCreditReserve: m.Curve.VirtualCreditReserve.String(),
		InitialAssetReserve:  m.Curve.InitialAssetReserve.String(),
		RealCreditReserve:    m.State.RealCreditReserve.String(),
		AssetReserve:         m.State.AssetReserve.String(),
		LiquidityCredits:     m.LiquidityCredits.String(),
		CirculatingSupply:    m.CirculatingSupply.String(),
		MaxSupply:            m.MaxSupply.String(),
		AssetDecimals:        int(m.AssetDecimals),
		PlatformFeeBps:       int(m.Fees.PlatformBPS),
		CreatorFeeBps:        int(m.Fees.CreatorBPS),
		StateVersion:         ptr(m.State.Version),
		Demo:                 m.Demo,
		CreatedAt:            m.CreatedAt,
	}
	if m.Description != "" {
		out.Description = ptr(m.Description)
	}
	if m.ImageURL != "" {
		out.ImageUrl = ptr(m.ImageURL)
	}
	if m.Moderation != "" {
		out.ModerationState = ptr(api.NativeMarketSummaryModerationState(m.Moderation))
	}
	if m.ActivatedAt != nil {
		out.ActivatedAt = m.ActivatedAt
	}
	// A market with no trade in the window has no 24-hour change, which is a
	// different fact from a change of zero. Both fields say so together.
	out.Has24hChange = ptr(m.HasChange24h)
	if m.HasChange24h {
		out.ReferencePrice24h = ptr(m.ReferencePrice24h.String())
		out.Change24hBps = ptr(int64(m.Change24hBPS))
	}
	return out
}

func toAPIMarketLimits(l LimitsInForce) api.MarketSafetyLimits {
	out := api.MarketSafetyLimits{SafetyPolicyVersion: l.Safety.Version}
	if l.Safety.MaxPriceImpactBPS != nil {
		out.MaxPriceImpactBps = ptr(int(*l.Safety.MaxPriceImpactBPS))
	}
	if l.Safety.MaxSlippageBPS != nil {
		out.MaxSlippageBps = ptr(int(*l.Safety.MaxSlippageBPS))
	}
	if l.Safety.CircuitBreakerMoveBPS != nil {
		out.CircuitBreakerMoveBps = ptr(int64(*l.Safety.CircuitBreakerMoveBPS))
	}
	if l.Safety.CircuitBreakerWindowSeconds != nil {
		out.CircuitBreakerWindowSeconds = ptr(*l.Safety.CircuitBreakerWindowSeconds)
	}
	if l.Safety.MinOpeningLiquidityCredits != nil {
		out.MinOpeningLiquidityCredits = ptr(l.Safety.MinOpeningLiquidityCredits.String())
	}
	if l.Safety.CreatorMayBuyOwnAsset != nil {
		out.CreatorMayBuyOwnAsset = ptr(*l.Safety.CreatorMayBuyOwnAsset)
	}
	if l.RiskPolicyVersion != "" {
		out.RiskPolicyVersion = ptr(l.RiskPolicyVersion)
	}
	if l.MaxNativeMarketConcentrationBPS != nil {
		out.MaxNativeMarketConcentrationBps = ptr(int(*l.MaxNativeMarketConcentrationBPS))
	}
	if l.MaxCreatorConcentrationBPS != nil {
		out.MaxCreatorConcentrationBps = ptr(int(*l.MaxCreatorConcentrationBPS))
	}
	return out
}
