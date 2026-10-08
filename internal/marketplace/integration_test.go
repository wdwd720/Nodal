//go:build integration

package marketplace

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/legalrouter"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/sandboxrt"
	nodalsdk "github.com/nodal/controlplane/internal/sandboxrt/sdk"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

var (
	testAppURL     = os.Getenv("CP_TEST_DATABASE_URL")
	testMigrateURL = os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	testDB         *db.DB
	testBuilder    *sandboxrt.Builder
	testRunner     *sandboxrt.Runner
	toolchainErr   error
)

func TestMain(m *testing.M) { os.Exit(testMain(m)) }

func testMain(m *testing.M) int {
	if testAppURL == "" || testMigrateURL == "" {
		return m.Run()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := migrate.Up(ctx, testMigrateURL); err != nil {
		fmt.Fprintln(os.Stderr, "marketplace integration: migrate up:", err)
		return 1
	}
	var err error
	if testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "marketplace-itest", MaxConns: 30}); err != nil {
		fmt.Fprintln(os.Stderr, "marketplace integration: open:", err)
		return 1
	}
	defer testDB.Close()
	dir, err := os.MkdirTemp("", "marketplace-itest-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "marketplace integration: temp dir:", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if goBin, lookErr := exec.LookPath("go"); lookErr != nil {
		toolchainErr = lookErr
	} else if goBin, err = filepath.Abs(goBin); err != nil {
		toolchainErr = err
	} else if testBuilder, err = sandboxrt.NewBuilder(sandboxrt.BuilderConfig{GoBinary: goBin, CacheDir: filepath.Join(dir, "gocache"),
		TempDir: dir, Timeout: 5 * time.Minute}); err != nil {
		toolchainErr = err
	}
	if testRunner, err = sandboxrt.NewRunner(context.Background(), sandboxrt.RunnerConfig{}); err != nil {
		fmt.Fprintln(os.Stderr, "marketplace integration: runner:", err)
		return 1
	}
	defer func() { _ = testRunner.Close(context.Background()) }()
	return m.Run()
}

func requireEnv(t *testing.T) {
	t.Helper()
	if testDB == nil {
		t.Skip("set CP_TEST_DATABASE_URL and CP_TEST_MIGRATE_DATABASE_URL")
	}
	if testBuilder == nil {
		t.Skipf("marketplace integration tests build strategies and need the go toolchain: %v", toolchainErr)
	}
}

// staticSections supplies the Risk V2 sections the marketplace does not own:
// a healthy, verified account, an open instrument and a fresh price. With
// omit set it supplies nothing, to prove the kernel fails closed.
type staticSections struct {
	clk  *clock.Fake
	omit bool
}

func (s *staticSections) RiskContext(_ context.Context, _ db.Querier, _ econ.Intent, _ sandboxrt.AcceptedProposal) (RiskContext, error) {
	if s.omit {
		return RiskContext{Jurisdiction: "US", Verification: valuedomain.VerificationNone}, nil
	}
	return RiskContext{
		Input: risk.AdvancedInput{
			Account:    &risk.AdvancedAccountSnapshot{Status: "ACTIVE"},
			Identity:   &risk.IdentitySnapshot{Verified: true},
			Instrument: &risk.InstrumentSnapshot{Status: "ACTIVE", SessionOpen: true},
			Oracle:     &risk.OracleSnapshot{Available: true, ObservedAt: s.clk.Now()},
			// A provider that tried to vouch for the strategy or the
			// sandbox run is overruled by the marketplace.
			Strategy: &risk.StrategySnapshot{Approved: true, ManifestPermits: true},
			Sandbox:  &risk.SandboxSnapshot{ArtifactVerified: true, Violations: 0},
		},
		Jurisdiction: "US", Verification: valuedomain.VerificationNone,
	}, nil
}

// executorCall is what the recording executor saw.
type executorCall struct {
	IntentID     econ.IntentID
	StatusAtCall econ.IntentStatus
	Verdict      risk.Verdict
	DecisionHash string
	Kind         econ.IntentKind
	ActorType    security.ActorType
	StrategyRef  string
	Mode         gates.Mode
}

