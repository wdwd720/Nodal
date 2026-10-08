package httpapi

import (
	"context"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/feeledger"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// Fixed identifiers of the fee-ledger handler tests.
var (
	feeSchedID   = mustTrID[feeledger.ScheduleID]("0193b2e0-0000-7000-8000-0000000000b1")
	feeQuoteID   = mustTrID[feeledger.DisclosureID]("0193b2e0-0000-7000-8000-0000000000b2")
	feeUnbound   = mustTrID[feeledger.DisclosureID]("0193b2e0-0000-7000-8000-0000000000b3")
	feeBookID    = mustTrID[feeledger.BookingID]("0193b2e0-0000-7000-8000-0000000000b4")
	feePeriodFix = mustTrID[feeledger.PeriodID]("0193b2e0-0000-7000-8000-0000000000b5")
	feeFlowID    = mustTrID[feeledger.FlowID]("0193b2e0-0000-7000-8000-0000000000b6")
	feeLineID    = mustTrID[feeledger.LineID]("0193b2e0-0000-7000-8000-0000000000b7")
	feeAbsent    = "0193b2e0-0000-7000-8000-0000000000bf"
)

func feeMoney(s string) money.USD {
	u, err := money.ParseUSD(s)
	if err != nil {
		panic(err)
	}
	return u
}

// feeTestSchedule is the schedule the fee tests publish and price under:
// 20 bps of spread, a 0.50 fixed fee, 10 bps of platform fee, 5 bps of
// venue pass-through and a 0.10 network estimate in the quote asset.
func feeTestSchedule() fees.Schedule {
	s := fees.ZeroSchedule(trUSD, 6)
	s.Version = "fees-v2"
	s.SpreadBPS, s.PercentageFeeBPS, s.VenuePassThroughBPS = 20, 10, 5
	s.FixedFee = trQty(500_000)
	s.NetworkFee = fees.NetworkFeeEstimate{Asset: trUSD, Amount: trQty(100_000)}
	return s
}

// feeTestStatement is a real crystallized statement of the fee engine: a
// period opened at 10,000.00 with its high-water mark there, valued at
// 11,000.00 half way and closed at 11,000.00 under a 20% fee.
func feeTestStatement(t *testing.T) (fees.PerformanceTerms, fees.PerformanceStatement) {
	t.Helper()
	terms := fees.PerformanceTerms{AccountRef: feeledger.AccountRef(testAccountID, "strat-a"), TermsVersion: "perf-v1",
		PeriodStart: testNow.Add(-30 * 24 * time.Hour), PeriodEnd: testNow, OpeningNAV: feeMoney("10000.00"), PriorHWM: feeMoney("10000.00"),
		FeeRateBPS: 2_000, Crystallization: fees.CrystallizeAtPeriodEnd}
	p, err := fees.OpenPerformancePeriod(terms)
	require.NoError(t, err)
	_, err = p.Accrue(fees.NAVSnapshot{At: testNow.Add(-15 * 24 * time.Hour), NAV: feeMoney("11000.00")})
	require.NoError(t, err)
	st, err := p.Crystallize(feeMoney("11000.00"), nil)
	require.NoError(t, err)
	return terms, st
}

// fakeFees is the fee-ledger port; it counts every call.
type fakeFees struct {
	stubErr
	mu       sync.Mutex
	calls    map[string]int
	last     map[string]any
	version  FeeScheduleVersionView
	quote    feeledger.StoredQuote
	booking  feeledger.Booking
	period   feeledger.Period
	preview  FeePreviewView
	verified *FeeVerification
}

func (f *fakeFees) hit(name string, arg any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[name]++
	f.last[name] = arg
}

func (f *fakeFees) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *fakeFees) arg(name string) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last[name]
}

func (f *fakeFees) Schedules(_ context.Context, cursor string, limit int) (FeeSchedulePage, error) {
	f.hit("Schedules", []any{cursor, limit})
	v := f.version
	return FeeSchedulePage{Items: []FeeScheduleSummary{{Key: "orders:paper", Latest: 3, Count: 3, Effective: &v},
		{Key: "transfers:later", Latest: 1, Count: 1}}, NextCursor: "next-keys"}, f.fail()
}

func (f *fakeFees) ScheduleHistory(_ context.Context, key string) (FeeScheduleHistoryView, error) {
	f.hit("ScheduleHistory", key)
	if key != "orders:paper" {
		return FeeScheduleHistoryView{}, errs.New(errs.CodeNotFound, "no fee schedule is published under this key")
	}
	older := f.version
	older.InForce, older.Schedule.Version = false, 1
	return FeeScheduleHistoryView{Key: key, Versions: []FeeScheduleVersionView{older, f.version}, EffectiveVersion: 2}, f.fail()
}

func (f *fakeFees) PreviewQuote(_ context.Context, in FeeQuoteInput) (FeePreviewView, error) {
	f.hit("PreviewQuote", in)
	if in.ScheduleKey != "orders:paper" {
		return FeePreviewView{}, errs.New(errs.CodeNotFound, "feeledger: no version of this schedule is in force at that instant").
			WithField("reason", feeledger.ReasonScheduleNotFound)
	}
	return f.preview, f.fail()
}

func (f *fakeFees) Quotes(_ context.Context, acct accounts.AccountID, cursor string, limit int) (FeeQuotePage, error) {
	f.hit("Quotes", acct)
	return FeeQuotePage{Items: []FeeQuoteView{{Quote: f.quote}}}, f.fail()
}

