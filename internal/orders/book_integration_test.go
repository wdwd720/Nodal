//go:build integration

package orders

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/matching"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
)

func (f *fixture) reservationStatus(rid string) string {
	f.t.Helper()
	var s string
	require.NoError(f.t, testDB.QueryRow(f.ctx, `SELECT status FROM asset_reservations WHERE id = $1`, rid).Scan(&s))
	return s
}

func (f *fixture) intentStatus(o OrderView) econ.IntentStatus {
	f.t.Helper()
	in, err := f.intents.Get(f.ctx, testDB, *o.IntentID)
	require.NoError(f.t, err)
	return in.Status
}

func (f *fixture) orderView(id OrderID) OrderView {
	f.t.Helper()
	o, err := f.svc.Order(f.ctx, testDB, id)
	require.NoError(f.t, err)
	return o
}

// submitExpectingRejection submits and returns the recorded refusal.
func (f *fixture) submitExpectingRejection(r OrderRequest) (StageOutcome, error) {
	f.t.Helper()
	out, err := f.pipe.Submit(f.ctx, r)
	require.Error(f.t, err)
	return out, err
}

// randomStream drives n customer commands through the service: limit orders
// around 100.00 from several funded accounts, with cancels and replaces.
func (f *fixture) randomStream(m spotMarket, accts []accounts.AccountID, n int, seed uint64) []OrderID {
	f.t.Helper()
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	var resting []OrderID
	for i := 0; i < n; i++ {
		acct := accts[rng.IntN(len(accts))]
		switch op := rng.IntN(10); {
		case op < 2 && len(resting) > 0:
			oid := resting[rng.IntN(len(resting))]
			o := f.orderView(oid)
			_, _ = f.pipe.Cancel(f.ctx, CancelRequest{OrderID: oid, AccountID: *o.AccountID, ActorType: security.ActorUser, ActorID: "u"})
		case op < 3 && len(resting) > 0:
			oid := resting[rng.IntN(len(resting))]
			o := f.orderView(oid)
			if o.Status.Resting() {
				price := fmt.Sprintf("%d.%02d", 99+rng.IntN(3), rng.IntN(100))
				_, _ = f.pipe.Replace(f.ctx, ReplaceRequest{OrderID: oid, AccountID: *o.AccountID, NewPrice: *m.price(f.t, price),
					NewQuantity:    o.FilledQuantity.Add(money.QuantityFromInt64(int64(1+rng.IntN(5)) * 100_000)),
					IdempotencyKey: uuid.NewString(), ActorType: security.ActorUser, ActorID: "u"})
			}
		default:
			side := SideBuy
			if rng.IntN(2) == 0 {
				side = SideSell
			}
			price := fmt.Sprintf("%d.%02d", 99+rng.IntN(3), rng.IntN(100))
			r := f.limit(m, acct, side, price, money.QuantityFromInt64(int64(1+rng.IntN(20))*100_000))
			if rng.IntN(6) == 0 {
				r.TimeInForce = IOC
			}
			out, err := f.pipe.Submit(f.ctx, r)
			if err == nil && out.Order.Status.Resting() {
				resting = append(resting, out.Order.ID)
			}
		}
		f.requireNoCrossedBook(m)
	}
	return resting
}

func (f *fixture) fundedAccounts(m spotMarket, n int) []accounts.AccountID {
	f.t.Helper()
	out := make([]accounts.AccountID, n)
	for i := range out {
		out[i] = f.account()
		f.fund(out[i], m.base.ID, 1_000_000_000)    // 1000 base units
		f.fund(out[i], m.quote.ID, 200_000_000_000) // 200,000 quote units
	}
	return out
}

