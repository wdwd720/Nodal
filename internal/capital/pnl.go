package capital

import (
	"errors"
	"time"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// Limit-hit reasons reported by applyRealizedPnL and SetStatus.
const (
	limitDailyLoss = "daily_loss"
	limitDrawdown  = "drawdown"
)

// nextUTCMidnight returns the first UTC midnight strictly after t. It is the
// daily-loss reset boundary: daily_loss_reset_at is always a UTC midnight.
func nextUTCMidnight(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day()+1, 0, 0, 0, 0, time.UTC)
}

// withDailyReset returns e with the daily-loss counter reset when the reset
// instant has passed at now, and reports whether it did.
func withDailyReset(e Envelope, now time.Time) (Envelope, bool) {
	if now.Before(e.DailyLossResetAt) {
		return e, false
	}
	e.DailyLoss = money.USD{}
	e.DailyLossResetAt = nextUTCMidnight(now)
	return e, true
}

// limitBreached reports which loss limit the envelope currently breaches:
// daily_loss when DailyLoss ≥ MaxDailyLoss, drawdown when CurrentDrawdown ≥
// MaxDrawdown, "" otherwise. A limit of zero is zero tolerance (any loss
// breaches it), never "unlimited": limits fail closed. A zero counter never
// breaches, so a fresh envelope with zero limits is usable until it loses.
func limitBreached(e Envelope) string {
	if e.DailyLoss.IsPositive() && e.DailyLoss.Cmp(e.MaxDailyLoss) >= 0 {
		return limitDailyLoss
	}
	if e.CurrentDrawdown.IsPositive() && e.CurrentDrawdown.Cmp(e.MaxDrawdown) >= 0 {
		return limitDrawdown
	}
	return ""
}

// pnlOutcome describes what applyRealizedPnL did beyond the arithmetic.
type pnlOutcome struct {
	DailyReset bool   // the daily-loss counter was reset before applying
	LimitHit   string // "" or one of limitDailyLoss / limitDrawdown
	Exhausted  bool   // status moved ACTIVE → EXHAUSTED in this call
}

// applyRealizedPnL applies a realized P&L amount (negative for a loss) to
// the envelope's P&L counters with exact, overflow-checked arithmetic:
//
//	realized_pnl     += pnl
//	realized_loss    += −pnl            (losses only)
//	daily_loss       += −pnl            (losses only; reset first if due)
//	current_drawdown  = peak − realized_pnl, peak = max(previous peak, realized_pnl)
//
// The previous peak is recovered as realized_pnl + current_drawdown, so no
// extra column is needed. When a limit is breached and the envelope is
// ACTIVE it becomes EXHAUSTED. That is limit enforcement, not an authority
// change: no principal is involved and the caller records it with a SYSTEM
// actor. Any other status is left untouched (a PAUSED envelope stays PAUSED;
// SetStatus refuses to re-activate it while a limit is breached).
func applyRealizedPnL(e Envelope, pnl money.USD, now time.Time) (Envelope, pnlOutcome, error) {
	var out pnlOutcome
	e, out.DailyReset = withDailyReset(e, now)

	peak, err := e.RealizedPnL.Add(e.CurrentDrawdown)
	if err != nil {
		return Envelope{}, out, moneyErr(err)
	}
	newPnL, err := e.RealizedPnL.Add(pnl)
	if err != nil {
		return Envelope{}, out, moneyErr(err)
	}
	if pnl.IsNegative() {
		loss, err := pnl.NegChecked()
		if err != nil {
			return Envelope{}, out, moneyErr(err)
		}
		if e.RealizedLoss, err = e.RealizedLoss.Add(loss); err != nil {
			return Envelope{}, out, moneyErr(err)
		}
		if e.DailyLoss, err = e.DailyLoss.Add(loss); err != nil {
			return Envelope{}, out, moneyErr(err)
		}
	}
	e.RealizedPnL = newPnL
	if newPnL.Cmp(peak) > 0 {
		peak = newPnL
	}
	if e.CurrentDrawdown, err = peak.Sub(newPnL); err != nil {
		return Envelope{}, out, moneyErr(err)
	}
	if reason := limitBreached(e); reason != "" {
		out.LimitHit = reason
		if e.Status == EnvelopeActive {
			e.Status = EnvelopeExhausted
			out.Exhausted = true
		}
	}
	return e, out, nil
}

// moneyErr maps money sentinels onto stable codes so exact-arithmetic
// failures reach the API as OVERFLOW / PRECISION_LOSS rather than INTERNAL.
func moneyErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, money.ErrOverflow):
		return errs.Wrap(err, errs.CodeOverflow, "amount outside the representable range")
	case errors.Is(err, money.ErrPrecisionLoss):
		return errs.Wrap(err, errs.CodePrecisionLoss, "amount cannot be represented exactly")
	default:
		return errs.Wrap(err, errs.CodeInternal, "money arithmetic failed")
	}
}
