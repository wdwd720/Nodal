//go:build integration

package derivatives

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
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// TestIntegration_FuturesPnLIsExactThroughTheLedger follows one long future
// through an open, a variation-margin settlement, a partial close and a
// close, and checks every balance against the hand-computed exact numbers. A
// contract is 0.01 BTC: $1 of price moves one contract's value by $0.01.
func TestIntegration_FuturesPnLIsExactThroughTheLedger(t *testing.T) {
	f := newFixture(t)
	fut := f.future(start.Add(30 * 24 * time.Hour))
	a := f.account()
	f.fund(a, usdUnits(100_000))
	f.mark(fut, 65_000, 65_000)

	// Paper buy: $65 000 × 1.001 = $65 065.00 on the tick grid. Initial
	// margin: MAJOR 10% of 10 × $650 = $650.
	open := f.open(a, fut, Long, 10)
	require.NotNil(t, open.Fill)
	assert.Equal(t, "6506500", open.Fill.PriceMantissa.String())
	assert.Equal(t, econ.StatusCompleted, open.Intent.Status)
	p := f.position(a, fut)
	assert.Equal(t, "10", p.Quantity.String())
	assert.Equal(t, "6506500000", p.CostValue.String())
	assert.Equal(t, "650000000", p.MarginPosted.String())
	assert.Equal(t, "99350000000", f.balance(a, ledger.CodeWallet).String())
	assert.Equal(t, "650000000", f.balance(a, ledger.CodeDerivativesMargin).String())
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM asset_reservations WHERE idempotency_key = $1 AND status = 'CONSUMED'`,
		"derivatives:reserve:"+open.Intent.ID.String()), "the margin was reserved, then consumed by the fill")

	// Variation margin for 12:00-13:00 at $66 000: 10 × $660 − $6 506.50 = $93.50.
	f.clk.Set(start.Add(time.Hour))
	f.mark(fut, 66_000, 66_000)
	var vm RunResult
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		vm, err = f.svc.RunVariationMargin(ctx, tx, RunRequest{InstrumentID: fut, IntervalStart: start})
		return err
	}))
	assert.False(t, vm.Replay)
	assert.Equal(t, "93500000", vm.Received.String())
	assert.True(t, vm.Paid.IsZero())
	assert.Equal(t, "99443500000", f.balance(a, ledger.CodeWallet).String())

	// Partial close of 4 at $64 000 × 0.999 = $63 936.00: a quarter... four
	// tenths of the baseline ($2 640) against $2 557.44 = −$82.56, and four
	// tenths of the margin ($260) released.
	f.clk.Advance(time.Second)
	f.mark(fut, 64_000, 64_000)
	var partial Outcome
	four := q(4)
	require.NoError(t, f.do(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		partial, err = f.svc.ClosePosition(ctx, tx, CloseRequest{IntentRequest: f.req(a, fut), Quantity: &four})
		return err
	}))
	assert.Equal(t, "6393600", partial.Fill.PriceMantissa.String())
	assert.Equal(t, "-82560000", partial.Fill.CashSettled.String())
	assert.Equal(t, "-45160000", partial.Fill.RealizedPnL.String(), "against the entry: $2 557.44 − $2 602.60")
	assert.Equal(t, "390000000", f.position(a, fut).MarginPosted.String())
	assert.Equal(t, "99620940000", f.balance(a, ledger.CodeWallet).String())

	closed := f.closeAll(a, fut)
	assert.Equal(t, "-123840000", closed.Fill.CashSettled.String())
	assert.Equal(t, "-67740000", closed.Fill.RealizedPnL.String())
	p = f.position(a, fut)
	assert.Equal(t, PositionClosed, p.Status)
	assert.True(t, p.Quantity.IsZero() && p.MarginPosted.IsZero() && p.CostValue.IsZero())
	// Σ realized = Σ closing value − entry value = $6 393.60 − $6 506.50, and
	// the cash over the life (+93.50 − 82.56 − 123.84) is the same number.
	assert.Equal(t, "-112900000", p.RealizedPnL.String())
	assert.Equal(t, "99887100000", f.balance(a, ledger.CodeWallet).String())
	assert.True(t, f.balance(a, ledger.CodeDerivativesMargin).IsZero())
	assert.Equal(t, "-112900000", f.balance(a, ledger.CodeRealizedPnL).String())
	// The clearing house was the counterparty of a one-sided book: it holds
	// exactly what the customer lost.
	assert.Equal(t, "112900000", f.clearingBalance(ledger.CodeClearingResult).String())

	var rep ClearingReport
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rep, err = f.svc.ReconcileClearing(ctx, tx, f.usd)
		return err
	}))
	assert.True(t, rep.Matched, "%+v", rep)
	assert.Equal(t, int64(3), rep.Flows)
	assert.Empty(t, rep.RecordID)

	// Every effect of the subsystem is APPLIED with its journal and evidence.
	assert.Zero(t, f.count(`SELECT count(*) FROM economic_effects e JOIN derivative_fills d ON e.effect_key LIKE 'deriv:fill:' || d.fill_key || ':%'
		WHERE d.position_id = $1 AND (e.status <> 'APPLIED' OR e.journal_transaction_id IS NULL OR e.evidence_ref IS NULL)`, p.ID))
	assert.Equal(t, 5, f.count(`SELECT count(*) FROM economic_effects e JOIN derivative_fills d ON e.effect_key LIKE 'deriv:fill:' || d.fill_key || ':%'
		WHERE d.position_id = $1`, p.ID), "open: margin; partial: release + cash; close: release + cash; nothing else")
}

// TestIntegration_VariationMarginRunsOncePerInterval: a replay and concurrent
// runs of the same interval settle once; a run before the interval ends, or
// without a fresh mark, is refused.
func TestIntegration_VariationMarginRunsOncePerInterval(t *testing.T) {
	f := newFixture(t)
	perp := f.perp()
	a, b := f.account(), f.account()
	f.fund(a, usdUnits(100_000))
	f.fund(b, usdUnits(100_000))
	f.mark(perp, 65_000, 65_000)
	f.open(a, perp, Long, 3)
	f.open(b, perp, Short, 2)

	run := func() (RunResult, error) {
		var r RunResult
		err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			r, err = f.svc.RunVariationMargin(ctx, tx, RunRequest{InstrumentID: perp, IntervalStart: start})
			return err
		})
		return r, err
	}
	_, err := run()
	require.Error(t, err, "the interval has not ended")
	f.clk.Set(start.Add(time.Hour + 2*time.Minute))
	_, err = run()
	require.Error(t, err, "no mark within the oracle max age")
	assert.Equal(t, errs.CodeStaleMarketData, errs.CodeOf(err))
	f.mark(perp, 65_500, 65_500)

	const n = 8
	var wg sync.WaitGroup
	results := make(chan RunResult, n)
	failures := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := run()
			if err != nil {
				failures <- err
				return
			}
			results <- r
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
	fresh := 0
	for r := range results {
		if !r.Replay {
			fresh++
			// a: 3 × ($655 − $650.65) = $13.05; b: −2 × ($655 − $649.35) = −$11.30
			assert.Equal(t, "13050000", r.Received.String())
			assert.Equal(t, "11300000", r.Paid.String())
		}
	}
	assert.Equal(t, 1, fresh, "exactly one run settles the interval")
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM variation_margin_runs WHERE instrument_id = $1`, perp))
	assert.Equal(t, 2, f.count(`SELECT count(*) FROM derivative_cash_flows WHERE instrument_id = $1 AND kind = 'VARIATION_MARGIN'`, perp))
	again, err := run()
	require.NoError(t, err)
	assert.True(t, again.Replay)
	assert.Empty(t, again.Journals)
	assert.Equal(t, 2, f.count(`SELECT count(*) FROM derivative_cash_flows WHERE instrument_id = $1 AND kind = 'VARIATION_MARGIN'`, perp))
}

