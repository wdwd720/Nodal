package derivatives

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

// RunRequest names one interval of one instrument. IntervalStart must lie on
// the interval grid (a whole number of intervals since the Unix epoch).
type RunRequest struct {
	InstrumentID  instruments.InstrumentID
	IntervalStart time.Time
	CorrelationID string
}

// RunResult is what a variation-margin or funding run did.
type RunResult struct {
	// ID is the run's claim row (variation_margin_runs or funding_rates).
	ID string
	// Replay is true when the interval had already been run: nothing was
	// written.
	Replay        bool
	IntervalStart time.Time
	IntervalEnd   time.Time
	MarkID        MarkID
	Positions     int
	// Paid is Σ collected from payers, Received Σ paid to receivers,
	// Shortfall Σ what payers could not pay.
	Paid      money.Quantity
	Received  money.Quantity
	Shortfall money.Quantity
	// Funding only: the premium index and the (clamped) rate in units of
	// 10^-8, and what one contract paid or received.
	PremiumIndexE8 int64
	RateE8         int64
	Clamped        bool
	PerContract    money.Quantity
	Journals       []ledger.TransactionID
}

// interval validates an interval start against a length and returns its end.
func interval(start time.Time, length time.Duration, now time.Time) (time.Time, error) {
	ms := length.Milliseconds()
	if ms <= 0 {
		return time.Time{}, errs.New(errs.CodeValidationFailed, "derivatives: interval length must be positive")
	}
	start = start.UTC()
	if start.UnixMilli()%ms != 0 || start.Nanosecond()%int(time.Millisecond) != 0 {
		return time.Time{}, errs.New(errs.CodeValidationFailed, "derivatives: interval start is not on the interval grid").
			WithField("interval_start", start).WithField("interval_ms", ms)
	}
	end := start.Add(length)
	if now.Before(end) {
		return time.Time{}, errs.New(errs.CodeValidationFailed, "derivatives: an interval is settled only after it ends").
			WithField("interval_end", end).WithField("now", now)
	}
	return end, nil
}

// obligationLeg plans the cash leg of a run for one position: received when
// amount > 0, collected (wallet, then the position's margin, then shortfall)
// when amount < 0. It returns the leg, the payment and the position after it.
func (s *Service) obligationLeg(ctx context.Context, tx pgx.Tx, ch econ.Facility, spec Spec, pos Position, amount money.Quantity,
	kind CashFlowKind, journal ledger.Kind, effectKind, key string, ref ledger.FinancialEventReference) (leg, payment, Position, error) {
	acct := pos.AccountID
	asset := spec.SettlementAssetID
	after := pos
	var pay payment
	flowAmount := amount
	if amount.IsNegative() {
		wallet, err := s.ledger.Balance(ctx, tx, ledger.CustomerAccount(acct, ledger.CodeWallet, asset))
		if err != nil {
			return leg{}, payment{}, Position{}, err
		}
		margin := money.Quantity{}
		if pos.Status == PositionOpen {
			margin = pos.MarginPosted
		}
		pay = splitPayment(amount.Neg(), wallet, margin)
		after.MarginPosted = pos.MarginPosted.Sub(pay.FromMargin)
		after.Shortfall = pos.Shortfall.Add(pay.Shortfall)
		flowAmount = pay.collected().Neg()
	}
	return leg{
		name: string(kind), key: key, effectKind: effectKind, kind: journal, account: &acct, reference: ref,
		entries: cashEntries(acct, ch, asset, amount, pay),
		flow:    s.flowFor(pos, spec, kind, flowAmount, pay.Shortfall, key, ref),
	}, pay, after, nil
}

// runEvidence is the archived document of a run.
type runEvidence struct {
	Kind          string      `json:"kind"`
	RunID         string      `json:"run_id"`
	InstrumentID  string      `json:"instrument_id"`
	IntervalStart time.Time   `json:"interval_start"`
	IntervalEnd   time.Time   `json:"interval_end"`
	MarkID        string      `json:"mark_id"`
	Mark          string      `json:"mark_mantissa"`
	Premium       int64       `json:"premium_index_e8"`
	Rate          int64       `json:"rate_e8"`
	Clamped       bool        `json:"clamped"`
	PerContract   string      `json:"per_contract"`
	Samples       int         `json:"samples"`
	PolicyVersion string      `json:"policy_version"`
	PolicyHash    string      `json:"policy_hash"`
	Legs          []postedLeg `json:"legs"`
}

