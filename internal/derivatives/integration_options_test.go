//go:build integration

package derivatives

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/quant"
)

func (f *fixture) settle(ins instruments.InstrumentID) SettlementResult {
	f.t.Helper()
	var r SettlementResult
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r, err = f.svc.SettleExpiry(ctx, tx, SettleRequest{InstrumentID: ins})
		return err
	}))
	return r
}

// TestIntegration_OptionExpirySettlementIsExact: a European call expiring
// in the money is exercised automatically and assigned; a put expiring out of
// the money pays nothing. Premiums ($5 a contract at $500 per BTC... here
// $1 000 per BTC = $10 a contract) and intrinsic values are exact to the
// micro-dollar, writers' margin is released, every position is SETTLED.
func TestIntegration_OptionExpirySettlementIsExact(t *testing.T) {
	f := newFixture(t)
	expiry := start.Add(48 * time.Hour)
	call := f.option(instruments.Call, instruments.European, 65_000, expiry)
	put := f.option(instruments.Put, instruments.European, 65_000, expiry)
	holder, writer, putHolder, putWriter := f.account(), f.account(), f.account(), f.account()
	for _, a := range []accounts.AccountID{holder, writer, putHolder, putWriter} {
		f.fund(a, usdUnits(100_000))
	}
	f.mark(call, 1_000, 65_000)
	f.mark(put, 1_000, 65_000)

	// The holder pays $1 001.00 a BTC (the mark plus 0.1%, on the tick), the
	// writer receives $999.00 and posts the scenario margin: worst grid point
	// +30% (S 84 500: $195 a contract, $390 for 2, less the $20 liability now)
	// plus 10% of the $1 300 underlying notional.
	buy := f.open(holder, call, Long, 2)
	assert.Equal(t, "100100", buy.Fill.PriceMantissa.String())
	assert.Equal(t, "-20020000", buy.Fill.CashSettled.String())
	sell := f.open(writer, call, Short, 2)
	assert.Equal(t, "99900", sell.Fill.PriceMantissa.String())
	assert.Equal(t, "500000000", f.position(writer, call).MarginPosted.String())
	assert.Equal(t, "20020000", f.balance(holder, ledger.CodeOptionPremiumPaid).String())
	assert.Equal(t, "19980000", f.balance(writer, ledger.CodeOptionPremiumReceived).String())
	f.open(putHolder, put, Long, 1)
	f.open(putWriter, put, Short, 1)

	f.clk.Set(expiry)
	f.markAt(call, dollars(3_000), dollars(68_000), expiry)
	f.markAt(put, dollars(0), dollars(68_000), expiry)

	r := f.settle(call)
	assert.Equal(t, SettleOption, r.Kind)
	assert.Equal(t, "6800000", r.Price.String(), "the underlying settles at $68 000")
	assert.Equal(t, "30000000", r.IntrinsicPerContract.String(), "$3 000 a BTC is $30 a contract")
	assert.Equal(t, "60000000", r.Received.String())
	assert.Equal(t, "60000000", r.Paid.String(), "what the holder receives the writer pays")
	require.Len(t, r.Exercises, 1)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM option_exercises WHERE id = $1 AND kind = 'AUTOMATIC' AND settlement_id = $2`,
		r.Exercises[0], r.ID))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM option_assignments WHERE exercise_id = $1 AND account_id = $2
		AND quantity = 2 AND amount = 60000000`, r.Exercises[0], writer))

	assert.Equal(t, "100039980000", f.balance(holder, ledger.CodeWallet).String())
	assert.True(t, f.balance(holder, ledger.CodeOptionPremiumPaid).IsZero())
	assert.Equal(t, "39980000", f.balance(holder, ledger.CodeRealizedPnL).String(), "$60 − $20.02")
	assert.Equal(t, "99959980000", f.balance(writer, ledger.CodeWallet).String())
	assert.True(t, f.balance(writer, ledger.CodeOptionPremiumReceived).IsZero())
	assert.True(t, f.balance(writer, ledger.CodeDerivativesMargin).IsZero())
	assert.Equal(t, "-40020000", f.balance(writer, ledger.CodeRealizedPnL).String(), "$19.98 − $60")
	assert.Equal(t, PositionSettled, f.position(holder, call).Status)
	assert.Equal(t, PositionSettled, f.position(writer, call).Status)

	pr := f.settle(put)
	assert.True(t, pr.IntrinsicPerContract.IsZero())
	assert.Empty(t, pr.Exercises)
	assert.True(t, pr.Received.IsZero() && pr.Paid.IsZero())
	assert.Equal(t, "-10010000", f.balance(putHolder, ledger.CodeRealizedPnL).String(), "the premium is the whole loss")
	assert.Equal(t, "9990000", f.balance(putWriter, ledger.CodeRealizedPnL).String(), "the premium is the whole gain")
	assert.Equal(t, "100009990000", f.balance(putWriter, ledger.CodeWallet).String(), "margin back, premium kept")
	assert.True(t, f.balance(putHolder, ledger.CodeOptionPremiumPaid).IsZero())
	assert.True(t, f.balance(putWriter, ledger.CodeOptionPremiumReceived).IsZero())

	// The clearing house keeps the disclosed paper slippage of both books:
	// ($20.02 − $19.98) + ($10.01 − $9.99).
	assert.Equal(t, "60000", f.clearingBalance(ledger.CodeClearingResult).String())
	assert.True(t, f.settle(call).Replay, "settled once")
}

