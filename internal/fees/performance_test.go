package fees

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

func date(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

// perfTerms is a 20% fee, no hurdle, January 2026 period.
func perfTerms(t testing.TB, opening, hwm string) PerformanceTerms {
	return PerformanceTerms{
		AccountRef:      "acct-1",
		TermsVersion:    "perf-terms-v1",
		PeriodStart:     date(2026, time.January, 1),
		PeriodEnd:       date(2026, time.February, 1),
		OpeningNAV:      usd(t, opening),
		PriorHWM:        usd(t, hwm),
		FeeRateBPS:      2_000,
		Crystallization: CrystallizeAtPeriodEnd,
	}
}

func flow(t testing.TB, at time.Time, amount, navBefore string) CapitalFlow {
	return CapitalFlow{At: at, Amount: usd(t, amount), NAVBefore: usd(t, navBefore)}
}

// crystallize computes a statement and checks every identity a statement
// must satisfy, plus the auditor's replay.
func crystallize(t *testing.T, terms PerformanceTerms, closing string, contributions ...StrategyContribution) PerformanceStatement {
	t.Helper()
	st, err := ComputePerformanceFee(terms, usd(t, closing), contributions)
	require.NoError(t, err)
	requireStatementIdentities(t, terms, st)
	return st
}

func requireStatementIdentities(t *testing.T, terms PerformanceTerms, st PerformanceStatement) {
	t.Helper()
	require.Equal(t, PerformanceModeSandbox, st.Mode, "a statement is never a charge")
	gross, err := st.ClosingNAVGross.Sub(st.OpeningNAV)
	require.NoError(t, err)
	gross, err = gross.Sub(st.NetFlows)
	require.NoError(t, err)
	require.Equal(t, gross, st.GrossPnL, "gross PnL = closing − opening − net flows")
	fee, err := st.FeeBase.MulBPS(st.FeeRateBPS, money.RoundDown)
	require.NoError(t, err)
	require.Equal(t, fee, st.FeeAmount, "fee = base × rate, rounded down")
	net, err := st.ClosingNAVGross.Sub(st.FeeAmount)
	require.NoError(t, err)
	require.Equal(t, net, st.ClosingNAVNet)
	require.Equal(t, maxUSD(st.HWMAdjusted, st.ClosingNAVNet), st.HWMAfter)
	require.False(t, st.FeeAmount.IsNegative())
	if st.ClosingNAVGross.Cmp(st.HWMAdjusted) <= 0 {
		require.True(t, st.FeeAmount.IsZero(), "no fee at or below the adjusted high-water mark")
	}

	accrued := st.PriorAccruedFee
	for i, l := range st.Lines {
		require.Equal(t, i+1, l.Seq)
		if l.Kind == LineCrystallization {
			require.Equal(t, len(st.Lines)-1, i, "crystallization is the last line")
			require.Equal(t, st.FeeAmount, l.Amount)
			require.True(t, l.AccrualAfter.IsZero())
			continue
		}
		require.Equal(t, accrued, l.AccrualBefore)
		accrued, err = accrued.Add(l.Amount)
		require.NoError(t, err)
		require.Equal(t, accrued, l.AccrualAfter)
	}
	require.Equal(t, st.FeeAmount, accrued, "prior accrual + Σ accrual and reversal lines = fee")

	if len(st.Attribution) > 0 {
		var sum money.USD
		for _, a := range st.Attribution {
			require.False(t, a.FeeShare.IsNegative())
			if !a.Eligible {
				require.True(t, a.FeeShare.IsZero(), "a losing strategy bears no fee")
			}
			sum, err = sum.Add(a.FeeShare)
			require.NoError(t, err)
		}
		require.Equal(t, st.FeeAmount, sum, "attribution sums exactly to the fee")
	}
	require.NoError(t, VerifyPerformanceStatement(terms, st), "the auditor's replay accepts the statement")
}

// TestPerformance_FeeTable: one period per row, every number by hand. The
// fee is 20% unless stated; the hurdle is zero unless stated.
func TestPerformance_FeeTable(t *testing.T) {
	jan := func(d int) time.Time { return date(2026, time.January, d) }
	cases := []struct {
		name                                           string
		opening, hwm, closing                          string
		flows                                          []CapitalFlow
		start, end                                     time.Time
		hurdle                                         money.BPS
		gross, adjusted, threshold, base, fee, net, hw string
	}{
		{name: "losing period", opening: "100000.00", hwm: "100000.00", closing: "90000.00",
			gross: "-10000.00", adjusted: "100000.00", threshold: "100000.00", base: "0.00", fee: "0.00", net: "90000.00", hw: "100000.00"},
		{name: "gain below the mark", opening: "80000.00", hwm: "100000.00", closing: "95000.00",
			gross: "15000.00", adjusted: "100000.00", threshold: "100000.00", base: "0.00", fee: "0.00", net: "95000.00", hw: "100000.00"},
		{name: "recovery exactly to the mark", opening: "90000.00", hwm: "100000.00", closing: "100000.00",
			gross: "10000.00", adjusted: "100000.00", threshold: "100000.00", base: "0.00", fee: "0.00", net: "100000.00", hw: "100000.00"},
		{name: "gain above the mark", opening: "100000.00", hwm: "100000.00", closing: "110000.00",
			gross: "10000.00", adjusted: "100000.00", threshold: "100000.00", base: "10000.00", fee: "2000.00", net: "108000.00", hw: "108000.00"},
		{name: "only the part above the mark", opening: "95000.00", hwm: "100000.00", closing: "104000.00",
			gross: "9000.00", adjusted: "100000.00", threshold: "100000.00", base: "4000.00", fee: "800.00", net: "103200.00", hw: "103200.00"},
		{name: "deposit only", opening: "100000.00", hwm: "100000.00", closing: "150000.00",
			flows: []CapitalFlow{flow(t, jan(15), "50000.00", "100000.00")},
			gross: "0.00", adjusted: "150000.00", threshold: "150000.00", base: "0.00", fee: "0.00", net: "150000.00", hw: "150000.00"},
		{name: "a deposit never fills the hole below the mark", opening: "80000.00", hwm: "100000.00", closing: "100000.00",
			flows: []CapitalFlow{flow(t, jan(15), "20000.00", "80000.00")},
			gross: "0.00", adjusted: "120000.00", threshold: "120000.00", base: "0.00", fee: "0.00", net: "100000.00", hw: "120000.00"},
		{name: "deposit after a gain", opening: "100000.00", hwm: "100000.00", closing: "165000.00",
			flows: []CapitalFlow{flow(t, jan(15), "50000.00", "110000.00")},
			gross: "15000.00", adjusted: "150000.00", threshold: "150000.00", base: "15000.00", fee: "3000.00", net: "162000.00", hw: "162000.00"},
		{name: "withdrawal halves the mark", opening: "80000.00", hwm: "100000.00", closing: "60000.00",
			flows: []CapitalFlow{flow(t, jan(10), "-40000.00", "80000.00")},
			gross: "20000.00", adjusted: "50000.00", threshold: "50000.00", base: "10000.00", fee: "2000.00", net: "58000.00", hw: "58000.00"},
		{name: "withdrawal rounds the mark up", opening: "70000.00", hwm: "100000.00", closing: "90000.00",
			flows: []CapitalFlow{flow(t, jan(10), "-10000.00", "70000.00")},
			gross: "30000.00", adjusted: "85714.29", threshold: "85714.29", base: "4285.71", fee: "857.14", net: "89142.86", hw: "89142.86"},
		{name: "never more than the period earned", opening: "100000.00", hwm: "100000.00", closing: "60000.00",
			flows: []CapitalFlow{flow(t, jan(10), "-25000.00", "50000.00")},
			gross: "-15000.00", adjusted: "50000.00", threshold: "50000.00", base: "0.00", fee: "0.00", net: "60000.00", hw: "60000.00"},
		{name: "full withdrawal then a fresh deposit", opening: "100000.00", hwm: "120000.00", closing: "5500.00",
			flows: []CapitalFlow{flow(t, jan(10), "-100000.00", "100000.00"), flow(t, jan(20), "5000.00", "0.00")},
			gross: "500.00", adjusted: "5000.00", threshold: "5000.00", base: "500.00", fee: "100.00", net: "5400.00", hw: "5400.00"},
		{name: "73-day hurdle is exactly 1% of 5%", opening: "100000.00", hwm: "100000.00", closing: "102000.00",
			start: date(2026, time.January, 1), end: date(2026, time.March, 15), hurdle: 500,
			gross: "2000.00", adjusted: "100000.00", threshold: "101000.00", base: "1000.00", fee: "200.00", net: "101800.00", hw: "101800.00"},
		{name: "30-day hurdle rounds the threshold up", opening: "100000.00", hwm: "100000.00", closing: "100500.00",
			start: date(2026, time.January, 1), end: date(2026, time.January, 31), hurdle: 500,
			gross: "500.00", adjusted: "100000.00", threshold: "100410.96", base: "89.04", fee: "17.80", net: "100482.20", hw: "100482.20"},
		{name: "a leap year is 366/365 of the annual hurdle", opening: "100000.00", hwm: "100000.00", closing: "110027.40",
			start: date(2028, time.January, 1), end: date(2029, time.January, 1), hurdle: 1_000,
			gross: "10027.40", adjusted: "100000.00", threshold: "110027.40", base: "0.00", fee: "0.00", net: "110027.40", hw: "110027.40"},
		{name: "half a day of hurdle", opening: "100000.00", hwm: "100000.00", closing: "100100.00",
			start: date(2026, time.January, 1), end: date(2026, time.January, 1).Add(12 * time.Hour), hurdle: 1_000,
			gross: "100.00", adjusted: "100000.00", threshold: "100013.70", base: "86.30", fee: "17.26", net: "100082.74", hw: "100082.74"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			terms := perfTerms(t, c.opening, c.hwm)
			terms.Flows = c.flows
			terms.HurdleAnnualBPS = c.hurdle
			if !c.start.IsZero() {
				terms.PeriodStart, terms.PeriodEnd = c.start, c.end
			}
			st := crystallize(t, terms, c.closing)
			require.Equal(t,
				[]string{c.gross, c.adjusted, c.threshold, c.base, c.fee, c.net, c.hw},
				[]string{st.GrossPnL.String(), st.HWMAdjusted.String(), st.Hurdle.Threshold.String(), st.FeeBase.String(),
					st.FeeAmount.String(), st.ClosingNAVNet.String(), st.HWMAfter.String()})
			require.Equal(t, usd(t, c.hwm), st.HWMBefore)
		})
	}
}

