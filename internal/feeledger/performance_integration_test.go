//go:build integration

package feeledger

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// managed is one customer account under a performance fee, accounted in a
// fresh simulated dollar so its balances are its own.
type managed struct {
	acct     accounts.AccountID
	asset    assets.Asset
	strategy string
}

func (f *fixture) managed() managed {
	a := f.simAsset("FLP")
	return managed{acct: f.customer(a.ID, units(100_000)), asset: a, strategy: "strat-" + uuid.NewString()[:8]}
}

var periodStart = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func (f *fixture) open(m managed, start, end time.Time, opening money.USD, hurdle, rate money.BPS) Period {
	f.t.Helper()
	p, err := do(f, func(ctx context.Context, tx pgx.Tx) (Period, error) {
		return f.svc.OpenPeriod(ctx, tx, OpenPeriodRequest{AccountID: m.acct, StrategyRef: m.strategy, FeeAsset: m.asset.ID,
			TermsVersion: "perf-v1", PeriodStart: start, PeriodEnd: end, OpeningNAV: opening, HurdleAnnualBPS: hurdle, FeeRateBPS: rate,
			IdempotencyKey: "open-" + uuid.NewString(), Actor: service})
	})
	require.NoError(f.t, err)
	return p
}

func (f *fixture) accrue(p Period, at time.Time, nav string) Line {
	f.t.Helper()
	l, err := do(f, func(ctx context.Context, tx pgx.Tx) (Line, error) {
		return f.svc.Accrue(ctx, tx, AccrueRequest{PeriodID: p.ID, At: at, NAV: usd(nav), Actor: service})
	})
	require.NoError(f.t, err)
	return l
}

func (f *fixture) flow(p Period, at time.Time, amount, navBefore string) {
	f.t.Helper()
	_, err := do(f, func(ctx context.Context, tx pgx.Tx) (Flow, error) {
		return f.svc.RecordFlow(ctx, tx, FlowRequest{PeriodID: p.ID, At: at, Amount: usd(amount), NAVBefore: usd(navBefore),
			IdempotencyKey: "flow-" + uuid.NewString(), Actor: service})
	})
	require.NoError(f.t, err)
}

func (f *fixture) crystallize(p Period, closing string, contributions ...fees.StrategyContribution) Period {
	f.t.Helper()
	if f.clk.Now().Before(p.Terms.PeriodEnd) {
		f.clk.Set(p.Terms.PeriodEnd)
	}
	out, err := do(f, func(ctx context.Context, tx pgx.Tx) (Period, error) {
		return f.svc.Crystallize(ctx, tx, CrystallizeRequest{PeriodID: p.ID, ClosingNAV: usd(closing), Contributions: contributions,
			Actor: service})
	})
	require.NoError(f.t, err)
	require.NoError(f.t, f.svc.VerifyPeriod(f.ctx, testDB, out.ID), "the stored statement replays byte for byte")
	return out
}

// charged is what the customer paid in performance fees, in cents.
func (f *fixture) charged(m managed) money.Quantity {
	return f.balance(ledger.CustomerAccount(m.acct, ledger.CodeFeesPerformance, m.asset.ID))
}

// cents converts a fee asset quantity (6 decimals) to cents.
func cents(q money.Quantity) string {
	c, err := q.Div(money.QuantityFromInt64(10_000), money.RoundExact)
	if err != nil {
		panic(err)
	}
	return c.String()
}

// TestIntegration_ALosingPeriodChargesNoFee: a period that ends below its
// opening NAV accrues nothing, crystallizes a zero fee, posts no charge, and
// leaves the high-water mark where it was.
func TestIntegration_ALosingPeriodChargesNoFee(t *testing.T) {
	f := newFixture(t)
	m := f.managed()
	p := f.open(m, periodStart, periodStart.Add(30*24*time.Hour), usd("1000.00"), 0, 2_000)
	l := f.accrue(p, periodStart.Add(10*24*time.Hour), "950.00")
	assert.Equal(t, fees.LineNoChange, l.Kind)
	assert.Nil(t, l.JournalTransactionID, "no change moves nothing")
	out := f.crystallize(p, "900.00")
	assert.Equal(t, PeriodCrystallized, out.Status)
	assert.True(t, out.Statement.FeeAmount.IsZero())
	assert.Equal(t, "1000.00", out.Statement.HWMAfter.String(), "the mark stays at the peak")
	assert.Nil(t, out.CrystallizationJournalID)
	assert.True(t, f.charged(m).IsZero())
	require.NotNil(t, out.GrossReturnBPS)
	assert.Equal(t, money.BPS(-1_000), *out.GrossReturnBPS, "-10%")
}

