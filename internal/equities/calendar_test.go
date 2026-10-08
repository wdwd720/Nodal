package equities

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/errs"
)

// thanksgiving2026 is a weekday closure used across the tests.
var thanksgiving2026 = NewDate(2026, time.November, 26)

func testCalendar(t testing.TB) *Calendar {
	t.Helper()
	c, err := NewCalendar(CalendarXNYS, []Holiday{
		{Date: thanksgiving2026, Name: "Thanksgiving Day"},
		{Date: NewDate(2026, time.December, 25), Name: "Christmas Day"},
		{Date: NewDate(2027, time.January, 1), Name: "New Year's Day"},
	})
	require.NoError(t, err)
	return c
}

// ny returns the instant hh:mm:ss New York time on y-m-d.
func ny(t testing.TB, y int, m time.Month, d, hh, mm, ss int) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	return time.Date(y, m, d, hh, mm, ss, 0, loc).UTC()
}

// TestCalendar_SessionTable is the session rule, boundary by boundary: every
// boundary belongs to the session it opens, weekends and holidays are closed
// all day, and daylight saving moves nothing in New York time.
func TestCalendar_SessionTable(t *testing.T) {
	c := testCalendar(t)
	cases := []struct {
		name string
		at   time.Time
		want Session
	}{
		{"overnight", ny(t, 2026, 10, 7, 3, 59, 59), SessionClosed},
		{"pre-market opens 04:00", ny(t, 2026, 10, 7, 4, 0, 0), SessionPreMarket},
		{"pre-market last second", ny(t, 2026, 10, 7, 9, 29, 59), SessionPreMarket},
		{"regular opens 09:30", ny(t, 2026, 10, 7, 9, 30, 0), SessionRegular},
		{"regular midday", ny(t, 2026, 10, 7, 12, 0, 0), SessionRegular},
		{"regular last second", ny(t, 2026, 10, 7, 15, 59, 59), SessionRegular},
		{"after-hours opens 16:00", ny(t, 2026, 10, 7, 16, 0, 0), SessionAfterHours},
		{"after-hours last second", ny(t, 2026, 10, 7, 19, 59, 59), SessionAfterHours},
		{"closed at 20:00", ny(t, 2026, 10, 7, 20, 0, 0), SessionClosed},
		{"closed before midnight", ny(t, 2026, 10, 7, 23, 59, 59), SessionClosed},
		{"saturday regular hours", ny(t, 2026, 10, 10, 11, 0, 0), SessionClosed},
		{"sunday pre-market hours", ny(t, 2026, 10, 11, 5, 0, 0), SessionClosed},
		{"holiday regular hours", ny(t, 2026, 11, 26, 11, 0, 0), SessionClosed},
		{"holiday after-hours", ny(t, 2026, 11, 26, 17, 0, 0), SessionClosed},
		{"day after holiday", ny(t, 2026, 11, 27, 9, 30, 0), SessionRegular},
		{"EST winter open", ny(t, 2026, 12, 2, 9, 30, 0), SessionRegular},
		{"EST winter pre-open", ny(t, 2026, 12, 2, 9, 29, 0), SessionPreMarket},
		{"first trading day after DST ends", ny(t, 2026, 11, 2, 9, 30, 0), SessionRegular},
		{"first trading day after DST starts", ny(t, 2026, 3, 9, 9, 30, 0), SessionRegular},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, c.SessionAt(tc.at))
		})
	}
	// The same UTC instant is a different session in summer and in winter:
	// 13:30 UTC is 09:30 EDT but 08:30 EST.
	assert.Equal(t, SessionRegular, c.SessionAt(time.Date(2026, 10, 7, 13, 30, 0, 0, time.UTC)))
	assert.Equal(t, SessionPreMarket, c.SessionAt(time.Date(2026, 12, 2, 13, 30, 0, 0, time.UTC)))
}

