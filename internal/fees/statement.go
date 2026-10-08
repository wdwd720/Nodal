package fees

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"sort"
	"strconv"
	"time"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// PerformanceStatementKind is the domain-separation tag and format version of
// a performance statement's canonical form.
const PerformanceStatementKind = "fees.performance_statement.v1"

// PerformanceModeSandbox is the only mode a statement is produced in. A
// statement is an accounting calculation: nothing in this package charges
// it, posts it to a ledger or moves money, and performance fees are excluded
// from live operation (REQUIREMENTS_TRACEABILITY R-004-5). The mode is in the
// canonical form so that no copy of a statement can be mistaken for a charge.
const PerformanceModeSandbox = "SANDBOX_CALCULATION"

// HurdleConvention names the day-count convention of the hurdle proration.
const HurdleConvention = "ACT/365F"

// PerformanceLineKind classifies a statement line.
type PerformanceLineKind string

// Line kinds.
const (
	// LineAccrual raises the running accrual.
	LineAccrual PerformanceLineKind = "ACCRUAL"
	// LineReversal lowers it: a compensating line written when NAV fell
	// after an earlier accrual. Earlier lines are never edited.
	LineReversal PerformanceLineKind = "REVERSAL"
	// LineNoChange records a valuation that left the accrual unchanged.
	LineNoChange PerformanceLineKind = "NO_CHANGE"
	// LineCrystallization finalizes the accrual as the period's fee and
	// resets the running accrual to zero.
	LineCrystallization PerformanceLineKind = "CRYSTALLIZATION"
)

// PerformanceLine is one append-only statement line. Amount is the signed
// change to the running accrual (positive for ACCRUAL, negative for
// REVERSAL, zero for NO_CHANGE); for CRYSTALLIZATION it is the fee
// crystallized. Every line records the valuation that produced it.
//
// Across a statement: PriorAccruedFee + Σ Amount over the ACCRUAL, REVERSAL
// and NO_CHANGE lines == FeeAmount == the CRYSTALLIZATION line's Amount.
type PerformanceLine struct {
	Seq             int                 `json:"seq"`
	Kind            PerformanceLineKind `json:"kind"`
	At              time.Time           `json:"at"`
	NAV             money.USD           `json:"nav"`
	HWMAdjusted     money.USD           `json:"hwm_adjusted"`
	HurdleThreshold money.USD           `json:"hurdle_threshold"`
	FeeBase         money.USD           `json:"fee_base"`
	Amount          money.USD           `json:"amount"`
	AccrualBefore   money.USD           `json:"accrual_before"`
	AccrualAfter    money.USD           `json:"accrual_after"`
}

// FlowRecord is one capital flow and its effect on the high-water mark.
type FlowRecord struct {
	At        time.Time `json:"at"`
	Amount    money.USD `json:"amount"`
	NAVBefore money.USD `json:"nav_before"`
	NAVAfter  money.USD `json:"nav_after"`
	HWMBefore money.USD `json:"hwm_before"`
	HWMAfter  money.USD `json:"hwm_after"`
}

// HurdleRecord is the hurdle as applied at the period end: Threshold =
// HWM_adjusted × (1 + AnnualBPS/10_000 × ElapsedNanos / YearNanos), rounded
// up. The durations are decimal strings of nanoseconds because they exceed
// the integers a JSON number carries exactly.
type HurdleRecord struct {
	AnnualBPS    money.BPS `json:"annual_bps"`
	Convention   string    `json:"convention"`
	ElapsedNanos string    `json:"elapsed_ns"`
	YearNanos    string    `json:"year_ns"`
	Threshold    money.USD `json:"threshold"`
	Rounding     string    `json:"rounding"`
}

// StrategyFeeShare is one strategy's part of the fee. Only strategies with a
// positive gross PnL (Eligible) bear the fee, pro rata to their gains;
// RemainderAllocated is the cent this strategy received when the pro-rata
// shares did not divide exactly (zero or 0.01).
type StrategyFeeShare struct {
	Strategy           string    `json:"strategy"`
	GrossPnL           money.USD `json:"gross_pnl"`
	Eligible           bool      `json:"eligible"`
	FeeShare           money.USD `json:"fee_share"`
	RemainderAllocated money.USD `json:"remainder_allocated"`
}

// PerformanceRounding states the rounding rules of a statement.
type PerformanceRounding struct {
	HWMWithdrawal   string `json:"hwm_withdrawal"`
	HurdleThreshold string `json:"hurdle_threshold"`
	Fee             string `json:"fee"`
	Attribution     string `json:"attribution"`
}

// AttributionLargestRemainder names the attribution rounding: each eligible
// strategy's exact share is rounded down to the cent, and the remaining
// cents go one each to the strategies with the largest discarded fractions,
// ties broken by strategy reference in ascending byte order.
const AttributionLargestRemainder = "largest_remainder"

func performanceRounding() PerformanceRounding {
	return PerformanceRounding{
		HWMWithdrawal:   money.RoundUp.String(),
		HurdleThreshold: money.RoundUp.String(),
		Fee:             money.RoundDown.String(),
		Attribution:     AttributionLargestRemainder,
	}
}