// TestPerformance_WithdrawalDoesNotEraseTheMark: after a losing period and a
// withdrawal of half the account, a recovery is charged only above the
// pro-rated mark (50,000), not on the whole recovery and not from zero.
func TestPerformance_WithdrawalDoesNotEraseTheMark(t *testing.T) {
	p1 := crystallize(t, perfTerms(t, "100000.00", "100000.00"), "80000.00")
	require.Equal(t, "0.00", p1.FeeAmount.String())
	require.Equal(t, "100000.00", p1.HWMAfter.String(), "a loss leaves the mark where it was")

	terms := p1.NextTerms(date(2026, time.March, 1), []CapitalFlow{flow(t, date(2026, time.February, 10), "-40000.00", "80000.00")})
	p2 := crystallize(t, terms, "60000.00")
	require.Len(t, p2.Flows, 1)
	require.Equal(t, "100000.00", p2.Flows[0].HWMBefore.String())
	require.Equal(t, "50000.00", p2.Flows[0].HWMAfter.String(), "half the NAV withdrawn, half the mark remains")
	require.Equal(t, "40000.00", p2.Flows[0].NAVAfter.String())
	require.Equal(t, "20000.00", p2.GrossPnL.String(), "the account recovered 20,000")
	require.Equal(t, "10000.00", p2.FeeBase.String(), "but only 10,000 of it is above the pro-rated mark")
	require.Equal(t, "2000.00", p2.FeeAmount.String())
}

