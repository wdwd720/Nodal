//go:build integration

package treasury

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/orders"
)

// TestIntegration_HouseCustomerCrossingNeedsADisclosedSandboxRule: by default
// a house order that would trade with a customer is refused by the order
// book. Only an operator can enable crossing, only with a disclosure that
// says the activity is simulated, and the disclosure, its evidence and an
// audit event are recorded with the rule version the trades then name.
func TestIntegration_HouseCustomerCrossingNeedsADisclosedSandboxRule(t *testing.T) {
	f := newFixture(t)
	m := f.market("100.00")
	cust := f.customer(m.base.ID, units(10))
	b := f.register(econ.HouseMarketMaking, false)
	f.setPolicy(b, f.policy(m))
	f.fund(b, f.usd.ID, units(10_000))
	ask, err := f.customerOrder(m, cust, orders.SideSell, "100.00", units(1))
	require.NoError(t, err)
	require.Equal(t, orders.StatusOpen, ask.Order.Status)

	refused := f.place(f.limit(b, m, econ.HouseMarketMaking, orders.SideBuy, "100.00", units(1)))
	requireRefused(t, refused.Rejection, refused.Action, orders.ReasonHouseCrossingForbidden)
	require.NotNil(t, refused.Order)
	assert.Equal(t, orders.StatusRejected, refused.Order.Status)

	disclosure := "Simulated sandbox market: the platform's house book may trade against your simulated orders on this book."
	set := func(r CrossingRuleRequest) (CrossingDisclosure, error) {
		return do(f, func(ctx context.Context, tx pgx.Tx) (CrossingDisclosure, error) {
			return f.svc.SetCrossingRule(ctx, tx, r)
		})
	}
	_, err = set(CrossingRuleRequest{InstrumentID: m.inst, Allow: true, Disclosure: disclosure, Reason: "x", Actor: houseRobot})
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err), "the house service cannot loosen its own crossing rule")
	_, err = set(CrossingRuleRequest{InstrumentID: m.inst, Allow: true, Disclosure: "the house may trade against you", Reason: "x",
		Actor: operator})
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "a disclosure must say the activity is simulated")

	c, err := set(CrossingRuleRequest{InstrumentID: m.inst, Allow: true, Disclosure: disclosure,
		Reason: "sandbox: let the house make markets for customers", Actor: operator})
	require.NoError(t, err)
	assert.Equal(t, 1, c.RuleVersion)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM audit_events WHERE action = 'treasury.crossing_rule.set' AND resource_id = $1`,
		c.BookID.String()))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM provider_evidence WHERE key = $1`,
		strings.TrimPrefix(c.EvidenceRef, "pg://provider_evidence/")))

	crossed := f.place(f.limit(b, m, econ.HouseMarketMaking, orders.SideBuy, "100.00", units(1)))
	require.NoError(t, crossed.Rejection)
	require.Len(t, crossed.Trades, 1)
	assert.Equal(t, 1, crossed.Trades[0].CrossingRuleVersion)
	assert.Equal(t, units(1).String(), f.inventory(b, m.base.ID).String())

	// Version 2 withdraws it, disclosed the same way; crossing is refused again.
	_, err = set(CrossingRuleRequest{InstrumentID: m.inst, Allow: false,
		Disclosure: "The simulated house book no longer trades against customer orders on this book.", Reason: "experiment over",
		Actor: operator})
	require.NoError(t, err)
	_, err = f.customerOrder(m, cust, orders.SideSell, "100.00", units(1))
	require.NoError(t, err)
	again := f.place(f.limit(b, m, econ.HouseMarketMaking, orders.SideBuy, "100.00", units(1)))
	requireRefused(t, again.Rejection, again.Action, orders.ReasonHouseCrossingForbidden)
	ds, err := f.svc.CrossingDisclosures(f.ctx, testDB, m.inst)
	require.NoError(t, err)
	require.Len(t, ds, 2)
	assert.True(t, ds[0].Allow)
	assert.False(t, ds[1].Allow)
	assert.Empty(t, f.reconcile(b))
}

