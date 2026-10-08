//go:build integration

package equities

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

var (
	testAppURL     = os.Getenv("CP_TEST_DATABASE_URL")
	testMigrateURL = os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	testDB         *db.DB
)

func TestMain(m *testing.M) { os.Exit(testMain(m)) }

func testMain(m *testing.M) int {
	if testAppURL == "" || testMigrateURL == "" {
		return m.Run()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := migrate.Up(ctx, testMigrateURL); err != nil {
		fmt.Fprintln(os.Stderr, "equities integration: migrate up:", err)
		return 1
	}
	var err error
	if testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "equities-itest", MaxConns: 40}); err != nil {
		fmt.Fprintln(os.Stderr, "equities integration: open:", err)
		return 1
	}
	defer testDB.Close()
	return m.Run()
}

func requireEnv(t *testing.T) {
	t.Helper()
	if testDB == nil {
		t.Skip("set CP_TEST_DATABASE_URL and CP_TEST_MIGRATE_DATABASE_URL")
	}
}

// capitalEmitter forwards capital's events onto the real outbox for
// registered topics, as the composition root's adapter does.
type capitalEmitter struct {
	outbox *event.Outbox
	clk    clock.Clock
}

func (e capitalEmitter) Emit(ctx context.Context, tx pgx.Tx, topic string, payload any) error {
	if _, ok := event.Lookup(event.Topic(topic)); !ok {
		return nil
	}
	ev, ok := payload.(capital.ReservationEvent)
	if !ok {
		return nil
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return e.outbox.Enqueue(ctx, tx, topic, event.Envelope{
		ID: event.NewEventID().String(), Type: topic, SchemaVersion: event.Topic(topic).Version(), Source: "capital",
		AggregateType: event.AggregateReservation, AggregateID: ev.ReservationID, OccurredAt: e.clk.Now(), Payload: body,
	})
}

// world is one test's equities deployment: its own clock, its own sandbox
// dollar (so platform revenue accounts are the test's alone), the real
// services over the shared test database.
type world struct {
	t        *testing.T
	ctx      context.Context
	clk      *clock.Fake
	svc      *Service
	venue    *PaperVenue
	ledger   *ledger.Service
	capital  *capital.Service
	prices   *valuation.PriceStore
	intents  *econ.IntentStore
	effects  *econ.EffectStore
	cash     assets.Asset
	schedule fees.Schedule
	sys      econ.Actor
}

type worldOption func(*Config)

func withKillSwitches(k econ.KillSwitchReader) worldOption {
	return func(c *Config) {
		c.KillSwitches = k
		checker, err := gates.NewChecker("LOCAL", func(gates.Capability) bool { return true }, c.Clock)
		if err != nil {
			panic(err)
		}
		c.Authorizer = econ.NewAuthorizer(checker, k)
	}
}

// wednesday10 is 10:00 New York time on Wednesday 2026-10-07.
var wednesday10 = time.Date(2026, 10, 7, 14, 0, 0, 0, time.UTC)

func newWorld(t *testing.T, opts ...worldOption) *world {
	requireEnv(t)
	ctx := context.Background()
	clk := clock.NewFake(wednesday10)
	out := event.NewOutbox(clk)
	aw := audit.NewWriterWithBuildVersion("equities-itest")
	led := ledger.NewService(clk, "equities-itest")
	capSvc := capital.NewService(clk, capitalEmitter{outbox: out, clk: clk})
	checker, err := gates.NewChecker("LOCAL", func(gates.Capability) bool { return true }, clk)
	require.NoError(t, err)
	kills := killswitch.NewChecker(killswitch.Policy{})
	router, err := legalrouter.New(legalrouter.ConservativePolicy())
	require.NoError(t, err)
	venue, err := NewPaperVenue(config.ProviderModeFake, PaperConfig{Name: "nodal-paper-equities", Calendar: testCalendar(t), SlippageBPS: 10})
	require.NoError(t, err)
	// A test-only simulated dollar with a unique symbol: production code
	// never creates one (econ owns the platform's sandbox dollar), and each
	// world having its own keeps the platform revenue accounts per test.
	cash, err := assets.NewRepository().Create(ctx, testDB, assets.Asset{
		Chain: assets.SandboxChain, Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated, Symbol: uniqueSymbol("EQUSD"),
		Name: "equities itest dollar", Decimals: 6, IsStablecoin: true, PegCurrency: "USD", RiskClass: assets.RiskSettlement,
		Status: assets.StatusActive,
	})
	require.NoError(t, err)
	sch := fees.Schedule{
		Version: "equities-itest-v1", QuoteAsset: cash.ID, QuoteDecimals: 6, SpreadBPS: 20, PercentageFeeBPS: 10,
		VenuePassThroughBPS: 3, SlippageReserveBPS: 50, Rounding: fees.DefaultRounding(),
	}
	intents := econ.NewIntentStore(clk, out, aw)
	effects := econ.NewEffectStore(clk, out, aw)
	cfg := Config{
		Clock: clk, Intents: intents, Effects: effects, Authorizer: econ.NewAuthorizer(checker, kills), KillSwitches: kills,
		Router: router, RiskPolicy: risk.DefaultAdvancedPolicy(), Capital: capSvc, Ledger: led,
		Prices: valuation.NewPriceStore(clk), Audit: aw, Adapter: venue, Schedules: []fees.Schedule{sch},
		MarketCollarBPS: 100, MinOrderNotional: money.USDFromMinor(1_00),
	}
	for _, o := range opts {
		o(&cfg)
	}
	svc, err := NewService(cfg)
	require.NoError(t, err)
	return &world{t: t, ctx: ctx, clk: clk, svc: svc, venue: venue, ledger: led, capital: capSvc,
		prices: valuation.NewPriceStore(clk), intents: intents, effects: effects, cash: cash, schedule: sch,
		sys: econ.Actor{Type: security.ActorOperator, ID: "equities-itest-operator"}}
}

func (w *world) inTx(fn func(ctx context.Context, tx pgx.Tx) error) error {
	return testDB.InTx(w.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, fn)
}

// at moves the clock to hh:mm New York time on y-m-d.
func (w *world) at(y int, m time.Month, d, hh, mm int) {
	w.clk.Set(ny(w.t, y, m, d, hh, mm, 0))
}

// account creates a verified customer account (the identity provider
// asserted an email address, so the account reaches NODAL_IDENTITY).
func (w *world) account() accounts.AccountID {
	w.t.Helper()
	repo := accounts.NewRepository()
	hash := sha256.Sum256([]byte(uuid.NewString()))
	u, err := repo.CreateUser(w.ctx, testDB, "equities-itest", uuid.NewString(), hash[:])
	require.NoError(w.t, err)
	a, err := repo.CreateAccount(w.ctx, testDB, u.ID, accounts.KindCustomer)
	require.NoError(w.t, err)
	return a.ID
}

func uniqueSymbol(prefix string) string {
	return prefix + strings.ToUpper(strings.ReplaceAll(uuid.NewString()[:8], "-", ""))
}

// list lists an equity with fractional shares at six decimals.
func (w *world) list(lot, minQty int64) Listing {
	w.t.Helper()
	return w.listKind(instruments.TypeEquity, lot, minQty)
}

// listKind lists an EQUITY or an ETF with fractional shares at six decimals.
func (w *world) listKind(kind instruments.InstrumentType, lot, minQty int64) Listing {
	w.t.Helper()
	var l Listing
	require.NoError(w.t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		l, err = w.svc.ListEquity(ctx, tx, ListingSpec{
			Kind: kind, Symbol: uniqueSymbol("EQ"), Name: "Example Corp", ExchangeMIC: "XNAS",
			CashAssetID: w.cash.ID, ShareDecimals: 6, PriceScale: 2, TickSize: q(1), LotSize: q(lot), MinQuantity: q(minQty),
			OracleMaxAgeMS: 60_000, OracleSources: []string{"itest-feed"}, RiskClass: assets.RiskStandard, Actor: w.sys,
		})
		return err
	}))
	return l
}

