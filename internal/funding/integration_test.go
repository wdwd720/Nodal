//go:build integration

package funding_test

import (
	"context"
	"fmt"
	"os"
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
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/funding"
	"github.com/nodal/controlplane/internal/funding/fundingtest"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
	"github.com/nodal/controlplane/internal/webhook"
)

// The suite runs against an isolated database provisioned by
// `go run ./scripts/testdb -name funding`, never the shared controlplane_test.
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := migrate.Up(ctx, testMigrateURL); err != nil {
		fmt.Fprintln(os.Stderr, "funding integration: migrate up:", err)
		return 1
	}
	var err error
	testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "funding-itest", MaxConns: 20})
	if err != nil {
		fmt.Fprintln(os.Stderr, "funding integration: open pool:", err)
		return 1
	}
	defer testDB.Close()
	return m.Run()
}

func requireEnv(t *testing.T) {
	t.Helper()
	if testDB == nil {
		t.Skip("CP_TEST_DATABASE_URL / CP_TEST_MIGRATE_DATABASE_URL not set; skipping integration test (go run ./scripts/testdb -name funding)")
	}
}

// USDC-like base units: 6 decimals.
const oneUSDC int64 = 1_000_000

func q(n int64) money.Quantity { return money.QuantityFromInt64(n) }

const walletAddr = "So11111111111111111111111111111111111111112"

type recordingEmitter struct{}

func (recordingEmitter) Emit(context.Context, pgx.Tx, string, any) error { return nil }

type fixture struct {
	t        *testing.T
	ctx      context.Context
	clk      *clock.Fake
	provider *fundingtest.Provider
	gates    *fundingtest.Gates
	kill     *fundingtest.KillSwitches
	observer *fundingtest.Observer
	ledger   *ledger.Service
	capital  *capital.Service
	accounts *accounts.Repository
	repo     *funding.Repository
	svc      *funding.Service
	cfg      funding.Config
	user     accounts.UserID
	account  accounts.AccountID
	asset    assets.Asset
	prefix   string // per-fixture idempotency key namespace (tests share one database)
}

func newFixture(t *testing.T, mutate ...func(*funding.Config, *funding.Deps)) *fixture {
	t.Helper()
	requireEnv(t)
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))
	f := &fixture{
		t: t, ctx: ctx, clk: clk, provider: fundingtest.NewProvider(clk, "whsec_test"),
		gates: &fundingtest.Gates{Active: map[gates.Capability]bool{}}, kill: &fundingtest.KillSwitches{}, observer: fundingtest.NewObserver(),
		ledger: ledger.NewService(clk, "funding-itest"), capital: capital.NewService(clk, recordingEmitter{}), accounts: accounts.NewRepository(),
		prefix: uuid.NewString()[:8] + ":",
	}
	repo, err := funding.NewRepository(clk, event.NewOutbox(clk), audit.NewWriterWithBuildVersion("funding-itest"))
	require.NoError(t, err)
	f.repo = repo
	u, err := f.accounts.CreateUser(ctx, testDB, "https://idp.test", "sub-"+uuid.NewString(), nil)
	require.NoError(t, err)
	f.user = u.ID
	acc, err := f.accounts.CreateAccount(ctx, testDB, u.ID, accounts.KindCustomer)
	require.NoError(t, err)
	f.account = acc.ID
	a, err := assets.NewRepository().Create(ctx, testDB, assets.Asset{
		Chain: "solana-devnet", MintAddress: "mint-" + uuid.NewString(), Kind: assets.KindSPLToken, ValueDomain: valuedomain.SelfCustodialCrypto,
		Symbol: "USDC", Name: "USD Coin", Decimals: 6, IsStablecoin: true, PegCurrency: "USD",
		RiskClass: assets.RiskSettlement, Status: assets.StatusActive,
	})
	require.NoError(t, err)
	f.asset = a
	f.cfg = funding.Config{
		Env: config.EnvTest, ProviderMode: config.ProviderModeFake, Reconcile: funding.ReconcilePolicy{ToleranceBPS: 100},
		Availability: funding.AvailabilityPolicy{Version: "availability-v1", ReversibleFor: 72 * time.Hour, HoldDuration: 72 * time.Hour},
		SessionTTL:   30 * time.Minute, SettlementTimeout: 2 * time.Hour,
	}
	deps := funding.Deps{
		DB: testDB, Clock: clk, Repo: repo, Provider: f.provider, Ledger: f.ledger, Holds: f.capital,
		Accounts: f.accounts, Assets: assets.NewRepository(), Gates: f.gates, KillSwitches: f.kill,
	}
	for _, m := range mutate {
		m(&f.cfg, &deps)
	}
	svc, err := funding.NewService(f.cfg, deps)
	require.NoError(t, err)
	f.svc = svc
	return f
}

func (f *fixture) principal() security.Principal {
	return security.Principal{
		SubjectID: f.user.String(), ActorType: security.ActorUser, Roles: []security.Role{security.RoleCustomer},
		AccountIDs: []string{f.account.String()}, AuthTime: f.clk.Now(), AMR: []string{"mfa"},
	}
}

// key namespaces an idempotency key to this fixture.
func (f *fixture) key(k string) string { return f.prefix + k }

func (f *fixture) request(key string) funding.StartRequest {
	fiat := int64(100_00)
	return funding.StartRequest{
		AccountID: f.account, AssetID: f.asset.ID, DestinationNetwork: "solana", DestinationCurrency: "usdc",
		DestinationAddress: walletAddr, FiatCurrency: "usd", FiatAmountMinor: &fiat, IdempotencyKey: f.key(key), CorrelationID: "corr-" + key,
	}
}

func (f *fixture) inTx(fn func(ctx context.Context, tx pgx.Tx) error) error {
	return testDB.InTx(f.ctx, db.TxOptions{}, fn)
}

