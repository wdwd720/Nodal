//go:build integration

package stableasset

import (
	"context"
	"crypto/sha256"
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
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/event"
	"github.com/nodal/controlplane/internal/gates"
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
		fmt.Fprintln(os.Stderr, "stableasset integration: migrate up:", err)
		return 1
	}
	var err error
	if testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "stableasset-itest", MaxConns: 40}); err != nil {
		fmt.Fprintln(os.Stderr, "stableasset integration: open:", err)
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

// Short chains: a 12-block window and finality two blocks deep.
const (
	itWindow = 12
	itDepth  = 2
	oracle   = "sandbox-oracle"
)

type nopEmitter struct{}

func (nopEmitter) Emit(context.Context, pgx.Tx, string, any) error { return nil }

type fixture struct {
	t       *testing.T
	ctx     context.Context
	clk     *clock.Fake
	host    *devnethost.Host
	svc     *Service
	ledger  *ledger.Service
	capital *capital.Service
	prices  *valuation.PriceStore
	recon   *reconciliation.Repository
	effects *econ.EffectStore
	intents *econ.IntentStore
	issuer  *IssuerSigner
	custody *CustodialWalletSigner
	snaps   *SnapshotSigner
	network string
	// quote is the peg's unit of account; cash and tbill are the reserve
	// assets. Every symbol is unique to the fixture: the platform's one
	// sandbox dollar belongs to econ and is never created here.
	quote, cash, tbill assets.AssetID
}

var operator = econ.Actor{Type: security.ActorOperator, ID: "operator-1"}

func newFixture(t *testing.T) *fixture {
	requireEnv(t)
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	out := event.NewOutbox(clk)
	aw := audit.NewWriterWithBuildVersion("stableasset-itest")
	tag := uuid.NewString()[:8]
	network := "sa-" + tag
	cfg := devnethost.DefaultConfig(network, config.EnvTest, devnet.NewKeypairFromSeed(sha256.Sum256([]byte("attester"))).PublicKey())
	for i := range cfg.Chains {
		cfg.Chains[i].BlockhashWindow, cfg.Chains[i].FinalityDepth, cfg.Chains[i].FeePerTx = itWindow, itDepth, 10
	}
	host, err := devnethost.Open(ctx, devnethost.NewPgStore(testDB), cfg, clk)
	require.NoError(t, err)
	checker, err := gates.NewChecker("TEST", func(gates.Capability) bool { return true }, clk)
	require.NoError(t, err)
	router, err := legalrouter.New(legalrouter.ConservativePolicy())
	require.NoError(t, err)
	f := &fixture{
		t: t, ctx: ctx, clk: clk, host: host, network: network,
		ledger: ledger.NewService(clk, "stableasset-itest"), capital: capital.NewService(clk, nopEmitter{}),
		prices: valuation.NewPriceStore(clk), recon: reconciliation.NewRepository(clk, out, aw),
		effects: econ.NewEffectStore(clk, out, aw), intents: econ.NewIntentStore(clk, out, aw),
		issuer: NewIssuerSigner(seed("issuer-" + tag)), custody: NewCustodialWalletSigner(seed("custody-" + tag)),
		snaps: NewSnapshotSigner(seed("snapshots-" + tag)),
	}
	f.svc, err = New(Deps{
		DB: testDB, Clock: clk, Host: host, Intents: f.intents, Effects: f.effects,
		Authorizer: econ.NewAuthorizer(checker, killswitch.NewChecker(killswitch.Policy{})),
		Facilities: econ.NewRegistry(), Ledger: f.ledger, Capital: f.capital, Risk: risk.NewStore(),
		RiskPolicy: risk.DefaultAdvancedPolicy(), Router: router, Prices: f.prices,
		Kills: killswitch.NewChecker(killswitch.Policy{}), Reconciliation: f.recon, Audit: aw,
		Accounts: accounts.NewRepository(), Assets: assets.NewRepository(),
		Issuer: f.issuer, Custody: f.custody, Snapshots: f.snaps,
	})
	require.NoError(t, err)
	f.quote = f.simAsset("UREF"+tag[:4], 6)
	f.cash = f.simAsset("RCSH"+tag[:4], 6)
	f.tbill = f.simAsset("RTBL"+tag[:4], 6)
	f.price(f.cash, 1_000_000, 6)
	f.price(f.tbill, 1_000_000, 6)
	return f
}

func (f *fixture) simAsset(symbol string, decimals uint8) assets.AssetID {
	f.t.Helper()
	a, err := assets.NewRepository().Create(f.ctx, testDB, assets.Asset{
		Chain: assets.SandboxChain, Kind: assets.KindSimulated, ValueDomain: valuedomain.Simulated,
		Symbol: symbol, Name: symbol + " (sandbox test asset)", Decimals: decimals,
		RiskClass: assets.RiskSettlement, Status: assets.StatusActive,
	})
	require.NoError(f.t, err)
	return a.ID
}

// price records an oracle observation of asset in the quote unit, now.
func (f *fixture) price(asset assets.AssetID, mantissa int64, scale int32) {
	f.t.Helper()
	_, err := f.prices.RecordPrice(f.ctx, testDB, valuation.PriceObservation{
		AssetID: asset, QuoteAssetID: f.quote, Mantissa: money.QuantityFromInt64(mantissa), Scale: scale,
		Source: oracle, ObservedAt: f.clk.Now(),
	})
	require.NoError(f.t, err)
}

// definition is a fully reserved 6-decimal stable asset pegged at 1.000000
// quote unit, paid in the cash reserve, with a tokenized reserve at 90%.
func (f *fixture) definition() Definition {
	return Definition{
		Name: "itest-" + uuid.NewString()[:12], Symbol: "PSA" + uuid.NewString()[:4], PegCurrency: "USD", Decimals: 6,
		IssuanceCap: money.QuantityFromInt64(1_000_000_000_000), RedemptionMode: RedemptionInstant,
		PegQuoteAssetID: f.quote, PegMantissa: money.QuantityFromInt64(1_000_000), PegScale: 6,
		OracleSource: oracle, MaxPriceAge: time.Hour, PegDeviationThreshold: 50, Attestable: true,
		Reserves: []ReserveSpec{
			{AssetID: f.cash, Kind: ReserveCashEquivalent, FactorBPS: 10_000, AcceptedForMint: true},
			{AssetID: f.tbill, Kind: ReserveTokenized, FactorBPS: 9_000},
		},
	}
}

func (f *fixture) register(mutate ...func(*Definition)) PlatformStableAsset {
	f.t.Helper()
	def := f.definition()
	for _, m := range mutate {
		m(&def)
	}
	a, err := f.svc.Register(f.ctx, def, operator)
	require.NoError(f.t, err)
	f.price(a.AssetID, 1_000_000, 6) // the market trades at the peg
	return a
}

type customer struct {
	id    accounts.AccountID
	ctx   context.Context
	actor econ.Actor
}

func (f *fixture) customer() customer {
	f.t.Helper()
	repo := accounts.NewRepository()
	u, err := repo.CreateUser(f.ctx, testDB, "stableasset-itest", uuid.NewString(), nil)
	require.NoError(f.t, err)
	acct, err := repo.CreateAccount(f.ctx, testDB, u.ID, accounts.KindCustomer)
	require.NoError(f.t, err)
	p := security.Principal{SubjectID: u.ID.String(), ActorType: security.ActorUser, AccountIDs: []string{acct.ID.String()}}
	return customer{id: acct.ID, ctx: security.WithPrincipal(f.ctx, p), actor: econ.Actor{Type: security.ActorUser, ID: u.ID.String()}}
}

// fund credits sandbox value to a customer's ledger WALLET (SANDBOX_FUNDING).
func (f *fixture) fund(c customer, asset assets.AssetID, q int64) {
	f.t.Helper()
	qty := money.QuantityFromInt64(q)
	require.NoError(f.t, testDB.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		ref := uuid.NewString()
		_, err := f.ledger.Post(ctx, tx, ledger.Posting{
			Kind: ledger.KindSandboxFunding, IdempotencyKey: "sbx-fund:" + ref,
			Reference: ledger.FinancialEventReference{Type: "sandbox_funding", ID: ref}, EffectiveAt: f.clk.Now(),
			Entries: []ledger.Entry{
				{Account: ledger.CustomerAccount(c.id, ledger.CodeWallet, asset), Side: ledger.Debit, Quantity: qty},
				{Account: ledger.CustomerAccount(c.id, ledger.CodeCapital, asset), Side: ledger.Credit, Quantity: qty},
			},
		})
		return err
	}))
}

