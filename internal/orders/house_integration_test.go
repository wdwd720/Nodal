//go:build integration

package orders

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

func (f *fixture) houseOrder(m spotMarket, h econ.HouseAccount, side Side, price string, qty money.Quantity) HouseOrderRequest {
	return HouseOrderRequest{InstrumentID: m.inst, HouseAccountID: h.ID, Side: side, Type: TypeLimit, TimeInForce: GTC,
		Price: m.price(f.t, price), Quantity: qty, IdempotencyKey: "house-" + uuid.NewString(), Actor: systemActor}
}

// TestIntegration_HouseCustomerCrossingIsRejectedByDefault: a house book never
// trades with a customer unless a versioned crossing rule allows it -- decided
// exactly by a dry run, before the command touches the book, in both
// directions -- and every decision names the rule version it applied, with
// archived evidence. House books trade with each other freely.
func TestIntegration_HouseCustomerCrossingIsRejectedByDefault(t *testing.T) {
	f := newFixture(t)
	m := f.spotMarket(marketOpts{makerBPS: 10, takerBPS: 20})
	h := f.houseBook("crossing")
	f.fundHouse(h, m.base.ID, units(10))
	f.fundHouse(h, m.quote.ID, units(10_000))
	h2 := f.houseBook("crossing-2")
	f.fundHouse(h2, m.quote.ID, units(10_000))
	customers := f.fundedAccounts(m, 2)
	customer, customer2 := customers[0], customers[1]

	// House books trade with each other freely, and pay the platform no fee.
	_, err := f.submitHouse(f.houseOrder(m, h, SideSell, "105.00", units(1)))
	require.NoError(t, err)
	hh, err := f.submitHouse(f.houseOrder(m, h2, SideBuy, "105.00", units(1)))
	require.NoError(t, err)
	require.Len(t, hh.Trades, 1)
	assert.Equal(t, 0, hh.Trades[0].CrossingRuleVersion)
	assert.True(t, hh.Trades[0].MakerFee.IsZero() && hh.Trades[0].TakerFee.IsZero())
	assert.Equal(t, units(1).String(), f.balance(h2.Account(ledger.CodeHouseInventory, m.base.ID)).String())
	assert.Equal(t, units(9).String(), f.balance(h.Account(ledger.CodeHouseInventory, m.base.ID)).String())
	assert.Equal(t, units(1).String(), f.balance(h2.Account(ledger.CodeHouseTradingInflow, m.base.ID)).String())

	houseSell, err := f.submitHouse(f.houseOrder(m, h, SideSell, "105.00", units(1)))
	require.NoError(t, err)
	assert.Equal(t, StatusOpen, houseSell.Order.Status)
	customerSell := f.submit(f.limit(m, customer, SideSell, "100.00", units(1)))

	// House buy against the customer's resting sell: refused before the book.
	before, err := f.svc.GetBook(f.ctx, testDB, m.inst)
	require.NoError(t, err)
	out, err := f.submitHouse(f.houseOrder(m, h, SideBuy, "100.00", units(1)))
	require.Error(t, err)
	assert.Equal(t, ReasonHouseCrossingForbidden, ReasonOf(err))
	assert.Equal(t, StatusRejected, out.Order.Status)
	assert.Zero(t, out.CommandSeq, "the book was never touched")
	after, err := f.svc.GetBook(f.ctx, testDB, m.inst)
	require.NoError(t, err)
	assert.Equal(t, before.StateHash, after.StateHash)
	assert.Equal(t, StatusOpen, f.orderView(customerSell.Order.ID).Status)
	var wouldCross, allowed bool
	var version int
	var evidence string
	var digest []byte
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT would_cross, allowed, rule_version, evidence_ref, evidence_sha256
		FROM book_crossing_decisions WHERE order_id = $1`, out.Order.ID).Scan(&wouldCross, &allowed, &version, &evidence, &digest))
	assert.True(t, wouldCross)
	assert.False(t, allowed)
	assert.Equal(t, 0, version, "the default policy, with no rule on record")
	f.requireArchived(evidence, digest)
	assert.Equal(t, units(9).String(), f.balance(h.Account(ledger.CodeHouseInventory, m.base.ID)).String())

	// A customer buy that would reach the resting house sell is refused as a
	// whole, even though it would first have traded with another customer.
	cust, err := f.pipe.Submit(f.ctx, f.limit(m, customer2, SideBuy, "105.00", units(2)))
	require.Error(t, err)
	assert.Equal(t, ReasonHouseCrossingForbidden, ReasonOf(err))
	assert.Equal(t, econ.StatusRejected, cust.Intent.Status)
	assert.Equal(t, "RELEASED", f.reservationStatus(cust.Order.ReservationID))
	assert.Equal(t, StatusOpen, f.orderView(customerSell.Order.ID).Status)
	assert.Equal(t, StatusOpen, f.orderView(houseSell.Order.ID).Status)

	// A versioned sandbox rule enables crossing; the trade names its version.
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		r, err := f.svc.SetCrossingRule(ctx, tx, m.inst, true, systemActor, "sandbox: let the house make markets for customers")
		assert.Equal(t, 1, r.Version)
		return err
	}))
	crossed, err := f.submitHouse(f.houseOrder(m, h, SideBuy, "100.00", units(1)))
	require.NoError(t, err)
	require.Len(t, crossed.Trades, 1)
	assert.Equal(t, 1, crossed.Trades[0].CrossingRuleVersion)
	assert.Equal(t, customerSell.Order.ID, crossed.Trades[0].MakerOrderID)
	assert.True(t, crossed.Trades[0].TakerFee.IsZero(), "the house pays no fee")
	assert.Equal(t, "100000", crossed.Trades[0].MakerFee.String(), "the customer pays its own maker fee")
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT allowed, rule_version FROM book_crossing_decisions WHERE order_id = $1`,
		crossed.Order.ID).Scan(&allowed, &version))
	assert.True(t, allowed)
	assert.Equal(t, 1, version)

	// Version 2 forbids it again.
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.SetCrossingRule(ctx, tx, m.inst, false, systemActor, "sandbox experiment over")
		return err
	}))
	f.submit(f.limit(m, customer, SideSell, "100.00", units(1)))
	_, err = f.submitHouse(f.houseOrder(m, h, SideBuy, "100.00", units(1)))
	assert.Equal(t, ReasonHouseCrossingForbidden, ReasonOf(err))
	f.requireNoCrossedBook(m)
}

