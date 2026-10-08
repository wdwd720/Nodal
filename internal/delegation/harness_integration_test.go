//go:build integration

package delegation

import (
	"context"
	"crypto/sha256"
	"errors"
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
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
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
		fmt.Fprintln(os.Stderr, "delegation integration: migrate up:", err)
		return 1
	}
	var err error
	if testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "delegation-itest", MaxConns: 40,
		LockTimeout: 60 * time.Second, StatementTimeout: 120 * time.Second}); err != nil {
		fmt.Fprintln(os.Stderr, "delegation integration: open:", err)
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

// The devnet runs fast in tests: a 12-block blockhash window and finality
// after 2 blocks.
const (
	itWindow = 12
	itDepth  = 2
	itFee    = 10
)

type nopEmitter struct{}

func (nopEmitter) Emit(context.Context, pgx.Tx, string, any) error { return nil }

type fixture struct {
	t        *testing.T
	ctx      context.Context
	clk      *clock.Fake
	tag      string
	host     *devnethost.Host
	svc      *Service
	deps     Deps
	signer   *DelegatedSigner
	venueKey *VenueSigner
	ledger   *ledger.Service
	capital  *capital.Service
	prices   *valuation.PriceStore
	effects  *econ.EffectStore
	intents  *econ.IntentStore
	recon    *reconciliation.Repository
	mintAuth devnet.Keypair
	usd      assets.Asset
	tok      assets.Asset
	house    econ.HouseAccount
	venue    Venue
}

var operator = Actor{Type: security.ActorOperator, ID: "delegation-itest-operator"}

func seedOf(label string) [32]byte { return sha256.Sum256([]byte("delegation-itest/" + label)) }

// newFixture wires the real service against the real database and a fresh
// devnet history of its own. Every asset, mint, house book and venue is the
// fixture's own, so tests never see each other's books.
func newFixture(t *testing.T) *fixture {
	requireEnv(t)
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	tag := strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	cfg := devnethost.DefaultConfig("dg-"+tag, config.EnvTest, devnet.NewKeypairFromSeed(seedOf("attester")).PublicKey())
	for i := range cfg.Chains {
		cfg.Chains[i].BlockhashWindow, cfg.Chains[i].FinalityDepth, cfg.Chains[i].FeePerTx = itWindow, itDepth, itFee
	}
	host, err := devnethost.Open(ctx, devnethost.NewPgStore(testDB), cfg, clk)
	require.NoError(t, err)
	out := event.NewOutbox(clk)
	aw := audit.NewWriterWithBuildVersion("delegation-itest")
	checker, err := gates.NewChecker("TEST", func(gates.Capability) bool { return true }, clk)
	require.NoError(t, err)
	kills := killswitch.NewChecker(killswitch.Policy{})
	router, err := legalrouter.New(legalrouter.ConservativePolicy())
	require.NoError(t, err)
	signer, err := NewDelegatedSigner(seedOf("master-"+tag), SignerConfig{Environment: config.EnvTest, Clock: clk,
		Policy: DefaultSigningPolicy(), Fees: HostFees(host)})
	require.NoError(t, err)
	f := &fixture{
		t: t, ctx: ctx, clk: clk, tag: tag, host: host, signer: signer, venueKey: NewVenueSigner(seedOf("venue-" + tag)),
		ledger: ledger.NewService(clk, "delegation-itest"), capital: capital.NewService(clk, nopEmitter{}),
		prices: valuation.NewPriceStore(clk), effects: econ.NewEffectStore(clk, out, aw), intents: econ.NewIntentStore(clk, out, aw),
		recon: reconciliation.NewRepository(clk, out, aw), mintAuth: devnet.NewKeypairFromSeed(seedOf("mint-authority-" + tag)),
	}
	f.deps = Deps{
		DB: testDB, Clock: clk, Environment: config.EnvTest, Host: host, Intents: f.intents, Effects: f.effects,
		Authorizer: econ.NewAuthorizer(checker, kills), Kills: kills, Ledger: f.ledger, Capital: f.capital, Risk: risk.NewStore(),
		RiskPolicy: risk.DefaultAdvancedPolicy(), Router: router, Prices: f.prices, Identity: compliance.NewAccountVerification(clk),
		Profiles: compliance.NewRepository(aw), Reconciliation: f.recon, Audit: aw, Accounts: accounts.NewRepository(),
		Assets: assets.NewRepository(), Signer: signer, Venues: []*VenueSigner{f.venueKey},
	}
	f.rebuild(nil)
	_, err = econ.EnsureSandboxDollar(ctx, testDB)
	require.NoError(t, err)

	require.NoError(t, host.Airdrop(ctx, devnethost.ChainSolana, f.mintAuth.PublicKey(), 1_000_000_000))
	usdMint := devnet.NewKeypairFromSeed(seedOf("usd-mint-" + tag)).PublicKey()
	tokMint := devnet.NewKeypairFromSeed(seedOf("tok-mint-" + tag)).PublicKey()
	require.NoError(t, host.EnsureMint(ctx, devnethost.ChainSolana, devnet.MintConfig{Address: usdMint, Decimals: 6, Authority: f.mintAuth.PublicKey()}))
	require.NoError(t, host.EnsureMint(ctx, devnethost.ChainSolana, devnet.MintConfig{Address: tokMint, Decimals: 9, Authority: f.mintAuth.PublicKey()}))
	f.usd = f.asset("DU"+strings.ToUpper(tag[:6]), usdMint, 6, true, assets.RiskSettlement)
	f.tok = f.asset("DT"+strings.ToUpper(tag[:6]), tokMint, 9, false, assets.RiskMajor)
	f.priceTok()

	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		f.house, err = econ.NewRegistry().CreateHouseAccount(ctx, tx, "dg-house-"+tag, econ.HouseLiquidityProvision, operator)
		if err != nil {
			return err
		}
		f.venue, err = f.svc.RegisterVenue(ctx, tx, VenueSpec{Name: "dg-" + tag, Chain: devnethost.ChainSolana,
			Address: f.venueKey.PublicKey(), HouseAccountID: f.house.ID, SpreadBPS: 50}, operator)
		return err
	}))
	// The venue's inventory, in its house book and on chain.
	f.fundHouse(f.tok, 1_000_000_000_000) // 1,000 tokens
	f.fundHouse(f.usd, 1_000_000_000)     // 1,000 dollars
	f.mintTo(f.venueKey.PublicKey(), f.tok, 1_000_000_000_000)
	f.mintTo(f.venueKey.PublicKey(), f.usd, 1_000_000_000)
	return f
}