// TestCalendar_OrderWindowRefusesMarketOrdersOutsideRegularHours: a market
// order is eligible only in the regular session, a limit order in the
// regular session or, with extended hours, pre-market and after-hours too;
// nothing is eligible on a closed day. Expiry is the end of the order's last
// session on its trade date.
func TestCalendar_OrderWindowRefusesMarketOrdersOutsideRegularHours(t *testing.T) {
	c := testCalendar(t)
	pre := ny(t, 2026, 10, 7, 8, 0, 0)
	regular := ny(t, 2026, 10, 7, 10, 0, 0)
	after := ny(t, 2026, 10, 7, 17, 0, 0)
	closed := ny(t, 2026, 10, 7, 21, 0, 0)
	weekend := ny(t, 2026, 10, 10, 10, 0, 0)
	holiday := ny(t, 2026, 11, 26, 10, 0, 0)
	cases := []struct {
		name     string
		at       time.Time
		typ      OrderType
		extended bool
		eligible bool
	}{
		{"market regular", regular, OrderMarket, false, true},
		{"market pre-market", pre, OrderMarket, false, false},
		{"market after-hours", after, OrderMarket, false, false},
		{"market closed", closed, OrderMarket, false, false},
		{"market weekend", weekend, OrderMarket, false, false},
		{"market holiday", holiday, OrderMarket, false, false},
		{"market claiming extended hours", regular, OrderMarket, true, false},
		{"limit regular", regular, OrderLimit, false, true},
		{"limit pre-market without extended", pre, OrderLimit, false, false},
		{"limit after-hours without extended", after, OrderLimit, false, false},
		{"limit extended pre-market", pre, OrderLimit, true, true},
		{"limit extended regular", regular, OrderLimit, true, true},
		{"limit extended after-hours", after, OrderLimit, true, true},
		{"limit extended closed", closed, OrderLimit, true, false},
		{"limit extended weekend", weekend, OrderLimit, true, false},
		{"limit extended holiday", holiday, OrderLimit, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok, _ := c.OrderWindow(tc.at, tc.typ, tc.extended)
			assert.Equal(t, tc.eligible, ok)
		})
	}
	_, _, exp := c.OrderWindow(regular, OrderLimit, false)
	assert.Equal(t, ny(t, 2026, 10, 7, 16, 0, 0), exp)
	_, _, exp = c.OrderWindow(pre, OrderLimit, true)
	assert.Equal(t, ny(t, 2026, 10, 7, 20, 0, 0), exp)
}

// TestCalendar_SettlementIsTPlusOneTradingDay: weekends and holidays are
// skipped, and nothing settles from a day nothing trades on.
func TestCalendar_SettlementIsTPlusOneTradingDay(t *testing.T) {
	c := testCalendar(t)
	for _, tc := range []struct{ trade, settle Date }{
		{NewDate(2026, 10, 7), NewDate(2026, 10, 8)},   // Wed -> Thu
		{NewDate(2026, 10, 9), NewDate(2026, 10, 12)},  // Fri -> Mon
		{NewDate(2026, 11, 25), NewDate(2026, 11, 27)}, // Wed -> Fri over Thanksgiving
		{NewDate(2026, 12, 24), NewDate(2026, 12, 28)}, // Thu -> Mon over Christmas and a weekend
		{NewDate(2026, 12, 31), NewDate(2027, 1, 4)},   // Thu -> Mon over New Year
	} {
		got, err := c.SettlementDate(tc.trade)
		require.NoError(t, err)
		assert.Equal(t, tc.settle, got, "trade %s", tc.trade)
	}
	_, err := c.SettlementDate(thanksgiving2026)
	require.Error(t, err)
	_, err = c.SettlementDate(NewDate(2026, 10, 10))
	require.Error(t, err)
}

// TestCalendar_ExDateRule: under T+1 the ex-date is the record date when it
// trades, the last trading day before it otherwise, and anything else is
// refused.
func TestCalendar_ExDateRule(t *testing.T) {
	c := testCalendar(t)
	for _, tc := range []struct{ record, ex Date }{
		{NewDate(2026, 10, 14), NewDate(2026, 10, 14)}, // Wed record, Wed ex
		{NewDate(2026, 10, 12), NewDate(2026, 10, 12)}, // Mon record, Mon ex
		{NewDate(2026, 10, 10), NewDate(2026, 10, 9)},  // Sat record, Fri ex
		{NewDate(2026, 11, 26), NewDate(2026, 11, 25)}, // holiday record, Wed ex
	} {
		got, err := c.ExDateFor(tc.record)
		require.NoError(t, err)
		assert.Equal(t, tc.ex, got, "record %s", tc.record)
		require.NoError(t, c.ValidateExDate(tc.ex, tc.record))
		err = c.ValidateExDate(tc.ex.AddDays(-1), tc.record)
		require.Error(t, err)
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	}
}

