//go:build integration

package derivatives

import (
	"context"
	"sync"
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
)

// fundingWindow is the 8-hour funding interval the fixture clock starts in.
var fundingWindow = time.Date(2026, 10, 7, 8, 0, 0, 0, time.UTC)

func (f *fixture) runFunding(perp instruments.InstrumentID, intervalStart time.Time) (RunResult, error) {
	var r RunResult
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r, err = f.svc.RunFunding(ctx, tx, RunRequest{InstrumentID: perp, IntervalStart: intervalStart})
		return err
	})
	return r, err
}

func (f *fixture) spec(ins instruments.InstrumentID) Spec {
	f.t.Helper()
	s, err := loadSpec(f.ctx, testDB, f.repo, ins)
	require.NoError(f.t, err)
	return s
}

// TestIntegration_FundingConservesOnABalancedBook: longs of 3 and 2 contracts
// against shorts of 4 and 1. The premium index is the exact average of the
// interval's (mark − index) / index; every long pays and every short receives
// the same per-contract amount, so Σ paid = Σ received exactly and the
// clearing house's CLEARING_RESULT does not move.
func TestIntegration_FundingConservesOnABalancedBook(t *testing.T) {
	f := newFixture(t)
	perp := f.perp()
	type holder struct {
		acct accounts.AccountID
		n    int64 // signed contracts
	}
	var book []holder
	for _, n := range []int64{3, 2, -4, -1} {
		a := f.account()
		f.fund(a, usdUnits(100_000))
		book = append(book, holder{acct: a, n: n})
	}
	m1 := f.mark(perp, 65_000, 64_935)
	for _, h := range book {
		side := Long
		if h.n < 0 {
			side = Short
		}
		f.open(h.acct, perp, side, absInt(h.n))
	}
	f.clk.Set(start.Add(time.Hour))
	m2 := f.mark(perp, 65_100, 65_000)
	f.clk.Set(fundingWindow.Add(8 * time.Hour))

	spec := f.spec(perp)
	premium, err := PremiumIndexE8([]IndexSample{sampleOf(m1), sampleOf(m2)})
	require.NoError(t, err)
	rate, clamped := ClampRate(premium, DefaultPolicy().MaxFundingRateE8)
	require.False(t, clamped)
	require.Positive(t, rate, "the mark traded above the index: longs pay")
	per, err := fundingPerContract(spec, m2.MarkMantissa, rate)
	require.NoError(t, err)
	require.True(t, per.IsPositive())

	resultBefore := f.clearingBalance(ledger.CodeClearingResult)
	pnlBefore := make([]money.Quantity, len(book))
	for i, h := range book {
		pnlBefore[i] = f.balance(h.acct, ledger.CodeRealizedPnL)
	}
	r, err := f.runFunding(perp, fundingWindow)
	require.NoError(t, err)
	assert.False(t, r.Replay)
	assert.Equal(t, premium, r.PremiumIndexE8)
	assert.Equal(t, rate, r.RateE8)
	assert.Equal(t, per.String(), r.PerContract.String())
	assert.Equal(t, 4, r.Positions)
	assert.Equal(t, q(5).Mul(per).String(), r.Paid.String())
	assert.Equal(t, r.Paid.String(), r.Received.String(), "Σ paid = Σ received exactly")
	assert.True(t, r.Shortfall.IsZero())
	assert.Equal(t, resultBefore.String(), f.clearingBalance(ledger.CodeClearingResult).String(),
		"a balanced book's funding moves nothing to the clearing house")
	for i, h := range book {
		want := q(h.n).Mul(per).Neg() // a long (n > 0) pays, a short receives
		got := f.balance(h.acct, ledger.CodeRealizedPnL).Sub(pnlBefore[i])
		assert.Equal(t, want.String(), got.String(), "account of %d contracts", h.n)
		assert.Equal(t, want.String(), f.position(h.acct, perp).FundingPnL.String())
	}
	assert.Equal(t, 4, f.count(`SELECT count(*) FROM funding_accruals a JOIN funding_rates r ON r.id = a.funding_rate_id
		WHERE r.instrument_id = $1`, perp))

	// A re-run of the interval posts nothing new.
	again, err := f.runFunding(perp, fundingWindow)
	require.NoError(t, err)
	assert.True(t, again.Replay)
	assert.Empty(t, again.Journals)
	assert.Equal(t, rate, again.RateE8)
	assert.Equal(t, 4, f.count(`SELECT count(*) FROM derivative_cash_flows WHERE instrument_id = $1 AND kind = 'FUNDING'`, perp))

	// The next interval, run by eight workers at once, is funded once.
	f.clk.Set(fundingWindow.Add(9 * time.Hour))
	f.mark(perp, 65_050, 65_000)
	f.clk.Set(fundingWindow.Add(16 * time.Hour))
	const n = 8
	var wg sync.WaitGroup
	results := make(chan RunResult, n)
	failures := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := f.runFunding(perp, fundingWindow.Add(8*time.Hour))
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
			assert.Equal(t, r.Paid.String(), r.Received.String())
		}
	}
	assert.Equal(t, 1, fresh)
	assert.Equal(t, 2, f.count(`SELECT count(*) FROM funding_rates WHERE instrument_id = $1`, perp))
	assert.Equal(t, 8, f.count(`SELECT count(*) FROM funding_accruals a JOIN funding_rates r ON r.id = a.funding_rate_id
		WHERE r.instrument_id = $1`, perp))
	assert.Equal(t, resultBefore.String(), f.clearingBalance(ledger.CodeClearingResult).String())
}