func (w *world) post(kind ledger.Kind, entries []ledger.Entry) ledger.PostResult {
	w.t.Helper()
	var res ledger.PostResult
	require.NoError(w.t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		var err error
		res, err = w.ledger.Post(ctx, tx, ledger.Posting{Kind: kind, IdempotencyKey: "itest:" + ref,
			Reference: ledger.FinancialEventReference{Type: "itest", ID: ref}, EffectiveAt: w.clk.Now(), Entries: entries})
		return err
	}))
	return res
}

// fund credits sandbox value of asset to the account's WALLET.
func (w *world) fund(acct accounts.AccountID, asset assets.AssetID, qty int64) {
	w.t.Helper()
	w.post(ledger.KindSandboxFunding, []ledger.Entry{
		{Account: ledger.CustomerAccount(acct, ledger.CodeWallet, asset), Side: ledger.Debit, Quantity: q(qty)},
		{Account: ledger.CustomerAccount(acct, ledger.CodeCapital, asset), Side: ledger.Credit, Quantity: q(qty)},
	})
}

// mark records the share price (mantissa at scale 2) observed a second ago.
func (w *world) mark(l Listing, cents int64) {
	w.t.Helper()
	_, err := w.prices.RecordPrice(w.ctx, testDB, valuation.PriceObservation{AssetID: l.ShareAsset.ID, QuoteAssetID: w.cash.ID,
		Mantissa: q(cents), Scale: 2, Source: "itest-feed", ObservedAt: w.clk.Now().Add(-time.Second)})
	require.NoError(w.t, err)
}