// TestPerformance_MultiPeriodCrystallization chains five monthly periods
// through ups, downs, a deposit and a withdrawal:
//
//	Jan  100,000 → 110,000                       fee 2,000  net 108,000  mark 108,000
//	Feb  108,000 →  97,200                       fee     0  net  97,200  mark 108,000
//	Mar   97,200 → 115,000 (deposit 10,000)      fee     0  net 115,000  mark 118,000
//	Apr  115,000 → 125,000                       fee 1,400  net 123,600  mark 123,600
//	May  123,600 →  70,000 (withdraw half)       fee 1,640  net  68,360  mark  68,360
func TestPerformance_MultiPeriodCrystallization(t *testing.T) {
	jan := crystallize(t, perfTerms(t, "100000.00", "100000.00"), "110000.00")
	feb := crystallize(t, jan.NextTerms(date(2026, time.March, 1), nil), "97200.00")
	mar := crystallize(t, feb.NextTerms(date(2026, time.April, 1),
		[]CapitalFlow{flow(t, date(2026, time.March, 10), "10000.00", "100000.00")}), "115000.00")
	apr := crystallize(t, mar.NextTerms(date(2026, time.May, 1), nil), "125000.00")
	may := crystallize(t, apr.NextTerms(date(2026, time.June, 1),
		[]CapitalFlow{flow(t, date(2026, time.May, 15), "-61800.00", "123600.00")}), "70000.00")

	rows := [][]string{}
	for _, st := range []PerformanceStatement{jan, feb, mar, apr, may} {
		rows = append(rows, []string{st.OpeningNAV.String(), st.HWMBefore.String(), st.FeeAmount.String(), st.ClosingNAVNet.String(), st.HWMAfter.String()})
	}
	require.Equal(t, [][]string{
		{"100000.00", "100000.00", "2000.00", "108000.00", "108000.00"},
		{"108000.00", "108000.00", "0.00", "97200.00", "108000.00"},
		{"97200.00", "108000.00", "0.00", "115000.00", "118000.00"},
		{"115000.00", "118000.00", "1400.00", "123600.00", "123600.00"},
		{"123600.00", "123600.00", "1640.00", "68360.00", "68360.00"},
	}, rows)
	require.Equal(t, "7800.00", mar.GrossPnL.String(), "March gained 7,800 but stayed below the deposit-raised mark")
	require.Equal(t, "7000.00", apr.FeeBase.String(), "April is charged only above the 118,000 mark, not on its whole 10,000 gain")

	hashes := map[string]bool{}
	for _, st := range []PerformanceStatement{jan, feb, mar, apr, may} {
		h, err := st.Hash()
		require.NoError(t, err)
		require.False(t, hashes[h])
		hashes[h] = true
	}
}

