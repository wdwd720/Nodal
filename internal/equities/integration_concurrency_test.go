//go:build integration

package equities

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/ledger"
)

// inTxRetrying runs fn in its own READ COMMITTED transaction, retrying the
// deadlocks and serialization failures db.InTx is built to retry.
func inTxRetrying(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return testDB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 5}, fn)
}

// TestIntegration_OrderPlacementIsExactlyOnceUnderConcurrency: many clients
// submitting and accepting the same request (same idempotency key) at once
// produce one intent, one order, one reservation, one risk decision and one
// venue submission -- and applying the venue's answer from many workers at
// once posts one trade.
func TestIntegration_OrderPlacementIsExactlyOnceUnderConcurrency(t *testing.T) {
	w := newWorld(t)
	acct := w.account()
	w.fund(acct, w.cash.ID, 10_000_000_000)
	l := w.list(1, 1)
	w.mark(l, 15_000)
	spec := market(SideBuy, 1_000_000)
	spec.key = "same-key-" + uuid.NewString()
	req := w.request(acct, l, spec)

	const n = 12
	var wg sync.WaitGroup
	orders := make(chan Order, n)
	failures := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var in econ.Intent
			if err := inTxRetrying(w.ctx, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				in, err = w.svc.SubmitOrderIntent(ctx, tx, req)
				return err
			}); err != nil {
				failures <- err
				return
			}
			var o Order
			if err := inTxRetrying(w.ctx, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				o, err = w.svc.AcceptOrder(ctx, tx, in.ID)
				return err
			}); err != nil {
				failures <- err
				return
			}
			orders <- o
		}()
	}
	wg.Wait()
	close(orders)
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
	ids := map[OrderID]bool{}
	var one Order
	for o := range orders {
		ids[o.ID] = true
		one = o
	}
	require.Len(t, ids, 1, "every client gets the same order")
	assert.Equal(t, OrderAccepted, one.Status)
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM economic_intents WHERE account_id = $1 AND idempotency_key = $2`, acct, spec.key))
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM equity_orders WHERE intent_id = $1`, one.IntentID))
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM asset_reservations WHERE idempotency_key = $1`, "equities:order:"+one.IntentID.String()))
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM advanced_risk_decisions WHERE intent_id = $1`, one.IntentID))
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM economic_effects WHERE effect_key = $1`, submitKey(one.ID)))

	report, err := w.svc.PlaceAtVenue(w.ctx, testDB, one.ID)
	require.NoError(t, err)
	var aw sync.WaitGroup
	applied := make(chan error, n)
	for range n {
		aw.Add(1)
		go func() {
			defer aw.Done()
			applied <- inTxRetrying(w.ctx, func(ctx context.Context, tx pgx.Tx) error {
				o, err := w.svc.ApplyOrderReport(ctx, tx, one.ID, report)
				if err == nil && o.Status != OrderFilled {
					return errs.Newf(errs.CodeInternal, "order is %s", o.Status)
				}
				return err
			})
		}()
	}
	aw.Wait()
	close(applied)
	for err := range applied {
		require.NoError(t, err, "a replayed answer is not an error")
	}
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM equity_fills WHERE order_id = $1`, one.ID))
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM journal_transactions WHERE idempotency_key = $1`, tradeKey(one.ID)))
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM equity_settlements WHERE order_id = $1`, one.ID))
	assert.Equal(t, "1000000", w.cust(acct, ledger.CodeUnsettledReceivable, l.ShareAsset.ID).String(), "one share, once")
}