func (w *world) balance(ref ledger.AccountRef) money.Quantity {
	w.t.Helper()
	b, err := w.ledger.Balance(w.ctx, testDB, ref)
	require.NoError(w.t, err)
	return b
}

func (w *world) cust(acct accounts.AccountID, code ledger.Code, asset assets.AssetID) money.Quantity {
	return w.balance(ledger.CustomerAccount(acct, code, asset))
}

func (w *world) platform(code ledger.Code) money.Quantity {
	return w.balance(ledger.PlatformAccount(code, w.cash.ID))
}

type orderSpec struct {
	side     Side
	typ      OrderType
	qty      int64
	limit    int64 // cents; LIMIT only
	extended bool
	key      string
}

func market(side Side, qty int64) orderSpec { return orderSpec{side: side, typ: OrderMarket, qty: qty} }

func limit(side Side, qty, cents int64) orderSpec {
	return orderSpec{side: side, typ: OrderLimit, qty: qty, limit: cents}
}

func (w *world) request(acct accounts.AccountID, l Listing, s orderSpec) OrderRequest {
	key := s.key
	if key == "" {
		key = "itest-" + uuid.NewString()
	}
	r := OrderRequest{AccountID: acct, InstrumentID: l.Instrument.ID, Side: s.side, Type: s.typ, Quantity: q(s.qty),
		ExtendedHours: s.extended, Mode: gates.ModePaper, IdempotencyKey: key, ActorType: security.ActorUser, ActorID: acct.String()}
	if s.typ == OrderLimit {
		r.LimitPrice = &PricePoint{Mantissa: q(s.limit), Scale: 2}
	}
	return r
}

// submit records and routes the intent (one transaction).
func (w *world) submit(req OrderRequest) econ.Intent {
	w.t.Helper()
	var in econ.Intent
	require.NoError(w.t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		in, err = w.svc.SubmitOrderIntent(ctx, tx, req)
		return err
	}))
	return in
}

