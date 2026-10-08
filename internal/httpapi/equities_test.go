package httpapi

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/equities"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

var (
	eqInstrument = instruments.NewInstrumentID()
	eqShare      = assets.NewAssetID()
	eqUSD        = econ.SandboxDollarID()
	eqOrderID    = booksFixedID[equities.OrderID]("0193b2e0-0000-7000-8000-0000000000c1")
	eqFillID     = booksFixedID[equities.FillID]("0193b2e0-0000-7000-8000-0000000000c2")
	eqSettleID   = booksFixedID[equities.SettlementID]("0193b2e0-0000-7000-8000-0000000000c3")
	eqActionID   = booksFixedID[equities.CorporateActionID]("0193b2e0-0000-7000-8000-0000000000c4")
	eqAppID      = booksFixedID[equities.ApplicationID]("0193b2e0-0000-7000-8000-0000000000c5")
	eqReserve    = booksFixedID[capital.ReservationID]("0193b2e0-0000-7000-8000-0000000000c6")
	eqIntent     = econ.NewIntentID()
	eqJournal    = ledger.NewTransactionID()
	eqEffect     = econ.NewEffectID()
	eqBase       = "/v1/advanced/equities"
	// eqDay is Wednesday 2026-10-07; it settles on Thursday.
	eqDay    = equities.NewDate(2026, 10, 7)
	eqSettle = equities.NewDate(2026, 10, 8)
)

func eqQty(n int64) money.Quantity { return money.QuantityFromInt64(n) }

func eqSampleListing() EquityListingView {
	return EquityListingView{
		Listing: equities.Listing{
			Instrument: instruments.Instrument{ID: eqInstrument, Type: instruments.TypeEquity, CanonicalName: "ACME XNAS",
				Status: assets.StatusActive},
			Terms: instruments.Version{Version: 1, ContentHash: []byte{0x0a, 0x0b}, Terms: instruments.Terms{
				Kind: instruments.TypeEquity, Symbol: "ACME", VenueCode: "nodal-paper-equities", ExchangeMIC: "XNAS",
				PriceScale: 2, TickSize: eqQty(1), LotSize: eqQty(1), MinQuantity: eqQty(1), QuantityDecimals: 6,
				OracleMaxAgeMS: 60_000, TradingCalendar: equities.CalendarXNYS,
			}},
			ShareAsset: assets.Asset{ID: eqShare, Symbol: "ACME", Name: "Acme (simulated share)", Decimals: 6},
			CashAsset:  assets.Asset{ID: eqUSD, Symbol: "SBX-USD", Decimals: 6},
		},
		Mark: &equities.Mark{Mantissa: eqQty(15_000), Scale: 2, Source: "sandbox-feed", ObservedAt: testNow},
	}
}

func eqSession() EquitySessionView {
	return EquitySessionView{
		Calendar: equities.CalendarXNYS, Timezone: "America/New_York", At: testNow, Session: equities.SessionRegular,
		TradeDate: eqDay, TradingDay: true, NextTradingDay: eqSettle, SettlementDate: eqSettle,
	}
}

func eqSampleSettlement(settled bool) EquitySettlementView {
	st := equities.Settlement{
		ID: eqSettleID, FillID: eqFillID, OrderID: eqOrderID, AccountID: testAccountID, InstrumentID: eqInstrument,
		AssetID: eqShare, Quantity: eqQty(2_000_000), TradeDate: eqDay, DueDate: eqSettle, CreatedAt: testNow,
	}
	if settled {
		at := testNow.Add(24 * time.Hour)
		j := ledger.NewTransactionID()
		st.SettledAt, st.JournalTransactionID = &at, &j
	}
	return EquitySettlementView{Settlement: st, AssetSymbol: "ACME", AssetDecimals: 6}
}

// eqSampleOrder is testAccountID's filled market buy of 2 shares at
// 150.15, with its charges and its pending settlement.
func eqSampleOrder(owner accounts.AccountID) EquityOrderView {
	reserved, protection := eqQty(303_456_789), eqQty(15_150)
	reservation := eqReserve
	o := equities.Order{
		ID: eqOrderID, AccountID: owner, InstrumentID: eqInstrument, IntentID: eqIntent, ShareAssetID: eqShare,
		CashAssetID: eqUSD, Symbol: "ACME", ExchangeMIC: "XNAS", TermsVersion: 1, Side: equities.SideBuy,
		Type: equities.OrderMarket, Mode: gates.ModePaper, Quantity: eqQty(2_000_000), QuantityDecimals: 6, PriceScale: 2,
		TickSize: eqQty(1), ProtectionPrice: &protection, Session: equities.SessionRegular, TradeDate: eqDay,
		ExpiresAt: testNow.Add(6 * time.Hour), Status: equities.OrderFilled, RejectionReasons: []string{},
		ReservationID: &reservation, ReservedQuantity: &reserved, Broker: "nodal-paper-equities",
		BrokerOrderID: "paper-" + eqOrderID.String(), ActorType: security.ActorUser, ActorID: testUserID.String(),
		CreatedAt: testNow, UpdatedAt: testNow,
	}
	st := eqSampleSettlement(false)
	f := equities.Fill{
		ID: eqFillID, OrderID: eqOrderID, AccountID: owner, InstrumentID: eqInstrument, Side: equities.SideBuy,
		Quantity: eqQty(2_000_000), Price: eqQty(15_015), PriceScale: 2,
		Mark:        equities.Mark{Mantissa: eqQty(15_000), Scale: 2, Source: "sandbox-feed", ObservedAt: testNow},
		SlippageBPS: 10, Venue: "nodal-paper-equities", VenueFillID: "paper-fill-1", ExecutedAt: testNow, TradeDate: eqDay,
		SettlementDate: eqSettle, Charges: equities.TradeCharges{Notional: eqQty(300_300_000), Spread: eqQty(300_300),
			PlatformFees: eqQty(300_300), VenueFees: eqQty(0), Total: eqQty(300_900_600)},
		FeeScheduleVersion: "equities-sandbox-v1", FeeScheduleHash: "fsh", FeeDisclosureHash: "fdh",
		JournalTransactionID: eqJournal, EffectID: eqEffect, CreatedAt: testNow,
	}
	return EquityOrderView{Order: o, Fill: &EquityFillView{Fill: f, Settlement: &st, Assets: EquityAssets{
		ShareSymbol: "ACME", ShareDecimals: 6, SettlementAssetID: eqUSD, SettlementSymbol: "SBX-USD", SettlementDecimals: 6,
	}}}
}