func (f *fixture) start(key string) funding.StartResult {
	f.t.Helper()
	res, err := f.svc.Start(f.ctx, f.principal(), f.request(key))
	require.NoError(f.t, err)
	return res
}

// providerEvent moves the fake session and applies it as a verified event.
func (f *fixture) providerEvent(sessionID string, status funding.ProviderStatus, raw string, opts ...func(*funding.Session)) funding.ApplyResult {
	f.t.Helper()
	f.provider.SetStatus(sessionID, status, raw, opts...)
	s, err := f.provider.GetSession(f.ctx, sessionID)
	require.NoError(f.t, err)
	return f.apply(funding.WebhookEvent{
		Identity: webhook.Identity{Provider: "fake", EventID: "evt_" + uuid.NewString(), EventType: "session.updated", SignedAt: f.clk.Now(), PublishedAt: f.clk.Now()},
		Session:  s, SessionKnown: true,
	})
}

func (f *fixture) apply(ev funding.WebhookEvent) funding.ApplyResult {
	f.t.Helper()
	var res funding.ApplyResult
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		r, err := f.svc.ApplyProviderEvent(ctx, tx, ev)
		res = r
		return err
	}))
	return res
}

func (f *fixture) deposit(depositID funding.DepositID) funding.Deposit {
	f.t.Helper()
	d, err := f.repo.Get(f.ctx, testDB, depositID)
	require.NoError(f.t, err)
	return d
}

func (f *fixture) balance(code ledger.Code) money.Quantity {
	f.t.Helper()
	b, err := f.ledger.Balance(f.ctx, testDB, ledger.CustomerAccount(f.account, code, f.asset.ID))
	if err != nil && errs.CodeOf(err) == errs.CodeNotFound {
		return money.Quantity{}
	}
	require.NoError(f.t, err)
	return b
}

func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, testDB.QueryRow(f.ctx, sql, args...).Scan(&n))
	return n
}

// confirmed drives a fresh deposit to PROVIDER_CONFIRMED with the given
// provider-reported destination amount.
func (f *fixture) confirmed(key, amount string) funding.Deposit {
	f.t.Helper()
	res := f.start(key)
	sid := res.Session.ID
	f.providerEvent(sid, funding.ProviderStatusCustomerActionRequired, "requires_payment")
	f.providerEvent(sid, funding.ProviderStatusProcessing, "fulfillment_processing")
	r := f.providerEvent(sid, funding.ProviderStatusConfirmed, "fulfillment_complete", func(s *funding.Session) {
		s.DestinationAmount, s.TransactionID = amount, "cxt_"+key
	})
	require.Equal(f.t, funding.ApplyApplied, r.Outcome)
	require.Equal(f.t, funding.StatusProviderConfirmed, r.Deposit.Status)
	return r.Deposit
}

// available drives a deposit through settlement, reconciliation and
// availability, returning it AVAILABLE with observed base units.
func (f *fixture) available(key string, observed int64) funding.Deposit {
	f.t.Helper()
	d := f.confirmed(key, money.QuantityFromInt64(observed).ToDecimalString(6))
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.RecordSettlement(ctx, tx, d.ID, q(observed), "sig-"+key, 123)
	}))
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		res, err := f.svc.Reconcile(ctx, tx, d.ID)
		if err == nil {
			require.True(f.t, res.Agreed)
		}
		return err
	}))
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.MarkAvailable(ctx, tx, d.ID, f.cfg.Availability)
	}))
	return f.deposit(d.ID)
}

func TestIntegration_Start_CreatesSessionAndIsIdempotent(t *testing.T) {
	f := newFixture(t)
	res := f.start("k1")
	require.False(t, res.Replayed)
	require.Equal(t, funding.StatusSessionCreated, res.Deposit.Status)
	require.NotEmpty(t, res.Session.ClientSecret, "the client secret is handed out once")
	require.Equal(t, res.Session.ID, res.Deposit.ProviderSessionID)
	require.Equal(t, "fake", res.Deposit.Provider)
	require.NotNil(t, res.Deposit.SessionCreatedAt)
	require.Equal(t, res.Deposit.ID.String(), f.provider.Created()[0].IdempotencyKey, "the deposit id is the provider idempotency key")
	require.True(t, f.provider.Created()[0].LockWalletAddress)
	require.Equal(t, "100.00", f.provider.Created()[0].SourceAmount)
	require.Empty(t, f.provider.Created()[0].Metadata["client_secret"])

	// Redirect "success" or a created session never credits anything.
	require.True(t, f.balance(ledger.CodeWallet).IsZero())

	trail, err := f.repo.Transitions(f.ctx, testDB, res.Deposit.ID)
	require.NoError(t, err)
	require.Len(t, trail, 1)
	require.Equal(t, funding.StatusCreated, trail[0].From)
	require.Equal(t, funding.StatusSessionCreated, trail[0].To)
	require.Equal(t, 2, f.count(`SELECT count(*) FROM outbox_events WHERE aggregate_id = $1`, res.Deposit.ID.String()), "created + transitioned")
	require.Equal(t, 2, f.count(`SELECT count(*) FROM audit_events WHERE resource_type = 'deposit' AND resource_id = $1`, res.Deposit.ID.String()))

	replay := f.start("k1")
	require.True(t, replay.Replayed)
	require.Equal(t, res.Deposit.ID, replay.Deposit.ID)
	require.Len(t, f.provider.Created(), 1, "a replay never mints a second provider session")

	// Same key from another account is a reuse violation.
	other, err := f.accounts.CreateAccount(f.ctx, testDB, f.user, accounts.KindCustomer)
	require.NoError(t, err)
	p := f.principal()
	p.AccountIDs = []string{other.ID.String()}
	req := f.request("k1")
	req.AccountID = other.ID
	_, err = f.svc.Start(f.ctx, p, req)
	require.Equal(t, errs.CodeInvalidIdempotencyReuse, errs.CodeOf(err))

	// Provider failure leaves the deposit CREATED and retryable with the same key.
	f.provider.CreateErr = errs.New(errs.CodeProviderUnavailable, "down")
	_, err = f.svc.Start(f.ctx, f.principal(), f.request("k2"))
	require.Equal(t, errs.CodeProviderUnavailable, errs.CodeOf(err))
	d, found, err := f.repo.GetByIdempotencyKey(f.ctx, testDB, f.key("k2"))
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, funding.StatusCreated, d.Status)
	f.provider.CreateErr = nil
	retried := f.start("k2")
	require.Equal(t, funding.StatusSessionCreated, retried.Deposit.Status)
	require.Equal(t, d.ID, retried.Deposit.ID)

	// A definitive provider rejection fails the deposit.
	f.provider.CreateErr = errs.New(errs.CodeEligibilityJurisdiction, "unsupported country")
	_, err = f.svc.Start(f.ctx, f.principal(), f.request("k3"))
	require.Equal(t, errs.CodeEligibilityJurisdiction, errs.CodeOf(err))
	d, _, err = f.repo.GetByIdempotencyKey(f.ctx, testDB, f.key("k3"))
	require.NoError(t, err)
	require.Equal(t, funding.StatusFailed, d.Status)
	f.provider.CreateErr = nil

	list, next, err := f.svc.ListDeposits(security.WithPrincipal(f.ctx, f.principal()), f.account, "", 2)
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.NotEmpty(t, next)
	more, next2, err := f.svc.ListDeposits(security.WithPrincipal(f.ctx, f.principal()), f.account, next, 2)
	require.NoError(t, err)
	require.Len(t, more, 1)
	require.Empty(t, next2)
	require.Equal(t, res.Deposit.ID, more[0].ID, "newest first")
}