// accept turns a routed intent into an order (one transaction).
func (w *world) accept(iid econ.IntentID) Order {
	w.t.Helper()
	var o Order
	require.NoError(w.t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		o, err = w.svc.AcceptOrder(ctx, tx, iid)
		return err
	}))
	return o
}

// apply applies a venue answer (one transaction).
func (w *world) apply(oid OrderID, r OrderReport) (Order, error) {
	var o Order
	err := w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		o, err = w.svc.ApplyOrderReport(ctx, tx, oid, r)
		return err
	})
	return o, err
}

// place sends an accepted order to the venue and applies the answer.
func (w *world) place(o Order) Order {
	w.t.Helper()
	if o.Status != OrderAccepted {
		return o
	}
	r, err := w.svc.PlaceAtVenue(w.ctx, testDB, o.ID)
	require.NoError(w.t, err)
	out, err := w.apply(o.ID, r)
	require.NoError(w.t, err)
	return out
}

// poll asks the venue about an order and applies the answer.
func (w *world) poll(o Order) Order {
	w.t.Helper()
	r, err := w.svc.PollVenue(w.ctx, testDB, o.ID)
	require.NoError(w.t, err)
	out, err := w.apply(o.ID, r)
	require.NoError(w.t, err)
	return out
}

// trade runs the whole pipeline: submit, accept, place, apply.
func (w *world) trade(acct accounts.AccountID, l Listing, s orderSpec) Order {
	w.t.Helper()
	in := w.submit(w.request(acct, l, s))
	require.Equal(w.t, econ.StatusRouted, in.Status, "the compiler routes a simulated equity order")
	return w.place(w.accept(in.ID))
}

func (w *world) settleDue() []Settlement {
	w.t.Helper()
	var out []Settlement
	require.NoError(w.t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = w.svc.SettleDue(ctx, tx, 500)
		return err
	}))
	return out
}

func (w *world) intent(iid econ.IntentID) econ.Intent {
	w.t.Helper()
	in, err := w.intents.Get(w.ctx, testDB, iid)
	require.NoError(w.t, err)
	return in
}

func (w *world) order(oid OrderID) Order {
	w.t.Helper()
	o, err := w.svc.Order(w.ctx, testDB, oid)
	require.NoError(w.t, err)
	return o
}

func (w *world) reservation(o Order) capital.Reservation {
	w.t.Helper()
	require.NotNil(w.t, o.ReservationID)
	r, err := w.capital.Get(w.ctx, testDB, *o.ReservationID)
	require.NoError(w.t, err)
	return r
}

func (w *world) count(sql string, args ...any) int {
	w.t.Helper()
	var n int
	require.NoError(w.t, testDB.QueryRow(w.ctx, sql, args...).Scan(&n))
	return n
}

// rawEntry and rawJournal bypass internal/ledger on purpose, as in
// internal/econ's integration tests, to prove the database's own refusals.
type rawEntry struct {
	ref  ledger.AccountRef
	side ledger.Side
	qty  int64
}

func (w *world) rawJournal(ctx context.Context, tx pgx.Tx, kind ledger.Kind, entries []rawEntry) error {
	txID := uuid.New()
	sum := sha256.Sum256([]byte(txID.String()))
	if _, err := tx.Exec(ctx, `INSERT INTO journal_transactions (id, kind, idempotency_key, reference_type, reference_id,
		effective_at, posted_by_actor_type, posted_by_actor_id, content_hash) VALUES ($1,$2,$3,'raw',$4,now(),'SYSTEM','raw',$5)`,
		txID, kind, "raw:"+txID.String(), txID.String(), sum[:]); err != nil {
		return err
	}
	for i, e := range entries {
		acct, err := w.ledger.EnsureAccount(ctx, tx, e.ref)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO journal_entries (id, transaction_id, seq, ledger_account_id, asset_id, side, quantity)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`, uuid.New(), txID, i, acct.ID, e.ref.AssetID, e.side, e.qty); err != nil {
			return err
		}
	}
	return nil
}
