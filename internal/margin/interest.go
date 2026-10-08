package margin

import (
	"math/big"
	"time"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// nsPerYear is the ACT/365 Fixed year in nanoseconds.
const nsPerYear int64 = 365 * 24 * 3_600 * 1_000_000_000

// InterestFor is the simple interest on principal at an annual rate between
// from and to, on the ACT/365 Fixed day count measured exactly in
// nanoseconds, rounded up (RoundCeil) to a whole numeraire base unit:
//
//	ceil(principal × rateBPS × (to − from) / (10_000 × 365 days))
//
// Rounding up is the lender's side: a period with a positive principal, rate
// and length always accrues at least one base unit. It returns zero when the
// period is empty or the principal or rate is not positive.
func InterestFor(principal money.Quantity, rate money.BPS, from, to time.Time) (money.Quantity, error) {
	if !to.After(from) || principal.Sign() <= 0 || rate <= 0 {
		return money.Quantity{}, nil
	}
	elapsed := to.Sub(from)
	num := new(big.Int).Mul(principal.BigInt(), big.NewInt(int64(rate)))
	num.Mul(num, big.NewInt(int64(elapsed)))
	den := new(big.Int).Mul(big.NewInt(10_000), big.NewInt(nsPerYear))
	q, err := money.QuantityFromBigInt(num).Div(money.QuantityFromBigInt(den), money.RoundCeil)
	if err != nil {
		return money.Quantity{}, errs.Wrap(err, errs.CodeInternal, "margin: interest arithmetic")
	}
	return q, nil
}

// Period is one interest accrual period [Start, End).
type Period struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// dbTime truncates t to the database's microsecond precision, in UTC, so a
// boundary computed here equals the one read back.
func dbTime(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

// nextMidnight returns the first UTC midnight strictly after t.
func nextMidnight(t time.Time) time.Time {
	t = t.UTC()
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return day.Add(24 * time.Hour)
}

// DayPeriods splits [from, until) at every UTC midnight, oldest first, and
// returns at most max periods. Without a stub only periods that end on a
// midnight are returned (the daily accrual run); with a stub the last period
// ends exactly at until (a repayment or a close accrues to the instant).
// Boundaries are truncated to microseconds, so the same inputs always yield
// the same periods and a replay finds the periods it already wrote.
func DayPeriods(from, until time.Time, stub bool, max int) []Period {
	from, until = dbTime(from), dbTime(until)
	var out []Period
	cur := from
	for len(out) < max && cur.Before(until) {
		end := nextMidnight(cur)
		if end.After(until) {
			if !stub {
				break
			}
			end = until
		}
		out = append(out, Period{Start: cur, End: end})
		cur = end
	}
	return out
}