func (f *fakeFees) Quote(_ context.Context, did feeledger.DisclosureID) (FeeQuoteView, error) {
	f.hit("Quote", did)
	switch did {
	case feeQuoteID:
		return FeeQuoteView{Quote: f.quote, Verification: f.verified}, f.fail()
	case feeUnbound:
		q := f.quote
		q.ID, q.AccountID = feeUnbound, nil
		return FeeQuoteView{Quote: q, Verification: f.verified}, f.fail()
	}
	return FeeQuoteView{}, errs.New(errs.CodeNotFound, "feeledger: quote disclosure not found")
}

func (f *fakeFees) Bookings(_ context.Context, acct accounts.AccountID, cursor string, limit int) (FeeBookingPage, error) {
	f.hit("Bookings", acct)
	return FeeBookingPage{Items: []FeeBookingView{{Booking: f.booking, Quote: f.quote}}, NextCursor: "next-bookings"}, f.fail()
}

func (f *fakeFees) Booking(_ context.Context, bid feeledger.BookingID) (FeeBookingView, error) {
	f.hit("Booking", bid)
	if bid != feeBookID {
		return FeeBookingView{}, errs.New(errs.CodeNotFound, "feeledger: booking not found")
	}
	return FeeBookingView{Booking: f.booking, Quote: f.quote, Verification: f.verified}, f.fail()
}

func (f *fakeFees) Periods(_ context.Context, acct accounts.AccountID, strategy, cursor string, limit int) (FeePeriodPage, error) {
	f.hit("Periods", []any{acct, strategy})
	return FeePeriodPage{Items: []FeePeriodView{{Period: f.period}}}, f.fail()
}

func (f *fakeFees) Period(_ context.Context, pid feeledger.PeriodID) (FeePeriodView, error) {
	f.hit("Period", pid)
	if pid != feePeriodFix {
		return FeePeriodView{}, errs.New(errs.CodeNotFound, "feeledger: performance period not found")
	}
	return FeePeriodView{Period: f.period, Verification: f.verified}, f.fail()
}

func (f *fakeFees) Publish(_ context.Context, r feeledger.PublishRequest) (FeeScheduleVersionView, error) {
	f.hit("Publish", r)
	return f.version, f.fail()
}

func (f *fakeFees) RecordQuote(_ context.Context, r FeeQuoteCommand) (FeeQuoteView, error) {
	f.hit("RecordQuote", r)
	return FeeQuoteView{Quote: f.quote}, f.fail()
}

func (f *fakeFees) ReverseBooking(_ context.Context, r feeledger.ReverseRequest) (FeeBookingView, error) {
	f.hit("ReverseBooking", r)
	b := f.booking
	at, journal := testNow.Add(time.Hour), ledger.NewTransactionID()
	b.Status, b.ReversalReasonCode, b.ReversedAt, b.ReversalJournalID = feeledger.BookingReversed, r.ReasonCode, &at, &journal
	return FeeBookingView{Booking: b, Quote: f.quote}, f.fail()
}

func (f *fakeFees) SpreadAudit(_ context.Context, filter feeledger.SpreadFilter) ([]feeledger.SpreadCharge, error) {
	f.hit("SpreadAudit", filter)
	d := f.quote.Disclosure
	return []feeledger.SpreadCharge{{BookingID: feeBookID, DisclosureID: feeQuoteID, AccountID: testAccountID, ScheduleKey: "orders:paper",
		ScheduleVersion: 2, ScheduleHash: d.ScheduleHash, DisclosureHash: "c0de", Side: fees.SideBuy, BaseQuantity: d.Input.BaseQty,
		ReferencePrice: d.ReferencePrice.Price(), ExecutionPrice: d.ExecutionPrice.Price(), SpreadBPS: d.Spread.BPS, SpreadAmount: d.Spread.Amount,
		QuoteAsset: trUSD, BookingMode: feeledger.BookingPosted, Status: feeledger.BookingBooked, JournalTransactionID: trJournal,
		BookedAt: testNow}}, f.fail()
}

func (f *fakeFees) SpreadTotals(_ context.Context, quoteAsset assets.AssetID) (FeeSpreadTotalsView, error) {
	f.hit("SpreadTotals", quoteAsset)
	return FeeSpreadTotalsView{QuoteAsset: quoteAsset, Booked: trQty(200_000), Reversed: trQty(0)}, f.fail()
}

func (f *fakeFees) OpenPeriod(_ context.Context, r feeledger.OpenPeriodRequest) (FeePeriodView, error) {
	f.hit("OpenPeriod", r)
	return FeePeriodView{Period: f.period}, f.fail()
}

func (f *fakeFees) RecordFlow(_ context.Context, r feeledger.FlowRequest) (feeledger.Flow, error) {
	f.hit("RecordFlow", r)
	return feeledger.Flow{ID: feeFlowID, PeriodID: r.PeriodID, Seq: 1, At: r.At, Amount: r.Amount, NAVBefore: r.NAVBefore, Actor: trOp,
		IdempotencyKey: r.IdempotencyKey}, f.fail()
}

func (f *fakeFees) Accrue(_ context.Context, r feeledger.AccrueRequest) (feeledger.Line, error) {
	f.hit("Accrue", r)
	return f.period.Lines[0], f.fail()
}

func (f *fakeFees) Crystallize(_ context.Context, r feeledger.CrystallizeRequest) (FeePeriodView, error) {
	f.hit("Crystallize", r)
	return FeePeriodView{Period: f.period}, f.fail()
}

func (f *fakeFees) CancelPeriod(_ context.Context, pid feeledger.PeriodID, reason string, actor econ.Actor) (FeePeriodView, error) {
	f.hit("CancelPeriod", []any{pid, reason, actor})
	p := f.period
	p.Status = feeledger.PeriodCancelled
	return FeePeriodView{Period: p}, f.fail()
}

