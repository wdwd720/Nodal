package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/activity"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/nativeasset"
	"github.com/nodal/controlplane/internal/nativemarket"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// --- samples ----------------------------------------------------------------

func sampleMarketSummary() nativemarket.MarketSummary {
	return nativemarket.MarketSummary{
		MarketID: testMarketID(),
		AssetID:  testAssetID, CreditAssetID: testAssetID,
		Name: "Doggu", Symbol: "DG", Description: "a market",
		MarketStatus: nativemarket.StatusActive, AssetStatus: nativeasset.StatusActive,
		Moderation:       nativeasset.ModerationApproved,
		CreatorAccountID: testAccountID,
		Curve: nativemarket.Curve{
			VirtualCreditReserve: qty("30000000000"),
			InitialAssetReserve:  qty("900000000000000"),
		},
		State: nativemarket.State{
			RealCreditReserve: qty("2000000000"), AssetReserve: qty("880000000000000"), Version: 3,
		},
		Fees:              nativemarket.Fees{PlatformBPS: 100, CreatorBPS: 50},
		LastPrice:         qty("36363636363636"),
		PriceScale:        nativemarket.PriceScale,
		ReferencePrice24h: qty("33333333333333"),
		Change24hBPS:      909,
		HasChange24h:      true,
		CreditVolume24h:   qty("2000000000"),
		Trades24h:         3,
		LiquidityCredits:  qty("32000000000"),
		RealCreditReserve: qty("2000000000"),
		MaxSupply:         qty("1000000000000000"),
		CirculatingSupply: qty("20000000000000"),
		AssetDecimals:     6,
		Demo:              true,
		CreatedAt:         testNow,
	}
}

func sampleMarketDetail() MarketDetailView {
	return MarketDetailView{
		Summary: sampleMarketSummary(),
		Limits: LimitsInForce{
			Safety:                          nativemarket.ConservativeSafetyPolicy(),
			RiskPolicyVersion:               "global-test",
			MaxNativeMarketConcentrationBPS: func() *money.BPS { v := money.BPS(2000); return &v }(),
			MaxCreatorConcentrationBPS:      func() *money.BPS { v := money.BPS(3000); return &v }(),
		},
		Holders: []nativemarket.Holding{
			{Rank: 1, Quantity: qty("12345"), ShareBPS: 6_000},
			{Rank: 2, Quantity: qty("8000"), ShareBPS: 4_000, IsYou: true, AccountID: testAccountID},
		},
	}
}

func sampleCandles() CandleView {
	return CandleView{
		MarketID: testMarketID(),
		Interval: nativemarket.Interval1m,
		From:     testNow, To: testNow.Add(time.Hour),
		PriceScale: nativemarket.PriceScale, AssetDecimals: 6,
		Candles: []nativemarket.Candle{{
			OpenTime: testNow, Open: qty("100"), High: qty("140"), Low: qty("90"), Close: qty("120"),
			CreditVolume: qty("500"), AssetVolume: qty("7"), Trades: 2,
		}},
	}
}

func sampleTape() TradeTapeView {
	return TradeTapeView{
		MarketID:   testMarketID(),
		PriceScale: nativemarket.PriceScale, AssetDecimals: 6,
		Prints: []nativemarket.Print{{
			MarketID: testMarketID(),
			Seq:      2, Side: nativemarket.Sell, PriceScale: nativemarket.PriceScale,
			SpotBefore: qty("140"), SpotAfter: qty("120"), EffectivePrice: qty("130"),
			CreditVolume: qty("200"), AssetVolume: qty("2"), PrintedAt: testNow,
		}},
	}
}