// rebuild re-wires the service after mutating its dependencies.
func (f *fixture) rebuild(mutate func(*Deps)) {
	f.t.Helper()
	if mutate != nil {
		mutate(&f.deps)
	}
	svc, err := New(f.deps)
	require.NoError(f.t, err)
	f.svc = svc
}

func (f *fixture) inTx(fn func(ctx context.Context, tx pgx.Tx) error) error {
	return testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, fn)
}

func (f *fixture) asset(symbol string, mint devnet.PublicKey, decimals uint8, stable bool, class assets.RiskClass) assets.Asset {
	f.t.Helper()
	a := assets.Asset{Chain: string(devnethost.ChainSolana), MintAddress: mint.String(), Kind: assets.KindSimulated,
		ValueDomain: valuedomain.Simulated, Symbol: symbol, Name: symbol + " (simulated devnet token)", Decimals: decimals,
		RiskClass: class, Status: assets.StatusActive}
	if stable {
		a.IsStablecoin, a.PegCurrency = true, valuation.USDPeg
	}
	out, err := assets.NewRepository().Create(f.ctx, testDB, a)
	require.NoError(f.t, err)
	return out
}

// priceTok records the token at 100.00 sandbox dollars, observed now.
func (f *fixture) priceTok() {
	f.t.Helper()
	_, err := f.prices.RecordPrice(f.ctx, testDB, valuation.PriceObservation{AssetID: f.tok.ID, QuoteAssetID: econ.SandboxDollarID(),
		Mantissa: money.QuantityFromInt64(10_000), Scale: 2, Source: "delegation-itest", ObservedAt: f.clk.Now()})
	require.NoError(f.t, err)
}

// advance moves the clock and refreshes the token's price.
func (f *fixture) advance(d time.Duration) {
	f.clk.Advance(d)
	f.priceTok()
}