// TestIntegration_DeterministicReplayYieldsByteEquivalentBookStateThroughTheDB
// proves, through the database, that the journal is the book: a full replay
// from the empty book and a snapshot-plus-tail replay both reproduce every
// recorded event byte for byte and the recorded state hash; a lost in-process
// engine, a second process and a command whose transaction rolled back all
// rebuild from the journal and carry on to the same state.
func TestIntegration_DeterministicReplayYieldsByteEquivalentBookStateThroughTheDB(t *testing.T) {
	f := newFixture(t)
	m := f.spotMarket(marketOpts{makerBPS: 5, takerBPS: 15, snapshotInterval: 7})
	accts := f.fundedAccounts(m, 4)
	f.randomStream(m, accts, 60, 7)

	proof, err := f.svc.VerifyJournal(f.ctx, testDB, m.inst)
	require.NoError(t, err)
	require.Positive(t, proof.Commands)
	assert.Equal(t, proof.StateHash, proof.FullReplayHash)
	assert.Equal(t, proof.StateHash, proof.SnapshotReplayHash)
	assert.Positive(t, proof.SnapshotSeq, "snapshots were taken along the way")
	assert.Positive(t, proof.Trades)
	assert.Positive(t, proof.OrdersChecked)

	// The in-process engine is lost: the next command rebuilds it from the
	// journal and the book carries on.
	f.svc.Forget(proof.BookID)
	f.submit(f.limit(m, accts[0], SideBuy, "98.00", units(1)))
	// A second process, with no engine in memory at all, does the same.
	other := f.newService()
	otherPipe, err := NewPipeline(other, testDB)
	require.NoError(t, err)
	_, err = otherPipe.Submit(f.ctx, f.limit(m, accts[1], SideSell, "102.50", units(1)))
	require.NoError(t, err)
	// And the first process, whose engine is now behind the journal, catches
	// up before its next command.
	f.submit(f.limit(m, accts[2], SideBuy, "102.50", units(1)))

	proof, err = f.svc.VerifyJournal(f.ctx, testDB, m.inst)
	require.NoError(t, err)
	live, err := f.svc.GetBook(f.ctx, testDB, m.inst)
	require.NoError(t, err)
	assert.Equal(t, live.StateHash, proof.FullReplayHash)
	assert.Equal(t, live.StateHash, proof.SnapshotReplayHash)

	// A command applied in memory inside a transaction that then rolls back
	// leaves the engine ahead of the journal; the next command notices and
	// rebuilds rather than trusting memory.
	m2 := f.spotMarket(marketOpts{makerBPS: 5, takerBPS: 15, snapshotInterval: 3})
	pair := f.fundedAccounts(m2, 2)
	staged, err := f.pipe.Submit(f.ctx, f.limit(m2, pair[0], SideBuy, "97.00", units(1)))
	require.NoError(t, err)
	before, err := f.svc.GetBook(f.ctx, testDB, m2.inst)
	require.NoError(t, err)
	var accepted StageOutcome
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		accepted, err = f.svc.Accept(ctx, tx, f.limit(m2, pair[1], SideSell, "97.00", units(1)))
		return err
	}))
	for _, stage := range []func(context.Context, pgx.Tx, OrderID) (StageOutcome, error){f.svc.Approve, f.svc.Reserve} {
		require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error { _, err := stage(ctx, tx, accepted.Order.ID); return err }))
	}
	errRollback := errors.New("roll this command back")
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		out, err := f.svc.Execute(ctx, tx, accepted.Order.ID)
		require.NoError(t, err)
		require.Len(t, out.Trades, 1, "it traded in memory")
		return errRollback
	})
	require.ErrorIs(t, err, errRollback)
	after, err := f.svc.GetBook(f.ctx, testDB, m2.inst)
	require.NoError(t, err)
	assert.Equal(t, before.StateHash, after.StateHash, "the rolled-back command left no trace in the journal")
	assert.Equal(t, StatusOpen, f.orderView(staged.Order.ID).Status, "nor in the resting order it traded with")
	// The order can still execute: the stage is idempotent and the engine is
	// rebuilt from the journal first.
	resumed, err := f.pipe.Resume(f.ctx, accepted.Order.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusFilled, resumed.Order.Status)
	assert.Equal(t, StatusFilled, f.orderView(staged.Order.ID).Status)

	for _, inst := range []spotMarket{m, m2} {
		proof, err := f.svc.VerifyJournal(f.ctx, testDB, inst.inst)
		require.NoError(t, err)
		live, err := f.svc.GetBook(f.ctx, testDB, inst.inst)
		require.NoError(t, err)
		assert.Equal(t, live.StateHash, proof.FullReplayHash)
		assert.Equal(t, live.StateHash, proof.SnapshotReplayHash)
	}
}

