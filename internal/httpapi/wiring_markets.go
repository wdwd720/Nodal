package httpapi

import (
	"context"
	"errors"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/activity"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/nativemarket"
	"github.com/nodal/controlplane/internal/risk"
)

// Adapters for market discovery, the portfolio and the activity timeline.
//
// # Why GET /me/activity exists beside GET /accounts/{id}/activity
//
// They answer different questions and read different tables, and unifying them
// would have made one of the two answers worse.
//
// `/accounts/{id}/activity` is the OPERATIONAL timeline of the hosted
// real-capital rail: trade intents, orders, venue fills, deposits, journal
// transactions and reconciliation records. It is what an operator reads when
// something went wrong with somebody's money, and it deliberately contains rows
// a customer has no use for.
//
// `/me/activity` is the PRODUCT timeline of §16: Credit purchases and
// reversals, internal-market trades, asset creation, payouts and adjustments,
// each with an amount, an origin, a temperature and a sentence. It is what a
// person reads on their own activity page.
//
// Merging them would mean either showing a customer their own journal
// transactions -- which is the same movement counted twice next to the trade
// that caused it -- or dropping the operational rows an operator needs.
// So both stay, the hosted rail keeps the one it has, and the product surface
// gets the one §16 describes. Recorded as D-066.

// MarketDataDeps are the services behind the discovery, chart and detail reads.
type MarketDataDeps struct {
	Markets *nativemarket.Service
	DB      *db.DB
	Clock   clock.Clock
	// RiskPolicies reads the composed GLOBAL risk policy, for the "limits in
	// force" disclosure. Nil means this deployment cannot report them, which
	// the response renders as an absent risk policy rather than as zero.
	RiskPolicies *risk.Store
}

type marketDataAdapter struct {
	deps MarketDataDeps
}

func (a marketDataAdapter) List(ctx context.Context, r nativemarket.ListRequest) (nativemarket.MarketPage, error) {
	return a.deps.Markets.ListMarkets(ctx, a.deps.DB, r)
}

func (a marketDataAdapter) Detail(ctx context.Context, marketID nativemarket.MarketID) (MarketDetailView, error) {
	summary, err := a.deps.Markets.MarketSummaryByID(ctx, a.deps.DB, marketID)
	if err != nil {
		return MarketDetailView{}, err
	}
	safety, err := a.deps.Markets.EffectiveSafetyPolicy(ctx, a.deps.DB)
	if err != nil {
		return MarketDetailView{}, err
	}
	holders, err := a.deps.Markets.Holders(ctx, a.deps.DB, summary.AssetID, TopHolderLimit)
	if err != nil {
		return MarketDetailView{}, err
	}
	limits := LimitsInForce{Safety: safety}
	if a.deps.RiskPolicies != nil {
		// The GLOBAL policy, with no account scope: these are the limits every
		// trader faces before their own ACCOUNT row tightens them. Reporting a
		// caller's composed policy here would leak one account's limits onto a
		// page every account reads.
		p, _, perr := a.deps.RiskPolicies.EffectivePolicy(ctx, a.deps.DB, "", "", a.deps.Clock.Now())
		switch {
		case errors.Is(perr, risk.ErrNoPolicy):
			// Left empty on purpose: this deployment has recorded no risk
			// policy, which means it can evaluate no internal trade at all.
			// Rendering a zero limit would say the opposite.
		case perr != nil:
			return MarketDetailView{}, perr
		default:
			rl := riskLimitsOf(p)
			limits.RiskPolicyVersion = rl.RiskPolicyVersion
			limits.MaxNativeMarketConcentrationBPS = rl.MaxNativeMarketConcentrationBPS
			limits.MaxCreatorConcentrationBPS = rl.MaxCreatorConcentrationBPS
		}
	}
	return MarketDetailView{Summary: summary, Limits: limits, Holders: holders}, nil
}

func (a marketDataAdapter) Candles(ctx context.Context, r nativemarket.CandleRequest) (CandleView, error) {
	m, err := a.deps.Markets.Market(ctx, a.deps.DB, r.MarketID)
	if err != nil {
		return CandleView{}, err
	}
	asset, err := assets.NewRepository().Get(ctx, a.deps.DB, m.AssetID)
	if err != nil {
		return CandleView{}, err
	}
	candles, err := a.deps.Markets.Candles(ctx, a.deps.DB, r)
	if err != nil {
		return CandleView{}, err
	}
	return CandleView{
		MarketID: r.MarketID, Interval: r.Interval, From: r.From.UTC(), To: r.To.UTC(),
		PriceScale: nativemarket.PriceScale, AssetDecimals: int(asset.Decimals), Candles: candles,
	}, nil
}