// PerformanceStatement is the deterministic accounting statement of one
// crystallized performance period. Its canonical form is the JSON encoding
// of this struct (fields in declaration order, no maps, UTC times, amounts as
// strings); Hash is the sha256 of it.
//
// The identities it satisfies:
//
//	GrossPnL      = ClosingNAVGross − OpeningNAV − NetFlows
//	FeeAmount     = FeeBase × FeeRateBPS / 10_000, rounded down
//	ClosingNAVNet = ClosingNAVGross − FeeAmount
//	HWMAfter      = max(HWMAdjusted, ClosingNAVNet)
//	Σ Attribution.FeeShare = FeeAmount (when attribution is given)
type PerformanceStatement struct {
	Kind            string                   `json:"kind"`
	Mode            string                   `json:"mode"`
	AccountRef      string                   `json:"account_ref"`
	TermsVersion    string                   `json:"terms_version"`
	PeriodStart     time.Time                `json:"period_start"`
	PeriodEnd       time.Time                `json:"period_end"`
	Crystallization CrystallizationFrequency `json:"crystallization"`
	OpeningNAV      money.USD                `json:"opening_nav"`
	Flows           []FlowRecord             `json:"flows"`
	NetFlows        money.USD                `json:"net_flows"`
	ClosingNAVGross money.USD                `json:"closing_nav_gross"`
	GrossPnL        money.USD                `json:"gross_pnl"`
	HWMBefore       money.USD                `json:"hwm_before"`
	HWMAdjusted     money.USD                `json:"hwm_adjusted"`
	Hurdle          HurdleRecord             `json:"hurdle"`
	FeeBase         money.USD                `json:"fee_base"`
	FeeRateBPS      money.BPS                `json:"fee_rate_bps"`
	FeeAmount       money.USD                `json:"fee_amount"`
	ClosingNAVNet   money.USD                `json:"closing_nav_net"`
	HWMAfter        money.USD                `json:"hwm_after"`
	PriorAccruedFee money.USD                `json:"prior_accrued_fee"`
	Lines           []PerformanceLine        `json:"lines"`
	Attribution     []StrategyFeeShare       `json:"attribution"`
	Rounding        PerformanceRounding      `json:"rounding"`
}

// Canonical returns the deterministic serialised form of the statement.
func (s PerformanceStatement) Canonical() ([]byte, error) {
	c := s
	c.PeriodStart, c.PeriodEnd = s.PeriodStart.UTC(), s.PeriodEnd.UTC()
	c.Flows = make([]FlowRecord, len(s.Flows))
	for i, fl := range s.Flows {
		fl.At = fl.At.UTC()
		c.Flows[i] = fl
	}
	c.Lines = make([]PerformanceLine, len(s.Lines))
	for i, l := range s.Lines {
		l.At = l.At.UTC()
		c.Lines[i] = l
	}
	c.Attribution = append(make([]StrategyFeeShare, 0, len(s.Attribution)), s.Attribution...)
	b, err := json.Marshal(c)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "fees: canonicalise performance statement")
	}
	return b, nil
}