// TestIntegration_AGainBelowTheHighWaterMarkChargesNoFee: after a losing
// period the next opens below its mark; recovering part of the way is a
// positive gross PnL that earns no fee.
func TestIntegration_AGainBelowTheHighWaterMarkChargesNoFee(t *testing.T) {
	f := newFixture(t)
	m := f.managed()
	p1 := f.crystallize(f.open(m, periodStart, periodStart.Add(30*24*time.Hour), usd("1000.00"), 0, 2_000), "800.00")
	p2 := f.open(m, time.Time{}, p1.Terms.PeriodEnd.Add(30*24*time.Hour), money.USD{}, 0, 2_000)
	assert.Equal(t, "800.00", p2.Terms.OpeningNAV.String(), "opens at the previous net close")
	assert.Equal(t, "1000.00", p2.Terms.PriorHWM.String(), "under the previous mark")
	require.NotNil(t, p2.PreviousID)
	assert.Equal(t, p1.ID, *p2.PreviousID)
	l := f.accrue(p2, p2.Terms.PeriodStart.Add(24*time.Hour), "990.00")
	assert.Equal(t, fees.LineNoChange, l.Kind)
	out := f.crystallize(p2, "950.00")
	assert.Equal(t, "150.00", out.Statement.GrossPnL.String(), "a real gain")
	assert.True(t, out.Statement.FeeAmount.IsZero(), "but below the mark")
	assert.True(t, f.charged(m).IsZero())
}

// TestIntegration_DepositsDoNotGeneratePerformance: a deposit raises the
// mark by itself, so NAV that grew only by the deposit earns nothing.
func TestIntegration_DepositsDoNotGeneratePerformance(t *testing.T) {
	f := newFixture(t)
	m := f.managed()
	p := f.open(m, periodStart, periodStart.Add(30*24*time.Hour), usd("1000.00"), 0, 2_000)
	f.flow(p, periodStart.Add(5*24*time.Hour), "500.00", "1000.00")
	l := f.accrue(p, periodStart.Add(6*24*time.Hour), "1500.00")
	assert.Equal(t, fees.LineNoChange, l.Kind)
	assert.Equal(t, "1500.00", l.HWMAdjusted.String())
	out := f.crystallize(p, "1500.00")
	assert.True(t, out.Statement.FeeAmount.IsZero())
	assert.True(t, out.Statement.GrossPnL.IsZero())
	assert.Equal(t, "500.00", out.Statement.NetFlows.String())
	// A flow cannot be slipped in before a valuation already recorded.
	_, err := do(f, func(ctx context.Context, tx pgx.Tx) (Flow, error) {
		return f.svc.RecordFlow(ctx, tx, FlowRequest{PeriodID: p.ID, At: periodStart.Add(24 * time.Hour), Amount: usd("1.00"),
			NAVBefore: usd("1000.00"), IdempotencyKey: "late-" + uuid.NewString(), Actor: service})
	})
	assert.Error(t, err)
}

