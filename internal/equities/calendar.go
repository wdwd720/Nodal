package equities

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	// The calendar must never depend on the host's zoneinfo database: a
	// missing or stale tzdata would move every session boundary silently.
	_ "time/tzdata"

	"github.com/nodal/controlplane/internal/errs"
)

// newYorkZone is the zone every US equity session is defined in.
const newYorkZone = "America/New_York"

// Session boundaries, in minutes after New York midnight. Every boundary
// belongs to the session it opens: 09:30:00 is REGULAR, 16:00:00 is
// AFTER_HOURS, 20:00:00 is CLOSED.
const (
	preMarketOpenMinute   = 4 * 60
	regularOpenMinute     = 9*60 + 30
	regularCloseMinute    = 16 * 60
	afterHoursCloseMinute = 20 * 60
)

// SettlementCycleDays is the settlement cycle: trades settle T+1 trading
// days after their trade date.
const SettlementCycleDays = 1

// maxCalendarScan bounds every walk across the calendar. A calendar in which
// no trading day exists within a year is malformed, and the walk fails
// closed rather than looping.
const maxCalendarScan = 370

// Date is a calendar day with no time and no zone (a trade date, a record
// date, a settlement date). The zero Date is "no date".
type Date struct {
	t time.Time // midnight UTC
}

// NewDate returns the date y-m-d.
func NewDate(y int, m time.Month, d int) Date {
	return Date{t: time.Date(y, m, d, 0, 0, 0, 0, time.UTC)}
}

// DateOf returns the UTC calendar day of t. It is meant for values read from
// a date column, which arrive as midnight UTC; it is never the trade date of
// an instant (Calendar.TradeDate is).
func DateOf(t time.Time) Date {
	y, m, d := t.UTC().Date()
	return NewDate(y, m, d)
}

// Bounds of a parsed date: nothing in this subsystem happens outside them,
// and the lower bound keeps a parsed date from ever being the zero Date.
const (
	minDateYear = 1900
	maxDateYear = 9999
)

// ParseDate parses the canonical form 2006-01-02, for years 1900 to 9999.
func ParseDate(s string) (Date, error) {
	if len(s) != len("2006-01-02") {
		return Date{}, errs.New(errs.CodeValidationFailed, "equities: a date is YYYY-MM-DD")
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return Date{}, errs.New(errs.CodeValidationFailed, "equities: a date is YYYY-MM-DD")
	}
	if y := t.Year(); y < minDateYear || y > maxDateYear {
		return Date{}, errs.Newf(errs.CodeValidationFailed, "equities: a date is between %d and %d", minDateYear, maxDateYear)
	}
	return Date{t: t.UTC()}, nil
}

// String renders 2006-01-02, or "" for the zero Date.
func (d Date) String() string {
	if d.IsZero() {
		return ""
	}
	return d.t.Format("2006-01-02")
}

// IsZero reports whether d is the zero Date.
func (d Date) IsZero() bool { return d.t.IsZero() }

// Time returns midnight UTC of d, the form a date column is written in.
func (d Date) Time() time.Time { return d.t }

// AddDays returns d moved by n calendar days.
func (d Date) AddDays(n int) Date { return Date{t: d.t.AddDate(0, 0, n)} }

// Weekday returns the day of the week.
func (d Date) Weekday() time.Weekday { return d.t.Weekday() }

// Compare returns -1, 0 or +1.
func (d Date) Compare(o Date) int { return d.t.Compare(o.t) }

// Before reports whether d is before o.
func (d Date) Before(o Date) bool { return d.t.Before(o.t) }

// After reports whether d is after o.
func (d Date) After(o Date) bool { return d.t.After(o.t) }

// Equal reports whether d and o are the same day.
func (d Date) Equal(o Date) bool { return d.t.Equal(o.t) }

// MarshalJSON renders the canonical string.
func (d Date) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// UnmarshalJSON parses the canonical string; "" is the zero Date.
func (d *Date) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errs.New(errs.CodeValidationFailed, "equities: a date is a JSON string")
	}
	if s == "" {
		*d = Date{}
		return nil
	}
	p, err := ParseDate(s)
	if err != nil {
		return err
	}
	*d = p
	return nil
}