func (f *fakeFees) ReverseCrystallization(_ context.Context, r feeledger.ReversePeriodRequest) (FeePeriodView, error) {
	f.hit("ReverseCrystallization", r)
	p := f.period
	p.Status, p.ReversalReasonCode = feeledger.PeriodReversed, r.ReasonCode
	return FeePeriodView{Period: p}, f.fail()
}

// newFakeFees holds a published schedule in force, a quote bound to
// testAccountID priced under it by the real fee engine, its booking, and a
// crystallized performance period with a real statement.
func newFakeFees(t *testing.T) *fakeFees {
	t.Helper()
	s := feeTestSchedule()
	canonical, err := s.Canonical()
	require.NoError(t, err)
	hash, err := s.Hash()
	require.NoError(t, err)
	published := feeledger.PublishedSchedule{ID: feeSchedID, Key: "orders:paper", Version: 2, Label: s.Version, Schedule: s,
		Canonical: canonical, Hash: hash, QuoteAsset: trUSD, EffectiveFrom: testNow, PublishedAt: testNow, Actor: trOp, Reason: "fees v2"}
	ref, err := money.PriceFromDecimalString("100.00", trUSD.String(), "test-oracle", testNow)
	require.NoError(t, err)
	in := fees.QuoteInput{Side: fees.SideBuy, BaseQty: trQty(10_000_000), BaseDecimals: 6, QuoteDecimals: 6, ReferencePrice: ref}
	q, err := s.Quote(in)
	require.NoError(t, err)
	d := q.Disclosure()
	dh, err := d.Hash()
	require.NoError(t, err)
	acct, base := testAccountID, trBase
	stored := feeledger.StoredQuote{ID: feeQuoteID, ScheduleID: feeSchedID, ScheduleKey: "orders:paper", ScheduleVersion: 2, ScheduleHash: hash,
		Subsystem: econ.SubsystemOrders, IdempotencyKey: "quote-key-0001", AccountID: &acct, BaseAsset: &base, Side: fees.SideBuy,
		Disclosure: d, DisclosureHash: dh, Charges: feeledger.ChargesOf(d), QuotedAt: testNow, ValidUntil: testNow.Add(2 * time.Minute)}
	terms, st := feeTestStatement(t)
	journal, effect := ledger.NewTransactionID(), econ.NewEffectID()
	crystallizedAt := testNow
	gross, net := money.BPS(1_000), money.BPS(800)
	period := feeledger.Period{ID: feePeriodFix, AccountID: testAccountID, StrategyRef: "strat-a", Seq: 1, Mode: gates.ModePaper,
		FeeAsset: trUSD, Terms: terms, TermsHash: "7e11", Status: feeledger.PeriodCrystallized, Accrued: feeMoney("0.00"),
		Lines: []feeledger.Line{{ID: feeLineID, PeriodID: feePeriodFix, Seq: 1, Kind: fees.LineAccrual, At: st.Lines[0].At, NAV: st.Lines[0].NAV,
			HWMAdjusted: st.Lines[0].HWMAdjusted, HurdleThreshold: st.Lines[0].HurdleThreshold, FeeBase: st.Lines[0].FeeBase,
			Amount: st.Lines[0].Amount, AccrualBefore: st.Lines[0].AccrualBefore, AccrualAfter: st.Lines[0].AccrualAfter,
			JournalTransactionID: &journal, EffectID: effect}},
		Statement: &st, StatementHash: "57a7", CrystallizationJournalID: &journal, CrystallizationEffectID: &effect,
		CrystallizedAt: &crystallizedAt, GrossReturnBPS: &gross, NetReturnBPS: &net}
	return &fakeFees{
		calls: map[string]int{}, last: map[string]any{},
		version: FeeScheduleVersionView{Schedule: published, QuoteAsset: TreasuryAsset{ID: trUSD, Symbol: "SBX-USD", Decimals: 6}, InForce: true},
		quote:   stored,
		booking: feeledger.Booking{ID: feeBookID, DisclosureID: feeQuoteID, AccountID: testAccountID, Mode: gates.ModePaper,
			BookingMode: feeledger.BookingPosted, Kind: ledger.KindSimTrade, JournalTransactionID: trJournal, EffectID: effect,
			Charges: stored.Charges, Status: feeledger.BookingBooked, EvidenceRef: "evidence://fees/charge", BookedAt: testNow},
		period:   period,
		preview:  FeePreviewView{Schedule: published, Disclosure: d, DisclosureHash: dh, Charges: feeledger.ChargesOf(d)},
		verified: &FeeVerification{Verified: true},
	}
}

func newFeesHarness(t *testing.T) (*harness, *fakeFees) {
	t.Helper()
	h := newHarness(t)
	ff := newFakeFees(t)
	ports := h.ports.ports()
	ports.Fees = ff
	h.server.opts.Ports = ports
	return h, ff
}

func feePreviewPath(key string) string {
	v := url.Values{}
	v.Set("schedule_key", key)
	v.Set("side", "BUY")
	v.Set("base_quantity", "10000000")
	v.Set("base_decimals", "6")
	v.Set("reference_price", "100.00")
	return "/v1/advanced/fees/quote-preview?" + v.Encode()
}

type feeMutation struct {
	name    string
	path    string
	body    any
	altBody any
	counter string
	stepUp  bool
}