func eqSampleAction() equities.CorporateAction {
	return equities.CorporateAction{
		ID: eqActionID, InstrumentID: eqInstrument, Type: equities.ActionCashDividend, Status: equities.ActionAnnounced,
		EffectiveDate: eqSettle, ShareAssetID: eqShare, CashAssetID: eqUSD,
		CashPerShare: &equities.PricePoint{Mantissa: eqQty(25), Scale: 2}, ExDate: eqSettle, RecordDate: eqSettle,
		PayDate: equities.NewDate(2026, 10, 15), Rounding: equities.RoundingSharesDownCashDown, Reason: "quarterly dividend",
		ContentHash: []byte{0xfe}, AnnouncedBy: econ.Actor{Type: security.ActorOperator, ID: "op-1"}, CreatedAt: testNow,
	}
}

func eqSampleApplication(owner accounts.AccountID) EquityApplicationView {
	j, e := ledger.NewTransactionID(), econ.NewEffectID()
	return EquityApplicationView{
		Application: equities.Application{
			ID: eqAppID, ActionID: eqActionID, AccountID: owner, EntitledQuantity: eqQty(2_000_000),
			SharesDelta: eqQty(0), CashAmount: eqQty(500_000), ExactCashNumerator: eqQty(500_000),
			ExactCashDenominator: eqQty(1), Rounding: equities.RoundingSharesDownCashDown, JournalTransactionID: &j,
			EffectID: &e, CreatedAt: testNow,
		},
		ActionType: equities.ActionCashDividend, InstrumentID: eqInstrument,
		Assets: EquityAssets{ShareSymbol: "ACME", ShareDecimals: 6, SettlementAssetID: eqUSD, SettlementSymbol: "SBX-USD", SettlementDecimals: 6},
	}
}

// fakeEquities is the equities port, recording what reached it.
type fakeEquities struct {
	stubErr
	mu        sync.Mutex
	order     EquityOrderView
	placement EquityPlacement
	action    equities.CorporateAction
	calls     map[string]int

	lastSubmit  equities.OrderRequest
	lastListing EquityListingRequest
	lastHoliday equities.Holiday
	lastSpec    equities.CorporateActionSpec
	lastReason  string
	lastAcct    accounts.AccountID
	lastState   string
	lastCursor  string
	lastLimit   int
	lastScope   *instruments.InstrumentID
}

func newFakeEquities() *fakeEquities {
	report := equities.OrderReport{Broker: "nodal-paper-equities", Status: equities.ReportFilled}
	return &fakeEquities{
		order: eqSampleOrder(testAccountID), placement: EquityPlacement{Placed: true, Report: &report},
		action: eqSampleAction(), calls: map[string]int{},
	}
}

func (f *fakeEquities) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *fakeEquities) hit(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[name]++
	return f.fail()
}

func (f *fakeEquities) Listings(_ context.Context, limit int) (EquityListingsView, error) {
	f.lastLimit = limit
	return EquityListingsView{Items: []EquityListingView{eqSampleListing()}, Session: eqSession()}, f.hit("Listings")
}

func (f *fakeEquities) Listing(context.Context, instruments.InstrumentID) (EquityListingView, error) {
	return eqSampleListing(), f.hit("Listing")
}

func (f *fakeEquities) ListEquity(_ context.Context, r EquityListingRequest) (EquityListingView, error) {
	f.lastListing = r
	v := eqSampleListing()
	v.Mark = nil
	return v, f.hit("ListEquity")
}

func (f *fakeEquities) Calendar(context.Context) (EquityCalendarView, error) {
	return EquityCalendarView{State: eqSession(), SettlementCycleDays: 1,
		Holidays: []equities.Holiday{{Date: equities.NewDate(2026, 11, 26), Name: "Thanksgiving Day"}}}, f.hit("Calendar")
}

func (f *fakeEquities) AddHoliday(_ context.Context, h equities.Holiday) (EquityCalendarView, error) {
	f.lastHoliday = h
	return EquityCalendarView{State: eqSession(), SettlementCycleDays: 1, Holidays: []equities.Holiday{h}}, f.hit("AddHoliday")
}

func (f *fakeEquities) Submit(_ context.Context, r EquityOrderSubmit) (EquityOrderSubmitted, error) {
	f.mu.Lock()
	f.lastSubmit = r.Request
	f.mu.Unlock()
	if err := f.hit("Submit"); err != nil {
		return EquityOrderSubmitted{}, err
	}
	return EquityOrderSubmitted{Order: f.order, Placement: f.placement}, nil
}

