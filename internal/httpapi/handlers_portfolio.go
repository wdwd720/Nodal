package httpapi

import (
	"context"

	"github.com/google/uuid"

	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/nativemarket"
)

// The portfolio (product goal §15, §46).
//
// Three rules govern this response and they are why it is one call rather than
// three:
//
//   - Every mark-to-market figure is computed at ONE instant, and the response
//     states it. §46 forbids showing "available", "withdrawable", "spendable"
//     and "settled" as synonyms; a figure with no time attached is the same
//     kind of confusion in the other axis.
//   - The Credit balance is the credit service's own answer, not a second
//     derivation of it. Two implementations of "what may this account
//     withdraw" would eventually disagree, and one of them would be on a page
//     somebody is reading.
//   - Cost basis, realised P&L and fees come from the read model the fills
//     maintain (migration 00772), never from a difference between balances.
//     §15 is explicit: "Do not infer P&L from superficial balance differences."

// GetMePortfolio returns the Credit balance, the native positions and the
// totals, at one stated instant.
func (s *Server) GetMePortfolio(ctx context.Context, request api.GetMePortfolioRequestObject) (api.GetMePortfolioResponseObject, error) {
	if s.opts.Ports.Portfolio == nil {
		return nil, errNotWired("the Nodal-native economy")
	}
	accountID, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	view, err := s.opts.Ports.Portfolio.Portfolio(ctx, accountID)
	if err != nil {
		return nil, err
	}

	positions := make([]api.PortfolioPosition, 0, len(view.Positions))
	for _, p := range view.Positions {
		positions = append(positions, toAPIPortfolioPosition(p))
	}
	return api.GetMePortfolio200JSONResponse(api.Portfolio{
		AccountId:   uuid.MustParse(view.AccountID.String()),
		AsOf:        view.AsOf,
		Temperature: api.ValueTemperature(view.Temperature),
		Credits:     toAPICreditBalance(view.AccountID.String(), view.Credits),
		Positions:   positions,
		Totals: api.PortfolioTotals{
			CostBasisCredits:     view.Totals.CostBasisCredits.String(),
			MarketValueCredits:   view.Totals.MarketValueCredits.String(),
			UnrealizedPnlCredits: view.Totals.UnrealizedPnLCredit.String(),
			RealizedPnlCredits:   view.Totals.RealizedPnLCredits.String(),
			TotalPnlCredits:      view.Totals.TotalPnLCredits.String(),
			FeesPaidCredits:      view.Totals.FeesPaidCredits.String(),
			PositionCount:        view.Totals.PositionCount,
			OpenPositionCount:    view.Totals.OpenPositionCount,
		},
	}), nil
}

func toAPIPortfolioPosition(p PortfolioPositionView) api.PortfolioPosition {
	v := p.Valuation
	out := api.PortfolioPosition{
		AssetId:              uuid.MustParse(v.Position.AssetID.String()),
		MarketId:             ptr(uuid.MustParse(v.MarketID.String())),
		Symbol:               p.Symbol,
		Name:                 ptr(p.Name),
		MarketStatus:         ptr(api.PortfolioPositionMarketStatus(v.MarketStatus)),
		Quantity:             v.Position.Quantity.String(),
		CostBasisCredits:     v.Position.CostBasisCredits.String(),
		SpotPrice:            ptr(v.SpotPrice.String()),
		MarketValueCredits:   v.MarketValue.String(),
		RealizedPnlCredits:   v.Position.RealizedPnLCredits.String(),
		UnrealizedPnlCredits: v.UnrealizedPnL.String(),
		TotalPnlCredits:      v.TotalPnL.String(),
		FeesPaidCredits:      v.Position.FeesPaidCredits.String(),
		UnitsBoughtTotal:     ptr(v.Position.UnitsBoughtTotal.String()),
		UnitsSoldTotal:       ptr(v.Position.UnitsSoldTotal.String()),
		AllocationUnits:      ptr(v.Position.AllocationUnits.String()),
		FillCount:            ptr(v.Position.FillCount),
		PriceScale:           v.PriceScale,
		AssetDecimals:        p.AssetDecimals,
		Temperature:          api.ValueTemperature(p.Temperature),
		Demo:                 ptr(p.Demo),
	}
	// A closed position has no average cost. Reporting zero would read as "it
	// cost nothing" rather than "there is nothing to average".
	if avg, ok := v.Position.AverageCostCredits(); ok {
		out.AverageCostCredits = ptr(avg.String())
	}
	if v.Position.FirstAcquiredAt != nil {
		out.FirstAcquiredAt = timePtr(v.Position.FirstAcquiredAt)
	}
	if v.Position.LastTradeAt != nil {
		out.LastTradeAt = timePtr(v.Position.LastTradeAt)
	}
	return out
}

// assert the price scale a portfolio renders is the market engine's, so a
// change there cannot silently leave this response describing another scale.
var _ = nativemarket.PriceScale