// TestIntegration_AOneSidedOptionBookSettlesAgainstTheClearingHouse: a
// holder with no customer writer is assigned against the clearing house, and
// a writer with no customer holder is exercised against by it. Both settle
// exactly, the clearing house's result moves by exactly its side of the
// flows, and reconciliation agrees.
func TestIntegration_AOneSidedOptionBookSettlesAgainstTheClearingHouse(t *testing.T) {
	f := newFixture(t)
	expiry := start.Add(48 * time.Hour)
	held := f.option(instruments.Call, instruments.European, 65_000, expiry)
	written := f.option(instruments.Call, instruments.European, 65_000, expiry)
	holder, writer := f.account(), f.account()
	f.fund(holder, usdUnits(100_000))
	f.fund(writer, usdUnits(100_000))
	f.mark(held, 1_000, 65_000)
	f.mark(written, 1_000, 65_000)
	f.open(holder, held, Long, 1)     // pays $10.01
	f.open(writer, written, Short, 2) // receives $19.98
	result := f.clearingBalance(ledger.CodeClearingResult)

	f.clk.Set(expiry)
	f.markAt(held, dollars(3_000), dollars(68_000), expiry)
	f.markAt(written, dollars(3_000), dollars(68_000), expiry)
	h := f.settle(held)
	assert.Equal(t, "30000000", h.Received.String())
	assert.True(t, h.Paid.IsZero())
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM option_assignments WHERE exercise_id = $1 AND short_position_id IS NULL
		AND account_id IS NULL AND quantity = 1 AND amount = 30000000`, h.Exercises[0]), "the clearing house is the writer of record")
	w := f.settle(written)
	assert.Equal(t, "60000000", w.Paid.String())
	require.Len(t, w.Exercises, 1)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM option_exercises WHERE id = $1 AND position_id IS NULL AND account_id IS NULL
		AND kind = 'AUTOMATIC' AND quantity = 2`, w.Exercises[0]), "the clearing house is the holder of record")
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM option_assignments WHERE exercise_id = $1 AND account_id = $2 AND quantity = 2`,
		w.Exercises[0], writer))

	// The clearing house paid the holder $30 and collected $60 from the
	// writer: its result moved by exactly +$30.
	assert.Equal(t, result.Add(q(30_000_000)).String(), f.clearingBalance(ledger.CodeClearingResult).String())
	assert.Equal(t, "19990000", f.balance(holder, ledger.CodeRealizedPnL).String(), "$30 − $10.01")
	assert.Equal(t, "-40020000", f.balance(writer, ledger.CodeRealizedPnL).String(), "$19.98 − $60")
	assert.True(t, f.reconcile().Matched)
}

// TestIntegration_AnAssignedWriterWhoCannotPayIsAShortfall: the holder is
// paid in full by the clearing house; the writer pays what it has (its
// released margin) and the rest is a shortfall, reported as such by the
// settlement and the register, never as a payment.
func TestIntegration_AnAssignedWriterWhoCannotPayIsAShortfall(t *testing.T) {
	f := newFixture(t)
	expiry := start.Add(48 * time.Hour)
	call := f.option(instruments.Call, instruments.European, 65_000, expiry)
	holder, writer := f.account(), f.account()
	f.fund(holder, usdUnits(100_000))
	f.fund(writer, usdUnits(100_000))
	f.mark(call, 1_000, 65_000)
	f.open(holder, call, Long, 2)
	f.open(writer, call, Short, 2) // $500 of margin
	f.drainWallet(writer)

	f.clk.Set(expiry)
	f.markAt(call, dollars(135_000), dollars(200_000), expiry)
	r := f.settle(call)
	// $135 000 of intrinsic value a BTC is $1 350 a contract, $2 700 for two.
	assert.Equal(t, "2700000000", r.Received.String(), "the holder is paid in full")
	assert.Equal(t, "500000000", r.Paid.String(), "the writer paid its released margin")
	assert.Equal(t, "2200000000", r.Shortfall.String())
	assert.True(t, f.balance(writer, ledger.CodeWallet).IsZero())
	assert.Equal(t, "2200000000", f.position(writer, call).Shortfall.String())
	assert.True(t, f.reconcile().Matched, "the register records what was collected and what was not")
}

// TestIntegration_AmericanExerciseAndFIFOAssignmentConserve: writers W1 (2
// contracts, opened first) and W2 (3) and holders H1 (4) and H2 (1) of an
// American call struck at $60 000. H1 exercises 3 at an index of $65 000:
// assignment takes W1's 2 and then 1 of W2's, each paying exactly the $50 a
// contract H1 receives.
func TestIntegration_AmericanExerciseAndFIFOAssignmentConserve(t *testing.T) {
	f := newFixture(t)
	call := f.option(instruments.Call, instruments.American, 60_000, start.Add(30*24*time.Hour))
	w1, w2, h1, h2 := f.account(), f.account(), f.account(), f.account()
	for _, a := range []accounts.AccountID{w1, w2, h1, h2} {
		f.fund(a, usdUnits(100_000))
	}
	f.mark(call, 5_500, 65_000)
	f.open(w1, call, Short, 2)
	f.clk.Advance(time.Second)
	f.open(w2, call, Short, 3)
	f.open(h1, call, Long, 4)
	f.open(h2, call, Long, 1)
	w2Margin := f.position(w2, call).MarginPosted
	assert.Equal(t, "765000000", w2Margin.String())

	f.clk.Advance(5 * time.Second)
	f.mark(call, 5_600, 65_000)
	before := map[accounts.AccountID]money.Quantity{}
	for _, a := range []accounts.AccountID{w1, w2, h1, h2} {
		before[a] = f.balance(a, ledger.CodeWallet)
	}
	three := q(3)
	req := ExerciseRequest{IntentRequest: f.req(h1, call), Quantity: &three}
	var out Outcome
	require.NoError(t, f.do(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = f.svc.ExerciseOption(ctx, tx, req)
		return err
	}))
	require.NotNil(t, out.ExerciseID)
	assert.Equal(t, SourceExercise, out.Fill.Source)
	assert.Equal(t, "-3", out.Fill.Quantity.String())

	receipts := f.balance(h1, ledger.CodeWallet).Sub(before[h1])
	w1Paid := before[w1].Sub(f.balance(w1, ledger.CodeWallet))
	w2Paid := before[w2].Sub(f.balance(w2, ledger.CodeWallet))
	assert.Equal(t, "150000000", receipts.String(), "3 × $50")
	// The writers' wallets also got their released margin back: W1 all of
	// its $510, W2 a third of its $765.
	assert.Equal(t, "100000000", w1Paid.Add(q(510_000_000)).String())
	assert.Equal(t, "50000000", w2Paid.Add(q(255_000_000)).String())
	assert.True(t, before[h2].Equal(f.balance(h2, ledger.CodeWallet)), "an unexercised holder is untouched")

	// Σ long receipts = Σ short payments, in the register.
	var exerciseSum, assignmentSum money.Quantity
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT coalesce(sum(amount) FILTER (WHERE kind = 'OPTION_EXERCISE'), 0),
		coalesce(sum(amount) FILTER (WHERE kind = 'OPTION_ASSIGNMENT'), 0) FROM derivative_cash_flows WHERE instrument_id = $1`, call).
		Scan(&exerciseSum, &assignmentSum))
	assert.Equal(t, "150000000", exerciseSum.String())
	assert.Equal(t, "-150000000", assignmentSum.String())

	// FIFO by open time: W1's 2, then 1 of W2's.
	rows, err := testDB.Query(f.ctx, `SELECT seq, account_id, quantity::text, amount::text FROM option_assignments
		WHERE exercise_id = $1 ORDER BY seq`, *out.ExerciseID)
	require.NoError(t, err)
	type asg struct {
		seq       int
		acct      accounts.AccountID
		qty, amnt string
	}
	var got []asg
	for rows.Next() {
		var a asg
		require.NoError(t, rows.Scan(&a.seq, &a.acct, &a.qty, &a.amnt))
		got = append(got, a)
	}
	rows.Close()
	require.Equal(t, []asg{{0, w1, "2", "100000000"}, {1, w2, "1", "50000000"}}, got)

	assert.Equal(t, PositionClosed, f.position(w1, call).Status, "fully assigned")
	assert.Equal(t, "-2", f.position(w2, call).Quantity.String())
	assert.Equal(t, "510000000", f.position(w2, call).MarginPosted.String(), "two thirds of $765 kept, rounded up")
	assert.Equal(t, "1", f.position(h1, call).Quantity.String())
	// H1 realized $150 against three quarters of its $220.22 premium.
	assert.Equal(t, "-15165000", f.position(h1, call).RealizedPnL.String())
	assert.Equal(t, "9890000", f.position(w1, call).RealizedPnL.String(), "$109.89 premium − $100")

	// A replay of the exercise intent exercises nothing more.
	require.NoError(t, f.do(func(ctx context.Context, tx pgx.Tx) error {
		again, err := f.svc.ExerciseOption(ctx, tx, req)
		assert.True(t, again.Replay)
		return err
	}))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM option_exercises WHERE instrument_id = $1`, call))
}

func TestIntegration_ExerciseIsRefusedWhenItCannotBeHonest(t *testing.T) {
	f := newFixture(t)
	european := f.option(instruments.Call, instruments.European, 60_000, start.Add(30*24*time.Hour))
	otm := f.option(instruments.Put, instruments.American, 60_000, start.Add(30*24*time.Hour))
	h := f.account()
	f.fund(h, usdUnits(100_000))
	f.mark(european, 5_500, 65_000)
	f.mark(otm, 100, 65_000)
	f.open(h, european, Long, 1)
	f.open(h, otm, Long, 1)

	exercise := func(ins instruments.InstrumentID) error {
		return f.do(func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.svc.ExerciseOption(ctx, tx, ExerciseRequest{IntentRequest: f.req(h, ins)})
			return err
		})
	}
	err := exercise(european)
	require.Error(t, err)
	assert.Equal(t, errs.CodeUnsupported, errs.CodeOf(err), "a European option is exercised automatically at expiry")
	err = exercise(otm)
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "an out-of-the-money option pays nothing")

	// An exercise settles at a fresh index: a stale one refuses it.
	f.clk.Advance(2 * time.Minute)
	american := f.option(instruments.Call, instruments.American, 60_000, start.Add(30*24*time.Hour))
	f.mark(american, 5_500, 65_000)
	f.open(h, american, Long, 1)
	f.clk.Advance(2 * time.Minute)
	err = exercise(american)
	require.Error(t, err)
	assert.Equal(t, errs.CodeStaleMarketData, errs.CodeOf(err))
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM option_exercises WHERE instrument_id = $1`, american))
}

