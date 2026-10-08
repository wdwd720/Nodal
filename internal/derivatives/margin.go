package derivatives

import (
	"time"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/quant"
)

// The margin model. Requirements are in base units of the settlement asset
// and every rounding is up (a requirement is never understated by rounding).
//
//   - Futures and perpetuals: linear. Initial and maintenance margin are the
//     notional |Q| × V(mark) times the policy's rates for the underlying's
//     risk class. A perpetual's initial margin also carries an expected
//     funding buffer: FundingBufferIntervals intervals at the maximum rate.
//     In the last ExpiryGapWindow before a future's expiry, initial margin is
//     at least the loss of an ExpiryGapShock move: the expiry jump.
//   - Long options: zero. The holder's maximum loss is the premium, which was
//     paid in full when the option was bought.
//   - Short options: the worst loss over the policy's deterministic grid of
//     underlying moves (quant.ScenarioGridChecked), each scenario valued at
//     its exact intrinsic value against the option's current mark, plus a
//     minimum margin on the underlying notional. In the expiry window the grid
//     gains the ±ExpiryGapShock scenarios. Maintenance is a policy share of
//     that.

// MarkInput is the market state a requirement is computed at.
type MarkInput struct {
	// Mark is the instrument's mark at its price scale, on its tick grid.
	Mark money.Quantity
	// HasIndex reports whether Index is known; options need it.
	HasIndex   bool
	Index      money.Quantity
	IndexScale int32
}

// markInputOf builds a MarkInput from a stored mark.
func markInputOf(m Mark) MarkInput {
	return MarkInput{Mark: m.MarkMantissa, HasIndex: m.HasIndex, Index: m.IndexMantissa, IndexScale: m.IndexScale}
}

// Requirement is the margin requirement of one position.
type Requirement struct {
	// Notional is |Q| × V(mark) for a linear position and the underlying
	// notional |Q| × V(index) for an option.
	Notional    money.Quantity `json:"notional"`
	Initial     money.Quantity `json:"initial"`
	Maintenance money.Quantity `json:"maintenance"`
	// ScenarioLoss is the worst scenario loss that entered the requirement
	// (a written option's grid, a future's expiry gap); zero otherwise.
	ScenarioLoss money.Quantity `json:"scenario_loss"`
	// FundingBuffer is the perpetual's expected-funding buffer.
	FundingBuffer money.Quantity `json:"funding_buffer"`
}

// inGapWindow reports whether now is in the expiry-jump window.
func inGapWindow(p Policy, s Spec, now time.Time) bool {
	if s.Expiry == nil || p.ExpiryGapWindowMS <= 0 || !now.Before(*s.Expiry) {
		return false
	}
	return s.Expiry.Sub(now) <= time.Duration(p.ExpiryGapWindowMS)*time.Millisecond
}

// ceilBPS returns ceil(v × bps / 10 000) for v >= 0.
func ceilBPS(v money.Quantity, bps money.BPS) (money.Quantity, error) {
	r, err := v.MulBPSChecked(bps, money.RoundCeil)
	if err != nil {
		return money.Quantity{}, errs.Wrap(err, errs.CodeInternal, "derivatives: apply a margin rate")
	}
	return r, nil
}

// RequirementFor computes the requirement of a position of signed quantity q.
func RequirementFor(p Policy, s Spec, q money.Quantity, m MarkInput, now time.Time) (Requirement, error) {
	if q.IsZero() {
		return Requirement{}, nil
	}
	if s.IsLinear() {
		return linearRequirement(p, s, q, m, now)
	}
	return optionRequirement(p, s, q, m, now)
}