// RunVariationMargin settles a future's or perpetual's open positions once
// per variation-margin interval: each position receives or pays
// Q × V(mark) − settled_value through the clearing house (VARIATION_MARGIN),
// and its baseline moves to the mark. The mark is the latest one fresh at the
// time of the run, applied to the positions as they stand at that time -- never
// an older mark against contracts traded after it, which would settle a
// difference that does not exist. The interval is the run's identity: it is
// run only after it ends, at most once (the claim row is UNIQUE; a replay
// posts nothing and concurrent runs settle once) and never after a later
// interval has run. Payers are collected before receivers are paid. A future
// past expiry is settled by SettleExpiry, not here.
func (s *Service) RunVariationMargin(ctx context.Context, tx pgx.Tx, r RunRequest) (RunResult, error) {
	if err := s.serializeClearing(ctx, tx); err != nil {
		return RunResult{}, err
	}
	spec, err := loadSpec(ctx, tx, s.instruments, r.InstrumentID)
	if err != nil {
		return RunResult{}, err
	}
	if !spec.IsLinear() {
		return RunResult{}, errs.New(errs.CodeValidationFailed, "derivatives: variation margin settles futures and perpetuals")
	}
	now := s.clk.Now().UTC()
	end, err := interval(r.IntervalStart, time.Duration(s.policy.VariationMarginIntervalMS)*time.Millisecond, now)
	if err != nil {
		return RunResult{}, err
	}
	start := r.IntervalStart.UTC()
	var existing VMRunID
	var n int
	err = tx.QueryRow(ctx, `SELECT id, positions FROM variation_margin_runs WHERE instrument_id = $1 AND interval_start = $2`,
		r.InstrumentID, start).Scan(&existing, &n)
	switch {
	case err == nil:
		return RunResult{ID: existing.String(), Replay: true, IntervalStart: start, IntervalEnd: end, Positions: n}, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return RunResult{}, dbErr("read variation margin run", err)
	}
	var later bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM variation_margin_runs WHERE instrument_id = $1 AND interval_start > $2)`,
		r.InstrumentID, start).Scan(&later); err != nil {
		return RunResult{}, dbErr("read later variation margin runs", err)
	}
	if later {
		return RunResult{}, errs.New(errs.CodeValidationFailed, "derivatives: a later interval has already been settled; this one is subsumed by it").
			WithField("interval_start", start)
	}
	if spec.ExpiredAt(now) {
		return RunResult{}, errs.New(errs.CodeValidationFailed, "derivatives: an expired future is settled by the final settlement")
	}
	mark, ok, err := markAt(ctx, tx, r.InstrumentID, now, spec.OracleMaxAge)
	if err != nil {
		return RunResult{}, err
	}
	if !ok {
		return RunResult{}, errs.New(errs.CodeStaleMarketData, "derivatives: no fresh mark; variation margin is never settled on a stale price").
			WithField("now", now)
	}
	eligible, err := lockPositionsForInstrument(ctx, tx, r.InstrumentID)
	if err != nil {
		return RunResult{}, err
	}
	runID := newVMRunID()
	if _, err := tx.Exec(ctx, `INSERT INTO variation_margin_runs (id, instrument_id, interval_start, interval_end, mark_id, mark_mantissa, positions)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, runID, r.InstrumentID, start, end, mark.ID, mark.MarkMantissa, len(eligible)); err != nil {
		if db.IsUniqueViolation(err) {
			// Only reachable when the caller's isolation level hid a
			// concurrent run that committed first: retrying replays it.
			return RunResult{}, errs.Wrap(err, errs.CodeConflict, "derivatives: this interval was settled concurrently; retry to replay it")
		}
		return RunResult{}, dbErr("claim variation margin run", err)
	}
	ch, err := s.ClearingHouse(ctx, tx)
	if err != nil {
		return RunResult{}, err
	}
	res := RunResult{ID: runID.String(), IntervalStart: start, IntervalEnd: end, MarkID: mark.ID, Positions: len(eligible)}
	ref := ledger.FinancialEventReference{Type: "variation_margin_run", ID: runID.String()}
	var payers, receivers []leg
	for _, p := range eligible {
		cash, settled, err := variationCash(spec, p.state(), mark.MarkMantissa)
		if err != nil {
			return RunResult{}, err
		}
		if cash.IsZero() {
			continue
		}
		key := "deriv:vm:" + r.InstrumentID.String() + ":" + strconv.FormatInt(start.UnixMilli(), 10) + ":" + p.ID.String()
		l, pay, after, err := s.obligationLeg(ctx, tx, ch, spec, p, cash, FlowVariationMargin, ledger.KindVariationMargin,
			EffectVariationMargin, key, ref)
		if err != nil {
			return RunResult{}, err
		}
		after.SettledValue = settled
		if _, err := savePosition(ctx, tx, after); err != nil {
			return RunResult{}, err
		}
		if cash.IsNegative() {
			res.Paid, res.Shortfall = res.Paid.Add(pay.collected()), res.Shortfall.Add(pay.Shortfall)
			payers = append(payers, l)
		} else {
			res.Received = res.Received.Add(cash)
			receivers = append(receivers, l)
		}
	}
	posted, err := s.executeLegs(ctx, tx, ch, append(payers, receivers...),
		"derivatives/variation-margin/"+r.InstrumentID.String()+"/"+strconv.FormatInt(start.UnixMilli(), 10),
		func(pl []postedLeg) any {
			return runEvidence{Kind: "VARIATION_MARGIN", RunID: runID.String(), InstrumentID: r.InstrumentID.String(),
				IntervalStart: start, IntervalEnd: end, MarkID: mark.ID.String(), Mark: mark.MarkMantissa.String(),
				PerContract: "0", PolicyVersion: s.policy.Version, PolicyHash: s.policy.Hash(), Legs: pl}
		}, r.CorrelationID)
	if err != nil {
		return RunResult{}, err
	}
	res.Journals = journals(posted)
	return res, nil
}