// recordingExecutor stands in for the orders subsystem's paper execution.
// It records every call and completes the intent like a paper venue would;
// in "fail" and "unknown" modes it refuses or loses the outcome.
type recordingExecutor struct {
	mu      sync.Mutex
	calls   []executorCall
	intents *econ.IntentStore
	mode    string
	// For mode "post": a paper fill against a house book, posted for real.
	ledger *ledger.Service
	house  econ.HouseAccount
	quote  assets.AssetID
	baseOf map[string]assets.AssetID
}

func (e *recordingExecutor) ExecutePaper(ctx context.Context, tx pgx.Tx, ex PaperExecution) (ExecutionReceipt, error) {
	cur, err := e.intents.Get(ctx, tx, ex.Intent.ID)
	if err != nil {
		return ExecutionReceipt{}, err
	}
	e.mu.Lock()
	e.calls = append(e.calls, executorCall{IntentID: ex.Intent.ID, StatusAtCall: cur.Status, Verdict: ex.Decision.Verdict,
		DecisionHash: ex.Decision.Hash, Kind: cur.Kind, ActorType: cur.ActorType, StrategyRef: cur.StrategyRef, Mode: cur.Mode})
	mode := e.mode
	e.mu.Unlock()
	venue := econ.Actor{Type: security.ActorService, ID: "paper-venue"}
	switch mode {
	case "fail":
		return ExecutionReceipt{}, errs.New(errs.CodeVenueUnavailable, "paper venue closed")
	case "unknown":
		if _, err := e.intents.Transition(ctx, tx, ex.Intent.ID, econ.StatusExecuting, venue, "submitted", ""); err != nil {
			return ExecutionReceipt{}, err
		}
		return ExecutionReceipt{ExternalRef: "paper-order:" + ex.Intent.ID.String()}, errs.New(errs.CodeSubmissionStateUnknown, "venue timed out")
	}
	ref := "paper-fill:" + ex.Intent.ID.String()
	if mode == "post" {
		if err := e.postFill(ctx, tx, ex, ref); err != nil {
			return ExecutionReceipt{}, err
		}
	}
	for _, to := range []econ.IntentStatus{econ.StatusExecuting, econ.StatusCompleted} {
		if _, err := e.intents.Transition(ctx, tx, ex.Intent.ID, to, venue, "paper fill", ""); err != nil {
			return ExecutionReceipt{}, err
		}
	}
	return ExecutionReceipt{ExternalRef: ref}, nil
}

// postFill books a SIM_TRADE (ADVANCED_FINANCIAL_MODEL §5): the buyer pays
// the notional in the quote asset and receives the base asset; the house
// book sells. Every party balances on its own, which the database enforces
// (SM002).
func (e *recordingExecutor) postFill(ctx context.Context, tx pgx.Tx, ex PaperExecution, ref string) error {
	notional, err := nodalsdk.ParseDecimal(ex.Proposal.Notional)
	if err != nil {
		return err
	}
	units, err := notional.Mul(nodalsdk.IntDecimal(1_000_000)) // quote asset: 6 decimals
	if err != nil {
		return err
	}
	if !units.IsInteger() {
		return errs.New(errs.CodePrecisionLoss, "notional is not a whole number of quote units")
	}
	whole, err := units.Rescale(0, nodalsdk.RoundDown)
	if err != nil {
		return err
	}
	quoteQty := money.QuantityFromBigInt(whole.Unscaled())
	baseQty := ex.Proposal.Quantity
	base := e.baseOf[ex.Proposal.InstrumentID]
	buyer := ex.Intent.AccountID
	_, err = e.ledger.Post(ctx, tx, ledger.Posting{
		Kind: ledger.KindSimTrade, IdempotencyKey: ref, EffectiveAt: ex.Decision.EvaluatedAt,
		Reference: ledger.FinancialEventReference{Type: "paper_fill", ID: ex.Intent.ID.String()},
		Entries: []ledger.Entry{
			{Account: ledger.CustomerAccount(buyer, ledger.CodeWallet, e.quote), Side: ledger.Credit, Quantity: quoteQty},
			{Account: ledger.CustomerAccount(buyer, ledger.CodeTradingOutflow, e.quote), Side: ledger.Debit, Quantity: quoteQty},
			{Account: ledger.CustomerAccount(buyer, ledger.CodeWallet, base), Side: ledger.Debit, Quantity: baseQty},
			{Account: ledger.CustomerAccount(buyer, ledger.CodeTradingInflow, base), Side: ledger.Credit, Quantity: baseQty},
			{Account: e.house.Account(ledger.CodeHouseInventory, base), Side: ledger.Credit, Quantity: baseQty},
			{Account: e.house.Account(ledger.CodeHouseTradingOutflow, base), Side: ledger.Debit, Quantity: baseQty},
			{Account: e.house.Account(ledger.CodeHouseInventory, e.quote), Side: ledger.Debit, Quantity: quoteQty},
			{Account: e.house.Account(ledger.CodeHouseTradingInflow, e.quote), Side: ledger.Credit, Quantity: quoteQty},
		},
	})
	return err
}