func linearRequirement(p Policy, s Spec, q money.Quantity, m MarkInput, now time.Time) (Requirement, error) {
	rates, err := p.linearRates(s.UnderlyingRiskClass)
	if err != nil {
		return Requirement{}, err
	}
	notional, err := s.Value(q.Abs(), m.Mark)
	if err != nil {
		return Requirement{}, err
	}
	r := Requirement{Notional: notional}
	if r.Initial, err = ceilBPS(notional, rates.InitialBPS); err != nil {
		return Requirement{}, err
	}
	if r.Maintenance, err = ceilBPS(notional, rates.MaintenanceBPS); err != nil {
		return Requirement{}, err
	}
	if s.Kind == instruments.TypePerpetual && p.FundingBufferIntervals > 0 {
		buf, err := notional.MulDiv(money.QuantityFromInt64(p.MaxFundingRateE8).Mul(money.QuantityFromInt64(p.FundingBufferIntervals)),
			money.QuantityFromInt64(FundingRateScale), money.RoundCeil)
		if err != nil {
			return Requirement{}, errs.Wrap(err, errs.CodeInternal, "derivatives: funding buffer")
		}
		r.FundingBuffer = buf
		r.Initial = r.Initial.Add(buf)
	}
	if inGapWindow(p, s, now) {
		gap, err := ceilBPS(notional, p.ExpiryGapShockBPS)
		if err != nil {
			return Requirement{}, err
		}
		r.ScenarioLoss = gap
		r.Initial = r.Initial.Max(gap)
	}
	return r, nil
}

// optionShocks is the scenario grid for a written option at now.
func optionShocks(p Policy, s Spec, now time.Time) []money.BPS {
	shocks := append([]money.BPS(nil), p.OptionShocksBPS...)
	if inGapWindow(p, s, now) && p.ExpiryGapShockBPS > 0 {
		shocks = append(shocks, -p.ExpiryGapShockBPS, p.ExpiryGapShockBPS)
	}
	return shocks
}

// scenarioIntrinsicValue returns the exact intrinsic value of one contract at
// an underlying price at indexScale, rounded up (it is a liability).
func scenarioIntrinsicValue(s Spec, underlying money.Quantity, indexScale int32) (money.Quantity, error) {
	intrinsic, _, _, c := intrinsicAt(s, underlying, indexScale)
	return s.ValueAnyPrice(money.QuantityFromInt64(1), intrinsic, c, money.RoundCeil)
}

func optionRequirement(p Policy, s Spec, q money.Quantity, m MarkInput, now time.Time) (Requirement, error) {
	if !m.HasIndex || !m.Index.IsPositive() {
		return Requirement{}, errs.New(errs.CodeStaleMarketData, "derivatives: an option is margined against its underlying's index and none is known").
			WithField("instrument_id", s.InstrumentID.String())
	}
	n := q.Abs()
	notional, err := s.ValueAnyPrice(n, m.Index, m.IndexScale, money.RoundCeil)
	if err != nil {
		return Requirement{}, err
	}
	r := Requirement{Notional: notional}
	if q.IsPositive() {
		// A holder can lose the premium and nothing more; it was paid.
		return r, nil
	}
	liabilityNow, err := s.Value(n, m.Mark)
	if err != nil {
		return Requirement{}, err
	}
	prices, err := quant.ScenarioGridChecked(m.Index, optionShocks(p, s, now), money.RoundHalfEven)
	if err != nil {
		return Requirement{}, err
	}
	worst := money.Quantity{}
	for _, px := range prices {
		per, err := scenarioIntrinsicValue(s, px, m.IndexScale)
		if err != nil {
			return Requirement{}, err
		}
		worst = worst.Max(n.Mul(per).Sub(liabilityNow))
	}
	minMargin, err := ceilBPS(notional, p.OptionMinMarginBPS)
	if err != nil {
		return Requirement{}, err
	}
	r.ScenarioLoss = worst
	r.Initial = worst.Add(minMargin)
	if r.Maintenance, err = ceilBPS(r.Initial, p.OptionMaintenanceRatioBPS); err != nil {
		return Requirement{}, err
	}
	return r, nil
}