func feeMutations() []feeMutation {
	period := "/v1/advanced/fees/periods/" + feePeriodFix.String()
	schedule := func(label string) map[string]any {
		return map[string]any{"label": label, "quote_asset_id": trUSD.String(), "quote_decimals": 6, "spread_bps": 20, "fixed_fee": "500000",
			"percentage_fee_bps": 10, "venue_pass_through_bps": 5, "network_fee_asset_id": trUSD.String(), "network_fee_amount": "100000",
			"slippage_reserve_bps": 0, "subscription_discount_bps": 0, "tiers": []any{}, "strategy_fee_bps": 0, "max_total_fee_bps": 0,
			"max_total_fee": "0", "min_fee": "0", "rounding": map[string]any{"buy_notional": "down", "sell_notional": "up", "spread": "down",
				"percentage_fee": "down", "venue_pass_through": "down", "strategy_fee": "down", "fee_cap": "down", "slippage_reserve": "up",
				"allow_platform_favourable": false}}
	}
	quote := func(price string) map[string]any {
		return map[string]any{"schedule_key": "orders:paper", "side": "BUY", "base_quantity": "10000000", "base_decimals": 6,
			"reference_price": price, "subsystem": "ORDERS", "mode": "PAPER", "account_id": testAccountID.String()}
	}
	return []feeMutation{
		{name: "publish", path: "/v1/advanced/fees/schedules", counter: "Publish", stepUp: true,
			body:    map[string]any{"key": "orders:paper", "schedule": schedule("fees-v2"), "reason": "fees v2"},
			altBody: map[string]any{"key": "orders:paper", "schedule": schedule("fees-v3"), "reason": "fees v3"}},
		{name: "record-quote", path: "/v1/advanced/fees/quotes", counter: "RecordQuote", body: quote("100.00"), altBody: quote("100.01")},
		{name: "reverse-booking", path: "/v1/advanced/fees/bookings/" + feeBookID.String() + "/reverse", counter: "ReverseBooking", stepUp: true,
			body:    map[string]any{"reason_code": "CUSTOMER_GOODWILL", "reason": "goodwill"},
			altBody: map[string]any{"reason_code": "CHARGED_IN_ERROR", "reason": "error"}},
		{name: "open-period", path: "/v1/advanced/fees/periods", counter: "OpenPeriod",
			body: map[string]any{"account_id": testAccountID.String(), "strategy_ref": "strat-a", "terms_version": "perf-v1",
				"period_start": "2026-08-07T12:00:00Z", "period_end": "2026-09-06T12:00:00Z", "opening_nav": "10000.00",
				"hurdle_annual_bps": 0, "fee_rate_bps": 2000},
			altBody: map[string]any{"account_id": testAccountID.String(), "strategy_ref": "strat-a", "terms_version": "perf-v1",
				"period_start": "2026-08-07T12:00:00Z", "period_end": "2026-09-06T12:00:00Z", "opening_nav": "10000.00",
				"hurdle_annual_bps": 0, "fee_rate_bps": 2500}},
		{name: "flow", path: period + "/flows", counter: "RecordFlow",
			body:    map[string]any{"at": "2026-08-20T12:00:00Z", "amount": "500.00", "nav_before": "10200.00"},
			altBody: map[string]any{"at": "2026-08-20T12:00:00Z", "amount": "-500.00", "nav_before": "10200.00"}},
		{name: "accrue", path: period + "/accruals", counter: "Accrue",
			body:    map[string]any{"at": "2026-08-22T12:00:00Z", "nav": "11000.00"},
			altBody: map[string]any{"at": "2026-08-22T12:00:00Z", "nav": "11000.01"}},
		{name: "crystallize", path: period + "/crystallize", counter: "Crystallize", stepUp: true,
			body:    map[string]any{"closing_nav": "11000.00"},
			altBody: map[string]any{"closing_nav": "11000.00", "contributions": []any{map[string]any{"strategy": "strat-a", "gross_pnl": "1000.00"}}}},
		{name: "cancel", path: period + "/cancel", counter: "CancelPeriod",
			body: map[string]any{"reason": "terms changed"}, altBody: map[string]any{"reason": "terms changed again"}},
		{name: "reverse-period", path: period + "/reverse", counter: "ReverseCrystallization", stepUp: true,
			body:    map[string]any{"reason_code": "NAV_RESTATED", "reason": "the closing NAV was restated"},
			altBody: map[string]any{"reason_code": "NAV_RESTATED", "reason": "restated twice"}},
	}
}

// --- transparency: what any signed-in customer reads -------------------------------------