func TestCalendar_RefusesUnknownCalendarsAndBadHolidays(t *testing.T) {
	_, err := NewCalendar("XLON", nil)
	require.Error(t, err)
	assert.Equal(t, errs.CodeUnsupported, errs.CodeOf(err))
	_, err = NewCalendar(CalendarXNYS, []Holiday{{Date: thanksgiving2026, Name: "a"}, {Date: thanksgiving2026, Name: "b"}})
	require.Error(t, err)
	_, err = NewCalendar(CalendarXNYS, []Holiday{{Date: thanksgiving2026}})
	require.Error(t, err)
	_, err = NewCalendar(CalendarXNYS, []Holiday{{Name: "no date"}})
	require.Error(t, err)
}

func TestDate_RoundTrips(t *testing.T) {
	d := NewDate(2026, 10, 7)
	assert.Equal(t, "2026-10-07", d.String())
	p, err := ParseDate("2026-10-07")
	require.NoError(t, err)
	assert.True(t, p.Equal(d))
	b, err := d.MarshalJSON()
	require.NoError(t, err)
	var back Date
	require.NoError(t, back.UnmarshalJSON(b))
	assert.True(t, back.Equal(d))
	for _, bad := range []string{"", "2026-10-7", "2026/10/07", "2026-13-01", "20261007", " 2026-10-07"} {
		_, err := ParseDate(bad)
		assert.Error(t, err, bad)
	}
	assert.Equal(t, d, DateOf(time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)))
}

// TestProp_SettlementDateIsTheNextTradingDay: for every trading day the
// settlement date is a later trading day with no trading day in between.
func TestProp_SettlementDateIsTheNextTradingDay(t *testing.T) {
	c := testCalendar(t)
	rapid.Check(t, func(rt *rapid.T) {
		d := NewDate(2026, 1, 1).AddDays(rapid.IntRange(0, 730).Draw(rt, "offset"))
		if !c.IsTradingDay(d) {
			if _, err := c.SettlementDate(d); err == nil {
				rt.Fatalf("%s does not trade and must not settle", d)
			}
			return
		}
		s, err := c.SettlementDate(d)
		if err != nil {
			rt.Fatalf("settle %s: %v", d, err)
		}
		if !s.After(d) || !c.IsTradingDay(s) {
			rt.Fatalf("%s settles on %s", d, s)
		}
		for x := d.AddDays(1); x.Before(s); x = x.AddDays(1) {
			if c.IsTradingDay(x) {
				rt.Fatalf("%s settles on %s, skipping trading day %s", d, s, x)
			}
		}
		ex, err := c.ExDateFor(d)
		if err != nil || !ex.Equal(d) {
			rt.Fatalf("under T+1 a trading-day record date %s is its own ex-date, got %s (%v)", d, ex, err)
		}
	})
}

// TestProp_SessionsPartitionTheTradingDay: within a trading day the session
// only ever moves forward CLOSED -> PRE -> REGULAR -> AFTER -> CLOSED, and on
// a closed day it never leaves CLOSED.
func TestProp_SessionsPartitionTheTradingDay(t *testing.T) {
	c := testCalendar(t)
	order := map[Session]int{SessionPreMarket: 1, SessionRegular: 2, SessionAfterHours: 3}
	rapid.Check(t, func(rt *rapid.T) {
		d := NewDate(2026, 1, 1).AddDays(rapid.IntRange(0, 730).Draw(rt, "offset"))
		start := c.StartOfDay(d)
		prevRank, seenClose := 0, false
		for m := 0; m < 24*60; m += 7 {
			s := c.SessionAt(start.Add(time.Duration(m) * time.Minute))
			if !c.IsTradingDay(d) {
				if s != SessionClosed {
					rt.Fatalf("%s is closed but %d minutes in is %s", d, m, s)
				}
				continue
			}
			r, open := order[s]
			switch {
			case !open && prevRank > 0:
				seenClose = true
			case open && seenClose:
				rt.Fatalf("%s reopened after the close at minute %d", d, m)
			case open && r < prevRank:
				rt.Fatalf("%s went back from rank %d to %s at minute %d", d, prevRank, s, m)
			case open:
				prevRank = r
			}
		}
	})
}

func FuzzParseDate(f *testing.F) {
	for _, s := range []string{"2026-10-07", "", "2026-02-30", "9999-12-31", "0000-01-01", "2026-10-07T00:00:00Z"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		d, err := ParseDate(s)
		if err != nil {
			return
		}
		again, err := ParseDate(d.String())
		if err != nil || !again.Equal(d) {
			t.Fatalf("%q parsed to %s which does not round-trip", s, d)
		}
	})
}
