//go:build integration

package nativemarket

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/nativeasset"
	"github.com/nodal/controlplane/internal/security"
)

// Positions, prints, candles, discovery and market safety, against real SQL.
//
// Everything here drives the engine rather than writing rows: the point of the
// read models is that only the database writes them, so a test that inserted
// one would be testing nothing.

// TestIntegration_PositionsAgreeWithTheGoStatementOfTheArithmetic drives a
// sequence of real fills and compares the rows migration 00772's triggers wrote
// against the same arithmetic written in Go.
//
// Two independent statements of the same thing, in different languages, that
// must agree. Either alone proves only that it agrees with itself.
func TestIntegration_PositionsAgreeWithTheGoStatementOfTheArithmetic(t *testing.T) {
	f := newFixture(t)
	trader := f.trader

	want := Position{AccountID: trader, AssetID: f.asset.AssetID}

	// Buy, buy, sell part, sell the rest. The partial sell is the case the
	// rounding rule is about.
	b1, err := f.buy(trader, 2_000_000_000, money.Quantity{})
	require.NoError(t, err)
	want = want.ApplyBuy(b1.Fill.AssetsOut, b1.Fill.CreditsIn,
		b1.Fill.PlatformFee.Add(b1.Fill.CreatorFee), f.clk.Now())
	f.requirePositionIs(t, trader, want)

	b2, err := f.buy(trader, 1_500_000_000, money.Quantity{})
	require.NoError(t, err)
	want = want.ApplyBuy(b2.Fill.AssetsOut, b2.Fill.CreditsIn,
		b2.Fill.PlatformFee.Add(b2.Fill.CreatorFee), f.clk.Now())
	f.requirePositionIs(t, trader, want)

	third, err := want.Quantity.Div(money.QuantityFromInt64(3), money.RoundDown)
	require.NoError(t, err)
	require.True(t, third.IsPositive())
	s1, err := f.sell(trader, third, money.Quantity{})
	require.NoError(t, err)
	want, err = want.ApplySell(s1.Fill.AssetsIn, s1.Fill.CreditsOut,
		s1.Fill.PlatformFee.Add(s1.Fill.CreatorFee), f.clk.Now())
	require.NoError(t, err)
	f.requirePositionIs(t, trader, want)

	s2, err := f.sell(trader, want.Quantity, money.Quantity{})
	require.NoError(t, err)
	want, err = want.ApplySell(s2.Fill.AssetsIn, s2.Fill.CreditsOut,
		s2.Fill.PlatformFee.Add(s2.Fill.CreatorFee), f.clk.Now())
	require.NoError(t, err)
	f.requirePositionIs(t, trader, want)

	got := f.position(t, trader)
	assert.True(t, got.Quantity.IsZero(), "the position is closed")
	assert.True(t, got.CostBasisCredits.IsZero(),
		"a closed position carries no basis; 00772's CHECK refuses one that does")
	assert.True(t, got.RealizedPnLCredits.IsNegative(),
		"a round trip through two fees cannot be profitable at an unchanged curve")
}

// TestIntegration_TheCreatorAllocationIsPartOfTheirPosition: a creator who
// never traded still holds what the mint gave them, at zero cost, and the
// portfolio agrees with the ledger about it.
func TestIntegration_TheCreatorAllocationIsPartOfTheirPosition(t *testing.T) {
	f := newFixture(t)
	p := f.position(t, f.creator)

	require.Equal(t, f.asset.Supply.CreatorAllocation.String(), p.Quantity.String())
	assert.Equal(t, f.asset.Supply.CreatorAllocation.String(), p.AllocationUnits.String())
	assert.True(t, p.CostBasisCredits.IsZero(), "nothing was paid for a grant")
	assert.Equal(t, int64(0), p.FillCount)
	assert.True(t, p.HoldsInvariant())

	assert.Equal(t, f.balance(f.creator, f.asset.AssetID, ledger.CodeNativeAssetBalance).String(),
		p.Quantity.String(), "the position must equal the ledger's own projection")
}