// TestIntegration_FIFOWithinAPriceLevel: at one price, resting orders fill in
// the order they arrived.
func TestIntegration_FIFOWithinAPriceLevel(t *testing.T) {
	f := newFixture(t)
	m := f.spotMarket(marketOpts{})
	accts := f.fundedAccounts(m, 4)
	var makers []OrderID
	for _, a := range accts[:3] {
		makers = append(makers, f.submit(f.limit(m, a, SideSell, "100.00", units(1))).Order.ID)
	}
	out := f.submit(f.limit(m, accts[3], SideBuy, "100.00", money.QuantityFromInt64(2_500_000)))
	require.Len(t, out.Trades, 3)
	for i, tr := range out.Trades {
		assert.Equal(t, makers[i], tr.MakerOrderID, "trade %d", i)
	}
	assert.Equal(t, "1000000", out.Trades[0].BaseQuantity.String())
	assert.Equal(t, "1000000", out.Trades[1].BaseQuantity.String())
	assert.Equal(t, "500000", out.Trades[2].BaseQuantity.String())
	assert.Equal(t, StatusFilled, f.orderView(makers[0]).Status)
	assert.Equal(t, StatusFilled, f.orderView(makers[1]).Status)
	assert.Equal(t, StatusPartiallyFilled, f.orderView(makers[2]).Status)
}

// TestIntegration_SelfTradePreventionNeverTradesAnOwnerWithItself: two orders
// of one account never trade; the incoming order's STP mode decides what is
// cancelled, and every cancelled remainder's reservation is handed back.
func TestIntegration_SelfTradePreventionNeverTradesAnOwnerWithItself(t *testing.T) {
	f := newFixture(t)
	m := f.spotMarket(marketOpts{})
	x := f.fundedAccounts(m, 1)[0]
	sell := f.submit(f.limit(m, x, SideSell, "100.00", units(1)))

	// CANCEL_TAKER (the default): the incoming buy is cancelled.
	buy, err := f.pipe.Submit(f.ctx, f.limit(m, x, SideBuy, "100.00", units(1)))
	require.NoError(t, err)
	assert.Empty(t, buy.Trades)
	assert.Equal(t, StatusCancelled, buy.Order.Status)
	assert.Equal(t, string(matching.CancelSTPTaker), buy.Order.CancelReason)
	assert.Equal(t, "RELEASED", f.reservationStatus(buy.Order.ReservationID))
	assert.Equal(t, StatusOpen, f.orderView(sell.Order.ID).Status)

	// CANCEL_MAKER: the resting sell is cancelled and the buy rests.
	r := f.limit(m, x, SideBuy, "100.00", units(1))
	r.STP = STPCancelMaker
	buy2 := f.submit(r)
	assert.Empty(t, buy2.Trades)
	assert.Equal(t, StatusOpen, buy2.Order.Status)
	cancelled := f.orderView(sell.Order.ID)
	assert.Equal(t, StatusCancelled, cancelled.Status)
	assert.Equal(t, string(matching.CancelSTPMaker), cancelled.CancelReason)
	assert.Equal(t, "RELEASED", f.reservationStatus(cancelled.ReservationID))
	assert.Equal(t, econ.StatusCompleted, f.intentStatus(cancelled))
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM book_trades WHERE instrument_id = $1`, m.inst))
}

// TestIntegration_DuplicateClientOrderIDIsRefusedByEngineRuleAndDatabase: a
// client order id is unique per owner among orders that were not rejected,
// under concurrency too, and a rejected order does not consume it.
func TestIntegration_DuplicateClientOrderIDIsRefusedByEngineRuleAndDatabase(t *testing.T) {
	f := newFixture(t)
	m := f.spotMarket(marketOpts{})
	accts := f.fundedAccounts(m, 2)
	x := accts[0]
	first := f.limit(m, x, SideBuy, "90.00", units(1))
	first.ClientOrderID = "client-1"
	f.submit(first)
	dup := f.limit(m, x, SideBuy, "91.00", units(1))
	dup.ClientOrderID = "client-1"
	out, err := f.submitExpectingRejection(dup)
	assert.Equal(t, ReasonDuplicateClientOrderID, ReasonOf(err))
	assert.Equal(t, StatusRejected, out.Order.Status)
	assert.Equal(t, econ.StatusRejected, out.Intent.Status)
	// Another owner may use the same reference.
	other := f.limit(m, accts[1], SideBuy, "90.00", units(1))
	other.ClientOrderID = "client-1"
	f.submit(other)

	// A rejected order does not consume its client order id.
	f.submit(f.limit(m, accts[1], SideSell, "95.00", units(1)))
	po := f.limit(m, x, SideBuy, "95.00", units(1))
	po.PostOnly, po.ClientOrderID = true, "client-2"
	_, err = f.submitExpectingRejection(po)
	assert.Equal(t, string(matching.RejectPostOnlyWouldCross), ReasonOf(err))
	again := f.limit(m, x, SideBuy, "89.00", units(1))
	again.ClientOrderID = "client-2"
	f.submit(again)

	// Under concurrency exactly one of many submissions keeps the id.
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := f.limit(m, x, SideBuy, fmt.Sprintf("8%d.00", i), units(1))
			r.ClientOrderID = "client-race"
			if _, err := f.pipe.Submit(f.ctx, r); err == nil {
				mu.Lock()
				accepted++
				mu.Unlock()
			} else {
				assert.Equal(t, ReasonDuplicateClientOrderID, ReasonOf(err))
			}
		}(i)
	}
	wg.Wait()
	assert.Equal(t, 1, accepted)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM book_orders WHERE owner_id = $1 AND client_order_id = 'client-race' AND status <> 'REJECTED'`, x))
}