// TestPerformance_AccrualThenReversal: accruals follow NAV within the period;
// a fall appends a compensating REVERSAL line and earlier lines never change.
//
//	Jan 10  NAV 105,000  accrual 1,000   ACCRUAL   +1,000
//	Jan 20  NAV 102,000  accrual   400   REVERSAL    −600
//	Jan 25  NAV 102,000  accrual   400   NO_CHANGE      0
//	Jan 28  NAV  99,000  accrual     0   REVERSAL    −400
//	Feb  1  NAV 103,000  fee       600   ACCRUAL     +600, then CRYSTALLIZATION 600
func TestPerformance_AccrualThenReversal(t *testing.T) {
	terms := perfTerms(t, "100000.00", "100000.00")
	p, err := OpenPerformancePeriod(terms)
	require.NoError(t, err)
	steps := []struct {
		day    int
		nav    string
		kind   PerformanceLineKind
		amount string
		after  string
	}{
		{10, "105000.00", LineAccrual, "1000.00", "1000.00"},
		{20, "102000.00", LineReversal, "-600.00", "400.00"},
		{25, "102000.00", LineNoChange, "0.00", "400.00"},
		{28, "99000.00", LineReversal, "-400.00", "0.00"},
	}
	var history [][]PerformanceLine
	for _, s := range steps {
		line, err := p.Accrue(NAVSnapshot{At: date(2026, time.January, s.day), NAV: usd(t, s.nav)})
		require.NoError(t, err)
		require.Equal(t, s.kind, line.Kind, "day %d", s.day)
		require.Equal(t, s.amount, line.Amount.String(), "day %d", s.day)
		require.Equal(t, s.after, line.AccrualAfter.String(), "day %d", s.day)
		require.Equal(t, usd(t, s.after), p.Accrued())
		history = append(history, p.Lines())
	}
	st, err := p.Crystallize(usd(t, "103000.00"), nil)
	require.NoError(t, err)
	requireStatementIdentities(t, terms, st)
	require.Equal(t, "600.00", st.FeeAmount.String())
	require.Len(t, st.Lines, 6)
	require.Equal(t, LineAccrual, st.Lines[4].Kind)
	require.Equal(t, "600.00", st.Lines[4].Amount.String())
	require.Equal(t, LineCrystallization, st.Lines[5].Kind)
	for _, h := range history {
		require.Equal(t, h, st.Lines[:len(h)], "a later accrual never edits an earlier line")
	}

	_, err = p.Accrue(NAVSnapshot{At: date(2026, time.January, 30), NAV: usd(t, "1.00")})
	require.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))
	_, err = p.Crystallize(usd(t, "1.00"), nil)
	require.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))
}

// TestPerformance_AccrualProratesTheHurdleToTheSnapshot: a 36.5% annual
// hurdle is 0.1% a day. On day 10 the threshold is 101,000 and NAV 102,000
// accrues 200; at the period end (31 days) it is 103,100, so NAV 103,000
// earns nothing and the 200 is reversed.
func TestPerformance_AccrualProratesTheHurdleToTheSnapshot(t *testing.T) {
	terms := perfTerms(t, "100000.00", "100000.00")
	terms.HurdleAnnualBPS = 3_650
	p, err := OpenPerformancePeriod(terms)
	require.NoError(t, err)
	line, err := p.Accrue(NAVSnapshot{At: date(2026, time.January, 11), NAV: usd(t, "102000.00")})
	require.NoError(t, err)
	require.Equal(t, "101000.00", line.HurdleThreshold.String())
	require.Equal(t, "200.00", line.Amount.String())

	st, err := p.Crystallize(usd(t, "103000.00"), nil)
	require.NoError(t, err)
	requireStatementIdentities(t, terms, st)
	require.Equal(t, "103100.00", st.Hurdle.Threshold.String())
	require.Equal(t, LineReversal, st.Lines[1].Kind)
	require.Equal(t, "-200.00", st.Lines[1].Amount.String())
	require.True(t, st.FeeAmount.IsZero())
	require.Equal(t, "2678400000000000", st.Hurdle.ElapsedNanos, "31 days")
	require.Equal(t, "31536000000000000", st.Hurdle.YearNanos)
	require.Equal(t, HurdleConvention, st.Hurdle.Convention)
}