// TestIntegration_PositionsReconcileAgainstTheLedger is the proof the read
// model is a read model: the database function compares every row against
// ledger_balances, which neither the table nor its triggers write.
func TestIntegration_PositionsReconcileAgainstTheLedger(t *testing.T) {
	f := newFixture(t)
	_, err := f.buy(f.trader, 1_000_000_000, money.Quantity{})
	require.NoError(t, err)
	_, err = f.buy(newAccountFundedBy(t, f), 500_000_000, money.Quantity{})
	require.NoError(t, err)

	bad, err := f.svc.VerifyPositions(f.ctx, testDB)
	require.NoError(t, err)
	assert.Empty(t, bad, "every native position must equal the ledger balance behind it: %+v", bad)
}

// TestIntegration_APositionCannotBeWrittenByTheApplication: cp_app has SELECT
// on native_positions and nothing else, so there is no path by which a cost
// basis can be set to something the trades do not support.
func TestIntegration_APositionCannotBeWrittenByTheApplication(t *testing.T) {
	f := newFixture(t)
	_, err := f.buy(f.trader, 1_000_000_000, money.Quantity{})
	require.NoError(t, err)

	_, err = testDB.Exec(f.ctx,
		`UPDATE native_positions SET cost_basis_credits = 1 WHERE account_id = $1`, f.trader)
	require.Error(t, err, "the application must not be able to rewrite a cost basis")

	_, err = testDB.Exec(f.ctx,
		`INSERT INTO native_positions (account_id, asset_id, quantity) VALUES ($1,$2,1)`,
		f.trader, f.asset.AssetID)
	require.Error(t, err, "the application must not be able to invent a position")
}

// TestIntegration_APrintThatDisagreesWithItsFillIsRefused: migration 00771
// recomputes every price from the fill and refuses a print that says the market
// traded somewhere it did not.
func TestIntegration_APrintThatDisagreesWithItsFillIsRefused(t *testing.T) {
	f := newFixture(t)
	res, err := f.buy(f.trader, 1_000_000_000, money.Quantity{})
	require.NoError(t, err)

	var printed struct {
		before, after, effective, creditVol, assetVol string
		seq                                           int64
	}
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT spot_price_before::text, spot_price_after::text, effective_price::text,
		        credit_volume::text, asset_volume::text, seq
		   FROM native_market_prints WHERE fill_id = $1`, res.FillID).
		Scan(&printed.before, &printed.after, &printed.effective, &printed.creditVol, &printed.assetVol, &printed.seq))

	assert.Equal(t, res.Fill.SpotBefore.String(), printed.before)
	assert.Equal(t, res.Fill.SpotAfter.String(), printed.after)
	assert.Equal(t, res.Fill.EffectivePrice.String(), printed.effective)
	assert.Equal(t, res.Fill.CreditsIn.String(), printed.creditVol)
	assert.Equal(t, res.Fill.AssetsOut.String(), printed.assetVol)

	// A print for the next fill that claims a price its own fill does not
	// support is refused by the database, whoever inserts it.
	second, err := f.buy(f.trader, 500_000_000, money.Quantity{})
	require.NoError(t, err)
	_, err = testDB.Exec(f.ctx, `DELETE FROM native_market_prints WHERE fill_id = $1`, second.FillID)
	require.Error(t, err, "a print is immutable")

	var count int
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT count(*) FROM native_market_prints WHERE market_id = $1`, f.market.ID).Scan(&count))
	assert.Equal(t, 2, count, "one print per fill, and only a fill makes one")
}