// TestIntegration_MarketMakingQuotesBothSides: a market maker quotes a
// post-only bid and ask on the internal book; another house book hits the
// bid; both registers record the trade; a crossed quote is refused; a book
// whose strategy does not permit an activity is refused.
func TestIntegration_MarketMakingQuotesBothSides(t *testing.T) {
	f := newFixture(t)
	m := f.market("100.00")
	mm := f.register(econ.HouseMarketMaking, false)
	f.setPolicy(mm, f.policy(m))
	f.fund(mm, f.usd.ID, units(10_000))
	f.fund(mm, m.base.ID, units(10))
	q, err := do(f, func(ctx context.Context, tx pgx.Tx) (QuoteOutcome, error) {
		return f.svc.QuoteTwoSided(ctx, tx, QuoteRequest{
			HouseAccountID: mm.HouseAccountID, InstrumentID: m.inst, Activity: econ.HouseMarketMaking, BidPrice: f.price("99.50"),
			BidQuantity: units(2), AskPrice: f.price("100.50"), AskQuantity: units(2), IdempotencyKey: "quote-" + uuid.NewString(),
			Actor: houseRobot,
		})
	})
	require.NoError(t, err)
	require.NoError(t, q.Bid.Rejection)
	require.NoError(t, q.Ask.Rejection)
	assert.True(t, q.Bid.Order.PostOnly && q.Ask.Order.PostOnly)
	assert.Equal(t, orders.StatusOpen, q.Bid.Order.Status)
	assert.Equal(t, orders.StatusOpen, q.Ask.Order.Status)
	depth, err := f.orders.Depth(f.ctx, testDB, m.inst, 5)
	require.NoError(t, err)
	require.Len(t, depth.Bids, 1)
	require.Len(t, depth.Asks, 1)

	_, err = do(f, func(ctx context.Context, tx pgx.Tx) (QuoteOutcome, error) {
		return f.svc.QuoteTwoSided(ctx, tx, QuoteRequest{
			HouseAccountID: mm.HouseAccountID, InstrumentID: m.inst, Activity: econ.HouseMarketMaking, BidPrice: f.price("101.00"),
			BidQuantity: units(1), AskPrice: f.price("100.00"), AskQuantity: units(1), IdempotencyKey: "crossed-" + uuid.NewString(),
			Actor: houseRobot,
		})
	})
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "a crossed quote is refused before anything is placed")

	hedger := f.register(econ.HouseHedging, false)
	f.setPolicy(hedger, f.policy(m))
	f.fund(hedger, m.base.ID, units(5))
	hit := f.place(PlaceOrderRequest{HouseAccountID: hedger.HouseAccountID, InstrumentID: m.inst, Activity: econ.HouseHedging,
		Side: orders.SideSell, Type: orders.TypeLimit, TimeInForce: orders.IOC, Price: f.price("99.00"), Quantity: units(1),
		IdempotencyKey: "hit-" + uuid.NewString(), Actor: houseRobot})
	require.NoError(t, hit.Rejection)
	require.Len(t, hit.Trades, 1)
	assert.Equal(t, q.Bid.Order.ID, hit.Trades[0].MakerOrderID)

	rep, err := do(f, func(ctx context.Context, tx pgx.Tx) (Report, error) { return f.svc.Report(ctx, tx, mm.HouseAccountID) })
	require.NoError(t, err)
	require.Len(t, rep.Assets, 1)
	// 10 funded at 100 (cost 1,000) + 1 bought at 99.50: 11 units costing 1,099.50.
	assert.Equal(t, units(11).String(), rep.Assets[0].Quantity.String())
	assert.Equal(t, "1099500000", rep.Assets[0].Cost.String())
	balances, err := f.svc.Balances(f.ctx, testDB, mm.HouseAccountID)
	require.NoError(t, err)
	for _, bal := range balances {
		if bal.Asset == m.base.ID {
			assert.Equal(t, units(1).String(), bal.InternalBook.Bought.String())
			assert.Equal(t, units(10).String(), bal.Funded.String())
			assert.Equal(t, units(1).String(), bal.WorkingBuys.String(), "one unit of the bid still rests")
			assert.Equal(t, units(2).String(), bal.WorkingSells.String())
			assert.Equal(t, units(2).String(), bal.Reserved.String(), "the ask reserves its base")
		}
	}

	// A hedging book does not make markets.
	notMine := f.place(f.limit(hedger, m, econ.HouseMarketMaking, orders.SideBuy, "90.00", units(1)))
	requireRefused(t, notMine.Rejection, notMine.Action, ReasonActivityNotPermitted)
	assert.Empty(t, f.reconcile(mm))
	assert.Empty(t, f.reconcile(hedger))
}