// TestPerformance_PriorAccrualIsReconciled: an accrual carried in from
// earlier sub-periods is trued up at the period end by a new line.
func TestPerformance_PriorAccrualIsReconciled(t *testing.T) {
	terms := perfTerms(t, "100000.00", "100000.00")
	terms.PriorAccruedFee = usd(t, "500.00")
	st := crystallize(t, terms, "101500.00")
	require.Equal(t, "300.00", st.FeeAmount.String())
	require.Len(t, st.Lines, 2)
	require.Equal(t, LineReversal, st.Lines[0].Kind)
	require.Equal(t, "-200.00", st.Lines[0].Amount.String())
	require.Equal(t, "500.00", st.Lines[0].AccrualBefore.String())
	require.Equal(t, "300.00", st.Lines[1].Amount.String())
}

// TestPerformance_RoundingTables pins every rounding rule: the fee rounds
// down, an adjusted mark and a hurdle threshold round up.
func TestPerformance_RoundingTables(t *testing.T) {
	t.Run("fee rounds down", func(t *testing.T) {
		for _, c := range []struct {
			base string
			rate money.BPS
			fee  string
		}{
			{"0.04", 2_000, "0.00"}, // 0.008
			{"0.05", 2_000, "0.01"}, // exact
			{"1.23", 2_000, "0.24"}, // 0.246
			{"99.99", 2_500, "24.99"},
			{"0.01", 10_000, "0.01"},
			{"12345.67", 1, "1.23"}, // 1.234567
		} {
			terms := perfTerms(t, "1000.00", "1000.00")
			terms.FeeRateBPS = c.rate
			closing, err := usd(t, "1000.00").Add(usd(t, c.base))
			require.NoError(t, err)
			st := crystallize(t, terms, closing.String())
			require.Equal(t, c.base, st.FeeBase.String())
			require.Equal(t, c.fee, st.FeeAmount.String(), "base %s at %d bps", c.base, c.rate)
		}
	})
	t.Run("withdrawal rounds the mark up", func(t *testing.T) {
		for _, c := range []struct{ hwm, navBefore, withdrawal, after string }{
			{"100000.00", "70000.00", "10000.00", "85714.29"}, // 85714.2857…
			{"100.00", "3.00", "1.00", "66.67"},               // 66.666…
			{"100.00", "3.00", "2.00", "33.34"},               // 33.333…
			{"0.01", "3.00", "1.00", "0.01"},                  // 0.00666…
			{"100.00", "80.00", "40.00", "50.00"},             // exact
			{"100.00", "80.00", "80.00", "0.00"},              // everything withdrawn
		} {
			rec, err := applyFlow(CapitalFlow{At: date(2026, time.January, 2), Amount: usd(t, "-"+c.withdrawal), NAVBefore: usd(t, c.navBefore)}, usd(t, c.hwm))
			require.NoError(t, err)
			require.Equal(t, c.after, rec.HWMAfter.String(), "%+v", c)
		}
	})
	t.Run("hurdle threshold rounds up", func(t *testing.T) {
		for _, c := range []struct {
			hwm       string
			annual    money.BPS
			elapsed   time.Duration
			threshold string
		}{
			{"100000.00", 500, 73 * 24 * time.Hour, "101000.00"},    // exactly 1%
			{"100000.00", 500, 30 * 24 * time.Hour, "100410.96"},    // 100410.958904…
			{"100000.00", 1_000, 366 * 24 * time.Hour, "110027.40"}, // 110027.397260…
			{"100000.00", 1_000, 12 * time.Hour, "100013.70"},       // 100013.698630…
			{"0.01", 1, time.Nanosecond, "0.02"},                    // any positive excess rounds up
			{"0.00", 10_000, HurdleYear, "0.00"},
			{"100.00", 0, HurdleYear, "100.00"},
		} {
			got, err := hurdleThreshold(usd(t, c.hwm), c.annual, c.elapsed)
			require.NoError(t, err)
			require.Equal(t, c.threshold, got.String(), "%+v", c)
		}
	})
}