// TestIntegration_CandlesMatchHandComputedFixtures: three trades in one minute
// and a fourth in the next, checked against OHLCV worked out by hand from the
// fills the engine produced.
func TestIntegration_CandlesMatchHandComputedFixtures(t *testing.T) {
	f := newFixture(t)
	start := f.clk.Now().UTC().Truncate(time.Minute)
	f.clk.Set(start.Add(10 * time.Second))

	a, err := f.buy(f.trader, 1_000_000_000, money.Quantity{})
	require.NoError(t, err)
	f.clk.Advance(10 * time.Second)
	b, err := f.buy(f.trader, 500_000_000, money.Quantity{})
	require.NoError(t, err)
	f.clk.Advance(10 * time.Second)
	c, err := f.sell(f.trader, a.Fill.AssetsOut, money.Quantity{})
	require.NoError(t, err)

	// The next minute.
	f.clk.Set(start.Add(time.Minute + 10*time.Second))
	d, err := f.buy(f.trader, 250_000_000, money.Quantity{})
	require.NoError(t, err)

	candles, err := f.svc.Candles(f.ctx, testDB, CandleRequest{
		MarketID: f.market.ID, Interval: Interval1m,
		From: start, To: start.Add(5 * time.Minute),
	})
	require.NoError(t, err)
	require.Len(t, candles, 2, "two minutes traded, so two candles and no filled-forward gaps")

	first := candles[0]
	assert.Equal(t, start, first.OpenTime)
	assert.Equal(t, int64(3), first.Trades)
	// Open is the pre-trade spot of the first print; close the post-trade spot
	// of the last.
	assert.Equal(t, a.Fill.SpotBefore.String(), first.Open.String())
	assert.Equal(t, c.Fill.SpotAfter.String(), first.Close.String())
	// The high is the highest spot the market touched in the bucket, which is
	// after the second buy. The low is where the bucket OPENED -- two buys and
	// a partial sell leave the price above where it started -- which is what
	// looking at both sides of every print is for: a candle built from trade
	// prices alone would have missed it.
	assert.Equal(t, b.Fill.SpotAfter.String(), first.High.String())
	assert.Equal(t, a.Fill.SpotBefore.String(), first.Low.String())
	assert.Equal(t, first.Open.String(), first.Low.String())
	assert.Greater(t, first.Close.BigInt().Cmp(first.Low.BigInt()), 0,
		"the sell gave back part of the move, not all of it")
	// Volume is the GROSS Credit side of every trade, whichever way it went.
	wantVolume := a.Fill.CreditsIn.Add(b.Fill.CreditsIn).Add(c.Fill.CreditsToPool)
	assert.Equal(t, wantVolume.String(), first.CreditVolume.String())
	wantUnits := a.Fill.AssetsOut.Add(b.Fill.AssetsOut).Add(c.Fill.AssetsIn)
	assert.Equal(t, wantUnits.String(), first.AssetVolume.String())

	second := candles[1]
	assert.Equal(t, start.Add(time.Minute), second.OpenTime)
	assert.Equal(t, int64(1), second.Trades)
	assert.Equal(t, d.Fill.SpotBefore.String(), second.Open.String())
	assert.Equal(t, d.Fill.SpotAfter.String(), second.Close.String())

	// A wider interval folds them into one bucket with the same open and close.
	hourly, err := f.svc.Candles(f.ctx, testDB, CandleRequest{
		MarketID: f.market.ID, Interval: Interval1h,
		From: start.Truncate(time.Hour), To: start.Truncate(time.Hour).Add(time.Hour),
	})
	require.NoError(t, err)
	require.Len(t, hourly, 1)
	assert.Equal(t, first.Open.String(), hourly[0].Open.String())
	assert.Equal(t, second.Close.String(), hourly[0].Close.String())
	assert.Equal(t, int64(4), hourly[0].Trades)
	assert.Equal(t, wantVolume.Add(d.Fill.CreditsIn).String(), hourly[0].CreditVolume.String())

	// A window before the market traded is EMPTY, not flat: §14 asks for an
	// honest empty state rather than an invented candle.
	empty, err := f.svc.Candles(f.ctx, testDB, CandleRequest{
		MarketID: f.market.ID, Interval: Interval1m,
		From: start.Add(-time.Hour), To: start.Add(-time.Minute),
	})
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// TestIntegration_TheTapeCarriesNoAccountIdentity.
func TestIntegration_TheTapeCarriesNoAccountIdentity(t *testing.T) {
	f := newFixture(t)
	buy, err := f.buy(f.trader, 1_000_000_000, money.Quantity{})
	require.NoError(t, err)
	f.clk.Advance(time.Second)
	sell, err := f.sell(f.trader, buy.Fill.AssetsOut, money.Quantity{})
	require.NoError(t, err)

	prints, err := f.svc.RecentPrints(f.ctx, testDB, f.market.ID, 10)
	require.NoError(t, err)
	require.Len(t, prints, 2)
	assert.Equal(t, sell.FillID, prints[0].FillID, "newest first")
	assert.Equal(t, Sell, prints[0].Side)
	assert.Equal(t, Buy, prints[1].Side)
	assert.Equal(t, PriceScale, prints[0].PriceScale)

	// The tape's type has nowhere to put an account, which is the guarantee.
	// Asserted over the type rather than over one value, so a field added
	// later cannot slip past it.
	pt := reflect.TypeOf(Print{})
	for i := 0; i < pt.NumField(); i++ {
		name := strings.ToLower(pt.Field(i).Name)
		assert.NotContains(t, name, "account",
			"the public tape must carry no account identity; Print.%s does", pt.Field(i).Name)
	}

	// The limit is bounded whatever a caller asks for.
	many, err := f.svc.RecentPrints(f.ctx, testDB, f.market.ID, 10_000)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(many), MaxPrints)
}