func (e *recordingExecutor) snapshot() []executorCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]executorCall(nil), e.calls...)
}

type fixture struct {
	t        *testing.T
	ctx      context.Context
	clk      *clock.Fake
	svc      *Service
	intents  *econ.IntentStore
	effects  *econ.EffectStore
	sandbox  *sandboxrt.Service
	auth     *econ.Authorizer
	exec     *recordingExecutor
	sections *staticSections
	router   *legalrouter.Router
	aw       audit.Writer
	prices   *valuation.PriceStore

	creatorAccount accounts.AccountID
	creator        econ.Actor
	follower       accounts.AccountID
	followerActor  econ.Actor
	operator       econ.Actor
	operator2      econ.Actor
	insA, insB     string
	// quoteAsset is the instruments' quote asset; baseOf maps an instrument
	// to its base asset.
	quoteAsset assets.AssetID
	baseOf     map[string]assets.AssetID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	requireEnv(t)
	clk := clock.NewFake(time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	out := event.NewOutbox(clk)
	aw := audit.NewWriterWithBuildVersion("itest")
	checker, err := gates.NewChecker("LOCAL", func(gates.Capability) bool { return true }, clk)
	require.NoError(t, err)
	auth := econ.NewAuthorizer(checker, killswitch.NewChecker(killswitch.Policy{}))
	router, err := legalrouter.New(legalrouter.ConservativePolicy())
	require.NoError(t, err)
	f := &fixture{t: t, ctx: context.Background(), clk: clk, intents: econ.NewIntentStore(clk, out, aw), effects: econ.NewEffectStore(clk, out, aw),
		auth: auth, sections: &staticSections{clk: clk}, router: router, aw: aw, prices: valuation.NewPriceStore(clk)}
	f.exec = &recordingExecutor{intents: f.intents}
	f.sandbox = sandboxrt.NewService(clk, auth, testRunner, aw)
	f.svc = f.newService(f.sandbox)
	f.creatorAccount, f.creator = f.account()
	f.follower, f.followerActor = f.account()
	f.operator = econ.Actor{Type: security.ActorOperator, ID: uuid.NewString()}
	f.operator2 = econ.Actor{Type: security.ActorOperator, ID: uuid.NewString()}
	f.insA, f.insB = f.instruments()
	f.trend()
	return f
}

func (f *fixture) config(sb *sandboxrt.Service) Config {
	return Config{Clock: f.clk, Intents: f.intents, Effects: f.effects, Auth: f.auth, Risk: risk.NewStore(),
		RiskPolicy: risk.DefaultAdvancedPolicy(), Router: f.router, Sandbox: sb, Builder: testBuilder, Sections: f.sections,
		Executor: f.exec, Audit: f.aw, ServiceID: "marketplace-itest"}
}

func (f *fixture) newService(sb *sandboxrt.Service) *Service {
	f.t.Helper()
	svc, err := NewService(f.config(sb))
	require.NoError(f.t, err)
	return svc
}

// mark records one observation of an instrument's pair in the price store.
func (f *fixture) mark(instrument string, at time.Time, price string) {
	f.t.Helper()
	p, err := money.PriceFromDecimalString(price, f.quoteAsset.String(), "itest-mark", at)
	require.NoError(f.t, err)
	_, err = f.prices.RecordPrice(f.ctx, testDB, valuation.PriceObservation{AssetID: f.baseOf[instrument], QuoteAssetID: f.quoteAsset,
		Mantissa: p.Mantissa, Scale: p.Scale, Source: "itest-mark", ObservedAt: at.UTC()})
	require.NoError(f.t, err)
}

// history records prices one minute apart, the last one now.
func (f *fixture) history(instrument string, prices ...string) {
	f.t.Helper()
	now := f.clk.Now()
	for i, p := range prices {
		f.mark(instrument, now.Add(-time.Duration(len(prices)-1-i)*time.Minute), p)
	}
}

// trend gives the first instrument a rising history (momentum buys it) and
// the second a falling one, the reference price the last close of each:
// the same market f.input describes.
func (f *fixture) trend() {
	f.history(f.insA, "140.00", "142.50", "145.00", "147.50", "150.00", "150.25")
	f.history(f.insB, "45.00", "44.00", "43.00", "42.00", "41.00", "40.10")
}

func (f *fixture) inTx(fn func(ctx context.Context, tx pgx.Tx) error) error {
	return testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, fn)
}

