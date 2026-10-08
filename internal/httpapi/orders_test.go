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
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/orders"
	"github.com/nodal/controlplane/internal/security"
)

// booksFixedID builds a typed identifier from a canonical UUIDv7.
func booksFixedID[T any, PT interface {
	*T
	UnmarshalText([]byte) error
}](s string) T {
	var v T
	if err := PT(&v).UnmarshalText([]byte(s)); err != nil {
		panic(err)
	}
	return v
}

var (
	booksInstrument = instruments.NewInstrumentID()
	booksBookID     = booksFixedID[orders.BookID]("0193b2e0-0000-7000-8000-0000000000b1")
	booksOrderID    = booksFixedID[orders.OrderID]("0193b2e0-0000-7000-8000-0000000000b2")
	booksMakerOrder = booksFixedID[orders.OrderID]("0193b2e0-0000-7000-8000-0000000000b3")
	booksIntentID   = econ.NewIntentID()
	booksBase       = assets.NewAssetID()
	booksQuote      = assets.NewAssetID()
	booksJournal    = ledger.NewTransactionID()
	booksEffect     = econ.NewEffectID()
	booksReserve    = "0193b2e0-0000-7000-8000-0000000000b4"
	booksBase0      = "/v1/advanced/books"
)

// booksTerms is a spot book at two price decimals: a tick is 0.01 and a lot
// 0.0001 of an eight-decimal base, so 2500.00 is 250000 ticks.
func booksTerms() orders.BookTerms {
	return orders.BookTerms{
		InstrumentID: booksInstrument, Kind: instruments.TypeSpotPair, BaseAsset: booksBase, QuoteAsset: booksQuote,
		BaseDecimals: 8, QuoteDecimals: 6, PriceScale: 2, TickSize: money.QuantityFromInt64(1),
		LotSize: money.QuantityFromInt64(10_000), MinQuantity: money.QuantityFromInt64(10_000),
		NotionalPerTickLot: money.QuantityFromInt64(1), MaxPriceTicks: 1_000_000_000, TradingCalendar: "24x7",
	}
}

func booksSchedule(version string, pct money.BPS) fees.Schedule {
	s := fees.ZeroSchedule(booksQuote, 6)
	s.Version, s.PercentageFeeBPS = version, pct
	return s
}

func booksSampleBook() BookView {
	b := orders.Book{
		ID: booksBookID, InstrumentID: booksInstrument, VenueLabel: orders.VenueLabelInternalBook, Mode: gates.ModePaper,
		Status: orders.BookOpen, Terms: booksTerms(), TermsSource: orders.TermsBookSpot, MakerScheduleHash: "mh1",
		TakerScheduleHash: "th1", SnapshotInterval: 64, LastCommandSeq: 7, LastEventSeq: 21, LastTradeSeq: 2,
	}
	b.StateHash[0], b.StateHash[31] = 0xab, 0xcd
	return BookView{
		Book:          b,
		Assets:        BookAssets{BaseSymbol: "SBX-ETH", BaseDecimals: 8, QuoteSymbol: "SBX-USD", QuoteDecimals: 6},
		MakerSchedule: booksSchedule("maker-10bps", 10), TakerSchedule: booksSchedule("taker-20bps", 20),
		Crossing: orders.CrossingRule{BookID: booksBookID}, CrossingDefault: true,
	}
}

// booksSampleOrder is testAccountID's partly filled GTC buy of 0.5 at 2500.00.
func booksSampleOrder(owner accounts.AccountID) BookOrderView {
	acct, intentID := owner, booksIntentID
	seq := int64(7)
	t := booksTerms()
	return BookOrderView{Order: orders.OrderView{
		ID: booksOrderID, BookID: booksBookID, InstrumentID: booksInstrument, VenueLabel: orders.VenueLabelInternalBook,
		OwnerType: orders.OwnerCustomer, OwnerID: owner.String(), AccountID: &acct, IntentID: &intentID, ClientOrderID: "c-1",
		Side: orders.SideBuy, Type: orders.TypeLimit, TimeInForce: orders.GTC, STP: orders.STPCancelTaker,
		LiquidityIntent: orders.ProvideLiquidity, Mode: gates.ModePaper, Price: t.PriceString(250_000), PriceTicks: 250_000,
		Quantity: t.LotsToQuantity(5_000), FilledQuantity: t.LotsToQuantity(2_000), RemainingQuantity: t.LotsToQuantity(3_000),
		CancelledQuantity: t.LotsToQuantity(0), QtyLots: 5_000, FilledLots: 2_000, RemainingLots: 3_000,
		Status: orders.StatusPartiallyFilled, ReservationID: booksReserve, AcceptedCommandSeq: &seq, LastCommandSeq: &seq,
		CreatedAt: testNow, UpdatedAt: testNow,
	}, Assets: booksSampleBook().Assets}
}

// booksSampleTrade is a trade of 0.2 at 2500.00: testOtherAcct's resting
// sell (the maker) and testAccountID's buy (the taker, the aggressor).
func booksSampleTrade() orders.Trade {
	t := booksTerms()
	return orders.Trade{
		BookID: booksBookID, TradeSeq: 2, CommandSeq: 7, EventSeq: 20, PriceTicks: 250_000, QtyLots: 2_000,
		Price: t.TicksToPrice(250_000, "internal-book", testNow), BaseQuantity: t.LotsToQuantity(2_000),
		QuoteNotional: money.QuantityFromInt64(500_000_000), MakerOrderID: booksMakerOrder, TakerOrderID: booksOrderID,
		MakerOwnerType: orders.OwnerCustomer, TakerOwnerType: orders.OwnerCustomer, MakerOwnerID: testOtherAcct.String(),
		TakerOwnerID: testAccountID.String(), AggressorSide: orders.SideBuy, MakerFee: money.QuantityFromInt64(500_000),
		TakerFee: money.QuantityFromInt64(1_000_000), MakerScheduleHash: "mh1", TakerScheduleHash: "th1",
		JournalTransactionID: booksJournal, EffectID: booksEffect, EvidenceRef: "pg://provider_evidence/advanced/orders/x.json",
		EvidenceSHA256: []byte{0xbe, 0xef},
	}
}