// TestIntegration_AnalyticsAreReferenceValuesAtTheBoundary checks the Greeks
// as the service delivers them, and that computing them posts nothing.
func TestIntegration_AnalyticsAreReferenceValuesAtTheBoundary(t *testing.T) {
	f := newFixture(t)
	expiry := start.Add(90 * 24 * time.Hour)
	call := f.option(instruments.Call, instruments.European, 65_000, expiry)
	put := f.option(instruments.Put, instruments.European, 65_000, expiry)
	f.mark(call, 6_000, 65_500)
	f.mark(put, 5_000, 65_500)
	journalsBefore := f.count(`SELECT count(*) FROM journal_transactions`)
	for _, c := range []struct {
		ins      instruments.InstrumentID
		lo, hi   int64
		expected string
	}{{call, 0, 1, "CALL"}, {put, -1, 0, "PUT"}} {
		a, err := f.svc.Analytics(f.ctx, testDB, AnalyticsRequest{InstrumentID: c.ins, VolBPS: 6_000, RateBPS: 300})
		require.NoError(t, err)
		assert.Equal(t, AnalyticsLabel, a.Label)
		assert.Equal(t, c.expected, a.Right)
		delta, err := quant.ParseDec(a.Delta)
		require.NoError(t, err)
		assert.True(t, delta.Cmp(quant.DecFromInt64(c.lo)) >= 0 && delta.Cmp(quant.DecFromInt64(c.hi)) <= 0, "delta %s", a.Delta)
		gamma, err := quant.ParseDec(a.Gamma)
		require.NoError(t, err)
		assert.True(t, gamma.Sign() > 0, "gamma %s", a.Gamma)
		vega, err := quant.ParseDec(a.Vega)
		require.NoError(t, err)
		assert.True(t, vega.Sign() > 0, "vega %s", a.Vega)
		assert.NotEmpty(t, a.ImpliedVol, "solved from the mark")
	}
	assert.Equal(t, journalsBefore, f.count(`SELECT count(*) FROM journal_transactions`), "analytics are never posted")
}
