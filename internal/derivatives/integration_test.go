//go:build integration

package derivatives

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
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/reconciliation"
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
		fmt.Fprintln(os.Stderr, "derivatives integration: migrate up:", err)
		return 1
	}
	var err error
	if testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "derivatives-itest", MaxConns: 40}); err != nil {
		fmt.Fprintln(os.Stderr, "derivatives integration: open:", err)
		return 1
	}
	defer testDB.Close()
	return m.Run()
}

// capitalEmitter forwards capital events with a registered topic onto the
// outbox, as the composition root's adapter does.
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

// start is the fixture clock's start: 12:00 UTC, inside the 08:00-16:00
// funding interval and on the hourly variation-margin grid.
var start = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

type fixture struct {
	t        *testing.T
	ctx      context.Context
	clk      *clock.Fake
	svc      *Service
	ledger   *ledger.Service
	intents  *econ.IntentStore
	effects  *econ.EffectStore
	prices   *valuation.PriceStore
	recon    *reconciliation.Repository
	riskSt   *risk.Store
	repo     *instruments.Repository
	usd      assets.AssetID
	btc      assets.AssetID
	spot     instruments.InstrumentID
	clearing string
}

func requireEnv(t *testing.T) {
	t.Helper()
	if testDB == nil {
		t.Skip("set CP_TEST_DATABASE_URL and CP_TEST_MIGRATE_DATABASE_URL")
	}
}

func newFixture(t *testing.T) *fixture { return newFixtureWith(t, nil) }

// newFixtureWith lets a test adjust the service configuration.
func newFixtureWith(t *testing.T, adjust func(*Config)) *fixture {
	requireEnv(t)
	clk := clock.NewFake(start)
	out := event.NewOutbox(clk)
	aw := audit.NewWriterWithBuildVersion("itest")
	checker, err := gates.NewChecker("LOCAL", func(gates.Capability) bool { return true }, clk)
	require.NoError(t, err)
	kills := killswitch.NewChecker(killswitch.Policy{})
	router, err := legalrouter.New(legalrouter.Policy{Version: "derivatives-itest-v1", Rules: []legalrouter.Rule{
		{Match: legalrouter.Key{Product: legalrouter.ProductSimulation}, Outcome: legalrouter.Allow,
			ReasonCode: "SANDBOX_SIMULATION", ApprovalReference: "ITEST-DERIVATIVES"},
		{Outcome: legalrouter.Deny, ReasonCode: "NO_APPROVAL_ON_RECORD"},
	}})
	require.NoError(t, err)
	f := &fixture{
		t: t, ctx: context.Background(), clk: clk, ledger: ledger.NewService(clk, "itest"),
		intents: econ.NewIntentStore(clk, out, aw), effects: econ.NewEffectStore(clk, out, aw),
		prices: valuation.NewPriceStore(clk), recon: reconciliation.NewRepository(clk, out, aw), riskSt: risk.NewStore(),
		repo: instruments.NewRepository(), clearing: "derivatives clearing house itest " + uuid.NewString(),
	}
	f.usd = f.simAsset("DVUSD", 6, assets.RiskSettlement)
	f.btc = f.simAsset("DVBTC", 8, assets.RiskMajor)
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		spot, err := f.repo.CreateSpotPair(ctx, tx, instruments.SpotPairSpec{
			Base: f.btc, Quote: f.usd, Settlement: f.usd, CanonicalName: "DVBTC/DVUSD " + uuid.NewString(),
			RiskClass: assets.RiskMajor, Status: assets.StatusActive, ActiveFrom: start.Add(-time.Hour),
		})
		f.spot = spot.ID
		return err
	}))
	cfg := Config{
		Clock: clk, Intents: f.intents, Effects: f.effects, Authorizer: econ.NewAuthorizer(checker, kills),
		Registry: econ.NewRegistry(), Ledger: f.ledger, Capital: capital.NewService(clk, capitalEmitter{outbox: out, clk: clk}),
		RiskStore: f.riskSt, RiskPolicy: risk.DefaultAdvancedPolicy(), Policy: DefaultPolicy(), Prices: f.prices,
		Instruments: f.repo, Router: router, KillSwitches: kills, Reconciliation: f.recon, USDAssetID: f.usd,
		ClearingHouseName: f.clearing,
	}
	if adjust != nil {
		adjust(&cfg)
	}
	f.svc, err = NewService(cfg)
	require.NoError(t, err)
	// The clearing house is the counterparty of every paper trade: give it
	// capital to pay a one-sided book from.
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.FundClearingHouse(ctx, tx, f.usd, usdUnits(10_000_000), "itest-"+uuid.NewString())
		return err
	}))
	return f
}

func (f *fixture) inTx(fn func(ctx context.Context, tx pgx.Tx) error) error {
	return testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, fn)
}