// fakeBooks is the order book port. It records every call so a test can
// prove what reached the service and that a replay did not.
type fakeBooks struct {
	stubErr
	mu     sync.Mutex
	book   BookView
	books  []BookView
	next   string
	order  BookOrderView
	trade  orders.Trade
	depth  orders.Depth
	proof  orders.BookProof
	jproof orders.JournalProof
	rule   orders.CrossingRule
	calls  map[string]int

	lastSubmit  BookOrderSubmit
	lastCancel  BookOrderCancel
	lastReplace BookOrderReplace
	lastOpen    BookOpen
	lastStatus  orders.BookStatus
	lastReason  string
	lastAllow   bool
	lastAcct    accounts.AccountID
	lastWorking bool
	lastCursor  string
	lastLimit   int
	lastLevels  int
}

func newFakeBooks() *fakeBooks {
	b := booksSampleBook()
	return &fakeBooks{
		book: b, books: []BookView{b}, next: "books-2", order: booksSampleOrder(testAccountID), trade: booksSampleTrade(),
		depth: orders.Depth{
			Bids: []orders.DepthLevel{{PriceTicks: 249_999, Price: "2499.99", QtyLots: 3_000, Orders: 2}},
			Asks: []orders.DepthLevel{{PriceTicks: 250_100, Price: "2501.00", QtyLots: 1, Orders: 1}},
		},
		proof: orders.BookProof{BookID: booksBookID, InstrumentID: booksInstrument, VenueLabel: orders.VenueLabelInternalBook,
			LastCommandSeq: 7, SnapshotSeq: 4, SnapshotHash: []byte{0x01, 0x02}},
		jproof: orders.JournalProof{BookID: booksBookID, InstrumentID: booksInstrument, Commands: 7, Events: 21, Trades: 2,
			OrdersChecked: 5, SnapshotSeq: 4},
		rule:  orders.CrossingRule{BookID: booksBookID, Version: 1, AllowHouseCustomerCrossing: true, Reason: "market making pilot", Actor: econ.Actor{Type: security.ActorOperator, ID: "op-1"}, CreatedAt: testNow},
		calls: map[string]int{},
	}
}

func (f *fakeBooks) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *fakeBooks) hit(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[name]++
	return f.fail()
}

func (f *fakeBooks) Books(_ context.Context, cursor string, limit int) ([]BookView, string, error) {
	f.lastCursor, f.lastLimit = cursor, limit
	return f.books, f.next, f.hit("Books")
}

func (f *fakeBooks) Book(context.Context, instruments.InstrumentID) (BookView, error) {
	return f.book, f.hit("Book")
}

func (f *fakeBooks) Depth(_ context.Context, _ instruments.InstrumentID, levels int) (BookDepthView, error) {
	f.lastLevels = levels
	return BookDepthView{Book: f.book.Book, Depth: f.depth}, f.hit("Depth")
}

func (f *fakeBooks) OpenBook(_ context.Context, r BookOpen) (BookView, error) {
	f.lastOpen = r
	return f.book, f.hit("OpenBook")
}

func (f *fakeBooks) SetBookStatus(_ context.Context, _ instruments.InstrumentID, to orders.BookStatus, reason string) (BookView, error) {
	f.lastStatus, f.lastReason = to, reason
	v := f.book
	v.Book.Status = to
	return v, f.hit("SetBookStatus")
}

func (f *fakeBooks) SetCrossingRule(_ context.Context, _ instruments.InstrumentID, allow bool, reason string) (orders.CrossingRule, error) {
	f.lastAllow, f.lastReason = allow, reason
	return f.rule, f.hit("SetCrossingRule")
}

func (f *fakeBooks) VerifyJournal(context.Context, instruments.InstrumentID) (orders.JournalProof, error) {
	return f.jproof, f.hit("VerifyJournal")
}

func (f *fakeBooks) command() BookCommandView {
	return BookCommandView{Order: f.order, IntentStatus: "EXECUTING", CommandSeq: 7, Trades: []orders.Trade{f.trade}}
}

func (f *fakeBooks) Submit(_ context.Context, r BookOrderSubmit) (BookCommandView, error) {
	f.mu.Lock()
	f.lastSubmit = r
	f.mu.Unlock()
	if err := f.hit("Submit"); err != nil {
		return BookCommandView{}, err
	}
	return f.command(), nil
}

func (f *fakeBooks) Cancel(_ context.Context, r BookOrderCancel) (BookCommandView, error) {
	f.mu.Lock()
	f.lastCancel = r
	f.mu.Unlock()
	if err := f.hit("Cancel"); err != nil {
		return BookCommandView{}, err
	}
	v := f.command()
	v.IntentStatus, v.Trades = "", nil
	v.Order.Order.Status = orders.StatusCancelled
	return v, nil
}

func (f *fakeBooks) Replace(_ context.Context, r BookOrderReplace) (BookCommandView, error) {
	f.mu.Lock()
	f.lastReplace = r
	f.mu.Unlock()
	if err := f.hit("Replace"); err != nil {
		return BookCommandView{}, err
	}
	v := f.command()
	v.IntentStatus = ""
	return v, nil
}