// TestIntegration_MarketDiscoveryFiltersSortsAndSearches.
func TestIntegration_MarketDiscoveryFiltersSortsAndSearches(t *testing.T) {
	f := newFixture(t)
	_, err := f.buy(f.trader, 2_000_000_000, money.Quantity{})
	require.NoError(t, err)

	page, err := f.svc.ListMarkets(f.ctx, testDB, ListRequest{
		Statuses: []Status{StatusActive}, Limit: 50,
	})
	require.NoError(t, err)
	require.True(t, page.Stable, "NEWEST is the stable ordering")
	mine := findMarket(t, page.Markets, f.market.ID)

	assert.Equal(t, f.asset.Symbol, mine.Symbol)
	assert.Equal(t, f.asset.Name, mine.Name)
	assert.Equal(t, f.creator, mine.CreatorAccountID)
	assert.Equal(t, StatusActive, mine.MarketStatus)
	assert.Equal(t, int64(1), mine.Trades24h)
	assert.Equal(t, "2000000000", mine.CreditVolume24h.String())
	assert.True(t, mine.HasChange24h, "the market traded in the window")
	assert.Positive(t, int(mine.Change24hBPS), "a buy raises the price")
	assert.Equal(t, mine.Curve.VirtualCreditReserve.Add(mine.State.RealCreditReserve).String(),
		mine.LiquidityCredits.String())
	assert.Equal(t, mine.Curve.InitialAssetReserve.Sub(mine.State.AssetReserve).String(),
		mine.CirculatingSupply.String())
	assert.False(t, mine.Demo, "a fixture market is not demo data")
	assert.Equal(t, uint8(6), mine.AssetDecimals)

	// The detail read and the list row are the same projection.
	detail, err := f.svc.MarketSummaryByID(f.ctx, testDB, f.market.ID)
	require.NoError(t, err)
	assert.Equal(t, mine.LastPrice.String(), detail.LastPrice.String())
	assert.Equal(t, mine.CreditVolume24h.String(), detail.CreditVolume24h.String())
	byAsset, err := f.svc.MarketSummaryByAsset(f.ctx, testDB, f.asset.AssetID)
	require.NoError(t, err)
	assert.Equal(t, f.market.ID, byAsset.MarketID)

	// Search finds it by symbol, by a word in its name and by a word in its
	// description, and does not find it by something it is not.
	for _, term := range []string{f.asset.Symbol, "Doggu", "test"} {
		found, ferr := f.svc.ListMarkets(f.ctx, testDB, ListRequest{Query: term, Limit: 50})
		require.NoError(t, ferr, "search %q", term)
		findMarket(t, found.Markets, f.market.ID)
	}
	none, err := f.svc.ListMarkets(f.ctx, testDB, ListRequest{Query: "zzzznotamarket", Limit: 50})
	require.NoError(t, err)
	assert.Empty(t, none.Markets)

	// A wildcard is not a wildcard.
	wild, err := f.svc.ListMarkets(f.ctx, testDB, ListRequest{Query: "%", Limit: 50})
	require.NoError(t, err)
	assert.Empty(t, wild.Markets, "a LIKE metacharacter must not match every market")

	// Filtering by creator narrows to that creator.
	byCreator, err := f.svc.ListMarkets(f.ctx, testDB, ListRequest{Creator: f.creator, Limit: 50})
	require.NoError(t, err)
	findMarket(t, byCreator.Markets, f.market.ID)
	for _, m := range byCreator.Markets {
		assert.Equal(t, f.creator, m.CreatorAccountID)
	}
	other, err := f.svc.ListMarkets(f.ctx, testDB, ListRequest{Creator: f.trader, Limit: 50})
	require.NoError(t, err)
	assert.Empty(t, other.Markets)

	// A status nobody's market is in returns nothing rather than everything.
	delisted, err := f.svc.ListMarkets(f.ctx, testDB, ListRequest{Statuses: []Status{StatusDelisted}, Limit: 50})
	require.NoError(t, err)
	for _, m := range delisted.Markets {
		assert.Equal(t, StatusDelisted, m.MarketStatus)
	}

	// Every sort runs and orders by its own key.
	for _, sort := range AllSorts() {
		p, serr := f.svc.ListMarkets(f.ctx, testDB, ListRequest{Sort: sort, Limit: 50})
		require.NoError(t, serr, "sort %s", sort)
		assert.Equal(t, sort.Stable(), p.Stable)
	}
	_, err = f.svc.ListMarkets(f.ctx, testDB, ListRequest{Sort: "BY_VIBES"})
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

// TestIntegration_PagingNewestSeesEveryMarketExactlyOnce.
func TestIntegration_PagingNewestSeesEveryMarketExactlyOnce(t *testing.T) {
	f := newFixture(t)
	// Five more markets, so there are at least six to page through.
	for i := 0; i < 5; i++ {
		f.clk.Advance(time.Second)
		newMarketFor(t, f)
	}

	seen := map[string]int{}
	cursor := ""
	pages := 0
	for {
		page, err := f.svc.ListMarkets(f.ctx, testDB, ListRequest{Sort: SortNewest, Limit: 2, Cursor: cursor})
		require.NoError(t, err)
		require.True(t, page.Stable)
		for _, m := range page.Markets {
			seen[m.MarketID.String()]++
		}
		pages++
		require.Less(t, pages, 100, "paging did not terminate")
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		assert.LessOrEqual(t, len(page.Markets), 2, "a page must not exceed the limit it was asked for")
	}
	require.GreaterOrEqual(t, len(seen), 6)
	for id, n := range seen {
		assert.Equal(t, 1, n, "market %s was returned %d times while paging a stable ordering", id, n)
	}

	// A tampered cursor is refused rather than silently restarting.
	_, err := f.svc.ListMarkets(f.ctx, testDB, ListRequest{Sort: SortNewest, Cursor: "not-a-cursor"})
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

// TestIntegration_TheSafetyPolicyRefusesWhatItSaysItRefuses drives the limits
// through the real engine, at the boundary.
func TestIntegration_TheSafetyPolicyRefusesWhatItSaysItRefuses(t *testing.T) {
	f := newFixture(t)

	// A market cannot open below the liquidity floor.
	floor := *ConservativeSafetyPolicy().MinOpeningLiquidityCredits
	err := testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		a := newDraftAsset(t, f, tx)
		_, cerr := f.svc.Create(ctx, tx, CreateRequest{
			AssetID: a, CreditAssetID: f.creditAsset, CreatorID: f.creator,
			PoolSupply:           qs("1000000000000000"),
			VirtualCreditReserve: floor.Sub(q(1)),
			IdempotencyKey:       "thin-" + uuid.NewString(), EffectiveAt: f.clk.Now(),
		})
		return cerr
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	// An order that would move the price further than the policy permits is
	// refused, and a smaller one is not -- which is what the message says.
	strict := ConservativeSafetyPolicy()
	strict.MaxPriceImpactBPS = bpsPtr(100)
	strict.Version = "test-strict-impact"
	f.svc.SetSafety(fixedSafety{p: strict})
	t.Cleanup(func() { f.svc.SetSafety(nil) })

	_, err = f.buy(f.trader, 2_000_000_000, money.Quantity{})
	require.Error(t, err)
	assert.Equal(t, errs.CodeVenueLiquidityInsufficient, errs.CodeOf(err))

	small, err := f.buy(f.trader, 1_000_000, money.Quantity{})
	require.NoError(t, err, "a smaller order moves the market less, which is the remedy the refusal names")
	require.True(t, small.Fill.AssetsOut.IsPositive())

	// A SELL is never refused for its own price impact: refusing an exit traps
	// a holder.
	f.svc.SetSafety(fixedSafety{p: func() SafetyPolicy {
		p := ConservativeSafetyPolicy()
		p.MaxPriceImpactBPS = bpsPtr(0)
		p.MaxSlippageBPS = bpsPtr(0)
		p.Version = "test-refuse-everything"
		return p
	}()})
	_, err = f.sell(f.trader, small.Fill.AssetsOut, money.Quantity{})
	require.NoError(t, err, "a holder must always be able to leave")

	// And a creator self-buy is refused only under a policy that says so.
	f.svc.SetSafety(fixedSafety{p: func() SafetyPolicy {
		p := ConservativeSafetyPolicy()
		no := false
		p.CreatorMayBuyOwnAsset = &no
		p.Version = "test-no-creator-buys"
		return p
	}()})
	f.fund(f.creator, 10_000_000_000)
	_, err = f.buy(f.creator, 100_000_000, money.Quantity{})
	require.Error(t, err)
	assert.Equal(t, errs.CodeAssetRestricted, errs.CodeOf(err))
}

// TestIntegration_TheCircuitBreakerPausesToCloseOnly: a move past the limit
// inside the window stands as a trade and stops the next one, and the pause is
// CLOSE_ONLY so holders can still leave.
func TestIntegration_TheCircuitBreakerPausesToCloseOnly(t *testing.T) {
	f := newFixture(t)
	p := ConservativeSafetyPolicy()
	p.CircuitBreakerMoveBPS = bpsPtr(500) // half a percent, so one ordinary buy trips it
	p.Version = "test-hair-trigger"
	f.svc.SetSafety(fixedSafety{p: p})
	t.Cleanup(func() { f.svc.SetSafety(nil) })

	res, err := f.buy(f.trader, 2_000_000_000, money.Quantity{})
	require.NoError(t, err, "the trade that trips the breaker still stands: it was legal when it was priced")
	require.NotNil(t, res.Breaker, "the trade moved the market past the limit and must have tripped it")
	assert.Equal(t, "test-hair-trigger", res.Breaker.PolicyVersion)
	assert.Greater(t, int(res.Breaker.MoveBPS), 500)
	assert.Equal(t, 300, res.Breaker.WindowSeconds)

	m, err := f.svc.Market(f.ctx, testDB, f.market.ID)
	require.NoError(t, err)
	require.Equal(t, StatusCloseOnly, m.Status, "the pause lets holders out and stops new exposure")

	// The next BUY is refused by the market's own status.
	_, err = f.buy(f.trader, 1_000_000, money.Quantity{})
	require.Error(t, err)
	assert.Equal(t, errs.CodeAssetRestricted, errs.CodeOf(err))

	// A SELL still works.
	_, err = f.sell(f.trader, res.Fill.AssetsOut, money.Quantity{})
	require.NoError(t, err)

	// The trip is evidence: which policy, which window, which two prices.
	var events int
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT count(*) FROM native_market_breaker_events WHERE market_id = $1`, f.market.ID).Scan(&events))
	assert.GreaterOrEqual(t, events, 1)

	// And it is a recorded status transition like any other, with the system
	// named as the actor.
	var actorType, actorID, reason string
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT actor_type, actor_id, reason FROM native_market_transitions
		  WHERE market_id = $1 AND to_status = 'CLOSE_ONLY'
		  ORDER BY occurred_at DESC LIMIT 1`, f.market.ID).Scan(&actorType, &actorID, &reason))
	assert.Equal(t, string(security.ActorSystem), actorType)
	assert.Equal(t, "market:circuit-breaker", actorID)
	assert.Contains(t, reason, "circuit breaker")

	// An operator resuming is a second, separate decision the table permits.
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			_, serr := f.svc.SetStatus(ctx, tx, f.market.ID, StatusActive, "investigated and resumed")
			return serr
		}))
}