func TestFeeSchedulesAndCalculator_ArePublicToCustomers(t *testing.T) {
	t.Parallel()
	h, ff := newFeesHarness(t)
	res := h.do(http.MethodGet, "/v1/advanced/fees/schedules", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw := res.raw()
	assert.Equal(t, "SIMULATED", raw["value_domain"])
	assert.Equal(t, "next-keys", raw["next_cursor"])
	items := raw["items"].([]any)
	first := items[0].(map[string]any)
	assert.Equal(t, float64(3), first["latest_version"])
	eff := first["effective"].(map[string]any)
	assert.Equal(t, true, eff["in_force"])
	sched := eff["schedule"].(map[string]any)
	assert.Equal(t, "500000", sched["fixed_fee"])
	assert.Equal(t, float64(20), sched["spread_bps"])
	assert.Equal(t, "fees-v2", sched["label"])
	assert.Equal(t, "down", sched["rounding"].(map[string]any)["buy_notional"], "the customer-favourable default")
	assert.Nil(t, items[1].(map[string]any)["effective"], "a key whose versions all take effect later has none in force")

	res = h.do(http.MethodGet, "/v1/advanced/fees/schedules/history?key=orders:paper", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw = res.raw()
	assert.Equal(t, float64(2), raw["effective_version"])
	versions := raw["versions"].([]any)
	require.Len(t, versions, 2)
	assert.Equal(t, false, versions[0].(map[string]any)["in_force"])
	assert.Equal(t, true, versions[1].(map[string]any)["in_force"])

	res = h.do(http.MethodGet, "/v1/advanced/fees/schedules/history?key=unknown:key", nil)
	require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())

	res = h.do(http.MethodGet, feePreviewPath("orders:paper"), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw = res.raw()
	assert.Equal(t, false, raw["recorded"])
	d := raw["disclosure"].(map[string]any)
	assert.Equal(t, "1000000000", d["notional_at_reference"], "10 units at 100.00")
	assert.Equal(t, "2000000", d["spread"].(map[string]any)["amount"], "20 bps of 1,000.00 is exactly 2.00")
	assert.Equal(t, "2000000", raw["charges"].(map[string]any)["spread"])
	assert.Equal(t, "1500000", raw["charges"].(map[string]any)["platform"], "a 0.50 fixed fee plus 10 bps of 1,000.00")
	assert.Equal(t, "500000", raw["charges"].(map[string]any)["venue"])
	assert.Equal(t, "1004100000", d["total_debit"], "notional + spread + fees + network, exactly")
	in := ff.arg("PreviewQuote").(FeeQuoteInput)
	assert.Equal(t, "100.00", in.ReferencePrice)
	assert.Equal(t, uint8(6), in.BaseDecimals)

	res = h.do(http.MethodGet, feePreviewPath("orders:unknown"), nil)
	require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, feeledger.ReasonScheduleNotFound, res.problem().Fields["reason"])
}

// An account's quotes, bookings and periods are its owner's, or an
// operator's who may read any account.
func TestFeeAccountReads_AreScopedToTheCallersAccount(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ path, counter string }{
		{"/v1/advanced/fees/quotes", "Quotes"}, {"/v1/advanced/fees/bookings", "Bookings"}, {"/v1/advanced/fees/periods", "Periods"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			h, ff := newFeesHarness(t)
			res := h.do(http.MethodGet, tc.path+"?account_id="+testAccountID.String(), nil)
			require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, "SIMULATED", res.raw()["value_domain"])
			assert.Equal(t, 1, ff.count(tc.counter))

			res = h.do(http.MethodGet, tc.path+"?account_id="+testOtherAcct.String(), nil)
			require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, errs.CodeForbidden, res.problem().Code)
			assert.Equal(t, 1, ff.count(tc.counter), "a foreign account never reaches the fee ledger")

			op := operatorPrincipal()
			h.as(&op)
			res = h.do(http.MethodGet, tc.path+"?account_id="+testOtherAcct.String(), nil)
			require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, 2, ff.count(tc.counter))
		})
	}
}

// A record read by its own id is scoped by the account stored on it, and a
// stranger's record answers exactly what an absent one answers.
func TestFeeRecordsByID_AreScopedByTheirOwnAccountAndMaskedAsNotFound(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ path, absent string }{
		{"/v1/advanced/fees/quotes/" + feeQuoteID.String(), "/v1/advanced/fees/quotes/" + feeAbsent},
		{"/v1/advanced/fees/bookings/" + feeBookID.String(), "/v1/advanced/fees/bookings/" + feeAbsent},
		{"/v1/advanced/fees/periods/" + feePeriodFix.String(), "/v1/advanced/fees/periods/" + feeAbsent},
	} {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			h, _ := newFeesHarness(t)
			res := h.do(http.MethodGet, tc.path, nil)
			require.Equal(t, http.StatusOK, res.Code, "the owner reads it; body=%s", res.Body.String())
			assert.Equal(t, "SIMULATED", res.raw()["value_domain"])

			stranger := strangerPrincipal()
			h.as(&stranger)
			foreign := h.do(http.MethodGet, tc.path, nil)
			require.Equal(t, http.StatusNotFound, foreign.Code, "body=%s", foreign.Body.String())
			fp := foreign.problem()
			absent := h.do(http.MethodGet, tc.absent, nil)
			require.Equal(t, http.StatusNotFound, absent.Code, "body=%s", absent.Body.String())
			ap := absent.problem()
			assert.Equal(t, fp.Code, ap.Code)
			assert.Equal(t, fp.Detail, ap.Detail, "a foreign and an absent record must read alike")
			assert.Equal(t, len(fp.Fields), len(ap.Fields))

			op := operatorPrincipal()
			h.as(&op)
			require.Equal(t, http.StatusOK, h.do(http.MethodGet, tc.path, nil).Code)
		})
	}
}