func TestPerformance_Attribution(t *testing.T) {
	t.Run("a remainder cent goes to the largest remainder, ties by name", func(t *testing.T) {
		// 3.00 gained at 10.34% is a 0.31 fee: 10⅓ cents each, so one
		// leftover cent goes to the alphabetically first strategy.
		terms := perfTerms(t, "100.00", "100.00")
		terms.FeeRateBPS = 1_034
		st := crystallize(t, terms, "103.00",
			StrategyContribution{"beta", usd(t, "1.00")},
			StrategyContribution{"alpha", usd(t, "1.00")},
			StrategyContribution{"gamma", usd(t, "1.00")})
		require.Equal(t, "0.31", st.FeeAmount.String())
		require.Equal(t, []StrategyFeeShare{
			{Strategy: "alpha", GrossPnL: usd(t, "1.00"), Eligible: true, FeeShare: usd(t, "0.11"), RemainderAllocated: usd(t, "0.01")},
			{Strategy: "beta", GrossPnL: usd(t, "1.00"), Eligible: true, FeeShare: usd(t, "0.10")},
			{Strategy: "gamma", GrossPnL: usd(t, "1.00"), Eligible: true, FeeShare: usd(t, "0.10")},
		}, st.Attribution)
	})
	t.Run("only positive contributors bear the fee", func(t *testing.T) {
		// 2.50 net gain at 4% is a 0.10 fee over 3.00 of positive gains:
		// A 6⅔ cents, B 3⅓; A has the larger remainder.
		terms := perfTerms(t, "100.00", "100.00")
		terms.FeeRateBPS = 400
		st := crystallize(t, terms, "102.50",
			StrategyContribution{"A", usd(t, "2.00")},
			StrategyContribution{"B", usd(t, "1.00")},
			StrategyContribution{"C", usd(t, "-0.50")})
		require.Equal(t, "0.10", st.FeeAmount.String())
		shares := []string{}
		for _, a := range st.Attribution {
			shares = append(shares, a.Strategy+"="+a.FeeShare.String())
		}
		require.Equal(t, []string{"A=0.07", "B=0.03", "C=0.00"}, shares)
		require.False(t, st.Attribution[2].Eligible)
	})
	t.Run("no fee, nothing attributed", func(t *testing.T) {
		st := crystallize(t, perfTerms(t, "100.00", "100.00"), "90.00",
			StrategyContribution{"A", usd(t, "5.00")}, StrategyContribution{"B", usd(t, "-15.00")})
		for _, a := range st.Attribution {
			require.True(t, a.FeeShare.IsZero())
		}
	})
	t.Run("contributions are validated before anything changes", func(t *testing.T) {
		terms := perfTerms(t, "100.00", "100.00")
		p, err := OpenPerformancePeriod(terms)
		require.NoError(t, err)
		closing := usd(t, "110.00")
		_, err = p.Crystallize(closing, []StrategyContribution{{"A", usd(t, "4.00")}, {"B", usd(t, "5.00")}})
		requireFields(t, err, errs.CodeValidationFailed, "contributions")
		_, err = p.Crystallize(closing, []StrategyContribution{{"A", usd(t, "5.00")}, {"A", usd(t, "5.00")}})
		requireFields(t, err, errs.CodeValidationFailed, "contributions[1].strategy")
		_, err = p.Crystallize(closing, []StrategyContribution{{"", usd(t, "10.00")}})
		requireFields(t, err, errs.CodeValidationFailed, "contributions[0].strategy")
		require.Empty(t, p.Lines(), "a refused crystallization appends nothing")

		st, err := p.Crystallize(closing, []StrategyContribution{{"A", usd(t, "4.00")}, {"B", usd(t, "6.00")}})
		require.NoError(t, err, "and may be retried")
		requireStatementIdentities(t, terms, st)
		require.Equal(t, "2.00", st.FeeAmount.String())
	})
}

func TestPerformance_TermsValidation(t *testing.T) {
	jan := func(d int) time.Time { return date(2026, time.January, d) }
	cases := []struct {
		name  string
		mut   func(*PerformanceTerms)
		field string
	}{
		{"no account", func(x *PerformanceTerms) { x.AccountRef = "" }, "account_ref"},
		{"no terms version", func(x *PerformanceTerms) { x.TermsVersion = strings.Repeat("v", MaxLabelLength+1) }, "terms_version"},
		{"no start", func(x *PerformanceTerms) { x.PeriodStart = time.Time{} }, "period_start"},
		{"end before start", func(x *PerformanceTerms) { x.PeriodEnd = x.PeriodStart }, "period_end"},
		{"endless period", func(x *PerformanceTerms) { x.PeriodEnd = x.PeriodStart.Add(MaxPerformancePeriod + 1) }, "period_end"},
		{"negative opening", func(x *PerformanceTerms) { x.OpeningNAV = money.USDFromMinor(-1) }, "opening_nav"},
		{"mark below opening", func(x *PerformanceTerms) { x.PriorHWM = money.USDFromMinor(x.OpeningNAV.Minor() - 1) }, "prior_hwm"},
		{"hurdle above 100%", func(x *PerformanceTerms) { x.HurdleAnnualBPS = 10_001 }, "hurdle_annual_bps"},
		{"negative rate", func(x *PerformanceTerms) { x.FeeRateBPS = -1 }, "fee_rate_bps"},
		{"unknown crystallization", func(x *PerformanceTerms) { x.Crystallization = "QUARTERLY" }, "crystallization"},
		{"negative prior accrual", func(x *PerformanceTerms) { x.PriorAccruedFee = money.USDFromMinor(-1) }, "prior_accrued_fee"},
		{"flow at the period start", func(x *PerformanceTerms) { x.Flows = []CapitalFlow{flow(t, x.PeriodStart, "1.00", "1.00")} }, "flows[0].at"},
		{"flow after the period end", func(x *PerformanceTerms) {
			x.Flows = []CapitalFlow{flow(t, x.PeriodEnd.Add(time.Nanosecond), "1.00", "1.00")}
		}, "flows[0].at"},
		{"simultaneous flows", func(x *PerformanceTerms) {
			x.Flows = []CapitalFlow{flow(t, jan(5), "1.00", "1.00"), flow(t, jan(5), "1.00", "2.00")}
		}, "flows[1].at"},
		{"flows out of order", func(x *PerformanceTerms) {
			x.Flows = []CapitalFlow{flow(t, jan(5), "1.00", "1.00"), flow(t, jan(4), "1.00", "2.00")}
		}, "flows[1].at"},
		{"zero flow", func(x *PerformanceTerms) { x.Flows = []CapitalFlow{flow(t, jan(5), "0.00", "1.00")} }, "flows[0].amount"},
		{"overdrawn", func(x *PerformanceTerms) { x.Flows = []CapitalFlow{flow(t, jan(5), "-100.01", "100.00")} }, "flows[0].amount"},
		{"negative NAV before a flow", func(x *PerformanceTerms) { x.Flows = []CapitalFlow{flow(t, jan(5), "1.00", "-1.00")} }, "flows[0].nav_before"},
		{"unrepresentable withdrawal", func(x *PerformanceTerms) {
			x.Flows = []CapitalFlow{{At: jan(5), Amount: money.MinUSD(), NAVBefore: money.MaxUSD()}}
		}, "flows[0].amount"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			terms := perfTerms(t, "100.00", "100.00")
			c.mut(&terms)
			requireFields(t, terms.Validate(), errs.CodeValidationFailed, c.field)
			_, err := OpenPerformancePeriod(terms)
			requireFields(t, err, errs.CodeValidationFailed, c.field)
		})
	}
}