// TestIntegration_TheBreakerDoesNotTripOnAMoveOutsideItsWindow.
func TestIntegration_TheBreakerDoesNotTripOnAMoveOutsideItsWindow(t *testing.T) {
	f := newFixture(t)
	p := ConservativeSafetyPolicy()
	p.CircuitBreakerMoveBPS = bpsPtr(500)
	window := 60
	p.CircuitBreakerWindowSeconds = &window
	p.Version = "test-short-window"
	f.svc.SetSafety(fixedSafety{p: p})
	t.Cleanup(func() { f.svc.SetSafety(nil) })

	// Two buys that together move the price well past the limit, but with more
	// than the window between them: each one alone is inside it.
	first, err := f.buy(f.trader, 200_000_000, money.Quantity{})
	require.NoError(t, err)
	require.Nil(t, first.Breaker)

	f.clk.Advance(2 * time.Minute)
	second, err := f.buy(f.trader, 200_000_000, money.Quantity{})
	require.NoError(t, err)
	assert.Nil(t, second.Breaker,
		"the earlier print is outside the window, so the reference is this trade's own opening price")

	m, err := f.svc.Market(f.ctx, testDB, f.market.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusActive, m.Status)
}

// --- helpers ----------------------------------------------------------------

// fixedSafety is a Safety that answers with one policy, for tests about a limit
// rather than about where the policy came from.
type fixedSafety struct{ p SafetyPolicy }