// TestIntegration_OrderTypesSettleAndReleaseExactly covers IOC, FOK, market
// with protection, post-only and reduce-only: every fill is consumed from the
// reservation and every unfilled remainder is handed back, to the unit.
func TestIntegration_OrderTypesSettleAndReleaseExactly(t *testing.T) {
	f := newFixture(t)
	m := f.spotMarket(marketOpts{makerBPS: 10, takerBPS: 20})
	accts := f.fundedAccounts(m, 2)
	maker, taker := accts[0], accts[1]
	f.submit(f.limit(m, maker, SideSell, "100.00", units(1)))
	f.submit(f.limit(m, maker, SideSell, "101.00", units(1)))

	// IOC: fills 1 at 100.00, the rest is cancelled and released.
	quoteBefore := f.wallet(taker, m.quote.ID)
	ioc := f.submit(f.order(m, taker, SideBuy, TypeLimit, IOC, "100.50", units(2)))
	require.Len(t, ioc.Trades, 1)
	assert.Equal(t, StatusCancelled, ioc.Order.Status)
	assert.Equal(t, string(matching.CancelIOCRemainder), ioc.Order.CancelReason)
	assert.Equal(t, "RELEASED", f.reservationStatus(ioc.Order.ReservationID))
	paid := quoteBefore.Sub(f.wallet(taker, m.quote.ID))
	assert.Equal(t, "100200000", paid.String(), "100.00 plus a 20 bps taker fee, nothing else")
	avail, err := f.capital.Availability(f.ctx, testDB, taker.String(), m.quote.ID, f.clk.Now())
	require.NoError(t, err)
	assert.True(t, avail.Reserved.IsZero(), "nothing stays reserved")

	// FOK that cannot fill completely: no trade, all released.
	fok, err := f.pipe.Submit(f.ctx, f.order(m, taker, SideBuy, TypeLimit, FOK, "101.00", units(3)))
	require.NoError(t, err)
	assert.Empty(t, fok.Trades)
	assert.Equal(t, string(matching.CancelFOK), fok.Order.CancelReason)
	assert.Equal(t, "RELEASED", f.reservationStatus(fok.Order.ReservationID))

	// Market buy with a protection price sweeps up to it and no further.
	f.submit(f.limit(m, maker, SideSell, "105.00", units(1)))
	mkt := f.submit(f.order(m, taker, SideBuy, TypeMarket, IOC, "102.00", units(5)))
	require.Len(t, mkt.Trades, 1)
	assert.EqualValues(t, 10_100, mkt.Trades[0].PriceTicks)
	assert.Equal(t, string(matching.CancelMarketRemainder), mkt.Order.CancelReason)

	// A market order with nothing within its protection is rejected, and its
	// reservation and intent go with it.
	none, err := f.submitExpectingRejection(f.order(m, taker, SideBuy, TypeMarket, IOC, "102.00", units(1)))
	assert.Equal(t, string(matching.RejectMarketNoLiquidity), ReasonOf(err))
	assert.Equal(t, StatusRejected, none.Order.Status)
	assert.Equal(t, econ.StatusRejected, none.Intent.Status)
	assert.Equal(t, "RELEASED", f.reservationStatus(none.Order.ReservationID))
	// A market order without a protection price cannot even be reserved.
	noProtection := f.order(m, taker, SideBuy, TypeMarket, IOC, "1.00", units(1))
	noProtection.Price = nil
	_, err = f.pipe.Submit(f.ctx, noProtection)
	assert.Equal(t, ReasonPriceRequired, ReasonOf(err))

	// Post-only that would cross is rejected.
	po := f.limit(m, taker, SideBuy, "105.00", units(1))
	po.PostOnly = true
	_, err = f.submitExpectingRejection(po)
	assert.Equal(t, string(matching.RejectPostOnlyWouldCross), ReasonOf(err))

	// Reduce-only: a sell capped at what the seller holds.
	holder := f.account()
	f.fund(holder, m.base.ID, 1_500_000)
	ro := f.limit(m, holder, SideSell, "110.00", units(3))
	ro.ReduceOnly = true
	capped := f.submit(ro)
	assert.Equal(t, StatusOpen, capped.Order.Status)
	assert.EqualValues(t, 30_000, capped.Order.QtyLots)
	assert.EqualValues(t, 15_000, capped.Order.RemainingLots, "only the holdings can be sold")
	assert.EqualValues(t, 15_000, capped.Order.CancelledLots)
	assert.EqualValues(t, 15_000, capped.Order.ReduceOnlyLimitLots)
	nothing := f.account()
	ro2 := f.limit(m, nothing, SideSell, "110.00", units(1))
	ro2.ReduceOnly = true
	_, err = f.submitExpectingRejection(ro2)
	assert.Equal(t, ReasonReduceOnlyNoPosition, ReasonOf(err))
	f.requireNoCrossedBook(m)
}