func (f *fakeEquities) Cancel(context.Context, equities.OrderID) (EquityOrderView, error) {
	if err := f.hit("Cancel"); err != nil {
		return EquityOrderView{}, err
	}
	v := f.order
	v.Order.Status, v.Fill = equities.OrderCancelled, nil
	return v, nil
}

func (f *fakeEquities) Order(context.Context, equities.OrderID) (EquityOrderView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["Order"]++
	return f.order, nil
}

func (f *fakeEquities) Orders(_ context.Context, acct accounts.AccountID, cursor string, limit int) ([]EquityOrderView, string, error) {
	f.lastAcct, f.lastCursor, f.lastLimit = acct, cursor, limit
	return []EquityOrderView{f.order}, "next-orders", f.hit("Orders")
}

func (f *fakeEquities) Fills(_ context.Context, acct accounts.AccountID, cursor string, limit int) ([]EquityFillView, string, error) {
	f.lastAcct, f.lastCursor, f.lastLimit = acct, cursor, limit
	return []EquityFillView{*f.order.Fill}, "", f.hit("Fills")
}

func (f *fakeEquities) Settlements(_ context.Context, acct accounts.AccountID, state, cursor string, limit int) ([]EquitySettlementView, string, error) {
	f.lastAcct, f.lastState, f.lastCursor, f.lastLimit = acct, state, cursor, limit
	return []EquitySettlementView{eqSampleSettlement(false), eqSampleSettlement(true)}, "", f.hit("Settlements")
}

func (f *fakeEquities) Applications(_ context.Context, acct accounts.AccountID, cursor string, limit int) ([]EquityApplicationView, string, error) {
	f.lastAcct, f.lastCursor, f.lastLimit = acct, cursor, limit
	return []EquityApplicationView{eqSampleApplication(acct)}, "", f.hit("Applications")
}

func (f *fakeEquities) RunSettlements(_ context.Context, limit int) (EquitySettlementRun, error) {
	f.lastLimit = limit
	return EquitySettlementRun{At: testNow, TradeDate: eqSettle, Batches: 1,
		Settled: []EquitySettlementView{eqSampleSettlement(true)}}, f.hit("RunSettlements")
}

func (f *fakeEquities) RunVenue(_ context.Context, limit int) (EquityVenueRun, error) {
	f.lastLimit = limit
	open := equities.OrderReport{Broker: "nodal-paper-equities", Status: equities.ReportOpen}
	return EquityVenueRun{At: testNow, Items: []EquityVenueRunItem{
		{OrderID: eqOrderID, Action: EquityVenuePoll, Before: equities.OrderOpen, After: equities.OrderOpen, Report: &open},
		{OrderID: eqOrderID, Action: EquityVenuePlace, Before: equities.OrderAccepted,
			Err: errs.New(errs.CodeInternal, "pq: secret driver detail")},
	}}, f.hit("RunVenue")
}

func (f *fakeEquities) CorporateActions(_ context.Context, scope *instruments.InstrumentID, cursor string, limit int) ([]equities.CorporateAction, string, error) {
	f.lastScope, f.lastCursor, f.lastLimit = scope, cursor, limit
	return []equities.CorporateAction{f.action}, "next-actions", f.hit("CorporateActions")
}

func (f *fakeEquities) CorporateAction(context.Context, equities.CorporateActionID) (equities.CorporateAction, error) {
	return f.action, f.hit("CorporateAction")
}

func (f *fakeEquities) AnnounceCorporateAction(_ context.Context, spec equities.CorporateActionSpec) (equities.CorporateAction, error) {
	f.mu.Lock()
	f.lastSpec = spec
	f.mu.Unlock()
	return f.action, f.hit("AnnounceCorporateAction")
}

func (f *fakeEquities) CancelCorporateAction(_ context.Context, _ equities.CorporateActionID, reason string) (equities.CorporateAction, error) {
	f.mu.Lock()
	f.lastReason = reason
	f.mu.Unlock()
	a := f.action
	a.Status = equities.ActionCancelled
	return a, f.hit("CancelCorporateAction")
}

func (f *fakeEquities) ApplyCorporateAction(context.Context, equities.CorporateActionID) (EquityCorporateActionResultView, error) {
	a := f.action
	a.Status = equities.ActionApplied
	applied := testNow
	a.AppliedAt = &applied
	return EquityCorporateActionResultView{
		Result:       equities.CorporateActionResult{Action: a},
		Applications: []EquityApplicationView{eqSampleApplication(testAccountID), eqSampleApplication(testOtherAcct)},
	}, f.hit("ApplyCorporateAction")
}

func newEquitiesHarness(t *testing.T) (*harness, *fakeEquities) {
	t.Helper()
	h := newHarness(t)
	fe := newFakeEquities()
	ports := h.ports.ports()
	ports.Equities = fe
	h.server.opts.Ports = ports
	return h, fe
}

func eqOrderBody() map[string]any {
	return map[string]any{
		"account_id": testAccountID.String(), "instrument_id": eqInstrument.String(), "side": "BUY", "order_type": "LIMIT",
		"quantity": "2000000", "limit_price": "150.25", "extended_hours": true, "mode": "SANDBOX",
	}
}

func eqListingBody() map[string]any {
	return map[string]any{
		"kind": "EQUITY", "symbol": "ACME", "name": "Acme", "exchange_mic": "XNAS", "share_decimals": 6, "price_scale": 2,
		"tick_size": "1", "lot_size": "1", "min_quantity": "1", "oracle_max_age_ms": 60000, "oracle_sources": []string{"feed"},
		"risk_class": "STANDARD",
	}
}