func TestIntegration_Start_Refusals(t *testing.T) {
	t.Run("gate inactive with live provider (fake checker)", func(t *testing.T) {
		f := newFixture(t, func(c *funding.Config, _ *funding.Deps) { c.ProviderMode = config.ProviderModeLive })
		_, err := f.svc.Start(f.ctx, f.principal(), f.request("g1"))
		require.Equal(t, errs.CodeCapabilityNotApproved, errs.CodeOf(err))
		require.Equal(t, []gates.Capability{gates.LiveFunding}, f.gates.Calls())
		_, found, err := f.repo.GetByIdempotencyKey(f.ctx, testDB, f.key("g1"))
		require.NoError(t, err)
		require.False(t, found, "nothing persisted when the gate refuses")
	})
	t.Run("gate inactive with live provider (real checker, no gate row)", func(t *testing.T) {
		clk := clock.NewFake(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))
		real, err := gates.NewChecker("TEST", func(gates.Capability) bool { return true }, clk)
		require.NoError(t, err)
		f := newFixture(t, func(c *funding.Config, d *funding.Deps) { c.ProviderMode = config.ProviderModeLive; d.Gates = real })
		_, err = f.svc.Start(f.ctx, f.principal(), f.request("g2"))
		require.Equal(t, errs.CodeCapabilityNotApproved, errs.CodeOf(err))
	})
	t.Run("gate skipped for fake provider in TEST", func(t *testing.T) {
		f := newFixture(t)
		f.start("g3")
		require.Empty(t, f.gates.Calls())
	})
	t.Run("kill switch (fake checker)", func(t *testing.T) {
		f := newFixture(t)
		f.kill.Blocked = true
		_, err := f.svc.Start(f.ctx, f.principal(), f.request("k1"))
		require.Equal(t, errs.CodeKillSwitchActive, errs.CodeOf(err))
		acts := f.kill.Actions()
		require.Len(t, acts, 1)
		require.Equal(t, killswitch.NewRisk, acts[0].Class)
		require.True(t, acts[0].Funding)
		require.Equal(t, f.account.String(), acts[0].AccountID)
	})
	t.Run("kill switch FUNDING_DISABLE (real checker)", func(t *testing.T) {
		requireEnv(t)
		_, err := testDB.Exec(context.Background(), `INSERT INTO kill_switches (id, kind, scope_id, active, severity, reason, activated_by_actor_id, activated_at)
			VALUES ($1, 'FUNDING_DISABLE', '*', true, 'SEVERE', 'funding itest', 'itest', now()) ON CONFLICT (kind, scope_id) DO NOTHING`, id.New[id.Any]())
		require.NoError(t, err)
		f := newFixture(t, func(_ *funding.Config, d *funding.Deps) { d.KillSwitches = killswitch.NewChecker(killswitch.Policy{}) })
		_, err = f.svc.Start(f.ctx, f.principal(), f.request("k2"))
		require.Equal(t, errs.CodeKillSwitchActive, errs.CodeOf(err))
	})
	t.Run("frozen account", func(t *testing.T) {
		f := newFixture(t)
		require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.accounts.Transition(ctx, tx, f.account, accounts.StatusChange{To: accounts.StatusFrozen, ActorType: "OPERATOR", ActorID: "ops", Reason: "test"}, f.clk.Now())
			return err
		}))
		_, err := f.svc.Start(f.ctx, f.principal(), f.request("fz"))
		require.Equal(t, errs.CodeAccountFrozen, errs.CodeOf(err))
	})
	t.Run("agent principal", func(t *testing.T) {
		f := newFixture(t)
		_, err := f.svc.Start(f.ctx, security.AgentPrincipal("agent-1", f.account.String()), f.request("ag"))
		require.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	})
	t.Run("currency must match the settlement asset", func(t *testing.T) {
		f := newFixture(t)
		req := f.request("cur")
		req.DestinationCurrency = "sol"
		_, err := f.svc.Start(f.ctx, f.principal(), req)
		require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	})
}