// TestIntegration_CancelAndReplace covers the customer commands on a resting
// order: cancel hands the reservation back and completes the intent; replace
// moves the reservation to exactly what the new order can spend, and refuses
// rather than half-applies when it cannot.
func TestIntegration_CancelAndReplace(t *testing.T) {
	f := newFixture(t)
	m := f.spotMarket(marketOpts{makerBPS: 10, takerBPS: 20})
	accts := f.fundedAccounts(m, 2)
	x, y := accts[0], accts[1]

	resting := f.submit(f.limit(m, x, SideBuy, "95.00", units(1)))
	_, err := f.pipe.Cancel(f.ctx, CancelRequest{OrderID: resting.Order.ID, AccountID: y, ActorType: security.ActorUser, ActorID: "y"})
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err), "only the owner cancels")
	c, err := f.pipe.Cancel(f.ctx, CancelRequest{OrderID: resting.Order.ID, AccountID: x, ActorType: security.ActorUser, ActorID: "x"})
	require.NoError(t, err)
	assert.Equal(t, StatusCancelled, c.Order.Status)
	assert.Equal(t, "RELEASED", f.reservationStatus(c.Order.ReservationID))
	assert.Equal(t, econ.StatusCompleted, f.intentStatus(c.Order))
	again, err := f.pipe.Cancel(f.ctx, CancelRequest{OrderID: resting.Order.ID, AccountID: x, ActorType: security.ActorUser, ActorID: "x"})
	require.NoError(t, err, "cancelling a cancelled order is a no-op")
	assert.Zero(t, again.CommandSeq)

	// Replace up: the reservation is resized to the new price, the old one
	// released.
	buy := f.submit(f.limit(m, x, SideBuy, "95.00", units(1)))
	key := uuid.NewString()
	rep, err := f.pipe.Replace(f.ctx, ReplaceRequest{OrderID: buy.Order.ID, AccountID: x, NewPrice: *m.price(t, "96.00"),
		NewQuantity: units(2), IdempotencyKey: key, ActorType: security.ActorUser, ActorID: "x"})
	require.NoError(t, err)
	assert.EqualValues(t, 9_600, rep.Order.PriceTicks)
	assert.EqualValues(t, 20_000, rep.Order.QtyLots)
	assert.NotEqual(t, buy.Order.ReservationID, rep.Order.ReservationID)
	assert.Equal(t, "RELEASED", f.reservationStatus(buy.Order.ReservationID))
	res, err := f.capital.Get(f.ctx, testDB, mustReservation(t, rep.Order.ReservationID))
	require.NoError(t, err)
	// 2 units at 96.00 is 192 quote units, plus at most 20 bps of fees.
	assert.Equal(t, "192384000", res.Quantity.String())
	replay, err := f.pipe.Replace(f.ctx, ReplaceRequest{OrderID: buy.Order.ID, AccountID: x, NewPrice: *m.price(t, "96.00"),
		NewQuantity: units(2), IdempotencyKey: key, ActorType: security.ActorUser, ActorID: "x"})
	require.NoError(t, err)
	assert.Zero(t, replay.CommandSeq, "a replayed replace changes nothing")

	// A replace the account cannot afford is refused and leaves the order and
	// its reservation exactly as they were.
	poor := f.account()
	f.fund(poor, m.quote.ID, 120_000_000)
	small := f.submit(f.limit(m, poor, SideBuy, "100.00", units(1)))
	_, err = f.pipe.Replace(f.ctx, ReplaceRequest{OrderID: small.Order.ID, AccountID: poor, NewPrice: *m.price(t, "100.00"),
		NewQuantity: units(5), IdempotencyKey: uuid.NewString(), ActorType: security.ActorUser, ActorID: "p"})
	require.Error(t, err)
	unchanged := f.orderView(small.Order.ID)
	assert.EqualValues(t, 10_000, unchanged.QtyLots)
	assert.Equal(t, small.Order.ReservationID, unchanged.ReservationID)
	assert.Equal(t, "ACTIVE", f.reservationStatus(unchanged.ReservationID))

	// A replace that crosses trades as the aggressor (the ask sits above the
	// resting 100.00 bid, so only the replace reaches it).
	f.submit(f.limit(m, y, SideSell, "101.00", units(1)))
	cross, err := f.pipe.Replace(f.ctx, ReplaceRequest{OrderID: buy.Order.ID, AccountID: x, NewPrice: *m.price(t, "101.00"),
		NewQuantity: units(2), IdempotencyKey: uuid.NewString(), ActorType: security.ActorUser, ActorID: "x"})
	require.NoError(t, err)
	require.Len(t, cross.Trades, 1)
	assert.Equal(t, StatusPartiallyFilled, cross.Order.Status)

	// An order cancelled before it reached the book is cancelled with its
	// intent.
	var accepted StageOutcome
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		accepted, err = f.svc.Accept(ctx, tx, f.limit(m, x, SideBuy, "90.00", units(1)))
		return err
	}))
	pre, err := f.pipe.Cancel(f.ctx, CancelRequest{OrderID: accepted.Order.ID, AccountID: x, ActorType: security.ActorUser, ActorID: "x"})
	require.NoError(t, err)
	assert.Equal(t, StatusCancelled, pre.Order.Status)
	assert.Equal(t, ReasonCancelledBeforeBook, pre.Order.CancelReason)
	assert.Equal(t, econ.StatusCancelled, f.intentStatus(pre.Order))
	f.requireNoCrossedBook(m)
}