func (f *fakeBooks) Order(context.Context, orders.OrderID) (BookOrderView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["Order"]++
	// A lookup by id answers what the service holds; a failure set for a
	// command is not the order's.
	return f.order, nil
}

func (f *fakeBooks) Orders(_ context.Context, acct accounts.AccountID, working bool, cursor string, limit int) ([]BookOrderView, string, error) {
	f.lastAcct, f.lastWorking, f.lastCursor, f.lastLimit = acct, working, cursor, limit
	return []BookOrderView{f.order}, f.next, f.hit("Orders")
}

func (f *fakeBooks) Trades(_ context.Context, acct accounts.AccountID, cursor string, limit int) ([]BookTradeView, string, error) {
	f.lastAcct, f.lastCursor, f.lastLimit = acct, cursor, limit
	return []BookTradeView{{Trade: f.trade, InstrumentID: booksInstrument, Assets: f.book.Assets}}, f.next, f.hit("Trades")
}

func (f *fakeBooks) TradeProof(context.Context, instruments.InstrumentID, int64) (BookTradeProof, error) {
	return BookTradeProof{Trade: BookTradeView{Trade: f.trade, InstrumentID: booksInstrument, Assets: f.book.Assets}, Proof: f.proof},
		f.hit("TradeProof")
}

func newBooksHarness(t *testing.T) (*harness, *fakeBooks) {
	t.Helper()
	h := newHarness(t)
	fb := newFakeBooks()
	ports := h.ports.ports()
	ports.Books = fb
	h.server.opts.Ports = ports
	return h, fb
}

// booksStranger owns an account that is party to nothing here.
func booksStranger() security.Principal {
	p := customerPrincipal()
	p.SubjectID = accounts.NewUserID().String()
	p.AccountIDs = []string{accounts.NewAccountID().String()}
	return p
}

// booksCounterparty owns testOtherAcct, the maker of the sample trade.
func booksCounterparty() security.Principal {
	p := customerPrincipal()
	p.SubjectID = accounts.NewUserID().String()
	p.AccountIDs = []string{testOtherAcct.String()}
	return p
}

func booksStaleOperator() security.Principal {
	p := operatorPrincipal()
	p.AuthTime = testNow.Add(-2 * time.Hour)
	return p
}

func booksOrderBody() map[string]any {
	return map[string]any{
		"account_id": testAccountID.String(), "side": "BUY", "order_type": "LIMIT", "time_in_force": "GTC",
		"price": "2500.00", "quantity": "50000000", "client_order_id": "c-1", "stp": "CANCEL_MAKER", "post_only": true,
	}
}

func booksFeeBody(version string, pct int) map[string]any {
	return map[string]any{"version": version, "quote_asset_id": booksQuote.String(), "quote_decimals": 6, "percentage_fee_bps": pct}
}

func booksOpenBody() map[string]any {
	return map[string]any{
		"instrument_id": booksInstrument.String(), "mode": "PAPER", "snapshot_interval": 8,
		"maker_fee": booksFeeBody("maker-10bps", 10), "taker_fee": booksFeeBody("taker-20bps", 20),
		"spot_terms": map[string]any{
			"kind": "SPOT_PAIR", "symbol": "SBX-ETH/SBX-USD", "venue_code": "NODAL-SIM", "base_asset_id": booksBase.String(),
			"quote_asset_id": booksQuote.String(), "settlement_asset_id": booksQuote.String(), "contract_multiplier": "1",
			"price_scale": 2, "tick_size": "1", "lot_size": "10000", "min_quantity": "10000", "quantity_decimals": 8,
			"settlement_style": "PHYSICAL", "trading_calendar": "24x7", "jurisdiction_tags": []string{},
			"oracle_max_age_ms": 30000, "oracle_sources": []string{"sandbox"}, "margin_model": "NONE",
			"corporate_action_behavior": "NONE", "transferability": "TRANSFERABLE",
		},
	}
}

// --- not wired ----------------------------------------------------------------

func TestBooks_UnwiredPortIsUnsupported(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	op := operatorPrincipal()
	for _, c := range []struct {
		method, path string
		body         any
		as           security.Principal
	}{
		{http.MethodGet, booksBase0, nil, customerPrincipal()},
		{http.MethodPost, booksBase0, booksOpenBody(), op},
		{http.MethodGet, booksBase0 + "/" + booksInstrument.String(), nil, customerPrincipal()},
		{http.MethodGet, booksBase0 + "/" + booksInstrument.String() + "/depth", nil, customerPrincipal()},
		{http.MethodPost, booksBase0 + "/" + booksInstrument.String() + "/orders", booksOrderBody(), customerPrincipal()},
		{http.MethodPost, booksBase0 + "/" + booksInstrument.String() + "/status", map[string]any{"status": "HALTED", "reason": "drill"}, op},
		{http.MethodPost, booksBase0 + "/" + booksInstrument.String() + "/crossing-rule",
			map[string]any{"allow_house_customer_crossing": true, "reason": "pilot"}, op},
		{http.MethodGet, booksBase0 + "/" + booksInstrument.String() + "/journal-verification", nil, op},
		{http.MethodGet, booksBase0 + "/" + booksInstrument.String() + "/trades/2/proof", nil, customerPrincipal()},
		{http.MethodGet, booksBase0 + "/orders?account_id=" + testAccountID.String(), nil, customerPrincipal()},
		{http.MethodGet, booksBase0 + "/orders/" + booksOrderID.String(), nil, customerPrincipal()},
		{http.MethodPost, booksBase0 + "/orders/" + booksOrderID.String() + "/cancel", map[string]any{"account_id": testAccountID.String()}, customerPrincipal()},
		{http.MethodPost, booksBase0 + "/orders/" + booksOrderID.String() + "/replace",
			map[string]any{"account_id": testAccountID.String(), "price": "2500.00", "quantity": "1"}, customerPrincipal()},
		{http.MethodGet, booksBase0 + "/trades?account_id=" + testAccountID.String(), nil, customerPrincipal()},
	} {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			p := c.as
			h.as(&p)
			res := h.do(c.method, c.path, c.body, "Idempotency-Key", "books-unwired-01")
			require.Equal(t, http.StatusUnprocessableEntity, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, errs.CodeUnsupported, res.problem().Code)
		})
	}
}