func samplePortfolio() PortfolioView {
	pos := nativemarket.Position{
		AccountID: testAccountID, AssetID: testAssetID,
		Quantity: qty("1000"), CostBasisCredits: qty("2000"),
		RealizedPnLCredits: qty("-150"), FeesPaidCredits: qty("30"),
		UnitsBoughtTotal: qty("1400"), UnitsSoldTotal: qty("400"),
		UpdatedAt: testNow,
	}
	return PortfolioView{
		AccountID: testAccountID, AsOf: testNow,
		Temperature: activity.TemperatureSimulated,
		Credits: credit.Balances{
			Gross: qty("5000"), Spendable: qty("4000"), Frozen: qty("1000"),
			PayoutEligible: qty("0"), Ineligible: qty("5000"),
			ByOrigin:      map[valuedomain.CreditOrigin]money.Quantity{valuedomain.OriginPromotional: qty("5000")},
			ByFinality:    map[valuedomain.FundingFinality]money.Quantity{valuedomain.FinalityUnfunded: qty("5000")},
			PolicyVersion: "sandbox-1",
		},
		Positions: []PortfolioPositionView{{
			Valuation: nativemarket.PositionValuation{
				Position: pos, MarketID: testMarketID(),
				MarketStatus: nativemarket.StatusActive,
				SpotPrice:    qty("3000000000000000000"), PriceScale: nativemarket.PriceScale,
				MarketValue: qty("3000"), UnrealizedPnL: qty("1000"), TotalPnL: qty("850"),
				AsOf: testNow,
			},
			Symbol: "DG", Name: "Doggu", AssetDecimals: 6, Demo: true,
			Temperature: activity.TemperatureSimulated,
		}},
		Totals: PortfolioTotals{
			CostBasisCredits: qty("2000"), MarketValueCredits: qty("3000"),
			UnrealizedPnLCredit: qty("1000"), RealizedPnLCredits: qty("-150"),
			TotalPnLCredits: qty("850"), FeesPaidCredits: qty("30"),
			PositionCount: 1, OpenPositionCount: 1,
		},
	}
}

func sampleActivityFeed() activity.Page {
	return activity.Page{
		Items: []activity.Item{{
			ID: testOrderID.String(), Kind: activity.KindNativeTrade, OccurredAt: testNow,
			Summary: "Bought DG on the internal market", Status: "",
			Amounts: []activity.Amount{
				{Unit: activity.UnitCredits, Value: "2000", Origin: "", Temperature: activity.TemperatureSimulated},
				{Unit: activity.UnitAssetUnits, Value: "1400", Symbol: "DG", Temperature: activity.TemperatureSimulated},
			},
			Reference: activity.Reference{Type: "native_market_fill", ID: testOrderID.String()},
			Simulated: true,
		}},
		NextCursor: "next-page",
	}
}

// --- tests ------------------------------------------------------------------