func TestPerformance_SnapshotValidation(t *testing.T) {
	terms := perfTerms(t, "100.00", "100.00")
	p, err := OpenPerformancePeriod(terms)
	require.NoError(t, err)
	for _, c := range []struct {
		name  string
		s     NAVSnapshot
		field string
	}{
		{"no time", NAVSnapshot{NAV: usd(t, "1.00")}, "at"},
		{"at the period start", NAVSnapshot{At: terms.PeriodStart, NAV: usd(t, "1.00")}, "at"},
		{"at the period end", NAVSnapshot{At: terms.PeriodEnd, NAV: usd(t, "1.00")}, "at"},
		{"negative NAV", NAVSnapshot{At: date(2026, time.January, 5), NAV: money.USDFromMinor(-1)}, "nav"},
	} {
		_, err := p.Accrue(c.s)
		requireFields(t, err, errs.CodeValidationFailed, c.field)
	}
	_, err = p.Accrue(NAVSnapshot{At: date(2026, time.January, 10), NAV: usd(t, "101.00")})
	require.NoError(t, err)
	_, err = p.Accrue(NAVSnapshot{At: date(2026, time.January, 10), NAV: usd(t, "101.00")})
	requireFields(t, err, errs.CodeValidationFailed, "at")
	_, err = p.Crystallize(money.USDFromMinor(-1), nil)
	requireFields(t, err, errs.CodeValidationFailed, "closing_nav")
}

// TestPerformance_Overflow: an amount outside the USD range is an OVERFLOW
// error, never a wrapped number.
func TestPerformance_Overflow(t *testing.T) {
	terms := perfTerms(t, "0.00", "0.00")
	terms.Flows = []CapitalFlow{{At: date(2026, time.January, 5), Amount: money.MaxUSD(), NAVBefore: money.USDFromMinor(1)}}
	_, err := ComputePerformanceFee(terms, usd(t, "1.00"), nil)
	require.Equal(t, errs.CodeOverflow, errs.CodeOf(err), "%v", err)

	huge := perfTerms(t, "0.00", "0.00")
	_, err = ComputePerformanceFee(huge, money.MaxUSD(), nil)
	require.NoError(t, err, "the largest NAV is still representable")
}