// --- books ---------------------------------------------------------------------

func TestBooks_ABookIsRenderedExactlyWithItsDisclosures(t *testing.T) {
	t.Parallel()
	h, fb := newBooksHarness(t)
	res := h.do(http.MethodGet, booksBase0+"?limit=5000&cursor=books-1", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, maxPageLimit, fb.lastLimit, "the limit is clamped")
	assert.Equal(t, "books-1", fb.lastCursor)
	var page api.AdvancedBookPage
	res.json(&page)
	assert.Equal(t, api.SIMULATED, page.ValueDomain)
	next, err := page.NextCursor.Get()
	require.NoError(t, err)
	assert.Equal(t, "books-2", next)
	require.Len(t, page.Items, 1)

	res = h.do(http.MethodGet, booksBase0+"/"+booksInstrument.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	raw := res.raw()
	assert.Equal(t, "SIMULATED", raw["value_domain"])
	assert.Equal(t, "SIMULATED_INTERNAL_BOOK", raw["venue_label"])
	assert.Equal(t, "0.01", raw["tick_price"], "one tick at the book's price scale, as an exact string")
	assert.Equal(t, "10000", raw["lot_size"], "base units, as an exact string")
	assert.Equal(t, "10000000.00", raw["max_price"])
	assert.Equal(t, "SBX-ETH", raw["base_symbol"])
	assert.EqualValues(t, 6, raw["quote_decimals"])
	assert.Equal(t, "BOOK_SPOT", raw["terms_source"])
	assert.NotContains(t, raw, "terms_version", "a book on spot terms has no canonical terms version")
	maker := raw["maker_fee"].(map[string]any)
	assert.EqualValues(t, 10, maker["percentage_fee_bps"])
	assert.Equal(t, "mh1", maker["hash"], "the schedule's hash is the one the book recorded")
	taker := raw["taker_fee"].(map[string]any)
	assert.EqualValues(t, 20, taker["percentage_fee_bps"])
	rule := raw["crossing_rule"].(map[string]any)
	assert.Equal(t, true, rule["is_default"], "with no rule recorded the default is disclosed")
	assert.Equal(t, false, rule["allow_house_customer_crossing"])
	assert.EqualValues(t, 0, rule["version"])
	assert.Equal(t, "ab", raw["state_hash"].(string)[:2])
}

func TestBooks_DepthIsAggregatedLevelsInBaseUnits(t *testing.T) {
	t.Parallel()
	h, fb := newBooksHarness(t)
	res := h.do(http.MethodGet, booksBase0+"/"+booksInstrument.String()+"/depth?levels=10", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, 10, fb.lastLevels)
	var d api.AdvancedBookDepth
	res.json(&d)
	assert.Equal(t, api.SIMULATED, d.ValueDomain)
	require.Len(t, d.Bids, 1)
	assert.Equal(t, "2499.99", d.Bids[0].Price)
	assert.Equal(t, "30000000", d.Bids[0].Quantity, "3000 lots of 10000 base units")
	assert.Equal(t, 2, d.Bids[0].Orders)
	require.Len(t, d.Asks, 1)
	assert.Equal(t, "10000", d.Asks[0].Quantity)
	assert.EqualValues(t, 7, d.LastCommandSeq)

	for _, bad := range []string{"0", "201", "x"} {
		res := h.do(http.MethodGet, booksBase0+"/"+booksInstrument.String()+"/depth?levels="+bad, nil)
		require.Equal(t, http.StatusBadRequest, res.Code, "levels=%s body=%s", bad, res.Body.String())
	}
}

// --- orders ----------------------------------------------------------------------

func TestBooks_SubmitPassesTheScopedOrderAndRendersItsOwnSideOfEveryFill(t *testing.T) {
	t.Parallel()
	h, fb := newBooksHarness(t)
	res := h.do(http.MethodPost, booksBase0+"/"+booksInstrument.String()+"/orders", booksOrderBody(), "Idempotency-Key", "books-submit-01")
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())

	got := fb.lastSubmit
	assert.Equal(t, booksInstrument, got.InstrumentID)
	assert.Equal(t, testAccountID, got.AccountID)
	assert.Equal(t, "2500.00", got.Price, "the price reaches the adapter exactly as typed")
	assert.Equal(t, "50000000", got.Quantity.String())
	assert.Equal(t, orders.SideBuy, got.Side)
	assert.Equal(t, orders.GTC, got.TimeInForce)
	assert.Equal(t, orders.STPCancelMaker, got.STP)
	assert.True(t, got.PostOnly)
	assert.Equal(t, "c-1", got.ClientOrderID)
	assert.Equal(t, "books-submit-01", got.IdempotencyKey, "the service's own replay key is the client's key")
	assert.Empty(t, got.Mode, "an omitted mode is the book's")

	var out api.AdvancedBookCommandResult
	res.json(&out)
	assert.Equal(t, api.SIMULATED, out.ValueDomain)
	require.NotNil(t, out.IntentStatus)
	assert.Equal(t, "EXECUTING", *out.IntentStatus)
	assert.Equal(t, "2500.00", out.Order.Price)
	assert.Equal(t, "50000000", out.Order.Quantity)
	assert.Equal(t, "20000000", out.Order.FilledQuantity)
	assert.Equal(t, api.BookOrderPartiallyFilled, out.Order.Status)
	require.Len(t, out.Fills, 1)
	fill := out.Fills[0]
	assert.Equal(t, api.BookLiquidityTaker, fill.Liquidity, "the account's order took liquidity")
	assert.Equal(t, api.BookSideBuy, fill.Side)
	assert.Equal(t, "1000000", fill.Fee, "the account's own (taker) fee")
	assert.Equal(t, "500000000", fill.QuoteNotional)
	assert.Equal(t, "20000000", fill.Quantity)
	assert.Equal(t, "2500.00", fill.Price)
	assert.Equal(t, api.BookOwnerCustomer, fill.CounterpartyOwnerType)
	assert.Equal(t, "beef", fill.EvidenceSha256)
	assert.NotContains(t, res.Body.String(), testOtherAcct.String(), "the counterparty's account is never shown")
}