// TestIntegration_FundingFlow_SettledAvailableReversed is PART 162 end to
// end with a reversal the wallet can cover.
func TestIntegration_FundingFlow_SettledAvailableReversed(t *testing.T) {
	f := newFixture(t)
	d := f.confirmed("flow", "100.000000")
	require.NotNil(t, d.ExpectedQuantity)
	require.Equal(t, q(100*oneUSDC).String(), d.ExpectedQuantity.String())
	require.Equal(t, "cxt_flow", d.ProviderRef)
	require.True(t, f.balance(ledger.CodeWallet).IsZero(), "provider confirmation alone never credits")

	// Chain receipt (99.99 USDC after a provider network fee: within 100 bps).
	observed := int64(99_990_000)
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.RecordSettlement(ctx, tx, d.ID, q(observed), "sig-flow", 4242)
	}))
	require.Equal(t, funding.StatusSettlementObserved, f.deposit(d.ID).Status)
	require.True(t, f.balance(ledger.CodeWallet).IsZero(), "an observed receipt is not yet reconciled")

	// Reconciliation posts FUNDING_SETTLED, even while FUNDING_DISABLE is active.
	f.kill.Blocked = true
	var rec funding.ReconcileResult
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		r, err := f.svc.Reconcile(ctx, tx, d.ID)
		rec = r
		return err
	}))
	f.kill.Blocked = false
	require.True(t, rec.Agreed)
	require.Equal(t, funding.StatusReconciled, rec.Deposit.Status)
	require.NotNil(t, rec.Deposit.JournalTransactionID)
	require.Equal(t, q(observed).String(), f.balance(ledger.CodeWallet).String())
	require.Equal(t, q(observed).String(), f.balance(ledger.CodeCapital).String())
	posted, err := f.ledger.Transaction(f.ctx, testDB, *rec.Deposit.JournalTransactionID)
	require.NoError(t, err)
	require.Equal(t, ledger.KindFundingSettled, posted.Kind)
	require.Equal(t, "deposit:"+d.ID.String()+":settled", posted.IdempotencyKey)

	// Reconcile is idempotent at the ledger: a second call is an illegal transition, not a double posting.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error { _, err := f.svc.Reconcile(ctx, tx, d.ID); return err })
	require.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))

	// Availability policy.
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.MarkAvailable(ctx, tx, d.ID, f.cfg.Availability)
	}))
	av := f.deposit(d.ID)
	require.Equal(t, funding.StatusAvailable, av.Status)
	require.True(t, av.BuyingPowerEligible)
	require.False(t, av.WithdrawalEligible, "funds can trade never implies funds can withdraw")
	require.Equal(t, "availability-v1", av.AvailabilityPolicyVersion)
	require.True(t, av.ReversibleUntil.Equal(f.clk.Now().Add(72*time.Hour)), "reversible_until = available_at + ReversibleFor")
	holds, err := f.capital.ActiveHolds(f.ctx, testDB, f.account.String(), f.asset.ID, f.clk.Now())
	require.NoError(t, err)
	require.Len(t, holds, 1)
	require.Equal(t, q(observed).String(), holds[0].Quantity.String())
	require.Equal(t, d.ID.String(), holds[0].DepositID)
	require.Equal(t, funding.HoldReasonReversibilityWindow, holds[0].Reason)

	// Withdrawal eligibility only after the window with a clean fraud state.
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		ids, err := f.svc.PromoteWithdrawalEligibility(ctx, tx, f.clk.Now(), 1000)
		require.NotContains(t, ids, d.ID, "window not elapsed")
		return err
	}))
	f.clk.Advance(72*time.Hour + time.Second)
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		ids, err := f.svc.PromoteWithdrawalEligibility(ctx, tx, f.clk.Now(), 1000)
		require.Contains(t, ids, d.ID, "the sweep is database-wide; other fixtures' deposits may promote too")
		return err
	}))
	require.True(t, f.deposit(d.ID).WithdrawalEligible)

	// Reversal with a sufficient wallet: one FUNDING_REVERSAL, no deficit.
	var rev funding.ReversalResult
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		r, err := f.svc.Reverse(ctx, tx, d.ID, "provider chargeback", "dispute:dp_1")
		rev = r
		return err
	}))
	require.False(t, rev.Deficit)
	require.Len(t, rev.TransactionIDs, 1)
	require.Equal(t, q(observed).String(), rev.Covered.String())
	require.True(t, rev.Shortfall.IsZero())
	require.True(t, f.balance(ledger.CodeWallet).IsZero())
	require.True(t, f.balance(ledger.CodeCapital).IsZero())
	require.True(t, f.balance(ledger.CodeDeficit).IsZero())
	rd := f.deposit(d.ID)
	require.Equal(t, funding.StatusReversed, rd.Status)
	require.Equal(t, funding.FraudConfirmed, rd.FraudState)
	require.False(t, rd.BuyingPowerEligible)
	require.False(t, rd.WithdrawalEligible)
	require.Equal(t, rev.TransactionIDs[0], *rd.ReversalJournalTransactionID)
	acct, err := f.accounts.Get(f.ctx, testDB, f.account)
	require.NoError(t, err)
	require.Equal(t, accounts.StatusActive, acct.Status, "no deficit, no freeze")
	holds, err = f.capital.ActiveHolds(f.ctx, testDB, f.account.String(), f.asset.ID, f.clk.Now())
	require.NoError(t, err)
	require.Empty(t, holds, "reversibility hold released with the reversal")
	require.Equal(t, 1, f.count(`SELECT count(*) FROM security_events WHERE account_id = $1 AND kind = 'funding_reversed'`, f.account))
	require.Equal(t, 0, f.count(`SELECT count(*) FROM security_events WHERE account_id = $1 AND kind = 'negative_deficit_accounts'`, f.account))
	require.Equal(t, 1, f.count(`SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND headers->>'event' = 'funding.reversed'`, d.ID.String()))
	require.Equal(t, 1, f.count(`SELECT count(*) FROM audit_events WHERE resource_id = $1 AND action = 'funding.reversed'`, d.ID.String()))

	trail, err := f.repo.Transitions(f.ctx, testDB, d.ID)
	require.NoError(t, err)
	var seq []funding.Status
	for _, tr := range trail {
		seq = append(seq, tr.To)
	}
	require.Equal(t, []funding.Status{
		funding.StatusSessionCreated, funding.StatusCustomerActionRequired, funding.StatusProviderProcessing, funding.StatusProviderConfirmed,
		funding.StatusSettlementObserved, funding.StatusReconciled, funding.StatusAvailable, funding.StatusReversed,
	}, seq)
	// A reversed deposit is terminal.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.Reverse(ctx, tx, d.ID, "again", "")
		return err
	})
	require.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))
}