// A disclosure bound to no account is an operator's to read; a customer is
// told it does not exist.
func TestFeeUnboundQuote_IsAnOperatorsRead(t *testing.T) {
	t.Parallel()
	h, _ := newFeesHarness(t)
	path := "/v1/advanced/fees/quotes/" + feeUnbound.String()
	res := h.do(http.MethodGet, path, nil)
	require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
	op := operatorPrincipal()
	h.as(&op)
	res = h.do(http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw := res.raw()
	assert.Nil(t, raw["account_id"])
	assert.Equal(t, true, raw["verification"].(map[string]any)["verified"])
}

// Re-proving a record reports what it found: a booking whose journal no longer
// carries its charges reads as not verified, with the stable reason.
func TestFeeVerification_ReportsAFindingWithItsReason(t *testing.T) {
	t.Parallel()
	h, ff := newFeesHarness(t)
	ff.verified = &FeeVerification{Verified: false, Code: string(errs.CodeValidationFailed), Reason: feeledger.ReasonJournalMismatch,
		Detail: "feeledger: the journal does not carry exactly the disclosed charges"}
	res := h.do(http.MethodGet, "/v1/advanced/fees/bookings/"+feeBookID.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw := res.raw()
	v := raw["verification"].(map[string]any)
	assert.Equal(t, false, v["verified"])
	assert.Equal(t, feeledger.ReasonJournalMismatch, v["reason"])
	line := raw["spread_line"].(map[string]any)
	assert.Equal(t, "2000000", line["spread_amount"])
	assert.Equal(t, float64(20), line["spread_bps"])
	assert.Equal(t, "10000", line["reference_price"].(map[string]any)["mantissa"])
	assert.Equal(t, "1004100000", raw["disclosure"].(map[string]any)["total_debit"])
}

func TestFeePeriod_RendersTheStatementExactly(t *testing.T) {
	t.Parallel()
	h, _ := newFeesHarness(t)
	res := h.do(http.MethodGet, "/v1/advanced/fees/periods/"+feePeriodFix.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw := res.raw()
	assert.Equal(t, "CRYSTALLIZED", raw["status"])
	assert.Equal(t, "10000.00", raw["terms"].(map[string]any)["prior_hwm"])
	st := raw["statement"].(map[string]any)
	assert.Equal(t, "200.00", st["fee_amount"], "20% of the 1,000.00 above the high-water mark")
	assert.Equal(t, "1000.00", st["fee_base"])
	assert.Equal(t, "10800.00", st["hwm_after"], "the next period opens at the net NAV")
	assert.Equal(t, float64(1000), raw["gross_return_bps"])
	line := raw["lines"].([]any)[0].(map[string]any)
	assert.Equal(t, "ACCRUAL", line["kind"])
	assert.Equal(t, "200.00", line["amount"])
	assert.Equal(t, true, raw["verification"].(map[string]any)["verified"])
}

// --- the operator side -------------------------------------------------------------------

func TestFeeOperatorRoutes_RefuseACustomer(t *testing.T) {
	t.Parallel()
	h, ff := newFeesHarness(t)
	for _, m := range feeMutations() {
		res := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "customer-fee-"+m.name)
		require.Equal(t, http.StatusForbidden, res.Code, "%s body=%s", m.name, res.Body.String())
		assert.Zero(t, ff.count(m.counter), m.name)
	}
	for _, path := range []string{"/v1/advanced/fees/spread-audit", "/v1/advanced/fees/spread-totals?quote_asset_id=" + trUSD.String()} {
		res := h.do(http.MethodGet, path, nil)
		require.Equal(t, http.StatusForbidden, res.Code, "%s body=%s", path, res.Body.String())
	}
}

// Publishing, reversing and crystallizing need a recent strong
// authentication; the rest of the lifecycle does not.
func TestFeeOperatorCommands_RequireStepUpToPublishChargeOrReverse(t *testing.T) {
	t.Parallel()
	h, ff := newFeesHarness(t)
	stale := operatorPrincipal()
	stale.AuthTime = testNow.Add(-2 * stepUpMaxAge)
	h.as(&stale)
	for _, m := range feeMutations() {
		res := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "stale-fee-"+m.name)
		if m.stepUp {
			require.Equal(t, http.StatusForbidden, res.Code, "%s body=%s", m.name, res.Body.String())
			assert.Equal(t, errs.CodeStepUpRequired, res.problem().Code, m.name)
			assert.Zero(t, ff.count(m.counter), m.name)
			continue
		}
		require.Equal(t, http.StatusOK, res.Code, "%s needs no step-up; body=%s", m.name, res.Body.String())
	}
}

func TestFeeOperatorCommands_ReplayAndRefuseKeyReuse(t *testing.T) {
	t.Parallel()
	for _, m := range feeMutations() {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			h, ff := newFeesHarness(t)
			op := operatorPrincipal()
			h.as(&op)
			key := "replay-fee-" + m.name + "-0"
			first := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", key)
			require.Equal(t, http.StatusOK, first.Code, "body=%s", first.Body.String())
			assert.Equal(t, "SIMULATED", first.raw()["value_domain"])
			second := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", key)
			require.Equal(t, http.StatusOK, second.Code, "body=%s", second.Body.String())
			assert.JSONEq(t, first.Body.String(), second.Body.String())
			assert.Equal(t, 1, ff.count(m.counter), "a replay never reaches the fee ledger")
			reuse := h.do(http.MethodPost, m.path, m.altBody, "Idempotency-Key", key)
			require.Equal(t, http.StatusConflict, reuse.Code, "body=%s", reuse.Body.String())
			assert.Equal(t, errs.CodeInvalidIdempotencyReuse, reuse.problem().Code)
			assert.Equal(t, 1, ff.count(m.counter))
		})
	}
}

