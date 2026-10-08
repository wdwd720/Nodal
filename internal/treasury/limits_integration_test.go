//go:build integration

package treasury

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/orders"
	"github.com/nodal/controlplane/internal/risk"
)

// requireRefused checks that an outcome is a recorded refusal carrying the
// given reason, and returns its action.
func requireRefused(t *testing.T, rej error, a Action, reason string) {
	t.Helper()
	require.Error(t, rej, "expected a refusal carrying %s", reason)
	assert.Equal(t, OutcomeRejected, a.Outcome)
	assert.Contains(t, append([]string{a.RejectionCode}, a.ReasonCodes...), reason, "reasons: %v", a.ReasonCodes)
}

// TestIntegration_TreasuryLossLimitStopsNewRisk: once the loss measure
// (realized PnL of the day plus every open loss at fresh marks) passes the
// daily loss limit, every risk-increasing house order and fill is refused --
// through Risk Kernel V2 as a circuit breaker, persisted with the limit
// evaluation -- while sales that reduce risk still go through. The next day
// the realized part resets; the open loss still counts.
func TestIntegration_TreasuryLossLimitStopsNewRisk(t *testing.T) {
	f := newFixture(t)
	m := f.market("100.00")
	b := f.register(econ.HouseMarketMaking, false)
	p := f.policy(m)
	p.DailyLossLimit = units(50)
	f.setPolicy(b, p)
	f.fund(b, f.usd.ID, units(10_000))
	require.NoError(t, f.paper(b, econ.HouseHedging, m.base.ID, orders.SideBuy, units(10)).Rejection)

	// The mark falls 6%: 10 units lose 60, past the limit of 50.
	f.mark(m.base.ID, "94.00")
	ordersBefore := f.count(`SELECT count(*) FROM book_orders WHERE house_account_id = $1`, b.HouseAccountID)
	buy := f.place(f.limit(b, m, econ.HouseMarketMaking, orders.SideBuy, "90.00", units(1)))
	requireRefused(t, buy.Rejection, buy.Action, ReasonDailyLossLimit)
	assert.Equal(t, errs.CodeRiskRejected, errs.CodeOf(buy.Rejection))
	assert.Contains(t, buy.Action.ReasonCodes, risk.ReasonCircuitBreaker, "the limit reached the kernel as a circuit breaker")
	require.NotNil(t, buy.Decision)
	assert.Equal(t, risk.Reject, buy.Decision.Verdict)
	assert.Contains(t, buy.Decision.TreasuryCodes, ReasonDailyLossLimit)
	assert.Equal(t, "-60000000", buy.Decision.Limits.LossMeasure.String())
	assert.Equal(t, ordersBefore, f.count(`SELECT count(*) FROM book_orders WHERE house_account_id = $1`, b.HouseAccountID),
		"nothing reached the order book")
	var verdict string
	var codes []string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT verdict, treasury_codes FROM treasury_risk_decisions WHERE id = $1`,
		buy.Decision.ID).Scan(&verdict, &codes))
	assert.Equal(t, "REJECT", verdict)
	assert.Contains(t, codes, ReasonDailyLossLimit)

	paperBuy := f.paper(b, econ.HouseHedging, m.base.ID, orders.SideBuy, units(1))
	requireRefused(t, paperBuy.Rejection, paperBuy.Action, ReasonDailyLossLimit)

	// Reducing risk is never blocked by a loss: a resting ask and a paper sale.
	ask := f.place(f.limit(b, m, econ.HouseMarketMaking, orders.SideSell, "99.00", units(2)))
	require.NoError(t, ask.Rejection)
	assert.Equal(t, orders.StatusOpen, ask.Order.Status)
	sell := f.paper(b, econ.HouseHedging, m.base.ID, orders.SideSell, units(5))
	require.NoError(t, sell.Rejection)
	rep, err := do(f, func(ctx context.Context, tx pgx.Tx) (Report, error) { return f.svc.Report(ctx, tx, b.HouseAccountID) })
	require.NoError(t, err)
	assert.Equal(t, "-30000000", rep.RealizedToday.String(), "5 sold at 94 against an average cost of 100")
	assert.Equal(t, "-30000000", rep.OpenLosses.String(), "5 still held at 94")
	assert.Equal(t, "-60000000", rep.LossMeasure.String())

	// The next trading day the realized loss no longer counts; the open loss
	// of 30 does, and is inside the limit: new risk is allowed again.
	f.clk.Advance(24 * time.Hour)
	f.mark(m.base.ID, "94.00")
	again := f.place(f.limit(b, m, econ.HouseMarketMaking, orders.SideBuy, "90.00", units(1)))
	require.NoError(t, again.Rejection)
	assert.Equal(t, "-30000000", again.Decision.Limits.LossMeasure.String())
	assert.Empty(t, f.reconcile(b))
}

// TestIntegration_InventoryCapIsEnforced: the cap on an asset counts what
// the house book holds plus every working buy plus the new request, on the
// book and on the paper venue alike; an asset with no cap cannot be
// accumulated at all; a cancel frees the room it held.
func TestIntegration_InventoryCapIsEnforced(t *testing.T) {
	f := newFixture(t)
	m := f.market("100.00")
	b := f.register(econ.HouseMarketMaking, false)
	p := f.policy(m)
	p.MaxInventory = []AssetLimit{{Asset: m.base.ID, MaxQuantity: units(5)}}
	f.setPolicy(b, p)
	f.fund(b, f.usd.ID, units(10_000))

	require.NoError(t, f.paper(b, econ.HouseHedging, m.base.ID, orders.SideBuy, units(3)).Rejection)
	resting := f.place(f.limit(b, m, econ.HouseMarketMaking, orders.SideBuy, "90.00", units(2)))
	require.NoError(t, resting.Rejection, "3 held + 2 working = 5, at the cap")
	assert.Equal(t, units(5).String(), resting.Decision.Limits.InventoryAfter.String())

	over := f.place(f.limit(b, m, econ.HouseMarketMaking, orders.SideBuy, "90.00", units(1)))
	requireRefused(t, over.Rejection, over.Action, ReasonInventoryCap)
	assert.Equal(t, units(6).String(), over.Decision.Limits.InventoryAfter.String())
	paperOver := f.paper(b, econ.HouseHedging, m.base.ID, orders.SideBuy, units(1))
	requireRefused(t, paperOver.Rejection, paperOver.Action, ReasonInventoryCap)

	// A sale is never capped.
	require.NoError(t, f.paper(b, econ.HouseHedging, m.base.ID, orders.SideSell, units(1)).Rejection)

	// Cancelling the working buy frees its room: 2 held + 1 = 3.
	c, err := do(f, func(ctx context.Context, tx pgx.Tx) (OrderOutcome, error) {
		return f.svc.CancelOrder(ctx, tx, CancelRequest{HouseAccountID: b.HouseAccountID, OrderID: resting.Order.ID,
			IdempotencyKey: "cancel-" + uuid.NewString(), Actor: houseRobot})
	})
	require.NoError(t, err)
	require.NoError(t, c.Rejection)
	assert.Equal(t, orders.StatusCancelled, c.Order.Status)
	require.NoError(t, f.paper(b, econ.HouseHedging, m.base.ID, orders.SideBuy, units(3)).Rejection, "2 held + 3 = 5")
	assert.Equal(t, units(5).String(), f.inventory(b, m.base.ID).String())

	// An asset the policy does not name cannot be accumulated.
	other := f.simAsset("TRX")
	f.mark(other.ID, "10.00")
	unknown := f.paper(b, econ.HouseHedging, other.ID, orders.SideBuy, units(1))
	requireRefused(t, unknown.Rejection, unknown.Action, ReasonAssetNotPermitted)
	assert.Empty(t, f.reconcile(b))
}

// TestIntegration_NoCustomerFundsAreAvailableToTheHouseStrategy: a house
// book can only ever spend its own HOUSE_INVENTORY. With a richly funded
// customer and an unfunded house book, every house order and fill is refused
// -- by the treasury, and by the order book's own house reservation when the
// treasury is bypassed -- and nothing of the customer's is touched.
func TestIntegration_NoCustomerFundsAreAvailableToTheHouseStrategy(t *testing.T) {
	f := newFixture(t)
	m := f.market("100.00")
	rich := f.customer(f.usd.ID, units(1_000_000))
	_ = f.customer(m.base.ID, units(1_000))
	b := f.register(econ.HouseMarketMaking, false)
	f.setPolicy(b, f.policy(m))
	walletBefore := f.balance(ledger.CustomerAccount(rich, ledger.CodeWallet, f.usd.ID))

	buy := f.place(f.limit(b, m, econ.HouseMarketMaking, orders.SideBuy, "100.00", units(1)))
	requireRefused(t, buy.Rejection, buy.Action, risk.ReasonInsufficientBalance)
	sell := f.place(f.limit(b, m, econ.HouseMarketMaking, orders.SideSell, "100.00", units(1)))
	requireRefused(t, sell.Rejection, sell.Action, risk.ReasonInsufficientBalance)
	paper := f.paper(b, econ.HouseHedging, m.base.ID, orders.SideBuy, units(1))
	requireRefused(t, paper.Rejection, paper.Action, ReasonInsufficientInventory)

	// Bypassing the treasury does not help: a house order exists only as the
	// result of a treasury action (00759), so an order placed through the
	// order book alone is refused at COMMIT and leaves nothing behind.
	bypass := "bypass-" + uuid.NewString()
	_, err := f.pipe.SubmitHouse(f.ctx, orders.HouseOrderRequest{
		InstrumentID: m.inst, HouseAccountID: b.HouseAccountID, Side: orders.SideBuy, Type: orders.TypeLimit,
		TimeInForce: orders.GTC, Price: f.price("100.00"), Quantity: units(1), IdempotencyKey: bypass, Actor: houseRobot,
	})
	require.Error(t, err)
	assert.Equal(t, "TY007", db.SQLState(err), "%v", err)
	assert.Zero(t, f.count(`SELECT count(*) FROM book_orders WHERE owner_id = $1 AND idempotency_key = $2`, b.HouseAccountID, bypass))

	assert.Equal(t, walletBefore.String(), f.balance(ledger.CustomerAccount(rich, ledger.CodeWallet, f.usd.ID)).String())
	assert.Zero(t, f.count(`SELECT count(*) FROM asset_reservations WHERE account_id = $1`, rich))
	balances, err := f.svc.Balances(f.ctx, testDB, b.HouseAccountID)
	require.NoError(t, err)
	for _, bal := range balances {
		assert.True(t, bal.Inventory.IsZero(), "the house book holds nothing of %s", bal.Asset)
	}
	// Every house account that exists belongs to this house book's segment.
	assert.Zero(t, f.count(`SELECT count(*) FROM ledger_accounts WHERE owner_id = $1 AND owner_type <> 'HOUSE'`, b.HouseAccountID))
}

// TestIntegration_KillSwitchPreventsNewHouseOrders: a CAPABILITY_DISABLE
// switch scoped to PRINCIPAL_TRADING, and the global kill, stop every new
// house order and fill -- recorded as refusals -- but never cancel a resting
// order or trade to reduce risk; cancels stay available.
func TestIntegration_KillSwitchPreventsNewHouseOrders(t *testing.T) {
	f := newFixture(t)
	m := f.market("100.00")
	b := f.register(econ.HouseMarketMaking, true)
	f.setPolicy(b, f.policy(m))
	f.fund(b, f.usd.ID, units(10_000))
	require.NoError(t, f.paper(b, econ.HouseHedging, m.base.ID, orders.SideBuy, units(4)).Rejection)
	resting := f.place(f.limit(b, m, econ.HouseMarketMaking, orders.SideBuy, "95.00", units(1)))
	require.NoError(t, resting.Rejection)
	ask := f.place(f.limit(b, m, econ.HouseMarketMaking, orders.SideSell, "105.00", units(1)))
	require.NoError(t, ask.Rejection)

	for _, sw := range []killswitch.Switch{
		{Kind: killswitch.CapabilityDisable, ScopeID: string(gates.PrincipalTrading), Active: true},
		{Kind: killswitch.GlobalNewRiskKill, ScopeID: killswitch.GlobalScope, Active: true},
	} {
		f.kills.set(sw)
		commandsBefore := f.count(`SELECT count(*) FROM book_commands`)
		fillsBefore := f.count(`SELECT count(*) FROM treasury_paper_fills WHERE house_account_id = $1`, b.HouseAccountID)
		for _, side := range orders.AllSides() {
			out := f.place(f.limit(b, m, econ.HouseMarketMaking, side, "100.00", units(1)))
			requireRefused(t, out.Rejection, out.Action, ReasonKillSwitch)
			assert.Equal(t, errs.CodeKillSwitchActive, errs.CodeOf(out.Rejection), "%s %s", sw.Kind, side)
			paper := f.paper(b, econ.HouseHedging, m.base.ID, side, units(1))
			requireRefused(t, paper.Rejection, paper.Action, ReasonKillSwitch)
		}
		assert.Equal(t, commandsBefore, f.count(`SELECT count(*) FROM book_commands`), "%s: nothing reached the book", sw.Kind)
		assert.Equal(t, fillsBefore, f.count(`SELECT count(*) FROM treasury_paper_fills WHERE house_account_id = $1`, b.HouseAccountID))
		// The switch cancelled nothing and traded nothing.
		for _, o := range []*orders.OrderView{resting.Order, ask.Order} {
			view, err := f.orders.Order(f.ctx, testDB, o.ID)
			require.NoError(t, err)
			assert.Equal(t, orders.StatusOpen, view.Status, "%s left %s resting", sw.Kind, o.Side)
		}
	}

	// A cancel is not new risk: it works while the switch is on.
	c, err := do(f, func(ctx context.Context, tx pgx.Tx) (OrderOutcome, error) {
		return f.svc.CancelOrder(ctx, tx, CancelRequest{HouseAccountID: b.HouseAccountID, OrderID: resting.Order.ID,
			IdempotencyKey: "cancel-" + uuid.NewString(), Actor: operator})
	})
	require.NoError(t, err)
	require.NoError(t, c.Rejection)
	assert.Equal(t, orders.StatusCancelled, c.Order.Status)

	f.kills.set()
	require.NoError(t, f.place(f.limit(b, m, econ.HouseMarketMaking, orders.SideBuy, "95.00", units(1))).Rejection,
		"with the switch released, new orders are accepted again")
}

// TestIntegration_ConcurrentHouseOrdersCannotBreachALimitTogether: eight
// house buys race for an inventory cap with room for five. Each fits alone;
// under the book's lock exactly five are accepted and the worst-case
// inventory never passes the cap.
func TestIntegration_ConcurrentHouseOrdersCannotBreachALimitTogether(t *testing.T) {
	f := newFixture(t)
	m := f.market("100.00")
	b := f.register(econ.HouseMarketMaking, false)
	p := f.policy(m)
	p.MaxInventory = []AssetLimit{{Asset: m.base.ID, MaxQuantity: units(5)}}
	f.setPolicy(b, p)
	f.fund(b, f.usd.ID, units(10_000))

	const n = 8
	var wg sync.WaitGroup
	results := make(chan OrderOutcome, n)
	failures := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := do(f, func(ctx context.Context, tx pgx.Tx) (OrderOutcome, error) {
				return f.svc.PlaceOrder(ctx, tx, f.limit(b, m, econ.HouseMarketMaking, orders.SideBuy, "90.00", units(1)))
			})
			if err != nil {
				failures <- err
				return
			}
			results <- out
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Fatalf("a command failed instead of being decided: %v", err)
	}
	accepted, refused := 0, 0
	for out := range results {
		if out.Rejection == nil {
			accepted++
			continue
		}
		refused++
		assert.Contains(t, out.Action.ReasonCodes, ReasonInventoryCap)
	}
	assert.Equal(t, 5, accepted)
	assert.Equal(t, 3, refused)
	var working string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT coalesce(sum(remaining_lots), 0)::text FROM book_orders
		WHERE house_account_id = $1 AND side = 'BUY' AND status = 'OPEN'`, b.HouseAccountID).Scan(&working))
	assert.Equal(t, "50000", working, "five working buys of 10,000 lots: exactly the cap")
	assert.Equal(t, 8, f.count(`SELECT count(*) FROM treasury_actions WHERE house_account_id = $1 AND kind = 'PLACE_ORDER'`, b.HouseAccountID))
}