func TestBooks_TheMakerSeesItsOwnSideAndFee(t *testing.T) {
	t.Parallel()
	h, fb := newBooksHarness(t)
	cp := booksCounterparty()
	h.as(&cp)
	res := h.do(http.MethodGet, booksBase0+"/trades?account_id="+testOtherAcct.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, testOtherAcct, fb.lastAcct)
	var page api.AdvancedBookFillPage
	res.json(&page)
	require.Len(t, page.Items, 1)
	f := page.Items[0]
	assert.Equal(t, api.BookLiquidityMaker, f.Liquidity)
	assert.Equal(t, api.BookSideSell, f.Side, "the maker rested on the side the aggressor took from")
	assert.Equal(t, "500000", f.Fee, "the maker's own fee")
	assert.Equal(t, booksMakerOrder.String(), f.OrderId.String())
	assert.NotContains(t, res.Body.String(), testAccountID.String(), "the taker's account is never shown")
}

func TestBooks_EveryMutationIsReplayedNotReExecuted(t *testing.T) {
	t.Parallel()
	inst := booksBase0 + "/" + booksInstrument.String()
	ord := booksBase0 + "/orders/" + booksOrderID.String()
	acct := testAccountID.String()
	changedOrder := booksOrderBody()
	changedOrder["price"] = "2500.01"
	changedOpen := booksOpenBody()
	changedOpen["snapshot_interval"] = 9
	for _, m := range []struct {
		name, path, counter string
		body, altBody       any
		operator            bool
		status              int
	}{
		{"submit", inst + "/orders", "Submit", booksOrderBody(), changedOrder, false, http.StatusCreated},
		{"cancel", ord + "/cancel", "Cancel", map[string]any{"account_id": acct}, map[string]any{"account_id": acct, "x": 1}, false, http.StatusOK},
		{"replace", ord + "/replace", "Replace", map[string]any{"account_id": acct, "price": "2501.00", "quantity": "60000000"},
			map[string]any{"account_id": acct, "price": "2501.00", "quantity": "60000001"}, false, http.StatusOK},
		{"open", booksBase0, "OpenBook", booksOpenBody(), changedOpen, true, http.StatusCreated},
		{"status", inst + "/status", "SetBookStatus", map[string]any{"status": "HALTED", "reason": "drill"},
			map[string]any{"status": "HALTED", "reason": "drill 2"}, true, http.StatusOK},
		{"crossing", inst + "/crossing-rule", "SetCrossingRule", map[string]any{"allow_house_customer_crossing": true, "reason": "pilot"},
			map[string]any{"allow_house_customer_crossing": false, "reason": "pilot"}, true, http.StatusOK},
	} {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			h, fb := newBooksHarness(t)
			if m.operator {
				op := operatorPrincipal()
				h.as(&op)
			}
			key := "books-replay-" + m.name
			first := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", key)
			require.Equal(t, m.status, first.Code, "body=%s", first.Body.String())
			require.Equal(t, 1, fb.count(m.counter))

			second := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", key)
			require.Contains(t, []int{http.StatusOK, m.status}, second.Code, "body=%s", second.Body.String())
			assert.JSONEq(t, first.Body.String(), second.Body.String(), "a replay answers the first response")
			assert.Equal(t, 1, fb.count(m.counter), "a replay never reaches the service")

			reuse := h.do(http.MethodPost, m.path, m.altBody, "Idempotency-Key", key)
			require.Equal(t, http.StatusConflict, reuse.Code, "body=%s", reuse.Body.String())
			assert.Equal(t, errs.CodeInvalidIdempotencyReuse, reuse.problem().Code)
			assert.Equal(t, 1, fb.count(m.counter))
		})
	}
}