// TestGetNativeMarkets_PassesEveryFilterThroughAndSaysWhetherPagingIsStable.
func TestGetNativeMarkets_PassesEveryFilterThroughAndSaysWhetherPagingIsStable(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	res := h.do(http.MethodGet,
		"/v1/native-markets?status=ACTIVE&status=CLOSE_ONLY&creator_account_id="+testAccountID.String()+
			"&q=dog&sort=VOLUME_24H&limit=7&cursor=abc", nil)
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())

	got := h.ports.marketData.lastList
	assert.Equal(t, []nativemarket.Status{nativemarket.StatusActive, nativemarket.StatusCloseOnly}, got.Statuses)
	// The creator filter is gone from this route: it is unauthenticated, and an
	// account id on it is both readable and enumerable (D-110). A caller that
	// sends one anyway gets the unfiltered list, because an unknown query
	// parameter is not a request this API answers differently.
	assert.True(t, got.Creator.IsZero(), "the public list must not take a creator filter")
	assert.Equal(t, "dog", got.Query)
	assert.Equal(t, nativemarket.SortVolume24h, got.Sort)
	assert.Equal(t, 7, got.Limit)
	assert.Equal(t, "abc", got.Cursor)

	var page api.NativeMarketPage
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &page))
	require.Len(t, page.Markets, 1)
	m := page.Markets[0]
	assert.Equal(t, "DG", m.Symbol)
	// The summary schema has no creator_account_id at all, so there is no
	// field here to assert against; the raw body is what proves it.
	assert.NotContains(t, res.Body.String(), "creator_account_id",
		"the public markets list must carry no account identity (D-110)")
	assert.Equal(t, "36363636363636", m.LastPrice)
	assert.Equal(t, nativemarket.PriceScale, m.PriceScale)
	assert.Equal(t, 6, m.AssetDecimals, "the asset's scale, not the Credit's (F-44)")
	require.NotNil(t, m.Has24hChange)
	assert.True(t, *m.Has24hChange)
	require.NotNil(t, m.Change24hBps)
	assert.Equal(t, int64(909), *m.Change24hBps)
	assert.True(t, m.Demo, "a demo market must say so")
	assert.Equal(t, "32000000000", m.LiquidityCredits)

	// A market that has not traded in the window reports no change rather than
	// a change of zero.
	quiet := sampleMarketSummary()
	quiet.HasChange24h = false
	quiet.Change24hBPS = 0
	h.ports.marketData.page = nativemarket.MarketPage{Markets: []nativemarket.MarketSummary{quiet}}
	res = h.do(http.MethodGet, "/v1/native-markets", nil)
	require.Equal(t, http.StatusOK, res.Code)
	// A fresh value: unmarshalling into the previous one would leave the
	// pointers it already had, and the absence being asserted here is the
	// whole point.
	var quietPage api.NativeMarketPage
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &quietPage))
	require.NotNil(t, quietPage.Markets[0].Has24hChange)
	assert.False(t, *quietPage.Markets[0].Has24hChange)
	assert.Nil(t, quietPage.Markets[0].Change24hBps)
	assert.False(t, quietPage.Stable, "only NEWEST claims stability")
}

// TestGetNativeMarketsMarketIdSummary_ReportsTheLimitsInForce is §47's
// disclosure: the limits that would refuse a trade are readable before it.
func TestGetNativeMarketsMarketIdSummary_ReportsTheLimitsInForce(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	res := h.do(http.MethodGet, "/v1/native-markets/"+testOrderID.String()+"/summary", nil)
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())

	var detail api.NativeMarketDetail
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &detail))
	assert.Equal(t, "DG", detail.Market.Symbol)
	assert.Equal(t, nativemarket.ConservativeSafetyVersion, detail.LimitsInForce.SafetyPolicyVersion)
	require.NotNil(t, detail.LimitsInForce.MaxPriceImpactBps)
	assert.Equal(t, 9_000, *detail.LimitsInForce.MaxPriceImpactBps)
	require.NotNil(t, detail.LimitsInForce.RiskPolicyVersion)
	assert.Equal(t, "global-test", *detail.LimitsInForce.RiskPolicyVersion)
	require.NotNil(t, detail.LimitsInForce.MaxNativeMarketConcentrationBps)
	assert.Equal(t, 2000, *detail.LimitsInForce.MaxNativeMarketConcentrationBps)
	require.NotNil(t, detail.TopHolders)
	require.Len(t, *detail.TopHolders, 2)
	// The creator is on the gated detail read and nowhere else (D-110).
	require.NotNil(t, detail.CreatorAccountId)
	assert.Equal(t, testAccountID.String(), detail.CreatorAccountId.String())

	// A deployment with no risk policy reports its absence rather than a zero
	// limit, which would say the opposite.
	none := sampleMarketDetail()
	none.Limits.RiskPolicyVersion = ""
	none.Limits.MaxNativeMarketConcentrationBPS = nil
	none.Limits.MaxCreatorConcentrationBPS = nil
	h.ports.marketData.detail = none
	res = h.do(http.MethodGet, "/v1/native-markets/"+testOrderID.String()+"/summary", nil)
	require.Equal(t, http.StatusOK, res.Code)
	var without api.NativeMarketDetail
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &without))
	assert.Nil(t, without.LimitsInForce.RiskPolicyVersion)
	assert.Nil(t, without.LimitsInForce.MaxNativeMarketConcentrationBps)
}