// Holiday is a full-day market closure.
type Holiday struct {
	Date Date
	Name string
}

// Calendar is an immutable trading calendar. It is safe for concurrent use.
type Calendar struct {
	code     string
	loc      *time.Location
	holidays map[Date]string
}

// NewCalendar builds the calendar code with the given full-day closures. The
// only calendar this build implements is XNYS; any other code fails closed,
// as does a host on which America/New_York cannot be loaded.
func NewCalendar(code string, holidays []Holiday) (*Calendar, error) {
	if code != CalendarXNYS {
		return nil, errs.Newf(errs.CodeUnsupported, "equities: trading calendar %q is not implemented; only %s is", code, CalendarXNYS).
			WithField("calendar", code)
	}
	loc, err := time.LoadLocation(newYorkZone)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "equities: the America/New_York zone is unavailable; the calendar fails closed")
	}
	c := &Calendar{code: code, loc: loc, holidays: make(map[Date]string, len(holidays))}
	for _, h := range holidays {
		if h.Date.IsZero() {
			return nil, errs.New(errs.CodeValidationFailed, "equities: a holiday needs a date")
		}
		name := strings.TrimSpace(h.Name)
		if name == "" {
			return nil, errs.Newf(errs.CodeValidationFailed, "equities: holiday %s needs a name", h.Date)
		}
		if _, dup := c.holidays[h.Date]; dup {
			return nil, errs.Newf(errs.CodeValidationFailed, "equities: holiday %s is listed twice", h.Date)
		}
		c.holidays[h.Date] = name
	}
	return c, nil
}

// Code returns the calendar code.
func (c *Calendar) Code() string { return c.code }

// Location returns the calendar's zone.
func (c *Calendar) Location() *time.Location { return c.loc }

// HolidayName returns the name of the closure on d, if any.
func (c *Calendar) HolidayName(d Date) (string, bool) {
	n, ok := c.holidays[d]
	return n, ok
}

// IsTradingDay reports whether d is a weekday that is not a holiday.
func (c *Calendar) IsTradingDay(d Date) bool {
	if d.IsZero() {
		return false
	}
	switch d.Weekday() {
	case time.Saturday, time.Sunday:
		return false
	}
	_, closed := c.holidays[d]
	return !closed
}

// TradeDate is the New York calendar day of the instant t. Every session of
// a day -- pre-market, regular and after-hours -- shares its trade date.
func (c *Calendar) TradeDate(t time.Time) Date {
	y, m, d := t.In(c.loc).Date()
	return NewDate(y, m, d)
}

// At returns the instant at minute (after New York midnight) of day d, in
// UTC. Daylight saving is the zone's business, not the caller's.
func (c *Calendar) At(d Date, minute int) time.Time {
	y, m, dd := d.t.Date()
	return time.Date(y, m, dd, minute/60, minute%60, 0, 0, c.loc).UTC()
}

// StartOfDay returns 00:00 New York time on d, in UTC.
func (c *Calendar) StartOfDay(d Date) time.Time { return c.At(d, 0) }

// SessionAt returns the session in force at t.
func (c *Calendar) SessionAt(t time.Time) Session {
	local := t.In(c.loc)
	if !c.IsTradingDay(c.TradeDate(t)) {
		return SessionClosed
	}
	minute := local.Hour()*60 + local.Minute()
	switch {
	case minute < preMarketOpenMinute:
		return SessionClosed
	case minute < regularOpenMinute:
		return SessionPreMarket
	case minute < regularCloseMinute:
		return SessionRegular
	case minute < afterHoursCloseMinute:
		return SessionAfterHours
	}
	return SessionClosed
}

// NextTradingDay returns the first trading day after d.
func (c *Calendar) NextTradingDay(d Date) (Date, error) {
	for i := 1; i <= maxCalendarScan; i++ {
		if n := d.AddDays(i); c.IsTradingDay(n) {
			return n, nil
		}
	}
	return Date{}, c.noTradingDay(d)
}