// A refusal the pipeline records reaches the client as its stable reason with
// the order it recorded, and a replay reproduces it without asking again.
func TestBooks_ARecordedRefusalKeepsItsReasonAndIsReplayed(t *testing.T) {
	t.Parallel()
	h, fb := newBooksHarness(t)
	fb.err = withOrderID(errs.New(errs.CodeValidationFailed, "orders: this client order id is already used by the account").
		WithField("reason", orders.ReasonDuplicateClientOrderID), booksOrderID)
	for i := 0; i < 2; i++ {
		res := h.do(http.MethodPost, booksBase0+"/"+booksInstrument.String()+"/orders", booksOrderBody(), "Idempotency-Key", "books-refused-01")
		require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
		p := res.problem()
		assert.Equal(t, errs.CodeValidationFailed, p.Code)
		assert.Equal(t, orders.ReasonDuplicateClientOrderID, p.Fields["reason"])
		assert.Equal(t, booksOrderID.String(), p.Fields["order_id"])
	}
	assert.Equal(t, 1, fb.count("Submit"), "the recorded refusal is replayed, not re-decided")

	fb.err = errs.New(errs.CodeRiskRejected, "risk").WithField("reason_codes", []string{"RISK_BALANCE_INSUFFICIENT"})
	res := h.do(http.MethodPost, booksBase0+"/"+booksInstrument.String()+"/orders", booksOrderBody(), "Idempotency-Key", "books-refused-02")
	require.Equal(t, http.StatusUnprocessableEntity, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeRiskRejected, res.problem().Code)
}

// Writing is ownership-only: neither a stranger nor an operator holding
// account:read_any may trade, cancel or replace for someone else's account.
func TestBooks_WritesAreOwnershipScoped(t *testing.T) {
	t.Parallel()
	ord := booksBase0 + "/orders/" + booksOrderID.String()
	for name, p := range map[string]security.Principal{"stranger": booksStranger(), "operator": operatorPrincipal()} {
		for _, c := range []struct {
			path string
			body any
		}{
			{booksBase0 + "/" + booksInstrument.String() + "/orders", booksOrderBody()},
			{ord + "/cancel", map[string]any{"account_id": testAccountID.String()}},
			{ord + "/replace", map[string]any{"account_id": testAccountID.String(), "price": "1.00", "quantity": "10000"}},
		} {
			t.Run(name+" "+c.path, func(t *testing.T) {
				t.Parallel()
				h, fb := newBooksHarness(t)
				pp := p
				h.as(&pp)
				res := h.do(http.MethodPost, c.path, c.body, "Idempotency-Key", "books-foreign-01")
				require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
				assert.Equal(t, errs.CodeForbidden, res.problem().Code)
				assert.Zero(t, fb.count("Submit")+fb.count("Cancel")+fb.count("Replace"))
			})
		}
	}
}

// A command on another account's order, even from an account the caller owns,
// is told the order does not exist, and nothing reaches the service.
func TestBooks_ACommandOnAnotherAccountsOrderIsNotFound(t *testing.T) {
	t.Parallel()
	for _, op := range []string{"cancel", "replace"} {
		t.Run(op, func(t *testing.T) {
			t.Parallel()
			h, fb := newBooksHarness(t)
			fb.order = booksSampleOrder(testOtherAcct)
			body := map[string]any{"account_id": testAccountID.String(), "price": "1.00", "quantity": "10000"}
			res := h.do(http.MethodPost, booksBase0+"/orders/"+booksOrderID.String()+"/"+op, body, "Idempotency-Key", "books-masked-01")
			require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, errs.CodeNotFound, res.problem().Code)
			assert.Zero(t, fb.count("Cancel")+fb.count("Replace"))
		})
	}
}

func TestBooks_AccountListsAreScopedToTheCallersAccount(t *testing.T) {
	t.Parallel()
	for _, path := range []string{booksBase0 + "/orders", booksBase0 + "/trades"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			h, fb := newBooksHarness(t)
			res := h.do(http.MethodGet, path+"?account_id="+testAccountID.String()+"&limit=5000&cursor=c-1", nil)
			require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, testAccountID, fb.lastAcct)
			assert.Equal(t, maxPageLimit, fb.lastLimit)
			assert.Equal(t, "c-1", fb.lastCursor)
			assert.Equal(t, "SIMULATED", res.raw()["value_domain"])

			res = h.do(http.MethodGet, path+"?account_id="+testOtherAcct.String(), nil)
			require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, errs.CodeForbidden, res.problem().Code)

			op := operatorPrincipal()
			h.as(&op)
			res = h.do(http.MethodGet, path+"?account_id="+testOtherAcct.String(), nil)
			require.Equal(t, http.StatusOK, res.Code, "an operator with account:read_any reads any account; body=%s", res.Body.String())
			assert.Equal(t, testOtherAcct, fb.lastAcct)
		})
	}
}

func TestBooks_TheOrderListScopeIsWorkingByDefault(t *testing.T) {
	t.Parallel()
	h, fb := newBooksHarness(t)
	base := booksBase0 + "/orders?account_id=" + testAccountID.String()
	require.Equal(t, http.StatusOK, h.do(http.MethodGet, base, nil).Code)
	assert.True(t, fb.lastWorking)
	require.Equal(t, http.StatusOK, h.do(http.MethodGet, base+"&scope=ALL", nil).Code)
	assert.False(t, fb.lastWorking)
	require.Equal(t, http.StatusOK, h.do(http.MethodGet, base+"&scope=WORKING", nil).Code)
	assert.True(t, fb.lastWorking)
	res := h.do(http.MethodGet, base+"&scope=OPEN", nil)
	require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeValidationFailed, res.problem().Code)
}