// TestGetCandles_RequiresItsWindowAndCarriesItsScales.
func TestGetCandles_RequiresItsWindowAndCarriesItsScales(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	from := testNow.UTC().Format(time.RFC3339)
	to := testNow.Add(time.Hour).UTC().Format(time.RFC3339)
	res := h.do(http.MethodGet,
		"/v1/native-markets/"+testOrderID.String()+"/candles?interval=1m&from="+from+"&to="+to, nil)
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	assert.Equal(t, nativemarket.Interval1m, h.ports.marketData.lastCand.Interval)

	var page api.NativeCandlePage
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &page))
	require.Len(t, page.Candles, 1)
	assert.Equal(t, "100", page.Candles[0].Open)
	assert.Equal(t, "140", page.Candles[0].High)
	assert.Equal(t, "90", page.Candles[0].Low)
	assert.Equal(t, "120", page.Candles[0].Close)
	assert.Equal(t, int64(2), page.Candles[0].Trades)
	assert.Equal(t, nativemarket.PriceScale, page.PriceScale)
	assert.Equal(t, 6, page.AssetDecimals)

	// No window is a validation error, not an unbounded scan.
	res = h.do(http.MethodGet, "/v1/native-markets/"+testOrderID.String()+"/candles?interval=1m", nil)
	assert.Equal(t, http.StatusBadRequest, res.Code)

	// A market with no history returns an empty list rather than invented
	// candles (§14's honest empty state).
	h.ports.marketData.candles = CandleView{
		MarketID: sampleCandles().MarketID, Interval: nativemarket.Interval1m,
		From: testNow, To: testNow.Add(time.Hour),
		PriceScale: nativemarket.PriceScale, AssetDecimals: 6,
	}
	res = h.do(http.MethodGet,
		"/v1/native-markets/"+testOrderID.String()+"/candles?interval=1m&from="+from+"&to="+to, nil)
	require.Equal(t, http.StatusOK, res.Code)
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &page))
	assert.Empty(t, page.Candles)
}

// TestGetTrades_TheTapeCarriesNoAccount.
func TestGetTrades_TheTapeCarriesNoAccount(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	res := h.do(http.MethodGet, "/v1/native-markets/"+testOrderID.String()+"/trades?limit=10", nil)
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	assert.NotContains(t, res.Body.String(), "account",
		"a public tape must not carry an account identity in any field")

	var page api.NativeTradePage
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &page))
	require.Len(t, page.Trades, 1)
	assert.Equal(t, api.NativeTradePrintSide("SELL"), page.Trades[0].Side)
	assert.Equal(t, "130", page.Trades[0].EffectivePrice)
}

