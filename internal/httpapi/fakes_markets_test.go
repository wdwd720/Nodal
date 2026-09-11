package httpapi

import (
	"context"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/activity"
	"github.com/nodal/controlplane/internal/nativemarket"
)

// Fakes for the discovery, portfolio and activity ports. They answer with
// whatever they were given, so a handler test is about the RESPONSE and not
// about the database underneath it.

type fakeMarketData struct {
	stubErr
	page    nativemarket.MarketPage
	detail  MarketDetailView
	candles CandleView
	trades  TradeTapeView
	// lastList records what the handler asked for, so a test can prove a query
	// parameter reached the domain rather than being dropped.
	lastList nativemarket.ListRequest
	lastCand nativemarket.CandleRequest
}

func (f *fakeMarketData) List(_ context.Context, r nativemarket.ListRequest) (nativemarket.MarketPage, error) {
	f.lastList = r
	if err := f.fail(); err != nil {
		return nativemarket.MarketPage{}, err
	}
	return f.page, nil
}

func (f *fakeMarketData) Detail(context.Context, nativemarket.MarketID) (MarketDetailView, error) {
	if err := f.fail(); err != nil {
		return MarketDetailView{}, err
	}
	return f.detail, nil
}

func (f *fakeMarketData) Candles(_ context.Context, r nativemarket.CandleRequest) (CandleView, error) {
	f.lastCand = r
	if err := f.fail(); err != nil {
		return CandleView{}, err
	}
	return f.candles, nil
}

func (f *fakeMarketData) Trades(context.Context, nativemarket.MarketID, int) (TradeTapeView, error) {
	if err := f.fail(); err != nil {
		return TradeTapeView{}, err
	}
	return f.trades, nil
}

type fakePortfolio struct {
	stubErr
	view PortfolioView
}

func (f *fakePortfolio) Portfolio(context.Context, accounts.AccountID) (PortfolioView, error) {
	if err := f.fail(); err != nil {
		return PortfolioView{}, err
	}
	return f.view, nil
}

type fakeActivityFeed struct {
	stubErr
	page     activity.Page
	lastKind []activity.Kind
}

func (f *fakeActivityFeed) Feed(_ context.Context, r activity.Request) (activity.Page, error) {
	f.lastKind = r.Kinds
	if err := f.fail(); err != nil {
		return activity.Page{}, err
	}
	return f.page, nil
}