// TestIntegration_TreasuryConversionDisclosesItsSpread: a treasury
// conversion between the sandbox dollar and another sandbox asset fills at a
// fresh mark through the policy's schedule; the spread and the fee are their
// own disclosed lines, booked to HOUSE_FEES, recomputable from the stored
// record; rebalancing moves a holding to its target; a conversion that does
// not involve the valuation asset, or without a fresh mark, is refused.
func TestIntegration_TreasuryConversionDisclosesItsSpread(t *testing.T) {
	f := newFixture(t)
	eur := f.simAsset("TREUR")
	f.mark(eur.ID, "1.10")
	b := f.register(econ.HouseTreasuryConversion, false)
	p := Policy{
		Label: "conversion-" + uuid.NewString()[:8], ValuationAsset: f.usd.ID, DailyLossLimit: units(100),
		MaxMarkAge: 30 * time.Second, MaxInventory: []AssetLimit{{Asset: eur.ID, MaxQuantity: units(10_000)}},
		PaperSpreadBPS: 50, PaperFeeBPS: 10,
	}
	f.setPolicy(b, p)
	f.fund(b, f.usd.ID, units(1_000))

	conv, err := do(f, func(ctx context.Context, tx pgx.Tx) (PaperOutcome, error) {
		return f.svc.Convert(ctx, tx, ConvertRequest{HouseAccountID: b.HouseAccountID, From: f.usd.ID, To: eur.ID, Quantity: units(100),
			IdempotencyKey: "conv-" + uuid.NewString(), Actor: houseRobot})
	})
	require.NoError(t, err)
	require.NoError(t, conv.Rejection)
	fill := conv.Fill
	// 100 EUR at 1.10: notional 110.00, spread 0.50% = 0.55, fee 0.10% = 0.11.
	assert.Equal(t, units(110).String(), fill.NotionalAtReference.String())
	assert.Equal(t, "550000", fill.SpreadAmount.String())
	assert.Equal(t, "110000", fill.FeeAmount.String())
	assert.Equal(t, "110660000", fill.TotalQuote.String())
	assert.Equal(t, money.BPS(50), fill.Disclosure.Spread.BPS)
	assert.Equal(t, "660000", f.balance(b.account(ledger.CodeHouseFees, f.usd.ID)).String(), "spread and fee, each on HOUSE_FEES")
	f.requireOnlyHouseLegs(b, fill.JournalTransactionID)
	stored, err := f.svc.Fill(f.ctx, testDB, fill.ID)
	require.NoError(t, err)
	s, err := orders.DecodeSchedule(stored.Schedule)
	require.NoError(t, err)
	require.NoError(t, fees.VerifyDisclosure(s, stored.Disclosure), "every unit is recomputable from the stored record")

	// Rebalancing is a TREASURY book's job, but not this one's strategy.
	wrong, _, err := do2(f, func(ctx context.Context, tx pgx.Tx) (PaperOutcome, bool, error) {
		return f.svc.Rebalance(ctx, tx, RebalanceRequest{HouseAccountID: b.HouseAccountID, Asset: eur.ID, Target: units(40),
			IdempotencyKey: "reb-" + uuid.NewString(), Actor: houseRobot})
	})
	require.NoError(t, err)
	requireRefused(t, wrong.Rejection, wrong.Action, ReasonActivityNotPermitted)
	reb := f.register(econ.HouseInventoryRebalancing, false)
	rp := p
	rp.Label = "rebalancing-" + uuid.NewString()[:8]
	f.setPolicy(reb, rp)
	f.fund(reb, f.usd.ID, units(1_000))
	out, noop, err := do2(f, func(ctx context.Context, tx pgx.Tx) (PaperOutcome, bool, error) {
		return f.svc.Rebalance(ctx, tx, RebalanceRequest{HouseAccountID: reb.HouseAccountID, Asset: eur.ID, Target: units(40),
			IdempotencyKey: "reb-" + uuid.NewString(), Actor: houseRobot})
	})
	require.NoError(t, err)
	require.False(t, noop)
	require.NoError(t, out.Rejection)
	assert.Equal(t, orders.SideBuy, out.Fill.Side)
	assert.Equal(t, units(40).String(), f.inventory(reb, eur.ID).String())
	out, noop, err = do2(f, func(ctx context.Context, tx pgx.Tx) (PaperOutcome, bool, error) {
		return f.svc.Rebalance(ctx, tx, RebalanceRequest{HouseAccountID: reb.HouseAccountID, Asset: eur.ID, Target: units(15),
			IdempotencyKey: "reb-" + uuid.NewString(), Actor: houseRobot})
	})
	require.NoError(t, err)
	require.NoError(t, out.Rejection)
	assert.Equal(t, orders.SideSell, out.Fill.Side)
	assert.Equal(t, units(15).String(), f.inventory(reb, eur.ID).String())
	_, noop, err = do2(f, func(ctx context.Context, tx pgx.Tx) (PaperOutcome, bool, error) {
		return f.svc.Rebalance(ctx, tx, RebalanceRequest{HouseAccountID: reb.HouseAccountID, Asset: eur.ID, Target: units(15),
			IdempotencyKey: "reb-" + uuid.NewString(), Actor: houseRobot})
	})
	require.NoError(t, err)
	assert.True(t, noop, "a holding at its target needs no fill")

	// The way back, then the refusals.
	back, err := do(f, func(ctx context.Context, tx pgx.Tx) (PaperOutcome, error) {
		return f.svc.Convert(ctx, tx, ConvertRequest{HouseAccountID: b.HouseAccountID, From: eur.ID, To: f.usd.ID, Quantity: units(50),
			IdempotencyKey: "back-" + uuid.NewString(), Actor: houseRobot})
	})
	require.NoError(t, err)
	require.NoError(t, back.Rejection)
	assert.Equal(t, orders.SideSell, back.Fill.Side)
	_, err = do(f, func(ctx context.Context, tx pgx.Tx) (PaperOutcome, error) {
		return f.svc.Convert(ctx, tx, ConvertRequest{HouseAccountID: b.HouseAccountID, From: eur.ID, To: eur.ID, Quantity: units(1),
			IdempotencyKey: "loop-" + uuid.NewString(), Actor: houseRobot})
	})
	assert.Equal(t, ReasonValuationAsset, ReasonOf(err))
	f.clk.Advance(31 * time.Second)
	stale, err := do(f, func(ctx context.Context, tx pgx.Tx) (PaperOutcome, error) {
		return f.svc.Convert(ctx, tx, ConvertRequest{HouseAccountID: b.HouseAccountID, From: f.usd.ID, To: eur.ID, Quantity: units(1),
			IdempotencyKey: "stale-" + uuid.NewString(), Actor: houseRobot})
	})
	require.NoError(t, err)
	requireRefused(t, stale.Rejection, stale.Action, ReasonMarkUnavailable)
	assert.Empty(t, f.reconcile(b))
	assert.Empty(t, f.reconcile(reb))
}

// do2 is do for a command with two results.
func do2[A, B any](f *fixture, fn func(ctx context.Context, tx pgx.Tx) (A, B, error)) (A, B, error) {
	var a A
	var b B
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		x, y, err := fn(ctx, tx)
		a, b = x, y
		return err
	})
	return a, b, err
}