func (f fixedSafety) SafetyPolicy(context.Context, db.Querier, time.Time) (SafetyPolicy, error) {
	return f.p, nil
}

func (f *fixture) position(t *testing.T, account accounts.AccountID) Position {
	t.Helper()
	p, err := f.svc.Position(f.ctx, testDB, account, f.asset.AssetID)
	require.NoError(t, err)
	return p
}

// requirePositionIs compares every maintained field against the Go statement of
// the same arithmetic.
func (f *fixture) requirePositionIs(t *testing.T, account accounts.AccountID, want Position) {
	t.Helper()
	got := f.position(t, account)
	require.True(t, got.HoldsInvariant(), "the stored position breaks its own invariant: %+v", got)
	assert.Equal(t, want.Quantity.String(), got.Quantity.String(), "quantity")
	assert.Equal(t, want.CostBasisCredits.String(), got.CostBasisCredits.String(), "cost basis")
	assert.Equal(t, want.RealizedPnLCredits.String(), got.RealizedPnLCredits.String(), "realised P&L")
	assert.Equal(t, want.FeesPaidCredits.String(), got.FeesPaidCredits.String(), "fees paid")
	assert.Equal(t, want.UnitsBoughtTotal.String(), got.UnitsBoughtTotal.String(), "units bought")
	assert.Equal(t, want.UnitsSoldTotal.String(), got.UnitsSoldTotal.String(), "units sold")
	assert.Equal(t, want.CreditsInTotal.String(), got.CreditsInTotal.String(), "credits in")
	assert.Equal(t, want.CreditsOutTotal.String(), got.CreditsOutTotal.String(), "credits out")
	assert.Equal(t, want.FillCount, got.FillCount, "fill count")
	assert.Equal(t, got.Quantity.String(),
		f.balance(account, f.asset.AssetID, ledger.CodeNativeAssetBalance).String(),
		"the position must equal the ledger balance behind it")
}