func (f *fixture) fundHouse(a assets.Asset, q int64) {
	f.t.Helper()
	qty := money.QuantityFromInt64(q)
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := f.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindHouseFunding, IdempotencyKey: "dg-house:" + ref,
			Reference: ledger.FinancialEventReference{Type: "house_funding", ID: ref}, EffectiveAt: f.clk.Now(),
			Entries: []ledger.Entry{
				{Account: f.house.Account(ledger.CodeHouseInventory, a.ID), Side: ledger.Debit, Quantity: qty},
				{Account: f.house.Account(ledger.CodeHouseCapital, a.ID), Side: ledger.Credit, Quantity: qty},
			},
		})
		return err
	}))
}

// mintTo issues q base units of a fixture asset to owner on chain and waits
// for finality.
func (f *fixture) mintTo(owner devnet.PublicKey, a assets.Asset, q uint64) {
	f.t.Helper()
	mint, err := devnet.ParsePublicKey(a.MintAddress)
	require.NoError(f.t, err)
	bh, err := f.host.LatestBlockhash(f.ctx, devnethost.ChainSolana)
	require.NoError(f.t, err)
	tx, err := devnet.Sign(devnet.Message{ChainID: devnethost.ChainSolana, RecentBlockhash: bh, FeePayer: f.mintAuth.PublicKey(),
		Instructions: []devnet.Instruction{
			devnet.TokenMintTo{Mint: mint, ToOwner: owner, Amount: q, Authority: f.mintAuth.PublicKey()},
			devnet.Memo{Data: []byte(uuid.NewString())},
		}}, f.mintAuth)
	require.NoError(f.t, err)
	raw, err := tx.Encode()
	require.NoError(f.t, err)
	txID, err := f.host.Submit(f.ctx, devnethost.ChainSolana, raw)
	require.NoError(f.t, err)
	_, err = f.host.ProduceBlocks(f.ctx, devnethost.ChainSolana, 1+itDepth)
	require.NoError(f.t, err)
	st, err := f.host.Status(f.ctx, devnethost.ChainSolana, txID)
	require.NoError(f.t, err)
	require.Equal(f.t, devnet.TxFinalized, st.State, "mint %s", st.Err)
}

func (f *fixture) chainBalance(owner devnet.PublicKey, a assets.Asset) uint64 {
	f.t.Helper()
	mint, err := devnet.ParsePublicKey(a.MintAddress)
	require.NoError(f.t, err)
	b, err := f.host.TokenBalance(f.ctx, devnethost.ChainSolana, owner, mint)
	require.NoError(f.t, err)
	return b
}

func (f *fixture) balance(acct accounts.AccountID, code ledger.Code, a assets.Asset) money.Quantity {
	f.t.Helper()
	b, err := f.ledger.Balance(f.ctx, testDB, ledger.CustomerAccount(acct, code, a.ID))
	require.NoError(f.t, err)
	return b
}

// --- Customers and their agents ------------------------------------------------------

type customer struct {
	user       accounts.UserID
	account    accounts.AccountID
	agentID    string
	strategyID string
	wallet     devnet.PublicKey
}