func eqDividendBody() map[string]any {
	return map[string]any{
		"instrument_id": eqInstrument.String(), "type": "CASH_DIVIDEND", "dividend_per_share": "0.25",
		"ex_date": "2026-10-08", "record_date": "2026-10-08", "pay_date": "2026-10-15", "reason": "quarterly dividend",
	}
}

func eqRoutes() []struct {
	method, path string
	body         any
	operator     bool
} {
	act := eqBase + "/corporate-actions/" + eqActionID.String()
	ord := eqBase + "/orders/" + eqOrderID.String()
	acct := "?account_id=" + testAccountID.String()
	return []struct {
		method, path string
		body         any
		operator     bool
	}{
		{http.MethodGet, eqBase + "/listings", nil, false},
		{http.MethodPost, eqBase + "/listings", eqListingBody(), true},
		{http.MethodGet, eqBase + "/listings/" + eqInstrument.String(), nil, false},
		{http.MethodGet, eqBase + "/calendar", nil, false},
		{http.MethodPost, eqBase + "/calendar/holidays", map[string]any{"date": "2026-11-26", "name": "Thanksgiving Day"}, true},
		{http.MethodGet, eqBase + "/orders" + acct, nil, false},
		{http.MethodPost, eqBase + "/orders", eqOrderBody(), false},
		{http.MethodGet, ord, nil, false},
		{http.MethodPost, ord + "/cancel", map[string]any{"account_id": testAccountID.String()}, false},
		{http.MethodGet, eqBase + "/fills" + acct, nil, false},
		{http.MethodGet, eqBase + "/settlements" + acct, nil, false},
		{http.MethodPost, eqBase + "/settlement-runs", map[string]any{}, true},
		{http.MethodPost, eqBase + "/venue-runs", map[string]any{}, true},
		{http.MethodGet, eqBase + "/corporate-actions", nil, false},
		{http.MethodPost, eqBase + "/corporate-actions", eqDividendBody(), true},
		{http.MethodGet, act, nil, false},
		{http.MethodPost, act + "/cancel", map[string]any{"reason": "mis-announced"}, true},
		{http.MethodPost, act + "/apply", nil, true},
		{http.MethodGet, eqBase + "/corporate-action-applications" + acct, nil, false},
	}
}

func TestEquities_UnwiredPortIsUnsupported(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for _, c := range eqRoutes() {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			p := customerPrincipal()
			if c.operator {
				p = operatorPrincipal()
			}
			h.as(&p)
			res := h.do(c.method, c.path, c.body, "Idempotency-Key", "equity-unwired-01")
			require.Equal(t, http.StatusUnprocessableEntity, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, errs.CodeUnsupported, res.problem().Code)
		})
	}
}

func TestEquities_ListingsCarryTheirTermsMarkAndTheSession(t *testing.T) {
	t.Parallel()
	h, fe := newEquitiesHarness(t)
	res := h.do(http.MethodGet, eqBase+"/listings?limit=5000", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, maxPageLimit, fe.lastLimit)
	var list api.AdvancedEquityListingList
	res.json(&list)
	assert.Equal(t, api.SIMULATED, list.ValueDomain)
	assert.Equal(t, api.EquitySessionRegular, list.Session.Session)
	assert.Equal(t, "2026-10-07", list.Session.TradeDate)
	require.NotNil(t, list.Session.SettlementDate)
	assert.Equal(t, "2026-10-08", *list.Session.SettlementDate, "a trade today settles T+1")
	require.Len(t, list.Items, 1)
	l := list.Items[0]
	assert.Equal(t, api.SIMULATED, l.ValueDomain)
	assert.Equal(t, "0.01", l.TickPrice)
	assert.Equal(t, "1", l.LotSize, "base units of the share asset")
	assert.Equal(t, 6, l.ShareDecimals)
	assert.Equal(t, "SBX-USD", l.SettlementSymbol)
	assert.Equal(t, "nodal-paper-equities", l.Venue)
	require.NotNil(t, l.Mark)
	assert.Equal(t, "150.00", l.Mark.Price, "the mark at its own scale, exactly")
	assert.Equal(t, "0a0b", l.TermsHash)

	res = h.do(http.MethodGet, eqBase+"/calendar", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var cal api.AdvancedEquityCalendar
	res.json(&cal)
	assert.Equal(t, 1, cal.SettlementCycleDays)
	require.Len(t, cal.Holidays, 1)
	assert.Equal(t, "2026-11-26", cal.Holidays[0].Date)
}

func TestEquities_SubmitPassesTheScopedOrderAndStatesThePlacement(t *testing.T) {
	t.Parallel()
	h, fe := newEquitiesHarness(t)
	res := h.do(http.MethodPost, eqBase+"/orders", eqOrderBody(), "Idempotency-Key", "equity-submit-01")
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())

	got := fe.lastSubmit
	assert.Equal(t, testAccountID, got.AccountID)
	assert.Equal(t, eqInstrument, got.InstrumentID)
	assert.Equal(t, equities.SideBuy, got.Side)
	assert.Equal(t, equities.OrderLimit, got.Type)
	assert.Equal(t, "2000000", got.Quantity.String())
	require.NotNil(t, got.LimitPrice)
	assert.Equal(t, "15025", got.LimitPrice.Mantissa.String(), "150.25 exactly: every digit kept")
	assert.Equal(t, int32(2), got.LimitPrice.Scale)
	assert.True(t, got.ExtendedHours)
	assert.Equal(t, gates.ModeSandbox, got.Mode)
	assert.Equal(t, "equity-submit-01", got.IdempotencyKey, "the service's own replay key is the client's key")
	assert.Equal(t, security.ActorUser, got.ActorType)
	assert.Equal(t, testUserID.String(), got.ActorID)

	var out api.AdvancedEquityOrderSubmission
	res.json(&out)
	assert.Equal(t, api.SIMULATED, out.ValueDomain)
	assert.Equal(t, api.EquityPlacementPlaced, out.Placement.Status)
	require.NotNil(t, out.Placement.VenueStatus)
	assert.Equal(t, api.EquityVenueFilled, *out.Placement.VenueStatus)
	o := out.Order
	assert.Equal(t, api.EquityOrderFilled, o.Status)
	assert.Equal(t, "2000000", o.Quantity)
	require.NotNil(t, o.ProtectionPrice)
	assert.Equal(t, "151.50", *o.ProtectionPrice)
	require.NotNil(t, o.ReservedQuantity)
	assert.Equal(t, "303456789", *o.ReservedQuantity)
	require.NotNil(t, o.Fill)
	f := *o.Fill
	assert.Equal(t, "150.15", f.Price)
	assert.Equal(t, "300300000", f.Notional)
	assert.Equal(t, "300300", f.Spread)
	assert.Equal(t, "300300", f.PlatformFees)
	assert.Equal(t, "0", f.VenueFees)
	assert.Equal(t, "300900600", f.Total)
	assert.Equal(t, "2026-10-08", f.SettlementDate)
	require.NotNil(t, f.Settlement)
	assert.Equal(t, api.EquitySettlementPending, f.Settlement.State, "the shares wait for T+1")
	assert.Nil(t, f.Settlement.SettledAt)
}