// TestPerformance_StatementIsDeterministicAndCanonical pins the statement
// format and shows the hash depends on content only.
func TestPerformance_StatementIsDeterministicAndCanonical(t *testing.T) {
	terms := perfTerms(t, "100.00", "100.00")
	terms.HurdleAnnualBPS = 0
	terms.Flows = []CapitalFlow{flow(t, date(2026, time.January, 15), "50.00", "105.00")}
	st := crystallize(t, terms, "160.00", StrategyContribution{"s1", usd(t, "10.00")})
	b, err := st.Canonical()
	require.NoError(t, err)
	require.Equal(t, `{"kind":"fees.performance_statement.v1","mode":"SANDBOX_CALCULATION","account_ref":"acct-1",`+
		`"terms_version":"perf-terms-v1","period_start":"2026-01-01T00:00:00Z","period_end":"2026-02-01T00:00:00Z",`+
		`"crystallization":"PERIOD_END","opening_nav":"100.00",`+
		`"flows":[{"at":"2026-01-15T00:00:00Z","amount":"50.00","nav_before":"105.00","nav_after":"155.00","hwm_before":"100.00","hwm_after":"150.00"}],`+
		`"net_flows":"50.00","closing_nav_gross":"160.00","gross_pnl":"10.00","hwm_before":"100.00","hwm_adjusted":"150.00",`+
		`"hurdle":{"annual_bps":0,"convention":"ACT/365F","elapsed_ns":"2678400000000000","year_ns":"31536000000000000","threshold":"150.00","rounding":"up"},`+
		`"fee_base":"10.00","fee_rate_bps":2000,"fee_amount":"2.00","closing_nav_net":"158.00","hwm_after":"158.00","prior_accrued_fee":"0.00",`+
		`"lines":[{"seq":1,"kind":"ACCRUAL","at":"2026-02-01T00:00:00Z","nav":"160.00","hwm_adjusted":"150.00","hurdle_threshold":"150.00",`+
		`"fee_base":"10.00","amount":"2.00","accrual_before":"0.00","accrual_after":"2.00"},`+
		`{"seq":2,"kind":"CRYSTALLIZATION","at":"2026-02-01T00:00:00Z","nav":"160.00","hwm_adjusted":"150.00","hurdle_threshold":"150.00",`+
		`"fee_base":"10.00","amount":"2.00","accrual_before":"2.00","accrual_after":"0.00"}],`+
		`"attribution":[{"strategy":"s1","gross_pnl":"10.00","eligible":true,"fee_share":"2.00","remainder_allocated":"0.00"}],`+
		`"rounding":{"hwm_withdrawal":"up","hurdle_threshold":"up","fee":"down","attribution":"largest_remainder"}}`, string(b))

	again := crystallize(t, terms, "160.00", StrategyContribution{"s1", usd(t, "10.00")})
	h1, err := st.Hash()
	require.NoError(t, err)
	h2, err := again.Hash()
	require.NoError(t, err)
	require.Equal(t, h1, h2)

	zoned := terms
	zone := time.FixedZone("UTC-8", -8*3600)
	zoned.PeriodStart, zoned.PeriodEnd = terms.PeriodStart.In(zone), terms.PeriodEnd.In(zone)
	zoned.Flows = []CapitalFlow{terms.Flows[0]}
	zoned.Flows[0].At = terms.Flows[0].At.In(zone)
	h3, err := crystallize(t, zoned, "160.00", StrategyContribution{"s1", usd(t, "10.00")}).Hash()
	require.NoError(t, err)
	require.Equal(t, h1, h3, "the statement is in UTC whatever zone its inputs carry")

	edited := st
	edited.Lines = append([]PerformanceLine(nil), st.Lines...)
	edited.Lines[0].Amount = usd(t, "1.99")
	h4, err := edited.Hash()
	require.NoError(t, err)
	require.NotEqual(t, h1, h4)
	requireFields(t, VerifyPerformanceStatement(terms, edited), errs.CodeValidationFailed, "lines")
}

// TestVerifyPerformanceStatement_ReplaysAgainstTheAgreedTerms: a statement
// computed under other terms, or edited after the fact, does not verify.
func TestVerifyPerformanceStatement_ReplaysAgainstTheAgreedTerms(t *testing.T) {
	terms := perfTerms(t, "100000.00", "100000.00")
	p, err := OpenPerformancePeriod(terms)
	require.NoError(t, err)
	_, err = p.Accrue(NAVSnapshot{At: date(2026, time.January, 10), NAV: usd(t, "105000.00")})
	require.NoError(t, err)
	st, err := p.Crystallize(usd(t, "103000.00"), nil)
	require.NoError(t, err)
	require.NoError(t, VerifyPerformanceStatement(terms, st))

	richer := terms
	richer.FeeRateBPS = 2_500
	requireFields(t, VerifyPerformanceStatement(richer, st), errs.CodeValidationFailed, "fee_amount", "fee_rate_bps")

	lowered := terms
	lowered.PriorHWM = usd(t, "100000.00")
	lowered.OpeningNAV = usd(t, "99000.00")
	requireFields(t, VerifyPerformanceStatement(lowered, st), errs.CodeValidationFailed, "opening_nav")

	forged := st
	forged.Mode = "LIVE"
	requireFields(t, VerifyPerformanceStatement(terms, forged), errs.CodeValidationFailed, "mode")

	truncated := st
	truncated.Lines = st.Lines[:1]
	requireFields(t, VerifyPerformanceStatement(terms, truncated), errs.CodeValidationFailed, "lines")
}