// An order by id answers NOT_FOUND to anyone but its owner and an operator who
// may read any account: exactly the answer an absent id gets.
func TestBooks_OrderByIDIsNotFoundForAStranger(t *testing.T) {
	t.Parallel()
	h, fb := newBooksHarness(t)
	path := booksBase0 + "/orders/" + booksOrderID.String()
	res := h.do(http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, "SIMULATED", res.raw()["value_domain"])

	op := operatorPrincipal()
	h.as(&op)
	require.Equal(t, http.StatusOK, h.do(http.MethodGet, path, nil).Code)

	s := booksStranger()
	h.as(&s)
	foreign := h.do(http.MethodGet, path, nil)
	require.Equal(t, http.StatusNotFound, foreign.Code, "body=%s", foreign.Body.String())
	assert.Equal(t, errs.CodeNotFound, foreign.problem().Code)
	assert.Equal(t, booksOrderID.String(), foreign.problem().Fields["order_id"])
	assert.NotContains(t, foreign.Body.String(), testAccountID.String())

	// A house order has no customer account: no customer route shows it.
	fb.order.Order.OwnerType, fb.order.Order.AccountID = orders.OwnerHouse, nil
	h.as(&op)
	res = h.do(http.MethodGet, path, nil)
	require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())

	res = h.do(http.MethodGet, booksBase0+"/orders/not-a-uuid", nil)
	require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
}

func TestBooks_TheTradeProofIsForItsPartiesOnly(t *testing.T) {
	t.Parallel()
	h, _ := newBooksHarness(t)
	path := booksBase0 + "/" + booksInstrument.String() + "/trades/2/proof"
	for name, p := range map[string]security.Principal{
		"taker": customerPrincipal(), "maker": booksCounterparty(), "operator": operatorPrincipal(),
	} {
		pp := p
		h.as(&pp)
		res := h.do(http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, res.Code, "%s: body=%s", name, res.Body.String())
		var proof api.AdvancedBookTradeProof
		res.json(&proof)
		assert.Equal(t, api.SIMULATED, proof.ValueDomain)
		assert.Equal(t, booksJournal.String(), proof.JournalTransactionId.String())
		assert.Equal(t, booksEffect.String(), proof.EffectId.String())
		assert.Equal(t, "beef", proof.EvidenceSha256)
		assert.Equal(t, "500000000", proof.QuoteNotional)
		assert.Equal(t, "500000", proof.MakerFee)
		assert.Equal(t, "1000000", proof.TakerFee)
		assert.Equal(t, "mh1", proof.MakerScheduleHash)
		assert.EqualValues(t, 7, proof.BookLastCommandSeq)
		assert.EqualValues(t, 4, proof.SnapshotSeq)
		require.NotNil(t, proof.SnapshotHash)
		assert.Equal(t, "0102", *proof.SnapshotHash)
		assert.NotContains(t, res.Body.String(), testOtherAcct.String(), "no account id is part of a proof")
		assert.NotContains(t, res.Body.String(), testAccountID.String())
	}
	s := booksStranger()
	h.as(&s)
	res := h.do(http.MethodGet, path, nil)
	require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeNotFound, res.problem().Code)

	res = h.do(http.MethodGet, booksBase0+"/"+booksInstrument.String()+"/trades/0/proof", nil)
	require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
}

// --- operator --------------------------------------------------------------------

func TestBooks_OpenBookCarriesTheSchedulesAndSpotTermsExactly(t *testing.T) {
	t.Parallel()
	h, fb := newBooksHarness(t)
	op := operatorPrincipal()
	h.as(&op)
	res := h.do(http.MethodPost, booksBase0, booksOpenBody(), "Idempotency-Key", "books-open-01")
	require.Equal(t, http.StatusCreated, res.Code, "body=%s", res.Body.String())
	got := fb.lastOpen
	assert.Equal(t, booksInstrument, got.InstrumentID)
	assert.Equal(t, gates.ModePaper, got.Mode)
	assert.Equal(t, 8, got.SnapshotInterval)
	assert.Equal(t, "maker-10bps", got.MakerSchedule.Version)
	assert.Equal(t, money.BPS(10), got.MakerSchedule.PercentageFeeBPS)
	assert.Equal(t, money.BPS(20), got.TakerSchedule.PercentageFeeBPS)
	assert.Equal(t, booksQuote, got.TakerSchedule.QuoteAsset)
	assert.Zero(t, got.MakerSchedule.SpreadBPS, "the request cannot even state a spread")
	assert.Equal(t, fees.DefaultRounding(), got.MakerSchedule.Rounding, "customer-favourable rounding")
	require.NotNil(t, got.SpotTerms)
	assert.Equal(t, instruments.TypeSpotPair, got.SpotTerms.Kind)
	assert.Equal(t, "10000", got.SpotTerms.LotSize.String())
	require.NotNil(t, got.SpotTerms.BaseAssetID)
	assert.Equal(t, booksBase, *got.SpotTerms.BaseAssetID)
	assert.Equal(t, int32(2), got.SpotTerms.PriceScale)

	// A book that was already open with the same terms is not a creation.
	fb.book.Existed = true
	res = h.do(http.MethodPost, booksBase0, booksOpenBody(), "Idempotency-Key", "books-open-02")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
}