// initialMarginForFill is the initial margin an opening fill of n (positive)
// contracts on side posts: the requirement of a position of exactly that
// size. It depends only on the fill, never on the rest of the position, so the
// amount reserved before the fill and the amount posted by it are the same
// number whatever happens concurrently.
func initialMarginForFill(p Policy, s Spec, side FillSide, n money.Quantity, m MarkInput, now time.Time) (money.Quantity, error) {
	q := n
	if side == Sell {
		q = n.Neg()
	}
	r, err := RequirementFor(p, s, q, m, now)
	if err != nil {
		return money.Quantity{}, err
	}
	return r.Initial, nil
}

// StressPosition is one position of a book under stress.
type StressPosition struct {
	Spec     Spec
	Quantity money.Quantity
	Mark     MarkInput
}

// ScenarioResult is the book's profit or loss under one shock.
type ScenarioResult struct {
	ShockBPS money.BPS      `json:"shock_bps"`
	PnL      money.Quantity `json:"pnl"`
}

// StressReport is the result of StressBook.
type StressReport struct {
	Scenarios []ScenarioResult `json:"scenarios"`
	// WorstPnL is the lowest scenario PnL (negative for a loss).
	WorstPnL money.Quantity `json:"worst_pnl"`
	// WorstShockBPS is the shock that produced it (the first, on ties).
	WorstShockBPS money.BPS `json:"worst_shock_bps"`
}

// StressBook revalues a fixed book under each shock and returns the book's
// PnL per scenario, exactly and deterministically. A linear position moves
// with its mark: Q × (V(mark × (1 + shock)) − V(mark)). An option is valued at
// its intrinsic value at the shocked index (the expiry-jump view) against its
// current mark: Q × V(intrinsic) − Q × V(mark). Each revaluation rounds once,
// toward negative infinity, so a reported loss is never understated.
func StressBook(book []StressPosition, shocks []money.BPS) (StressReport, error) {
	if len(shocks) == 0 {
		return StressReport{}, errs.New(errs.CodeValidationFailed, "derivatives: a stress test needs at least one shock")
	}
	out := StressReport{}
	hundred := money.QuantityFromInt64(int64(money.OneHundredPercent))
	for i, shock := range shocks {
		if shock < -money.OneHundredPercent {
			return StressReport{}, errs.New(errs.CodeValidationFailed, "derivatives: a shock cannot move a price below zero")
		}
		total := money.Quantity{}
		for _, bp := range book {
			s := bp.Spec
			now, err := s.Value(bp.Quantity, bp.Mark.Mark)
			if err != nil {
				return StressReport{}, err
			}
			var shocked money.Quantity
			if s.IsLinear() {
				px := bp.Mark.Mark.Mul(hundred.Add(money.QuantityFromInt64(int64(shock))))
				if shocked, err = s.ValueAnyPrice(bp.Quantity, px, s.PriceScale+4, money.RoundFloor); err != nil {
					return StressReport{}, err
				}
			} else {
				if !bp.Mark.HasIndex {
					return StressReport{}, errs.New(errs.CodeStaleMarketData, "derivatives: an option is stressed against its index and none is known")
				}
				prices, err := quant.ScenarioGridChecked(bp.Mark.Index, []money.BPS{shock}, money.RoundHalfEven)
				if err != nil {
					return StressReport{}, err
				}
				intrinsic, _, _, c := intrinsicAt(s, prices[0], bp.Mark.IndexScale)
				if shocked, err = s.ValueAnyPrice(bp.Quantity, intrinsic, c, money.RoundFloor); err != nil {
					return StressReport{}, err
				}
			}
			total = total.Add(shocked.Sub(now))
		}
		out.Scenarios = append(out.Scenarios, ScenarioResult{ShockBPS: shock, PnL: total})
		if i == 0 || total.Cmp(out.WorstPnL) < 0 {
			out.WorstPnL, out.WorstShockBPS = total, shock
		}
	}
	return out, nil
}