// A placement that recorded no answer of the venue says so; an internal
// detail never reaches the client.
func TestEquities_APlacementThatDidNotHappenIsSaid(t *testing.T) {
	t.Parallel()
	h, fe := newEquitiesHarness(t)
	fe.order.Order.Status, fe.order.Fill = equities.OrderAccepted, nil
	fe.placement = EquityPlacement{Err: errs.New(errs.CodeStaleMarketData, "equities: the paper venue quotes only against a fresh mark")}
	res := h.do(http.MethodPost, eqBase+"/orders", eqOrderBody(), "Idempotency-Key", "equity-unplaced-01")
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	var out api.AdvancedEquityOrderSubmission
	res.json(&out)
	assert.Equal(t, api.EquityPlacementNotPlaced, out.Placement.Status)
	require.NotNil(t, out.Placement.ErrorCode)
	assert.Equal(t, string(errs.CodeStaleMarketData), *out.Placement.ErrorCode)
	assert.Contains(t, out.Placement.Detail, "fresh mark")
	assert.Equal(t, api.EquityOrderAccepted, out.Order.Status)

	fe.placement = EquityPlacement{Err: errs.New(errs.CodeInternal, "pq: secret driver detail")}
	res = h.do(http.MethodPost, eqBase+"/orders", eqOrderBody(), "Idempotency-Key", "equity-unplaced-02")
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	assert.NotContains(t, res.Body.String(), "secret driver detail")
}

func TestEquities_EveryMutationIsReplayedNotReExecuted(t *testing.T) {
	t.Parallel()
	act := eqBase + "/corporate-actions/" + eqActionID.String()
	ord := eqBase + "/orders/" + eqOrderID.String()
	changed := func(base map[string]any, k string, v any) map[string]any {
		out := map[string]any{}
		for kk, vv := range base {
			out[kk] = vv
		}
		out[k] = v
		return out
	}
	for _, m := range []struct {
		name, path, counter string
		body, altBody       any
		altPath             string
		operator            bool
		status              int
	}{
		{name: "submit", path: eqBase + "/orders", counter: "Submit", body: eqOrderBody(),
			altBody: changed(eqOrderBody(), "limit_price", "150.26"), status: http.StatusCreated},
		{name: "cancel", path: ord + "/cancel", counter: "Cancel", body: map[string]any{"account_id": testAccountID.String()},
			altBody: map[string]any{"account_id": testAccountID.String(), "x": 1}, status: http.StatusOK},
		{name: "listing", path: eqBase + "/listings", counter: "ListEquity", body: eqListingBody(),
			altBody: changed(eqListingBody(), "symbol", "ACMF"), operator: true, status: http.StatusCreated},
		{name: "holiday", path: eqBase + "/calendar/holidays", counter: "AddHoliday",
			body:    map[string]any{"date": "2026-11-26", "name": "Thanksgiving Day"},
			altBody: map[string]any{"date": "2026-11-27", "name": "Thanksgiving Day"}, operator: true, status: http.StatusOK},
		{name: "announce", path: eqBase + "/corporate-actions", counter: "AnnounceCorporateAction", body: eqDividendBody(),
			altBody: changed(eqDividendBody(), "dividend_per_share", "0.26"), operator: true, status: http.StatusCreated},
		{name: "cancel-action", path: act + "/cancel", counter: "CancelCorporateAction", body: map[string]any{"reason": "mis-announced"},
			altBody: map[string]any{"reason": "mis-announced twice"}, operator: true, status: http.StatusOK},
		{name: "apply", path: act + "/apply", counter: "ApplyCorporateAction",
			altPath: eqBase + "/corporate-actions/" + eqOrderID.String() + "/apply", operator: true, status: http.StatusOK},
		{name: "settlement-run", path: eqBase + "/settlement-runs", counter: "RunSettlements", body: map[string]any{"limit": 10},
			altBody: map[string]any{"limit": 11}, operator: true, status: http.StatusOK},
		{name: "venue-run", path: eqBase + "/venue-runs", counter: "RunVenue", body: map[string]any{"limit": 10},
			altBody: map[string]any{"limit": 11}, operator: true, status: http.StatusOK},
	} {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			h, fe := newEquitiesHarness(t)
			if m.operator {
				op := operatorPrincipal()
				h.as(&op)
			}
			key := "equity-replay-" + m.name
			first := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", key)
			require.Equal(t, m.status, first.Code, "body=%s", first.Body.String())
			require.Equal(t, 1, fe.count(m.counter))

			second := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", key)
			require.Contains(t, []int{http.StatusOK, m.status}, second.Code, "body=%s", second.Body.String())
			assert.JSONEq(t, first.Body.String(), second.Body.String(), "a replay answers the first response")
			assert.Equal(t, 1, fe.count(m.counter), "a replay never reaches the service")

			path, body := m.path, m.altBody
			if m.altPath != "" {
				path, body = m.altPath, m.body
			}
			reuse := h.do(http.MethodPost, path, body, "Idempotency-Key", key)
			require.Equal(t, http.StatusConflict, reuse.Code, "body=%s", reuse.Body.String())
			assert.Equal(t, errs.CodeInvalidIdempotencyReuse, reuse.problem().Code)
			assert.Equal(t, 1, fe.count(m.counter))
		})
	}
}