func TestBooks_CrossingAndStatusCarryTheOperatorsDecision(t *testing.T) {
	t.Parallel()
	h, fb := newBooksHarness(t)
	op := operatorPrincipal()
	h.as(&op)
	inst := booksBase0 + "/" + booksInstrument.String()
	res := h.do(http.MethodPost, inst+"/crossing-rule", map[string]any{"allow_house_customer_crossing": true, "reason": "pilot"},
		"Idempotency-Key", "books-cross-01")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.True(t, fb.lastAllow)
	assert.Equal(t, "pilot", fb.lastReason)
	var rule api.AdvancedBookCrossingRule
	res.json(&rule)
	assert.Equal(t, 1, rule.Version)
	assert.True(t, rule.AllowHouseCustomerCrossing)
	assert.False(t, rule.IsDefault)
	require.NotNil(t, rule.ActorId)
	assert.Equal(t, "op-1", *rule.ActorId)

	res = h.do(http.MethodPost, inst+"/status", map[string]any{"status": "HALTED", "reason": "drill"}, "Idempotency-Key", "books-status-01")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, orders.BookHalted, fb.lastStatus)
	assert.Equal(t, "HALTED", res.raw()["status"])

	res = h.do(http.MethodGet, inst+"/journal-verification", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var v api.AdvancedBookJournalVerification
	res.json(&v)
	assert.EqualValues(t, 7, v.Commands)
	assert.EqualValues(t, 5, v.OrdersChecked)
}

// Operator commands need sandbox:operate and a recent step-up; the journal
// verification reads under sandbox:operate. A customer reaches none of them.
func TestBooks_OperatorRoutesNeedOperateAndStepUp(t *testing.T) {
	t.Parallel()
	inst := booksBase0 + "/" + booksInstrument.String()
	commands := []struct {
		path string
		body any
	}{
		{booksBase0, booksOpenBody()},
		{inst + "/status", map[string]any{"status": "HALTED", "reason": "drill"}},
		{inst + "/crossing-rule", map[string]any{"allow_house_customer_crossing": true, "reason": "pilot"}},
	}
	for _, c := range commands {
		t.Run(c.path, func(t *testing.T) {
			t.Parallel()
			h, fb := newBooksHarness(t)
			res := h.do(http.MethodPost, c.path, c.body, "Idempotency-Key", "books-op-cust-1")
			require.Equal(t, http.StatusForbidden, res.Code, "a customer: body=%s", res.Body.String())
			stale := booksStaleOperator()
			h.as(&stale)
			res = h.do(http.MethodPost, c.path, c.body, "Idempotency-Key", "books-op-stale-1")
			require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, errs.CodeStepUpRequired, res.problem().Code)
			assert.Zero(t, fb.count("OpenBook")+fb.count("SetBookStatus")+fb.count("SetCrossingRule"))
		})
	}
	h, _ := newBooksHarness(t)
	res := h.do(http.MethodGet, inst+"/journal-verification", nil)
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
}

func TestBooks_RequestsAreValidatedBeforeTheService(t *testing.T) {
	t.Parallel()
	inst := booksBase0 + "/" + booksInstrument.String()
	for name, c := range map[string]struct {
		path string
		body map[string]any
	}{
		"decimal quantity": {inst + "/orders", func() map[string]any { b := booksOrderBody(); b["quantity"] = "1.5"; return b }()},
		"float quantity":   {inst + "/orders", func() map[string]any { b := booksOrderBody(); b["quantity"] = "1e6"; return b }()},
		"bad account":      {inst + "/orders", func() map[string]any { b := booksOrderBody(); b["account_id"] = "not-a-uuid"; return b }()},
		"replace quantity": {booksBase0 + "/orders/" + booksOrderID.String() + "/replace",
			map[string]any{"account_id": testAccountID.String(), "price": "1.00", "quantity": "-"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h, fb := newBooksHarness(t)
			res := h.do(http.MethodPost, c.path, c.body, "Idempotency-Key", "books-invalid-01")
			require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, errs.CodeValidationFailed, res.problem().Code)
			assert.Zero(t, fb.count("Submit")+fb.count("Replace"))
		})
	}
	t.Run("bad fee asset", func(t *testing.T) {
		t.Parallel()
		h, fb := newBooksHarness(t)
		op := operatorPrincipal()
		h.as(&op)
		body := booksOpenBody()
		body["maker_fee"] = map[string]any{"version": "v", "quote_asset_id": "00000000-0000-0000-0000-000000000000",
			"quote_decimals": 6, "percentage_fee_bps": 1}
		res := h.do(http.MethodPost, booksBase0, body, "Idempotency-Key", "books-invalid-02")
		require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
		assert.Zero(t, fb.count("OpenBook"))
	})
}

func TestBooks_PortErrorsKeepTheirCode(t *testing.T) {
	t.Parallel()
	h, fb := newBooksHarness(t)
	fb.err = errs.New(errs.CodeNotFound, "orders: no book is open for this instrument")
	res := h.do(http.MethodGet, booksBase0+"/"+booksInstrument.String(), nil)
	require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeNotFound, res.problem().Code)

	fb.err = errs.New(errs.CodeConflict, "orders: this instrument's book is already open with different terms")
	op := operatorPrincipal()
	h.as(&op)
	res = h.do(http.MethodPost, booksBase0, booksOpenBody(), "Idempotency-Key", "books-conflict-01")
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeConflict, res.problem().Code)
}

// withOrderID adds the recorded order's id to a coded refusal and leaves
// everything else alone.
func TestBooks_WithOrderIDNamesTheRecordedOrderOnly(t *testing.T) {
	t.Parallel()
	coded := errs.New(errs.CodeValidationFailed, "refused").WithField("reason", "X")
	out, ok := errs.As(withOrderID(coded, booksOrderID))
	require.True(t, ok)
	assert.Equal(t, booksOrderID.String(), out.Fields["order_id"])
	assert.Equal(t, "X", out.Fields["reason"])
	assert.Equal(t, errs.CodeValidationFailed, out.Code)

	assert.Same(t, coded, withOrderID(coded, orders.OrderID{}), "no recorded order, nothing to add")
	plain := context.Canceled
	assert.Equal(t, plain, withOrderID(plain, booksOrderID))
}
