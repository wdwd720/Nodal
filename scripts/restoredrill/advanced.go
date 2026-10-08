package main

import (
	"context"
	"fmt"
	"time"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/orders"
	"github.com/nodal/controlplane/internal/predmarket"
	"github.com/nodal/controlplane/internal/stableasset"
	"github.com/nodal/controlplane/internal/transfers"
	"github.com/nodal/controlplane/scripts/internal/sandboxworld"
)

// advancedStart is the instant the advanced economy's clock starts at. It is
// fixed for the reason seedDomainA's epoch is: every run seeds the same
// economy, timestamps included.
var advancedStart = time.Date(2026, 10, 7, 14, 30, 0, 0, time.UTC)

const (
	// advancedOracle is the valuation source of every mark the seed records.
	advancedOracle = "restore-drill-oracle"
	// tradePrice is SBX-ETH's mark and the price both limit orders name, in
	// SBX-USD.
	tradePrice = "2500.00"
	// stableSymbol is the stable asset the seed issues.
	stableSymbol = "SBX-DRILL"
)

// seedAdvanced writes a small advanced economy into the source database,
// through the real services, driven as scripts/advanceddemo drives them: the
// sandbox world (scripts/internal/sandboxworld) wired over the drill's
// database in LOCAL, where every value is SIMULATED.
//
// It exists for the reason seedDomainA does. Every advanced table took part in
// the row-count comparison and every one of them restored EMPTY, so the drill
// compared zero with zero and had never shown an advanced book surviving a
// restore.
//
// The economy: the faucet funds two customers; Alice sends Bob part of hers;
// the two trade on an internal book, Alice's resting limit sell filled by
// Bob's crossing limit buy; Bob posts collateral into a margin account; Alice
// mints a stable asset on the simulated chain; and both buy complete sets of a
// prediction market. Each step is checked by what its service returned, so a
// seed that stopped doing what it says fails here instead of leaving an area
// quietly empty; the area report (compareAreas) then checks that the rows are
// in the source and came back. It returns a one-line account of what it did.
func seedAdvanced(ctx context.Context, appDSN string) (string, error) {
	w, err := sandboxworld.Open(ctx, sandboxworld.Options{AppDSN: appDSN, Start: advancedStart, MaxConns: 4, Component: "restoredrill"})
	if err != nil {
		return "", err
	}
	defer w.Close()
	usd := func(n int64) money.Quantity { return sandboxworld.Units(n, w.USD.Decimals) }
	showUSD := func(q money.Quantity) string { return q.ToDecimalString(w.USD.Decimals) + " " + w.USD.Symbol }

	alice, err := w.Customer(ctx, "alice")
	if err != nil {
		return "", err
	}
	bob, err := w.Customer(ctx, "bob")
	if err != nil {
		return "", err
	}
	eth, err := w.SimAsset(ctx, "SBX-ETH", "Sandbox ether (simulated)", 8, assets.RiskMajor, false)
	if err != nil {
		return "", err
	}
	showETH := func(q money.Quantity) string { return q.ToDecimalString(eth.Decimals) + " " + eth.Symbol }

	// Sandbox funding of the two customers.
	grants := []struct {
		c     sandboxworld.Customer
		asset assets.AssetID
		qty   money.Quantity
		key   string
	}{
		{alice, w.USD.ID, usd(1_000), "drill-fund-alice-usd"},
		{alice, eth.ID, sandboxworld.Units(1, eth.Decimals), "drill-fund-alice-eth"},
		{bob, w.USD.ID, usd(1_000), "drill-fund-bob-usd"},
	}
	for _, g := range grants {
		if _, err := w.Fund(ctx, g.c, g.asset, g.qty, g.key); err != nil {
			return "", fmt.Errorf("faucet grant %s: %w", g.key, err)
		}
	}

	// A customer-to-customer transfer.
	sent := usd(250)
	tr, err := w.Transfer(ctx, alice.P, transfers.SendRequest{
		SenderAccountID: alice.Account, RecipientAccountID: bob.Account, AssetID: w.USD.ID, Quantity: sent,
		Mode: gates.ModeSandbox, Memo: "restore drill transfer", IdempotencyKey: "drill-transfer", CorrelationID: "restoredrill",
	})
	if err != nil {
		return "", fmt.Errorf("transfer: %w", err)
	}
	if tr.Transfer.Status != transfers.StatusCompleted {
		return "", fmt.Errorf("transfer: status %s, want %s", tr.Transfer.Status, transfers.StatusCompleted)
	}

	// An internal book on which two limit orders trade: a lot is 0.0001
	// SBX-ETH, so one tick-lot is one SBX-USD base unit and the notional is
	// exact; maker 10 bps and taker 20 bps of it.
	lot := money.QuantityFromInt64(10_000)
	book, err := w.OpenSpotBook(ctx, sandboxworld.SpotBook{
		Base: eth, Mode: gates.ModePaper, Lot: lot,
		Maker: w.BookSchedule("restore-drill-maker-10bps", 10), Taker: w.BookSchedule("restore-drill-taker-20bps", 20),
		SnapshotInterval: 2,
	})
	if err != nil {
		return "", fmt.Errorf("open the book: %w", err)
	}
	if _, err := w.Mark(ctx, eth.ID, tradePrice, advancedOracle); err != nil {
		return "", err
	}
	price, err := money.PriceFromDecimalString(tradePrice, w.USD.ID.String(), "order", w.Clock.Now())
	if err != nil {
		return "", err
	}
	traded := lot.Mul(money.QuantityFromInt64(2_000)) // 0.2 SBX-ETH
	sell, err := w.OrderPipeline.Submit(ctx, sandboxworld.LimitOrder(alice, book.InstrumentID, orders.SideSell, price, traded, "drill-sell"))
	if err != nil {
		return "", fmt.Errorf("the resting sell: %w", err)
	}
	if sell.Order.Status != orders.StatusOpen {
		return "", fmt.Errorf("the resting sell: status %s, want %s", sell.Order.Status, orders.StatusOpen)
	}
	buy, err := w.OrderPipeline.Submit(ctx, sandboxworld.LimitOrder(bob, book.InstrumentID, orders.SideBuy, price, traded, "drill-buy"))
	if err != nil {
		return "", fmt.Errorf("the crossing buy: %w", err)
	}
	if len(buy.Trades) != 1 || buy.Order.Status != orders.StatusFilled {
		return "", fmt.Errorf("the crossing buy: %d trades and status %s, want one trade and %s", len(buy.Trades), buy.Order.Status, orders.StatusFilled)
	}
	trade := buy.Trades[0]

	// A margin account with posted collateral.
	collateral := usd(100)
	if _, err := w.OpenMarginAccount(ctx, bob, gates.ModePaper); err != nil {
		return "", fmt.Errorf("open the margin account: %w", err)
	}
	if _, err := w.PostCollateral(ctx, bob, w.USD.ID, collateral, "drill-collateral"); err != nil {
		return "", fmt.Errorf("post collateral: %w", err)
	}

	// A stable asset issued on the simulated chain: fully reserved in the
	// sandbox dollar, pegged at 1.000000 of it.
	operator, err := w.OperatorActor(ctx, "drill-stable-operator")
	if err != nil {
		return "", err
	}
	stable, err := w.Stable.Register(ctx, stableasset.Definition{
		Name: "restore-drill-dollar", Symbol: stableSymbol, PegCurrency: "USD", Decimals: 6,
		IssuanceCap: sandboxworld.Units(1_000_000, 6), RedemptionMode: stableasset.RedemptionInstant,
		PegQuoteAssetID: w.USD.ID, PegMantissa: money.QuantityFromInt64(1_000_000), PegScale: 6,
		OracleSource: advancedOracle, MaxPriceAge: time.Hour, PegDeviationThreshold: 50, Attestable: true,
		Reserves: []stableasset.ReserveSpec{
			{AssetID: w.USD.ID, Kind: stableasset.ReserveCashEquivalent, FactorBPS: 10_000, AcceptedForMint: true},
		},
	}, operator)
	if err != nil {
		return "", fmt.Errorf("register the stable asset: %w", err)
	}
	if _, err := w.Mark(ctx, stable.AssetID, "1.000000", advancedOracle); err != nil {
		return "", err
	}
	minted := sandboxworld.Units(100, stable.Decimals)
	mint, err := w.MintStable(ctx, alice, stable, minted, "drill-mint")
	if err != nil {
		return "", fmt.Errorf("mint the stable asset: %w", err)
	}
	if mint.Request.Status != stableasset.MintIssued {
		return "", fmt.Errorf("mint the stable asset: status %s %s, want %s", mint.Request.Status, mint.Request.FailureCode, stableasset.MintIssued)
	}

	// A prediction market with complete sets issued to both customers.
	creator, err := w.OperatorActor(ctx, "drill-market-creator")
	if err != nil {
		return "", err
	}
	market, err := w.OpenPredictionMarket(ctx, predmarket.MarketSpec{
		Symbol:   "SBX-DRILL-1007",
		Question: "Will the sandbox event board certify YES for the restore drill's fixture market?",
		Outcomes: []string{"YES", "NO"}, CollateralAssetID: w.USD.ID, Mode: gates.ModeSandbox,
		OpenAt: w.Clock.Now(), CloseAt: w.Clock.Now().Add(2 * time.Hour), DisputeWindow: 30 * time.Minute,
		OracleSource: w.Oracle.Source(), OraclePublicKey: w.Oracle.PublicKey(),
	}, creator)
	if err != nil {
		return "", fmt.Errorf("open the prediction market: %w", err)
	}
	if market.Status != predmarket.StatusOpen {
		return "", fmt.Errorf("open the prediction market: status %s, want %s", market.Status, predmarket.StatusOpen)
	}
	var sets int64
	for _, s := range []struct {
		c    sandboxworld.Customer
		sets int64
	}{{alice, 5}, {bob, 10}} {
		if _, err := w.IssueCompleteSets(ctx, market, s.c, s.sets, "drill-sets-"+s.c.Label); err != nil {
			return "", fmt.Errorf("complete sets for %s: %w", s.c.Label, err)
		}
		sets += s.sets
	}
	escrow, err := w.Predictions.Books(ctx, w.DB, market)
	if err != nil {
		return "", err
	}
	if !escrow.Escrow.Equal(usd(sets)) {
		return "", fmt.Errorf("prediction escrow holds %s, want %s for %d complete sets", showUSD(escrow.Escrow), showUSD(usd(sets)), sets)
	}

	return fmt.Sprintf("%d faucet grants to 2 customers; a %s transfer; a %s trade at %s on an internal %s/%s book "+
		"(%s notional, maker fee %s, taker fee %s); %s posted as margin collateral; %s %s minted on %s; "+
		"%d complete sets of prediction market %s (%s in escrow)",
		len(grants), showUSD(sent), showETH(trade.BaseQuantity), tradePrice, eth.Symbol, w.USD.Symbol,
		showUSD(trade.QuoteNotional), showUSD(trade.MakerFee), showUSD(trade.TakerFee), showUSD(collateral),
		minted.ToDecimalString(stable.Decimals), stableSymbol, stable.Chain, sets, market.Symbol, showUSD(escrow.Escrow)), nil
}