// TestFunding_ReversalCreatesDeficitAndFreezes is FINANCIAL_MODEL §2.2: the
// customer swapped 70 of 100 USDC away; the reversal posts two balanced
// transactions, DEFICIT (credit-normal) equals the shortfall, the account
// is FROZEN and the alert row exists.
func TestFunding_ReversalCreatesDeficitAndFreezes(t *testing.T) {
	f := newFixture(t)
	d := f.available("deficit", 100*oneUSDC)
	require.Equal(t, funding.StatusAvailable, d.Status)

	// Dispose 70 USDC through a trade (Cr WALLET / Dr TRADING_OUTFLOW).
	fillID := uuid.NewString()
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindTradeFill, IdempotencyKey: "fill:" + fillID, Reference: ledger.FinancialEventReference{Type: "fill", ID: fillID},
			EffectiveAt: f.clk.Now(), Description: "swap", Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(f.account, ledger.CodeWallet, f.asset.ID), Side: ledger.Credit, Quantity: q(70 * oneUSDC)},
				{Account: ledger.CustomerAccount(f.account, ledger.CodeTradingOutflow, f.asset.ID), Side: ledger.Debit, Quantity: q(70 * oneUSDC)},
			},
		})
		return err
	}))
	require.Equal(t, q(30*oneUSDC).String(), f.balance(ledger.CodeWallet).String())

	var rev funding.ReversalResult
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		r, err := f.svc.Reverse(ctx, tx, d.ID, "provider chargeback", "dispute:dp_2")
		rev = r
		return err
	}))
	require.True(t, rev.Deficit)
	require.True(t, rev.AccountFrozen)
	require.Len(t, rev.TransactionIDs, 2)
	require.Equal(t, q(30*oneUSDC).String(), rev.Covered.String())
	require.Equal(t, q(70*oneUSDC).String(), rev.Shortfall.String())

	require.True(t, f.balance(ledger.CodeWallet).IsZero(), "WALLET never negative")
	require.True(t, f.balance(ledger.CodeCapital).IsZero())
	require.Equal(t, q(70*oneUSDC).String(), f.balance(ledger.CodeDeficit).String(), "DEFICIT credit-normal balance == shortfall")
	require.Equal(t, q(70*oneUSDC).String(), f.balance(ledger.CodeTradingOutflow).String())
	// Debit-normal (WALLET 0 + TRADING_OUTFLOW 70) == credit-normal (CAPITAL 0 + DEFICIT 70).
	debitNormal := f.balance(ledger.CodeWallet).Add(f.balance(ledger.CodeTradingOutflow))
	creditNormal := f.balance(ledger.CodeCapital).Add(f.balance(ledger.CodeDeficit))
	require.Equal(t, debitNormal.String(), creditNormal.String())

	t1, err := f.ledger.Transaction(f.ctx, testDB, rev.TransactionIDs[0])
	require.NoError(t, err)
	t2, err := f.ledger.Transaction(f.ctx, testDB, rev.TransactionIDs[1])
	require.NoError(t, err)
	require.Equal(t, ledger.KindFundingReversal, t1.Kind)
	require.Equal(t, ledger.KindFundingReversalDeficit, t2.Kind)
	for _, tr := range []ledger.Transaction{t1, t2} {
		var dr, cr money.Quantity
		for _, e := range tr.Entries {
			if e.Side == ledger.Debit {
				dr = dr.Add(e.Quantity)
			} else {
				cr = cr.Add(e.Quantity)
			}
		}
		require.Equal(t, dr.String(), cr.String(), "transaction %s balances", tr.Kind)
	}

	acct, err := f.accounts.Get(f.ctx, testDB, f.account)
	require.NoError(t, err)
	require.Equal(t, accounts.StatusFrozen, acct.Status)
	require.NotNil(t, acct.FrozenAt)
	require.Equal(t, 1, f.count(`SELECT count(*) FROM security_events WHERE account_id = $1 AND kind = 'negative_deficit_accounts' AND severity = 'CRITICAL'`, f.account))
	require.Equal(t, 1, f.count(`SELECT count(*) FROM account_status_transitions WHERE account_id = $1 AND to_status = 'FROZEN'`, f.account))
	rd := f.deposit(d.ID)
	require.Equal(t, funding.StatusReversed, rd.Status)
	require.Equal(t, rev.TransactionIDs[0], *rd.ReversalJournalTransactionID)
	require.Equal(t, 1, f.count(`SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND headers->>'event' = 'funding.reversed' AND (payload->'detail'->>'deficit')::boolean`, d.ID.String()))
}