// The handler hands the fee ledger exactly the request.
func TestFeeOperatorCommands_PassTheRequestToTheService(t *testing.T) {
	t.Parallel()
	h, ff := newFeesHarness(t)
	op := operatorPrincipal()
	h.as(&op)
	ms := map[string]feeMutation{}
	for _, m := range feeMutations() {
		ms[m.name] = m
	}
	res := h.do(http.MethodPost, ms["publish"].path, ms["publish"].body, "Idempotency-Key", "pass-publish-01")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	pub := ff.arg("Publish").(feeledger.PublishRequest)
	want, err := feeTestSchedule().Canonical()
	require.NoError(t, err)
	got, err := pub.Schedule.Canonical()
	require.NoError(t, err)
	assert.Equal(t, string(want), string(got), "the schedule arrives exactly as the engine's own")
	assert.True(t, pub.EffectiveFrom.IsZero(), "absent means now, the fee ledger's default")
	assert.Equal(t, security.ActorOperator, pub.Actor.Type)

	res = h.do(http.MethodPost, ms["record-quote"].path, ms["record-quote"].body, "Idempotency-Key", "pass-quote-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	cmd := ff.arg("RecordQuote").(FeeQuoteCommand)
	assert.Equal(t, econ.SubsystemOrders, cmd.Subsystem)
	assert.Equal(t, testAccountID, *cmd.AccountID)
	assert.Equal(t, "pass-quote-0001", cmd.IdempotencyKey)
	assert.Equal(t, "10000000", cmd.Input.BaseQty.String())

	res = h.do(http.MethodPost, ms["open-period"].path, ms["open-period"].body, "Idempotency-Key", "pass-open-00001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	open := ff.arg("OpenPeriod").(feeledger.OpenPeriodRequest)
	assert.Equal(t, "10000.00", open.OpeningNAV.String())
	assert.Equal(t, money.BPS(2000), open.FeeRateBPS)
	assert.True(t, open.FeeAsset.IsZero(), "the sandbox dollar unless named")

	res = h.do(http.MethodPost, ms["crystallize"].path, ms["crystallize"].altBody, "Idempotency-Key", "pass-cryst-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	cr := ff.arg("Crystallize").(feeledger.CrystallizeRequest)
	require.Len(t, cr.Contributions, 1)
	assert.Equal(t, "1000.00", cr.Contributions[0].GrossPnL.String())

	res = h.do(http.MethodPost, ms["reverse-booking"].path, ms["reverse-booking"].body, "Idempotency-Key", "pass-reverse-01")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw := res.raw()
	assert.Equal(t, "REVERSED", raw["status"])
	assert.Equal(t, "CUSTOMER_GOODWILL", raw["reversal_reason_code"])

	res = h.do(http.MethodGet, "/v1/advanced/fees/spread-audit?quote_asset_id="+trUSD.String()+"&include_reversed=true&since=2026-09-01T00:00:00Z", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	f := ff.arg("SpreadAudit").(feeledger.SpreadFilter)
	assert.True(t, f.IncludeReversed)
	assert.Equal(t, trUSD, *f.QuoteAsset)
	assert.Equal(t, "2000000", res.raw()["items"].([]any)[0].(map[string]any)["spread_amount"])

	res = h.do(http.MethodGet, "/v1/advanced/fees/spread-totals?quote_asset_id="+trUSD.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, "200000", res.raw()["booked"])
}

// A refusal of the fee ledger keeps its code and reason.
func TestFeeRefusals_KeepTheirCodeAndReason(t *testing.T) {
	t.Parallel()
	h, ff := newFeesHarness(t)
	op := operatorPrincipal()
	h.as(&op)
	ff.err = errs.New(errs.CodeInvalidStateTransition, "feeledger: a period crystallizes at its end, not before").
		WithField("reason", feeledger.ReasonPeriodNotEnded)
	m := feeMutations()[6]
	require.Equal(t, "crystallize", m.name)
	res := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "refused-cryst-01")
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, feeledger.ReasonPeriodNotEnded, res.problem().Fields["reason"])
	ff.err = errs.New(errs.CodeValidationFailed, "feeledger: a schedule never takes effect in the past").WithField("reason", feeledger.ReasonBackdated)
	res = h.do(http.MethodPost, feeMutations()[0].path, feeMutations()[0].body, "Idempotency-Key", "refused-publish1")
	require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, feeledger.ReasonBackdated, res.problem().Fields["reason"])
}

func TestFeeRequests_AreValidatedBeforeTheService(t *testing.T) {
	t.Parallel()
	period := "/v1/advanced/fees/periods/" + feePeriodFix.String()
	badRounding := feeMutations()[0].body.(map[string]any)
	sched := map[string]any{}
	for k, v := range badRounding["schedule"].(map[string]any) {
		sched[k] = v
	}
	sched["rounding"] = map[string]any{"buy_notional": "nearest", "sell_notional": "down", "spread": "down", "percentage_fee": "down",
		"venue_pass_through": "down", "strategy_fee": "down", "fee_cap": "down", "slippage_reserve": "up", "allow_platform_favourable": false}
	fractional := map[string]any{}
	for k, v := range badRounding["schedule"].(map[string]any) {
		fractional[k] = v
	}
	fractional["fixed_fee"] = "0.50"
	quote := func(mutate func(map[string]any)) map[string]any {
		q := map[string]any{"schedule_key": "orders:paper", "side": "BUY", "base_quantity": "1", "base_decimals": 6,
			"reference_price": "100.00", "subsystem": "ORDERS", "mode": "PAPER"}
		mutate(q)
		return q
	}
	cases := []struct {
		name, method, path string
		body               any
		field              string
	}{
		{"rounding mode", http.MethodPost, "/v1/advanced/fees/schedules", map[string]any{"key": "orders:paper", "schedule": sched, "reason": "r"},
			"schedule.rounding.buy_notional"},
		{"fixed fee in dollars", http.MethodPost, "/v1/advanced/fees/schedules",
			map[string]any{"key": "orders:paper", "schedule": fractional, "reason": "r"}, "schedule.fixed_fee"},
		{"quote subsystem", http.MethodPost, "/v1/advanced/fees/quotes", quote(func(q map[string]any) { q["subsystem"] = "CASINO" }), "subsystem"},
		{"quote mode", http.MethodPost, "/v1/advanced/fees/quotes", quote(func(q map[string]any) { q["mode"] = "ON_CHAIN" }), "mode"},
		{"quote side", http.MethodPost, "/v1/advanced/fees/quotes", quote(func(q map[string]any) { q["side"] = "HOLD" }), "side"},
		{"quote price", http.MethodPost, "/v1/advanced/fees/quotes", quote(func(q map[string]any) { q["reference_price"] = "1e2" }), "reference_price"},
		{"quote decimals", http.MethodPost, "/v1/advanced/fees/quotes", quote(func(q map[string]any) { q["base_decimals"] = 39 }), "base_decimals"},
		{"quote volume", http.MethodPost, "/v1/advanced/fees/quotes", quote(func(q map[string]any) { q["trailing_volume_usd"] = "10" }),
			"trailing_volume_usd"},
		{"quote account", http.MethodPost, "/v1/advanced/fees/quotes", quote(func(q map[string]any) { q["account_id"] = nilUUIDText }), "account_id"},
		{"preview side", http.MethodGet, "/v1/advanced/fees/quote-preview?schedule_key=orders:paper&side=HOLD&base_quantity=1&base_decimals=6&reference_price=1",
			nil, "side"},
		{"preview price", http.MethodGet, "/v1/advanced/fees/quote-preview?schedule_key=orders:paper&side=BUY&base_quantity=1&base_decimals=6&reference_price=abc",
			nil, "reference_price"},
		{"preview decimals", http.MethodGet, "/v1/advanced/fees/quote-preview?schedule_key=orders:paper&side=BUY&base_quantity=1&base_decimals=40&reference_price=1",
			nil, "base_decimals"},
		{"history key", http.MethodGet, "/v1/advanced/fees/schedules/history?key=", nil, "key"},
		{"open account", http.MethodPost, "/v1/advanced/fees/periods", map[string]any{"account_id": nilUUIDText, "strategy_ref": "s",
			"terms_version": "v", "period_end": "2026-09-06T12:00:00Z", "hurdle_annual_bps": 0, "fee_rate_bps": 2000}, "account_id"},
		{"open nav", http.MethodPost, "/v1/advanced/fees/periods", map[string]any{"account_id": testAccountID.String(), "strategy_ref": "s",
			"terms_version": "v", "period_end": "2026-09-06T12:00:00Z", "opening_nav": "10000", "hurdle_annual_bps": 0, "fee_rate_bps": 2000},
			"opening_nav"},
		{"flow amount", http.MethodPost, period + "/flows", map[string]any{"at": "2026-08-20T12:00:00Z", "amount": "1.5", "nav_before": "1.00"},
			"amount"},
		{"accrual nav", http.MethodPost, period + "/accruals", map[string]any{"at": "2026-08-20T12:00:00Z", "nav": "a lot"}, "nav"},
		{"closing nav", http.MethodPost, period + "/crystallize", map[string]any{"closing_nav": "11000"}, "closing_nav"},
		{"contribution", http.MethodPost, period + "/crystallize", map[string]any{"closing_nav": "11000.00",
			"contributions": []any{map[string]any{"strategy": "a", "gross_pnl": "x"}}}, "contributions.gross_pnl"},
		{"audit window", http.MethodGet, "/v1/advanced/fees/spread-audit?since=2026-09-02T00:00:00Z&until=2026-09-01T00:00:00Z", nil, "until"},
		{"period id", http.MethodGet, "/v1/advanced/fees/periods/" + nilUUIDText, nil, "feePeriodId"},
		{"booking id", http.MethodGet, "/v1/advanced/fees/bookings/" + nilUUIDText, nil, "feeBookingId"},
		{"strategy filter", http.MethodGet, "/v1/advanced/fees/periods?account_id=" + testAccountID.String() + "&strategy_ref=%20", nil, "strategy_ref"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, ff := newFeesHarness(t)
			op := operatorPrincipal()
			h.as(&op)
			res := h.do(tc.method, tc.path, tc.body, "Idempotency-Key", "validate-fee-0001")
			require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
			p := res.problem()
			assert.Equal(t, errs.CodeValidationFailed, p.Code)
			assert.Equal(t, tc.field, p.Fields["field"])
			ff.mu.Lock()
			defer ff.mu.Unlock()
			assert.Empty(t, ff.calls, "a malformed request never reaches the fee ledger")
		})
	}
}