// TestIntegration_AWithdrawalDoesNotEraseTheAccrual: after a gain is
// accrued, a withdrawal scales the mark with the capital that remains; the
// accrual on the remaining capital stands (reduced pro rata, not erased) and
// is what crystallizes.
func TestIntegration_AWithdrawalDoesNotEraseTheAccrual(t *testing.T) {
	f := newFixture(t)
	m := f.managed()
	p := f.open(m, periodStart, periodStart.Add(30*24*time.Hour), usd("1000.00"), 0, 2_000)
	first := f.accrue(p, periodStart.Add(5*24*time.Hour), "1200.00")
	assert.Equal(t, fees.LineAccrual, first.Kind)
	assert.Equal(t, "40.00", first.AccrualAfter.String(), "20% of the 200 gain")
	f.flow(p, periodStart.Add(6*24*time.Hour), "-600.00", "1200.00")
	after := f.accrue(p, periodStart.Add(7*24*time.Hour), "600.00")
	assert.Equal(t, "500.00", after.HWMAdjusted.String(), "the mark scaled by the half that remains")
	assert.Equal(t, "20.00", after.AccrualAfter.String(), "half the accrual stands with half the capital")
	assert.Equal(t, fees.LineReversal, after.Kind, "the change is a compensating line, never an edit")
	out := f.crystallize(p, "600.00")
	assert.Equal(t, "20.00", out.Statement.FeeAmount.String())
	assert.Equal(t, "2000", cents(f.charged(m)), "20.00 charged")
	// The platform's accrual accounts are back to zero once crystallized.
	assert.True(t, f.balance(ledger.PlatformAccount(ledger.CodePerformanceFeeAccrued, m.asset.ID)).IsZero())
	assert.True(t, f.balance(ledger.PlatformAccount(ledger.CodePerformanceFeeDeferred, m.asset.ID)).IsZero())
	assert.Equal(t, "2000", cents(f.balance(ledger.PlatformAccount(ledger.CodePerformanceFeeRevenue, m.asset.ID))))
}

// TestIntegration_MultiPeriodCrystallizationChainsTheHighWaterMark: three
// periods chained by the database: a fee on the first gain, none on a
// drawdown, and on the recovery a fee only on what passed the old mark.
func TestIntegration_MultiPeriodCrystallizationChainsTheHighWaterMark(t *testing.T) {
	f := newFixture(t)
	m := f.managed()
	month := 30 * 24 * time.Hour
	p1 := f.crystallize(f.open(m, periodStart, periodStart.Add(month), usd("1000.00"), 0, 2_000), "1200.00")
	assert.Equal(t, "40.00", p1.Statement.FeeAmount.String())
	assert.Equal(t, "1160.00", p1.Statement.HWMAfter.String())
	p2 := f.crystallize(f.open(m, time.Time{}, p1.Terms.PeriodEnd.Add(month), money.USD{}, 0, 2_000), "1100.00")
	assert.True(t, p2.Statement.FeeAmount.IsZero())
	assert.Equal(t, "1160.00", p2.Statement.HWMAfter.String())
	p3 := f.crystallize(f.open(m, time.Time{}, p2.Terms.PeriodEnd.Add(month), money.USD{}, 0, 2_000), "1300.00")
	assert.Equal(t, "140.00", p3.Statement.FeeBase.String(), "only the gain above 1160")
	assert.Equal(t, "28.00", p3.Statement.FeeAmount.String())
	assert.Equal(t, "6800", cents(f.charged(m)), "40.00 + 28.00")
	periods, err := f.svc.Periods(f.ctx, testDB, m.acct, m.strategy)
	require.NoError(t, err)
	require.Len(t, periods, 3)
	for i, p := range periods {
		assert.Equal(t, i+1, p.Seq)
		if i > 0 {
			assert.Equal(t, periods[i-1].ID, *p.PreviousID)
			assert.Equal(t, periods[i-1].Statement.ClosingNAVNet, p.Terms.OpeningNAV)
			assert.Equal(t, periods[i-1].Statement.HWMAfter, p.Terms.PriorHWM)
		}
	}
	// Opening a period that does not start where the chain ended is refused.
	_, err = do(f, func(ctx context.Context, tx pgx.Tx) (Period, error) {
		return f.svc.OpenPeriod(ctx, tx, OpenPeriodRequest{AccountID: m.acct, StrategyRef: m.strategy, FeeAsset: m.asset.ID,
			TermsVersion: "perf-v1", PeriodStart: periodStart, PeriodEnd: p3.Terms.PeriodEnd.Add(month), FeeRateBPS: 2_000,
			IdempotencyKey: "gap-" + uuid.NewString(), Actor: service})
	})
	assert.Equal(t, ReasonChainBroken, ReasonOf(err))
}