func TestIntegration_ProviderEvents(t *testing.T) {
	f := newFixture(t)
	res := f.start("pe")
	sid := res.Session.ID

	r := f.providerEvent(sid, funding.ProviderStatusUnknown, "quote_ready_v2")
	require.Equal(t, funding.ApplyApplied, r.Outcome)
	require.Equal(t, funding.StatusReviewRequired, r.Deposit.Status, "unknown status escalates, never crashes")
	r = f.providerEvent(sid, funding.ProviderStatusConfirmed, "fulfillment_complete")
	require.Equal(t, funding.ApplyNoOp, r.Outcome, "operator owns REVIEW_REQUIRED")

	// Stale, rejected, wallet mismatch, unknown session, unknown event type.
	res2 := f.start("pe2")
	sid2 := res2.Session.ID
	r = f.providerEvent(sid2, funding.ProviderStatusProcessing, "fulfillment_processing")
	require.Equal(t, funding.StatusProviderProcessing, r.Deposit.Status, "skipping CUSTOMER_ACTION_REQUIRED is legal: webhooks are unordered")
	r = f.providerEvent(sid2, funding.ProviderStatusInitialized, "initialized")
	require.Equal(t, funding.ApplyNoOp, r.Outcome, "stale status is a no-op")
	r = f.providerEvent(sid2, funding.ProviderStatusRejected, "rejected")
	require.Equal(t, funding.StatusFailed, r.Deposit.Status)
	r = f.providerEvent(sid2, funding.ProviderStatusConfirmed, "fulfillment_complete")
	require.Equal(t, funding.ApplyNoOp, r.Outcome, "terminal deposit ignores further events")

	res3 := f.start("pe3")
	r = f.providerEvent(res3.Session.ID, funding.ProviderStatusConfirmed, "fulfillment_complete", func(s *funding.Session) {
		s.DestinationAmount, s.WalletAddress = "100.00", "AttackerWallet11111111111111111111111111111"
	})
	require.Equal(t, funding.StatusReviewRequired, r.Deposit.Status, "wallet mismatch escalates")

	res4 := f.start("pe4")
	r = f.providerEvent(res4.Session.ID, funding.ProviderStatusConfirmed, "fulfillment_complete", func(s *funding.Session) {
		s.DestinationAmount = "100.1234567"
	})
	require.Equal(t, funding.StatusReviewRequired, r.Deposit.Status, "unrepresentable amount escalates")

	res5 := f.start("pe5")
	r = f.providerEvent(res5.Session.ID, funding.ProviderStatusConfirmed, "fulfillment_complete")
	require.Equal(t, funding.StatusReviewRequired, r.Deposit.Status, "confirmation without an amount escalates")

	r = f.apply(funding.WebhookEvent{Identity: webhook.Identity{Provider: "fake", EventID: "evt_x", EventType: "session.updated"}, Session: funding.Session{ID: "cos_unknown", Status: funding.ProviderStatusConfirmed}, SessionKnown: true})
	require.Equal(t, funding.ApplyIgnored, r.Outcome)
	r = f.apply(funding.WebhookEvent{Identity: webhook.Identity{Provider: "fake", EventID: "evt_y", EventType: "something.else"}})
	require.Equal(t, funding.ApplyIgnored, r.Outcome)
	r = f.apply(funding.WebhookEvent{Identity: webhook.Identity{Provider: "other", EventID: "evt_z", EventType: "session.updated"}, Session: funding.Session{ID: sid}, SessionKnown: true})
	require.Equal(t, funding.ApplyIgnored, r.Outcome)

	// Dispatch maps outcomes for the pipeline.
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		disp, err := f.svc.Dispatch(ctx, tx, funding.WebhookEvent{Identity: webhook.Identity{Provider: "fake", EventID: "evt_w", EventType: "x"}})
		require.Equal(t, webhook.Ignored, disp)
		return err
	}))
}

func TestIntegration_Reconcile_MismatchEscalates(t *testing.T) {
	f := newFixture(t)
	d := f.confirmed("mm", "100.000000")
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.RecordSettlement(ctx, tx, d.ID, q(90*oneUSDC), "sig-mm", 1)
	}))
	var rec funding.ReconcileResult
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		r, err := f.svc.Reconcile(ctx, tx, d.ID)
		rec = r
		return err
	}))
	require.False(t, rec.Agreed)
	require.Equal(t, funding.StatusReviewRequired, rec.Deposit.Status)
	require.Equal(t, q(10*oneUSDC).String(), rec.Difference.String())
	require.True(t, f.balance(ledger.CodeWallet).IsZero(), "a disagreement never posts")
	require.Nil(t, rec.Deposit.JournalTransactionID)
}

func TestIntegration_IllegalTransitions(t *testing.T) {
	f := newFixture(t)
	res := f.start("ill")
	depositID := res.Deposit.ID
	ev := funding.SystemEvidence("test", "", "")
	cases := map[string]func(ctx context.Context, tx pgx.Tx) error{
		"SESSION_CREATED -> AVAILABLE": func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.repo.Transition(ctx, tx, depositID, funding.StatusAvailable, ev)
			return err
		},
		"MarkAvailable before RECONCILED": func(ctx context.Context, tx pgx.Tx) error {
			return f.svc.MarkAvailable(ctx, tx, depositID, f.cfg.Availability)
		},
		"Reverse before posting": func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.svc.Reverse(ctx, tx, depositID, "x", "")
			return err
		},
		"RecordSettlement before PROVIDER_CONFIRMED": func(ctx context.Context, tx pgx.Tx) error {
			return f.svc.RecordSettlement(ctx, tx, depositID, q(1), "sig", 1)
		},
		"REVIEW_REQUIRED -> RECONCILED without a posting": func(ctx context.Context, tx pgx.Tx) error {
			if _, err := f.repo.Transition(ctx, tx, depositID, funding.StatusReviewRequired, ev); err != nil {
				return err
			}
			_, err := f.repo.Transition(ctx, tx, depositID, funding.StatusReconciled, ev)
			return err
		},
	}
	for name, fn := range cases {
		err := f.inTx(fn)
		require.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err), name)
	}
	require.Equal(t, funding.StatusSessionCreated, f.deposit(depositID).Status, "failed transitions leave no trace")

	agent := funding.TransitionEvidence{ActorType: security.ActorAgent, ActorID: "agent", Reason: "x"}
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.repo.Transition(ctx, tx, depositID, funding.StatusCancelled, agent)
		return err
	})
	require.Equal(t, errs.CodeForbidden, errs.CodeOf(err))

	// The database refuses a status change without a transition row in the same transaction.
	_, err = testDB.Exec(f.ctx, `UPDATE deposits SET status = 'CANCELLED' WHERE id = $1`, depositID)
	require.Error(t, err)
	require.Equal(t, "AU001", db.SQLState(err))
	require.Equal(t, funding.StatusSessionCreated, f.deposit(depositID).Status)
}