// do runs fn in its own transaction. A refusal is committed -- the recorded
// rejection is the point of it -- and returned.
func (f *fixture) do(fn func(ctx context.Context, tx pgx.Tx) error) error {
	var refusal error
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		refusal = nil
		err := fn(ctx, tx)
		if _, ok := AsRefusal(err); ok {
			refusal = err
			return nil
		}
		return err
	})
	if err != nil {
		return err
	}
	return refusal
}

func (f *fixture) simAsset(prefix string, decimals uint8, class assets.RiskClass) assets.AssetID {
	f.t.Helper()
	sym := prefix + strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", "")[:6])
	a, err := assets.NewRepository().Create(f.ctx, testDB, assets.Asset{
		Chain: assets.SandboxChain, Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated,
		Symbol: sym, Name: sym + " (sandbox)", Decimals: decimals, RiskClass: class, Status: assets.StatusActive,
	})
	require.NoError(f.t, err)
	return a.ID
}

// account is a customer account whose owner's identity is VERIFIED.
func (f *fixture) account() accounts.AccountID {
	f.t.Helper()
	return f.accountWithIdentity("VERIFIED")
}

// accountWithIdentity is a customer account whose owner's compliance profile
// is in the given identity state ("" for no profile at all).
func (f *fixture) accountWithIdentity(state string) accounts.AccountID {
	f.t.Helper()
	repo := accounts.NewRepository()
	u, err := repo.CreateUser(f.ctx, testDB, "derivatives-itest", uuid.NewString(), nil)
	require.NoError(f.t, err)
	a, err := repo.CreateAccount(f.ctx, testDB, u.ID, accounts.KindCustomer)
	require.NoError(f.t, err)
	if state != "" {
		_, err = testDB.Pool().Exec(f.ctx, `INSERT INTO compliance_profiles (user_id, identity_state, verified_at)
			VALUES ($1, $2, CASE WHEN $2 = 'VERIFIED' THEN now() END)`, u.ID, state)
		require.NoError(f.t, err)
	}
	return a.ID
}

// fund credits an account's wallet with sandbox dollars.
func (f *fixture) fund(acct accounts.AccountID, qty money.Quantity) { f.fundIn(acct, f.usd, qty) }

// fundIn credits an account's wallet with a sandbox asset.
func (f *fixture) fundIn(acct accounts.AccountID, asset assets.AssetID, qty money.Quantity) {
	f.t.Helper()
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := f.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindSandboxFunding, IdempotencyKey: "itest-fund:" + ref,
			Reference: ledger.FinancialEventReference{Type: "sandbox_funding", ID: ref}, EffectiveAt: f.clk.Now(),
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(acct, ledger.CodeWallet, asset), Side: ledger.Debit, Quantity: qty},
				{Account: ledger.CustomerAccount(acct, ledger.CodeCapital, asset), Side: ledger.Credit, Quantity: qty},
			},
		})
		return err
	}))
}

func (f *fixture) terms(kind instruments.InstrumentType) instruments.Terms {
	return f.termsIn(kind, f.usd)
}

// termsIn returns terms settling in a given sandbox asset.
func (f *fixture) termsIn(kind instruments.InstrumentType, settlement assets.AssetID) instruments.Terms {
	under := f.spot
	t := instruments.Terms{
		Kind: kind, Symbol: "DV-" + string(kind) + "-" + strings.ToUpper(uuid.NewString()[:8]), VenueCode: "NODAL-SANDBOX",
		SettlementAssetID: settlement, UnderlyingInstrumentID: &under, ContractMultiplier: q(1_000_000), PriceScale: 2,
		TickSize: q(50), LotSize: q(1), MinQuantity: q(1), SettlementStyle: instruments.SettleCash, TradingCalendar: Calendar24x7,
		OracleMaxAgeMS: 60_000, OracleSources: []string{"itest"}, CorporateActionBehavior: instruments.CorporateActionsNone,
		Transferability: instruments.NonTransferable,
	}
	switch kind {
	case instruments.TypePerpetual:
		t.MarginModel, t.FundingIntervalMS = instruments.MarginLinear, 8*3600*1000
	case instruments.TypeFuture:
		t.MarginModel = instruments.MarginLinear
	case instruments.TypeOption:
		t.MarginModel = instruments.MarginOption
	}
	return t
}

func (f *fixture) create(t instruments.Terms) instruments.InstrumentID {
	f.t.Helper()
	var id instruments.InstrumentID
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		ins, _, err := f.repo.CreateAdvanced(ctx, tx, instruments.AdvancedSpec{
			Terms: t, CanonicalName: t.Symbol, RiskClass: assets.RiskSpeculative, Status: assets.StatusActive,
			ActiveFrom: start.Add(-time.Hour), ActorType: "OPERATOR", ActorID: "itest-operator",
		})
		id = ins.ID
		return err
	}))
	return id
}

