package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/orders"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/scripts/internal/sandboxworld"
)

func init() {
	register("D", "Internal matching: two sandbox customers trade on the internal book, with disclosed fees and an audit proof", scenarioMatching)
}

// bookSchedule is a book fee schedule charging pct basis points of notional
// in the quote asset, and nothing else.
func bookSchedule(quote assets.AssetID, version string, pct money.BPS) fees.Schedule {
	s := fees.ZeroSchedule(quote, 6)
	s.Version = version
	s.PercentageFeeBPS = pct
	return s
}

// limitOrder is a GTC limit order the customer places as itself.
func limitOrder(c sandboxworld.Customer, inst instruments.InstrumentID, side orders.Side, price money.Price, qty money.Quantity, key string) orders.OrderRequest {
	return orders.OrderRequest{
		InstrumentID: inst, AccountID: c.Account, Side: side, Type: orders.TypeLimit, TimeInForce: orders.GTC,
		Price: &price, Quantity: qty, IdempotencyKey: key, ActorType: security.ActorUser, ActorID: c.User.String(),
	}
}

func scenarioMatching(ctx context.Context, w *sandboxworld.World, rec *recorder) error {
	usd := func(n int64) money.Quantity { return sandboxworld.Units(n, w.USD.Decimals) }
	eth, err := w.SimAsset(ctx, "SBX-ETH", "Sandbox ether (simulated)", 8, assets.RiskMajor, false)
	if err != nil {
		return err
	}
	ethUnits := func(n int64) money.Quantity { return sandboxworld.Units(n, eth.Decimals) }
	showUSD := func(q money.Quantity) string { return q.ToDecimalString(w.USD.Decimals) + " SBX-USD" }
	showETH := func(q money.Quantity) string { return q.ToDecimalString(eth.Decimals) + " SBX-ETH" }

	// The book: SBX-ETH/SBX-USD at two price decimals. A tick is 0.01 SBX-USD
	// and a lot 0.0001 SBX-ETH, so one tick-lot is exactly one SBX-USD base
	// unit and every notional is an integer with nothing to round.
	var book orders.Book
	if err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		inst, err := w.Instruments.CreateSpotPair(ctx, tx, instruments.SpotPairSpec{
			Base: eth.ID, Quote: w.USD.ID, Settlement: w.USD.ID, CanonicalName: "SBX-ETH/SBX-USD",
			RiskClass: assets.RiskMajor, Status: assets.StatusActive, ActiveFrom: w.Clock.Now().Add(-time.Hour),
		})
		if err != nil {
			return err
		}
		base, quote := eth.ID, w.USD.ID
		terms := instruments.Terms{
			Kind: instruments.TypeSpotPair, Symbol: "SBX-ETH/SBX-USD", VenueCode: "NODAL-SIM", BaseAssetID: &base,
			QuoteAssetID: &quote, SettlementAssetID: quote, ContractMultiplier: money.QuantityFromInt64(1), PriceScale: 2,
			TickSize: money.QuantityFromInt64(1), LotSize: money.QuantityFromInt64(10_000),
			MinQuantity: money.QuantityFromInt64(10_000), QuantityDecimals: eth.Decimals,
			SettlementStyle: instruments.SettlePhysical, TradingCalendar: "24x7", OracleMaxAgeMS: 30_000,
			MarginModel: instruments.MarginNone, CorporateActionBehavior: instruments.CorporateActionsNone,
			Transferability: instruments.Transferable,
		}
		book, err = w.Orders.OpenBook(ctx, tx, orders.OpenBookRequest{
			InstrumentID: inst.ID, Mode: gates.ModePaper, SpotTerms: &terms,
			MakerSchedule:    bookSchedule(w.USD.ID, "demo-d-maker-10bps", 10),
			TakerSchedule:    bookSchedule(w.USD.ID, "demo-d-taker-20bps", 20),
			SnapshotInterval: 2, Actor: w.System,
		})
		return err
	}); err != nil {
		return fmt.Errorf("open the book: %w", err)
	}
	if _, err := w.Mark(ctx, eth.ID, "2500.00", "demo-oracle"); err != nil {
		return err
	}
	rec.step("a PAPER internal book for SBX-ETH/SBX-USD opens with a disclosed schedule: maker 10 bps, taker 20 bps of notional; SBX-ETH marks at 2,500.00",
		"book", book.ID.String(), "instrument", book.InstrumentID.String(), "venue", book.VenueLabel,
		"maker_schedule", book.MakerScheduleHash, "taker_schedule", book.TakerScheduleHash)

	dana, err := w.Customer(ctx, "dana-maker")
	if err != nil {
		return err
	}
	erin, err := w.Customer(ctx, "erin-taker")
	if err != nil {
		return err
	}
	if _, err := w.Fund(ctx, dana, eth.ID, ethUnits(3), "demo-d-fund-dana"); err != nil {
		return fmt.Errorf("fund dana: %w", err)
	}
	if _, err := w.Fund(ctx, erin, w.USD.ID, usd(6_000), "demo-d-fund-erin"); err != nil {
		return fmt.Errorf("fund erin: %w", err)
	}
	rec.step("the faucet grants Dana 3 SBX-ETH and Erin 6,000 SBX-USD",
		"dana_account", dana.Account.String(), "erin_account", erin.Account.String())

	feeRevenue := ledger.PlatformAccount(ledger.CodePlatformFeeRevenue, w.USD.ID)
	revenueBefore, err := w.Balance(ctx, feeRevenue)
	if err != nil {
		return err
	}
	price, err := money.PriceFromDecimalString("2500.00", w.USD.ID.String(), "order", w.Clock.Now())
	if err != nil {
		return err
	}

	sell, err := w.OrderPipeline.Submit(ctx, limitOrder(dana, book.InstrumentID, orders.SideSell, price, ethUnits(2), "demo-d-sell"))
	if err != nil {
		return fmt.Errorf("dana's sell: %w", err)
	}
	rec.step("Dana places a GTC limit sell of 2 SBX-ETH at 2,500.00; nothing crosses, so it rests on the book",
		"order", sell.Order.ID.String(), "intent", sell.Intent.ID.String(), "reservation", sell.Order.ReservationID,
		"command_seq", fmt.Sprint(sell.CommandSeq))
	rec.check("the maker's order rests and its intent is executing",
		sell.Order.Status == orders.StatusOpen && sell.Intent.Status == econ.StatusExecuting,
		"order %s, intent %s", sell.Order.Status, sell.Intent.Status)

	buy, err := w.OrderPipeline.Submit(ctx, limitOrder(erin, book.InstrumentID, orders.SideBuy, price, ethUnits(2), "demo-d-buy"))
	if err != nil {
		return fmt.Errorf("erin's buy: %w", err)
	}
	if len(buy.Trades) != 1 {
		return fmt.Errorf("erin's buy produced %d trades, want 1", len(buy.Trades))
	}
	trade := buy.Trades[0]
	rec.step("Erin places a crossing GTC limit buy of 2 SBX-ETH at 2,500.00: one trade of 5,000 SBX-USD notional, settled in one SIM_TRADE journal",
		"order", buy.Order.ID.String(), "trade_seq", fmt.Sprint(trade.TradeSeq), "journal", trade.JournalTransactionID.String(),
		"effect", trade.EffectID.String(), "evidence", trade.EvidenceRef+" sha256="+hex.EncodeToString(trade.EvidenceSHA256))
	rec.check("the taker's order filled and both intents completed",
		buy.Order.Status == orders.StatusFilled && buy.Intent.Status == econ.StatusCompleted,
		"order %s, intent %s", buy.Order.Status, buy.Intent.Status)
	rec.check("the trade is 2 SBX-ETH at 2,500.00, a 5,000 SBX-USD notional",
		trade.BaseQuantity.Equal(ethUnits(2)) && trade.QuoteNotional.Equal(usd(5_000)) && trade.Price.String() == price.String(),
		"%s at %d ticks of 0.01 for %s", showETH(trade.BaseQuantity), trade.PriceTicks, showUSD(trade.QuoteNotional))
	makerFee, takerFee := sandboxworld.Units(5, w.USD.Decimals), sandboxworld.Units(10, w.USD.Decimals)
	rec.check("the fees are exactly the disclosed schedule: maker 10 bps (5 SBX-USD), taker 20 bps (10 SBX-USD)",
		trade.MakerFee.Equal(makerFee) && trade.TakerFee.Equal(takerFee), "maker %s, taker %s", showUSD(trade.MakerFee), showUSD(trade.TakerFee))

	makerOrder, err := w.Orders.Order(ctx, w.DB, sell.Order.ID)
	if err != nil {
		return err
	}
	var makerIntent econ.Intent
	if makerOrder.IntentID != nil {
		if makerIntent, err = w.Intents.Get(ctx, w.DB, *makerOrder.IntentID); err != nil {
			return err
		}
	}
	rec.check("the maker's order was filled by the taker's command and its intent completed",
		makerOrder.Status == orders.StatusFilled && makerIntent.Status == econ.StatusCompleted,
		"order %s, intent %s", makerOrder.Status, makerIntent.Status)

	type want struct {
		who   string
		c     sandboxworld.Customer
		asset assets.AssetID
		qty   money.Quantity
		show  func(money.Quantity) string
	}
	balancesOK, detail := true, []string{}
	for _, x := range []want{
		{"Dana", dana, eth.ID, ethUnits(1), showETH},
		{"Dana", dana, w.USD.ID, usd(5_000).Sub(makerFee), showUSD},
		{"Erin", erin, eth.ID, ethUnits(2), showETH},
		{"Erin", erin, w.USD.ID, usd(1_000).Sub(takerFee), showUSD},
	} {
		got, err := w.Wallet(ctx, x.c, x.asset)
		if err != nil {
			return err
		}
		if !got.Equal(x.qty) {
			balancesOK = false
		}
		detail = append(detail, fmt.Sprintf("%s %s (want %s)", x.who, x.show(got), x.show(x.qty)))
	}
	rec.check("every wallet moved by exactly the trade and its disclosed fee", balancesOK, "%s", strings.Join(detail, "; "))

	revenueAfter, err := w.Balance(ctx, feeRevenue)
	if err != nil {
		return err
	}
	collected := revenueAfter.Sub(revenueBefore)
	rec.check("the platform's fee revenue grew by exactly the two fees", collected.Equal(makerFee.Add(takerFee)),
		"PLATFORM_FEE_REVENUE +%s", showUSD(collected))

	danaUSD, _ := w.Wallet(ctx, dana, w.USD.ID)
	erinUSD, _ := w.Wallet(ctx, erin, w.USD.ID)
	danaETH, _ := w.Wallet(ctx, dana, eth.ID)
	erinETH, _ := w.Wallet(ctx, erin, eth.ID)
	rec.check("value is conserved: no SBX-USD or SBX-ETH appeared or vanished",
		danaUSD.Add(erinUSD).Add(collected).Equal(usd(6_000)) && danaETH.Add(erinETH).Equal(ethUnits(3)),
		"%s + %s + fees %s; %s + %s", showUSD(danaUSD), showUSD(erinUSD), showUSD(collected), showETH(danaETH), showETH(erinETH))

	var consumed int
	if err := w.DB.QueryRow(ctx, `SELECT count(*) FROM asset_reservations WHERE id IN ($1, $2) AND status = 'CONSUMED'`,
		makerOrder.ReservationID, buy.Order.ReservationID).Scan(&consumed); err != nil {
		return err
	}
	rec.check("both reservations were used up and closed", consumed == 2, "%d of 2 CONSUMED", consumed)

	var kind string
	if err := w.DB.QueryRow(ctx, `SELECT kind FROM journal_transactions WHERE id = $1`, trade.JournalTransactionID).Scan(&kind); err != nil {
		return err
	}
	eff, err := w.Effects.Get(ctx, w.DB, trade.EffectID)
	if err != nil {
		return err
	}
	rec.check("the trade's journal is a SIM_TRADE (balanced per party by the database) and its effect APPLIED it",
		kind == string(ledger.KindSimTrade) && eff.Status == econ.EffectApplied && eff.JournalTransactionID != nil &&
			*eff.JournalTransactionID == trade.JournalTransactionID && eff.EvidenceRef == trade.EvidenceRef,
		"journal kind %s, effect %s", kind, eff.Status)
	var archived []byte
	key := strings.TrimPrefix(trade.EvidenceRef, "pg://provider_evidence/")
	if err := w.DB.QueryRow(ctx, `SELECT sha256 FROM provider_evidence WHERE key = $1`, key).Scan(&archived); err != nil {
		return fmt.Errorf("read archived evidence: %w", err)
	}
	rec.check("the trade's evidence is in the write-once archive with the digest the trade names",
		key != trade.EvidenceRef && bytes.Equal(archived, trade.EvidenceSHA256), "%s", trade.EvidenceRef)

	var allowed int
	if err := w.DB.QueryRow(ctx, `SELECT count(*) FROM advanced_risk_decisions WHERE intent_id IN ($1, $2) AND verdict = 'ALLOW'`,
		*makerOrder.IntentID, buy.Intent.ID).Scan(&allowed); err != nil {
		return err
	}
	rec.check("Risk Kernel V2 recorded an ALLOW decision for each order before it reached the book", allowed == 2,
		"%d ALLOW decisions", allowed)

	// Erin has 990 SBX-USD left: an order needing 2,500 plus its fee cannot
	// be reserved, so it is refused before it reaches the book.
	over, overErr := w.OrderPipeline.Submit(ctx, limitOrder(erin, book.InstrumentID, orders.SideBuy, price, ethUnits(1), "demo-d-overreach"))
	rec.step("Erin tries to buy 1 more SBX-ETH at 2,500.00 with 990 SBX-USD left",
		"order", over.Order.ID.String(), "status", string(over.Order.Status), "reject_reason", over.Order.RejectReason)
	rec.check("an order beyond the customer's balance is refused and never reaches the book",
		overErr != nil && over.Order.Status == orders.StatusRejected && len(over.Trades) == 0,
		"status %s: %v", over.Order.Status, overErr)

	proof, err := w.Orders.VerifyJournal(ctx, w.DB, book.InstrumentID)
	if err != nil {
		return fmt.Errorf("verify the book journal: %w", err)
	}
	rec.step("the book's command journal is replayed from the empty book and from its latest snapshot",
		"commands", fmt.Sprint(proof.Commands), "events", fmt.Sprint(proof.Events), "trades", fmt.Sprint(proof.Trades),
		"state_hash", hex.EncodeToString(proof.StateHash[:]))
	rec.check("both replays reproduce the recorded book state hash",
		proof.StateHash == proof.FullReplayHash && proof.StateHash == proof.SnapshotReplayHash,
		"full %s, snapshot %s", hex.EncodeToString(proof.FullReplayHash[:8]), hex.EncodeToString(proof.SnapshotReplayHash[:8]))
	rec.check("the journal holds exactly the one trade", proof.Trades == 1, "%d trades", proof.Trades)
	return nil
}