// TestGetNativeMarketsMarketIdSummary_NamesNoHolder.
//
// top_holders rendered {account_id, quantity} to any caller with
// native_asset:read, which every customer role has, so any signed-in stranger
// could read a market's largest positions by account and watch them move trade
// by trade (F-197). A row is now a rank, a quantity and a share; the one row a
// caller may see marked is their own, and even that carries no id, because a
// field that is sometimes an identity is one a client will render as one.
func TestGetNativeMarketsMarketIdSummary_NamesNoHolder(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	res := h.do(http.MethodGet, "/v1/native-markets/"+testOrderID.String()+"/summary", nil)
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	assert.True(t, h.ports.marketData.lastCaller.IsZero(),
		"a request that named no account marks no row")

	// The rendered holder list, byte for byte: the assertion is about what goes
	// on the wire, because a struct with an unset field still has the field.
	var body struct {
		TopHolders json.RawMessage `json:"top_holders"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &body))
	assert.NotContains(t, string(body.TopHolders), "account_id",
		"the holder list names nobody, not even the caller")

	var detail api.NativeMarketDetail
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &detail))
	require.NotNil(t, detail.TopHolders)
	rows := *detail.TopHolders
	require.Len(t, rows, 2)
	assert.Equal(t, 1, rows[0].Rank)
	assert.Equal(t, "12345", rows[0].Quantity)
	assert.Equal(t, 6_000, rows[0].ShareBps)
	assert.Nil(t, rows[0].IsYou, "somebody else's row is not marked at all")
	require.NotNil(t, rows[1].IsYou)
	assert.True(t, *rows[1].IsYou)

	// Naming an account the caller owns marks their row and nothing else.
	res = h.do(http.MethodGet,
		"/v1/native-markets/"+testOrderID.String()+"/summary?account_id="+testAccountID.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	assert.Equal(t, testAccountID, h.ports.marketData.lastCaller)

	// Naming somebody else's account is refused, exactly as it is everywhere
	// else: this parameter is a claim about who the caller is.
	other := accounts.NewAccountID()
	res = h.do(http.MethodGet,
		"/v1/native-markets/"+testOrderID.String()+"/summary?account_id="+other.String(), nil)
	assert.Equal(t, http.StatusForbidden, res.Code)
}

// TestGetMePortfolio_StatesItsAsOfAndItsTemperatures is §15 and §46 together:
// one instant for every mark, the credit service's own balance breakdown, and
// P&L that keeps its sign.
func TestGetMePortfolio_StatesItsAsOfAndItsTemperatures(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	res := h.do(http.MethodGet, "/v1/me/portfolio?account_id="+testAccountID.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())

	var p api.Portfolio
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &p))
	assert.Equal(t, testNow.UTC(), p.AsOf.UTC(), "every mark must state the instant it was true")
	assert.Equal(t, api.ValueTemperature("SIMULATED"), p.Temperature)

	// The Credit balance is the credit service's, with its four figures and
	// its policy version -- not a number this handler derived.
	assert.Equal(t, "5000", p.Credits.Gross)
	assert.Equal(t, "4000", p.Credits.Spendable)
	assert.Equal(t, "1000", p.Credits.Frozen)
	assert.Equal(t, "0", p.Credits.PayoutEligible)
	assert.Equal(t, "sandbox-1", p.Credits.PolicyVersion)

	require.Len(t, p.Positions, 1)
	pos := p.Positions[0]
	assert.Equal(t, "DG", pos.Symbol)
	assert.Equal(t, "1000", pos.Quantity)
	assert.Equal(t, "2000", pos.CostBasisCredits)
	assert.Equal(t, "3000", pos.MarketValueCredits)
	assert.Equal(t, "1000", pos.UnrealizedPnlCredits)
	assert.Equal(t, "-150", pos.RealizedPnlCredits, "a loss keeps its sign")
	assert.Equal(t, "850", pos.TotalPnlCredits)
	require.NotNil(t, pos.AverageCostCredits)
	assert.Equal(t, "2000000000000000000", *pos.AverageCostCredits, "2 Credits a unit at eighteen places")
	require.NotNil(t, pos.Demo)
	assert.True(t, *pos.Demo)
	assert.Equal(t, api.ValueTemperature("SIMULATED"), pos.Temperature)

	assert.Equal(t, "-150", p.Totals.RealizedPnlCredits)
	assert.Equal(t, 1, p.Totals.OpenPositionCount)

	// A closed position reports no average cost rather than a cost of nothing.
	closed := samplePortfolio()
	closed.Positions[0].Valuation.Position.Quantity = qty("0")
	closed.Positions[0].Valuation.Position.CostBasisCredits = qty("0")
	h.ports.portfolio.view = closed
	res = h.do(http.MethodGet, "/v1/me/portfolio?account_id="+testAccountID.String(), nil)
	require.Equal(t, http.StatusOK, res.Code)
	var closedOut api.Portfolio
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &closedOut))
	assert.Nil(t, closedOut.Positions[0].AverageCostCredits)
}

// TestGetMePortfolio_IsScopedToItsOwner: another account's portfolio is not
// readable, and the refusal does not say whether it exists.
func TestGetMePortfolio_IsScopedToItsOwner(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	other := accounts.NewAccountID()
	res := h.do(http.MethodGet, "/v1/me/portfolio?account_id="+other.String(), nil)
	assert.Equal(t, http.StatusForbidden, res.Code)

	res = h.do(http.MethodGet, "/v1/me/activity?account_id="+other.String(), nil)
	assert.Equal(t, http.StatusForbidden, res.Code)
}

// TestGetMeActivity_RendersAmountsWithTheirUnitAndTemperature.
func TestGetMeActivity_RendersAmountsWithTheirUnitAndTemperature(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	res := h.do(http.MethodGet,
		"/v1/me/activity?account_id="+testAccountID.String()+"&kind=NATIVE_TRADE&kind=CREDIT_PURCHASE", nil)
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	assert.Equal(t,
		[]activity.Kind{activity.KindNativeTrade, activity.KindCreditPurchase},
		h.ports.activityFeed.lastKind)

	var page api.ActivityFeedPage
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &page))
	require.Len(t, page.Items, 1)
	it := page.Items[0]
	assert.Equal(t, api.ActivityFeedKind("NATIVE_TRADE"), it.Kind)
	assert.Equal(t, "Bought DG on the internal market", it.Summary)
	assert.True(t, it.Simulated)
	assert.Equal(t, "native_market_fill", it.Reference.Type)
	require.Len(t, it.Amounts, 2)
	assert.Equal(t, api.ActivityAmountUnit("CREDITS"), it.Amounts[0].Unit)
	assert.Equal(t, "2000", it.Amounts[0].Value)
	assert.Equal(t, api.ValueTemperature("SIMULATED"), it.Amounts[0].Temperature)
	assert.Equal(t, api.ActivityAmountUnit("ASSET_UNITS"), it.Amounts[1].Unit)
	require.NotNil(t, it.Amounts[1].Symbol)
	assert.Equal(t, "DG", *it.Amounts[1].Symbol)
	next, err := page.NextCursor.Get()
	require.NoError(t, err)
	assert.Equal(t, "next-page", next, "the continuation must reach the client")
}

// TestMarketSurfaces_AnswerUnsupportedWhenTheyAreNotWired: a deployment that
// has not provisioned the internal economy says so, rather than returning an
// empty markets page a browser would render as "there are no markets".
func TestMarketSurfaces_AnswerUnsupportedWhenTheyAreNotWired(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.server.opts.Ports.MarketData = nil
	h.server.opts.Ports.Portfolio = nil
	h.server.opts.Ports.ActivityFeed = nil

	for _, path := range []string{
		"/v1/native-markets",
		"/v1/native-markets/" + testOrderID.String() + "/summary",
		"/v1/native-markets/" + testOrderID.String() + "/trades",
		"/v1/me/portfolio?account_id=" + testAccountID.String(),
		"/v1/me/activity?account_id=" + testAccountID.String(),
	} {
		res := h.do(http.MethodGet, path, nil)
		require.Equal(t, http.StatusUnprocessableEntity, res.Code, "%s: %s", path, res.Body.String())
		assert.Equal(t, errs.CodeUnsupported, res.problem().Code)
	}
}

// testMarketID is the market the harness's fakes describe. It reuses the
// harness's order id so mountedRoutes' {marketId} substitution reaches it.
func testMarketID() nativemarket.MarketID {
	id, err := nativemarket.ParseMarketID(testOrderID.String())
	if err != nil {
		panic(err)
	}
	return id
}