// A refusal the service records reaches the client as its stable code with
// the order it recorded and every reason, and is replayed, not re-decided.
func TestEquities_ARecordedRefusalKeepsItsCodeAndIsReplayed(t *testing.T) {
	t.Parallel()
	h, fe := newEquitiesHarness(t)
	refused := fe.order.Order
	refused.Status, refused.RejectionCode, refused.RejectionReasons = equities.OrderRejected, equities.RejectInsufficientBuyingPower, []string{}
	fe.err = refused.Refusal()
	for i := 0; i < 2; i++ {
		res := h.do(http.MethodPost, eqBase+"/orders", eqOrderBody(), "Idempotency-Key", "equity-refused-01")
		require.Equal(t, http.StatusUnprocessableEntity, res.Code, "body=%s", res.Body.String())
		p := res.problem()
		assert.Equal(t, errs.CodeInsufficientBuyingPower, p.Code)
		assert.Equal(t, eqOrderID.String(), p.Fields["order_id"])
		assert.Equal(t, string(equities.RejectInsufficientBuyingPower), p.Fields["rejection_code"])
	}
	assert.Equal(t, 1, fe.count("Submit"))
}

func TestEquities_WritesAreOwnershipScoped(t *testing.T) {
	t.Parallel()
	for name, p := range map[string]security.Principal{"stranger": booksStranger(), "operator": operatorPrincipal()} {
		for _, c := range []struct {
			path string
			body any
		}{
			{eqBase + "/orders", eqOrderBody()},
			{eqBase + "/orders/" + eqOrderID.String() + "/cancel", map[string]any{"account_id": testAccountID.String()}},
		} {
			t.Run(name+" "+c.path, func(t *testing.T) {
				t.Parallel()
				h, fe := newEquitiesHarness(t)
				pp := p
				h.as(&pp)
				res := h.do(http.MethodPost, c.path, c.body, "Idempotency-Key", "equity-foreign-01")
				require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
				assert.Equal(t, errs.CodeForbidden, res.problem().Code)
				assert.Zero(t, fe.count("Submit")+fe.count("Cancel"))
			})
		}
	}
}

func TestEquities_CancellingAnotherAccountsOrderIsNotFound(t *testing.T) {
	t.Parallel()
	h, fe := newEquitiesHarness(t)
	fe.order = eqSampleOrder(testOtherAcct)
	res := h.do(http.MethodPost, eqBase+"/orders/"+eqOrderID.String()+"/cancel",
		map[string]any{"account_id": testAccountID.String()}, "Idempotency-Key", "equity-masked-01")
	require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeNotFound, res.problem().Code)
	assert.Zero(t, fe.count("Cancel"))
}

func TestEquities_AccountListsAreScopedToTheCallersAccount(t *testing.T) {
	t.Parallel()
	for _, path := range []string{eqBase + "/orders", eqBase + "/fills", eqBase + "/settlements",
		eqBase + "/corporate-action-applications"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			h, fe := newEquitiesHarness(t)
			res := h.do(http.MethodGet, path+"?account_id="+testAccountID.String()+"&limit=5000&cursor=c-1", nil)
			require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, testAccountID, fe.lastAcct)
			assert.Equal(t, maxPageLimit, fe.lastLimit)
			assert.Equal(t, "c-1", fe.lastCursor)
			assert.Equal(t, "SIMULATED", res.raw()["value_domain"])

			res = h.do(http.MethodGet, path+"?account_id="+testOtherAcct.String(), nil)
			require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, errs.CodeForbidden, res.problem().Code)

			op := operatorPrincipal()
			h.as(&op)
			res = h.do(http.MethodGet, path+"?account_id="+testOtherAcct.String(), nil)
			require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, testOtherAcct, fe.lastAcct)
		})
	}
}

func TestEquities_SettlementsArePendingUntilSettled(t *testing.T) {
	t.Parallel()
	h, fe := newEquitiesHarness(t)
	base := eqBase + "/settlements?account_id=" + testAccountID.String()
	res := h.do(http.MethodGet, base, nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, EquitySettlementsAll, fe.lastState)
	var page api.AdvancedEquitySettlementPage
	res.json(&page)
	require.Len(t, page.Items, 2)
	assert.Equal(t, api.EquitySettlementPending, page.Items[0].State)
	assert.Nil(t, page.Items[0].JournalTransactionId)
	assert.Equal(t, api.EquitySettlementSettled, page.Items[1].State)
	assert.NotNil(t, page.Items[1].SettledAt)
	assert.NotNil(t, page.Items[1].JournalTransactionId)
	assert.Equal(t, "2000000", page.Items[0].Quantity)
	assert.Equal(t, "2026-10-08", page.Items[0].DueDate)

	for state, want := range map[string]string{"PENDING": EquitySettlementsPending, "SETTLED": EquitySettlementsSettled, "ALL": EquitySettlementsAll} {
		require.Equal(t, http.StatusOK, h.do(http.MethodGet, base+"&state="+state, nil).Code)
		assert.Equal(t, want, fe.lastState)
	}
	res = h.do(http.MethodGet, base+"&state=LATE", nil)
	require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
}