// TestIntegration_FillApplicationIsExactlyOnce: re-applying the venue's
// answer is a replay that changes nothing; an answer that contradicts the
// recorded fill, or belongs to another order, is refused.
func TestIntegration_FillApplicationIsExactlyOnce(t *testing.T) {
	w := newWorld(t)
	acct := w.account()
	w.fund(acct, w.cash.ID, 10_000_000_000)
	l := w.list(1, 1)
	w.mark(l, 15_000)
	o := w.accept(w.submit(w.request(acct, l, market(SideBuy, 1_000_000))).ID)
	report, err := w.svc.PlaceAtVenue(w.ctx, testDB, o.ID)
	require.NoError(t, err)
	first, err := w.apply(o.ID, report)
	require.NoError(t, err)
	require.Equal(t, OrderFilled, first.Status)
	wallet := w.cust(acct, ledger.CodeWallet, w.cash.ID)

	again, err := w.apply(o.ID, report)
	require.NoError(t, err)
	assert.Equal(t, first.UpdatedAt, again.UpdatedAt, "a replay writes nothing")
	assert.Equal(t, wallet.String(), w.cust(acct, ledger.CodeWallet, w.cash.ID).String())
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM equity_order_transitions WHERE order_id = $1`, o.ID))

	moved := report
	fill := *report.Fill
	fill.Price = fill.Price.Add(q(1))
	moved.Fill = &fill
	_, err = w.apply(o.ID, moved)
	require.Error(t, err)
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err), "a second, different fill for a filled order is a conflict")

	cancelled := report
	cancelled.Status, cancelled.Fill = ReportCancelled, nil
	_, err = w.apply(o.ID, cancelled)
	require.Error(t, err)
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err))

	other := w.accept(w.submit(w.request(acct, l, market(SideBuy, 1_000_000))).ID)
	_, err = w.apply(other.ID, report)
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "a report names its own order")
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM journal_transactions WHERE idempotency_key = $1`, tradeKey(o.ID)))

	// A fill outside the order's limit is refused before anything is posted.
	lim := w.accept(w.submit(w.request(acct, l, limit(SideBuy, 1_000_000, 14_000))).ID)
	r, err := w.svc.PlaceAtVenue(w.ctx, testDB, lim.ID)
	require.NoError(t, err)
	require.Equal(t, ReportOpen, r.Status)
	_, err = w.apply(lim.ID, r)
	require.NoError(t, err)
	forged := r
	forged.Status = ReportFilled
	forged.Fill = &BrokerFill{VenueFillID: "forged", Quantity: q(1_000_000), Price: q(14_500), PriceScale: 2,
		Mark: *mark(14_500, 2, w.clk.Now()), ExecutedAt: w.clk.Now()}
	_, err = w.apply(lim.ID, forged)
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	assert.Equal(t, OrderOpen, w.order(lim.ID).Status)
}

// TestIntegration_ConcurrentSellsCannotSellTheSameSharesTwice: five shares,
// eight concurrent orders each selling all five: exactly one is accepted --
// the capital reservation on the share asset admits one -- and the others
// are refused for insufficient buying power. Then eight one-share sells race
// for what is left: exactly as many succeed as there are shares.
func TestIntegration_ConcurrentSellsCannotSellTheSameSharesTwice(t *testing.T) {
	w := newWorld(t)
	acct := w.account()
	l := w.list(1, 1)
	w.fund(acct, l.ShareAsset.ID, 5_000_000)
	w.mark(l, 15_000)

	race := func(n int, qty int64) []Order {
		t.Helper()
		intents := make([]econ.Intent, n)
		for i := range intents {
			intents[i] = w.submit(w.request(acct, l, market(SideSell, qty)))
		}
		var wg sync.WaitGroup
		out := make([]Order, n)
		errsCh := make(chan error, n)
		for i := range intents {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				errsCh <- inTxRetrying(w.ctx, func(ctx context.Context, tx pgx.Tx) error {
					var err error
					out[i], err = w.svc.AcceptOrder(ctx, tx, intents[i].ID)
					return err
				})
			}(i)
		}
		wg.Wait()
		close(errsCh)
		for err := range errsCh {
			require.NoError(t, err)
		}
		return out
	}
	statuses := func(os []Order) (accepted []Order, rejected int) {
		for _, o := range os {
			switch o.Status {
			case OrderAccepted:
				accepted = append(accepted, o)
			case OrderRejected:
				assert.Equal(t, RejectInsufficientBuyingPower, o.RejectionCode)
				rejected++
			}
		}
		return accepted, rejected
	}

	accepted, rejected := statuses(race(8, 5_000_000))
	require.Len(t, accepted, 1, "the same five shares are sold once")
	assert.Equal(t, 7, rejected)
	assert.Equal(t, OrderFilled, w.place(accepted[0]).Status)
	assert.True(t, w.cust(acct, ledger.CodeWallet, l.ShareAsset.ID).IsZero())

	w.fund(acct, l.ShareAsset.ID, 5_000_000)
	accepted, rejected = statuses(race(8, 1_000_000))
	require.Len(t, accepted, 5, "five one-share sells fit in five shares")
	assert.Equal(t, 3, rejected)
	for _, o := range accepted {
		assert.Equal(t, OrderFilled, w.place(o).Status)
	}
	assert.True(t, w.cust(acct, ledger.CodeWallet, l.ShareAsset.ID).IsZero(), "never negative, never oversold")
	assert.Equal(t, "10000000", w.cust(acct, ledger.CodeTradingOutflow, l.ShareAsset.ID).String())
}