// TestIntegration_HouseReservationsNeverOverspendInventory: a house book
// reserves its HOUSE_INVENTORY under its own row lock, so concurrent orders
// can never commit more than it holds; a cancel hands the reservation back;
// a house order is idempotent and never placed by an agent.
func TestIntegration_HouseReservationsNeverOverspendInventory(t *testing.T) {
	f := newFixture(t)
	m := f.spotMarket(marketOpts{})
	h := f.houseBook("inventory")
	f.fundHouse(h, m.base.ID, units(2))
	var wg sync.WaitGroup
	results := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.submitHouse(f.houseOrder(m, h, SideSell, "120.00", money.QuantityFromInt64(1_500_000)))
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	ok := 0
	for err := range results {
		if err == nil {
			ok++
		} else {
			assert.Equal(t, ReasonHouseInventory, ReasonOf(err))
		}
	}
	assert.Equal(t, 1, ok, "2 units of inventory cover one order of 1.5")
	var reserved string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT coalesce(sum(quantity - consumed_quantity), 0)::text FROM book_house_reservations
		WHERE house_account_id = $1 AND status = 'ACTIVE'`, h.ID).Scan(&reserved))
	assert.Equal(t, "1500000", reserved)

	var resting OrderID
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT id FROM book_orders WHERE house_account_id = $1 AND status = 'OPEN'`, h.ID).Scan(&resting))
	c, err := f.pipe.command(f.ctx, func(ctx context.Context, tx pgx.Tx) (CommandOutcome, error) {
		return f.svc.CancelHouseOrder(ctx, tx, h.ID, resting, systemActor)
	})
	require.NoError(t, err)
	assert.Equal(t, StatusCancelled, c.Order.Status)
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT coalesce(sum(quantity - consumed_quantity), 0)::text FROM book_house_reservations
		WHERE house_account_id = $1 AND status = 'ACTIVE'`, h.ID).Scan(&reserved))
	assert.Equal(t, "0", reserved)

	r := f.houseOrder(m, h, SideSell, "120.00", units(1))
	first, err := f.submitHouse(r)
	require.NoError(t, err)
	second, err := f.submitHouse(r)
	require.NoError(t, err)
	assert.Equal(t, first.Order.ID, second.Order.ID, "a replay returns the first order")
	changed := r
	changed.Quantity = money.QuantityFromInt64(1_100_000)
	_, err = f.submitHouse(changed)
	assert.Equal(t, errs.CodeInvalidIdempotencyReuse, errs.CodeOf(err))
	agent := f.houseOrder(m, h, SideSell, "120.00", units(1))
	agent.Actor = econ.Actor{Type: security.ActorAgent, ID: "agent-1"}
	_, err = f.submitHouse(agent)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
}