func (a marketDataAdapter) Trades(ctx context.Context, marketID nativemarket.MarketID, limit int) (TradeTapeView, error) {
	m, err := a.deps.Markets.Market(ctx, a.deps.DB, marketID)
	if err != nil {
		return TradeTapeView{}, err
	}
	asset, err := assets.NewRepository().Get(ctx, a.deps.DB, m.AssetID)
	if err != nil {
		return TradeTapeView{}, err
	}
	prints, err := a.deps.Markets.RecentPrints(ctx, a.deps.DB, marketID, limit)
	if err != nil {
		return TradeTapeView{}, err
	}
	return TradeTapeView{
		MarketID: marketID, PriceScale: nativemarket.PriceScale,
		AssetDecimals: int(asset.Decimals), Prints: prints,
	}, nil
}

// --- portfolio --------------------------------------------------------------

// PortfolioDeps are what §15's answer is assembled from.
type PortfolioDeps struct {
	Markets *nativemarket.Service
	// Credits is the SAME port GET /credits/balance uses. The portfolio does
	// not re-derive a balance: two implementations of "what may this account
	// withdraw" would eventually disagree, and one of them would be on a page
	// somebody is reading.
	Credits CreditsPort
	DB      *db.DB
	Clock   clock.Clock
	// Simulated is cfg.SandboxTier(): on a sandbox tier no value is real, so
	// every figure in the portfolio is SIMULATED and says so.
	Simulated bool
}

type portfolioAdapter struct {
	deps PortfolioDeps
}

func (a portfolioAdapter) Portfolio(ctx context.Context, accountID accounts.AccountID) (PortfolioView, error) {
	// One instant for every mark below. Reading the clock per position would
	// produce a page whose rows are true at different times and whose totals
	// are true at none of them.
	asOf := a.deps.Clock.Now().UTC()

	balances, err := a.deps.Credits.Balance(ctx, accountID)
	if err != nil {
		return PortfolioView{}, err
	}
	rows, err := a.deps.Markets.PortfolioPositions(ctx, a.deps.DB, accountID)
	if err != nil {
		return PortfolioView{}, err
	}

	temperature := activity.TemperatureEconomy
	if a.deps.Simulated {
		temperature = activity.TemperatureSimulated
	}
	view := PortfolioView{
		AccountID: accountID, AsOf: asOf, Temperature: temperature, Credits: balances,
	}
	totals := PortfolioTotals{}
	for _, row := range rows {
		valuation := nativemarket.Value(row.Position, row.Market, row.State, asOf)
		positionTemp := temperature
		if row.Demo {
			positionTemp = activity.TemperatureSimulated
		}
		view.Positions = append(view.Positions, PortfolioPositionView{
			Valuation: valuation, Symbol: row.Symbol, Name: row.Name,
			AssetDecimals: int(row.AssetDecimals), Demo: row.Demo, Temperature: positionTemp,
		})
		totals.CostBasisCredits = totals.CostBasisCredits.Add(row.Position.CostBasisCredits)
		totals.MarketValueCredits = totals.MarketValueCredits.Add(valuation.MarketValue)
		totals.UnrealizedPnLCredit = totals.UnrealizedPnLCredit.Add(valuation.UnrealizedPnL)
		totals.RealizedPnLCredits = totals.RealizedPnLCredits.Add(row.Position.RealizedPnLCredits)
		totals.FeesPaidCredits = totals.FeesPaidCredits.Add(row.Position.FeesPaidCredits)
		totals.PositionCount++
		if row.Position.Quantity.Sign() > 0 {
			totals.OpenPositionCount++
		}
	}
	totals.TotalPnLCredits = totals.UnrealizedPnLCredit.Add(totals.RealizedPnLCredits)
	view.Totals = totals
	return view, nil
}

// --- activity ---------------------------------------------------------------

type activityFeedAdapter struct {
	feed *activity.Feed
	db   *db.DB
}

func (a activityFeedAdapter) Feed(ctx context.Context, r activity.Request) (activity.Page, error) {
	return a.feed.Activity(ctx, a.db, r)
}

// WireMarketData attaches the three ports when the services behind them exist.
//
// It is called from Wire alongside wireNativeEconomy. A deployment with no
// native market service gets nil ports and UNSUPPORTED answers, which is the
// truth about a deployment that has not provisioned the internal economy.
func WireMarketData(p *Ports, markets MarketDataDeps, portfolio PortfolioDeps, feed *activity.Feed, database *db.DB) {
	if markets.Markets != nil && markets.DB != nil && markets.Clock != nil {
		p.MarketData = marketDataAdapter{deps: markets}
	}
	if portfolio.Markets != nil && portfolio.Credits != nil && portfolio.DB != nil && portfolio.Clock != nil {
		p.Portfolio = portfolioAdapter{deps: portfolio}
	}
	if feed != nil && database != nil {
		p.ActivityFeed = activityFeedAdapter{feed: feed, db: database}
	}
}