func newAccountFundedBy(t *testing.T, f *fixture) accounts.AccountID {
	t.Helper()
	a := newAccount(t)
	f.fund(a, 50_000_000_000)
	return a
}

func findMarket(t *testing.T, markets []MarketSummary, want MarketID) MarketSummary {
	t.Helper()
	for _, m := range markets {
		if m.MarketID == want {
			return m
		}
	}
	t.Fatalf("market %s is not in the %d returned", want, len(markets))
	return MarketSummary{}
}

// newDraftAsset creates a screened DRAFT asset and returns its id, for tests
// about what happens when a market is opened for one.
func newDraftAsset(t *testing.T, f *fixture, tx pgx.Tx) assets.AssetID {
	t.Helper()
	suffix := uuid.NewString()[:6]
	a, _, err := f.assetSv.CreateDraft(t.Context(), tx, nativeasset.CreateRequest{
		CreatorAccountID: f.creator,
		Name:             "Thin " + suffix,
		Symbol:           "TH" + strings.ToUpper(suffix[:4]),
		Description:      "a test asset",
		Supply: nativeasset.SupplyModel{
			MaxSupply:         qs("1000000000000000"),
			CreatorAllocation: qs("0"),
		},
	})
	require.NoError(t, err)
	return a.AssetID
}