// TestIntegration_ReplayOfASubmissionProducesNoSecondEffect: the same
// submission twice is one order, one command, one effect.
func TestIntegration_ReplayOfASubmissionProducesNoSecondEffect(t *testing.T) {
	f := newFixture(t)
	m := f.spotMarket(marketOpts{})
	x := f.fundedAccounts(m, 1)[0]
	r := f.limit(m, x, SideBuy, "99.00", units(1))
	first := f.submit(r)
	second := f.submit(r)
	assert.Equal(t, first.Order.ID, second.Order.ID)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM economic_effects WHERE effect_key = $1`, "orders:submit:"+first.Order.ID.String()))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM book_commands WHERE order_id = $1`, first.Order.ID))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM asset_reservations WHERE idempotency_key = $1`, "orders:reserve:"+first.Order.ID.String()))
	_, err := f.pipe.Resume(f.ctx, first.Order.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM book_commands WHERE order_id = $1`, first.Order.ID))

	// The same key with different content is refused.
	changed := r
	changed.Quantity = units(2)
	_, err = f.pipe.Submit(f.ctx, changed)
	assert.Equal(t, errs.CodeInvalidIdempotencyReuse, errs.CodeOf(err))
}

// TestIntegration_RefusalsAreRecordedAndFailClosed: an unverified identity,
// a stale mark, a halted book and an unaffordable order are each refused with
// a stable reason, recorded on the intent and the order, and never post.
func TestIntegration_RefusalsAreRecordedAndFailClosed(t *testing.T) {
	f := newFixture(t)
	m := f.spotMarket(marketOpts{})
	x := f.fundedAccounts(m, 1)[0]

	stranger := f.unverifiedAccount()
	f.fund(stranger, m.quote.ID, 1_000_000_000)
	out, err := f.submitExpectingRejection(f.limit(m, stranger, SideBuy, "99.00", units(1)))
	assert.Equal(t, errs.CodeRiskRejected, errs.CodeOf(err))
	assert.Contains(t, out.Order.RejectReason, risk.ReasonIdentityUnverified)
	assert.Equal(t, econ.StatusRejected, out.Intent.Status)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM advanced_risk_decisions WHERE intent_id = $1 AND verdict = 'REJECT'`, out.Intent.ID))

	poor := f.account()
	f.fund(poor, m.quote.ID, 1_000_000)
	_, err = f.submitExpectingRejection(f.limit(m, poor, SideBuy, "99.00", units(1)))
	assert.Equal(t, errs.CodeRiskRejected, errs.CodeOf(err))

	// A stale mark blocks a buy (new risk) but not a sell (risk reduction).
	f.clk.Advance(time.Hour)
	stale, err := f.submitExpectingRejection(f.limit(m, x, SideBuy, "99.00", units(1)))
	assert.Contains(t, stale.Order.RejectReason, risk.ReasonOracleStale)
	f.submit(f.limit(m, x, SideSell, "101.00", units(1)))
	f.mark(m, "100.00")

	// A halted book refuses new orders and still accepts cancels.
	resting := f.submit(f.limit(m, x, SideBuy, "98.00", units(1)))
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.SetBookStatus(ctx, tx, m.inst, BookHalted, systemActor, "operator halt")
		return err
	}))
	_, err = f.pipe.Submit(f.ctx, f.limit(m, x, SideBuy, "98.00", units(1)))
	assert.Equal(t, ReasonBookNotOpen, ReasonOf(err))
	_, err = f.pipe.Cancel(f.ctx, CancelRequest{OrderID: resting.Order.ID, AccountID: x, ActorType: security.ActorUser, ActorID: "x"})
	require.NoError(t, err)
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM book_trades WHERE instrument_id = $1`, m.inst))
}