// TestIntegration_PerformanceFeeRoundsExactly: the fee is the fee base times
// the rate rounded down to the cent, the hurdle threshold is rounded up, and
// the attribution sums to the fee exactly with the remaining cents going to
// the largest remainders.
func TestIntegration_PerformanceFeeRoundsExactly(t *testing.T) {
	f := newFixture(t)
	m := f.managed()
	p := f.open(m, periodStart, periodStart.Add(365*24*time.Hour), usd("1000.00"), 0, 1_999)
	out := f.crystallize(p, "1123.45",
		fees.StrategyContribution{Strategy: "a", GrossPnL: usd("100.00")},
		fees.StrategyContribution{Strategy: "b", GrossPnL: usd("33.45")},
		fees.StrategyContribution{Strategy: "c", GrossPnL: usd("-10.00")})
	assert.Equal(t, "123.45", out.Statement.FeeBase.String())
	assert.Equal(t, "24.67", out.Statement.FeeAmount.String(), "123.45 x 19.99% = 24.677655, rounded down")
	var total money.USD
	for _, a := range out.Statement.Attribution {
		var err error
		total, err = total.Add(a.FeeShare)
		require.NoError(t, err)
		if a.Strategy == "c" {
			assert.True(t, a.FeeShare.IsZero(), "a losing strategy bears none")
		}
	}
	assert.Equal(t, out.Statement.FeeAmount, total)
	assert.Equal(t, 3, f.count(`SELECT count(*) FROM performance_fee_attributions WHERE period_id = $1`, out.ID))

	// A hurdle: 10% a year over half a year on a mark of 1000 is a threshold
	// of exactly 1050 (rounded up), and only the gain above it is charged.
	m2 := f.managed()
	h := f.open(m2, periodStart, periodStart.Add(365*12*time.Hour), usd("1000.00"), 1_000, 2_000)
	ho := f.crystallize(h, "1100.00")
	assert.Equal(t, "1050.00", ho.Statement.Hurdle.Threshold.String())
	assert.Equal(t, "50.00", ho.Statement.FeeBase.String())
	assert.Equal(t, "10.00", ho.Statement.FeeAmount.String())
}

// TestIntegration_ConcurrentAccrualsAndCrystallizationsAreExactlyOnce: the
// same valuation asked for eight times at once is one line, one effect, one
// journal; eight crystallizations at once are one statement and one charge.
func TestIntegration_ConcurrentAccrualsAndCrystallizationsAreExactlyOnce(t *testing.T) {
	f := newFixture(t)
	m := f.managed()
	p := f.open(m, periodStart, periodStart.Add(30*24*time.Hour), usd("1000.00"), 0, 2_000)
	at := periodStart.Add(3 * 24 * time.Hour)
	const n = 8
	var wg sync.WaitGroup
	lines := make(chan LineID, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := do(f, func(ctx context.Context, tx pgx.Tx) (Line, error) {
				return f.svc.Accrue(ctx, tx, AccrueRequest{PeriodID: p.ID, At: at, NAV: usd("1100.00"), Actor: service})
			})
			if assert.NoError(t, err) {
				lines <- l.ID
			}
		}()
	}
	wg.Wait()
	close(lines)
	seen := map[LineID]bool{}
	for l := range lines {
		seen[l] = true
	}
	assert.Len(t, seen, 1)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM performance_fee_lines WHERE period_id = $1`, p.ID))
	assert.Equal(t, "2000", cents(f.balance(ledger.PlatformAccount(ledger.CodePerformanceFeeAccrued, m.asset.ID))), "20.00 accrued once")
	_, err := do(f, func(ctx context.Context, tx pgx.Tx) (Line, error) {
		return f.svc.Accrue(ctx, tx, AccrueRequest{PeriodID: p.ID, At: at, NAV: usd("1200.00"), Actor: service})
	})
	assert.Equal(t, errs.CodeInvalidIdempotencyReuse, errs.CodeOf(err), "one instant has one NAV")

	f.clk.Set(p.Terms.PeriodEnd)
	statements := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := do(f, func(ctx context.Context, tx pgx.Tx) (Period, error) {
				return f.svc.Crystallize(ctx, tx, CrystallizeRequest{PeriodID: p.ID, ClosingNAV: usd("1300.00"), Actor: service})
			})
			if assert.NoError(t, err) {
				statements <- out.StatementHash
			}
		}()
	}
	wg.Wait()
	close(statements)
	hashes := map[string]bool{}
	for h := range statements {
		hashes[h] = true
	}
	assert.Len(t, hashes, 1)
	assert.Equal(t, "6000", cents(f.charged(m)), "60.00 charged once")
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM journal_transactions WHERE kind = 'PERFORMANCE_FEE_CRYSTALLIZATION'
		AND reference_id IN (SELECT id::text FROM performance_fee_lines WHERE period_id = $1)`, p.ID))
	require.NoError(t, f.svc.VerifyPeriod(f.ctx, testDB, p.ID))
}