func (f *fixture) account() (accounts.AccountID, econ.Actor) {
	f.t.Helper()
	repo := accounts.NewRepository()
	u, err := repo.CreateUser(f.ctx, testDB, "marketplace-itest", uuid.NewString(), nil)
	require.NoError(f.t, err)
	a, err := repo.CreateAccount(f.ctx, testDB, u.ID, accounts.KindCustomer)
	require.NoError(f.t, err)
	return a.ID, econ.Actor{Type: security.ActorUser, ID: u.ID.String()}
}

// instruments creates two simulated spot pairs and returns their ids in byte
// order: the first is the one the test manifests allow.
func (f *fixture) instruments() (string, string) {
	f.t.Helper()
	repo := assets.NewRepository()
	mk := func(symbol string) assets.AssetID {
		a, err := repo.Create(f.ctx, testDB, assets.Asset{Chain: assets.SandboxChain, Kind: assets.KindSimulated,
			ValueDomain: valuedomain.Simulated, Symbol: symbol, Name: symbol + " (sandbox)", Decimals: 6,
			RiskClass: assets.RiskSettlement, Status: assets.StatusActive})
		require.NoError(f.t, err)
		return a.ID
	}
	suffix := uuid.NewString()[:8]
	quote := mk("MKTQ" + suffix)
	f.quoteAsset = quote
	f.baseOf = map[string]assets.AssetID{}
	var ids []string
	for _, base := range []string{"MKTA" + suffix, "MKTB" + suffix} {
		baseID := mk(base)
		require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			ins, err := instruments.NewRepository().CreateSpotPair(ctx, tx, instruments.SpotPairSpec{Base: baseID, Quote: quote, Settlement: quote,
				CanonicalName: base + "/MKTQ" + suffix, RiskClass: assets.RiskMajor, Status: assets.StatusActive,
				ActiveFrom: f.clk.Now().Add(-time.Hour)})
			if err == nil {
				ids = append(ids, ins.ID.String())
				f.baseOf[ins.ID.String()] = baseID
			}
			return err
		}))
	}
	sort.Strings(ids)
	return ids[0], ids[1]
}

// input is a market and portfolio snapshot in which the first instrument
// trends up and the second trends down: what a simulation runs on, and what a
// caller once handed a follow (which the service now refuses).
func (f *fixture) input(asOf time.Time) sandboxrt.Input {
	return sandboxrt.Input{
		AsOf: asOf,
		Instruments: []sandboxrt.InputInstrument{
			{InstrumentID: f.insA, Kind: "SPOT_PAIR", BaseDecimals: 6, QuoteDecimals: 6, Mark: "150.25", BestBid: "150.20", BestAsk: "150.30",
				Closes: []string{"140.00", "142.50", "145.00", "147.50", "150.00", "152.50"}},
			{InstrumentID: f.insB, Kind: "SPOT_PAIR", BaseDecimals: 6, QuoteDecimals: 6, Mark: "40.10",
				Closes: []string{"45.00", "44.00", "43.00", "42.00", "41.00"}},
		},
		Portfolio: sandboxrt.InputPortfolio{Cash: "10000.00", Positions: []sandboxrt.InputPosition{{InstrumentID: f.insA, Quantity: "2000000"}}},
	}
}