// TestIntegration_SingleTransactionSubmitIsAtomic: Service.Submit carries an
// order through every stage in one transaction; rolled back, it leaves
// nothing behind -- no intent, no order, no command -- and the book carries
// on from its journal.
func TestIntegration_SingleTransactionSubmitIsAtomic(t *testing.T) {
	f := newFixture(t)
	m := f.spotMarket(marketOpts{makerBPS: 10, takerBPS: 20})
	accts := f.fundedAccounts(m, 2)
	var rest StageOutcome
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rest, err = f.svc.submitAtomic(ctx, tx, f.limit(m, accts[0], SideSell, "100.00", units(1)))
		return err
	}))
	assert.Equal(t, StatusOpen, rest.Order.Status)
	assert.Equal(t, econ.StatusExecuting, rest.Intent.Status)

	r := f.limit(m, accts[1], SideBuy, "100.00", units(1))
	errRollback := errors.New("roll back")
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		out, err := f.svc.submitAtomic(ctx, tx, r)
		require.NoError(t, err)
		require.Equal(t, StatusFilled, out.Order.Status)
		return errRollback
	})
	require.ErrorIs(t, err, errRollback)
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM economic_intents WHERE account_id = $1 AND idempotency_key = $2`, accts[1], r.IdempotencyKey))
	assert.Equal(t, StatusOpen, f.orderView(rest.Order.ID).Status)

	var filled StageOutcome
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		filled, err = f.svc.submitAtomic(ctx, tx, r)
		return err
	}))
	assert.Equal(t, StatusFilled, filled.Order.Status)
	assert.Equal(t, econ.StatusCompleted, filled.Intent.Status, "every intent step happened in the one transaction")
	assert.Len(t, filled.Trades, 1)
	_, err = f.svc.VerifyJournal(f.ctx, testDB, m.inst)
	require.NoError(t, err)
}

func mustReservation(t *testing.T, s string) capital.ReservationID {
	t.Helper()
	rid, err := capital.ParseReservationID(s)
	require.NoError(t, err)
	return rid
}