// TestIntegration_ACrystallizationIsReversedByCompensation: a reversal
// returns exactly the fee and leaves the statement as it was; a period
// another one continues from cannot be reversed; after a reversal a corrected
// period opens in its place; an open period is cancelled by reversing its
// accrual.
func TestIntegration_ACrystallizationIsReversedByCompensation(t *testing.T) {
	f := newFixture(t)
	m := f.managed()
	month := 30 * 24 * time.Hour
	p1 := f.crystallize(f.open(m, periodStart, periodStart.Add(month), usd("1000.00"), 0, 2_000), "1200.00")
	p2 := f.crystallize(f.open(m, time.Time{}, p1.Terms.PeriodEnd.Add(month), money.USD{}, 0, 2_000), "1300.00")
	reverse := func(p Period) (Period, error) {
		return do(f, func(ctx context.Context, tx pgx.Tx) (Period, error) {
			return f.svc.ReverseCrystallization(ctx, tx, ReversePeriodRequest{PeriodID: p.ID, ReasonCode: "WRONG_CLOSING_NAV",
				Reason: "the closing valuation was corrected", Actor: operator})
		})
	}
	_, err := reverse(p1)
	assert.Equal(t, ReasonPeriodContinued, ReasonOf(err))
	before := f.charged(m)
	r2, err := reverse(p2)
	require.NoError(t, err)
	assert.Equal(t, PeriodReversed, r2.Status)
	assert.Equal(t, p2.StatementHash, r2.StatementHash, "the statement stays as it was")
	assert.Equal(t, cents(before.Sub(f.charged(m))), cents(money.QuantityFromInt64(p2.Statement.FeeAmount.Minor()*10_000)),
		"exactly the fee comes back")
	var compensated string
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT status FROM economic_effects WHERE id = $1`, *p2.CrystallizationEffectID).Scan(&compensated))
	assert.Equal(t, "COMPENSATED", compensated)

	corrected := f.crystallize(f.open(m, time.Time{}, p2.Terms.PeriodEnd, money.USD{}, 0, 2_000), "1250.00")
	assert.Equal(t, p1.ID, *corrected.PreviousID, "the corrected period continues from the last standing one")
	assert.Equal(t, p2.Terms.PeriodStart, corrected.Terms.PeriodStart)

	// Cancelling an open period reverses its accrual.
	m2 := f.managed()
	open := f.open(m2, periodStart, periodStart.Add(month), usd("1000.00"), 0, 2_000)
	f.accrue(open, periodStart.Add(24*time.Hour), "1100.00")
	cancelled, err := do(f, func(ctx context.Context, tx pgx.Tx) (Period, error) {
		return f.svc.CancelPeriod(ctx, tx, open.ID, "terms were set up wrongly", operator)
	})
	require.NoError(t, err)
	assert.Equal(t, PeriodCancelled, cancelled.Status)
	assert.True(t, cancelled.Accrued.IsZero())
	assert.True(t, f.balance(ledger.PlatformAccount(ledger.CodePerformanceFeeAccrued, m2.asset.ID)).IsZero())
	assert.True(t, f.charged(m2).IsZero())
}

// TestIntegration_TheDatabaseRefusesInvalidPerformanceStates writes around Go.
func TestIntegration_TheDatabaseRefusesInvalidPerformanceStates(t *testing.T) {
	f := newFixture(t)
	m := f.managed()
	month := 30 * 24 * time.Hour
	p1 := f.crystallize(f.open(m, periodStart, periodStart.Add(month), usd("1000.00"), 0, 2_000), "1200.00")
	open := f.open(m, time.Time{}, p1.Terms.PeriodEnd.Add(month), money.USD{}, 0, 2_000)
	line := f.accrue(open, open.Terms.PeriodStart.Add(24*time.Hour), "1250.00")

	period := func(previous *PeriodID, opening int64) error {
		return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO performance_fee_periods (id, account_id, strategy_ref, period_seq, previous_period_id, mode,
				fee_asset_id, terms_version, period_start, period_end, opening_nav_minor, prior_hwm_minor, hurdle_annual_bps, fee_rate_bps,
				crystallization, prior_accrued_fee_minor, terms_hash, status, accrued_fee_minor, opened_by_actor_type, opened_by_actor_id,
				idempotency_key, request_hash)
				VALUES ($1,$2,$3,3,$4,'PAPER',$5,'raw',$6,$7,$8,$9,0,2000,'PERIOD_END',0,$10,'OPEN',0,'OPERATOR','op',$11,$12)`,
				uuid.New(), m.acct, m.strategy, previous, m.asset.ID, p1.Terms.PeriodEnd, p1.Terms.PeriodEnd.Add(month), opening,
				p1.Statement.HWMAfter.Minor(), hashHex([]byte("x")), "raw-"+uuid.NewString(), make([]byte, 32))
			return err
		})
	}
	assert.Equal(t, "FE006", db.SQLState(period(&p1.ID, 116000)), "a second live period continuing from the same one")
	assert.Equal(t, "FE006", db.SQLState(period(nil, 116000)), "a fresh chain beside a crystallized one")

	// A line that does not continue the accrual (FE007).
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO performance_fee_lines (id, period_id, seq, kind, occurred_at, nav_minor, hwm_adjusted_minor,
			hurdle_threshold_minor, fee_base_minor, amount_minor, accrual_before_minor, accrual_after_minor, effect_id)
			VALUES ($1,$2,2,'NO_CHANGE',$3,125000,116000,116000,9000,0,0,0,$4)`,
			uuid.New(), open.ID, line.At.Add(time.Hour), line.EffectID)
		return err
	})
	assert.Equal(t, "FE007", db.SQLState(err), "accrual_before 0 after a line that left 18.00: %v", err)

	// An accrual changed without a line (FE008 at commit).
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE performance_fee_periods SET accrued_fee_minor = 99999 WHERE id = $1`, open.ID)
		return err
	})
	assert.Equal(t, "FE008", db.SQLState(err), "%v", err)

	// Terms never change; statements are written once (FE009).
	owner, err := pgx.Connect(f.ctx, testMigrateURL)
	require.NoError(t, err)
	defer owner.Close(f.ctx)
	_, err = owner.Exec(f.ctx, `UPDATE performance_fee_periods SET fee_rate_bps = 0 WHERE id = $1`, open.ID)
	assert.Equal(t, "FE009", db.SQLState(err))
	_, err = owner.Exec(f.ctx, `UPDATE performance_fee_periods SET fee_amount_minor = 0 WHERE id = $1`, p1.ID)
	assert.Equal(t, "FE009", db.SQLState(err))
	_, err = owner.Exec(f.ctx, `DELETE FROM performance_fee_lines WHERE period_id = $1`, open.ID)
	assert.Equal(t, "P0001", db.SQLState(err), "lines are append-only")
	// A status change without its transition row (AU001).
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE performance_fee_periods SET status = 'CANCELLED', accrued_fee_minor = 0 WHERE id = $1`, open.ID)
		return err
	})
	require.Error(t, err)
	assert.Contains(t, []string{"AU001", "FE008"}, db.SQLState(err), "%v", err)
}