// customer creates a verified, sanctions-clear user with one ACTIVE customer
// account, a strategy and an agent for it, and funds the account's delegated
// wallet with usd dollars (books and chain).
func (f *fixture) customer(usd int64) customer {
	f.t.Helper()
	repo := accounts.NewRepository()
	u, err := repo.CreateUser(f.ctx, testDB, "delegation-itest", uuid.NewString(), nil)
	require.NoError(f.t, err)
	a, err := repo.CreateAccount(f.ctx, testDB, u.ID, accounts.KindCustomer)
	require.NoError(f.t, err)
	verifiedAt := f.clk.Now().Add(-24 * time.Hour)
	strategyID, versionID, agentID := id.New[id.Any](), id.New[id.Any](), id.New[id.Any]()
	hash := sha256.Sum256([]byte(versionID.String()))
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := compliance.NewRepository(audit.NewWriterWithBuildVersion("itest")).Upsert(ctx, tx, compliance.Profile{
			UserID: u.ID, IdentityState: compliance.IdentityVerified, SanctionsState: compliance.SanctionsClear, VerifiedAt: &verifiedAt,
		}, compliance.Change{ActorType: security.ActorSystem, ActorID: "itest-idv", Reason: "fixture"}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO strategies (id, owner_account_id, owner_user_id, name, source_kind, status, created_by_actor_type, created_by_actor_id)
			VALUES ($1, $2, $3, $4, 'TYPESCRIPT_SDK', 'ACTIVE', 'USER', $5)`, strategyID, a.ID, u.ID, "strategy-"+strategyID.String(), u.ID.String()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO strategy_versions (id, strategy_id, version, schema_version, ir, ir_hash, effect_set, status, source_kind, source_hash,
				compiler_version, risk_policy_version, risk_policy_hash, model_budget, data_budget, envelope_requirements, human_readable, built_at)
			VALUES ($1, $2, 1, 1, '{}'::jsonb, $3, '{}', 'COMPILED', 'TYPESCRIPT_SDK', $3, 'test', 'risk-v1', $3, '{}'::jsonb, '{}'::jsonb, '{}'::jsonb, 'test strategy', $4)`,
			versionID, strategyID, hash[:], f.clk.Now()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO agents (id, account_id, strategy_id, strategy_version_id, name, stage, state, created_by_actor_type, created_by_actor_id)
			VALUES ($1, $2, $3, $4, $5, 'VALIDATED', 'VALIDATED', 'USER', $6)`, agentID, a.ID, strategyID, versionID, "agent-"+agentID.String(), u.ID.String())
		return err
	}))
	c := customer{user: u.ID, account: a.ID, agentID: agentID.String(), strategyID: strategyID.String(),
		wallet: f.signer.PublicKey(a.ID, devnethost.ChainSolana)}
	if usd > 0 {
		f.fundCustomer(c, f.usd, usd)
	}
	return c
}

// fundCustomer credits q base units of a fixture asset to the customer's
// books (SANDBOX_FUNDING) and the same units to its delegated wallet on chain.
func (f *fixture) fundCustomer(c customer, a assets.Asset, q int64) {
	f.t.Helper()
	qty := money.QuantityFromInt64(q)
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := f.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindSandboxFunding, IdempotencyKey: "dg-fund:" + ref,
			Reference: ledger.FinancialEventReference{Type: "sandbox_funding", ID: ref}, EffectiveAt: f.clk.Now(),
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(c.account, ledger.CodeWallet, a.ID), Side: ledger.Debit, Quantity: qty},
				{Account: ledger.CustomerAccount(c.account, ledger.CodeCapital, a.ID), Side: ledger.Credit, Quantity: qty},
			},
		})
		return err
	}))
	f.mintTo(c.wallet, a, uint64(q))
}

// owner is the customer's principal: the account's owner, strongly
// authenticated now.
func (f *fixture) owner(c customer) context.Context {
	return security.WithPrincipal(f.ctx, security.Principal{SubjectID: c.user.String(), ActorType: security.ActorUser,
		Roles: []security.Role{security.RoleCustomer}, AccountIDs: []string{c.account.String()}, AuthTime: f.clk.Now(), AMR: []string{"mfa"}})
}

// agent is the customer's agent's principal.
func (f *fixture) agent(c customer) context.Context {
	return security.WithPrincipal(f.ctx, security.Principal{SubjectID: c.agentID, ActorType: security.ActorAgent,
		AccountIDs: []string{c.account.String()}})
}

func (f *fixture) operatorCtx() context.Context {
	return security.WithPrincipal(f.ctx, security.Principal{SubjectID: operator.ID, ActorType: security.ActorOperator,
		Roles: []security.Role{security.RoleOperations}, AuthTime: f.clk.Now(), AMR: []string{"mfa"}})
}

// grantRequest is a generous sandbox grant to the customer's agent.
func (f *fixture) grantRequest(c customer, extra ...devnet.PublicKey) GrantRequest {
	return GrantRequest{
		AccountID: c.account, AgentID: c.agentID, Mode: gates.ModeSandbox,
		InstrumentClasses: []InstrumentClass{ClassStableValue, ClassSpotToken},
		ActionClasses:     []ActionClass{ActionAcquireExposure, ActionReduceExposure, ActionProposeWithdrawal},
		MaxNotional:       money.USDFromMinor(1_000_000), MaxDailyNotional: money.USDFromMinor(500_000),
		MaxSingleTrade:       money.USDFromMinor(100_000),
		DestinationAllowlist: append([]devnet.PublicKey{f.venueKey.PublicKey()}, extra...),
		ChainAllowlist:       []devnet.ChainID{devnethost.ChainSolana}, ExpiresAt: f.clk.Now().Add(24 * time.Hour),
		IdempotencyKey: "grant-" + uuid.NewString(),
	}
}

func (f *fixture) grant(c customer, mutate func(*GrantRequest), extra ...devnet.PublicKey) Capability {
	f.t.Helper()
	req := f.grantRequest(c, extra...)
	if mutate != nil {
		mutate(&req)
	}
	var out Capability
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, _, err = f.svc.GrantCapability(f.owner(c), tx, req)
		return err
	}))
	return out
}

func (f *fixture) revoke(ctx context.Context, cid CapabilityID) (Capability, error) {
	var out Capability
	err := f.inTx(func(_ context.Context, tx pgx.Tx) error {
		var err error
		out, err = f.svc.RevokeCapability(ctx, tx, RevokeRequest{CapabilityID: cid, Reason: "itest revocation"})
		return err
	})
	return out, err
}

func (f *fixture) swapRequest(c customer, cid CapabilityID, nonce uint64, amountIn int64) SwapRequest {
	return SwapRequest{CapabilityID: cid, AccountID: c.account, Nonce: nonce, Action: ActionAcquireExposure, VenueID: f.venue.ID,
		PayAssetID: f.usd.ID, ReceiveAssetID: f.tok.ID, AmountIn: money.QuantityFromInt64(amountIn), MaxSlippageBPS: 100,
		Jurisdiction: "US-CA", CorrelationID: "corr-" + uuid.NewString()}
}

// requestSwap runs RequestSwap as the agent in its own transaction. A
// recorded refusal is committed (that is what makes it a record) and
// returned as the error; any other error rolls back.
func (f *fixture) requestSwap(ctx context.Context, req SwapRequest) (SwapTicket, error) {
	var ticket SwapTicket
	err := f.inTx(func(_ context.Context, tx pgx.Tx) error {
		var err error
		ticket, err = f.svc.RequestSwap(ctx, tx, req)
		return err
	})
	if err != nil {
		return SwapTicket{}, err
	}
	return ticket, ticket.Refusal
}

func (f *fixture) mustSwap(c customer, cid CapabilityID, nonce uint64, amountIn int64) Swap {
	f.t.Helper()
	ticket, err := f.requestSwap(f.agent(c), f.swapRequest(c, cid, nonce, amountIn))
	require.NoError(f.t, err)
	return ticket.Swap
}

func (f *fixture) swap(sid SwapID) Swap {
	f.t.Helper()
	sw, err := f.svc.GetSwap(f.ctx, testDB, sid)
	require.NoError(f.t, err)
	return sw
}

func (f *fixture) intentOf(iid econ.IntentID) econ.Intent {
	f.t.Helper()
	in, err := f.intents.Get(f.ctx, testDB, iid)
	require.NoError(f.t, err)
	return in
}

func (f *fixture) reservationOf(rid capital.ReservationID) capital.Reservation {
	f.t.Helper()
	r, err := f.capital.Get(f.ctx, testDB, rid)
	require.NoError(f.t, err)
	return r
}

func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, testDB.QueryRow(f.ctx, sql, args...).Scan(&n))
	return n
}

// ownerConn is a connection as the migration owner, which holds every
// privilege: what is still refused there is refused by a trigger.
func ownerConn(t *testing.T) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), testMigrateURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// rawTx runs fn in its own transaction on conn and commits.
func rawTx(ctx context.Context, conn interface {
	Begin(context.Context) (pgx.Tx, error)
}, fn func(tx pgx.Tx) error) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

func requireCode(t *testing.T, err error, code errs.Code) {
	t.Helper()
	require.Error(t, err)
	require.Equal(t, code, errs.CodeOf(err), "%v", err)
}

func requireSQLState(t *testing.T, err error, state string) {
	t.Helper()
	require.Error(t, err)
	require.Equal(t, state, db.SQLState(err), "%v", err)
}

func requireReason(t *testing.T, err error, reason string) {
	t.Helper()
	require.Error(t, err)
	var e *errs.Error
	require.True(t, errors.As(err, &e), "%v", err)
	got := reasonOf(err)
	require.True(t, strings.Contains(got, reason) || strings.Contains(err.Error(), reason), "want %s, got %s (%v)", reason, got, err)
}

func legalrouterConservative() (*legalrouter.Router, error) {
	return legalrouter.New(legalrouter.ConservativePolicy())
}
