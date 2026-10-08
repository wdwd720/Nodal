//go:build integration

package orders

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// TestIntegration_CancelRaceHasExactlyOneOutcome: a cancel and an aggressive
// order racing for the same resting order, in separate transactions, are
// serialized by the book's lock, so exactly one of them wins -- the order is
// either filled (and the cancel finds it not open) or cancelled (and the
// aggressor never trades with it) -- and the ledger agrees with the winner.
func TestIntegration_CancelRaceHasExactlyOneOutcome(t *testing.T) {
	f := newFixture(t)
	m := f.spotMarket(marketOpts{makerBPS: 10, takerBPS: 20})
	maker, taker := f.account(), f.account()
	f.fund(maker, m.base.ID, 100_000_000)
	f.fund(taker, m.quote.ID, 100_000_000_000)
	const rounds = 24
	fills, cancels := 0, 0
	for i := 0; i < rounds; i++ {
		resting := f.submit(f.limit(m, maker, SideSell, "100.00", units(1)))
		// The aggressor is accepted, approved and reserved beforehand, so the
		// race is between two single transactions -- the cancel and the
		// aggressor's Execute -- contending for the book's lock.
		var staged StageOutcome
		require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			staged, err = f.svc.Accept(ctx, tx, f.order(m, taker, SideBuy, TypeLimit, IOC, "100.00", units(1)))
			return err
		}))
		for _, stage := range []func(context.Context, pgx.Tx, OrderID) (StageOutcome, error){f.svc.Approve, f.svc.Reserve} {
			require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error { _, err := stage(ctx, tx, staged.Order.ID); return err }))
		}
		var wg sync.WaitGroup
		var cancelErr, buyErr error
		var buy StageOutcome
		start := make(chan struct{})
		headStart := time.Duration(i%3) * time.Millisecond // alternate who is likelier to win
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if i%2 == 1 {
				time.Sleep(headStart)
			}
			_, cancelErr = f.pipe.Cancel(f.ctx, CancelRequest{OrderID: resting.Order.ID, AccountID: maker, ActorType: security.ActorUser, ActorID: "maker"})
		}()
		go func() {
			defer wg.Done()
			<-start
			if i%2 == 0 {
				time.Sleep(headStart)
			}
			buyErr = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
				var err error
				buy, err = f.svc.Execute(ctx, tx, staged.Order.ID)
				return err
			})
		}()
		close(start)
		wg.Wait()
		require.NoError(t, buyErr)
		_, err := f.pipe.Resume(f.ctx, staged.Order.ID)
		require.NoError(t, err)
		final := f.orderView(resting.Order.ID)
		trades := f.count(`SELECT count(*) FROM book_trades WHERE maker_order_id = $1`, resting.Order.ID)
		switch final.Status {
		case StatusFilled:
			fills++
			assert.Equal(t, 1, trades)
			require.Error(t, cancelErr, "a cancel that lost the race is refused")
			assert.Equal(t, ReasonNotOpen, ReasonOf(cancelErr))
			assert.Len(t, buy.Trades, 1)
			assert.Equal(t, "CONSUMED", f.reservationStatus(final.ReservationID))
		case StatusCancelled:
			cancels++
			assert.Equal(t, 0, trades)
			require.NoError(t, cancelErr)
			assert.Empty(t, buy.Trades, "the aggressor never reached the cancelled order")
			assert.Equal(t, "RELEASED", f.reservationStatus(final.ReservationID))
		default:
			t.Fatalf("round %d: the resting order ended %s", i, final.Status)
		}
	}
	t.Logf("cancel race: %d filled first, %d cancelled first", fills, cancels)
	assert.Equal(t, rounds, fills+cancels)
	// The ledger agrees with the outcomes, to the unit.
	bought := money.QuantityFromInt64(int64(fills) * 1_000_000)
	assert.Equal(t, bought.String(), f.wallet(taker, m.base.ID).String())
	assert.Equal(t, money.QuantityFromInt64(100_000_000).Sub(bought).String(), f.wallet(maker, m.base.ID).String())
	notional := money.QuantityFromInt64(int64(fills) * 100_000_000)
	fees := money.QuantityFromInt64(int64(fills) * (100_000 + 200_000))
	assert.Equal(t, notional.Sub(money.QuantityFromInt64(int64(fills)*100_000)).String(), f.wallet(maker, m.quote.ID).String())
	assert.Equal(t, fees.String(), f.balance(ledger.PlatformAccount(ledger.CodePlatformFeeRevenue, m.quote.ID)).String())
	avail, err := f.capital.Availability(f.ctx, testDB, maker.String(), m.base.ID, f.clk.Now())
	require.NoError(t, err)
	assert.True(t, avail.Reserved.IsZero(), "nothing stays reserved for orders that are gone")
}