func TestEquities_OrderByIDIsNotFoundForAStranger(t *testing.T) {
	t.Parallel()
	h, _ := newEquitiesHarness(t)
	path := eqBase + "/orders/" + eqOrderID.String()
	require.Equal(t, http.StatusOK, h.do(http.MethodGet, path, nil).Code)
	op := operatorPrincipal()
	h.as(&op)
	require.Equal(t, http.StatusOK, h.do(http.MethodGet, path, nil).Code)
	s := booksStranger()
	h.as(&s)
	res := h.do(http.MethodGet, path, nil)
	require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeNotFound, res.problem().Code)
	assert.Equal(t, eqOrderID.String(), res.problem().Fields["order_id"])
	assert.NotContains(t, res.Body.String(), testAccountID.String())
}

func TestEquities_OperatorRoutesNeedOperateAndStepUp(t *testing.T) {
	t.Parallel()
	for _, c := range eqRoutes() {
		if !c.operator {
			continue
		}
		t.Run(c.path, func(t *testing.T) {
			t.Parallel()
			h, fe := newEquitiesHarness(t)
			res := h.do(c.method, c.path, c.body, "Idempotency-Key", "equity-op-cust-1")
			require.Equal(t, http.StatusForbidden, res.Code, "a customer: body=%s", res.Body.String())
			stale := booksStaleOperator()
			h.as(&stale)
			res = h.do(c.method, c.path, c.body, "Idempotency-Key", "equity-op-stale-1")
			require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, errs.CodeStepUpRequired, res.problem().Code)
			for _, counter := range []string{"ListEquity", "AddHoliday", "AnnounceCorporateAction", "CancelCorporateAction",
				"ApplyCorporateAction", "RunSettlements", "RunVenue"} {
				assert.Zero(t, fe.count(counter))
			}
		})
	}
}

func TestEquities_RequestsAreValidatedBeforeTheService(t *testing.T) {
	t.Parallel()
	order := func(k string, v any) map[string]any {
		b := eqOrderBody()
		b[k] = v
		return b
	}
	for name, c := range map[string]struct {
		path     string
		body     any
		operator bool
	}{
		"limit with a sign":     {eqBase + "/orders", order("limit_price", "-1.00"), false},
		"limit with exponent":   {eqBase + "/orders", order("limit_price", "1e3"), false},
		"limit with two points": {eqBase + "/orders", order("limit_price", "1.2.3"), false},
		"decimal quantity":      {eqBase + "/orders", order("quantity", "1.5"), false},
		"bad instrument":        {eqBase + "/orders", order("instrument_id", "nope"), false},
		"bad holiday date":      {eqBase + "/calendar/holidays", map[string]any{"date": "2026-13-01", "name": "x"}, true},
		"listing decimals": {eqBase + "/listings", func() map[string]any {
			b := eqListingBody()
			b["share_decimals"] = 19
			return b
		}(), true},
		"listing lot": {eqBase + "/listings", func() map[string]any {
			b := eqListingBody()
			b["lot_size"] = "0.5"
			return b
		}(), true},
		"dividend amount": {eqBase + "/corporate-actions", func() map[string]any {
			b := eqDividendBody()
			b["dividend_per_share"] = "0.2x"
			return b
		}(), true},
		"dividend date": {eqBase + "/corporate-actions", func() map[string]any {
			b := eqDividendBody()
			b["pay_date"] = "15/10/2026"
			return b
		}(), true},
		"run limit": {eqBase + "/settlement-runs", map[string]any{"limit": 501}, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h, fe := newEquitiesHarness(t)
			if c.operator {
				op := operatorPrincipal()
				h.as(&op)
			}
			res := h.do(http.MethodPost, c.path, c.body, "Idempotency-Key", "equity-invalid-01")
			require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, errs.CodeValidationFailed, res.problem().Code)
			for _, counter := range []string{"Submit", "AddHoliday", "ListEquity", "AnnounceCorporateAction", "RunSettlements"} {
				assert.Zero(t, fe.count(counter))
			}
		})
	}
}

