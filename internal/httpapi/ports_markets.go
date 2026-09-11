package httpapi

import (
	"context"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/activity"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/nativemarket"
	"github.com/nodal/controlplane/internal/risk"
)

// Ports for market discovery, charts, the portfolio and the activity timeline
// (product goal §§12-16, §35, §47).
//
// They follow the rule every other port here does: one method per domain call,
// no transaction handling above this line, and a nil port answers UNSUPPORTED
// rather than an empty page. An empty markets list and a deployment with no
// internal economy look identical to a browser, and only one of them is a
// product.

// MarketDataPort reads the markets page, the asset detail screen, the chart and
// the tape.
type MarketDataPort interface {
	List(ctx context.Context, r nativemarket.ListRequest) (nativemarket.MarketPage, error)
	Detail(ctx context.Context, marketID nativemarket.MarketID) (MarketDetailView, error)
	Candles(ctx context.Context, r nativemarket.CandleRequest) (CandleView, error)
	Trades(ctx context.Context, marketID nativemarket.MarketID, limit int) (TradeTapeView, error)
}

// MarketDetailView is everything §13's trading screen needs about one market.
type MarketDetailView struct {
	Summary nativemarket.MarketSummary
	Limits  LimitsInForce
	Holders []nativemarket.Holding
}

// LimitsInForce is what §47 asks to be visible: the limits that would refuse a
// trade, from both documents that hold one.
//
// The market-safety policy is about the VENUE and always exists (a deployment
// that has recorded none runs the compiled-in conservative one). The risk
// kernel's policy is about an ACCOUNT and may genuinely be absent, in which
// case no internal trade can be evaluated at all -- so RiskPolicyVersion is
// empty and the two concentration limits are nil, which is the honest rendering
// of "this deployment has not decided".
type LimitsInForce struct {
	Safety                          nativemarket.SafetyPolicy
	RiskPolicyVersion               string
	MaxNativeMarketConcentrationBPS *money.BPS
	MaxCreatorConcentrationBPS      *money.BPS
}

// CandleView is one bounded window of a market's history plus the scales its
// numbers are in.
type CandleView struct {
	MarketID      nativemarket.MarketID
	Interval      nativemarket.Interval
	From          time.Time
	To            time.Time
	PriceScale    int
	AssetDecimals int
	Candles       []nativemarket.Candle
}

// TradeTapeView is a market's recent public prints.
type TradeTapeView struct {
	MarketID      nativemarket.MarketID
	PriceScale    int
	AssetDecimals int
	Prints        []nativemarket.Print
}

// PortfolioPort reads an account's Credit balance and native positions
// together, at one instant.
type PortfolioPort interface {
	Portfolio(ctx context.Context, accountID accounts.AccountID) (PortfolioView, error)
}

// PortfolioView is §15's answer, assembled at one instant.
//
// Credits comes from internal/credit, unchanged: this package does not
// re-derive a balance, and the breakdown it carries -- gross, spendable,
// frozen, payout-eligible, and the reasons for each gap -- is the same one
// GET /credits/balance returns, because it is literally the same call.
type PortfolioView struct {
	AccountID accounts.AccountID
	AsOf      time.Time
	// Temperature is what kind of value this deployment's Credits are: ECONOMY
	// normally, SIMULATED on a sandbox tier where nothing can move.
	Temperature activity.Temperature
	Credits     credit.Balances
	Positions   []PortfolioPositionView
	Totals      PortfolioTotals
}

// PortfolioPositionView is one position marked against its market.
type PortfolioPositionView struct {
	Valuation     nativemarket.PositionValuation
	Symbol        string
	Name          string
	AssetDecimals int
	Demo          bool
	Temperature   activity.Temperature
}

// PortfolioTotals sums the positions. Every figure is a sum of exact integers;
// nothing here is an average of an average.
type PortfolioTotals struct {
	CostBasisCredits    money.Quantity
	MarketValueCredits  money.Quantity
	UnrealizedPnLCredit money.Quantity
	RealizedPnLCredits  money.Quantity
	TotalPnLCredits     money.Quantity
	FeesPaidCredits     money.Quantity
	PositionCount       int
	OpenPositionCount   int
}

// ActivityFeedPort reads the unified timeline (§16).
//
// It is deliberately separate from ActivityPort, which serves
// GET /accounts/{id}/activity. See wiring_markets.go for why both exist.
type ActivityFeedPort interface {
	Feed(ctx context.Context, r activity.Request) (activity.Page, error)
}

// riskLimitsOf pulls the two native concentration limits out of a composed risk
// policy, for the "limits in force" disclosure.
func riskLimitsOf(p risk.Policy) LimitsInForce {
	out := LimitsInForce{RiskPolicyVersion: p.Version}
	out.MaxNativeMarketConcentrationBPS = p.MaxNativeMarketConcentrationBPS
	out.MaxCreatorConcentrationBPS = p.MaxCreatorConcentrationBPS
	return out
}