// Hash is the hex sha256 of the canonical form.
func (s PerformanceStatement) Hash() (string, error) {
	b, err := s.Canonical()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// VerifyPerformanceStatement replays a statement. The caller supplies the
// agreed terms and the period's flows from its own sources; the valuations
// are the ones the statement records (the time and NAV of every accrual
// line, the gross closing NAV and the strategy contributions). The period is
// recomputed from those and the statement must be byte-for-byte the result,
// which proves every accrual, reversal, fee and attribution line follows from
// the terms and the recorded valuations. It returns nil or a VALIDATION_FAILED
// *errs.Error naming every differing section.
//
// It cannot prove that the recorded NAVs were true; that is the valuation's
// evidence, not this package's.
func VerifyPerformanceStatement(terms PerformanceTerms, st PerformanceStatement) error {
	n := len(st.Lines)
	if n < 2 || st.Lines[n-1].Kind != LineCrystallization {
		return errs.New(errs.CodeValidationFailed, "fees: performance statement does not verify").
			WithField("lines", "a statement ends with its period-end true-up and a CRYSTALLIZATION line")
	}
	p, err := OpenPerformancePeriod(terms)
	if err != nil {
		return err
	}
	for _, l := range st.Lines[:n-2] {
		if _, err := p.Accrue(NAVSnapshot{At: l.At, NAV: l.NAV}); err != nil {
			return err
		}
	}
	contributions := make([]StrategyContribution, 0, len(st.Attribution))
	for _, a := range st.Attribution {
		contributions = append(contributions, StrategyContribution{Strategy: a.Strategy, GrossPnL: a.GrossPnL})
	}
	want, err := p.Crystallize(st.ClosingNAVGross, contributions)
	if err != nil {
		return err
	}
	wb, err := want.Canonical()
	if err != nil {
		return err
	}
	gb, err := st.Canonical()
	if err != nil {
		return err
	}
	diff, err := diffCanonical(wb, gb)
	if err != nil {
		return err
	}
	f := fieldErrors{}
	for _, k := range diff {
		f.add(k, "differs from the period recomputed from the terms and the recorded valuations")
	}
	return f.err("fees: performance statement does not verify")
}

// NextTerms returns the terms of the period that follows s and ends at end:
// it opens at s's net NAV with s's new high-water mark, under the same
// account, terms version, rates and crystallization, with no accrual carried
// in (crystallization reset it).
func (s PerformanceStatement) NextTerms(end time.Time, flows []CapitalFlow) PerformanceTerms {
	return PerformanceTerms{
		AccountRef:      s.AccountRef,
		TermsVersion:    s.TermsVersion,
		PeriodStart:     s.PeriodEnd,
		PeriodEnd:       end,
		OpeningNAV:      s.ClosingNAVNet,
		PriorHWM:        s.HWMAfter,
		HurdleAnnualBPS: s.Hurdle.AnnualBPS,
		FeeRateBPS:      s.FeeRateBPS,
		Crystallization: s.Crystallization,
		Flows:           append([]CapitalFlow(nil), flows...),
	}
}

// attribute splits fee across the contributing strategies pro rata to their
// positive gross PnL, exactly, with the largest-remainder rule. The
// contributions must sum to the period's gross PnL. The result is sorted by
// strategy reference.
func attribute(fee, gross money.USD, contributions []StrategyContribution) ([]StrategyFeeShare, error) {
	shares := make([]StrategyFeeShare, 0, len(contributions))
	if len(contributions) == 0 {
		return shares, nil
	}
	if err := validateContributions(gross, contributions); err != nil {
		return nil, err
	}
	sorted := append([]StrategyContribution(nil), contributions...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Strategy < sorted[j].Strategy })

	positive := new(big.Int)
	for _, c := range sorted {
		if c.GrossPnL.IsPositive() {
			positive.Add(positive, big.NewInt(c.GrossPnL.Minor()))
		}
	}
	// A fee needs a positive gross PnL, and contributions summing to a
	// positive total include a positive one; this cannot happen.
	if fee.IsPositive() && positive.Sign() == 0 {
		return nil, errs.New(errs.CodeInternal, "fees: a positive fee with no positive contributor")
	}
	feeMinor := big.NewInt(fee.Minor())
	remainders := make([]*big.Int, len(sorted))
	allocated := new(big.Int)
	for i, c := range sorted {
		share := StrategyFeeShare{Strategy: c.Strategy, GrossPnL: c.GrossPnL, Eligible: c.GrossPnL.IsPositive()}
		if share.Eligible && fee.IsPositive() {
			q, r := new(big.Int).QuoRem(new(big.Int).Mul(feeMinor, big.NewInt(c.GrossPnL.Minor())), positive, new(big.Int))
			share.FeeShare = money.USDFromMinor(q.Int64()) // q ≤ fee, so it fits
			remainders[i] = r
			allocated.Add(allocated, q)
		}
		shares = append(shares, share)
	}
	leftover := new(big.Int).Sub(feeMinor, allocated).Int64() // < number of eligible strategies
	order := make([]int, 0, len(sorted))
	for i := range sorted {
		if remainders[i] != nil {
			order = append(order, i)
		}
	}
	sort.SliceStable(order, func(a, b int) bool {
		if c := remainders[order[a]].Cmp(remainders[order[b]]); c != 0 {
			return c > 0
		}
		return sorted[order[a]].Strategy < sorted[order[b]].Strategy
	})
	cent := money.USDFromMinor(1)
	for k := int64(0); k < leftover; k++ {
		i := order[k]
		share, err := shares[i].FeeShare.Add(cent)
		if err != nil {
			return nil, arithmeticError(err)
		}
		shares[i].FeeShare = share
		shares[i].RemainderAllocated = cent
	}
	return shares, nil
}

// validateContributions checks references and that the contributions sum to
// the period's gross PnL.
func validateContributions(gross money.USD, contributions []StrategyContribution) error {
	f := fieldErrors{}
	if len(contributions) > MaxStrategyContributions {
		f.addf("contributions", "at most %d strategies", MaxStrategyContributions)
		return f.err("fees: invalid strategy attribution")
	}
	seen := make(map[string]bool, len(contributions))
	sum := new(big.Int)
	for i, c := range contributions {
		key := "contributions[" + strconv.Itoa(i) + "].strategy"
		checkLabel(f, key, c.Strategy)
		if seen[c.Strategy] {
			f.add(key, "duplicate strategy")
		}
		seen[c.Strategy] = true
		sum.Add(sum, big.NewInt(c.GrossPnL.Minor()))
	}
	if sum.Cmp(big.NewInt(gross.Minor())) != 0 {
		f.addf("contributions", "must sum to the period's gross PnL %s", gross)
	}
	return f.err("fees: invalid strategy attribution")
}

// formatNanos renders a duration as a decimal string of nanoseconds.
func formatNanos(d time.Duration) string { return strconv.FormatInt(int64(d), 10) }
