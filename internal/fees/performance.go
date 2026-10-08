package fees

import (
	"strconv"
	"time"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// Performance fee limits.
const (
	// MaxPerformancePeriod bounds a period so that every elapsed time is an
	// exact time.Duration. It is an arithmetic guard, not a business rule.
	MaxPerformancePeriod = 100 * 366 * 24 * time.Hour
	// MaxCapitalFlows bounds the flows of one period.
	MaxCapitalFlows = 10_000
	// MaxStrategyContributions bounds the attribution of one period.
	MaxStrategyContributions = 1_000
)

// HurdleYear is the year of the Actual/365 Fixed convention the hurdle is
// prorated by: elapsed time over exactly 365 days, whatever the calendar.
const HurdleYear = 365 * 24 * time.Hour

// CrystallizationFrequency says when an accrued performance fee becomes
// final.
type CrystallizationFrequency string

// CrystallizeAtPeriodEnd crystallizes once, at the end of each period; a
// period's accruals before then are provisional and may be reversed. It is
// the only frequency this package implements.
const CrystallizeAtPeriodEnd CrystallizationFrequency = "PERIOD_END"

// CapitalFlow is a deposit (positive Amount) or a withdrawal (negative
// Amount) into a managed account. NAVBefore is the account's gross NAV
// immediately before the flow, struck by the caller's valuation at At; a
// withdrawal needs it to reduce the high-water mark pro rata. Simultaneous
// flows must be netted into one by the caller.
type CapitalFlow struct {
	At        time.Time
	Amount    money.USD
	NAVBefore money.USD
}

// PerformanceTerms is one performance period of a managed account or
// strategy: the terms in force and the state carried in from the previous
// period. All NAVs are GROSS of performance fees.
//
// PriorHWM must be at least OpeningNAV: a crystallization always leaves the
// high-water mark at or above the net NAV that opens the next period, and an
// account's first period starts with its high-water mark at its opening NAV.
// A high-water mark below the opening NAV would charge, in this period,
// performance earned before it began.
//
// PriorAccruedFee is a fee accrued but not crystallized by earlier
// sub-periods of this period (for example before a restart); accruals
// continue from it and the period-end true-up reconciles it.
type PerformanceTerms struct {
	AccountRef      string
	TermsVersion    string
	PeriodStart     time.Time
	PeriodEnd       time.Time
	OpeningNAV      money.USD
	PriorHWM        money.USD
	HurdleAnnualBPS money.BPS
	FeeRateBPS      money.BPS
	Crystallization CrystallizationFrequency
	PriorAccruedFee money.USD
	// Flows is in strictly ascending time order, each strictly after
	// PeriodStart and at or before PeriodEnd.
	Flows []CapitalFlow
}

// Validate checks the terms. It returns nil or a VALIDATION_FAILED
// *errs.Error whose Fields name every offending field.
func (t PerformanceTerms) Validate() error {
	f := fieldErrors{}
	checkLabel(f, "account_ref", t.AccountRef)
	checkLabel(f, "terms_version", t.TermsVersion)
	switch {
	case t.PeriodStart.IsZero():
		f.add("period_start", "required")
	case t.PeriodEnd.IsZero():
		f.add("period_end", "required")
	case !t.PeriodEnd.After(t.PeriodStart):
		f.add("period_end", "must be after period_start")
	case t.PeriodEnd.Sub(t.PeriodStart) > MaxPerformancePeriod:
		f.add("period_end", "the period is longer than MaxPerformancePeriod")
	}
	if t.OpeningNAV.IsNegative() {
		f.add("opening_nav", "must not be negative")
	}
	switch {
	case t.PriorHWM.IsNegative():
		f.add("prior_hwm", "must not be negative")
	case t.PriorHWM.Cmp(t.OpeningNAV) < 0:
		f.add("prior_hwm", "must be at least opening_nav; performance earned before the period cannot be charged in it")
	}
	checkBPS(f, "hurdle_annual_bps", t.HurdleAnnualBPS, 0, money.OneHundredPercent)
	checkBPS(f, "fee_rate_bps", t.FeeRateBPS, 0, money.OneHundredPercent)
	if t.Crystallization != CrystallizeAtPeriodEnd {
		f.addf("crystallization", "must be %s", CrystallizeAtPeriodEnd)
	}
	if t.PriorAccruedFee.IsNegative() {
		f.add("prior_accrued_fee", "must not be negative")
	}
	t.validateFlows(f)
	return f.err("fees: invalid performance terms")
}

func (t PerformanceTerms) validateFlows(f fieldErrors) {
	if len(t.Flows) > MaxCapitalFlows {
		f.addf("flows", "at most %d flows", MaxCapitalFlows)
		return
	}
	for i, fl := range t.Flows {
		key := "flows[" + strconv.Itoa(i) + "]"
		switch {
		case fl.At.IsZero():
			f.add(key+".at", "required")
		case !fl.At.After(t.PeriodStart) || fl.At.After(t.PeriodEnd):
			f.add(key+".at", "must be after period_start and at or before period_end")
		case i > 0 && !fl.At.After(t.Flows[i-1].At):
			f.add(key+".at", "flows must be in strictly ascending time order; net simultaneous flows into one")
		}
		switch {
		case fl.Amount.IsZero():
			f.add(key+".amount", "must not be zero")
		case fl.NAVBefore.IsNegative():
			f.add(key+".nav_before", "must not be negative")
		case fl.Amount.IsNegative() && fl.Amount.Minor() == money.MinUSDMinor:
			f.add(key+".amount", "outside the representable range")
		case fl.Amount.IsNegative() && -fl.Amount.Minor() > fl.NAVBefore.Minor():
			f.add(key+".amount", "a withdrawal cannot exceed the NAV before it")
		}
	}
}

// NAVSnapshot is the gross NAV (before any performance fee) observed at At.
type NAVSnapshot struct {
	At  time.Time
	NAV money.USD
}

// valuation is the fee as if the period ended at `at` with gross NAV `nav`.
type valuation struct {
	at          time.Time
	nav         money.USD
	flows       []FlowRecord
	netFlows    money.USD
	hwmAdjusted money.USD
	elapsed     time.Duration
	threshold   money.USD
	grossPnL    money.USD
	feeBase     money.USD
	fee         money.USD
}

// value computes the fee at `at` from the terms, applying every flow at or
// before `at`. The rules, in order:
//
//  1. Flows adjust the high-water mark: a deposit raises it by the deposit;
//     a withdrawal scales it by NAVafter / NAVbefore (the fraction of NAV
//     that remains), rounded up. Flows therefore never create or destroy
//     performance: a deposit is matched by an equal rise in the mark, and
//     after a withdrawal the remaining capital is as far from the mark, in
//     relative terms, as before (to the cent, rounded in the investor's
//     favour).
//  2. Gross PnL = NAV − opening NAV − net flows.
//  3. Hurdle threshold = HWM_adjusted × (1 + annual hurdle × elapsed / 365
//     days), elapsed measured exactly from the period start (Actual/365
//     Fixed), rounded up.
//  4. Fee base = min(max(0, NAV − max(HWM_adjusted, threshold)),
//     max(0, gross PnL)): only gains above both the mark and the hurdle,
//     and never more than the period actually earned.
//  5. Fee = fee base × rate, rounded down.
//
// Every rounding is in the investor's favour.
func (t PerformanceTerms) value(at time.Time, nav money.USD) (valuation, error) {
	v := valuation{at: at, nav: nav, hwmAdjusted: t.PriorHWM, flows: []FlowRecord{}}
	for _, fl := range t.Flows {
		if fl.At.After(at) {
			break
		}
		rec, err := applyFlow(fl, v.hwmAdjusted)
		if err != nil {
			return valuation{}, err
		}
		if v.netFlows, err = v.netFlows.Add(fl.Amount); err != nil {
			return valuation{}, arithmeticError(err)
		}
		v.hwmAdjusted = rec.HWMAfter
		v.flows = append(v.flows, rec)
	}
	v.elapsed = at.Sub(t.PeriodStart)
	var err error
	if v.threshold, err = hurdleThreshold(v.hwmAdjusted, t.HurdleAnnualBPS, v.elapsed); err != nil {
		return valuation{}, err
	}
	gross, err := nav.Sub(t.OpeningNAV)
	if err == nil {
		gross, err = gross.Sub(v.netFlows)
	}
	if err != nil {
		return valuation{}, arithmeticError(err)
	}
	v.grossPnL = gross
	bar := v.hwmAdjusted
	if v.threshold.Cmp(bar) > 0 {
		bar = v.threshold
	}
	above, err := nav.Sub(bar)
	if err != nil {
		return valuation{}, arithmeticError(err)
	}
	v.feeBase = minUSD(maxUSD(above, money.USD{}), maxUSD(gross, money.USD{}))
	if v.fee, err = v.feeBase.MulBPS(t.FeeRateBPS, money.RoundDown); err != nil {
		return valuation{}, arithmeticError(err)
	}
	return v, nil
}

// applyFlow applies one flow to the high-water mark.
func applyFlow(fl CapitalFlow, hwm money.USD) (FlowRecord, error) {
	rec := FlowRecord{At: fl.At.UTC(), Amount: fl.Amount, NAVBefore: fl.NAVBefore, HWMBefore: hwm}
	var err error
	if rec.NAVAfter, err = fl.NAVBefore.Add(fl.Amount); err != nil {
		return FlowRecord{}, arithmeticError(err)
	}
	if fl.Amount.IsPositive() {
		rec.HWMAfter, err = hwm.Add(fl.Amount)
	} else {
		rec.HWMAfter, err = hwm.MulRatio(rec.NAVAfter.Minor(), fl.NAVBefore.Minor(), money.RoundUp)
	}
	if err != nil {
		return FlowRecord{}, arithmeticError(err)
	}
	return rec, nil
}

// hurdleThreshold returns hwm × (1 + annual × elapsed / HurdleYear), rounded
// up, computed as hwm × (10_000·Y + annual·elapsed) / (10_000·Y) with Y and
// elapsed in nanoseconds.
func hurdleThreshold(hwm money.USD, annual money.BPS, elapsed time.Duration) (money.USD, error) {
	den := money.QuantityFromInt64(int64(HurdleYear)).Mul(money.QuantityFromInt64(int64(money.OneHundredPercent)))
	num := den.Add(money.QuantityFromInt64(int64(annual)).Mul(money.QuantityFromInt64(int64(elapsed))))
	return usdMulDiv(hwm, num, den, money.RoundUp)
}

func maxUSD(a, b money.USD) money.USD {
	if a.Cmp(b) >= 0 {
		return a
	}
	return b
}

func minUSD(a, b money.USD) money.USD {
	if a.Cmp(b) <= 0 {
		return a
	}
	return b
}

// StrategyContribution is one strategy's gross PnL for the period. When an
// account's gross PnL comes from several strategies, the contributions must
// sum exactly to the period's gross PnL.
type StrategyContribution struct {
	Strategy string
	GrossPnL money.USD
}

// PerformancePeriod accrues and crystallizes the performance fee of one
// period. It is a calculation only: it never charges, posts or moves
// anything, and its statement is a sandbox record (see PerformanceStatement).
//
// Accrue may be called any number of times, at strictly increasing times
// before the period end, each time with the gross NAV then; every call
// appends exactly one line and never alters an earlier one. A fall in NAV
// therefore shows up as a new compensating REVERSAL line, never as an edit.
// Crystallize closes the period. A PerformancePeriod is not safe for
// concurrent use.
type PerformancePeriod struct {
	terms   PerformanceTerms
	lines   []PerformanceLine
	accrued money.USD
	lastAt  time.Time
	closed  bool
}

// OpenPerformancePeriod validates terms and starts a period whose running
// accrual is terms.PriorAccruedFee.
func OpenPerformancePeriod(terms PerformanceTerms) (*PerformancePeriod, error) {
	if err := terms.Validate(); err != nil {
		return nil, err
	}
	t := terms
	t.Flows = append([]CapitalFlow(nil), terms.Flows...)
	return &PerformancePeriod{terms: t, accrued: t.PriorAccruedFee, lastAt: t.PeriodStart}, nil
}

// Accrued returns the running accrual.
func (p *PerformancePeriod) Accrued() money.USD { return p.accrued }

// Lines returns a copy of the lines appended so far.
func (p *PerformancePeriod) Lines() []PerformanceLine {
	return append([]PerformanceLine(nil), p.lines...)
}

// Accrue values the period at s as if it ended then (hurdle prorated to
// s.At, flows up to and including s.At applied) and appends the change in
// the running accrual as one line: ACCRUAL when it rises, REVERSAL when it
// falls, NO_CHANGE otherwise.
func (p *PerformancePeriod) Accrue(s NAVSnapshot) (PerformanceLine, error) {
	if p.closed {
		return PerformanceLine{}, errs.New(errs.CodeInvalidStateTransition, "fees: the performance period is already crystallized")
	}
	f := fieldErrors{}
	switch {
	case s.At.IsZero():
		f.add("at", "required")
	case !s.At.After(p.lastAt):
		f.add("at", "must be after the period start and every earlier accrual")
	case !s.At.Before(p.terms.PeriodEnd):
		f.add("at", "must be before period_end; the period end is valued by Crystallize")
	}
	if s.NAV.IsNegative() {
		f.add("nav", "must not be negative")
	}
	if err := f.err("fees: invalid NAV snapshot"); err != nil {
		return PerformanceLine{}, err
	}
	v, err := p.terms.value(s.At, s.NAV)
	if err != nil {
		return PerformanceLine{}, err
	}
	line, err := p.deltaLine(v)
	if err != nil {
		return PerformanceLine{}, err
	}
	p.lines = append(p.lines, line)
	p.accrued = v.fee
	p.lastAt = s.At
	return line, nil
}

// deltaLine builds the line that moves the running accrual to v.fee.
func (p *PerformancePeriod) deltaLine(v valuation) (PerformanceLine, error) {
	delta, err := v.fee.Sub(p.accrued)
	if err != nil {
		return PerformanceLine{}, arithmeticError(err)
	}
	kind := LineNoChange
	switch delta.Sign() {
	case 1:
		kind = LineAccrual
	case -1:
		kind = LineReversal
	}
	return PerformanceLine{
		Seq:             len(p.lines) + 1,
		Kind:            kind,
		At:              v.at.UTC(),
		NAV:             v.nav,
		HWMAdjusted:     v.hwmAdjusted,
		HurdleThreshold: v.threshold,
		FeeBase:         v.feeBase,
		Amount:          delta,
		AccrualBefore:   p.accrued,
		AccrualAfter:    v.fee,
	}, nil
}

// Crystallize values the period at its end with the gross closing NAV,
// appends the period-end true-up line and the CRYSTALLIZATION line, attributes
// the fee to contributing strategies, and returns the statement. The new
// high-water mark is max(HWM_adjusted, net NAV). On error nothing changes and
// the call may be retried.
func (p *PerformancePeriod) Crystallize(closingNAV money.USD, contributions []StrategyContribution) (PerformanceStatement, error) {
	if p.closed {
		return PerformanceStatement{}, errs.New(errs.CodeInvalidStateTransition, "fees: the performance period is already crystallized")
	}
	if closingNAV.IsNegative() {
		return PerformanceStatement{}, errs.New(errs.CodeValidationFailed, "fees: invalid closing NAV").
			WithField("closing_nav", "must not be negative")
	}
	t := p.terms
	v, err := t.value(t.PeriodEnd, closingNAV)
	if err != nil {
		return PerformanceStatement{}, err
	}
	attribution, err := attribute(v.fee, v.grossPnL, contributions)
	if err != nil {
		return PerformanceStatement{}, err
	}
	trueUp, err := p.deltaLine(v)
	if err != nil {
		return PerformanceStatement{}, err
	}
	net, err := closingNAV.Sub(v.fee)
	if err != nil {
		return PerformanceStatement{}, arithmeticError(err)
	}
	crystallized := PerformanceLine{
		Seq:             trueUp.Seq + 1,
		Kind:            LineCrystallization,
		At:              v.at.UTC(),
		NAV:             v.nav,
		HWMAdjusted:     v.hwmAdjusted,
		HurdleThreshold: v.threshold,
		FeeBase:         v.feeBase,
		Amount:          v.fee,
		AccrualBefore:   v.fee,
		AccrualAfter:    money.USD{},
	}
	p.lines = append(p.lines, trueUp, crystallized)
	p.accrued = money.USD{}
	p.closed = true
	return PerformanceStatement{
		Kind:            PerformanceStatementKind,
		Mode:            PerformanceModeSandbox,
		AccountRef:      t.AccountRef,
		TermsVersion:    t.TermsVersion,
		PeriodStart:     t.PeriodStart.UTC(),
		PeriodEnd:       t.PeriodEnd.UTC(),
		Crystallization: t.Crystallization,
		OpeningNAV:      t.OpeningNAV,
		Flows:           v.flows,
		NetFlows:        v.netFlows,
		ClosingNAVGross: closingNAV,
		GrossPnL:        v.grossPnL,
		HWMBefore:       t.PriorHWM,
		HWMAdjusted:     v.hwmAdjusted,
		Hurdle: HurdleRecord{
			AnnualBPS:    t.HurdleAnnualBPS,
			Convention:   HurdleConvention,
			ElapsedNanos: formatNanos(v.elapsed),
			YearNanos:    formatNanos(HurdleYear),
			Threshold:    v.threshold,
			Rounding:     money.RoundUp.String(),
		},
		FeeBase:         v.feeBase,
		FeeRateBPS:      t.FeeRateBPS,
		FeeAmount:       v.fee,
		ClosingNAVNet:   net,
		HWMAfter:        maxUSD(v.hwmAdjusted, net),
		PriorAccruedFee: t.PriorAccruedFee,
		Lines:           p.Lines(),
		Attribution:     attribution,
		Rounding:        performanceRounding(),
	}, nil
}

// ComputePerformanceFee is OpenPerformancePeriod followed by Crystallize,
// for a period without intermediate accruals.
func ComputePerformanceFee(terms PerformanceTerms, closingNAV money.USD, contributions []StrategyContribution) (PerformanceStatement, error) {
	p, err := OpenPerformancePeriod(terms)
	if err != nil {
		return PerformanceStatement{}, err
	}
	return p.Crystallize(closingNAV, contributions)
}