func (f *fixture) perp() instruments.InstrumentID {
	return f.create(f.terms(instruments.TypePerpetual))
}

func (f *fixture) future(expiry time.Time) instruments.InstrumentID {
	t := f.terms(instruments.TypeFuture)
	t.Expiry = &expiry
	return f.create(t)
}

func (f *fixture) option(right instruments.OptionRight, style instruments.ExerciseStyle, strikeDollars int64, expiry time.Time) instruments.InstrumentID {
	t := f.terms(instruments.TypeOption)
	k := dollars(strikeDollars)
	t.Expiry, t.StrikeMantissa, t.OptionRight, t.ExerciseStyle = &expiry, &k, right, style
	return f.create(t)
}

// mark records a mark (whole dollars) with an explicit index, observed now.
func (f *fixture) mark(ins instruments.InstrumentID, markDollars, indexDollars int64) Mark {
	f.t.Helper()
	return f.markAt(ins, dollars(markDollars), dollars(indexDollars), f.clk.Now())
}

func (f *fixture) markAt(ins instruments.InstrumentID, mark, index money.Quantity, at time.Time) Mark {
	f.t.Helper()
	var m Mark
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		mp := money.Price{Mantissa: mark, Scale: 2, QuoteAsset: f.usd.String(), Source: "itest", At: at}
		ip := money.Price{Mantissa: index, Scale: 2, QuoteAsset: f.usd.String(), Source: "itest-index", At: at}
		var err error
		m, err = f.svc.RecordMark(ctx, tx, RecordMarkRequest{InstrumentID: ins, Mark: mp, Source: "itest", ObservedAt: at, Index: &ip})
		return err
	}))
	return m
}

func (f *fixture) req(acct accounts.AccountID, ins instruments.InstrumentID) IntentRequest {
	key := "itest-" + uuid.NewString()
	return IntentRequest{
		AccountID: acct, InstrumentID: ins, Mode: gates.ModePaper, IdempotencyKey: key,
		ActorType: security.ActorUser, ActorID: "user-" + acct.String(), Jurisdiction: "US-CA",
		Verification: valuedomain.VerificationNodalIdentity, CorrelationID: "corr-" + key,
	}
}

func (f *fixture) open(acct accounts.AccountID, ins instruments.InstrumentID, side Side, n int64) Outcome {
	f.t.Helper()
	var out Outcome
	require.NoError(f.t, f.do(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = f.svc.OpenPosition(ctx, tx, OpenRequest{IntentRequest: f.req(acct, ins), Side: side, Quantity: q(n)})
		return err
	}))
	return out
}

func (f *fixture) closeAll(acct accounts.AccountID, ins instruments.InstrumentID) Outcome {
	f.t.Helper()
	var out Outcome
	require.NoError(f.t, f.do(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = f.svc.ClosePosition(ctx, tx, CloseRequest{IntentRequest: f.req(acct, ins)})
		return err
	}))
	return out
}

// balance is a customer account's balance in the fixture dollar.
func (f *fixture) balance(acct accounts.AccountID, code ledger.Code) money.Quantity {
	f.t.Helper()
	b, err := f.ledger.Balance(f.ctx, testDB, ledger.CustomerAccount(acct, code, f.usd))
	require.NoError(f.t, err)
	return b
}

func (f *fixture) clearingHouse() econ.Facility {
	f.t.Helper()
	var ch econ.Facility
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ch, err = f.svc.clearingHouse(ctx, tx)
		return err
	}))
	return ch
}

func (f *fixture) clearingBalance(code ledger.Code) money.Quantity {
	f.t.Helper()
	b, err := f.ledger.Balance(f.ctx, testDB, f.clearingHouse().Account(code, f.usd))
	require.NoError(f.t, err)
	return b
}

func (f *fixture) position(acct accounts.AccountID, ins instruments.InstrumentID) Position {
	f.t.Helper()
	var p Position
	require.NoError(f.t, testDB.QueryRow(f.ctx, `SELECT `+positionColumns+` FROM derivative_positions
		WHERE account_id = $1 AND instrument_id = $2 ORDER BY created_at DESC, id DESC LIMIT 1`, acct, ins).
		Scan(&p.ID, &p.AccountID, &p.InstrumentID, &p.SettlementAssetID, &p.Status, &p.Quantity, &p.CostValue,
			&p.SettledValue, &p.RealizedPnL, &p.FundingPnL, &p.MarginPosted, &p.Shortfall, &p.Version, &p.OpenedAt, &p.ClosedAt))
	return p
}

func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, testDB.QueryRow(f.ctx, sql, args...).Scan(&n))
	return n
}