var facts = AccountFacts{Jurisdiction: "US-CA", Verification: valuedomain.VerificationEnhanced, IdentityVerified: true}

func (f *fixture) mintRequest(c customer, a PlatformStableAsset, q int64, key string, mode gates.Mode) MintRequest {
	return MintRequest{StableAssetID: a.ID, AccountID: c.id, Quantity: money.QuantityFromInt64(q), ReserveAssetID: f.cash,
		IdempotencyKey: key, Mode: mode, Actor: c.actor, Facts: facts}
}

func (f *fixture) mint(c customer, a PlatformStableAsset, q int64) (MintResult, error) {
	return f.svc.Mint(c.ctx, f.mintRequest(c, a, q, "mint-"+uuid.NewString(), gates.ModeSandbox))
}

func (f *fixture) mustMint(c customer, a PlatformStableAsset, q int64) MintRecord {
	f.t.Helper()
	res, err := f.mint(c, a, q)
	require.NoError(f.t, err)
	require.Equal(f.t, MintIssued, res.Request.Status, "failure %q", res.Request.FailureCode)
	return res.Request
}

func (f *fixture) burnRequest(c customer, a PlatformStableAsset, q int64, key string, mode gates.Mode) BurnRequest {
	return BurnRequest{StableAssetID: a.ID, AccountID: c.id, Quantity: money.QuantityFromInt64(q), IdempotencyKey: key,
		Mode: mode, Actor: c.actor, Facts: facts}
}