func TestFeeRoutes_WithoutAPortAreUnsupported(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	op := operatorPrincipal()
	h.as(&op)
	for _, path := range []string{"/v1/advanced/fees/schedules", "/v1/advanced/fees/schedules/history?key=orders:paper",
		feePreviewPath("orders:paper"), "/v1/advanced/fees/quotes?account_id=" + testAccountID.String(),
		"/v1/advanced/fees/quotes/" + feeQuoteID.String(), "/v1/advanced/fees/bookings?account_id=" + testAccountID.String(),
		"/v1/advanced/fees/bookings/" + feeBookID.String(), "/v1/advanced/fees/periods?account_id=" + testAccountID.String(),
		"/v1/advanced/fees/periods/" + feePeriodFix.String(), "/v1/advanced/fees/spread-audit",
		"/v1/advanced/fees/spread-totals?quote_asset_id=" + trUSD.String()} {
		res := h.do(http.MethodGet, path, nil)
		require.Equal(t, http.StatusUnprocessableEntity, res.Code, "%s body=%s", path, res.Body.String())
		assert.Equal(t, errs.CodeUnsupported, res.problem().Code, path)
	}
	for _, m := range feeMutations() {
		res := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "unwired-fee-"+m.name)
		require.Equal(t, http.StatusUnprocessableEntity, res.Code, "%s body=%s", m.name, res.Body.String())
		assert.Equal(t, errs.CodeUnsupported, res.problem().Code, m.name)
	}
}