// TestIntegration_ALateVariationMarginRunSettlesAtTheCurrentMark: a run for
// an interval that ended before more contracts were traded settles the
// position as it stands now at the mark of now -- the new contracts are not
// settled against an older price -- and an interval earlier than one already
// settled is refused.
func TestIntegration_ALateVariationMarginRunSettlesAtTheCurrentMark(t *testing.T) {
	f := newFixture(t)
	perp := f.perp()
	a := f.account()
	f.fund(a, usdUnits(100_000))
	f.mark(perp, 65_000, 65_000)
	f.open(a, perp, Long, 1) // at 65 065
	f.clk.Set(start.Add(90 * time.Minute))
	f.mark(perp, 70_000, 70_000)
	f.open(a, perp, Long, 9) // at 70 070, after the 12:00-13:00 interval ended

	vm := func(intervalStart time.Time) (RunResult, error) {
		var r RunResult
		err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			r, err = f.svc.RunVariationMargin(ctx, tx, RunRequest{InstrumentID: perp, IntervalStart: intervalStart})
			return err
		})
		return r, err
	}
	r, err := vm(start)
	require.NoError(t, err)
	// 10 × $700 − ($650.65 + 9 × $700.70): the first contract's $49.35 gain
	// less the new contracts' own $6.30 of slippage -- nothing phantom.
	assert.Equal(t, "43050000", r.Received.String())
	assert.True(t, r.Paid.IsZero())
	assert.Equal(t, "7000000000", f.position(a, perp).SettledValue.String(), "the baseline is the position at the current mark")

	_, err = vm(start.Add(-time.Hour))
	require.Error(t, err, "an earlier interval than one already settled")
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	again, err := vm(start)
	require.NoError(t, err)
	assert.True(t, again.Replay, "the settled interval itself replays")
}