// allowDecision records an ALLOW decision for a book fill, standing in for
// the order book's own pre-trade evaluation.
func (f *fixture) allowDecision(acct accounts.AccountID, ins instruments.InstrumentID) risk.DecisionID {
	f.t.Helper()
	// The book's order is not an economic intent of this package, so the
	// decision names none (intent_id references economic_intents).
	in := risk.AdvancedInput{
		Stage: risk.StagePreTrade,
		Intent: risk.AdvancedIntent{Kind: "TAKE_LIQUIDITY", AccountID: acct.String(), ActorType: "USER",
			Capability: string(gates.Derivatives), Mode: string(gates.ModePaper), InstrumentID: ins.String(), RiskIncreasing: true},
		Identity:   &risk.IdentitySnapshot{Verified: true},
		Account:    &risk.AdvancedAccountSnapshot{Status: "ACTIVE"},
		Instrument: &risk.InstrumentSnapshot{Status: "ACTIVE", SessionOpen: true},
		Oracle:     &risk.OracleSnapshot{Available: true, ObservedAt: f.clk.Now()},
		Now:        f.clk.Now(),
	}
	d := risk.EvaluateAdvanced(risk.DefaultAdvancedPolicy(), in)
	require.Equal(f.t, risk.Allow, d.Verdict, "%v", d.ReasonCodes)
	var did risk.DecisionID
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		did, err = f.riskSt.RecordAdvancedDecision(ctx, tx, in, d, "itest")
		return err
	}))
	return did
}

// bookFill applies one side of a matched book trade.
func (f *fixture) bookFill(acct accounts.AccountID, ins instruments.InstrumentID, side FillSide, n int64, price money.Quantity, mark Mark, dir FillDirection) FillResult {
	f.t.Helper()
	did := f.allowDecision(acct, ins)
	var res FillResult
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = f.svc.ApplyFill(ctx, tx, Fill{
			Key: "book:" + uuid.NewString(), AccountID: acct, InstrumentID: ins, Side: side, Quantity: q(n), Price: price,
			Source: SourceBook, MarkID: mark.ID, Direction: dir, DecisionID: did, OccurredAt: f.clk.Now(),
		})
		return err
	}))
	return res
}

// drainWallet withdraws everything in an account's dollar wallet.
func (f *fixture) drainWallet(acct accounts.AccountID) {
	f.t.Helper()
	wallet := f.balance(acct, ledger.CodeWallet)
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := f.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindSandboxWithdrawal, IdempotencyKey: "itest-withdraw:" + ref,
			Reference: ledger.FinancialEventReference{Type: "sandbox_withdrawal", ID: ref}, EffectiveAt: f.clk.Now(),
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(acct, ledger.CodeWithdrawn, f.usd), Side: ledger.Debit, Quantity: wallet},
				{Account: ledger.CustomerAccount(acct, ledger.CodeWallet, f.usd), Side: ledger.Credit, Quantity: wallet},
			},
		})
		return err
	}))
}

// capitalReserve is a reservation of another, unrelated order.
func capitalReserve(acct accounts.AccountID, asset assets.AssetID, qty money.Quantity) capital.ReserveRequest {
	return capital.ReserveRequest{AccountID: acct.String(), AssetID: asset, Quantity: qty, ActorType: security.ActorSystem,
		ActorID: "itest-other-order", IdempotencyKey: "itest-other-" + uuid.NewString(), TTL: time.Hour, Reason: "another order"}
}

// --- raw journal helper (bypasses internal/ledger on purpose) -----------------

type rawEntry struct {
	ref  ledger.AccountRef
	side ledger.Side
	qty  int64
}

func rawJournal(ctx context.Context, f *fixture, tx pgx.Tx, kind ledger.Kind, entries []rawEntry) (uuid.UUID, error) {
	txID := uuid.New()
	sum := sha256.Sum256([]byte(txID.String()))
	if _, err := tx.Exec(ctx, `INSERT INTO journal_transactions (id, kind, idempotency_key, reference_type, reference_id,
		effective_at, posted_by_actor_type, posted_by_actor_id, content_hash) VALUES ($1,$2,$3,'raw',$4,now(),'SYSTEM','raw',$5)`,
		txID, kind, "raw:"+txID.String(), txID.String(), sum[:]); err != nil {
		return txID, err
	}
	for i, e := range entries {
		acct, err := f.ledger.EnsureAccount(ctx, tx, e.ref)
		if err != nil {
			return txID, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO journal_entries (id, transaction_id, seq, ledger_account_id, asset_id, side, quantity)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`, uuid.New(), txID, i, acct.ID, e.ref.AssetID, e.side, e.qty); err != nil {
			return txID, err
		}
	}
	return txID, nil
}