// TestIntegration_Driver_AdvancesToAvailable runs the Temporal-free
// lifecycle driver through polling, observation, reconciliation and
// availability.
func TestIntegration_Driver_AdvancesToAvailable(t *testing.T) {
	f := newFixture(t)
	drv, err := funding.NewDriver(f.svc, f.observer)
	require.NoError(t, err)
	res := f.start("drv")
	depositID, sid := res.Deposit.ID, res.Session.ID

	require.NoError(t, drv.Advance(f.ctx, depositID))
	require.Equal(t, funding.StatusCustomerActionRequired, f.deposit(depositID).Status, "poll applies initialized")
	f.provider.SetStatus(sid, funding.ProviderStatusConfirmed, "fulfillment_complete", func(s *funding.Session) {
		s.DestinationAmount, s.TransactionID = "100.000000", "cxt_drv"
	})
	require.NoError(t, drv.Advance(f.ctx, depositID))
	require.Equal(t, funding.StatusProviderConfirmed, f.deposit(depositID).Status)

	require.NoError(t, drv.Advance(f.ctx, depositID))
	require.Equal(t, funding.StatusProviderConfirmed, f.deposit(depositID).Status, "no receipt yet: nothing changes")
	require.NotEmpty(t, f.observer.Queries())
	require.Equal(t, walletAddr, f.observer.Queries()[0].Address)

	f.observer.Credit(walletAddr, funding.ChainReceipt{Quantity: q(100 * oneUSDC), Signature: "sig-drv", Slot: 99, ObservedAt: f.clk.Now()})
	require.NoError(t, drv.Advance(f.ctx, depositID))
	require.Equal(t, funding.StatusSettlementObserved, f.deposit(depositID).Status)
	require.NoError(t, drv.Advance(f.ctx, depositID))
	require.Equal(t, funding.StatusReconciled, f.deposit(depositID).Status)
	require.NoError(t, drv.Advance(f.ctx, depositID))
	d := f.deposit(depositID)
	require.Equal(t, funding.StatusAvailable, d.Status)
	require.True(t, d.BuyingPowerEligible)
	require.False(t, d.WithdrawalEligible)
	require.Equal(t, q(100*oneUSDC).String(), f.balance(ledger.CodeWallet).String(), "underlying USDC shown")

	require.NoError(t, drv.Advance(f.ctx, depositID))
	require.False(t, f.deposit(depositID).WithdrawalEligible, "window not elapsed")
	f.clk.Advance(73 * time.Hour)
	require.NoError(t, drv.Advance(f.ctx, depositID))
	require.True(t, f.deposit(depositID).WithdrawalEligible)
	require.NoError(t, drv.Advance(f.ctx, depositID), "idempotent once eligible")

	// Settlement timeout escalates a confirmed deposit without a receipt.
	res2 := f.start("drv2")
	f.provider.SetStatus(res2.Session.ID, funding.ProviderStatusConfirmed, "fulfillment_complete", func(s *funding.Session) { s.DestinationAmount = "5.000000" })
	require.NoError(t, drv.Advance(f.ctx, res2.Deposit.ID))
	f.clk.Advance(3 * time.Hour)
	require.NoError(t, drv.Advance(f.ctx, res2.Deposit.ID))
	require.Equal(t, funding.StatusReviewRequired, f.deposit(res2.Deposit.ID).Status)

	// Abandoned sessions expire through the driver and through ExpireStale.
	res3 := f.start("drv3")
	f.clk.Advance(31 * time.Minute)
	require.NoError(t, drv.Advance(f.ctx, res3.Deposit.ID))
	require.Equal(t, funding.StatusExpired, f.deposit(res3.Deposit.ID).Status)
	res4 := f.start("drv4")
	f.clk.Advance(31 * time.Minute)
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		ids, err := f.svc.ExpireStale(ctx, tx, f.clk.Now(), 50)
		require.Contains(t, ids, res4.Deposit.ID)
		return err
	}))
	require.Equal(t, funding.StatusExpired, f.deposit(res4.Deposit.ID).Status)
}

func TestIntegration_GetDeposit_TenantScoped(t *testing.T) {
	f := newFixture(t)
	res := f.start("get")
	got, err := f.svc.GetDeposit(security.WithPrincipal(f.ctx, f.principal()), res.Deposit.ID)
	require.NoError(t, err)
	require.Equal(t, res.Deposit.ID, got.ID)
	other := f.principal()
	other.AccountIDs = []string{accounts.NewAccountID().String()}
	_, err = f.svc.GetDeposit(security.WithPrincipal(f.ctx, other), res.Deposit.ID)
	require.Equal(t, errs.CodeNotFound, errs.CodeOf(err), "cross-tenant reads are indistinguishable from missing")
	_, err = f.svc.GetDeposit(f.ctx, res.Deposit.ID)
	require.Equal(t, errs.CodeUnauthenticated, errs.CodeOf(err))
}