func TestEquities_CorporateActionsAreReadAndAnnouncedExactly(t *testing.T) {
	t.Parallel()
	h, fe := newEquitiesHarness(t)
	res := h.do(http.MethodGet, eqBase+"/corporate-actions?instrument_id="+eqInstrument.String()+"&cursor=a-1", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	require.NotNil(t, fe.lastScope)
	assert.Equal(t, eqInstrument, *fe.lastScope)
	assert.Equal(t, "a-1", fe.lastCursor)
	var page api.AdvancedEquityCorporateActionPage
	res.json(&page)
	require.Len(t, page.Items, 1)
	a := page.Items[0]
	assert.Equal(t, api.EquityActionDividend, a.Type)
	require.NotNil(t, a.DividendPerShare)
	assert.Equal(t, "0.25", *a.DividendPerShare)
	require.NotNil(t, a.PayDate)
	assert.Equal(t, "2026-10-15", *a.PayDate)
	assert.Nil(t, a.Ratio)
	assert.Nil(t, a.FinalDate)

	op := operatorPrincipal()
	h.as(&op)
	body := eqDividendBody()
	body["rounding"] = "SHARES_DOWN_CASH_HALF_EVEN"
	res = h.do(http.MethodPost, eqBase+"/corporate-actions", body, "Idempotency-Key", "equity-announce-01")
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	spec := fe.lastSpec
	assert.Equal(t, eqInstrument, spec.InstrumentID)
	assert.Equal(t, equities.ActionCashDividend, spec.Type)
	require.NotNil(t, spec.CashPerShare)
	assert.Equal(t, "25", spec.CashPerShare.Mantissa.String())
	assert.Equal(t, int32(2), spec.CashPerShare.Scale)
	assert.True(t, spec.ExDate.Equal(equities.NewDate(2026, 10, 8)))
	assert.True(t, spec.PayDate.Equal(equities.NewDate(2026, 10, 15)))
	assert.True(t, spec.EffectiveDate.IsZero(), "a dividend's effective date is its ex-date, the service's to set")
	assert.Equal(t, equities.RoundingSharesDownCashHalfEven, spec.Rounding)

	res = h.do(http.MethodPost, eqBase+"/corporate-actions/"+eqActionID.String()+"/apply", nil, "Idempotency-Key", "equity-apply-01")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var result api.AdvancedEquityCorporateActionResult
	res.json(&result)
	assert.Equal(t, api.SIMULATED, result.ValueDomain)
	assert.Equal(t, api.EquityActionApplied, result.Action.Status)
	require.Len(t, result.Applications, 2)
	app := result.Applications[0]
	assert.Equal(t, "500000", app.SettlementAmount)
	assert.Equal(t, "2000000", app.EntitledQuantity)
	assert.Equal(t, "0", app.SharesDelta)
	assert.Equal(t, "SBX-USD", app.SettlementSymbol)
}

func TestEquities_TheWorkersRunsReportEachItemRedacted(t *testing.T) {
	t.Parallel()
	h, fe := newEquitiesHarness(t)
	op := operatorPrincipal()
	h.as(&op)
	res := h.do(http.MethodPost, eqBase+"/settlement-runs", map[string]any{}, "Idempotency-Key", "equity-settle-run-1")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, 0, fe.lastLimit, "no limit asked: the adapter's default")
	var run api.AdvancedEquitySettlementRun
	res.json(&run)
	assert.Equal(t, 1, run.Batches)
	require.Len(t, run.Settled, 1)
	assert.Equal(t, api.EquitySettlementSettled, run.Settled[0].State)
	assert.Equal(t, "2026-10-08", run.TradeDate)

	res = h.do(http.MethodPost, eqBase+"/venue-runs", map[string]any{"limit": 3}, "Idempotency-Key", "equity-venue-run-1")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, 3, fe.lastLimit)
	var vr api.AdvancedEquityVenueRun
	res.json(&vr)
	require.Len(t, vr.Items, 2)
	assert.Equal(t, api.EquityVenueActionPoll, vr.Items[0].Action)
	require.NotNil(t, vr.Items[0].VenueStatus)
	assert.Equal(t, api.EquityVenueOpen, *vr.Items[0].VenueStatus)
	assert.Equal(t, api.EquityVenueActionPlace, vr.Items[1].Action)
	assert.Nil(t, vr.Items[1].StatusAfter)
	require.NotNil(t, vr.Items[1].ErrorCode)
	assert.Equal(t, string(errs.CodeInternal), *vr.Items[1].ErrorCode)
	assert.NotContains(t, res.Body.String(), "secret driver detail")
}

func TestEquities_ListingRequestsAreTranslatedExactly(t *testing.T) {
	t.Parallel()
	h, fe := newEquitiesHarness(t)
	op := operatorPrincipal()
	h.as(&op)
	res := h.do(http.MethodPost, eqBase+"/listings", eqListingBody(), "Idempotency-Key", "equity-list-01")
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	got := fe.lastListing
	assert.True(t, got.DefaultSettlement, "no settlement asset named: the sandbox dollar")
	assert.Equal(t, instruments.TypeEquity, got.Spec.Kind)
	assert.Equal(t, uint8(6), got.Spec.ShareDecimals)
	assert.Equal(t, "1", got.Spec.TickSize.String())
	assert.Equal(t, assets.RiskStandard, got.Spec.RiskClass)
	assert.Equal(t, []string{"feed"}, got.Spec.OracleSources)

	body := eqListingBody()
	body["settlement_asset_id"] = eqShare.String()
	res = h.do(http.MethodPost, eqBase+"/listings", body, "Idempotency-Key", "equity-list-02")
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	assert.False(t, fe.lastListing.DefaultSettlement)
	assert.Equal(t, eqShare, fe.lastListing.Spec.CashAssetID)
}

func TestEquities_PortErrorsKeepTheirCode(t *testing.T) {
	t.Parallel()
	h, fe := newEquitiesHarness(t)
	fe.err = errs.New(errs.CodeValidationFailed, "equities: the instrument is not an equity or ETF")
	res := h.do(http.MethodGet, eqBase+"/listings/"+eqInstrument.String(), nil)
	require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeValidationFailed, res.problem().Code)

	fe.err = errs.New(errs.CodeInvalidStateTransition, "equities: order is FILLED; the venue owes it nothing")
	res = h.do(http.MethodPost, eqBase+"/orders/"+eqOrderID.String()+"/cancel",
		map[string]any{"account_id": testAccountID.String()}, "Idempotency-Key", "equity-cancel-late")
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeInvalidStateTransition, res.problem().Code)
}