// PreviousTradingDay returns the last trading day before d.
func (c *Calendar) PreviousTradingDay(d Date) (Date, error) {
	for i := 1; i <= maxCalendarScan; i++ {
		if p := d.AddDays(-i); c.IsTradingDay(p) {
			return p, nil
		}
	}
	return Date{}, c.noTradingDay(d)
}

func (c *Calendar) noTradingDay(d Date) error {
	return errs.Newf(errs.CodeInternal, "equities: calendar %s has no trading day within %d days of %s", c.code, maxCalendarScan, d)
}

// SettlementDate returns the settlement date of a trade on tradeDate:
// SettlementCycleDays trading days later. A trade date that is not a trading
// day is refused: nothing trades on it.
func (c *Calendar) SettlementDate(tradeDate Date) (Date, error) {
	if !c.IsTradingDay(tradeDate) {
		return Date{}, errs.Newf(errs.CodeValidationFailed, "equities: %s is not a trading day on %s", tradeDate, c.code).
			WithField("trade_date", tradeDate.String())
	}
	d := tradeDate
	for i := 0; i < SettlementCycleDays; i++ {
		n, err := c.NextTradingDay(d)
		if err != nil {
			return Date{}, err
		}
		d = n
	}
	return d, nil
}

// OrderWindow answers, for an order of type typ arriving at t, which session
// is in force, whether the order may be accepted in it, and when it expires.
//
// A market order is eligible only in the regular session (outside it, it is
// refused, never queued). A limit order is eligible in the regular session,
// and also in pre-market and after-hours when extended is set. Every order is
// a DAY order and expires at the end of the last session it may trade in on
// its trade date: 16:00, or 20:00 for an extended-hours order.
func (c *Calendar) OrderWindow(t time.Time, typ OrderType, extended bool) (Session, bool, time.Time) {
	s := c.SessionAt(t)
	day := c.TradeDate(t)
	expires := c.At(day, regularCloseMinute)
	if extended {
		expires = c.At(day, afterHoursCloseMinute)
	}
	switch {
	case typ == OrderMarket:
		return s, s == SessionRegular && !extended, expires
	case typ == OrderLimit && extended:
		return s, s == SessionPreMarket || s == SessionRegular || s == SessionAfterHours, expires
	case typ == OrderLimit:
		return s, s == SessionRegular, expires
	}
	return s, false, expires
}

// ExDateFor returns the ex-date for a record date: the first trading day
// whose trades settle after the record date. Under T+1 it is the record date
// itself when that is a trading day.
func (c *Calendar) ExDateFor(record Date) (Date, error) {
	if record.IsZero() {
		return Date{}, errs.New(errs.CodeValidationFailed, "equities: a record date is required")
	}
	// The latest trading day on or before the record date settles after it
	// only if nothing earlier does, so walk back while the previous trading
	// day still settles after the record date.
	ex := record
	if !c.IsTradingDay(ex) {
		p, err := c.PreviousTradingDay(ex)
		if err != nil {
			return Date{}, err
		}
		ex = p
	}
	for i := 0; i < maxCalendarScan; i++ {
		settle, err := c.SettlementDate(ex)
		if err != nil {
			return Date{}, err
		}
		if !settle.After(record) {
			// ex itself settles by the record date: the ex-date is later.
			return c.NextTradingDay(ex)
		}
		prev, err := c.PreviousTradingDay(ex)
		if err != nil {
			return Date{}, err
		}
		prevSettle, err := c.SettlementDate(prev)
		if err != nil {
			return Date{}, err
		}
		if !prevSettle.After(record) {
			return ex, nil
		}
		ex = prev
	}
	return Date{}, c.noTradingDay(record)
}

// ValidateExDate refuses an ex-date that is not the one the settlement cycle
// implies for the record date.
func (c *Calendar) ValidateExDate(ex, record Date) error {
	want, err := c.ExDateFor(record)
	if err != nil {
		return err
	}
	if !ex.Equal(want) {
		return errs.New(errs.CodeValidationFailed, fmt.Sprintf(
			"equities: under T+%d on %s the ex-date for record date %s is %s, not %s",
			SettlementCycleDays, c.code, record, want, ex)).
			WithField("ex_date", ex.String()).
			WithField("expected_ex_date", want.String())
	}
	return nil
}