// TestIntegration_OutOfOrderWebhooksNeverMoveADepositBackwards is PART LXXII
// item 5.
//
// Item 4 is duplicate delivery and has its own tests. This is the other half
// and the one that actually loses money if it is wrong: providers deliver
// webhooks over independent HTTP connections with independent retries, so the
// event generated LAST routinely arrives FIRST. A system that applies whatever
// it was handed most recently will happily walk a confirmed deposit back to
// "awaiting the customer" and then re-confirm it, posting twice.
//
// The property is not "events arrive in order" — they do not, and nothing here
// can make them. It is that the deposit's status is a function of the furthest
// point the provider has ever reported, never of the last packet received.
func TestIntegration_OutOfOrderWebhooksNeverMoveADepositBackwards(t *testing.T) {
	f := newFixture(t)
	res := f.start("ooo")
	sid, depositID := res.Session.ID, res.Deposit.ID

	// Take a snapshot of each stage WITHOUT delivering it, so the deliveries
	// below are genuinely the provider's earlier statements arriving late
	// rather than a rewritten present.
	snap := func(status funding.ProviderStatus, raw string, opts ...func(*funding.Session)) funding.Session {
		f.provider.SetStatus(sid, status, raw, opts...)
		s, err := f.provider.GetSession(f.ctx, sid)
		require.NoError(t, err)
		return s
	}
	action := snap(funding.ProviderStatusCustomerActionRequired, "requires_payment")
	processing := snap(funding.ProviderStatusProcessing, "fulfillment_processing")
	confirmed := snap(funding.ProviderStatusConfirmed, "fulfillment_complete", func(s *funding.Session) {
		s.DestinationAmount, s.TransactionID = "100.000000", "cxt_ooo"
	})

	deliver := func(name string, s funding.Session) funding.ApplyResult {
		return f.apply(funding.WebhookEvent{
			Identity: webhook.Identity{
				Provider: "fake", EventID: "evt_ooo_" + name, EventType: "session.updated",
				SignedAt: f.clk.Now(), PublishedAt: f.clk.Now(),
			},
			Session: s, SessionKnown: true,
		})
	}

	// Backwards: the newest event first, then the two older ones.
	r := deliver("confirmed", confirmed)
	require.Equal(t, funding.ApplyApplied, r.Outcome)
	require.Equal(t, funding.StatusProviderConfirmed, r.Deposit.Status)

	r = deliver("processing", processing)
	require.Equal(t, funding.ApplyNoOp, r.Outcome, "a late earlier event must not be applied")
	require.Equal(t, funding.StatusProviderConfirmed, r.Deposit.Status)
	require.Contains(t, r.Reason, "does not advance")

	r = deliver("action", action)
	require.Equal(t, funding.ApplyNoOp, r.Outcome)
	require.Equal(t, funding.StatusProviderConfirmed, r.Deposit.Status)

	// The deposit records the states it actually entered, not the ones the
	// provider mentioned afterwards. Skipping forward is legal precisely
	// because webhooks are unordered; skipping BACKWARD is not a thing.
	d := f.deposit(depositID)
	require.Equal(t, funding.StatusProviderConfirmed, d.Status)
	require.NotNil(t, d.ProviderConfirmedAt)
	var processingAt, actionAt *time.Time
	require.NoError(t, testDB.QueryRow(f.ctx,
		`SELECT provider_processing_at, customer_action_at FROM deposits WHERE id = $1`,
		depositID).Scan(&processingAt, &actionAt))
	require.Nil(t, processingAt, "a state the deposit never entered must not be stamped")
	require.Nil(t, actionAt)

	require.Equal(t, 1, f.count(
		`SELECT count(*) FROM deposit_transitions WHERE deposit_id = $1 AND to_status = 'PROVIDER_CONFIRMED'`, depositID,
	))
	require.Equal(t, 0, f.count(
		`SELECT count(*) FROM deposit_transitions WHERE deposit_id = $1 AND to_status IN ('PROVIDER_PROCESSING','CUSTOMER_ACTION_REQUIRED')`, depositID,
	))

	// And the out-of-order stream still ends where an ordered one would, with
	// the customer credited exactly once.
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.RecordSettlement(ctx, tx, depositID, q(100*oneUSDC), "sig-ooo", 7)
	}))
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		rec, err := f.svc.Reconcile(ctx, tx, depositID)
		require.True(t, rec.Agreed)
		return err
	}))
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.MarkAvailable(ctx, tx, depositID, f.cfg.Availability)
	}))
	require.Equal(t, q(100*oneUSDC).String(), f.balance(ledger.CodeWallet).String())

	// A straggler arriving after the money is available changes nothing. This
	// is the delivery that would be most expensive to get wrong, because by
	// now there are postings behind the deposit.
	r = deliver("confirmed-again", confirmed)
	require.Equal(t, funding.ApplyNoOp, r.Outcome)
	require.Equal(t, funding.StatusAvailable, r.Deposit.Status)
	require.Equal(t, q(100*oneUSDC).String(), f.balance(ledger.CodeWallet).String(),
		"a late webhook must not post a second credit")
	require.Equal(t, 1, f.count(
		`SELECT count(*) FROM deposit_transitions WHERE deposit_id = $1 AND to_status = 'PROVIDER_CONFIRMED'`, depositID,
	))
}

// TestIntegration_ARejectionArrivingAfterConfirmationEscalates covers the one
// out-of-order case that must NOT be a silent no-op.
//
// A rejection is not on the main chain, so the rank comparison that discards
// stale forward states says nothing about it. If the provider says "delivered"
// and then "rejected", one of those two statements is false and there is no
// way to tell which from here — so the deposit goes to a human rather than
// being failed on the second statement or ignored on the strength of the
// first. Failing it would strand a deposit whose crypto may already be on
// chain; ignoring it would discard a report of fraud.
func TestIntegration_ARejectionArrivingAfterConfirmationEscalates(t *testing.T) {
	f := newFixture(t)
	d := f.confirmed("rej-after", "100.000000")
	require.Equal(t, funding.StatusProviderConfirmed, d.Status)

	r := f.providerEvent(d.ProviderSessionID, funding.ProviderStatusRejected, "rejected")
	require.Equal(t, funding.ApplyApplied, r.Outcome)
	require.Equal(t, funding.StatusReviewRequired, r.Deposit.Status,
		"a contradiction between two provider statements is a question for a human")
	require.Contains(t, r.Reason, "rejection after confirmation")
	require.True(t, f.balance(ledger.CodeWallet).IsZero(), "an escalation posts nothing")
}