// fundingPlan is one position's funding for an interval.
type fundingPlan struct {
	pos    Position
	qty    money.Quantity
	dir    FundingDirection
	amount money.Quantity
	flow   *CashFlow
}

// RunFunding charges a perpetual's funding for one interval. The premium
// index is the exact average of (mark − index) / index over the marks of the
// interval, rounded half-even once to 10^-8, then clamped to the policy's
// ±maximum. One contract's funding is V(last mark of the interval) × |rate|,
// rounded down once; every position open at the funding time (the interval
// end) pays or receives |quantity at that time| × that amount -- longs pay a
// positive rate, shorts a negative one -- through the clearing house
// (FUNDING_PAYMENT). funding_rates is UNIQUE per (instrument, interval start)
// and funding_accruals per (position, interval start): a re-run posts nothing
// new, and concurrent runs fund once.
func (s *Service) RunFunding(ctx context.Context, tx pgx.Tx, r RunRequest) (RunResult, error) {
	if err := s.serializeClearing(ctx, tx); err != nil {
		return RunResult{}, err
	}
	spec, err := loadSpec(ctx, tx, s.instruments, r.InstrumentID)
	if err != nil {
		return RunResult{}, err
	}
	if spec.Kind != instruments.TypePerpetual {
		return RunResult{}, errs.New(errs.CodeValidationFailed, "derivatives: funding is charged on perpetuals")
	}
	now := s.clk.Now().UTC()
	end, err := interval(r.IntervalStart, spec.FundingInterval, now)
	if err != nil {
		return RunResult{}, err
	}
	start := r.IntervalStart.UTC()
	samples, err := marksWithIndexIn(ctx, tx, r.InstrumentID, start, end)
	if err != nil {
		return RunResult{}, err
	}
	if len(samples) == 0 {
		return RunResult{}, errs.New(errs.CodeStaleMarketData, "derivatives: no mark with an index in the funding interval; funding is not charged on nothing").
			WithField("interval_start", start)
	}
	ss := make([]IndexSample, len(samples))
	for i, m := range samples {
		ss[i] = sampleOf(m)
	}
	premium, err := PremiumIndexE8(ss)
	if err != nil {
		return RunResult{}, err
	}
	rate, clamped := ClampRate(premium, s.policy.MaxFundingRateE8)
	ref := samples[len(samples)-1]
	per, err := fundingPerContract(spec, ref.MarkMantissa, rate)
	if err != nil {
		return RunResult{}, err
	}
	positions, err := lockPositionsOpenAt(ctx, tx, r.InstrumentID, end)
	if err != nil {
		return RunResult{}, err
	}
	rateID := newFundingRateID()
	var claimed FundingRateID
	err = tx.QueryRow(ctx, `INSERT INTO funding_rates (id, instrument_id, interval_start, interval_end, samples, premium_index_e8,
		rate_e8, clamped, mark_id, mark_mantissa, per_contract_amount) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (instrument_id, interval_start) DO NOTHING RETURNING id`,
		rateID, r.InstrumentID, start, end, len(samples), premium, rate, clamped, ref.ID, ref.MarkMantissa, per).Scan(&claimed)
	if errors.Is(err, pgx.ErrNoRows) {
		var existing FundingRateID
		var storedRate, storedPremium int64
		var storedClamped bool
		var storedPer money.Quantity
		if err := tx.QueryRow(ctx, `SELECT id, premium_index_e8, rate_e8, clamped, per_contract_amount FROM funding_rates
			WHERE instrument_id = $1 AND interval_start = $2`, r.InstrumentID, start).
			Scan(&existing, &storedPremium, &storedRate, &storedClamped, &storedPer); err != nil {
			return RunResult{}, dbErr("read funding rate", err)
		}
		return RunResult{ID: existing.String(), Replay: true, IntervalStart: start, IntervalEnd: end,
			PremiumIndexE8: storedPremium, RateE8: storedRate, Clamped: storedClamped, PerContract: storedPer}, nil
	}
	if err != nil {
		return RunResult{}, dbErr("claim funding rate", err)
	}
	ch, err := s.ClearingHouse(ctx, tx)
	if err != nil {
		return RunResult{}, err
	}
	res := RunResult{ID: rateID.String(), IntervalStart: start, IntervalEnd: end, MarkID: ref.ID,
		PremiumIndexE8: premium, RateE8: rate, Clamped: clamped, PerContract: per}
	evRef := ledger.FinancialEventReference{Type: "funding_rate", ID: rateID.String()}
	var plans []fundingPlan
	var payers, receivers []leg
	for _, p := range positions {
		qty, err := quantityAt(ctx, tx, p.ID, end)
		if err != nil {
			return RunResult{}, err
		}
		if qty.IsZero() {
			continue
		}
		res.Positions++
		fp := fundingPlan{pos: p, qty: qty, dir: fundingDirection(qty, rate), amount: qty.Abs().Mul(per)}
		if fp.amount.IsZero() {
			plans = append(plans, fp)
			continue
		}
		signed := fp.amount
		if fp.dir == FundingPay {
			signed = signed.Neg()
		}
		key := "deriv:funding:" + r.InstrumentID.String() + ":" + strconv.FormatInt(start.UnixMilli(), 10) + ":" + p.ID.String()
		l, pay, after, err := s.obligationLeg(ctx, tx, ch, spec, p, signed, FlowFunding, ledger.KindFundingPayment, EffectFunding, key, evRef)
		if err != nil {
			return RunResult{}, err
		}
		if l.flow != nil {
			l.flow.ID = newCashFlowID()
			fp.flow = l.flow
		}
		after.FundingPnL = p.FundingPnL.Add(signed)
		if _, err := savePosition(ctx, tx, after); err != nil {
			return RunResult{}, err
		}
		if fp.dir == FundingPay {
			res.Paid, res.Shortfall = res.Paid.Add(pay.collected()), res.Shortfall.Add(pay.Shortfall)
			payers = append(payers, l)
		} else {
			res.Received = res.Received.Add(fp.amount)
			receivers = append(receivers, l)
		}
		plans = append(plans, fp)
	}
	posted, err := s.executeLegs(ctx, tx, ch, append(payers, receivers...),
		"derivatives/funding/"+r.InstrumentID.String()+"/"+strconv.FormatInt(start.UnixMilli(), 10),
		func(pl []postedLeg) any {
			return runEvidence{Kind: "FUNDING", RunID: rateID.String(), InstrumentID: r.InstrumentID.String(),
				IntervalStart: start, IntervalEnd: end, MarkID: ref.ID.String(), Mark: ref.MarkMantissa.String(),
				Premium: premium, Rate: rate, Clamped: clamped, PerContract: per.String(), Samples: len(samples),
				PolicyVersion: s.policy.Version, PolicyHash: s.policy.Hash(), Legs: pl}
		}, r.CorrelationID)
	if err != nil {
		return RunResult{}, err
	}
	for _, fp := range plans {
		var flowID any
		if fp.flow != nil {
			flowID = fp.flow.ID
		}
		if _, err := tx.Exec(ctx, `INSERT INTO funding_accruals (id, funding_rate_id, position_id, interval_start, quantity, direction, amount, cash_flow_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, newAccrualID(), rateID, fp.pos.ID, start, fp.qty, fp.dir, fp.amount, flowID); err != nil {
			return RunResult{}, dbErr("record funding accrual", err)
		}
	}
	res.Journals = journals(posted)
	return res, nil
}