func (f *fixture) manifest() sandboxrt.Manifest {
	return sandboxrt.Manifest{AllowedIntentKinds: []econ.IntentKind{econ.KindAcquireExposure, econ.KindReduceExposure},
		AllowedInstrumentIDs: []string{f.insA}, MaxNotionalPerIntent: "1000.00", MaxIntentsPerRun: 2, MinIntervalSeconds: 3600}.Canonicalize()
}

func loadStrategy(t *testing.T, name string) sandboxrt.Source {
	t.Helper()
	dir := filepath.Join("testdata", "strategies", name)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	src := sandboxrt.Source{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name())) // #nosec G304 -- test strategies
		require.NoError(t, err)
		src[e.Name()] = b
	}
	return src
}

func (f *fixture) versionRequest(strategy string) VersionRequest {
	return VersionRequest{
		Description: "test strategy " + strategy, RiskClass: RiskModerate, SupportedInstrumentIDs: []string{f.insA, f.insB},
		Manifest: f.manifest(), Source: loadStrategy(f.t, strategy), SourceCommit: "test-" + strategy,
		BacktestEvidenceRef: "evidence://backtests/" + strategy, DataDependencies: []string{"closes", "marks"},
		Fees: FeeModel{PerformanceFeeBPS: 2000, ManagementFeeBPS: 100, HighWaterMark: true}, Config: map[string]string{"order_units": "1000000"},
	}
}

func (f *fixture) draft(strategy string) (Listing, Version) {
	f.t.Helper()
	var l Listing
	var v Version
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		l, v, err = f.svc.CreateListing(ctx, tx, ListingRequest{CreatorAccountID: f.creatorAccount, Creator: f.creator,
			Title: "Listing " + strategy, Version: f.versionRequest(strategy)})
		return err
	}))
	return l, v
}

// step runs one lifecycle operation in its own transaction.
func (f *fixture) step(fn func(ctx context.Context, tx pgx.Tx) (Version, error)) Version {
	f.t.Helper()
	var v Version
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		v, err = fn(ctx, tx)
		return err
	}))
	return v
}

// validate builds a SUBMITTED version, between transactions.
func (f *fixture) validate(vid VersionID) (Version, Validation) {
	f.t.Helper()
	v, val, err := f.svc.RunValidation(f.ctx, testDB, vid, econ.SystemActor("marketplace.validator"))
	require.NoError(f.t, err)
	return v, val
}

// approved takes a strategy from source to an APPROVED, CLEARED version.
func (f *fixture) approved(strategy string) (Listing, Version) {
	f.t.Helper()
	l, v := f.draft(strategy)
	f.step(func(ctx context.Context, tx pgx.Tx) (Version, error) { return f.svc.Submit(ctx, tx, v.ID, f.creator) })
	f.validate(v.ID)
	f.step(func(ctx context.Context, tx pgx.Tx) (Version, error) {
		return f.svc.StartReview(ctx, tx, v.ID, f.operator)
	})
	v = f.step(func(ctx context.Context, tx pgx.Tx) (Version, error) {
		return f.svc.Approve(ctx, tx, v.ID, f.operator, "MEETS_POLICY", "looked at it")
	})
	require.Equal(f.t, StatusApproved, v.Status)
	require.Equal(f.t, ModerationCleared, v.Moderation)
	return l, v
}

func (f *fixture) install(v Version) Install {
	f.t.Helper()
	var in Install
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		in, _, err = f.svc.InstallVersion(ctx, tx, InstallRequest{VersionID: v.ID, AccountID: f.follower, Mode: gates.ModePaper, Actor: f.followerActor})
		return err
	}))
	return in
}

// follow paper-follows an install now, as the follower who owns it.
func (f *fixture) follow(svc *Service, in Install) (FollowRun, error) {
	var run FollowRun
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		run, err = svc.PaperFollow(ctx, tx, FollowRequest{InstallID: in.ID, Trigger: TriggerExplicit, Actor: f.followerActor})
		return err
	})
	return run, err
}

func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, testDB.QueryRow(f.ctx, sql, args...).Scan(&n))
	return n
}