// TestIntegration_FundingOnAOneSidedBookIsAbsorbedByTheClearingHouse: only
// longs. The rate is clamped to the policy maximum, the position that closed
// after the funding time still pays for the interval it was open in, the one
// opened after it pays nothing, and CLEARING_RESULT absorbs exactly the
// difference between what was paid and what was received.
func TestIntegration_FundingOnAOneSidedBookIsAbsorbedByTheClearingHouse(t *testing.T) {
	f := newFixture(t)
	perp := f.perp()
	a, b, late := f.account(), f.account(), f.account()
	f.fund(a, usdUnits(100_000))
	f.fund(b, usdUnits(100_000))
	f.fund(late, usdUnits(100_000))
	// $65 000 against an index of $64 000: a 1.5625% premium, clamped to 0.75%.
	f.mark(perp, 65_000, 64_000)
	f.open(a, perp, Long, 3)
	f.open(b, perp, Long, 2)

	end := fundingWindow.Add(8 * time.Hour)
	f.clk.Set(end.Add(20 * time.Minute))
	f.mark(perp, 65_000, 65_000)
	f.open(late, perp, Long, 4) // after the funding time: owes nothing for it
	f.closeAll(b, perp)         // after the funding time: still owes it
	require.Equal(t, PositionClosed, f.position(b, perp).Status)

	resultBefore := f.clearingBalance(ledger.CodeClearingResult)
	bWallet := f.balance(b, ledger.CodeWallet)
	r, err := f.runFunding(perp, fundingWindow)
	require.NoError(t, err)
	assert.True(t, r.Clamped)
	assert.Equal(t, int64(1_562_500), r.PremiumIndexE8)
	assert.Equal(t, int64(750_000), r.RateE8)
	assert.Equal(t, "4875000", r.PerContract.String(), "0.75% of $650")
	assert.Equal(t, 2, r.Positions, "a and b were open at the funding time; late was not")
	assert.Equal(t, "24375000", r.Paid.String(), "5 contracts × $4.875")
	assert.True(t, r.Received.IsZero())
	assert.Equal(t, resultBefore.Add(r.Paid).String(), f.clearingBalance(ledger.CodeClearingResult).String(),
		"the clearing house absorbs exactly Σ paid − Σ received")
	assert.Equal(t, bWallet.Sub(q(9_750_000)).String(), f.balance(b, ledger.CodeWallet).String(), "the closed position paid its 2 contracts")
	pb := f.position(b, perp)
	assert.Equal(t, PositionClosed, pb.Status)
	assert.Equal(t, "-9750000", pb.FundingPnL.String(), "funding is booked against the closed position")
	assert.True(t, f.position(late, perp).FundingPnL.IsZero())
}

func TestIntegration_FundingFailsClosedWithoutMarksOrOffTheGrid(t *testing.T) {
	f := newFixture(t)
	perp := f.perp()
	f.clk.Set(fundingWindow.Add(8 * time.Hour))
	_, err := f.runFunding(perp, fundingWindow)
	require.Error(t, err)
	assert.Equal(t, errs.CodeStaleMarketData, errs.CodeOf(err), "funding is never charged on no price")
	_, err = f.runFunding(perp, fundingWindow.Add(time.Hour))
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "an interval start off the 8-hour grid")
	_, err = f.runFunding(perp, fundingWindow.Add(8*time.Hour))
	require.Error(t, err, "an interval that has not ended")
	fut := f.future(start.Add(48 * time.Hour))
	_, err = f.runFunding(fut, fundingWindow)
	require.Error(t, err, "funding is a perpetual's")
}