// TestIntegration_ConcurrentSettlementSettlesOnce: many settlement workers
// racing over the same due settlement post one EQUITY_SETTLEMENT.
func TestIntegration_ConcurrentSettlementSettlesOnce(t *testing.T) {
	w := newWorld(t)
	acct := w.account()
	w.fund(acct, w.cash.ID, 10_000_000_000)
	l := w.list(1, 1)
	w.mark(l, 15_000)
	o := w.trade(acct, l, market(SideBuy, 3_000_000))
	require.Equal(t, OrderFilled, o.Status)
	f, _, err := w.svc.FillForOrder(w.ctx, testDB, o.ID)
	require.NoError(t, err)
	w.at(2026, 10, 8, 9, 0)

	const n = 10
	var wg sync.WaitGroup
	counts := make(chan int, n)
	errsCh := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mine := 0
			defer func() { counts <- mine }()
			for {
				var batch SettlementBatch
				if err := inTxRetrying(w.ctx, func(ctx context.Context, tx pgx.Tx) error {
					var err error
					batch, err = w.svc.SettleDueBatch(ctx, tx, 500)
					return err
				}); err != nil {
					errsCh <- err
					return
				}
				if len(batch.Failures) > 0 {
					errsCh <- fmt.Errorf("settlement %s failed: %w", batch.Failures[0].SettlementID, batch.Failures[0].Err)
					return
				}
				settled := batch.Settled
				if len(settled) == 0 {
					return
				}
				for _, s := range settled {
					if s.FillID == f.ID {
						mine++
					}
				}
			}
		}()
	}
	wg.Wait()
	close(counts)
	close(errsCh)
	for err := range errsCh {
		require.NoError(t, err)
	}
	total := 0
	for c := range counts {
		total += c
	}
	assert.Equal(t, 1, total, "exactly one worker settled the fill")
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM journal_transactions WHERE idempotency_key = $1`, "equities:settlement:"+f.ID.String()))
	assert.Equal(t, "3000000", w.cust(acct, ledger.CodeWallet, l.ShareAsset.ID).String())
	assert.True(t, w.cust(acct, ledger.CodeUnsettledReceivable, l.ShareAsset.ID).IsZero())
}

// TestIntegration_SettlementWorkersAcrossAccountsNeverDeadlock: workers
// draining the due settlements of many accounts and instruments at once --
// each transaction settles one account, with its instrument locks taken
// before anything is posted -- finish without a deadlock or a retry, and
// every settlement settles exactly once.
func TestIntegration_SettlementWorkersAcrossAccountsNeverDeadlock(t *testing.T) {
	w := newWorld(t)
	listings := []Listing{w.list(1, 1), w.list(1, 1), w.list(1, 1)}
	for _, l := range listings {
		w.mark(l, 15_000)
	}
	var fills []FillID
	for range 4 {
		acct := w.account()
		w.fund(acct, w.cash.ID, 10_000_000_000)
		for _, l := range listings {
			o := w.trade(acct, l, market(SideBuy, 1_000_000))
			require.Equal(t, OrderFilled, o.Status)
			f, ok, err := w.svc.FillForOrder(w.ctx, testDB, o.ID)
			require.NoError(t, err)
			require.True(t, ok)
			fills = append(fills, f.ID)
		}
	}
	w.at(2026, 10, 8, 9, 0)

	const workers = 8
	var wg sync.WaitGroup
	errsCh := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				var batch SettlementBatch
				// No retries: a deadlock would surface here as an error.
				if err := w.inTx(func(ctx context.Context, tx pgx.Tx) error {
					var err error
					batch, err = w.svc.SettleDueBatch(ctx, tx, 500)
					return err
				}); err != nil {
					errsCh <- err
					return
				}
				if len(batch.Failures) > 0 {
					errsCh <- fmt.Errorf("settlement %s failed: %w", batch.Failures[0].SettlementID, batch.Failures[0].Err)
					return
				}
				settled := batch.Settled
				if len(settled) == 0 {
					return
				}
				for _, s := range settled[1:] {
					if s.AccountID != settled[0].AccountID {
						errsCh <- fmt.Errorf("one batch settled accounts %s and %s", settled[0].AccountID, s.AccountID)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	close(errsCh)
	for err := range errsCh {
		require.NoError(t, err)
	}
	for _, fid := range fills {
		st, ok, err := w.svc.SettlementForFill(w.ctx, testDB, fid)
		require.NoError(t, err)
		require.True(t, ok)
		assert.True(t, st.Settled())
		assert.Equal(t, 1, w.count(`SELECT count(*) FROM journal_transactions WHERE idempotency_key = $1`, "equities:settlement:"+fid.String()))
	}
}