// TestIntegration_ConcurrentSettlementPostsOnce: N concurrent final
// settlements of one future settle every position exactly once, and the
// settlement is exact.
func TestIntegration_ConcurrentSettlementPostsOnce(t *testing.T) {
	f := newFixture(t)
	expiry := start.Add(2 * time.Hour)
	fut := f.future(expiry)
	a, b, c := f.account(), f.account(), f.account()
	f.fund(a, usdUnits(100_000))
	f.fund(b, usdUnits(100_000))
	f.fund(c, usdUnits(100_000))
	f.mark(fut, 65_000, 65_000)
	f.open(a, fut, Long, 2)  // at 65 065
	f.open(b, fut, Short, 3) // at 64 935
	f.open(c, fut, Long, 1)  // at 65 065

	settle := func() (SettlementResult, error) {
		var r SettlementResult
		err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			r, err = f.svc.SettleExpiry(ctx, tx, SettleRequest{InstrumentID: fut})
			return err
		})
		return r, err
	}
	_, err := settle()
	require.Error(t, err, "a contract settles only after expiry")

	f.clk.Set(expiry)
	f.markAt(fut, dollars(66_000), dollars(66_000), expiry)
	const n = 8
	var wg sync.WaitGroup
	results := make(chan SettlementResult, n)
	failures := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := settle()
			if err != nil {
				failures <- err
				return
			}
			results <- r
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
	fresh := 0
	for r := range results {
		if !r.Replay {
			fresh++
			assert.Equal(t, "6600000", r.Price.String())
			// a +$18.70, c +$9.35 received; b −$31.95 paid.
			assert.Equal(t, "28050000", r.Received.String())
			assert.Equal(t, "31950000", r.Paid.String())
		}
	}
	assert.Equal(t, 1, fresh, "exactly one settlement")
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM derivative_settlements WHERE instrument_id = $1`, fut))
	assert.Equal(t, 3, f.count(`SELECT count(*) FROM derivative_cash_flows WHERE instrument_id = $1 AND kind = 'FUTURE_SETTLEMENT'`, fut))
	assert.Equal(t, 3, f.count(`SELECT count(*) FROM derivative_fills WHERE instrument_id = $1 AND source = 'EXPIRY'`, fut))
	assert.Equal(t, 3, f.count(`SELECT count(*) FROM derivative_positions WHERE instrument_id = $1 AND status = 'SETTLED'`, fut))
	assert.Equal(t, "100018700000", f.balance(a, ledger.CodeWallet).String())
	assert.Equal(t, "99968050000", f.balance(b, ledger.CodeWallet).String())
	assert.Equal(t, "100009350000", f.balance(c, ledger.CodeWallet).String())
	for _, acct := range []string{a.String(), b.String(), c.String()} {
		assert.Zero(t, f.count(`SELECT count(*) FROM ledger_accounts la JOIN ledger_balances lb ON lb.ledger_account_id = la.id
			WHERE la.owner_id = $1 AND la.code = 'DERIVATIVES_MARGIN' AND lb.balance <> 0`, acct), "margin released")
	}
	// The book was matched in contracts (2 + 1 − 3 = 0): the clearing house
	// keeps only the disclosed paper slippage it was paid ($3.90).
	assert.Equal(t, "3900000", f.clearingBalance(ledger.CodeClearingResult).String())

	replay, err := settle()
	require.NoError(t, err)
	assert.True(t, replay.Replay)

	// A book fill matched a millisecond before expiry but applied after the
	// settlement would open a position nothing will ever settle: refused.
	m, _, err := LatestMark(f.ctx, testDB, fut)
	require.NoError(t, err)
	did := f.allowDecision(a, fut)
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.ApplyFill(ctx, tx, Fill{Key: "book:" + uuid.NewString(), AccountID: a, InstrumentID: fut, Side: Buy,
			Quantity: q(1), Price: dollars(66_000), Source: SourceBook, MarkID: m.ID, Direction: FillEither, DecisionID: did,
			OccurredAt: expiry.Add(-time.Millisecond)})
		return err
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))
	assert.Zero(t, f.count(`SELECT count(*) FROM derivative_positions WHERE instrument_id = $1 AND status = 'OPEN'`, fut))

	// And the database refuses a second settlement row outright, even one the
	// expiry rule fully explains.
	var mark uuid.UUID
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT mark_id FROM derivative_settlements WHERE instrument_id = $1`, fut).Scan(&mark))
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO derivative_settlements (id, instrument_id, kind, settlement_mantissa, price_scale, price_source,
			price_rule, mark_id, positions, settled_at) VALUES ($1,$2,'FUTURE',6600000,2,'raw','EXPIRY_MARK',$3,0,now())`, uuid.New(), fut, mark)
		return err
	})
	require.Error(t, err)
	assert.True(t, db.IsUniqueViolation(err), "%v", err)
}

// TestIntegration_AnUnpayableObligationIsCollectedFromMarginThenRecordedAsAShortfall:
// a customer who has withdrawn everything but its margin loses more than that
// margin at a variation-margin settlement. The wallet pays what it has
// (nothing), the posted margin pays next, and the rest is a shortfall: on the
// cash flow, on the position, absorbed by the clearing house -- never a
// negative balance -- and the register still reconciles.
func TestIntegration_AnUnpayableObligationIsCollectedFromMarginThenRecordedAsAShortfall(t *testing.T) {
	f := newFixture(t)
	perp := f.perp()
	a := f.account()
	f.fund(a, usdUnits(100_000))
	f.mark(perp, 65_000, 65_000)
	f.open(a, perp, Long, 10) // at $65 065, $796.25 of margin
	f.drainWallet(a)
	resultBefore := f.clearingBalance(ledger.CodeClearingResult)

	// At $55 000 the position owes 10 × ($550 − $650.65) = $1 006.50.
	f.clk.Set(start.Add(time.Hour))
	f.mark(perp, 55_000, 55_000)
	var r RunResult
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r, err = f.svc.RunVariationMargin(ctx, tx, RunRequest{InstrumentID: perp, IntervalStart: start})
		return err
	}))
	assert.Equal(t, "796250000", r.Paid.String(), "all the margin there was")
	assert.Equal(t, "210250000", r.Shortfall.String())
	p := f.position(a, perp)
	assert.True(t, p.MarginPosted.IsZero())
	assert.Equal(t, "210250000", p.Shortfall.String())
	assert.True(t, f.balance(a, ledger.CodeWallet).IsZero())
	assert.True(t, f.balance(a, ledger.CodeDerivativesMargin).IsZero())
	assert.Equal(t, resultBefore.Add(q(796_250_000)).String(), f.clearingBalance(ledger.CodeClearingResult).String(),
		"the clearing house received what was collected and absorbs the rest")
	var amount, shortfall money.Quantity
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT amount, shortfall FROM derivative_cash_flows WHERE position_id = $1 AND kind = 'VARIATION_MARGIN'`,
		p.ID).Scan(&amount, &shortfall))
	assert.Equal(t, "-796250000", amount.String())
	assert.Equal(t, "210250000", shortfall.String())
	rep := f.reconcile()
	assert.True(t, rep.Matched, "%+v", rep)
	assert.Equal(t, "210250000", rep.RegisterShortfall.String())
}
