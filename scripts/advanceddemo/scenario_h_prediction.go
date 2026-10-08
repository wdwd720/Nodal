package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/orders"
	"github.com/nodal/controlplane/internal/predmarket"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/scripts/internal/sandboxworld"
)

func init() {
	register("H", "Prediction market: complete sets, outcome shares traded on the internal book, oracle resolution, dispute window and exact settlement", scenarioPrediction)
}

func scenarioPrediction(ctx context.Context, w *sandboxworld.World, rec *recorder) error {
	usd := func(n int64) money.Quantity { return sandboxworld.Units(n, w.USD.Decimals) }
	cents := func(n int64) money.Quantity { return money.QuantityFromInt64(n * 10_000) }
	show := func(q money.Quantity) string { return q.ToDecimalString(w.USD.Decimals) }
	shares := func(n int64) money.Quantity { return predmarket.ShareUnit().Mul(money.QuantityFromInt64(n)) }

	creator, err := w.OperatorActor(ctx, "olivia-market-creator")
	if err != nil {
		return err
	}
	resolver, err := w.OperatorActor(ctx, "rafael-resolver")
	if err != nil {
		return err
	}
	settler, err := w.OperatorActor(ctx, "sam-settler")
	if err != nil {
		return err
	}
	// The operator who records what the sandbox oracle saw can never resolve
	// the market: dual control between the oracle's word and the ruling.
	observer, err := w.OperatorActor(ctx, "oona-oracle-observer")
	if err != nil {
		return err
	}
	scheduler := econ.SystemActor("predmarket-scheduler")

	spec := predmarket.MarketSpec{
		Symbol:   "SBX-RAIN-1008",
		Question: "Will the sandbox weather board certify rain in the sandbox city on 2026-10-08?",
		Outcomes: []string{"YES", "NO"}, CollateralAssetID: w.USD.ID, Mode: gates.ModeSandbox,
		OpenAt: w.Clock.Now(), CloseAt: w.Clock.Now().Add(2 * time.Hour), DisputeWindow: 30 * time.Minute,
		OracleSource: w.Oracle.Source(), OraclePublicKey: w.Oracle.PublicKey(),
	}
	var m predmarket.Market
	if err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		m, _, err = w.Predictions.CreateMarket(ctx, tx, spec, creator)
		return err
	}); err != nil {
		return fmt.Errorf("create market: %w", err)
	}
	if err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		m, err = w.Predictions.Open(ctx, tx, m.ID, creator)
		return err
	}); err != nil {
		return fmt.Errorf("open market: %w", err)
	}
	yes, no := m.Outcomes[0], m.Outcomes[1]
	rec.step("an operator defines and opens a SANDBOX binary market collateralised in SBX-USD, resolvable only by the sandbox oracle's signed report, with a 30-minute dispute window",
		"market", m.ID.String(), "facility", m.FacilityID.String(), "yes_instrument", yes.InstrumentID.String(),
		"no_instrument", no.InstrumentID.String(), "oracle", w.Oracle.Source())
	rec.check("the market is OPEN", m.Status == predmarket.StatusOpen, "status %s", m.Status)

	paula, err := w.Customer(ctx, "paula-predicts-rain")
	if err != nil {
		return err
	}
	quinn, err := w.Customer(ctx, "quinn-predicts-dry")
	if err != nil {
		return err
	}
	rosa, err := w.Customer(ctx, "rosa-buys-no")
	if err != nil {
		return err
	}
	for _, c := range []sandboxworld.Customer{paula, quinn, rosa} {
		if _, err := w.Fund(ctx, c, w.USD.ID, usd(100), "demo-h-fund-"+c.Label); err != nil {
			return fmt.Errorf("fund %s: %w", c.Label, err)
		}
	}
	issue := func(c sandboxworld.Customer, sets int64) (predmarket.ActionResult, error) {
		var res predmarket.ActionResult
		err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			res, err = w.Predictions.Issue(ctx, tx, predmarket.PositionRequest{
				MarketID: m.ID, AccountID: c.Account, Quantity: shares(sets), IdempotencyKey: "demo-h-issue-" + c.Label,
				ActorType: security.ActorUser, ActorID: c.User.String(),
			})
			return err
		})
		return res, err
	}
	pIssue, err := issue(paula, 10)
	if err != nil {
		return fmt.Errorf("paula's complete sets: %w", err)
	}
	if _, err := issue(quinn, 5); err != nil {
		return fmt.Errorf("quinn's complete sets: %w", err)
	}
	books, err := w.Predictions.Books(ctx, w.DB, m)
	if err != nil {
		return err
	}
	rec.step("Paula buys 10 complete sets and Quinn 5: each set costs 1 SBX-USD into the market's escrow and is one YES and one NO share",
		"paula_intent", pIssue.Intent.ID.String(), "paula_effect", pIssue.Effect.ID.String(), "escrow", show(books.Escrow)+" SBX-USD")
	rec.check("escrow holds exactly the collateral of the 15 sets and the escrow identity holds",
		books.Escrow.Equal(usd(15)) && len(books.Violations(money.Quantity{})) == 0, "escrow %s, violations %v",
		show(books.Escrow), books.Violations(money.Quantity{}))

	// The outcome shares trade on the same internal book as any other
	// simulated instrument: one book per outcome, priced in SBX-USD per share,
	// capped at 1.00 because a share never pays more.
	for _, o := range m.Outcomes {
		var b orders.Book
		if err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			b, err = w.Orders.OpenBook(ctx, tx, orders.OpenBookRequest{
				InstrumentID: o.InstrumentID, Mode: gates.ModeSandbox,
				MakerSchedule:    bookSchedule(w.USD.ID, "demo-h-maker-0bps", 0),
				TakerSchedule:    bookSchedule(w.USD.ID, "demo-h-taker-0bps", 0),
				SnapshotInterval: 4, Actor: w.System,
			})
			return err
		}); err != nil {
			return fmt.Errorf("open the %s book: %w", o.Symbol, err)
		}
		rec.step(fmt.Sprintf("an internal SANDBOX book opens for %s.%s with a zero-fee schedule; its price ceiling is 1.00", m.Symbol, o.Symbol),
			"book", b.ID.String(), "max_price_ticks", fmt.Sprint(b.Terms.MaxPriceTicks))
	}
	// Buying a share acquires exposure, which Risk Kernel V2 values at a fresh
	// reference mark and refuses without one (RISK_ORACLE_UNAVAILABLE). The
	// sandbox reference feed marks each outcome; nothing here is a quote.
	for _, mk := range []struct {
		o  predmarket.Outcome
		px string
	}{{yes, "0.60"}, {no, "0.40"}} {
		if _, err := w.Mark(ctx, mk.o.ShareAssetID, mk.px, "demo-reference-feed"); err != nil {
			return err
		}
	}
	rec.step("the sandbox reference feed marks YES at 0.60 and NO at 0.40 SBX-USD, the fresh marks Risk Kernel V2 values exposure at")
	price := func(s string) (money.Price, error) {
		return money.PriceFromDecimalString(s, w.USD.ID.String(), "order", w.Clock.Now())
	}
	trade := func(seller, buyer sandboxworld.Customer, o predmarket.Outcome, px string, n int64, key string) (orders.Trade, error) {
		p, err := price(px)
		if err != nil {
			return orders.Trade{}, err
		}
		if _, err := w.OrderPipeline.Submit(ctx, limitOrder(seller, o.InstrumentID, orders.SideSell, p, shares(n), key+"-sell")); err != nil {
			return orders.Trade{}, fmt.Errorf("%s's sell: %w", seller.Label, err)
		}
		out, err := w.OrderPipeline.Submit(ctx, limitOrder(buyer, o.InstrumentID, orders.SideBuy, p, shares(n), key+"-buy"))
		if err != nil {
			return orders.Trade{}, fmt.Errorf("%s's buy: %w", buyer.Label, err)
		}
		if len(out.Trades) != 1 || out.Order.Status != orders.StatusFilled {
			return orders.Trade{}, fmt.Errorf("%s's buy: %d trades, status %s", buyer.Label, len(out.Trades), out.Order.Status)
		}
		return out.Trades[0], nil
	}
	t1, err := trade(paula, rosa, no, "0.35", 10, "demo-h-no")
	if err != nil {
		return err
	}
	rec.step("Paula sells 10 NO shares at 0.35 on the book; Rosa's crossing buy takes them for 3.50 SBX-USD",
		"trade_seq", fmt.Sprint(t1.TradeSeq), "journal", t1.JournalTransactionID.String(), "effect", t1.EffectID.String())
	t2, err := trade(quinn, paula, yes, "0.62", 5, "demo-h-yes")
	if err != nil {
		return err
	}
	rec.step("Quinn sells 5 YES shares at 0.62; Paula buys them for 3.10 SBX-USD",
		"trade_seq", fmt.Sprint(t2.TradeSeq), "journal", t2.JournalTransactionID.String(), "effect", t2.EffectID.String())

	type holding struct {
		who        string
		c          sandboxworld.Customer
		usd        money.Quantity
		yes, noQty int64
	}
	expect := func(hs []holding) (bool, string) {
		ok, parts := true, []string{}
		for _, h := range hs {
			cash, _ := w.Wallet(ctx, h.c, w.USD.ID)
			y, _ := w.Wallet(ctx, h.c, yes.ShareAssetID)
			n, _ := w.Wallet(ctx, h.c, no.ShareAssetID)
			if !cash.Equal(h.usd) || !y.Equal(shares(h.yes)) || !n.Equal(shares(h.noQty)) {
				ok = false
			}
			parts = append(parts, fmt.Sprintf("%s %s SBX-USD, %s YES, %s NO", h.who, show(cash),
				y.ToDecimalString(predmarket.ShareDecimals), n.ToDecimalString(predmarket.ShareDecimals)))
		}
		return ok, strings.Join(parts, "; ")
	}
	ok, detail := expect([]holding{
		{"Paula", paula, usd(90).Add(cents(40)), 15, 0},
		{"Quinn", quinn, usd(98).Add(cents(10)), 0, 5},
		{"Rosa", rosa, usd(96).Add(cents(50)), 0, 10},
	})
	rec.check("after the two trades every holding is exactly what the trades say", ok, "%s", detail)
	books, err = w.Predictions.Books(ctx, w.DB, m)
	if err != nil {
		return err
	}
	rec.check("trading moved shares between holders without changing escrow or shares outstanding",
		books.Escrow.Equal(usd(15)) && len(books.Violations(money.Quantity{})) == 0, "escrow %s, violations %v",
		show(books.Escrow), books.Violations(money.Quantity{}))

	// Close: the scheduler closes the market once its close time passes, and
	// the outcome books stop taking orders.
	w.Clock.Set(m.CloseAt.Add(time.Minute))
	var closed []predmarket.MarketID
	if err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		closed, err = w.Predictions.CloseDue(ctx, tx, scheduler, 100)
		return err
	}); err != nil {
		return fmt.Errorf("close due markets: %w", err)
	}
	m, err = w.Predictions.Get(ctx, w.DB, m.ID)
	if err != nil {
		return err
	}
	rec.step("the close time passes and the scheduler closes the market", "closed_markets", fmt.Sprint(len(closed)))
	rec.check("the market is CLOSED", m.Status == predmarket.StatusClosed, "status %s", m.Status)
	late, err := price("0.90")
	if err != nil {
		return err
	}
	lateOut, lateErr := w.OrderPipeline.Submit(ctx, limitOrder(rosa, yes.InstrumentID, orders.SideBuy, late, shares(1), "demo-h-late"))
	rec.check("an order after close is refused and trades nothing", lateErr != nil && len(lateOut.Trades) == 0,
		"status %q: %v", lateOut.Order.Status, lateErr)

	// Resolution: only the oracle's signed report can resolve the market, and
	// the operator who resolves it is neither its creator nor its settler.
	if err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := w.Predictions.RequestResolution(ctx, tx, m.ID, scheduler)
		return err
	}); err != nil {
		return fmt.Errorf("request resolution: %w", err)
	}
	observedAt := m.CloseAt.Add(30 * time.Second)
	var observed predmarket.ObservationResult
	if err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		observed, err = w.Predictions.RecordObservation(ctx, tx, predmarket.ObservationRequest{
			MarketID: m.ID, Outcome: "YES", ObservedAt: observedAt, Actor: observer,
			Evidence: []byte(`{"certified":"YES","board":"sandbox weather board","city":"sandbox city"}`),
		})
		return err
	}); err != nil {
		return fmt.Errorf("record the oracle observation: %w", err)
	}
	report, err := w.Predictions.FetchReport(ctx, w.DB, m.ID)
	if err != nil {
		return fmt.Errorf("fetch the oracle report: %w", err)
	}
	var observerRefused error
	if err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, observerRefused = w.Predictions.Resolve(ctx, tx, predmarket.ResolveRequest{MarketID: m.ID, Report: report, Actor: observer})
		return observerRefused
	}); err == nil {
		return fmt.Errorf("the operator who recorded the observation resolved the market")
	}
	rec.check("the operator who recorded the observation cannot resolve the market",
		errs.CodeOf(observerRefused) == errs.CodeForbidden, "%v", observerRefused)
	var resolved predmarket.ResolveResult
	if err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		resolved, err = w.Predictions.Resolve(ctx, tx, predmarket.ResolveRequest{MarketID: m.ID, Report: report, Actor: resolver,
			Reason: "the sandbox oracle's signed report certifies YES"})
		return err
	}); err != nil {
		return fmt.Errorf("resolve: %w", err)
	}
	rec.step("an operator records the sandbox oracle's observation after close (signed and stored durably); a second operator resolves the market to YES with the stored, verified report",
		"observation", observed.Observation.ID.String(), "recorded_by", observed.Observation.RecordedBy.ID,
		"resolution", resolved.Resolution.ID.String(), "kind", string(resolved.Resolution.Kind), "outcome", "YES")
	rec.check("the market is RESOLVED by the oracle", resolved.Market.Status == predmarket.StatusResolved &&
		resolved.Resolution.Kind == predmarket.ResolutionOracle, "status %s, kind %s", resolved.Market.Status, resolved.Resolution.Kind)

	settle := func() (predmarket.SettleResult, error) {
		var out predmarket.SettleResult
		err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			out, err = w.Predictions.Settle(ctx, tx, predmarket.SettleRequest{MarketID: m.ID, Actor: settler})
			return err
		})
		return out, err
	}
	_, earlyErr := settle()
	rec.check("settlement inside the dispute window is refused", earlyErr != nil && errs.CodeOf(earlyErr) == errs.CodeInvalidStateTransition,
		"%v", earlyErr)

	w.Clock.Set(w.Clock.Now().Add(31 * time.Minute))
	st, err := settle()
	if err != nil {
		return fmt.Errorf("settle: %w", err)
	}
	rec.step("the dispute window passes with no dispute and a third operator settles the market from escrow",
		"settlement", st.Settlement.ID.String(), "paid", show(st.Settlement.TotalPaid)+" SBX-USD", "holders", fmt.Sprint(st.Settlement.HolderCount))
	rec.check("the payouts are the escrow exactly, with no residual and nothing rounded",
		st.Market.Status == predmarket.StatusSettled && st.Settlement.TotalPaid.Equal(usd(15)) && st.Settlement.Residual.IsZero() &&
			st.Settlement.RoundingPolicy == predmarket.RoundingExact,
		"paid %s of escrow %s, residual %s, rounding %s", show(st.Settlement.TotalPaid), show(st.Settlement.EscrowBefore),
		show(st.Settlement.Residual), st.Settlement.RoundingPolicy)
	ok, detail = expect([]holding{
		{"Paula", paula, usd(105).Add(cents(40)), 0, 0},
		{"Quinn", quinn, usd(98).Add(cents(10)), 0, 0},
		{"Rosa", rosa, usd(96).Add(cents(50)), 0, 0},
	})
	rec.check("each YES share paid 1 SBX-USD, each NO share nothing, and every share was retired", ok, "%s", detail)
	var total money.Quantity
	for _, c := range []sandboxworld.Customer{paula, quinn, rosa} {
		bal, err := w.Wallet(ctx, c, w.USD.ID)
		if err != nil {
			return err
		}
		total = total.Add(bal)
	}
	rec.check("SBX-USD is conserved across the three customers", total.Equal(usd(300)), "%s SBX-USD", show(total))

	books, err = w.Predictions.Books(ctx, w.DB, m)
	if err != nil {
		return err
	}
	var recon reconciliation.Record
	if err := w.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		recon, err = w.Predictions.ReconcileEscrow(ctx, tx, predmarket.ReconcileRequest{MarketID: m.ID})
		return err
	}); err != nil {
		return fmt.Errorf("reconcile escrow: %w", err)
	}
	rec.check("the escrow is empty and its reconciliation record is MATCHED",
		books.Escrow.IsZero() && recon.Status == reconciliation.StatusMatched, "escrow %s, record %s", show(books.Escrow), recon.Status)
	again, err := settle()
	if err != nil {
		return fmt.Errorf("settle again: %w", err)
	}
	rec.check("settling again pays nothing and returns the stored record", again.Replay && len(again.Lines) == 0,
		"replay %t, %d lines", again.Replay, len(again.Lines))
	drift, err := ledger.VerifyBalances(ctx, w.DB)
	if err != nil {
		return err
	}
	rec.check("no ledger balance drifted from its entries", len(drift) == 0, "%d drifted balances", len(drift))
	inst, err := w.Instruments.Get(ctx, w.DB, yes.InstrumentID)
	if err != nil {
		return err
	}
	rec.check("the settled outcome instruments are halted", inst.Status == assets.StatusHalted, "%s.%s %s", m.Symbol, yes.Symbol, inst.Status)
	return nil
}