// TestIntegration_ConcurrentSubmitsReplayToTheLiveStateHash: many goroutines
// submitting to one book in their own transactions are serialized by the
// book's lock into one journal, and that journal replays -- from the empty
// book and from a snapshot -- to exactly the live state hash, which the
// in-process engine holds as well.
func TestIntegration_ConcurrentSubmitsReplayToTheLiveStateHash(t *testing.T) {
	f := newFixture(t)
	m := f.spotMarket(marketOpts{makerBPS: 5, takerBPS: 10, snapshotInterval: 10})
	const workers, each = 12, 8
	accts := f.fundedAccounts(m, workers)
	var wg sync.WaitGroup
	errsCh := make(chan error, workers*each)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				side := SideBuy
				if (w+i)%2 == 0 {
					side = SideSell
				}
				price := fmt.Sprintf("%d.%02d", 99+(w+i)%3, (w*7+i*13)%100)
				_, err := f.pipe.Submit(f.ctx, f.limit(m, accts[w], side, price, money.QuantityFromInt64(int64(1+(w+i)%7)*100_000)))
				if err != nil && errs.CodeOf(err) != errs.CodeRiskRejected && ReasonOf(err) == "" {
					errsCh <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errsCh)
	for err := range errsCh {
		require.NoError(t, err)
	}
	live, err := f.svc.GetBook(f.ctx, testDB, m.inst)
	require.NoError(t, err)
	require.EqualValues(t, workers*each, live.LastCommandSeq, "every submission is one journalled command")
	proof, err := f.svc.VerifyJournal(f.ctx, testDB, m.inst)
	require.NoError(t, err)
	assert.Equal(t, live.StateHash, proof.FullReplayHash)
	assert.Equal(t, live.StateHash, proof.SnapshotReplayHash)
	e := f.svc.engines.entry(live.ID)
	e.mu.Lock()
	assert.Equal(t, live.StateHash, e.hash, "the in-process engine is the journal's state")
	e.mu.Unlock()
	f.requireNoCrossedBook(m)
}

// TestIntegration_ConcurrentBuysCannotOverspendTheWallet: concurrent buys
// whose reservations together exceed the wallet cannot all succeed; what was
// reserved never exceeds what the account holds.
func TestIntegration_ConcurrentBuysCannotOverspendTheWallet(t *testing.T) {
	f := newFixture(t)
	m := f.spotMarket(marketOpts{makerBPS: 10, takerBPS: 20})
	x := f.account()
	f.fund(x, m.quote.ID, 1000_000_000) // 1000 quote units
	const n = 14
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 1 unit at 99.00 reserves 99 plus at most 0.198 of fees.
			_, err := f.pipe.Submit(f.ctx, f.limit(m, x, SideBuy, "99.00", units(1)))
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				accepted++
				return
			}
			code := errs.CodeOf(err)
			assert.True(t, code == errs.CodeRiskRejected || code == errs.CodeInsufficientBuyingPower, "refused for want of funds, not %v", err)
		}()
	}
	wg.Wait()
	assert.Equal(t, 10, accepted, "1000 / 99.198 allows exactly ten")
	avail, err := f.capital.Availability(f.ctx, testDB, x.String(), m.quote.ID, f.clk.Now())
	require.NoError(t, err)
	assert.LessOrEqual(t, avail.Reserved.Cmp(avail.WalletBalance), 0, "never more reserved than held")
	assert.Equal(t, money.QuantityFromInt64(int64(accepted)*99_198_000).String(), avail.Reserved.String())
}