func (f *fixture) burn(c customer, a PlatformStableAsset, q int64) (BurnResult, error) {
	return f.svc.Burn(c.ctx, f.burnRequest(c, a, q, "burn-"+uuid.NewString(), gates.ModeSandbox))
}

func (f *fixture) balance(ref ledger.AccountRef) string {
	f.t.Helper()
	b, err := f.ledger.Balance(f.ctx, testDB, ref)
	require.NoError(f.t, err)
	return b.String()
}

func (f *fixture) wallet(c customer, asset assets.AssetID) string {
	return f.balance(ledger.CustomerAccount(c.id, ledger.CodeWallet, asset))
}

func (f *fixture) chainBalance(c customer, a PlatformStableAsset) uint64 {
	f.t.Helper()
	b, err := f.host.TokenBalance(f.ctx, a.Chain, f.custody.HolderAddress(c.id), a.Mint)
	require.NoError(f.t, err)
	return b
}

func (f *fixture) chainSupply(a PlatformStableAsset) uint64 {
	f.t.Helper()
	s, err := f.host.Supply(f.ctx, a.Chain, a.Mint)
	require.NoError(f.t, err)
	return s
}

func (f *fixture) reserveLedger(a PlatformStableAsset) ReserveLedger {
	f.t.Helper()
	rl, err := f.svc.ReserveLedger(f.ctx, testDB, a.ID)
	require.NoError(f.t, err)
	return rl
}

func (f *fixture) asset(a PlatformStableAsset) PlatformStableAsset {
	f.t.Helper()
	got, err := f.svc.Get(f.ctx, testDB, a.ID)
	require.NoError(f.t, err)
	return got
}

func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, testDB.QueryRow(f.ctx, sql, args...).Scan(&n))
	return n
}

// submissions counts the SUBMIT events the host journaled for a transaction:
// the proof that something was, or was not, submitted again.
func (f *fixture) submissions(txID devnet.TxID) int {
	return f.count(`SELECT count(*) FROM devnet_events WHERE network = $1 AND kind = 'SUBMIT' AND tx_id = $2`, f.network, string(txID))
}

func (f *fixture) reconcile(a PlatformStableAsset) SupplyReconciliation {
	f.t.Helper()
	var out SupplyReconciliation
	require.NoError(f.t, testDB.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = f.svc.ReconcileSupply(ctx, tx, a.ID, reconciliation.ModePeriodic, operator)
		return err
	}))
	return out
}

func (f *fixture) monitor(a PlatformStableAsset) MonitorReport {
	f.t.Helper()
	var out MonitorReport
	require.NoError(f.t, testDB.InTx(f.ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = f.svc.RunMonitor(ctx, tx, a.ID, operator)
		return err
	}))
	return out
}

// outOfBandMint has the issuer sign and submit a mint the platform never
// planned or booked: a rogue path the signer cannot tell from a real one,
// which reconciliation must catch.
func (f *fixture) outOfBandMint(a PlatformStableAsset, to devnet.PublicKey, amount uint64) {
	f.t.Helper()
	tip, err := f.host.Tip(f.ctx, a.Chain)
	require.NoError(f.t, err)
	plan := MintPlan{Version: mintPlanVersion, RequestID: uuid.NewString(), Chain: string(a.Chain), Mint: a.Mint.String(),
		Recipient: to.String(), Amount: fmt.Sprint(amount), MintAuthority: f.issuer.PublicKey().String(),
		FeePayer: f.issuer.PublicKey().String(), RecentBlockhash: tip.Hash.String()}
	msg, err := plan.Message()
	require.NoError(f.t, err)
	signed, err := f.issuer.SignMint(plan, msg)
	require.NoError(f.t, err)
	_, err = f.host.Submit(f.ctx, a.Chain, signed.Raw)
	require.NoError(f.t, err)
	_, err = f.host.ProduceBlocks(f.ctx, a.Chain, 1+itDepth)
	require.NoError(f.t, err)
}

// outOfBandBurn has a holder's custodied key burn units the platform never
// booked a redemption for.
func (f *fixture) outOfBandBurn(c customer, a PlatformStableAsset, amount uint64) {
	f.t.Helper()
	holder := f.custody.HolderAddress(c.id)
	tip, err := f.host.Tip(f.ctx, a.Chain)
	require.NoError(f.t, err)
	plan := BurnPlan{Version: burnPlanVersion, RequestID: uuid.NewString(), Chain: string(a.Chain), Mint: a.Mint.String(),
		Holder: holder.String(), Amount: fmt.Sprint(amount), FeePayer: holder.String(), RecentBlockhash: tip.Hash.String()}
	msg, err := plan.Message()
	require.NoError(f.t, err)
	signed, err := f.custody.SignHolderBurn(c.id, plan, msg)
	require.NoError(f.t, err)
	require.NoError(f.t, f.host.EnsureNativeBalance(f.ctx, a.Chain, holder, 1_000_000, 100_000_000))
	_, err = f.host.Submit(f.ctx, a.Chain, signed.Raw)
	require.NoError(f.t, err)
	_, err = f.host.ProduceBlocks(f.ctx, a.Chain, 1+itDepth)
	require.NoError(f.t, err)
}