// newMarketFor opens one more live market, so a paging test has something to
// page through.
func newMarketFor(t *testing.T, f *fixture) MarketID {
	t.Helper()
	var out MarketID
	suffix := uuid.NewString()[:6]
	require.NoError(t, testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted},
		func(ctx context.Context, tx pgx.Tx) error {
			a, _, err := f.assetSv.CreateDraft(ctx, tx, nativeasset.CreateRequest{
				CreatorAccountID: f.creator,
				Name:             "Paged " + suffix,
				Symbol:           "PG" + strings.ToUpper(suffix[:4]),
				Description:      "a paging fixture",
				Supply: nativeasset.SupplyModel{
					MaxSupply:         qs("1000000000000000"),
					CreatorAllocation: qs("0"),
				},
			})
			if err != nil {
				return err
			}
			if _, err := f.assetSv.SetStatus(ctx, tx, a.AssetID, nativeasset.StatusPendingReview, "submitted"); err != nil {
				return err
			}
			if _, err := f.assetSv.SetModeration(ctx, tx, a.AssetID, nativeasset.ModerationApproved, "test fixture"); err != nil {
				return err
			}
			if _, err := f.assetSv.Activate(ctx, tx, a.AssetID, "test fixture"); err != nil {
				return err
			}
			m, err := f.svc.Create(ctx, tx, CreateRequest{
				AssetID: a.AssetID, CreditAssetID: f.creditAsset, CreatorID: f.creator,
				PoolSupply:           a.Supply.PoolSupply(),
				VirtualCreditReserve: qs("30000000000"),
				Fees:                 Fees{PlatformBPS: 100, CreatorBPS: 50},
				IdempotencyKey:       "mint-" + suffix, EffectiveAt: f.clk.Now(),
			})
			if err != nil {
				return err
			}
			open, err := f.svc.SetStatus(ctx, tx, m.ID, StatusActive, "test fixture")
			if err != nil {
				return err
			}
			out = open.ID
			return nil
		}))
	return out
}
